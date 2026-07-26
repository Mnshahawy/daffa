<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import AppIcon from './AppIcon.vue'

/**
 * A free-text field with suggestions — the styled replacement for `<input list>` + `<datalist>`.
 *
 * The native datalist is the right SHAPE (type freely, or pick from a list) and the wrong look:
 * its popup is drawn by the browser, ignores every theme token, renders detached and misaligned,
 * and against a dark app reads as a bug. It cannot be styled at all. So the shape is kept and the
 * popup is rebuilt — a themed panel, teleported and positioned against the field the same way
 * DropdownMenu does, so a scrolling ancestor cannot clip it.
 *
 * The value stays FREE TEXT on purpose: these are name filters ("" = any), not a closed set, so a
 * value that matches nothing in the list is legal — the list simply empties. The suggestions are a
 * shortcut, never a gate.
 */
const props = withDefaults(
  defineProps<{
    modelValue: string
    options: string[]
    id?: string
    placeholder?: string
    /** Extra classes for the input — e.g. `font-mono text-xs` to match a monospaced form. */
    inputClass?: string
  }>(),
  { placeholder: 'any' },
)

const emit = defineEmits<{ 'update:modelValue': [string] }>()

const root = ref<HTMLElement>()
const inputEl = ref<HTMLInputElement>()
const panel = ref<HTMLElement>()
const open = ref(false)
const active = ref(-1)
const style = ref<Record<string, string>>({})

// Empty or "the value IS one of the options" both show the whole list — you have picked something,
// and reopening to browse the rest should not require clearing the field first. Otherwise it is a
// case-insensitive substring match.
const matches = computed(() => {
  const q = props.modelValue.trim().toLowerCase()
  if (!q) return props.options
  if (props.options.some((o) => o.toLowerCase() === q)) return props.options
  return props.options.filter((o) => o.toLowerCase().includes(q))
})

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
  // Below unless it does not fit and above fits better — the same rule DropdownMenu flips on.
  const goTop = !(roomBelow >= h || roomBelow >= roomAbove)
  let top = goTop ? trigger.top - GAP - h : trigger.bottom + GAP
  top = Math.max(EDGE, Math.min(top, vh - h - EDGE))
  style.value = { top: `${top}px`, left: `${trigger.left}px`, width: `${trigger.width}px` }
}

async function show() {
  if (!matches.value.length) {
    open.value = false
    return
  }
  // Parked off-screen for the frame before it can be measured — a fixed box with no coordinates
  // would flash at the top of the document first.
  style.value = { top: '-9999px', left: '-9999px' }
  open.value = true
  await nextTick()
  place()
}

function close() {
  open.value = false
  active.value = -1
}

function choose(option: string) {
  emit('update:modelValue', option)
  close()
  inputEl.value?.focus()
}

// Opening is driven by the user's intent — a click on the field, a keystroke, an arrow, the
// chevron — never by `focus`. Focus fires again when `choose` returns focus to the input after a
// pick, and opening on it would spring the panel straight back up the instant a selection is made.
function onClickInput() {
  if (!open.value) void show()
}

function onInput(e: Event) {
  emit('update:modelValue', (e.target as HTMLInputElement).value)
  active.value = -1
  // props.modelValue (and so `matches`) updates after the parent re-renders, so decide on the next
  // tick: empty ⇒ nothing to suggest, already open ⇒ just re-fit, otherwise open.
  void nextTick(() => {
    if (!matches.value.length) return close()
    if (open.value) return place()
    void show()
  })
}

function onKey(e: KeyboardEvent) {
  if (e.key === 'ArrowDown') {
    e.preventDefault()
    if (!open.value) return void show()
    active.value = Math.min(active.value + 1, matches.value.length - 1)
    scrollActiveIntoView()
  } else if (e.key === 'ArrowUp') {
    e.preventDefault()
    active.value = Math.max(active.value - 1, 0)
    scrollActiveIntoView()
  } else if (e.key === 'Enter') {
    // Only steal Enter when a suggestion is highlighted — otherwise it must reach the form so the
    // field does not swallow submit.
    if (open.value && active.value >= 0) {
      e.preventDefault()
      choose(matches.value[active.value])
    }
  } else if (e.key === 'Escape') {
    if (open.value) {
      e.preventDefault()
      close()
    }
  }
}

async function scrollActiveIntoView() {
  await nextTick()
  panel.value?.querySelector('[data-active="true"]')?.scrollIntoView({ block: 'nearest' })
}

// The chevron opens the full list without needing a keystroke, and closes it if already open —
// the affordance the native select's chevron gives, kept.
function toggle() {
  if (open.value) return close()
  inputEl.value?.focus()
  void show()
}

// The value can change from outside (the parent resets the form, or a linked field clears this
// one). If the panel is open when that happens, re-filter and re-measure.
watch(
  () => [props.modelValue, props.options] as const,
  () => {
    if (!open.value) return
    if (!matches.value.length) return close()
    void nextTick(place)
  },
)

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
})
</script>

<template>
  <div ref="root" class="relative">
    <input
      :id="id"
      ref="inputEl"
      :value="modelValue"
      :placeholder="placeholder"
      class="field pr-9"
      :class="inputClass"
      autocomplete="off"
      role="combobox"
      aria-autocomplete="list"
      :aria-expanded="open"
      data-cursor="text"
      @input="onInput"
      @click="onClickInput"
      @keydown="onKey"
    />
    <!-- Sits on top of the input's right padding; the input still owns the click everywhere else. -->
    <button
      type="button"
      tabindex="-1"
      aria-label="Toggle suggestions"
      class="absolute inset-y-0 right-0 grid w-9 place-items-center text-[var(--text-subtle)] transition-transform"
      :class="open && 'rotate-180'"
      @click="toggle"
    >
      <AppIcon name="chevronDown" class="size-4" />
    </button>

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
            v-for="(o, i) in matches"
            :key="o"
            :data-active="i === active"
            role="option"
            :aria-selected="o === modelValue"
            class="cursor-pointer truncate rounded-lg px-2.5 py-1.5 text-sm"
            :class="i === active ? 'bg-[var(--surface-sunken)]' : 'hover:bg-[var(--surface-sunken)]'"
            @mouseenter="active = i"
            @click="choose(o)"
          >
            {{ o }}
          </li>
        </ul>
      </Transition>
    </Teleport>
  </div>
</template>
