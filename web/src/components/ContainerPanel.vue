<script setup lang="ts">
import { computed, defineAsyncComponent, nextTick, onUnmounted, ref, shallowRef, watch } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import { daffa } from '@/lib/api'
import { streamLogs, type LogLine } from '@/lib/stream'
import { useSession } from '@/stores/session'
import { Cap } from '@/lib/caps'
import StatsPanel from '@/components/StatsPanel.vue'
import MetricPanel from '@/components/MetricPanel.vue'

// xterm is ~290 KB — most of this panel's weight, for a feature most visits never use.
// Loading it only when someone actually opens the Shell tab keeps the rest cheap.
const ExecTerminal = defineAsyncComponent(() => import('@/components/ExecTerminal.vue'))

/**
 * One container's working surface — stats, logs, shell — reusable anywhere a container
 * appears: the container page owns one, and a stack's Containers tab embeds one per
 * selected container.
 *
 * Stats first, deliberately. The question that opens a container is usually "what is it
 * DOING", and the answer to that is a graph; the log is where you go when the graph made
 * you suspicious.
 */
const props = defineProps<{
  env: string
  id: string
  /** Which machine the container is on — required on a multi-node swarm, absent otherwise. */
  node?: string
}>()

const session = useSession()

type Tab = 'stats' | 'logs' | 'shell'
const tab = ref<Tab>('stats')

// Deduplicated with any other query for the same container by key, so embedding this
// panel next to a header that also inspects costs one request, not two.
const { data: inspect } = useQuery({
  queryKey: ['container', () => props.env, () => props.id, () => props.node],
  queryFn: () => daffa.container(props.env, props.id, props.node),
  enabled: computed(() => !!props.env && !!props.id),
})

const name = computed(() => {
  const n = (inspect.value?.Name as string) ?? ''
  return n.replace(/^\//, '') || props.id.slice(0, 12)
})
const state = computed(
  () => ((inspect.value?.State as Record<string, unknown>)?.Status as string) ?? '',
)
const running = computed(() => state.value === 'running')
const image = computed(() => (inspect.value?.Config as Record<string, unknown>)?.Image as string)

// A shell is only meaningful in a running container, and only for someone allowed to open
// one. Rather than show a tab that errors when clicked, we don't show it.
//
// containers.exec, NOT containers.edit: the Docker socket runs as root, so a shell in a
// container is effectively root on the host. Someone trusted to restart a service is not
// thereby trusted with that.
const canExec = computed(() => running.value && session.can(Cap.ContainersExec))

const tabs = computed(() => {
  const t: { id: Tab; label: string }[] = [
    { id: 'stats', label: 'Stats' },
    { id: 'logs', label: 'Logs' },
  ]
  if (canExec.value) t.push({ id: 'shell', label: 'Shell' })
  return t
})

// The Shell tab disappears the moment the container stops — and if you were standing on it
// when that happened, the panel was left with a selected tab that no longer exists and
// nothing at all below the tab strip. Fall back to the stats, which are the front door.
watch(canExec, (ok) => {
  if (!ok && tab.value === 'shell') tab.value = 'stats'
})

// ── logs ──────────────────────────────────────────────────────────────────────
// A ring buffer: a chatty container would otherwise grow the DOM without bound until
// the tab dies. shallowRef because the lines are immutable — deep reactivity on 2000
// objects buys nothing and costs on every push.
const MAX_LINES = 2000
const lines = shallowRef<LogLine[]>([])
const follow = ref(true)
const logEl = ref<HTMLElement>()

let stop: (() => void) | undefined

watch(
  [() => props.env, () => props.id, () => props.node],
  ([env, cid, nodeID]) => {
    stop?.()
    lines.value = []
    if (!env || !cid) return

    stop = streamLogs(
      env,
      cid,
      {
        onMessage: (line) => {
          const next =
            lines.value.length >= MAX_LINES
              ? [...lines.value.slice(lines.value.length - MAX_LINES + 1), line]
              : [...lines.value, line]
          lines.value = next

          if (follow.value) {
            nextTick(() => {
              if (logEl.value) logEl.value.scrollTop = logEl.value.scrollHeight
            })
          }
        },
      },
      200,
      nodeID,
    )
  },
  { immediate: true },
)
onUnmounted(() => stop?.())

// If the person scrolls up, stop yanking them back to the bottom — that fight is the
// most irritating thing a log viewer can do.
function onScroll() {
  const el = logEl.value
  if (!el) return
  follow.value = el.scrollHeight - el.scrollTop - el.clientHeight < 40
}
</script>

<template>
  <div>
    <p v-if="image" class="subtle mb-4 truncate font-mono text-xs">{{ image }}</p>

    <div class="mb-4 flex gap-1" role="tablist">
      <button
        v-for="t in tabs"
        :key="t.id"
        role="tab"
        :aria-selected="tab === t.id"
        class="rounded-[var(--radius-control)] px-3 py-1.5 text-sm transition"
        :class="tab === t.id ? 'font-medium' : 'muted hover:text-[var(--text)]'"
        :style="
          tab === t.id ? { background: 'var(--accent-soft)', color: 'var(--accent-text)' } : undefined
        "
        @click="tab = t.id"
      >
        {{ t.label }}
      </button>
    </div>

    <!-- Stats: what it is doing now, and what it has been doing.
         The live panel streams from the daemon; the history comes from the sampler, and is
         keyed on the container's NAME rather than its id — a redeploy mints a new id, and a
         chart that resets every deploy is a chart that can never show you a slow leak. -->
    <template v-if="tab === 'stats'">
      <StatsPanel :env="env" :container="id" :running="running" :node="node" />
      <div class="mt-8 border-t pt-6" :style="{ borderColor: 'var(--border)' }">
        <MetricPanel :container="name" />
      </div>
    </template>

    <!-- Logs -->
    <div v-show="tab === 'logs'" class="surface overflow-hidden rounded-[var(--radius-card)]">
      <div
        class="flex items-center justify-between border-b px-4 py-2"
        :style="{ borderColor: 'var(--border)' }"
      >
        <span class="text-sm font-medium">Logs</span>
        <label :for="`log-follow-${id}`" class="muted flex items-center gap-2 text-xs">
          <input
            :id="`log-follow-${id}`"
            v-model="follow"
            type="checkbox"
            class="accent-[var(--accent)]"
          />
          Follow
        </label>
      </div>

      <div
        ref="logEl"
        class="h-[60vh] overflow-y-auto p-3 font-mono text-xs leading-relaxed"
        :style="{ background: 'var(--surface-sunken)' }"
        @scroll="onScroll"
      >
        <p v-if="!lines.length" class="muted">Waiting for output…</p>
        <!-- stderr is the half of the stream you came for. It is red, and it is the only red
             on this panel — `appear` so a feed that is moving looks like it is moving. -->
        <div
          v-for="(line, i) in lines"
          :key="i"
          class="appear whitespace-pre-wrap break-all"
          :style="line.stream === 'stderr' ? { color: 'var(--danger)' } : undefined"
        >{{ line.text }}</div>
      </div>
    </div>

    <!-- Shell. Keyed on the container so switching containers gets a fresh session
         rather than a terminal still attached to the last one. -->
    <ExecTerminal v-if="tab === 'shell' && canExec" :key="id" :env="env" :container="id" :node="node" />
  </div>
</template>
