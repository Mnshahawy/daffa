<script setup lang="ts">
/**
 * A stack's auto-deploy: which webhook's pushes may deploy it, whether it is listening right
 * now, and which files a push must touch. The webhook itself — URL, secret, last delivery —
 * lives in Settings → Webhooks, because one webhook can drive many stacks. "Create one for
 * this stack" is the one-step path for the common single-stack repository.
 */
import { computed, ref, watch } from 'vue'
import { useQuery, useQueryClient } from '@tanstack/vue-query'
import { daffa, type Stack } from '@/lib/api'
import { Cap } from '@/lib/caps'
import { useSession } from '@/stores/session'
import WebhookSecretReveal from '@/components/WebhookSecretReveal.vue'
import { BaseButton, CopyButton, toast } from '@mnshahawy/daffa-console-ui'

const props = defineProps<{ stack: Stack; canWrite: boolean }>()

const qc = useQueryClient()
const session = useSession()

const enabled = ref(props.stack.auto_deploy)
const webhookId = ref(props.stack.webhook_id ?? '')
const watchPaths = ref(props.stack.watch_paths ?? '')
const busy = ref(false)
// The secret of a webhook this panel just created — shown once, here, then never again.
const minted = ref<{ path: string; secret: string } | null>(null)

watch(
  () => props.stack,
  (s) => {
    enabled.value = s.auto_deploy
    webhookId.value = s.webhook_id ?? ''
    watchPaths.value = s.watch_paths ?? ''
  },
)

const canSeeWebhooks = computed(() => session.canAnywhere(Cap.WebhooksView))
const { data: webhooks } = useQuery({
  queryKey: ['webhooks'],
  queryFn: daffa.webhooks,
  enabled: canSeeWebhooks,
})
// Linkable: fleet-wide, or on this stack's cluster — the rule the server enforces.
const linkable = computed(() =>
  (webhooks.value ?? []).filter((w) => !w.env_id || w.env_id === props.stack.env_id),
)
const linked = computed(() => (webhooks.value ?? []).find((w) => w.id === webhookId.value))
const canCreateWebhook = computed(() => session.can(Cap.WebhooksEdit, props.stack.env_id))
const url = (path: string) => `${location.origin}${path}`

async function save(opts: { create?: boolean } = {}) {
  if (enabled.value && !webhookId.value && !opts.create) {
    toast.err(new Error('Pick the webhook whose pushes should deploy this stack, or create one.'), 'Not saved.')
    return
  }
  busy.value = true
  try {
    const r = await daffa.setAutoDeploy(props.stack.id, {
      enabled: opts.create ? true : enabled.value,
      webhook_id: opts.create ? undefined : webhookId.value,
      create_webhook: opts.create || undefined,
      watch_paths: watchPaths.value,
    })
    // A freshly minted secret gets its own one-time reveal — that IS the feedback.
    if (r.secret && r.webhook_id) minted.value = { path: `/webhooks/${r.webhook_id}`, secret: r.secret }
    else toast.ok('Auto-deploy saved.')
    await Promise.all([
      qc.invalidateQueries({ queryKey: ['stack'] }),
      qc.invalidateQueries({ queryKey: ['webhooks'] }),
    ])
  } catch (e) {
    toast.err(e, 'Could not save.')
    enabled.value = props.stack.auto_deploy // put the toggle back where it was
  } finally {
    busy.value = false
  }
}

// The toggle saves at once when there is something to save; turning it on with no webhook
// picked just reveals the picker.
function onToggle() {
  if (enabled.value && !webhookId.value) return
  void save()
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
          @change="onToggle"
        />
        <span class="muted">{{ enabled ? 'On' : 'Off' }}</span>
      </label>
      <span v-else class="muted text-xs">{{ stack.auto_deploy ? 'On' : 'Off' }}</span>
    </div>

    <div class="p-4">
      <WebhookSecretReveal v-if="minted" class="mb-4" :path="minted.path" :secret="minted.secret" />

      <p v-if="!enabled" class="muted text-sm">
        Off. A push to
        <code class="font-mono">{{ stack.git_ref || 'the tracked branch' }}</code> changes nothing
        until someone deploys.
        <template v-if="stack.webhook_id">
          The webhook stays linked, so turning this back on needs nothing from the git server.
        </template>
      </p>

      <template v-else>
        <!-- Which webhook -->
        <label for="ad-webhook" class="mb-1.5 block text-sm font-medium">Webhook</label>
        <div class="flex flex-wrap items-center gap-2">
          <select
            id="ad-webhook"
            v-model="webhookId"
            class="field max-w-sm"
            :disabled="!canWrite || busy || !canSeeWebhooks"
          >
            <option value="">— pick a webhook —</option>
            <option v-for="w in linkable" :key="w.id" :value="w.id">
              {{ w.name }}{{ w.env_name ? '' : ' (any cluster)' }}
            </option>
            <!-- Linked to one this person cannot list: keep it selectable, by id. -->
            <option v-if="webhookId && !linked" :value="webhookId">the linked webhook</option>
          </select>
          <BaseButton
            v-if="canWrite && canCreateWebhook && !webhookId"
            intent="secondary"
            size="sm"
            :loading="busy"
            @click="save({ create: true })"
          >
            Create one for this stack
          </BaseButton>
        </div>
        <div v-if="linked" class="mt-2 flex items-center gap-2 font-mono text-xs">
          <code class="subtle break-all">POST {{ url(linked.path) }}</code>
          <CopyButton intent="ghost" size="xs" :text="url(linked.path)" />
        </div>
        <p class="muted mt-1 text-xs">
          A push deploys this stack only if it is for
          <code class="font-mono break-all">{{ stack.git_url }}</code>, on
          <code class="font-mono">{{ stack.git_ref || 'any branch' }}</code>, and touches a watched
          file. Secrets, rotation and the last delivery are in
          <RouterLink :to="{ name: 'settings-webhooks' }" class="underline">Settings → Webhooks</RouterLink>.
        </p>

        <!-- Watched paths -->
        <label for="watch-paths" class="mb-1.5 mt-4 block text-sm font-medium">
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
          <code v-for="p in stack.watching" :key="p" class="ml-1 font-mono break-all">{{ p }}</code>
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
      </template>
    </div>
  </div>
</template>
