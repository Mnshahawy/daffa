# Daffa manifests — authoring reference

You are writing a **Daffa manifest**: one YAML document declaring resources that should
exist on a Daffa server. It is consumed by `daffa plan -f file.yaml` (report what would
change, touch nothing) and `daffa apply -f file.yaml` (make it so). This reference is
complete — everything the format accepts is described here; anything else is rejected.

## Core rules

1. **Apply is ensure-only.** It creates what is missing and updates safe fields. It
   never deletes anything, and it never rotates trust material (CAs, certificates,
   generated secrets) — differences there are reported as `drifted` and left alone.
   Removing a resource is done in the Daffa console or API, never by removing a line.
2. **Resources are declared and cross-referenced by NAME, never by id.** A reference
   may point at something declared in the same document, or at something that already
   exists on the server (an "external" reference). A reference that resolves to
   neither makes that resource `blocked`, not the whole document.
3. **No secret value ever appears in the document.** Secret fields are slots (a name,
   filled later by a human), `value_from_env` slots (filled by the CLI from ITS
   environment at submit time), `from_generated` (minted by Daffa), or composed values
   whose only secret parts are `${generated:...}` references. A literal secret is a
   parse or validation error.
4. **Unknown fields are errors.** So is a second YAML document in the stream, and a
   missing `version`. Do not invent fields.
5. **The document may be partial.** Declaring one stack and one certificate is a
   complete, valid manifest. Nothing forces whole-installation declarations.
6. **Re-applying an unchanged document is a no-op** (everything reports `in-sync`).
   Write manifests so this holds: prefer stable names; never rely on apply-time
   randomness outside `generated_secrets`.

## Top-level structure

```yaml
version: 1            # required; 1 is the only version
name: my-app          # optional label for the apply history (not an identity)
cluster: prod         # default cluster (a Daffa environment NAME) for env-scoped
                      # resources; each may override with its own `cluster:` key.
                      # The cluster must already exist — a manifest cannot create one.

ssh_keys: []          # all sections optional, any subset is valid
registries: []
git_credentials: []
networks: []
cas: []
certificates: []
keyrings: []
cert_deliveries: []
keyring_deliveries: []
generated_secrets: []
stacks: []
volume_sources: []
```

Apply walks kinds in a fixed dependency order (credentials → networks → CAs →
certificates → keyrings → deliveries → generated secrets → stacks → volume sources),
so intra-document references "just work" regardless of section order. Within a kind,
document order is preserved — and for `stacks:` it is the deploy order
`daffa apply --deploy` walks.

## Name rules

| Applies to | Rule |
|---|---|
| stacks | lowercase letters, digits, `-`, `_`; ≤63 chars; must start with letter/digit (it becomes the compose project name) |
| certificates, CAs, keyrings, secret-file names | `[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}` (they become file names in volumes) |
| networks, volumes | Docker local-name rule: `[a-zA-Z0-9][a-zA-Z0-9_.-]*` |
| registries, git credentials, ssh keys, clusters | any single line ≤128 chars, no leading/trailing space |
| env var keys and `value_from_env` names | POSIX: `[A-Za-z_][A-Za-z0-9_]*` |
| generated secrets | `[A-Za-z0-9][A-Za-z0-9_-]{0,63}` |

## Sections

### ssh_keys — generate-if-missing keypairs

```yaml
ssh_keys:
  - { name: deploy-key, algo: ed25519 }   # algo optional; empty = server default
```

Generate-only: there is no import form (a private key in the document is forbidden by
design). An existing key with a different algo is `drifted`, never regenerated.

### registries — image-pull credentials (global)

```yaml
registries:
  - name: ghcr
    url: ghcr.io
    username: ci-bot
    password: { value_from_env: GHCR_TOKEN }   # optional
```

The password is create-only in Daffa's API. If the registry is missing AND no password
value is resolvable, the resource is `blocked` (Daffa refuses to create a credential
husk that deploys would trip over). `url` is required.

### git_credentials — repo-read credentials (global)

```yaml
git_credentials:
  - name: app-repo
    kind: token                              # token | ssh
    username: ci-bot
    token: { value_from_env: FORGEJO_TOKEN } # token kind only
  - name: infra-repo
    kind: ssh
    ssh_key: deploy-key                      # ssh kind only: an ssh_keys name
    host_key: "example.com ssh-ed25519 AAAA…" # optional pin; public material
```

`kind` is required. Token kind must not set `ssh_key`; ssh kind must not set `token`.
Like registries: create-only secret, so missing + unresolvable = `blocked`.

### networks — Docker networks on a cluster

```yaml
networks:
  - { name: app-internal, driver: overlay, attachable: true }
  - { name: app-edge }        # driver empty = overlay
```

Env-scoped (uses the default `cluster:` or its own). Existing networks whose driver or
attachable differ are `drifted` — Docker cannot mutate them in place. Make a network
`attachable: true` if any deploy hook container must join it.

### cas — certificate authorities (global)

```yaml
cas:
  - name: app-ca
    common_name: App Internal CA   # REQUIRED
    org: Example Corp              # optional
    key_algo: ecdsa-p256           # optional; empty = server default
    days: 3650                     # optional; 0 = server default (3650)
    outbound_trust: false          # optional; ABSENT = true (Daffa's own outbound
                                   # TLS trusts this root). Set false for roots that
                                   # exist only to be bundled into deliveries.
```

Never rotated by apply: any parameter difference on an existing CA is `drifted`,
except `outbound_trust`, which is the one safe `update`. A CA referenced elsewhere by
name but not declared here must already exist on the server, or the referrer blocks.

### certificates — leaves issued by a named CA

```yaml
certificates:
  - name: api
    cluster: prod          # optional override; OR `shared: true` for an env-less
                           # cert usable on every cluster (mutually exclusive)
    ca: app-ca             # REQUIRED (by name; declared here or pre-existing)
    sans: [api.internal, api]   # REQUIRED, at least one. Each entry is a host name,
                                # an IP, or a URI (spiffe://trust-domain/workload) —
                                # the kind is derived from the value, never declared.
                                # The first name-or-address entry becomes the CN;
                                # a URI never does.
    usages: [server, client]    # values: server, client; empty = server default
    key_algo: ecdsa-p256        # optional
    validity_days: 398          # optional; 0 = 398
    renew_before_days: 30       # optional; the one safe UPDATE field
```

Never re-issued by apply: differing SANs/usages/key_algo/CA are `drifted`. The name
becomes the delivered filename (`<name>.crt` / `<name>.key`), and mTLS peers usually
attribute callers by CN — so the name is identity, choose it deliberately.

A **URI SAN** is the other way to be attributed, and the one to reach for when the
caller's identity is finer-grained than a host name — a per-tenant or per-region
workload, where the peer authorizes on the identity rather than on where it dialled
from. It rides in the same list:

```yaml
certificates:
  - name: orders
    ca: app-ca
    usages: [server, client]        # a client EKU, or the peer cannot PRESENT this
    sans:
      - orders                      # dns
      - spiffe://example.internal/region/eu-01/svc/orders   # uri
```

### keyrings — versioned application encryption keys (global)

```yaml
keyrings:
  - { name: app-secrets, rotate_days: 90 }   # 0 = manual rotation only
```

`rotate_days` is the one updatable field.

### cert_deliveries — certificate material written into a volume

```yaml
cert_deliveries:
  - cluster: prod                 # optional override
    volume: app-certs             # the delivery's identity on that cluster
    certs:                        # certificates carried, by name; may be empty
      - { name: api, default: true }   # at most one default
    mount_path: /certs            # where the CONSUMER mounts it; empty = default
    uid: 0                        # optional; keys are written 0600, so match the
    gid: 0                        # consumer's runtime uid (e.g. 999 for postgres)
    traefik: false                # true also writes a Traefik file-provider fragment;
                                  # at most ONE traefik delivery per volume
    restart_targets: "svc1 svc2"  # space-separated container names bounced after a
                                  # changed sync; leave "" on Swarm (task names vary)
    bundle_cas: [app-ca]          # which roots ride ca-bundle.crt; empty = all
```

A delivery must carry at least one of `certs` / `bundle_cas` (bundle-only = a
trust-only volume). A delivery is an AUDIENCE: whoever mounts the volume holds every
private key in it, so give database/privileged leaves their own delivery and volume,
never the shared one. The manifest manages at most one delivery per (cluster, volume);
if several already exist there, it is `blocked`.

### keyring_deliveries — keyrings written into a volume

```yaml
keyring_deliveries:
  - { keyring: app-secrets, volume: app-keyring, uid: 0, gid: 0 }
  - { keyring: other-ring,  volume: app-keyring }   # several keyrings may share a
                                                    # volume (each writes <name>.json)
```

Identity is (cluster, keyring, volume). No update path: uid/gid/restart changes on an
existing delivery are `drifted`.

### generated_secrets — values Daffa mints and injects

```yaml
generated_secrets:
  - { name: db-password, format: alphanumeric, length: 32 }
  # format: alphanumeric (default) | hex | base64 ; length: 16–128, 0 = 32
```

Minted with crypto/rand at first apply, sealed, stored globally by name. Rules:

- **Generated once.** Re-applies read the value back; changed format/length is
  `drifted`, never a regeneration.
- **Every declared generated secret must be REFERENCED** by at least one stack slot in
  the same document, or validation fails. References use the short forms below.
- Names are global on the server: when generating manifests per cluster, prefix names
  with the cluster (`myapp-prod-db-password`) so clusters never share a value.

### stacks — stack registrations (source, watch paths, secret slots)

```yaml
stacks:
  - name: api
    cluster: prod               # optional override
    engine: swarm               # REQUIRED: compose | swarm; immutable once created
    source:                     # exactly ONE of git / compose
      git:
        url: https://example.com/app.git
        ref: main               # empty = default branch
        path: stacks/api.yml    # path to the compose file in the repo
        credential: app-repo    # git_credentials name; empty = public repo
    # OR:  compose: |            # inline compose YAML
    #        services: …
    watch_paths: ["stacks/api.yml", "config/**"]  # globs that trigger auto-deploy
    auto_deploy: true
    env:
      - { key: LOG_LEVEL, value: info }                  # plaintext, inline is fine
      - { key: API_TOKEN, secret: true }                 # slot: human fills in console
      - { key: SMTP_PASSWORD, secret: true,
          value_from_env: SMTP_PASSWORD }                # CLI fills from ITS env
      - { key: PG_PASSWORD, secret: true,
          from_generated: db-password }                  # Daffa injects
      - { key: DSN, secret: true,
          value: "postgres://app:${generated:db-password}@db:5432/app" }  # composed
    secret_files:               # Swarm engine ONLY (they become raft secrets)
      - api_signing_key                                       # slot (bare string)
      - { name: db_password, from_generated: db-password }    # injected
      - { name: database_url,
          value: "postgres://app:${generated:db-password}@db:5432/app" }  # composed
```

Constraints and semantics:

- An env var has at most ONE source (`value` / `value_from_env` / `from_generated`).
  `value_from_env`, `from_generated`, and `${generated:…}` references all require
  `secret: true`. A secret `value` MUST contain at least one `${generated:…}`
  reference — otherwise it would be a literal secret in the document.
- A secret-file `value` must likewise reference at least one generated secret;
  `from_generated` and `value` are mutually exclusive.
- `${generated:name}` is the ONLY substitution the format has, legal only inside
  secret values. There is no other templating — generate the document fully resolved.
- Slot semantics: plain slots and `value_from_env` slots are never overwritten once
  filled (absence of the variable is not intent). Generated-backed slots are OWNED:
  apply converges them to the stored value.
- `secret_files` on a `compose`-engine stack is a validation error — use secret env
  vars there.
- Apply REGISTERS stacks; it never deploys them. `daffa apply --deploy` deploys the
  manifest's stacks in document order afterwards, refusing any stack that still has
  unfilled slots. So order `stacks:` by dependency (database before app, app before
  edge).
- Env merge is additive: vars the manifest does not declare are left untouched.
- A compose-engine stack on a multi-node cluster cannot be placed by manifest (it
  needs a node pinned in the console first) and reports `blocked`.

### volume_sources — managed volume content (git or inline)

```yaml
volume_sources:
  - cluster: prod               # optional override
    volume: traefik-config      # identity: (cluster, volume)
    source:                     # exactly ONE of git / files
      git: { url: "https://…/infra.git", ref: main, path: traefik/, credential: infra-repo }
    # OR: files:
    #   - { path: dynamic.yml, content: "…" }   # relative paths only, no `..`
    stack: edge                 # optional: the stack whose deploys sync this first
    restart_targets: ""
    uid: 0
    gid: 0
    auto_sync: false            # true mints a push webhook for git sources; prefer
                                # false + putting the path in the consuming stack's
                                # watch_paths (one trigger per stack, no races)
```

## What apply reports (verdicts)

Each resource gets one verdict: `create` (missing, will be/was created), `update`
(exists, safe fields converged), `in-sync` (matches), `drifted` (differs on fields
apply refuses to touch — report only), `blocked` (a dependency is unmet; detail names
it), `unfilled` (a secret slot has no value yet). The report also carries an
`unfilled:` list naming every empty slot per stack.

CLI exit codes: `daffa plan` → 0 all in-sync, 2 changes/drift pending, 1 error — use
it as a CI drift check. `daffa apply` → 0 applied, 2 drifted/blocked remain, 1 error.

## Authoring checklist

- `version: 1` present; every env-scoped resource reaches a cluster (default or own).
- Every reference (`ca:`, `credential:`, `certs:`, `keyring:`, `stack:`,
  `from_generated`) names something declared here or known to exist on the server.
- Every declared generated secret is referenced; every external secret is a slot or
  `value_from_env`; no literal secret anywhere.
- Stack names are compose-safe; engines are explicit; stacks are in deploy order.
- Uniqueness: one resource per name per kind (per cluster where scoped); one delivery
  per volume; one Traefik delivery per volume; at most one default cert per delivery.
- Prefer external references over re-declaring resources another manifest owns.
