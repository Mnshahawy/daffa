<script setup lang="ts">
import { RouterLink } from 'vue-router'
import type { RouteLocationRaw } from 'vue-router'
import AppIcon from './AppIcon.vue'

/**
 * Every page starts with one of these, so every page starts the same way.
 *
 * The crumb trail is the fix for the thing that made detail pages feel like a dead end: a
 * deployment page used to be reachable from two places and led back to neither, so the only
 * way up was the browser's Back button. Now the path is on the page, and it is the same path
 * whichever route you arrived by.
 *
 * Portainer renders TWO rows of chrome before any content — a breadcrumb bar and then a
 * separate title bar, each with its own controls. One row. Vertical space on a 13" laptop is
 * the scarcest thing in an ops console.
 */
defineProps<{
  title: string
  /** e.g. "12 of 40" — the count belongs beside the noun, not in a corner. */
  count?: string | number
  description?: string
  /** Ancestors, nearest last. The current page is the title and is not repeated here. */
  crumbs?: { label: string; to: RouteLocationRaw }[]
}>()
</script>

<template>
  <header class="mb-5">
    <nav v-if="crumbs?.length" class="mb-1.5 flex items-center gap-1 text-xs" aria-label="Breadcrumb">
      <template v-for="(c, i) in crumbs" :key="i">
        <RouterLink
          :to="c.to"
          class="subtle rounded px-1 py-0.5 transition hover:text-[var(--text)]"
        >
          {{ c.label }}
        </RouterLink>
        <AppIcon name="chevronRight" class="size-3 shrink-0 opacity-40" />
      </template>
    </nav>

    <div class="flex flex-wrap items-center gap-x-3 gap-y-2">
      <h1 class="text-xl font-semibold tracking-[-0.01em]">{{ title }}</h1>

      <span
        v-if="count !== undefined"
        class="rounded-md px-1.5 py-0.5 font-mono text-xs subtle"
        :style="{ background: 'var(--surface-sunken)' }"
      >
        {{ count }}
      </span>

      <!-- Actions sit on the title line, right-aligned. They are the reason you came. -->
      <div class="ml-auto flex flex-wrap items-center gap-2">
        <slot name="actions" />
      </div>
    </div>

    <p v-if="description" class="muted mt-1.5 max-w-2xl text-sm leading-relaxed">
      {{ description }}
    </p>
  </header>
</template>
