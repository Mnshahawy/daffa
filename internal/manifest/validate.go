package manifest

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// The name rules mirror the imperative API's, so a manifest never passes validation
// only to be refused resource-by-resource at apply time:
//
//   - stack names mirror api.validProjectName (the name becomes the compose project
//     name, and compose is the one that ultimately accepts or rejects it);
//   - certificate, CA, keyring and secret-file names mirror the API's cert-name rule
//     (they become file names on delivery volumes);
//   - network and volume names are what the Docker daemon accepts for local names;
//   - env var names are POSIX, for both the key and value_from_env.
var (
	certLikeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
	dockerName   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	envVarName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func validProjectName(n string) bool {
	if n == "" || len(n) > 63 {
		return false
	}
	for i, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}

// validLabel is the rule for names that are labels rather than file or project
// names (registries, git credentials, SSH keys, clusters): anything readable, no
// leading/trailing space, one line.
func validLabel(n string) bool {
	return n != "" && len(n) <= 128 &&
		strings.TrimSpace(n) == n && !strings.ContainsAny(n, "\r\n")
}

// Validate reports every problem in the document at once, joined — a manifest
// author fixing a file wants the full list, not one error per round-trip.
func (m *Manifest) Validate() error {
	var errs []error
	fail := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("manifest: "+format, a...))
	}

	if m.Version != 1 {
		fail("version: 1 is required (got %d)", m.Version)
	}
	if m.Name != "" && !validLabel(m.Name) {
		fail("name: must be a single line of at most 128 characters")
	}
	if m.Cluster != "" && !validLabel(m.Cluster) {
		fail("cluster: %q is not a valid cluster name", m.Cluster)
	}

	// needsCluster enforces that every env-scoped resource lands somewhere: its own
	// cluster, or the manifest default.
	needsCluster := func(kind Kind, name, cluster string) {
		if cluster == "" && m.Cluster == "" {
			fail("%s %q: no cluster — set the manifest-level cluster or one on the resource", kind, name)
		}
		if cluster != "" && !validLabel(cluster) {
			fail("%s %q: cluster %q is not a valid cluster name", kind, name, cluster)
		}
	}
	// dup tracks uniqueness within one kind. The key includes the scope for
	// env-scoped kinds, so the same stack name on two clusters stays legal.
	seen := map[string]bool{}
	dup := func(kind Kind, scope, name string) {
		k := string(kind) + "\x00" + scope + "\x00" + name
		if seen[k] {
			fail("%s %q: declared twice", kind, name)
		}
		seen[k] = true
	}
	secretRef := func(kind Kind, name, field string, ref *SecretRef) {
		if ref != nil && ref.ValueFromEnv != "" && !envVarName.MatchString(ref.ValueFromEnv) {
			fail("%s %q: %s: value_from_env %q is not a valid environment variable name", kind, name, field, ref.ValueFromEnv)
		}
	}

	for _, k := range m.SSHKeys {
		if !validLabel(k.Name) {
			fail("%s %q: invalid name", KindSSHKey, k.Name)
		}
		dup(KindSSHKey, "", k.Name)
	}

	for _, reg := range m.Registries {
		if !validLabel(reg.Name) {
			fail("%s %q: invalid name", KindRegistry, reg.Name)
		}
		dup(KindRegistry, "", reg.Name)
		if reg.URL == "" {
			fail("%s %q: url is required", KindRegistry, reg.Name)
		}
		secretRef(KindRegistry, reg.Name, "password", reg.Password)
	}

	for _, gc := range m.GitCredentials {
		if !validLabel(gc.Name) {
			fail("%s %q: invalid name", KindGitCredential, gc.Name)
		}
		dup(KindGitCredential, "", gc.Name)
		switch gc.Kind {
		case "token":
			if gc.SSHKey != "" {
				fail("%s %q: ssh_key belongs to kind ssh, not token", KindGitCredential, gc.Name)
			}
		case "ssh":
			if gc.Token != nil {
				fail("%s %q: token belongs to kind token, not ssh", KindGitCredential, gc.Name)
			}
			if gc.SSHKey == "" {
				fail("%s %q: kind ssh requires ssh_key", KindGitCredential, gc.Name)
			}
		default:
			fail("%s %q: kind must be token or ssh (got %q)", KindGitCredential, gc.Name, gc.Kind)
		}
		secretRef(KindGitCredential, gc.Name, "token", gc.Token)
	}

	for _, n := range m.Networks {
		if !dockerName.MatchString(n.Name) {
			fail("%s %q: invalid name", KindNetwork, n.Name)
		}
		needsCluster(KindNetwork, n.Name, n.Cluster)
		dup(KindNetwork, n.Cluster, n.Name)
	}

	for _, ca := range m.CAs {
		if !certLikeName.MatchString(ca.Name) {
			fail("%s %q: invalid name", KindCA, ca.Name)
		}
		dup(KindCA, "", ca.Name)
		if strings.TrimSpace(ca.CommonName) == "" {
			fail("%s %q: common_name is required — it is what the CA calls itself in every chain it signs", KindCA, ca.Name)
		}
		if ca.Days < 0 {
			fail("%s %q: days cannot be negative", KindCA, ca.Name)
		}
	}

	for _, c := range m.Certificates {
		if !certLikeName.MatchString(c.Name) {
			fail("%s %q: invalid name", KindCertificate, c.Name)
		}
		// A shared certificate is env-less; the scope key below keeps a shared
		// "api" and a cluster-scoped "api" distinct, like the store's unique index.
		scope := c.Cluster
		switch {
		case c.Shared && c.Cluster != "":
			fail("%s %q: shared and cluster are mutually exclusive", KindCertificate, c.Name)
		case c.Shared:
			scope = "\x00shared"
		default:
			needsCluster(KindCertificate, c.Name, c.Cluster)
		}
		dup(KindCertificate, scope, c.Name)
		if c.CA == "" {
			fail("%s %q: ca is required", KindCertificate, c.Name)
		}
		if len(c.SANs) == 0 {
			fail("%s %q: at least one SAN is required", KindCertificate, c.Name)
		}
		for _, u := range c.Usages {
			if u != "server" && u != "client" {
				fail("%s %q: usage must be server or client (got %q)", KindCertificate, c.Name, u)
			}
		}
		if c.ValidityDays < 0 || c.RenewBeforeDays < 0 {
			fail("%s %q: validity_days and renew_before_days cannot be negative", KindCertificate, c.Name)
		}
	}

	for _, kr := range m.Keyrings {
		if !certLikeName.MatchString(kr.Name) {
			fail("%s %q: invalid name", KindKeyring, kr.Name)
		}
		dup(KindKeyring, "", kr.Name)
		if kr.RotateDays < 0 {
			fail("%s %q: rotate_days cannot be negative", KindKeyring, kr.Name)
		}
	}

	for _, d := range m.CertDeliveries {
		if !dockerName.MatchString(d.Volume) {
			fail("%s %q: invalid volume name", KindCertDelivery, d.Volume)
		}
		needsCluster(KindCertDelivery, d.Volume, d.Cluster)
		dup(KindCertDelivery, d.Cluster, d.Volume)
		if len(d.Certs) == 0 && len(d.BundleCAs) == 0 {
			fail("%s %q: declares neither certs nor bundle_cas — an empty delivery delivers nothing", KindCertDelivery, d.Volume)
		}
		defaults := 0
		for _, c := range d.Certs {
			if c.Name == "" {
				fail("%s %q: a cert entry has no name", KindCertDelivery, d.Volume)
			}
			if c.Default {
				defaults++
			}
		}
		if defaults > 1 {
			fail("%s %q: at most one cert can be the default", KindCertDelivery, d.Volume)
		}
	}

	for _, d := range m.KeyringDeliveries {
		if !dockerName.MatchString(d.Volume) {
			fail("%s %q: invalid volume name", KindKeyringDelivery, d.Volume)
		}
		needsCluster(KindKeyringDelivery, d.Volume, d.Cluster)
		// Keyed on the KEYRING too: several keyrings legitimately deliver into one
		// shared volume (each writes its own <name>.json), and the store's unique key
		// is (keyring, env, volume). Only the same keyring into the same volume twice
		// is a duplicate.
		dup(KindKeyringDelivery, d.Cluster+"\x00"+d.Keyring, d.Volume)
		if d.Keyring == "" {
			fail("%s %q: keyring is required", KindKeyringDelivery, d.Volume)
		}
	}

	for _, st := range m.Stacks {
		if !validProjectName(st.Name) {
			fail("%s %q: invalid name — it becomes the compose project name (lowercase letters, digits, - and _)", KindStack, st.Name)
		}
		needsCluster(KindStack, st.Name, st.Cluster)
		dup(KindStack, st.Cluster, st.Name)
		if st.Engine != "compose" && st.Engine != "swarm" {
			fail("%s %q: engine must be compose or swarm (got %q) — it is immutable, so the document says it out loud", KindStack, st.Name, st.Engine)
		}
		validateStackSource(fail, st)
		envSeen := map[string]bool{}
		for _, e := range st.Env {
			if !envVarName.MatchString(e.Key) {
				fail("%s %q: env key %q is not a valid environment variable name", KindStack, st.Name, e.Key)
			}
			if envSeen[e.Key] {
				fail("%s %q: env key %q declared twice", KindStack, st.Name, e.Key)
			}
			envSeen[e.Key] = true
			if e.Secret && e.Value != "" {
				fail("%s %q: env %q: a secret cannot carry a literal value — leave it a slot, or use value_from_env", KindStack, st.Name, e.Key)
			}
			if !e.Secret && e.ValueFromEnv != "" {
				fail("%s %q: env %q: value_from_env implies secret: true", KindStack, st.Name, e.Key)
			}
			if e.ValueFromEnv != "" && !envVarName.MatchString(e.ValueFromEnv) {
				fail("%s %q: env %q: value_from_env %q is not a valid environment variable name", KindStack, st.Name, e.Key, e.ValueFromEnv)
			}
		}
		// File-shaped secrets become Swarm raft secrets; on a compose stack the file
		// could never mount (the bundle exists only inside the runner container), so
		// the API refuses them there — catch it here, where the fix is one line away.
		if len(st.SecretFiles) > 0 && st.Engine != "swarm" {
			fail("%s %q: secret_files are a Swarm feature — on a compose stack use secret env vars instead", KindStack, st.Name)
		}
		fileSeen := map[string]bool{}
		for _, name := range st.SecretFiles {
			if !certLikeName.MatchString(name) {
				fail("%s %q: secret file %q: invalid name — it becomes a file name", KindStack, st.Name, name)
			}
			if fileSeen[name] {
				fail("%s %q: secret file %q declared twice", KindStack, st.Name, name)
			}
			fileSeen[name] = true
		}
	}

	for _, v := range m.VolumeSources {
		if !dockerName.MatchString(v.Volume) {
			fail("%s %q: invalid volume name", KindVolumeSource, v.Volume)
		}
		needsCluster(KindVolumeSource, v.Volume, v.Cluster)
		dup(KindVolumeSource, v.Cluster, v.Volume)
		switch {
		case v.Source.Git != nil && len(v.Source.Files) > 0:
			fail("%s %q: source is git or inline files, not both", KindVolumeSource, v.Volume)
		case v.Source.Git == nil && len(v.Source.Files) == 0:
			fail("%s %q: source needs git or inline files", KindVolumeSource, v.Volume)
		case v.Source.Git != nil:
			validateGitSource(fail, KindVolumeSource, v.Volume, v.Source.Git)
		default:
			pathSeen := map[string]bool{}
			for _, f := range v.Source.Files {
				// Delivered files land inside the volume; a path that climbs out of
				// it is an escape, refused here rather than trusted to the deliverer.
				clean := path.Clean(f.Path)
				if f.Path == "" || path.IsAbs(f.Path) || clean == ".." || strings.HasPrefix(clean, "../") {
					fail("%s %q: file path %q must be relative and stay inside the volume", KindVolumeSource, v.Volume, f.Path)
					continue
				}
				if pathSeen[clean] {
					fail("%s %q: file path %q declared twice", KindVolumeSource, v.Volume, f.Path)
				}
				pathSeen[clean] = true
			}
		}
	}

	return errors.Join(errs...)
}

func validateStackSource(fail func(string, ...any), st Stack) {
	switch {
	case st.Source.Git != nil && st.Source.Compose != "":
		fail("%s %q: source is git or inline compose, not both", KindStack, st.Name)
	case st.Source.Git == nil && st.Source.Compose == "":
		fail("%s %q: source needs git or inline compose", KindStack, st.Name)
	case st.Source.Git != nil:
		validateGitSource(fail, KindStack, st.Name, st.Source.Git)
	}
}

func validateGitSource(fail func(string, ...any), kind Kind, name string, g *GitSource) {
	if g.URL == "" {
		fail("%s %q: git source needs a url", kind, name)
	}
}
