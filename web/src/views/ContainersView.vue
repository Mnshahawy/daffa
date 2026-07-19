<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { useQuery, useQueryClient } from '@tanstack/vue-query'
import { bytes, daffa, type Container, type ContainerAction } from '@/lib/api'
import { streamEvents } from '@/lib/stream'
import { toast } from '@/lib/toast'
import { useSession } from '@/stores/session'
import { containerStatus, containerUptime } from '@/lib/status'
import { Cap } from '@/lib/caps'
import ContainerActions from '@/components/ContainerActions.vue'
import EmptyState from '@/components/ui/EmptyState.vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import SearchInput from '@/components/SearchInput.vue'
import StatusPill from '@/components/ui/StatusPill.vue'

const session = useSession()
const qc = useQueryClient()
const filter = ref('')

const { data: containers, isLoading } = useQuery({
  queryKey: ['containers', () => session.envId],
  queryFn: () => daffa.containers(session.envId),
  enabled: computed(() => !!session.envId),
})

// Stats for the RUNNING containers only, as a periodic snapshot rather than N held-open
// streams. This is the difference between a dashboard that idles at nothing and one that pins
// a core just by being left open on a second monitor.
const runningIds = computed(() =>
  (containers.value ?? []).filter((c) => c.state === 'running').map((c) => c.id),
)

const { data: stats } = useQuery({
  queryKey: ['stats', () => session.envId, runningIds],
  queryFn: () => daffa.stats(session.envId, runningIds.value),
  enabled: computed(() => !!session.envId && runningIds.value.length > 0),
  refetchInterval: 3000,
  // A dropped sample should leave the last number on screen, not blank the column.
  placeholderData: (prev) => prev,
})

const statsById = computed(() => new Map((stats.value ?? []).map((s) => [s.id, s])))

// Docker tells us what changed; we invalidate rather than poll. A list that refreshes on a
// timer is either stale or wasteful, and usually both.
let stop: (() => void) | undefined
watch(
  () => session.envId,
  (env) => {
    stop?.()
    if (!env) return
    stop = streamEvents(env, {
      onMessage: () => qc.invalidateQueries({ queryKey: ['containers'] }),
    })
  },
  { immediate: true },
)
onUnmounted(() => stop?.())

// Group by compose project, so a stack reads as a stack rather than as scattered containers.
const groups = computed(() => {
  const q = filter.value.toLowerCase()
  const matched = (containers.value ?? []).filter(
    (c) =>
      !q ||
      c.name.toLowerCase().includes(q) ||
      c.image.toLowerCase().includes(q) ||
      c.project.toLowerCase().includes(q),
  )

  const byProject = new Map<string, Container[]>()
  for (const c of matched) {
    const key = c.project || ''
    if (!byProject.has(key)) byProject.set(key, [])
    byProject.get(key)!.push(c)
  }
  // Ungrouped containers sort last: named stacks are what people look for first.
  return [...byProject.entries()].sort(([a], [b]) =>
    a === '' ? 1 : b === '' ? -1 : a.localeCompare(b),
  )
})

// How many rows are actually on screen. The header used to show the unfiltered total while the
// table showed one row, which is a header describing a page you are not looking at.
const shownCount = computed(() => groups.value.reduce((n, [, items]) => n + items.length, 0))

// Past tense, for the toast. The one the operator just clicked, confirmed.
const acted: Record<ContainerAction, string> = {
  start: 'started',
  stop: 'stopped',
  restart: 'restarted',
  pause: 'paused',
  unpause: 'resumed',
  kill: 'killed',
  remove: 'removed',
}

async function act(c: Container, action: ContainerAction) {
  try {
    await daffa.action(session.envId, c.id, action, action === 'remove')
    toast.ok(`${c.name} ${acted[action]}.`)
  } catch (e) {
    toast.err(e, `Could not ${action} ${c.name}.`)
  }
  await qc.invalidateQueries({ queryKey: ['containers'] })
}

// Docker reports a published port once per address family, so a plainly-bound 8080 arrives
// twice and used to render as "8080→8080 8080→8080". Nobody has two of that port; they have
// one port on two stacks. Collapse them.
function ports(c: Container): string {
  const seen = new Set<string>()
  for (const p of c.ports ?? []) {
    if (p.public) seen.add(`${p.public}→${p.private}`)
  }
  return [...seen].join('  ')
}
</script>

<template>
  <div>
    <PageHeader
      title="Containers"
      :count="
        containers
          ? filter
            ? `${shownCount} of ${containers.length}`
            : containers.length
          : undefined
      "
    >
      <template #actions>
        <SearchInput
          v-model="filter"
          placeholder="Name, image, or project…"
          class="w-72"
        />
      </template>
    </PageHeader>

    <p v-if="isLoading" class="muted text-sm">Loading containers…</p>

    <EmptyState
      v-else-if="!containers?.length"
      icon="box"
      title="Nothing is running on this host"
      body="Containers show up here the moment they exist, whether Daffa deployed them or not. Deploy a stack, or start something on the host directly."
    />

    <!-- A filter that matches nothing rendered a blank page, which reads as a broken list rather
         than as an empty result. -->
    <p v-else-if="!shownCount" class="muted text-sm">No containers match “{{ filter }}”.</p>

    <div v-else class="space-y-6">
      <section v-for="[project, items] in groups" :key="project">
        <h2 v-if="project" class="eyebrow mb-2">{{ project }}</h2>

        <div class="surface overflow-x-auto rounded-[var(--radius-card)]">
          <table class="w-full table-fixed text-sm">
            <!-- Fixed widths, declared once and identical for every project.
                 Each project is its own <table>, and with the browser's automatic layout each
                 one sized its columns to its own contents — so `api`'s CPU column landed
                 in a different place from `web_devcontainer`'s, and the eye could not run
                 down a column that did not stay still. -->
            <colgroup>
              <col class="w-[9.5rem]" />
              <col />
              <col class="w-20" />
              <col class="w-24" />
              <col class="w-52" />
              <col class="w-[13rem]" />
            </colgroup>

            <!-- The columns say what they are. Before, CPU and memory were two unlabelled
                 numeric columns and the only way to tell them apart was to know which was
                 which. -->
            <thead>
              <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
                <th class="eyebrow px-4 py-2 text-left font-medium">Status</th>
                <th class="eyebrow py-2 pr-4 text-left font-medium">Container</th>
                <th class="eyebrow hidden py-2 pr-4 text-right font-medium md:table-cell">CPU</th>
                <th class="eyebrow hidden py-2 pr-4 text-right font-medium md:table-cell">Memory</th>
                <th class="eyebrow hidden py-2 pr-4 text-left font-medium lg:table-cell">Ports</th>
                <th class="eyebrow py-2 pr-4 text-right font-medium">
                  <span class="sr-only">Actions</span>
                </th>
              </tr>
            </thead>

            <tbody>
              <tr
                v-for="c in items"
                :key="c.id"
                class="border-b transition last:border-0 hover:bg-[var(--surface-sunken)]"
                :style="{ borderColor: 'var(--border)' }"
              >
                <!-- One status component, everywhere. Running is green — which sounds obvious
                     until you notice Dokploy renders a running container as a solid BLACK
                     badge, and colours "running" yellow because in their schema it means a
                     build is in progress. -->
                <td class="px-4 py-3">
                  <StatusPill :status="containerStatus(c.state, c.status)" />
                  <div v-if="containerUptime(c.status)" class="subtle mt-1 text-xs">
                    up {{ containerUptime(c.status) }}
                  </div>
                </td>

                <td class="py-3 pr-4">
                  <RouterLink
                    :to="{ name: 'container', params: { id: c.id }, query: c.node_id ? { node: c.node_id } : {} }"
                    class="font-medium transition hover:text-[var(--accent-text)]"
                  >
                    {{ c.service || c.name }}
                  </RouterLink>
                  <div class="subtle mt-0.5 truncate font-mono text-xs">{{ c.image }}</div>
                </td>

                <td class="hidden py-3 pr-4 text-right font-mono text-xs md:table-cell">
                  <template v-if="statsById.get(c.id)">
                    {{ statsById.get(c.id)!.cpu.toFixed(1) }}%
                  </template>
                  <span v-else class="subtle">—</span>
                </td>

                <td class="muted hidden py-3 pr-4 text-right font-mono text-xs md:table-cell">
                  <template v-if="statsById.get(c.id)">
                    {{ bytes(statsById.get(c.id)!.memory) }}
                  </template>
                  <span v-else class="subtle">—</span>
                </td>

                <td class="subtle hidden py-3 pr-4 font-mono text-xs lg:table-cell">
                  {{ ports(c) || '—' }}
                </td>

                <td class="py-3 pr-4">
                  <ContainerActions
                    :state="c.state"
                    :name="c.service || c.name"
                    :disabled="!session.can(Cap.ContainersEdit)"
                    @action="(a) => act(c, a)"
                  />
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>
    </div>
  </div>
</template>
