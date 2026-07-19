/**
 * One status vocabulary, for everything.
 *
 * Portainer runs four in parallel — Button has 12 colour names, Badge has 11, Icon has 11,
 * and the container list actually uses none of them, falling back to Bootstrap 3's
 * `.label-success`. Two of those vocabularies routinely appear on the same table row.
 * Dokploy has one, but wired backwards: a running container renders as a BLACK badge, and
 * "running" is coloured yellow because in their schema it means "a build is running".
 *
 * So: six tones, defined once, and a set of functions that map each kind of thing Daffa
 * knows about onto them. If you need a colour for a state, it comes from here.
 */
export type Tone = 'success' | 'warn' | 'danger' | 'info' | 'neutral' | 'accent'

export interface Status {
  tone: Tone
  /** What a person calls it. Not the wire value. */
  label: string
  /**
   * Something is happening RIGHT NOW and the next poll may say something different.
   * Drives the pulse. It must never be set on a state that is merely bad and stable —
   * a dot that breathes says "wait", and a wedged container is not worth waiting for.
   */
  live?: boolean
  /** The bit you would otherwise have to open the logs to find out. */
  detail?: string
}

/**
 * A Docker container's lifecycle state.
 *
 * `restarting` is amber and live: it is either coming back or it is crash-looping, and from
 * one sample you cannot tell which.
 *
 * `exited` is the one that has to be read properly, and getting it wrong is how a dashboard
 * becomes noise. **Exit 0 is not a failure.** A migration container, an init job, a one-shot
 * `alembic upgrade head` — all of them run, succeed, and exit 0, and they then sit in
 * `docker ps -a` forever. Painting those red means a healthy host reports four problems on
 * the first morning, and the fifth time you see it you stop reading the list at all.
 *
 * So: exit 0 is `Completed`, and it is neutral. Anything non-zero is a real failure and
 * carries its code — 137 (OOM-killed) and 1 are entirely different mornings, and Portainer is
 * right to surface the number inline rather than make you open the logs for it.
 */
// Docker reports a container's healthcheck two ways: the `docker ps` status string carries it in
// parentheses — "Up 3 hours (healthy)" / "(unhealthy)" / "(health: starting)" — and inspect gives
// it structured as State.Health.Status ("healthy" | "unhealthy" | "starting" | "none"). Both funnel
// through here so the pill can say it, whichever the caller has.
export function containerHealth(statusText?: string): 'healthy' | 'unhealthy' | 'starting' | undefined {
  if (!statusText) return undefined
  if (/\(unhealthy\)/i.test(statusText)) return 'unhealthy'
  if (/\(health: starting\)/i.test(statusText)) return 'starting'
  if (/\(healthy\)/i.test(statusText)) return 'healthy'
  return undefined
}

// containerUptime pulls the human "up" duration out of the `docker ps` status string:
// "Up 3 hours (healthy)" → "3 hours". Empty for anything not currently up (Exited, Created, …).
export function containerUptime(statusText?: string): string {
  if (!statusText) return ''
  const m = /^Up\s+(.+?)(?:\s+\((?:healthy|unhealthy|health: starting)\))?$/i.exec(statusText.trim())
  return m ? m[1] : ''
}

function normalizeHealth(h?: string): 'healthy' | 'unhealthy' | 'starting' | undefined {
  switch (h) {
    case 'healthy':
    case 'unhealthy':
    case 'starting':
      return h
    default:
      return undefined // "none" (no healthcheck), or a status string handed in whole
  }
}

// containerStatus maps a container's lifecycle state to a pill. For a RUNNING container it also
// surfaces the healthcheck when there is one: `health` (from inspect) wins, else it is read out of
// `statusText` (the `docker ps` string). Unhealthy earns an amber dot — a running-but-failing
// container is not the plain green "Running" it would otherwise read as.
export function containerStatus(state: string, statusText?: string, health?: string): Status {
  switch (state) {
    case 'running': {
      const h = normalizeHealth(health) ?? containerHealth(statusText)
      if (h === 'unhealthy') return { tone: 'warn', label: 'Running', detail: 'unhealthy' }
      if (h) return { tone: 'success', label: 'Running', detail: h } // healthy | starting
      return { tone: 'success', label: 'Running' }
    }
    case 'paused':
      return { tone: 'warn', label: 'Paused' }
    case 'restarting':
      return { tone: 'warn', label: 'Restarting', live: true }
    case 'removing':
      return { tone: 'danger', label: 'Removing', live: true }
    case 'created':
      return { tone: 'info', label: 'Created' }
    // A deployment lifecycle hook: declared in the compose file, run around deploys,
    // never deployed. Reporting it as "missing" read a healthy stack as broken.
    case 'hook':
      return { tone: 'info', label: 'Hook', detail: 'runs around deploys' }
    case 'exited': {
      const code = exitCode(statusText)
      // Unknown code: Docker did not tell us, so do not invent alarm. Neutral.
      if (code === undefined || code === 0) return { tone: 'neutral', label: 'Completed' }
      return { tone: 'danger', label: 'Exited', detail: `code ${code}` }
    }
    case 'dead':
      return { tone: 'danger', label: 'Dead' }
    default:
      return { tone: 'neutral', label: state || 'Unknown' }
  }
}

/** Docker writes "Exited (137) 3 minutes ago". The code is the only part you cannot guess. */
function exitCode(statusText?: string): number | undefined {
  const m = statusText?.match(/\((\d+)\)/)
  return m ? Number(m[1]) : undefined
}

/**
 * A stack's deployment state.
 *
 * Portainer's stack list has NO status column at all: a healthy stack shows nothing, and you
 * cannot sort or filter by state. This is the column that fixes that.
 *
 * "Never deployed" and "the last deploy failed" are different facts and the UI used to say
 * the first when it meant the second — compose can create a container and then fail to start
 * it, leaving a stack you can curl but that Daffa never recorded a clean `up` for.
 */
export function stackStatus(s: {
  deployed_at?: string
  last_deploy_status?: string
  drifted?: boolean
}): Status {
  if (s.last_deploy_status === 'running') return { tone: 'accent', label: 'Deploying', live: true }
  if (s.deployed_at) {
    // Drift is not a failure — what is running is exactly what you asked for, it is just no
    // longer what the repository says. That is a thing to know, not a thing to panic about.
    return s.drifted
      ? { tone: 'warn', label: 'Drifted', detail: 'source has moved on' }
      : { tone: 'success', label: 'Deployed' }
  }
  if (s.last_deploy_status === 'failed') return { tone: 'danger', label: 'Deploy failed' }
  if (s.last_deploy_status === 'cancelled') return { tone: 'neutral', label: 'Cancelled' }
  return { tone: 'neutral', label: 'Never deployed' }
}

/**
 * A single deploy attempt.
 *
 * The wire value for a success is `ok` (store.DeployOK), never `succeeded` — a deploy that
 * worked was falling through to the default and rendering as a grey pill reading "ok", which
 * is the one outcome that should be unmistakable at a glance. Both are accepted here so that
 * the mapping cannot silently miss again.
 *
 * The exit code rides along as the detail, the same way a container's does: "did it fail" and
 * "with what" are one question, and 137 (OOM) and 1 are entirely different mornings.
 */
export function deploymentStatus(status: string, exitCode?: number): Status {
  switch (status) {
    case 'running':
      return { tone: 'accent', label: 'Deploying', live: true }
    case 'ok':
    case 'succeeded':
      return { tone: 'success', label: 'Succeeded' }
    case 'failed':
      return {
        tone: 'danger',
        label: 'Failed',
        detail: exitCode != null ? `exit ${exitCode}` : undefined,
      }
    case 'cancelled':
      return { tone: 'neutral', label: 'Cancelled' }
    default:
      return { tone: 'neutral', label: status }
  }
}

/** A host. Offline is red, not grey: an unreachable host is a problem, not an absence. */
/**
 * A Swarm service's replica health.
 *
 * The number is the whole story: `2/3` is not "running", it is "one of these is not coming up and
 * you should go and read its task". So a service that is short of its target is WARN, not success,
 * even though two thirds of it is serving traffic.
 *
 * A GLOBAL service has no replica count — it runs one task per node — so it is counted against the
 * nodes, and the caller renders "2/3 nodes". Saying "2/3 replicas" of a global service is a fluent
 * lie, and it is the kind that survives a code review because it reads correctly.
 */
export function serviceStatus(s: { mode: string; desired: number; running: number }): Status {
  const unit = s.mode === 'global' ? 'nodes' : 'replicas'
  const detail = `${s.running}/${s.desired} ${unit}`

  if (s.desired === 0) return { tone: 'neutral', label: 'Scaled to zero' }
  if (s.running === 0) return { tone: 'danger', label: 'Down', detail }
  if (s.running < s.desired) return { tone: 'warn', label: 'Degraded', detail, live: true }
  return { tone: 'success', label: 'Running', detail }
}

/**
 * One task's state — and this is the status that actually gets read.
 *
 * `desired` and `state` together are the diagnosis. A task whose desired state is `running` and
 * whose actual state is `pending` has not been placed, and Task.error says why in plain words
 * ("no suitable node (insufficient memory on 2 nodes)"). A task that is `shutdown` because swarm
 * MEANT to shut it down is not a failure at all — it is what a rolling update looks like from
 * underneath — so it is neutral rather than red. Painting those the same colour is how a healthy
 * deploy comes to look like an outage.
 */
// The error is deliberately NOT put in the pill's detail. The row renders it in full, on its own
// line, because it is the answer — and a pill that repeats it just prints the same sentence twice.
export function taskStatus(t: { desired: string; state: string; error?: string }): Status {
  const wanted = t.desired === 'running'

  switch (t.state) {
    case 'running':
      return { tone: 'success', label: 'Running' }
    case 'starting':
    case 'preparing':
    case 'assigned':
    case 'accepted':
      return { tone: 'info', label: cap(t.state), live: true }
    case 'pending':
      // Pending with an error is not "still thinking" — it is stuck, and the error is the reason.
      return t.error
        ? { tone: 'danger', label: 'Cannot place' }
        : { tone: 'warn', label: 'Pending', live: true }
    case 'complete':
      return { tone: 'neutral', label: 'Complete' }
    case 'shutdown':
      // Deliberate, if swarm wanted it. Only a shutdown swarm did NOT want is a failure.
      return wanted
        ? { tone: 'danger', label: 'Shut down' }
        : { tone: 'neutral', label: 'Shut down' }
    case 'failed':
    case 'rejected':
    case 'orphaned':
      return { tone: 'danger', label: cap(t.state) }
    default:
      return { tone: 'neutral', label: cap(t.state) || 'Unknown' }
  }
}

/** A machine in a Swarm. Drained is not broken — somebody did it on purpose — so it is warn, and
 *  the row says which. A node that is `down` is the one worth a colour that stops you. */
export function nodeStatus(n: {
  reachable: boolean
  in_swarm: boolean
  state?: string
  availability?: string
}): Status {
  if (!n.in_swarm) return { tone: 'warn', label: 'Not in the swarm' }
  if (n.state === 'down') return { tone: 'danger', label: 'Down' }
  if (n.availability === 'drain') return { tone: 'warn', label: 'Drained' }
  if (n.availability === 'pause') return { tone: 'warn', label: 'Paused' }
  if (n.state !== 'ready') return { tone: 'warn', label: cap(n.state ?? '') || 'Unknown' }
  // Ready, but Daffa cannot reach it: no shell, no stats, no containers listed from it. That is
  // not a swarm problem, it is a Daffa one, and it is worth its own colour.
  if (!n.reachable) return { tone: 'info', label: 'No agent' }
  return { tone: 'success', label: 'Ready' }
}

function cap(s: string): string {
  return s ? s[0].toUpperCase() + s.slice(1) : s
}

export function hostStatus(status: string): Status {
  return status === 'online'
    ? { tone: 'success', label: 'Online' }
    : { tone: 'danger', label: 'Offline' }
}

/** The CSS custom property each tone resolves to. The single bridge from tone to colour. */
export const toneVar: Record<Tone, string> = {
  success: 'var(--success)',
  warn: 'var(--warn)',
  danger: 'var(--danger)',
  info: 'var(--info)',
  neutral: 'var(--text-subtle)',
  accent: 'var(--accent)',
}

export const toneSoftVar: Record<Tone, string> = {
  success: 'var(--success-soft)',
  warn: 'var(--warn-soft)',
  danger: 'var(--danger-soft)',
  info: 'var(--info-soft)',
  neutral: 'var(--surface-sunken)',
  accent: 'var(--accent-soft)',
}
