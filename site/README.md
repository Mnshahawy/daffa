# Daffa docs site

The public documentation at <https://mnshahawy.github.io/daffa/>, built with
[VitePress](https://vitepress.dev). Fully static output — deployable to GitHub Pages or
Cloudflare Pages with no server.

## Single source of truth

The site shares Daffa's real assets instead of copying them, so the two cannot drift:

- **Design tokens** — `../brand/tokens.css`, the exact colour system the console imports
  (`web/src/style.css`). The theme (`.vitepress/theme/custom.css`) maps VitePress's
  `--vp-c-*` variables onto it. Light/dark switches on `data-theme` + the OS media query,
  identical to the console (VitePress's own `.dark` appearance is disabled).
- **Fonts** — `../brand/fonts/*.woff2`, the same IBM Plex faces, bundled by Vite.
- **API reference** — generated from `../internal/api/openapi.json`. `scripts/prebuild.mjs`
  copies it (and `../web/public/mark.svg`) into `public/` on every build, so the reference
  always matches the routes. Regenerate the spec with `go generate ./internal/api`.

`public/` is git-ignored; it is reproduced on every build.

## Develop

```sh
pnpm install
pnpm dev        # http://localhost:5173, hot reload (runs the asset prebuild first)
pnpm build      # static output → .vitepress/dist
pnpm preview    # serve the built output
```

## Content

Markdown under `site/`, with the nav and sidebar declared in `.vitepress/config.mts`:

```
index.md                 home (hero + feature grid)
guide/                   introduction, getting started, features, stacks, backups, security
reference/api.md         mounts <ApiReference/> (renders openapi.json)
reference/configuration.md
```

## Deploy

**GitHub Pages** — automatic via `.github/workflows/docs.yml` on pushes to `main` that touch
the docs, `brand/`, the OpenAPI spec, or the logo. It builds with `DOCS_BASE=/daffa/`
(project Pages serve under `/<repo>/`) and publishes `.vitepress/dist`.

**Cloudflare Pages** — point a project at this repo with:

| Setting | Value |
| --- | --- |
| Root directory | `site` |
| Build command | `pnpm install --frozen-lockfile && pnpm build` |
| Build output directory | `.vitepress/dist` |

Cloudflare serves at the domain root, so leave `DOCS_BASE` unset (defaults to `/`). The
prebuild reaches up to the repo root for the OpenAPI spec and logo, so the whole repository
must be checked out (it is, by default).
