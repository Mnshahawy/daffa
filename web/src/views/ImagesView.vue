<script setup lang="ts">
import { computed, ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { bytes, daffa, type Image } from '@/lib/api'
import { useSession } from '@/stores/session'
import { confirm } from '@/lib/confirm'
import BaseButton from '@/components/ui/BaseButton.vue'
import EmptyState from '@/components/ui/EmptyState.vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import PruneButton from '@/components/PruneButton.vue'
import SearchInput from '@/components/SearchInput.vue'
import { Cap } from '@/lib/caps'
import { toast } from '@/lib/toast'

const session = useSession()
const qc = useQueryClient()
const filter = ref('')

const { data: images, isLoading } = useQuery({
  queryKey: ['images', () => session.envId],
  queryFn: () => daffa.images(session.envId),
  enabled: computed(() => !!session.envId),
})

// Match on the tag and on the id, because "which one is sha256:3f26…?" is a question you
// ask about an untagged image precisely when you cannot search for it by name.
const shown = computed(() => {
  const q = filter.value.trim().toLowerCase()
  if (!q) return images.value ?? []
  return (images.value ?? []).filter(
    (i) =>
      i.id.toLowerCase().includes(q) ||
      (i.tags ?? []).some((t) => t.toLowerCase().includes(q)) ||
      (i.dangling && '<untagged>'.includes(q)),
  )
})

const remove = useMutation({
  mutationFn: ({ id, force }: { id: string; force: boolean }) =>
    daffa.removeImage(session.envId, id, force),
  onSuccess: () => toast.ok('Image removed.'),
  onError: (e) => toast.err(e, 'Could not remove the image.'),
  onSettled: () => qc.invalidateQueries({ queryKey: ['images'] }),
})

const reclaimable = computed(() =>
  (images.value ?? []).filter((i) => !i.in_use).reduce((sum, i) => sum + i.size, 0),
)

function label(img: Image): string {
  if (img.dangling) return '<untagged>'
  return img.tags?.[0] ?? img.id.slice(7, 19)
}

async function onRemove(img: Image) {
  // An image in use is pinned by a container (running OR stopped). Forcing it is a
  // choice someone should make deliberately, not discover — so the force flag is the
  // checkbox in the dialog, ticked, rather than something the click quietly implies.
  const ok = await confirm({
    title: `Remove ${label(img)}?`,
    body: img.in_use
      ? 'A container is still using this image. Removing it untags the image and deletes its layers; the containers built on it keep running, but nothing can be recreated or rolled back from it without pulling again.'
      : 'The image and its layers go. Anything that needs it again will have to pull it from the registry.',
    confirmLabel: 'Remove',
    intent: 'danger',
    checkbox: img.in_use
      ? {
          label: 'Force removal',
          hint: 'Docker refuses to remove an image a container depends on unless you insist.',
          default: true,
        }
      : undefined,
  })
  if (!ok) return

  remove.mutate({ id: img.id, force: img.in_use && ok.checked })
}
</script>

<template>
  <div>
    <PageHeader
      title="Images"
      :count="images ? (filter ? `${shown.length} of ${images.length}` : images.length) : undefined"
      :description="images ? `${bytes(reclaimable)} of this is not in use by any container.` : undefined"
    >
      <template #actions>
        <SearchInput v-if="images?.length" v-model="filter" placeholder="Tag or id…" class="w-64" />
        <PruneButton target="images" label="Prune dangling" />
      </template>
    </PageHeader>

    <p v-if="isLoading" class="muted text-sm">Loading…</p>

    <EmptyState
      v-else-if="!images?.length"
      icon="archive"
      title="No images on this cluster"
      body="An image is the filesystem a container runs from. They arrive when you pull one or deploy a stack, and they stay on the host until something removes them."
    />

    <p v-else-if="!shown.length" class="muted text-sm">No images match “{{ filter }}”.</p>

    <div v-else class="surface overflow-hidden rounded-[var(--radius-card)]">
      <table class="w-full text-sm">
        <!-- Size is the reason anybody opens this page. It is mono and right-aligned so the
             big ones stand out down the column rather than having to be read one by one. -->
        <thead>
          <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
            <th class="eyebrow px-4 py-2 text-left font-medium">Image</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Usage</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">Size</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">
              <span class="sr-only">Actions</span>
            </th>
          </tr>
        </thead>

        <tbody>
          <tr
            v-for="img in shown"
            :key="img.id"
            class="border-b transition last:border-0 hover:bg-[var(--surface-sunken)]"
            :style="{ borderColor: 'var(--border)' }"
          >
            <td class="py-3 pl-4 pr-4">
              <div class="font-medium" :class="img.dangling ? 'muted italic' : ''">
                {{ label(img) }}
              </div>
              <div class="subtle mt-0.5 font-mono text-xs">{{ img.id.slice(7, 19) }}</div>
            </td>

            <td class="py-3 pr-4">
              <span
                v-if="img.in_use"
                class="rounded-md px-1.5 py-0.5 text-xs font-medium"
                :style="{ background: 'var(--success-soft)', color: 'var(--success)' }"
              >
                in use
              </span>
              <span v-else class="subtle text-xs">unused</span>
            </td>

            <td class="muted py-3 pr-4 text-right font-mono text-xs">{{ bytes(img.size) }}</td>

            <td class="py-3 pr-4 text-right">
              <!-- Removing an image destroys layers. Red, and it is the only red on the row. -->
              <BaseButton
                v-if="session.can(Cap.ImagesEdit)"
                intent="danger"
                size="xs"
                :loading="remove.isPending.value && remove.variables.value?.id === img.id"
                :disabled="remove.isPending.value"
                @click="onRemove(img)"
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
