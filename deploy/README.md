# Deploying Daffa

A one-command install that brings up Daffa with a bundled PostgreSQL, and
optionally a Traefik reverse proxy that gets an automatic Let's Encrypt
certificate. Three files:

| File | What it is |
| --- | --- |
| `install.sh` | Installs Docker if missing, resolves the latest release, writes the stack into `/opt/daffa`, starts it, creates the first admin, and (internal mode) issues the edge certificate. |
| `docker-compose.yml` | The stack: Daffa + PostgreSQL, with Traefik behind a `traefik` compose profile. Driven entirely by `.env`. **The single source of truth** — the installer downloads it from the release tag. |
| `traefik.yml` | Traefik static config. Per-router TLS — one proxy serves public (Let's Encrypt) **and** internal (Daffa-delivered) sites at once. The installer appends the ACME resolver when an email is given. |
| `.env.example` | Every knob the compose file reads, documented. Copy to `.env` for the manual path. |

`install.sh` does **not** carry its own copy of the stack. It asks the GitHub
releases API for the latest tag, downloads `deploy/docker-compose.yml` from the
repo **at that tag**, and pins the image to the same tag — so an install is a
single, reproducible version across the compose file, the image, and the source.
Pin a specific release with `--version v1.2.3`; before the first release exists it
falls back to `main` and `:latest` with a warning.

## One command

**With HTTPS** (a domain that resolves to this host, ports 80/443 reachable):

```sh
curl -fsSL https://raw.githubusercontent.com/mnshahawy/daffa/main/deploy/install.sh \
  | sudo bash -s -- --domain daffa.example.com --acme-email you@example.com
```

**Internal domain** (private DNS, not reachable by Let's Encrypt — Daffa issues the cert
and prints a trust bundle):

```sh
curl -fsSL https://raw.githubusercontent.com/mnshahawy/daffa/main/deploy/install.sh \
  | sudo bash -s -- --domain daffa.internal --internal
```

**Localhost only** (no domain, plain http on `127.0.0.1:8080`):

```sh
curl -fsSL https://raw.githubusercontent.com/mnshahawy/daffa/main/deploy/install.sh | sudo bash
```

Run interactively (no flags) and it prompts for the domain, whether it's public, and the
email; leave the domain blank to get localhost mode. On success it prints the URL and, on
a fresh install, the generated admin password (and, for an internal domain, the trust
bundle to install on clients).

## Non-interactive (automation)

`--no-prompts` never asks anything — it uses flags and defaults only, and errors
if a required value (like `--acme-email` in TLS mode) is missing:

```sh
sudo ./install.sh --no-prompts \
  --domain daffa.example.com \
  --acme-email you@example.com \
  --admin-user ops \
  --admin-password 'a-strong-passphrase'
```

`install.sh --help` lists every flag.

## The modes

|  | Public TLS (`--domain`) | Internal TLS (`--domain --internal`) | Direct (no domain) |
| --- | --- | --- | --- |
| Proxy | Traefik on :80/:443 | Traefik on :80/:443 | none |
| Certificate | Let's Encrypt, auto-renewed | Daffa's internal CA, auto-renewed | none |
| Reachability | :443 must be public | private DNS is fine | `127.0.0.1:8080` |
| Extra step | — | install the printed trust bundle on clients | — |
| `DAFFA_SECURE_COOKIE` | `true` | `true` | `false` |

Public vs internal is chosen by `--internal` / `--public`, or prompted ("Is the
domain reachable from the public internet?"). Traefik-vs-direct is chosen by
`--traefik` / `--no-traefik`, or inferred from whether you gave a `--domain`.

Direct mode is for a local machine or when you run your own TLS terminator. Do
not expose a direct-mode install on a public interface — with Secure cookies off
and no TLS, the session cookie crosses the network in the clear.

### Internal domains and the trust bundle

With `--internal`, Daffa issues the console's certificate from an internal CA
(`daffa edge init` runs it) and delivers it into the `daffa-edge-certs` volume that
Traefik's file provider watches — renewals hot-reload, no restart. Because the CA is
Daffa's own, clients don't trust it yet: the installer writes the CA to
`/opt/daffa/ca-bundle.crt` and prints how to install it. On each machine that opens the
console:

```sh
# Linux
sudo cp ca-bundle.crt /usr/local/share/ca-certificates/daffa.crt && sudo update-ca-certificates
# macOS
sudo security add-trusted-cert -d -k /Library/Keychains/System.keychain ca-bundle.crt
```

The CA, its certificate, and the delivery are marked **protected** in the console —
they cannot be deleted from the UI, since removing them would take the console's own TLS
down. Re-run `install.sh` (or `docker compose exec daffa /usr/local/bin/daffa edge init
--domain <d> --volume daffa-edge-certs`) any time; it is idempotent.

### Managed as a stack

After bringing the deployment up, the installer registers it with Daffa as an **inline
stack** named `daffa` (`daffa stack adopt`) — the same object type Daffa manages for
anything else you deploy. So the console's stack features apply to Daffa's own deployment:
you see it in the stack list, drift detection watches it, and **you can change the domain
(or image, or any variable) in the UI and redeploy** — the redeploy runs `docker compose`
against the same `daffa` project.

Adoption records what's already running; it does not redeploy, and drift reads clean. A
couple of consequences worth knowing:

- The compose file uses **absolute** paths (`DAFFA_CONFIG_DIR`, default `/opt/daffa`) for
  its bind mounts, because a redeploy runs inside Daffa's ephemeral runner where a relative
  path would resolve to the wrong place. Keep the install directory where it is, or update
  `DAFFA_CONFIG_DIR` in `.env` if you move it.
- Redeploying the stack recreates Daffa's own container (and Traefik, Postgres). That is
  supported — the runner is supervised by the daemon, not by Daffa — but it is the console
  restarting itself, so expect a few seconds of downtime on a domain change.
- For an **internal** domain, changing the domain also means re-issuing the certificate:
  after the redeploy, re-run `docker compose exec daffa /usr/local/bin/daffa edge init
  --domain <new> --volume daffa-edge-certs`.

### Managing Traefik config

Because the stack is inline (no git repo), its Traefik configuration is delivered from
Daffa itself, via **inline volume sources** — the console's Volume sources page, with files
authored right there instead of synced from a repo. The installer sets two up:

- `daffa-traefik-config` → **`traefik.yml`** (the static config: entrypoints, providers,
  the ACME resolver). Edit it and **redeploy the stack** to apply — Traefik reads its static
  config once at startup.
- `daffa-edge-certs` → **`middlewares.yml`** and any other dynamic config (middlewares,
  routers, TLS options). These **hot-reload** — a sync applies them with no restart. This
  volume also holds the delivered certificates; each writer only ever touches its own files.

So adding a middleware is: edit `middlewares.yml` under Volume sources, sync. Changing an
entrypoint or the ACME email is: edit `traefik.yml`, then redeploy the `daffa` stack. No
SSH, no hand-editing files on the box.

Inline volume sources are a general feature — any inline stack can carry config files this
way, not just Daffa's own.

### Protected networks and volumes

The installer sets `DAFFA_SYSTEM_NETWORKS` and `DAFFA_SYSTEM_VOLUMES` to the stack's own
resources (the database volume, the edge-cert volume, the console's networks). Daffa marks
them `system` and refuses to remove them from the console — the same posture as Docker's
own bridge/host/none networks. Edit the lists in `.env` to protect more.

## The manual path

No `curl | bash` required:

```sh
cp .env.example .env
$EDITOR .env            # set POSTGRES_PASSWORD, DAFFA_DOMAIN/ACME_EMAIL or the direct-mode block, DOCKER_GID
docker compose up -d
# first admin (the binary path is required — docker exec bypasses the entrypoint):
docker compose exec daffa /usr/local/bin/daffa user add -u admin --role Admin
```

Find your Docker group id for `DOCKER_GID` with `getent group docker | cut -d: -f3`.

## Day two

Everything runs under the `daffa` compose project in the install directory
(`/opt/daffa` by default):

```sh
cd /opt/daffa
docker compose ps
docker compose logs -f daffa
docker compose down                             # stop (volumes are kept)
```

**Upgrading** — re-run `install.sh`. It re-resolves the latest release, re-fetches
that tag's compose file, re-points the image, pulls, and restarts — leaving your
`.env` secrets and admin account untouched. Pass `--version vX.Y.Z` to move to (or
stay on) a specific release instead of the latest.

The compose file in the install dir is a downloaded copy pinned to the installed
tag; `docker compose pull && up -d` there re-pulls that same tag. Use `install.sh`
to change which release is deployed.

## What to back up

Two Docker volumes hold everything that matters:

- `daffa-data` — the master key (`master.key`) that seals every secret Daffa
  stores. **Lose this and every registry password, IdP client secret, and other
  sealed value must be re-entered.**
- `daffa-pg` — the PostgreSQL data.

## Notes

- **The database is a convenience, not a requirement.** Daffa's own default is
  SQLite and it never provisions a database on its own; this template bundles
  Postgres because a turnkey install is expected to bring its own datastore. To
  use an existing cluster instead, point `DAFFA_DB_URL` at it in `.env` and
  delete the `postgres` service.
- **Managing this host's Traefik from inside Daffa.** Daffa can issue and deliver
  internal-CA certificates into volumes Traefik hot-reloads (see the certs
  feature). The Traefik here is a plain edge proxy for reaching the console; the
  two don't conflict, but if you later manage this proxy's config from Daffa,
  treat this compose file as the source of truth for how it starts.
