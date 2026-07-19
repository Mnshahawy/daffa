<script setup lang="ts">
import { computed, ref, watch, watchEffect } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useQuery, useQueryClient } from '@tanstack/vue-query'
import { ApiError, daffa, type EnvVarItem, type StackAction, type StackSecretItem, type Task } from '@/lib/api'
import { toast } from '@/lib/toast'
import { useSession } from '@/stores/session'
import AutoDeployPanel from '@/components/AutoDeployPanel.vue'
import DropdownMenu from '@/components/DropdownMenu.vue'
import ContainerPanel from '@/components/ContainerPanel.vue'
import DeploymentLog from '@/components/DeploymentLog.vue'
import StackEnvEditor from '@/components/StackEnvEditor.vue'
import StackSecretsEditor from '@/components/StackSecretsEditor.vue'
import MetricPanel from '@/components/MetricPanel.vue'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import CopyButton from '@/components/ui/CopyButton.vue'
import EmptyState from '@/components/ui/EmptyState.vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import Select from '@/components/ui/Select.vue'
import StatusPill from '@/components/ui/StatusPill.vue'
import { Cap } from '@/lib/caps'
import { confirm } from '@/lib/confirm'
import { ago, absolute, actionLabel, duration, shortSha } from '@/lib/format'
import { containerStatus, containerUptime, deploymentStatus } from '@/lib/status'
import { setTitle } from '@/lib/title'

const route = useRoute()
const router = useRouter()
const session = useSession()
const qc = useQueryClient()

const id = computed(() => route.params.id as string)
const busy = ref(false)

// Tabs, because this page had become one long scroll: actions, log, services, metrics, variables,
// auto-deploy and history stacked on top of each other, and the thing you came for was always
// somewhere below the fold.
type Tab = 'overview' | 'containers' | 'deployments' | 'compose' | 'environment' | 'secrets' | 'settings'
const tab = ref<Tab>('overview')
// The Containers tab embeds ContainerPanel, which reads container inspect/logs/stats — a
// DIFFERENT capability (containers.view) from the stacks.view that opens this page. The server
// enforces it regardless, but showing a tab that silently 403s (and an "Open full page" link the
// router would bounce) is a discourtesy every other tab here avoids by staying in the stack's own
// capability. Hide it unless the viewer can actually use it.
const tabs = computed<{ id: Tab; label: string }[]>(() => {
  const list: { id: Tab; label: string }[] = [{ id: 'overview', label: 'Overview' }]
  if (session.can(Cap.ContainersView, stack.value?.env_id)) {
    list.push({ id: 'containers', label: 'Containers' })
  }
  list.push({ id: 'deployments', label: 'Deployments' })
  // Inline stacks store their compose in Daffa, so it is editable here. Git-backed stacks
  // edit theirs in the repo, so the tab would be a dead end for them — hide it.
  if (stack.value?.source_kind === 'inline') {
    list.push({ id: 'compose', label: 'Compose' })
  }
  list.push({ id: 'environment', label: 'Environment' })
  // File secrets are a Swarm-only feature (they become raft secrets). A compose file: secret can't
  // mount through the runner, so the tab would be a dead end — compose stacks use secret env vars.
  if (stack.value?.engine === 'swarm') {
    list.push({ id: 'secrets', label: 'Secrets' })
  }
  list.push({ id: 'settings', label: 'Settings' })
  return list
})

const { data, isLoading } = useQuery({
  queryKey: ['stack', () => id.value],
  queryFn: () => daffa.stack(id.value),
  // While a deploy is running the state changes underneath us; poll until it settles.
  refetchInterval: () => (busy.value ? 2000 : false),
})

const { data: deployments } = useQuery({
  queryKey: ['deployments', 'stack', () => id.value],
  queryFn: () => daffa.stackDeployments(id.value),
  refetchInterval: () => (busy.value ? 2000 : false),
})

const stack = computed(() => data.value?.stack)
const status = computed(() => data.value?.status)

// Name the tab after the stack, once we know what it is called. The router set "Stacks" on the
// way in; this is the part it could not know yet.
watchEffect(() => setTitle(stack.value?.name, 'Stacks'))

// The live deploy: while one is going, the top of the page is about watching it.
const live = computed(() => deployments.value?.find((d) => d.status === 'running'))
watch(live, (d) => {
  busy.value = !!d
  if (!d) {
    // One just finished: refresh everything it may have changed.
    void qc.invalidateQueries({ queryKey: ['stack'] })
    void qc.invalidateQueries({ queryKey: ['containers'] })
  }
})

// The most recent deploy, live or not. Shown on Overview so that a failure is the first thing on
// screen when you come back to the page — which is exactly when you want it, and which used to be
// impossible because the log panel vanished the moment the deploy ended.
const latest = computed(() => live.value ?? deployments.value?.[0])

async function act(action: StackAction) {
  const name = stack.value?.name ?? 'this stack'

  // `down` removes the containers. Volumes survive (Daffa never passes --volumes), but the person
  // should still be told out loud what goes and what stays before they take a service off the air.
  if (action === 'down') {
    const ok = await confirm({
      title: `Take ${name} down?`,
      body:
        'Every container and network in this stack is removed, and the service goes off the air. ' +
        'Volumes are NOT removed — your data stays. Deploy again to bring it back.',
      confirmLabel: 'Take down',
      intent: 'caution',
    })
    if (!ok) return
  }

  // `down + volumes` DELETES the stack's named volumes — the data in them, a database included — and
  // nothing puts it back. It used to fire straight from a plain button with no confirmation at all,
  // which is how a misclick wipes a production database. So it earns the strongest guard on this
  // page short of deletion: red, spelled out, and type-the-name-to-mean-it.
  if (action === 'down+volumes') {
    const ok = await confirm({
      title: `Take ${name} down and delete its volumes?`,
      body:
        'Every container and network is removed AND the stack’s named volumes are deleted — the ' +
        'data in them, including any database, is destroyed. This cannot be undone.',
      confirmLabel: 'Down + delete volumes',
      intent: 'danger',
      typeToConfirm: name,
    })
    if (!ok) return
  }

  busy.value = true
  try {
    await daffa.stackAction(id.value, action)
    await qc.invalidateQueries({ queryKey: ['deployments'] })
    // No success toast: the action only STARTS a deploy here. Its real outcome — progress and
    // failure alike — streams into the DeploymentLog panel, and a premature "done" would lie.
  } catch (e) {
    busy.value = false
    toast.err(e, 'The action failed.')
  }
}

// ── the Containers tab ──────────────────────────────────────────────────────────
//
// One selector, one embedded ContainerPanel — the same surface the container page is,
// without leaving the stack. What goes IN the selector differs by engine, because the two
// engines answer "which containers?" differently:
//
//   compose  the status already carries each service's container id — one per service.
//   swarm    the status carries the SERVICE id; the containers are its tasks, one per
//            replica, each on whichever machine the swarm chose. So the tasks are
//            resolved here, and each option carries its node — a container id is unique
//            per DAEMON, not per cluster.
type ContainerOption = { key: string; label: string; container: string; node?: string }

const isSwarmStack = computed(() => stack.value?.engine === 'swarm')

// The swarm node-id → Daffa node-id join, needed to route requests to the right daemon.
const { data: clusterNodes } = useQuery({
  queryKey: ['cluster-nodes', () => stack.value?.env_id],
  queryFn: () => daffa.clusterNodes(stack.value!.env_id),
  enabled: computed(() => isSwarmStack.value && !!stack.value?.env_id),
})

// Hooks are not containers-in-waiting; a service without an id has nothing to open.
const containerServices = computed(() =>
  (status.value?.services ?? []).filter((s) => s.state !== 'hook' && s.container_id),
)

const { data: swarmTasks } = useQuery({
  queryKey: ['stack-tasks', () => id.value, () => containerServices.value.map((s) => s.container_id).join(',')],
  queryFn: async () => {
    const out: { service: string; tasks: Task[] }[] = []
    await Promise.all(
      containerServices.value.map(async (s) => {
        out.push({ service: s.name, tasks: await daffa.tasks(stack.value!.env_id, s.container_id!) })
      }),
    )
    return out
  },
  enabled: computed(() => isSwarmStack.value && !!stack.value?.env_id && containerServices.value.length > 0),
  refetchInterval: () => (busy.value ? 2000 : false),
})

const containerOptions = computed<ContainerOption[]>(() => {
  if (!isSwarmStack.value) {
    return containerServices.value.map((s) => ({
      key: s.name,
      label: s.name,
      container: s.container_id!,
    }))
  }
  const opts: ContainerOption[] = []
  // Hold swarm options back until the node→daemon join has loaded. A swarm task's container is
  // node-local, so emitting an option before we can resolve its node routes the first inspect/logs
  // request to the wrong daemon (or none) and flashes an error the moment the tab opens. This
  // recomputes when clusterNodes arrives, so the dropdown fills in a beat later, correctly routed.
  if (!clusterNodes.value) return opts
  for (const { service, tasks: ts } of swarmTasks.value ?? []) {
    for (const t of ts) {
      if (t.state !== 'running' || !t.container_id) continue
      const node = clusterNodes.value?.find((n) => n.swarm_node_id === t.node_id)?.node_id
      opts.push({
        key: `${service}.${t.slot}`,
        label: t.node ? `${service}.${t.slot} on ${t.node}` : `${service}.${t.slot}`,
        container: t.container_id,
        node,
      })
    }
  }
  return opts
})

const selectedContainer = ref('')
// Keep the selection meaningful across redeploys: prefer the same service slot when its
// container id changed underneath us, fall back to the first option.
watch(containerOptions, (opts) => {
  if (!opts.some((o) => o.key === selectedContainer.value)) {
    selectedContainer.value = opts[0]?.key ?? ''
  }
}, { immediate: true })

const currentContainer = computed(() =>
  containerOptions.value.find((o) => o.key === selectedContainer.value),
)

// The header used to be a row of six buttons, destructive ones sitting flush beside routine ones.
// Now it is the one action people came for — Deploy — and a menu for the rest. WHICH actions exist
// is still the engine's business (compose stops, swarm does not), so the menu is built from the
// server's list, split into what is routine and what destroys something.
const lifecycleOrder: StackAction[] = ['pull', 'restart', 'stop']
const lifecycleActions = computed(() =>
  lifecycleOrder.filter((a) => stack.value?.actions?.includes(a)),
)
// Down (service off the air, data kept) and Down + volumes (data DELETED) — grouped, separated, and
// coloured, because a menu that lists them next to "Restart" is how the dangerous one gets clicked.
const destructiveActions = computed(() =>
  (['down', 'down+volumes'] as StackAction[]).filter((a) => stack.value?.actions?.includes(a)),
)
const hasMenu = computed(() => lifecycleActions.value.length + destructiveActions.value.length > 0)

// The colour a menu item wears. down+volumes is the only one that destroys data, so it is the only
// red; the rest that take the service away and give it back are amber; pull destroys nothing.
function actionTone(a: StackAction): 'danger' | 'caution' | 'neutral' {
  if (a === 'down+volumes') return 'danger'
  if (a === 'stop' || a === 'restart' || a === 'down') return 'caution'
  return 'neutral'
}
function toneColor(a: StackAction): string | undefined {
  const t = actionTone(a)
  return t === 'danger' ? 'var(--danger)' : t === 'caution' ? 'var(--warn)' : undefined
}

// ── source preview (git stacks) ───────────────────────────────────────────────────
// A git-backed stack edits its compose in the repo, so it has no Compose tab to read it back in.
// This shows the RESOLVED file the detail endpoint already carries (data.yaml) in a modal, so you
// can see exactly what would deploy without leaving for the repo host.
const showSource = ref(false)
const sourceYaml = computed(() => data.value?.yaml ?? '')

// Whether the "also delete its volumes" box is even DRAWN comes from the engine's action list, not
// from this file's opinion. Swarm cannot remove a stack's volumes — they are node-local, and the
// manager has no authority over them — so offering a box that silently does nothing would be worse
// than not offering it: somebody ticks it, Daffa says ok, and the data is still there.
//
// This is the same mechanism that already keeps dead buttons from shipping, reused rather than
// reinvented.
const canRemoveVolumes = computed(() => stack.value?.actions?.includes('down+volumes'))

// Deleting a stack removes what it deployed. It used to remove only the record and leave the
// containers running — a warning in a confirm() does not make that a good outcome, because what
// you were left with was containers Daffa had forgotten the name of and could no longer offer to
// clean up.
//
// The two things that are NOT part of "delete" get their own control, because they are not more
// deleting — they are different acts. Removing the volumes destroys data, so it is the checkbox
// on the dialog, asked at the one moment it can be answered. And forgetting a stack without
// cleaning it up is the old behaviour, which is right in exactly one situation and wrong in
// every other.
const deleteBusy = ref(false)
const deleteError = ref('')

async function askDelete() {
  const name = stack.value?.name
  if (!name) return

  const swarm = stack.value?.engine === 'swarm'

  const res = await confirm({
    title: `Delete ${name}?`,
    body: swarm
      ? 'Its services and networks are removed (docker stack rm), and then the stack itself. Its ' +
        'volumes are left alone: they live on each node, and Swarm has no authority over them — ' +
        'remove them per node, under Volumes.'
      : 'Its containers and network are removed (compose down), and then the stack itself. Daffa ' +
        'keeps nothing about it afterwards, and this cannot be undone.',
    confirmLabel: 'Delete',
    intent: 'danger',
    // Volumes are not "more delete". They are the database. And the box is only drawn by an engine
    // that can actually honour it.
    checkbox: canRemoveVolumes.value
      ? {
          label: 'Also delete its volumes',
          hint: 'This destroys the data in them and cannot be undone. Leave it unticked and the volumes stay, exactly as compose down would.',
        }
      : undefined,
    // The one act on this page that nothing can put back.
    typeToConfirm: name,
  })
  if (!res) return

  await runDelete(res.checked, false)
}

async function runDelete(volumes: boolean, force: boolean) {
  deleteBusy.value = true
  deleteError.value = ''
  try {
    await daffa.deleteStack(id.value, { volumes, force })
    await qc.invalidateQueries({ queryKey: ['stacks'] })
    toast.ok('Stack deleted.')
    void router.push({ name: 'stacks' })
    deleteBusy.value = false
  } catch (e) {
    const err = e instanceof ApiError ? e : undefined
    deleteError.value = err?.message ?? 'Could not delete the stack.'
    deleteBusy.value = false

    // A deploy is in flight: that is not a dead end, it is "wait". Everything else is.
    if (!force && err?.code !== 'run_in_progress') await askForget(volumes)
  }
}

// Offered only once a teardown has actually been TRIED and FAILED — never up front. Put beside
// Delete, "leave the containers running" becomes something people pick by accident, which is
// exactly how the orphans happened in the first place.
//
// It is offered on ANY failure, not just an unreachable host, because the alternative is a stack
// that can never be removed at all: if compose cannot tear it down for some reason we did not
// anticipate, refusing forever is not safety, it is a dead end. But by then the operator has SEEN
// the error, which is the whole point — they are choosing to leave containers running, rather
// than discovering later that they did.
async function askForget(volumes: boolean) {
  const ok = await confirm({
    title: `Forget ${stack.value?.name} anyway?`,
    body:
      `The teardown failed: ${deleteError.value} — Daffa can still drop the stack from its own ` +
      `records, but the containers keep RUNNING on the host. Nothing here will know their name ` +
      `or offer to clean them up again; you would have to remove them by hand.`,
    confirmLabel: 'Forget it',
    intent: 'danger',
  })
  if (ok) await runDelete(volumes, true)
}

async function saveEnv(vars: EnvVarItem[]) {
  await daffa.setStackEnv(id.value, vars)
  await qc.invalidateQueries({ queryKey: ['stack'] })
  await qc.invalidateQueries({ queryKey: ['stackenv'] })
}

async function saveSecrets(secrets: StackSecretItem[]) {
  await daffa.setStackSecrets(id.value, secrets)
  await qc.invalidateQueries({ queryKey: ['stack'] })
  await qc.invalidateQueries({ queryKey: ['stacksecrets'] })
}

// ── Compose (inline stacks) ──────────────────────────────────────────────────────
// A git-backed stack edits its compose in the repo; an inline stack has no repo, so its
// compose lives in Daffa and is edited here. Without this, an inline stack's YAML could only
// be changed by re-running `daffa stack adopt` or hitting the API directly — the console
// could edit env vars and secrets but not the file itself.
const composeDraft = ref('')
const composeBusy = ref(false)
const composeError = ref('')
const composeSaved = ref(false)
// The last body the server handed us. We reseed the editor from a background refetch only when
// the user has not typed away from it, so a poll mid-edit never stomps unsaved work.
let composeSeeded: string | undefined
watch(
  () => stack.value?.inline_yaml,
  (yaml) => {
    if (yaml === undefined) return
    if (composeSeeded === undefined || composeDraft.value === composeSeeded) {
      composeDraft.value = yaml
    }
    composeSeeded = yaml
  },
  { immediate: true },
)
const composeDirty = computed(
  () => stack.value?.source_kind === 'inline' && composeDraft.value !== (stack.value?.inline_yaml ?? ''),
)

async function saveCompose() {
  if (!stack.value) return
  composeBusy.value = true
  composeError.value = ''
  composeSaved.value = false
  try {
    // Preserve every other source field; the update handler rewrites them all from the body,
    // so omitting one would blank it. Engine is left out on purpose — it is immutable and the
    // server rejects any attempt to change it. inline_yaml is the only thing we mean to change.
    await daffa.updateStack(id.value, {
      group_name: stack.value.group_name,
      source_kind: stack.value.source_kind,
      git_url: stack.value.git_url,
      git_ref: stack.value.git_ref,
      git_path: stack.value.git_path,
      git_credential_id: stack.value.git_credential_id,
      inline_yaml: composeDraft.value,
    })
    await qc.invalidateQueries({ queryKey: ['stack'] })
    composeSaved.value = true
  } catch (err) {
    composeError.value = (err as ApiError)?.message ?? 'Could not save the compose file.'
  } finally {
    composeBusy.value = false
  }
}

// ── Switch an inline stack to git ────────────────────────────────────────────────────
// An inline stack keeps its compose in Daffa; pointing it at a repo makes the repo the source
// of truth from the next deploy on, without losing env vars, secrets or history. Only inline →
// git is offered — the server refuses the reverse — and the server probes the repo (clone, find
// the compose file, parse it) before committing the switch, so a bad URL/ref/credential is
// rejected here rather than at the next deploy.
const canSeeGitCreds = computed(() => session.can(Cap.GitCredsView))
const { data: gitCreds } = useQuery({
  queryKey: ['gitcreds'],
  queryFn: daffa.gitCredentials,
  enabled: canSeeGitCreds,
})
const switchGit = ref({ git_url: '', git_ref: 'main', git_path: 'docker-compose.yml', git_credential_id: '' })
const switchBusy = ref(false)
const switchError = ref('')

function isSSHUrl(url: string): boolean {
  return url.startsWith('git@') || url.startsWith('ssh://')
}

// The most common way to get this wrong is to paste an SSH URL and pick a token (or the
// reverse). The server refuses either way; saying so up front is cheaper than a failed probe.
const switchMismatch = computed(() => {
  const url = switchGit.value.git_url.trim()
  if (!url) return ''
  const cred = gitCreds.value?.find((c) => c.id === switchGit.value.git_credential_id)
  const ssh = isSSHUrl(url)
  if (ssh && !cred) return 'This is an SSH URL, so it needs an SSH credential.'
  if (ssh && cred?.kind === 'token')
    return 'That credential is an access token, but this is an SSH URL. Tokens work over https:// only.'
  if (!ssh && cred?.kind === 'ssh')
    return 'That credential is an SSH key, but this is not an SSH URL. Use the repository’s SSH URL (git@host:org/repo.git).'
  return ''
})

async function switchToGit() {
  if (!stack.value || !switchGit.value.git_url.trim()) return
  const res = await confirm({
    title: 'Switch this stack to git?',
    body:
      'The inline compose currently stored in Daffa will be discarded; the repository becomes ' +
      'the source of truth. Environment variables, secrets and deployment history are kept.',
    confirmLabel: 'Switch to git',
    intent: 'caution',
  })
  if (!res) return

  switchBusy.value = true
  switchError.value = ''
  try {
    await daffa.updateStack(id.value, {
      group_name: stack.value.group_name,
      source_kind: 'git',
      git_url: switchGit.value.git_url.trim(),
      git_ref: switchGit.value.git_ref.trim(),
      git_path: switchGit.value.git_path.trim(),
      git_credential_id: switchGit.value.git_credential_id || undefined,
      inline_yaml: '',
    })
    // The refetch flips source_kind to git: the Compose tab disappears and the auto-deploy
    // panel takes over, both driven off stack.source_kind.
    await qc.invalidateQueries({ queryKey: ['stack'] })
  } catch (err) {
    switchError.value = (err as ApiError)?.message ?? 'Could not switch this stack to git.'
  } finally {
    switchBusy.value = false
  }
}

const stateLabel: Record<string, string> = {
  running: 'Running',
  partial: 'Partially running',
  stopped: 'Stopped',
  not_deployed: 'Not deployed',
  unreachable: 'Host unreachable',
}

function triggeredBy(d: { trigger_kind: string; started_by_name?: string }): string {
  if (d.trigger_kind === 'webhook') return 'push'
  if (d.trigger_kind === 'rollback') return 'rollback'
  return d.started_by_name || 'manual'
}
</script>

<template>
  <div v-if="isLoading" class="muted text-sm">Loading…</div>

  <div v-else-if="stack">
    <PageHeader :title="stack.name" :crumbs="[{ label: 'Stacks', to: { name: 'stacks' } }]">
      <!--
        Actions come from the SERVER's list, not from one written here. Compose can stop a stack and
        swarm cannot, so a hardcoded row of five buttons would ship two dead ones the day a swarm
        stack appears — and a button that returns an error is worse than no button.
      -->
      <template v-if="session.can(Cap.StacksEdit, stack.env_id)" #actions>
        <BaseButton
          v-if="stack.actions.includes('up')"
          intent="primary"
          :loading="busy"
          @click="act('up')"
        >
          <AppIcon v-if="!busy" name="rocket" class="size-3.5" />
          Deploy
        </BaseButton>

        <!-- Everything that is not Deploy lives behind one menu now, so the destructive ones do not
             sit flush against the routine ones where the wrong one gets clicked. -->
        <DropdownMenu v-if="hasMenu" align="right">
          <template #trigger>
            <span class="btn btn-secondary btn-sm" :class="{ 'opacity-45': busy }">
              Actions
              <AppIcon name="more" class="size-3.5" />
            </span>
          </template>

          <button
            v-for="a in lifecycleActions"
            :key="a"
            :disabled="busy"
            class="flex w-full items-center rounded-lg px-2 py-1.5 text-left text-sm transition hover:bg-[var(--surface-sunken)] disabled:opacity-45"
            @click="act(a)"
          >
            {{ actionLabel(a) }}
          </button>

          <!-- Destructive actions are pushed below a rule and coloured; down+volumes is the only red
               one because it is the only one that deletes data. -->
          <div
            v-if="destructiveActions.length && lifecycleActions.length"
            class="my-1 border-t"
            :style="{ borderColor: 'var(--border)' }"
          />
          <button
            v-for="a in destructiveActions"
            :key="a"
            :disabled="busy"
            class="flex w-full items-center rounded-lg px-2 py-1.5 text-left text-sm transition hover:bg-[var(--surface-sunken)] disabled:opacity-45"
            :style="{ color: toneColor(a) }"
            @click="act(a)"
          >
            {{ actionLabel(a) }}
          </button>
        </DropdownMenu>
      </template>
    </PageHeader>

    <!--
      The engine, said out loud. This is the whole point of the redesign: the entity is called a
      stack, and until now nothing anywhere told you that what actually ran was `docker compose`
      and never `docker stack`. You had to read the source to find out.
    -->
    <div class="-mt-3 mb-5 flex flex-wrap items-center gap-x-3 gap-y-1.5 text-xs">
      <span
        class="subtle rounded-md border px-1.5 py-0.5 font-mono text-[10px]"
        :style="{ borderColor: 'var(--border)' }"
      >
        {{ stack.engine_label }}
      </span>
      <span v-if="status" class="muted">{{ stateLabel[status.state] ?? status.state }}</span>
      <span v-if="stack.group_name" class="muted">{{ stack.group_name }}</span>
      <span v-if="stack.deployed_commit" class="subtle font-mono">
        live: {{ shortSha(stack.deployed_commit) }}
      </span>
      <!-- A git stack has no Compose tab (its file lives in the repo), so this is the way to read
           the resolved file back without leaving for the repo host. -->
      <button
        v-if="stack.source_kind === 'git' && sourceYaml"
        class="inline-flex items-center gap-1 transition hover:text-[var(--accent-text)]"
        @click="showSource = true"
      >
        <AppIcon name="file" class="size-3" />
        View source
      </button>
    </div>

    <!-- A broken source is the one time the editor matters most, so it must not hide it. -->
    <p
      v-if="data?.source_error"
      class="mb-4 rounded-[var(--radius-control)] border px-4 py-3 text-sm"
      :style="{
        background: 'var(--danger-soft)',
        borderColor: 'color-mix(in oklch, var(--danger) 30%, transparent)',
      }"
    >
      <strong>The source could not be read:</strong> {{ data.source_error }}
    </p>


    <p
      v-else-if="status?.changed"
      class="mb-4 rounded-[var(--radius-control)] border px-4 py-3 text-sm"
      :style="{
        background: 'var(--warn-soft)',
        borderColor: 'color-mix(in oklch, var(--warn) 30%, transparent)',
      }"
    >
      The source has changed since the last deploy. Deploy to apply it.
    </p>

    <!--
      THE VOLUME TRAP.

      A named volume in Swarm is NODE-LOCAL. If the task is ever rescheduled — a node drains, a
      machine reboots, a rolling update moves it — the new task gets a fresh, EMPTY volume of the
      same name on the new machine, and the database it was serving is gone from its point of view.
      The service comes up healthy. Nothing errors.

      It is the most expensive Swarm mistake available and it is completely silent. Dokploy hides it
      behind an automatic node.role==manager constraint; Portainer says nothing at all. Daffa already
      parses the compose file, so it just says so — here, and at the head of the deploy log.

      Not a refusal. A statement.
    -->
    <div
      v-for="w in data?.warnings ?? []"
      :key="w.service"
      class="mb-4 rounded-[var(--radius-control)] border px-4 py-3 text-sm"
      :style="{
        background: 'var(--warn-soft)',
        borderColor: 'color-mix(in oklch, var(--warn) 30%, transparent)',
      }"
    >
      <strong>{{ w.service }} may not find its data if it moves.</strong>
      {{ w.text }}
    </div>

    <!-- Tabs -->
    <div class="mb-5 flex gap-1" role="tablist">
      <button
        v-for="t in tabs"
        :key="t.id"
        role="tab"
        :aria-selected="tab === t.id"
        class="rounded-[var(--radius-control)] px-3 py-1.5 text-sm transition"
        :class="tab === t.id ? 'font-medium' : 'muted hover:text-[var(--text)]'"
        :style="
          tab === t.id
            ? { background: 'var(--accent-soft)', color: 'var(--accent-text)' }
            : undefined
        "
        @click="tab = t.id"
      >
        {{ t.label }}
      </button>
    </div>

    <!-- ── Overview ────────────────────────────────────────────────────────────── -->
    <template v-if="tab === 'overview'">
      <!--
        The most recent deploy, live or finished. It STAYS after the run ends — that is the entire
        point of this panel. It used to be shown only while a deploy was in flight, so the output
        of the one that FAILED vanished at exactly the moment it became worth reading.
      -->
      <div v-if="latest" class="mb-6">
        <div class="mb-2 flex flex-wrap items-center gap-x-3 gap-y-1">
          <span class="text-sm font-medium">{{ actionLabel(latest.action) }}</span>
          <StatusPill :status="deploymentStatus(latest.status, latest.exit_code)" />
          <time class="subtle text-xs" :title="absolute(latest.started_at)">
            {{ ago(latest.started_at) }}
            <template v-if="duration(latest)">
              · <span class="font-mono">{{ duration(latest) }}</span>
            </template>
          </time>
          <BaseButton
            intent="link"
            class="ml-auto text-xs"
            :to="{ name: 'deployment', params: { id: latest.id } }"
          >
            Open deployment →
          </BaseButton>
        </div>
        <DeploymentLog
          :deployment="latest"
          @end="qc.invalidateQueries({ queryKey: ['deployments'] })"
        />
      </div>

      <!-- Services -->
      <div
        v-if="status?.services?.length"
        class="surface mb-6 overflow-hidden rounded-[var(--radius-card)]"
      >
        <div
          class="border-b px-4 py-2 text-sm font-medium"
          :style="{ borderColor: 'var(--border)' }"
        >
          Services
        </div>

        <table class="w-full text-sm">
          <thead>
            <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
              <th class="eyebrow px-4 py-2 text-left font-medium">State</th>
              <th class="eyebrow py-2 pr-4 text-left font-medium">Service</th>
              <th class="eyebrow py-2 pr-4 text-left font-medium">Image</th>
            </tr>
          </thead>

          <tbody>
            <tr
              v-for="svc in status.services"
              :key="svc.name"
              class="border-b last:border-0"
              :style="{ borderColor: 'var(--border)' }"
            >
              <td class="px-4 py-3">
                <StatusPill :status="containerStatus(svc.state, svc.status)" />
                <div v-if="containerUptime(svc.status)" class="subtle mt-1 text-xs">
                  up {{ containerUptime(svc.status) }}
                </div>
              </td>

              <td class="py-3 pr-4 font-medium">{{ svc.name }}</td>

              <td class="muted py-3 pr-4 font-mono text-xs">
                {{ svc.declared }}
                <!-- A running image that differs from the declared one is the drift you most want
                     to see; spell it out rather than leaving it to be noticed. -->
                <span
                  v-if="svc.running && svc.running !== svc.declared"
                  :style="{ color: 'var(--warn)' }"
                >
                  (running {{ svc.running }})
                </span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- The stack's resource use: its containers, SUMMED at each instant. A stack using 40% is
           what its services add up to — averaging them instead would report a stack's load as its
           mean container's load, which is a number that means nothing and which quietly falls
           every time you add a sidecar. -->
      <div class="surface rounded-[var(--radius-card)] p-5">
        <MetricPanel :stack="stack.name" :env="stack.env_id" />
      </div>
    </template>

    <!-- ── Deployments ─────────────────────────────────────────────────────────── -->
    <!-- Containers: the container page's surface, embedded — pick a container, get its
         stats, logs and shell without leaving the stack. -->
    <template v-else-if="tab === 'containers'">
      <EmptyState
        v-if="!containerOptions.length"
        icon="box"
        title="No running containers"
        body="This stack has no containers to open — it is not deployed, or everything in it has stopped. Deploy it and they appear here."
      />
      <template v-else>
        <div class="mb-4 flex flex-wrap items-center gap-3">
          <label for="stack-container" class="text-sm font-medium">Container</label>
          <Select id="stack-container" v-model="selectedContainer" class="max-w-xs">
            <option v-for="o in containerOptions" :key="o.key" :value="o.key">{{ o.label }}</option>
          </Select>
          <!-- The full page still exists; this is the way out to it. -->
          <RouterLink
            v-if="currentContainer"
            :to="{
              name: 'container',
              params: { id: currentContainer.container },
              query: currentContainer.node ? { node: currentContainer.node } : {},
            }"
            class="muted text-xs underline decoration-dotted underline-offset-2 transition hover:text-[var(--text)]"
          >
            Open full page
          </RouterLink>
        </div>
        <ContainerPanel
          v-if="currentContainer"
          :key="currentContainer.container"
          :env="stack!.env_id"
          :id="currentContainer.container"
          :node="currentContainer.node"
        />
      </template>
    </template>

    <template v-else-if="tab === 'deployments'">
      <EmptyState
        v-if="!deployments?.length"
        icon="rocket"
        title="This stack has never been deployed"
        body="Every attempt — by hand, by push, or by rollback — is kept here with the output it produced, so the deploy that failed last night is still readable this morning. Press Deploy to run the first one."
      />

      <div v-else class="surface overflow-x-auto rounded-[var(--radius-card)]">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
              <th class="eyebrow px-4 py-2 text-left font-medium">Outcome</th>
              <th class="eyebrow py-2 pr-4 text-left font-medium">Action</th>
              <th class="eyebrow hidden py-2 pr-4 text-left font-medium md:table-cell">Commit</th>
              <th class="eyebrow hidden py-2 pr-4 text-left font-medium sm:table-cell">Trigger</th>
              <th class="eyebrow py-2 pr-4 text-right font-medium">Took</th>
              <th class="eyebrow py-2 pr-4 text-right font-medium">Started</th>
            </tr>
          </thead>

          <tbody>
            <!-- Every row is a LINK to a page of its own. That is the fix: a failed deploy now has
                 a URL you can send to somebody, instead of being a selection inside this page. -->
            <tr
              v-for="d in deployments"
              :key="d.id"
              class="border-b transition last:border-0 hover:bg-[var(--surface-sunken)]"
              :style="{ borderColor: 'var(--border)' }"
            >
              <td class="px-4 py-3">
                <StatusPill :status="deploymentStatus(d.status, d.exit_code)" />
              </td>

              <td class="py-3 pr-4">
                <RouterLink
                  :to="{ name: 'deployment', params: { id: d.id } }"
                  class="font-medium transition hover:text-[var(--accent-text)]"
                >
                  {{ actionLabel(d.action) }}
                </RouterLink>
              </td>

              <td class="subtle hidden py-3 pr-4 font-mono text-xs md:table-cell">
                <span v-if="d.commit_sha" :title="d.commit_subject">
                  {{ shortSha(d.commit_sha) }}
                </span>
                <span v-else>—</span>
              </td>

              <td class="muted hidden py-3 pr-4 text-xs sm:table-cell">{{ triggeredBy(d) }}</td>

              <td class="subtle py-3 pr-4 text-right font-mono text-xs">
                {{ duration(d) || '—' }}
              </td>

              <td class="py-3 pr-4 text-right">
                <time class="subtle text-xs whitespace-nowrap" :title="absolute(d.started_at)">
                  {{ ago(d.started_at) }}
                </time>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>

    <!-- ── Compose (inline stacks) ─────────────────────────────────────────────── -->
    <template v-else-if="tab === 'compose'">
      <div class="surface rounded-[var(--radius-card)] p-5">
        <div class="flex items-start justify-between gap-3">
          <div>
            <h2 class="text-sm font-medium">Compose file</h2>
            <p class="muted mt-1 text-xs">
              This stack has no git repo — its compose is stored in Daffa. Saving updates the
              stored file; it takes effect on the next deploy.
            </p>
          </div>
          <span
            v-if="composeDirty"
            class="shrink-0 rounded px-2 py-0.5 text-xs"
            :style="{ background: 'var(--warn-soft)', color: 'var(--warn)' }"
          >
            Unsaved
          </span>
        </div>

        <textarea
          v-model="composeDraft"
          rows="24"
          spellcheck="false"
          :readonly="!session.can(Cap.StacksEdit, stack.env_id)"
          class="field mt-4 w-full font-mono text-xs"
          data-cursor="text"
        />

        <p v-if="composeError" class="mt-3 text-sm" :style="{ color: 'var(--danger)' }">
          {{ composeError }}
        </p>
        <p v-else-if="composeSaved && !composeDirty" class="muted mt-3 text-xs">
          Saved. Deploy the stack to apply the change.
        </p>

        <div v-if="session.can(Cap.StacksEdit, stack.env_id)" class="mt-4 flex gap-2">
          <BaseButton intent="primary" :loading="composeBusy" :disabled="!composeDirty" @click="saveCompose">
            Save
          </BaseButton>
          <BaseButton
            v-if="composeDirty"
            intent="ghost"
            :disabled="composeBusy"
            @click="composeDraft = stack.inline_yaml ?? ''"
          >
            Revert
          </BaseButton>
        </div>
      </div>
    </template>

    <!-- ── Environment ─────────────────────────────────────────────────────────── -->
    <template v-else-if="tab === 'environment'">
      <StackEnvEditor
        :stack-id="id"
        :can-write="session.can(Cap.StacksEdit, stack.env_id)"
        @save="saveEnv"
      />
    </template>

    <!-- ── Secrets ─────────────────────────────────────────────────────────────── -->
    <template v-else-if="tab === 'secrets'">
      <StackSecretsEditor
        :stack-id="id"
        :can-write="session.can(Cap.StacksEdit, stack.env_id)"
        @save="saveSecrets"
      />
    </template>

    <!-- ── Settings ────────────────────────────────────────────────────────────── -->
    <template v-else>
      <AutoDeployPanel
        v-if="stack.source_kind === 'git'"
        :stack="stack"
        :can-write="session.can(Cap.StacksEdit, stack.env_id)"
      />

      <!-- ── Switch an inline stack to git ─────────────────────────────────────────── -->
      <form
        v-if="stack.source_kind === 'inline' && session.can(Cap.StacksEdit, stack.env_id)"
        class="surface rounded-[var(--radius-card)] p-5"
        @submit.prevent="switchToGit"
      >
        <h2 class="text-sm font-medium">Switch to git</h2>
        <p class="muted mt-1 text-xs">
          Point this stack at a repository. The compose stored in Daffa is discarded and the repo
          becomes the source of truth from the next deploy on; environment variables, secrets and
          deployment history are kept.
        </p>

        <div class="mt-4 grid gap-4 sm:grid-cols-3">
          <div class="sm:col-span-2">
            <label for="sw-url" class="mb-1.5 block text-sm font-medium">Repository URL</label>
            <input
              id="sw-url"
              v-model="switchGit.git_url"
              required
              placeholder="https://git.example.com/team/app.git"
              class="field font-mono text-xs"
            />
          </div>
          <div>
            <label for="sw-ref" class="mb-1.5 block text-sm font-medium">Branch or tag</label>
            <input id="sw-ref" v-model="switchGit.git_ref" class="field font-mono text-xs" />
          </div>
        </div>

        <div class="mt-4 grid gap-4 sm:grid-cols-2">
          <div>
            <label for="sw-path" class="mb-1.5 block text-sm font-medium">Compose file path</label>
            <input id="sw-path" v-model="switchGit.git_path" class="field font-mono text-xs" />
          </div>
          <div>
            <label for="sw-cred" class="mb-1.5 block text-sm font-medium">Credential</label>
            <Select id="sw-cred" v-model="switchGit.git_credential_id">
              <option value="">None — public repository</option>
              <option v-for="c in gitCreds" :key="c.id" :value="c.id">
                {{ c.name }} ({{ c.kind === 'ssh' ? 'SSH' : 'token' }})
              </option>
            </Select>
            <p class="subtle mt-1 text-xs">
              <RouterLink
                v-if="canSeeGitCreds"
                :to="{ name: 'settings-git' }"
                class="transition hover:text-[var(--accent-text)]"
              >
                Manage credentials in Settings → Git
              </RouterLink>
              <span v-else>Ask an admin to add one under Settings → Git.</span>
            </p>
          </div>
        </div>

        <p
          v-if="switchMismatch"
          class="mt-4 rounded-[var(--radius-control)] px-3 py-2 text-xs"
          :style="{
            background: 'var(--warn-soft)',
            border: '1px solid color-mix(in oklch, var(--warn) 30%, transparent)',
          }"
        >
          {{ switchMismatch }}
        </p>

        <p v-if="switchError" class="mt-3 text-sm" :style="{ color: 'var(--danger)' }">
          {{ switchError }}
        </p>

        <BaseButton
          type="submit"
          intent="primary"
          class="mt-4"
          :loading="switchBusy"
          :disabled="!switchGit.git_url.trim()"
        >
          Switch to git
        </BaseButton>
      </form>

      <div
        v-if="session.can(Cap.StacksEdit, stack.env_id)"
        class="surface mt-6 rounded-[var(--radius-card)] p-5"
      >
        <h2 class="text-sm font-medium">Delete this stack</h2>
        <p class="muted mt-1 text-xs">
          Its containers and network are removed, and then the stack itself.
        </p>

        <p v-if="deleteError" class="mt-3 text-sm" :style="{ color: 'var(--danger)' }">
          {{ deleteError }}
        </p>

        <BaseButton intent="danger" class="mt-3" :loading="deleteBusy" @click="askDelete">
          <AppIcon v-if="!deleteBusy" name="trash" class="size-3.5" />
          Delete stack
        </BaseButton>
      </div>
    </template>

    <!-- ── Source preview (git stacks) ─────────────────────────────────────────────
         The resolved compose file the deploy would ship, read-only, in a modal. -->
    <Teleport to="body">
      <div
        v-if="showSource"
        class="fixed inset-0 z-50 flex items-center justify-center p-4"
        style="background: color-mix(in oklch, black 55%, transparent)"
        @click.self="showSource = false"
      >
        <div class="surface flex max-h-[80vh] w-full max-w-3xl flex-col rounded-[var(--radius-card)]">
          <div
            class="flex items-center justify-between border-b px-5 py-3"
            :style="{ borderColor: 'var(--border)' }"
          >
            <div class="min-w-0">
              <h2 class="text-sm font-medium">Source</h2>
              <p class="subtle truncate font-mono text-xs">
                {{ stack?.git_path || 'docker-compose.yml' }}@{{ stack?.git_ref || 'HEAD' }}
              </p>
            </div>
            <div class="flex items-center gap-1">
              <CopyButton :text="sourceYaml" />
              <button
                class="btn btn-ghost btn-sm btn-icon"
                aria-label="Close"
                @click="showSource = false"
              >
                <AppIcon name="x" class="size-3.5" />
              </button>
            </div>
          </div>
          <pre
            class="overflow-auto p-4 font-mono text-xs leading-relaxed"
            :style="{ background: 'var(--surface-sunken)' }"
          ><code>{{ sourceYaml }}</code></pre>
        </div>
      </div>
    </Teleport>
  </div>
</template>
