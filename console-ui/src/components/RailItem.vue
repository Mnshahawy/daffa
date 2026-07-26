<script setup lang="ts">
import { RouterLink, type RouteLocationRaw } from 'vue-router'
import AppIcon from './AppIcon.vue'
import type { IconName } from '../lib/icons'

/**
 * One entry in the nav rail: icon + truncated label + the accent bar when active.
 *
 * This exact RouterLink block was hand-repeated three times inside one AppShell (and again
 * in the next app's), which is how one copy ends up with a different active treatment than
 * its neighbours. The shell keeps its own layout and grouping; the row itself is written
 * once, here.
 *
 * `active` is a prop, not computed here — only the shell knows whether a detail route
 * should light its parent section up.
 */
defineProps<{
  to: RouteLocationRaw
  icon: IconName
  label: string
  active?: boolean
  /** Rail collapsed to icons-only: hide the label, center the icon, tooltip the name. */
  collapsed?: boolean
}>()
</script>

<template>
  <RouterLink
    :to="to"
    class="relative flex items-center gap-2.5 rounded-[var(--radius-control)] px-2.5 py-2 text-sm transition"
    :class="[
      collapsed ? 'justify-center px-0' : '',
      active ? 'font-medium' : 'muted hover:bg-[var(--rail-hover)] hover:text-[var(--text)]',
    ]"
    :style="active ? { background: 'var(--accent-soft)', color: 'var(--accent-text)' } : undefined"
    :title="collapsed ? label : undefined"
  >
    <span
      v-if="active"
      class="absolute left-0 top-1/2 h-5 w-[3px] -translate-y-1/2 rounded-r-full"
      :style="{ background: 'var(--accent)' }"
    />
    <AppIcon :name="icon" class="size-4 shrink-0" />
    <span v-if="!collapsed" class="truncate">{{ label }}</span>
  </RouterLink>
</template>
