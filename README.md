<img src="web/public/mark.svg" alt="" width="72" align="left" hspace="16" vspace="4">

# Daffa

**دفّة** — the helm. A lean, self-hosted console for operating Docker containers and
deploying Compose stacks across one or many hosts.

<br clear="left">


Daffa exists because the obvious tools ask you to pay for the parts you need and carry
the parts you don't. Every feature here works in every copy: there is no licensed tier,
no feature flag waiting for a subscription, and no telemetry.

It is one static Go binary with the web UI compiled into it. No Node process runs in
production; no database server is required.

> **Status: M4 — feature complete.** Operations, Compose deployment, and encrypted
> database backups, across many hosts.

## What it does

- **Deploy Compose stacks** — from a git repository or an inline file, with managed
  environment variables and registry credentials. Daffa shows you when the source has
  drifted from what's actually running. Lifecycle hooks run your migration before the
  deploy and your smoke test after it — on both engines, including Swarm, where
  `docker stack deploy` alone can't sequence anything — and a failed smoke test can roll
  the stack back by itself. See `docs/hooks.md`.
- **Back up databases** — on a schedule, streamed straight from the database container
  to any S3-compatible store, encrypted so that Daffa itself cannot read them.
- **Operate containers** — grouped by Compose project: inspect, start/stop/restart/kill/
  pause/remove, follow logs, watch live CPU and memory, and open a shell.
- **Manage many hosts** — an agent on each machine dials *out* to Daffa. No inbound
  port, no Docker socket on the network, nothing to punch through a NAT. Remote hosts
  are not a lesser tier: they get containers, logs, exec, stats and deploys identically,
  because they run through the same code.
- **Reclaim disk** — images, volumes and networks annotated with what actually depends
  on them, plus prunes that tell you what they freed.
- **Run an internal CA** — create or import a root, issue certificates in a click, and let
  Daffa renew them automatically and deliver them into volumes Traefik hot-reloads. CA
  rotation is a two-phase flow with an overlap window, so both roots are trusted while you
  distribute the new one — no flag-day. See `docs/certs.md`.
- **Know who did what** — every mutating action, and every action *refused*, lands in an
  audit log.
- **Authenticate however you already do** — local username/password accounts, any
  standards-compliant OIDC provider, or both. Plus a single-use break-glass link from
  the shell for the day your identity provider is the thing that's down.

Deliberately out of scope: building images from source (your CI already does that),
DNS (your registrar already does that), public-CA certificates (your reverse proxy's ACME
already does that — Daffa manages *internal* CAs, the thing ACME can't), and Kubernetes.

## Stacks

A stack is a set of services deployed together from one compose file, on one host, under
one project name. Git is the recommended source: the repository stays the source of truth
and Daffa is only the thing that executes it.

Every stack says which **engine** applies it — `Docker Compose` today, Swarm later — and
the engine decides which actions the stack even has, so the UI never offers a button the
engine cannot honour. (Yes, a "stack" currently runs `docker compose`, not `docker stack`.
That is what the engine label is there to tell you, rather than making you guess.)

Deploys **do not run inside the Daffa process**. Daffa writes the compose file, a
rendered `.env`, and any registry credentials into a tar, copies it into a short-lived
`docker:cli` container on the target host, and lets that container run
`docker compose up`. Three things fall out of that:

- **Daffa can deploy the stack it is part of.** Recreating its own container mid-deploy
  doesn't kill the deploy — the runner is supervised by dockerd, not by Daffa. When
  Daffa comes back it finds the runner it left behind and reports how it ended.
- **No host needs a compose binary**, or the right version of one. The Compose version
  is pinned by Daffa, so a deploy behaves the same everywhere.
- **Remote hosts deploy through the tunnel** with no extra machinery: copying a tar into
  a container is just another Docker API call.

`down` removes containers and networks. It never passes `--volumes` — a stack's volumes
are someone's database, and deleting them is a decision that should be made explicitly,
one volume at a time, by a person who has been told what it is.

### Deployments

Every attempt to change what is running is a **deployment**, with a page of its own at
`/deployments/<id>` — a URL you can send to somebody. That includes the attempts that
never reached a container: a compose file that will not parse and a container that exits 1
are both just a failed deploy with a log, and they sit in the same list, read the same way.

A deployment records what it shipped (the git commit and its subject), what started it
(you, a push, or a rollback), how it went, and its full output. `/deployments` shows every
one across every stack and host — which is the view you want when you know something broke
but not yet where.

Two things you can do with one:

- **Cancel** a deploy that is not going to finish. It is stopped, not undone: whatever it
  already changed is still changed, and Daffa says so rather than implying otherwise. The
  stack is immediately deployable again.
- **Roll back** to any deploy that succeeded. Daffa re-applies the compose file stored on
  *that deployment* — it does not go back to git, so a moved branch, a deleted tag or an
  unreachable repo cannot stop you restoring what worked. Image tags come from that file,
  so a service on a floating tag like `:latest` will not truly roll back; pin your tags if
  you want this to mean what it says.

Old deployments are pruned: the last 50 per stack, plus anything from the last 90 days.

### Auto-deploy on push

A stack can redeploy itself when you push. It is **off until you turn it on**, per stack:
"the compose file changed" and "put this in production right now" are different
statements, and a tool that conflates them eventually deploys someone's half-finished
branch at 2am.

Turn it on in the stack's page and Daffa gives you a URL and a secret to paste into your
repository's webhook settings (Forgejo, Gitea, GitHub and GitLab are all understood). A
push then arrives signed, and Daffa deploys **only if all of these hold**:

- the signature verifies (HMAC-SHA256 over the exact body, with that stack's own secret),
- the push was to the branch the stack tracks — a push to a feature branch does nothing,
- and it changed a file the stack **watches**.

That last one is the useful part. By default a stack watches only its own compose file,
so a README typo will not redeploy anything. You can widen it with globs — one per line:

```
deploy/compose.yml
config/**
```

`*` stays within a path segment; `**` crosses them. The webhook endpoint is the only
route that can start a deploy without a session, so it sits outside the API, refuses
anything unsigned, and records every attempt — accepted or rejected — in the audit log.

**Known limitation:** relative bind mounts (`./data:/data`) are resolved by Compose
relative to the project directory, which for a runner-based deploy is a path inside the
runner rather than on the host. Use named volumes, or absolute host paths, instead.

## Managing another host

In the console, an admin adds an agent under **Agents** and gets a one-time command to
run on the target machine:

```sh
daffa agent --server https://ops.example.com --token <join-token>
```

That's it. The agent enrolls, exchanges the join token for its own credential, and holds
a tunnel open. The host appears in the environment switcher; everything Daffa does
locally, it now does there.

The join token is single-use and expires in 30 minutes. If the server is behind an
internal CA the host doesn't trust, add `--insecure`: the agent pins the certificate it
saw at enrollment and refuses to reconnect if it ever changes, which is the property
that actually matters. The agent needs the Docker socket (`--docker-host`, default
`unix:///var/run/docker.sock`) and nothing else — no database, no port, no certificate.

## Documentation

Full documentation — getting started, features, stacks, backups, security, and a generated
API reference — lives at **<https://mnshahawy.github.io/daffa/>**. The site source is in
[`site/`](site/).

## Install on a server

The shortest path to a running console on a real host is the installer. It installs Docker
if it is missing, brings Daffa up behind Traefik with an automatic Let's Encrypt
certificate and a bundled PostgreSQL, and creates the first admin:

```sh
curl -fsSL https://raw.githubusercontent.com/Mnshahawy/daffa/main/deploy/install.sh \
  | sudo bash -s -- --domain daffa.example.com --acme-email you@example.com
```

Point the domain at the host first; Traefik issues the certificate on the first request to
`:443`. Leave off `--domain` for a localhost-only install (plain http, no proxy — fine
behind a tunnel or your own edge), and add `--no-prompts` for unattended automation.

For a domain that isn't reachable by a public ACME challenge (a private hostname,
split-horizon DNS, an air-gapped network), add `--internal`: Daffa issues the console's
certificate from its own internal CA, delivers it into a volume Traefik hot-reloads, and
prints a CA trust bundle to install on client machines. The CA, certificate, delivery, and
the stack's own networks/volumes are all marked **protected** — they cannot be deleted from
the console, so nobody takes the console's own TLS or database down with a click.

The installer also registers the deployment with Daffa as its own **inline stack**, so it
shows up like any other and you can change the domain (or any variable) from the console and
redeploy — Daffa manages its own stack. The bundled Traefik is a per-router edge proxy: it
serves this console *and* other public or internal sites on the box side by side, each
picking its own certificate strategy.

The installer resolves the **latest release** and pins the compose file, the image and the
source to that one tag, so an install is reproducible; `--version vX.Y.Z` picks a specific
one. It bundles PostgreSQL for convenience — Daffa itself never provisions a database and is
perfectly happy on SQLite (see [Configuration](#configuration)); set `DAFFA_DB_URL` to point
at an existing cluster instead. The compose template it uses — Daffa + Postgres, Traefik on
an opt-in profile, over two segmented networks — and the manual path both live in
[`deploy/`](deploy/).

## Quick start

```sh
make build     # compiles the SPA, embeds it, produces bin/daffa
make user      # create the first admin account (prompts for a password)
make dev       # serve on :8099 against the local Docker socket
```

Then open <http://localhost:8099>.

`make dev` sets `DAFFA_SECURE_COOKIE=false`, which is needed *only* because a plain-http
localhost cannot carry a `Secure` cookie — the browser would silently drop it and you
would never stay signed in. In production, behind TLS, leave it alone.

The port defaults to 8099 rather than 8080, which is a crowded port on most development
machines. Override it with `make dev PORT=9000`.

With Docker:

```sh
docker run -d --name daffa \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v daffa-data:/var/lib/daffa \
  --group-add "$(getent group docker | cut -d: -f3)" \
  -p 8080:8080 \
  ghcr.io/mnshahawy/daffa:latest
```

## Configuration

Everything is environment variables; nothing is baked in.

| Variable | Default | Meaning |
| --- | --- | --- |
| `DAFFA_ADDR` | `:8080` | Listen address |
| `DAFFA_DATA_DIR` | `/var/lib/daffa` | State directory (SQLite file, master key) |
| `DAFFA_DB_URL` | SQLite in `DATA_DIR` | `sqlite:///path/daffa.db` or `postgres://…` |
| `DAFFA_DOCKER_HOST` | `unix:///var/run/docker.sock` | Local Docker endpoint |
| `DAFFA_MASTER_KEY_FILE` | `$DATA_DIR/master.key` | 32-byte key for secrets at rest; generated on first run |
| `DAFFA_SECURE_COOKIE` | `true` | Set `false` only for http:// localhost |
| `DAFFA_TRUST_PROXY` | `false` | Believe `X-Forwarded-For` from a proxy you run — on behind the installer's Traefik, off for a direct connection |
| `DAFFA_SESSION_TTL` | `12h` | Session lifetime |
| `DAFFA_LOCAL_AUTH` | `true` | Allow username/password sign-in |
| `DAFFA_SYSTEM_NETWORKS` | — | Comma-separated networks marked `system` (refused for removal), on top of bridge/host/none |
| `DAFFA_SYSTEM_VOLUMES` | — | Comma-separated volumes marked `system` (refused for removal) |

### Using an existing PostgreSQL

Daffa never provisions or bundles a database server. Point it at a database that already
exists and it will keep its tables in their own `daffa` schema, so it can share a cluster
with anything else:

```sh
DAFFA_DB_URL="postgres://daffa:secret@db:5432/daffa?sslmode=require"
```

SQLite (the default) needs no configuration at all and is entirely adequate — Daffa's
state is a few thousand rows.

### Identity providers

Identity providers are **not** environment variables. They are configured in the UI, under
**Settings → Authentication**, and **there may be more than one** — a company IdP for staff
and a second for contractors, say. Give a provider its issuer and Daffa discovers the rest;
any compliant one works (Zitadel, Keycloak, Authentik, Dex, Auth0) and nothing in the code
knows about a specific one. Client secrets are encrypted with the master key and cannot be
read back.

Each provider has a **slug**, which fixes its callback URL:

```
https://ops.example.com/api/auth/callback/<slug>
```

Register that exact string with the provider. The settings page shows it for you.

Map the provider's groups claim onto Daffa roles there too. The claim may be a list of
strings, a single string, or an object keyed by role name (which is what Zitadel emits) —
all three are understood. Someone in several mapped groups gets **all** of the roles those
groups name: capabilities are a set, and they add up. There is no "highest" role.

Roles the provider grants are re-synced on every sign-in, so removing someone from a group
takes effect the next time they log in. Roles you grant **inside Daffa** are yours and
survive that re-sync.

A user whose claims map to nothing, at a provider with no default role, is **refused** at
sign-in with a message saying why. Handing them a session with no permissions would render
an empty application, which reads as a bug rather than as a decision.

> A fresh Daffa has no identity provider until someone signs in locally and adds one.
> `daffa user add` and break-glass cover that.

Set `DAFFA_LOCAL_AUTH=false` to make SSO the only way in — but keep break-glass in mind,
because it becomes your only way back.

### Roles and capabilities

There are no fixed roles. A **role** is a set of **capabilities**, a user's permissions are
the union of every role they hold, and you build whatever roles you need under
**Settings → Roles**.

Every object has **view** and **edit**, and edit implies view:

```
containers  images  networks  volumes  stacks  backups  storage
gitcreds    hosts   registries  users   roles   settings    audit (view only)
```

Four capabilities are **granted separately and are never implied by edit**:

| Capability | Why it is on its own |
| --- | --- |
| `containers.exec` | A shell in a container, on a socket that runs as root — effectively root on the host. Being trusted to *restart* a service is not the same thing. |
| `system.prune` | Bulk, irreversible deletion across a whole host. |
| `backups.restore` | Overwrites a live database with an old one. |
| `backups.download` | Pulls the encrypted dump of an entire database out of the system. |

Three roles ship, and all of them are just presets you can change:

| Role | Holds |
| --- | --- |
| **Viewer** | Every `view`. No actions. |
| **Operator** | Runs the fleet: containers, stacks, backup jobs. **No shell, no prune, no restore** — add them deliberately if you want them. |
| **Admin** | Everything, *including capabilities added in future versions*. Built in, cannot be narrowed or deleted. |

`roles.edit` is administrative in the fullest sense: anyone who can edit a role can grant
themselves everything in it. Treat it as admin, not as "just permissions management".

Daffa refuses to let you lock yourself out — you cannot delete the last admin role, strip
the last administrator, or disable them.

### Break-glass

If the identity provider is unreachable — quite possibly *because* the stack it runs in
is the one you're trying to fix — mint a single-use admin link from the box:

```sh
daffa admin-token -url https://ops.example.com
```

The token is stored hashed, expires in ten minutes, and dies on first use. Requiring
shell access is the authorization: that shell already reaches the Docker socket, which
is root-equivalent anyway.

## Backups

A backup job runs `pg_dumpall` (or `mysqldump`, or `mongodump`) **inside the database
container** and streams the result out:

```
docker exec pg_dumpall  →  gzip  →  age  →  S3 multipart upload
```

Nothing is buffered to disk at either end and memory stays flat, so a 100 GB database
backs up from a box whose disk is already full of that database.

### Encryption, and why restore is a CLI command

By default a snapshot is encrypted to an **age public key**. Daffa holds only the public
key, which means **Daffa cannot read its own backups** — and neither can anyone who
steals the bucket, or the server, or both. That property is worth more than it sounds:
an attacker who compromises the box does not thereby inherit every snapshot you have ever
taken, including the ones from before they arrived.

Keys are managed under **Settings → Certificates**. Generate one there and Daffa creates
the keypair in memory, hands you the private half as a one-time download, and stores only
the public half — the download screen will not let you past until you have the file. Or
generate your own and import just the public key:

```sh
age-keygen -o key.txt      # prints the public key; store key.txt safely OFF this box
```

Backup jobs then *select* named keys instead of having recipients pasted into them. Pick
two — a personal key and a break-glass key held somewhere independent — and every snapshot
is encrypted to both; any one private key restores.

That is also why **restore is a CLI command and not a button**. Restoring needs the
private key. If the web UI asked you to paste it in, the key would reach the server, and
the whole guarantee above would evaporate. So the console shows you the exact command and
the CLI does the decryption locally:

```sh
daffa restore --server https://ops.example.com \
  --job <job-id> --snapshot <key> --user you --identity ~/key.txt
```

Daffa streams the *ciphertext* down to your machine, your machine decrypts it, and the
plaintext is streamed back into the database container. It asks you to type the job name
before it overwrites anything, and the restore is audited at both ends.

You can also turn encryption **off** (`encryption: none`), which stores a plain gzip
dump. Restore then needs no key at all. The trade is exactly what it sounds like: anyone
who can read the bucket can read your database. The UI labels those jobs as unencrypted.

Deleting a backup job stops future backups. It never deletes snapshots that already
exist — Daffa does not touch your bucket's contents. Use your storage provider's
lifecycle rules for retention.

## Security

Daffa holds the Docker socket, so it is as privileged as root on the host it manages.
It is designed to be run on a private network (a VPN, a tunnel), not on the public
internet. Concretely: authenticated by default with no anonymous endpoint but
`/healthz`; `viewer` is genuinely inert; every mutation and every refusal is audited;
sessions and break-glass tokens are stored only as hashes; secrets at rest are sealed
with AES-256-GCM.

## Development

```sh
make test       # unit tests (SQLite only)
make test-pg    # tests against BOTH dialects; spins up a throwaway Postgres, then removes it
make lint       # go vet + vue-tsc
```

There are two dev loops, and which one you want depends on what you're changing.

**Working on the backend** — `make dev` rebuilds the SPA, embeds it, and serves
everything from the one binary on :8099. This is exactly what production runs, so it is
the loop to trust when you care about the real thing. Re-run it after a change.

**Working on the frontend** — leave `make dev` running in one terminal, and in another:

```sh
cd web && pnpm dev        # Vite on :5173, hot reload
```

Open <http://localhost:5173>. Vite serves the UI and proxies `/api` to the binary on
:8099, so the session cookie is same-origin and behaves exactly as it does in production.
(If you moved the server port, match it: `DAFFA_PORT=9000 pnpm dev`.)

A note on the Docker socket: Daffa defaults to `unix:///var/run/docker.sock`, which works
on Linux and on macOS with Docker Desktop (it symlinks that path). Colima, Rancher Desktop
and rootless Docker put the socket elsewhere — point `DAFFA_DOCKER_HOST` at whatever
`docker context inspect --format '{{.Endpoints.docker.Host}}'` prints.

## Roadmap

- ~~**M1** — exec terminal, live stats, images/volumes/networks, prune.~~ Done.
- ~~**M2** — remote hosts via an out-dialing agent.~~ Done.
- ~~**M3** — Compose stacks from git or an inline file, with env vars, registries and
  drift detection.~~ Done.
- ~~**M4** — scheduled database backups, streamed and encrypted to any S3-compatible
  store, with a client-side restore.~~ Done.

Beyond that: Swarm visibility, filesystem/volume backups, per-environment roles.
Nothing is planned that would put a feature behind a paywall — there isn't one.

## Licence

Apache 2.0. See [LICENSE](LICENSE).
