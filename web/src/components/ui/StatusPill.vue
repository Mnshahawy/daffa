<script setup lang="ts">
import { computed } from 'vue'
import { toneSoftVar, toneVar, type Status } from '@/lib/status'

/**
 * The one way a state is shown. A dot, the word, and — when there is one — the detail that
 * saves you a trip to the logs ("exited · code 137").
 *
 * Colour is never the only carrier: the word is always there in `full` mode, and in `dot`
 * mode the state is in the tooltip and in an sr-only span. Roughly one reader in twelve
 * cannot tell the red dot from the green one, and a console where "is it up?" is answerable
 * only by hue is a console they cannot use.
 */
const props = withDefaults(defineProps<{ status: Status; variant?: 'full' | 'dot' }>(), {
  variant: 'full',
})

const color = computed(() => toneVar[props.status.tone])
const soft = computed(() => toneSoftVar[props.status.tone])

const title = computed(() =>
  props.status.detail ? `${props.status.label} — ${props.status.detail}` : props.status.label,
)
</script>

<template>
  <span
    v-if="variant === 'dot'"
    class="inline-flex items-center"
    :title="title"
    role="status"
    :style="{ color }"
  >
    <span class="relative flex size-2">
      <!-- The pulse only ever means "this is in flight right now". -->
      <span v-if="status.live" class="pulse-ring absolute inset-0" />
      <span class="relative size-2 rounded-full bg-current" />
    </span>
    <span class="sr-only">{{ title }}</span>
  </span>

  <span
    v-else
    class="inline-flex items-center gap-1.5 rounded-md px-1.5 py-0.5 text-xs font-medium"
    role="status"
    :style="{ color, background: soft }"
  >
    <span class="relative flex size-1.5 shrink-0">
      <span v-if="status.live" class="pulse-ring absolute inset-0" />
      <span class="relative size-1.5 rounded-full bg-current" />
    </span>
    {{ status.label }}
    <span v-if="status.detail" class="font-mono text-[10px] opacity-70">{{ status.detail }}</span>
  </span>
</template>
