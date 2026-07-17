<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { ApiError, bytes, daffa, type BackupJob } from '@/lib/api'
import { useSession } from '@/stores/session'
import BackupSnapshots from '@/components/BackupSnapshots.vue'
import { Cap } from '@/lib/caps'
import { confirm } from '@/lib/confirm'
import { type Status } from '@/lib/status'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import EmptyState from '@/components/ui/EmptyState.vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import StatusPill from '@/components/ui/StatusPill.vue'

const session = useSession()
const qc = useQueryClient()

const { data: jobs, isLoading } = useQuery({
  queryKey: ['backups'],
  queryFn: daffa.backups,
  refetchInterval: 10_000, // a running backup has no event to hang off
})

const adding = ref(false)
const error = ref('')
const expanded = ref<string | null>(null)

// Storage is chosen, not retyped. The bucket and its credentials live in Settings →
// Storage, where they were tested when they were saved.
const { data: targets } = useQuery({
  queryKey: ['storage'],
  queryFn: daffa.storage,
  // Only fetched for someone who could actually pick one — otherwise this is a
  // guaranteed 403 on every visit to the page.
  enabled: computed(() => session.can(Cap.StorageView)),
})

// Keys are chosen, not pasted — they live in Settings → Certificates, where generating
// one forces the private half to be downloaded before anything else happens.
const { data: keys } = useQuery({
  queryKey: ['keys'],
  queryFn: daffa.encryptionKeys,
  enabled: computed(() => session.canAnywhere(Cap.KeysView)),
})

const form = ref({
  name: '',
  container: '',
  engine: 'postgres' as BackupJob['engine'],
  databases: '',
  db_user: '',
  db_password: '',
  volume: '',
  stop_containers: '',
  schedule: '0 3 * * *',
  storage_id: '',
  prefix: '',
  encryption: 'age' as 'age' | 'none',
  key_ids: [] as string[],
})

// The volume engine's subject is a volume, not a container — the database fields mean
// nothing to it, so the form stops asking rather than greying out.
const isVolume = computed(() => form.value.engine === 'volume')

function toggleKey(id: string) {
  const ks = form.value.key_ids
  form.value.key_ids = ks.includes(id) ? ks.filter((k) => k !== id) : [...ks, id]
}

// Preselect when there is only one — a required dropdown with a single option is a
// question with one answer.
watch(
  targets,
  (t) => {
    if (!form.value.storage_id && t?.length === 1) form.value.storage_id = t[0].id
  },
  { immediate: true },
)

const create = useMutation({
  mutationFn: () => daffa.createBackup({ ...form.value, env_id: session.envId }),
  onSuccess: () => {
    adding.value = false
    error.value = ''
    qc.invalidateQueries({ queryKey: ['backups'] })
  },
  onError: (e) => {
    error.value = e instanceof ApiError ? e.message : 'Could not create the job.'
  },
})

const run = useMutation({
  mutationFn: (id: string) => daffa.runBackup(id),
  onSettled: () => qc.invalidateQueries({ queryKey: ['backups'] }),
})

const toggle = useMutation({
  mutationFn: (id: string) => daffa.toggleBackup(id),
  onSettled: () => qc.invalidateQueries({ queryKey: ['backups'] }),
})

const remove = useMutation({
  mutationFn: (id: string) => daffa.deleteBackup(id),
  onSettled: () => qc.invalidateQueries({ queryKey: ['backups'] }),
})

async function onRemove(j: BackupJob) {
  const ok = await confirm({
    title: `Delete the backup job ${j.name}?`,
    body: 'The snapshots already in your bucket are NOT deleted — Daffa never touches them. This only stops future backups: the schedule, its settings and its run history go, and nothing will dump this database again.',
    confirmLabel: 'Delete',
    intent: 'danger',
    // A job that has never run has nothing behind it. A job that HAS run is the thing standing
    // between this database and a bad morning, so deleting it is worth typing the name for.
    typeToConfirm: j.last_run ? j.name : undefined,
  })
  if (!ok) return
  remove.mutate(j.id)
}

async function onPause(j: BackupJob) {
  // Resuming costs nothing. Pausing means the backups quietly stop happening, which is exactly
  // the failure this page exists to prevent — so it is the one that gets asked about.
  if (j.enabled) {
    const ok = await confirm({
      title: `Pause the backup job ${j.name}?`,
      body: 'It stops running on its schedule until you resume it. Nothing already in the bucket is touched, but no new snapshot will be taken — and a paused job looks exactly like a working one on the morning you need it.',
      confirmLabel: 'Pause',
      intent: 'caution',
    })
    if (!ok) return
  }
  toggle.mutate(j.id)
}

/**
 * The last run is the only thing that matters at a glance: a red pill here is the whole reason
 * this page exists. A backup in flight pulses — the next poll may say something different.
 */
function runStatus(j: BackupJob): Status {
  const r = j.last_run
  if (!r) return { tone: 'neutral', label: 'Never run' }
  switch (r.status) {
    case 'running':
      return { tone: 'accent', label: 'Backing up', live: true }
    case 'ok':
      return { tone: 'success', label: 'Backed up', detail: bytes(r.bytes) }
    case 'failed':
      return { tone: 'danger', label: 'Failed' }
    default:
      return { tone: 'neutral', label: r.status }
  }
}
</script>

<template>
  <div>
    <PageHeader
      title="Backups"
      :count="jobs?.length"
      description="Dumps a database out of a running container — or tars a named volume — and streams it straight to object storage. Nothing is written to the host's disk."
    >
      <template #actions>
        <BaseButton
          v-if="session.can(Cap.BackupsEdit)"
          :intent="adding ? 'secondary' : 'primary'"
          @click="adding = !adding"
        >
          <AppIcon v-if="!adding" name="plus" class="size-4" />
          {{ adding ? 'Cancel' : 'New job' }}
        </BaseButton>
      </template>
    </PageHeader>

    <!-- New job -->
    <form
      v-if="adding"
      class="surface mb-6 space-y-5 rounded-[var(--radius-card)] p-5"
      @submit.prevent="create.mutate()"
    >
      <div class="grid gap-4 sm:grid-cols-3">
        <div>
          <label for="b-name" class="mb-1.5 block text-sm font-medium">Name</label>
          <input
            id="b-name"
            v-model="form.name"
            required
            placeholder="prod-db"
            class="field"
            data-cursor="text"
          />
        </div>
        <div v-if="!isVolume">
          <label for="b-container" class="mb-1.5 block text-sm font-medium">Container</label>
          <input
            id="b-container"
            v-model="form.container"
            required
            placeholder="platform-postgres-1"
            class="field font-mono text-xs"
            data-cursor="text"
          />
          <p class="subtle mt-1 text-xs">
            The database container itself — the dump runs inside it.
          </p>
        </div>
        <div v-else>
          <label for="b-volume" class="mb-1.5 block text-sm font-medium">Volume</label>
          <input
            id="b-volume"
            v-model="form.volume"
            required
            placeholder="forgejo-data"
            class="field font-mono text-xs"
            data-cursor="text"
          />
          <p class="subtle mt-1 text-xs">
            The named volume to snapshot. No user container is touched — the daemon reads it.
          </p>
        </div>
        <div>
          <label for="b-engine" class="mb-1.5 block text-sm font-medium">Engine</label>
          <select id="b-engine" v-model="form.engine" class="field">
            <option value="postgres">PostgreSQL</option>
            <option value="mysql">MySQL / MariaDB</option>
            <option value="mongodb">MongoDB</option>
            <option value="volume">Volume — tar of a named volume</option>
          </select>
          <p v-if="isVolume" class="subtle mt-1 text-xs">
            For file-shaped data: repositories, uploads, provisioning state. A file-level snapshot
            of a live database is torn — for databases use a database engine.
          </p>
        </div>
      </div>

      <div class="grid gap-4 sm:grid-cols-4">
        <template v-if="!isVolume">
          <div>
            <label for="b-databases" class="mb-1.5 block text-sm font-medium">Databases</label>
            <input
              id="b-databases"
              v-model="form.databases"
              placeholder="all"
              class="field font-mono text-xs"
              data-cursor="text"
            />
            <p class="subtle mt-1 text-xs">Empty = everything, roles included.</p>
          </div>
          <div>
            <label for="b-user" class="mb-1.5 block text-sm font-medium">DB user</label>
            <input
              id="b-user"
              v-model="form.db_user"
              placeholder="postgres"
              class="field"
              data-cursor="text"
            />
          </div>
          <div>
            <label for="b-password" class="mb-1.5 block text-sm font-medium">DB password</label>
            <input
              id="b-password"
              v-model="form.db_password"
              type="password"
              placeholder="often not needed"
              class="field"
            />
          </div>
        </template>
        <div v-else class="sm:col-span-3">
          <label for="b-stop" class="mb-1.5 block text-sm font-medium">
            Stop during snapshot <span class="subtle font-normal">(optional)</span>
          </label>
          <input
            id="b-stop"
            v-model="form.stop_containers"
            placeholder="forgejo"
            class="field font-mono text-xs"
            data-cursor="text"
          />
          <p class="subtle mt-1 text-xs">
            Space-separated container names, stopped for the duration and restarted after — even
            when the snapshot fails. Downtime traded for consistency, chosen per job, in writing.
          </p>
        </div>
        <div>
          <label for="b-schedule" class="mb-1.5 block text-sm font-medium">Schedule</label>
          <input
            id="b-schedule"
            v-model="form.schedule"
            placeholder="0 3 * * *"
            class="field font-mono text-xs"
            data-cursor="text"
          />
          <p class="subtle mt-1 text-xs">Cron, in UTC. Empty = manual only.</p>
        </div>
      </div>

      <div>
        <div class="eyebrow mb-2">Destination</div>

        <div
          v-if="!targets?.length"
          class="rounded-[var(--radius-control)] px-3 py-2 text-sm"
          :style="{
            background: 'var(--warn-soft)',
            border: '1px solid color-mix(in oklch, var(--warn) 30%, transparent)',
          }"
        >
          No storage targets yet.
          <RouterLink
            :to="{ name: 'settings-storage' }"
            class="font-medium transition hover:text-[var(--accent-text)]"
          >
            Add one under Settings → Storage
          </RouterLink>
          first.
        </div>

        <div v-else class="grid gap-4 sm:grid-cols-2">
          <div>
            <label for="b-storage" class="mb-1.5 block text-sm font-medium">Storage target</label>
            <select id="b-storage" v-model="form.storage_id" required class="field">
              <option value="" disabled>Choose a bucket…</option>
              <option v-for="t in targets" :key="t.id" :value="t.id">
                {{ t.name }} — {{ t.bucket }}
              </option>
            </select>
          </div>
          <div>
            <label for="b-prefix" class="mb-1.5 block text-sm font-medium">
              Path within the bucket
            </label>
            <input
              id="b-prefix"
              v-model="form.prefix"
              placeholder="e.g. prod/postgres (optional)"
              class="field font-mono text-xs"
              data-cursor="text"
            />
          </div>
        </div>
      </div>

      <div>
        <div class="eyebrow mb-2">Encryption</div>
        <label for="b-enc-age" class="mb-2 flex items-center gap-2 text-sm">
          <input
            id="b-enc-age"
            v-model="form.encryption"
            type="radio"
            value="age"
            class="accent-[var(--color-accent-500)]"
          />
          Encrypt to an age public key <span class="muted">(recommended)</span>
        </label>
        <label for="b-enc-none" class="mb-3 flex items-center gap-2 text-sm">
          <input
            id="b-enc-none"
            v-model="form.encryption"
            type="radio"
            value="none"
            class="accent-[var(--color-accent-500)]"
          />
          None — store the dump as plain gzip
        </label>

        <template v-if="form.encryption === 'age'">
          <div v-if="keys?.length" class="space-y-1.5">
            <label
              v-for="k in keys"
              :key="k.id"
              class="flex items-center gap-2 text-sm"
              :for="'b-key-' + k.id"
            >
              <input
                :id="'b-key-' + k.id"
                type="checkbox"
                :checked="form.key_ids.includes(k.id)"
                class="accent-[var(--color-accent-500)]"
                @change="toggleKey(k.id)"
              />
              <span class="font-medium">{{ k.name }}</span>
              <span class="subtle truncate font-mono text-xs">{{ k.recipient }}</span>
            </label>
          </div>
          <div
            v-else
            class="rounded-[var(--radius-control)] px-3 py-2 text-sm"
            :style="{
              background: 'var(--warn-soft)',
              border: '1px solid color-mix(in oklch, var(--warn) 30%, transparent)',
            }"
          >
            No encryption keys yet.
            <RouterLink
              :to="{ name: 'settings-certificates' }"
              class="font-medium transition hover:text-[var(--accent-text)]"
            >
              Generate one under Settings → Certificates
            </RouterLink>
            first — the private half is yours to download, and Daffa never stores it.
          </div>
          <p class="subtle mt-2 text-xs leading-relaxed">
            Every snapshot is encrypted to <strong>all</strong> selected keys; any one private key
            restores. Pick two — a personal key and a break-glass key held somewhere independent —
            so losing one does not mean losing the backups.
          </p>
        </template>
        <p v-else class="subtle text-xs leading-relaxed">
          Anyone who can read the bucket can read your database. Only reasonable if the storage
          itself is private and you accept that.
        </p>
      </div>

      <p v-if="error" class="text-sm" :style="{ color: 'var(--danger)' }">{{ error }}</p>

      <BaseButton
        type="submit"
        intent="primary"
        size="md"
        :loading="create.isPending.value"
        :disabled="!targets?.length"
      >
        Create job
      </BaseButton>
    </form>

    <p v-if="isLoading" class="muted text-sm">Loading…</p>

    <EmptyState
      v-else-if="!jobs?.length && !adding"
      icon="archive"
      title="No backup jobs yet"
      body="A backup job dumps a database out of its running container on a schedule and streams it straight to S3-compatible storage — encrypted to your public key, and never written to the host's disk. Without one, the only copy of that database is the container it lives in."
    >
      <template #action>
        <BaseButton
          v-if="session.can(Cap.BackupsEdit)"
          intent="primary"
          size="md"
          @click="adding = true"
        >
          <AppIcon name="plus" class="size-4" />
          Add backup job
        </BaseButton>
      </template>
    </EmptyState>

    <div v-else-if="jobs?.length" class="space-y-3">
      <div v-for="j in jobs" :key="j.id" class="surface overflow-hidden rounded-[var(--radius-card)]">
        <div class="flex flex-wrap items-start gap-3 p-4">
          <!-- The last run is the only thing that matters at a glance: a red dot here is
               the whole reason this page exists. -->
          <div class="min-w-0 flex-1">
            <div class="flex flex-wrap items-center gap-2">
              <StatusPill :status="runStatus(j)" />

              <span class="font-medium">{{ j.name }}</span>
              <span class="subtle font-mono text-xs">{{ j.engine }}</span>

              <span
                v-if="!j.enabled"
                class="rounded-md px-1.5 py-0.5 text-xs font-medium"
                :style="{ background: 'var(--surface-sunken)', color: 'var(--text-muted)' }"
              >
                paused
              </span>

              <span
                v-if="j.encryption === 'none'"
                class="rounded-md px-1.5 py-0.5 text-xs font-medium"
                :style="{ background: 'var(--warn-soft)', color: 'var(--warn)' }"
                title="Anyone who can read the bucket can read this database"
              >
                unencrypted
              </span>
            </div>

            <div class="subtle mt-1 truncate font-mono text-xs">
              {{ j.engine === 'volume' ? j.volume : j.container }} → {{ j.storage_name || j.bucket
              }}<span v-if="j.prefix">/{{ j.prefix }}</span
              ><span v-if="j.schedule"> · {{ j.schedule }} UTC</span>
            </div>

            <div
              v-if="j.last_run"
              class="mt-1 text-xs"
              :class="j.last_run.status === 'failed' ? '' : 'muted'"
              :style="j.last_run.status === 'failed' ? { color: 'var(--danger)' } : undefined"
            >
              <template v-if="j.last_run.status === 'failed'">
                last run failed: {{ j.last_run.error }}
              </template>
              <template v-else-if="j.last_run.status === 'running'">running…</template>
              <template v-else>
                last backup <span class="font-mono">{{ bytes(j.last_run.bytes) }}</span> ·
                <time :title="j.last_run.started_at">
                  {{ new Date(j.last_run.started_at).toLocaleString() }}
                </time>
              </template>
            </div>
            <div v-else class="muted mt-1 text-xs">never run</div>
          </div>

          <div class="flex shrink-0 items-center gap-1">
            <!-- Listing snapshots is reading, so it is gated on backups.view like the rest
                 of the page — not on being able to change the job. -->
            <BaseButton
              intent="ghost"
              size="xs"
              :aria-expanded="expanded === j.id"
              @click="expanded = expanded === j.id ? null : j.id"
            >
              <AppIcon
                name="chevronRight"
                class="size-3.5 transition-transform"
                :class="expanded === j.id ? 'rotate-90' : ''"
              />
              Snapshots
            </BaseButton>

            <template v-if="session.can(Cap.BackupsEdit)">
              <BaseButton
                intent="primary"
                size="xs"
                :disabled="run.isPending.value"
                @click="run.mutate(j.id)"
              >
                <AppIcon name="play" class="size-3" />
                Run now
              </BaseButton>

              <BaseButton
                :intent="j.enabled ? 'caution' : 'secondary'"
                size="xs"
                :disabled="toggle.isPending.value"
                @click="onPause(j)"
              >
                <AppIcon :name="j.enabled ? 'pause' : 'play'" class="size-3" />
                {{ j.enabled ? 'Pause' : 'Resume' }}
              </BaseButton>

              <BaseButton
                intent="danger"
                size="xs"
                :disabled="remove.isPending.value"
                @click="onRemove(j)"
              >
                <AppIcon name="trash" class="size-3.5" />
                Delete
              </BaseButton>
            </template>
          </div>
        </div>

        <BackupSnapshots v-if="expanded === j.id" :job="j" />
      </div>
    </div>
  </div>
</template>
