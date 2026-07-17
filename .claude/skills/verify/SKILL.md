---
name: verify
description: Build, launch and drive Daffa against the live local Docker daemon to verify changes end-to-end.
---

# Verifying Daffa against a live daemon

Build and bootstrap (password prompts read from stdin when piped; 12-char minimum):

```sh
S=$(mktemp -d)
go build -o $S/daffa ./cmd/daffa
printf 'verify-password-123\nverify-password-123\n' | DAFFA_DATA_DIR=$S/data $S/daffa user add -u admin --role Admin
DAFFA_ADDR=127.0.0.1:18080 DAFFA_DATA_DIR=$S/data $S/daffa serve > $S/server.log 2>&1 &
```

The local socket registers itself as environment "Local" on boot — no setup call. Sign in
and find its id:

```sh
curl -s -c $S/cookies -X POST http://127.0.0.1:18080/api/auth/login \
  -H 'Content-Type: application/json' -d '{"username":"admin","password":"verify-password-123"}'
curl -s -b $S/cookies http://127.0.0.1:18080/api/environments   # → env id
```

Gotchas learned the hard way:

- Mutating requests from curl work without an Origin header (CLI-style callers are
  allowed); the cookie jar is all you need.
- `stacks.Resolve`/`ResolveTree` accept a plain local filesystem path as `git_url` —
  a fixture repo in a temp dir works, including shallow clones. No git server needed.
- S3 surface: `docker run -d --name verify-minio -p 19000:9000 -e MINIO_ROOT_USER=… -e
  MINIO_ROOT_PASSWORD=… quay.io/minio/minio server /data`, bucket via a `quay.io/minio/mc`
  one-shot with `--network host`. Region `auto` is fine.
- The CLI (`daffa restore`) authenticates with `DAFFA_USER`/`DAFFA_PASSWORD` env vars;
  `--yes` skips the typed confirmation.
- Helper containers (`daffa.volsync` label) must be gone after any volume operation:
  `docker ps -aq --filter label=daffa.volsync` should be empty — leftovers mean a broken
  cleanup path.
- The runner image `docker:27-cli` has an entrypoint that rewrites commands that are
  docker subcommands (`rm` → `docker rm`). Helpers must set `Entrypoint`, not `Cmd`.
  This shipped as a real bug once.
- Webhook signatures: `printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET"` into
  `X-Hub-Signature-256: sha256=<hex>`.

Flows worth driving for volume changes: source sync then `docker run --rm -v <vol>:/data
alpine ls -la /data` (check modes and `.daffa-manifest`); mirror deletion with a
consumer-written file that must survive; the delete guard on a sourced volume; backup run
then `mc cat … | tar tzv`; restore refusals (mounted volume, non-empty without `--wipe`).

Tear down: kill the server pid, `docker rm -f verify-minio`, `docker volume rm` the
fixtures.
