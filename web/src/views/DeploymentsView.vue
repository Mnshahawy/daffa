<script setup lang="ts">
import { useQuery } from '@tanstack/vue-query'
import { computed, ref } from 'vue'
import { daffa, type DeploymentStatus as Status } from '@/lib/api'
import { ago, absolute, actionLabel, duration, shortSha } from '@/lib/format'
import { deploymentStatus } from '@/lib/status'
import BaseButton from '@/components/ui/BaseButton.vue'
import EmptyState from '@/components/ui/EmptyState.vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import StatusPill from '@/components/ui/StatusPill.vue'

// The cross-stack feed.
//
// It exists because the per-stack history only helps once you already know which stack broke.
// When somebody says "the site is down", you do not — and until now there was nowhere to look
// that would tell you.

const status = ref<Status | ''>('')
const host = ref('')

const { data: hosts } = useQuery({ queryKey: ['environments'], queryFn: daffa.environments })

const { data: deployments, isLoading } = useQuery({
  queryKey: ['deployments', () => status.value, () => host.value],
  queryFn: () =>
    daffa.deployments({
      status: status.value || undefined,
      host: host.value || undefined,
    }),
  // A deploy in the list that is still running is a deploy somebody is probably watching.
  refetchInterval: (q) =>
    q.state.data?.some((d) => d.status === 'running') ? 3000 : 30_000,
})

const hostName = computed(() => {
  const map = new Map((hosts.value ?? []).map((h) => [h.id, h.name]))
  return (id?: string) => (id ? (map.get(id) ?? id) : '')
})

// What started it, in two words, because the column is narrow and "manual" tells you nothing
// that the person's name does not tell you better.
function triggeredBy(d: { trigger_kind: string; started_by_name?: string }): string {
  if (d.trigger_kind === 'webhook') return 'push'
  if (d.trigger_kind === 'rollback') return 'rollback'
  return d.started_by_name || 'manual'
}

const filters: { value: Status | ''; label: string }[] = [
  { value: '', label: 'All' },
  { value: 'failed', label: 'Failed' },
  { value: 'running', label: 'Running' },
  { value: 'ok', label: 'Succeeded' },
  { value: 'cancelled', label: 'Cancelled' },
]
</script>

<template>
  <div>
    <PageHeader
      title="Deployments"
      :count="deployments?.length"
      description="Every attempt to change what is running, on every host — the place to start when somebody says “the site is down” and nobody knows which stack broke."
    >
      <template #actions>
        <!-- Failed first among the filters, because that is the reason anybody opens this page. -->
        <div class="flex items-center gap-1" role="group" aria-label="Filter by outcome">
          <BaseButton
            v-for="f in filters"
            :key="f.value"
            :intent="status === f.value ? 'primary' : 'ghost'"
            :aria-pressed="status === f.value"
            @click="status = f.value"
          >
            {{ f.label }}
          </BaseButton>
        </div>

        <div class="w-40">
          <label for="dep-host" class="sr-only">Host</label>
          <select id="dep-host" v-model="host" class="field py-1.5 text-xs">
            <option value="">Every host</option>
            <option v-for="h in hosts" :key="h.id" :value="h.id">{{ h.name }}</option>
          </select>
        </div>
      </template>
    </PageHeader>

    <p v-if="isLoading" class="muted text-sm">Loading…</p>

    <EmptyState
      v-else-if="!deployments?.length"
      icon="rocket"
      :title="status ? `No ${status === 'ok' ? 'succeeded' : status} deployments` : 'Nothing has been deployed yet'"
      :body="
        status
          ? 'Nothing here matches that filter. Widen it to All to see the rest of the feed.'
          : 'Every deploy Daffa runs — by hand, by webhook, or by rollback — is recorded here with its full output, so a failure that happened at 3am is still readable at 9am. Deploy a stack and the first one shows up.'
      "
    >
      <template #action>
        <BaseButton v-if="!status" intent="primary" size="md" :to="{ name: 'stacks' }">
          Go to stacks
        </BaseButton>
        <BaseButton v-else @click="status = ''">Show all deployments</BaseButton>
      </template>
    </EmptyState>

    <div v-else class="surface overflow-x-auto rounded-[var(--radius-card)]">
      <table class="w-full text-sm">
        <thead>
          <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
            <th class="eyebrow px-4 py-2 text-left font-medium">Outcome</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Stack</th>
            <th class="eyebrow hidden py-2 pr-4 text-left font-medium sm:table-cell">Host</th>
            <th class="eyebrow hidden py-2 pr-4 text-left font-medium md:table-cell">Commit</th>
            <th class="eyebrow hidden py-2 pr-4 text-left font-medium lg:table-cell">Trigger</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">Took</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">Started</th>
          </tr>
        </thead>

        <tbody>
          <tr
            v-for="d in deployments"
            :key="d.id"
            class="border-b transition last:border-0 hover:bg-[var(--surface-sunken)]"
            :style="{ borderColor: 'var(--border)' }"
          >
            <td class="px-4 py-3">
              <StatusPill :status="deploymentStatus(d.status, d.exit_code)" />
            </td>

            <!-- The whole row is the link. A deployment has one URL and this is how you get to it. -->
            <td class="py-3 pr-4">
              <RouterLink
                :to="{ name: 'deployment', params: { id: d.id } }"
                class="font-medium transition hover:text-[var(--accent-text)]"
              >
                {{ d.stack_name || 'a deleted stack' }}
              </RouterLink>
              <span class="muted ml-2 text-xs">{{ actionLabel(d.action) }}</span>
            </td>

            <td class="muted hidden py-3 pr-4 text-xs sm:table-cell">{{ hostName(d.env_id) }}</td>

            <td class="subtle hidden py-3 pr-4 font-mono text-xs md:table-cell">
              <span v-if="d.commit_sha" :title="d.commit_subject">{{ shortSha(d.commit_sha) }}</span>
              <span v-else>—</span>
            </td>

            <td class="muted hidden py-3 pr-4 text-xs lg:table-cell">{{ triggeredBy(d) }}</td>

            <td class="subtle py-3 pr-4 text-right font-mono text-xs">{{ duration(d) || '—' }}</td>

            <td class="py-3 pr-4 text-right">
              <time class="subtle text-xs whitespace-nowrap" :title="absolute(d.started_at)">
                {{ ago(d.started_at) }}
              </time>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
