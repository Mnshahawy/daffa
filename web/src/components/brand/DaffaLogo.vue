<script setup lang="ts">
import DaffaMark from './DaffaMark.vue'

// Mark plus wordmark. The wordmark is live text, not a path: the font is already loaded, so
// it stays crisp at any zoom, it is selectable, and a screen reader reads it as the word it
// is. Portainer ships two logo files to handle light and dark; this needs none.
withDefaults(defineProps<{ size?: 'sm' | 'md' | 'lg' }>(), { size: 'md' })

const mark = { sm: 'size-6', md: 'size-7', lg: 'size-10' }
const word = { sm: 'text-base', md: 'text-lg', lg: 'text-2xl' }
</script>

<template>
  <span class="flex items-center gap-2.5">
    <DaffaMark :class="mark[size]" />
    <!-- Tight tracking: the wordmark is a lockup, not a sentence. -->
    <span class="font-semibold tracking-[-0.02em]" :class="word[size]">Daffa</span>
  </span>
</template>
