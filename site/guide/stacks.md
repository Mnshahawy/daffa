# Stacks & deployments

A stack is a set of services deployed together from one compose file, on one host, under one
project name. Git is the recommended source: the repository stays the source of truth and
Daffa is only the thing that executes it.

Every stack says which **engine** applies it — `Docker Compose` today, Swarm later — and the
engine decides which actions the stack even has, so the UI never offers a button the engine
cannot honour.

## Deploys run outside the Daffa process

Daffa writes the compose file, a rendered `.env`, and any registry credentials into a tar,
copies it into a short-lived `docker:cli` container on the target host, and lets that
container run `docker compose up`. Three things follow:

- **Daffa can deploy the stack it is part of.** Recreating its own container mid-deploy does
  not kill the deploy — the runner is supervised by dockerd, not by Daffa. When Daffa comes
  back it finds the runner it left behind and reports how it ended.
- **No host needs a compose binary**, or the right version of one. The Compose version is
  pinned by Daffa, so a deploy behaves the same everywhere.
- **Remote hosts deploy through the tunnel** with no extra machinery: copying a tar into a
  container is just another Docker API call.

::: warning Relative bind mounts
`down` removes containers and networks. It never passes `--volumes` — a stack's volumes are
someone's database. And relative bind mounts (`./data:/data`) resolve, for a runner-based
deploy, to a path inside the runner rather than on the host. Use named volumes or absolute
host paths instead.
:::

## Deployments

Every attempt to change what is running is a **deployment**, with a page of its own at
`/deployments/<id>` — a URL you can send to somebody. That includes attempts that never
reached a container: a compose file that will not parse and a container that exits 1 are
both a failed deploy with a log, and they sit in the same list, read the same way.

A deployment records what it shipped (the git commit and its subject), what started it (you,
a push, or a rollback), how it went, and its full output.

- **Cancel** a deploy that is not going to finish. It is stopped, not undone: whatever it
  already changed is still changed, and Daffa says so. The stack is immediately deployable
  again.
- **Roll back** to any deploy that succeeded. Daffa re-applies the compose file stored on
  *that deployment* — it does not go back to git, so a moved branch or deleted tag cannot
  stop you restoring what worked. Image tags come from that file, so pin your tags if you
  want a rollback to mean what it says.

Old deployments are pruned: the last 50 per stack, plus anything from the last 90 days.

## Auto-deploy on push

A stack can redeploy itself when you push. It is **off until you turn it on**, per stack:
"the compose file changed" and "put this in production right now" are different statements.

Turn it on in the stack's page and Daffa gives you a URL and a secret to paste into your
repository's webhook settings (Forgejo, Gitea, GitHub and GitLab are all understood). A push
then deploys **only if all of these hold**:

- the signature verifies (HMAC-SHA256 over the exact body, with that stack's own secret),
- the push was to the branch the stack tracks, and
- it changed a file the stack **watches**.

By default a stack watches only its own compose file, so a README typo will not redeploy
anything. Widen it with globs, one per line:

```
deploy/compose.yml
config/**
```

`*` stays within a path segment; `**` crosses them. The webhook endpoint is the only route
that can start a deploy without a session, so it refuses anything unsigned and records every
attempt — accepted or rejected — in the audit log.

## Lifecycle hooks

Hooks run your migration before the deploy and your smoke test after it — on both engines,
including Swarm, where `docker stack deploy` alone cannot sequence anything. A failed smoke
test can roll the stack back by itself.
