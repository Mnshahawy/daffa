// Formatting shared by everything that shows a timestamp, a duration, or a byte count. It
// lives here rather than in each view (or each app) because a duration that reads "2m 3s"
// on one page and "123s" on another is the kind of inconsistency nobody files a bug about
// and everybody notices. Domain verb maps (compose actions, mail verbs) stay app-local.

/** How long something took. Empty while it is still going — a running job has no duration. */
export function duration(d: { started_at: string; ended_at?: string }): string {
  if (!d.ended_at) return ''
  return humanMs(new Date(d.ended_at).getTime() - new Date(d.started_at).getTime())
}

/** How long something has been going. For the job you are watching right now. */
export function elapsed(startedAt: string, now: number): string {
  return humanMs(now - new Date(startedAt).getTime())
}

export function humanMs(ms: number): string {
  if (ms < 0) return ''
  if (ms < 1000) return `${ms}ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`
  return `${Math.floor(ms / 60_000)}m ${Math.round((ms % 60_000) / 1000)}s`
}

/**
 * "3 minutes ago". A feed is read by scanning down it, and an absolute timestamp on every row
 * makes you do arithmetic on each one to find out whether it is the row you are looking for.
 * The exact time is still there, as the title attribute.
 *
 * `terse` gives "3m ago" for dense tables where the long form would wrap.
 */
export function ago(iso: string, opts?: { terse?: boolean }): string {
  const secs = Math.floor((Date.now() - new Date(iso).getTime()) / 1000)
  if (secs < 60) return 'just now'

  const table: [number, string, string][] = [
    [60, 'minute', 'm'],
    [3600, 'hour', 'h'],
    [86_400, 'day', 'd'],
    [604_800, 'week', 'w'],
  ]
  let unit = 'week'
  let short = 'w'
  let size = 604_800
  for (const [s, name, abbr] of table) {
    if (secs < s * 60 || name === 'week') {
      unit = name
      short = abbr
      size = s
      break
    }
  }

  const n = Math.floor(secs / size)
  if (opts?.terse) return `${n}${short} ago`
  return `${n} ${unit}${n === 1 ? '' : 's'} ago`
}

export function absolute(iso: string): string {
  return new Date(iso).toLocaleString()
}

/** "1.4 GB". Binary-ish steps at 1000 because operators read disk vendors' units all day. */
export function bytes(n?: number): string {
  if (n === undefined || n === null || Number.isNaN(n)) return ''
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = n
  let i = 0
  while (v >= 1000 && i < units.length - 1) {
    v /= 1000
    i++
  }
  return `${i === 0 ? v : v.toFixed(1)} ${units[i]}`
}

/** A commit is identified by its first seven characters everywhere else in the world too. */
export function shortSha(sha?: string): string {
  return sha ? sha.slice(0, 7) : ''
}

/** Days until an ISO date; negative when past. For cert-expiry columns. */
export function daysLeft(iso: string): number {
  return Math.floor((new Date(iso).getTime() - Date.now()) / 86_400_000)
}
