// Package manifest defines the declarative provisioning document: one YAML file
// declaring resources that should exist on a Daffa server, consumed by the manifest
// plan/apply endpoints and by `daffa plan` / `daffa apply`. See docs/provisioning.md
// for the feature's design.
//
// Three properties of the format are load-bearing, and the schema enforces all three
// by shape rather than by convention:
//
//   - It is dumb YAML. No templating, no variables, no ids — resources are declared
//     and cross-referenced by NAME, and the server resolves names against its store
//     at plan time. Generation (values files, naming conventions, loops) belongs to
//     whatever produces the document, never to Daffa.
//
//   - No secret can appear in it. Every secret-bearing field is a SecretRef — a
//     mapping that can only say where a value comes from (`value_from_env`), never
//     carry one. A literal string where a SecretRef belongs is a type error at parse
//     time, not a lint warning. This is what makes an applied document safe to store
//     verbatim and a manifest safe to commit.
//
//   - It is partial by design. A document declaring two stacks and one certificate
//     is complete and valid. References that nothing in the document declares (a CA
//     that already exists on the server, say) are not errors here — the reconciler
//     resolves them against the store and reports `blocked` when they are absent.
package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"

	"go.yaml.in/yaml/v3"
)

// Manifest is the whole document. Every section is optional; unknown fields are
// rejected, so a typo fails the parse instead of silently declaring nothing.
type Manifest struct {
	// Version is the schema version. 1 is the only one.
	Version int `yaml:"version"`

	// Name labels the document in the apply history. It is not an identity: two
	// documents may share a name, and renaming one abandons nothing.
	Name string `yaml:"name"`

	// Cluster is the default cluster (an environment name) for the env-scoped
	// resources below. Any of them may override it with its own `cluster:` key.
	// The cluster must already exist — a manifest cannot create one.
	Cluster string `yaml:"cluster"`

	SSHKeys           []SSHKey          `yaml:"ssh_keys"`
	Registries        []Registry        `yaml:"registries"`
	GitCredentials    []GitCredential   `yaml:"git_credentials"`
	Networks          []Network         `yaml:"networks"`
	CAs               []CA              `yaml:"cas"`
	Certificates      []Certificate     `yaml:"certificates"`
	Keyrings          []Keyring         `yaml:"keyrings"`
	CertDeliveries    []CertDelivery    `yaml:"cert_deliveries"`
	KeyringDeliveries []KeyringDelivery `yaml:"keyring_deliveries"`
	GeneratedSecrets  []GeneratedSecret `yaml:"generated_secrets"`
	Stacks            []Stack           `yaml:"stacks"`
	VolumeSources     []VolumeSource    `yaml:"volume_sources"`
}

// GeneratedSecret declares a value Daffa GENERATES at first apply, seals under the
// master key, and injects into every slot that references it — one value reaching N
// stacks without a human typing it identically N times. Two rules are load-bearing:
//
//   - Generated ONCE, never again. A re-apply reads the stored value back; silently
//     regenerating would take every consumer down in undefined order. Rotation is a
//     deliberate future verb, not an apply side effect.
//   - A declared secret nobody references is a validation error. Every generated
//     secret has owners — the stacks whose slots consume it — and its creation is
//     authorized by exactly their stacks.edit capabilities.
//
// Names are GLOBAL, like every credential store in Daffa: a generator emitting
// per-cluster manifests namespaces them itself (amany-<cluster>-db-password).
type GeneratedSecret struct {
	Name   string `yaml:"name"`
	Format string `yaml:"format"` // alphanumeric (default) | hex | base64
	Length int    `yaml:"length"` // characters; 0 = 32
}

// SecretRef is a slot for a secret value. The document can say where the value comes
// from — a variable in the environment of whoever SUBMITS the document — but it
// cannot carry one: SecretRef has no value field on purpose, and because it decodes
// as a mapping, writing `password: hunter2` is a parse error rather than a leak.
//
// The CLI resolves ValueFromEnv from its own environment and sends the values
// BESIDE the document, keyed by variable name, so the stored document stays
// byte-identical to what was written.
type SecretRef struct {
	ValueFromEnv string `yaml:"value_from_env"`
}

// SSHKey declares a keypair to generate if missing. Generate-only: importing a
// private key would put secret material in the document, so it has no import shape.
type SSHKey struct {
	Name string `yaml:"name"`
	Algo string `yaml:"algo"` // empty = server default
}

// Registry declares a registry credential. The password is create-only in the API
// (there is no update route), so a missing registry with no resolvable password is
// reported `blocked` rather than created as a husk that deploys would trip over.
type Registry struct {
	Name     string     `yaml:"name"`
	URL      string     `yaml:"url"`
	Username string     `yaml:"username"`
	Password *SecretRef `yaml:"password"`
}

// GitCredential declares a git credential: a token, or a reference to an SSH key.
type GitCredential struct {
	Name     string     `yaml:"name"`
	Kind     string     `yaml:"kind"` // token | ssh
	Username string     `yaml:"username"`
	Token    *SecretRef `yaml:"token"`    // token kind only
	SSHKey   string     `yaml:"ssh_key"`  // ssh kind only: an ssh_keys name
	HostKey  string     `yaml:"host_key"` // optional pin; public material
}

// Network declares a Docker network on a cluster. Existing networks whose options
// differ are reported as drift — Docker cannot mutate them in place, and recreating
// one under live services is not a thing apply should ever do.
type Network struct {
	Cluster    string `yaml:"cluster"`
	Name       string `yaml:"name"`
	Driver     string `yaml:"driver"` // empty = overlay
	Attachable bool   `yaml:"attachable"`
}

// CA declares a certificate authority. Trust material is never rotated by apply: an
// existing CA whose parameters differ is reported as drift, untouched.
type CA struct {
	Name       string `yaml:"name"`
	CommonName string `yaml:"common_name"`
	Org        string `yaml:"org"`
	KeyAlgo    string `yaml:"key_algo"` // empty = server default
	Days       int    `yaml:"days"`     // 0 = server default
	// OutboundTrust is a pointer because absent must mean the API's default (true —
	// Daffa's own outbound TLS trusts the CA), and a plain bool cannot say "absent".
	OutboundTrust *bool `yaml:"outbound_trust"`
}

// Certificate declares a leaf certificate issued by a named CA. Like CAs, an
// existing certificate is never re-issued by apply.
type Certificate struct {
	Name    string `yaml:"name"`
	Cluster string `yaml:"cluster"`
	// Shared makes the certificate env-less — usable by every cluster. Mutually
	// exclusive with Cluster, and immutable once created, like the API's env scope.
	Shared          bool     `yaml:"shared"`
	CA              string   `yaml:"ca"`
	SANs            []string `yaml:"sans"`
	Usages          []string `yaml:"usages"` // server, client; empty = server default
	KeyAlgo         string   `yaml:"key_algo"`
	ValidityDays    int      `yaml:"validity_days"`
	RenewBeforeDays int      `yaml:"renew_before_days"`
}

// Keyring declares a symmetric encryption keyring.
type Keyring struct {
	Name       string `yaml:"name"`
	RotateDays int    `yaml:"rotate_days"`
}

// DeliveryCert names one certificate a delivery carries.
type DeliveryCert struct {
	Name    string `yaml:"name"`
	Default bool   `yaml:"default"`
}

// CertDelivery declares a certificate delivery into a volume. A delivery with no
// certs and only bundle_cas is legal — that is how a trust-only volume (CA bundle
// for outbound verification) is expressed.
type CertDelivery struct {
	Cluster        string         `yaml:"cluster"`
	Volume         string         `yaml:"volume"`
	Certs          []DeliveryCert `yaml:"certs"`
	MountPath      string         `yaml:"mount_path"`
	UID            int            `yaml:"uid"`
	GID            int            `yaml:"gid"`
	Traefik        bool           `yaml:"traefik"`
	RestartTargets string         `yaml:"restart_targets"`
	BundleCAs      []string       `yaml:"bundle_cas"`
}

// KeyringDelivery declares a keyring delivery into a volume.
type KeyringDelivery struct {
	Keyring        string `yaml:"keyring"`
	Cluster        string `yaml:"cluster"`
	Volume         string `yaml:"volume"`
	UID            int    `yaml:"uid"`
	GID            int    `yaml:"gid"`
	RestartTargets string `yaml:"restart_targets"`
}

// GitSource points at compose or volume content in a git repository.
type GitSource struct {
	URL        string `yaml:"url"`
	Ref        string `yaml:"ref"`        // empty = the remote's default branch
	Path       string `yaml:"path"`       // empty = repository root
	Credential string `yaml:"credential"` // a git_credentials name; empty = public
}

// StackSource is a stack's compose source: a git location or an inline compose
// file, exactly one.
type StackSource struct {
	Git     *GitSource `yaml:"git"`
	Compose string     `yaml:"compose"`
}

// EnvVar declares one stack environment variable. The legal shapes:
//
//	{key: LOG_LEVEL, value: info}                     plaintext, inline
//	{key: DB_PASSWORD, secret: true}                  slot: created empty, reported unfilled
//	{key: DB_PASSWORD, secret: true,
//	 value_from_env: DB_PASSWORD}                     slot the submitter fills
//	{key: DB_PASSWORD, secret: true,
//	 from_generated: db-password}                     injected from a generated secret
//	{key: DSN, secret: true, value:
//	 "postgres://app:${generated:db-password}@db/x"}  composed: the structure stays
//	                                                  reviewable, only the reference
//	                                                  is secret
//
// A secret with a literal value that references NO generated secret is a validation
// error — that is the doctrine, not a style preference. Generated-backed slots are
// OWNED by the manifest: apply converges them to the stored value, unlike plain
// slots, which are never overwritten once a human fills them.
type EnvVar struct {
	Key           string `yaml:"key"`
	Value         string `yaml:"value"`
	Secret        bool   `yaml:"secret"`
	ValueFromEnv  string `yaml:"value_from_env"`
	FromGenerated string `yaml:"from_generated"`
}

// Stack declares a stack REGISTRATION: source, watch paths, env and secret slots.
// Apply never deploys — deploying is a runtime action with its own concurrency
// guard, driven separately (the CLI's --deploy walks stacks in document order).
type Stack struct {
	Name       string      `yaml:"name"`
	Cluster    string      `yaml:"cluster"`
	Engine     string      `yaml:"engine"` // compose | swarm; immutable once created
	Source     StackSource `yaml:"source"`
	WatchPaths []string    `yaml:"watch_paths"`
	AutoDeploy bool        `yaml:"auto_deploy"`
	Env        []EnvVar    `yaml:"env"`
	// SecretFiles are file-shaped secrets (compose `secrets:` files). A bare name is
	// a slot, filled through the UI or API after apply; from_generated and composed
	// value forms are owned and injected, like the EnvVar shapes.
	SecretFiles []SecretFile `yaml:"secret_files"`
}

// SecretFile is one stack secret file. Three shapes, mirroring EnvVar:
//
//	db_password                                        slot (bare string)
//	{name: db_password, from_generated: db-password}   injected
//	{name: database_url, value:
//	 "postgres://app:${generated:db-password}@db/x"}   composed
//
// A value that references no generated secret is refused — it would put a literal
// secret in the document.
type SecretFile struct {
	Name          string `yaml:"name"`
	FromGenerated string `yaml:"from_generated"`
	Value         string `yaml:"value"`
}

// UnmarshalYAML accepts the bare-string slot form alongside the mapping forms, and
// keeps the mapping strict by hand — yaml.Node.Decode has no KnownFields, and losing
// typo detection on exactly the secret-bearing type would be the wrong place to relax.
func (s *SecretFile) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		s.Name = node.Value
		return nil
	}
	var raw map[string]string
	if err := node.Decode(&raw); err != nil {
		return err
	}
	for k := range raw {
		switch k {
		case "name", "from_generated", "value":
		default:
			return fmt.Errorf("secret file: unknown field %q", k)
		}
	}
	s.Name, s.FromGenerated, s.Value = raw["name"], raw["from_generated"], raw["value"]
	return nil
}

// File is one inline volume-source file.
type File struct {
	Path    string `yaml:"path"`
	Content string `yaml:"content"`
}

// VolumeSourceSpec is a volume source's content: a git location or inline files,
// exactly one.
type VolumeSourceSpec struct {
	Git   *GitSource `yaml:"git"`
	Files []File     `yaml:"files"`
}

// VolumeSource declares managed content for a volume.
type VolumeSource struct {
	Cluster        string           `yaml:"cluster"`
	Volume         string           `yaml:"volume"`
	Source         VolumeSourceSpec `yaml:"source"`
	Stack          string           `yaml:"stack"` // linked stack, by name
	RestartTargets string           `yaml:"restart_targets"`
	UID            int              `yaml:"uid"`
	GID            int              `yaml:"gid"`
	AutoSync       bool             `yaml:"auto_sync"`
}

// Parse decodes a manifest strictly: unknown fields are errors, and so is a second
// YAML document in the stream. It does not validate — call Validate, so a caller
// that wants to report every problem at once gets them all, not just the first.
func Parse(doc []byte) (*Manifest, error) {
	dec := yaml.NewDecoder(bytes.NewReader(doc))
	dec.KnownFields(true)

	var m Manifest
	if err := dec.Decode(&m); errors.Is(err, io.EOF) {
		return nil, errors.New("manifest: the document is empty")
	} else if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}

	var extra any
	if err := dec.Decode(&extra); err == nil {
		return nil, errors.New("manifest: a manifest is one YAML document; found a second")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	return &m, nil
}

// Hash is the document's identity in the apply history: the hash of the BYTES as
// submitted, not of the parsed form, so history answers "was this exact file
// applied?" — the question an operator diffing an incident actually has.
func Hash(doc []byte) string {
	sum := sha256.Sum256(doc)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// generatedRef matches ${generated:<name>} — the ONE substitution form the manifest
// allows, legal only inside secret values. It exists because the alternative is worse:
// either a composed value like a DSN becomes an opaque human-typed secret (structure
// unreviewable, typos invisible until a deploy fails), or composition happens outside
// Daffa and the secret leaks into a committed file.
var generatedRef = regexp.MustCompile(`\$\{generated:([A-Za-z0-9][A-Za-z0-9_-]*)\}`)

// GeneratedRefs returns the generated-secret names a value references, in order.
func GeneratedRefs(s string) []string {
	var out []string
	for _, m := range generatedRef.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

// ExpandGenerated substitutes every ${generated:name} via resolve. ok is false when
// any reference could not be resolved — the caller must not use a half-expanded value.
func ExpandGenerated(s string, resolve func(name string) (string, bool)) (string, bool) {
	ok := true
	out := generatedRef.ReplaceAllStringFunc(s, func(ref string) string {
		name := generatedRef.FindStringSubmatch(ref)[1]
		v, found := resolve(name)
		if !found {
			ok = false
			return ref
		}
		return v
	})
	return out, ok
}
