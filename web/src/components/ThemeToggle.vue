<script setup lang="ts">
import { theme, type Theme } from '@/lib/theme'
import AppIcon from './ui/AppIcon.vue'
import type { IconName } from '@/lib/icons'

// Three options, not a two-way switch. "System" is a real answer — most people want the app to
// follow their OS — and a binary toggle silently takes that choice away the first time it is
// touched.
const options: { id: Theme; label: string; icon: IconName }[] = [
  { id: 'system', label: 'System', icon: 'monitor' },
  { id: 'light', label: 'Light', icon: 'sun' },
  { id: 'dark', label: 'Dark', icon: 'moon' },
]
</script>

<template>
  <div class="px-2 py-1.5">
    <div class="eyebrow mb-1.5">Theme</div>

    <div
      class="flex gap-0.5 rounded-lg p-0.5"
      :style="{ background: 'var(--surface-sunken)' }"
      role="radiogroup"
      aria-label="Theme"
    >
      <button
        v-for="o in options"
        :key="o.id"
        role="radio"
        :aria-checked="theme === o.id"
        :title="o.label"
        class="flex flex-1 items-center justify-center gap-1.5 rounded-md px-2 py-1 text-xs transition"
        :class="theme === o.id ? 'font-medium shadow-[var(--shadow-raised)]' : 'muted hover:text-[var(--text)]'"
        :style="
          theme === o.id
            ? { background: 'var(--surface-raised)', color: 'var(--accent-text)' }
            : undefined
        "
        @click.stop="theme = o.id"
      >
        <AppIcon :name="o.icon" class="size-3.5" />
        {{ o.label }}
      </button>
    </div>
  </div>
</template>
