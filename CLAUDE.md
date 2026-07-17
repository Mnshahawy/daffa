# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Daffa (دفّة, "the helm") is a self-hosted Docker operations console — one static Go binary
with the Vue SPA embedded, SQLite by default or Postgres, no telemetry, no licensed tiers.
It manages the box it runs on, which drives several design decisions below.

## Commands

```sh
go build ./...                     # build everything
go test ./...                      # full suite (SQLite; Postgres paths skip)
go test ./internal/store/ -run TestMigrate0014 -v    # one test
go vet ./...

go test ./internal/caps -update    # regenerate caps_golden.json after adding a capability
go generate ./internal/caps        # regenerate web/src/lib/caps.ts from the Go registry

go generate ./internal/api         # regenerate openapi.json + web/src/lib/api.ts from the route table
go test ./internal/api -update     # shrink the OpenAPI coverage/manual-client ratchet goldens after backfilling

cd web && pnpm build               # vue-tsc typecheck + vite build → internal/web/dist (embedded)
cd web && pnpm dev                 # vite dev server

DAFFA_TEST_PG_URL=postgres://… go test ./internal/store/   # exercise the Postgres dialect
DAFFA_ADDR=127.0.0.1:8080 DAFFA_DATA_DIR=/tmp/daffa go run ./cmd/daffa   # run locally

make hooks                         # install the pre-commit hook that mirrors CI locally
```

A caps test executes the real `web/src/lib/caps.ts` under node — if it fails after adding a
capability, run `go generate ./internal/caps`, never hand-edit caps.ts.

## The feature spine

Every feature follows the same layers, and they must land together:

1. **Migration** — `internal/store/migrate.go`. Append-only once shipped; common-subset SQL
   (TEXT/INTEGER, `?` placeholders — `rebind` converts for Postgres). `pg` is extra
   Postgres-only SQL; `fn` is a Go step in the same transaction, for transforms SQL can't
   express. Test against a *populated older* database using the `stopAfter` seam (see
   `migrate_test.go` — every migration bug that shipped passed fresh-schema tests).
   Exception: the partitioned `metric_samples` DDL lives in `store/metrics.go`, not a
   migration, on purpose.
2. **Store** — one file per entity in `internal/store/`: struct with `Enc`-suffixed sealed
   fields, a `xxxCols` const, a `scanXxx` helper, CRUD + a `List(ctx, global, envs)` that
   *filters* via `envIn` (never gates). Helpers: `now` (a var, for tests), `ts`/`parseTS`
   (RFC3339 text), `nullStr`, `nullTS`, `boolInt`, `NewID`, sentinel `store.ErrNotFound`.
   IDs are prefixed: `mon_`, `ca_`, `crt_`, `key_`, `dlv_`.
3. **Capabilities** — `internal/caps/caps.go`. Namespaced bits, append-only within a
   namespace (`TestBitsNeverMove` + golden file). A `Cap` is a struct so the zero value
   fails closed. Register twice: the `var` and the `Def` in `All`. `Normalize` materialises
   edit⇒view at grant time. `ScopeGlobal` caps cannot be granted per-host. Adding a
   namespace costs no migration (role_caps is row-per-namespace). Credential-store pattern:
   view is env-grantable and secret-free, edit is global-only.
4. **Routes + handlers** — the single authorization table `Server.apiRoutes()` in
   `internal/api/server.go`. Every route declares a `cap` + `scope`, or an `open` reason —
   `TestEveryRouteIsGuarded` enforces it. Scope kinds: `scopeGlobal`, `scopeEnv` (env in
   path), `scopeAny` (list routes), `scopeStack/Job/Deployment/Monitor` (middleware resolves
   the id, checks the cap at the owning host, stashes the entity in context, returns **404
   not 403**), and `scopeBody` (handler must call `s.mayUseEnv` after decoding — the list is
   pinned by a test). Handlers audit every mutation (`s.audit`); notification failures never
   fail the operation.
5. **Web** — typed client methods on the `daffa` object in `web/src/lib/api.ts`, a view in
   `web/src/views/`, nav in `web/src/lib/nav.ts` (one declaration feeds rail, palette, and
   router fallback), route in `router.ts` with `meta.cap`. Buttons gate on `session.can()`.
   Secrets never come back to the client — expose `has_secret`-style booleans.
   `api.ts` is GENERATED (like caps.ts — never hand-edit): declare `req`/`resp`/`ts` on the
   route plus `//oapi:` annotations, delete any manual copies from `api-manual.ts`, then
   `go generate ./internal/api`. See docs/openapi.md.

## Secrets: three postures, on purpose

- **Sealed at rest**: every `_enc` column is AES-256-GCM under `<DataDir>/master.key`
  (`config.Sealer`). Losing the key means re-entering secrets, not losing data. Sealed
  values are write-only through the API.
- **Never on the box**: age backup keys. The server stores public recipients only and
  actively rejects `AGE-SECRET-KEY-`; decryption happens in the CLI on the operator's
  machine (`--identity`). Key generation returns the private half exactly once
  (`handleCreateKey`) and never persists it. Do not weaken this: "the box cannot read its
  own backups" is a stated invariant.
- **Plaintext on purpose**: public material — certificates, age recipients — is stored
  unsealed, with comments saying so.

## Other load-bearing patterns

- **Docker access** goes through `dockerx.Pool` → `Env` → `Node.Client`: the same moby
  client whether local socket or agent tunnel (agents dial *out*; no inbound port). Code
  that works locally works remotely if it sticks to Docker API calls.
- **Nothing long-running executes in-process**: deploys run in an ephemeral pinned
  `docker:cli` runner container (`stacks/runner.go`) fed by `CopyToContainer`; cert
  delivery writes volumes the same way (`api/cert_delivery.go` — copy into a *running*
  helper, or the volume shadows the files). Detached runners survive Daffa redeploying
  itself.
- **Background work** hangs off `Server.Start()`: notify outbox worker, metrics collector +
  retention, backup scheduler, cert renewal worker. Handlers that spawn work detach with
  `context.WithoutCancel(r.Context())`.
- **Refuse, don't orphan**: deleting anything another row depends on is refused via an
  `InUse` count (storage targets, CAs, certificates, encryption keys).

## Conventions

- Docs in `docs/` are per-feature design documents (`design.md` is the umbrella; `certs.md`,
  `monitoring.md`, `rbac.md`, `stacks.md`, `swarm.md`…). They are opinionated and
  reasoning-heavy; substantial features get one, and code comments explain *why* — the
  failure mode a choice avoids — not what the next line does. Match that register.
- Error strings: `pkg: what went wrong`, written for the operator who reads them
  ("was the master key replaced?"). User-facing refusals name the fix.
- User-visible mistakes are 400s with the reason; `httpx.Fail(w, r, code, "snake_code", msg)`.
- Timestamps are RFC3339 text everywhere except `metric_samples` (epoch seconds — the
  lexicographic-sort trap is documented in `docs/monitoring.md`).
- JS capability masks must use BigInt (`hasCap` in caps.ts) — bitwise ops are 32-bit; this
  shipped as a real bug once and the comment block explains it.
- Known flaky test: `internal/notify TestWorkerRetriesAFailingChannel` (timing); rerun
  before assuming a regression.
- CI (`.github/workflows/ci.yml`) is the source of truth for "green"; `make hooks` installs
  a tracked `.githooks/pre-commit` that runs the same `go`-job checks locally — gofmt,
  `go vet`, generated-file freshness (`go generate ./internal/caps ./internal/api` must
  leave the tree clean — a stale caps.ts/api.ts/openapi.json is a red X), `go build`,
  `go test` — plus `pnpm build` when `web/` or `brand/` changed. The `dist/` embed target is
  kept alive on a fresh clone by a tracked `internal/web/dist/.gitkeep`; the SPA has to be
  built (`pnpm build` / `make web`) before it ships anything real.
- Toolchain is pinned on purpose: pnpm is **9** in CI *and* the Docker build stage —
  corepack's floating default moved to pnpm 10, whose ignored-builds gate exits non-zero and
  broke the image build. The web build also `@import`s the repo-root `brand/tokens.css`, so
  the Docker web stage copies `brand/` in (it lives outside `web/`). Actions are pinned to
  their node24 majors. The release workflow is manual and takes a `bump` (major/minor/patch,
  default minor); it derives the next version from the latest tag rather than a typed string.
