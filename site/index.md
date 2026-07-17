---
layout: home

hero:
  name: Daffa
  text: The helm for your Docker fleet
  tagline: A lean, self-hosted console for operating containers and deploying Compose stacks across one host or many. One static Go binary with the UI baked in — no telemetry, no licensed tier, Apache-2.0.
  image:
    src: /mark.svg
    alt: Daffa
  actions:
    - theme: brand
      text: Get started
      link: /guide/getting-started
    - theme: alt
      text: Features
      link: /guide/features
    - theme: alt
      text: API reference
      link: /reference/api
    - theme: alt
      text: GitHub
      link: https://github.com/Mnshahawy/daffa

features:
  - title: Deploy Compose stacks
    details: From a git repository or an inline file, with managed environment variables and registry credentials. Daffa detects drift from the source, runs your migration before and smoke test after, and can roll back on its own.
  - title: Operate containers
    details: Grouped by Compose project — inspect, start, stop, restart, kill, pause, remove; follow logs; watch live CPU and memory; and open a shell. Remote hosts behave exactly like the local one.
  - title: Manage many hosts
    details: An agent on each machine dials out to Daffa. No inbound port, no Docker socket on the network, nothing to punch through a NAT. Remote hosts run through the very same code, not a lesser tier.
  - title: Encrypted backups
    details: Scheduled database dumps streamed straight to any S3-compatible store, encrypted to an age public key so that Daffa itself cannot read them. Restore is a CLI command, because that is where the private key belongs.
  - title: Internal CA & certificates
    details: Create or import a root, issue certificates in a click, and let Daffa renew and deliver them into the volumes Traefik hot-reloads. CA rotation is a two-phase flow with an overlap window — no flag-day.
  - title: RBAC & audit
    details: Roles are sets of capabilities you compose yourself; a viewer is genuinely inert. Sign in with local accounts or any OIDC provider, with a break-glass link for the day your IdP is down. Every mutation, and every refusal, is audited.
---
