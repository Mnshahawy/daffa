<script setup lang="ts">
import { computed, ref } from 'vue'
import { useQuery, useQueryClient } from '@tanstack/vue-query'
import { bytes, daffa } from '@/lib/api'
import { useSession } from '@/stores/session'
import { hostStatus, nodeStatus } from '@/lib/status'
import { Cap } from '@/lib/caps'
import { confirm } from '@/lib/confirm'
import { type ClusterNode, type JoinTokens, type LogConfigRequest } from '@/lib/api'
import { toast } from '@/lib/toast'
import BaseButton from '@/components/ui/BaseButton.vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import StatusPill from '@/components/ui/StatusPill.vue'
import PruneButton from '@/components/PruneButton.vue'
import MetricPanel from '@/components/MetricPanel.vue'
import LogConfigForm from '@/components/LogConfigForm.vue'

const session = useSession()
const enabled = computed(() => !!session.envId)

// Shares the switcher's cache — this is the same list it already polls, not a second poll.
const { data: environments } = useQuery({
  queryKey: ['environments'],
  queryFn: daffa.environments,
})

const host = computed(() => environments.value?.find((e) => e.id === session.envId))

// Disk usage is genuinely expensive on the daemon (it walks every layer), so it is not
// on the 15s cadence the rest of the app uses. It changes slowly; asking constantly
// would cost more than it tells you.
// What this environment is MADE OF.
//
// The join of two lists: what the Swarm says its machines are, and what Daffa can actually reach.
// Portainer has both lists and never reconciles them for the user, so "why can't I get a shell on
// this task?" stays a question. Here it is a sentence on the row, before you click.
//
// Nodes get no nav entry of their own. This page already answered "what am I pointed at?", and for
// a Swarm the answer IS the node table.
const { data: nodes } = useQuery({
  queryKey: ['cluster-nodes', () => session.envId],
  queryFn: () => daffa.clusterNodes(session.envId),
  enabled: computed(() => !!session.envId),
})

// ── which node the page is about ──────────────────────────────────────────────────
//
// The node table is the selector: click a machine and the whole page — daemon summary, disk, and
// the CPU/memory history — reads THAT node instead of the manager. Before this, only disk followed
// a separate dropdown and every other number silently meant the manager; the table already lists
// the nodes, so it is the natural place to choose between them.
//
// A node is addressable only when Daffa has an agent on it (node_id set); an unreachable Swarm
// member has no daemon to ask, so its row is not selectable. The default is the node this cluster
// IS — its leader/manager (what the manager-answering endpoints already returned), falling back to
// whatever single reachable machine there is on a standalone host.
const selectedNode = ref('')
const reachableNodes = computed(() => (nodes.value ?? []).filter((n) => n.node_id))
const multiNode = computed(() => reachableNodes.value.length > 1)
const defaultNode = computed(
  () =>
    reachableNodes.value.find((n) => n.leader)?.node_id || reachableNodes.value[0]?.node_id || '',
)
// A stale selection (the chosen node drained away, or the switcher moved to another cluster) falls
// back to the default rather than pinning the page to a node that is no longer there.
const activeNode = computed(() =>
  reachableNodes.value.some((n) => n.node_id === selectedNode.value)
    ? selectedNode.value
    : defaultNode.value,
)
const activeNodeName = computed(
  () => (nodes.value ?? []).find((n) => n.node_id === activeNode.value)?.name ?? '',
)

function selectNode(nodeId?: string) {
  if (nodeId) selectedNode.value = nodeId
}

// The daemon summary (running, images, cpus, memory — the instrument strip and the identity line)
// for the selected node. Keyed on the node so switching refetches; an empty node lets the server
// answer for the manager, which is the default anyway.
const { data: info } = useQuery({
  queryKey: ['info', () => session.envId, () => activeNode.value],
  queryFn: () => daffa.info(session.envId, activeNode.value || undefined),
  enabled,
})

const qc = useQueryClient()
const canEditNodes = computed(() => session.can(Cap.NodesEdit))
const busyNode = ref('')

async function nodeOp(n: ClusterNode, body: { availability?: string; role?: string }) {
  if (!n.swarm_node_id) return
  busyNode.value = n.swarm_node_id
  try {
    await daffa.updateNode(session.envId, n.swarm_node_id, body)
    toast.ok('Node updated.')
    await qc.invalidateQueries({ queryKey: ['cluster-nodes'] })
  } catch (e) {
    toast.err(e, 'Could not update the node.')
  }
  busyNode.value = ''
}

// DRAIN. The one that needs a sentence BEFORE it, not after.
//
// Draining a node evicts SWARM TASKS ONLY. A plain container on that machine — including a Compose
// stack pinned to it — keeps running, because Swarm does not know it exists. An operator who drains
// a node in order to reboot the machine, believing everything has moved off it, will be wrong. That
// is worth saying at the moment they do it, not in a runbook they read afterwards.
async function drain(n: ClusterNode) {
  const ok = await confirm({
    title: `Drain ${n.name}?`,
    body:
      'Every Swarm task on this machine is stopped and rescheduled elsewhere — if there is room ' +
      'elsewhere. A service that cannot be placed will simply stop.\n\n' +
      'It evicts SWARM TASKS ONLY. Plain containers on this machine keep running, including any ' +
      'Compose stack pinned to it, because Swarm does not know they exist. Draining is not the ' +
      'same as emptying.',
    confirmLabel: 'Drain',
    intent: 'caution',
  })
  if (!ok) return
  await nodeOp(n, { availability: 'drain' })
}

async function demote(n: ClusterNode) {
  const ok = await confirm({
    title: `Demote ${n.name} to a worker?`,
    body:
      "It stops holding the Swarm's consensus. Managers vote, so an EVEN number of them cannot " +
      'break a tie — demoting the wrong one can leave a cluster that runs but can no longer be ' +
      'changed. Swarm refuses to leave you with none.',
    confirmLabel: 'Demote',
    intent: 'caution',
  })
  if (!ok) return
  await nodeOp(n, { role: 'worker' })
}

// ── the cluster's own existence ─────────────────────────────────────────────────

const canEditSwarm = computed(() => session.can(Cap.SwarmEdit))
const swarmBusy = ref(false)
const tokens = ref<JoinTokens | null>(null)

// Creating a Swarm out of a standalone host. It is a real change to what this environment IS, and
// reconciliation notices immediately — the operator does not sit looking at a page that still says
// "standalone" while their Swarm exists.
async function initSwarm() {
  const ok = await confirm({
    title: `Make ${host.value?.name} a Swarm?`,
    body:
      'This host becomes a single-node Swarm, and its own manager. Nothing that is already running ' +
      'is touched — plain containers keep running exactly as they are — but the environment gains ' +
      'services, tasks, secrets and configs, and can be deployed to with Swarm stacks.',
    confirmLabel: 'Create the Swarm',
    intent: 'primary',
  })
  if (!ok) return

  swarmBusy.value = true
  try {
    await daffa.swarmInit(session.envId)
    toast.ok('Swarm initialized.')
    await qc.invalidateQueries({ queryKey: ['environments'] })
    await qc.invalidateQueries({ queryKey: ['cluster-nodes'] })
  } catch (e) {
    toast.err(e, 'Could not initialize the swarm.')
  }
  swarmBusy.value = false
}

// The join tokens are CREDENTIALS: anybody holding one can add a machine to the cluster, and a
// machine in the cluster runs whatever the cluster schedules onto it. So they are fetched on
// demand, by an explicit act, rather than sitting on a page somebody left open — and reading one is
// audited as an event in its own right.
async function showTokens() {
  try {
    tokens.value = await daffa.joinTokens(session.envId)
  } catch (e) {
    toast.err(e, 'Could not load the join tokens.')
  }
}

// Leaving. For the LAST MANAGER this dissolves the cluster: the raft store goes, and with it every
// service, secret and config DEFINITION. The containers keep running until something stops them,
// which makes the damage quiet as well as total — so the dialog says what it destroys rather than
// asking whether you are sure.
async function leaveSwarm() {
  const name = host.value?.name ?? 'this cluster'
  const ok = await confirm({
    title: `Dissolve the Swarm on ${name}?`,
    body:
      'This is the last manager, so leaving DESTROYS THE CLUSTER: every service, secret and config ' +
      'definition goes with it, and they cannot be recovered.\n\n' +
      'The containers those services created keep running, orphaned, until something stops them — ' +
      'so it will look as though nothing happened. Nothing will be serving from a definition any ' +
      'more.',
    confirmLabel: 'Dissolve the Swarm',
    intent: 'danger',
    typeToConfirm: name,
  })
  if (!ok) return

  swarmBusy.value = true
  try {
    await daffa.swarmLeave(session.envId, true)
    toast.ok('Left the swarm.')
    tokens.value = null
    await qc.invalidateQueries({ queryKey: ['environments'] })
    await qc.invalidateQueries({ queryKey: ['cluster-nodes'] })
  } catch (e) {
    toast.err(e, 'Could not leave the swarm.')
  }
  swarmBusy.value = false
}

// ── container log defaults ───────────────────────────────────────────────────────
//
// THIS host's default logging for deployed services: its override if one is set, else
// the fleet default from Settings. Applied at the next deploy — log options are fixed at
// container creation, so nothing restarts because this changed.
const canViewLogging = computed(() => session.can(Cap.LoggingView))
const canEditLogging = computed(() => session.can(Cap.LoggingEdit))

const { data: logConfig } = useQuery({
  queryKey: ['host-log-config', () => session.envId],
  queryFn: () => daffa.hostLogConfig(session.envId),
  enabled: computed(() => !!session.envId && canViewLogging.value),
})

const logBusy = ref(false)

const logSource = computed(() => {
  if (!logConfig.value) return ''
  if (logConfig.value.override) return 'host override'
  if (logConfig.value.global) return 'global default'
  return 'none — the daemon default applies, which is typically unbounded'
})

async function saveLogConfig(body: LogConfigRequest) {
  logBusy.value = true
  try {
    await daffa.saveHostLogConfig(session.envId, body)
    toast.ok('Logging configuration saved.')
    await qc.invalidateQueries({ queryKey: ['host-log-config'] })
  } catch (e) {
    toast.err(e, 'Could not save.')
  } finally {
    logBusy.value = false
  }
}

async function clearLogConfig() {
  logBusy.value = true
  try {
    await daffa.clearHostLogConfig(session.envId)
    toast.ok('Logging reverted to defaults.')
    await qc.invalidateQueries({ queryKey: ['host-log-config'] })
  } catch (e) {
    toast.err(e, 'Could not revert.')
  } finally {
    logBusy.value = false
  }
}

// Disk is fanned out across every node (each machine has its own layers), fetched once on load.
const { data: df, isLoading: dfLoading } = useQuery({
  queryKey: ['df', () => session.envId],
  queryFn: () => daffa.df(session.envId),
  enabled,
  staleTime: 60_000,
})

// Disk for the selected node (fanned out above). The node is chosen in the node table.
const selectedNodeDisk = computed(() => (df.value ?? []).find((d) => d.node_id === activeNode.value))
const selectedDisk = computed(() => selectedNodeDisk.value?.disk)

// The machine's whole root filesystem — the denominator Docker's footprint is a fraction of. It is
// best-effort (a probe container), so it can be absent while the Docker breakdown is present.
const machineDisk = computed(() => selectedNodeDisk.value?.machine ?? null)
const machinePct = computed(() =>
  machineDisk.value && machineDisk.value.total > 0
    ? Math.round((machineDisk.value.used / machineDisk.value.total) * 100)
    : 0,
)
// Amber past three-quarters, red near full: a disk about to fill is the failure this whole card
// exists to warn about, so it changes colour before it is too late to act on.
const machineBar = computed(() =>
  machinePct.value >= 90 ? 'var(--danger)' : machinePct.value >= 75 ? 'var(--warn)' : 'var(--accent)',
)

const rows = computed(() =>
  selectedDisk.value
    ? [
        { label: 'Images', ...selectedDisk.value.images, prune: 'images' as const, pruneLabel: 'Prune dangling' },
        { label: 'Containers', ...selectedDisk.value.containers, prune: 'containers' as const, pruneLabel: 'Prune stopped' },
        { label: 'Volumes', ...selectedDisk.value.volumes, prune: 'volumes' as const, pruneLabel: 'Prune anonymous' },
        { label: 'Build cache', ...selectedDisk.value.build_cache, prune: 'build-cache' as const, pruneLabel: 'Prune cache' },
      ]
    : [],
)

// The instrument strip. Four numbers, read in one glance.
const instruments = computed<{ label: string; value: string; of?: string }[]>(() =>
  info.value
    ? [
        { label: 'Running', value: `${info.value.running}`, of: `/${info.value.containers}` },
        { label: 'Images', value: `${info.value.images}` },
        { label: 'CPUs', value: `${info.value.ncpu}` },
        { label: 'Memory', value: bytes(info.value.mem_total) },
      ]
    : [],
)
</script>

<template>
  <div>
    <PageHeader
      title="Cluster"
      :description="
        info ? `${info.name} · Docker ${info.server_version} · ${info.os} (${info.arch})` : undefined
      "
    >
      <template #actions>
        <StatusPill v-if="host" :status="hostStatus(host.status)" />
      </template>
    </PageHeader>

    <div class="space-y-6">
      <!--
        THE CLUSTER'S OWN EXISTENCE.

        Creating one, letting machines in, dissolving it. The join tokens are the thing to guard:
        anybody holding one can add a machine to the cluster, and a machine in the cluster runs
        whatever the cluster schedules onto it. So they are fetched by an explicit act rather than
        left sitting on a page, they come from exactly one route, and reading one is audited.
      -->
      <div v-if="canEditSwarm">
        <h2 class="eyebrow mb-2">Swarm</h2>

        <div class="surface rounded-[var(--radius-card)] p-5">
          <!-- Standalone: offer to become one. -->
          <template v-if="!host?.swarm">
            <p class="muted mb-3 text-sm">
              This cluster is a standalone host. Making it a Swarm gives it services, tasks,
              secrets and configs, and lets you deploy Swarm stacks to it. Nothing that is already
              running is touched.
            </p>
            <BaseButton intent="primary" size="sm" :loading="swarmBusy" @click="initSwarm">
              Make this a Swarm
            </BaseButton>
          </template>

          <!-- A Swarm: the join tokens, and the way out. -->
          <template v-else>
            <div class="flex flex-wrap items-center gap-2">
              <BaseButton intent="secondary" size="sm" @click="showTokens">
                Show join tokens
              </BaseButton>
              <BaseButton intent="danger" size="sm" :loading="swarmBusy" @click="leaveSwarm">
                Dissolve the Swarm
              </BaseButton>
            </div>

            <div v-if="tokens" class="mt-4 space-y-3">
              <p class="text-xs" :style="{ color: 'var(--warn)' }">
                <strong>These are keys, not information.</strong> Anybody holding one can add a
                machine to this cluster, and a machine in the cluster runs whatever the cluster
                schedules onto it. Reading them has been recorded in the audit log.
              </p>

              <div>
                <div class="eyebrow mb-1">Add a worker</div>
                <pre
                  class="overflow-x-auto rounded-[var(--radius-control)] p-3 font-mono text-xs"
                  :style="{ background: 'var(--surface-sunken)' }"
                  >docker swarm join --token {{ tokens.worker }} {{ tokens.addr }}</pre
                >
              </div>

              <div>
                <div class="eyebrow mb-1">Add a manager</div>
                <pre
                  class="overflow-x-auto rounded-[var(--radius-control)] p-3 font-mono text-xs"
                  :style="{ background: 'var(--surface-sunken)' }"
                  >docker swarm join --token {{ tokens.manager }} {{ tokens.addr }}</pre
                >
                <p class="subtle mt-1 text-xs">
                  Managers hold the Swarm's consensus and vote, so an EVEN number of them cannot
                  break a tie. Three is the smallest number that survives losing one.
                </p>
              </div>

              <p class="subtle text-xs">
                Daffa will not see the new machine until an agent is enrolled on it — the Swarm will
                schedule onto it either way, but its containers will not be listed and its tasks
                will have no shell.
              </p>
            </div>
          </template>
        </div>
      </div>

      <!-- Nodes: what this environment is made of, AND the page's node selector. On a multi-node
           Swarm, clicking a reachable machine points the daemon summary, disk and history below at
           it; the active row carries the accent bar. -->
      <div v-if="nodes?.length">
        <div class="mb-2 flex items-baseline justify-between gap-3">
          <h2 class="eyebrow">
            {{ host?.swarm ? `Nodes · ${nodes.length}` : 'Node' }}
          </h2>
          <span v-if="multiNode" class="subtle text-xs">
            Select a machine to point this page at it
          </span>
        </div>

        <div class="surface overflow-x-auto rounded-[var(--radius-card)]">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
                <th class="eyebrow px-4 py-2 text-left font-medium">Machine</th>
                <th class="eyebrow py-2 pr-4 text-left font-medium">State</th>
                <th class="eyebrow hidden py-2 pr-4 text-left font-medium md:table-cell">Role</th>
                <th class="eyebrow hidden py-2 pr-4 text-right font-medium md:table-cell">CPU / Memory</th>
                <th class="eyebrow py-2 pr-4 text-right font-medium">
                  <span class="sr-only">Actions</span>
                </th>
              </tr>
            </thead>

            <tbody>
              <tr
                v-for="n in nodes"
                :key="n.swarm_node_id || n.node_id"
                class="border-b transition last:border-0"
                :class="multiNode && n.node_id ? 'cursor-pointer hover:bg-[var(--surface-sunken)]' : ''"
                :style="{
                  borderColor: 'var(--border)',
                  ...(multiNode && n.node_id === activeNode
                    ? { background: 'var(--accent-soft)', boxShadow: 'inset 3px 0 0 0 var(--accent)' }
                    : {}),
                }"
                :aria-selected="multiNode && n.node_id === activeNode"
                @click="multiNode && selectNode(n.node_id)"
              >
                <td class="py-3 pl-4 pr-4">
                  <div class="font-medium">{{ n.name }}</div>
                  <div class="subtle mt-0.5 font-mono text-xs">
                    <span v-if="n.version">Docker {{ n.version }}</span>
                    <span v-if="n.leader"> · leader</span>
                  </div>
                </td>

                <td class="py-3 pr-4">
                  <StatusPill :status="nodeStatus(n)" />
                  <!--
                    THE SENTENCE. Not an error when you click Shell — a statement, on the row,
                    before you click. This is the whole reason the two lists are joined.
                  -->
                  <div v-if="!n.reachable" class="subtle mt-1 text-xs">
                    No Daffa agent here — its containers are not listed, and its tasks have no
                    shell. Their logs still work.
                  </div>
                </td>

                <td class="muted hidden py-3 pr-4 font-mono text-xs md:table-cell">
                  {{ n.role ?? '—' }}
                  <span v-if="n.availability && n.availability !== 'active'" class="subtle">
                    · {{ n.availability }}
                  </span>
                </td>

                <td class="muted hidden py-3 pr-4 text-right font-mono text-xs md:table-cell">
                  <span v-if="n.cpus">{{ n.cpus }} CPU · {{ bytes(n.memory ?? 0) }}</span>
                  <span v-else class="subtle">—</span>
                </td>

                <!--
                  Node operations. Their own capability, because draining a machine moves
                  EVERYBODY's workload while scaling one service moves one — an operator trusted
                  with the second has not thereby been trusted with the first.
                -->
                <td class="py-3 pr-4 text-right" @click.stop>
                  <div v-if="canEditNodes && n.in_swarm" class="flex flex-wrap justify-end gap-1">
                    <BaseButton
                      v-if="n.availability !== 'active'"
                      intent="secondary"
                      size="xs"
                      :loading="busyNode === n.swarm_node_id"
                      title="Schedulable again"
                      @click="nodeOp(n, { availability: 'active' })"
                    >
                      Activate
                    </BaseButton>
                    <BaseButton
                      v-if="n.availability === 'active'"
                      intent="caution"
                      size="xs"
                      :loading="busyNode === n.swarm_node_id"
                      title="Evict its Swarm tasks. Plain containers keep running."
                      @click="drain(n)"
                    >
                      Drain
                    </BaseButton>
                    <BaseButton
                      v-if="n.role === 'worker'"
                      intent="secondary"
                      size="xs"
                      :loading="busyNode === n.swarm_node_id"
                      title="Make this machine a Swarm manager"
                      @click="nodeOp(n, { role: 'manager' })"
                    >
                      Promote
                    </BaseButton>
                    <BaseButton
                      v-else-if="n.role === 'manager'"
                      intent="caution"
                      size="xs"
                      :loading="busyNode === n.swarm_node_id"
                      title="Stop holding the Swarm's consensus"
                      @click="demote(n)"
                    >
                      Demote
                    </BaseButton>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <!-- Daemon -->
      <div
        v-if="info"
        class="grid grid-cols-2 overflow-hidden rounded-[var(--radius-card)] md:grid-cols-4"
        :style="{ background: 'var(--surface-raised)', border: '1px solid var(--border)' }"
      >
        <div
          v-for="(m, i) in instruments"
          :key="m.label"
          class="px-4 py-3.5"
          :class="i > 0 ? 'border-l' : ''"
          :style="{ borderColor: 'var(--border)' }"
        >
          <div class="eyebrow">{{ m.label }}</div>
          <div class="mt-1 font-mono text-2xl font-semibold tracking-tight">
            {{ m.value }}<span v-if="m.of" class="subtle text-base">{{ m.of }}</span>
          </div>
        </div>
      </div>

      <!-- Disk -->
      <div class="surface overflow-hidden rounded-[var(--radius-card)]">
        <div
          class="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1 border-b px-5 py-3"
          :style="{ borderColor: 'var(--border)' }"
        >
          <div class="flex items-baseline gap-3">
            <span class="font-medium">Disk usage</span>
            <!-- Which node's layers, chosen in the node table above. -->
            <span v-if="multiNode" class="subtle text-xs">on {{ activeNodeName }}</span>
          </div>
          <span v-if="selectedDisk" class="muted font-mono text-xs">
            <span class="subtle">Docker</span> {{ bytes(selectedDisk.total_size) }} used ·
            <strong class="font-medium" :style="{ color: 'var(--text)' }">
              {{ bytes(selectedDisk.reclaimable) }}
            </strong>
            reclaimable
          </span>
        </div>

        <!-- The machine's whole root filesystem: the denominator the Docker table above is a slice
             of. Best-effort (a probe container), so it is simply absent when it could not be read —
             the Docker breakdown never waits on it. -->
        <div
          v-if="machineDisk"
          class="border-b px-5 py-3"
          :style="{ borderColor: 'var(--border)' }"
        >
          <div class="mb-2 flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
            <span class="eyebrow">Machine disk</span>
            <span class="muted font-mono text-xs">
              <strong class="font-medium" :style="{ color: 'var(--text)' }">
                {{ bytes(machineDisk.used) }}
              </strong>
              of {{ bytes(machineDisk.total) }} · {{ machinePct }}% full ·
              {{ bytes(machineDisk.free) }} free
            </span>
          </div>
          <div
            class="h-2 w-full overflow-hidden rounded-full"
            :style="{ background: 'var(--surface-sunken)' }"
            role="progressbar"
            :aria-valuenow="machinePct"
            aria-valuemin="0"
            aria-valuemax="100"
          >
            <div
              class="h-full rounded-full"
              :style="{ width: `${machinePct}%`, background: machineBar }"
            />
          </div>
        </div>

        <p v-if="dfLoading" class="muted px-5 py-4 text-sm">Measuring…</p>

        <!-- The card stays overflow-hidden for its corners; the table scrolls in its own box. -->
        <div v-else class="overflow-x-auto">
          <table class="w-full text-sm">
          <thead>
            <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
              <th class="eyebrow py-2 pl-5 pr-4 text-left font-medium">Kind</th>
              <th class="eyebrow hidden py-2 pr-4 text-right font-medium md:table-cell">Count</th>
              <th class="eyebrow py-2 pr-4 text-right font-medium">Size</th>
              <th class="eyebrow py-2 pr-4 text-right font-medium">Reclaimable</th>
              <th class="eyebrow py-2 pr-4 text-right font-medium">
                <span class="sr-only">Actions</span>
              </th>
            </tr>
          </thead>

          <tbody>
            <tr
              v-for="row in rows"
              :key="row.label"
              class="border-b last:border-0"
              :style="{ borderColor: 'var(--border)' }"
            >
              <td class="py-3 pl-5 pr-4 font-medium">{{ row.label }}</td>
              <td class="muted hidden py-3 pr-4 text-right font-mono text-xs md:table-cell">{{ row.count }}</td>
              <td class="py-3 pr-4 text-right font-mono text-xs">{{ bytes(row.size) }}</td>
              <td class="py-3 pr-4 text-right font-mono text-xs">
                <!-- Amber, not red: reclaimable space is an opportunity, not a fault. -->
                <span v-if="row.reclaimable > 0" :style="{ color: 'var(--warn)' }">
                  {{ bytes(row.reclaimable) }}
                </span>
                <span v-else class="subtle">—</span>
              </td>
              <td class="py-2 pr-4 text-right">
                <PruneButton :target="row.prune" :label="row.pruneLabel" class="justify-end" />
              </td>
            </tr>
          </tbody>
          </table>
        </div>
      </div>

      <!-- Container log defaults: the other disk story. The table above is what images and
           volumes are sitting on; unrotated json-file logs are what quietly joins them. -->
      <div v-if="canViewLogging" class="surface rounded-[var(--radius-card)] p-5">
        <div class="mb-1 flex flex-wrap items-baseline justify-between gap-x-3">
          <h3 class="text-sm font-semibold">Container log defaults</h3>
          <span v-if="logConfig" class="subtle text-xs">
            in effect: <span class="font-mono">{{ logConfig.effective?.driver ?? 'none' }}</span>
            · {{ logSource }}
          </span>
        </div>
        <p class="muted mb-4 max-w-[70ch] text-sm leading-relaxed">
          The log driver and rotation injected into stacks deployed to this cluster, for
          services that don't declare their own
          <span class="font-mono text-xs">logging:</span>. Set here, it overrides the fleet
          default from Settings; applied at each service's next deploy.
        </p>

        <LogConfigForm
          :model-value="logConfig?.override ?? logConfig?.global ?? null"
          :disabled="!canEditLogging"
          :busy="logBusy"
          :show-clear="!!logConfig?.override"
          clear-label="Revert to the global default"
          @save="saveLogConfig"
          @clear="clearLogConfig"
        />
        <p v-if="logConfig && !logConfig.override && logConfig.global" class="subtle mt-3 text-xs">
          Showing the global default — saving makes it this cluster's own override.
        </p>
      </div>

      <!-- The MACHINE's own load — the whole box's CPU and memory, read from its /proc, not the sum
           of the containers on it. Per node on a Swarm (the node chosen in the table above). Disk
           usage above is what is SITTING here; this is what is being USED. -->
      <div class="surface rounded-[var(--radius-card)] p-5">
        <div v-if="multiNode" class="subtle mb-3 text-xs">
          Machine metrics for <span class="font-medium">{{ activeNodeName }}</span>
        </div>
        <MetricPanel host :node="activeNode" :key="activeNode" />
      </div>
    </div>
  </div>
</template>
