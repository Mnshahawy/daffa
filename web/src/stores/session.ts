import { defineStore } from 'pinia'
import { ref, watch } from 'vue'
import { daffa, type AuthConfig, type Me } from '@/lib/api'
import { hasCap, type CapValue } from '@/lib/caps'

// Session and environment selection are the only genuinely global state. Everything
// else is server data and belongs to vue-query, not here.
const ENV_KEY = 'daffa.env'

export const useSession = defineStore('session', () => {
  const user = ref<Me | null>(null)
  const authConfig = ref<AuthConfig | null>(null)
  // Remember which host you were looking at. Being bounced back to the local socket on
  // every reload is a small thing that gets irritating quickly once there is a fleet.
  const envId = ref<string>(localStorage.getItem(ENV_KEY) ?? '')
  const loaded = ref(false)

  watch(envId, (id) => {
    if (id) localStorage.setItem(ENV_KEY, id)
  })

  // ensureLoaded is idempotent: the router guard calls it on every navigation, but
  // only the first one hits the network.
  async function ensureLoaded() {
    if (loaded.value) return
    loaded.value = true

    authConfig.value = await daffa.authConfig().catch(() => null)
    user.value = await daffa.me().catch(() => null)
  }

  async function login(username: string, password: string) {
    user.value = await daffa.login(username, password)
  }

  /**
   * Re-read our own identity and capabilities.
   *
   * Editing a role or a membership can change what the person doing the editing may do —
   * the server recomputes it on the next request, but the UI would keep showing buttons
   * that are now 403s until a reload. Call this after any change to roles or users.
   */
  async function refresh() {
    user.value = await daffa.me().catch(() => null)
  }

  async function logout() {
    await daffa.logout().catch(() => {})
    user.value = null
    location.assign('/login')
  }

  /**
   * Does the signed-in user hold this capability, on the host they are looking at?
   *
   * Grants can be limited to one host, so the answer is `what they hold everywhere` OR
   * `what they hold here`. Passing an explicit host overrides the selected one — the users
   * screen needs to ask about a host other than the current one.
   *
   * Note it still takes ONE argument in the common case, and every existing call site is
   * unchanged. That works because a global-only capability (users.edit, roles.edit,
   * settings.edit…) can never appear in a scoped mask — the server refuses to grant such a
   * role on a host — so the same expression answers both kinds of question correctly.
   *
   * This is the ONLY question the UI asks about permissions. It is a mirror of the server's
   * check, never a substitute for it: hiding a button is a courtesy, and the route behind it
   * refuses on its own.
   */
  function can(cap: CapValue, env?: string): boolean {
    const u = user.value
    if (!u) return false
    if (hasCap(u.caps, cap)) return true

    const here = env ?? envId.value
    return here ? hasCap(u.caps_by_env?.[here], cap) : false
  }

  /**
   * Does the user hold this capability ANYWHERE — globally, or on any host?
   *
   * For deciding whether a page is worth showing at all, when the answer does not depend on
   * which host is selected: the Settings tabs, and the fleet-wide credential lists.
   */
  function canAnywhere(cap: CapValue): boolean {
    const u = user.value
    if (!u) return false
    if (hasCap(u.caps, cap)) return true
    return Object.values(u.caps_by_env ?? {}).some((m) => hasCap(m, cap))
  }

  return { user, authConfig, envId, ensureLoaded, login, logout, refresh, can, canAnywhere }
})
