import { createApp } from 'vue'
import { createPinia } from 'pinia'
import { VueQueryPlugin } from '@tanstack/vue-query'

import App from './App.vue'
import { router } from './router'
import { initTheme } from './lib/theme'
import './style.css'

// Before mount, so a stored preference does not flash the wrong theme first.
initTheme()

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
