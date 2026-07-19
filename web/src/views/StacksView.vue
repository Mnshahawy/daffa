<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { daffa, type Stack } from '@/lib/api'
import { useSession } from '@/stores/session'
import { Cap } from '@/lib/caps'
import { toast } from '@/lib/toast'
import { ago, shortSha } from '@/lib/format'
import { stackStatus } from '@/lib/status'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import ComboBox from '@/components/ui/ComboBox.vue'
import EmptyState from '@/components/ui/EmptyState.vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import Select from '@/components/ui/Select.vue'
import SearchInput from '@/components/SearchInput.vue'
import StatusPill from '@/components/ui/StatusPill.vue'
import ComposeImages from '@/components/ComposeImages.vue'

const session = useSession()
const router = useRouter()
const qc = useQueryClient()

const { data: stacks, isLoading } = useQuery({ queryKey: ['stacks'], queryFn: daffa.stacks })

// Stacks belong to an environment; showing another host's here would be confusing, since the
// actions would target the wrong daemon.
const mine = computed(() => (stacks.value ?? []).filter((s) => s.env_id === session.envId))

const filter = ref('')
// The inline-compose editor has two tabs: the raw file, and an Images view that lists the
// compose file's images and validates new tags against the registry. See ComposeImages.vue.
const yamlTab = ref<'editor' | 'images'>('editor')
const shown = computed(() => {
  const q = filter.value.trim().toLowerCase()
  if (!q) return mine.value
  return mine.value.filter(
    (s) =>
      s.name.toLowerCase().includes(q) ||
      (s.git_url ?? '').toLowerCase().includes(q) ||
      (s.group_name ?? '').toLowerCase().includes(q),
  )
})

// Groups are a label, not a hierarchy — the list simply collapses under them. Ungrouped stacks
// come last, under no heading at all: a group called "Other" is a group nobody chose.
const grouped = computed(() => {
  const groups = new Map<string, Stack[]>()
  for (const s of shown.value) {
    const g = s.group_name?.trim() ?? ''
    if (!groups.has(g)) groups.set(g, [])
    groups.get(g)!.push(s)
  }
  return [...groups.entries()]
    .sort(([a], [b]) => (a === '' ? 1 : b === '' ? -1 : a.localeCompare(b)))
    .map(([name, stacks]) => ({ name, stacks }))
})

// Existing groups, offered as ComboBox suggestions so the second stack in a group is picked
// rather than retyped — which is how you end up with "platform" and "Platform" side by side.
const knownGroups = computed(() =>
  [...new Set(mine.value.map((s) => s.group_name?.trim()).filter((g): g is string => !!g))].sort(),
)

const collapsed = ref(new Set<string>())
function toggle(name: string) {
  const next = new Set(collapsed.value)
  next.has(name) ? next.delete(name) : next.add(name)
  collapsed.value = next
}

const adding = ref(false)

const { data: gitCreds } = useQuery({ queryKey: ['gitcreds'], queryFn: daffa.gitCredentials })

// The environment decides which questions the form even asks. A standalone host has one engine and
// one machine, so both are answered before anybody opens the page.
const { data: environments } = useQuery({
  queryKey: ['environments'],
  queryFn: () => daffa.environments(),
})
const currentEnv = computed(() => environments.value?.find((e) => e.id === session.envId))
const isSwarm = computed(() => !!currentEnv.value?.swarm)
const nodeChoices = computed(() => currentEnv.value?.nodes ?? [])

const form = ref({
  name: '',
  group_name: '',
  engine: 'compose' as 'compose' | 'swarm',
  node_id: '',
  source_kind: 'git' as 'git' | 'inline',
  git_url: '',
  git_ref: 'main',
  git_path: 'docker-compose.yml',
  git_credential_id: '',
  // The sample carries the full hook pipeline on purpose: hooks are invisible until you
  // know they exist, and the sample is where somebody first meets what a stack can do.
  // The explicit attachable network is load-bearing, not decoration — on a Swarm, hook
  // containers can only join attachable networks, and a sample that fails validation on
  // one engine teaches the wrong lesson.
  inline_yaml: `services:
  app:
    image: nginx:alpine
    ports:
      - "8080:80"
    networks: [web]

  # A hook is a normal service that Daffa runs AROUND the deploy instead of deploying.
  # These just say hello — a real pre_deploy runs your migrations, a real post_deploy
  # your smoke test. See docs/hooks.md.
  hello:
    image: busybox
    command: ["echo", "pre-deploy hook: the previous version is still running untouched"]
    networks: [web]

  verify:
    image: busybox
    command: ["echo", "post-deploy hook: the new version is live — this is where a smoke test belongs"]
    networks: [web]

networks:
  web:
    attachable: true   # lets hook containers join it on a Swarm; harmless on Compose

x-daffa:
  hooks:
    pre_deploy: [hello]
    post_deploy: [verify]
`,
})

// A Swarm environment defaults to the Swarm engine, which is what somebody who built a Swarm meant
// to use. Choosing Compose there is a deliberate act — "I want this on one named machine" — and the
// form then makes them say which.
watch(
  isSwarm,
  (swarm) => {
    form.value.engine = swarm ? 'swarm' : 'compose'
    form.value.node_id = ''
  },
  { immediate: true },
)

// Mirrors the server's rule, so the mistake is caught while the form is still open.
function isSSHUrl(url: string): boolean {
  if (url.startsWith('ssh://')) return true
  if (/^(https?|git):\/\//.test(url)) return false
  const at = url.indexOf('@')
  const colon = url.indexOf(':')
  const slash = url.indexOf('/')
  return at > 0 && colon > at && (slash < 0 || colon < slash)
}

const urlKindMismatch = computed(() => {
  const url = form.value.git_url.trim()
  if (!url) return ''
  const cred = gitCreds.value?.find((c) => c.id === form.value.git_credential_id)
  const ssh = isSSHUrl(url)

  if (ssh && !cred) return 'This is an SSH URL, so it needs an SSH credential.'
  if (ssh && cred?.kind === 'token')
    return 'That credential is an access token, but this is an SSH URL. Tokens work over https:// only.'
  if (!ssh && cred?.kind === 'ssh')
    return 'That credential is an SSH key, but this is not an SSH URL. Use the repository’s SSH URL (git@host:org/repo.git).'
  return ''
})

const create = useMutation({
  mutationFn: () => daffa.createStack({ ...form.value, env_id: session.envId }),
  onSuccess: (stack: Stack) => {
    toast.ok('Stack created.')
    qc.invalidateQueries({ queryKey: ['stacks'] })
    router.push({ name: 'stack', params: { id: stack.id } })
  },
  onError: (e) => toast.err(e, 'Could not create the stack.'),
})
</script>

<template>
  <div>
    <PageHeader
      title="Stacks"
      :count="stacks ? (filter ? `${shown.length} of ${mine.length}` : mine.length) : undefined"
    >
      <template #actions>
        <SearchInput
          v-if="mine.length"
          v-model="filter"
          placeholder="Name or repository…"
          class="w-64"
        />
        <BaseButton
          v-if="session.can(Cap.StacksEdit)"
          :intent="adding ? 'secondary' : 'primary'"
          @click="adding = !adding"
        >
          <AppIcon v-if="!adding" name="plus" class="size-4" />
          {{ adding ? 'Cancel' : 'New stack' }}
        </BaseButton>
      </template>
    </PageHeader>

    <!-- ── New ─────────────────────────────────────────────────────────────────── -->
    <form
      v-if="adding"
      class="surface mb-6 space-y-4 rounded-[var(--radius-card)] p-5"
      @submit.prevent="create.mutate()"
    >
      <div class="grid gap-4 sm:grid-cols-2">
        <div>
          <label for="s-name" class="mb-1.5 block text-sm font-medium">Name</label>
          <input id="s-name" v-model="form.name" required placeholder="my-app" class="field" />
          <p class="subtle mt-1 text-xs">
            Becomes the compose project name. Lowercase, digits, - or _.
          </p>
        </div>

        <!--
          ENGINE and PLACEMENT are different questions — how the file is applied, and where it
          runs — and collapsing them would re-create the implicitness the engine field exists to
          remove. Both are only ASKED on a Swarm, because on a standalone environment each has
          exactly one possible answer and asking would be theatre.
        -->
        <div v-if="isSwarm">
          <label for="s-engine" class="mb-1.5 block text-sm font-medium">Engine</label>
          <Select id="s-engine" v-model="form.engine">
            <option value="swarm">Docker Swarm — the scheduler places it</option>
            <option value="compose">Docker Compose — you place it, on one node</option>
          </Select>
          <p class="subtle mt-1 text-xs">
            On a Swarm this is the question of <em>who picks the machine</em>. Swarm schedules the
            services across the cluster; Compose runs them on the one node you choose, and only
            that node.
          </p>
        </div>

        <!-- Only when there is genuinely more than one answer. A single-node swarm has one. -->
        <div v-if="isSwarm && form.engine === 'compose' && nodeChoices.length > 1">
          <label for="s-node" class="mb-1.5 block text-sm font-medium">Node</label>
          <Select id="s-node" v-model="form.node_id" required>
            <option value="" disabled>Choose the machine…</option>
            <option v-for="n in nodeChoices" :key="n.id" :value="n.id">{{ n.name }}</option>
          </Select>
          <p class="subtle mt-1 text-xs">
            Its containers land here, and only here. A change of Swarm leadership will not move
            them.
          </p>
        </div>

        <div>
          <label for="s-source" class="mb-1.5 block text-sm font-medium">Source</label>
          <Select id="s-source" v-model="form.source_kind">
            <option value="git">Git repository</option>
            <option value="inline">Inline compose file</option>
          </Select>
          <p class="subtle mt-1 text-xs">
            Git keeps the repository as the source of truth; Daffa only executes it.
          </p>
        </div>

        <div>
          <label for="s-group" class="mb-1.5 block text-sm font-medium">
            Group <span class="subtle font-normal">(optional)</span>
          </label>
          <ComboBox id="s-group" v-model="form.group_name" :options="knownGroups" placeholder="platform" />
          <p class="subtle mt-1 text-xs">Just a label — the list collapses under it.</p>
        </div>
      </div>

      <template v-if="form.source_kind === 'git'">
        <div class="grid gap-4 sm:grid-cols-3">
          <div class="sm:col-span-2">
            <label for="s-url" class="mb-1.5 block text-sm font-medium">Repository URL</label>
            <input
              id="s-url"
              v-model="form.git_url"
              required
              placeholder="https://git.example.com/team/app.git"
              class="field font-mono text-xs"
            />
          </div>
          <div>
            <label for="s-ref" class="mb-1.5 block text-sm font-medium">Branch or tag</label>
            <input id="s-ref" v-model="form.git_ref" class="field font-mono text-xs" />
          </div>
        </div>

        <div class="grid gap-4 sm:grid-cols-2">
          <div>
            <label for="s-path" class="mb-1.5 block text-sm font-medium">Compose file path</label>
            <input id="s-path" v-model="form.git_path" class="field font-mono text-xs" />
          </div>
          <div>
            <label for="s-cred" class="mb-1.5 block text-sm font-medium">Credential</label>
            <Select id="s-cred" v-model="form.git_credential_id">
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

        <!-- The most common way to get this wrong is to paste an SSH URL and pick a token (or
             the reverse). The server refuses either way; saying so up front is cheaper than a
             failed deploy. -->
        <p
          v-if="urlKindMismatch"
          class="rounded-[var(--radius-control)] px-3 py-2 text-xs"
          :style="{
            background: 'var(--warn-soft)',
            border: '1px solid color-mix(in oklch, var(--warn) 30%, transparent)',
          }"
        >
          {{ urlKindMismatch }}
        </p>
      </template>

      <div v-else>
        <div class="mb-1.5 flex gap-4 text-sm" role="tablist">
          <button
            type="button"
            role="tab"
            :aria-selected="yamlTab === 'editor'"
            :class="yamlTab === 'editor' ? 'font-medium' : 'muted hover:text-[var(--text)]'"
            @click="yamlTab = 'editor'"
          >
            Compose file
          </button>
          <button
            type="button"
            role="tab"
            :aria-selected="yamlTab === 'images'"
            :class="yamlTab === 'images' ? 'font-medium' : 'muted hover:text-[var(--text)]'"
            @click="yamlTab = 'images'"
          >
            Images
          </button>
        </div>
        <textarea
          v-show="yamlTab === 'editor'"
          id="s-yaml"
          v-model="form.inline_yaml"
          rows="10"
          spellcheck="false"
          class="field font-mono text-xs"
          data-cursor="text"
        />
        <!-- Remounts on switch, so it always parses the current editor contents. Apply hands the
             rewritten YAML straight back into the form. -->
        <ComposeImages
          v-if="yamlTab === 'images'"
          :env-id="session.envId"
          :yaml="form.inline_yaml"
          :can-edit="session.can(Cap.StacksEdit, session.envId)"
          @applied="form.inline_yaml = $event"
        />
      </div>

      <BaseButton type="submit" intent="primary" size="md" :loading="create.isPending.value">
        Create stack
      </BaseButton>
    </form>

    <p v-if="isLoading" class="muted text-sm">Loading…</p>

    <EmptyState
      v-else-if="!mine.length && !adding"
      icon="layers"
      title="No stacks on this host yet"
      body="A stack is a set of services deployed together from one compose file. Point Daffa at a git repository and it will keep the repository as the source of truth."
    >
      <template #action>
        <BaseButton
          v-if="session.can(Cap.StacksEdit)"
          intent="primary"
          size="md"
          @click="adding = true"
        >
          <AppIcon name="plus" class="size-4" />
          New stack
        </BaseButton>
      </template>
    </EmptyState>

    <p v-else-if="mine.length && !shown.length" class="muted text-sm">
      No stacks match “{{ filter }}”.
    </p>

    <div v-else-if="shown.length" class="space-y-4">
      <div
        v-for="g in grouped"
        :key="g.name || '_'"
        class="surface overflow-hidden rounded-[var(--radius-card)]"
      >
        <!-- Ungrouped stacks get no heading at all. A group called "Other" is a group nobody
             chose, and it makes the ungrouped case look like a decision. -->
        <button
          v-if="g.name"
          class="flex w-full items-center gap-2 border-b px-4 py-2.5 text-left text-sm font-medium transition hover:bg-[var(--surface-sunken)]"
          :style="{ borderColor: 'var(--border)' }"
          :aria-expanded="!collapsed.has(g.name)"
          @click="toggle(g.name)"
        >
          <AppIcon
            name="chevronRight"
            class="subtle size-3.5 transition-transform"
            :class="collapsed.has(g.name) ? '' : 'rotate-90'"
          />
          {{ g.name }}
          <span class="subtle font-mono text-xs font-normal">{{ g.stacks.length }}</span>
        </button>

        <table v-if="!collapsed.has(g.name)" class="w-full text-sm">
          <!-- Columns get headers. They did not before, which meant a column of numbers with
               nothing anywhere on the page to say what they counted. -->
          <thead>
            <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
              <th class="eyebrow px-4 py-2 text-left font-medium">Status</th>
              <th class="eyebrow py-2 pr-4 text-left font-medium">Stack</th>
              <th class="eyebrow hidden py-2 pr-4 text-left font-medium md:table-cell">Live</th>
              <th class="eyebrow py-2 pr-4 text-right font-medium">Last deploy</th>
            </tr>
          </thead>

          <tbody>
            <tr
              v-for="s in g.stacks"
              :key="s.id"
              class="border-b transition last:border-0 hover:bg-[var(--surface-sunken)]"
              :style="{ borderColor: 'var(--border)' }"
            >
              <!-- The status column Portainer never shipped. Over there a healthy stack shows
                   nothing at all, and you cannot sort or filter by state. -->
              <td class="px-4 py-3">
                <StatusPill :status="stackStatus(s)" />
              </td>

              <td class="py-3 pr-4">
                <div class="flex flex-wrap items-center gap-2">
                  <RouterLink
                    :to="{ name: 'stack', params: { id: s.id } }"
                    class="font-medium transition hover:text-[var(--accent-text)]"
                  >
                    {{ s.name }}
                  </RouterLink>

                  <!-- The engine, on every row. The entity is called a stack, and nothing
                       anywhere used to say that what actually ran was `docker compose` and
                       never `docker stack`. -->
                  <span
                    class="subtle rounded-md border px-1.5 py-0.5 font-mono text-[10px]"
                    :style="{ borderColor: 'var(--border)' }"
                  >
                    {{ s.engine_label }}
                  </span>
                </div>

                <div class="subtle mt-0.5 truncate font-mono text-xs">
                  <template v-if="s.source_kind === 'git'">
                    {{ s.git_url }}<span v-if="s.git_ref"> @ {{ s.git_ref }}</span>
                  </template>
                  <template v-else>inline compose file</template>
                </div>
              </td>

              <!-- What is actually live. A date says when; only the commit says what. -->
              <td class="subtle hidden py-3 pr-4 font-mono text-xs md:table-cell">
                {{ shortSha(s.deployed_commit) || '—' }}
              </td>

              <td class="py-3 pr-4 text-right">
                <time v-if="s.deployed_at" class="subtle text-xs" :title="s.deployed_at">
                  {{ ago(s.deployed_at) }}
                </time>
                <span v-else class="subtle text-xs">—</span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</template>
