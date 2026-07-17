<script setup lang="ts">
import { nextTick, onBeforeUnmount, onMounted, ref } from 'vue'

// A small menu primitive. Deliberately hand-rolled rather than pulled from a component library:
// it is a button, a list, and two dismissal rules, and a dependency for that would cost more
// bytes than the whole thing.
//
// The panel is TELEPORTED to <body> and positioned against the trigger, rather than absolutely
// positioned inside it. An absolute panel is clipped by any ancestor that scrolls, and the
// tables here scroll horizontally — so the row kebab, whose entire job is to hold the actions
// that did not fit, was being cut off by the very container it lived in. A short table clipped
// it worst of all: there was no row below to spill into.
//
// Teleporting also means the menu cannot be trapped by the rail's overflow, and cannot be
// painted under a sibling that happens to establish a stacking context.
const props = withDefaults(
  defineProps<{
    align?: 'left' | 'right'
    /** Preferred side. Honoured when there is room, flipped when there is not. */
    placement?: 'bottom' | 'top'
    /** Trigger fills its container. For the rail, where every row is full-width. */
    block?: boolean
  }>(),
  { align: 'left', placement: 'bottom' },
)

const open = ref(false)
const root = ref<HTMLElement>()
const panel = ref<HTMLElement>()
const style = ref<Record<string, string>>({})

// Breathing room between the trigger and the panel, and between the panel and the viewport edge.
const GAP = 6
const EDGE = 8

function place() {
  const trigger = root.value?.getBoundingClientRect()
  const el = panel.value
  if (!trigger || !el) return

  // offsetWidth/Height, not getBoundingClientRect: the enter transition translates the panel,
  // and a transformed rect would measure the animation rather than the box.
  const w = el.offsetWidth
  const h = el.offsetHeight
  const vw = document.documentElement.clientWidth
  const vh = document.documentElement.clientHeight

  const roomBelow = vh - trigger.bottom - GAP
  const roomAbove = trigger.top - GAP

  // Open the way we were asked to, unless it does not fit and the other way does.
  const wantTop = props.placement === 'top'
  const goTop = wantTop ? roomAbove >= h || roomAbove >= roomBelow : !(roomBelow >= h || roomBelow >= roomAbove)

  let top = goTop ? trigger.top - GAP - h : trigger.bottom + GAP
  top = Math.max(EDGE, Math.min(top, vh - h - EDGE))

  let left = props.align === 'right' ? trigger.right - w : trigger.left
  left = Math.max(EDGE, Math.min(left, vw - w - EDGE))

  style.value = {
    top: `${top}px`,
    left: `${left}px`,
    // A full-width trigger gets a panel that lines up with it, rather than one that
    // mysteriously stops short.
    ...(props.block ? { minWidth: `${trigger.width}px` } : {}),
  }
}

async function toggle() {
  if (open.value) return close()

  // Parked off-screen for the frame between existing and being measured. A `fixed` element with
  // no coordinates falls back to its static position, which would put it briefly at the top of
  // the document — visible as a flash in the wrong corner.
  style.value = { top: '-9999px', left: '-9999px' }
  open.value = true

  // The panel has to exist before it can be measured.
  await nextTick()
  place()
}

function close() {
  open.value = false
}

function onDocClick(e: MouseEvent) {
  const target = e.target as Node
  // The panel is no longer inside `root`, so it needs its own check — otherwise the first click
  // on any menu item would be read as a click outside and dismiss the menu before it fired.
  if (root.value?.contains(target) || panel.value?.contains(target)) return
  close()
}

function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape') close()
}

// Fixed coordinates go stale the moment anything moves. Capture phase, because the thing that
// scrolls is usually an inner container (a table), not the window.
function onReflow() {
  if (open.value) place()
}

onMounted(() => {
  document.addEventListener('click', onDocClick)
  document.addEventListener('keydown', onKey)
  window.addEventListener('scroll', onReflow, true)
  window.addEventListener('resize', onReflow)
})

onBeforeUnmount(() => {
  document.removeEventListener('click', onDocClick)
  document.removeEventListener('keydown', onKey)
  window.removeEventListener('scroll', onReflow, true)
  window.removeEventListener('resize', onReflow)
})

defineExpose({ close })
</script>

<template>
  <div ref="root" class="relative">
    <button
      type="button"
      class="flex items-center gap-1.5"
      :class="block ? 'w-full' : ''"
      :aria-expanded="open"
      aria-haspopup="menu"
      @click="toggle"
    >
      <slot name="trigger" :open="open" />
    </button>

    <Teleport to="body">
      <Transition
        enter-active-class="transition duration-100 ease-out"
        enter-from-class="opacity-0 -translate-y-1"
        leave-active-class="transition duration-75 ease-in"
        leave-to-class="opacity-0"
      >
        <div
          v-if="open"
          ref="panel"
          class="fixed z-40 min-w-52 overflow-hidden rounded-xl p-1 shadow-[var(--shadow-overlay)]"
          :style="{
            ...style,
            background: 'var(--surface-overlay)',
            border: '1px solid var(--border)',
          }"
          role="menu"
          @click="close"
        >
          <slot />
        </div>
      </Transition>
    </Teleport>
  </div>
</template>
