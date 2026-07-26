<script setup lang="ts">
import { Comment, Fragment, computed, nextTick, onBeforeUnmount, onMounted, ref, useSlots } from 'vue'
import type { VNode } from 'vue'
import AppIcon from './AppIcon.vue'

/**
 * A select whose dropdown is OURS, not the browser's.
 *
 * A native `<select>` can be restyled shut but not open: the popup it drops is drawn by the OS,
 * ignores every theme token, and against a dark console reads as a different application — the same
 * reason the ComboBox next to it is hand-rolled. So this is the ComboBox's popover with a
 * fixed set of choices instead of free text: one themed listbox, positioned the same way, animated
 * the same way, so a select and a combobox in the same row are visibly the same control.
 *
 * The API is unchanged from the native version on purpose — the `<option>`s still go in the slot,
 * `v-model` (and `v-model.number`) still bind — so no call site had to be rewritten. The options are
 * read out of the slot's vnodes rather than rendered as DOM.
 *
 * What a real `<select>` gave for free and this cannot: the mobile system picker, and native
 * `required` validation. `required` is kept as a prop for intent, but an empty submit is now caught
 * server-side and surfaced as a toast rather than blocked in the browser — the trade for a themed
 * list. The interactive-listbox ARIA (activedescendant, roles) is built by hand below.
 */
const props = defineProps<{
  modelValue: string | number
  /** Populated by `v-model.number` — coerce the chosen option's value back to a number. */
  modelModifiers?: { number?: boolean }
  id?: string
  required?: boolean
  disabled?: boolean
  /** Shown greyed when nothing is selected yet — the empty state of a "Choose a cluster…" select. */
  placeholder?: string
  /** Classes for the trigger button itself: density/type, e.g. `py-1 text-xs` for a compact one. */
  selectClass?: string
}>()

const emit = defineEmits<{ 'update:modelValue': [string | number]; change: [] }>()

const slots = useSlots()

interface Opt {
  value: string | number
  label: string
  disabled: boolean
}

// The label text of an <option>, however its content was authored — a plain string, an
// interpolation, or a mix — flattened to what the user reads.
function optionText(node: VNode): string {
  const c = node.children as unknown
  if (typeof c === 'string') return c
  if (Array.isArray(c)) {
    return c
      .map((x) => (typeof x === 'string' ? x : x && typeof x === 'object' ? optionText(x as VNode) : ''))
      .join('')
  }
  return ''
}

// Walk the slot's vnodes into a flat option list. A v-for arrives as a Fragment whose children are
// the real options; a v-if that is false arrives as a Comment; both are seen through here.
function walk(nodes: VNode[], out: Opt[]) {
  for (const n of nodes) {
    if (!n || n.type === Comment) continue
    if (n.type === Fragment) {
      if (Array.isArray(n.children)) walk(n.children as VNode[], out)
      continue
    }
    if (n.type === 'option') {
      const p = (n.props ?? {}) as Record<string, unknown>
      out.push({
        value: (p.value as string | number) ?? '',
        label: optionText(n).trim(),
        disabled: p.disabled != null && p.disabled !== false,
      })
    }
  }
}

const options = computed<Opt[]>(() => {
  const out: Opt[] = []
  if (slots.default) walk(slots.default(), out)
  return out
})

const selected = computed(() => options.value.find((o) => String(o.value) === String(props.modelValue)))
const display = computed(() => selected.value?.label ?? '')

// A stable id per instance so each option can be pointed at by aria-activedescendant.
const uid = ++seq
const optionId = (i: number) => `sel-${uid}-opt-${i}`

const root = ref<HTMLElement>()
const button = ref<HTMLButtonElement>()
const panel = ref<HTMLElement>()
const open = ref(false)
const active = ref(-1)
const style = ref<Record<string, string>>({})

const GAP = 6
const EDGE = 8

function place() {
  const trigger = root.value?.getBoundingClientRect()
  const el = panel.value
  if (!trigger || !el) return
  const h = el.offsetHeight
  const vh = document.documentElement.clientHeight
  const roomBelow = vh - trigger.bottom - GAP
  const roomAbove = trigger.top - GAP
  const goTop = !(roomBelow >= h || roomBelow >= roomAbove)
  let top = goTop ? trigger.top - GAP - h : trigger.bottom + GAP
  top = Math.max(EDGE, Math.min(top, vh - h - EDGE))
  style.value = { top: `${top}px`, left: `${trigger.left}px`, width: `${trigger.width}px` }
}

async function show() {
  if (props.disabled || !options.value.length) return
  style.value = { top: '-9999px', left: '-9999px' }
  open.value = true
  // Open ONTO the current choice, so the first arrow-key press moves from where you are.
  active.value = options.value.findIndex((o) => String(o.value) === String(props.modelValue))
  if (active.value < 0) active.value = options.value.findIndex((o) => !o.disabled)
  await nextTick()
  place()
  scrollActiveIntoView()
}

function close() {
  open.value = false
  active.value = -1
}

function choose(i: number) {
  const opt = options.value[i]
  if (!opt || opt.disabled) return
  const v = props.modelModifiers?.number ? Number(opt.value) : opt.value
  close()
  button.value?.focus()
  if (String(v) === String(props.modelValue)) return
  emit('update:modelValue', v)
  emit('change')
}

// Skip disabled options when moving, so the highlight never lands somewhere Enter would refuse.
function move(delta: number) {
  const opts = options.value
  if (!opts.length) return
  let i = active.value
  for (let step = 0; step < opts.length; step++) {
    i = (i + delta + opts.length) % opts.length
    if (!opts[i].disabled) break
  }
  active.value = i
  scrollActiveIntoView()
}

async function scrollActiveIntoView() {
  await nextTick()
  panel.value?.querySelector('[data-active="true"]')?.scrollIntoView({ block: 'nearest' })
}

// Type a few letters to jump to a matching option, the way a native select does. The buffer
// clears itself after a pause so a new word starts fresh.
let typeahead = ''
let typeaheadTimer: number | undefined
function onType(key: string) {
  typeahead += key.toLowerCase()
  window.clearTimeout(typeaheadTimer)
  typeaheadTimer = window.setTimeout(() => (typeahead = ''), 600)
  const i = options.value.findIndex((o) => !o.disabled && o.label.toLowerCase().startsWith(typeahead))
  if (i >= 0) {
    if (open.value) {
      active.value = i
      scrollActiveIntoView()
    } else {
      choose(i)
    }
  }
}

function onKey(e: KeyboardEvent) {
  switch (e.key) {
    case 'ArrowDown':
      e.preventDefault()
      open.value ? move(1) : show()
      break
    case 'ArrowUp':
      e.preventDefault()
      open.value ? move(-1) : show()
      break
    case 'Home':
      if (open.value) {
        e.preventDefault()
        active.value = -1
        move(1)
      }
      break
    case 'End':
      if (open.value) {
        e.preventDefault()
        active.value = 0
        move(-1)
      }
      break
    case 'Enter':
    case ' ':
      e.preventDefault()
      open.value ? choose(active.value) : show()
      break
    case 'Escape':
      if (open.value) {
        e.preventDefault()
        close()
      }
      break
    case 'Tab':
      close()
      break
    default:
      if (e.key.length === 1 && !e.metaKey && !e.ctrlKey && !e.altKey) onType(e.key)
  }
}

function toggle() {
  open.value ? close() : show()
}

function onDocPointer(e: PointerEvent) {
  const t = e.target as Node
  if (root.value?.contains(t) || panel.value?.contains(t)) return
  close()
}
function onReflow() {
  if (open.value) place()
}

onMounted(() => {
  document.addEventListener('pointerdown', onDocPointer)
  window.addEventListener('scroll', onReflow, true)
  window.addEventListener('resize', onReflow)
})
onBeforeUnmount(() => {
  document.removeEventListener('pointerdown', onDocPointer)
  window.removeEventListener('scroll', onReflow, true)
  window.removeEventListener('resize', onReflow)
  window.clearTimeout(typeaheadTimer)
})
</script>

<script lang="ts">
let seq = 0
</script>

<template>
  <div ref="root" class="relative">
    <button
      :id="id"
      ref="button"
      type="button"
      class="field flex items-center pr-9 text-left"
      :class="selectClass"
      :disabled="disabled"
      role="combobox"
      aria-haspopup="listbox"
      :aria-expanded="open"
      :aria-required="required || undefined"
      :aria-activedescendant="open && active >= 0 ? optionId(active) : undefined"
      @click="toggle"
      @keydown="onKey"
    >
      <span class="min-w-0 flex-1 truncate" :class="!selected && 'text-[var(--text-subtle)]'">
        {{ display || placeholder }}
      </span>
    </button>

    <span
      class="pointer-events-none absolute inset-y-0 right-0 grid w-9 place-items-center text-[var(--text-subtle)] transition-transform"
      :class="[open && 'rotate-180', disabled && 'opacity-55']"
      aria-hidden="true"
    >
      <AppIcon name="chevronDown" class="size-4" />
    </span>

    <Teleport to="body">
      <Transition
        enter-active-class="transition duration-100 ease-out"
        enter-from-class="opacity-0 -translate-y-1"
        leave-active-class="transition duration-75 ease-in"
        leave-to-class="opacity-0"
      >
        <ul
          v-if="open"
          ref="panel"
          class="fixed z-40 max-h-60 overflow-auto rounded-xl p-1 shadow-[var(--shadow-overlay)]"
          :style="{ ...style, background: 'var(--surface-overlay)', border: '1px solid var(--border)' }"
          role="listbox"
        >
          <li
            v-for="(o, i) in options"
            :id="optionId(i)"
            :key="i"
            :data-active="i === active"
            role="option"
            :aria-selected="String(o.value) === String(modelValue)"
            :aria-disabled="o.disabled || undefined"
            class="flex items-center gap-2 rounded-lg px-2.5 py-1.5 text-sm"
            :class="[
              o.disabled
                ? 'cursor-default text-[var(--text-subtle)]'
                : i === active
                  ? 'cursor-pointer bg-[var(--surface-sunken)]'
                  : 'cursor-pointer',
            ]"
            @mouseenter="!o.disabled && (active = i)"
            @click="choose(i)"
          >
            <span class="min-w-0 flex-1 truncate">{{ o.label }}</span>
            <!-- The current choice is marked, the one thing a fixed list has that a combobox does not. -->
            <AppIcon
              v-if="String(o.value) === String(modelValue) && !o.disabled"
              name="check"
              class="size-3.5 shrink-0 text-[var(--accent-text)]"
            />
          </li>
        </ul>
      </Transition>
    </Teleport>
  </div>
</template>
