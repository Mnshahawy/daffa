# Contributing to Daffa

Thanks for helping. Daffa is one static Go binary with a Vue SPA embedded — SQLite by
default, Postgres optional, no telemetry. It manages the box it runs on, and that shapes
most of the design. Read `CLAUDE.md` for the architecture in full; the essentials are below.

## Getting set up

```sh
go build ./...        # build everything (needs a checked-in internal/web/dist/.gitkeep)
go test ./...         # full suite — SQLite; Postgres paths skip without DAFFA_TEST_PG_URL
go vet ./...
make dev              # run on :8099 against the local Docker socket
```

Front end:

```sh
cd web
pnpm install
pnpm build            # vue-tsc typecheck + vite build → internal/web/dist (embedded)
pnpm dev              # vite dev server
```

To exercise the Postgres dialect locally, `make test-pg` spins a throwaway Postgres and
runs the suite against both.

## The feature spine

A feature is not one layer — it is all of them, landing together:

1. **Migration** — `internal/store/migrate.go`, append-only once shipped, tested against a
   *populated older* DB via the `stopAfter` seam.
2. **Store** — one file per entity; `Enc`-sealed secret fields; `List` filters, never gates.
3. **Capabilities** — `internal/caps/caps.go`; namespaced, append-only bits; registered twice.
4. **Routes + handlers** — the `Server.apiRoutes()` table; every route declares a `cap` +
   `scope` or an `open` reason; mutations audit.
5. **Web** — typed client, a view, nav, a route with `meta.cap`; buttons gate on `session.can()`.

The PR template walks this list. If your change touches only one layer, say why the rest
don't apply.

## Generated code — never hand-edit

Three files are generated from Go and guarded by CI:

```sh
go generate ./internal/caps    # → web/src/lib/caps.ts
go generate ./internal/api     # → openapi.json + web/src/lib/api.ts
go test ./internal/caps -update # → caps_golden.json (after adding a capability)
```

CI fails if these are stale. Regenerate and commit — do not edit `caps.ts` or `api.ts` by hand.

## Secrets

Three deliberate postures, documented in `CLAUDE.md`: sealed-at-rest (`_enc` columns),
never-on-the-box (age backup keys), and plaintext-on-purpose (public certs, recipients).
Secrets never travel back to the client — expose `has_secret`-style booleans instead. Don't
weaken "the box cannot read its own backups."

## Pull requests

- Branch off `main`; PRs target `main`.
- Green CI is required: **Go tests**, **Postgres tests**, **Web build**, **Docker image**.
  The branch is protected (see below), so merges wait for all four plus one approval and
  resolved review threads.
- Keep commits focused; write messages for the operator who reads them later.
- Security issues: use a private advisory
  (`Security → Advisories → Report a vulnerability`), never a public issue.

## Maintainer notes

### Branch protection

`main` protection lives as two importable rulesets, split on purpose — a ruleset's
`bypass_actors` applies to the *whole* ruleset, so the bypassable and unconditional rules
have to live apart:

- **`protect-main.json`** — PR + one approval + required status checks. The **Admin**
  repository role (`actor_id: 5`) is a bypass actor, so the owner can push straight to
  `main` when needed.
- **`protect-main-history.json`** — blocks force-push (`non_fast_forward`) and branch
  deletion, with an empty bypass list. This applies to *everyone, including the owner* — no
  one rewrites or deletes `main`.

Apply each once via **Settings → Rules → Rulesets → New ruleset → Import a ruleset**, or
with the API:

```sh
gh api repos/Mnshahawy/daffa/rulesets --input .github/rulesets/protect-main.json
gh api repos/Mnshahawy/daffa/rulesets --input .github/rulesets/protect-main-history.json
```

Two things to keep honest:

- The `required_status_checks` contexts match the CI job names exactly — rename a job and
  you must update the ruleset, or protection silently stops enforcing that check.
- `actor_id: 5` is GitHub's built-in **Admin** role. If your import doesn't grant the
  bypass as expected, confirm the id with
  `gh api repos/Mnshahawy/daffa/rulesets/RULESET_ID` after importing and adjust.

### Cutting a release

Releases are manual. Run the **Release** workflow
(**Actions → Release → Run workflow**) with a version like `v1.2.3`. It:

1. Validates the version and that the tag is new.
2. Creates and pushes the annotated tag on the chosen commit.
3. Builds the multi-arch image (`linux/amd64`, `linux/arm64`) and pushes to
   `ghcr.io/mnshahawy/daffa` (`:vX.Y.Z`, `:X.Y`, and `:latest` unless it's a pre-release).
4. Opens a GitHub Release with generated notes.

Pushing to GHCR uses the built-in `GITHUB_TOKEN` — no PAT needed. After the first release,
set the package visibility to public under **Packages** if you want anonymous `docker pull`.
Pre-release versions (`v1.2.3-rc.1`) are tagged as pre-releases and skip `:latest`.
