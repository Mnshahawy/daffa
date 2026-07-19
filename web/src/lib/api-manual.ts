// The hand-written part of the API client: methods whose ergonomics the generator cannot
// reproduce — query-string builders, splat and default args — plus their types. It only
// shrinks: a route that declares req/resp/ts in internal/api/server.go's table gets a
// generated method instead (see docs/openapi.md).
//
// Two rules keep the api ⇄ api-manual import cycle safe:
//   1. Nothing but the generated api.ts may import this file.
//   2. This file must never reference `api` at module top level — only inside method
//      bodies, which defer evaluation until after both modules have initialized.
//
// To migrate a method: declare req/resp/ts on its route, DELETE the copies here (the
// generator refuses to run while both exist), and `go generate ./internal/api`.

import type { CapSet } from './caps'
// Type-only: migrated interfaces are referenced back from the generated file. A type
// import adds no runtime edge, so the evaluation-order story in the header holds.
import type { CertAuthority, Certificate, Deployment, EnvVarItem, NewAgent, StackSecretItem, Stats, User } from './api'
import { api } from './api'
import { nodeQuery } from './api-helpers'

/** A role a user holds, and where it applies. */
export interface RoleHeld {
  name: string
  scope: 'global' | 'env'
  env_name?: string
}

export interface Me {
  id: string
  label: string
  email: string
  kind: 'local' | 'oidc'
  roles: RoleHeld[]
  is_admin: boolean
  /**
   * What they hold EVERYWHERE — one mask per functional area. Test it with session.can(),
   * never by indexing it yourself: the bit numbers are only meaningful within their area.
   */
  caps: CapSet
  /** What they hold on each particular host, on top of caps. */
  caps_by_env: Record<string, CapSet>
  /** The global set, spelled out. For display and support, never for decisions. */
  caps_names: string[]
}

export interface AuthConfig {
  local_auth: boolean
  /** One sign-in button per enabled identity provider. There may be several. */
  providers: { slug: string; name: string }[]
}

export type PruneTarget = 'images' | 'containers' | 'networks' | 'volumes' | 'build-cache'

export type DeploymentStatus = 'running' | 'ok' | 'failed' | 'cancelled'

/** Narrows the cross-stack feed. Every field is optional. */
export interface DeploymentFilter {
  status?: DeploymentStatus
  stack?: string
  host?: string
  trigger?: string
}

/**
 * What an engine can do to a stack.
 *
 * `down+volumes` is not a button anybody presses — it is a CAPABILITY, and it is in this union so
 * the UI can ask the engine whether the "also delete its volumes" checkbox should even be drawn.
 * Compose can. Swarm cannot: a swarm stack's volumes are node-local and the manager has no
 * authority over them, so a checkbox there would silently do nothing.
 */
export type StackAction = 'up' | 'pull' | 'stop' | 'down' | 'restart' | 'down+volumes'

// ── volume sources ─────────────────────────────────────────────────────────────

// ── certificates & encryption keys ─────────────────────────────────────────────

// ── keyrings ───────────────────────────────────────────────────────────────────

// ── API tokens ─────────────────────────────────────────────────────────────────

export type ContainerAction =
  | 'start'
  | 'stop'
  | 'restart'
  | 'kill'
  | 'pause'
  | 'unpause'
  | 'remove'

// ── users, roles, identity providers ──────────────────────────────────────────

/** A role granted at a scope. env_id empty ⇒ everywhere. */
export interface Grant {
  role_id: string
  env_id: string
}

// ── resource monitors ─────────────────────────────────────────────────────────

export interface MetricPoint {
  ts: string
  cpu_avg: number
  cpu_max: number
  mem_avg: number
  mem_max: number
  mem_pct: number
  mem_limit: number
}

export type MetricRange = '1h' | '6h' | '24h' | '7d'

export const manualDaffa = {
  authConfig: () => api.get<AuthConfig>('/api/auth/config'),

  me: () => api.get<Me>('/api/auth/me'),
  login: (username: string, password: string) =>
    api.post<Me>('/api/auth/login', { username, password }),
  setUserPassword: (id: string, password: string) =>
    api.put<{ status: string }>(`/api/users/${id}/password`, { password }),
  setUserRoles: (id: string, grants: Grant[]) =>
    api.put<User>(`/api/users/${id}/roles`, { grants }),

  renameEnvironment: (id: string, name: string) =>
    api.patch<{ id: string; name: string }>(`/api/clusters/${id}`, { name }),
  scaleService: (env: string, id: string, replicas: number) =>
    api.post(`/api/clusters/${env}/services/${id}/scale`, { replicas }),
  removeNode: (env: string, id: string, force = false) =>
    api.del(`/api/clusters/${env}/nodes/${id}${force ? '?force=true' : ''}`),

  swarmInit: (env: string, advertiseAddr = '') =>
    api.post<{ node_id: string }>(`/api/clusters/${env}/swarm/init`, {
      advertise_addr: advertiseAddr,
    }),
  swarmLeave: (env: string, force = false) =>
    api.post(`/api/clusters/${env}/swarm/leave${force ? '?force=true' : ''}`),
  /**
   * A container is NODE-LOCAL: its id is unique per daemon, not per cluster, so on a Swarm every
   * one of these has to say which machine it means.
   *
   * The list already tags each row with its node, so the caller always has the value — and on a
   * standalone environment (and a single-node swarm) it is omitted entirely, because there is only
   * one possible answer and the server fills it in. The rule is arity, not kind.
   *
   * This is also the whole of cross-node exec. Portainer needed a gossiping agent mesh and two HTTP
   * headers to reach a container on another machine; here the server already holds a tunnel per
   * node, so naming the node IS the routing.
   */
  container: (env: string, id: string, node?: string) =>
    api.get<Record<string, unknown>>(
      `/api/clusters/${env}/containers/${id}${nodeQuery(node)}`,
    ),
  action: (env: string, id: string, action: ContainerAction, force = false, node?: string) =>
    api.post<{ status: string }>(
      `/api/clusters/${env}/containers/${id}/${action}${nodeQuery(node, force ? { force: 'true' } : undefined)}`,
    ),

  // The client names the containers to sample — it is the only party that knows what
  // is actually on screen.
  stats: (env: string, ids: string[], node?: string) =>
    api.get<Stats[]>(
      `/api/clusters/${env}/stats${nodeQuery(node, { ids: ids.join(',') })}`,
    ),

  removeImage: (env: string, id: string, force = false) =>
    api.del(`/api/clusters/${env}/images/${encodeURIComponent(id)}${force ? '?force=true' : ''}`),

  removeVolume: (env: string, name: string, force = false) =>
    api.del(
      `/api/clusters/${env}/volumes/${encodeURIComponent(name)}${force ? '?force=true' : ''}`,
    ),

  removeNetwork: (env: string, id: string) =>
    api.del(`/api/clusters/${env}/networks/${encodeURIComponent(id)}`),

  /**
   * Remove a stack AND what it deployed.
   *
   * `volumes` also destroys the stack's named volumes — that is data, and it is why it is a
   * separate flag rather than part of what delete quietly means.
   *
   * `force` skips the teardown and only forgets the stack. It exists for one case: the host is
   * gone, so the cleanup cannot run, and without it the stack could never be removed at all.
   */
  deleteStack: (id: string, opts: { volumes?: boolean; force?: boolean } = {}) => {
    const q = new URLSearchParams()
    if (opts.volumes) q.set('volumes', 'true')
    if (opts.force) q.set('force', 'true')
    const qs = q.toString()
    return api.del(`/api/stacks/${id}${qs ? `?${qs}` : ''}`)
  },
  setStackEnv: (id: string, vars: EnvVarItem[]) => api.put(`/api/stacks/${id}/env`, { vars }),
  setStackSecrets: (id: string, secrets: StackSecretItem[]) =>
    api.put(`/api/stacks/${id}/secrets`, { secrets }),

  // ── deployments ───────────────────────────────────────────────────────────────
  //
  // A deployment is addressed by its own id, not nested under its stack, so it has ONE canonical
  // URL — the thing you can paste into a message. Both the cross-stack feed and a stack's own
  // history link to the same page.

  /** Every stack's, newest first — for when you know something broke but not yet where. */
  deployments: (f: DeploymentFilter = {}) => {
    const q = new URLSearchParams()
    for (const [k, v] of Object.entries(f)) if (v) q.set(k, v)
    const qs = q.toString()
    return api.get<Deployment[]>(`/api/deployments${qs ? `?${qs}` : ''}`)
  },

  // known_hosts pre-fills the pinned-keys field; verified is true only for github.com, whose keys
  // come from an authenticated endpoint rather than a trust-on-first-use scan.
  discoverGitHostKeys: (host: string) =>
    api.get<{ known_hosts: string; verified: boolean }>(
      `/api/gitcreds/host-keys?host=${encodeURIComponent(host)}`,
    ),

  activateCA: (id: string) => api.post<CertAuthority>(`/api/certs/cas/${id}/activate`, { confirm: true }),

  renewCert: (id: string, rotateKey = false) =>
    api.post<Certificate>(`/api/certs/${id}/renew`, { rotate_key: rotateKey }),
  createAgent: (name: string) => api.post<NewAgent>('/api/agents', { name }),
  metrics: (env: string, params: { range: MetricRange; container?: string; stack?: string }) => {
    const q = new URLSearchParams({ range: params.range })
    if (params.container) q.set('container', params.container)
    if (params.stack) q.set('stack', params.stack)
    return api.get<MetricPoint[]>(`/api/clusters/${env}/metrics?${q}`)
  },

}
