<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { daffa, type Agent, type NewAgent, type Environment } from '@/lib/api'
import { toast } from '@/lib/toast'
import { confirm } from '@/lib/confirm'
import { hostStatus, type Status } from '@/lib/status'
import { useSession } from '@/stores/session'
import { Cap } from '@/lib/caps'
import { streamProvision } from '@/lib/stream'
import { RouterLink } from 'vue-router'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import CopyButton from '@/components/ui/CopyButton.vue'
import EmptyState from '@/components/ui/EmptyState.vue'
import Select from '@/components/ui/Select.vue'
import StatusPill from '@/components/ui/StatusPill.vue'

const qc = useQueryClient()
const session = useSession()

// The clusters are the environments the switcher shows; poll them so a cluster coming online after
// an SSH reconnect or an agent join updates without a manual refresh.
const { data: clusters } = useQuery({
  queryKey: ['environments'],
  queryFn: daffa.environments,
  refetchInterval: 10_000,
})

const { data: sshKeys } = useQuery({ queryKey: ['ssh-keys'], queryFn: daffa.sshKeys })
const hasKeys = computed(() => (sshKeys.value?.length ?? 0) > 0)

// ── a cluster's transport, for the badge ────────────────────────────────────────
function transport(c: Environment): string {
  if (c.swarm) return c.nodes.length === 1 ? 'Swarm' : `Swarm · ${c.nodes.length} nodes`
  const kind = c.nodes[0]?.kind
  if (kind === 'local') return 'Local socket'
  if (kind === 'ssh') return 'SSH'
  if (kind === 'agent') return 'Agent'
  return '—'
}

// Only SSH clusters are removed here (the backend refuses the local and agent-backed ones).
function removable(c: Environment): boolean {
  return c.nodes.length > 0 && c.nodes.every((n) => n.kind === 'ssh')
}

// ── add a cluster over SSH ───────────────────────────────────────────────────────
const adding = ref(false)
const blank = () => ({ name: '', host: '', port: 22, user: '', key_id: '', endpoint: '' })
const form = ref(blank())
const testResult = ref<{ ok: boolean; reachable: boolean; message: string } | null>(null)

const test = useMutation({
  mutationFn: () => daffa.testClusterConnection(form.value),
  onSuccess: (r) => {
    testResult.value = r.ok
      ? { ok: true, reachable: true, message: `Reached Docker ${r.server_version} on ${r.os}/${r.arch}.` }
      : { ok: false, reachable: r.reachable, message: r.error ?? 'The connection failed.' }
  },
  onError: (e) => toast.err(e, 'Could not run the test.'),
})

// ── provisioning: install Docker on a reachable-but-Docker-less machine ──────────
const canProvision = computed(() => session.can(Cap.ClustersProvision))
const provisioning = ref(false)
const provisionLog = ref<string[]>([])
let stopProvision: (() => void) | null = null

// Offered exactly when SSH worked but Docker did not answer — the one case installing Docker fixes.
const canOfferProvision = computed(
  () => canProvision.value && testResult.value && !testResult.value.ok && testResult.value.reachable,
)

// Abort an in-flight provision if the operator leaves the page.
onBeforeUnmount(() => stopProvision?.())

function provision() {
  stopProvision?.() // abort any previous run
  provisionLog.value = []
  provisioning.value = true
  stopProvision = streamProvision(form.value, {
    log: (t) => provisionLog.value.push(t),
    end: () => {
      provisioning.value = false
      toast.ok('Docker installed — testing the connection again.')
      test.mutate()
    },
    error: (m) => {
      provisioning.value = false
      provisionLog.value.push('ERROR ' + m)
    },
  })
}

const createCluster = useMutation({
  mutationFn: () => daffa.createCluster(form.value),
  onSuccess: () => {
    toast.ok('Cluster added.')
    form.value = blank()
    testResult.value = null
    adding.value = false
    qc.invalidateQueries({ queryKey: ['environments'] })
  },
  onError: (e) => toast.err(e, 'Could not add the cluster.'),
})

const removeCluster = useMutation({
  mutationFn: (id: string) => daffa.deleteCluster(id),
  onSuccess: () => toast.ok('Cluster removed.'),
  onError: (e) => toast.err(e, 'Could not remove the cluster.'),
  onSettled: () => qc.invalidateQueries({ queryKey: ['environments'] }),
})

async function onRemoveCluster(c: Environment) {
  const ok = await confirm({
    title: `Remove the cluster ${c.name}?`,
    body: 'Daffa stops connecting to it and forgets it. Nothing on the remote machine changes — its containers keep running.',
    confirmLabel: 'Remove',
    intent: 'danger',
  })
  if (!ok) return
  removeCluster.mutate(c.id)
}

// ── add a node to a cluster's Swarm ──────────────────────────────────────────────
// A node joins an existing Swarm, over SSH (Daffa dials the machine) or via an agent (the machine
// dials out). Either way Daffa issues the join — nobody runs `docker swarm join`. Both only appear
// on a Swarm cluster the caller may edit nodes on.
const addingNodeTo = ref<string | null>(null)
const nodeBlank = () => ({
  connection: 'ssh' as 'ssh' | 'agent',
  name: '',
  host: '',
  port: 22,
  user: '',
  key_id: '',
  endpoint: '',
  role: 'worker' as 'worker' | 'manager',
  advertise_addr: '',
})
const nodeForm = ref(nodeBlank())
const mintedAgent = ref<NewAgent | null>(null)

function canAddNode(c: Environment): boolean {
  return c.swarm && session.can(Cap.NodesEdit, c.id)
}

function toggleAddNode(id: string) {
  addingNodeTo.value = addingNodeTo.value === id ? null : id
  nodeForm.value = nodeBlank()
  mintedAgent.value = null
}

const addNode = useMutation({
  mutationFn: (cluster: string) =>
    daffa.addNode(cluster, {
      name: nodeForm.value.name,
      host: nodeForm.value.host,
      port: nodeForm.value.port,
      user: nodeForm.value.user,
      key_id: nodeForm.value.key_id,
      endpoint: nodeForm.value.endpoint,
      role: nodeForm.value.role,
    }),
  onSuccess: () => {
    toast.ok('Node joining — it appears here once it connects.')
    addingNodeTo.value = null
    nodeForm.value = nodeBlank()
    qc.invalidateQueries({ queryKey: ['environments'] })
  },
  onError: (e) => toast.err(e, 'Could not add the node.'),
})

const createAgent = useMutation({
  mutationFn: (cluster: string) =>
    daffa.createAgent({
      name: nodeForm.value.name,
      cluster,
      role: nodeForm.value.role,
      advertise_addr: nodeForm.value.advertise_addr,
    }),
  onSuccess: (agent) => {
    mintedAgent.value = agent // the expansion stays open so the join command can be copied
    qc.invalidateQueries({ queryKey: ['agents'] })
  },
  onError: (e) => toast.err(e, 'Could not create the agent.'),
})

function submitNode(cluster: string) {
  if (nodeForm.value.connection === 'ssh') addNode.mutate(cluster)
  else createAgent.mutate(cluster)
}

// ── agents (status of machines that dial out) ────────────────────────────────────
const { data: agents } = useQuery({
  queryKey: ['agents'],
  queryFn: daffa.agents,
  refetchInterval: 10_000,
})

const removeAgent = useMutation({
  mutationFn: (id: string) => daffa.deleteAgent(id),
  onSuccess: () => toast.ok('Agent removed.'),
  onError: (e) => toast.err(e, 'Could not remove the agent.'),
  onSettled: () => {
    qc.invalidateQueries({ queryKey: ['agents'] })
    qc.invalidateQueries({ queryKey: ['environments'] })
  },
})

async function onRemoveAgent(a: Agent) {
  const ok = await confirm({
    title: `Remove the agent ${a.name}?`,
    body: 'Its tunnel is cut immediately and the node disappears from Daffa. Nothing on the machine itself is changed — its containers keep running.',
    confirmLabel: 'Remove',
    intent: 'danger',
  })
  if (!ok) return
  removeAgent.mutate(a.id)
}

function agentStatus(a: Agent): Status {
  if (a.status === 'pending')
    return { tone: 'warn', label: 'Pending', live: true, detail: 'waiting to check in' }
  return hostStatus(a.status)
}

function installCommand(a: NewAgent): string {
  return `daffa agent --server ${location.origin} --token ${a.join_token}`
}
</script>

<template>
  <div>
    <div class="mb-5">
      <h2 class="text-base font-semibold">Clusters</h2>
      <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
        A cluster is a Docker environment Daffa manages — the local box, or a machine reached over
        SSH. Each is what you pick in the switcher and scope grants to. Add nodes to a Swarm cluster
        over SSH or via an agent.
      </p>
    </div>

    <!-- The clusters that exist -->
    <div class="surface mb-6 overflow-hidden rounded-[var(--radius-card)]">
      <table class="w-full text-sm">
        <thead>
          <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
            <th class="eyebrow px-4 py-2 text-left font-medium">Status</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Cluster</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Transport</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">Actions</th>
          </tr>
        </thead>
        <tbody>
          <template v-for="c in clusters" :key="c.id">
            <tr
              class="border-b transition hover:bg-[var(--surface-sunken)]"
              :class="{ 'last:border-0': addingNodeTo !== c.id }"
              :style="{ borderColor: 'var(--border)' }"
            >
              <td class="px-4 py-3"><StatusPill :status="hostStatus(c.status)" /></td>
              <td class="py-3 pr-4">
                <div class="font-medium">{{ c.name }}</div>
                <div class="subtle mt-0.5 text-xs">
                  {{ c.nodes.length }} node{{ c.nodes.length === 1 ? '' : 's' }}
                </div>
              </td>
              <td class="subtle py-3 pr-4 font-mono text-xs">{{ transport(c) }}</td>
              <td class="py-3 pr-4 text-right">
                <div class="flex items-center justify-end gap-1">
                  <BaseButton
                    v-if="canAddNode(c)"
                    :intent="addingNodeTo === c.id ? 'primary' : 'secondary'"
                    size="xs"
                    @click="toggleAddNode(c.id)"
                  >
                    <AppIcon name="plus" class="size-3.5" />
                    Add node
                  </BaseButton>
                  <BaseButton
                    v-if="removable(c)"
                    intent="danger"
                    size="xs"
                    :disabled="removeCluster.isPending.value"
                    @click="onRemoveCluster(c)"
                  >
                    <AppIcon name="trash" class="size-3.5" />
                    Remove
                  </BaseButton>
                  <span v-if="!canAddNode(c) && !removable(c)" class="subtle text-xs">—</span>
                </div>
              </td>
            </tr>

            <!-- Add a node to this cluster's Swarm — over SSH or via an agent. -->
            <tr
              v-if="addingNodeTo === c.id"
              class="border-b last:border-0"
              :style="{ borderColor: 'var(--border)', background: 'var(--surface-sunken)' }"
            >
              <td colspan="4" class="px-4 py-4">
                <div class="mb-3 flex gap-1">
                  <BaseButton
                    :intent="nodeForm.connection === 'ssh' ? 'primary' : 'ghost'"
                    size="xs"
                    @click="nodeForm.connection = 'ssh'"
                  >
                    Over SSH
                  </BaseButton>
                  <BaseButton
                    :intent="nodeForm.connection === 'agent' ? 'primary' : 'ghost'"
                    size="xs"
                    @click="nodeForm.connection = 'agent'"
                  >
                    Via agent
                  </BaseButton>
                </div>

                <div class="grid gap-3 sm:grid-cols-2">
                  <div>
                    <label class="mb-1 block text-xs font-medium">Name</label>
                    <input v-model="nodeForm.name" placeholder="worker-1" class="field" data-cursor="text" />
                  </div>
                  <div>
                    <label class="mb-1 block text-xs font-medium">Role</label>
                    <Select v-model="nodeForm.role">
                      <option value="worker">Worker</option>
                      <option value="manager">Manager</option>
                    </Select>
                  </div>
                </div>

                <!-- SSH: Daffa dials the machine -->
                <form v-if="nodeForm.connection === 'ssh'" class="mt-3 space-y-3" @submit.prevent="submitNode(c.id)">
                  <p v-if="!hasKeys" class="text-sm" :style="{ color: 'var(--warn)' }">
                    You need an SSH key first.
                    <RouterLink :to="{ name: 'settings-ssh' }" class="underline">Add one under SSH keys</RouterLink>.
                  </p>
                  <template v-else>
                    <div class="grid gap-3 sm:grid-cols-[1fr_6rem_1fr]">
                      <div>
                        <label class="mb-1 block text-xs font-medium">Host</label>
                        <input v-model="nodeForm.host" required placeholder="10.0.0.10" class="field font-mono text-xs" data-cursor="text" />
                      </div>
                      <div>
                        <label class="mb-1 block text-xs font-medium">Port</label>
                        <input v-model.number="nodeForm.port" type="number" min="1" max="65535" class="field" />
                      </div>
                      <div>
                        <label class="mb-1 block text-xs font-medium">SSH user</label>
                        <input v-model="nodeForm.user" required placeholder="docker" class="field font-mono text-xs" data-cursor="text" />
                      </div>
                    </div>
                    <div class="grid gap-3 sm:grid-cols-2">
                      <div>
                        <label class="mb-1 block text-xs font-medium">SSH key</label>
                        <Select v-model="nodeForm.key_id">
                          <option value="" disabled>Choose a key…</option>
                          <option v-for="k in sshKeys" :key="k.id" :value="k.id">{{ k.name }} ({{ k.algo }})</option>
                        </Select>
                      </div>
                      <div>
                        <label class="mb-1 block text-xs font-medium">Docker endpoint <span class="subtle font-normal">(optional)</span></label>
                        <input v-model="nodeForm.endpoint" placeholder="unix:///var/run/docker.sock" class="field font-mono text-xs" data-cursor="text" />
                      </div>
                    </div>
                    <p class="subtle text-xs">
                      The join needs 2377/tcp, 7946/tcp+udp and 4789/udp open between the node and the
                      manager; Daffa cannot open a firewall for you.
                    </p>
                    <BaseButton type="submit" intent="primary" size="sm" :loading="addNode.isPending.value" :disabled="!nodeForm.key_id">
                      Join node
                    </BaseButton>
                  </template>
                </form>

                <!-- Agent: the machine dials out; Daffa joins it when it connects -->
                <form v-else class="mt-3 space-y-3" @submit.prevent="submitNode(c.id)">
                  <div class="sm:w-1/2">
                    <label class="mb-1 block text-xs font-medium">
                      Advertise address <span class="subtle font-normal">(optional)</span>
                    </label>
                    <input v-model="nodeForm.advertise_addr" placeholder="the node's reachable IP" class="field font-mono text-xs" data-cursor="text" />
                    <p class="subtle mt-1 text-xs">
                      The address the manager reaches this node at, for the overlay. Leave blank to let
                      the node detect it — set it if the node has more than one network.
                    </p>
                  </div>
                  <p class="subtle text-xs">
                    An agent dials out to Daffa (for a machine behind NAT). It needs the Swarm ports to
                    the manager for the overlay just the same. When it connects, Daffa joins it.
                  </p>

                  <div
                    v-if="mintedAgent"
                    class="rounded-[var(--radius-control)] border p-3"
                    :style="{ background: 'var(--accent-soft)', borderColor: 'color-mix(in oklch, var(--accent) 35%, transparent)' }"
                  >
                    <p class="text-xs font-medium">Run this on <strong>{{ mintedAgent.name }}</strong>:</p>
                    <div class="mt-2 flex items-start gap-2 rounded-[var(--radius-control)] p-2 font-mono text-xs" :style="{ background: 'var(--surface-sunken)' }">
                      <code class="flex-1 break-all">{{ installCommand(mintedAgent) }}</code>
                      <CopyButton intent="ghost" size="xs" class="shrink-0" :text="installCommand(mintedAgent!)" />
                    </div>
                    <p class="muted mt-2 text-xs">
                      Shown once, expires in <span class="font-mono">{{ Math.round(mintedAgent.expires_in / 60) }}</span> minutes.
                      When the agent connects, Daffa joins it to this cluster's Swarm.
                    </p>
                  </div>

                  <BaseButton v-else type="submit" intent="primary" size="sm" :loading="createAgent.isPending.value" :disabled="!nodeForm.name">
                    Create join command
                  </BaseButton>
                </form>
              </td>
            </tr>
          </template>
        </tbody>
      </table>
    </div>

    <!-- Add a cluster over SSH -->
    <div class="surface mb-8 rounded-[var(--radius-card)] p-5">
      <div class="flex flex-wrap items-center gap-x-3 gap-y-2">
        <div class="min-w-0">
          <h3 class="text-sm font-semibold">Add a cluster over SSH</h3>
          <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
            Connect to a machine that already runs Docker. Daffa dials out over SSH — the machine
            needs port 22 reachable and the SSH user needs access to its Docker socket.
          </p>
        </div>
        <div class="ml-auto">
          <BaseButton :intent="adding ? 'secondary' : 'primary'" @click="adding = !adding">
            <AppIcon v-if="!adding" name="plus" class="size-4" />
            {{ adding ? 'Cancel' : 'Add cluster' }}
          </BaseButton>
        </div>
      </div>

      <div v-if="adding" class="mt-5">
        <p v-if="!hasKeys" class="text-sm" :style="{ color: 'var(--warn)' }">
          You need an SSH key first.
          <RouterLink :to="{ name: 'settings-ssh' }" class="underline">Add one under SSH keys</RouterLink>
          — then add its public half to the target's <code class="font-mono">authorized_keys</code>.
        </p>

        <form v-else class="space-y-4" @submit.prevent="createCluster.mutate()">
          <div class="grid gap-4 sm:grid-cols-2">
            <div>
              <label for="c-name" class="mb-1.5 block text-sm font-medium">Name</label>
              <input id="c-name" v-model="form.name" required placeholder="prod-eu" class="field" data-cursor="text" />
            </div>
            <div>
              <label for="c-key" class="mb-1.5 block text-sm font-medium">SSH key</label>
              <Select id="c-key" v-model="form.key_id">
                <option value="" disabled>Choose a key…</option>
                <option v-for="k in sshKeys" :key="k.id" :value="k.id">{{ k.name }} ({{ k.algo }})</option>
              </Select>
            </div>
          </div>

          <div class="grid gap-4 sm:grid-cols-[1fr_7rem_1fr]">
            <div>
              <label for="c-host" class="mb-1.5 block text-sm font-medium">Host</label>
              <input id="c-host" v-model="form.host" required placeholder="10.0.0.9 or host.example.com" class="field font-mono text-xs" data-cursor="text" />
            </div>
            <div>
              <label for="c-port" class="mb-1.5 block text-sm font-medium">Port</label>
              <input id="c-port" v-model.number="form.port" type="number" min="1" max="65535" class="field" />
            </div>
            <div>
              <label for="c-user" class="mb-1.5 block text-sm font-medium">SSH user</label>
              <input id="c-user" v-model="form.user" required placeholder="docker" class="field font-mono text-xs" data-cursor="text" />
            </div>
          </div>

          <div>
            <label for="c-endpoint" class="mb-1.5 block text-sm font-medium">
              Docker endpoint <span class="subtle font-normal">(optional)</span>
            </label>
            <input id="c-endpoint" v-model="form.endpoint" placeholder="unix:///var/run/docker.sock" class="field font-mono text-xs" data-cursor="text" />
          </div>

          <div class="flex flex-wrap items-center gap-2">
            <BaseButton type="button" intent="secondary" size="md" :loading="test.isPending.value" :disabled="!form.host || !form.user || !form.key_id" @click="test.mutate()">
              Test connection
            </BaseButton>
            <BaseButton type="submit" intent="primary" size="md" :loading="createCluster.isPending.value">
              Add cluster
            </BaseButton>
            <span v-if="testResult" class="text-xs" :style="{ color: testResult.ok ? 'var(--success)' : 'var(--danger)' }">
              {{ testResult.ok ? '✓' : '✗' }} {{ testResult.message }}
            </span>
          </div>

          <!-- SSH worked but Docker is absent: offer to install it over SSH. -->
          <div v-if="canOfferProvision" class="rounded-[var(--radius-control)] border p-3" :style="{ borderColor: 'var(--border)' }">
            <div class="flex flex-wrap items-center gap-2">
              <span class="text-sm">Docker is not installed on this machine.</span>
              <BaseButton type="button" intent="secondary" size="sm" :loading="provisioning" @click="provision()">
                <AppIcon name="download" class="size-3.5" />
                Set up this machine
              </BaseButton>
              <span class="subtle text-xs">Installs Docker over SSH. Needs root or passwordless sudo.</span>
            </div>
            <pre
              v-if="provisionLog.length"
              class="mt-3 max-h-64 overflow-auto rounded-[var(--radius-control)] p-3 font-mono text-xs"
              :style="{ background: 'var(--surface-sunken)' }"
            ><span v-for="(l, i) in provisionLog" :key="i" class="block whitespace-pre-wrap">{{ l }}</span></pre>
          </div>
        </form>
      </div>
    </div>

    <!-- Agents: the connection state of machines that dial out (added per cluster, above) -->
    <div v-if="agents?.length" class="mb-3">
      <h3 class="text-sm font-semibold">Agents</h3>
      <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
        Machines that dial out to Daffa. Each is enrolled to a cluster from its “Add node” button;
        this is where you see whether it has checked in.
      </p>
    </div>

    <div v-if="agents?.length" class="surface overflow-hidden rounded-[var(--radius-card)]">
      <table class="w-full text-sm">
        <thead>
          <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
            <th class="eyebrow px-4 py-2 text-left font-medium">Status</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Agent</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Version</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">Actions</th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="a in agents"
            :key="a.id"
            class="border-b transition last:border-0 hover:bg-[var(--surface-sunken)]"
            :style="{ borderColor: 'var(--border)' }"
          >
            <td class="px-4 py-3"><StatusPill :status="agentStatus(a)" /></td>
            <td class="py-3 pr-4">
              <div class="font-medium">{{ a.name }}</div>
              <div class="subtle mt-0.5 text-xs">
                <template v-if="a.status === 'pending'">waiting for the machine to check in</template>
                <template v-else-if="a.last_seen">last seen {{ new Date(a.last_seen).toLocaleString() }}</template>
                <template v-else>never seen</template>
              </div>
            </td>
            <td class="subtle py-3 pr-4 font-mono text-xs">{{ a.version || '—' }}</td>
            <td class="py-3 pr-4 text-right">
              <BaseButton intent="danger" size="xs" :disabled="removeAgent.isPending.value" @click="onRemoveAgent(a)">
                <AppIcon name="trash" class="size-3.5" />
                Remove
              </BaseButton>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <EmptyState
      v-else
      icon="server"
      title="No agents"
      body="Add a node to a Swarm cluster “via agent” to bring a machine that can only dial out under the same console."
    />
  </div>
</template>
