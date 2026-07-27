<script setup lang="ts">
// Fleet deliveries: the Settings surface for composing certificates from ANY cluster —
// grouped into subdirectories, each with its own trust bundle — into one volume on a
// consumer's cluster. The worked example is Wali: one console, a subdir per platform
// environment, each holding that environment's client cert/key and its roots.
import { computed, ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { daffa, type FleetDelivery, type FleetGroupRequest } from '@/lib/api'
import { Cap } from '@/lib/caps'
import { useSession } from '@/stores/session'
import { confirm, toast, AppIcon, BaseButton, EmptyState, StatusPill } from '@mnshahawy/daffa-console-ui'
import type { Status } from '@/lib/status'

const session = useSession()
const qc = useQueryClient()
const canEdit = computed(() => session.can(Cap.FleetEdit))

const { data: deliveries } = useQuery({ queryKey: ['fleet-deliveries'], queryFn: daffa.fleetDeliveries })
const { data: clusters } = useQuery({
  queryKey: ['environments'],
  queryFn: daffa.environments,
  enabled: canEdit,
})
const { data: cas } = useQuery({ queryKey: ['cert-cas'], queryFn: daffa.cas, enabled: canEdit })
const { data: certs } = useQuery({ queryKey: ['certs'], queryFn: daffa.certs, enabled: canEdit })

// A staged successor is not selectable — you select the incumbent and the successor
// rides along by lineage; the server refuses it anyway, this just keeps the form honest.
const selectableCAs = computed(() => (cas.value ?? []).filter((ca) => ca.status !== 'next'))

function refresh() {
  qc.invalidateQueries({ queryKey: ['fleet-deliveries'] })
}

// ── the form ────────────────────────────────────────────────────────────────────

type GroupDraft = { subdir: string; bundle_cas: string[]; certs: string[] }

const editing = ref<FleetDelivery | null>(null)
const adding = ref(false)
const blank = () => ({
  env_id: '',
  volume: 'daffa-fleet-certs',
  uid: 0,
  gid: 0,
  restart_targets: '',
  groups: [{ subdir: '', bundle_cas: [], certs: [] }] as GroupDraft[],
})
const form = ref(blank())

function openCreate() {
  editing.value = null
  form.value = blank()
  adding.value = true
}

function openEdit(d: FleetDelivery) {
  editing.value = d
  form.value = {
    env_id: d.env_id,
    volume: d.volume,
    uid: d.uid,
    gid: d.gid,
    restart_targets: d.restart_targets ?? '',
    groups: d.groups.map((g) => ({
      subdir: g.subdir,
      bundle_cas: [...(g.bundle_cas ?? [])],
      certs: g.certs.map((c) => c.cert_id),
    })),
  }
  adding.value = true
}

function addGroup() {
  form.value.groups.push({ subdir: '', bundle_cas: [], certs: [] })
}

function removeGroup(i: number) {
  form.value.groups.splice(i, 1)
}

const save = useMutation({
  mutationFn: () => {
    const f = form.value
    const groups: FleetGroupRequest[] = f.groups.map((g) => ({
      subdir: g.subdir.trim(),
      bundle_cas: g.bundle_cas,
      certs: g.certs,
    }))
    const body = {
      env_id: f.env_id,
      volume: f.volume.trim(),
      uid: Number(f.uid) || 0,
      gid: Number(f.gid) || 0,
      restart_targets: f.restart_targets,
      groups,
    }
    return editing.value ? daffa.updateFleetDelivery(editing.value.id, body) : daffa.createFleetDelivery(body)
  },
  onSuccess: () => {
    toast.ok(editing.value ? 'Fleet delivery updated.' : 'Fleet delivery created — first sync is running.')
    adding.value = false
    editing.value = null
    form.value = blank()
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not save the fleet delivery.'),
})

// ── row actions ─────────────────────────────────────────────────────────────────

function status(d: FleetDelivery): Status {
  switch (d.status) {
    case 'ok':
      return { tone: 'success', label: 'Synced' }
    case 'error':
      return { tone: 'danger', label: 'Failed' }
    default:
      return { tone: 'accent', label: 'Pending', live: true }
  }
}

/** One line per group, for the table: "eu-west-prod: cell-manager (prod)". */
function groupSummary(d: FleetDelivery): string[] {
  return d.groups.map((g) => {
    const names = g.certs.map((c) => c.cert_name + (c.env_name ? ` (${c.env_name})` : '')).join(', ')
    const dir = g.subdir || '(root)'
    return names ? `${dir}: ${names}` : dir
  })
}

const sync = useMutation({
  mutationFn: (id: string) => daffa.syncFleetDelivery(id),
  onSettled: refresh,
  onSuccess: () => toast.ok('Fleet delivery synced.'),
  onError: (e) => toast.err(e, 'Sync failed.'),
})

const remove = useMutation({
  mutationFn: (id: string) => daffa.deleteFleetDelivery(id),
  onSuccess: () => {
    toast.ok('Fleet delivery deleted.')
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not delete the fleet delivery.'),
})

async function onRemove(d: FleetDelivery) {
  const ok = await confirm({
    title: `Stop delivering to ${d.volume} on ${d.env_name || d.env_id}?`,
    body: 'The volume and the files already in it are left in place — the consumer may be running on them right now. They just stop being renewed. Remove the volume yourself once nothing mounts it.',
    confirmLabel: 'Delete delivery',
    intent: 'danger',
  })
  if (ok) remove.mutate(d.id)
}
</script>

<template>
  <section>
    <div class="mb-4 flex flex-wrap items-center gap-x-3 gap-y-2">
      <div class="min-w-0">
        <h2 class="text-base font-semibold">Fleet deliveries</h2>
        <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
          One volume on a consumer's cluster, carrying certificates from any cluster — a
          subdirectory per source, each with its own cert, key and trust bundle. For fleet
          consoles that dial many clusters' control planes. Roots are chosen per group, or
          derived from whatever CA signed the group's certificates.
        </p>
      </div>
      <div class="ml-auto">
        <BaseButton v-if="canEdit" :intent="adding ? 'secondary' : 'primary'" size="sm" @click="adding ? ((adding = false), (editing = null)) : openCreate()">
          <AppIcon v-if="!adding" name="plus" class="size-3.5" />
          {{ adding ? 'Cancel' : 'Add delivery' }}
        </BaseButton>
      </div>
    </div>

    <form v-if="adding" class="surface mb-5 rounded-[var(--radius-card)] p-5" @submit.prevent="save.mutate()">
      <div class="grid gap-4 sm:grid-cols-4">
        <div>
          <label for="fd-env" class="mb-1.5 block text-sm font-medium">Cluster</label>
          <select id="fd-env" v-model="form.env_id" required class="field" :disabled="!!editing">
            <option value="" disabled>Where the volume lives…</option>
            <option v-for="e in clusters" :key="e.id" :value="e.id">{{ e.name }}</option>
          </select>
          <p v-if="editing" class="subtle mt-1 text-xs">Not editable — moving a delivery is a new delivery.</p>
        </div>
        <div>
          <label for="fd-volume" class="mb-1.5 block text-sm font-medium">Volume</label>
          <input id="fd-volume" v-model="form.volume" required class="field font-mono" :disabled="!!editing" data-cursor="text" />
        </div>
        <div>
          <label for="fd-uid" class="mb-1.5 block text-sm font-medium">UID / GID</label>
          <div class="flex gap-2">
            <input id="fd-uid" v-model.number="form.uid" type="number" min="0" class="field" data-cursor="text" />
            <input v-model.number="form.gid" type="number" min="0" class="field" aria-label="GID" data-cursor="text" />
          </div>
        </div>
        <div>
          <label for="fd-restart" class="mb-1.5 block text-sm font-medium">Restart containers</label>
          <input id="fd-restart" v-model="form.restart_targets" placeholder="optional, space-separated" class="field font-mono text-xs" data-cursor="text" />
          <p class="subtle mt-1 text-xs">Leave empty when the consumer hot-reloads.</p>
        </div>
      </div>

      <div v-for="(g, i) in form.groups" :key="i" class="mt-4 rounded-[var(--radius-card)] p-4" :style="{ background: 'var(--surface-sunken)' }">
        <div class="mb-3 flex items-center gap-3">
          <div class="grow">
            <label :for="`fd-subdir-${i}`" class="mb-1.5 block text-sm font-medium">Subdirectory</label>
            <input :id="`fd-subdir-${i}`" v-model="g.subdir" placeholder="empty = the volume root" class="field max-w-xs font-mono" data-cursor="text" />
          </div>
          <BaseButton v-if="form.groups.length > 1" intent="danger" size="xs" class="mt-6" title="Remove this group" @click="removeGroup(i)">
            <AppIcon name="trash" class="size-3.5" />
          </BaseButton>
        </div>

        <div class="grid gap-4 sm:grid-cols-2">
          <div>
            <div class="mb-1.5 text-sm font-medium">Certificates <span class="subtle font-normal">— from any cluster</span></div>
            <div class="max-h-40 space-y-1 overflow-y-auto pr-2">
              <label v-for="c in certs" :key="c.id" class="flex items-center gap-2 text-sm">
                <input v-model="g.certs" type="checkbox" :value="c.id" class="accent-[var(--color-accent-500)]" />
                <span class="font-mono text-xs">{{ c.name }}</span>
                <span class="subtle text-xs">{{ c.env_name || 'shared' }}</span>
              </label>
              <p v-if="!certs?.length" class="subtle text-xs">No certificates yet — issue them on a cluster's Certificates page first.</p>
            </div>
          </div>
          <div>
            <div class="mb-1.5 text-sm font-medium">Trust bundle roots</div>
            <div class="max-h-40 space-y-1 overflow-y-auto pr-2">
              <label v-for="ca in selectableCAs" :key="ca.id" class="flex items-center gap-2 text-sm">
                <input v-model="g.bundle_cas" type="checkbox" :value="ca.id" class="accent-[var(--color-accent-500)]" />
                <span class="font-mono text-xs">{{ ca.name }}</span>
              </label>
            </div>
            <p class="subtle mt-1 text-xs">
              None selected = derived from the CAs that signed this group's certificates —
              which is usually what a per-cluster subdirectory wants.
            </p>
          </div>
        </div>
      </div>

      <div class="mt-4 flex items-center gap-3">
        <BaseButton intent="secondary" size="sm" type="button" @click="addGroup">
          <AppIcon name="plus" class="size-3.5" />
          Add group
        </BaseButton>
        <BaseButton type="submit" intent="primary" size="md" :loading="save.isPending.value">
          {{ editing ? 'Save delivery' : 'Create delivery' }}
        </BaseButton>
      </div>
    </form>

    <EmptyState
      v-if="!deliveries?.length && !adding"
      icon="layers"
      title="No fleet deliveries yet"
      body="Deliver per-cluster client certificates and trust bundles into one volume that a fleet console mounts — each cluster under its own subdirectory, renewed and rotated by Daffa."
    >
      <template #action>
        <BaseButton v-if="canEdit" intent="primary" size="md" @click="openCreate">
          <AppIcon name="plus" class="size-4" />
          Add delivery
        </BaseButton>
      </template>
    </EmptyState>

    <div v-else-if="deliveries?.length" class="surface overflow-x-auto rounded-[var(--radius-card)]">
      <table class="w-full text-sm">
        <thead>
          <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
            <th class="eyebrow px-4 py-2 text-left font-medium">Volume</th>
            <th class="eyebrow py-2 pr-3 text-left font-medium">Carries</th>
            <th class="eyebrow py-2 pr-3 text-left font-medium">Status</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">Actions</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="d in deliveries" :key="d.id" class="border-b align-top last:border-0" :style="{ borderColor: 'var(--border)' }">
            <td class="max-w-0 py-3 pl-4 pr-3">
              <div class="font-mono text-xs font-medium">{{ d.volume }}</div>
              <div class="subtle mt-0.5 text-xs">on {{ d.env_name || d.env_id }}</div>
            </td>
            <td class="py-3 pr-3">
              <div v-for="line in groupSummary(d)" :key="line" class="subtle truncate font-mono text-xs">{{ line }}</div>
            </td>
            <td class="py-3 pr-3">
              <StatusPill :status="status(d)" />
              <div v-if="d.last_error" class="mt-1 max-w-xs truncate text-xs" :style="{ color: 'var(--danger)' }" :title="d.last_error">
                {{ d.last_error }}
              </div>
            </td>
            <td class="py-3 pr-4 text-right">
              <div v-if="canEdit" class="flex flex-col items-end gap-1 md:flex-row md:items-center md:justify-end">
                <BaseButton intent="secondary" size="xs" :disabled="sync.isPending.value" @click="sync.mutate(d.id)">
                  <AppIcon name="restart" class="size-3" />
                  Sync now
                </BaseButton>
                <BaseButton intent="secondary" size="xs" @click="openEdit(d)">Edit</BaseButton>
                <BaseButton intent="danger" size="xs" :disabled="remove.isPending.value" @click="onRemove(d)">
                  <AppIcon name="trash" class="size-3.5" />
                </BaseButton>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
</template>
