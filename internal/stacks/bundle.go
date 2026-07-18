// Package stacks turns a stack's declared source into something that can be deployed,
// and runs the deploy.
//
// The shape of the thing is: resolve a source (git or inline) into a BUNDLE — a tar of
// the compose file, a rendered .env, and optionally a docker config with registry
// credentials — then hand that bundle to a runner container that executes
// `docker compose up` against the target daemon.
//
// Daffa never runs compose in its own process. It cannot: it manages the stack it is
// itself part of, and a `compose up` that recreates the server mid-deploy would kill
// the deploy. See runner.go.
package stacks

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

// Bundle is everything the runner needs, as a tar it can be copied into a container.
type Bundle struct {
	Tar  []byte
	Hash string // content hash: identifies exactly this compose file + env
	YAML string // kept for validation and for showing the user what will run

	// Env is the resolved (plaintext) variables, carried alongside the tar because one engine
	// cannot read them from it: `docker stack deploy` has no --env-file and interpolates ${VAR}
	// only from the process environment. See Engine.RunnerEnv.
	//
	// This never reaches the database. Secrets live in stack_envs and in the rendered .env inside
	// the runner; docs/stacks.md §2 forbids writing them to a deployment row, and a Bundle is not
	// one.
	Env []EnvVar
}

// EnvVar is one resolved (plaintext) variable. These exist only in memory, between
// unsealing and writing into the bundle.
type EnvVar struct {
	Key   string
	Value string
}

// Secret is one resolved (plaintext) stack secret, written into the bundle as a file the
// compose secrets: primitive mounts. Like EnvVar it exists only in memory, between unsealing
// and writing the tar — it is never persisted anywhere the tar is not. See docs/secrets.md.
type Secret struct {
	Name    string
	Content string
}

// RegistryAuth is a resolved credential, again only in memory.
type RegistryAuth struct {
	URL      string
	Username string
	Password string
}

const (
	composePath = "docker-compose.yml"
	envPath     = ".env"
	authPath    = "config.json"
	// secretsDir is where stack secrets land in the bundle. A compose secrets: entry sources
	// them with `file: ./daffa-secrets/<name>`; the marker is the convention that lets Daffa
	// tell its own managed files from the operator's own repo files. See docs/secrets.md.
	secretsDir = "daffa-secrets"
)

// DaffaSecretRef reports whether a compose secret's `file:` source points into Daffa's
// managed directory, and if so returns the secret name Daffa must hold sealed material for.
// It accepts the forms an operator writes (`./daffa-secrets/x`, `daffa-secrets/x`); compose
// normalises the leading `./` away before we ever see it. A nested path is not a valid name.
func DaffaSecretRef(file string) (string, bool) {
	f := path.Clean(strings.TrimSpace(file))
	prefix := secretsDir + "/"
	if !strings.HasPrefix(f, prefix) {
		return "", false
	}
	name := strings.TrimPrefix(f, prefix)
	if name == "" || strings.ContainsAny(name, "/\\") {
		return "", false
	}
	return name, true
}

// Build assembles the bundle. The hash covers the compose file AND the env, because a
// changed variable is a changed deployment even when the YAML is identical — that is
// exactly the case where "nothing changed, why did it redeploy?" would otherwise be a
// lie in the other direction.
func Build(yaml string, env []EnvVar, auths []*RegistryAuth) (*Bundle, error) {
	return BuildPlanned(yaml, &HookPlan{DeployYAML: yaml}, env, nil, auths)
}

// BuildPlanned assembles a bundle from a hook plan: the engine gets plan.DeployYAML as
// its compose file, and hooks.yml rides along when there are hooks.
//
// The hash — the deployment's identity, what drift detection compares — is computed over
// the ORIGINAL file, not the split. The split is Daffa's derivation, and a new Daffa
// version deriving it slightly differently must not make every hooked stack in the fleet
// read as "source changed". Bundle.YAML is the original too, for the same reason: it is
// what the user wrote, shown back to them.
func BuildPlanned(yaml string, plan *HookPlan, env []EnvVar, secrets []Secret, auths []*RegistryAuth) (*Bundle, error) {
	if strings.TrimSpace(yaml) == "" {
		return nil, fmt.Errorf("stacks: the compose file is empty")
	}

	sorted := make([]EnvVar, len(env))
	copy(sorted, env)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })

	envFile := renderEnv(sorted)

	secs := make([]Secret, len(secrets))
	copy(secs, secrets)
	sort.Slice(secs, func(i, j int) bool { return secs[i].Name < secs[j].Name })

	h := sha256.New()
	h.Write([]byte(yaml))
	h.Write([]byte{0})
	h.Write([]byte(envFile))
	// A rotated secret is a changed deployment, exactly like a changed env var — so it feeds
	// the hash, and drift detection sees it. The plaintext content is only ever hashed here;
	// nothing persisted carries it.
	for _, s := range secs {
		h.Write([]byte{0})
		h.Write([]byte(s.Name))
		h.Write([]byte{0})
		h.Write([]byte(s.Content))
	}
	hash := hex.EncodeToString(h.Sum(nil))[:16]

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	files := []struct {
		name string
		body string
		mode int64
	}{
		{composePath, plan.DeployYAML, 0o644},
		{envPath, envFile, 0o600}, // may hold secrets
	}
	for _, s := range secs {
		// The name is the filename, so a '/' or '..' in it would write outside the bundle
		// directory. Handlers validate the name on the way in; this is the backstop.
		if s.Name == "" || strings.ContainsAny(s.Name, "/\\") || strings.Contains(s.Name, "..") {
			return nil, fmt.Errorf("stacks: unsafe secret name %q", s.Name)
		}
		files = append(files, struct {
			name string
			body string
			mode int64
		}{secretsDir + "/" + s.Name, s.Content, 0o400})
	}
	if plan.HooksYAML != "" {
		files = append(files, struct {
			name string
			body string
			mode int64
		}{hooksPath, plan.HooksYAML, 0o644})
	}
	if len(auths) > 0 {
		cfg, err := dockerConfig(auths)
		if err != nil {
			return nil, err
		}
		files = append(files, struct {
			name string
			body string
			mode int64
		}{authPath, cfg, 0o600})
	}

	for _, f := range files {
		hdr := &tar.Header{
			Name: f.name,
			Mode: f.mode,
			Size: int64(len(f.body)),
			// A fixed mtime keeps the tar byte-identical for identical content, so the
			// hash means what it says.
			ModTime: time.Unix(0, 0),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("stacks: writing bundle: %w", err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			return nil, fmt.Errorf("stacks: writing bundle: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("stacks: closing bundle: %w", err)
	}

	return &Bundle{Tar: buf.Bytes(), Hash: hash, YAML: yaml, Env: sorted}, nil
}

// renderEnv writes a compose-compatible .env file. Values are quoted so that a password
// containing a space, a #, or a newline does not silently truncate or comment out the
// rest of the file — which is the sort of bug that only shows up in production, on the
// one credential that happened to have a special character in it.
func renderEnv(vars []EnvVar) string {
	var b strings.Builder
	for _, v := range vars {
		if v.Key == "" {
			continue
		}
		b.WriteString(v.Key)
		b.WriteString("=")
		b.WriteString(quoteEnv(v.Value))
		b.WriteString("\n")
	}
	return b.String()
}

func quoteEnv(v string) string {
	// Compose's dotenv parser understands double quotes with backslash escapes.
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`)
	return `"` + r.Replace(v) + `"`
}

// dockerConfig renders the auth file the docker CLI reads — one `auths` entry per registry this
// deploy pulls from. It lives only inside the runner container, which is removed after the run.
//
// A credential WITH a username is HTTP Basic (base64(user:pass)); the docker CLI uses it directly
// and also to drive the Bearer token exchange, so it covers both basic and bearer registries.
//
// A credential with NO username is a bare token, and there it matters: base64(":token") is a
// malformed Basic header (there is no user half), which every registry rejects — the exact auth
// failure this used to produce. Docker's `registrytoken` field is this case: the value is sent as
// `Authorization: Bearer <token>`. So a username-less credential goes there instead of `auth`.
func dockerConfig(auths []*RegistryAuth) (string, error) {
	type authEntry struct {
		Auth          string `json:"auth,omitempty"`
		RegistryToken string `json:"registrytoken,omitempty"`
	}
	entries := make(map[string]authEntry, len(auths))
	for _, a := range auths {
		if a.Username == "" {
			entries[a.URL] = authEntry{RegistryToken: a.Password}
			continue
		}
		entries[a.URL] = authEntry{Auth: base64.StdEncoding.EncodeToString([]byte(a.Username + ":" + a.Password))}
	}
	b, err := json.Marshal(map[string]any{"auths": entries})
	if err != nil {
		return "", fmt.Errorf("stacks: rendering docker config: %w", err)
	}
	return string(b), nil
}
