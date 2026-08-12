# Manifests

A **manifest** is one YAML file declaring resources that should exist on your Daffa
server — stacks, certificates, deliveries, credentials, secret slots. Two verbs consume
it: `daffa plan` reports what would change without touching anything, and `daffa apply`
makes it so. Standing up a whole application becomes: write the manifest, apply, fill
the reported secret slots, deploy.

```sh
daffa plan  -f myapp.yaml --server https://daffa.example.com --token $DAFFA_TOKEN
daffa apply -f myapp.yaml --deploy
```

The CLI is the `daffa` binary itself — grab a prebuilt one for your platform from the
[releases page](https://github.com/Mnshahawy/daffa/releases) (Linux, macOS, and Windows,
with a `SHA256SUMS` beside them), drop it on your `PATH`, and point it at your server
with `--server`/`--token` or the `DAFFA_SERVER`/`DAFFA_TOKEN` environment variables.

Apply is **ensure-only**: it creates what is missing and updates safe fields, but it
never deletes anything and never rotates trust — a certificate that differs from its
declaration is reported as *drifted* and left alone. Removing things stays a decision
you make in the console, not a side effect of editing a file. Re-applying an unchanged
manifest is a no-op, and `daffa plan`'s exit code (0 in sync, 2 changes pending) makes
a one-line CI drift check.

## Writing manifests with an LLM

The complete authoring reference — every section, field, name rule, and secret form,
written to be handed to a language model as context — is maintained alongside the
schema and published with these docs. Paste it into your assistant's context and ask
for the manifest you need.

<script setup>
import { ref } from 'vue'
import { withBase } from 'vitepress'

const state = ref('idle')
async function copyReference() {
  try {
    const res = await fetch(withBase('/llms-manifests.txt'))
    if (!res.ok) throw new Error(String(res.status))
    await navigator.clipboard.writeText(await res.text())
    state.value = 'copied'
  } catch {
    state.value = 'failed'
  }
  setTimeout(() => { state.value = 'idle' }, 2500)
}
</script>

<p>
  <button class="copy-llms" :class="state" @click="copyReference">
    <span v-if="state === 'idle'">📋 Copy the LLM reference</span>
    <span v-else-if="state === 'copied'">✓ Copied to clipboard</span>
    <span v-else>Copy failed — open the raw file below</span>
  </button>
  &nbsp;<a :href="withBase('/llms-manifests.txt')" target="_blank" rel="noreferrer">view raw</a>
</p>

<style scoped>
.copy-llms {
  border: 1px solid var(--vp-c-brand-1);
  color: var(--vp-c-brand-1);
  background: transparent;
  border-radius: 8px;
  padding: 8px 16px;
  font-size: 14px;
  font-weight: 600;
  cursor: pointer;
  transition: background-color .2s, color .2s;
}
.copy-llms:hover { background: var(--vp-c-brand-soft); }
.copy-llms.copied { border-color: var(--vp-c-green-1); color: var(--vp-c-green-1); }
.copy-llms.failed { border-color: var(--vp-c-danger-1); color: var(--vp-c-danger-1); }
</style>

## A small example

```yaml
version: 1
cluster: prod

git_credentials:
  - name: app-repo
    kind: token
    username: ci-bot
    token: { value_from_env: FORGEJO_TOKEN }

cas:
  - { name: app-ca, common_name: App Internal CA }

certificates:
  - name: api
    ca: app-ca
    sans: [api.internal]
    usages: [server, client]

cert_deliveries:
  - volume: app-certs
    certs: [{ name: api, default: true }]
    bundle_cas: [app-ca]

generated_secrets:
  - { name: db-password }

stacks:
  - name: db
    engine: swarm
    source:
      git: { url: "https://…/app.git", ref: main, path: stacks/db.yml, credential: app-repo }
    secret_files:
      - { name: db_password, from_generated: db-password }
  - name: api
    engine: swarm
    source:
      git: { url: "https://…/app.git", ref: main, path: stacks/api.yml, credential: app-repo }
    watch_paths: ["stacks/api.yml"]
    auto_deploy: true
    env:
      - { key: SMTP_PASSWORD, secret: true }          # a slot you fill in the console
      - { key: DSN, secret: true,
          value: "postgres://app:${generated:db-password}@db:5432/app" }
```

Everything is declared and cross-referenced **by name** — no ids anywhere, and a
reference may point at something the document declares or at something that already
exists on the server. The document is partial by design: one stack and one certificate
is a complete manifest.

## Secrets never ride in the file

Secret fields are *slots*. A slot is filled one of four ways, and a literal secret in
the document is rejected at parse time:

- **In the console** — apply creates the slot, the report lists it as *unfilled*, and
  a value you type is never overwritten by later applies.
- **`value_from_env`** — the CLI resolves the named variable from *its own*
  environment at submit time and sends the value beside the document, which stays
  byte-identical and safe to commit.
- **`from_generated`** — Daffa mints the value at first apply (sealed at rest, never
  regenerated) and injects it into every slot that references it. One generated
  password can reach the database *and* every service that dials it.
- **Composed values** — `${generated:name}` inside a secret value, so a DSN's
  structure stays reviewable in the file while its secret part never appears:
  `postgres://app:${generated:db-password}@db:5432/app`.

## Verdicts

Every resource in a plan or apply gets one verdict:

| verdict | meaning |
|---|---|
| `create` | nothing with this name exists; apply creates it |
| `update` | exists; safe fields converge |
| `in-sync` | exists and matches |
| `drifted` | differs on fields apply refuses to touch — reported, never changed |
| `blocked` | a dependency is unmet; the detail names it |
| `unfilled` | a secret slot still needs a value |

Every apply is recorded — the document verbatim, who ran it, and the full report —
under **Manifests** in the console.
