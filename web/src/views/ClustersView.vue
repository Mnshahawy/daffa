<script setup lang="ts">
import { computed, ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { daffa, type Agent, type NewAgent, type Environment } from '@/lib/api'
import { toast } from '@/lib/toast'
import { confirm } from '@/lib/confirm'
import { hostStatus, type Status } from '@/lib/status'
import { RouterLink } from 'vue-router'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import CopyButton from '@/components/ui/CopyButton.vue'
import EmptyState from '@/components/ui/EmptyState.vue'
import Select from '@/components/ui/Select.vue'
import StatusPill from '@/components/ui/StatusPill.vue'

const qc = useQueryClient()

// The clusters are the environments the switcher shows; poll them so a cluster coming online
// after an SSH reconnect updates without a manual refresh.
const { data: clusters } = useQuery({
  queryKey: ['environments'],
  queryFn: daffa.environments,
  refetchInterval: 10_000,
})

// SSH keys to dial with. Adding a cluster needs one; if there are none we point at the key store.
const { data: sshKeys } = useQuery({ queryKey: ['ssh-keys'], queryFn: daffa.sshKeys })

// ── a cluster's transport, for the badge ────────────────────────────────────────
// A cluster is made of nodes; its transport is how Daffa reaches them. Swarm clusters say their
// size instead, since "how it is reached" stops being one answer.
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
const testResult = ref<{ ok: boolean; message: string } | null>(null)

const test = useMutation({
  mutationFn: () => daffa.testClusterConnection(form.value),
  onSuccess: (r) => {
    testResult.value = r.ok
      ? { ok: true, message: `Reached Docker ${r.server_version} on ${r.os}/${r.arch}.` }
      : { ok: false, message: r.error ?? 'The connection failed.' }
  },
  onError: (e) => toast.err(e, 'Could not run the test.'),
})

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

// ── add a node via agent (a machine that dials OUT) ──────────────────────────────
const { data: agents, isLoading: agentsLoading } = useQuery({
  queryKey: ['agents'],
  queryFn: daffa.agents,
  refetchInterval: 10_000,
})

const agentName = ref('')
const createdAgent = ref<NewAgent | null>(null)

const createAgent = useMutation({
  mutationFn: (n: string) => daffa.createAgent(n),
  onSuccess: (agent) => {
    createdAgent.value = agent
    agentName.value = ''
    qc.invalidateQueries({ queryKey: ['agents'] })
  },
  onError: (e) => toast.err(e, 'Could not create the agent.'),
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

const hasKeys = computed(() => (sshKeys.value?.length ?? 0) > 0)
</script>

<template>
  <div>
    <div class="mb-5">
      <h2 class="text-base font-semibold">Clusters</h2>
      <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
        A cluster is a Docker environment Daffa manages — the local box, a machine reached over
        SSH, or an agent that dials out. Each is what you pick in the switcher and scope grants to.
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
          <tr
            v-for="c in clusters"
            :key="c.id"
            class="border-b transition last:border-0 hover:bg-[var(--surface-sunken)]"
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
              <span v-else class="subtle text-xs">—</span>
            </td>
          </tr>
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
            <p class="subtle mt-1 text-xs">Where the daemon listens on the remote machine. Leave blank for the standard socket.</p>
          </div>

          <div class="flex flex-wrap items-center gap-2">
            <BaseButton type="button" intent="secondary" size="md" :loading="test.isPending.value" :disabled="!form.host || !form.user || !form.key_id" @click="test.mutate()">
              Test connection
            </BaseButton>
            <BaseButton type="submit" intent="primary" size="md" :loading="createCluster.isPending.value">
              Add cluster
            </BaseButton>
            <span
              v-if="testResult"
              class="text-xs"
              :style="{ color: testResult.ok ? 'var(--success)' : 'var(--danger)' }"
            >
              {{ testResult.ok ? '✓' : '✗' }} {{ testResult.message }}
            </span>
          </div>
        </form>
      </div>
    </div>

    <!-- Add a node via agent -->
    <div class="mb-5">
      <h3 class="text-sm font-semibold">Nodes via agent</h3>
      <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
        An agent runs on a machine and dials out to Daffa — for a host behind NAT with no inbound
        port. Each enrolled agent becomes its own cluster until it joins a Swarm.
      </p>
    </div>

    <div class="surface mb-6 rounded-[var(--radius-card)] p-5">
      <form class="flex items-end gap-3" @submit.prevent="createAgent.mutate(agentName)">
        <div class="flex-1">
          <label for="agent-name" class="mb-1.5 block text-sm font-medium">New agent</label>
          <input id="agent-name" v-model="agentName" required placeholder="e.g. prod-1" class="field" data-cursor="text" />
        </div>
        <BaseButton type="submit" intent="primary" size="md" :loading="createAgent.isPending.value">
          Add agent
        </BaseButton>
      </form>

      <div
        v-if="createdAgent"
        class="mt-5 rounded-[var(--radius-control)] border p-4"
        :style="{ background: 'var(--accent-soft)', borderColor: 'color-mix(in oklch, var(--accent) 35%, transparent)' }"
      >
        <p class="text-sm font-medium">Run this on <strong>{{ createdAgent.name }}</strong>:</p>
        <div class="mt-2 flex items-start gap-2 rounded-[var(--radius-control)] p-3 font-mono text-xs" :style="{ background: 'var(--surface-sunken)' }">
          <code class="flex-1 break-all">{{ installCommand(createdAgent) }}</code>
          <CopyButton intent="ghost" size="xs" class="shrink-0" :text="installCommand(createdAgent!)" />
        </div>
        <p class="muted mt-2 text-xs">
          This join token is shown once and expires in
          <span class="font-mono">{{ Math.round(createdAgent.expires_in / 60) }}</span> minutes.
        </p>
      </div>
    </div>

    <p v-if="agentsLoading" class="muted text-sm">Loading…</p>

    <EmptyState
      v-else-if="!agents?.length"
      icon="server"
      title="No agents yet"
      body="Add an agent to bring a machine that can only dial out under the same console — or add a cluster over SSH above."
    />

    <div v-else class="surface overflow-hidden rounded-[var(--radius-card)]">
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
  </div>
</template>
