// Formatting shared by everything that shows a deployment. It lives here rather than in each
// view because a duration that reads "2m 3s" on one page and "123s" on another is the kind of
// inconsistency nobody files a bug about and everybody notices.

/** How long something took. Empty while it is still going — a running deploy has no duration. */
export function duration(d: { started_at: string; ended_at?: string }): string {
  if (!d.ended_at) return ''
  return humanMs(new Date(d.ended_at).getTime() - new Date(d.started_at).getTime())
}

/** How long something has been going. For the deploy you are watching right now. */
export function elapsed(startedAt: string, now: number): string {
  return humanMs(now - new Date(startedAt).getTime())
}

function humanMs(ms: number): string {
  if (ms < 0) return ''
  if (ms < 1000) return `${ms}ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`
  return `${Math.floor(ms / 60_000)}m ${Math.round((ms % 60_000) / 1000)}s`
}

/**
 * "3 minutes ago". A feed is read by scanning down it, and an absolute timestamp on every row
 * makes you do arithmetic on each one to find out whether it is the deploy you are looking for.
 * The exact time is still there, as the title attribute.
 */
export function ago(iso: string): string {
  const secs = Math.floor((Date.now() - new Date(iso).getTime()) / 1000)
  if (secs < 60) return 'just now'

  const table: [number, string][] = [
    [60, 'minute'],
    [3600, 'hour'],
    [86_400, 'day'],
    [604_800, 'week'],
  ]
  let unit = 'week'
  let size = 604_800
  for (const [s, name] of table) {
    if (secs < s * 60 || name === 'week') {
      unit = name
      size = s
      break
    }
  }

  const n = Math.floor(secs / size)
  return `${n} ${unit}${n === 1 ? '' : 's'} ago`
}

export function absolute(iso: string): string {
  return new Date(iso).toLocaleString()
}

/** A commit is identified by its first seven characters everywhere else in the world too. */
export function shortSha(sha?: string): string {
  return sha ? sha.slice(0, 7) : ''
}

// The action names are compose's, not English. `up` is a fine thing to type at a shell and a
// poor thing to read in a sentence — "The up failed" is not something a person would say. So the
// wire keeps the verb and the UI says the words.

/** For a button or a heading: "Deploy". */
export function actionLabel(action: string): string {
  return (
    {
      up: 'Deploy',
      pull: 'Pull + deploy',
      restart: 'Restart',
      stop: 'Stop',
      down: 'Down',
      'down+volumes': 'Down + volumes',
    }[action] ?? action
  )
}

/** For the middle of a sentence: "The deploy failed." */
export function actionNoun(action: string): string {
  return (
    {
      up: 'deploy',
      pull: 'pull and deploy',
      restart: 'restart',
      stop: 'stop',
      down: 'teardown',
      'down+volumes': 'teardown',
    }[action] ?? action
  )
}
