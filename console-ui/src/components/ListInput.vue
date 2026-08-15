<script setup lang="ts">
import { computed, nextTick, ref } from 'vue'
import AppIcon from './AppIcon.vue'

/**
 * A list of short values, edited as a list.
 *
 * The thing this replaces is a plain text input holding "a.example.com b.example.com
 * 10.0.0.5" and a hint underneath explaining the separator. That control lies twice: the
 * value is a LIST everywhere else in the system (the API takes an array, the manifest takes
 * a YAML sequence), and the operator cannot see where one entry ends — a trailing space, a
 * comma someone's editor inserted, a value that quietly merged with its neighbour. Every one
 * of those is invisible until a certificate does not match the host it was meant for.
 *
 * So the value here IS `string[]`, and each entry is a chip you can see the boundaries of and
 * remove on its own. Typing stays fast, because the list is still just text under the hands:
 *
 *   - Enter, comma or Tab commits what is typed;
 *   - a paste of "a.example.com, b.example.com" (or several lines) commits every entry at once,
 *     which is the shape values arrive in — out of a terminal, a ticket, another console;
 *   - Backspace on an empty box takes the last chip back for editing rather than deleting it
 *     outright, so a typo at the end costs one keystroke, not a re-type;
 *   - blur commits too. Clicking "Issue" with a half-typed entry must not silently drop it.
 *
 * `describe` labels each chip with what its value MEANS to the server (SANs use it to show
 * dns / ip / uri). It is a mirror of a decision the backend already made, never a second
 * implementation of it: the point is to let the operator confirm that `spiffe://…` was read
 * as an identity and not as a hostname, before they find out from a failed handshake.
 */
const props = withDefaults(
  defineProps<{
    modelValue: string[]
    id?: string
    placeholder?: string
    /** Extra classes for the text box and the chips — e.g. `font-mono text-xs`. */
    inputClass?: string
    /** Blocks form submit while the list is empty, like `required` on an input. */
    required?: boolean
    /** Short label for what one entry is, shown inside its chip. */
    describe?: (value: string) => string
  }>(),
  { placeholder: 'Type a value, press Enter' },
)

const emit = defineEmits<{ 'update:modelValue': [string[]] }>()

const draft = ref('')
const inputEl = ref<HTMLInputElement>()

// Separators are whitespace and commas — the two ways a list arrives pasted. Nothing this
// control holds (a host name, an address, a URI) may contain either, so splitting can never
// cut a value in half.
const SEPARATORS = /[\s,]+/

const empty = computed(() => props.modelValue.length === 0)

function commit(text: string) {
  const parts = text.split(SEPARATORS).filter(Boolean)
  if (!parts.length) return
  // Duplicates are dropped rather than refused: adding a value already in the list is a
  // no-op the operator meant, not an error worth a message.
  const next = [...props.modelValue]
  for (const p of parts) if (!next.includes(p)) next.push(p)
  emit('update:modelValue', next)
}

function commitDraft() {
  commit(draft.value)
  draft.value = ''
}

function remove(i: number) {
  emit('update:modelValue', props.modelValue.filter((_, j) => j !== i))
  inputEl.value?.focus()
}

function onKey(e: KeyboardEvent) {
  if (e.key === 'Enter' || e.key === ',') {
    // Enter must never reach the form here: in a one-field form the browser would submit,
    // and the entry being typed would be lost on the way.
    e.preventDefault()
    commitDraft()
  } else if (e.key === 'Tab' && draft.value.trim()) {
    e.preventDefault()
    commitDraft()
  } else if (e.key === 'Backspace' && !draft.value && !empty.value) {
    e.preventDefault()
    const last = props.modelValue[props.modelValue.length - 1]
    emit('update:modelValue', props.modelValue.slice(0, -1))
    draft.value = last
  }
}

function onPaste(e: ClipboardEvent) {
  const text = e.clipboardData?.getData('text') ?? ''
  if (!SEPARATORS.test(text)) return // a single value: let it land in the box and be edited
  e.preventDefault()
  commit(draft.value + text)
  draft.value = ''
}

// The chips are not focusable, so a click anywhere in the box should land in the text box —
// the whole control has to behave like the single input it replaces.
function focusInput(e: MouseEvent) {
  if ((e.target as HTMLElement).closest('button')) return
  inputEl.value?.focus()
}

defineExpose({ focus: () => nextTick(() => inputEl.value?.focus()) })
</script>

<template>
  <div
    class="field flex flex-wrap items-center gap-1.5 py-1.5 focus-within:border-[var(--accent)] focus-within:shadow-[0_0_0_3px_var(--accent-soft)]"
    data-cursor="text"
    @mousedown="focusInput"
  >
    <span
      v-for="(v, i) in modelValue"
      :key="`${v}-${i}`"
      class="inline-flex max-w-full items-center gap-1.5 rounded-md py-0.5 pl-2 pr-1"
      :class="inputClass"
      :style="{ background: 'var(--surface-overlay)', border: '1px solid var(--border)' }"
    >
      <span class="truncate">{{ v }}</span>
      <span v-if="describe" class="eyebrow shrink-0 text-[0.5625rem] leading-none">{{ describe(v) }}</span>
      <button
        type="button"
        class="shrink-0 rounded p-0.5 text-[var(--text-subtle)] hover:text-[var(--text)]"
        :aria-label="`Remove ${v}`"
        @click="remove(i)"
      >
        <AppIcon name="x" class="size-3" />
      </button>
    </span>

    <input
      :id="id"
      ref="inputEl"
      v-model="draft"
      type="text"
      class="min-w-32 flex-1 bg-transparent outline-none"
      :class="inputClass"
      :placeholder="empty ? placeholder : ''"
      :required="required && empty"
      autocomplete="off"
      autocapitalize="off"
      spellcheck="false"
      @keydown="onKey"
      @paste="onPaste"
      @blur="commitDraft"
    />
  </div>
</template>
