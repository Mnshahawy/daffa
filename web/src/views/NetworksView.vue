<script setup lang="ts">
import { computed, ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { daffa, type Network } from '@/lib/api'
import { useSession } from '@/stores/session'
import { confirm } from '@/lib/confirm'
import BaseButton from '@/components/ui/BaseButton.vue'
import EmptyState from '@/components/ui/EmptyState.vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import PruneButton from '@/components/PruneButton.vue'
import SearchInput from '@/components/SearchInput.vue'
import { Cap } from '@/lib/caps'

const session = useSession()
const qc = useQueryClient()
const filter = ref('')

const { data: networks, isLoading } = useQuery({
  queryKey: ['networks', () => session.envId],
  queryFn: () => daffa.networks(session.envId),
  enabled: computed(() => !!session.envId),
})

// Match the attached containers too — a compose network's name is often less memorable
// than the services sitting on it.
const shown = computed(() => {
  const q = filter.value.trim().toLowerCase()
  if (!q) return networks.value ?? []
  return (networks.value ?? []).filter(
    (n) =>
      n.name.toLowerCase().includes(q) ||
      n.driver.toLowerCase().includes(q) ||
      (n.containers ?? []).some((c) => c.toLowerCase().includes(q)),
  )
})

const remove = useMutation({
  mutationFn: (id: string) => daffa.removeNetwork(session.envId, id),
  onSettled: () => qc.invalidateQueries({ queryKey: ['networks'] }),
})

// bridge/host/none are Docker's own — system networks. The server refuses to touch them
// (400 system_network), so no button is offered: one that always fails is worse than none.
function removable(n: Network): boolean {
  return !n.system && !n.containers?.length && session.can(Cap.NetworksEdit)
}

async function onRemove(n: Network) {
  const ok = await confirm({
    title: `Remove ${n.name}?`,
    body: 'Nothing is attached to it, so nothing loses connectivity today. The network is gone for good — anything that expects it will fail to start until it is recreated.',
    confirmLabel: 'Remove',
    intent: 'danger',
  })
  if (!ok) return

  remove.mutate(n.id)
}
</script>

<template>
  <div>
    <PageHeader
      title="Networks"
      :count="networks ? (filter ? `${shown.length} of ${networks.length}` : networks.length) : undefined"
    >
      <template #actions>
        <SearchInput
          v-if="networks?.length"
          v-model="filter"
          placeholder="Name, driver, or container…"
          class="w-64"
        />
        <PruneButton target="networks" label="Prune unused" />
      </template>
    </PageHeader>

    <p v-if="isLoading" class="muted text-sm">Loading…</p>

    <EmptyState
      v-else-if="!networks?.length"
      icon="network"
      title="No networks on this host"
      body="A network is what lets containers find each other by name. Compose makes one per stack; bridge, host and none are built into Docker and are always here."
    />

    <p v-else-if="!shown.length" class="muted text-sm">No networks match “{{ filter }}”.</p>

    <div v-else class="surface overflow-hidden rounded-[var(--radius-card)]">
      <table class="w-full text-sm">
        <thead>
          <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
            <th class="eyebrow px-4 py-2 text-left font-medium">Network</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Kind</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">Attached</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">
              <span class="sr-only">Actions</span>
            </th>
          </tr>
        </thead>

        <tbody>
          <tr
            v-for="n in shown"
            :key="n.id"
            class="border-b transition last:border-0 hover:bg-[var(--surface-sunken)]"
            :style="{ borderColor: 'var(--border)' }"
          >
            <td class="py-3 pl-4 pr-4">
              <div class="font-medium">{{ n.name }}</div>
              <div class="subtle mt-0.5 font-mono text-xs">
                {{ n.driver }}<span v-if="n.internal"> · internal</span>
              </div>
            </td>

            <td class="muted py-3 pr-4 text-xs">
              <span
                v-if="n.system"
                class="inline-block rounded-md border px-1.5 py-0.5 font-mono text-[10px]"
                :style="{ borderColor: 'var(--border)' }"
                title="Docker's own network — it exists on every daemon and cannot be changed or removed."
              >
                system
              </span>
              <span v-else-if="n.containers?.length">in use</span>
              <span v-else class="subtle">unused</span>
            </td>

            <!-- The count, not the sentence. It is the number you compare down the column. -->
            <td class="muted py-3 pr-4 text-right font-mono text-xs">
              {{ n.containers?.length ?? 0 }}
            </td>

            <td class="py-3 pr-4 text-right">
              <BaseButton
                v-if="removable(n)"
                intent="danger"
                size="xs"
                :loading="remove.isPending.value && remove.variables.value === n.id"
                :disabled="remove.isPending.value"
                @click="onRemove(n)"
              >
                Remove
              </BaseButton>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
