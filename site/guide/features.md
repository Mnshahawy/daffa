# Features

The console is organised the way the job actually breaks down: what you came to **deploy**,
what you **operate** day to day, the **resources** underneath, and the **records** of what
happened. Settings sits alongside, for access, connections and alerts.

## Deploy

- **Stacks** — Compose projects and their sources. Git is the recommended source: the
  repository stays the source of truth and Daffa is the thing that executes it. Each stack
  declares its **engine** (Docker Compose today, Swarm later), and the engine decides which
  actions the stack even has. See [Stacks & deployments](/guide/stacks).
- **Deployments** — every attempt to change what is running, newest first, each with a URL
  you can send to someone. Includes the attempts that never reached a container. Cancel one
  that will not finish, or roll back to any deploy that succeeded.
- **Volume sources** — named volumes kept in sync from a git subtree: configuration, not
  data (Traefik dynamic config, provisioning files). Updated by webhook, the same way a
  stack is.

## Operate

- **Containers** — what is running, grouped by Compose project. Inspect, start, stop,
  restart, kill, pause and remove; follow logs; watch live CPU and memory; and open a shell
  straight to the daemon that can serve it — locally or on any agent host, through the same
  code path.
- **Services** — when the environment is a Swarm: what it is running, and *why a task will
  not start*. Sits next to Containers rather than in a separate "cluster" section, and is
  simply absent when the environment is not a Swarm.
- **Backups** — scheduled database backups and their snapshots. See [Backups](/guide/backups).

## Resources

The machinery underneath — where you go to inspect and reclaim, not every day.

- **Images** — pulled images and what still depends on them.
- **Volumes** — persistent data, and what is using it. Deleting one is a deliberate, single
  decision: a stack's `down` never passes `--volumes`.
- **Networks** — Docker networks and their attachments. The system networks (bridge, host,
  none) are Docker's, and Daffa leaves them alone.
- **Host** — the daemon, its version and its disk. Prunes tell you what they freed.

### Automatic cleanup

A fleet-wide schedule (Settings → Automatic cleanup), overridable per host, that sweeps
stopped containers, unused images, unused networks and build cache. Two things keep it
from being `docker system prune -a` on a timer:

- **An age floor.** Nothing created inside the window (a week by default) is touched, so
  the last few releases stay rollback-able.
- **Images it never removes.** List patterns, one per line, as `docker images` prints
  them: `myorg/ci-runner` keeps every tag of that repository, `ghcr.io/myorg/*` keeps
  everything under the prefix, `name@sha256:…` keeps exactly that content. Images you
  build can carry `LABEL daffa.keep=true` instead. Daffa's own deploy-runner image is
  always kept.

Daffa cannot tell a locally built image from a pulled one — under Docker's containerd
image store both carry a registry-style digest — so an image that only exists on the host
(a CI runner image, say) needs a pattern or the label to survive the sweep. Volumes are
never swept automatically.

## Records

- **Audit** — every mutating action, and every one *refused*. This is the view you want
  when you need to know who did what, and when.

## Settings

Grouped by the three questions Settings answers: who gets in, what Daffa can reach, and
when it should speak up.

### Access

- **Users** — who can sign in.
- **Roles** — what they are allowed to do, as sets of [capabilities](/guide/security#roles-and-capabilities).
- **Authentication** — local passwords and identity providers (any standards-compliant
  OIDC provider; more than one is fine).

### Connections

- **Hosts** — the machines Daffa manages (add an agent here).
- **Git** — access to your repositories, for stacks and volume sources.
- **Registries** — credentials for private images.
- **Storage** — S3-compatible buckets for backups.
- **Certificates** — internal CAs, the certificates they sign, and backup encryption keys.
- **Keyrings** — rotatable application encryption keys, delivered to hosts in volumes.

### Alerts

- **Resource monitors** — alert when a container stays over a CPU or memory line.
- **Container logs** — the default log driver and rotation applied to deployed stacks.
- **Notifications** — who gets told, and when.

Everything you can do is gated by a capability, and the console only shows what your roles
actually permit — a menu full of links that bounce you back where you came from is worse
than a short menu. See [Security & access](/guide/security).
