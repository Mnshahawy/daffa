<script setup lang="ts">
import { computed } from 'vue'
import type { CapArea, Capability } from '@/lib/api'

const props = defineProps<{
  /** The registry's areas, in display order. These are the section headers. */
  areas: CapArea[]
  capabilities: Capability[]
  /** The capability names currently granted. */
  modelValue: string[]
  disabled?: boolean
}>()

const emit = defineEmits<{ 'update:modelValue': [string[]] }>()

// A STANDALONE capability is one that is not half of a view/edit pair — an action that stands on
// its own: open a root shell, drain a node, read a Swarm's join token. The server marks these
// with an empty mode (Go's ModeStandalone), and that one signal is the whole declaration. Reading
// it here — rather than repeating a list of names — is what lets a standalone capability added in
// Go land in the "granted separately" section below without a frontend change. The list this
// replaced omitted nodes.edit and swarm.edit, so those two could not be granted from this screen
// at all. Standalone caps are shown apart from the view/edit grid on purpose: burying "open a root
// shell" in a row of tickboxes next to "see the list of images" is how it gets granted by accident.
function isStandalone(c: Capability): boolean {
  return c.mode === ''
}

interface Row {
  object: string
  view?: Capability
  edit?: Capability
}

interface Section {
  area: CapArea
  rows: Row[]
}

/**
 * One section per functional AREA, one row per object within it.
 *
 * The sections come from the server's area list, not from one written here. That is the whole
 * point of the server sending them: a capability added in Go lands in the right section without
 * a frontend change, and a capability whose area the UI did not know about would otherwise
 * vanish from the editor entirely — an administrator could never grant it, and nothing would
 * say why.
 */
const sections = computed<Section[]>(() =>
  props.areas
    .map((area) => {
      const byObject = new Map<string, Row>()
      for (const c of props.capabilities) {
        if (c.ns !== area.ns || isStandalone(c)) continue
        const row = byObject.get(c.object) ?? { object: c.object }
        if (c.mode === 'view') row.view = c
        if (c.mode === 'edit') row.edit = c
        byObject.set(c.object, row)
      }
      return { area, rows: [...byObject.values()] }
    })
    .filter((s) => s.rows.length > 0),
)

const standalone = computed(() => props.capabilities.filter(isStandalone))

const held = computed(() => new Set(props.modelValue))

function has(name?: string): boolean {
  return !!name && held.value.has(name)
}

function toggle(cap: Capability, on: boolean) {
  const next = new Set(props.modelValue)

  if (on) {
    next.add(cap.name)
    // Edit implies view — the server materialises this on save anyway, so showing the box
    // unticked while the server ticks it would make the screen lie about what was saved.
    if (cap.mode === 'edit') {
      const view = props.capabilities.find((c) => c.object === cap.object && c.mode === 'view')
      if (view) next.add(view.name)
    }
  } else {
    next.delete(cap.name)
    // Un-ticking view has to take edit with it, or the save would silently put view back and the
    // click would appear to do nothing.
    if (cap.mode === 'view') {
      const edit = props.capabilities.find((c) => c.object === cap.object && c.mode === 'edit')
      if (edit) next.delete(edit.name)
    }
  }

  emit('update:modelValue', [...next])
}

const labels: Record<string, string> = {
  containers: 'Containers',
  images: 'Images',
  networks: 'Networks',
  volumes: 'Volumes',
  stacks: 'Stacks',
  backups: 'Backups',
  storage: 'Storage targets',
  gitcreds: 'Git credentials',
  hosts: 'Hosts & agents',
  registries: 'Registries',
  users: 'Users',
  roles: 'Roles',
  settings: 'Authentication',
  monitors: 'Resource monitors',
  audit: 'Audit log',
}

// ── Legibility, not logic ─────────────────────────────────────────────────────
// Nothing below decides anything; it only counts what the code above already decided.
//
// A wall of forty identical checkboxes answers "is containers.exec ticked?" and refuses to
// answer "what can this role actually DO?" — which is the only question anyone opens this
// screen with. So each area carries its own tally, and the header carries the whole one.

/** Every capability offered as a box within a section — view and edit, in row order. */
function sectionCaps(s: Section): Capability[] {
  return s.rows.flatMap((r) => [r.view, r.edit].filter((c): c is Capability => !!c))
}

const tallies = computed(() => {
  const m = new Map<string, { held: number; total: number }>()
  for (const s of sections.value) {
    const caps = sectionCaps(s)
    m.set(s.area.ns, {
      held: caps.filter((c) => held.value.has(c.name)).length,
      total: caps.length,
    })
  }
  return m
})

const grantedTotal = computed(() => props.capabilities.filter((c) => held.value.has(c.name)).length)

/**
 * Granted reads as granted before you have read a word of it: the chip fills, its border comes
 * forward, and the box inside it carries a tick. Three signals, only one of which is colour —
 * roughly one reader in twelve cannot use the colour at all.
 */
function chipStyle(on: boolean) {
  return on
    ? {
        borderColor: 'var(--accent)',
        background: 'var(--accent-soft)',
        color: 'var(--accent-text)',
      }
    : { borderColor: 'var(--border)', color: 'var(--text-subtle)' }
}
</script>

<template>
  <div>
    <div class="mb-3 flex flex-wrap items-baseline gap-x-2 gap-y-1">
      <h3 class="text-sm font-semibold">Capabilities</h3>
      <span class="subtle font-mono text-xs">
        {{ grantedTotal }} of {{ capabilities.length }} granted
      </span>
      <p class="muted basis-full text-xs">
        Ticking <span class="font-mono">edit</span> ticks <span class="font-mono">view</span> with
        it — you cannot change what you cannot see.
      </p>
    </div>

    <div class="space-y-3">
      <section
        v-for="s in sections"
        :key="s.area.ns"
        class="overflow-hidden rounded-[var(--radius-card)] border"
        :style="{ borderColor: 'var(--border)' }"
      >
        <!-- One area, one header, one tally. The tally is what turns forty boxes into a
             sentence you can read from across the desk: "Docker 4/8, Access 0/6". -->
        <header
          class="flex flex-wrap items-baseline gap-x-2.5 gap-y-0.5 border-b px-4 py-2.5"
          :style="{ borderColor: 'var(--border)', background: 'var(--surface-sunken)' }"
        >
          <h4 class="eyebrow">{{ s.area.label }}</h4>
          <p class="muted min-w-0 flex-1 text-xs">{{ s.area.description }}</p>
          <span
            class="shrink-0 font-mono text-xs"
            :style="{
              color: tallies.get(s.area.ns)?.held ? 'var(--accent-text)' : 'var(--text-subtle)',
            }"
          >
            {{ tallies.get(s.area.ns)?.held ?? 0 }}/{{ tallies.get(s.area.ns)?.total ?? 0 }}
          </span>
        </header>

        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
                <th class="eyebrow px-4 py-1.5 text-left font-medium">Object</th>
                <th class="eyebrow w-24 py-1.5 text-center font-medium">View</th>
                <th class="eyebrow w-24 py-1.5 pr-4 text-center font-medium">Edit</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="row in s.rows"
                :key="row.object"
                class="border-b transition last:border-0 hover:bg-[var(--surface-sunken)]"
                :style="{ borderColor: 'var(--border)' }"
              >
                <td class="py-2.5 pr-4 pl-4">
                  <span class="font-medium">{{ labels[row.object] ?? row.object }}</span>
                  <p v-if="row.view" class="muted mt-0.5 text-xs">{{ row.view.description }}</p>
                </td>

                <td class="py-2.5 text-center">
                  <!-- The capability's real name lives on the chip, in mono, in the tooltip and
                       in its accessible name. It is what the audit log will call it, and what an
                       API token will be refused for. -->
                  <label
                    v-if="row.view"
                    :for="`cap-${row.view.name}`"
                    class="inline-flex items-center gap-1.5 rounded-[var(--radius-control)] border px-2 py-1 text-xs font-medium transition"
                    :style="chipStyle(has(row.view.name))"
                    :title="`${row.view.name} — ${row.view.description}`"
                  >
                    <input
                      :id="`cap-${row.view.name}`"
                      type="checkbox"
                      :checked="has(row.view.name)"
                      :disabled="disabled"
                      class="accent-[var(--accent)]"
                      :aria-label="row.view.name"
                      @change="toggle(row.view, ($event.target as HTMLInputElement).checked)"
                    />
                    View
                  </label>
                </td>

                <td class="py-2.5 pr-4 text-center">
                  <label
                    v-if="row.edit"
                    :for="`cap-${row.edit.name}`"
                    class="inline-flex items-center gap-1.5 rounded-[var(--radius-control)] border px-2 py-1 text-xs font-medium transition"
                    :style="chipStyle(has(row.edit.name))"
                    :title="`${row.edit.name} — ${row.edit.description}`"
                  >
                    <input
                      :id="`cap-${row.edit.name}`"
                      type="checkbox"
                      :checked="has(row.edit.name)"
                      :disabled="disabled"
                      class="accent-[var(--accent)]"
                      :aria-label="row.edit.name"
                      @change="toggle(row.edit, ($event.target as HTMLInputElement).checked)"
                    />
                    Edit
                  </label>
                  <span v-else class="subtle text-xs" title="This object cannot be edited">—</span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>
    </div>

    <!-- The standalone actions, apart and spelled out. -->
    <div
      class="mt-4 rounded-[var(--radius-card)] border p-4"
      :style="{
        borderColor: 'color-mix(in oklch, var(--warn) 35%, transparent)',
        background: 'var(--warn-soft)',
      }"
    >
      <p class="text-sm font-medium">Granted separately</p>
      <p class="muted mt-0.5 mb-3 text-xs">
        Each of these is an action on its own, never implied by an object's “edit”. Being trusted
        to restart a container is not the same as being trusted with a root shell on the host it
        runs on, or to drain a machine out from under everybody's workload.
      </p>

      <div class="space-y-1">
        <label
          v-for="c in standalone"
          :key="c.name"
          :for="`cap-${c.name}`"
          class="flex items-start gap-2.5 rounded-[var(--radius-control)] border px-2.5 py-2 transition"
          :style="
            has(c.name)
              ? {
                  borderColor: 'color-mix(in oklch, var(--warn) 45%, transparent)',
                  background: 'var(--surface-raised)',
                }
              : { borderColor: 'transparent' }
          "
        >
          <input
            :id="`cap-${c.name}`"
            type="checkbox"
            :checked="has(c.name)"
            :disabled="disabled"
            class="mt-0.5 accent-[var(--accent)]"
            @change="toggle(c, ($event.target as HTMLInputElement).checked)"
          />
          <span class="text-sm">
            <code class="font-mono text-xs">{{ c.name }}</code>
            <span class="muted block text-xs">{{ c.description }}</span>
          </span>
        </label>
      </div>
    </div>
  </div>
</template>
