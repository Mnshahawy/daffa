import { ref, watch } from 'vue'

export type Theme = 'system' | 'light' | 'dark'

// Set by initTheme(); the default keeps a consumer that forgets to init working, at the
// cost of sharing one preference across consoles served from the same origin in dev.
let storageKey = 'console.theme'

export const theme = ref<Theme>('system')

// `system` means "no opinion" — remove the attribute entirely and let the media query in
// the tokens stylesheet decide. Stamping data-theme="system" would be a value the CSS has
// to know about, which is one more thing to keep in step for no benefit.
function apply(t: Theme) {
  const root = document.documentElement
  if (t === 'system') {
    root.removeAttribute('data-theme')
  } else {
    root.setAttribute('data-theme', t)
  }
}

/**
 * Call once at app startup, before mount. The key is per-app ('daffa.theme',
 * 'diwan.theme', …) so two consoles on localhost don't fight over one preference.
 */
export function initTheme(opts?: { storageKey?: string }) {
  if (opts?.storageKey) storageKey = opts.storageKey

  const v = localStorage.getItem(storageKey)
  theme.value = v === 'light' || v === 'dark' ? v : 'system'
  apply(theme.value)

  watch(theme, (t) => {
    apply(t)
    if (t === 'system') localStorage.removeItem(storageKey)
    else localStorage.setItem(storageKey, t)
  })
}

// resolved is what is actually on screen right now — which for `system` depends on the
// OS, and changes underneath us when the OS does.
export const resolved = ref<'light' | 'dark'>('light')

function currentlyDark(): boolean {
  const explicit = document.documentElement.getAttribute('data-theme')
  if (explicit === 'dark') return true
  if (explicit === 'light') return false
  return window.matchMedia('(prefers-color-scheme: dark)').matches
}

if (typeof window !== 'undefined') {
  resolved.value = currentlyDark() ? 'dark' : 'light'
  const media = window.matchMedia('(prefers-color-scheme: dark)')
  media.addEventListener('change', () => {
    resolved.value = currentlyDark() ? 'dark' : 'light'
  })
  watch(theme, () => {
    // Read after the attribute has been applied.
    queueMicrotask(() => {
      resolved.value = currentlyDark() ? 'dark' : 'light'
    })
  })
}
