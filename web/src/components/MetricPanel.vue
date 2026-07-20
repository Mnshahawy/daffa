<script setup lang="ts">
/**
 * CPU and memory over time, with a range picker.
 *
 * One component for the container, stack and host pages: they ask the same question of the same
 * endpoint and differ only in the filter, so three copies of this would be three places for the
 * range picker to drift.
 */
import { computed, ref } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import { daffa, type MetricRange } from '@/lib/api'
import { useSession } from '@/stores/session'
import BaseButton from './ui/BaseButton.vue'
import MetricChart from './MetricChart.vue'

const props = defineProps<{
  /** Filter to one container. Omit for a whole stack or host. */
  container?: string
  /** Filter to one stack. Its containers are SUMMED — see the store's Series. */
  stack?: string
  /** The MACHINE's own CPU/memory (the whole box), not the container aggregate. For the cluster page. */
  host?: boolean
  /** With `host`, narrow to one node of a Swarm. Omit to sum every node's machine metrics. */
  node?: string
  /** Defaults to the host currently selected in the switcher. */
  env?: string
}>()

const session = useSession()
const range = ref<MetricRange>('1h')
const ranges: MetricRange[] = ['1h', '6h', '24h', '7d']

const env = computed(() => props.env ?? session.envId)

const { data, isFetching } = useQuery({
  // The range and the filter are part of the key, so switching range refetches rather than
  // redrawing the previous window's data under a new label.
  queryKey: computed(() => [
    'metrics',
    env.value,
    props.container,
    props.stack,
    props.host,
    props.node,
    range.value,
  ]),
  queryFn: () =>
    daffa.metrics(env.value, {
      range: range.value,
      container: props.container,
      stack: props.stack,
      host: props.host,
      node: props.node,
    }),
  enabled: computed(() => !!env.value),
  // The sampler writes every 30s by default. Refetching faster than that only redraws the same
  // points; a minute is a chart, not a live feed — the live feed is StatsPanel.
  refetchInterval: 60_000,
})

const points = computed(() => data.value ?? [])
</script>

<template>
  <section class="flex flex-col gap-4">
    <header class="flex items-center justify-between">
      <h3 class="text-sm font-semibold">History</h3>

      <!-- A range picker is navigation, not an action: the selected one is the accent, the
           rest are bare. Nothing here costs anything, so nothing here is loud. -->
      <div class="flex gap-1" role="group" aria-label="Time range">
        <BaseButton
          v-for="r in ranges"
          :key="r"
          :intent="range === r ? 'primary' : 'ghost'"
          size="xs"
          :aria-pressed="range === r"
          @click="range = r"
        >
          {{ r }}
        </BaseButton>
      </div>
    </header>

    <div class="grid gap-6 md:grid-cols-2">
      <MetricChart :points="points" kind="cpu" :range="range" :loading="isFetching && !points.length" />
      <MetricChart :points="points" kind="memory" :range="range" :loading="isFetching && !points.length" />
    </div>
  </section>
</template>
