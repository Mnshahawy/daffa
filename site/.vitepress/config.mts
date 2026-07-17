import { defineConfig } from 'vitepress'
import { fileURLToPath } from 'node:url'
import { resolve } from 'node:path'

// GitHub project Pages serve under /<repo>/; Cloudflare Pages and custom domains serve at
// root. So the base is an input, not a constant: the docs workflow sets DOCS_BASE=/daffa/,
// everything else defaults to '/'.
const base = process.env.DOCS_BASE || '/'

// The colour system and fonts live in the repo-root brand/ directory, imported by the theme
// CSS. Let Vite's dev server read outside site/.
const repoRoot = resolve(fileURLToPath(import.meta.url), '../../..')

export default defineConfig({
  lang: 'en-US',
  title: 'Daffa',
  description:
    'Daffa is a lean, self-hosted console for operating Docker containers and deploying Compose stacks across one host or many. One static Go binary, no telemetry, Apache-2.0.',
  base,

  // The console themes via `data-theme` + the OS media query; the docs match it exactly with
  // a custom toggle (see theme/), so brand/tokens.css drives both surfaces from one file.
  // VitePress's own `.dark`-class appearance is therefore turned off.
  appearance: false,
  cleanUrls: true,
  lastUpdated: true,

  // localhost URLs are legitimate in install docs; don't treat them as dead links.
  ignoreDeadLinks: [/^https?:\/\/localhost/],

  head: [
    ['link', { rel: 'icon', type: 'image/svg+xml', href: `${base}mark.svg` }],
    ['meta', { name: 'theme-color', content: '#5943bf' }],
    // Apply the theme before first paint so there is no flash. Sets `data-theme` (which
    // drives brand/tokens.css, same as the console) AND toggles VitePress's own `.dark`
    // class so its internals — Shiki code highlighting especially — follow along.
    [
      'script',
      {},
      `try{var d=document.documentElement,c=localStorage.getItem('daffa-theme');if(c==='light'||c==='dark')d.setAttribute('data-theme',c);d.classList.toggle('dark',c==='dark'||(c!=='light'&&matchMedia('(prefers-color-scheme: dark)').matches));}catch(e){}`,
    ],
  ],

  themeConfig: {
    logo: '/mark.svg',
    siteTitle: 'Daffa',

    nav: [
      { text: 'Getting started', link: '/guide/getting-started' },
      { text: 'Features', link: '/guide/features' },
      { text: 'Security', link: '/guide/security' },
      { text: 'API', link: '/reference/api' },
    ],

    sidebar: {
      '/guide/': [
        {
          text: 'Guide',
          items: [
            { text: 'Introduction', link: '/guide/' },
            { text: 'Getting started', link: '/guide/getting-started' },
            { text: 'Features', link: '/guide/features' },
            { text: 'Stacks & deployments', link: '/guide/stacks' },
            { text: 'Backups', link: '/guide/backups' },
            { text: 'Security & access', link: '/guide/security' },
          ],
        },
      ],
      '/reference/': [
        {
          text: 'Reference',
          items: [
            { text: 'API reference', link: '/reference/api' },
            { text: 'Configuration', link: '/reference/configuration' },
          ],
        },
      ],
    },

    socialLinks: [{ icon: 'github', link: 'https://github.com/Mnshahawy/daffa' }],
    search: { provider: 'local' },

    editLink: {
      pattern: 'https://github.com/Mnshahawy/daffa/edit/main/site/:path',
      text: 'Edit this page on GitHub',
    },

    footer: {
      message: 'دفّة — the helm. Released under the Apache-2.0 License.',
      copyright: 'Copyright © 2026 Mohamed Elshahawi',
    },
  },

  vite: {
    server: { fs: { allow: [repoRoot] } },
  },
})
