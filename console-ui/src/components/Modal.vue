<script setup lang="ts">
import AppIcon from './AppIcon.vue'

/**
 * A themed dialog for forms. The whole thing is a flex column capped at the viewport:
 * the header and footer stay put while the BODY scrolls, so a long form (the role
 * matrix) can never push the title or the Save button off-screen. Confirmations go
 * through ConfirmHost; this is for the flows that carry inputs.
 *
 * The footer slot lives outside the scroll area, so its buttons use the `form=` attribute
 * to submit the form in the body — which is why each caller gives its <form> an id.
 */
withDefaults(defineProps<{ title: string; description?: string; size?: 'lg' | 'xl' | '2xl' }>(), {
  size: 'lg',
})
const emit = defineEmits<{ close: [] }>()

const maxW = { lg: 'max-w-lg', xl: 'max-w-2xl', '2xl': 'max-w-3xl' }

function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape') emit('close')
}
</script>

<template>
  <Teleport to="body">
    <Transition
      enter-active-class="transition duration-150 ease-out"
      enter-from-class="opacity-0"
      leave-active-class="transition duration-100 ease-in"
      leave-to-class="opacity-0"
      appear
    >
      <div
        class="fixed inset-0 z-40 grid place-items-center p-4 sm:p-6"
        style="background: color-mix(in oklch, black 45%, transparent)"
        role="dialog"
        aria-modal="true"
        :aria-label="title"
        tabindex="-1"
        @keydown="onKey"
        @click.self="emit('close')"
      >
        <div
          class="flex max-h-[calc(100dvh-3rem)] w-full flex-col rounded-xl shadow-[var(--shadow-overlay)]"
          :class="maxW[size]"
          style="background: var(--surface-overlay); border: 1px solid var(--border)"
        >
          <header class="flex shrink-0 items-start justify-between gap-4 px-5 pb-4 pt-5">
            <div class="min-w-0">
              <h2 class="text-[15px] font-semibold">{{ title }}</h2>
              <p v-if="description" class="muted mt-0.5 text-sm leading-relaxed">{{ description }}</p>
            </div>
            <button
              class="subtle -mr-1 -mt-1 shrink-0 rounded p-1 transition hover:bg-[var(--surface-sunken)] hover:text-[var(--text)]"
              aria-label="Close"
              @click="emit('close')"
            >
              <AppIcon name="x" class="size-4" />
            </button>
          </header>

          <div class="min-h-0 flex-1 overflow-y-auto px-5 pb-1">
            <slot />
          </div>

          <footer
            v-if="$slots.footer"
            class="flex shrink-0 items-center justify-end gap-2 border-t px-5 py-4"
            :style="{ borderColor: 'var(--border)' }"
          >
            <slot name="footer" />
          </footer>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>
