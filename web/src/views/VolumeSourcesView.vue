<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { ApiError, daffa, type VolumeSource, type VolumeSourceRequest } from '@/lib/api'
import { toast } from '@/lib/toast'
import { useSession } from '@/stores/session'
import { Cap } from '@/lib/caps'
import { confirm } from '@/lib/confirm'
import { ago, shortSha } from '@/lib/format'
import { type Status } from '@/lib/status'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import CopyButton from '@/components/ui/CopyButton.vue'
import EmptyState from '@/components/ui/EmptyState.vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import Select from '@/components/ui/Select.vue'
import StatusPill from '@/components/ui/StatusPill.vue'

const session = useSession()
const qc = useQueryClient()

const { data: sources, isLoading } = useQuery({
  queryKey: ['volume-sources'],
  queryFn: daffa.volumeSources,
  // A source created seconds ago is still syncing in the background — poll until it settles.
  refetchInterval: 10_000,
})

const { data: environments } = useQuery({
  queryKey: ['environments'],
  queryFn: () => daffa.environments(),
})

// Stacks and credentials are chosen, not retyped — and only fetched for someone whose
// request would not be a guaranteed 403.
const { data: stacks } = useQuery({
  queryKey: ['stacks'],
  queryFn: daffa.stacks,
  enabled: computed(() => session.canAnywhere(Cap.StacksView)),
})
const { data: gitCreds } = useQuery({
  queryKey: ['gitcreds'],
  queryFn: daffa.gitCredentials,
  enabled: computed(() => session.canAnywhere(Cap.GitCredsView)),
})

const canEditAnywhere = computed(() => session.canAnywhere(Cap.VolsourcesEdit))

// Only hosts where this person may actually create a source. Offering the others would be
// a form that ends in a 403.
const editableEnvs = computed(() =>
  (environments.value ?? []).filter((e) => session.can(Cap.VolsourcesEdit, e.id)),
)

// ── the form ──────────────────────────────────────────────────────────────────

const blank = () => ({
  env_id: '',
  volume: '',
  source_kind: 'git' as 'git' | 'inline',
  git_url: '',
  git_ref: 'main',
  git_path: '',
  git_credential_id: '',
  // Inline only: the files delivered into the volume, authored right here.
  files: [] as { path: string; content: string }[],
  uid: 0,
  gid: 0,
  stack_id: '',
  restart_targets: '',
  auto_sync: false,
})
const form = ref(blank())
const open = ref(false)
const editing = ref<VolumeSource | null>(null)
const busy = ref(false)

// The secret is shown exactly once, when it is minted. It is sealed in the database
// afterwards and there is no way to read it back — the stacks auto-deploy pattern verbatim.
const minted = ref<{ source: VolumeSource; secret: string } | null>(null)

function startAdd() {
  editing.value = null
  form.value = blank()
  // Default to the host you are looking at, when it is one you can edit.
  form.value.env_id = editableEnvs.value.some((e) => e.id === session.envId)
    ? session.envId
    : (editableEnvs.value[0]?.id ?? '')
  open.value = true
}

async function startEdit(s: VolumeSource) {
  editing.value = s
  form.value = {
    env_id: s.env_id,
    volume: s.volume,
    source_kind: (s.source_kind ?? 'git') as 'git' | 'inline',
    git_url: s.git_url ?? '',
    git_ref: s.git_ref ?? '',
    git_path: s.git_path ?? '',
    git_credential_id: s.git_credential_id ?? '',
    files: [],
    uid: s.uid,
    gid: s.gid,
    stack_id: s.stack_id ?? '',
    restart_targets: s.restart_targets ?? '',
    auto_sync: s.auto_sync,
  }
  open.value = true
  // The list omits file contents (they can be large); fetch them for the editor.
  if (form.value.source_kind === 'inline') {
    try {
      const full = await daffa.volumeSource(s.id)
      form.value.files = (full.files ?? []).map((f) => ({ path: f.path, content: f.content }))
    } catch {
      // Leave the editor empty rather than block it — the operator can re-enter the files.
    }
  }
}

function addFile() {
  form.value.files.push({ path: '', content: '' })
}
function removeFile(i: number) {
  form.value.files.splice(i, 1)
}

function close() {
  open.value = false
  editing.value = null
  form.value = blank()
}

// Stacks that could plausibly be linked: same host, or a deploy there could never mount
// the volume — the server refuses the mismatch, so it is not offered.
const stackChoices = computed(() =>
  (stacks.value ?? []).filter((s) => s.env_id === form.value.env_id),
)
watch(
  () => form.value.env_id,
  () => {
    if (form.value.stack_id && !stackChoices.value.some((s) => s.id === form.value.stack_id)) {
      form.value.stack_id = ''
    }
  },
)

async function save(rotate = false) {
  // Switching an inline source to git throws away the files stored in Daffa. It is one-way and the
  // server refuses the reverse, so it earns a confirm — the same caution the stack switch shows.
  if (editing.value?.source_kind === 'inline' && form.value.source_kind === 'git') {
    const ok = await confirm({
      title: `Switch ${form.value.volume} to git?`,
      body:
        'The inline files stored in Daffa are discarded and the repository becomes the source of ' +
        'truth. The volume’s contents are replaced from the repo on the next sync; its UID/GID and ' +
        'any linked stack are kept.',
      confirmLabel: 'Switch to git',
      intent: 'caution',
    })
    if (!ok) return
  }

  busy.value = true
  try {
    const body: VolumeSourceRequest = { ...form.value, rotate }
    const editingSource = editing.value
    const r = editingSource
      ? await daffa.updateVolumeSource(editingSource.id, body)
      : await daffa.createVolumeSource(body)
    // A minted secret gets its own one-time reveal panel — that IS the success feedback, so a
    // toast on top of it would be redundant. Otherwise, confirm the save landed.
    if (r.webhook_secret) minted.value = { source: r.source, secret: r.webhook_secret }
    else toast.ok(editingSource ? 'Source saved.' : 'Source created.')
    await qc.invalidateQueries({ queryKey: ['volume-sources'] })
    close()
  } catch (e) {
    toast.err(e, 'Could not save the volume source.')
  } finally {
    busy.value = false
  }
}

// Rotating destroys nothing you can point at, but it does silently stop webhook syncs until
// the new secret reaches the git server — the stack rotate's caution, verbatim.
async function askRotate() {
  const ok = await confirm({
    title: 'Rotate the webhook secret?',
    body:
      'The current secret stops working the moment the new one is minted. Pushes will not sync ' +
      'until you paste the new secret into your git server’s webhook settings — and it is shown ' +
      'only once.',
    confirmLabel: 'Rotate',
    intent: 'caution',
  })
  if (ok) void save(true)
}

// ── row actions ───────────────────────────────────────────────────────────────

// Sync is synchronous on purpose: the row's status pill and this error line together are
// the outcome, not "started". Errors are kept per source so a failing sync on one row does
// not paint the whole page red.
const syncErrors = ref<Record<string, string>>({})

const sync = useMutation({
  mutationFn: (id: string) => daffa.syncVolumeSource(id),
  onSuccess: (_data, id) => {
    delete syncErrors.value[id]
  },
  onError: (e, id) => {
    syncErrors.value[id] = e instanceof ApiError ? e.message : 'The sync failed.'
  },
  onSettled: () => qc.invalidateQueries({ queryKey: ['volume-sources'] }),
})

const remove = useMutation({
  mutationFn: (id: string) => daffa.deleteVolumeSource(id),
  onSuccess: () => toast.ok('Source deleted.'),
  onError: (e) => toast.err(e, 'Could not delete the volume source.'),
  onSettled: () => qc.invalidateQueries({ queryKey: ['volume-sources'] }),
})

async function onRemove(s: VolumeSource) {
  const ok = await confirm({
    title: `Delete the source for ${s.volume}?`,
    body:
      'The volume and everything in it stay exactly where they are — the consumer may still be ' +
      'running, and yanking config out from under it would be a worse surprise than a stale file. ' +
      'Daffa just stops syncing: the volume becomes an ordinary volume, removable under Volumes ' +
      'whenever you decide.',
    confirmLabel: 'Delete',
    intent: 'danger',
  })
  if (!ok) return
  remove.mutate(s.id)
}

// ── how a source reads ────────────────────────────────────────────────────────

function sourceStatus(s: VolumeSource): Status {
  switch (s.status) {
    case 'ok':
      return { tone: 'success', label: 'Synced' }
    case 'error':
      return { tone: 'danger', label: 'Error' }
    default:
      // pending: the background sync is running right now; the next poll may say ok.
      return { tone: 'accent', label: 'Syncing', live: true }
  }
}

/** repo @ ref // subtree for git; a plain label for inline (its content is on the editor). */
function sourceLine(s: VolumeSource): string {
  if (s.source_kind === 'inline') return 'inline — files managed in Daffa'
  let line = s.git_url ?? ''
  if (s.git_ref) line += ` @ ${s.git_ref}`
  if (s.git_path) line += ` // ${s.git_path}`
  return line
}

function webhookUrl(id: string): string {
  return `${location.origin}/webhooks/volume-sources/${id}`
}

</script>

<template>
  <div>
    <PageHeader
      title="Volume sources"
      :count="sources?.length"
      description="A source declares that a named volume's contents come from a git subtree — Daffa creates the volume, fills it, and keeps it current. For config that belongs in a repo; a volume holding precious data wants a backup job instead."
    >
      <template #actions>
        <BaseButton
          v-if="canEditAnywhere"
          :intent="open ? 'secondary' : 'primary'"
          @click="open ? close() : startAdd()"
        >
          <AppIcon v-if="!open" name="plus" class="size-4" />
          {{ open ? 'Cancel' : 'New source' }}
        </BaseButton>
      </template>
    </PageHeader>

    <!-- The one time the webhook secret is visible. It survives the form closing, because
         it appears exactly once and closing the form must not eat it. -->
    <div
      v-if="minted"
      class="mb-6 rounded-[var(--radius-card)] border p-4"
      :style="{
        background: 'var(--accent-soft)',
        borderColor: 'color-mix(in oklch, var(--accent) 40%, transparent)',
      }"
    >
      <div class="mb-2 flex items-start justify-between gap-2">
        <p class="text-sm font-medium">
          Webhook secret for {{ minted.source.volume }} — copy it now
        </p>
        <BaseButton intent="ghost" size="xs" aria-label="Dismiss" @click="minted = null">
          <AppIcon name="x" class="size-3.5" />
        </BaseButton>
      </div>

      <p class="muted mb-2 text-xs">
        Add this to your repository's webhook settings. Content type
        <code class="font-mono">application/json</code>, event: push.
      </p>
      <div
        class="mb-2 flex items-start gap-2 rounded-[var(--radius-control)] p-2.5 font-mono text-xs"
        :style="{ background: 'var(--surface)' }"
      >
        <code class="flex-1 break-all">{{ webhookUrl(minted.source.id) }}</code>
        <CopyButton intent="ghost" size="xs" :text="webhookUrl(minted.source.id)" />
      </div>

      <div
        class="flex items-start gap-2 rounded-[var(--radius-control)] p-2.5 font-mono text-xs"
        :style="{ background: 'var(--surface)' }"
      >
        <code class="flex-1 break-all">{{ minted.secret }}</code>
        <CopyButton intent="ghost" size="xs" :text="minted.secret" />
      </div>
      <p class="muted mt-1.5 text-xs">
        This is shown once. It is stored encrypted and cannot be read back — if you lose it, edit
        the source and rotate it, then update the git server.
      </p>
    </div>

    <!-- ── New / edit ─────────────────────────────────────────────────────────── -->
    <form
      v-if="open"
      class="surface mb-6 space-y-4 rounded-[var(--radius-card)] p-5"
      @submit.prevent="save()"
    >
      <h3 class="text-sm font-semibold">
        {{ editing ? `Edit the source for ${editing.volume}` : 'New volume source' }}
      </h3>

      <div class="grid gap-4 sm:grid-cols-3">
        <div>
          <label for="vs-env" class="mb-1.5 block text-sm font-medium">Host</label>
          <!-- Host and volume are what a source IS — retargeting one would strand the old
               volume with a manifest nothing owns. The server ignores them on update, so
               the form says so up front instead of silently dropping an edit. -->
          <Select
            id="vs-env"
            v-model="form.env_id"
            required
            :disabled="!!editing"
          >
            <option value="" disabled>Choose a cluster…</option>
            <option v-for="e in editing ? (environments ?? []) : editableEnvs" :key="e.id" :value="e.id">
              {{ e.name }}
            </option>
          </Select>
          <p v-if="editing" class="subtle mt-1 text-xs">
            Fixed. To move a source, delete it and create another — both halves explicit.
          </p>
        </div>
        <div>
          <label for="vs-volume" class="mb-1.5 block text-sm font-medium">Volume</label>
          <input
            id="vs-volume"
            v-model="form.volume"
            required
            :disabled="!!editing"
            placeholder="traefik-dynamic"
            class="field font-mono text-xs"
            data-cursor="text"
          />
          <p class="subtle mt-1 text-xs">
            Created if it does not exist — a fresh node deploys clean. Mount it
            <code class="font-mono">:ro</code> in the consumer.
          </p>
        </div>
        <div>
          <label for="vs-stack" class="mb-1.5 block text-sm font-medium">
            Linked stack <span class="subtle font-normal">(optional)</span>
          </label>
          <Select id="vs-stack" v-model="form.stack_id">
            <option value="">None</option>
            <option v-for="s in stackChoices" :key="s.id" :value="s.id">{{ s.name }}</option>
          </Select>
          <p class="subtle mt-1 text-xs">
            Linked, the stack's deploys sync this source first — and fail loudly if the sync fails,
            so a stack never comes up against config Daffa knows is stale.
          </p>
        </div>
      </div>

      <div>
        <label class="mb-1.5 block text-sm font-medium">Source</label>
        <!-- Locked only for a source that is ALREADY git: the server refuses git → inline (the
             files live in the repo, so there is nothing to convert back). An inline source can be
             pointed at a repo, which is the switch below. -->
        <Select v-model="form.source_kind" :disabled="!!editing && editing.source_kind !== 'inline'">
          <option value="git">Git repository — synced from a repo</option>
          <option value="inline">Inline — files authored here</option>
        </Select>
        <p class="subtle mt-1 text-xs">
          Inline is for a volume with no repo behind it — Traefik's static config and dynamic
          middlewares, edited here and delivered on deploy.
          <template v-if="!editing">
            An inline source can later be switched to git; the reverse is not offered.
          </template>
          <template v-else-if="editing.source_kind === 'git'">
            A git source cannot be converted back to inline — its files live in the repo.
          </template>
        </p>
        <p
          v-if="editing?.source_kind === 'inline' && form.source_kind === 'git'"
          class="mt-1 text-xs"
          :style="{ color: 'var(--warn)' }"
        >
          Switching to git discards the inline files stored here; the repository becomes the source
          of truth and the volume is refilled from it on the next sync.
        </p>
      </div>

      <template v-if="form.source_kind === 'git'">
        <div class="grid gap-4 sm:grid-cols-3">
          <div class="sm:col-span-2">
            <label for="vs-url" class="mb-1.5 block text-sm font-medium">Repository URL</label>
            <input
              id="vs-url"
              v-model="form.git_url"
              :required="form.source_kind === 'git'"
              placeholder="https://git.example.com/team/infra.git"
              class="field font-mono text-xs"
              data-cursor="text"
            />
          </div>
          <div>
            <label for="vs-ref" class="mb-1.5 block text-sm font-medium">Branch or tag</label>
            <input id="vs-ref" v-model="form.git_ref" class="field font-mono text-xs" data-cursor="text" />
          </div>
        </div>

        <div class="grid gap-4 sm:grid-cols-2">
          <div>
            <label for="vs-path" class="mb-1.5 block text-sm font-medium">Directory in the repo</label>
            <input
              id="vs-path"
              v-model="form.git_path"
              placeholder="traefik/dynamic"
              class="field font-mono text-xs"
              data-cursor="text"
            />
            <p class="subtle mt-1 text-xs">
              The subtree delivered into the volume. Empty means the whole repository.
            </p>
          </div>
          <div>
            <label for="vs-cred" class="mb-1.5 block text-sm font-medium">Credential</label>
            <Select id="vs-cred" v-model="form.git_credential_id">
              <option value="">None — public repository</option>
              <option v-for="c in gitCreds" :key="c.id" :value="c.id">
                {{ c.name }} ({{ c.kind === 'ssh' ? 'SSH' : 'token' }})
              </option>
            </Select>
            <p class="subtle mt-1 text-xs">
              <RouterLink
                v-if="session.can(Cap.GitCredsView)"
                :to="{ name: 'settings-git' }"
                class="transition hover:text-[var(--accent-text)]"
              >
                Manage credentials in Settings → Git
              </RouterLink>
              <span v-else>Ask an admin to add one under Settings → Git.</span>
            </p>
          </div>
        </div>
      </template>

      <div v-else class="space-y-3">
        <div class="flex items-center justify-between">
          <label class="block text-sm font-medium">Files</label>
          <BaseButton intent="ghost" size="xs" @click="addFile">Add file</BaseButton>
        </div>
        <p v-if="form.files.length === 0" class="subtle text-xs">
          No files yet. Add one — e.g. <code class="font-mono">traefik.yml</code>, or
          <code class="font-mono">dynamic/middlewares.yml</code>. Paths are relative; use
          <code class="font-mono">/</code> for subdirectories.
        </p>
        <div v-for="(f, i) in form.files" :key="i" class="rounded-md border border-[var(--border)] p-3">
          <div class="mb-2 flex items-center gap-2">
            <input
              v-model="f.path"
              placeholder="dynamic/middlewares.yml"
              class="field flex-1 font-mono text-xs"
              data-cursor="text"
            />
            <BaseButton intent="ghost" size="xs" @click="removeFile(i)">Remove</BaseButton>
          </div>
          <textarea
            v-model="f.content"
            rows="6"
            spellcheck="false"
            class="field w-full font-mono text-xs"
            data-cursor="text"
          />
        </div>
      </div>

      <div class="grid gap-4 sm:grid-cols-4">
        <div>
          <label for="vs-uid" class="mb-1.5 block text-sm font-medium">Owner uid</label>
          <input
            id="vs-uid"
            v-model.number="form.uid"
            type="number"
            min="0"
            class="field font-mono text-xs"
          />
        </div>
        <div>
          <label for="vs-gid" class="mb-1.5 block text-sm font-medium">Owner gid</label>
          <input
            id="vs-gid"
            v-model.number="form.gid"
            type="number"
            min="0"
            class="field font-mono text-xs"
          />
          <p class="subtle mt-1 text-xs">
            Ownership of the written files — a consumer that drops privileges can still read its
            config.
          </p>
        </div>
        <div class="sm:col-span-2">
          <label for="vs-restart" class="mb-1.5 block text-sm font-medium">
            Restart after a changed sync <span class="subtle font-normal">(optional)</span>
          </label>
          <input
            id="vs-restart"
            v-model="form.restart_targets"
            placeholder="my-app-1 my-worker-1"
            class="field font-mono text-xs"
            data-cursor="text"
          />
          <p class="subtle mt-1 text-xs">
            Space-separated container names, bounced only when content actually changed — for
            consumers that cannot hot-reload. Leave empty for Traefik and friends.
          </p>
        </div>
      </div>

      <div v-if="form.source_kind === 'git'">
        <label for="vs-autosync" class="flex items-center gap-2 text-sm">
          <input
            id="vs-autosync"
            v-model="form.auto_sync"
            type="checkbox"
            class="accent-[var(--color-accent-500)]"
          />
          Sync on push — a webhook from your git server triggers the sync
        </label>
        <p class="subtle mt-1 text-xs">
          Edit the config in the repo, merge, done — no redeploy. Saving with this on mints a
          webhook secret, shown once.
        </p>

        <div
          v-if="editing && form.auto_sync && editing.has_webhook_secret"
          class="mt-2 flex items-center gap-3"
        >
          <span class="muted text-xs">A secret is configured.</span>
          <!-- Recoverable, but it stops push-syncs until the git server is updated: caution. -->
          <BaseButton intent="caution" size="xs" :loading="busy" @click="askRotate">
            Rotate secret
          </BaseButton>
        </div>
      </div>

      <div class="flex items-center gap-2">
        <BaseButton type="submit" intent="primary" size="md" :loading="busy">
          {{ editing ? 'Save' : 'Create source' }}
        </BaseButton>
        <BaseButton v-if="editing" intent="secondary" size="md" @click="close">Cancel</BaseButton>
      </div>
    </form>

    <p v-if="isLoading" class="muted text-sm">Loading…</p>

    <EmptyState
      v-else-if="!sources?.length && !open"
      icon="file"
      title="No volume sources yet"
      body="A volume source fills a named volume from a git subtree and keeps it current — Traefik dynamic config, init scripts, provisioning — so the repo stays the only source of truth. Secrets don't belong here: those ride sealed stack env vars or cert deliveries."
    >
      <template #action>
        <BaseButton v-if="canEditAnywhere" intent="primary" size="md" @click="startAdd">
          <AppIcon name="plus" class="size-4" />
          New source
        </BaseButton>
      </template>
    </EmptyState>

    <div v-else-if="sources?.length" class="space-y-3">
      <div
        v-for="s in sources"
        :key="s.id"
        class="surface rounded-[var(--radius-card)] p-4"
      >
        <div class="flex flex-wrap items-start gap-3">
          <div class="min-w-0 flex-1">
            <div class="flex flex-wrap items-center gap-2">
              <StatusPill :status="sourceStatus(s)" />

              <span class="break-all font-medium">{{ s.volume }}</span>

              <span
                v-if="s.env_name"
                class="subtle rounded-md border px-1.5 py-0.5 font-mono text-[10px] whitespace-nowrap"
                :style="{ borderColor: 'var(--border)' }"
              >
                {{ s.env_name }}
              </span>

              <span
                v-if="s.auto_sync"
                class="rounded-md px-1.5 py-0.5 text-xs font-medium"
                :style="{ background: 'var(--accent-soft)', color: 'var(--accent-text)' }"
                title="A push to the repository syncs this volume"
              >
                sync on push
              </span>
            </div>

            <div class="subtle mt-1 break-all font-mono text-xs">
              {{ sourceLine(s) }}
              <template v-if="s.stack_name"> · linked to {{ s.stack_name }}</template>
            </div>

            <!-- Which commit's config is live — the whole reason synced_commit exists. -->
            <div v-if="s.synced_commit" class="muted mt-1 text-xs">
              live: <span class="font-mono">{{ shortSha(s.synced_commit) }}</span>
              <template v-if="s.synced_at">
                · <time :title="s.synced_at">synced {{ ago(s.synced_at) }}</time>
              </template>
            </div>

            <div
              v-if="s.status === 'error' && s.last_error"
              class="mt-1 text-xs"
              :style="{ color: 'var(--danger)' }"
              :title="s.last_error"
            >
              {{ s.last_error }}
            </div>
            <div v-if="syncErrors[s.id]" class="mt-1 text-xs" :style="{ color: 'var(--danger)' }">
              {{ syncErrors[s.id] }}
            </div>

            <!-- Say-so lines: the sync went through, and you should read these anyway. -->
            <div
              v-if="s.warnings?.length"
              class="mt-2 space-y-1 rounded-[var(--radius-control)] px-3 py-2 text-xs leading-relaxed"
              :style="{ background: 'var(--warn-soft)' }"
            >
              <p v-for="w in s.warnings" :key="w" class="flex items-start gap-2">
                <AppIcon name="alert" class="mt-0.5 size-3.5 shrink-0" :style="{ color: 'var(--warn)' }" />
                <span>{{ w }}</span>
              </p>
              <p class="muted pl-5">
                Secrets belong in sealed stack env vars or cert deliveries — a config volume is
                delivered in the clear.
              </p>
            </div>

            <!-- The webhook endpoint, on the row that answers to it. The secret is not here —
                 it was shown once, when it was minted. -->
            <div v-if="s.auto_sync" class="mt-2 flex items-center gap-2 font-mono text-xs">
              <code class="subtle break-all">POST {{ webhookUrl(s.id) }}</code>
              <CopyButton intent="ghost" size="xs" :text="webhookUrl(s.id)" />
            </div>
          </div>

          <div v-if="session.can(Cap.VolsourcesEdit, s.env_id)" class="flex shrink-0 items-center gap-1">
            <BaseButton
              intent="primary"
              size="xs"
              :loading="sync.isPending.value && sync.variables.value === s.id"
              :disabled="sync.isPending.value"
              @click="sync.mutate(s.id)"
            >
              <AppIcon name="restart" class="size-3" />
              Sync now
            </BaseButton>

            <BaseButton intent="secondary" size="xs" @click="startEdit(s)">
              <AppIcon name="pencil" class="size-3.5" />
              Edit
            </BaseButton>

            <BaseButton
              intent="danger"
              size="xs"
              :disabled="remove.isPending.value"
              @click="onRemove(s)"
            >
              <AppIcon name="trash" class="size-3.5" />
              Delete
            </BaseButton>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
