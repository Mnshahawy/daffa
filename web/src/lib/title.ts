import { allNavItems, allSettingsTabs } from './nav'

export const APP_NAME = 'Daffa'

/**
 * The tab title.
 *
 * Every page said "Daffa", which is useless the moment there is more than one tab open — and an
 * operator running a fleet has one per host, plus the deploy they are watching, plus the logs of
 * the thing that broke. The tab strip is a navigation surface and it was showing nothing.
 *
 * Parts run MOST SPECIFIC FIRST, and the product name comes last, because a browser truncates a
 * tab from the right. "api-gateway · Stacks · Daffa" degrades to "api-gate…", which is the part
 * you needed; "Daffa · Stacks · api-gateway" degrades to "Daffa…", which is every tab.
 */
export function setTitle(...parts: (string | undefined | null | false)[]): void {
  const trail = parts.filter(Boolean) as string[]
  document.title = trail.length ? `${trail.join(' · ')} · ${APP_NAME}` : APP_NAME
}

/**
 * What each route is called, taken from the nav registry rather than written out a second time —
 * so a page and the menu entry that points at it cannot drift into disagreeing about its name.
 */
const staticTitles = new Map<string, string[]>()

for (const item of allNavItems) staticTitles.set(item.name, [item.label])
for (const tab of allSettingsTabs) staticTitles.set(tab.name, [tab.label, 'Settings'])

staticTitles.set('login', ['Sign in'])
staticTitles.set('no-access', ['No access'])

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
