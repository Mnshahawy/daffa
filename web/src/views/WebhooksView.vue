<script setup lang="ts">
/**
 * Webhooks: the door a git server pushes through, linked to any number of stacks and volume
 * sources. One webhook for a repository that feeds five stacks — or one organisation-level
 * webhook for every repository — and each linked target decides for itself (repository,
 * branch, watched paths) whether a push deploys it. See .ai/webhooks.md.
 *
 * Linking happens on the target (a stack's Settings tab, a volume source's form); this page
 * is where webhooks are made, rotated and retired, and where "why did my push not deploy?"
 * is answered: the last delivery, verdict by verdict.
 */
import { computed, ref, watchEffect } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { daffa, type Webhook, type WebhookDeliveryResult } from '@/lib/api'
import { Cap } from '@/lib/caps'
import { ago, absolute } from '@/lib/format'
import { useSession } from '@/stores/session'
import WebhookSecretReveal from '@/components/WebhookSecretReveal.vue'
import {
  AppIcon,
  BaseButton,
  CopyButton,
  EmptyState,
  confirm,
  toast,
} from '@mnshahawy/daffa-console-ui'

const qc = useQueryClient()
const session = useSession()

const { data: webhooks, isLoading } = useQuery({ queryKey: ['webhooks'], queryFn: daffa.webhooks })
const { data: environments } = useQuery({ queryKey: ['environments'], queryFn: daffa.environments })

// A fleet-wide webhook needs webhooks.edit fleet-wide — the monitor rule — so the "every
// cluster" option only appears for a global holder; everyone else picks a cluster they hold it on.
const canCreateFleet = computed(() => session.can(Cap.WebhooksEdit, ''))
const creatableEnvs = computed(() =>
  (environments.value ?? []).filter((e) => session.can(Cap.WebhooksEdit, e.id)),
)
const canCreate = computed(() => canCreateFleet.value || creatableEnvs.value.length > 0)
const canEdit = (w: Webhook) => session.can(Cap.WebhooksEdit, w.env_id ?? '')

const form = ref({ name: '', env_id: '' })
// '' means fleet-wide, which only a global holder may pick; everyone else starts on the
// first cluster they can create on, so the select never shows an option that is not there.
watchEffect(() => {
  if (!canCreateFleet.value && !form.value.env_id && creatableEnvs.value.length) {
    form.value.env_id = creatableEnvs.value[0].id
  }
})
// The just-minted secret, keyed by webhook — shown on that webhook's card until dismissed.
const minted = ref<{ id: string; secret: string } | null>(null)

const create = useMutation({
  mutationFn: () => daffa.createWebhook({ name: form.value.name.trim(), env_id: form.value.env_id || undefined }),
  onSuccess: (r) => {
    minted.value = { id: r.webhook.id, secret: r.secret }
    form.value = { name: '', env_id: form.value.env_id }
    qc.invalidateQueries({ queryKey: ['webhooks'] })
  },
  onError: (e) => toast.err(e, 'Could not create the webhook.'),
})

const rotate = useMutation({
  mutationFn: (id: string) => daffa.rotateWebhook(id),
  onSuccess: (r) => {
    minted.value = { id: r.webhook.id, secret: r.secret }
    qc.invalidateQueries({ queryKey: ['webhooks'] })
  },
  onError: (e) => toast.err(e, 'Could not rotate the secret.'),
})

const remove = useMutation({
  mutationFn: (id: string) => daffa.deleteWebhook(id),
  onSuccess: () => toast.ok('Webhook deleted.'),
  onSettled: () => qc.invalidateQueries({ queryKey: ['webhooks'] }),
  onError: (e) => toast.err(e, 'Could not delete the webhook.'),
})

// Rotating destroys nothing you can point at, but it silently stops every linked target
// deploying until the new secret reaches the git server. Caution, and asked.
async function askRotate(w: Webhook) {
  const n = w.targets.length + w.hidden_targets
  const ok = await confirm({
    title: `Rotate the secret of ${w.name}?`,
    body:
      `The current secret stops working the moment the new one is minted. Pushes will not reach ` +
      `${n === 1 ? 'the one target' : `any of the ${n} targets`} linked to it until you paste the ` +
      `new secret into your git server — and it is shown only once.`,
    confirmLabel: 'Rotate',
    intent: 'caution',
  })
  if (ok) rotate.mutate(w.id)
}

async function askRemove(w: Webhook) {
  const ok = await confirm({
    title: `Delete ${w.name}?`,
    body: 'Pushes to its URL will answer 404 from now on. Remove the webhook from your git server too.',
    confirmLabel: 'Delete',
    intent: 'danger',
  })
  if (ok) remove.mutate(w.id)
}

const url = (w: Webhook) => `${location.origin}${w.path}`

// A verdict's colour: acted, acted-by-something-else, did nothing, went wrong.
function tone(r: WebhookDeliveryResult): string {
  switch (r.status) {
    case 'deployed':
    case 'synced':
    case 'deploying':
    case 'syncing':
      return 'var(--success)'
    case 'failed':
      return 'var(--danger)'
    case 'skipped':
      return 'var(--warn)'
    default:
      return 'var(--text-subtle)'
  }
}
</script>

<template>
  <div>
    <div class="mb-5 min-w-0">
      <h2 class="text-base font-semibold">Webhooks</h2>
      <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
        Where your git server tells Daffa about a push. Link one webhook to every stack and volume
        source a repository feeds — each decides for itself, by repository, branch and watched
        paths, whether a push deploys it. Link webhooks from a stack's Settings tab or a volume
        source's form.
      </p>
    </div>

    <form
      v-if="canCreate"
      class="surface mb-6 rounded-[var(--radius-card)] p-5"
      @submit.prevent="create.mutate()"
    >
      <div class="grid gap-4 sm:grid-cols-2">
        <div>
          <label for="wh-name" class="mb-1.5 block text-sm font-medium">Name</label>
          <input
            id="wh-name"
            v-model="form.name"
            required
            placeholder="platform-repo"
            class="field"
            data-cursor="text"
          />
          <p class="subtle mt-1 text-xs">Usually the repository it listens to.</p>
        </div>
        <div>
          <label for="wh-env" class="mb-1.5 block text-sm font-medium">Links to</label>
          <select id="wh-env" v-model="form.env_id" class="field">
            <option v-if="canCreateFleet" value="">Stacks on any cluster</option>
            <option v-for="e in creatableEnvs" :key="e.id" :value="e.id">
              Stacks on {{ e.name }}
            </option>
          </select>
          <p class="subtle mt-1 text-xs">
            A fleet-wide webhook suits one repository deploying staging from one branch and
            production from another.
          </p>
        </div>
      </div>
      <BaseButton
        type="submit"
        intent="primary"
        size="md"
        class="mt-4"
        :loading="create.isPending.value"
      >
        Create webhook
      </BaseButton>
    </form>

    <p v-if="isLoading" class="muted text-sm">Loading…</p>

    <EmptyState
      v-else-if="!webhooks?.length"
      icon="plug"
      title="No webhooks yet"
      body="Without one, stacks deploy when someone presses Deploy. Create a webhook, add its URL and secret to your repository, and link it to the stacks that repository feeds."
    />

    <div v-else class="space-y-4">
      <div
        v-for="w in webhooks"
        :key="w.id"
        class="surface rounded-[var(--radius-card)] p-4"
      >
        <div class="flex flex-wrap items-start justify-between gap-3">
          <div class="min-w-0">
            <div class="flex items-center gap-2">
              <AppIcon name="plug" class="size-4 shrink-0" />
              <span class="font-medium">{{ w.name }}</span>
              <span class="rounded-md px-1.5 py-0.5 text-xs" :style="{ background: 'var(--surface-sunken)', color: 'var(--text-muted)' }">{{ w.env_name ?? 'any cluster' }}</span>
            </div>
            <div class="mt-1.5 flex items-center gap-2 font-mono text-xs">
              <code class="subtle break-all">POST {{ url(w) }}</code>
              <CopyButton intent="ghost" size="xs" :text="url(w)" />
            </div>
          </div>
          <div v-if="canEdit(w)" class="flex shrink-0 gap-2">
            <BaseButton intent="caution" size="xs" :disabled="rotate.isPending.value" @click="askRotate(w)">
              Rotate secret
            </BaseButton>
            <!-- Deleting one that still drives something is refused server-side; say so here. -->
            <BaseButton
              intent="danger"
              size="xs"
              :disabled="remove.isPending.value || w.targets.length + w.hidden_targets > 0"
              :title="w.targets.length + w.hidden_targets > 0 ? 'Unlink its stacks and volume sources first' : undefined"
              @click="askRemove(w)"
            >
              <AppIcon name="trash" class="size-3.5" />
              Delete
            </BaseButton>
          </div>
        </div>

        <WebhookSecretReveal
          v-if="minted?.id === w.id"
          class="mt-3"
          :path="w.path"
          :secret="minted.secret"
        />

        <div class="mt-4 grid gap-4 md:grid-cols-2">
          <div>
            <div class="eyebrow mb-1.5">Drives</div>
            <p v-if="!w.targets.length && !w.hidden_targets" class="subtle text-xs">
              Nothing yet — link it from a stack's Settings tab.
            </p>
            <ul class="space-y-1 text-sm">
              <li v-for="t in w.targets" :key="t.id" class="flex items-center gap-2">
                <AppIcon :name="t.kind === 'stack' ? 'layers' : 'database'" class="size-3.5 shrink-0" />
                <RouterLink
                  v-if="t.kind === 'stack'"
                  :to="{ name: 'stack', params: { id: t.id } }"
                  class="hover:underline"
                >
                  {{ t.name }}
                </RouterLink>
                <span v-else>{{ t.name }}</span>
                <span class="subtle text-xs">{{ t.env_name }}</span>
                <span v-if="t.paused" class="rounded-md px-1.5 py-0.5 text-xs" :style="{ background: 'var(--surface-sunken)', color: 'var(--text-muted)' }" title="Linked, but auto-deploy is off — pushes are acknowledged and ignored">paused</span>
              </li>
              <li v-if="w.hidden_targets" class="subtle text-xs">
                and {{ w.hidden_targets }} on clusters you cannot see
              </li>
            </ul>
          </div>

          <div>
            <div class="eyebrow mb-1.5">Last delivery</div>
            <p v-if="!w.last_delivery_at" class="subtle text-xs">
              Nothing has arrived yet. A forge's "test" or "ping" button will show up here.
            </p>
            <template v-else>
              <p class="text-xs" :title="absolute(w.last_delivery_at)">
                {{
                  w.last_delivery?.event === 'ping'
                    ? 'A ping'
                    : w.last_delivery?.event === 'test'
                      ? 'A test delivery (nothing deployed)'
                      : 'A push'
                }}
                <template v-if="w.last_delivery?.ref">
                  to <code class="font-mono">{{ w.last_delivery.ref.replace('refs/heads/', '') }}</code>
                </template>
                {{ ago(w.last_delivery_at) }}
              </p>
              <ul v-if="w.last_delivery?.results.length" class="mt-1.5 space-y-1 text-xs">
                <li v-for="r in w.last_delivery.results" :key="r.kind + r.id" class="flex gap-2">
                  <span class="shrink-0 font-medium" :style="{ color: tone(r) }">{{ r.status }}</span>
                  <span class="min-w-0">
                    {{ r.name }}
                    <RouterLink
                      v-if="r.deployment_id"
                      :to="{ name: 'deployment', params: { id: r.deployment_id } }"
                      class="subtle hover:underline"
                    >
                      — view deployment
                    </RouterLink>
                    <span v-if="r.reason" class="subtle"> — {{ r.reason }}</span>
                  </span>
                </li>
              </ul>
              <p v-else-if="w.last_delivery?.event === 'push'" class="subtle mt-1.5 text-xs">
                Nothing linked to act on.
              </p>
            </template>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
