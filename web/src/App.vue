<script setup lang="ts">
import { RouterView } from 'vue-router'
import ConfirmHost from '@/components/ui/ConfirmHost.vue'
import ToastHost from '@/components/ui/ToastHost.vue'
</script>

<template>
  <RouterView />

  <!-- Mounted once. Every confirmation in the product renders here — see lib/confirm.ts for
       why there is exactly one of them, and why none of them is titled "Are you sure?". -->
  <ConfirmHost />

  <!-- Mounted once. Every action's outcome — saved, deleted, failed — renders here; see
       lib/toast.ts for why feedback is one channel and why it is transient. -->
  <ToastHost />
</template>
