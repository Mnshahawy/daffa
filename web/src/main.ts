import { createApp } from 'vue'
import { createPinia } from 'pinia'
import { VueQueryPlugin } from '@tanstack/vue-query'

import { initTheme, setAppName } from '@mnshahawy/daffa-console-ui'

import App from './App.vue'
import { router } from './router'
import './style.css'

setAppName('Daffa')
// Before mount, so a stored preference does not flash the wrong theme first. The storage
// key predates the shared kit — keeping it means existing preferences survive.
initTheme({ storageKey: 'daffa.theme' })

createApp(App)
  .use(createPinia())
  .use(router)
  .use(VueQueryPlugin, {
    queryClientConfig: {
      defaultOptions: {
        queries: {
          // Docker events drive invalidation, so background refetching would just
          // be duplicate work. Lists still refresh on focus in case a stream died.
          refetchOnWindowFocus: true,
          refetchInterval: false,
          staleTime: 5_000,
          retry: 1,
        },
      },
    },
  })
  .mount('#app')
