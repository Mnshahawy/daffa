import { ref, shallowRef } from 'vue'

/**
 * One confirmation dialog, for the whole app.
 *
 * The rules it exists to enforce:
 *
 *  1. **The title is the action.** "Remove nginx?" — not "Are you sure?". Portainer titles
 *     every single confirm in the product "Are you sure?", which is the least informative
 *     sentence available: by the time you are reading it you already know you are being
 *     asked, and what you need to know is WHAT you are about to do and to WHICH thing.
 *
 *  2. **Only confirm what is worth confirming.** Dokploy wraps Deploy, Reload and even
 *     Accept Invitation in a confirm. Confirm everything and people learn to click through
 *     without reading — which spends the one bit of attention you were saving for the
 *     dialog that actually mattered. Deploys do not get one. `docker rm` does.
 *
 *  3. **The consequential sub-choice belongs at the decision point**, not in a settings
 *     page and not in a second dialog. "Also remove anonymous volumes" is a question you
 *     can only answer while looking at the thing you are removing.
 *
 * Replaces window.confirm(), which cannot be styled, cannot be themed, cannot carry a
 * checkbox, and stops the whole JS thread while it is open.
 */
export interface ConfirmRequest {
  /** The action, phrased as the question. "Remove nginx?" */
  title: string
  /** What will actually happen. Say the irreversible part out loud. */
  body?: string
  /** The verb on the button. Must match the title's verb — "Remove", not "OK". */
  confirmLabel?: string
  cancelLabel?: string
  /**
   * `danger` for anything that destroys data — it paints the button solid red, which is the
   * loudest thing the design system can say and is reserved for exactly this moment.
   * `caution` for disruptive-but-recoverable (stop, restart).
   */
  intent?: 'danger' | 'caution' | 'primary'
  /** A consequential sub-choice, asked where it is answerable. */
  checkbox?: { label: string; hint?: string; default?: boolean }
  /**
   * Make them type this to proceed. For the genuinely unrecoverable only — deleting a stack,
   * pruning volumes. And it is the NAME, nothing else: Dokploy asks you to type
   * `myapi/myproject-myapi-a1b2c3`, which is impossible to type and therefore always pasted,
   * which defeats the entire purpose of asking.
   */
  typeToConfirm?: string
}

export interface ConfirmResult {
  /** State of the sub-choice checkbox, if there was one. */
  checked: boolean
}

export const request = shallowRef<ConfirmRequest | null>(null)
export const checked = ref(false)
export const typed = ref('')

let resolver: ((value: ConfirmResult | null) => void) | null = null

/** Returns null if they backed out, or the result (with the checkbox state) if they went ahead. */
export function confirm(req: ConfirmRequest): Promise<ConfirmResult | null> {
  request.value = req
  checked.value = req.checkbox?.default ?? false
  typed.value = ''
  return new Promise((resolve) => {
    resolver = resolve
  })
}

export function resolve(ok: boolean) {
  const r = resolver
  const result = ok ? { checked: checked.value } : null
  request.value = null
  resolver = null
  r?.(result)
}
