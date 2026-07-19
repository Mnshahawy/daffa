<script setup lang="ts">
import { computed } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import { daffa } from '@/lib/api'
import { useSession } from '@/stores/session'
import { containerStatus, deploymentStatus, stackStatus } from '@/lib/status'
import { ago, shortSha } from '@/lib/format'
import { Cap } from '@/lib/caps'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import StatusPill from '@/components/ui/StatusPill.vue'
import type { IconName } from '@/lib/icons'

/**
 * The front door.
 *
 * The app used to land you on the stack list, which answers "what exists". The question you
 * actually arrive with — especially at 2am — is "is anything wrong", and answering it meant
 * visiting stacks, then containers, then deployments, and holding all three in your head.
 *
 * So: a strip of instruments, and then the only section that really matters — everything that
 * is currently not right, gathered from all three, each row a link to the thing itself.
 *
 * Portainer's dashboard is a row of big numbers that are mostly counts of things you did not
 * ask about. Dokploy has no equivalent screen at all. A number is only worth the pixels if it
 * changes what you do next, so every count here is also a filter you can act on.
 */
const session = useSession()

const { data: environments } = useQuery({
  queryKey: ['environments'],
  queryFn: daffa.environments,
  refetchInterval: 15_000,
})

const { data: stacks } = useQuery({ queryKey: ['stacks'], queryFn: daffa.stacks })

const { data: containers } = useQuery({
  queryKey: ['containers', () => session.envId],
  queryFn: () => daffa.containers(session.envId),
  enabled: computed(() => !!session.envId && session.can(Cap.ContainersView)),
})

const { data: deployments } = useQuery({
  queryKey: ['deployments', 'recent'],
  queryFn: () => daffa.deployments(),
})

const mine = computed(() => (stacks.value ?? []).filter((s) => s.env_id === session.envId))
const running = computed(() => (containers.value ?? []).filter((c) => c.state === 'running'))
const hostsOnline = computed(() => (environments.value ?? []).filter((e) => e.status === 'online'))

const recent = computed(() =>
  (deployments.value ?? [])
    .filter((d) => !session.envId || !d.env_id || d.env_id === session.envId)
    .slice(0, 6),
)

/**
 * Everything that is not right, in one list.
 *
 * The ordering is by how much it should ruin your morning, not by type: a cluster you cannot
 * reach outranks a stack whose repository has moved on.
 */
interface Concern {
  key: string
  icon: IconName
  tone: 'danger' | 'warn'
  title: string
  detail: string
  to: { name: string; params?: Record<string, string> }
}

const concerns = computed<Concern[]>(() => {
  const out: Concern[] = []

  for (const e of environments.value ?? []) {
    if (e.status === 'offline') {
      out.push({
        key: `host:${e.id}`,
        icon: 'server',
        tone: 'danger',
        title: e.name,
        detail: 'Cluster unreachable — nothing can be deployed or operated here',
        to: { name: 'settings-clusters' },
      })
    }
  }

  for (const s of mine.value) {
    const st = stackStatus(s)
    if (st.tone === 'danger') {
      out.push({
        key: `stack:${s.id}`,
        icon: 'layers',
        tone: 'danger',
        title: s.name,
        detail: 'Last deploy failed',
        to: { name: 'stack', params: { id: s.id } },
      })
    } else if (st.tone === 'warn') {
      out.push({
        key: `stack:${s.id}`,
        icon: 'layers',
        tone: 'warn',
        title: s.name,
        detail: st.detail ?? st.label,
        to: { name: 'stack', params: { id: s.id } },
      })
    }
  }

  for (const c of containers.value ?? []) {
    const st = containerStatus(c.state, c.status)
    // `created` is informational, not a problem: it has simply not been started yet.
    if (st.tone !== 'danger' && st.tone !== 'warn') continue
    out.push({
      key: `container:${c.id}`,
      icon: 'box',
      tone: st.tone === 'danger' ? 'danger' : 'warn',
      title: c.service || c.name,
      detail: st.detail ? `${st.label} · ${st.detail}` : st.label,
      to: { name: 'container', params: { id: c.id } },
    })
  }

  return out.sort((a, b) => (a.tone === b.tone ? 0 : a.tone === 'danger' ? -1 : 1))
})

// The instruments. Each one is a link, because a count you cannot act on is decoration.
const instruments = computed(() => [
  {
    label: 'Clusters online',
    value: `${hostsOnline.value.length}/${environments.value?.length ?? 0}`,
    bad: hostsOnline.value.length < (environments.value?.length ?? 0),
    to: { name: 'settings-clusters' },
    show: true,
  },
  {
    label: 'Stacks',
    value: String(mine.value.length),
    bad: false,
    to: { name: 'stacks' },
    show: session.can(Cap.StacksView),
  },
  {
    label: 'Containers running',
    value: `${running.value.length}/${containers.value?.length ?? 0}`,
    bad: false,
    to: { name: 'containers' },
    show: session.can(Cap.ContainersView),
  },
  {
    label: 'Needs attention',
    value: String(concerns.value.length),
    bad: concerns.value.length > 0,
    to: { name: 'containers' },
    show: true,
  },
])
</script>

<template>
  <div>
    <PageHeader title="Overview" :description="`Everything on ${environments?.find((e) => e.id === session.envId)?.name ?? 'this cluster'}, at a glance.`">
      <template #actions>
        <BaseButton v-if="session.can(Cap.StacksEdit)" intent="primary" :to="{ name: 'stacks' }">
          <AppIcon name="plus" class="size-4" />
          New stack
        </BaseButton>
      </template>
    </PageHeader>

    <!-- ── The instrument strip ────────────────────────────────────────────────
         Hairline-separated, mono numerals, no chart junk. It is a binnacle, not a dashboard:
         four numbers you read in one glance, and every one of them is a door. -->
    <div
      class="mb-6 grid grid-cols-2 overflow-hidden rounded-[var(--radius-card)] md:grid-cols-4"
      :style="{ background: 'var(--surface-raised)', border: '1px solid var(--border)' }"
    >
      <RouterLink
        v-for="(m, i) in instruments.filter((x) => x.show)"
        :key="m.label"
        :to="m.to"
        class="group px-4 py-3.5 transition hover:bg-[var(--surface-sunken)]"
        :class="i > 0 ? 'border-l' : ''"
        :style="{ borderColor: 'var(--border)' }"
      >
        <div class="eyebrow">{{ m.label }}</div>
        <div
          class="mt-1 font-mono text-2xl font-semibold tracking-tight"
          :style="m.bad ? { color: 'var(--danger)' } : undefined"
        >
          {{ m.value }}
        </div>
      </RouterLink>
    </div>

    <div class="grid gap-6 lg:grid-cols-5">
      <!-- ── Needs attention ─────────────────────────────────────────────────── -->
      <section class="lg:col-span-3">
        <h2 class="mb-2.5 flex items-center gap-2 text-sm font-semibold">
          Needs attention
          <span
            v-if="concerns.length"
            class="rounded-md px-1.5 py-0.5 font-mono text-xs"
            :style="{ background: 'var(--danger-soft)', color: 'var(--danger)' }"
          >
            {{ concerns.length }}
          </span>
        </h2>

        <!-- The good case gets a real answer, not an empty box. -->
        <div
          v-if="!concerns.length"
          class="flex items-center gap-3 rounded-[var(--radius-card)] px-4 py-5 text-sm"
          :style="{
            background: 'var(--success-soft)',
            border: '1px solid color-mix(in oklch, var(--success) 25%, transparent)',
          }"
        >
          <AppIcon name="check" class="size-4 shrink-0" :style="{ color: 'var(--success)' }" />
          <span>
            <strong>All clear.</strong>
            <span class="muted">
              Every cluster is reachable, every stack deployed, every container up.</span
            >
          </span>
        </div>

        <div v-else class="surface overflow-hidden rounded-[var(--radius-card)]">
          <RouterLink
            v-for="c in concerns"
            :key="c.key"
            :to="c.to"
            class="flex items-center gap-3 border-b px-4 py-3 text-sm transition last:border-0 hover:bg-[var(--surface-sunken)]"
            :style="{ borderColor: 'var(--border)' }"
          >
            <span
              class="grid size-7 shrink-0 place-items-center rounded-lg"
              :style="{
                background: c.tone === 'danger' ? 'var(--danger-soft)' : 'var(--warn-soft)',
                color: c.tone === 'danger' ? 'var(--danger)' : 'var(--warn)',
              }"
            >
              <AppIcon :name="c.icon" class="size-3.5" />
            </span>

            <span class="min-w-0 flex-1">
              <span class="block truncate font-medium">{{ c.title }}</span>
              <span class="muted block truncate text-xs">{{ c.detail }}</span>
            </span>

            <AppIcon name="chevronRight" class="subtle size-4 shrink-0" />
          </RouterLink>
        </div>
      </section>

      <!-- ── Recent deploys ──────────────────────────────────────────────────── -->
      <section class="lg:col-span-2">
        <h2 class="mb-2.5 flex items-center gap-2 text-sm font-semibold">
          Recent deploys
          <RouterLink
            :to="{ name: 'deployments' }"
            class="muted ml-auto text-xs font-normal transition hover:text-[var(--accent-text)]"
          >
            All
          </RouterLink>
        </h2>

        <div v-if="!recent.length" class="surface rounded-[var(--radius-card)] px-4 py-5">
          <p class="muted text-sm">Nothing has been deployed yet.</p>
        </div>

        <div v-else class="surface overflow-hidden rounded-[var(--radius-card)]">
          <RouterLink
            v-for="d in recent"
            :key="d.id"
            :to="{ name: 'deployment', params: { id: d.id } }"
            class="flex items-center gap-3 border-b px-4 py-2.5 text-sm transition last:border-0 hover:bg-[var(--surface-sunken)]"
            :style="{ borderColor: 'var(--border)' }"
          >
            <StatusPill :status="deploymentStatus(d.status)" variant="dot" />

            <span class="min-w-0 flex-1">
              <span class="block truncate font-medium">{{ d.stack_name ?? 'stack' }}</span>
              <span class="subtle block truncate font-mono text-[11px]">
                {{ shortSha(d.commit_sha) || d.trigger_kind }}
              </span>
            </span>

            <time class="subtle shrink-0 text-xs" :title="d.started_at">
              {{ ago(d.started_at) }}
            </time>
          </RouterLink>
        </div>
      </section>
    </div>
  </div>
</template>
