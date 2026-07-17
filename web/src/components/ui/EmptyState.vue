<script setup lang="ts">
import AppIcon from './AppIcon.vue'
import type { IconName } from '@/lib/icons'

/**
 * An empty screen is an invitation to act, not a shrug.
 *
 * "No stacks on this environment yet." is a fact. It is not an answer to the question the
 * person actually has, which is "so what do I do?". Every empty state here says what the
 * thing is FOR and puts the way to make one directly underneath.
 */
defineProps<{
  icon: IconName
  title: string
  /** What this kind of thing is for. One sentence, no apology. */
  body?: string
}>()
</script>

<template>
  <div class="surface flex flex-col items-center rounded-[var(--radius-card)] px-6 py-14 text-center">
    <div
      class="mb-4 grid size-11 place-items-center rounded-xl"
      :style="{ background: 'var(--surface-sunken)', color: 'var(--text-subtle)' }"
    >
      <AppIcon :name="icon" class="size-5" />
    </div>

    <h2 class="text-sm font-semibold">{{ title }}</h2>
    <p v-if="body" class="muted mt-1.5 max-w-sm text-sm leading-relaxed">{{ body }}</p>

    <div v-if="$slots.action" class="mt-5">
      <slot name="action" />
    </div>
  </div>
</template>
