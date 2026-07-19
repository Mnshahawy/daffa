<script setup lang="ts">
import { computed, ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import {
  daffa,
  type CertAuthority,
  type CertDelivery,
  type Certificate,
  type EncryptionKey,
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
const canEditCerts = computed(() => session.can(Cap.CertsEdit))
const canEditKeys = computed(() => session.can(Cap.KeysEdit))

const { data: cas } = useQuery({ queryKey: ['cert-cas'], queryFn: daffa.cas })
const { data: certs } = useQuery({ queryKey: ['certs'], queryFn: daffa.certs })
const { data: deliveries } = useQuery({ queryKey: ['cert-deliveries'], queryFn: daffa.certDeliveries })
const { data: keys } = useQuery({
  queryKey: ['keys'],
  queryFn: daffa.encryptionKeys,
  enabled: computed(() => session.canAnywhere(Cap.KeysView)),
})
const { data: envs } = useQuery({ queryKey: ['environments'], queryFn: daffa.environments })

function refresh() {
  qc.invalidateQueries({ queryKey: ['cert-cas'] })
  qc.invalidateQueries({ queryKey: ['certs'] })
  qc.invalidateQueries({ queryKey: ['cert-deliveries'] })
  qc.invalidateQueries({ queryKey: ['keys'] })
}

/** The bundle is a download, not JSON — a plain navigation with the session cookie. */
function openBundle() {
  window.open('/api/certs/bundle', '_blank')
}

function daysLeft(notAfter: string): number {
  return Math.floor((new Date(notAfter).getTime() - Date.now()) / 86_400_000)
}
function expiry(notAfter: string): string {
  const d = daysLeft(notAfter)
  if (d < 0) return 'EXPIRED'
  if (d === 0) return 'expires today'
  return `${d}d left`
}

// ── authorities ─────────────────────────────────────────────────────────────────

const addingCA = ref(false)
const caUpload = ref(false)
const caBlank = () => ({ name: '', common_name: '', org: '', cert_pem: '', key_pem: '' })
const caForm = ref(caBlank())

const createCA = useMutation({
  mutationFn: () => {
    const f = caForm.value
    return daffa.createCA(
      caUpload.value
        ? { name: f.name, cert_pem: f.cert_pem, key_pem: f.key_pem }
        : { name: f.name, common_name: f.common_name, org: f.org },
    )
  },
  onSuccess: () => {
    caForm.value = caBlank()
    addingCA.value = false
    toast.ok('CA created.')
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not create the CA.'),
})

function caStatus(ca: CertAuthority): Status {
  switch (ca.status) {
    case 'active':
      return daysLeft(ca.not_after) < ca.warn_days
        ? { tone: 'warn', label: 'Active', detail: 'rotation due' }
        : { tone: 'success', label: 'Active' }
    case 'next':
      return { tone: 'accent', label: 'Staged', detail: 'distribute, then activate' }
    default:
      return { tone: 'neutral', label: 'Retired' }
  }
}

const rotateCA = useMutation({
  mutationFn: (id: string) => daffa.rotateCA(id, { overlap_days: 30 }),
  onSuccess: () => {
    toast.ok('Rotation staged.')
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not stage the rotation.'),
})

async function onRotate(ca: CertAuthority) {
  const ok = await confirm({
    title: `Rotate the CA ${ca.name}?`,
    body: 'This stages a NEW root alongside the current one — nothing is re-signed and nothing breaks. The trust bundle carries both roots for 30 days while you install the new one everywhere that trusts the old (operator machines, WARP profiles). When distribution is done, come back and activate it.',
    confirmLabel: 'Stage successor',
    intent: 'caution',
  })
  if (ok) rotateCA.mutate(ca.id)
}

const activateCA = useMutation({
  mutationFn: (id: string) => daffa.activateCA(id),
  onSuccess: () => {
    toast.ok('CA activated.')
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not activate the CA.'),
})

async function onActivate(ca: CertAuthority) {
  const ok = await confirm({
    title: `Activate ${ca.name}?`,
    body: 'Every certificate under the old root is re-signed by this one, and delivered volumes update within the hour. Anything that has NOT installed the new root will stop trusting them the moment its consumer reloads. Only confirm if the new root is distributed everywhere.',
    confirmLabel: 'Activate',
    intent: 'danger',
    typeToConfirm: ca.name,
  })
  if (ok) activateCA.mutate(ca.id)
}

const removeCA = useMutation({
  mutationFn: (id: string) => daffa.deleteCA(id),
  onSuccess: () => {
    toast.ok('CA deleted.')
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not delete the CA.'),
})

async function onRemoveCA(ca: CertAuthority) {
  if (ca.in_use > 0) {
    toast.warn(`${ca.name} has ${ca.in_use} certificate${ca.in_use === 1 ? '' : 's'}. Delete or re-issue them first.`)
    return
  }
  const ok = await confirm({
    title: `Delete the CA ${ca.name}?`,
    body: 'Its private key is destroyed with it. Anything that still trusts this root keeps trusting it until the certificate expires, but nothing new can ever be signed by it again.',
    confirmLabel: 'Delete',
    intent: 'danger',
    typeToConfirm: ca.name,
  })
  if (ok) removeCA.mutate(ca.id)
}

// ── certificates ────────────────────────────────────────────────────────────────

const addingCert = ref(false)
const certUpload = ref(false)
const certBlank = () => ({ name: '', ca_id: '', sans: '', cert_pem: '', chain_pem: '', key_pem: '' })
const certForm = ref(certBlank())
const signingCAs = computed(() => (cas.value ?? []).filter((c) => c.can_sign && c.status === 'active'))

const createCert = useMutation({
  mutationFn: () => {
    const f = certForm.value
    return daffa.createCert(
      certUpload.value
        ? { name: f.name, cert_pem: f.cert_pem, chain_pem: f.chain_pem, key_pem: f.key_pem }
        : { name: f.name, ca_id: f.ca_id, sans: f.sans.split(/[\s,]+/).filter(Boolean) },
    )
  },
  onSuccess: () => {
    certForm.value = certBlank()
    addingCert.value = false
    toast.ok('Certificate created.')
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not create the certificate.'),
})

function certStatus(c: Certificate): Status {
  if (c.status === 'error') return { tone: 'danger', label: 'Renewal failing' }
  const d = daysLeft(c.not_after)
  if (d < 0) return { tone: 'danger', label: 'Expired' }
  if (d <= c.renew_before_days && !c.ca_id)
    return { tone: 'warn', label: 'Expiring', detail: 'upload a replacement' }
  return { tone: 'success', label: 'Valid' }
}

const renewCert = useMutation({
  mutationFn: (id: string) => daffa.renewCert(id),
  onSuccess: () => {
    toast.ok('Certificate renewed.')
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not renew the certificate.'),
})

const removeCert = useMutation({
  mutationFn: (id: string) => daffa.deleteCert(id),
  onSuccess: () => {
    toast.ok('Certificate deleted.')
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not delete the certificate.'),
})

async function onRemoveCert(c: Certificate) {
  if (c.in_use > 0) {
    toast.warn(`${c.name} is carried by ${c.in_use} deliver${c.in_use === 1 ? 'y' : 'ies'}. Delete them first.`)
    return
  }
  const ok = await confirm({
    title: `Delete the certificate ${c.name}?`,
    body: 'Its private key is destroyed with it. Files already delivered to volumes are left in place — Daffa never deletes key material out from under a running consumer — but they will never be renewed again.',
    confirmLabel: 'Delete',
    intent: 'danger',
  })
  if (ok) removeCert.mutate(c.id)
}

// ── deliveries ──────────────────────────────────────────────────────────────────

const addingDelivery = ref(false)
const deliveryBlank = () => ({
  env_id: '',
  cert_id: '',
  volume: 'daffa-certs',
  traefik: true,
  restart_targets: '',
})
const deliveryForm = ref(deliveryBlank())

const createDelivery = useMutation({
  mutationFn: () => daffa.createCertDelivery({ ...deliveryForm.value }),
  onSuccess: () => {
    deliveryForm.value = deliveryBlank()
    addingDelivery.value = false
    toast.ok('Delivery created.')
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not create the delivery.'),
})

function deliveryStatus(d: CertDelivery): Status {
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
  mutationFn: (id: string) => daffa.syncCertDelivery(id),
  onSettled: refresh,
  onSuccess: () => toast.ok('Delivery synced.'),
  onError: (e) => toast.err(e, 'Sync failed.'),
})

const removeDelivery = useMutation({
  mutationFn: (id: string) => daffa.deleteCertDelivery(id),
  onSuccess: () => {
    toast.ok('Delivery deleted.')
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not delete the delivery.'),
})

async function onRemoveDelivery(d: CertDelivery) {
  const ok = await confirm({
    title: `Stop delivering to ${d.volume} on ${d.env_name || d.env_id}?`,
    body: 'The volume and the files already in it are left in place — the consumer may be serving with them right now. They just stop being renewed. Remove the volume yourself once nothing mounts it.',
    confirmLabel: 'Delete delivery',
    intent: 'danger',
  })
  if (ok) removeDelivery.mutate(d.id)
}

// ── encryption keys ─────────────────────────────────────────────────────────────

const addingKey = ref(false)
const keyImport = ref(false)
const keyForm = ref({ name: '', recipient: '' })

/** The one-time generate result. Held only until the operator confirms the download. */
const generated = ref<{ name: string; recipient: string; identity_file: string } | null>(null)
const downloaded = ref(false)

const createKey = useMutation({
  mutationFn: () =>
    daffa.createKey(
      keyImport.value
        ? { name: keyForm.value.name, recipient: keyForm.value.recipient }
        : { name: keyForm.value.name },
    ),
  onSuccess: (res) => {
    if (res.identity_file) {
      // Generated: the private key exists HERE and nowhere else. Do not close
      // anything until the person has it.
      generated.value = { name: res.name, recipient: res.recipient, identity_file: res.identity_file }
      downloaded.value = false
    }
    keyForm.value = { name: '', recipient: '' }
    addingKey.value = false
    toast.ok('Key created.')
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not create the key.'),
})

function downloadIdentity() {
  if (!generated.value) return
  const blob = new Blob([generated.value.identity_file], { type: 'text/plain' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = `daffa-${generated.value.name}.key`
  a.click()
  URL.revokeObjectURL(url)
  downloaded.value = true
}

const removeKey = useMutation({
  mutationFn: (id: string) => daffa.deleteKey(id),
  onSuccess: () => {
    toast.ok('Key deleted.')
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not delete the key.'),
})

async function onRemoveKey(k: EncryptionKey) {
  if (k.in_use > 0) {
    toast.warn(`${k.name} is used by ${k.in_use} backup job${k.in_use === 1 ? '' : 's'}. Point them at another key first.`)
    return
  }
  const ok = await confirm({
    title: `Delete the key ${k.name}?`,
    body: 'Only the public half is deleted — Daffa never had the private one. Snapshots already encrypted to it stay readable by whoever holds the private key.',
    confirmLabel: 'Delete',
    intent: 'danger',
  })
  if (ok) removeKey.mutate(k.id)
}
</script>

<template>
  <div class="space-y-10">
    <!-- ── authorities ─────────────────────────────────────────────────────── -->
    <section>
      <div class="mb-4 flex flex-wrap items-center gap-x-3 gap-y-2">
        <div class="min-w-0">
          <h2 class="text-base font-semibold">Certificate authorities</h2>
          <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
            The internal roots your fleet trusts. Daffa signs, renews and rotates with them —
            rotation staged first with an overlap window, so both roots are trusted while the new
            one is distributed.
          </p>
        </div>
        <div class="ml-auto flex items-center gap-2">
          <BaseButton intent="secondary" size="sm" @click="openBundle">
            <AppIcon name="shield" class="size-3.5" />
            Trust bundle
          </BaseButton>
          <BaseButton
            v-if="canEditCerts"
            :intent="addingCA ? 'secondary' : 'primary'"
            size="sm"
            @click="addingCA = !addingCA"
          >
            <AppIcon v-if="!addingCA" name="plus" class="size-3.5" />
            {{ addingCA ? 'Cancel' : 'Add CA' }}
          </BaseButton>
        </div>
      </div>

      <form
        v-if="addingCA"
        class="surface mb-5 rounded-[var(--radius-card)] p-5"
        @submit.prevent="createCA.mutate()"
      >
        <div class="mb-3 flex items-center gap-4 text-sm">
          <label class="flex items-center gap-2">
            <input v-model="caUpload" type="radio" :value="false" class="accent-[var(--color-accent-500)]" />
            Create a new root
          </label>
          <label class="flex items-center gap-2">
            <input v-model="caUpload" type="radio" :value="true" class="accent-[var(--color-accent-500)]" />
            Upload an existing one
          </label>
        </div>

        <div class="grid gap-4 sm:grid-cols-3">
          <div>
            <label for="ca-name" class="mb-1.5 block text-sm font-medium">Name</label>
            <input id="ca-name" v-model="caForm.name" required placeholder="internal-ca" class="field" data-cursor="text" />
          </div>
          <template v-if="!caUpload">
            <div>
              <label for="ca-cn" class="mb-1.5 block text-sm font-medium">Common name</label>
              <input id="ca-cn" v-model="caForm.common_name" required placeholder="Example Internal CA" class="field" data-cursor="text" />
            </div>
            <div>
              <label for="ca-org" class="mb-1.5 block text-sm font-medium">Organization</label>
              <input id="ca-org" v-model="caForm.org" placeholder="optional" class="field" data-cursor="text" />
            </div>
          </template>
        </div>

        <div v-if="caUpload" class="mt-4 grid gap-4 sm:grid-cols-2">
          <div>
            <label for="ca-pem" class="mb-1.5 block text-sm font-medium">CA certificate (PEM)</label>
            <textarea id="ca-pem" v-model="caForm.cert_pem" required rows="5" class="field font-mono text-xs" placeholder="-----BEGIN CERTIFICATE-----" />
          </div>
          <div>
            <label for="ca-key" class="mb-1.5 block text-sm font-medium">CA private key (PEM)</label>
            <textarea id="ca-key" v-model="caForm.key_pem" rows="5" class="field font-mono text-xs" placeholder="-----BEGIN PRIVATE KEY----- (leave empty for a trust-only anchor)" />
            <p class="subtle mt-1 text-xs">
              With the key, Daffa can issue and auto-renew. Without it, the CA is bundled and
              delivered but never signs.
            </p>
          </div>
        </div>

        <BaseButton type="submit" intent="primary" size="md" class="mt-4" :loading="createCA.isPending.value">
          {{ caUpload ? 'Import CA' : 'Create CA' }}
        </BaseButton>
      </form>

      <EmptyState
        v-if="!cas?.length && !addingCA"
        icon="shield"
        title="No certificate authorities yet"
        body="Create an internal root here (or import the one from /etc/internal-ca), and Daffa takes over what the renewal cron and the rotation checklist used to do: issuing, renewing, rotating with overlap, and telling you when to act."
      >
        <template #action>
          <BaseButton v-if="canEditCerts" intent="primary" size="md" @click="addingCA = true">
            <AppIcon name="plus" class="size-4" />
            Add CA
          </BaseButton>
        </template>
      </EmptyState>

      <div v-else-if="cas?.length" class="surface overflow-hidden rounded-[var(--radius-card)]">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
              <th class="eyebrow px-4 py-2 text-left font-medium">Authority</th>
              <th class="eyebrow py-2 pr-3 text-left font-medium">Status</th>
              <th class="eyebrow py-2 pr-3 text-right font-medium">Expiry</th>
              <th class="eyebrow py-2 pr-3 text-right font-medium">Signed</th>
              <th class="eyebrow py-2 pr-4 text-right font-medium">Actions</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="ca in cas" :key="ca.id" class="border-b last:border-0" :style="{ borderColor: 'var(--border)' }">
              <td class="max-w-0 py-3 pl-4 pr-3">
                <div class="flex items-center gap-2">
                  <span class="font-medium">{{ ca.name }}</span>
                  <span v-if="!ca.can_sign" class="rounded-md px-1.5 py-0.5 text-xs" :style="{ background: 'var(--surface-sunken)', color: 'var(--text-muted)' }" title="Uploaded without its key: bundled and delivered, never signs">trust-only</span>
                </div>
                <div class="subtle mt-0.5 truncate font-mono text-xs">{{ ca.subject }}</div>
              </td>
              <td class="py-3 pr-3"><StatusPill :status="caStatus(ca)" /></td>
              <td class="subtle py-3 pr-3 text-right font-mono text-xs" :title="ca.not_after">
                {{ expiry(ca.not_after) }}
              </td>
              <td class="subtle py-3 pr-3 text-right font-mono text-xs">{{ ca.in_use }}</td>
              <td class="py-3 pr-4 text-right">
                <div v-if="canEditCerts" class="flex items-center justify-end gap-1">
                  <BaseButton v-if="ca.status === 'active' && ca.can_sign" intent="secondary" size="xs" :disabled="rotateCA.isPending.value" @click="onRotate(ca)">
                    <AppIcon name="restart" class="size-3" />
                    Rotate
                  </BaseButton>
                  <BaseButton v-if="ca.status === 'next'" intent="caution" size="xs" :disabled="activateCA.isPending.value" @click="onActivate(ca)">
                    Activate
                  </BaseButton>
                  <BaseButton intent="danger" size="xs" :disabled="removeCA.isPending.value" @click="onRemoveCA(ca)">
                    <AppIcon name="trash" class="size-3.5" />
                  </BaseButton>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <!-- ── certificates ────────────────────────────────────────────────────── -->
    <section>
      <div class="mb-4 flex flex-wrap items-center gap-x-3 gap-y-2">
        <div class="min-w-0">
          <h2 class="text-base font-semibold">Certificates</h2>
          <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
            Issued ones renew themselves inside their window — same key, new signature, delivered
            without a restart. Uploaded ones are tracked and nagged about, because only you can
            replace them.
          </p>
        </div>
        <div class="ml-auto">
          <BaseButton v-if="canEditCerts" :intent="addingCert ? 'secondary' : 'primary'" size="sm" @click="addingCert = !addingCert">
            <AppIcon v-if="!addingCert" name="plus" class="size-3.5" />
            {{ addingCert ? 'Cancel' : 'Add certificate' }}
          </BaseButton>
        </div>
      </div>

      <form v-if="addingCert" class="surface mb-5 rounded-[var(--radius-card)] p-5" @submit.prevent="createCert.mutate()">
        <div class="mb-3 flex items-center gap-4 text-sm">
          <label class="flex items-center gap-2">
            <input v-model="certUpload" type="radio" :value="false" class="accent-[var(--color-accent-500)]" />
            Issue from a CA
          </label>
          <label class="flex items-center gap-2">
            <input v-model="certUpload" type="radio" :value="true" class="accent-[var(--color-accent-500)]" />
            Upload
          </label>
        </div>

        <div class="grid gap-4 sm:grid-cols-3">
          <div>
            <label for="crt-name" class="mb-1.5 block text-sm font-medium">Name</label>
            <input id="crt-name" v-model="certForm.name" required placeholder="web-frontend" class="field" data-cursor="text" />
            <p class="subtle mt-1 text-xs">Becomes the filenames in the volume: {{ certForm.name || 'name' }}.crt / .key</p>
          </div>
          <template v-if="!certUpload">
            <div>
              <label for="crt-ca" class="mb-1.5 block text-sm font-medium">Authority</label>
              <Select id="crt-ca" v-model="certForm.ca_id" required>
                <option value="" disabled>Choose a CA…</option>
                <option v-for="ca in signingCAs" :key="ca.id" :value="ca.id">{{ ca.name }}</option>
              </Select>
            </div>
            <div>
              <label for="crt-sans" class="mb-1.5 block text-sm font-medium">SANs</label>
              <input id="crt-sans" v-model="certForm.sans" required placeholder="app.example.com www.example.com" class="field font-mono text-xs" data-cursor="text" />
              <p class="subtle mt-1 text-xs">Space-separated. The first one is the common name. Editable later — editing re-issues.</p>
            </div>
          </template>
        </div>

        <div v-if="certUpload" class="mt-4 grid gap-4 sm:grid-cols-3">
          <div>
            <label for="crt-pem" class="mb-1.5 block text-sm font-medium">Certificate (PEM)</label>
            <textarea id="crt-pem" v-model="certForm.cert_pem" required rows="5" class="field font-mono text-xs" />
          </div>
          <div>
            <label for="crt-chain" class="mb-1.5 block text-sm font-medium">Chain (optional)</label>
            <textarea id="crt-chain" v-model="certForm.chain_pem" rows="5" class="field font-mono text-xs" />
          </div>
          <div>
            <label for="crt-key" class="mb-1.5 block text-sm font-medium">Private key (PEM)</label>
            <textarea id="crt-key" v-model="certForm.key_pem" required rows="5" class="field font-mono text-xs" />
          </div>
        </div>

        <BaseButton type="submit" intent="primary" size="md" class="mt-4" :loading="createCert.isPending.value">
          {{ certUpload ? 'Upload certificate' : 'Issue certificate' }}
        </BaseButton>
      </form>

      <div v-if="certs?.length" class="surface overflow-hidden rounded-[var(--radius-card)]">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
              <th class="eyebrow px-4 py-2 text-left font-medium">Certificate</th>
              <th class="eyebrow py-2 pr-3 text-left font-medium">Status</th>
              <th class="eyebrow py-2 pr-3 text-left font-medium">Authority</th>
              <th class="eyebrow py-2 pr-3 text-right font-medium">Expiry</th>
              <th class="eyebrow py-2 pr-4 text-right font-medium">Actions</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="c in certs" :key="c.id" class="border-b last:border-0" :style="{ borderColor: 'var(--border)' }">
              <td class="max-w-0 py-3 pl-4 pr-3">
                <div class="font-medium">{{ c.name }}</div>
                <div class="subtle mt-0.5 truncate font-mono text-xs" :title="c.sans.join(' ')">{{ c.sans.join(' ') }}</div>
                <div v-if="c.last_error" class="mt-0.5 truncate text-xs" :style="{ color: 'var(--danger)' }" :title="c.last_error">{{ c.last_error }}</div>
              </td>
              <td class="py-3 pr-3"><StatusPill :status="certStatus(c)" /></td>
              <td class="subtle py-3 pr-3 text-xs">{{ c.ca_name || 'uploaded' }}</td>
              <td class="subtle py-3 pr-3 text-right font-mono text-xs" :title="c.not_after">{{ expiry(c.not_after) }}</td>
              <td class="py-3 pr-4 text-right">
                <div v-if="canEditCerts" class="flex items-center justify-end gap-1">
                  <BaseButton v-if="c.ca_id" intent="secondary" size="xs" :disabled="renewCert.isPending.value" @click="renewCert.mutate(c.id)">
                    <AppIcon name="restart" class="size-3" />
                    Renew
                  </BaseButton>
                  <BaseButton intent="danger" size="xs" :disabled="removeCert.isPending.value" @click="onRemoveCert(c)">
                    <AppIcon name="trash" class="size-3.5" />
                  </BaseButton>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <p v-else-if="!addingCert" class="muted text-sm">No certificates yet.</p>
    </section>

    <!-- ── deliveries ──────────────────────────────────────────────────────── -->
    <section>
      <div class="mb-4 flex flex-wrap items-center gap-x-3 gap-y-2">
        <div class="min-w-0">
          <h2 class="text-base font-semibold">Deliveries</h2>
          <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
            A delivery keeps a certificate (and the trust bundle) current inside a named volume on
            a host. Mount it read-only — for Traefik, at
            <code class="font-mono text-xs">/etc/traefik/dynamic-certs</code> with the file
            provider watching it, and renewals become hot reloads instead of restarts.
          </p>
        </div>
        <div class="ml-auto">
          <BaseButton v-if="canEditCerts" :intent="addingDelivery ? 'secondary' : 'primary'" size="sm" @click="addingDelivery = !addingDelivery">
            <AppIcon v-if="!addingDelivery" name="plus" class="size-3.5" />
            {{ addingDelivery ? 'Cancel' : 'Add delivery' }}
          </BaseButton>
        </div>
      </div>

      <form v-if="addingDelivery" class="surface mb-5 rounded-[var(--radius-card)] p-5" @submit.prevent="createDelivery.mutate()">
        <div class="grid gap-4 sm:grid-cols-4">
          <div>
            <label for="dlv-env" class="mb-1.5 block text-sm font-medium">Host</label>
            <Select id="dlv-env" v-model="deliveryForm.env_id" required>
              <option value="" disabled>Choose a cluster…</option>
              <option v-for="e in envs" :key="e.id" :value="e.id">{{ e.name }}</option>
            </Select>
          </div>
          <div>
            <label for="dlv-cert" class="mb-1.5 block text-sm font-medium">Certificate</label>
            <Select id="dlv-cert" v-model="deliveryForm.cert_id">
              <option value="">Trust bundle only</option>
              <option v-for="c in certs" :key="c.id" :value="c.id">{{ c.name }}</option>
            </Select>
          </div>
          <div>
            <label for="dlv-volume" class="mb-1.5 block text-sm font-medium">Volume</label>
            <input id="dlv-volume" v-model="deliveryForm.volume" required class="field font-mono text-xs" data-cursor="text" />
          </div>
          <div>
            <label for="dlv-restart" class="mb-1.5 block text-sm font-medium">Restart after sync</label>
            <input id="dlv-restart" v-model="deliveryForm.restart_targets" placeholder="container names (optional)" class="field font-mono text-xs" data-cursor="text" />
            <p class="subtle mt-1 text-xs">Only for consumers that cannot hot-reload. Traefik's file provider does not need it.</p>
          </div>
        </div>
        <label class="mt-3 flex items-center gap-2 text-sm">
          <input v-model="deliveryForm.traefik" type="checkbox" class="accent-[var(--color-accent-500)]" />
          Render a Traefik file-provider fragment (tls.yml) into the volume
        </label>
        <BaseButton type="submit" intent="primary" size="md" class="mt-4" :loading="createDelivery.isPending.value">
          Create delivery
        </BaseButton>
      </form>

      <div v-if="deliveries?.length" class="surface overflow-hidden rounded-[var(--radius-card)]">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
              <th class="eyebrow px-4 py-2 text-left font-medium">Delivery</th>
              <th class="eyebrow py-2 pr-3 text-left font-medium">Status</th>
              <th class="eyebrow py-2 pr-3 text-right font-medium">Last synced</th>
              <th class="eyebrow py-2 pr-4 text-right font-medium">Actions</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="d in deliveries" :key="d.id" class="border-b last:border-0" :style="{ borderColor: 'var(--border)' }">
              <td class="max-w-0 py-3 pl-4 pr-3">
                <div class="font-medium">
                  {{ d.cert_name || 'trust bundle' }}
                  <span class="subtle">→ {{ d.volume }} on {{ d.env_name || d.env_id }}</span>
                </div>
                <div v-if="d.last_error" class="mt-0.5 truncate text-xs" :style="{ color: 'var(--danger)' }" :title="d.last_error">{{ d.last_error }}</div>
              </td>
              <td class="py-3 pr-3"><StatusPill :status="deliveryStatus(d)" /></td>
              <td class="subtle py-3 pr-3 text-right font-mono text-xs">
                <time v-if="d.synced_at" :title="d.synced_at">{{ new Date(d.synced_at).toLocaleString() }}</time>
                <span v-else>never</span>
              </td>
              <td class="py-3 pr-4 text-right">
                <div v-if="canEditCerts" class="flex items-center justify-end gap-1">
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

    <!-- ── encryption keys ─────────────────────────────────────────────────── -->
    <section v-if="session.canAnywhere(Cap.KeysView)">
      <div class="mb-4 flex flex-wrap items-center gap-x-3 gap-y-2">
        <div class="min-w-0">
          <h2 class="text-base font-semibold">Backup encryption keys</h2>
          <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
            age keypairs backups encrypt to. Daffa keeps only the public half — the private key is
            downloaded once at generation and never stored, so this box can write backups it
            cannot read. Keep two: a personal key and a break-glass key held somewhere independent.
          </p>
        </div>
        <div class="ml-auto">
          <BaseButton v-if="canEditKeys" :intent="addingKey ? 'secondary' : 'primary'" size="sm" @click="addingKey = !addingKey">
            <AppIcon v-if="!addingKey" name="key" class="size-3.5" />
            {{ addingKey ? 'Cancel' : 'Add key' }}
          </BaseButton>
        </div>
      </div>

      <!-- The one-time download. Modal-ish on purpose: this value exists nowhere else. -->
      <div v-if="generated" class="mb-5 rounded-[var(--radius-card)] p-5" :style="{ background: 'var(--warn-soft)', border: '1px solid color-mix(in oklch, var(--warn) 40%, transparent)' }">
        <p class="mb-1 text-sm font-semibold">Download the private key for “{{ generated.name }}” — now.</p>
        <p class="mb-3 text-sm leading-relaxed">
          This is the only time it will ever exist outside your machine. Daffa stored the public
          half only; close this without downloading and the key is gone — backups encrypted to it
          would be unreadable by anyone, forever. Put the file in a password manager and an
          offline copy, never on this box.
        </p>
        <div class="flex flex-wrap items-center gap-3">
          <BaseButton intent="primary" size="md" @click="downloadIdentity">
            Download daffa-{{ generated.name }}.key
          </BaseButton>
          <BaseButton intent="secondary" size="md" :disabled="!downloaded" :title="downloaded ? '' : 'Download it first'" @click="generated = null">
            I have stored it safely
          </BaseButton>
        </div>
      </div>

      <form v-if="addingKey" class="surface mb-5 rounded-[var(--radius-card)] p-5" @submit.prevent="createKey.mutate()">
        <div class="mb-3 flex items-center gap-4 text-sm">
          <label class="flex items-center gap-2">
            <input v-model="keyImport" type="radio" :value="false" class="accent-[var(--color-accent-500)]" />
            Generate a keypair
          </label>
          <label class="flex items-center gap-2">
            <input v-model="keyImport" type="radio" :value="true" class="accent-[var(--color-accent-500)]" />
            Import a public key
          </label>
        </div>
        <div class="grid gap-4 sm:grid-cols-2">
          <div>
            <label for="key-name" class="mb-1.5 block text-sm font-medium">Name</label>
            <input id="key-name" v-model="keyForm.name" required placeholder="mohamed-personal" class="field" data-cursor="text" />
          </div>
          <div v-if="keyImport">
            <label for="key-recipient" class="mb-1.5 block text-sm font-medium">age public key</label>
            <input id="key-recipient" v-model="keyForm.recipient" required placeholder="age1…" class="field font-mono text-xs" data-cursor="text" />
            <p class="subtle mt-1 text-xs">
              From <code class="font-mono">age-keygen -y key.txt</code>. Never paste the private
              key — the server refuses it.
            </p>
          </div>
        </div>
        <BaseButton type="submit" intent="primary" size="md" class="mt-4" :loading="createKey.isPending.value">
          {{ keyImport ? 'Import key' : 'Generate key' }}
        </BaseButton>
        <p v-if="!keyImport" class="subtle mt-2 text-xs">
          Generated in memory, handed to you once, never stored. The next screen is the download —
          do not skip it.
        </p>
      </form>

      <div v-if="keys?.length" class="surface overflow-hidden rounded-[var(--radius-card)]">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
              <th class="eyebrow px-4 py-2 text-left font-medium">Key</th>
              <th class="eyebrow py-2 pr-3 text-left font-medium">Source</th>
              <th class="eyebrow py-2 pr-3 text-right font-medium">In use</th>
              <th class="eyebrow py-2 pr-4 text-right font-medium">Actions</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="k in keys" :key="k.id" class="border-b last:border-0" :style="{ borderColor: 'var(--border)' }">
              <td class="max-w-0 py-3 pl-4 pr-3">
                <div class="font-medium">{{ k.name }}</div>
                <div class="subtle mt-0.5 truncate font-mono text-xs" :title="k.recipient">{{ k.recipient }}</div>
              </td>
              <td class="subtle py-3 pr-3 text-xs">{{ k.source }}</td>
              <td class="subtle py-3 pr-3 text-right font-mono text-xs">
                {{ k.in_use }} job{{ k.in_use === 1 ? '' : 's' }}
              </td>
              <td class="py-3 pr-4 text-right">
                <BaseButton v-if="canEditKeys" intent="danger" size="xs" :disabled="removeKey.isPending.value" @click="onRemoveKey(k)">
                  <AppIcon name="trash" class="size-3.5" />
                </BaseButton>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <p v-else-if="!addingKey" class="muted text-sm">No encryption keys yet.</p>
    </section>
  </div>
</template>
