<script setup lang="ts">
/**
 * A secret value you can both READ and EDIT in one field — masked by default, with the reveal
 * control inset at the trailing edge so the row reads like any other input.
 *
 * The security posture is intact: for an EXISTING secret the plaintext is not in the page until the
 * eye is clicked (it is fetched then from an audited endpoint), and hiding it again drops it from the
 * DOM. The eye is shown only when the viewer may see the value — `canReveal` for a stored secret,
 * `canWrite` for one being authored (you can always see what you are typing). The server checks the
 * capability on every reveal, so the hidden button is a courtesy, never the guard.
 *
 * v-model is the value to SAVE. For an existing secret it stays "" — meaning "unchanged" — until the
 * field is actually edited; revealing alone does not count as a change, so a reveal-and-save keeps
 * the stored value byte-for-byte.
 */
import { ref, watch } from 'vue'
import AppIcon from './ui/AppIcon.vue'
import BaseButton from './ui/BaseButton.vue'
import CopyButton from './ui/CopyButton.vue'

const props = defineProps<{
  modelValue: string
  /** A saved secret (value fetched on reveal, "" means keep) vs a new one being authored. */
  existing: boolean
  canWrite: boolean
  canReveal: boolean
  /** Fetches the stored plaintext. Only called for an existing secret, on first reveal. */
  reveal: () => Promise<string>
  multiline?: boolean
  inputId?: string
}>()
const emit = defineEmits<{ 'update:modelValue': [string] }>()

// text is what the field holds. An existing secret starts masked and empty (its plaintext was never
// sent); a new one — or a value the parent already has in hand — starts visible.
const text = ref(props.modelValue ?? '')
const shown = ref(!props.existing || !!props.modelValue)
const dirty = ref(false)
const loading = ref(false)
const error = ref('')

// The eye reveals a STORED secret (needs the cap) or toggles a value you are typing (always yours).
const showEye = () => (props.existing ? props.canReveal : props.canWrite)

// Emit "" for an existing secret nobody has edited — the server reads that as "keep the current
// value". Anything typed, or a new secret, is sent as-is.
watch([text, dirty], () => emit('update:modelValue', props.existing && !dirty.value ? '' : text.value))

function onInput(e: Event) {
  text.value = (e.target as HTMLInputElement | HTMLTextAreaElement).value
  dirty.value = true
}

async function toggle() {
  error.value = ''
  if (shown.value) {
    shown.value = false
    if (props.existing && !dirty.value) text.value = '' // re-mask: the fetched plaintext leaves the DOM
    return
  }
  // Fetch the stored value the first time an untouched existing secret is revealed.
  if (props.existing && !dirty.value && text.value === '') {
    loading.value = true
    try {
      text.value = await props.reveal()
    } catch (e) {
      error.value = e instanceof Error ? e.message : 'Could not reveal.'
      loading.value = false
      return
    }
    loading.value = false
  }
  shown.value = true
}

const placeholder = () =>
  props.existing && !dirty.value
    ? '•••••••• (unchanged)'
    : props.multiline
      ? 'file contents'
      : 'value'
</script>

<template>
  <div class="relative min-w-0">
    <!-- A file secret cannot be password-masked, so an unrevealed one is simply empty (its plaintext
         was never sent); the placeholder says "unchanged", and the eye fetches it on demand. -->
    <textarea
      v-if="multiline"
      :id="inputId"
      :value="text"
      :disabled="!canWrite"
      rows="3"
      :placeholder="placeholder()"
      class="field py-1.5 pr-9 font-mono text-xs"
      data-cursor="text"
      @input="onInput"
    />
    <input
      v-else
      :id="inputId"
      :type="shown ? 'text' : 'password'"
      :value="text"
      :disabled="!canWrite"
      :placeholder="placeholder()"
      class="field py-1.5 font-mono text-xs"
      :class="showEye() ? (shown && text ? 'pr-14' : 'pr-9') : ''"
      data-cursor="text"
      @input="onInput"
    />

    <!-- The masking control, inset at the trailing edge. Copy rides alongside once revealed. -->
    <div
      class="absolute right-1.5 flex items-center gap-0.5"
      :class="multiline ? 'top-1.5' : 'inset-y-0'"
    >
      <CopyButton v-if="shown && text" :text="text" />
      <BaseButton
        v-if="showEye()"
        intent="ghost"
        size="xs"
        icon
        :loading="loading"
        :label="shown ? 'Hide value' : 'Reveal value'"
        :title="shown ? 'Hide' : 'Reveal'"
        @click="toggle"
      >
        <AppIcon :name="shown ? 'eyeOff' : 'eye'" class="size-3.5" />
      </BaseButton>
    </div>

    <p v-if="error" class="mt-1 text-xs" :style="{ color: 'var(--danger)' }">{{ error }}</p>
  </div>
</template>
