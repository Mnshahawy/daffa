<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { streamServiceLogs, type LogLine } from '@/lib/stream'

const props = defineProps<{ env: string; service: string }>()

// A ring buffer, for the same reason the container log viewer has one: a chatty service will
// happily produce more lines than a browser can hold, and the lines you want are the last ones.
const MAX = 2000
const lines = ref<LogLine[]>([])
const connected = ref(false)

// FILTER BY TASK. A merged stream is exactly what you want until one replica misbehaves and you need
// only its lines. The choices are the tasks actually SEEN in the stream, accumulated separately from
// the ring buffer so a filter does not disappear the moment its task goes quiet and scrolls out.
const taskFilter = ref('') // '' = every task
const seenTasks = ref<string[]>([])

function noteTask(task?: string) {
  if (task && !seenTasks.value.includes(task)) seenTasks.value = [...seenTasks.value, task].sort()
}

const visibleLines = computed(() =>
  taskFilter.value ? lines.value.filter((l) => l.task === taskFilter.value) : lines.value,
)

function toggleTask(task?: string) {
  if (!task) return
  taskFilter.value = taskFilter.value === task ? '' : task
}

// Non-fatal notices from the server — a Swarm node the manager cannot reach, so some tasks' logs
// are missing. The server already de-dupes, but a stream that reconnects could repeat one, so the
// set does too. These do NOT stop the log; they sit above it as a standing caveat.
const warnings = ref<{ text: string; detail: string }[]>([])

function addWarning(raw: string) {
  const missing = /not available|incomplete log stream|could not be retrieved/i.test(raw)
  const text = missing
    ? "Some tasks' logs are missing — one or more nodes can't be reached. Showing the rest."
    : raw
  if (warnings.value.some((w) => w.text === text)) return
  warnings.value.push({ text, detail: raw })
}

// Colour per task, so a merged stream reads like docker-compose's: the same replica always the same
// hue, and you follow one thread down the page by colour alone. Hashed, not assigned, so it is
// stable across reconnects without tracking which colours are taken. Mid-tone values chosen to stay
// legible on the sunken surface in both themes.
const PALETTE = [
  '#e5637d',
  '#d98c3f',
  '#bf9f2b',
  '#5aa469',
  '#3fa7a7',
  '#5b8dd6',
  '#9b7bd4',
  '#c56bb0',
]
function tagColor(task: string): string {
  let h = 0
  for (let i = 0; i < task.length; i++) h = (h * 31 + task.charCodeAt(i)) >>> 0
  return PALETTE[h % PALETTE.length]
}

let stop: (() => void) | undefined

watch(
  () => [props.env, props.service],
  ([env, service]) => {
    stop?.()
    lines.value = []
    warnings.value = []
    seenTasks.value = []
    taskFilter.value = ''
    connected.value = false
    if (!env || !service) return

    stop = streamServiceLogs(env, service, {
      onMessage: (line) => {
        lines.value.push(line)
        if (lines.value.length > MAX) lines.value.splice(0, lines.value.length - MAX)
        noteTask(line.task)
        connected.value = true
      },
      onWarn: addWarning,
    })
  },
  { immediate: true },
)

onUnmounted(() => stop?.())
</script>

<template>
  <div class="space-y-2">
    <!-- The partial-coverage caveat: shown once, amber (a warning, not a fault), and it never stops
         the logs below from streaming. Hover for the daemon's own words. -->
    <div
      v-for="w in warnings"
      :key="w.text"
      class="flex items-start gap-2 rounded-[var(--radius-control)] px-3 py-2 text-xs"
      :style="{ background: 'var(--warn-soft)', color: 'var(--warn)' }"
      :title="w.detail"
    >
      <span aria-hidden="true">⚠</span>
      <span>{{ w.text }}</span>
    </div>

    <!-- Filter by task — one chip per replica the stream has shown, coloured to match its lines, so
         picking one is the same gesture as clicking a line's tag. Only worth showing once there is
         more than one task to choose between. -->
    <div v-if="seenTasks.length > 1" class="flex flex-wrap items-center gap-1.5 text-xs">
      <button
        class="rounded-full px-2.5 py-0.5 transition"
        :style="
          taskFilter === ''
            ? { background: 'var(--accent-soft)', color: 'var(--accent-text)' }
            : { color: 'var(--text-muted)' }
        "
        @click="taskFilter = ''"
      >
        All
      </button>
      <button
        v-for="t in seenTasks"
        :key="t"
        class="rounded-full px-2.5 py-0.5 font-mono transition"
        :style="
          taskFilter === t
            ? { background: tagColor(t), color: '#fff' }
            : { color: tagColor(t), border: '1px solid var(--border)' }
        "
        @click="toggleTask(t)"
      >
        {{ t }}
      </button>
    </div>

    <div
      class="overflow-auto rounded-[var(--radius-card)] p-4 font-mono text-xs leading-relaxed"
      :style="{ background: 'var(--surface-sunken)', maxHeight: '60vh' }"
    >
      <p v-if="!lines.length" class="muted">
        Waiting for output. Swarm collects these from every node running a task, so they arrive even
        for machines Daffa has no agent on.
      </p>
      <p v-else-if="!visibleLines.length" class="muted">
        No lines from <span class="font-mono">{{ taskFilter }}</span> in the buffer yet.
      </p>

      <div v-for="(l, i) in visibleLines" :key="i" class="flex gap-2">
        <!-- Which replica said it. Fixed-ish column so the messages line up; the machine it ran on
             is on hover, because the hostname is long and the same on every line from a node.
             Clicking it filters to that task — the tag IS the control. -->
        <button
          v-if="l.task"
          type="button"
          class="inline-block shrink-0 cursor-pointer select-none text-left tabular-nums hover:underline"
          :style="{ color: tagColor(l.task), minWidth: '7ch' }"
          :title="l.node ? `${l.task} · ${l.node} — click to filter` : `${l.task} — click to filter`"
          @click="toggleTask(l.task)"
          >{{ l.task }}</button
        ><span v-else class="shrink-0" :style="{ minWidth: '7ch' }"></span>
        <span
          class="min-w-0 whitespace-pre-wrap break-all"
          :style="{ color: l.stream === 'stderr' ? 'var(--danger)' : 'var(--text)' }"
          >{{ l.text }}</span
        >
      </div>
    </div>
  </div>
</template>
