<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { RouterLink, RouterView, useRoute } from 'vue-router'
import { useQuery } from '@tanstack/vue-query'
import { daffa } from '@/lib/api'
import { navGroups, allSettingsTabs } from '@/lib/nav'
import { useSession } from '@/stores/session'
import DaffaLogo from '@/components/brand/DaffaLogo.vue'
import DaffaMark from '@/components/brand/DaffaMark.vue'
import AppIcon from '@/components/ui/AppIcon.vue'
import CommandPalette from '@/components/ui/CommandPalette.vue'
import DropdownMenu from '@/components/DropdownMenu.vue'
import HostSwitcher from '@/components/HostSwitcher.vue'
import ThemeToggle from '@/components/ThemeToggle.vue'
import BaseButton from '@/components/ui/BaseButton.vue'

const session = useSession()
const route = useRoute()

const palette = ref<InstanceType<typeof CommandPalette>>()

const { data: environments } = useQuery({
  queryKey: ['environments'],
  queryFn: daffa.environments,
  refetchInterval: 15_000,
})

// Pick an environment, and re-pick if the remembered one is gone — a removed agent (or a
// localStorage value from another deployment) would otherwise leave the app pointing at an
// environment that does not exist, with every panel silently empty.
watch(
  environments,
  (envs) => {
    if (!envs?.length) return
    const stillExists = envs.some((e) => e.id === session.envId)
    if (!session.envId || !stillExists) session.envId = envs[0].id
  },
  { immediate: true },
)

const current = computed(() => environments.value?.find((e) => e.id === session.envId))

// Pages that only exist on a Swarm.
//
// This cannot be a capability: `services.view` says whether a person MAY see services, and that is
// a different question from whether this environment HAS any. A standalone host has no services,
// no tasks and no nodes to promote — offering the page would be offering a page that can only ever
// answer "this environment is not a Swarm".
const swarmOnly = new Set(['services'])

// Only the groups this person has anything in. A heading over nothing is worse than no heading.
const groups = computed(() =>
  navGroups
    .map((g) => ({
      ...g,
      items: g.items.filter(
        (i) => session.can(i.cap) && (!swarmOnly.has(i.name) || current.value?.swarm),
      ),
    }))
    .filter((g) => g.items.length),
)

const canOpenSettings = computed(() => allSettingsTabs.some((t) => session.canAnywhere(t.cap)))

// …and land them on the first tab they can actually read, not a fixed one they may not.
const settingsLanding = computed(
  () => allSettingsTabs.find((t) => session.canAnywhere(t.cap))?.name ?? 'settings-users',
)

// A detail view belongs to its parent as far as the rail is concerned: reading a deployment
// should keep "Deployments" lit, not light nothing at all.
const parents: Record<string, string> = {
  service: 'services',
  container: 'containers',
  stack: 'stacks',
  deployment: 'deployments',
}

function active(name: string): boolean {
  return route.name === name || parents[route.name as string] === name
}

const settingsActive = computed(() => String(route.name ?? '').startsWith('settings'))

// "Admin" or "Operator on staging, Viewer" — the scope is part of what a role IS here.
const roleSummary = computed(() => {
  const rs = session.user?.roles ?? []
  if (!rs.length) return 'no roles'
  return rs.map((r) => (r.env_name ? `${r.name} on ${r.env_name}` : r.name)).join(', ')
})

// Collapsing is remembered. Someone who works on a 13" screen collapses it once, not daily.
const collapsed = ref(localStorage.getItem('daffa.rail') === 'collapsed')
watch(collapsed, (c) => localStorage.setItem('daffa.rail', c ? 'collapsed' : 'open'))

const mac = navigator.platform.toUpperCase().includes('MAC')
</script>

<template>
  <div class="min-h-dvh lg:flex">
    <!-- ── The rail ──────────────────────────────────────────────────────────────
         Its own surface, deeper than the page. It is the one piece of chrome always on
         screen, so it is the one piece that carries the brand.

         The old nav was a 56px strip with four links and a "Docker" dropdown hiding
         Containers — the most-visited page in the product — behind a hover. Everything is
         one click now, and grouped by what it is FOR. -->
    <aside
      class="sticky top-0 z-30 hidden h-dvh shrink-0 flex-col border-r transition-[width] duration-200 lg:flex"
      :class="collapsed ? 'w-[68px]' : 'w-60'"
      :style="{ background: 'var(--rail)', borderColor: 'var(--border)' }"
    >
      <div class="flex h-14 items-center px-3.5">
        <RouterLink
          :to="{ name: 'overview' }"
          class="flex items-center rounded-lg"
          aria-label="Daffa — Overview"
        >
          <DaffaLogo v-if="!collapsed" size="sm" />
          <DaffaMark v-else class="size-7" />
        </RouterLink>
      </div>

      <!-- The host is the context every page below is scoped to, so it sits above them all,
           not tucked in a far corner of a top bar. -->
      <div class="px-3 pb-3">
        <HostSwitcher :collapsed="collapsed" />
      </div>

      <!-- ⌘K. Given a permanent home rather than left as an easter egg — Dokploy's palette is
           genuinely good and completely undiscoverable, because nothing on screen mentions it. -->
      <div class="px-3 pb-2">
        <button
          class="flex w-full items-center gap-2 rounded-[var(--radius-control)] border px-2.5 py-1.5 text-left text-sm transition"
          :class="collapsed ? 'justify-center px-0' : ''"
          :style="{ borderColor: 'var(--border)', color: 'var(--text-subtle)' }"
          :title="collapsed ? 'Search' : undefined"
          @click="palette?.show()"
        >
          <AppIcon name="search" class="size-4 shrink-0" />
          <template v-if="!collapsed">
            <span class="flex-1">Search…</span>
            <kbd
              class="rounded border px-1 py-0.5 font-mono text-[10px]"
              :style="{ borderColor: 'var(--border)' }"
            >
              {{ mac ? '⌘' : 'Ctrl ' }}K
            </kbd>
          </template>
        </button>
      </div>

      <nav class="flex-1 space-y-4 overflow-y-auto px-3 py-2">
        <div v-for="(g, gi) in groups" :key="g.title ?? gi">
          <div v-if="g.title && !collapsed" class="eyebrow px-2.5 pb-1.5">{{ g.title }}</div>
          <!-- Collapsed, the heading becomes a rule: the grouping survives, the words don't. -->
          <div
            v-else-if="g.title && collapsed"
            class="mx-2 mb-2 border-t"
            :style="{ borderColor: 'var(--border)' }"
          />

          <RouterLink
            v-for="item in g.items"
            :key="item.name"
            :to="{ name: item.name }"
            class="relative flex items-center gap-2.5 rounded-[var(--radius-control)] px-2.5 py-2 text-sm transition"
            :class="[
              collapsed ? 'justify-center px-0' : '',
              active(item.name)
                ? 'font-medium'
                : 'muted hover:bg-[var(--rail-hover)] hover:text-[var(--text)]',
            ]"
            :style="
              active(item.name)
                ? { background: 'var(--accent-soft)', color: 'var(--accent-text)' }
                : undefined
            "
            :title="collapsed ? item.label : undefined"
          >
            <!-- The rudder post: a bar down the left edge of wherever you are. -->
            <span
              v-if="active(item.name)"
              class="absolute left-0 top-1/2 h-5 w-[3px] -translate-y-1/2 rounded-r-full"
              :style="{ background: 'var(--accent)' }"
            />
            <AppIcon :name="item.icon" class="size-4 shrink-0" />
            <span v-if="!collapsed" class="truncate">{{ item.label }}</span>
          </RouterLink>
        </div>
      </nav>

      <!-- Configure-once things live at the bottom, out of the daily path. -->
      <div class="space-y-1 border-t p-3" :style="{ borderColor: 'var(--border)' }">
        <RouterLink
          v-if="canOpenSettings"
          :to="{ name: settingsLanding }"
          class="flex items-center gap-2.5 rounded-[var(--radius-control)] px-2.5 py-2 text-sm transition"
          :class="[
            collapsed ? 'justify-center px-0' : '',
            settingsActive ? 'font-medium' : 'muted hover:bg-[var(--rail-hover)] hover:text-[var(--text)]',
          ]"
          :style="
            settingsActive ? { background: 'var(--accent-soft)', color: 'var(--accent-text)' } : undefined
          "
          :title="collapsed ? 'Settings' : undefined"
        >
          <AppIcon name="cog" class="size-4 shrink-0" />
          <span v-if="!collapsed">Settings</span>
        </RouterLink>

        <DropdownMenu align="left" placement="top" block>
          <template #trigger>
            <span
              class="muted flex w-full items-center gap-2.5 rounded-[var(--radius-control)] px-2.5 py-2 text-sm transition hover:bg-[var(--rail-hover)] hover:text-[var(--text)]"
              :class="collapsed ? 'justify-center px-0' : ''"
              :title="collapsed ? (session.user?.label ?? '') : undefined"
            >
              <span
                class="grid size-5 shrink-0 place-items-center rounded-full text-[10px] font-semibold"
                :style="{ background: 'var(--accent-soft)', color: 'var(--accent-text)' }"
              >
                {{ (session.user?.label ?? '?').charAt(0).toUpperCase() }}
              </span>
              <span v-if="!collapsed" class="truncate">{{ session.user?.label }}</span>
            </span>
          </template>

          <div class="muted px-2 py-1.5 text-xs">
            Signed in as <strong class="text-[var(--text)]">{{ session.user?.label }}</strong>
            <div class="mt-0.5">{{ roleSummary }}</div>
          </div>

          <div class="my-1 border-t pt-1" :style="{ borderColor: 'var(--border)' }">
            <!-- @click.stop: choosing a theme should not dismiss the menu you are choosing it
                 in — you want to see the result. -->
            <ThemeToggle @click.stop />
          </div>

          <div class="border-t pt-1" :style="{ borderColor: 'var(--border)' }">
            <RouterLink
              :to="{ name: 'tokens' }"
              class="muted flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left text-sm transition hover:bg-[var(--surface-sunken)] hover:text-[var(--text)]"
            >
              <AppIcon name="key" class="size-4" />
              API tokens
            </RouterLink>
            <button
              class="muted flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left text-sm transition hover:bg-[var(--surface-sunken)] hover:text-[var(--text)]"
              @click="session.logout()"
            >
              <AppIcon name="logOut" class="size-4" />
              Sign out
            </button>
          </div>
        </DropdownMenu>

        <button
          class="muted flex w-full items-center gap-2.5 rounded-[var(--radius-control)] px-2.5 py-2 text-sm transition hover:bg-[var(--rail-hover)] hover:text-[var(--text)]"
          :class="collapsed ? 'justify-center px-0' : ''"
          :aria-label="collapsed ? 'Expand sidebar' : 'Collapse sidebar'"
          @click="collapsed = !collapsed"
        >
          <AppIcon
            name="chevronsLeft"
            class="size-4 shrink-0 transition-transform"
            :class="collapsed ? 'rotate-180' : ''"
          />
          <span v-if="!collapsed">Collapse</span>
        </button>
      </div>
    </aside>

    <!-- ── Narrow screens ──────────────────────────────────────────────────────────
         The rail is a poor fit under 1024px, so below that the nav becomes a horizontally
         scrolling strip. It keeps the icons and the grouping order; it drops the headings,
         which have nowhere to go. -->
    <header
      class="sticky top-0 z-30 border-b backdrop-blur lg:hidden"
      :style="{
        borderColor: 'var(--border)',
        background: 'color-mix(in oklch, var(--surface) 88%, transparent)',
      }"
    >
      <div class="flex h-14 items-center gap-3 px-4">
        <RouterLink :to="{ name: 'overview' }" aria-label="Daffa — Overview">
          <DaffaLogo size="sm" />
        </RouterLink>

        <div class="ml-auto flex items-center gap-2">
          <BaseButton intent="ghost" size="sm" icon label="Search" @click="palette?.show()">
            <AppIcon name="search" class="size-4" />
          </BaseButton>
          <HostSwitcher />
          <DropdownMenu align="right">
            <template #trigger>
              <span
                class="grid size-7 place-items-center rounded-full text-xs font-semibold"
                :style="{ background: 'var(--accent-soft)', color: 'var(--accent-text)' }"
              >
                {{ (session.user?.label ?? '?').charAt(0).toUpperCase() }}
              </span>
            </template>
            <div class="muted px-2 py-1.5 text-xs">
              Signed in as <strong class="text-[var(--text)]">{{ session.user?.label }}</strong>
              <div class="mt-0.5">{{ roleSummary }}</div>
            </div>
            <div class="my-1 border-t pt-1" :style="{ borderColor: 'var(--border)' }">
              <ThemeToggle @click.stop />
            </div>
            <div class="border-t pt-1" :style="{ borderColor: 'var(--border)' }">
              <RouterLink
                v-if="canOpenSettings"
                :to="{ name: settingsLanding }"
                class="muted block rounded-lg px-2 py-1.5 text-sm transition hover:bg-[var(--surface-sunken)]"
              >
                Settings
              </RouterLink>
              <RouterLink
                :to="{ name: 'tokens' }"
                class="muted block rounded-lg px-2 py-1.5 text-sm transition hover:bg-[var(--surface-sunken)]"
              >
                API tokens
              </RouterLink>
              <button
                class="muted block w-full rounded-lg px-2 py-1.5 text-left text-sm transition hover:bg-[var(--surface-sunken)]"
                @click="session.logout()"
              >
                Sign out
              </button>
            </div>
          </DropdownMenu>
        </div>
      </div>

      <nav class="flex gap-1 overflow-x-auto px-3 pb-2">
        <RouterLink
          v-for="item in groups.flatMap((g) => g.items)"
          :key="item.name"
          :to="{ name: item.name }"
          class="flex shrink-0 items-center gap-1.5 rounded-[var(--radius-control)] px-2.5 py-1.5 text-sm transition"
          :class="active(item.name) ? 'font-medium' : 'muted'"
          :style="
            active(item.name)
              ? { background: 'var(--accent-soft)', color: 'var(--accent-text)' }
              : undefined
          "
        >
          <AppIcon :name="item.icon" class="size-4" />
          {{ item.label }}
        </RouterLink>
      </nav>
    </header>

    <!-- ── Content ─────────────────────────────────────────────────────────────── -->
    <main class="min-w-0 flex-1">
      <!-- Not centred. The rail is the left edge of the product, and content that drifts away
           from it on a wide screen reads as unmoored. Capped, but anchored: slack collects on
           the right, where there is nothing to be next to. -->
      <div class="max-w-7xl p-5 sm:p-6 lg:p-8">
        <!-- An offline host is not a footnote. Every action on the page below is about to
             fail, and saying so once, here, beats thirty individual errors. -->
        <div
          v-if="current && current.status === 'offline'"
          class="mb-5 flex items-start gap-2.5 rounded-[var(--radius-card)] px-4 py-3 text-sm"
          :style="{
            background: 'var(--danger-soft)',
            border: '1px solid color-mix(in oklch, var(--danger) 30%, transparent)',
          }"
          role="alert"
        >
          <AppIcon name="alert" class="mt-px size-4 shrink-0" :style="{ color: 'var(--danger)' }" />
          <div>
            <strong>{{ current.name }}</strong> is unreachable. Actions are unavailable until it
            comes back.
          </div>
        </div>

        <RouterView />
      </div>
    </main>

    <CommandPalette ref="palette" />
  </div>
</template>
