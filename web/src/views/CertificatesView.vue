<script setup lang="ts">
import { computed, ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { daffa, type CertDelivery, type Certificate } from '@/lib/api'
import { Cap } from '@/lib/caps'
import { useSession } from '@/stores/session'
import { confirm } from '@/lib/confirm'
import { toast } from '@/lib/toast'
import { daysLeft, expiry } from '@/lib/certdates'
import type { Status } from '@/lib/status'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import Select from '@/components/ui/Select.vue'
import StatusPill from '@/components/ui/StatusPill.vue'

// Cluster-scoped: this page shows the certificates THIS cluster consumes — its own plus
// the shared ones — and the deliveries that put them into its volumes. The lists come
// back visibility-filtered from the server; the cut by CURRENT cluster happens here,
// because the cluster switcher is a client-side lens, not a capability. The roots that
// sign everything are fleet trust and live under Settings → Authorities.

const session = useSession()
const qc = useQueryClient()
const canEditCerts = computed(() => session.can(Cap.CertsEdit))

const { data: cas } = useQuery({ queryKey: ['cert-cas'], queryFn: daffa.cas })
const { data: allCerts } = useQuery({ queryKey: ['certs'], queryFn: daffa.certs })
const { data: allDeliveries } = useQuery({ queryKey: ['cert-deliveries'], queryFn: daffa.certDeliveries })
const { data: envs } = useQuery({ queryKey: ['environments'], queryFn: daffa.environments })

const clusterName = computed(
  () => envs.value?.find((e) => e.id === session.envId)?.name ?? 'this cluster',
)

const certs = computed(() =>
  (allCerts.value ?? []).filter((c) => !c.env_id || c.env_id === session.envId),
)
const deliveries = computed(() =>
  (allDeliveries.value ?? []).filter((d) => d.env_id === session.envId),
)

function refresh() {
  qc.invalidateQueries({ queryKey: ['cert-cas'] })
  qc.invalidateQueries({ queryKey: ['certs'] })
  qc.invalidateQueries({ queryKey: ['cert-deliveries'] })
}

// ── certificates ────────────────────────────────────────────────────────────────

const addingCert = ref(false)
const certUpload = ref(false)
const certBlank = () => ({
  name: '',
  shared: false,
  ca_id: '',
  sans: '',
  server: true,
  client: false,
  cert_pem: '',
  chain_pem: '',
  key_pem: '',
})
const certForm = ref(certBlank())
const signingCAs = computed(() => (cas.value ?? []).filter((c) => c.can_sign && c.status === 'active'))

const createCert = useMutation({
  mutationFn: () => {
    const f = certForm.value
    // Created from a cluster's page, the cert belongs to that cluster unless
    // explicitly shared — the narrow default; other clusters never see it.
    const env_id = f.shared ? '' : session.envId
    const usages = [...(f.server ? ['server'] : []), ...(f.client ? ['client'] : [])]
    return daffa.createCert(
      certUpload.value
        ? { name: f.name, env_id, cert_pem: f.cert_pem, chain_pem: f.chain_pem, key_pem: f.key_pem }
        : { name: f.name, env_id, ca_id: f.ca_id, sans: f.sans.split(/[\s,]+/).filter(Boolean), usages },
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
// Set while editing an existing delivery; null while creating one. The form is the same
// shape either way — a delivery IS the set of files Daffa manages in one volume, so
// "carries these certificates" and "lands at this path" are edits, not a new delivery.
const editingDelivery = ref<CertDelivery | null>(null)
const DEFAULT_MOUNT_PATH = '/etc/traefik/dynamic-certs'
const deliveryBlank = () => ({
  certs: [] as { cert_id: string; is_default: boolean }[],
  volume: 'daffa-certs',
  mount_path: DEFAULT_MOUNT_PATH,
  traefik: true,
  restart_targets: '',
  bundle_cas: [] as string[],
})
const deliveryForm = ref(deliveryBlank())

function carriesCert(id: string) {
  return deliveryForm.value.certs.some((c) => c.cert_id === id)
}

function toggleDeliveryCert(id: string) {
  const sel = deliveryForm.value.certs
  const i = sel.findIndex((c) => c.cert_id === id)
  if (i >= 0) sel.splice(i, 1)
  // First certificate added becomes the default: with one certificate that is what the
  // old single-cert fragment always said, and it is the answer an operator expects.
  else sel.push({ cert_id: id, is_default: !sel.some((c) => c.is_default) })
}

// Exactly one default, or none: Traefik has a single stores.default.defaultCertificate,
// and clicking the current default again clears it (Traefik then keeps its own self-signed
// default for unmatched SNI, which the hint below says out loud).
function setDefaultCert(id: string) {
  const sel = deliveryForm.value.certs
  const wasDefault = sel.find((c) => c.cert_id === id)?.is_default
  sel.forEach((c) => (c.is_default = !wasDefault && c.cert_id === id))
}

function startEditDelivery(d: CertDelivery) {
  editingDelivery.value = d
  addingDelivery.value = true
  deliveryForm.value = {
    certs: (d.certs ?? []).map((c) => ({ cert_id: c.cert_id, is_default: c.is_default })),
    volume: d.volume,
    mount_path: d.mount_path || DEFAULT_MOUNT_PATH,
    traefik: d.traefik,
    restart_targets: d.restart_targets ?? '',
    bundle_cas: [...(d.bundle_cas ?? [])],
  }
}

function closeDeliveryForm() {
  addingDelivery.value = false
  editingDelivery.value = null
  deliveryForm.value = deliveryBlank()
}

// Selectable bundle roots: anything not mid-rotation — a staged successor rides along on
// its own and the server refuses selecting it directly.
const selectableCAs = computed(() => (cas.value ?? []).filter((c) => c.status !== 'next'))

function toggleBundleCA(id: string) {
  const sel = deliveryForm.value.bundle_cas
  const i = sel.indexOf(id)
  if (i >= 0) sel.splice(i, 1)
  else sel.push(id)
}

const caNames = computed(() => new Map((cas.value ?? []).map((c) => [c.id, c.name])))
const certNames = computed(() => new Map((certs.value ?? []).map((c) => [c.id, c.name])))
function bundleLabel(d: CertDelivery): string {
  if (!d.bundle_cas?.length) return 'all roots'
  return d.bundle_cas.map((id) => caNames.value.get(id) ?? id).join(', ')
}

const saveDelivery = useMutation({
  mutationFn: () => {
    const d = editingDelivery.value
    return d
      ? daffa.updateCertDelivery(d.id, { ...deliveryForm.value })
      : daffa.createCertDelivery({ ...deliveryForm.value, env_id: session.envId })
  },
  onSuccess: () => {
    const edited = !!editingDelivery.value
    closeDeliveryForm()
    toast.ok(edited ? 'Delivery updated.' : 'Delivery created.')
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not save the delivery.'),
})

// What the delivery will write into the volume — the list Traefik's file provider ignores
// (it reads only .toml/.yaml/.yml), which is exactly why a git-sourced volume source can
// share the same directory.
const deliveryFileNames = computed(() => {
  const names = deliveryForm.value.certs
    .map((c) => certNames.value.get(c.cert_id))
    .filter(Boolean)
    .flatMap((n) => [`${n}.crt`, `${n}.key`])
  if (deliveryForm.value.traefik && deliveryForm.value.certs.length) names.unshift('tls.yml')
  return [...names, 'ca-bundle.crt']
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
</script>

<template>
  <div class="space-y-10">
    <!-- ── certificates ────────────────────────────────────────────────────── -->
    <section>
      <div class="mb-4 flex flex-wrap items-center gap-x-3 gap-y-2">
        <div class="min-w-0">
          <h2 class="text-base font-semibold">Certificates</h2>
          <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
            What {{ clusterName }} serves and presents — its own certificates plus the shared
            ones. Issued ones renew themselves inside their window; uploaded ones are tracked and
            nagged about. The signing roots live under Settings → Authorities.
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
            <div>
              <span class="mb-1.5 block text-sm font-medium">Used as</span>
              <div class="flex items-center gap-4 text-sm">
                <label class="flex items-center gap-2">
                  <input v-model="certForm.server" type="checkbox" class="accent-[var(--color-accent-500)]" />
                  Server
                </label>
                <label class="flex items-center gap-2">
                  <input v-model="certForm.client" type="checkbox" class="accent-[var(--color-accent-500)]" />
                  Client
                </label>
              </div>
              <p class="subtle mt-1 text-xs">
                mTLS peers need both: the client half is what lets a service PRESENT this cert to
                another. Editable later — editing re-issues with the same key.
              </p>
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

        <label class="mt-3 flex items-center gap-2 text-sm">
          <input v-model="certForm.shared" type="checkbox" class="accent-[var(--color-accent-500)]" />
          Share with every cluster
        </label>
        <p class="subtle mt-1 text-xs">
          Unshared, it belongs to {{ clusterName }} and other clusters never see it — and each
          cluster can have its own certificate under this name. Fixed after creation.
        </p>

        <BaseButton type="submit" intent="primary" size="md" class="mt-4" :loading="createCert.isPending.value">
          {{ certUpload ? 'Upload certificate' : 'Issue certificate' }}
        </BaseButton>
      </form>

      <div v-if="certs.length" class="surface overflow-x-auto rounded-[var(--radius-card)]">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
              <th class="eyebrow px-4 py-2 text-left font-medium">Certificate</th>
              <th class="eyebrow py-2 pr-3 text-left font-medium">Status</th>
              <th class="eyebrow hidden py-2 pr-3 text-left font-medium md:table-cell">Authority</th>
              <th class="eyebrow hidden py-2 pr-3 text-right font-medium md:table-cell">Expiry</th>
              <th class="eyebrow py-2 pr-4 text-right font-medium">Actions</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="c in certs" :key="c.id" class="border-b last:border-0" :style="{ borderColor: 'var(--border)' }">
              <td class="max-w-0 py-3 pl-4 pr-3">
                <div class="flex items-center gap-2">
                  <span class="font-medium">{{ c.name }}</span>
                  <span v-if="!c.env_id" class="rounded-md px-1.5 py-0.5 text-xs" :style="{ background: 'var(--surface-sunken)', color: 'var(--text-muted)' }" title="Shared: every cluster sees and can deliver it">shared</span>
                  <span v-if="c.usages?.includes('client')" class="rounded-md px-1.5 py-0.5 text-xs" :style="{ background: 'var(--surface-sunken)', color: 'var(--text-muted)' }" :title="c.usages.includes('server') ? 'Carries both serverAuth and clientAuth — an mTLS identity' : 'clientAuth only'">{{ c.usages.includes('server') ? 'mTLS' : 'client' }}</span>
                </div>
                <div class="subtle mt-0.5 truncate font-mono text-xs" :title="c.sans.join(' ')">{{ c.sans.join(' ') }}</div>
                <!-- The expiry column is hidden on a phone, but a cert's expiry is the thing
                     this page exists to answer for — so it rides along under the name. -->
                <div class="subtle mt-0.5 font-mono text-xs md:hidden" :title="c.not_after">expires {{ expiry(c.not_after) }}</div>
                <div v-if="c.last_error" class="mt-0.5 truncate text-xs" :style="{ color: 'var(--danger)' }" :title="c.last_error">{{ c.last_error }}</div>
              </td>
              <td class="py-3 pr-3"><StatusPill :status="certStatus(c)" /></td>
              <td class="subtle hidden py-3 pr-3 text-xs md:table-cell">{{ c.ca_name || 'uploaded' }}</td>
              <td class="subtle hidden py-3 pr-3 text-right font-mono text-xs md:table-cell" :title="c.not_after">{{ expiry(c.not_after) }}</td>
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
      <p v-else-if="!addingCert" class="muted text-sm">
        No certificates for {{ clusterName }} yet — its own and shared ones will show here.
      </p>
    </section>

    <!-- ── deliveries ──────────────────────────────────────────────────────── -->
    <section>
      <div class="mb-4 flex flex-wrap items-center gap-x-3 gap-y-2">
        <div class="min-w-0">
          <h2 class="text-base font-semibold">Deliveries</h2>
          <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
            A delivery keeps certificates and the trust bundle current inside a named volume on
            {{ clusterName }}. Mount it read-only; with Traefik's file provider watching that
            directory, renewals become hot reloads instead of restarts. The PEMs are invisible
            to Traefik — it reads only <code class="font-mono text-xs">.yml</code>,
            <code class="font-mono text-xs">.yaml</code> and
            <code class="font-mono text-xs">.toml</code> — so a git-sourced volume can share the
            same directory for middlewares and routers.
          </p>
        </div>
        <div class="ml-auto">
          <BaseButton v-if="canEditCerts" :intent="addingDelivery ? 'secondary' : 'primary'" size="sm" @click="addingDelivery ? closeDeliveryForm() : (addingDelivery = true)">
            <AppIcon v-if="!addingDelivery" name="plus" class="size-3.5" />
            {{ addingDelivery ? 'Cancel' : 'Add delivery' }}
          </BaseButton>
        </div>
      </div>

      <form v-if="addingDelivery" class="surface mb-5 rounded-[var(--radius-card)] p-5" @submit.prevent="saveDelivery.mutate()">
        <div>
          <span class="mb-1.5 block text-sm font-medium">Certificates</span>
          <div v-if="certs.length" class="flex flex-col gap-1.5">
            <div v-for="c in certs" :key="c.id" class="flex items-center gap-3 text-sm">
              <label class="flex items-center gap-2">
                <input :checked="carriesCert(c.id)" type="checkbox" class="accent-[var(--color-accent-500)]" @change="toggleDeliveryCert(c.id)" />
                {{ c.name }}
              </label>
              <button
                v-if="carriesCert(c.id) && deliveryForm.traefik"
                type="button"
                class="rounded-md px-1.5 py-0.5 text-xs"
                :style="{
                  background: deliveryForm.certs.find((x) => x.cert_id === c.id)?.is_default ? 'var(--accent-soft)' : 'var(--surface-sunken)',
                  color: deliveryForm.certs.find((x) => x.cert_id === c.id)?.is_default ? 'var(--accent-text)' : 'var(--text-muted)',
                }"
                title="Traefik's stores.default.defaultCertificate — served when no certificate matches the requested name"
                @click="setDefaultCert(c.id)"
              >
                default
              </button>
            </div>
          </div>
          <p v-else class="subtle text-xs">No certificates on {{ clusterName }} yet.</p>
          <p class="subtle mt-1 text-xs">
            None selected = trust bundle only: the volume carries
            <code class="font-mono">ca-bundle.crt</code> and nothing else. With no default marked,
            Traefik keeps its own self-signed certificate for unmatched names.
          </p>
        </div>
        <div class="mt-4 grid gap-4 sm:grid-cols-3">
          <div>
            <label for="dlv-volume" class="mb-1.5 block text-sm font-medium">Volume</label>
            <input id="dlv-volume" v-model="deliveryForm.volume" required :disabled="!!editingDelivery" class="field font-mono text-xs disabled:opacity-60" data-cursor="text" />
            <p v-if="editingDelivery" class="subtle mt-1 text-xs">Moving to another volume means a new delivery.</p>
          </div>
          <div>
            <label for="dlv-mount" class="mb-1.5 block text-sm font-medium">Mount path</label>
            <input id="dlv-mount" v-model="deliveryForm.mount_path" required class="field font-mono text-xs" data-cursor="text" />
            <p class="subtle mt-1 text-xs">Where the consumer mounts this volume — Traefik resolves the paths inside tls.yml itself.</p>
          </div>
          <div>
            <label for="dlv-restart" class="mb-1.5 block text-sm font-medium">Restart after sync</label>
            <input id="dlv-restart" v-model="deliveryForm.restart_targets" placeholder="container names (optional)" class="field font-mono text-xs" data-cursor="text" />
            <p class="subtle mt-1 text-xs">Only for consumers that cannot hot-reload. Traefik's file provider does not need it.</p>
          </div>
        </div>
        <div v-if="selectableCAs.length > 1" class="mt-4">
          <span class="mb-1.5 block text-sm font-medium">Trust bundle roots</span>
          <div class="flex flex-wrap items-center gap-4 text-sm">
            <label v-for="ca in selectableCAs" :key="ca.id" class="flex items-center gap-2">
              <input :checked="deliveryForm.bundle_cas.includes(ca.id)" type="checkbox" class="accent-[var(--color-accent-500)]" @change="toggleBundleCA(ca.id)" />
              {{ ca.name }}
            </label>
          </div>
          <p class="subtle mt-1 text-xs">
            What this volume's ca-bundle.crt carries — the roots the consumer will trust as mTLS
            peers. None selected = every root. Rotations follow along: a staged successor joins its
            root's bundles automatically.
          </p>
        </div>
        <label class="mt-3 flex items-center gap-2 text-sm">
          <input v-model="deliveryForm.traefik" type="checkbox" class="accent-[var(--color-accent-500)]" />
          Render a Traefik file-provider fragment (tls.yml) into the volume
        </label>
        <p class="subtle mt-1 text-xs">
          One delivery per volume may do this — two would rewrite each other's tls.yml forever.
        </p>
        <p class="subtle mt-3 text-xs">
          Writes: <code class="font-mono">{{ deliveryFileNames.join(', ') }}</code>
        </p>
        <BaseButton type="submit" intent="primary" size="md" class="mt-4" :loading="saveDelivery.isPending.value">
          {{ editingDelivery ? 'Save delivery' : 'Create delivery' }}
        </BaseButton>
      </form>

      <div v-if="deliveries.length" class="surface overflow-x-auto rounded-[var(--radius-card)]">
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
                  <template v-if="d.certs?.length">
                    <span v-for="(c, i) in d.certs" :key="c.cert_id">
                      {{ i ? ', ' : '' }}{{ c.cert_name || c.cert_id
                      }}<span v-if="c.is_default && d.traefik" class="subtle text-xs"> (default)</span>
                    </span>
                  </template>
                  <template v-else>trust bundle</template>
                  <span class="subtle">→ {{ d.volume }}</span>
                </div>
                <div class="subtle mt-0.5 truncate text-xs" :title="bundleLabel(d)">
                  bundle: {{ bundleLabel(d) }} · mounted at {{ d.mount_path }}
                </div>
                <div v-if="d.last_error" class="mt-0.5 truncate text-xs" :style="{ color: 'var(--danger)' }" :title="d.last_error">{{ d.last_error }}</div>
              </td>
              <td class="py-3 pr-3"><StatusPill :status="deliveryStatus(d)" /></td>
              <td class="subtle hidden py-3 pr-3 text-right font-mono text-xs md:table-cell">
                <time v-if="d.synced_at" :title="d.synced_at">{{ new Date(d.synced_at).toLocaleString() }}</time>
                <span v-else>never</span>
              </td>
              <td class="py-3 pr-4 text-right">
                <div v-if="canEditCerts" class="flex items-center justify-end gap-1">
                  <BaseButton intent="secondary" size="xs" @click="startEditDelivery(d)">Edit</BaseButton>
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
      <p v-else-if="!addingDelivery" class="muted text-sm">No deliveries on {{ clusterName }} yet.</p>
    </section>
  </div>
</template>
