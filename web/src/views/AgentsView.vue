<script setup lang="ts">
import { ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { daffa, type Agent, type NewAgent } from '@/lib/api'
import { toast } from '@/lib/toast'
import { confirm } from '@/lib/confirm'
import { hostStatus, type Status } from '@/lib/status'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import CopyButton from '@/components/ui/CopyButton.vue'
import EmptyState from '@/components/ui/EmptyState.vue'
import StatusPill from '@/components/ui/StatusPill.vue'

const qc = useQueryClient()

const { data: agents, isLoading } = useQuery({
  queryKey: ['agents'],
  queryFn: daffa.agents,
  refetchInterval: 10_000, // a tunnel coming up has no event to hang off
})

const name = ref('')
// The join token is shown ONCE, here, and never stored anywhere it can be read back.
// If it is lost, the answer is to delete the agent and add it again — not to go
// looking for it.
const created = ref<NewAgent | null>(null)

const create = useMutation({
  mutationFn: (n: string) => daffa.createAgent(n),
  onSuccess: (agent) => {
    created.value = agent
    name.value = ''
    qc.invalidateQueries({ queryKey: ['agents'] })
  },
  onError: (e) => toast.err(e, 'Could not create the agent.'),
})

const remove = useMutation({
  mutationFn: (id: string) => daffa.deleteAgent(id),
  onSuccess: () => toast.ok('Agent removed.'),
  onError: (e) => toast.err(e, 'Could not remove the agent.'),
  onSettled: () => {
    qc.invalidateQueries({ queryKey: ['agents'] })
    qc.invalidateQueries({ queryKey: ['environments'] })
  },
})

async function onRemove(a: Agent) {
  const ok = await confirm({
    title: `Remove the agent ${a.name}?`,
    body: 'Its tunnel is cut immediately and the host disappears from Daffa. Nothing on the host itself is changed — its containers keep running.',
    confirmLabel: 'Remove',
    intent: 'danger',
  })
  if (!ok) return
  remove.mutate(a.id)
}

/**
 * hostStatus knows two states, and an agent has three: it can be enrolled and not yet have
 * called home. Calling that "offline" would be a lie — nothing is wrong, the host simply has
 * not run the command yet — so it is amber and live rather than red.
 */
function agentStatus(a: Agent): Status {
  if (a.status === 'pending')
    return { tone: 'warn', label: 'Pending', live: true, detail: 'waiting to check in' }
  return hostStatus(a.status)
}

function installCommand(a: NewAgent): string {
  const server = location.origin
  return `daffa agent --server ${server} --token ${a.join_token}`
}

function seen(a: Agent): string {
  if (!a.last_seen) return 'never'
  return new Date(a.last_seen).toLocaleString()
}
</script>

<template>
  <div>
    <div class="mb-5 flex flex-wrap items-center gap-x-3 gap-y-2">
      <div class="min-w-0">
        <h2 class="text-base font-semibold">Agents</h2>
        <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
          An agent runs on a host you want to manage and dials out to Daffa. The host needs no
          inbound port and exposes no Docker socket to the network.
        </p>
      </div>
    </div>

    <!-- Add -->
    <div class="surface mb-6 rounded-[var(--radius-card)] p-5">
      <form class="flex items-end gap-3" @submit.prevent="create.mutate(name)">
        <div class="flex-1">
          <label for="agent-name" class="mb-1.5 block text-sm font-medium">New agent</label>
          <input
            id="agent-name"
            v-model="name"
            required
            placeholder="e.g. prod-1"
            class="field"
            data-cursor="text"
          />
        </div>
        <BaseButton type="submit" intent="primary" size="md" :loading="create.isPending.value">
          Add agent
        </BaseButton>
      </form>

      <!-- The one-time token -->
      <div
        v-if="created"
        class="mt-5 rounded-[var(--radius-control)] border p-4"
        :style="{
          background: 'var(--accent-soft)',
          borderColor: 'color-mix(in oklch, var(--accent) 35%, transparent)',
        }"
      >
        <p class="text-sm font-medium">
          Run this on <strong>{{ created.name }}</strong
          >:
        </p>
        <div
          class="mt-2 flex items-start gap-2 rounded-[var(--radius-control)] p-3 font-mono text-xs"
          :style="{ background: 'var(--surface-sunken)' }"
        >
          <code class="flex-1 break-all">{{ installCommand(created) }}</code>
          <CopyButton intent="ghost" size="xs" class="shrink-0" :text="installCommand(created!)" />
        </div>
        <p class="muted mt-2 text-xs">
          This join token is shown once and expires in
          <span class="font-mono">{{ Math.round(created.expires_in / 60) }}</span> minutes. It can
          be used a single time; after that the agent authenticates with its own credential.
        </p>
      </div>
    </div>

    <!-- List -->
    <p v-if="isLoading" class="muted text-sm">Loading…</p>

    <EmptyState
      v-else-if="!agents?.length"
      icon="server"
      title="No agents yet"
      body="Daffa is managing only its local Docker socket. Add an agent to bring another host under the same console — it dials out to Daffa, so the host needs no inbound port."
    />

    <div v-else class="surface overflow-hidden rounded-[var(--radius-card)]">
      <table class="w-full text-sm">
        <thead>
          <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
            <th class="eyebrow px-4 py-2 text-left font-medium">Status</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Host</th>
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
            <td class="px-4 py-3">
              <StatusPill :status="agentStatus(a)" />
            </td>

            <td class="py-3 pr-4">
              <div class="font-medium">{{ a.name }}</div>
              <div class="subtle mt-0.5 text-xs">
                <template v-if="a.status === 'pending'"> waiting for the host to check in </template>
                <template v-else> last seen {{ seen(a) }} </template>
              </div>
            </td>

            <td class="subtle py-3 pr-4 font-mono text-xs">{{ a.version || '—' }}</td>

            <td class="py-3 pr-4 text-right">
              <BaseButton
                intent="danger"
                size="xs"
                :disabled="remove.isPending.value"
                @click="onRemove(a)"
              >
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
