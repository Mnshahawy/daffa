<script setup lang="ts">
import { computed, ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import {
  daffa,
  type Keyring,
  type KeyringDelivery,
  type KeyringVersion,
} from '@/lib/api'
import { Cap } from '@/lib/caps'
import { useSession } from '@/stores/session'
import { confirm } from '@/lib/confirm'
import { toast } from '@/lib/toast'
import type { Status } from '@/lib/status'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import EmptyState from '@/components/ui/EmptyState.vue'
import Select from '@/components/ui/Select.vue'
import StatusPill from '@/components/ui/StatusPill.vue'

const session = useSession()
const qc = useQueryClient()
const canEdit = computed(() => session.can(Cap.KeyringsEdit))

const { data: keyrings } = useQuery({ queryKey: ['keyrings'], queryFn: daffa.keyrings })
const { data: deliveries } = useQuery({
  queryKey: ['keyring-deliveries'],
  queryFn: daffa.keyringDeliveries,
})
const { data: envs } = useQuery({ queryKey: ['environments'], queryFn: daffa.environments })

function refresh() {
  qc.invalidateQueries({ queryKey: ['keyrings'] })
  qc.invalidateQueries({ queryKey: ['keyring-deliveries'] })
}

function age(createdAt: string): string {
  const d = Math.floor((Date.now() - new Date(createdAt).getTime()) / 86_400_000)
  if (d <= 0) return 'today'
  return `${d}d old`
}

function activeVersion(k: Keyring): KeyringVersion | undefined {
  return k.versions.find((v) => v.state === 'active')
}

// ── keyrings ────────────────────────────────────────────────────────────────────

const adding = ref(false)
const form = ref({ name: '', rotate_days: 0 })

const createKeyring = useMutation({
  mutationFn: () => daffa.createKeyring({ ...form.value }),
  onSuccess: () => {
    toast.ok('Keyring created.')
    form.value = { name: '', rotate_days: 0 }
    adding.value = false
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not create the keyring.'),
})

function keyringStatus(k: Keyring): Status {
  const active = activeVersion(k)
  if (!active) return { tone: 'danger', label: 'No active version' }
  if (k.rotate_days > 0) {
    const daysOld = (Date.now() - new Date(active.created_at).getTime()) / 86_400_000
    if (daysOld >= k.rotate_days)
      return { tone: 'warn', label: 'Rotation due', detail: 'the worker retries hourly' }
    return { tone: 'success', label: `Rotates every ${k.rotate_days}d` }
  }
  return { tone: 'neutral', label: 'Manual rotation' }
}

const rotate = useMutation({
  mutationFn: (id: string) => daffa.rotateKeyring(id),
  onSuccess: () => {
    toast.ok('Keyring rotated.')
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not rotate the keyring.'),
})

async function onRotate(k: Keyring) {
  const ok = await confirm({
    title: `Rotate ${k.name}?`,
    body: 'A new version becomes current; consumers pick it up on their next read (or restart, if the delivery lists restart targets). Every prior version stays delivered, so old data stays readable. Rotation protects what is written from now on — it does not re-encrypt anything.',
    confirmLabel: 'Rotate',
    intent: 'caution',
  })
  if (ok) rotate.mutate(k.id)
}

const retire = useMutation({
  mutationFn: (p: { id: string; vid: string }) => daffa.retireKeyringVersion(p.id, p.vid),
  onSuccess: () => {
    toast.ok('Version retired.')
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not retire the version.'),
})

async function onRetire(k: Keyring, v: KeyringVersion) {
  const ok = await confirm({
    title: `Retire version ${v.id}?`,
    body: 'It is removed from every delivered volume on the next sync. Anything still encrypted under this version becomes UNREADABLE to every consumer — retire a version only after the application has re-encrypted or no longer needs that data.',
    confirmLabel: 'Retire version',
    intent: 'danger',
    typeToConfirm: k.name,
  })
  if (ok) retire.mutate({ id: k.id, vid: v.id })
}

const updateSchedule = useMutation({
  mutationFn: (p: { id: string; rotate_days: number }) =>
    daffa.updateKeyring(p.id, { rotate_days: p.rotate_days }),
  onSuccess: () => {
    toast.ok('Schedule updated.')
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not update the schedule.'),
})

const removeKeyring = useMutation({
  mutationFn: (id: string) => daffa.deleteKeyring(id),
  onSuccess: () => {
    toast.ok('Keyring deleted.')
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not delete the keyring.'),
})

async function onRemoveKeyring(k: Keyring) {
  const carried = (deliveries.value ?? []).filter((d) => d.keyring_id === k.id).length
  if (carried > 0) {
    toast.err(
      null,
      `${k.name} is carried by ${carried} deliver${carried === 1 ? 'y' : 'ies'}. Delete them first.`,
    )
    return
  }
  const ok = await confirm({
    title: `Delete the keyring ${k.name}?`,
    body: 'Every version is destroyed with it — the sealed rows here are the ONLY durable copy of the material. Files already delivered to volumes are left in place, but they will never rotate again, and once those volumes are gone the keys are gone with them.',
    confirmLabel: 'Delete',
    intent: 'danger',
    typeToConfirm: k.name,
  })
  if (ok) removeKeyring.mutate(k.id)
}

/** Which keyring's version timeline is unfolded. */
const openTimeline = ref<string | null>(null)

// ── deliveries ──────────────────────────────────────────────────────────────────

const addingDelivery = ref(false)
const deliveryBlank = () => ({
  keyring_id: '',
  env_id: '',
  volume: 'daffa-keys',
  restart_targets: '',
})
const deliveryForm = ref(deliveryBlank())

const createDelivery = useMutation({
  mutationFn: () => daffa.createKeyringDelivery({ ...deliveryForm.value }),
  onSuccess: () => {
    toast.ok('Delivery created.')
    deliveryForm.value = deliveryBlank()
    addingDelivery.value = false
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not create the delivery.'),
})

function deliveryStatus(d: KeyringDelivery): Status {
  switch (d.status) {
    case 'ok':
      return { tone: 'success', label: 'Synced' }
    case 'error':
      return { tone: 'danger', label: 'Failed' }
    default:
      return { tone: 'accent', label: 'Pending', live: true }
  }
}

const syncDelivery = useMutation({
  mutationFn: (id: string) => daffa.syncKeyringDelivery(id),
  onSuccess: () => toast.ok('Delivery synced.'),
  onSettled: refresh,
  onError: (e) => toast.err(e, 'Sync failed.'),
})

const removeDelivery = useMutation({
  mutationFn: (id: string) => daffa.deleteKeyringDelivery(id),
  onSuccess: () => {
    toast.ok('Delivery deleted.')
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not delete the delivery.'),
})

async function onRemoveDelivery(d: KeyringDelivery) {
  const ok = await confirm({
    title: `Stop delivering to ${d.volume} on ${d.env_name || d.env_id}?`,
    body: 'The volume and the files already in it are left in place — the application may be encrypting with them right now. They just stop rotating. Remove the volume yourself once nothing mounts it.',
    confirmLabel: 'Delete delivery',
    intent: 'danger',
  })
  if (ok) removeDelivery.mutate(d.id)
}
</script>

<template>
  <div class="space-y-10">
    <!-- ── keyrings ────────────────────────────────────────────────────────── -->
    <section>
      <div class="mb-4 flex flex-wrap items-center gap-x-3 gap-y-2">
        <div class="min-w-0">
          <h2 class="text-base font-semibold">Keyrings</h2>
          <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
            Rotatable encryption keys for your applications. A keyring is a stable name over
            versions: apps encrypt with the current one and keep every prior version readable, so
            rotation never bricks old data. The material is generated here, sealed, and only ever
            leaves as files in a delivered volume — it has no other copy, so keep this database
            and its master key in your backups.
          </p>
        </div>
        <div class="ml-auto">
          <BaseButton
            v-if="canEdit"
            :intent="adding ? 'secondary' : 'primary'"
            size="sm"
            @click="adding = !adding"
          >
            <AppIcon v-if="!adding" name="plus" class="size-3.5" />
            {{ adding ? 'Cancel' : 'Add keyring' }}
          </BaseButton>
        </div>
      </div>

      <form
        v-if="adding"
        class="surface mb-5 rounded-[var(--radius-card)] p-5"
        @submit.prevent="createKeyring.mutate()"
      >
        <div class="grid gap-4 sm:grid-cols-3">
          <div>
            <label for="kr-name" class="mb-1.5 block text-sm font-medium">Name</label>
            <input id="kr-name" v-model="form.name" required placeholder="orders-db" class="field" data-cursor="text" />
            <p class="subtle mt-1 text-xs">
              Becomes the filenames in the volume: {{ form.name || 'name' }}.json / .current.key.
              Not renameable later.
            </p>
          </div>
          <div>
            <label for="kr-rotate" class="mb-1.5 block text-sm font-medium">Rotate every (days)</label>
            <input id="kr-rotate" v-model.number="form.rotate_days" type="number" min="0" class="field" data-cursor="text" />
            <p class="subtle mt-1 text-xs">
              0 = manual only. Schedule it once your app re-reads the file (or is listed as a
              restart target).
            </p>
          </div>
        </div>
        <BaseButton type="submit" intent="primary" size="md" class="mt-4" :loading="createKeyring.isPending.value">
          Create keyring
        </BaseButton>
        <p class="subtle mt-2 text-xs">
          Version 1 is generated immediately. You never see the material — it goes sealed into the
          database and plaintext only into delivered volumes.
        </p>
      </form>

      <EmptyState
        v-if="!keyrings?.length && !adding"
        icon="key"
        title="No keyrings yet"
        body="A keyring gives an application a data-encryption key with a real rotation story: versioned material delivered into a volume, where the app encrypts with the current version and decrypts by the version id it stored beside each ciphertext."
      >
        <template #action>
          <BaseButton v-if="canEdit" intent="primary" size="md" @click="adding = true">
            <AppIcon name="plus" class="size-4" />
            Add keyring
          </BaseButton>
        </template>
      </EmptyState>

      <div v-else-if="keyrings?.length" class="surface overflow-x-auto rounded-[var(--radius-card)]">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
              <th class="eyebrow px-4 py-2 text-left font-medium">Keyring</th>
              <th class="eyebrow py-2 pr-3 text-left font-medium">Rotation</th>
              <th class="eyebrow hidden py-2 pr-3 text-right font-medium md:table-cell">Current version</th>
              <th class="eyebrow py-2 pr-3 text-right font-medium">Versions</th>
              <th class="eyebrow py-2 pr-4 text-right font-medium">Actions</th>
            </tr>
          </thead>
          <tbody>
            <template v-for="k in keyrings" :key="k.id">
              <tr class="border-b last:border-0" :style="{ borderColor: 'var(--border)' }">
                <td class="max-w-0 py-3 pl-4 pr-3">
                  <div class="font-medium">{{ k.name }}</div>
                  <div class="subtle mt-0.5 truncate font-mono text-xs">
                    {{ k.name }}.json · {{ k.name }}.current.key
                  </div>
                </td>
                <td class="py-3 pr-3"><StatusPill :status="keyringStatus(k)" /></td>
                <td class="subtle hidden py-3 pr-3 text-right font-mono text-xs md:table-cell">
                  <template v-if="activeVersion(k)">
                    {{ activeVersion(k)!.id }}
                    <span class="ml-1">({{ age(activeVersion(k)!.created_at) }})</span>
                  </template>
                  <span v-else>—</span>
                </td>
                <td class="subtle py-3 pr-3 text-right font-mono text-xs">
                  <button
                    class="underline decoration-dotted underline-offset-2"
                    @click="openTimeline = openTimeline === k.id ? null : k.id"
                  >
                    {{ k.versions.length }}
                  </button>
                </td>
                <td class="py-3 pr-4 text-right">
                  <div v-if="canEdit" class="flex items-center justify-end gap-1">
                    <BaseButton intent="secondary" size="xs" :disabled="rotate.isPending.value" @click="onRotate(k)">
                      <AppIcon name="restart" class="size-3" />
                      Rotate
                    </BaseButton>
                    <BaseButton intent="danger" size="xs" :disabled="removeKeyring.isPending.value" @click="onRemoveKeyring(k)">
                      <AppIcon name="trash" class="size-3.5" />
                    </BaseButton>
                  </div>
                </td>
              </tr>

              <!-- The version timeline: the kid an app stored is findable here forever. -->
              <tr v-if="openTimeline === k.id" class="border-b last:border-0" :style="{ borderColor: 'var(--border)' }">
                <td colspan="5" class="px-4 pb-4 pt-1" :style="{ background: 'var(--surface-sunken)' }">
                  <div class="space-y-1.5 pt-3">
                    <div
                      v-for="v in k.versions"
                      :key="v.id"
                      class="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs"
                    >
                      <code class="font-mono">{{ v.id }}</code>
                      <StatusPill
                        :status="
                          v.state === 'active'
                            ? { tone: 'success', label: 'Active' }
                            : v.state === 'decrypt_only'
                              ? { tone: 'accent', label: 'Decrypt only' }
                              : { tone: 'neutral', label: 'Retired' }
                        "
                      />
                      <span class="subtle">created {{ new Date(v.created_at).toLocaleDateString() }}</span>
                      <BaseButton
                        v-if="canEdit && v.state === 'decrypt_only'"
                        intent="danger"
                        size="xs"
                        :disabled="retire.isPending.value"
                        @click="onRetire(k, v)"
                      >
                        Retire
                      </BaseButton>
                    </div>
                    <div v-if="canEdit" class="flex items-center gap-2 pt-2 text-xs">
                      <label :for="`kr-sched-${k.id}`" class="subtle">Rotate every</label>
                      <input
                        :id="`kr-sched-${k.id}`"
                        :value="k.rotate_days"
                        type="number"
                        min="0"
                        class="field w-20 py-1 text-xs"
                        data-cursor="text"
                        @change="
                          updateSchedule.mutate({
                            id: k.id,
                            rotate_days: Number(($event.target as HTMLInputElement).value),
                          })
                        "
                      />
                      <span class="subtle">days (0 = manual)</span>
                    </div>
                  </div>
                </td>
              </tr>
            </template>
          </tbody>
        </table>
      </div>
    </section>

    <!-- ── deliveries ──────────────────────────────────────────────────────── -->
    <section>
      <div class="mb-4 flex flex-wrap items-center gap-x-3 gap-y-2">
        <div class="min-w-0">
          <h2 class="text-base font-semibold">Deliveries</h2>
          <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
            A delivery keeps a keyring current inside a named volume on a host — mount it
            read-only wherever you like; the files carry no paths. The app contract: encrypt with
            <code class="font-mono text-xs">keys[current]</code> from the
            <code class="font-mono text-xs">.json</code> and store that version id beside the
            ciphertext; decrypt by looking the stored id back up; re-read the file on a cadence
            that suits your rotation schedule, or list the container as a restart target.
          </p>
        </div>
        <div class="ml-auto">
          <BaseButton v-if="canEdit" :intent="addingDelivery ? 'secondary' : 'primary'" size="sm" @click="addingDelivery = !addingDelivery">
            <AppIcon v-if="!addingDelivery" name="plus" class="size-3.5" />
            {{ addingDelivery ? 'Cancel' : 'Add delivery' }}
          </BaseButton>
        </div>
      </div>

      <form v-if="addingDelivery" class="surface mb-5 rounded-[var(--radius-card)] p-5" @submit.prevent="createDelivery.mutate()">
        <div class="grid gap-4 sm:grid-cols-4">
          <div>
            <label for="kdl-keyring" class="mb-1.5 block text-sm font-medium">Keyring</label>
            <Select id="kdl-keyring" v-model="deliveryForm.keyring_id" required>
              <option value="" disabled>Choose a keyring…</option>
              <option v-for="k in keyrings" :key="k.id" :value="k.id">{{ k.name }}</option>
            </Select>
          </div>
          <div>
            <label for="kdl-env" class="mb-1.5 block text-sm font-medium">Host</label>
            <Select id="kdl-env" v-model="deliveryForm.env_id" required>
              <option value="" disabled>Choose a cluster…</option>
              <option v-for="e in envs" :key="e.id" :value="e.id">{{ e.name }}</option>
            </Select>
          </div>
          <div>
            <label for="kdl-volume" class="mb-1.5 block text-sm font-medium">Volume</label>
            <input id="kdl-volume" v-model="deliveryForm.volume" required class="field font-mono text-xs" data-cursor="text" />
          </div>
          <div>
            <label for="kdl-restart" class="mb-1.5 block text-sm font-medium">Restart after sync</label>
            <input id="kdl-restart" v-model="deliveryForm.restart_targets" placeholder="container names (optional)" class="field font-mono text-xs" data-cursor="text" />
            <p class="subtle mt-1 text-xs">Only for apps that read the key once at boot and never again.</p>
          </div>
        </div>
        <BaseButton type="submit" intent="primary" size="md" class="mt-4" :loading="createDelivery.isPending.value">
          Create delivery
        </BaseButton>
      </form>

      <div v-if="deliveries?.length" class="surface overflow-x-auto rounded-[var(--radius-card)]">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
              <th class="eyebrow px-4 py-2 text-left font-medium">Delivery</th>
              <th class="eyebrow py-2 pr-3 text-left font-medium">Status</th>
              <th class="eyebrow hidden py-2 pr-3 text-right font-medium md:table-cell">Last synced</th>
              <th class="eyebrow py-2 pr-4 text-right font-medium">Actions</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="d in deliveries" :key="d.id" class="border-b last:border-0" :style="{ borderColor: 'var(--border)' }">
              <td class="max-w-0 py-3 pl-4 pr-3">
                <div class="font-medium">
                  {{ d.keyring_name || d.keyring_id }}
                  <span class="subtle">→ {{ d.volume }} on {{ d.env_name || d.env_id }}</span>
                </div>
                <div v-if="d.last_error" class="mt-0.5 truncate text-xs" :style="{ color: 'var(--danger)' }" :title="d.last_error">{{ d.last_error }}</div>
              </td>
              <td class="py-3 pr-3"><StatusPill :status="deliveryStatus(d)" /></td>
              <td class="subtle hidden py-3 pr-3 text-right font-mono text-xs md:table-cell">
                <time v-if="d.synced_at" :title="d.synced_at">{{ new Date(d.synced_at).toLocaleString() }}</time>
                <span v-else>never</span>
              </td>
              <td class="py-3 pr-4 text-right">
                <div v-if="canEdit" class="flex items-center justify-end gap-1">
                  <BaseButton intent="secondary" size="xs" :disabled="syncDelivery.isPending.value" @click="syncDelivery.mutate(d.id)">
                    <AppIcon name="restart" class="size-3" />
                    Sync now
                  </BaseButton>
                  <BaseButton intent="danger" size="xs" :disabled="removeDelivery.isPending.value" @click="onRemoveDelivery(d)">
                    <AppIcon name="trash" class="size-3.5" />
                  </BaseButton>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <p v-else-if="!addingDelivery" class="muted text-sm">No deliveries yet.</p>
    </section>
  </div>
</template>
