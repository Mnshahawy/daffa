let appName = 'Console'

/** Call once at startup with the product name ('Daffa', 'Diwan', …). */
export function setAppName(name: string) {
  appName = name
}

/**
 * The tab title.
 *
 * Every page saying just the product name is useless the moment there is more than one tab
 * open — and an operator running a fleet has one tab per host, plus the deploy they are
 * watching, plus the logs of the thing that broke. The tab strip is a navigation surface.
 *
 * Parts run MOST SPECIFIC FIRST, and the product name comes last, because a browser
 * truncates a tab from the right. "api-gateway · Stacks · Daffa" degrades to "api-gate…",
 * which is the part you needed; "Daffa · Stacks · api-gateway" degrades to "Daffa…",
 * which is every tab.
 *
 * Route-name → title mapping stays in the app (it knows its nav registry); this is just
 * the formatter every app shares.
 */
export function setTitle(...parts: (string | undefined | null | false)[]): void {
  const trail = parts.filter(Boolean) as string[]
  document.title = trail.length ? `${trail.join(' · ')} · ${appName}` : appName
}
