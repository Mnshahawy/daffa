<script setup lang="ts">
import { useRoute } from 'vue-router'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'

// A URL that matches no route — a mistyped path, a stale bookmark, a link to something that has
// since been renamed. It renders INSIDE the app shell (it is an AppShell child, not a top-level
// route), so the rail and switcher stay put and the reader is one click from anywhere rather than
// stranded on a bare page. No meta.cap: not being found is not a permission.
//
// It shows the path that missed, because "page not found" without saying WHICH page is the kind of
// message that makes someone doubt they typed it right when they did.
const route = useRoute()
</script>

<template>
  <div class="mx-auto max-w-lg py-20">
    <div class="surface rounded-[var(--radius-card)] p-8 text-center">
      <div
        class="mx-auto mb-4 grid size-11 place-items-center rounded-xl"
        :style="{ background: 'var(--surface-sunken)', color: 'var(--text-subtle)' }"
      >
        <AppIcon name="compass" class="size-5" />
      </div>

      <h1 class="text-base font-semibold">This page does not exist</h1>

      <p class="muted mx-auto mt-2 max-w-md text-sm leading-relaxed">
        Nothing here answers to that address. The link may be out of date, or the thing it pointed
        at may have been renamed or removed.
      </p>

      <!-- The address that missed, in mono — so a mistype is obvious and a stale link is
           quotable when reporting it. -->
      <div
        class="mt-5 rounded-[var(--radius-control)] px-4 py-3 text-left"
        :style="{ background: 'var(--surface-sunken)' }"
      >
        <p class="eyebrow">You asked for</p>
        <p class="mt-1 break-all font-mono text-sm">{{ route.fullPath }}</p>
      </div>

      <div class="mt-6 flex items-center justify-center gap-2">
        <BaseButton intent="secondary" :to="{ name: 'overview' }">
          <AppIcon name="rocket" class="size-3.5" />
          Back to Daffa
        </BaseButton>

        <BaseButton intent="ghost" @click="$router.back()">
          <AppIcon name="chevronRight" class="size-3.5 rotate-180" />
          Go back
        </BaseButton>
      </div>
    </div>
  </div>
</template>
