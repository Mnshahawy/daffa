<script setup lang="ts">
import { ref } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import { bytes, daffa, type BackupJob, type Snapshot } from '@/lib/api'
import { Cap } from '@/lib/caps'
import { useSession } from '@/stores/session'
import { type Status } from '@/lib/status'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import CopyButton from '@/components/ui/CopyButton.vue'
import StatusPill from '@/components/ui/StatusPill.vue'

const props = defineProps<{ job: BackupJob }>()

const session = useSession()

const {
  data: snapshots,
  isLoading,
  error,
} = useQuery({
  // The getter is the key's reactive dependency: vue-query treats `queryKey` specially and
  // CALLS any function it finds there (cloneDeepUnref, utils.ts), from inside the computed()
  // that recomputes the options — so the id reaches the key as a value AND the query refetches
  // when it changes. Two snapshot lists on screen at once therefore get their own cache
  // entries, which is the whole point: they belong to different jobs.
  queryKey: ['snapshots', () => props.job.id],
  queryFn: () => daffa.snapshots(props.job.id),
})

const selected = ref<Snapshot | null>(null)

// Restore is a CLI command, and the UI's job is to hand you the exact one — not to ask
// for your private key. If this page had a "paste your key here" box, then the key would
// travel to the server, and the server could read every backup it has ever taken. The
// whole reason for encrypting to a public key is that it cannot.
function restoreCommand(s: Snapshot): string {
  const parts = [
    'daffa restore',
    `--server ${location.origin}`,
    `--job ${props.job.id}`,
    `--snapshot ${s.key}`,
    `--user <you>`,
  ]
  if (s.encrypted) parts.push('--identity ~/key.txt')
  return parts.join(' \\\n  ')
}

/** Encryption is a state of the object in the bucket, so it is said the way every other state
 *  in Daffa is said. An unencrypted snapshot is a database sitting in a bucket in the clear. */
function encryptionStatus(s: Snapshot): Status {
  return s.encrypted
    ? { tone: 'success', label: 'Encrypted' }
    : { tone: 'warn', label: 'Not encrypted' }
}
</script>

<template>
  <div
    class="border-t px-4 py-4"
    :style="{ borderColor: 'var(--border)', background: 'var(--surface-sunken)' }"
  >
    <p v-if="isLoading" class="muted text-sm">Reading the bucket…</p>
    <p v-else-if="error" class="text-sm" :style="{ color: 'var(--danger)' }">
      Could not list the bucket: {{ (error as Error).message }}
    </p>
    <p v-else-if="!snapshots?.length" class="muted text-sm">
      No snapshots in the bucket yet. Run the job and the first one appears here.
    </p>

    <template v-else>
      <div class="eyebrow mb-2">Snapshots ({{ snapshots.length }})</div>

      <div class="max-h-56 overflow-y-auto">
        <table class="w-full text-xs">
          <thead>
            <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
              <th class="eyebrow py-1.5 pr-3 text-left font-medium">Key</th>
              <th class="eyebrow py-1.5 pr-3 text-right font-medium">Size</th>
              <th class="eyebrow py-1.5 pr-3 text-right font-medium">Taken</th>
              <th class="eyebrow py-1.5 pr-3 text-left font-medium">Encryption</th>
              <th class="eyebrow py-1.5 text-right font-medium">Restore</th>
            </tr>
          </thead>

          <tbody>
            <tr
              v-for="s in snapshots"
              :key="s.key"
              class="border-b last:border-0"
              :style="{
                borderColor: 'var(--border)',
                background: selected?.key === s.key ? 'var(--surface-raised)' : undefined,
              }"
            >
              <td class="max-w-0 truncate py-1.5 pr-3 font-mono" :title="s.key">{{ s.key }}</td>
              <td class="muted py-1.5 pr-3 text-right font-mono">{{ bytes(s.size) }}</td>
              <td class="muted py-1.5 pr-3 text-right font-mono">
                <time :title="s.modified">{{ new Date(s.modified).toLocaleDateString() }}</time>
              </td>
              <td class="py-1.5 pr-3">
                <StatusPill :status="encryptionStatus(s)" />
              </td>
              <td class="py-1.5 text-right">
                <BaseButton
                  intent="ghost"
                  size="xs"
                  :aria-expanded="selected?.key === s.key"
                  @click="selected = selected?.key === s.key ? null : s"
                >
                  <AppIcon
                    name="chevronRight"
                    class="size-3 transition-transform"
                    :class="selected?.key === s.key ? 'rotate-90' : ''"
                  />
                  Restore
                </BaseButton>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- The restore instructions.
           Gated on backups.restore, because the command will not work without it: the
           server refuses both the download and the restore. Printing a recipe that ends in
           a 403 is worse than not printing one. -->
      <div
        v-if="selected && session.can(Cap.BackupsRestore)"
        class="mt-4 rounded-[var(--radius-control)] border p-3"
        :style="{ borderColor: 'var(--border)' }"
      >
        <p class="mb-2 text-xs font-medium">Restore this snapshot</p>
        <p class="muted mb-2 text-xs leading-relaxed">
          <template v-if="selected.encrypted">
            Run this on your own machine. Your age private key never leaves it — Daffa cannot
            decrypt this snapshot, and neither can anyone who copies the bucket.
          </template>
          <template v-else>
            This snapshot is <strong>not encrypted</strong>. Anyone who can read the bucket can read
            the database.
          </template>
        </p>

        <div
          class="flex items-start gap-2 rounded-[var(--radius-control)] p-2.5 font-mono text-xs"
          :style="{ background: 'var(--surface)' }"
        >
          <pre class="flex-1 overflow-x-auto whitespace-pre">{{ restoreCommand(selected) }}</pre>
          <CopyButton intent="secondary" size="xs" class="shrink-0" :text="restoreCommand(selected!)" />
        </div>

        <!-- The volume engine's two refusals are worth saying before the command runs into
             them: in use ⇒ stop the consumers first, non-empty ⇒ --wipe, explicitly. -->
        <p v-if="job.engine === 'volume'" class="muted mt-2 text-xs leading-relaxed">
          The server refuses to restore while any container mounts the volume — stop them first.
          It also refuses a non-empty volume, because a merge of two states of the data is garbage
          that only shows up later: add <code class="font-mono">--wipe</code> to empty it first,
          explicitly.
        </p>
        <p v-else class="muted mt-2 text-xs">
          It will ask you to confirm before overwriting the live database.
        </p>
      </div>
    </template>
  </div>
</template>
