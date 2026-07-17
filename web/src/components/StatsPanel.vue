<script setup lang="ts">
import { onUnmounted, ref, watch } from 'vue'
import { streamStats, type StatsSample } from '@/lib/stream'
import { bytes } from '@/lib/api'

// node: which machine the container is on. Stats are node-local — no manager can read the
// stats of a container on another machine — so on a Swarm this is how the stream is routed.
const props = defineProps<{ env: string; container: string; running: boolean; node?: string }>()

const stats = ref<StatsSample | null>(null)

// A short history, purely so the numbers have shape. This is not a metrics system and
// should not pretend to be one — real history lives in whatever you already ship logs
// and metrics to.
const HISTORY = 60
const cpuHistory = ref<number[]>([])

let stop: (() => void) | undefined

watch(
  [() => props.env, () => props.container, () => props.running, () => props.node],
  ([env, id, running, node]) => {
    stop?.()
    stats.value = null
    cpuHistory.value = []
    if (!env || !id || !running) return

    stop = streamStats(
      env,
      id,
      {
        onMessage: (s) => {
          stats.value = s
          const next = [...cpuHistory.value, s.cpu]
          cpuHistory.value = next.length > HISTORY ? next.slice(-HISTORY) : next
        },
      },
      node as string | undefined,
    )
  },
  { immediate: true },
)
onUnmounted(() => stop?.())

/**
 * Vertical headroom, in viewBox units, kept clear at the top and bottom of the plot.
 *
 * Without it a value of 0 lands on y=100 — exactly the bottom edge of the viewBox. An SVG clips
 * at its viewport, so a stroke centred on the edge loses half its width, and what is left of a
 * 1.5px line is a sub-pixel sliver that on a dark background is simply not there. An idle
 * container drew NOTHING, which reads as "no data" when the truth is "zero, and here it is".
 *
 * The same thing happens at the top whenever the load is flat: every sample equals the peak, so
 * every point maps to y=0 and the whole trace is half-clipped against the top edge.
 */
const PAD = 6

/** Where a zero sample sits. The baseline is drawn here, so a flat trace lies visibly on it. */
const FLOOR = 100 - PAD

// A sparkline scaled to its own peak: the shape of the load is what you read here, and a fixed
// 0-100% axis would flatten everything interesting on an idle container.
function sparkline(values: number[]): string {
  if (values.length < 2) return ''

  // Floored at 1: a container flickering between 0.1% and 0.3% has a peak worth ignoring, and
  // scaling to it would turn sampling noise into a mountain range.
  const max = Math.max(...values, 1)
  const span = 100 - PAD * 2

  return values
    .map((v, i) => {
      const x = (i / (values.length - 1)) * 100
      const y = PAD + (1 - Math.min(v / max, 1)) * span
      return `${i === 0 ? 'M' : 'L'}${x.toFixed(1)},${y.toFixed(1)}`
    })
    .join(' ')
}
</script>

<template>
  <div class="surface rounded-[var(--radius-card)] p-4">
    <p v-if="!running" class="muted text-sm">Not running — no stats to report.</p>
    <p v-else-if="!stats" class="muted text-sm">Sampling…</p>

    <div v-else class="space-y-4">
      <div class="grid grid-cols-2 gap-4 sm:grid-cols-4">
        <div>
          <div class="eyebrow">CPU</div>
          <div class="mt-1 font-mono text-xl font-semibold">{{ stats.cpu.toFixed(1) }}%</div>
        </div>
        <div>
          <div class="eyebrow">Memory</div>
          <div class="mt-1 font-mono text-xl font-semibold">{{ bytes(stats.memory) }}</div>
          <div class="subtle mt-0.5 font-mono text-xs">
            {{ stats.mem_pct.toFixed(1) }}% of {{ bytes(stats.mem_limit) }}
          </div>
        </div>
        <div>
          <div class="eyebrow">Network</div>
          <div class="mt-1 font-mono text-sm">↓ {{ bytes(stats.net_rx) }}</div>
          <div class="font-mono text-sm">↑ {{ bytes(stats.net_tx) }}</div>
        </div>
        <div>
          <div class="eyebrow">Block I/O</div>
          <div class="mt-1 font-mono text-sm">R {{ bytes(stats.block_read) }}</div>
          <div class="font-mono text-sm">W {{ bytes(stats.block_write) }}</div>
        </div>
      </div>

      <!-- The live CPU trace. One series, so it is the brand colour — this is not a status.
           It spans the full width under all four figures, so it has to say which one it plots. -->
      <div v-if="cpuHistory.length > 1">
        <div class="eyebrow mb-1">CPU · live</div>

        <svg
          class="h-16 w-full"
          viewBox="0 0 100 100"
          preserveAspectRatio="none"
          role="img"
          :aria-label="`CPU over the last ${cpuHistory.length} samples, currently ${stats.cpu.toFixed(1)}%`"
        >
          <!-- Zero. A trace lying flat on this line reads as "idle"; the same trace with no line
               under it reads as "broken", which is what an idle container used to look like. -->
          <line
            x1="0"
            :y1="FLOOR"
            x2="100"
            :y2="FLOOR"
            stroke="var(--border)"
            stroke-width="1"
            vector-effect="non-scaling-stroke"
          />

          <path
            :d="sparkline(cpuHistory)"
            fill="none"
            stroke="var(--accent)"
            stroke-width="1.5"
            stroke-linejoin="round"
            vector-effect="non-scaling-stroke"
          />
        </svg>
      </div>
    </div>
  </div>
</template>
