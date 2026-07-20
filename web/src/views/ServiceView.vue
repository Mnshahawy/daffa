<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { daffa } from '@/lib/api'
import { toast } from '@/lib/toast'
import { useSession } from '@/stores/session'
import { Cap } from '@/lib/caps'
import { confirm } from '@/lib/confirm'
import { serviceStatus, taskStatus } from '@/lib/status'
import PageHeader from '@/components/ui/PageHeader.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import StatusPill from '@/components/ui/StatusPill.vue'
import ServiceLogs from '@/components/ServiceLogs.vue'

const route = useRoute()
const router = useRouter()
const qc = useQueryClient()
const session = useSession()
const id = computed(() => String(route.params.id))

const canEdit = computed(() => session.can(Cap.ServicesEdit))

function refresh() {
  qc.invalidateQueries({ queryKey: ['service'] })
  qc.invalidateQueries({ queryKey: ['tasks'] })
  qc.invalidateQueries({ queryKey: ['services'] })
}

async function run(fn: () => Promise<unknown>, ok: string) {
  try {
    await fn()
    toast.ok(ok)
    refresh()
  } catch (e) {
    toast.err(e, String(e))
  }
}

// SCALE. Zero is a real answer somebody means — it is Swarm's version of "stop" — so it is offered
// plainly rather than hidden behind a word that means something else.
const scaleTo = ref<number | null>(null)
const scaling = ref(false)

async function applyScale() {
  const n = scaleTo.value
  if (n === null || n < 0) return

  if (n === 0) {
    const ok = await confirm({
      title: `Scale ${service.value?.name} to zero?`,
      body:
        'Every task stops. The service still exists and keeps its configuration, so scaling it ' +
        'back up restores it — but nothing it serves will be served until you do.',
      confirmLabel: 'Scale to zero',
      intent: 'caution',
    })
    if (!ok) return
  }

  scaling.value = true
  await run(() => daffa.scaleService(session.envId, id.value, n), 'Service scaled.')
  scaling.value = false
}

// REDEPLOY is `docker service update --force`: recreate every task, re-resolving the image against
// the registry. On a floating tag it is the only way to get the new bytes without editing anything.
const redeploy = useMutation({
  mutationFn: () => daffa.redeployService(session.envId, id.value),
  onSuccess: () => {
    toast.ok('Service redeployed.')
    refresh()
  },
  onError: (e) => toast.err(e, String(e)),
})

// ROLLBACK puts back the PREVIOUS spec, which swarm keeps for exactly this. It is the fastest way
// out of a bad update, which is why it sits next to the thing that reports one.
const rollback = useMutation({
  mutationFn: () => daffa.rollbackService(session.envId, id.value),
  onSuccess: () => {
    toast.ok('Service rolled back.')
    refresh()
  },
  onError: (e) => toast.err(e, String(e)),
})

async function removeService() {
  const name = service.value?.name
  if (!name) return

  const ok = await confirm({
    title: `Remove ${name}?`,
    body:
      'The service and its tasks are removed. Its volumes are NOT: they are node-local, they are ' +
      'on whichever machines its tasks ran on, and they are somebody\'s data.\n\n' +
      'If this service belongs to a stack, redeploying the stack will simply put it back — remove ' +
      'the stack instead if that is what you meant.',
    confirmLabel: 'Remove',
    intent: 'danger',
    typeToConfirm: name,
  })
  if (!ok) return

  await run(async () => {
    await daffa.removeService(session.envId, id.value)
    router.push({ name: 'services' })
  }, 'Service removed.')
}

const { data: service } = useQuery({
  queryKey: ['service', () => session.envId, () => id.value],
  queryFn: () => daffa.service(session.envId, id.value),
  enabled: computed(() => !!session.envId),
})

// Tasks move on their own — a rescheduling service changes underneath you — so this is the one
// list worth a timer. It is cheap, and a stale task table is the thing you are staring at when you
// most need it to be true.
const { data: tasks, isLoading } = useQuery({
  queryKey: ['tasks', () => session.envId, () => id.value],
  queryFn: () => daffa.tasks(session.envId, id.value),
  enabled: computed(() => !!session.envId),
  refetchInterval: 4000,
  placeholderData: (prev) => prev,
})

const tab = ref<'tasks' | 'logs'>('tasks')

// FILTER BY STATE. A service redeployed a few times buries its running tasks under a drift of
// shut-down ones Swarm replaced — so the table defaults to "Active" (what Swarm currently WANTS,
// desired running), which keeps every live and failing task while hiding the historical noise.
//
// The rest of the menu is built from the states this service ACTUALLY has: no point offering
// "Failed" when nothing failed. Two cross-cutting views bookend the real states — Active (the
// default) and All. 'active'/'all' are sentinels; any other value is a literal task state.
const taskFilter = ref<string>('active')

// The order the daemon's own lifecycle runs in, so the menu reads running → … → failed rather than
// alphabetically. Anything unforeseen sorts to the end rather than vanishing.
const STATE_ORDER = [
  'running', 'starting', 'preparing', 'assigned', 'accepted', 'ready', 'pending',
  'new', 'allocated', 'complete', 'shutdown', 'failed', 'rejected', 'orphaned', 'remove',
]
const cap = (s: string) => (s ? s[0].toUpperCase() + s.slice(1) : s)

const presentStates = computed(() => {
  const counts = new Map<string, number>()
  for (const t of tasks.value ?? []) counts.set(t.state, (counts.get(t.state) ?? 0) + 1)
  return [...counts.entries()]
    .sort((a, b) => (STATE_ORDER.indexOf(a[0]) + 1 || 99) - (STATE_ORDER.indexOf(b[0]) + 1 || 99))
    .map(([state, count]) => ({ state, count }))
})

const activeCount = computed(() => (tasks.value ?? []).filter((t) => t.desired === 'running').length)

const filteredTasks = computed(() => {
  const all = tasks.value ?? []
  if (taskFilter.value === 'all') return all
  if (taskFilter.value === 'active') return all.filter((t) => t.desired === 'running')
  return all.filter((t) => t.state === taskFilter.value)
})

// A state can vanish while it is the selected filter — the last failed task gets reaped, say. Fall
// back to Active rather than leave the dropdown pointing at an option that no longer exists.
watch(presentStates, (states) => {
  const f = taskFilter.value
  if (f !== 'active' && f !== 'all' && !states.some((s) => s.state === f)) taskFilter.value = 'active'
})

// A task names its machine with the SWARM's node id. A container link needs DAFFA's — they are
// different identifiers for the same machine, and the node table is precisely the join between
// them. nodeOf is that translation, and it returns nothing for a machine Daffa has no agent on,
// which is exactly when there is no shell to offer.
const { data: clusterNodes } = useQuery({
  queryKey: ['cluster-nodes', () => session.envId],
  queryFn: () => daffa.clusterNodes(session.envId),
  enabled: computed(() => !!session.envId),
})

function nodeOf(swarmNodeID: string): string | undefined {
  return clusterNodes.value?.find((n) => n.swarm_node_id === swarmNodeID)?.node_id
}

/** Nodes the Swarm placed tasks on that Daffa cannot reach. No shell, no stats — say so once. */
const unreachable = computed(() => {
  const names = new Set(
    (tasks.value ?? []).filter((t) => t.node && !t.reachable).map((t) => t.node as string),
  )
  return [...names]
})

function ago(ts: string): string {
  const s = Math.max(0, (Date.now() - new Date(ts).getTime()) / 1000)
  if (s < 60) return `${Math.round(s)}s`
  if (s < 3600) return `${Math.round(s / 60)}m`
  if (s < 86400) return `${Math.round(s / 3600)}h`
  return `${Math.round(s / 86400)}d`
}
</script>

<template>
  <div>
    <PageHeader
      :title="service?.name ?? 'Service'"
      :crumbs="[{ label: 'Services', to: { name: 'services' } }]"
      :description="service?.tag"
    >
      <template #actions>
        <StatusPill v-if="service" :status="serviceStatus(service)" />

        <template v-if="canEdit && service">
          <!-- Scale to zero is Swarm's version of "stop". It is offered under its own name,
               because "Stop" would mean something Swarm does not do. -->
          <div class="flex items-center gap-1">
            <input
              :value="scaleTo ?? service.desired"
              type="number"
              min="0"
              :disabled="service.mode === 'global'"
              :title="
                service.mode === 'global'
                  ? 'A global service runs one task per node. It has no replica count.'
                  : 'Replicas'
              "
              class="field w-16 px-2 py-1 text-sm"
              @input="scaleTo = Number(($event.target as HTMLInputElement).value)"
            />
            <BaseButton
              intent="secondary"
              size="sm"
              :loading="scaling"
              :disabled="service.mode === 'global' || scaleTo === null || scaleTo === service.desired"
              @click="applyScale"
            >
              Scale
            </BaseButton>
          </div>

          <BaseButton
            intent="primary"
            size="sm"
            :loading="redeploy.isPending.value"
            title="Recreate every task, re-resolving the image against the registry"
            @click="redeploy.mutate()"
          >
            Redeploy
          </BaseButton>

          <BaseButton
            intent="caution"
            size="sm"
            :loading="rollback.isPending.value"
            title="Put back the previous spec, which Swarm keeps for exactly this"
            @click="rollback.mutate()"
          >
            Roll back
          </BaseButton>

          <BaseButton intent="danger" size="sm" @click="removeService">Remove</BaseButton>
        </template>
      </template>
    </PageHeader>

    <!--
      The rollback nobody else shows. Swarm tried the new spec, could not make it stick, and put
      the old one back — so the deploy reported success and then quietly undid itself.
    -->
    <div
      v-if="service?.update_state?.startsWith('rollback')"
      class="mb-4 rounded-[var(--radius-control)] px-3 py-2 text-sm"
      :style="{ background: 'var(--warn-soft)', color: 'var(--warn)' }"
    >
      <strong>Swarm rolled this service back.</strong>
      The last update did not hold, so the previous spec was restored.
      <span v-if="service.update_message" class="font-mono text-xs">
        — {{ service.update_message }}
      </span>
    </div>

    <div class="mb-4 flex gap-1">
      <button
        v-for="t in (['tasks', 'logs'] as const)"
        :key="t"
        class="rounded-[var(--radius-control)] px-3 py-1.5 text-sm capitalize transition"
        :style="
          tab === t
            ? { background: 'var(--accent-soft)', color: 'var(--accent-text)' }
            : { color: 'var(--text-muted)' }
        "
        @click="tab = t"
      >
        {{ t }}
      </button>
    </div>

    <template v-if="tab === 'tasks'">
      <!--
        THE TASK TABLE. A service that says 0/3 tells you nothing; the task underneath says
        "no suitable node (insufficient memory on 2 nodes)", and that is the entire answer.
      -->
      <p v-if="isLoading" class="muted text-sm">Loading…</p>

      <p v-else-if="!tasks?.length" class="muted text-sm">
        This service has no tasks. Swarm has not tried to place it yet.
      </p>

      <template v-else>
        <p v-if="unreachable.length" class="muted mb-3 text-xs">
          Daffa has no agent on {{ unreachable.join(', ') }}, so tasks there have no shell and no
          stats. Their logs still work — the manager collects those.
        </p>

        <!-- Filter by state — options built from the states this service actually has. Defaults to
             Active so a redeployed service's live tasks are not buried under the ones Swarm replaced. -->
        <div class="mb-3 flex items-center justify-end gap-2">
          <label class="eyebrow" for="task-state-filter">State</label>
          <select id="task-state-filter" v-model="taskFilter" class="field w-auto py-1 text-xs">
            <option value="active">Active ({{ activeCount }})</option>
            <option v-for="s in presentStates" :key="s.state" :value="s.state">
              {{ cap(s.state) }} ({{ s.count }})
            </option>
            <option value="all">All ({{ tasks?.length ?? 0 }})</option>
          </select>
        </div>

        <div class="surface overflow-hidden rounded-[var(--radius-card)]">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
                <th class="eyebrow px-4 py-2 text-left font-medium">Task</th>
                <th class="eyebrow py-2 pr-4 text-left font-medium">Node</th>
                <th class="eyebrow py-2 pr-4 text-left font-medium">Desired</th>
                <th class="eyebrow py-2 pr-4 text-left font-medium">State</th>
                <th class="eyebrow py-2 pr-4 text-right font-medium">Since</th>
              </tr>
            </thead>

            <tbody>
              <tr v-if="!filteredTasks.length" class="border-b last:border-0" :style="{ borderColor: 'var(--border)' }">
                <td colspan="5" class="muted py-4 pl-4 text-sm">
                  No {{ taskFilter === 'all' ? '' : taskFilter }} tasks right now.
                </td>
              </tr>
              <tr
                v-for="t in filteredTasks"
                :key="t.id"
                class="border-b transition last:border-0 hover:bg-[var(--surface-sunken)]"
                :style="{ borderColor: 'var(--border)' }"
              >
                <td class="py-3 pl-4 pr-4">
                  <div class="font-medium">{{ service?.name }}.{{ t.slot || 1 }}</div>
                  <div class="subtle mt-0.5 font-mono text-xs">{{ t.id.slice(0, 12) }}</div>
                </td>

                <td class="py-3 pr-4 text-xs">
                  <span v-if="t.node" class="font-mono">{{ t.node }}</span>
                  <!-- Never placed. That IS the answer, and it is not a blank cell. -->
                  <span v-else class="subtle italic">not placed</span>
                  <div v-if="t.node && !t.reachable" class="subtle mt-0.5">no agent</div>
                </td>

                <td class="muted py-3 pr-4 font-mono text-xs">{{ t.desired }}</td>

                <td class="py-3 pr-4">
                  <StatusPill :status="taskStatus(t)" />
                  <!--
                    The string this whole page exists for. Not truncated, not in a tooltip, not
                    one click away: it is the answer, so it is on the row.
                  -->
                  <div
                    v-if="t.error"
                    class="mt-1 font-mono text-xs"
                    :style="{ color: 'var(--danger)' }"
                  >
                    {{ t.error }}
                  </div>
                </td>

                <td class="py-3 pr-4 text-right">
                  <!--
                    THE SHELL, on the machine the task is actually on.

                    A manager cannot exec into a container on another node — that is Docker, and no
                    tool escapes it. But the server holds a tunnel per node, so the container link
                    simply names the node and the shell routes itself. Portainer needed a gossiping
                    agent mesh to do this.

                    Offered only when the task HAS a container and Daffa can reach its machine —
                    which the row already says, so nobody clicks a button that cannot work.
                  -->
                  <RouterLink
                    v-if="t.container_id && t.reachable && t.state === 'running'"
                    :to="{
                      name: 'container',
                      params: { id: t.container_id },
                      query: nodeOf(t.node_id) ? { node: nodeOf(t.node_id) } : {},
                    }"
                    class="text-xs underline"
                    :style="{ color: 'var(--accent-text)' }"
                  >
                    Shell
                  </RouterLink>
                  <span class="muted ml-3 font-mono text-xs">{{ ago(t.since) }}</span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
    </template>

    <template v-else>
      <!--
        Service logs are the ONE cluster-wide stream Docker proxies for us: the manager collects
        from every node running a task, so this works with no agent on the workers at all.
      -->
      <ServiceLogs :key="id" :env="session.envId" :service="id" />
    </template>
  </div>
</template>
