# Getting started

Daffa needs access to a Docker socket — that is the whole dependency. Three ways to run it:
the one-command installer, a plain `docker run`, or the single binary built from source.

## One-command install

On a fresh server this is the shortest path to a running console. The installer installs
Docker if it is missing, brings Daffa up behind [Traefik](https://traefik.io) with an
automatic Let's Encrypt certificate and a bundled PostgreSQL, and creates the first admin.

With HTTPS — point the domain's DNS at the host first, then:

```sh
curl -fsSL https://raw.githubusercontent.com/Mnshahawy/daffa/main/deploy/install.sh \
  | sudo bash -s -- --domain daffa.example.com --acme-email you@example.com
```

Traefik issues the certificate on the first request to `:443`.

Internal domain — a private hostname a public Let's Encrypt challenge can't reach. Daffa
issues the certificate from its own CA and prints a trust bundle to install on clients:

```sh
curl -fsSL https://raw.githubusercontent.com/Mnshahawy/daffa/main/deploy/install.sh \
  | sudo bash -s -- --domain daffa.internal --internal
```

Localhost only — no domain, plain http on `127.0.0.1:8080`:

```sh
curl -fsSL https://raw.githubusercontent.com/Mnshahawy/daffa/main/deploy/install.sh | sudo bash
```

Run it with no flags and it prompts; leave the domain blank for the localhost mode. Add
`--no-prompts` to rely on flags only for unattended automation.

The installer resolves the **latest release** and pins the compose file, the image and the
source to that one tag, so an install is reproducible; pass `--version vX.Y.Z` to choose a
specific one. It bundles PostgreSQL for convenience — Daffa itself never provisions a
database and is perfectly happy on SQLite (see [Configuration](#configuration)); set
`DAFFA_DB_URL` to point at an existing cluster instead. The compose template it uses, the
`.env` it writes, and the by-hand path are documented in
[`deploy/`](https://github.com/Mnshahawy/daffa/tree/main/deploy).

::: tip First account
In this flow the installer creates the admin and prints the generated password. The manual
paths below have no default credentials — you create the account yourself.
:::

With `--internal`, `daffa edge init` issues the console's certificate from an internal CA
and delivers it into a volume Traefik hot-reloads; the CA, its certificate, the delivery,
and the stack's own networks and volumes are all marked **protected** or **system**, so they
can't be deleted from the console — you can't take the console's own TLS or database down
with a click. Install the printed `ca-bundle.crt` on each machine that opens the console, or
browsers will warn about the unknown CA.

The installer also registers the deployment as Daffa's own **inline stack**, so it appears
in the stack list like anything else — change the domain (or any variable) in the UI and
redeploy, and Daffa applies it to itself. And the bundled Traefik is a per-router proxy:
this console and other public or internal sites on the same host coexist, each choosing its
own certificate strategy.

## Run with Docker

```sh
docker run -d --name daffa \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v daffa-data:/var/lib/daffa \
  --group-add "$(getent group docker | cut -d: -f3)" \
  -p 8080:8080 \
  ghcr.io/mnshahawy/daffa:latest
```

`--group-add` grants the container the host's `docker` group rather than running it as
root. The image is distroless and runs as a non-root user.

### docker compose

```yaml
services:
  daffa:
    image: ghcr.io/mnshahawy/daffa:latest
    restart: unless-stopped
    ports:
      - "8080:8080"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - daffa-data:/var/lib/daffa
    # The host's docker group id: `getent group docker | cut -d: -f3`
    group_add:
      - "999"

volumes:
  daffa-data:
```

## Create the first account

Daffa has no default credentials. Create the first administrator from the CLI — for a
container, exec the binary inside it:

```sh
docker exec -it daffa daffa user add -u admin --role Admin
```

Building from source, the Makefile wraps the same command as `make user`. Then open
<http://localhost:8080> and sign in.

## Build from source

```sh
make build     # compiles the SPA, embeds it, produces bin/daffa
make user      # create the first admin account (prompts for a password)
make dev       # serve on :8099 against the local Docker socket
```

`make dev` sets `DAFFA_SECURE_COOKIE=false`, needed *only* because a plain-http localhost
cannot carry a `Secure` cookie — the browser would drop it and you would never stay signed
in. In production, behind TLS, leave it alone. The dev port defaults to 8099 (8080 is
crowded on most dev machines); override with `make dev PORT=9000`.

## Behind a reverse proxy

Daffa holds the Docker socket, so it is as privileged as root on the host it manages. Run
it on a private network — a VPN or a tunnel — behind TLS, not on the public internet. It is
authenticated by default with no anonymous endpoint except `/healthz`. Keep
`DAFFA_SECURE_COOKIE=true` (the default) once you are on `https://`.

When a proxy you control terminates TLS in front of Daffa, set `DAFFA_TRUST_PROXY=true` so
the rightmost `X-Forwarded-For` entry is believed — that is what puts the real client
address in the audit log and the login rate limiter, rather than the proxy's. Leave it off
for a direct connection: an untrusted client can send any `X-Forwarded-For` it likes. The
one-command installer's Traefik mode sets this for you.

## Configuration

Everything is environment variables; nothing is baked in. The full list is on the
[Configuration reference](/reference/configuration). The essentials:

| Variable | Default | Meaning |
| --- | --- | --- |
| `DAFFA_ADDR` | `:8080` | Listen address |
| `DAFFA_DATA_DIR` | `/var/lib/daffa` | State directory (SQLite file, master key) |
| `DAFFA_DB_URL` | SQLite in `DATA_DIR` | `sqlite:///path/daffa.db` or `postgres://…` |
| `DAFFA_DOCKER_HOST` | `unix:///var/run/docker.sock` | Local Docker endpoint |
| `DAFFA_SECURE_COOKIE` | `true` | Set `false` only for http:// localhost |

### Using an existing PostgreSQL

Daffa never provisions or bundles a database server. Point it at a database that already
exists and it keeps its tables in their own `daffa` schema, so it can share a cluster with
anything else:

```sh
DAFFA_DB_URL="postgres://daffa:secret@db:5432/daffa?sslmode=require"
```

SQLite (the default) needs no configuration at all and is entirely adequate.

## Add another host

In the console, an admin adds an agent under **Settings → Hosts** and gets a one-time
command to run on the target machine:

```sh
daffa agent --server https://ops.example.com --token <join-token>
```

The agent enrolls, exchanges the join token for its own credential, and holds a tunnel
open — it dials *out*, so no inbound port is required. The host then appears in the
environment switcher and behaves exactly like the local one. The join token is single-use
and expires in 30 minutes; the agent needs the Docker socket and nothing else.
