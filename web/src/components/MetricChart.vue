<script setup lang="ts">
/**
 * A CPU or memory chart, drawn by hand in SVG.
 *
 * No charting library. The whole application is about 51 KB gzipped and the smallest of them
 * would roughly double that, to draw two line charts. What is actually needed here is a
 * polyline and an axis, and that is ninety lines.
 *
 * The server has already bucketed the data to ~240 points and given each bucket an average AND
 * a maximum, so there is no downsampling to do here — only drawing. The band between the two
 * is the point of the chart: a mean alone smooths away the spike you opened it to find, and a
 * maximum alone makes a container that blipped once look permanently on fire.
 *
 * Every colour in here is a token, and that is not housekeeping. An UNDEFINED custom property
 * makes the whole declaration invalid, and an invalid property falls back to its initial value —
 * which for `stroke` is `none`. `stroke: var(--accent)` against a token nobody defined draws
 * NOTHING: no error, no warning, and a chart that looks like it has no data. That shipped once.
 * internal/web/css_test.go now fails the build if any var() here is not defined in style.css.
 *
 * The two marks are the two brand hues, never a status hue: the average is the accent, the
 * peak envelope is marine. Green, amber and red mean state in this app, and a chart is not a
 * state.
 */
import { computed, ref } from 'vue'
import type { MetricPoint, MetricRange } from '@/lib/api'

const props = defineProps<{
  points: MetricPoint[]
  kind: 'cpu' | 'memory'
  range: MetricRange
  /** Memory only: the limit, so the axis can be a share of it rather than an arbitrary peak. */
  loading?: boolean
}>()

// The SVG's internal coordinate space. It scales to whatever width the container gives it, so
// these are not pixels — they are just a convenient grid to draw on.
const W = 600
const H = 140
const PAD_L = 4
const PAD_B = 16

// Headroom at the top, for the same reason StatsPanel keeps some: an SVG clips at its viewport,
// so a line drawn on y=0 loses half its stroke width. CPU's axis tops out at exactly 100, which
// means a container pegged at 100% — the one moment you are certainly looking at this chart —
// would have its trace half-erased against the top edge.
const PAD_T = 3

interface Series {
  avg: number
  max: number
  ts: number
}

const series = computed<Series[]>(() =>
  props.points.map((p) => ({
    ts: new Date(p.ts).getTime(),
    avg: props.kind === 'cpu' ? p.cpu_avg : p.mem_avg,
    max: props.kind === 'cpu' ? p.cpu_max : p.mem_max,
  })),
)

const peak = computed(() => Math.max(...series.value.map((s) => s.max), 0))
const mean = computed(() =>
  series.value.length ? series.value.reduce((a, s) => a + s.avg, 0) / series.value.length : 0,
)

/**
 * The top of the y-axis.
 *
 * CPU is a percentage of the container's allowance, so it is always 0..100 and the axis is
 * fixed — which matters more than it sounds. An auto-scaled CPU axis makes a container idling
 * at 2% look identical to one pegged at 100%, and you only notice the difference by reading
 * the numbers, which is exactly what a chart is for not having to do.
 *
 * Memory has no natural ceiling in bytes, so it scales to the peak, rounded up so the line does
 * not touch the top of the box.
 */
const yMax = computed(() => {
  if (props.kind === 'cpu') return 100
  return peak.value > 0 ? peak.value * 1.15 : 1
})

function x(i: number): number {
  if (series.value.length <= 1) return PAD_L
  return PAD_L + (i / (series.value.length - 1)) * (W - PAD_L * 2)
}

function y(v: number): number {
  const floor = H - PAD_B
  return floor - Math.min(v / yMax.value, 1) * (floor - PAD_T)
}

/** The filled band between the bucket averages and the bucket peaks. */
const band = computed(() => {
  if (!series.value.length) return ''
  const top = series.value.map((s, i) => `${x(i)},${y(s.max)}`).join(' ')
  const bottom = series.value
    .map((s, i) => `${x(i)},${y(s.avg)}`)
    .reverse()
    .join(' ')
  return `${top} ${bottom}`
})

const line = computed(() => series.value.map((s, i) => `${x(i)},${y(s.avg)}`).join(' '))

/** A few x-axis labels — first, middle, last. More than that and they collide. */
const ticks = computed(() => {
  const n = series.value.length
  if (n < 2) return []
  const at = [0, Math.floor(n / 2), n - 1]
  return at.map((i) => ({ x: x(i), label: timeLabel(series.value[i].ts) }))
})

function timeLabel(ts: number): string {
  const d = new Date(ts)
  // Over a day, the hour alone is ambiguous — say which day.
  if (props.range === '7d' || props.range === '24h') {
    return d.toLocaleString(undefined, { day: 'numeric', month: 'short', hour: '2-digit' })
  }
  return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })
}

function fmt(v: number): string {
  if (props.kind === 'cpu') return `${v.toFixed(0)}%`
  return bytes(v)
}

function bytes(v: number): string {
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${i === 0 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`
}

// The gridline values: quarters of the axis.
const gridlines = computed(() => [0.25, 0.5, 0.75, 1].map((f) => ({ y: y(yMax.value * f), v: yMax.value * f })))

// ── hover readout ───────────────────────────────────────────────────────────────
//
// The header shows the peak and average across the WHOLE window; hovering answers the other
// question — what was it doing at THIS moment. The chart is bucketed, so a hover lands on a
// bucket and reads back both of its numbers.
//
// These are POINTER events, not mouse events, so the same code serves a mouse hover and a finger
// dragged across the chart — see the template and `touch-action` for how touch keeps the page
// scrollable while still scrubbing.
const svgEl = ref<SVGSVGElement | null>(null)
const hoverIndex = ref<number | null>(null)

function onMove(e: PointerEvent) {
  const el = svgEl.value
  const n = series.value.length
  if (!el || n === 0) return
  const rect = el.getBoundingClientRect()
  if (rect.width === 0) return

  // The viewBox is stretched to the element width (preserveAspectRatio="none"), and that stretch
  // is linear in x — so the cursor's fraction across the element is its fraction across the data.
  // Invert x(i) rather than eyeballing it, so the dot sits on the sample and not a pixel beside it.
  const xSvg = ((e.clientX - rect.left) / rect.width) * W
  const i = n === 1 ? 0 : Math.round(((xSvg - PAD_L) / (W - PAD_L * 2)) * (n - 1))
  hoverIndex.value = Math.max(0, Math.min(n - 1, i))
}

// A mouse leaving the chart clears the readout — there is nothing to read once the cursor is gone.
// A lifting FINGER does not: pointerleave fires on touch-lift too, but on a phone there is no hover
// to fall back on, so a value that vanished the instant you lifted your finger would be one you
// could never actually read. The last reading stays on screen, to be replaced by the next touch or
// cleared if the gesture is cancelled (see onCancel).
function onLeave(e?: PointerEvent) {
  if (!e || e.pointerType === 'mouse') hoverIndex.value = null
}

// The browser cancels the pointer when it decides a gesture was a scroll after all — the finger was
// panning the page, not scrubbing the chart. That is not an inspection, so clear it.
function onCancel() {
  hoverIndex.value = null
}

// Everything the overlay needs, in ONE unit system: x as a percentage of the width (the SVG fills
// its box, so a percent maps straight through the stretch), y in pixels (the box is exactly H tall,
// so a viewBox y IS a pixel offset). That is what lets an HTML dot land on an SVG line without a
// resize observer.
const hover = computed(() => {
  const i = hoverIndex.value
  if (i == null) return null
  const s = series.value[i]
  if (!s) return null
  return {
    s,
    leftPct: (x(i) / W) * 100,
    avgTop: y(s.avg),
    maxTop: y(s.max),
    guideX: x(i),
  }
})

// Keep the tooltip inside the box: anchor it to the point near the middle, but let it hang off to
// one side as the point approaches an edge rather than clipping against it.
const tooltipStyle = computed(() => {
  const h = hover.value
  if (!h) return {}
  const align = h.leftPct < 22 ? 0 : h.leftPct > 78 ? -100 : -50
  return { left: `${h.leftPct}%`, transform: `translateX(${align}%)` }
})

function fullTime(ts: number): string {
  return new Date(ts).toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  })
}
</script>

<template>
  <div class="flex flex-col gap-1.5">
    <header class="flex items-baseline justify-between">
      <h4 class="eyebrow">{{ kind === 'cpu' ? 'CPU' : 'Memory' }}</h4>

      <!-- The two numbers, each swatched in the colour of the mark it belongs to, so the band
           and the line do not need a separate legend. -->
      <div v-if="series.length" class="muted flex items-baseline gap-3.5 text-xs">
        <span class="inline-flex items-center gap-1.5">
          <span class="size-1.5 rounded-full" :style="{ background: 'var(--color-marine-500)' }" />
          peak
          <strong class="font-mono font-medium" :style="{ color: 'var(--text)' }">
            {{ fmt(peak) }}
          </strong>
        </span>
        <span class="inline-flex items-center gap-1.5">
          <span class="size-1.5 rounded-full" :style="{ background: 'var(--accent)' }" />
          avg
          <strong class="font-mono font-medium" :style="{ color: 'var(--text)' }">
            {{ fmt(mean) }}
          </strong>
        </span>
      </div>
    </header>

    <p v-if="loading" class="empty">Loading…</p>

    <!--
      No samples. This is a normal state, not a failure: a container that started a minute ago
      has no history, and sampling can be switched off entirely. Say which, rather than showing
      an empty box that looks broken.
    -->
    <p v-else-if="!series.length" class="empty">
      No samples yet for this range. Resource monitoring may be off, or this container may be
      newer than the window.
    </p>

    <!-- The pointer handlers and touch-action live on this WRAPPER, not the <svg>. Chromium does
         not honour touch-action on an inline SVG element, so a horizontal scrub there would be
         swallowed as a scroll attempt and cancelled. On the HTML div it is honoured, and the div
         covers exactly the same box. -->
    <div
      v-else
      class="chart"
      @pointerdown="onMove"
      @pointermove="onMove"
      @pointerleave="onLeave"
      @pointercancel="onCancel"
    >
      <svg
        ref="svgEl"
        :viewBox="`0 0 ${W} ${H}`"
        preserveAspectRatio="none"
        role="img"
        :aria-label="`${kind} over the last ${range}`"
      >
        <line
          v-for="g in gridlines"
          :key="g.v"
          class="grid"
          :x1="PAD_L" :x2="W - PAD_L" :y1="g.y" :y2="g.y"
        />
        <polygon class="band" :points="band" />
        <polyline class="line" :points="line" />

        <!-- The hover guide. A vertical line survives the non-uniform stretch (it is one column
             wide) where a circle would become an ellipse — those are drawn in HTML below. -->
        <line
          v-if="hover"
          class="guide"
          :x1="hover.guideX" :x2="hover.guideX" :y1="PAD_T" :y2="H - PAD_B"
        />

        <text
          v-for="t in ticks"
          :key="t.label"
          class="tick"
          :x="t.x"
          :y="H - 4"
          :text-anchor="t.x < W / 4 ? 'start' : t.x > (W * 3) / 4 ? 'end' : 'middle'"
        >{{ t.label }}</text>
      </svg>

      <!-- Overlay: dots and tooltip in HTML, so nothing is distorted by the SVG stretch and the
           text reads at a real font size. pointer-events stay off so the SVG keeps the mouse. -->
      <template v-if="hover">
        <span
          class="dot"
          :style="{ left: `${hover.leftPct}%`, top: `${hover.maxTop}px`, background: 'var(--color-marine-500)' }"
        />
        <span
          class="dot"
          :style="{ left: `${hover.leftPct}%`, top: `${hover.avgTop}px`, background: 'var(--accent)' }"
        />
        <div class="tooltip" :style="tooltipStyle">
          <div class="t-time">{{ fullTime(hover.s.ts) }}</div>
          <div class="t-row">
            <span class="t-swatch" :style="{ background: 'var(--color-marine-500)' }" />
            peak
            <strong>{{ fmt(hover.s.max) }}</strong>
          </div>
          <div class="t-row">
            <span class="t-swatch" :style="{ background: 'var(--accent)' }" />
            avg
            <strong>{{ fmt(hover.s.avg) }}</strong>
          </div>
        </div>
      </template>
    </div>

    <!--
      The vertical scale, said once in words. Four gridline labels would be four numbers to
      read; "0 to 100%" is the whole of it, and for CPU it never changes — which is the point,
      because a fixed axis is what lets you compare two containers at a glance.
    -->
    <footer v-if="series.length" class="axis">
      vertical scale: 0 to {{ fmt(yMax) }}
    </footer>
  </div>
</template>

<style scoped>
/* The chart box: the SVG plus the HTML hover overlay share this coordinate frame. Exactly 140px
   tall, which is what lets a viewBox y double as a pixel offset for the overlay. */
.chart {
  position: relative;
  height: 140px;
  /* pan-y hands VERTICAL drags back to the page (so a finger swiping down the chart still scrolls),
     while HORIZONTAL drags come to us as pointer events to scrub the readout. Without this the
     browser would treat a horizontal drag as a scroll attempt and cancel the gesture — and it has
     to be on this HTML wrapper, because Chromium ignores touch-action on an inline <svg>. */
  touch-action: pan-y;
  /* A drag across the chart should not paint a text selection over the axis labels. */
  user-select: none;
  -webkit-user-select: none;
}

svg {
  width: 100%;
  height: 140px;
  overflow: visible;
  cursor: crosshair;
}

/* The hover guide. Dimmer than the line, so it locates without competing with it. */
.guide {
  stroke: var(--text-muted);
  stroke-width: 1;
  stroke-dasharray: 3 3;
  opacity: 0.5;
  vector-effect: non-scaling-stroke;
  pointer-events: none;
}

/* A dot on each mark. Centred on its point; drawn in HTML so it stays a circle. */
.dot {
  position: absolute;
  width: 7px;
  height: 7px;
  border-radius: 9999px;
  transform: translate(-50%, -50%);
  box-shadow: 0 0 0 2px var(--surface);
  pointer-events: none;
}

.tooltip {
  position: absolute;
  top: 0;
  z-index: 10;
  margin-top: -4px;
  padding: 0.4rem 0.55rem;
  border: 1px solid var(--border);
  border-radius: var(--radius-control);
  background: var(--surface);
  box-shadow: var(--shadow-overlay);
  font-size: 0.7rem;
  line-height: 1.35;
  white-space: nowrap;
  pointer-events: none;
}

.t-time {
  color: var(--text-muted);
  margin-bottom: 0.15rem;
  font-variant-numeric: tabular-nums;
}

.t-row {
  display: flex;
  align-items: center;
  gap: 0.4rem;
  color: var(--text-muted);
}

.t-row strong {
  margin-left: auto;
  padding-left: 0.75rem;
  color: var(--text);
  font-family: var(--font-mono);
  font-weight: 500;
  font-variant-numeric: tabular-nums;
}

.t-swatch {
  width: 0.4rem;
  height: 0.4rem;
  border-radius: 9999px;
}

/* Gridlines are chrome: the border token, the quietest line the system has. */
.grid {
  stroke: var(--border);
  stroke-width: 1;
  vector-effect: non-scaling-stroke;
}

/* The spread between each bucket's average and its peak. Marine — the informational hue. */
.band {
  fill: var(--color-marine-500);
  opacity: 0.16;
}

/* The average. The brand accent, and the thing your eye follows. */
.line {
  fill: none;
  stroke: var(--accent);
  stroke-width: 1.5;
  vector-effect: non-scaling-stroke;
  stroke-linejoin: round;
}

.tick {
  fill: var(--text-muted);
  font-family: var(--font-mono);
  font-size: 9px;
}

.axis {
  font-size: 0.7rem;
  color: var(--text-muted);
  font-variant-numeric: tabular-nums;
}

.empty {
  margin: 0;
  padding: 1.75rem 0;
  text-align: center;
  font-size: 0.85rem;
  color: var(--text-muted);
}
</style>
