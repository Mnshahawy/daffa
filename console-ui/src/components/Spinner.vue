<script setup lang="ts">
/**
 * The one loading indicator. Until now it existed only inside BaseButton, so every
 * page-level loading state hand-rolled its own — or worse, showed nothing and let a slow
 * fetch look like an empty result. Inherit color from the parent (`currentColor`), size
 * with the same scale buttons use.
 */
withDefaults(defineProps<{ size?: 'xs' | 'sm' | 'md' }>(), { size: 'sm' })

const px = { xs: '0.75rem', sm: '1rem', md: '1.5rem' }
</script>

<template>
  <span
    class="inline-block animate-spin rounded-full border-2 border-current border-t-transparent align-middle"
    :style="{ width: px[size], height: px[size] }"
    role="status"
    aria-label="Loading"
  />
</template>
