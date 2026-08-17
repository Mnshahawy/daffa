/**
 * @mnshahawy/daffa-console-ui — the shared component kit for Daffa-family operations consoles
 * (Daffa, Diwan, the Amany cell-manager, …).
 *
 * Distributed as SOURCE: consumers compile the .vue files with their own Vite + vue
 * plugin, and Tailwind must scan this package for the utility classes the components
 * use — add to the app stylesheet:
 *
 *   @source '../node_modules/@mnshahawy/daffa-console-ui/src';
 *
 * Styles are imported separately (styles/tokens.css, styles/fonts.css, styles/base.css) —
 * see styles/base.css for the exact import order.
 */

// ── Components ───────────────────────────────────────────────────────────────
export { default as AppIcon } from './components/AppIcon.vue'
export { default as BaseButton } from './components/BaseButton.vue'
export { default as ComboBox } from './components/ComboBox.vue'
export { default as ConfirmHost } from './components/ConfirmHost.vue'
export { default as CopyButton } from './components/CopyButton.vue'
export { default as DropdownMenu } from './components/DropdownMenu.vue'
export { default as EmptyState } from './components/EmptyState.vue'
export { default as ListInput } from './components/ListInput.vue'
export { default as Modal } from './components/Modal.vue'
export { default as PageHeader } from './components/PageHeader.vue'
export { default as RailItem } from './components/RailItem.vue'
export { default as SearchInput } from './components/SearchInput.vue'
export { default as SecretInput } from './components/SecretInput.vue'
export { default as Select } from './components/Select.vue'
export { default as Spinner } from './components/Spinner.vue'
export { default as StatusPill } from './components/StatusPill.vue'
export { default as ThemeToggle } from './components/ThemeToggle.vue'
export { default as ToastHost } from './components/ToastHost.vue'
export { default as WizardModal } from './components/WizardModal.vue'
export type { WizardStep, WizardAction } from './components/WizardModal.vue'

// ── Channels / stores ────────────────────────────────────────────────────────
export { toast, toasts, dismiss, errorMessage, type Toast } from './lib/toast'
export { confirm, checked, request, resolve, typed, type ConfirmRequest } from './lib/confirm'
export { theme, resolved, initTheme, type Theme } from './lib/theme'

// ── Vocabulary / helpers ─────────────────────────────────────────────────────
export { toneVar, toneSoftVar, type Tone, type Status } from './lib/status'
export { iconPaths, type IconName } from './lib/icons'
export { hasCap, type CapSet, type CapValue } from './lib/caps'
export { setAppName, setTitle } from './lib/title'
export {
  ago,
  absolute,
  bytes,
  daysLeft,
  duration,
  elapsed,
  humanMs,
  shortSha,
} from './lib/format'
