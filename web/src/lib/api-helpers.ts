// Hand-written display/query helpers that ride alongside the generated client. They are
// not API bindings, so they live outside the generated file — api.ts re-exports them,
// and every view keeps importing from '@/lib/api'.

// bytes renders a size the way an operator reads it — three significant figures is all
// anyone acts on, and "1.4 GB" beats "1503238553".
export function bytes(n: number): string {
  if (n <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), units.length - 1)
  const v = n / 1024 ** i
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`
}

/**
 * The ?node= parameter, and it is required only when there is more than one node to choose between.
 *
 * The rule is ARITY, not kind: a standalone environment has one node, and so does a single-node
 * swarm — which is the topology most people actually run. A parameter becomes required at exactly
 * the moment it becomes meaningful, so callers pass `undefined` and the server fills it in.
 */
export function nodeQuery(node?: string, extra?: Record<string, string>): string {
  const q = new URLSearchParams(extra)
  if (node) q.set('node', node)
  const s = q.toString()
  return s ? `?${s}` : ''
}
