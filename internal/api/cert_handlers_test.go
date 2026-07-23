package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mnshahawy/daffa/internal/certs"
	"github.com/Mnshahawy/daffa/internal/config"
	"github.com/Mnshahawy/daffa/internal/dockerx"
	"github.com/Mnshahawy/daffa/internal/notify"
	"github.com/Mnshahawy/daffa/internal/store"
)

// The whole CA lifecycle, end to end through the handlers: create a CA, issue a leaf,
// stage a rotation, watch the bundle carry both roots, activate, and confirm the leaf was
// re-signed under the new root without its key changing. This is the state machine
// docs/certs.md promises, exercised as HTTP.
func TestCARotationLifecycle(t *testing.T) {
	s, ctx := certServer(t)

	// ── create a CA and issue a leaf ────────────────────────────────────────────
	ca := postJSON[caView](t, s.handleCreateCA, "POST", "/api/certs/cas", nil,
		`{"name":"internal-ca","common_name":"Example Internal CA","org":"Example"}`, http.StatusOK)
	if !ca.CanSign || ca.Status != "active" {
		t.Fatalf("created CA: %+v", ca)
	}

	leaf := postJSON[certView](t, s.handleCreateCertificate, "POST", "/api/certs", nil,
		`{"name":"web-frontend","ca_id":"`+ca.ID+`","sans":["app.example.com","www.example.com"]}`, http.StatusOK)
	if len(leaf.SANs) != 2 || leaf.CAID != ca.ID {
		t.Fatalf("issued leaf: %+v", leaf)
	}

	stored, err := s.store.CertificateByID(ctx, leaf.ID)
	if err != nil {
		t.Fatal(err)
	}
	keyBefore, err := s.sealer.Open(stored.KeyEnc)
	if err != nil {
		t.Fatal(err)
	}
	if err := certs.Verify(stored.CertPEM, mustCA(t, s, ctx, ca.ID).CertPEM); err != nil {
		t.Fatalf("the issued leaf does not verify against its CA: %v", err)
	}

	// A CA with leaves refuses deletion.
	rec := call(s.handleDeleteCA, "DELETE", "/api/certs/cas/"+ca.ID, map[string]string{"id": ca.ID}, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("deleting a CA with live leaves returned %d; it must refuse", rec.Code)
	}

	// ── phase 1: rotate ─────────────────────────────────────────────────────────
	next := postJSON[caView](t, s.handleRotateCA, "POST", "/api/certs/cas/"+ca.ID+"/rotate",
		map[string]string{"id": ca.ID}, `{"overlap_days":30}`, http.StatusOK)
	if next.Status != "next" || next.RotatesID != ca.ID || next.Overlap == nil {
		t.Fatalf("staged CA: %+v", next)
	}

	// A second rotate while one is staged is refused — internal-ca.sh's guard.
	rec = call(s.handleRotateCA, "POST", "/api/certs/cas/"+ca.ID+"/rotate",
		map[string]string{"id": ca.ID}, `{}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("a second rotate returned %d; want 409", rec.Code)
	}

	// During overlap the bundle carries BOTH roots, so distribution can begin while
	// nothing has changed for the leaves.
	bundle, err := s.trustBundle(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := certs.ParseCerts(bundle); len(got) != 2 {
		t.Fatalf("the overlap bundle holds %d roots; want 2", len(got))
	}
	if err := certs.Verify(stored.CertPEM, bundle); err != nil {
		t.Fatalf("the untouched leaf must verify against the overlap bundle: %v", err)
	}

	// ── phase 2: activate ───────────────────────────────────────────────────────
	// Without confirmation it refuses — activating with an undistributed root is the
	// mistake the two-phase flow exists to prevent.
	rec = call(s.handleActivateCA, "POST", "/api/certs/cas/"+next.ID+"/activate",
		map[string]string{"id": next.ID}, `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("activate without confirm returned %d; want 400", rec.Code)
	}

	promoted := postJSON[caView](t, s.handleActivateCA, "POST", "/api/certs/cas/"+next.ID+"/activate",
		map[string]string{"id": next.ID}, `{"confirm":true}`, http.StatusOK)
	// RotatesID survives promotion as the lineage back-pointer — it is what keeps the
	// retired root inside SELECTED bundles through the overlap tail.
	if promoted.Status != "active" || promoted.RotatesID != ca.ID {
		t.Fatalf("promoted CA: %+v", promoted)
	}
	old := mustCA(t, s, ctx, ca.ID)
	if old.Status != "retired" || old.OverlapUntil.IsZero() {
		t.Fatalf("the incumbent should be retired with an overlap tail: %+v", old)
	}

	// The leaf was re-signed under the new root — with the SAME key, so consumers that
	// mounted the key file never noticed.
	resigned, err := s.store.CertificateByID(ctx, leaf.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resigned.CAID != promoted.ID {
		t.Errorf("the leaf still points at the retired CA")
	}
	if err := certs.Verify(resigned.CertPEM, mustCA(t, s, ctx, promoted.ID).CertPEM); err != nil {
		t.Errorf("the re-signed leaf does not verify against the new root: %v", err)
	}
	keyAfter, err := s.sealer.Open(resigned.KeyEnc)
	if err != nil {
		t.Fatal(err)
	}
	if keyBefore != keyAfter {
		t.Error("activation changed a leaf's private key — renewal must reuse it")
	}
	if err := certs.CheckPair(resigned.CertPEM, keyAfter); err != nil {
		t.Errorf("the re-signed leaf and its key no longer match: %v", err)
	}

	// The retired root stays in the bundle until its overlap tail passes.
	bundle, _ = s.trustBundle(ctx, nil)
	if got, _ := certs.ParseCerts(bundle); len(got) != 2 {
		t.Errorf("right after activation the bundle holds %d roots; want 2 (retired stays through the tail)", len(got))
	}
}

// The generate response is the ONLY place the private half ever exists, and the database
// row it leaves behind must hold nothing that could decrypt a backup.
func TestKeyGenerationNeverStoresThePrivateHalf(t *testing.T) {
	s, ctx := certServer(t)

	rec := call(s.handleCreateKey, "POST", "/api/keys", nil, `{"name":"personal"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("generate: %d %s", rec.Code, rec.Body)
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp["recipient"], "age1") {
		t.Fatalf("recipient: %q", resp["recipient"])
	}
	if !strings.Contains(resp["identity_file"], "AGE-SECRET-KEY-") {
		t.Fatal("the one-time response must carry the identity file")
	}

	// The stored row: public half only.
	k, err := s.store.EncryptionKeyByID(ctx, resp["id"])
	if err != nil {
		t.Fatal(err)
	}
	if k.Recipient != resp["recipient"] || k.Source != "generated" {
		t.Fatalf("stored key: %+v", k)
	}
	// Nothing anywhere in the row smells like a private key. Belt and braces — the
	// struct has no field for one, but a marshaling bug could smuggle it into a name.
	blob, _ := json.Marshal(k)
	if strings.Contains(string(blob), "AGE-SECRET-KEY-") {
		t.Fatal("the private key reached the database")
	}

	// Importing a private key is refused with an error that says PRIVATE.
	rec = call(s.handleCreateKey, "POST", "/api/keys", nil,
		`{"name":"oops","recipient":"AGE-SECRET-KEY-1QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQ"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "PRIVATE") {
		t.Fatalf("importing a private key: %d %s", rec.Code, rec.Body)
	}

	// Importing the public recipient works.
	rec = call(s.handleCreateKey, "POST", "/api/keys", nil,
		`{"name":"break-glass","recipient":"`+resp["recipient"]+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("importing a valid recipient: %d %s", rec.Code, rec.Body)
	}
}

// A delivery that SELECTS its roots must behave through a rotation exactly like one that
// carries them all: the staged successor joins its bundle, activation rewrites the
// selection to the promoted root, and the retired root rides the overlap tail. An
// unrelated CA never leaks in — that isolation is the reason selection exists.
func TestSelectedBundlesFollowLineage(t *testing.T) {
	s, ctx := certServer(t)

	caA := postJSON[caView](t, s.handleCreateCA, "POST", "/api/certs/cas", nil,
		`{"name":"platform-ca","common_name":"Platform CA"}`, http.StatusOK)
	caB := postJSON[caView](t, s.handleCreateCA, "POST", "/api/certs/cas", nil,
		`{"name":"internal-ca","common_name":"Internal CA"}`, http.StatusOK)

	env, _, err := s.store.UpsertLocalEnvironment(ctx, "Local", "unix:///var/run/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	d := &store.CertDelivery{EnvID: env.ID, Volume: "platform-trust", BundleCAs: caA.ID}
	if err := s.store.CreateCertDelivery(ctx, d); err != nil {
		t.Fatal(err)
	}

	roots := func() []string {
		t.Helper()
		files, _, err := s.deliveryFiles(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := certs.ParseCerts(string(files["ca-bundle.crt"]))
		if err != nil {
			t.Fatal(err)
		}
		var cns []string
		for _, c := range parsed {
			cns = append(cns, c.Subject.CommonName)
		}
		return cns
	}

	if got := roots(); len(got) != 1 || got[0] != "Platform CA" {
		t.Fatalf("selected bundle = %v; want the selected root only", got)
	}

	// While a rotation is staged, the successor rides along; the unrelated CA still does not.
	next := postJSON[caView](t, s.handleRotateCA, "POST", "/api/certs/cas/"+caA.ID+"/rotate",
		map[string]string{"id": caA.ID}, `{"overlap_days":30}`, http.StatusOK)
	if got := roots(); len(got) != 2 {
		t.Fatalf("staged bundle = %v; want incumbent + successor", got)
	}

	// A CA still selected by a delivery refuses deletion — same refusal as live leaves.
	rec := call(s.handleDeleteCA, "DELETE", "/api/certs/cas/"+caA.ID, map[string]string{"id": caA.ID}, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("deleting a selected CA returned %d; it must refuse", rec.Code)
	}

	// Activation rewrites the selection to the promoted root and keeps the retired one
	// through its overlap tail.
	postJSON[caView](t, s.handleActivateCA, "POST", "/api/certs/cas/"+next.ID+"/activate",
		map[string]string{"id": next.ID}, `{"confirm":true}`, http.StatusOK)
	d, err = s.store.CertDeliveryByID(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.BundleCAs != next.ID {
		t.Fatalf("activation should rewrite the selection to the promoted root; got %q", d.BundleCAs)
	}
	if got := roots(); len(got) != 2 {
		t.Fatalf("post-activation bundle = %v; want promoted + retired-in-overlap", got)
	}

	// The unrelated root never appeared; a delivery with NO selection carries everything.
	for _, cn := range roots() {
		if cn == "Internal CA" {
			t.Fatal("an unselected CA leaked into a selected bundle")
		}
	}
	all := &store.CertDelivery{EnvID: env.ID, Volume: "everything"}
	if err := s.store.CreateCertDelivery(ctx, all); err != nil {
		t.Fatal(err)
	}
	files, _, err := s.deliveryFiles(ctx, all)
	if err != nil {
		t.Fatal(err)
	}
	if parsed, _ := certs.ParseCerts(string(files["ca-bundle.crt"])); len(parsed) != 3 {
		t.Fatalf("the unselected bundle holds %d roots; want all 3", len(parsed))
	}
	_ = caB
}

// Env scoping and usages, through the handlers: an mTLS leaf carries both EKUs and KEEPS
// them across renewal; the environment is immutable; an env-scoped cert refuses to be
// delivered elsewhere (both at create and at sync time).
func TestCertEnvScopingAndUsages(t *testing.T) {
	s, ctx := certServer(t)

	ca := postJSON[caView](t, s.handleCreateCA, "POST", "/api/certs/cas", nil,
		`{"name":"platform-ca","common_name":"Platform CA"}`, http.StatusOK)
	env, _, err := s.store.UpsertLocalEnvironment(ctx, "Local", "unix:///var/run/docker.sock")
	if err != nil {
		t.Fatal(err)
	}

	leaf := postJSON[certView](t, s.handleCreateCertificate, "POST", "/api/certs", nil,
		`{"name":"cellauth","env_id":"`+env.ID+`","ca_id":"`+ca.ID+`","sans":["cellauth"],"usages":["server","client"]}`,
		http.StatusOK)
	if leaf.EnvID != env.ID || strings.Join(leaf.Usages, " ") != "server client" {
		t.Fatalf("issued leaf: %+v", leaf)
	}

	stored, err := s.store.CertificateByID(ctx, leaf.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parsed, _ := certs.ParseCert(stored.CertPEM); certs.UsagesOf(parsed) != "server client" {
		t.Fatalf("issued PEM usages = %q; want both EKUs", certs.UsagesOf(parsed))
	}

	// Renewal must keep clientAuth — the silent-outage case the usages column exists for.
	postJSON[certView](t, s.handleRenewCertificate, "POST", "/api/certs/"+leaf.ID+"/renew",
		map[string]string{"id": leaf.ID}, `{}`, http.StatusOK)
	stored, _ = s.store.CertificateByID(ctx, leaf.ID)
	if parsed, _ := certs.ParseCert(stored.CertPEM); certs.UsagesOf(parsed) != "server client" {
		t.Fatal("renewal dropped an EKU")
	}

	// The environment is immutable, like the name.
	rec := call(s.handleUpdateCertificate, "PUT", "/api/certs/"+leaf.ID,
		map[string]string{"id": leaf.ID}, `{"env_id":"env_other"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "immutable_env") {
		t.Fatalf("env change: %d %s", rec.Code, rec.Body)
	}

	// Unknown usages are refused with the fix named.
	rec = call(s.handleCreateCertificate, "POST", "/api/certs", nil,
		`{"name":"bad","ca_id":"`+ca.ID+`","sans":["x"],"usages":["codeSigning"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown usage: %d %s", rec.Code, rec.Body)
	}

	// An env-scoped cert refuses delivery into another environment — at sync time here;
	// the create handler refuses the same way before a row ever exists.
	other := &store.Environment{Name: "staging"}
	if err := s.store.CreateEnvironment(ctx, other); err != nil {
		t.Fatal(err)
	}
	wrong := &store.CertDelivery{EnvID: other.ID, CertID: leaf.ID, Volume: "v"}
	if err := s.store.CreateCertDelivery(ctx, wrong); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.deliveryFiles(ctx, wrong); err == nil ||
		!strings.Contains(err.Error(), "another environment") {
		t.Fatalf("cross-env delivery files: err %v; want the environment refusal", err)
	}
}

// A CA with outbound_trust off is bundled and delivered but never joins the pool Daffa's
// own registry/git reach-out verifies against.
func TestOutboundTrustGatesManagedCAs(t *testing.T) {
	s, ctx := certServer(t)

	postJSON[caView](t, s.handleCreateCA, "POST", "/api/certs/cas", nil,
		`{"name":"ours","common_name":"Ours"}`, http.StatusOK)
	theirs := postJSON[caView](t, s.handleCreateCA, "POST", "/api/certs/cas", nil,
		`{"name":"theirs","common_name":"Theirs","outbound_trust":false}`, http.StatusOK)
	if theirs.OutboundTrust {
		t.Fatal("outbound_trust: false was not honoured at create")
	}

	if pems := s.managedCAPEMs(ctx); len(pems) != 1 {
		t.Fatalf("managedCAPEMs returned %d CAs; the outbound_trust=false one must be excluded", len(pems))
	}

	// The flag is editable — the one CA setting that is a setting.
	updated := postJSON[caView](t, s.handleUpdateCA, "PUT", "/api/certs/cas/"+theirs.ID,
		map[string]string{"id": theirs.ID}, `{"outbound_trust":true}`, http.StatusOK)
	if !updated.OutboundTrust {
		t.Fatalf("update did not flip outbound_trust: %+v", updated)
	}
	if pems := s.managedCAPEMs(ctx); len(pems) != 2 {
		t.Fatalf("managedCAPEMs returned %d CAs after the flip; want 2", len(pems))
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────────

func certServer(t *testing.T) (*Server, context.Context) {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	key, err := config.NewMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := config.NewSealer(key)
	if err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.DiscardHandler)
	// The pool is empty but present: handlers and the background resync reach it, and an
	// empty pool answers "not connected" where a nil one would panic a goroutine.
	return &Server{store: st, sealer: sealer, pool: dockerx.NewPool(), notify: notify.New(st, fakeSealer{}, log)}, ctx
}

func call(h http.HandlerFunc, method, path string, pathValues map[string]string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func postJSON[T any](t *testing.T, h http.HandlerFunc, method, path string, pathValues map[string]string, body string, want int) T {
	t.Helper()
	rec := call(h, method, path, pathValues, body)
	if rec.Code != want {
		t.Fatalf("%s %s returned %d: %s", method, path, rec.Code, rec.Body)
	}
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding %s response: %v", path, err)
	}
	return out
}

func mustCA(t *testing.T, s *Server, ctx context.Context, id string) *store.CertAuthority {
	t.Helper()
	ca, err := s.store.CertAuthorityByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// The edge resources the installer provisions (CA, certificate, delivery) are marked
// protected, and protected is refused for deletion — the console cannot be made to delete
// its own TLS with a click. The flag must also survive a store round-trip.
func TestProtectedEdgeResourcesRefuseDeletion(t *testing.T) {
	s, ctx := certServer(t)

	ca := &store.CertAuthority{Name: "daffa-edge-ca", CertPEM: "ca", KeyEnc: "k", Status: "active", Protected: true}
	if err := s.store.CreateCertAuthority(ctx, ca); err != nil {
		t.Fatal(err)
	}
	cert := &store.Certificate{Name: "daffa-edge", CAID: ca.ID, SANs: "daffa.internal", CertPEM: "c", KeyEnc: "k", Protected: true}
	if err := s.store.CreateCertificate(ctx, cert); err != nil {
		t.Fatal(err)
	}
	env, _, err := s.store.UpsertLocalEnvironment(ctx, "Local", "unix:///var/run/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	dlv := &store.CertDelivery{EnvID: env.ID, CertID: cert.ID, Volume: "daffa-edge-certs", Traefik: true, Protected: true}
	if err := s.store.CreateCertDelivery(ctx, dlv); err != nil {
		t.Fatal(err)
	}

	// The flag round-trips rather than being dropped on write/read.
	if got := mustCA(t, s, ctx, ca.ID); !got.Protected {
		t.Fatal("CA.Protected did not survive a store round-trip")
	}

	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
		path string
		id   string
	}{
		{"ca", s.handleDeleteCA, "/api/certs/cas/" + ca.ID, ca.ID},
		{"cert", s.handleDeleteCertificate, "/api/certs/" + cert.ID, cert.ID},
		{"delivery", s.handleDeleteCertDelivery, "/api/certs/deliveries/" + dlv.ID, dlv.ID},
	} {
		rec := call(tc.h, "DELETE", tc.path, map[string]string{"id": tc.id}, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: delete returned %d, want 400", tc.name, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "protected") {
			t.Errorf("%s: refusal body %q lacks 'protected'", tc.name, rec.Body.String())
		}
	}
}
