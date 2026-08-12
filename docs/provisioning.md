# Manifests: declarative provisioning

Standing up a fully-managed application on Daffa is a long chain of imperative calls in a
forced order: credentials, cluster, CA, certificates, deliveries, keyrings, volume
sources, stack registration, secret entry, deploy. Roughly ten API calls or UI screens
for one git stack; several dozen for a real deployment. Nothing records the intended end
state, so there is no way to diff a live install against what it should be, and standing
up a second copy of anything means replaying human memory.

A **manifest** is one YAML document declaring the resources that should exist. Two verbs
consume it: **plan** (what would change, touching nothing) and **apply** (make it so).
Deployment becomes: write the manifest, `daffa apply`, fill the reported secret slots,
deploy.

Why "manifest" and not "provision": provisioning is already a word here —
`POST /api/clusters/provision` SSHes into a bare machine and installs Docker. Reusing it
for a different feature would make two unrelated capabilities sound like one.

## Doctrine

Everything below follows five rules. They are not stylistic; each one closes a failure
mode.

**Ensure-only.** Apply creates what is missing and updates safe fields. It never
deletes, and it never rotates trust: a CA or certificate whose declared parameters
differ from the stored row is reported as *drifted*, not regenerated. The failure mode
this avoids is the worst one a declarative tool has: a removed line — or a typo'd name —
silently destroying a resource other things depend on. Daffa's whole delete posture is
"refuse, don't orphan"; a file diff is not allowed to override it. Removing a resource
stays a decision somebody makes on purpose, in the UI or via DELETE.

**Names are identities.** Every resource is declared and cross-referenced by name, keyed
on the unique index the store already enforces (`environments.name`,
`stacks(env_id, name)`, `certs(env_id, name)`, …). The server resolves names to ids at
plan time. No id ever appears in a manifest — which kills the "create it in the UI, then
paste the id back into the file" loop, the most error-prone step of any half-declarative
setup. The flip side is stated plainly: renaming a resource in a manifest does not
rename anything — it declares a *new* resource and strands the old one, and plan will
say so by showing a `create` where an `in-sync` was expected.

**No secret ever appears in the document.** Secret-bearing fields are *slots*: the
manifest declares the name, and the report says which slots are unfilled. A slot may say
where its value comes from — `value_from_env: NAME` — which the *submitting client*
resolves from its own environment, exactly as trustworthy as typing the value into the
UI. Resolved values travel beside the document, never inside it, so the document that
gets stored in the apply history is byte-identical to the file that was written, and a
manifest is always safe to commit. The schema enforces this by shape: a secret field
only decodes as a mapping, so `password: hunter2` is a parse error, not a lint warning.

**No templating.** The manifest is dumb, fully-resolved YAML. No variables, no loops, no
naming conventions. Anything that generates manifests — a values file, a script, a
platform's cell definition — lives outside Daffa and emits plain documents. This is the
Kubernetes/Helm split, and it is what keeps any one consumer's shape from leaking into
the format everyone shares.

**Partial by design.** A document declaring two stacks and one certificate is complete
and valid. References the document does not itself declare (a CA that already exists on
the server) are resolved against the store; only a reference that resolves nowhere
blocks. Nobody is forced into whole-installation declarations to use the feature.

## The document

```yaml
version: 1
name: my-app            # a label for the apply history, not an identity
cluster: prod           # default cluster; any resource may override with its own

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

networks:
  - { name: app-internal, driver: overlay, attachable: true }

cas:
  - { name: app-ca, common_name: App Internal CA, key_algo: ecdsa-p256, days: 3650 }

certificates:
  - name: api
    ca: app-ca
    sans: [api.internal, api]
    usages: [server, client]
    validity_days: 398
    renew_before_days: 30

cert_deliveries:
  - volume: app-certs
    certs: [{ name: api, default: true }]
    restart_targets: "api"
    bundle_cas: [app-ca]

keyrings:
  - { name: app-secrets, rotate_days: 90 }

stacks:
  - name: api
    engine: swarm
    source:
      git: { url: "https://…/app.git", ref: main, path: stacks/api.yml, credential: app-repo }
    watch_paths: ["stacks/api.yml"]
    auto_deploy: true
    env:
      - { key: LOG_LEVEL, value: info }        # plaintext is fine inline
      - { key: DB_PASSWORD, secret: true }     # a slot: created empty, reported unfilled
    secret_files: [db_client_key]

volume_sources:
  - volume: traefik-config
    source:
      git: { url: "https://…/infra.git", ref: main, path: traefik/, credential: app-repo }
    stack: api
    restart_targets: "traefik"
```

Sections: `ssh_keys`, `registries`, `git_credentials`, `networks`, `cas`,
`certificates`, `keyrings`, `cert_deliveries`, `keyring_deliveries`, `stacks`,
`volume_sources`. All optional; unknown fields are errors, so a typo fails the parse
instead of silently declaring nothing.

Deliberately absent:

- **Ids.** See above.
- **Deploy actions.** Apply registers stacks; it does not deploy them. A deploy is a
  runtime action with its own concurrency guard and log stream, and folding it into
  apply would make "apply the document" and "restart production" the same button. The
  CLI's `--deploy` flag closes the gap by driving the existing deploy endpoints in
  document order, refusing any stack with known-unfilled secret slots.
- **Cluster creation.** A manifest requires its clusters to exist. SSH clusters
  dial-before-persist with host keys pinned on first contact, and agent clusters can
  only wait for the agent to dial out — neither collapses into "ensure it exists".
- **Swarm topology.** Membership is discovered from the daemon, never asserted; a
  document that could disagree with the Swarm would always lose.
- **Age private keys.** The box cannot read its own backups; no document changes that.

## Plan, apply, and verdicts

Plan and apply run the same walk; plan just never executes. Kinds are processed in a
fixed dependency order (credentials → networks → CAs → certificates → keyrings →
deliveries → stacks → volume sources), so there is no user-facing DAG to author or
debug. Every resource gets one verdict:

| verdict    | meaning                                                                  |
|------------|--------------------------------------------------------------------------|
| `create`   | nothing with this name exists; apply creates it                          |
| `update`   | exists; safe fields differ; apply updates them                           |
| `in-sync`  | exists and matches                                                       |
| `drifted`  | exists but differs on fields apply refuses to change — trust material, a |
|            | stack's engine, a certificate's scope. Report-only, visible in exit codes |
| `blocked`  | a dependency is unmet (cluster missing, referenced CA nowhere)           |
| `unfilled` | exists, but a secret slot has no value                                   |

Apply is not transactional across resources — Docker-side effects cannot be rolled
back — but every step is idempotent, so a failed apply is resumed by running apply
again, and the report says exactly where it stopped.

Secret-bearing resources split by what the API can actually do:

- Resources whose secrets are updatable after creation (stack env and secret files) are
  created with the slot empty and reported `unfilled`. Apply never overwrites a slot a
  human has since filled.
- Resources whose secret is create-only (registries, git credentials — they have no
  update route) are created only when a value is resolvable; otherwise the report says
  `blocked` and names what is needed. Creating a husk with an empty password would move
  the failure to deploy time, where it is unrecognizable.

## Authorization

There is no manifest capability that grants anything. Every resource in a document is
checked against the same capability its imperative route declares, at the same scope —
a manifest touching certificates and stacks needs `certs.edit` and `stacks.edit`
exactly as the equivalent hand-made calls would. The check is a whole-document
preflight: one missing capability refuses the entire apply, before any mutation, naming
what is missing. A partially-authorized apply would be worse than a refusal — the
operator would have to diff what happened against what didn't.

Plan runs the same preflight. Planning is a dry-run of apply, and "what would change"
reveals existence and drift; someone who may not edit certificates has no business
planning them.

Every apply is recorded — document, hash, actor, per-resource report — and every
mutation is audited individually, so the history answers both "what changed" and "was
this exact file ever applied".

## The CLI

```
daffa plan  -f manifest.yaml
daffa apply -f manifest.yaml [--deploy]
```

Both are remote commands (`--server`/`DAFFA_SERVER`, `--token`/`DAFFA_TOKEN`), like
`daffa restore`. `plan` exits 0 when everything is in sync, 2 when changes or drift are
pending, 1 on error — so a CI drift check is one line. `value_from_env` slots are
resolved client-side before submission; a missing variable fails fast, named, before
the server sees anything.
