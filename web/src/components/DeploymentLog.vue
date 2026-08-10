<script setup lang="ts">
import { computed, nextTick, onUnmounted, ref, watch } from 'vue'
import type { Deployment } from '@/lib/api'
import { streamDeployment, type DeploymentEnd } from '@/lib/stream'
import { AppIcon } from '@mnshahawy/daffa-console-ui'
import { BaseButton } from '@mnshahawy/daffa-console-ui'
import { CopyButton } from '@mnshahawy/daffa-console-ui'

const props = defineProps<{ deployment: Deployment }>()
const emit = defineEmits<{ end: [DeploymentEnd] }>()

const text = ref('')
const loading = ref(true)
const error = ref('')
// The live stream outran what this view keeps, so the head of it is gone for good — distinct
// from `deployment.log_truncated`, which is the SERVER having dropped it from the stored log.
const dropped = ref(false)

// Follow the tail while it is running — but stop the moment the reader scrolls up. Someone who
// has scrolled back to read an error is looking at something; yanking them to the bottom every
// time a line arrives is how a log becomes unreadable exactly when it matters.
const follow = ref(true)
const wrap = ref(true)
const box = ref<HTMLElement>()

let stop: (() => void) | undefined

// Two independent ceilings, because a deploy log has two ways of getting out of hand and
// they need different answers.
//
// KEPT bounds what we hold in memory. A live stream has no server-side size limit the way the
// stored log does (stacks.LogLimit), and EventSource reconnects by itself — so a deploy that
// prints forever, or one whose row is stuck `running`, would otherwise append its output to
// this string over and over until the tab dies. Set above the stored-log limit so a FINISHED
// log is never trimmed here; this only ever bites a runaway live stream.
//
// RENDERED bounds the DOM. Every line is its own element, so line count — not byte count — is
// what actually costs: 25,000 lines is 25,000 elements, and no byte ceiling saves you from
// that on a log of short repeated lines. A scroll-back further than this is not something
// anyone reads in a browser; that is what Download is for.
const maxKeptChars = 2 << 20
const maxRenderedLines = 2000

function subscribe(id: string) {
  stop?.()
  text.value = ''
  error.value = ''
  dropped.value = false
  loading.value = true
  follow.value = true

  stop = streamDeployment(id, {
    onLog: (chunk, replace) => {
      loading.value = false
      const next = replace ? chunk : text.value + chunk
      // Drop from the HEAD: the end of a log is the part that says how it went. Cut on a line
      // boundary so the first surviving line is a whole one rather than a fragment that reads
      // like the deploy started mid-word.
      if (next.length > maxKeptChars) {
        const cut = next.length - maxKeptChars
        const nl = next.indexOf('\n', cut)
        text.value = next.slice(nl === -1 ? cut : nl + 1)
        dropped.value = true
      } else {
        text.value = next
      }
      if (follow.value) nextTick(scrollToBottom)
    },
    onEnd: (end) => {
      loading.value = false
      emit('end', end)
    },
    onError: (message) => {
      loading.value = false
      error.value = message
    },
  })
}

const allLines = computed(() => text.value.split('\n'))

// How many lines exist but are not on screen — the render window's offset, and also the number
// the header owns up to. Kept separate from `dropped` (bytes thrown away for good): a line
// outside the window is still in `text`, so Copy and Download still have it.
const hidden = computed(() => Math.max(0, allLines.value.length - maxRenderedLines))

// One element per line, keyed by ABSOLUTE position, so that a line which has ALREADY been
// rendered is reused and only the lines that just arrived mount — and only they play the
// `appear` fade. A feed that is moving should look like it is moving, rather than silently
// replacing itself. The key has to be absolute rather than the index within the window, or
// every line would take a new key the moment the window slides and the whole log would
// re-mount and re-animate on each arriving chunk.
const lines = computed(() =>
  hidden.value === 0 ? allLines.value : allLines.value.slice(hidden.value),
)

const placeholder = computed(() =>
  loading.value
    ? 'Loading…'
    : props.deployment.status === 'running'
      ? 'Starting…'
      : 'This deploy produced no output.',
)

function scrollToBottom() {
  const el = box.value
  if (el) el.scrollTop = el.scrollHeight
}

function onScroll() {
  const el = box.value
  if (!el) return
  // Within a few pixels of the bottom counts as "at the bottom" — a scroll container is rarely
  // exactly there, and demanding exactness would silently turn following off.
  follow.value = el.scrollHeight - el.scrollTop - el.clientHeight < 32
}

watch(() => props.deployment.id, subscribe, { immediate: true })
onUnmounted(() => stop?.())

// A deploy log is the thing people paste into a ticket or send to a colleague, and a 5000-line
// compose failure does not survive a copy-paste into a chat window.
function download() {
  const blob = new Blob([text.value], { type: 'text/plain' })
  const a = document.createElement('a')
  a.href = URL.createObjectURL(blob)
  a.download = `${props.deployment.stack_name ?? 'deploy'}-${props.deployment.id}.log`
  a.click()
  URL.revokeObjectURL(a.href)
}
</script>

<template>
  <div class="surface overflow-hidden rounded-[var(--radius-card)]">
    <div
      class="flex flex-wrap items-center gap-x-3 gap-y-1 border-b px-4 py-2 text-xs"
      :style="{ borderColor: 'var(--border)' }"
    >
      <span class="text-sm font-medium">Output</span>

      <!--
        A truncated log that does not say so just appears to begin mid-sentence, and the reader
        assumes the deploy did. Only the END is kept, which is the part that says why it failed.
      -->
      <span
        v-if="deployment.log_truncated"
        class="rounded-md px-1.5 py-0.5 font-mono text-[10px]"
        :style="{ background: 'var(--warn-soft)', color: 'var(--warn)' }"
        title="This deploy printed more than Daffa keeps. The end of the log — the part that says how it went — was kept; the beginning was dropped."
      >
        truncated
      </span>

      <!--
        Say so rather than just showing fewer lines. A log that silently begins partway through
        is one an operator reads as the deploy having begun there.
      -->
      <span v-if="hidden || dropped" class="muted">
        showing the last {{ lines.length.toLocaleString() }} lines<template v-if="dropped">
          — earlier output was dropped</template
        >
      </span>

      <span v-if="!follow && deployment.status === 'running'" class="muted">
        paused — scroll to the bottom to follow again
      </span>

      <div class="ml-auto flex items-center gap-2">
        <label for="log-wrap" class="muted flex items-center gap-1.5">
          <input
            id="log-wrap"
            v-model="wrap"
            type="checkbox"
            class="size-3 accent-[var(--accent)]"
          />
          Wrap
        </label>

        <CopyButton v-if="text" intent="ghost" size="xs" :text="text" />

        <BaseButton v-if="text" intent="ghost" size="xs" @click="download">
          <AppIcon name="download" class="size-3" />
          Download
        </BaseButton>
      </div>
    </div>

    <p
      v-if="error"
      class="px-4 py-2 text-sm"
      :style="{ background: 'var(--danger-soft)', color: 'var(--danger)' }"
    >
      {{ error }}
    </p>

    <!-- The terminal keeps its terminal: mono, sunken, and left alone. -->
    <pre
      ref="box"
      class="max-h-[32rem] overflow-auto p-3 font-mono text-xs leading-relaxed"
      :class="wrap ? 'whitespace-pre-wrap break-words' : 'whitespace-pre'"
      :style="{ background: 'var(--surface-sunken)' }"
      @scroll="onScroll"
    ><template v-if="text"><span
        v-for="(line, i) in lines"
        :key="hidden + i"
        class="appear block min-h-[1.2em]"
      >{{ line }}</span></template><span v-else class="subtle">{{ placeholder }}</span></pre>
  </div>
</template>
