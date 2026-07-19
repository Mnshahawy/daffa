import { reactive } from 'vue'
import { ApiError } from '@/lib/api'
import type { Tone } from '@/lib/status'

/**
 * One transient-feedback channel, for the whole app.
 *
 * The rules it exists to enforce:
 *
 *  1. **Every mutation says whether it worked.** A save that succeeds silently and a save
 *     that failed silently look identical from the chair, so people click again — and a
 *     deploy or a delete fired twice because the first click "did nothing" is a real outage.
 *     So: the outcome of an action is always spoken, in the same place, in the same shape.
 *
 *  2. **Feedback is transient, not a log.** A toast confirms and then leaves. It is not the
 *     audit trail (that is the Audit view) and it is not where a form's validation lives (a
 *     field error belongs next to the field). It is the "did the thing I just clicked land?"
 *     answer, and once you have read it it is noise, so it dismisses itself.
 *
 *  3. **Errors linger; confirmations do not.** "Saved" needs a glance; "Could not save — the
 *     master key was replaced" needs to be read, and hurrying it off the screen is how the
 *     one message that mattered is the one nobody got to finish reading.
 *
 * Mirrors lib/confirm.ts: a single reactive store, rendered once by ToastHost in App.vue.
 */
export interface Toast {
  id: number
  /** Reuses the app-wide status vocabulary so a toast's colour matches every pill. */
  tone: Extract<Tone, 'success' | 'danger' | 'warn' | 'info'>
  message: string
}

export const toasts = reactive<Toast[]>([])

// Never let a burst of toasts bury the screen — a failed batch that fires ten in a row should
// leave the last few readable, not a wall. Oldest fall off the top.
const MAX_VISIBLE = 4
const TTL_OK = 3500
const TTL_LINGER = 6500

let seq = 0
const timers = new Map<number, number>()

function push(tone: Toast['tone'], message: string, ttl: number) {
  const id = ++seq
  toasts.push({ id, tone, message })
  while (toasts.length > MAX_VISIBLE) dismiss(toasts[0].id)
  timers.set(id, window.setTimeout(() => dismiss(id), ttl))
  return id
}

export function dismiss(id: number) {
  const t = timers.get(id)
  if (t !== undefined) {
    window.clearTimeout(t)
    timers.delete(id)
  }
  const i = toasts.findIndex((x) => x.id === id)
  if (i !== -1) toasts.splice(i, 1)
}

/** Pull the operator-facing reason out of a caught error, falling back to a written sentence.
 *  ApiError carries the server's message (`pkg: what went wrong`); anything else is unexpected
 *  and gets the caller's fallback rather than a raw stack the operator cannot act on. */
export function errorMessage(e: unknown, fallback: string): string {
  return e instanceof ApiError && e.message ? e.message : fallback
}

export const toast = {
  ok: (message: string) => push('success', message, TTL_OK),
  info: (message: string) => push('info', message, TTL_OK),
  warn: (message: string) => push('warn', message, TTL_LINGER),
  /** `toast.err(e, 'Could not save the role.')` — the fallback names the action that failed. */
  err: (e: unknown, fallback: string) => push('danger', errorMessage(e, fallback), TTL_LINGER),
}
