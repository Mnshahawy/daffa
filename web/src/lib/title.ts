import { allNavItems, allSettingsTabs } from './nav'

// The title formatter lives in the shared kit (setAppName('Daffa') is called at startup
// in main.ts); re-exported so existing `@/lib/title` imports keep working. The
// route-name → title maps below stay app-local — they know Daffa's nav registry.
export { setTitle } from '@mnshahawy/daffa-console-ui'
import { setTitle } from '@mnshahawy/daffa-console-ui'

/**
 * What each route is called, taken from the nav registry rather than written out a second time —
 * so a page and the menu entry that points at it cannot drift into disagreeing about its name.
 */
const staticTitles = new Map<string, string[]>()

for (const item of allNavItems) staticTitles.set(item.name, [item.label])
for (const tab of allSettingsTabs) staticTitles.set(tab.name, [tab.label, 'Settings'])

staticTitles.set('login', ['Sign in'])
staticTitles.set('no-access', ['No access'])
staticTitles.set('not-found', ['Not found'])

/**
 * The section a detail page belongs to. It is the best the router can do on its own — the entity
 * has not been fetched yet at navigation time — so the view refines it to include the name once
 * it has one. Until then the tab says "Stacks", not "Daffa".
 */
const detailSections: Record<string, string> = {
  service: 'Services',
  stack: 'Stacks',
  container: 'Containers',
  deployment: 'Deployments',
}

/** Called from the router on every navigation. Views with a name of their own then override it. */
export function setTitleForRoute(name: string | symbol | null | undefined): void {
  if (typeof name !== 'string') return setTitle()

  const parts = staticTitles.get(name)
  if (parts) return setTitle(...parts)

  setTitle(detailSections[name])
}
