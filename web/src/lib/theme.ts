import { ref, watch } from 'vue'

export type Theme = 'system' | 'light' | 'dark'

const KEY = 'daffa.theme'

function stored(): Theme {
  const v = localStorage.getItem(KEY)
  return v === 'light' || v === 'dark' ? v : 'system'
}

export const theme = ref<Theme>(stored())

// `system` means "no opinion" — remove the attribute entirely and let the media query in
// style.css decide. Stamping data-theme="system" would be a value the CSS has to know
// about, which is one more thing to keep in step for no benefit.
function apply(t: Theme) {
  const root = document.documentElement
  if (t === 'system') {
    root.removeAttribute('data-theme')
  } else {
    root.setAttribute('data-theme', t)
  }
}

export function initTheme() {
  apply(theme.value)

  watch(theme, (t) => {
    apply(t)
    if (t === 'system') localStorage.removeItem(KEY)
    else localStorage.setItem(KEY, t)
  })
}

// resolved is what is actually on screen right now — which for `system` depends on the
// OS, and changes underneath us when the OS does.
export const resolved = ref<'light' | 'dark'>(currentlyDark() ? 'dark' : 'light')

function currentlyDark(): boolean {
  const explicit = document.documentElement.getAttribute('data-theme')
  if (explicit === 'dark') return true
  if (explicit === 'light') return false
  return window.matchMedia('(prefers-color-scheme: dark)').matches
}

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
