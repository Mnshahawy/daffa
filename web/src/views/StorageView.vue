<script setup lang="ts">
import { ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { ApiError, daffa, type StorageTarget } from '@/lib/api'
import { confirm } from '@/lib/confirm'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import EmptyState from '@/components/ui/EmptyState.vue'

const qc = useQueryClient()
const error = ref('')
const adding = ref(false)

const { data: targets, isLoading } = useQuery({
  queryKey: ['storage'],
  queryFn: daffa.storage,
})

const blank = () => ({ name: '', endpoint: '', region: 'auto', bucket: '', key_id: '', secret: '' })
const form = ref(blank())

const create = useMutation({
  mutationFn: () => daffa.createStorage(form.value),
  onSuccess: () => {
    form.value = blank()
    adding.value = false
    error.value = ''
    qc.invalidateQueries({ queryKey: ['storage'] })
  },
  onError: (e) => {
    error.value = e instanceof ApiError ? e.message : 'Could not save the target.'
  },
})

const remove = useMutation({
  mutationFn: (id: string) => daffa.deleteStorage(id),
  onSettled: () => qc.invalidateQueries({ queryKey: ['storage'] }),
  onError: (e) => {
    error.value = e instanceof ApiError ? e.message : 'Could not delete the target.'
  },
})

async function onRemove(t: StorageTarget) {
  // The server refuses anyway if jobs depend on it; saying so here saves a round trip
  // and an error message that arrives after the click.
  if (t.in_use > 0) {
    error.value = `${t.name} is used by ${t.in_use} backup job${t.in_use === 1 ? '' : 's'}. Point them elsewhere first.`
    return
  }
  const ok = await confirm({
    title: `Delete the storage target ${t.name}?`,
    body: 'The snapshots already in the bucket are NOT deleted — Daffa never touches them, and they stay exactly where they are. What goes is Daffa\'s way in: the endpoint, the access key and the secret, which is encrypted at rest and cannot be read back. You would have to enter it again from scratch.',
    confirmLabel: 'Delete',
    intent: 'danger',
    typeToConfirm: t.name,
  })
  if (!ok) return
  remove.mutate(t.id)
}
</script>

<template>
  <div>
    <div class="mb-5 flex flex-wrap items-center gap-x-3 gap-y-2">
      <div class="min-w-0">
        <h2 class="text-base font-semibold">Storage targets</h2>
        <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
          A bucket that backup jobs write to — Cloudflare R2, Backblaze B2, MinIO, AWS S3, anything
          that speaks the protocol. Configure it once and point as many jobs at it as you like.
        </p>
      </div>

      <div class="ml-auto">
        <BaseButton :intent="adding ? 'secondary' : 'primary'" @click="adding = !adding">
          <AppIcon v-if="!adding" name="plus" class="size-4" />
          {{ adding ? 'Cancel' : 'Add target' }}
        </BaseButton>
      </div>
    </div>

    <form
      v-if="adding"
      class="surface mb-6 rounded-[var(--radius-card)] p-5"
      @submit.prevent="create.mutate()"
    >
      <div class="grid gap-4 sm:grid-cols-2">
        <div>
          <label for="st-name" class="mb-1.5 block text-sm font-medium">Name</label>
          <input
            id="st-name"
            v-model="form.name"
            required
            placeholder="r2-backups"
            class="field"
            data-cursor="text"
          />
        </div>
        <div>
          <label for="st-bucket" class="mb-1.5 block text-sm font-medium">Bucket</label>
          <input
            id="st-bucket"
            v-model="form.bucket"
            required
            placeholder="backups"
            class="field font-mono text-xs"
            data-cursor="text"
          />
        </div>
        <div class="sm:col-span-2">
          <label for="st-endpoint" class="mb-1.5 block text-sm font-medium">Endpoint</label>
          <input
            id="st-endpoint"
            v-model="form.endpoint"
            required
            placeholder="https://<account>.r2.cloudflarestorage.com"
            class="field font-mono text-xs"
            data-cursor="text"
          />
        </div>
        <div>
          <label for="st-key" class="mb-1.5 block text-sm font-medium">Access key ID</label>
          <input
            id="st-key"
            v-model="form.key_id"
            required
            class="field font-mono text-xs"
            data-cursor="text"
          />
        </div>
        <div>
          <label for="st-secret" class="mb-1.5 block text-sm font-medium">Secret access key</label>
          <input id="st-secret" v-model="form.secret" required type="password" class="field" />
        </div>
      </div>

      <p v-if="error" class="mt-3 text-sm" :style="{ color: 'var(--danger)' }">{{ error }}</p>

      <BaseButton
        type="submit"
        intent="primary"
        size="md"
        class="mt-4"
        :loading="create.isPending.value"
      >
        {{ create.isPending.value ? 'Testing the connection…' : 'Save target' }}
      </BaseButton>
      <p class="subtle mt-2 text-xs">
        Daffa lists the bucket before saving, so a wrong key is caught now rather than by a backup
        failing at 3am.
      </p>
    </form>

    <p v-if="error && !adding" class="mb-3 text-sm" :style="{ color: 'var(--danger)' }">
      {{ error }}
    </p>

    <p v-if="isLoading" class="muted text-sm">Loading…</p>

    <EmptyState
      v-else-if="!targets?.length"
      icon="database"
      title="No storage targets yet"
      body="A storage target is an S3-compatible bucket — R2, B2, MinIO, S3. It is where backups go: a backup job streams the dump straight into one, so a job cannot be created until there is at least one here."
    >
      <template #action>
        <BaseButton intent="primary" size="md" @click="adding = true">
          <AppIcon name="plus" class="size-4" />
          Add target
        </BaseButton>
      </template>
    </EmptyState>

    <div v-else class="surface overflow-hidden rounded-[var(--radius-card)]">
      <table class="w-full text-sm">
        <thead>
          <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
            <th class="eyebrow px-4 py-2 text-left font-medium">Target</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">In use</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">Actions</th>
          </tr>
        </thead>

        <tbody>
          <tr
            v-for="t in targets"
            :key="t.id"
            class="border-b transition last:border-0 hover:bg-[var(--surface-sunken)]"
            :style="{ borderColor: 'var(--border)' }"
          >
            <td class="py-3 pl-4 pr-4">
              <div class="font-medium">{{ t.name }}</div>
              <div class="subtle mt-0.5 truncate font-mono text-xs">
                {{ t.endpoint }}/{{ t.bucket }}
              </div>
            </td>

            <td class="subtle py-3 pr-4 text-right font-mono text-xs">
              {{ t.in_use }} job{{ t.in_use === 1 ? '' : 's' }}
            </td>

            <td class="py-3 pr-4 text-right">
              <BaseButton
                intent="danger"
                size="xs"
                :disabled="remove.isPending.value"
                @click="onRemove(t)"
              >
                <AppIcon name="trash" class="size-3.5" />
                Delete
              </BaseButton>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
