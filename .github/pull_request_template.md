<!--
Daffa lands features as a spine: migration → store → caps → routes/handlers → web,
together. If your change touches one layer, say why the others don't apply.
-->

## What & why

<!-- What does this change do, and what problem does it solve? Link issues: Closes #123 -->

## The spine

<!-- Tick what this change touches; delete the line if it genuinely doesn't apply. -->

- [ ] Migration (`internal/store/migrate.go`) — append-only, tested against a populated older DB
- [ ] Store entity (`internal/store/`) — `Enc`-sealed fields, `List` filters (never gates)
- [ ] Capability (`internal/caps/caps.go`) — registered twice, `go generate ./internal/caps` run
- [ ] Route + handler (`internal/api/server.go`) — declares `cap` + `scope` or `open` reason, audits mutations
- [ ] Web (`web/src/`) — typed client, nav, route `meta.cap`, buttons gate on `session.can()`
- [ ] Docs (`docs/`) — design doc for a substantial feature

## Checklist

- [ ] `go build ./...` and `go test ./...` pass
- [ ] `go vet ./...` clean and files are `gofmt`-ed
- [ ] Generated files committed (`caps.ts`, `api.ts`, `openapi.json`, `caps_golden.json`) — not hand-edited
- [ ] `cd web && pnpm build` passes (vue-tsc typecheck + vite build)
- [ ] Secrets stay server-side (`_enc` sealed; client sees `has_secret`-style booleans only)

## Notes for reviewers

<!-- Anything non-obvious: a design tradeoff, a deliberate non-change, a follow-up deferred. -->
