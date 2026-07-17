<script setup lang="ts">
import { computed, ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { ApiError, bytes, daffa, type Volume } from '@/lib/api'
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

const { data: volumes, isLoading } = useQuery({
  queryKey: ['volumes', () => session.envId],
  queryFn: () => daffa.volumes(session.envId),
  enabled: computed(() => !!session.envId),
})

// The attachments that declare what a volume IS: sourced from git (disposable copies of a
// repo) or backed up (someone said the contents are precious). Fetched only for someone
// who could see them — otherwise this is a guaranteed 403 on every visit to the page.
const { data: sources } = useQuery({
  queryKey: ['volume-sources'],
  queryFn: daffa.volumeSources,
  enabled: computed(() => session.can(Cap.VolsourcesView)),
})
const { data: backupJobs } = useQuery({
  queryKey: ['backups'],
  queryFn: daffa.backups,
  enabled: computed(() => session.can(Cap.BackupsView)),
})

const sourced = computed(() => {
  const set = new Set<string>()
  for (const s of sources.value ?? []) if (s.env_id === session.envId) set.add(s.volume)
  return set
})

// Only ENABLED jobs count: a paused backup looks exactly like a working one on the morning
// you need it, and this badge must not help it hide.
const backedUp = computed(() => {
  const set = new Set<string>()
  for (const j of backupJobs.value ?? []) {
    if (j.enabled && j.engine === 'volume' && j.env_id === session.envId && j.volume) {
      set.add(j.volume)
    }
  }
  return set
})

// Also match the containers that mount it: "which volumes does this stack use?" is the
// question, and the answer is not in the volume's own (often hash-shaped) name.
const shown = computed(() => {
  const q = filter.value.trim().toLowerCase()
  if (!q) return volumes.value ?? []
  return (volumes.value ?? []).filter(
    (v) =>
      v.name.toLowerCase().includes(q) ||
      (v.used_by ?? []).some((c) => c.toLowerCase().includes(q)),
  )
})

// The refusal has to reach the person: a volume a source or an enabled backup job
// references is refused server-side, and the error names what to detach first.
const removeError = ref('')
const remove = useMutation({
  mutationFn: (name: string) => daffa.removeVolume(session.envId, name),
  onSuccess: () => {
    removeError.value = ''
  },
  onError: (e) => {
    removeError.value = e instanceof ApiError ? e.message : 'Could not remove the volume.'
  },
  onSettled: () => qc.invalidateQueries({ queryKey: ['volumes'] }),
})

const orphaned = computed(() => (volumes.value ?? []).filter((v) => !v.used_by?.length).length)

async function onRemove(v: Volume) {
  // A volume is DATA. Deleting one is not like deleting a container, and the dialog
  // should not pretend otherwise — so it names the volume and demands it back.
  const users = v.used_by?.length
    ? ` It is currently mounted by ${v.used_by.join(', ')}.`
    : ''
  const ok = await confirm({
    title: `Remove ${v.name}?`,
    body: `Deleting this volume destroys its data permanently. This cannot be undone.${users}`,
    confirmLabel: 'Remove',
    intent: 'danger',
    typeToConfirm: v.name,
  })
  if (!ok) return

  remove.mutate(v.name)
}
</script>

<template>
  <div>
    <PageHeader
      title="Volumes"
      :count="volumes ? (filter ? `${shown.length} of ${volumes.length}` : volumes.length) : undefined"
      :description="volumes?.length ? `${orphaned} of these are mounted by nothing.` : undefined"
    >
      <template #actions>
        <SearchInput
          v-if="volumes?.length"
          v-model="filter"
          placeholder="Name or container…"
          class="w-64"
        />
        <PruneButton target="volumes" label="Prune anonymous" />
      </template>
    </PageHeader>

    <p
      v-if="removeError"
      role="alert"
      class="mb-4 rounded-[var(--radius-control)] px-3 py-2 text-sm"
      :style="{ background: 'var(--danger-soft)', color: 'var(--danger)' }"
    >
      {{ removeError }}
    </p>

    <p v-if="isLoading" class="muted text-sm">Loading…</p>

    <EmptyState
      v-else-if="!volumes?.length"
      icon="database"
      title="No volumes on this host"
      body="A volume is where a container's data outlives the container. Compose creates them on the first deploy; nothing here is deleted when a stack comes down."
    />

    <p v-else-if="!shown.length" class="muted text-sm">No volumes match “{{ filter }}”.</p>

    <div v-else class="surface overflow-hidden rounded-[var(--radius-card)]">
      <table class="w-full text-sm">
        <thead>
          <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
            <th class="eyebrow px-4 py-2 text-left font-medium">Volume</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Mounted by</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">Size</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">
              <span class="sr-only">Actions</span>
            </th>
          </tr>
        </thead>

        <tbody>
          <tr
            v-for="v in shown"
            :key="v.name"
            class="border-b transition last:border-0 hover:bg-[var(--surface-sunken)]"
            :style="{ borderColor: 'var(--border)' }"
          >
            <td class="py-3 pl-4 pr-4">
              <div class="flex flex-wrap items-center gap-2">
                <span class="break-all font-medium">{{ v.name }}</span>
                <!-- The declared attachments, so the list shows each volume for what it is.
                     Declared, never inferred: nothing here guesses from the name. -->
                <span
                  v-if="sourced.has(v.name)"
                  class="rounded-md px-1.5 py-0.5 text-xs font-medium"
                  :style="{ background: 'var(--accent-soft)', color: 'var(--accent-text)' }"
                  title="A volume source keeps this volume in sync from git — its contents are disposable copies of the repo"
                >
                  sourced
                </span>
                <span
                  v-if="backedUp.has(v.name)"
                  class="rounded-md px-1.5 py-0.5 text-xs font-medium"
                  :style="{ background: 'var(--success-soft)', color: 'var(--success)' }"
                  title="An enabled backup job snapshots this volume to object storage"
                >
                  backed up
                </span>
              </div>
              <div class="subtle mt-0.5 font-mono text-xs">{{ v.driver }}</div>
            </td>

            <td class="py-3 pr-4">
              <span v-if="v.used_by?.length" class="muted text-xs">
                {{ v.used_by.join(', ') }}
              </span>
              <!-- Unused is amber, not red: an orphaned volume is very often the last copy of
                   something, and the point of flagging it is "look at this", not "delete it". -->
              <span
                v-else
                class="rounded-md px-1.5 py-0.5 text-xs font-medium"
                :style="{ background: 'var(--warn-soft)', color: 'var(--warn)' }"
              >
                unused
              </span>
            </td>

            <td class="muted py-3 pr-4 text-right font-mono text-xs">
              {{ v.size >= 0 ? bytes(v.size) : '—' }}
            </td>

            <td class="py-3 pr-4 text-right">
              <BaseButton
                v-if="session.can(Cap.VolumesEdit)"
                intent="danger"
                size="xs"
                :loading="remove.isPending.value && remove.variables.value === v.name"
                :disabled="remove.isPending.value"
                @click="onRemove(v)"
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
