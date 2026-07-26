<script setup lang="ts">
import { ref } from 'vue'
import AppIcon from './AppIcon.vue'

/**
 * A secret ENTRY field: masked by default, with a reveal toggle so someone typing a long
 * owner token can check it before they commit. Meant for write-only secrets — the app
 * seals the value server-side and never sends it back — so this only ever holds what was typed,
 * never a value read from the server.
 */
defineProps<{
  id?: string
  placeholder?: string
  autocomplete?: string
  required?: boolean
}>()

const model = defineModel<string>({ required: true })
const revealed = ref(false)
</script>

<template>
  <div class="relative">
    <input
      :id="id"
      v-model="model"
      :type="revealed ? 'text' : 'password'"
      :placeholder="placeholder"
      :autocomplete="autocomplete ?? 'off'"
      :required="required"
      class="field pr-10 font-mono text-xs"
      spellcheck="false"
    />
    <button
      type="button"
      class="subtle absolute inset-y-0 right-0 grid w-9 place-items-center rounded-r-[var(--radius-control)] transition hover:text-[var(--text)]"
      :aria-label="revealed ? 'Hide' : 'Reveal'"
      @click="revealed = !revealed"
    >
      <AppIcon :name="revealed ? 'eyeOff' : 'eye'" class="size-4" />
    </button>
  </div>
</template>
