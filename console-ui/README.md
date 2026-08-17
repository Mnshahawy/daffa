# @mnshahawy/daffa-console-ui

The shared component kit for Daffa-family operations consoles — Daffa itself
and any other single-binary Go + Vue console that wants the same grammar: quiet chrome,
one status vocabulary, one action grammar, colour that always means something.

Extracted from Daffa's `web/src` after its second console (Diwan) forked the same
components file-for-file. The fork proved the components were stable; this package stops
the copies drifting.

## What's in it

- **`styles/tokens.css`** — the full colour system (accent ramp, surfaces, status tones,
  radii, shadows, dark mode), parameterized on five hue variables so each console keeps
  its own brand. See the header comment for the knobs.
- **`styles/fonts.css`** — IBM Plex Sans Variable + IBM Plex Mono, bundled woff2, latin only.
- **`styles/base.css`** — Tailwind v4 layer: cursor grammar, focus ring, `surface` /
  `card` / `eyebrow` / `field` / `pulse-ring` / `appear` utilities, and the `.btn` action
  grammar (primary / secondary / ghost / caution / danger / danger-solid × xs / sm / md).
- **Components** — `BaseButton`, `Select`, `ComboBox`, `SearchInput`, `SecretInput`,
  `ListInput` (a `string[]` edited as removable chips, instead of a text field with a
  separator rule in the hint underneath), `Modal`, `WizardModal` (one long ordered form
  split into steps, with a rail that says how many there are before you commit to the
  first — for a dialog that has outgrown a screen, never for an unordered form),
  `DropdownMenu`, `PageHeader`,
  `StatusPill`, `EmptyState`, `AppIcon` (+
  `iconPaths`), `CopyButton`, `Spinner`, `RailItem`, `ThemeToggle`, and the singleton
  hosts `ToastHost` / `ConfirmHost`.
- **Channels** — `toast` (transient feedback, errors linger), `confirm` (typed/checkbox
  confirm flow), `theme` (system/light/dark with per-app storage key).
- **Helpers** — `hasCap` (BigInt capability masking), `setTitle`, `ago` / `bytes` /
  `duration` / `daysLeft` formatters, the `Tone`/`Status` vocabulary.

Distributed as **source**. Consumers compile the `.vue` files with their own Vite + Vue
plugin — no build step here, no double-compiled runtime, and the embedded-binary use case
keeps full tree-shaking.

## Using it

Published to the **GitHub Packages** npm registry, not npmjs.com, so the scope has to be
pointed at it — and GitHub Packages requires a token even for reading a public package.
A personal access token with `read:packages` is enough; in CI, `GITHUB_TOKEN` works.

```ini
# .npmrc (consumer, next to package.json)
@mnshahawy:registry=https://npm.pkg.github.com
//npm.pkg.github.com/:_authToken=${GITHUB_TOKEN}
```

```jsonc
// package.json (published) …
"dependencies": { "@mnshahawy/daffa-console-ui": "^0.1.0" }
// … or, inside this repo / a sibling checkout (no registry, no token):
"dependencies": { "@mnshahawy/daffa-console-ui": "file:../daffa/console-ui" }
```

App stylesheet (order matters — tokens and fonts are plain CSS, `base.css` needs Tailwind):

```css
@import 'tailwindcss';

/* Your brand: Daffa indigo 285 / marine 198; Diwan lapis 258 / sand 85 ×0.65. */
:root {
  --ui-hue: 285;
  --ui-hue-2: 198;
  --ui-chroma-2: 1;
  --ui-hue-neutral: 265;
  --ui-hue-rail: 275;
}

@import '@mnshahawy/daffa-console-ui/styles/tokens.css';
@import '@mnshahawy/daffa-console-ui/styles/fonts.css';
@import '@mnshahawy/daffa-console-ui/styles/base.css';

/* Tailwind must scan the package source for the utilities its components use. */
@source '../node_modules/@mnshahawy/daffa-console-ui/src';

@theme {
  --font-sans: 'IBM Plex Sans Variable', ui-sans-serif, system-ui, sans-serif;
  --font-mono: 'IBM Plex Mono', ui-monospace, 'SF Mono', monospace;
}
```

App startup:

```ts
import { initTheme, setAppName } from '@mnshahawy/daffa-console-ui'

setAppName('Diwan')
initTheme({ storageKey: 'diwan.theme' })
```

Mount `ToastHost` and `ConfirmHost` exactly once, in `App.vue`.

## What stays in the app

The generated `api.ts` / `caps.ts`, `nav.ts`, the session store, the router, `AppShell`
(built from `RailItem`s), brand marks, and every domain component. The rule of thumb: if
it names a domain noun (container, mailbox, cell), it does not belong here.

## Versioning / publishing

Published to GitHub Packages from this repo's release workflow alongside the Daffa image,
under the same version number, authenticated with the workflow's own `GITHUB_TOKEN` — there
is no npm publish secret to rotate, and the package is owned by the repository that builds
it. Consumers pin a caret range; breaking component API changes are a major bump like
anywhere else.
