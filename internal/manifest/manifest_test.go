package manifest

import (
	"strings"
	"testing"
)

// fullDoc exercises every section once. It is the schema's reference example: if a
// field is renamed, this breaks loudly here before it breaks quietly for a user.
const fullDoc = `
version: 1
name: my-app
cluster: prod

ssh_keys:
  - { name: deploy-key, algo: ed25519 }

registries:
  - name: ghcr
    url: ghcr.io
    username: ci-bot
    password: { value_from_env: GHCR_TOKEN }

git_credentials:
  - name: app-repo
    kind: token
    username: ci-bot
    token: { value_from_env: FORGEJO_TOKEN }
  - name: infra-repo
    kind: ssh
    ssh_key: deploy-key

networks:
  - { name: app-internal, driver: overlay, attachable: true }

cas:
  - { name: app-ca, common_name: App Internal CA, key_algo: ecdsa-p256, days: 3650 }

certificates:
  - name: api
    ca: app-ca
    sans: [api.internal, api]
    usages: [server, client]
    key_algo: ecdsa-p256
    validity_days: 398
    renew_before_days: 30
  - name: roots
    shared: true
    ca: app-ca
    sans: [roots.internal]

keyrings:
  - { name: app-secrets, rotate_days: 90 }

cert_deliveries:
  - volume: app-certs
    certs: [{ name: api, default: true }]
    mount_path: /certs
    restart_targets: "api"
    bundle_cas: [app-ca]
  - volume: trust-only
    bundle_cas: [app-ca]

keyring_deliveries:
  - { keyring: app-secrets, volume: app-keyring, uid: 100, gid: 100 }

stacks:
  - name: api
    engine: swarm
    source:
      git: { url: "https://example.com/app.git", ref: main, path: stacks/api.yml, credential: app-repo }
    watch_paths: ["stacks/api.yml", "config/**"]
    auto_deploy: true
    env:
      - { key: LOG_LEVEL, value: info }
      - { key: DB_PASSWORD, secret: true }
      - { key: SMTP_PASSWORD, secret: true, value_from_env: SMTP_PASSWORD }
    secret_files: [db_client_key]
  - name: edge
    engine: compose
    source:
      compose: |
        services:
          traefik:
            image: traefik:v3

volume_sources:
  - volume: traefik-config
    source:
      git: { url: "https://example.com/infra.git", ref: main, path: traefik/, credential: infra-repo }
    stack: edge
    restart_targets: "traefik"
  - volume: hosts-config
    source:
      files:
        - { path: hosts, content: "10.0.0.1 db\n" }
    auto_sync: true
`

func TestParseFullDocument(t *testing.T) {
	m, err := Parse([]byte(fullDoc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if m.Name != "my-app" || m.Cluster != "prod" {
		t.Errorf("header: got name %q cluster %q", m.Name, m.Cluster)
	}
	if len(m.Stacks) != 2 || len(m.Certificates) != 2 || len(m.VolumeSources) != 2 {
		t.Fatalf("sections: %d stacks, %d certs, %d volume sources",
			len(m.Stacks), len(m.Certificates), len(m.VolumeSources))
	}
	if m.Registries[0].Password == nil || m.Registries[0].Password.ValueFromEnv != "GHCR_TOKEN" {
		t.Errorf("registry password slot: %+v", m.Registries[0].Password)
	}
	if !m.Certificates[1].Shared {
		t.Errorf("shared cert lost its flag: %+v", m.Certificates[1])
	}
	env := m.Stacks[0].Env
	if !env[1].Secret || env[1].Value != "" || env[2].ValueFromEnv != "SMTP_PASSWORD" {
		t.Errorf("env slots: %+v", env)
	}
	if m.Stacks[1].Source.Compose == "" || m.Stacks[1].Source.Git != nil {
		t.Errorf("inline stack source: %+v", m.Stacks[1].Source)
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string // substring of the error
	}{
		{"empty document", "", "empty"},
		{"comment-only document", "# nothing here\n", "empty"},
		{"unknown field", "version: 1\nclusterr: prod\n", "clusterr"},
		{"second document", "version: 1\n---\nversion: 1\n", "one YAML document"},
		{"literal secret", "version: 1\nregistries:\n  - name: r\n    url: u\n    password: hunter2\n", "cannot unmarshal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc))
			if err == nil {
				t.Fatal("parsed without error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"missing version",
			"name: x\n",
			"version"},
		{"wrong version",
			"version: 2\n",
			"version"},
		{"no cluster anywhere",
			"version: 1\nstacks:\n  - name: api\n    engine: compose\n    source: {compose: 'services: {}'}\n",
			"no cluster"},
		{"duplicate stacks",
			"version: 1\ncluster: p\nstacks:\n  - {name: api, engine: compose, source: {compose: 'x: y'}}\n  - {name: api, engine: compose, source: {compose: 'x: y'}}\n",
			"declared twice"},
		{"same stack name on two clusters is fine, same cert name shared and scoped is fine",
			"version: 1\ncluster: p\n" +
				"stacks:\n  - {name: api, engine: compose, source: {compose: 'x: y'}}\n" +
				"  - {name: api, cluster: q, engine: compose, source: {compose: 'x: y'}}\n" +
				"cas: [{name: ca1, common_name: CA One}]\n" +
				"certificates:\n  - {name: api, ca: ca1, sans: [a]}\n" +
				"  - {name: api, shared: true, ca: ca1, sans: [a]}\n",
			""},
		{"uppercase stack name",
			"version: 1\ncluster: p\nstacks:\n  - {name: API, engine: compose, source: {compose: 'x: y'}}\n",
			"compose project name"},
		{"missing engine",
			"version: 1\ncluster: p\nstacks:\n  - {name: api, source: {compose: 'x: y'}}\n",
			"engine"},
		{"both stack sources",
			"version: 1\ncluster: p\nstacks:\n  - {name: api, engine: compose, source: {compose: 'x: y', git: {url: u}}}\n",
			"not both"},
		{"neither stack source",
			"version: 1\ncluster: p\nstacks:\n  - {name: api, engine: compose, source: {}}\n",
			"source needs"},
		{"git source without url",
			"version: 1\ncluster: p\nstacks:\n  - {name: api, engine: compose, source: {git: {ref: main}}}\n",
			"needs a url"},
		{"secret env with literal value",
			"version: 1\ncluster: p\nstacks:\n  - name: api\n    engine: compose\n    source: {compose: 'x: y'}\n    env: [{key: DB_PASSWORD, secret: true, value: hunter2}]\n",
			"cannot carry a literal value"},
		{"value_from_env without secret",
			"version: 1\ncluster: p\nstacks:\n  - name: api\n    engine: compose\n    source: {compose: 'x: y'}\n    env: [{key: DB_PASSWORD, value_from_env: DB_PASSWORD}]\n",
			"implies secret"},
		{"bad env key",
			"version: 1\ncluster: p\nstacks:\n  - name: api\n    engine: compose\n    source: {compose: 'x: y'}\n    env: [{key: 9BAD, value: v}]\n",
			"not a valid environment variable name"},
		{"duplicate env key",
			"version: 1\ncluster: p\nstacks:\n  - name: api\n    engine: compose\n    source: {compose: 'x: y'}\n    env: [{key: A, value: v}, {key: A, value: w}]\n",
			"declared twice"},
		{"bad value_from_env name",
			"version: 1\nregistries:\n  - {name: r, url: u, password: {value_from_env: 'NOT OK'}}\n",
			"not a valid environment variable name"},
		{"shared cert with cluster",
			"version: 1\ncertificates:\n  - {name: c, cluster: p, shared: true, ca: x, sans: [a]}\n",
			"mutually exclusive"},
		{"cert without ca",
			"version: 1\ncluster: p\ncertificates:\n  - {name: c, sans: [a]}\n",
			"ca is required"},
		{"cert without sans",
			"version: 1\ncluster: p\ncertificates:\n  - {name: c, ca: x}\n",
			"SAN"},
		{"cert bad usage",
			"version: 1\ncluster: p\ncertificates:\n  - {name: c, ca: x, sans: [a], usages: [signing]}\n",
			"usage must be server or client"},
		{"gitcred bad kind",
			"version: 1\ngit_credentials:\n  - {name: g, kind: password}\n",
			"kind must be token or ssh"},
		{"gitcred ssh without key",
			"version: 1\ngit_credentials:\n  - {name: g, kind: ssh}\n",
			"requires ssh_key"},
		{"gitcred token with ssh_key",
			"version: 1\ngit_credentials:\n  - {name: g, kind: token, ssh_key: k}\n",
			"belongs to kind ssh"},
		{"registry without url",
			"version: 1\nregistries:\n  - {name: r}\n",
			"url is required"},
		{"empty delivery",
			"version: 1\ncluster: p\ncert_deliveries:\n  - {volume: v}\n",
			"delivers nothing"},
		{"two default certs",
			"version: 1\ncluster: p\ncert_deliveries:\n  - {volume: v, certs: [{name: a, default: true}, {name: b, default: true}]}\n",
			"at most one cert"},
		{"keyring delivery without keyring",
			"version: 1\ncluster: p\nkeyring_deliveries:\n  - {volume: v}\n",
			"keyring is required"},
		{"several keyrings sharing one delivery volume is fine",
			"version: 1\ncluster: p\nkeyring_deliveries:\n  - {keyring: a, volume: v}\n  - {keyring: b, volume: v}\n",
			""},
		{"same keyring into the same volume twice",
			"version: 1\ncluster: p\nkeyring_deliveries:\n  - {keyring: a, volume: v}\n  - {keyring: a, volume: v}\n",
			"declared twice"},
		{"volume source both shapes",
			"version: 1\ncluster: p\nvolume_sources:\n  - {volume: v, source: {git: {url: u}, files: [{path: a, content: b}]}}\n",
			"not both"},
		{"volume source neither shape",
			"version: 1\ncluster: p\nvolume_sources:\n  - {volume: v, source: {}}\n",
			"source needs"},
		{"file path escapes",
			"version: 1\ncluster: p\nvolume_sources:\n  - {volume: v, source: {files: [{path: ../etc/passwd, content: x}]}}\n",
			"stay inside the volume"},
		{"absolute file path",
			"version: 1\ncluster: p\nvolume_sources:\n  - {volume: v, source: {files: [{path: /etc/hosts, content: x}]}}\n",
			"stay inside the volume"},
		{"bad secret file name",
			"version: 1\ncluster: p\nstacks:\n  - {name: api, engine: compose, source: {compose: 'x: y'}, secret_files: ['a/b']}\n",
			"becomes a file name"},
		{"bad network name",
			"version: 1\ncluster: p\nnetworks:\n  - {name: '-bad'}\n",
			"invalid name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Parse([]byte(tc.doc))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			err = m.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("expected valid, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("validated without error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// Every error a manifest has is reported in one pass — an author fixing a file gets
// the full list, not one problem per round-trip.
func TestValidateJoinsAllErrors(t *testing.T) {
	doc := "version: 2\nregistries:\n  - {name: r}\ncluster: p\ncertificates:\n  - {name: c, ca: x}\n"
	m, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	err = m.Validate()
	if err == nil {
		t.Fatal("validated without error")
	}
	for _, want := range []string{"version", "url is required", "SAN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("joined error is missing %q: %v", want, err)
		}
	}
}

func TestHash(t *testing.T) {
	a, b := Hash([]byte("version: 1\n")), Hash([]byte("version: 1\n"))
	if a != b {
		t.Errorf("hash is not deterministic: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "sha256:") || len(a) != len("sha256:")+64 {
		t.Errorf("hash shape: %q", a)
	}
	if Hash([]byte("version: 1")) == a {
		t.Error("hash ignores the bytes")
	}
}
