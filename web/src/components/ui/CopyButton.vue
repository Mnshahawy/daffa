<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue'
import { toast } from '@/lib/toast'
import BaseButton from './BaseButton.vue'
import AppIcon from './AppIcon.vue'

/**
 * The one copy-to-clipboard button.
 *
 * Before this existed, "copy" was reinvented at every call site — seven of them, and no two
 * agreed on whether anything happened when you clicked. Most did nothing visible at all:
 * `navigator.clipboard.writeText(x)` and no feedback, so the only way to know the copy landed
 * was to paste somewhere and check. A couple hand-rolled a check-mark swap. One returned a
 * rejected promise on a clipboard permission denial that nobody caught, so it failed silently.
 *
 * So: click copies, the glyph flips to a green check and the label to "Copied" for a beat, and
 * a clipboard that refuses (insecure context, denied permission) says so out loud instead of
 * looking like it worked. `intent`/`size` pass straight through to BaseButton so it drops into
 * a toolbar or a prose block unchanged.
 */
const props = withDefaults(
  defineProps<{
    text: string
    intent?: 'primary' | 'secondary' | 'ghost' | 'caution' | 'danger' | 'danger-solid' | 'link'
    size?: 'xs' | 'sm' | 'md'
    /** The resting label. "" for an icon-only button (still needs `ariaLabel`). */
    label?: string
    copiedLabel?: string
    /** Accessible name — required when `label` is empty. */
    ariaLabel?: string
    disabled?: boolean
  }>(),
  { intent: 'ghost', size: 'sm', label: 'Copy', copiedLabel: 'Copied' },
)

const emit = defineEmits<{ copied: [] }>()

const copied = ref(false)
let timer: number | undefined

const iconSize = computed(() => (props.size === 'md' ? 'size-4' : props.size === 'xs' ? 'size-3' : 'size-3.5'))

async function onClick() {
  try {
    await navigator.clipboard.writeText(props.text)
  } catch {
    // Clipboard access is gated on a secure context and a user gesture; when the browser
    // refuses, saying "Copied" would be a lie the operator only discovers on paste.
    toast.warn('Could not copy — copy it manually.')
    return
  }
  emit('copied')
  copied.value = true
  window.clearTimeout(timer)
  timer = window.setTimeout(() => (copied.value = false), 1500)
}

onBeforeUnmount(() => window.clearTimeout(timer))
</script>

<template>
  <BaseButton
    :intent="intent"
    :size="size"
    :disabled="disabled"
    :label="ariaLabel"
    @click="onClick"
  >
    <!-- The check inherits the success colour so "it worked" is legible before the word is
         read; the swap is keyed so the reduced-motion rule can neutralise it globally. -->
    <Transition
      mode="out-in"
      enter-active-class="transition duration-150 ease-out"
      enter-from-class="scale-75 opacity-0"
    >
      <AppIcon
        :key="copied ? 'done' : 'copy'"
        :name="copied ? 'check' : 'copy'"
        :class="[iconSize, copied && 'text-[var(--success)]']"
      />
    </Transition>
    <span v-if="label">{{ copied ? copiedLabel : label }}</span>
  </BaseButton>
</template>
