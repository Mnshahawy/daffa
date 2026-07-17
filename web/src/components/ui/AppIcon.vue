<script setup lang="ts">
import { iconPaths, type IconName } from '@/lib/icons'

defineProps<{ name: IconName }>()
</script>

<template>
  <svg
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="1.75"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
  >
    <path v-for="(d, i) in iconPaths[name]" :key="i" :d="d" />
  </svg>
</template>
