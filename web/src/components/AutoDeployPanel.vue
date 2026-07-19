<script setup lang="ts">
import { ref, watch } from 'vue'
import { useQueryClient } from '@tanstack/vue-query'
import { daffa, type Stack } from '@/lib/api'
import { toast } from '@/lib/toast'
import { confirm } from '@/lib/confirm'
import BaseButton from './ui/BaseButton.vue'
import CopyButton from './ui/CopyButton.vue'

const props = defineProps<{ stack: Stack; canWrite: boolean }>()

const qc = useQueryClient()
const enabled = ref(props.stack.auto_deploy)
const watchPaths = ref(props.stack.watch_paths ?? '')
const busy = ref(false)

// The secret is shown exactly once, when it is minted. It is sealed in the database
// afterwards and there is no way to read it back — which is the point, and is why the
// panel says so rather than leaving someone hunting for it later.
const secret = ref('')

watch(
  () => props.stack,
  (s) => {
    enabled.value = s.auto_deploy
    watchPaths.value = s.watch_paths ?? ''
  },
)

const webhookUrl = `${location.origin}/webhooks/stacks/${props.stack.id}`

async function save(rotate = false) {
  busy.value = true
  try {
    const r = await daffa.setAutoDeploy(props.stack.id, {
      enabled: enabled.value,
      watch_paths: watchPaths.value,
      rotate,
    })
    // A freshly minted secret gets its own one-time reveal below — that IS the feedback, so a
    // toast on top of it would be redundant. Otherwise, confirm the save landed.
    if (r.secret) secret.value = r.secret
    else toast.ok('Auto-deploy saved.')
    await qc.invalidateQueries({ queryKey: ['stack'] })
  } catch (e) {
    toast.err(e, 'Could not save.')
    enabled.value = props.stack.auto_deploy // put the toggle back where it was
  } finally {
    busy.value = false
  }
}

// Rotating does not destroy anything you can point at, but it does silently stop auto-deploy
// until the new secret reaches the git server — and a webhook that has quietly stopped firing
// is the kind of thing nobody notices until a release does not happen. Caution, and asked.
async function askRotate() {
  const ok = await confirm({
    title: 'Rotate the webhook secret?',
    body:
      'The current secret stops working the moment the new one is minted. Pushes will not deploy ' +
      'until you paste the new secret into your git server’s webhook settings — and it is shown ' +
      'only once.',
    confirmLabel: 'Rotate',
    intent: 'caution',
  })
  if (ok) void save(true)
}

</script>

<template>
  <div class="surface overflow-hidden rounded-[var(--radius-card)]">
    <div
      class="flex items-center justify-between border-b px-4 py-2"
      :style="{ borderColor: 'var(--border)' }"
    >
      <span class="text-sm font-medium">Auto-deploy on push</span>

      <label v-if="canWrite" for="auto-deploy" class="flex items-center gap-2 text-xs">
        <input
          id="auto-deploy"
          v-model="enabled"
          type="checkbox"
          :disabled="busy"
          class="accent-[var(--accent)]"
          @change="save()"
        />
        <span class="muted">{{ enabled ? 'On' : 'Off' }}</span>
      </label>
      <span v-else class="muted text-xs">{{ stack.auto_deploy ? 'On' : 'Off' }}</span>
    </div>

    <div class="p-4">
      <p v-if="!enabled" class="muted text-sm">
        Off. A push to
        <code class="font-mono">{{ stack.git_ref || 'the tracked branch' }}</code> changes nothing
        until someone deploys.
      </p>

      <template v-else>
        <!-- Watched paths -->
        <label for="watch-paths" class="mb-1.5 block text-sm font-medium">
          Deploy when these files change
        </label>
        <textarea
          id="watch-paths"
          v-model="watchPaths"
          :disabled="!canWrite || busy"
          rows="3"
          spellcheck="false"
          :placeholder="stack.git_path || 'docker-compose.yml'"
          class="field font-mono text-xs"
          data-cursor="text"
        />
        <p class="muted mt-1 text-xs">
          One glob per line — <code class="font-mono">*</code> stays within a path segment,
          <code class="font-mono">**</code> crosses them. Leave empty to watch only
          <code class="font-mono">{{ stack.git_path || 'docker-compose.yml' }}</code
          >.
        </p>

        <div v-if="stack.watching?.length" class="muted mt-2 text-xs">
          Currently watching:
          <code v-for="p in stack.watching" :key="p" class="ml-1 font-mono">{{ p }}</code>
        </div>

        <BaseButton
          v-if="canWrite"
          intent="primary"
          class="mt-3"
          :loading="busy"
          @click="save()"
        >
          Save
        </BaseButton>

        <!-- Webhook -->
        <div class="mt-5 border-t pt-4" :style="{ borderColor: 'var(--border)' }">
          <p class="mb-2 text-sm font-medium">Webhook</p>
          <p class="muted mb-2 text-xs">
            Add this to your repository's webhook settings. Content type
            <code class="font-mono">application/json</code>, event: push.
          </p>

          <div
            class="mb-2 flex items-start gap-2 rounded-[var(--radius-control)] p-2.5 font-mono text-xs"
            :style="{ background: 'var(--surface-sunken)' }"
          >
            <code class="flex-1 break-all">{{ webhookUrl }}</code>
            <CopyButton intent="ghost" size="xs" :text="webhookUrl" />
          </div>

          <!-- The one time the secret is visible -->
          <div
            v-if="secret"
            class="rounded-[var(--radius-control)] border p-3"
            :style="{
              background: 'var(--accent-soft)',
              borderColor: 'color-mix(in oklch, var(--accent) 40%, transparent)',
            }"
          >
            <p class="mb-1.5 text-xs font-medium">Secret — copy it now</p>
            <div
              class="flex items-start gap-2 rounded-[var(--radius-control)] p-2 font-mono text-xs"
              :style="{ background: 'var(--surface)' }"
            >
              <code class="flex-1 break-all">{{ secret }}</code>
              <CopyButton intent="ghost" size="xs" :text="secret" />
            </div>
            <p class="muted mt-1.5 text-xs">
              This is shown once. It is stored encrypted and cannot be read back — if you lose it,
              rotate it and update the git server.
            </p>
          </div>

          <div v-else-if="stack.has_secret" class="flex items-center gap-3">
            <span class="muted text-xs">A secret is configured.</span>
            <!-- Recoverable, but it stops auto-deploy until the git server is updated: caution. -->
            <BaseButton
              v-if="canWrite"
              intent="caution"
              size="xs"
              :loading="busy"
              @click="askRotate"
            >
              Rotate secret
            </BaseButton>
          </div>
        </div>
      </template>
    </div>
  </div>
</template>
