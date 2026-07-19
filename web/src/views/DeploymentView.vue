<script setup lang="ts">
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { computed, watchEffect } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import type { RouteLocationRaw } from 'vue-router'
import DeploymentLog from '@/components/DeploymentLog.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import StatusPill from '@/components/ui/StatusPill.vue'
import { daffa } from '@/lib/api'
import { Cap } from '@/lib/caps'
import { confirm } from '@/lib/confirm'
import { toast } from '@/lib/toast'
import { absolute, actionLabel, actionNoun, duration, shortSha } from '@/lib/format'
import { deploymentStatus } from '@/lib/status'
import { setTitle } from '@/lib/title'
import { useSession } from '@/stores/session'

const route = useRoute()
const router = useRouter()
const session = useSession()
const qc = useQueryClient()

const id = computed(() => route.params.id as string)

const { data: dep, isLoading } = useQuery({
  queryKey: ['deployment', () => id.value],
  queryFn: () => daffa.deployment(id.value),
  // Poll only while it is in flight. The log arrives over SSE; this is just the header.
  refetchInterval: (q) => (q.state.data?.status === 'running' ? 2000 : false),
})

const running = computed(() => dep.value?.status === 'running')

// The tab names the stack and what is happening to it — "api-gateway · Deploying" while it runs,
// "api-gateway · Failed" once it has not. Watching a deploy from a background tab is the whole
// reason this page exists, and the title is the only part of it you can see from there.
watchEffect(() => {
  const d = dep.value
  setTitle(d?.stack_name, d && deploymentStatus(d.status, d.exit_code).label, 'Deployments')
})

// The path back up. A deployment is reachable from the cross-stack feed and from its stack's
// history, and it used to lead back to neither — the only way up was the browser's Back button.
const crumbs = computed(() => {
  const c: { label: string; to: RouteLocationRaw }[] = [
    { label: 'Deployments', to: { name: 'deployments' } },
  ]
  if (dep.value?.stack_id) {
    c.push({
      label: dep.value.stack_name || 'Stack',
      to: { name: 'stack', params: { id: dep.value.stack_id } },
    })
  }
  return c
})

// What actually happened, in a sentence, at the top of the page.
//
// Especially for a cancellation: "cancelled" alone reads as "nothing happened", and the one
// thing an operator must not believe about a cancelled deploy is that it undid itself.
const verdict = computed(() => {
  const d = dep.value
  if (!d) return ''
  switch (d.status) {
    case 'running':
      return 'This deploy is still going.'
    case 'ok':
      return ''
    case 'failed':
      return `The ${actionNoun(d.action)} failed${d.exit_code != null ? ` with exit code ${d.exit_code}` : ''}.`
    case 'cancelled':
      return (
        `This deploy was cancelled while it was running. It was stopped, not undone — ` +
        `whatever it had already changed is still changed, so the stack may be part-way ` +
        `between the old state and the new one. Deploy again to settle it.`
      )
  }
  return ''
})

// The verdict banner's tone, from the same three tokens the rest of the app uses for state.
const verdictStyle = computed(() => {
  const s = dep.value?.status
  if (s === 'failed')
    return {
      background: 'var(--danger-soft)',
      borderColor: 'color-mix(in oklch, var(--danger) 30%, transparent)',
    }
  if (s === 'cancelled')
    return {
      background: 'var(--warn-soft)',
      borderColor: 'color-mix(in oklch, var(--warn) 30%, transparent)',
    }
  return { background: 'var(--surface-sunken)', borderColor: 'var(--border)' }
})

const trigger = computed(() => {
  const d = dep.value
  if (!d) return ''
  switch (d.trigger_kind) {
    case 'webhook':
      return 'a push (auto-deploy)'
    case 'rollback':
      return 'a rollback'
    default:
      return d.started_by_name || 'a person'
  }
})

const cancel = useMutation({
  mutationFn: () => daffa.cancelDeployment(id.value),
  onSuccess: () => {
    toast.ok('Deployment cancelled.')
    void qc.invalidateQueries({ queryKey: ['deployment'] })
  },
  onError: (e) => toast.err(e, 'Could not cancel the deploy.'),
})

const redeploy = useMutation({
  mutationFn: () => daffa.redeploy(id.value),
  onSuccess: (r) => {
    toast.ok('Redeploy started.')
    void qc.invalidateQueries({ queryKey: ['stack'] })
    void qc.invalidateQueries({ queryKey: ['deployments'] })
    // Follow the new deploy, which is what the person pressing the button wants to watch.
    void router.push({ name: 'deployment', params: { id: r.deployment_id } })
  },
  onError: (e) => toast.err(e, 'Could not redeploy.'),
})

// Stopping a run mid-flight leaves the stack between two states, and that is a thing to be told
// BEFORE the click rather than read in the verdict afterwards. Caution, not danger: nothing is
// destroyed, and the way out is to deploy again.
async function askCancel() {
  const ok = await confirm({
    title: 'Cancel this deploy?',
    body:
      'It will be stopped, not undone — whatever it has already changed stays changed, so the ' +
      'stack may be left part-way between the old state and the new one. Deploy again to settle it.',
    confirmLabel: 'Cancel deploy',
    // Never "Cancel" here: against "Cancel deploy" the word means both things at once.
    cancelLabel: 'Let it finish',
    intent: 'caution',
  })
  if (ok) cancel.mutate()
}

// Redeploy is a deploy to production. It gets a sentence, not a shrug.
async function askRedeploy() {
  const ok = await confirm({
    title: 'Put this deploy back?',
    body:
      'Daffa will re-apply the compose file from THIS deployment — it does not go back to git, ' +
      'so a moved branch or an unreachable repo cannot stop it, and your current environment ' +
      'variables and registry credentials are used. Image tags come from that file: a service on ' +
      'a floating tag such as :latest still resolves to whatever is newest now, so that service ' +
      'will not actually roll back. Pinned tags will.',
    confirmLabel: 'Redeploy',
    intent: 'primary',
  })
  if (ok) redeploy.mutate()
}
</script>

<template>
  <div v-if="isLoading" class="muted text-sm">Loading…</div>

  <div v-else-if="dep">
    <PageHeader :title="actionLabel(dep.action)" :crumbs="crumbs">
      <template #actions>
        <StatusPill :status="deploymentStatus(dep.status, dep.exit_code)" />

        <template v-if="session.can(Cap.StacksEdit, dep.env_id)">
          <!-- Stopping a running deploy is disruptive and recoverable: amber, not red. -->
          <BaseButton
            v-if="running"
            intent="caution"
            :loading="cancel.isPending.value"
            @click="askCancel"
          >
            Cancel deploy
          </BaseButton>

          <BaseButton
            v-if="dep.redeployable"
            intent="primary"
            :loading="redeploy.isPending.value"
            @click="askRedeploy"
          >
            Redeploy this
          </BaseButton>
        </template>
      </template>
    </PageHeader>

    <p
      v-if="verdict"
      class="mb-4 rounded-[var(--radius-control)] border px-4 py-3 text-sm"
      :style="verdictStyle"
    >
      {{ verdict }}
    </p>

    <!-- The facts. Everything somebody needs in order to say what this deploy was. -->
    <dl
      class="surface mb-6 grid grid-cols-2 gap-x-6 gap-y-4 rounded-[var(--radius-card)] p-5 text-sm sm:grid-cols-4"
    >
      <div>
        <dt class="eyebrow">Started</dt>
        <dd class="mt-0.5 font-mono text-xs" :title="absolute(dep.started_at)">
          {{ absolute(dep.started_at) }}
        </dd>
      </div>
      <div>
        <dt class="eyebrow">Duration</dt>
        <dd class="mt-0.5 font-mono text-xs">{{ duration(dep) || (running ? 'running…' : '—') }}</dd>
      </div>
      <div>
        <dt class="eyebrow">Triggered by</dt>
        <dd class="mt-0.5">{{ trigger }}</dd>
      </div>
      <div>
        <dt class="eyebrow">Engine</dt>
        <dd class="mt-0.5">{{ dep.engine === 'compose' ? 'Docker Compose' : dep.engine }}</dd>
      </div>

      <!-- The commit is the answer to "what did this actually ship". An inline stack has none,
           and says so rather than showing an empty box. -->
      <div class="col-span-2">
        <dt class="eyebrow">Commit</dt>
        <dd v-if="dep.commit_sha" class="mt-0.5">
          <span class="font-mono text-xs">{{ shortSha(dep.commit_sha) }}</span>
          <span v-if="dep.commit_subject" class="muted"> · {{ dep.commit_subject }}</span>
        </dd>
        <dd v-else class="muted mt-0.5">
          — (this stack's compose file is stored in Daffa, not in git)
        </dd>
      </div>

      <div v-if="dep.rollback_of" class="col-span-2">
        <dt class="eyebrow">Rolled back to</dt>
        <dd class="mt-0.5">
          <BaseButton
            intent="link"
            :to="{ name: 'deployment', params: { id: dep.rollback_of } }"
          >
            an earlier deployment
          </BaseButton>
        </dd>
      </div>
    </dl>

    <DeploymentLog
      :deployment="dep"
      @end="() => qc.invalidateQueries({ queryKey: ['deployment'] })"
    />
  </div>

  <p v-else class="muted text-sm">No such deployment.</p>
</template>
