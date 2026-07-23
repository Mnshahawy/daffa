<script setup lang="ts">
import { computed, ref } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import { daffa, type Service } from '@/lib/api'
import { useSession } from '@/stores/session'
import { serviceStatus } from '@/lib/status'
import PageHeader from '@/components/ui/PageHeader.vue'
import SearchInput from '@/components/SearchInput.vue'
import EmptyState from '@/components/ui/EmptyState.vue'
import StatusPill from '@/components/ui/StatusPill.vue'

const session = useSession()
const filter = ref('')

const { data: services, isLoading } = useQuery({
  queryKey: ['services', () => session.envId],
  queryFn: () => daffa.services(session.envId),
  enabled: computed(() => !!session.envId),
})

const shown = computed(() => {
  const q = filter.value.trim().toLowerCase()
  const list = services.value ?? []
  if (!q) return list
  return list.filter(
    (s) =>
      s.name.toLowerCase().includes(q) ||
      s.tag.toLowerCase().includes(q) ||
      (s.stack ?? '').toLowerCase().includes(q),
  )
})

/** Replicas, in the unit the service actually has. A global service has no replica count. */
function replicas(s: Service): string {
  return `${s.running}/${s.desired}`
}

/**
 * Swarm records how the LAST update ended, and this is the one place anybody would see it.
 *
 * `rollback_completed` means swarm gave up on a deploy and put the old spec back — so the deploy
 * reported success, and then quietly undid itself. That is precisely the gap between "the command
 * worked" and "the thing is running", and neither Portainer nor Dokploy shows it.
 */
function rolledBack(s: Service): boolean {
  return s.update_state === 'rollback_completed' || s.update_state === 'rollback_started'
}
</script>

<template>
  <div>
    <PageHeader
      title="Services"
      :count="services ? (filter ? `${shown.length} of ${services.length}` : services.length) : undefined"
      description="What the Swarm is running. A service is a desired state; its tasks are the attempts to reach it."
    >
      <template #actions>
        <SearchInput
          v-if="services?.length"
          v-model="filter"
          placeholder="Name, image, or stack…"
          class="w-full sm:w-64"
        />
      </template>
    </PageHeader>

    <p v-if="isLoading" class="muted text-sm">Loading…</p>

    <EmptyState
      v-else-if="!services?.length"
      icon="layers"
      title="No services on this cluster"
      body="A service is what a Swarm runs: a desired number of copies of one image, placed on whatever nodes can take them. Deploy a stack to create some."
    />

    <p v-else-if="!shown.length" class="muted text-sm">No services match “{{ filter }}”.</p>

    <div v-else class="surface overflow-x-auto rounded-[var(--radius-card)]">
      <table class="w-full text-sm">
        <thead>
          <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
            <th class="eyebrow px-4 py-2 text-left font-medium">Service</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">State</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">Replicas</th>
            <th class="eyebrow hidden py-2 pr-4 text-left font-medium md:table-cell">Ports</th>
          </tr>
        </thead>

        <tbody>
          <RouterLink
            v-for="s in shown"
            :key="s.id"
            v-slot="{ navigate }"
            :to="{ name: 'service', params: { id: s.id } }"
            custom
          >
            <tr
              class="cursor-pointer border-b transition last:border-0 hover:bg-[var(--surface-sunken)]"
              :style="{ borderColor: 'var(--border)' }"
              @click="navigate"
            >
              <td class="py-3 pl-4 pr-4">
                <div class="font-medium">{{ s.name }}</div>
                <div class="subtle mt-0.5 break-all font-mono text-xs">
                  {{ s.tag }}
                  <span v-if="s.stack"> · {{ s.stack }}</span>
                  <span v-if="s.mode === 'global'"> · global</span>
                </div>
              </td>

              <td class="py-3 pr-4">
                <StatusPill :status="serviceStatus(s)" />
                <!-- The deploy that succeeded and then undid itself. -->
                <div v-if="rolledBack(s)" class="mt-1 text-xs" :style="{ color: 'var(--warn)' }">
                  Last update rolled back
                </div>
              </td>

              <td class="muted py-3 pr-4 text-right font-mono text-xs">
                {{ replicas(s) }}
                <span class="subtle">{{ s.mode === 'global' ? ' nodes' : '' }}</span>
              </td>

              <td class="muted hidden py-3 pr-4 font-mono text-xs md:table-cell">
                <span v-if="s.ports?.length">{{ s.ports.join(', ') }}</span>
                <span v-else class="subtle">—</span>
              </td>
            </tr>
          </RouterLink>
        </tbody>
      </table>
    </div>
  </div>
</template>
