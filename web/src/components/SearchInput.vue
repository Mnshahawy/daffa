<script setup lang="ts">
import AppIcon from './ui/AppIcon.vue'

// One search box for every list. Containers had one and the other lists did not, which is
// exactly the kind of inconsistency that makes a tool feel unfinished — the moment a host has
// 180 images, the list without a filter is the one you resent.
const model = defineModel<string>({ required: true })

defineProps<{ placeholder?: string }>()
</script>

<template>
  <div class="relative">
    <AppIcon
      name="search"
      class="subtle pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2"
    />

    <input
      v-model="model"
      type="search"
      :placeholder="placeholder ?? 'Filter…'"
      class="field py-1.5 pl-8 pr-8"
      data-cursor="text"
    />

    <!-- A visible way out. Some browsers draw their own clear affordance on type=search and
         some do not, so provide one rather than depend on it. -->
    <button
      v-if="model"
      class="subtle absolute right-2 top-1/2 -translate-y-1/2 rounded p-0.5 transition hover:text-[var(--text)]"
      aria-label="Clear filter"
      title="Clear"
      @click="model = ''"
    >
      <AppIcon name="x" class="size-3" />
    </button>
  </div>
</template>
