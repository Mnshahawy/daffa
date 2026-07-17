<script setup lang="ts">
import { RouterView } from 'vue-router'
import ConfirmHost from '@/components/ui/ConfirmHost.vue'
</script>

<template>
  <RouterView />

  <!-- Mounted once. Every confirmation in the product renders here — see lib/confirm.ts for
       why there is exactly one of them, and why none of them is titled "Are you sure?". -->
  <ConfirmHost />
</template>
