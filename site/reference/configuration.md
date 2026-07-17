# Configuration

Everything is environment variables; nothing is baked in. Identity providers and most
operational settings are configured in the UI, not here — see [Security & access](/guide/security).

| Variable | Default | Meaning |
| --- | --- | --- |
| `DAFFA_ADDR` | `:8080` | Listen address |
| `DAFFA_DATA_DIR` | `/var/lib/daffa` | State directory (SQLite file, master key) |
| `DAFFA_DB_URL` | SQLite in `DATA_DIR` | `sqlite:///path/daffa.db` or `postgres://…` |
| `DAFFA_DOCKER_HOST` | `unix:///var/run/docker.sock` | Local Docker endpoint |
| `DAFFA_MASTER_KEY_FILE` | `$DATA_DIR/master.key` | 32-byte key for secrets at rest; generated on first run |
| `DAFFA_SECURE_COOKIE` | `true` | Set `false` only for http:// localhost |
| `DAFFA_TRUST_PROXY` | `false` | Believe `X-Forwarded-For` from a proxy you run — on behind a reverse proxy (e.g. the installer's Traefik), off for a direct connection |
| `DAFFA_SESSION_TTL` | `12h` | Session lifetime |
| `DAFFA_LOCAL_AUTH` | `true` | Allow username/password sign-in |
| `DAFFA_SYSTEM_NETWORKS` | — | Comma-separated networks marked `system` (removal refused), on top of bridge/host/none |
| `DAFFA_SYSTEM_VOLUMES` | — | Comma-separated volumes marked `system` (removal refused) |

## Storage

- **SQLite** (the default) needs no configuration at all and is entirely adequate — Daffa's
  state is a few thousand rows. The file lives in `DAFFA_DATA_DIR`.
- **PostgreSQL**: point `DAFFA_DB_URL` at a database that already exists. Daffa never
  provisions or bundles a database server; it keeps its tables in their own `daffa` schema,
  so it can share a cluster with anything else.

```sh
DAFFA_DB_URL="postgres://daffa:secret@db:5432/daffa?sslmode=require"
```

## The master key

`DAFFA_MASTER_KEY_FILE` seals every secret at rest with AES-256-GCM. It is generated on
first run if absent. **Back it up**: losing it means re-entering every stored secret
(registry passwords, OIDC client secrets, git credentials), though it does not lose any
other data. It is never required to read backups — those use separate age keys held off the
box. See [Security & access](/guide/security#secrets).

## Protected resources

`DAFFA_SYSTEM_NETWORKS` and `DAFFA_SYSTEM_VOLUMES` name Docker resources the deployment
depends on — its own database volume, the edge-certificate volume, the networks the console
sits on. Daffa marks them `system` in the API and refuses to remove them, the same way it
already refuses Docker's own bridge/host/none networks. The one-command installer fills these
in for the stack it writes; add your own, comma-separated, to protect more.

This is distinct from a CA, certificate, or delivery being **protected** — that is a flag on
the row (set by `daffa edge init`), not configuration. Both refusals share one goal: the
console cannot be used to delete the plumbing that keeps the console itself running.

## The Docker socket

Daffa defaults to `unix:///var/run/docker.sock`, which works on Linux and on macOS with
Docker Desktop. Colima, Rancher Desktop and rootless Docker put the socket elsewhere — point
`DAFFA_DOCKER_HOST` at whatever this prints:

```sh
docker context inspect --format '{{.Endpoints.docker.Host}}'
```
