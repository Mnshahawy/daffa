<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { bytes, daffa, type BackupJob } from '@/lib/api'
import { useSession } from '@/stores/session'
import BackupSnapshots from '@/components/BackupSnapshots.vue'
import { Cap } from '@/lib/caps'
import { confirm } from '@mnshahawy/daffa-console-ui'
import { toast } from '@mnshahawy/daffa-console-ui'
import { type Status } from '@/lib/status'
import { AppIcon } from '@mnshahawy/daffa-console-ui'
import { BaseButton } from '@mnshahawy/daffa-console-ui'
import { EmptyState } from '@mnshahawy/daffa-console-ui'
import { PageHeader } from '@mnshahawy/daffa-console-ui'
import { Select } from '@mnshahawy/daffa-console-ui'
import { StatusPill } from '@mnshahawy/daffa-console-ui'
import { WizardModal, type WizardStep } from '@mnshahawy/daffa-console-ui'

const session = useSession()
const qc = useQueryClient()

const { data: jobs, isLoading } = useQuery({
  queryKey: ['backups'],
  queryFn: daffa.backups,
  refetchInterval: 10_000, // a running backup has no event to hang off
})

// The endpoint answers "which jobs may you see" (the RBAC filter); the switcher answers
// "which host are you looking at". Narrowing here rather than server-side keeps the
// ['backups'] cache env-free — VolumesView reads the same entry and would otherwise get
// this page's host imposed on it.
const mine = computed(() => (jobs.value ?? []).filter((j) => j.env_id === session.envId))

const adding = ref(false)
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
  exclude_paths: '',
  schedule: '0 3 * * *',
  storage_id: '',
  prefix: '',
  encryption: 'age' as 'age' | 'none',
  key_ids: [] as string[],
})

// The volume engine's subject is a volume, not a container — the database fields mean
// nothing to it, so the form stops asking rather than greying out.
const isVolume = computed(() => form.value.engine === 'volume')

// A sourced volume is a disposable copy of a git repo: backing it up would only snapshot what git
// already tracks, and the next sync would undo any restore. The server refuses such a job; this
// catches it in the form so the volume field says so before the rest is filled in.
const { data: volSources } = useQuery({
  queryKey: ['volume-sources'],
  queryFn: daffa.volumeSources,
  enabled: computed(() => session.can(Cap.VolsourcesView)),
})
const sourcedVolumes = computed(() => {
  const set = new Set<string>()
  for (const s of volSources.value ?? []) if (s.env_id === session.envId) set.add(s.volume)
  return set
})
const volumeIsSourced = computed(
  () => isVolume.value && sourcedVolumes.value.has(form.value.volume.trim()),
)

function toggleKey(id: string) {
  const ks = form.value.key_ids
  form.value.key_ids = ks.includes(id) ? ks.filter((k) => k !== id) : [...ks, id]
}

/**
 * A backup job is four decisions and about thirty fields, and half of them only apply to
 * one engine — as one screen it was the longest form in the console and most of it was
 * irrelevant to whoever was reading it. The order is the sentence the job describes: back
 * up THIS, including THAT, TO there, readable by THEM.
 *
 * Each `complete` here is a rule the server enforces anyway. Stating it as a gate means it
 * is said while the reader is on the step that can fix it, rather than as a 400 after the
 * other three steps have been filled in.
 */
const steps = computed<WizardStep[]>(() => [
  {
    id: 'subject',
    title: 'What to back up',
    description: 'The engine decides how the data comes out — a dump for a database, a tar for a volume.',
    complete: !volumeIsSourced.value,
  },
  {
    id: 'contents',
    title: isVolume.value ? 'Files' : 'Databases',
    description: isVolume.value
      ? 'What the tar leaves out, and what has to stop while it is taken.'
      : 'Which databases, and the credentials the dump runs as. Empty is usually right.',
  },
  {
    id: 'destination',
    title: 'Destination',
    description: 'Where each snapshot lands, and how often one is taken.',
    complete: !!targets.value?.length,
  },
  {
    id: 'encryption',
    title: 'Encryption',
    description:
      'Snapshots are encrypted on the way out, to public keys Daffa holds. The private halves are yours — this box cannot read its own backups.',
    // The server refuses "encrypt to nobody" — and it is the last thing you want to
    // discover after filling in a database password.
    complete: form.value.encryption === 'none' || form.value.key_ids.length > 0,
  },
])

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
    toast.ok('Backup job created.')
    qc.invalidateQueries({ queryKey: ['backups'] })
  },
  onError: (e) => toast.err(e, 'Could not create the job.'),
})

const run = useMutation({
  mutationFn: (id: string) => daffa.runBackup(id),
  onSuccess: () => toast.ok('Backup started.'),
  onError: (e) => toast.err(e, 'Could not start the backup.'),
  onSettled: () => qc.invalidateQueries({ queryKey: ['backups'] }),
})

const toggle = useMutation({
  mutationFn: (id: string) => daffa.toggleBackup(id),
  onSuccess: () => toast.ok('Backup job updated.'),
  onError: (e) => toast.err(e, 'Could not update the job.'),
  onSettled: () => qc.invalidateQueries({ queryKey: ['backups'] }),
})

// The schedule is the one part of a job that gets revised after the fact — 03:00 turns out to
// be when the nightly report runs. Editing it in place beats deleting and recreating the job,
// which loses the run history and asks for the database password again to change an hour.
const editingSchedule = ref<string | null>(null)
const scheduleDraft = ref('')

function onEditSchedule(j: BackupJob) {
  if (editingSchedule.value === j.id) {
    editingSchedule.value = null
    return
  }
  editingSchedule.value = j.id
  scheduleDraft.value = j.schedule ?? ''
}

const saveSchedule = useMutation({
  mutationFn: (id: string) => daffa.setBackupSchedule(id, { schedule: scheduleDraft.value }),
  onSuccess: () => {
    editingSchedule.value = null
    toast.ok('Schedule updated.')
  },
  onError: (e) => toast.err(e, 'Could not change the schedule.'),
  onSettled: () => qc.invalidateQueries({ queryKey: ['backups'] }),
})

const remove = useMutation({
  mutationFn: (id: string) => daffa.deleteBackup(id),
  onSuccess: () => toast.ok('Backup job deleted.'),
  onError: (e) => toast.err(e, 'Could not delete the job.'),
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
      :count="jobs ? mine.length : undefined"
      description="Dumps a database out of a running container — or tars a named volume — and streams it straight to object storage. Nothing is written to the host's disk."
    >
      <template #actions>
        <BaseButton v-if="session.can(Cap.BackupsEdit)" intent="primary" @click="adding = true">
          <AppIcon name="plus" class="size-4" />
          New job
        </BaseButton>
      </template>
    </PageHeader>

    <!-- New job -->
    <WizardModal
      v-if="adding"
      title="New backup job"
      :steps="steps"
      submit-label="Create job"
      :submitting="create.isPending.value"
      @close="adding = false"
      @submit="create.mutate()"
    >
      <template #subject>
        <div class="grid gap-4 sm:grid-cols-2">
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
          <div>
            <label for="b-engine" class="mb-1.5 block text-sm font-medium">Engine</label>
            <Select id="b-engine" v-model="form.engine">
              <option value="postgres">PostgreSQL</option>
              <option value="mysql">MySQL / MariaDB</option>
              <option value="mongodb">MongoDB</option>
              <option value="volume">Volume — tar of a named volume</option>
            </Select>
            <p v-if="isVolume" class="subtle mt-1 text-xs leading-relaxed">
              For file-shaped data: repositories, uploads, provisioning state. A file-level
              snapshot of a live database is torn — for databases use a database engine.
            </p>
          </div>
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
          <p class="subtle mt-1 text-xs">The database container itself — the dump runs inside it.</p>
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
          <p v-if="!volumeIsSourced" class="subtle mt-1 text-xs">
            The named volume to snapshot. No user container is touched — the daemon reads it.
          </p>
          <p
            v-else
            class="mt-1.5 rounded-[var(--radius-control)] px-3 py-2 text-xs leading-relaxed"
            :style="{
              background: 'var(--warn-soft)',
              border: '1px solid color-mix(in oklch, var(--warn) 30%, transparent)',
            }"
          >
            This volume is kept in sync from git by a volume source — its contents are a disposable
            copy of the repo, so there is nothing to back up that git does not already track. Back up
            the repository instead.
          </p>
        </div>
      </template>

      <template #contents>
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
          <div class="grid gap-4 sm:grid-cols-2">
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
          </div>
        </template>

        <template v-else>
          <div>
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
            <p class="subtle mt-1 text-xs leading-relaxed">
              Space-separated container names, stopped for the duration and restarted after — even
              when the snapshot fails. Downtime traded for consistency, chosen per job, in writing.
            </p>
          </div>
          <div>
            <label for="b-exclude" class="mb-1.5 block text-sm font-medium">
              Exclude paths <span class="subtle font-normal">(optional)</span>
            </label>
            <textarea
              id="b-exclude"
              v-model="form.exclude_paths"
              rows="3"
              placeholder="cache&#10;tmp/sessions&#10;logs"
              class="field font-mono text-xs"
              data-cursor="text"
            />
            <p class="subtle mt-1 text-xs leading-relaxed">
              One path per line, relative to the volume root — dropped from the snapshot. A directory
              drops its whole subtree. For regenerable junk (caches, logs) that need not be backed up.
            </p>
          </div>
        </template>
      </template>

      <template #destination>
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

        <template v-else>
          <div>
            <label for="b-storage" class="mb-1.5 block text-sm font-medium">Storage target</label>
            <Select id="b-storage" v-model="form.storage_id" required>
              <option value="" disabled>Choose a bucket…</option>
              <option v-for="t in targets" :key="t.id" :value="t.id">
                {{ t.name }} — {{ t.bucket }}
              </option>
            </Select>
          </div>
          <div class="grid gap-4 sm:grid-cols-2">
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
        </template>
      </template>

      <template #encryption>
        <!--
          The recipient list is not a sibling of the choice above it — it IS that choice,
          and floating it out at the same indent made the step read as three unrelated
          groups. Nesting it inside the option's own panel is most of what fixes this step.
        -->
        <div class="space-y-2">
          <label for="b-enc-age" class="flex items-center gap-2.5 text-sm">
            <input
              id="b-enc-age"
              v-model="form.encryption"
              type="radio"
              value="age"
              class="shrink-0 accent-[var(--color-accent-500)]"
            />
            Encrypt to an age public key <span class="muted">(recommended)</span>
          </label>
          <label for="b-enc-none" class="flex items-center gap-2.5 text-sm">
            <input
              id="b-enc-none"
              v-model="form.encryption"
              type="radio"
              value="none"
              class="shrink-0 accent-[var(--color-accent-500)]"
            />
            None — store the dump as plain gzip
          </label>
        </div>

        <div
          v-if="form.encryption === 'age'"
          class="rounded-[var(--radius-control)] p-3"
          :style="{ background: 'var(--surface-sunken)' }"
        >
          <template v-if="keys?.length">
            <div class="eyebrow mb-2">Readable by</div>
            <div class="space-y-1">
              <!--
                The name gets a fixed column so the recipients line up: ragged, the eye has
                to re-find the start of every key, and these are forty characters of base32
                that differ in the middle.
              -->
              <label
                v-for="k in keys"
                :key="k.id"
                class="flex items-center gap-3 text-sm"
                :for="'b-key-' + k.id"
              >
                <input
                  :id="'b-key-' + k.id"
                  type="checkbox"
                  :checked="form.key_ids.includes(k.id)"
                  class="shrink-0 accent-[var(--color-accent-500)]"
                  @change="toggleKey(k.id)"
                />
                <span class="w-28 shrink-0 truncate font-medium">{{ k.name }}</span>
                <span class="subtle min-w-0 flex-1 truncate font-mono text-xs">
                  {{ k.recipient }}
                </span>
              </label>
            </div>

            <!-- Create job is disabled until one is ticked. A disabled button with no
                 stated reason is a dead end, so the reason lives next to the fix. -->
            <p
              v-if="!form.key_ids.length"
              class="mt-2.5 text-xs leading-relaxed"
              :style="{ color: 'var(--warn)' }"
            >
              Pick at least one. A job encrypted to nobody is one Daffa will refuse to save.
            </p>
            <p v-else class="subtle mt-2.5 text-xs leading-relaxed">
              Every snapshot is encrypted to <strong>all</strong> of these, and any one private
              key restores it. Two is the number worth having — a personal key, and a
              break-glass key kept somewhere independent.
            </p>
          </template>

          <div
            v-else
            class="rounded-[var(--radius-control)] px-3 py-2 text-sm leading-relaxed"
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
        </div>

        <p v-else class="subtle text-xs leading-relaxed">
          Anyone who can read the bucket can read your database. Only reasonable if the storage
          itself is private and you accept that.
        </p>
      </template>
    </WizardModal>

    <p v-if="isLoading" class="muted text-sm">Loading…</p>

    <EmptyState
      v-else-if="!mine.length"
      icon="archive"
      title="No backup jobs on this cluster yet"
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

    <div v-else-if="mine.length" class="space-y-3">
      <div v-for="j in mine" :key="j.id" class="surface overflow-hidden rounded-[var(--radius-card)]">
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
              ><span v-if="j.schedule"> · {{ j.schedule }} UTC</span
              ><span v-else> · manual only</span>
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

          <div class="flex w-full flex-wrap items-center gap-1 sm:w-auto sm:shrink-0">
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
                intent="ghost"
                size="xs"
                :aria-expanded="editingSchedule === j.id"
                @click="onEditSchedule(j)"
              >
                <AppIcon name="pencil" class="size-3" />
                Schedule
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

        <form
          v-if="editingSchedule === j.id"
          class="border-t px-4 py-3"
          :style="{ borderColor: 'var(--border)', background: 'var(--surface-sunken)' }"
          @submit.prevent="saveSchedule.mutate(j.id)"
        >
          <label :for="'b-sched-' + j.id" class="mb-1.5 block text-sm font-medium">Schedule</label>
          <div class="flex flex-wrap items-center gap-2">
            <input
              :id="'b-sched-' + j.id"
              v-model="scheduleDraft"
              placeholder="0 3 * * *"
              class="field max-w-56 font-mono text-xs"
              data-cursor="text"
            />
            <BaseButton type="submit" intent="primary" size="xs" :loading="saveSchedule.isPending.value">
              Save
            </BaseButton>
            <BaseButton intent="ghost" size="xs" @click="editingSchedule = null">Cancel</BaseButton>
          </div>
          <p class="subtle mt-1.5 text-xs">
            Cron, in UTC. Empty = manual only. The new time takes effect immediately — the
            paused/running state and everything else about the job are untouched.
          </p>
        </form>

        <BackupSnapshots v-if="expanded === j.id" :job="j" />
      </div>
    </div>
  </div>
</template>
