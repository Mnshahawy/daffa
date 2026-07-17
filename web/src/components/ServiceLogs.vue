<script setup lang="ts">
import { onUnmounted, ref, watch } from 'vue'
import { streamServiceLogs, type LogLine } from '@/lib/stream'

const props = defineProps<{ env: string; service: string }>()

// A ring buffer, for the same reason the container log viewer has one: a chatty service will
// happily produce more lines than a browser can hold, and the lines you want are the last ones.
const MAX = 2000
const lines = ref<LogLine[]>([])
const connected = ref(false)

let stop: (() => void) | undefined

watch(
  () => [props.env, props.service],
  ([env, service]) => {
    stop?.()
    lines.value = []
    connected.value = false
    if (!env || !service) return

    stop = streamServiceLogs(env, service, {
      onMessage: (line) => {
        lines.value.push(line)
        if (lines.value.length > MAX) lines.value.splice(0, lines.value.length - MAX)
        connected.value = true
      },
    })
  },
  { immediate: true },
)

onUnmounted(() => stop?.())
</script>

<template>
  <div
    class="overflow-auto rounded-[var(--radius-card)] p-4 font-mono text-xs leading-relaxed"
    :style="{ background: 'var(--surface-sunken)', maxHeight: '60vh' }"
  >
    <p v-if="!lines.length" class="muted">
      Waiting for output. Swarm collects these from every node running a task, so they arrive even
      for machines Daffa has no agent on.
    </p>

    <div
      v-for="(l, i) in lines"
      :key="i"
      :style="{ color: l.stream === 'stderr' ? 'var(--danger)' : 'var(--text)' }"
    >
      {{ l.text }}
    </div>
  </div>
</template>
