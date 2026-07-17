import DefaultTheme from 'vitepress/theme'
import { h } from 'vue'
import type { Theme } from 'vitepress'
import ThemeToggle from './components/ThemeToggle.vue'
import ApiReference from './components/ApiReference.vue'
import './custom.css'

export default {
  extends: DefaultTheme,
  Layout() {
    // Our own theme toggle lives in the nav bar (VitePress's built-in one is disabled so a
    // single data-theme mechanism drives both the docs and the console).
    return h(DefaultTheme.Layout, null, {
      'nav-bar-content-after': () => h(ThemeToggle),
    })
  },
  enhanceApp({ app }) {
    app.component('ApiReference', ApiReference)
  },
} satisfies Theme
