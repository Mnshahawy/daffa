<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { useQuery } from '@tanstack/vue-query'
import { daffa } from '@/lib/api'
import { allNavItems, allSettingsTabs } from '@/lib/nav'
import { useSession } from '@/stores/session'
import { containerStatus } from '@/lib/status'
import AppIcon from './AppIcon.vue'
import StatusPill from './StatusPill.vue'
import type { IconName } from '@/lib/icons'

/**
 * ⌘K — go anywhere, by name.
 *
 * This is the answer to "hard to navigate" that no amount of rearranging a menu gives you:
 * once you know the name of the thing, the hierarchy stops mattering. You do not go
 * Containers → filter → scroll → click. You type three letters of it.
 *
 * ⌘K, not Dokploy's ⌘J. ⌘K is the convention — Slack, Linear, GitHub, VS Code — and picking
 * a different key for the sake of it means every new user's first instinct fails silently.
 *
 * It searches what actually exists on this host, not just the menu: stacks and containers by
 * name, plus every page and settings tab the person is allowed to open.
 */
const router = useRouter()
const session = useSession()

const open = ref(false)
const q = ref('')
const cursor = ref(0)
const input = ref<HTMLInputElement>()
const listEl = ref<HTMLElement>()

// Only fetched while the palette is open — this is a navigation aid, not a reason to hold two
// more queries live on every page in the app.
const { data: stacks } = useQuery({
  queryKey: ['stacks'],
  queryFn: daffa.stacks,
  enabled: open,
})

const { data: containers } = useQuery({
  queryKey: ['containers', () => session.envId],
  queryFn: () => daffa.containers(session.envId),
  enabled: computed(() => open.value && !!session.envId),
})

interface Entry {
  id: string
  label: string
  hint: string
  icon: IconName
  group: string
  to: { name: string; params?: Record<string, string> }
  /** Container rows carry their live state — you often only wanted to know if it was up. */
  state?: string
  statusText?: string
}

const entries = computed<Entry[]>(() => {
  const out: Entry[] = []

  for (const i of allNavItems) {
    if (!session.canAnywhere(i.cap)) continue
    out.push({
      id: `nav:${i.name}`,
      label: i.label,
      hint: i.hint,
      icon: i.icon,
      group: 'Go to',
      to: { name: i.name },
    })
  }

  for (const s of stacks.value ?? []) {
    if (s.env_id !== session.envId) continue
    out.push({
      id: `stack:${s.id}`,
      label: s.name,
      hint: s.group_name ? `Stack · ${s.group_name}` : 'Stack',
      icon: 'layers',
      group: 'Stacks',
      to: { name: 'stack', params: { id: s.id } },
    })
  }

  for (const c of containers.value ?? []) {
    out.push({
      id: `container:${c.id}`,
      label: c.service || c.name,
      hint: c.image,
      icon: 'box',
      group: 'Containers',
      to: { name: 'container', params: { id: c.id } },
      state: c.state,
      statusText: c.status,
    })
  }

  for (const t of allSettingsTabs) {
    if (!session.canAnywhere(t.cap)) continue
    out.push({
      id: `settings:${t.name}`,
      label: t.label,
      hint: t.hint,
      icon: t.icon,
      group: 'Settings',
      to: { name: t.name },
    })
  }

  return out
})

// Subsequence match, not substring: "cnt" finds "containers" and "pgb" finds "postgres-backup".
// It is how every palette worth using behaves, and typing the whole word defeats the point.
function score(e: Entry, query: string): number {
  const hay = `${e.label} ${e.hint}`.toLowerCase()
  const label = e.label.toLowerCase()

  if (label.startsWith(query)) return 0
  if (label.includes(query)) return 1
  if (hay.includes(query)) return 2

  let i = 0
  for (const ch of label) {
    if (ch === query[i]) i++
    if (i === query.length) return 3
  }
  return -1
}

const results = computed(() => {
  const query = q.value.trim().toLowerCase()
  if (!query) return entries.value.slice(0, 12)

  return entries.value
    .map((e) => ({ e, s: score(e, query) }))
    .filter((r) => r.s >= 0)
    .sort((a, b) => a.s - b.s)
    .slice(0, 20)
    .map((r) => r.e)
})

// Results are shown grouped, but the cursor walks the FLAT list — otherwise arrow-down would
// have to know about headings, and it does not need to.
const grouped = computed(() => {
  const g = new Map<string, Entry[]>()
  for (const e of results.value) {
    if (!g.has(e.group)) g.set(e.group, [])
    g.get(e.group)!.push(e)
  }
  return [...g.entries()]
})

function flatIndex(e: Entry): number {
  return results.value.findIndex((r) => r.id === e.id)
}

watch(results, () => (cursor.value = 0))

async function show() {
  open.value = true
  q.value = ''
  cursor.value = 0
  await nextTick()
  input.value?.focus()
}

function hide() {
  open.value = false
}

function choose(e?: Entry) {
  const target = e ?? results.value[cursor.value]
  if (!target) return
  router.push(target.to)
  hide()
}

function move(delta: number) {
  const n = results.value.length
  if (!n) return
  cursor.value = (cursor.value + delta + n) % n
  nextTick(() => {
    listEl.value
      ?.querySelector('[data-active="true"]')
      ?.scrollIntoView({ block: 'nearest' })
  })
}

function onGlobalKey(e: KeyboardEvent) {
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
    e.preventDefault()
    open.value ? hide() : show()
  }
}

function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape') return hide()
  if (e.key === 'ArrowDown') {
    e.preventDefault()
    move(1)
  }
  if (e.key === 'ArrowUp') {
    e.preventDefault()
    move(-1)
  }
  if (e.key === 'Enter') {
    e.preventDefault()
    choose()
  }
}

onMounted(() => document.addEventListener('keydown', onGlobalKey))
onBeforeUnmount(() => document.removeEventListener('keydown', onGlobalKey))

defineExpose({ show })
</script>

<template>
  <Teleport to="body">
    <Transition
      enter-active-class="transition duration-150 ease-out"
      enter-from-class="opacity-0"
      leave-active-class="transition duration-100 ease-in"
      leave-to-class="opacity-0"
    >
      <div
        v-if="open"
        class="fixed inset-0 z-50 flex items-start justify-center p-4 pt-[12vh]"
        style="background: color-mix(in oklch, black 40%, transparent)"
        @click.self="hide"
      >
        <div
          class="w-full max-w-xl overflow-hidden rounded-xl shadow-[var(--shadow-overlay)]"
          style="background: var(--surface-overlay); border: 1px solid var(--border)"
          role="dialog"
          aria-modal="true"
          aria-label="Search"
        >
          <div class="flex items-center gap-2.5 border-b px-4" :style="{ borderColor: 'var(--border)' }">
            <AppIcon name="search" class="subtle size-4 shrink-0" />
            <input
              ref="input"
              v-model="q"
              class="flex-1 bg-transparent py-3.5 text-sm outline-none placeholder:text-[var(--text-subtle)]"
              placeholder="Search stacks, containers and pages…"
              autocomplete="off"
              spellcheck="false"
              aria-autocomplete="list"
              @keydown="onKey"
            />
            <kbd
              class="rounded border px-1.5 py-0.5 font-mono text-[10px] subtle"
              :style="{ borderColor: 'var(--border)' }"
            >
              esc
            </kbd>
          </div>

          <div ref="listEl" class="max-h-80 overflow-y-auto p-1.5" role="listbox">
            <p v-if="!results.length" class="muted px-3 py-8 text-center text-sm">
              Nothing matches “{{ q }}”.
            </p>

            <div v-for="[group, items] in grouped" :key="group">
              <div class="eyebrow px-2.5 pb-1 pt-2">{{ group }}</div>

              <button
                v-for="e in items"
                :key="e.id"
                role="option"
                :aria-selected="flatIndex(e) === cursor"
                :data-active="flatIndex(e) === cursor"
                class="flex w-full items-center gap-2.5 rounded-lg px-2.5 py-2 text-left transition"
                :style="
                  flatIndex(e) === cursor
                    ? { background: 'var(--accent-soft)', color: 'var(--accent-text)' }
                    : undefined
                "
                @mousemove="cursor = flatIndex(e)"
                @click="choose(e)"
              >
                <AppIcon :name="e.icon" class="size-4 shrink-0 opacity-70" />

                <span class="min-w-0 flex-1">
                  <span class="block truncate text-sm font-medium">{{ e.label }}</span>
                  <span class="muted block truncate font-mono text-[11px]">{{ e.hint }}</span>
                </span>

                <StatusPill
                  v-if="e.state"
                  :status="containerStatus(e.state, e.statusText)"
                  variant="dot"
                />
              </button>
            </div>
          </div>

          <div
            class="flex items-center gap-3 border-t px-3 py-2 text-[11px] subtle"
            :style="{ borderColor: 'var(--border)' }"
          >
            <span><kbd class="font-mono">↑↓</kbd> navigate</span>
            <span><kbd class="font-mono">↵</kbd> open</span>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>
