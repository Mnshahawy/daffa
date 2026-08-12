package api

// Manifest plan/apply: the declarative provisioning endpoints. See docs/provisioning.md
// for the doctrine; the short version that shapes this file:
//
//   - ENSURE-ONLY. Create what is missing, update safe fields, never delete, never
//     rotate trust. A CA or certificate that differs from its declaration is DRIFT,
//     reported and left alone.
//   - Names are identities. Every resource is resolved by the same unique key its
//     store enforces; ids never appear in a document.
//   - Secrets are slots. A value arrives beside the document (resolved by the CLI
//     from ITS environment), or the slot is reported unfilled. The document itself is
//     stored byte-identical, which is safe because the schema cannot carry a literal.
//   - Authorization is per-resource, preflighted for the WHOLE document before any
//     mutation. A partially-authorized apply would be worse than a refusal.
//
// Plan and apply are one walk with execution switched off or on, so they cannot
// disagree about what would happen.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/network"

	"github.com/Mnshahawy/daffa/internal/auth"
	"github.com/Mnshahawy/daffa/internal/caps"
	"github.com/Mnshahawy/daffa/internal/certs"
	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/httpx"
	"github.com/Mnshahawy/daffa/internal/manifest"
	"github.com/Mnshahawy/daffa/internal/sshx"
	"github.com/Mnshahawy/daffa/internal/stacks"
	"github.com/Mnshahawy/daffa/internal/store"
)

// ── wire shapes ─────────────────────────────────────────────────────────────────────

type manifestRequest struct {
	// Document is the manifest YAML, verbatim. Parsed here, server-side, so there is
	// exactly one parser and the UI can submit documents later without a second one.
	Document string `json:"document"`
	// Values carries the resolved value_from_env secrets, keyed by VARIABLE name.
	// They ride beside the document — never inside it — so the stored document stays
	// byte-identical to the file that was written.
	Values map[string]string `json:"values"`
}

// Verdicts, in the order of escalating attention they ask of the operator.
const (
	verdictInSync   = "in-sync"
	verdictCreate   = "create"
	verdictUpdate   = "update"
	verdictUnfilled = "unfilled"
	verdictBlocked  = "blocked"
	verdictDrifted  = "drifted"
)

type manifestResourceView struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Cluster string `json:"cluster,omitempty"`
	Verdict string `json:"verdict"`
	Detail  string `json:"detail,omitempty"`
	// ID is set on every verdict that resolved an existing or created row — the CLI's
	// --deploy walk depends on it being present for in-sync and update, not just create.
	ID string `json:"id,omitempty"`
}

type manifestUnfilledView struct {
	Kind    string `json:"kind"` // stack_env | stack_secret_file
	Stack   string `json:"stack,omitempty"`
	Cluster string `json:"cluster,omitempty"`
	Name    string `json:"name"`
}

type manifestSummaryView struct {
	Create   int `json:"create"`
	Update   int `json:"update"`
	InSync   int `json:"in_sync"`
	Drifted  int `json:"drifted"`
	Blocked  int `json:"blocked"`
	Unfilled int `json:"unfilled"`
}

type manifestReport struct {
	Name      string                 `json:"name"`
	DocHash   string                 `json:"doc_hash"`
	Resources []manifestResourceView `json:"resources"`
	Unfilled  []manifestUnfilledView `json:"unfilled"`
	Summary   manifestSummaryView    `json:"summary"`
}

// ── authorization ───────────────────────────────────────────────────────────────────

// manifestCapFor is THE authorization rule for manifests: each kind is checked against
// the same capability its imperative route declares, at the same scope. envScoped
// reports whether the check runs at the resource's cluster (true) or globally.
// TestManifestPreflightCoversEveryKind pins that every kind answers.
func manifestCapFor(k manifest.Kind) (c caps.Cap, envScoped bool) {
	switch k {
	case manifest.KindSSHKey:
		return caps.SSHKeysEdit, false
	case manifest.KindRegistry:
		return caps.RegistriesEdit, false
	case manifest.KindGitCredential:
		return caps.GitCredsEdit, false
	case manifest.KindNetwork:
		return caps.NetworksEdit, true
	case manifest.KindCA, manifest.KindCertificate, manifest.KindCertDelivery:
		return caps.CertsEdit, false
	case manifest.KindKeyring, manifest.KindKeyringDelivery:
		return caps.KeyringsEdit, false
	case manifest.KindStack:
		return caps.StacksEdit, true
	case manifest.KindVolumeSource:
		return caps.VolSourcesEdit, true
	}
	return caps.Cap{}, false // zero Cap fails closed in Has
}

// ── run state ───────────────────────────────────────────────────────────────────────

type manifestRun struct {
	s      *Server
	m      *manifest.Manifest
	values map[string]string
	user   *store.User
	apply  bool

	// envs resolves cluster NAME → env id; a name that resolved to nothing is absent,
	// and every resource on it verdicts blocked.
	envs   map[string]string
	report manifestReport

	// declaredIdx backs declares(); built on first use.
	declaredIdx map[string]bool
}

// declares reports whether the DOCUMENT declares a resource of this kind and name.
// It exists for plan: a reference that resolves to nothing in the store but is
// declared earlier in the same document will exist by the time apply reaches it (the
// walk runs in dependency order), so plan reads it as create, not blocked. Apply
// never needs the shortcut — its dependencies are really there — so a genuinely
// missing reference still blocks.
func (r *manifestRun) declares(k manifest.Kind, name string) bool {
	if r.declaredIdx == nil {
		idx := map[string]bool{}
		put := func(k manifest.Kind, name string) { idx[string(k)+"\x00"+name] = true }
		for _, x := range r.m.SSHKeys {
			put(manifest.KindSSHKey, x.Name)
		}
		for _, x := range r.m.GitCredentials {
			put(manifest.KindGitCredential, x.Name)
		}
		for _, x := range r.m.CAs {
			put(manifest.KindCA, x.Name)
		}
		for _, x := range r.m.Certificates {
			put(manifest.KindCertificate, x.Name)
		}
		for _, x := range r.m.Keyrings {
			put(manifest.KindKeyring, x.Name)
		}
		for _, x := range r.m.Stacks {
			put(manifest.KindStack, x.Name)
		}
		r.declaredIdx = idx
	}
	return r.declaredIdx[string(k)+"\x00"+name]
}

// pending is true when a missing reference is satisfied by the document itself and
// this is only a plan — see declares.
func (r *manifestRun) pending(k manifest.Kind, name string) bool {
	return !r.apply && r.declares(k, name)
}

// cluster resolves a resource's effective cluster name: its own, or the default.
func (r *manifestRun) cluster(own string) string {
	if own != "" {
		return own
	}
	return r.m.Cluster
}

func (r *manifestRun) push(v manifestResourceView) {
	r.report.Resources = append(r.report.Resources, v)
	switch v.Verdict {
	case verdictCreate:
		r.report.Summary.Create++
	case verdictUpdate:
		r.report.Summary.Update++
	case verdictInSync:
		r.report.Summary.InSync++
	case verdictDrifted:
		r.report.Summary.Drifted++
	case verdictBlocked:
		r.report.Summary.Blocked++
	case verdictUnfilled:
		r.report.Summary.Unfilled++
	}
}

func (r *manifestRun) secret(ref *manifest.SecretRef) (value string, ok bool) {
	if ref == nil || ref.ValueFromEnv == "" {
		return "", false
	}
	v, ok := r.values[ref.ValueFromEnv]
	return v, ok && v != ""
}

// blockedFor names what a missing secret slot needs — the register the CLI prints.
func secretNeed(ref *manifest.SecretRef) string {
	if ref == nil || ref.ValueFromEnv == "" {
		return "no value source declared — create it in the console, or declare value_from_env"
	}
	return "needs a value for " + ref.ValueFromEnv
}

// ── handlers ────────────────────────────────────────────────────────────────────────

func (s *Server) handleManifestPlan(w http.ResponseWriter, r *http.Request) {
	s.runManifest(w, r, false)
}

func (s *Server) handleManifestApply(w http.ResponseWriter, r *http.Request) {
	s.runManifest(w, r, true)
}

func (s *Server) runManifest(w http.ResponseWriter, r *http.Request, apply bool) {
	var req manifestRequest
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.BadRequest(w, r, err.Error())
		return
	}
	m, err := manifest.Parse([]byte(req.Document))
	if err != nil {
		httpx.Fail(w, r, http.StatusBadRequest, "bad_manifest", err.Error())
		return
	}
	if err := m.Validate(); err != nil {
		// The full joined list — an author fixing a file wants every problem at once.
		httpx.Fail(w, r, http.StatusBadRequest, "bad_manifest", err.Error())
		return
	}
	u, ok := auth.UserFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, http.StatusUnauthorized, "unauthenticated", "Sign in to continue.")
		return
	}

	run := &manifestRun{
		s: s, m: m, values: req.Values, user: u, apply: apply,
		envs: map[string]string{},
		report: manifestReport{
			Name:    m.Name,
			DocHash: manifest.Hash([]byte(req.Document)),
		},
	}

	if code, msg := run.preflight(r.Context()); code != "" {
		s.recordDenial(r, u, code)
		httpx.Fail(w, r, http.StatusForbidden, "missing_capability", msg)
		return
	}

	walkErr := run.walk(r.Context())

	// Record the row even when the walk died partway: the history exists to answer
	// "what did that apply actually do", and a partial answer is exactly what the
	// operator resuming a failed apply needs. The document is stored verbatim — safe
	// by construction, the schema cannot carry a secret.
	reportJSON, err := json.Marshal(run.report)
	if err == nil {
		row := &store.ManifestApply{
			Name: m.Name, DocHash: run.report.DocHash, Document: req.Document,
			Report: string(reportJSON), AppliedBy: u.ID, DryRun: !apply,
		}
		if err := s.store.CreateManifestApply(r.Context(), row); err != nil {
			// History must never fail the operation it records — same rule as notify.
			slog.Warn("manifest: recording apply history", "err", err)
		}
	}

	if apply {
		s.audit(r.Context(), store.AuditEntry{
			Action: "manifest.apply", Target: m.Name, Outcome: outcomeOf(walkErr),
			Detail: store.AuditDetail(map[string]any{
				"doc_hash": run.report.DocHash,
				"create":   run.report.Summary.Create, "update": run.report.Summary.Update,
				"blocked": run.report.Summary.Blocked, "drifted": run.report.Summary.Drifted,
			}),
		})
	}

	if walkErr != nil {
		httpx.Error(w, r, walkErr)
		return
	}
	httpx.JSON(w, http.StatusOK, run.report)
}

func outcomeOf(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

// preflight resolves every cluster the document names and checks every resource's
// capability. It returns a denial code and an operator-facing message; both empty
// means the whole document is authorized.
//
// A cluster that resolves to nothing gets ONE of two treatments, and the difference
// is deliberate: a caller holding the capability globally learns "no such cluster"
// as a blocked verdict (they are entitled to see the fleet); anyone else gets the
// same refusal whether the cluster is missing or merely theirs-not-to-touch —
// the manifest equivalent of the 404-not-403 rule in the scope middleware.
func (r *manifestRun) preflight(ctx context.Context) (code, msg string) {
	type need struct {
		cap     caps.Cap
		cluster string // "" = global
	}
	var needs []need
	add := func(k manifest.Kind, cluster string) {
		c, envScoped := manifestCapFor(k)
		if !envScoped {
			cluster = ""
		}
		needs = append(needs, need{c, cluster})
	}

	for range r.m.SSHKeys {
		add(manifest.KindSSHKey, "")
	}
	for range r.m.Registries {
		add(manifest.KindRegistry, "")
	}
	for range r.m.GitCredentials {
		add(manifest.KindGitCredential, "")
	}
	for _, n := range r.m.Networks {
		add(manifest.KindNetwork, r.cluster(n.Cluster))
	}
	for range r.m.CAs {
		add(manifest.KindCA, "")
	}
	for range r.m.Certificates {
		add(manifest.KindCertificate, "")
	}
	for range r.m.Keyrings {
		add(manifest.KindKeyring, "")
	}
	for range r.m.CertDeliveries {
		add(manifest.KindCertDelivery, "")
	}
	for range r.m.KeyringDeliveries {
		add(manifest.KindKeyringDelivery, "")
	}
	for _, st := range r.m.Stacks {
		add(manifest.KindStack, r.cluster(st.Cluster))
	}
	for _, v := range r.m.VolumeSources {
		add(manifest.KindVolumeSource, r.cluster(v.Cluster))
	}

	for _, n := range needs {
		if n.cluster == "" {
			if !r.user.Caps.Has(n.cap, "") {
				return "missing_capability:" + n.cap.Name(),
					fmt.Sprintf("Applying this manifest needs %s, held fleet-wide.", n.cap.Name())
			}
			continue
		}
		envID, err := r.resolveCluster(ctx, n.cluster)
		if err != nil {
			return "", "" // a store error surfaces in the walk, not as a denial
		}
		if envID == "" {
			// Unknown cluster. Global holders get an honest blocked verdict later;
			// everyone else cannot tell missing from forbidden.
			if r.user.Caps.Has(n.cap, "") {
				continue
			}
			return "missing_capability:" + n.cap.Name(), refusalFor(n.cap, n.cluster)
		}
		if !r.user.Caps.Has(n.cap, envID) {
			return "missing_capability:" + n.cap.Name(), refusalFor(n.cap, n.cluster)
		}
	}
	return "", ""
}

// refusalFor is worded to answer identically for "you may not" and "it does not
// exist" — naming the cluster only as the DOCUMENT declared it.
func refusalFor(c caps.Cap, cluster string) string {
	return fmt.Sprintf("Applying this manifest needs %s on cluster %q — either you do not hold it there, or no such cluster exists.", c.Name(), cluster)
}

// resolveCluster memoizes cluster name → env id; "" (with nil error) is a miss.
func (r *manifestRun) resolveCluster(ctx context.Context, name string) (string, error) {
	if id, ok := r.envs[name]; ok {
		return id, nil
	}
	env, err := r.s.store.EnvironmentByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		r.envs[name] = ""
		return "", nil
	}
	if err != nil {
		return "", err
	}
	r.envs[name] = env.ID
	return env.ID, nil
}

// envFor wraps resolveCluster for the walk: a miss pushes a blocked verdict and
// returns done=true so the caller moves on.
func (r *manifestRun) envFor(ctx context.Context, k manifest.Kind, name, cluster string) (envID string, blocked bool, err error) {
	id, err := r.resolveCluster(ctx, cluster)
	if err != nil {
		return "", false, err
	}
	if id == "" {
		r.push(manifestResourceView{
			Kind: string(k), Name: name, Cluster: cluster,
			Verdict: verdictBlocked, Detail: fmt.Sprintf("no such cluster %q", cluster),
		})
		return "", true, nil
	}
	return id, false, nil
}

// ── the walk ────────────────────────────────────────────────────────────────────────

// walk visits every kind in manifest.Order. Each ensure computes its verdict from the
// store and, when r.apply is set, executes create/update verdicts as it goes. An error
// return is an EXECUTION failure (store, sealer, daemon) — everything a document can
// get wrong becomes a verdict, not an error, so a plan can always finish.
func (r *manifestRun) walk(ctx context.Context) error {
	for _, kind := range manifest.Order {
		var err error
		switch kind {
		case manifest.KindSSHKey:
			for _, x := range r.m.SSHKeys {
				if err = r.ensureSSHKey(ctx, x); err != nil {
					return err
				}
			}
		case manifest.KindRegistry:
			for _, x := range r.m.Registries {
				if err = r.ensureRegistry(ctx, x); err != nil {
					return err
				}
			}
		case manifest.KindGitCredential:
			for _, x := range r.m.GitCredentials {
				if err = r.ensureGitCredential(ctx, x); err != nil {
					return err
				}
			}
		case manifest.KindNetwork:
			for _, x := range r.m.Networks {
				if err = r.ensureNetwork(ctx, x); err != nil {
					return err
				}
			}
		case manifest.KindCA:
			for _, x := range r.m.CAs {
				if err = r.ensureCA(ctx, x); err != nil {
					return err
				}
			}
		case manifest.KindCertificate:
			for _, x := range r.m.Certificates {
				if err = r.ensureCertificate(ctx, x); err != nil {
					return err
				}
			}
		case manifest.KindKeyring:
			for _, x := range r.m.Keyrings {
				if err = r.ensureKeyring(ctx, x); err != nil {
					return err
				}
			}
		case manifest.KindCertDelivery:
			for _, x := range r.m.CertDeliveries {
				if err = r.ensureCertDelivery(ctx, x); err != nil {
					return err
				}
			}
		case manifest.KindKeyringDelivery:
			for _, x := range r.m.KeyringDeliveries {
				if err = r.ensureKeyringDelivery(ctx, x); err != nil {
					return err
				}
			}
		case manifest.KindStack:
			for _, x := range r.m.Stacks {
				if err = r.ensureStack(ctx, x); err != nil {
					return err
				}
			}
		case manifest.KindVolumeSource:
			for _, x := range r.m.VolumeSources {
				if err = r.ensureVolumeSource(ctx, x); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// auditEnsure records one executed mutation. Verdicts that mutate nothing are not
// audit events — the summary entry in runManifest covers the apply itself.
func (r *manifestRun) auditEnsure(ctx context.Context, k manifest.Kind, envID, name, verdict string) {
	if !r.apply {
		return
	}
	r.s.audit(ctx, store.AuditEntry{
		EnvID: envID, Action: "manifest." + verdict, Target: string(k) + " " + name, Outcome: "ok",
	})
}

// ── per-kind ensures ────────────────────────────────────────────────────────────────

func (r *manifestRun) ensureSSHKey(ctx context.Context, k manifest.SSHKey) error {
	res := manifestResourceView{Kind: string(manifest.KindSSHKey), Name: k.Name}
	existing, err := r.s.store.SSHKeyByName(ctx, k.Name)
	switch {
	case err == nil:
		res.ID = existing.ID
		// The key material is immutable — a different algo cannot be converged, only
		// reported. Empty declared algo accepts whatever exists.
		if k.Algo != "" && !strings.EqualFold(k.Algo, existing.Algo) {
			res.Verdict, res.Detail = verdictDrifted, fmt.Sprintf("exists with algo %s; a key is never regenerated by apply", existing.Algo)
		} else {
			res.Verdict = verdictInSync
		}
	case errors.Is(err, store.ErrNotFound):
		res.Verdict = verdictCreate
		if r.apply {
			km, err := sshx.Generate(k.Algo, "daffa:"+k.Name)
			if err != nil {
				res.Verdict, res.Detail = verdictBlocked, err.Error()
				break
			}
			sealed, err := r.s.sealer.Seal(km.PrivatePEM)
			if err != nil {
				return err
			}
			sealedPass, err := r.s.sealer.Seal("")
			if err != nil {
				return err
			}
			key := &store.SSHKey{
				Name: k.Name, Algo: km.Algo, PublicKey: km.AuthorizedKey,
				Fingerprint: km.Fingerprint, PrivateKeyEnc: sealed, PassphraseEnc: sealedPass,
				CreatedBy: r.user.ID,
			}
			if err := r.s.store.CreateSSHKey(ctx, key); err != nil {
				return fmt.Errorf("manifest: creating ssh key %q: %w", k.Name, err)
			}
			res.ID = key.ID
			r.auditEnsure(ctx, manifest.KindSSHKey, "", k.Name, verdictCreate)
		}
	default:
		return err
	}
	r.push(res)
	return nil
}

func (r *manifestRun) ensureRegistry(ctx context.Context, reg manifest.Registry) error {
	res := manifestResourceView{Kind: string(manifest.KindRegistry), Name: reg.Name}
	host := dockerx.RegistryHost(reg.URL)
	existing, err := r.s.store.RegistryByName(ctx, reg.Name)
	switch {
	case err == nil:
		res.ID = existing.ID
		// The password is create-only in the API and unreadable here, so it is out of
		// the comparison; url/username changes cannot be converged without it.
		if (host != "" && existing.URL != host) || existing.Username != reg.Username {
			res.Verdict, res.Detail = verdictDrifted, "exists with a different url or username; registries have no update — recreate it deliberately"
		} else {
			res.Verdict = verdictInSync
		}
	case errors.Is(err, store.ErrNotFound):
		// Blocked-not-husk: a registry row with an empty password would authenticate
		// nothing and move the failure to deploy time, where it is unrecognizable.
		password, ok := r.secret(reg.Password)
		if !ok {
			res.Verdict, res.Detail = verdictBlocked, secretNeed(reg.Password)
			break
		}
		if host == "" {
			res.Verdict, res.Detail = verdictBlocked, fmt.Sprintf("%q does not look like a registry host", reg.URL)
			break
		}
		res.Verdict = verdictCreate
		if r.apply {
			sealed, err := r.s.sealer.Seal(password)
			if err != nil {
				return err
			}
			row := &store.Registry{Name: reg.Name, URL: host, Username: reg.Username, PasswordEnc: sealed}
			if err := r.s.store.CreateRegistry(ctx, row); err != nil {
				return fmt.Errorf("manifest: creating registry %q: %w", reg.Name, err)
			}
			res.ID = row.ID
			r.auditEnsure(ctx, manifest.KindRegistry, "", reg.Name, verdictCreate)
		}
	default:
		return err
	}
	r.push(res)
	return nil
}

func (r *manifestRun) ensureGitCredential(ctx context.Context, gc manifest.GitCredential) error {
	res := manifestResourceView{Kind: string(manifest.KindGitCredential), Name: gc.Name}
	existing, err := r.s.store.GitCredentialByName(ctx, gc.Name)
	switch {
	case err == nil:
		res.ID = existing.ID
		drift := existing.Kind != gc.Kind || existing.Username != strings.TrimSpace(gc.Username)
		if gc.Kind == store.GitSSH && !drift {
			key, err := r.s.store.SSHKeyByName(ctx, gc.SSHKey)
			drift = err != nil || existing.SSHKeyID != key.ID
		}
		if gc.HostKey != "" && existing.HostKey != strings.TrimSpace(gc.HostKey) {
			drift = true
		}
		if drift {
			res.Verdict, res.Detail = verdictDrifted, "exists with different settings; git credentials have no update — recreate it deliberately"
		} else {
			res.Verdict = verdictInSync
		}
	case errors.Is(err, store.ErrNotFound):
		row := &store.GitCredential{
			Name: gc.Name, Kind: gc.Kind, Username: strings.TrimSpace(gc.Username),
			HostKey: strings.TrimSpace(gc.HostKey), CreatedBy: r.user.ID,
		}
		switch gc.Kind {
		case store.GitToken:
			token, ok := r.secret(gc.Token)
			if !ok {
				res.Verdict, res.Detail = verdictBlocked, secretNeed(gc.Token)
				r.push(res)
				return nil
			}
			sealed, err := r.s.sealer.Seal(token)
			if err != nil {
				return err
			}
			row.TokenEnc = sealed
		case store.GitSSH:
			key, err := r.s.store.SSHKeyByName(ctx, gc.SSHKey)
			switch {
			case errors.Is(err, store.ErrNotFound) && r.pending(manifest.KindSSHKey, gc.SSHKey):
				// Plan: the key is declared earlier in this document and will exist
				// by the time apply reaches this credential.
			case errors.Is(err, store.ErrNotFound):
				res.Verdict, res.Detail = verdictBlocked, fmt.Sprintf("no such SSH key %q", gc.SSHKey)
				r.push(res)
				return nil
			case err != nil:
				return err
			default:
				row.SSHKeyID = key.ID
			}
		}
		res.Verdict = verdictCreate
		if r.apply {
			if err := r.s.store.CreateGitCredential(ctx, row); err != nil {
				return fmt.Errorf("manifest: creating git credential %q: %w", gc.Name, err)
			}
			res.ID = row.ID
			r.auditEnsure(ctx, manifest.KindGitCredential, "", gc.Name, verdictCreate)
		}
	default:
		return err
	}
	r.push(res)
	return nil
}

func (r *manifestRun) ensureNetwork(ctx context.Context, n manifest.Network) error {
	cluster := r.cluster(n.Cluster)
	res := manifestResourceView{Kind: string(manifest.KindNetwork), Name: n.Name, Cluster: cluster}
	envID, blocked, err := r.envFor(ctx, manifest.KindNetwork, n.Name, cluster)
	if err != nil || blocked {
		return err
	}
	env, err := r.s.pool.Get(envID)
	if err != nil {
		res.Verdict, res.Detail = verdictBlocked, "that cluster is not connected"
		r.push(res)
		return nil
	}
	// Overlay networks exist cluster-wide but are created through a manager; a
	// standalone host has exactly one daemon to ask.
	node, err := env.Control()
	if err != nil {
		if node, err = env.One(); err != nil {
			res.Verdict, res.Detail = verdictBlocked, "no reachable daemon on that cluster"
			r.push(res)
			return nil
		}
	}

	driver := n.Driver
	if driver == "" {
		driver = "overlay" // stack deploy's own default, same as the x-daffa hooks
	}
	inspect, err := node.Client.NetworkInspect(ctx, n.Name, network.InspectOptions{})
	if err == nil {
		res.ID = inspect.ID
		// Docker cannot mutate a network in place, and recreating one under live
		// services is not a thing apply should ever do — differences are drift.
		if inspect.Driver != driver || inspect.Attachable != n.Attachable {
			res.Verdict = verdictDrifted
			res.Detail = fmt.Sprintf("exists as %s (attachable=%t); networks cannot be changed in place", inspect.Driver, inspect.Attachable)
		} else {
			res.Verdict = verdictInSync
		}
		r.push(res)
		return nil
	}

	res.Verdict = verdictCreate
	if r.apply {
		created, err := node.Client.NetworkCreate(ctx, n.Name, network.CreateOptions{
			Driver: driver, Attachable: n.Attachable,
		})
		if err != nil {
			res.Verdict, res.Detail = verdictBlocked, fmt.Sprintf("creating the network: %v", err)
			r.push(res)
			return nil
		}
		res.ID = created.ID
		r.auditEnsure(ctx, manifest.KindNetwork, envID, n.Name, verdictCreate)
	}
	r.push(res)
	return nil
}

func (r *manifestRun) ensureCA(ctx context.Context, ca manifest.CA) error {
	res := manifestResourceView{Kind: string(manifest.KindCA), Name: ca.Name}
	outbound := ca.OutboundTrust == nil || *ca.OutboundTrust
	existing, err := r.s.store.CertAuthorityByName(ctx, ca.Name)
	switch {
	case err == nil:
		res.ID = existing.ID
		switch {
		// Trust material is never rotated by apply. Parameter differences are drift.
		case ca.KeyAlgo != "" && existing.KeyAlgo != ca.KeyAlgo:
			res.Verdict, res.Detail = verdictDrifted, fmt.Sprintf("exists with key algo %s; a CA is never re-keyed by apply", existing.KeyAlgo)
		case !strings.Contains(existing.Subject, "CN="+strings.TrimSpace(ca.CommonName)):
			res.Verdict, res.Detail = verdictDrifted, fmt.Sprintf("exists as %q; a CA is never re-issued by apply", existing.Subject)
		case existing.OutboundTrust != outbound:
			// The one safe field: whether Daffa's own outbound TLS trusts this CA.
			res.Verdict = verdictUpdate
			if r.apply {
				existing.OutboundTrust = outbound
				if err := r.s.store.UpdateCertAuthority(ctx, existing); err != nil {
					return fmt.Errorf("manifest: updating CA %q: %w", ca.Name, err)
				}
				r.auditEnsure(ctx, manifest.KindCA, "", ca.Name, verdictUpdate)
			}
		default:
			res.Verdict = verdictInSync
		}
	case errors.Is(err, store.ErrNotFound):
		res.Verdict = verdictCreate
		if r.apply {
			algo := certs.KeyAlgo(ca.KeyAlgo)
			if algo == "" {
				algo = certs.ECDSAP256
			}
			days := ca.Days
			if days <= 0 {
				days = 3650
			}
			certPEM, keyPEM, err := certs.CreateCA(strings.TrimSpace(ca.CommonName), strings.TrimSpace(ca.Org), algo, days)
			if err != nil {
				res.Verdict, res.Detail = verdictBlocked, err.Error()
				break
			}
			parsed, _ := certs.ParseCert(certPEM)
			sealed, err := r.s.sealer.Seal(keyPEM)
			if err != nil {
				return err
			}
			row := &store.CertAuthority{
				Name: ca.Name, CertPEM: certPEM, KeyEnc: sealed,
				Subject: parsed.Subject.String(), KeyAlgo: string(algo),
				NotBefore: parsed.NotBefore, NotAfter: parsed.NotAfter,
				Status: "active", OutboundTrust: outbound, CreatedBy: r.user.ID,
			}
			if err := r.s.store.CreateCertAuthority(ctx, row); err != nil {
				return fmt.Errorf("manifest: creating CA %q: %w", ca.Name, err)
			}
			res.ID = row.ID
			r.auditEnsure(ctx, manifest.KindCA, "", ca.Name, verdictCreate)
		}
	default:
		return err
	}
	r.push(res)
	return nil
}

func (r *manifestRun) ensureCertificate(ctx context.Context, c manifest.Certificate) error {
	cluster := r.cluster(c.Cluster)
	res := manifestResourceView{Kind: string(manifest.KindCertificate), Name: c.Name}
	envID := ""
	if !c.Shared {
		res.Cluster = cluster
		var blocked bool
		var err error
		envID, blocked, err = r.envFor(ctx, manifest.KindCertificate, c.Name, cluster)
		if err != nil || blocked {
			return err
		}
	}

	sans := cleanSANs(c.SANs)
	usages, err := certs.NormalizeUsages(c.Usages)
	if err != nil {
		res.Verdict, res.Detail = verdictBlocked, err.Error()
		r.push(res)
		return nil
	}

	existing, err := r.s.store.CertificateByName(ctx, envID, c.Name)
	switch {
	case err == nil:
		res.ID = existing.ID
		ca, caErr := r.s.store.CertAuthorityByName(ctx, c.CA)
		switch {
		case strings.Join(sans, " ") != existing.SANs:
			res.Verdict, res.Detail = verdictDrifted, fmt.Sprintf("exists for %q; a certificate is never re-issued by apply — renew it deliberately", existing.SANs)
		case usages != "" && usages != existing.Usages:
			res.Verdict, res.Detail = verdictDrifted, fmt.Sprintf("exists with usages %q; a certificate is never re-issued by apply", existing.Usages)
		case c.KeyAlgo != "" && existing.KeyAlgo != c.KeyAlgo:
			res.Verdict, res.Detail = verdictDrifted, fmt.Sprintf("exists with key algo %s; a certificate is never re-issued by apply", existing.KeyAlgo)
		case caErr == nil && existing.CAID != "" && existing.CAID != ca.ID:
			res.Verdict, res.Detail = verdictDrifted, "exists signed by a different CA; a certificate is never re-signed by apply"
		case c.RenewBeforeDays != 0 && existing.RenewBeforeDays != c.RenewBeforeDays:
			// The renewal window is bookkeeping, not trust — the one safe update.
			res.Verdict = verdictUpdate
			if r.apply {
				existing.RenewBeforeDays = c.RenewBeforeDays
				if err := r.s.store.UpdateCertificate(ctx, existing); err != nil {
					return fmt.Errorf("manifest: updating certificate %q: %w", c.Name, err)
				}
				r.auditEnsure(ctx, manifest.KindCertificate, envID, c.Name, verdictUpdate)
			}
		default:
			res.Verdict = verdictInSync
		}
	case errors.Is(err, store.ErrNotFound):
		ca, err := r.s.store.CertAuthorityByName(ctx, c.CA)
		if errors.Is(err, store.ErrNotFound) {
			if r.pending(manifest.KindCA, c.CA) {
				res.Verdict = verdictCreate // the CA is created earlier in this same apply
				break
			}
			res.Verdict, res.Detail = verdictBlocked, fmt.Sprintf("no such certificate authority %q", c.CA)
			break
		} else if err != nil {
			return err
		}
		if !ca.CanSign() {
			res.Verdict, res.Detail = verdictBlocked, fmt.Sprintf("CA %q was uploaded without its private key, so it cannot sign", c.CA)
			break
		}
		res.Verdict = verdictCreate
		if r.apply {
			algo := certs.KeyAlgo(c.KeyAlgo)
			if algo == "" {
				algo = certs.ECDSAP256
			}
			days := c.ValidityDays
			if days <= 0 {
				days = 398
			}
			caKey, err := r.s.sealer.Open(ca.KeyEnc)
			if err != nil {
				return errors.New("manifest: could not decrypt the CA key (was the master key replaced?)")
			}
			certPEM, keyPEM, err := certs.Issue(ca.CertPEM, caKey, sans, algo, days, usages)
			if err != nil {
				res.Verdict, res.Detail = verdictBlocked, err.Error()
				break
			}
			parsed, _ := certs.ParseCert(certPEM)
			sealed, err := r.s.sealer.Seal(keyPEM)
			if err != nil {
				return err
			}
			row := &store.Certificate{
				Name: c.Name, EnvID: envID, CAID: ca.ID,
				CertPEM: certPEM, KeyEnc: sealed,
				SANs: strings.Join(sans, " "), Usages: usages, KeyAlgo: string(algo),
				NotBefore: parsed.NotBefore, NotAfter: parsed.NotAfter,
				ValidityDays: days, RenewBeforeDays: c.RenewBeforeDays, CreatedBy: r.user.ID,
			}
			if err := r.s.store.CreateCertificate(ctx, row); err != nil {
				return fmt.Errorf("manifest: creating certificate %q: %w", c.Name, err)
			}
			res.ID = row.ID
			r.auditEnsure(ctx, manifest.KindCertificate, envID, c.Name, verdictCreate)
		}
	default:
		return err
	}
	r.push(res)
	return nil
}

func (r *manifestRun) ensureKeyring(ctx context.Context, k manifest.Keyring) error {
	res := manifestResourceView{Kind: string(manifest.KindKeyring), Name: k.Name}
	existing, err := r.s.store.KeyringByName(ctx, k.Name)
	switch {
	case err == nil:
		res.ID = existing.ID
		if existing.RotateDays != k.RotateDays {
			res.Verdict = verdictUpdate
			if r.apply {
				existing.RotateDays = k.RotateDays
				if err := r.s.store.UpdateKeyring(ctx, existing); err != nil {
					return fmt.Errorf("manifest: updating keyring %q: %w", k.Name, err)
				}
				r.auditEnsure(ctx, manifest.KindKeyring, "", k.Name, verdictUpdate)
			}
		} else {
			res.Verdict = verdictInSync
		}
	case errors.Is(err, store.ErrNotFound):
		res.Verdict = verdictCreate
		if r.apply {
			row := &store.Keyring{Name: k.Name, RotateDays: k.RotateDays, CreatedBy: r.user.ID}
			if err := r.s.store.CreateKeyring(ctx, row); err != nil {
				return fmt.Errorf("manifest: creating keyring %q: %w", k.Name, err)
			}
			// Seed the first version, exactly like the create handler: a keyring with
			// nothing to encrypt with is an invalid state. And like it, leave no
			// half-made keyring behind on failure.
			if _, err := r.s.rotateKeyring(ctx, row); err != nil {
				_ = r.s.store.DeleteKeyring(ctx, row.ID)
				return fmt.Errorf("manifest: seeding keyring %q: %w", k.Name, err)
			}
			res.ID = row.ID
			r.auditEnsure(ctx, manifest.KindKeyring, "", k.Name, verdictCreate)
		}
	default:
		return err
	}
	r.push(res)
	return nil
}

func (r *manifestRun) ensureCertDelivery(ctx context.Context, d manifest.CertDelivery) error {
	cluster := r.cluster(d.Cluster)
	res := manifestResourceView{Kind: string(manifest.KindCertDelivery), Name: d.Volume, Cluster: cluster}
	envID, blocked, err := r.envFor(ctx, manifest.KindCertDelivery, d.Volume, cluster)
	if err != nil || blocked {
		return err
	}

	// Resolve declared cert names: env-scoped first, then shared — the same reach a
	// delivery has. Missing anywhere blocks the delivery, not the walk; missing but
	// declared in this document only softens a PLAN (see declares).
	pendingRefs := false
	certSet := make([]store.DeliveryCert, 0, len(d.Certs))
	for _, dc := range d.Certs {
		c, err := r.s.store.CertificateByName(ctx, envID, dc.Name)
		if errors.Is(err, store.ErrNotFound) {
			c, err = r.s.store.CertificateByName(ctx, "", dc.Name)
		}
		if errors.Is(err, store.ErrNotFound) {
			if r.pending(manifest.KindCertificate, dc.Name) {
				pendingRefs = true
				continue
			}
			res.Verdict, res.Detail = verdictBlocked, fmt.Sprintf("no such certificate %q on this cluster or shared", dc.Name)
			r.push(res)
			return nil
		} else if err != nil {
			return err
		}
		certSet = append(certSet, store.DeliveryCert{CertID: c.ID, IsDefault: dc.Default})
	}
	bundleIDs := make([]string, 0, len(d.BundleCAs))
	for _, name := range d.BundleCAs {
		ca, err := r.s.store.CertAuthorityByName(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			if r.pending(manifest.KindCA, name) {
				pendingRefs = true
				continue
			}
			res.Verdict, res.Detail = verdictBlocked, fmt.Sprintf("no such certificate authority %q in bundle_cas", name)
			r.push(res)
			return nil
		} else if err != nil {
			return err
		}
		bundleIDs = append(bundleIDs, ca.ID)
	}
	mountPath, err := cleanMountPath(d.MountPath)
	if err != nil {
		res.Verdict, res.Detail = verdictBlocked, err.Error()
		r.push(res)
		return nil
	}

	existing, err := r.s.store.DeliveriesForVolume(ctx, envID, d.Volume)
	if err != nil {
		return err
	}
	switch {
	case len(existing) > 1:
		// (env, volume) is deliberately not unique for plain deliveries. Several
		// hand-made ones sharing a volume is legal — but then "the delivery for this
		// volume" is not a question a manifest can answer, so it manages none of them.
		res.Verdict, res.Detail = verdictBlocked, fmt.Sprintf("%d deliveries already write this volume — a manifest manages at most one per volume", len(existing))
	case len(existing) == 1:
		ex := existing[0]
		res.ID = ex.ID
		if pendingRefs {
			// Plan only: some referenced material is created by this same document,
			// so the stored set cannot be compared yet.
			res.Verdict, res.Detail = verdictUpdate, "certificate set completes once this document's creations exist"
			break
		}
		if certSetEqual(ex.Certs, certSet) && ex.MountPath == mountPath &&
			ex.UID == d.UID && ex.GID == d.GID && ex.Traefik == d.Traefik &&
			ex.RestartTargets == strings.TrimSpace(d.RestartTargets) &&
			stringSetEqual(strings.Fields(ex.BundleCAs), bundleIDs) {
			res.Verdict = verdictInSync
			break
		}
		res.Verdict = verdictUpdate
		if r.apply {
			ex.Certs, ex.MountPath, ex.UID, ex.GID = certSet, mountPath, d.UID, d.GID
			ex.Traefik = d.Traefik
			ex.RestartTargets = strings.TrimSpace(d.RestartTargets)
			ex.BundleCAs = strings.Join(bundleIDs, " ")
			if ex.Traefik {
				if err := r.s.refuseSecondTraefikDelivery(ctx, ex); err != nil {
					res.Verdict, res.Detail = verdictBlocked, err.Error()
					break
				}
			}
			if err := r.s.store.UpdateCertDelivery(ctx, ex); err != nil {
				return fmt.Errorf("manifest: updating delivery for %q: %w", d.Volume, err)
			}
			r.auditEnsure(ctx, manifest.KindCertDelivery, envID, d.Volume, verdictUpdate)
			// Forced resync in the background, like the update handler: an edit that
			// only moves mount_path still has to rewrite tls.yml.
			go func(dd store.CertDelivery) {
				dd.SyncedHash = ""
				r.s.reportDeliverySync(context.WithoutCancel(ctx), &dd)
			}(*ex)
		}
	default:
		if _, err := r.s.pool.Get(envID); err != nil {
			res.Verdict, res.Detail = verdictBlocked, "that cluster is not connected"
			break
		}
		res.Verdict = verdictCreate
		if r.apply {
			row := &store.CertDelivery{
				EnvID: envID, Certs: certSet, Volume: d.Volume, MountPath: mountPath,
				UID: d.UID, GID: d.GID, Traefik: d.Traefik,
				RestartTargets: strings.TrimSpace(d.RestartTargets),
				BundleCAs:      strings.Join(bundleIDs, " "),
				CreatedBy:      r.user.ID,
			}
			if row.Traefik {
				if err := r.s.refuseSecondTraefikDelivery(ctx, row); err != nil {
					res.Verdict, res.Detail = verdictBlocked, err.Error()
					break
				}
			}
			if _, err := r.s.store.FleetDeliveryForVolume(ctx, envID, d.Volume); err == nil {
				res.Verdict, res.Detail = verdictBlocked, "a fleet delivery already writes this volume"
				break
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if err := r.s.store.CreateCertDelivery(ctx, row); err != nil {
				return fmt.Errorf("manifest: creating delivery for %q: %w", d.Volume, err)
			}
			res.ID = row.ID
			r.auditEnsure(ctx, manifest.KindCertDelivery, envID, d.Volume, verdictCreate)
			go func(dd store.CertDelivery) {
				r.s.reportDeliverySync(context.WithoutCancel(ctx), &dd)
			}(*row)
		}
	}
	r.push(res)
	return nil
}

func (r *manifestRun) ensureKeyringDelivery(ctx context.Context, d manifest.KeyringDelivery) error {
	cluster := r.cluster(d.Cluster)
	res := manifestResourceView{Kind: string(manifest.KindKeyringDelivery), Name: d.Volume, Cluster: cluster}
	envID, blocked, err := r.envFor(ctx, manifest.KindKeyringDelivery, d.Volume, cluster)
	if err != nil || blocked {
		return err
	}
	k, err := r.s.store.KeyringByName(ctx, d.Keyring)
	if errors.Is(err, store.ErrNotFound) {
		if r.pending(manifest.KindKeyring, d.Keyring) {
			res.Verdict = verdictCreate // the keyring is created earlier in this same apply
			r.push(res)
			return nil
		}
		res.Verdict, res.Detail = verdictBlocked, fmt.Sprintf("no such keyring %q", d.Keyring)
		r.push(res)
		return nil
	} else if err != nil {
		return err
	}

	existing, err := r.s.store.KeyringDeliveryForVolume(ctx, k.ID, envID, d.Volume)
	switch {
	case err == nil:
		res.ID = existing.ID
		// Keyring deliveries have no update route; placement differences are drift.
		if existing.UID != d.UID || existing.GID != d.GID ||
			existing.RestartTargets != strings.TrimSpace(d.RestartTargets) {
			res.Verdict, res.Detail = verdictDrifted, "exists with different uid/gid/restart targets; keyring deliveries have no update — recreate it deliberately"
		} else {
			res.Verdict = verdictInSync
		}
	case errors.Is(err, store.ErrNotFound):
		if _, err := r.s.pool.Get(envID); err != nil {
			res.Verdict, res.Detail = verdictBlocked, "that cluster is not connected"
			break
		}
		res.Verdict = verdictCreate
		if r.apply {
			row := &store.KeyringDelivery{
				KeyringID: k.ID, EnvID: envID, Volume: d.Volume,
				UID: d.UID, GID: d.GID,
				RestartTargets: strings.TrimSpace(d.RestartTargets),
				CreatedBy:      r.user.ID,
			}
			if err := r.s.store.CreateKeyringDelivery(ctx, row); err != nil {
				return fmt.Errorf("manifest: creating keyring delivery for %q: %w", d.Volume, err)
			}
			res.ID = row.ID
			r.auditEnsure(ctx, manifest.KindKeyringDelivery, envID, d.Volume, verdictCreate)
			go func(dd store.KeyringDelivery) {
				r.s.reportKeyringDeliverySync(context.WithoutCancel(ctx), &dd)
			}(*row)
		}
	default:
		return err
	}
	r.push(res)
	return nil
}

func (r *manifestRun) ensureStack(ctx context.Context, st manifest.Stack) error {
	cluster := r.cluster(st.Cluster)
	res := manifestResourceView{Kind: string(manifest.KindStack), Name: st.Name, Cluster: cluster}
	envID, blocked, err := r.envFor(ctx, manifest.KindStack, st.Name, cluster)
	if err != nil || blocked {
		return err
	}

	credID := ""
	if st.Source.Git != nil && st.Source.Git.Credential != "" {
		cred, err := r.s.store.GitCredentialByName(ctx, st.Source.Git.Credential)
		switch {
		case errors.Is(err, store.ErrNotFound) && r.pending(manifest.KindGitCredential, st.Source.Git.Credential):
			// Plan: created earlier in this same document.
		case errors.Is(err, store.ErrNotFound):
			res.Verdict, res.Detail = verdictBlocked, fmt.Sprintf("no such git credential %q", st.Source.Git.Credential)
			r.push(res)
			return nil
		case err != nil:
			return err
		default:
			credID = cred.ID
		}
	}

	existing, err := r.s.store.StackByName(ctx, envID, st.Name)
	created := false
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Placement, headless: the same table placementFor enforces. A swarm stack is
		// placed by the scheduler; a compose stack on a multi-node cluster must name
		// its machine, which the manifest cannot yet do — an honest blocked verdict.
		env, err := r.s.pool.Get(envID)
		if err != nil {
			res.Verdict, res.Detail = verdictBlocked, "that cluster is not connected"
			r.push(res)
			return nil
		}
		nodeID := ""
		if st.Engine == stacks.SwarmEngine.Name() {
			if !env.IsSwarm() {
				res.Verdict, res.Detail = verdictBlocked, "this cluster is a standalone host, not a Swarm, so it cannot run a Swarm stack"
				r.push(res)
				return nil
			}
		} else {
			node, err := env.One()
			if err != nil {
				res.Verdict, res.Detail = verdictBlocked, "a Compose stack on a multi-node cluster must be pinned to a node — create it in the console, then re-apply"
				r.push(res)
				return nil
			}
			nodeID = node.ID
		}
		res.Verdict = verdictCreate
		if !r.apply {
			r.push(res)
			return r.reportStackSlots(ctx, st, cluster, nil)
		}
		row := &store.Stack{
			EnvID: envID, NodeID: nodeID, Name: st.Name, Engine: st.Engine,
			CreatedBy: r.user.ID,
		}
		applyStackSource(row, st, credID)
		if err := r.s.store.CreateStack(ctx, row); err != nil {
			return fmt.Errorf("manifest: creating stack %q: %w", st.Name, err)
		}
		existing, created = row, true
		res.ID = row.ID
		r.auditEnsure(ctx, manifest.KindStack, envID, st.Name, verdictCreate)
	case err != nil:
		return err
	default:
		res.ID = existing.ID
		if existing.Engine != st.Engine {
			res.Verdict, res.Detail = verdictDrifted, fmt.Sprintf("exists with engine %s, which is immutable — create a new stack to change it", existing.Engine)
			r.push(res)
			return nil
		}
		if existing.SourceKind == "git" && st.Source.Compose != "" {
			// The API refuses git → inline (the repo is the source of truth; there is
			// nothing to convert back). Same rule, same reason.
			res.Verdict, res.Detail = verdictDrifted, "exists as a git stack; only inline → git can be converged"
			r.push(res)
			return nil
		}
	}

	// Converge source, auto-deploy and env slots. Every change flips the verdict to
	// update; none leaves in-sync standing.
	changed := false
	if !created {
		want := *existing
		applyStackSource(&want, st, credID)
		if want.SourceKind != existing.SourceKind || want.GitURL != existing.GitURL ||
			want.GitRef != existing.GitRef || want.GitPath != existing.GitPath ||
			want.GitCredentialID != existing.GitCredentialID || want.InlineYAML != existing.InlineYAML {
			changed = true
			if r.apply {
				if err := r.s.store.UpdateStackSource(ctx, &want); err != nil {
					return fmt.Errorf("manifest: updating stack %q: %w", st.Name, err)
				}
			}
		}
		existing = &want
	}

	watch := strings.Join(st.WatchPaths, "\n")
	if existing.AutoDeploy != st.AutoDeploy || existing.WatchPaths != watch {
		changed = true
		if r.apply {
			if err := r.s.store.SetStackAutoDeploy(ctx, existing.ID, st.AutoDeploy, watch); err != nil {
				return fmt.Errorf("manifest: setting auto-deploy on %q: %w", st.Name, err)
			}
		}
	}
	// A git stack turning auto-deploy on needs a webhook secret to verify pushes with.
	if r.apply && st.AutoDeploy && existing.SourceKind == "git" && existing.WebhookSecretEnc == "" {
		secret, err := randomToken()
		if err != nil {
			return err
		}
		sealed, err := r.s.sealer.Seal(secret)
		if err != nil {
			return err
		}
		if err := r.s.store.SetStackWebhookSecret(ctx, existing.ID, sealed); err != nil {
			return fmt.Errorf("manifest: minting webhook secret for %q: %w", st.Name, err)
		}
	}

	envChanged, err := r.ensureStackEnv(ctx, existing, st)
	if err != nil {
		return err
	}
	changed = changed || envChanged

	switch {
	case created:
		res.Verdict = verdictCreate
	case changed:
		res.Verdict = verdictUpdate
		if r.apply {
			r.auditEnsure(ctx, manifest.KindStack, envID, st.Name, verdictUpdate)
		}
	default:
		res.Verdict = verdictInSync
	}
	r.push(res)
	return r.reportStackSlots(ctx, st, cluster, existing)
}

// applyStackSource copies a manifest stack's declared source onto a store row.
func applyStackSource(row *store.Stack, st manifest.Stack, credID string) {
	if st.Source.Git != nil {
		row.SourceKind = "git"
		row.GitURL = strings.TrimSpace(st.Source.Git.URL)
		row.GitRef = strings.TrimSpace(st.Source.Git.Ref)
		row.GitPath = strings.TrimSpace(st.Source.Git.Path)
		row.GitCredentialID = credID
		row.InlineYAML = ""
	} else {
		row.SourceKind = "inline"
		row.InlineYAML = st.Source.Compose
		row.GitURL, row.GitRef, row.GitPath, row.GitCredentialID = "", "", "", ""
	}
}

// ensureStackEnv merges declared env vars into the stack's stored set. MERGE, not
// replace: the manifest declares what must exist; vars someone added by hand are not
// its to delete. Returns whether anything was (or would be) written.
func (r *manifestRun) ensureStackEnv(ctx context.Context, row *store.Stack, st manifest.Stack) (bool, error) {
	if row == nil || len(st.Env) == 0 {
		return false, nil
	}
	stored, err := r.s.store.StackEnv(ctx, row.ID)
	if err != nil {
		return false, err
	}
	byKey := map[string]int{}
	for i, e := range stored {
		byKey[e.Key] = i
	}

	changed := false
	for _, v := range st.Env {
		want, haveValue := "", false
		switch {
		case !v.Secret:
			want, haveValue = v.Value, true
		case v.ValueFromEnv != "":
			if val, ok := r.values[v.ValueFromEnv]; ok && val != "" {
				// A value the submitter resolved is declared intent — it overwrites.
				want, haveValue = val, true
			}
		}

		i, exists := byKey[v.Key]
		switch {
		case !exists:
			// New key. A secret slot with no value is created EMPTY — visible in the
			// console as a secret awaiting its value — and reported unfilled.
			sealed, err := r.s.sealer.Seal(want)
			if err != nil {
				return changed, err
			}
			stored = append(stored, store.StackEnv{Key: v.Key, ValueEnc: sealed, IsSecret: v.Secret})
			byKey[v.Key] = len(stored) - 1
			changed = true
		case haveValue:
			cur, err := r.s.sealer.Open(stored[i].ValueEnc)
			if err != nil {
				return changed, fmt.Errorf("manifest: could not decrypt env %s on %q (was the master key replaced?)", v.Key, st.Name)
			}
			if cur != want || stored[i].IsSecret != v.Secret {
				sealed, err := r.s.sealer.Seal(want)
				if err != nil {
					return changed, err
				}
				stored[i] = store.StackEnv{Key: v.Key, ValueEnc: sealed, IsSecret: v.Secret}
				changed = true
			}
		default:
			// A filled slot is never overwritten by a slot declaration — a human put
			// that value there.
		}
	}

	if changed && r.apply {
		if err := r.s.store.SetStackEnv(ctx, row.ID, stored); err != nil {
			return changed, fmt.Errorf("manifest: storing env for %q: %w", st.Name, err)
		}
	}
	return changed, nil
}

// reportStackSlots lists the stack's unfilled secret slots: env vars whose sealed
// value is empty, and declared secret files that have no stored content. Secret FILES
// are never created as rows — an empty raft secret is a bad boot waiting to happen —
// they simply stay unfilled until someone PUTs content.
func (r *manifestRun) reportStackSlots(ctx context.Context, st manifest.Stack, cluster string, row *store.Stack) error {
	unfilled := func(kind, name string) {
		r.report.Unfilled = append(r.report.Unfilled, manifestUnfilledView{
			Kind: kind, Stack: st.Name, Cluster: cluster, Name: name,
		})
	}

	var stored []store.StackEnv
	var secretRows []store.StackSecret
	if row != nil {
		var err error
		if stored, err = r.s.store.StackEnv(ctx, row.ID); err != nil {
			return err
		}
		if secretRows, err = r.s.store.StackSecrets(ctx, row.ID); err != nil {
			return err
		}
	}
	envByKey := map[string]store.StackEnv{}
	for _, e := range stored {
		envByKey[e.Key] = e
	}
	haveSecret := map[string]bool{}
	for _, sec := range secretRows {
		haveSecret[sec.Name] = sec.ContentEnc != ""
	}

	for _, v := range st.Env {
		if !v.Secret {
			continue
		}
		e, ok := envByKey[v.Key]
		if !ok {
			unfilled("stack_env", v.Key)
			continue
		}
		val, err := r.s.sealer.Open(e.ValueEnc)
		if err != nil {
			return fmt.Errorf("manifest: could not decrypt env %s on %q (was the master key replaced?)", v.Key, st.Name)
		}
		if val == "" {
			unfilled("stack_env", v.Key)
		}
	}
	for _, name := range st.SecretFiles {
		if !haveSecret[name] {
			unfilled("stack_secret_file", name)
		}
	}
	return nil
}

func (r *manifestRun) ensureVolumeSource(ctx context.Context, v manifest.VolumeSource) error {
	cluster := r.cluster(v.Cluster)
	res := manifestResourceView{Kind: string(manifest.KindVolumeSource), Name: v.Volume, Cluster: cluster}
	envID, blocked, err := r.envFor(ctx, manifest.KindVolumeSource, v.Volume, cluster)
	if err != nil || blocked {
		return err
	}

	credID, stackID := "", ""
	if v.Source.Git != nil && v.Source.Git.Credential != "" {
		cred, err := r.s.store.GitCredentialByName(ctx, v.Source.Git.Credential)
		switch {
		case errors.Is(err, store.ErrNotFound) && r.pending(manifest.KindGitCredential, v.Source.Git.Credential):
			// Plan: created earlier in this same document.
		case errors.Is(err, store.ErrNotFound):
			res.Verdict, res.Detail = verdictBlocked, fmt.Sprintf("no such git credential %q", v.Source.Git.Credential)
			r.push(res)
			return nil
		case err != nil:
			return err
		default:
			credID = cred.ID
		}
	}
	if v.Stack != "" {
		st, err := r.s.store.StackByName(ctx, envID, v.Stack)
		switch {
		case errors.Is(err, store.ErrNotFound) && r.pending(manifest.KindStack, v.Stack):
			// Plan: created earlier in this same document.
		case errors.Is(err, store.ErrNotFound):
			res.Verdict, res.Detail = verdictBlocked, fmt.Sprintf("no such stack %q on this cluster to link", v.Stack)
			r.push(res)
			return nil
		case err != nil:
			return err
		default:
			stackID = st.ID
		}
	}

	var files []store.VolSourceFile
	names := make([]string, 0, len(v.Source.Files))
	for _, f := range v.Source.Files {
		files = append(files, store.VolSourceFile{Path: f.Path, Content: f.Content})
		names = append(names, f.Path)
	}
	if len(names) > 0 {
		// Same preflight as the create/update handler: a delivery may own filenames in
		// this volume (the mixed Traefik dynamic directory), and refusing at plan time
		// beats storing a file set that can never be delivered.
		if err := r.s.refuseDeliveryOwnedNames(ctx, envID, v.Volume, names); err != nil {
			res.Verdict, res.Detail = verdictBlocked, err.Error()
			r.push(res)
			return nil
		}
		if err := r.s.refuseFleetOwnedNames(ctx, envID, v.Volume, names); err != nil {
			res.Verdict, res.Detail = verdictBlocked, err.Error()
			r.push(res)
			return nil
		}
	}

	existing, err := r.s.store.VolumeSourceByVolume(ctx, envID, v.Volume)
	switch {
	case errors.Is(err, store.ErrNotFound):
		res.Verdict = verdictCreate
		if r.apply {
			row := &store.VolumeSource{
				EnvID: envID, Volume: v.Volume,
				UID: v.UID, GID: v.GID, StackID: stackID,
				RestartTargets: strings.TrimSpace(v.RestartTargets),
			}
			applyVolumeSourceSpec(row, v, credID)
			if err := r.s.store.CreateVolumeSource(ctx, row); err != nil {
				return fmt.Errorf("manifest: creating volume source %q: %w", v.Volume, err)
			}
			if row.SourceKind == "inline" {
				if err := r.s.store.SetVolSourceFiles(ctx, row.ID, files); err != nil {
					return fmt.Errorf("manifest: storing files for %q: %w", v.Volume, err)
				}
			}
			if row.AutoSync && row.WebhookSecretEnc == "" {
				secret, err := randomToken()
				if err != nil {
					return err
				}
				sealed, err := r.s.sealer.Seal(secret)
				if err != nil {
					return err
				}
				row.WebhookSecretEnc = sealed
				if err := r.s.store.UpdateVolumeSource(ctx, row); err != nil {
					return fmt.Errorf("manifest: storing webhook secret for %q: %w", v.Volume, err)
				}
			}
			res.ID = row.ID
			r.auditEnsure(ctx, manifest.KindVolumeSource, envID, v.Volume, verdictCreate)
			go func(vv store.VolumeSource) {
				vv.SyncedHash = ""
				_ = r.s.reportVolumeSourceSync(context.WithoutCancel(ctx), &vv)
			}(*row)
		}
	case err != nil:
		return err
	default:
		res.ID = existing.ID
		want := *existing
		want.UID, want.GID, want.StackID = v.UID, v.GID, stackID
		want.RestartTargets = strings.TrimSpace(v.RestartTargets)
		applyVolumeSourceSpec(&want, v, credID)

		filesChanged := false
		if want.SourceKind == "inline" {
			cur, err := r.s.store.VolSourceFiles(ctx, existing.ID)
			if err != nil {
				return err
			}
			filesChanged = !volFilesEqual(cur, files)
		}
		if want.SourceKind == existing.SourceKind && want.GitURL == existing.GitURL &&
			want.GitRef == existing.GitRef && want.GitPath == existing.GitPath &&
			want.GitCredentialID == existing.GitCredentialID && want.AutoSync == existing.AutoSync &&
			want.UID == existing.UID && want.GID == existing.GID &&
			want.StackID == existing.StackID && want.RestartTargets == existing.RestartTargets &&
			!filesChanged {
			res.Verdict = verdictInSync
			break
		}
		res.Verdict = verdictUpdate
		if r.apply {
			if err := r.s.store.UpdateVolumeSource(ctx, &want); err != nil {
				return fmt.Errorf("manifest: updating volume source %q: %w", v.Volume, err)
			}
			if want.SourceKind == "inline" {
				if err := r.s.store.SetVolSourceFiles(ctx, want.ID, files); err != nil {
					return fmt.Errorf("manifest: storing files for %q: %w", v.Volume, err)
				}
			}
			r.auditEnsure(ctx, manifest.KindVolumeSource, envID, v.Volume, verdictUpdate)
			go func(vv store.VolumeSource) {
				vv.SyncedHash = ""
				_ = r.s.reportVolumeSourceSync(context.WithoutCancel(ctx), &vv)
			}(want)
		}
	}
	r.push(res)
	return nil
}

func applyVolumeSourceSpec(row *store.VolumeSource, v manifest.VolumeSource, credID string) {
	if v.Source.Git != nil {
		row.SourceKind = "git"
		row.GitURL = strings.TrimSpace(v.Source.Git.URL)
		row.GitRef = strings.TrimSpace(v.Source.Git.Ref)
		row.GitPath = strings.TrimSpace(v.Source.Git.Path)
		row.GitCredentialID = credID
		row.AutoSync = v.AutoSync
	} else {
		row.SourceKind = "inline"
		// An inline source has no repository: git fields and the push-driven webhook
		// are meaningless, same rule as applyVolumeSourceRequest.
		row.GitURL, row.GitRef, row.GitPath, row.GitCredentialID = "", "", "", ""
		row.AutoSync = false
	}
}

// ── comparisons ─────────────────────────────────────────────────────────────────────

func certSetEqual(a, b []store.DeliveryCert) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(c store.DeliveryCert) string { return fmt.Sprintf("%s|%t", c.CertID, c.IsDefault) }
	as, bs := make([]string, len(a)), make([]string, len(b))
	for i := range a {
		as[i] = key(a[i])
	}
	for i := range b {
		bs[i] = key(b[i])
	}
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func stringSetEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func volFilesEqual(a []store.VolSourceFile, b []store.VolSourceFile) bool {
	if len(a) != len(b) {
		return false
	}
	byPath := map[string]store.VolSourceFile{}
	for _, f := range a {
		byPath[f.Path] = f
	}
	for _, f := range b {
		cur, ok := byPath[f.Path]
		if !ok || cur.Content != f.Content {
			return false
		}
	}
	return true
}

// ── history ─────────────────────────────────────────────────────────────────────────

type manifestApplyView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	DocHash   string `json:"doc_hash"`
	AppliedBy string `json:"applied_by"`
	AppliedAt string `json:"applied_at"`
	DryRun    bool   `json:"dry_run"`
	// Document and Report ride only on the detail view; the list stays light. Report
	// is the typed shape rather than raw stored JSON so the generated client tells
	// the truth about what is on the wire.
	Document string          `json:"document,omitempty"`
	Report   *manifestReport `json:"report,omitempty"`
}

func viewManifestApply(m *store.ManifestApply, full bool) manifestApplyView {
	v := manifestApplyView{
		ID: m.ID, Name: m.Name, DocHash: m.DocHash,
		AppliedBy: m.AppliedBy, AppliedAt: m.AppliedAt.Format(time.RFC3339),
		DryRun: m.DryRun,
	}
	if full {
		v.Document = m.Document
		var rep manifestReport
		// We wrote this JSON; a row it cannot parse is a row without a report, not an error.
		if err := json.Unmarshal([]byte(m.Report), &rep); err == nil {
			v.Report = &rep
		}
	}
	return v
}

func (s *Server) handleListManifestApplies(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListManifestApplies(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	out := make([]manifestApplyView, 0, len(list))
	for _, m := range list {
		out = append(out, viewManifestApply(m, false))
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (s *Server) handleGetManifestApply(w http.ResponseWriter, r *http.Request) {
	m, err := s.store.ManifestApplyByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		httpx.Fail(w, r, http.StatusNotFound, "no_such_apply", "No such manifest apply.")
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, viewManifestApply(m, true))
}
