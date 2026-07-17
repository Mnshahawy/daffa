---
aside: false
outline: false
---

# API reference

Everything the console does, it does over this HTTP API — and so can you. The base path is
`/api`. Authenticate with a **session cookie** (after signing in) or a **personal API token**
sent as a bearer token; mint one under your account in the console.

This page is generated from the OpenAPI document the server produces from its route table,
so it is always in step with the running code. Each operation names the **capability** it
requires and the **scope** at which that capability is checked — the same authorization rule
the server enforces. Operations marked *no capability required* still need a session or
token unless they are explicitly **public**.

<ApiReference />
