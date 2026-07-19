<script setup lang="ts">
import { dismiss, toasts } from '@/lib/toast'
import { toneSoftVar, toneVar } from '@/lib/status'
import AppIcon from './AppIcon.vue'
import type { IconName } from '@/lib/icons'

// Mounted once, in App.vue. Every toast in the product renders here — see lib/toast.ts for
// why there is exactly one channel and why confirmations leave but errors linger.

// The tone already carries the colour (toneVar); the glyph just makes success/failure legible
// at a glance without reading, for the peripheral case where the toast appears while you are
// looking elsewhere on the page.
const icon: Record<string, IconName> = {
  success: 'check',
  danger: 'x',
  warn: 'alert',
  info: 'alert',
}
</script>

<template>
  <Teleport to="body">
    <!-- aria-live=polite, not assertive: a "Saved" should not interrupt a screen reader
         mid-sentence, and a failure the user just caused is expected, not an emergency. -->
    <div
      class="pointer-events-none fixed inset-x-0 bottom-0 z-[60] flex flex-col items-center gap-2 p-4 sm:items-end"
      role="region"
      aria-live="polite"
      aria-label="Notifications"
    >
      <TransitionGroup
        enter-active-class="transition duration-200 ease-out"
        enter-from-class="translate-y-2 opacity-0"
        leave-active-class="transition duration-150 ease-in absolute"
        leave-to-class="translate-y-1 opacity-0"
        move-class="transition duration-150 ease-out"
      >
        <div
          v-for="t in toasts"
          :key="t.id"
          class="pointer-events-auto flex w-full max-w-sm items-start gap-2.5 rounded-lg p-3 pr-2.5 shadow-[var(--shadow-overlay)]"
          style="background: var(--surface-overlay); border: 1px solid var(--border)"
          role="status"
        >
          <div
            class="mt-px grid size-5 shrink-0 place-items-center rounded-md"
            :style="{ background: toneSoftVar[t.tone], color: toneVar[t.tone] }"
          >
            <AppIcon :name="icon[t.tone]" class="size-3.5" />
          </div>

          <p class="min-w-0 flex-1 text-sm leading-snug break-words">{{ t.message }}</p>

          <!-- A toast dismisses itself, but a long error you have finished reading should not
               make you wait out its timer. -->
          <button
            class="btn btn-ghost btn-icon btn-xs -my-0.5 -mr-0.5 shrink-0"
            aria-label="Dismiss"
            @click="dismiss(t.id)"
          >
            <AppIcon name="x" class="size-3.5" />
          </button>
        </div>
      </TransitionGroup>
    </div>
  </Teleport>
</template>
