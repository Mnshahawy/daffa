<script setup lang="ts">
// Mirrors the console's theming: an explicit choice is stored under the same localStorage key
// ('daffa-theme') and applied to `data-theme`, which drives brand/tokens.css; with no choice,
// the OS preference wins via that file's media query. On top of that we keep VitePress's own
// `.dark` class in sync so its internals (Shiki code highlighting) follow — VitePress's
// built-in appearance is disabled so this one mechanism owns the theme.
import { ref, onMounted } from 'vue'

const dark = ref(false)
const MQ = '(prefers-color-scheme: dark)'

function storedChoice(): 'light' | 'dark' | null {
  try {
    const s = localStorage.getItem('daffa-theme')
    if (s === 'light' || s === 'dark') return s
  } catch (e) { /* private mode */ }
  return null
}

function resolvedDark(): boolean {
  const c = storedChoice()
  return c ? c === 'dark' : window.matchMedia(MQ).matches
}

// choice === null means "follow the OS": clear the attribute and let the media query decide.
function apply(choice: 'light' | 'dark' | null) {
  const root = document.documentElement
  if (choice) root.setAttribute('data-theme', choice)
  else root.removeAttribute('data-theme')
  const isDark = choice ? choice === 'dark' : window.matchMedia(MQ).matches
  root.classList.toggle('dark', isDark)
  dark.value = isDark
}

function toggle() {
  const next = resolvedDark() ? 'light' : 'dark'
  try { localStorage.setItem('daffa-theme', next) } catch (e) { /* private mode */ }
  apply(next)
}

onMounted(() => {
  dark.value = resolvedDark()
  // Follow live OS changes only while the user hasn't pinned a choice.
  window.matchMedia(MQ).addEventListener('change', () => {
    if (!storedChoice()) apply(null)
  })
})
</script>

<template>
  <button
    class="theme-toggle"
    type="button"
    :aria-label="dark ? 'Switch to light theme' : 'Switch to dark theme'"
    :title="dark ? 'Switch to light theme' : 'Switch to dark theme'"
    @click="toggle"
  >
    <svg v-if="dark" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <circle cx="12" cy="12" r="4" /><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4" />
    </svg>
    <svg v-else viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z" />
    </svg>
  </button>
</template>

<style scoped>
.theme-toggle {
  display: inline-grid;
  place-items: center;
  width: 2rem;
  height: 2rem;
  margin-left: 0.25rem;
  border-radius: var(--radius-control);
  border: 1px solid var(--border);
  background: var(--surface-raised);
  color: var(--text-muted);
  cursor: pointer;
  transition: color 0.12s ease, border-color 0.12s ease;
}
.theme-toggle:hover { color: var(--text); border-color: var(--border-strong); }
.theme-toggle svg { width: 1.1rem; height: 1.1rem; }
</style>
