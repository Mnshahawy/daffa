/**
 * One status vocabulary, for everything.
 *
 * Portainer runs four in parallel — Button has 12 colour names, Badge has 11, Icon has 11,
 * and the container list actually uses none of them, falling back to Bootstrap 3's
 * `.label-success`. Two of those vocabularies routinely appear on the same table row.
 * Dokploy has one, but wired backwards: a running container renders as a BLACK badge.
 *
 * So: six tones, defined once. Each app maps its own domain states onto them (a
 * `containerStatus()` in Daffa, a `nodeStatus()` in Diwan, a `cellStatus()` in a fleet
 * console) — those mappers live in the app, next to the domain they describe. If you need
 * a colour for a state, it comes from here.
 */
export type Tone = 'success' | 'warn' | 'danger' | 'info' | 'neutral' | 'accent'

export interface Status {
  tone: Tone
  /** What a person calls it. Not the wire value. */
  label: string
  /**
   * Something is happening RIGHT NOW and the next poll may say something different.
   * Drives the pulse. It must never be set on a state that is merely bad and stable —
   * a dot that breathes says "wait", and a wedged process is not worth waiting for.
   */
  live?: boolean
  /** The bit you would otherwise have to open the logs to find out. */
  detail?: string
}

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
