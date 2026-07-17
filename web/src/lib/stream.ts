// SSE helpers. The browser's EventSource reconnects on its own and speaks exactly
// the protocol the Go side writes, so there is nothing to hand-roll here beyond
// typing the payloads and cleaning up.

export interface LogLine {
  stream: 'stdout' | 'stderr'
  text: string
}

export interface DockerEvent {
  action: string
  id: string
  name: string
}

type Handlers<T> = {
  onMessage: (payload: T) => void
  onError?: (message: string) => void
}

function subscribe<T>(url: string, event: string, h: Handlers<T>): () => void {
  const es = new EventSource(url, { withCredentials: true })

  es.addEventListener(event, (e) => {
    try {
      h.onMessage(JSON.parse((e as MessageEvent).data) as T)
    } catch {
      // A malformed frame is not worth tearing the stream down for.
    }
  })

  es.addEventListener('error', (e) => {
    const data = (e as MessageEvent).data
    if (data && h.onError) {
      try {
        h.onError(JSON.parse(data).message)
      } catch {
        h.onError('Stream error.')
      }
    }
    // A transport-level error (no data) is EventSource reconnecting; leave it be.
  })

  return () => es.close()
}

/**
 * A service's logs, across the whole cluster.
 *
 * This is the ONE cluster-wide stream Docker proxies for us: the manager collects from every node
 * running a task, so it works with no agent on the workers at all. Exec and stats are NOT proxied
 * — they are container-scoped, and no manager can reach into a container on another machine —
 * which is why those are routed to a node instead.
 */
export function streamServiceLogs(
  env: string,
  id: string,
  h: Handlers<LogLine>,
  tail = 200,
): () => void {
  return subscribe<LogLine>(
    `/api/environments/${env}/services/${id}/logs?tail=${tail}&follow=true`,
    'log',
    h,
  )
}

export function streamLogs(
  env: string,
  id: string,
  h: Handlers<LogLine>,
  tail = 200,
  node?: string,
): () => void {
  return subscribe<LogLine>(
    `/api/environments/${env}/containers/${id}/logs?tail=${tail}&follow=true${node ? `&node=${node}` : ''}`,
    'log',
    h,
  )
}

// streamEvents is how lists stay fresh: the daemon tells us what changed, and we
// invalidate exactly that instead of polling on a timer.
export function streamEvents(env: string, h: Handlers<DockerEvent>): () => void {
  return subscribe<DockerEvent>(`/api/environments/${env}/events`, 'docker', h)
}

export interface StatsSample {
  id: string
  cpu: number
  memory: number
  mem_limit: number
  mem_pct: number
  net_rx: number
  net_tx: number
  block_read: number
  block_write: number
}

// streamStats follows ONE container — the one being looked at. The list view uses the
// snapshot endpoint instead; see daffa.stats().
export function streamStats(
  env: string,
  id: string,
  h: Handlers<StatsSample>,
  node?: string,
): () => void {
  return subscribe<StatsSample>(
    `/api/environments/${env}/containers/${id}/stats${node ? `?node=${node}` : ''}`,
    'stats',
    h,
  )
}

export interface DeploymentEnd {
  status: 'ok' | 'failed' | 'cancelled'
  exit_code?: number
}

/**
 * Follow a deployment's log.
 *
 * ONE URL serves both cases: a running deploy is followed live off its runner container, and a
 * finished one is replayed from the database. That is what makes a deployment link work whether
 * you open it during the deploy or a week later — which is the entire point of a deployment
 * having a link.
 *
 * It needs its own subscriber rather than the generic one above because it is the only stream
 * that ENDS: the server sends a final `end` frame with the verdict, and without handling it a
 * viewer would just watch the output stop and have to guess how it went.
 */
export function streamDeployment(
  id: string,
  h: {
    /**
     * replace=true means this frame is the AUTHORITATIVE stored log, not an increment:
     * the server sends it when a live deploy finishes, because the stored log carries
     * phase headers and hook output the container streams never did. Swap, don't append.
     */
    onLog: (text: string, replace?: boolean) => void
    onEnd: (end: DeploymentEnd) => void
    onError?: (message: string) => void
  },
): () => void {
  const es = new EventSource(`/api/deployments/${id}/logs`, { withCredentials: true })

  es.addEventListener('log', (e) => {
    try {
      const frame = JSON.parse((e as MessageEvent).data) as { text: string; replace?: boolean }
      h.onLog(frame.text, frame.replace)
    } catch {
      // A malformed frame is not worth tearing the stream down for.
    }
  })

  es.addEventListener('end', (e) => {
    try {
      h.onEnd(JSON.parse((e as MessageEvent).data) as DeploymentEnd)
    } catch {
      /* ignore */
    }
    // The deploy is over and the server will not send more. Closing here stops EventSource
    // reconnecting forever to an endpoint that has nothing left to say.
    es.close()
  })

  es.addEventListener('error', (e) => {
    const data = (e as MessageEvent).data
    if (data && h.onError) {
      try {
        h.onError(JSON.parse(data).message)
      } catch {
        h.onError('Stream error.')
      }
    }
  })

  return () => es.close()
}

// Terminal frames are raw bytes in both directions; only control messages (resize) are
// JSON, so there is no parsing of terminal output anywhere in this app.
export function openExec(
  env: string,
  id: string,
  rows: number,
  cols: number,
  node?: string,
): WebSocket {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  // The node is how a shell reaches a container on ANOTHER machine. The server holds a tunnel per
  // node, so this one parameter is the entire routing mechanism — no agent mesh, no gossip.
  const url =
    `${proto}//${location.host}/api/environments/${env}/containers/${id}/exec` +
    `?rows=${rows}&cols=${cols}${node ? `&node=${node}` : ''}`
  const ws = new WebSocket(url)
  ws.binaryType = 'arraybuffer'
  return ws
}
