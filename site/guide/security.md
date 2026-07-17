# Security & access

Daffa holds the Docker socket, so it is as privileged as root on the host it manages. It is
designed to run on a private network (a VPN, a tunnel), not on the public internet.
Concretely:

- authenticated by default, with no anonymous endpoint but `/healthz`;
- `viewer` is genuinely inert — view capabilities grant no actions;
- every mutation and every refusal is audited;
- sessions and break-glass tokens are stored only as hashes;
- secrets at rest are sealed with AES-256-GCM.

## Secrets

Daffa keeps secrets in one of three postures, on purpose:

- **Sealed at rest.** Every sealed column is AES-256-GCM under a master key at
  `<DataDir>/master.key`. Losing the key means re-entering secrets, not losing data. Sealed
  values are write-only through the API — the client sees a `has_secret` boolean, never the
  value.
- **Never on the box.** Age backup keys: the server stores public recipients only and
  actively rejects private keys. Decryption happens in the CLI on the operator's machine.
  "The box cannot read its own backups" is a stated invariant. See [Backups](/guide/backups).
- **Plaintext on purpose.** Public material — certificates, age recipients — is stored
  unsealed, and the code says so.

## Roles and capabilities

There are no fixed roles. A **role** is a set of **capabilities**, a user's permissions are
the union of every role they hold, and you build whatever roles you need under
**Settings → Roles**.

Every object has **view** and **edit**, and edit implies view:

```
containers  images  networks  volumes  stacks  backups  storage
gitcreds    hosts   registries  users   roles   settings    audit (view only)
```

Four capabilities are **granted separately and never implied by edit**:

| Capability | Why it is on its own |
| --- | --- |
| `containers.exec` | A shell in a container, on a root-owned socket — effectively root on the host. Being trusted to *restart* a service is not the same thing. |
| `system.prune` | Bulk, irreversible deletion across a whole host. |
| `backups.restore` | Overwrites a live database with an old one. |
| `backups.download` | Pulls the encrypted dump of an entire database out of the system. |

Three roles ship, and all of them are just presets you can change:

| Role | Holds |
| --- | --- |
| **Viewer** | Every `view`. No actions. |
| **Operator** | Runs the fleet: containers, stacks, backup jobs. **No shell, no prune, no restore.** |
| **Admin** | Everything, *including capabilities added in future versions*. Built in; cannot be narrowed or deleted. |

::: warning roles.edit is administrative
Anyone who can edit a role can grant themselves everything in it. Treat `roles.edit` as
admin, not as "just permissions management". Daffa also refuses to let you lock yourself
out — you cannot delete the last admin role, strip the last administrator, or disable them.
:::

## Authentication

Sign in with local username/password accounts, any standards-compliant OIDC provider, or
both. Identity providers are configured in the UI under **Settings → Authentication**, and
there may be more than one. Give a provider its issuer and Daffa discovers the rest; any
compliant one works (Zitadel, Keycloak, Authentik, Dex, Auth0) and nothing in the code knows
about a specific one. Client secrets are encrypted with the master key and cannot be read
back.

Each provider has a **slug**, which fixes its callback URL:

```
https://ops.example.com/api/auth/callback/<slug>
```

Map the provider's groups claim onto Daffa roles there too. Someone in several mapped groups
gets **all** of the roles those groups name — capabilities are a set, and they add up. Roles
the provider grants are re-synced on every sign-in; roles you grant **inside Daffa** are
yours and survive that re-sync.

Set `DAFFA_LOCAL_AUTH=false` to make SSO the only way in — but keep break-glass in mind,
because it becomes your only way back.

## Break-glass

If the identity provider is unreachable — quite possibly *because* the stack it runs in is
the one you are trying to fix — mint a single-use admin link from the box:

```sh
daffa admin-token -url https://ops.example.com
```

The token is stored hashed, expires in ten minutes, and dies on first use. Requiring shell
access is the authorization: that shell already reaches the Docker socket, which is
root-equivalent anyway.
