# Introduction

**Daffa** (دفّة, "the helm") is a self-hosted console for operating Docker containers and
deploying Compose stacks across one host or many. It is one static Go binary with the Vue
web UI compiled into it — no Node process runs in production, and no database server is
required.

Daffa exists because the obvious tools ask you to pay for the parts you need and carry the
parts you don't. Every feature here works in every copy: there is no licensed tier, no
feature flag waiting for a subscription, and no telemetry.

::: tip Status
**M4 — feature complete.** Operations, Compose deployment, and encrypted database backups,
across many hosts.
:::

## What it does

- **Deploy Compose stacks** from a git repository or an inline file, with managed
  environment variables and registry credentials, drift detection, lifecycle hooks, and
  self-rollback. See [Stacks & deployments](/guide/stacks).
- **Operate containers** grouped by Compose project: inspect, start/stop/restart/kill/
  pause/remove, follow logs, watch live CPU and memory, and open a shell.
- **Manage many hosts** with an agent that dials *out* — no inbound port, no Docker socket
  on the network. Remote hosts get containers, logs, exec, stats and deploys identically.
- **Back up databases** on a schedule, streamed straight from the database container to any
  S3-compatible store, encrypted so that Daffa itself cannot read them. See
  [Backups](/guide/backups).
- **Run an internal CA**: issue certificates, renew them automatically, and deliver them
  into volumes a reverse proxy hot-reloads, with two-phase CA rotation.
- **Know who did what** — every mutating action, and every action *refused*, lands in an
  audit log.
- **Authenticate however you already do** — local accounts, any standards-compliant OIDC
  provider, or both, plus a single-use break-glass link. See [Security & access](/guide/security).

## Deliberately out of scope

Building images from source (your CI already does that), DNS (your registrar already does
that), public-CA certificates (your reverse proxy's ACME already does that — Daffa manages
*internal* CAs, the thing ACME can't), and Kubernetes.

## How it's built

- **One binary, your data.** The SPA is compiled into the Go binary; the shipped image
  carries neither Node nor a shell. SQLite by default — Daffa's state is a few thousand
  rows — or point it at a Postgres you already run. It never provisions a database.
- **Nothing long-running executes in-process.** Deploys run in a short-lived `docker:cli`
  runner supervised by dockerd, so Daffa can deploy the very stack it is part of and
  survive recreating its own container mid-deploy.
- **Secrets have three postures, on purpose** — sealed at rest, never on the box, or
  plaintext because the material is public. See [Security & access](/guide/security#secrets).

Ready? Head to [Getting started](/guide/getting-started).
