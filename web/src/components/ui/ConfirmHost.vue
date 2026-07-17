<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { checked, request, resolve, typed } from '@/lib/confirm'
import BaseButton from './BaseButton.vue'

// Mounted once, in App.vue. Every confirm in the product comes through here.
const confirmBtn = ref<HTMLElement>()
const typeInput = ref<HTMLInputElement>()

const intent = computed(() => {
  const i = request.value?.intent ?? 'primary'
  return i === 'danger' ? 'danger-solid' : i === 'caution' ? 'caution' : 'primary'
})

// Focus goes to the way OUT, not the way through. If the dialog is asking whether to destroy
// something, the button under the finger should not be the one that destroys it.
watch(request, async (r) => {
  if (!r) return
  await nextTick()
  if (r.typeToConfirm) typeInput.value?.focus()
})

const blocked = computed(() => {
  const r = request.value
  if (!r?.typeToConfirm) return false
  return typed.value.trim() !== r.typeToConfirm
})

function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape') resolve(false)
}
</script>

<template>
  <Teleport to="body">
    <Transition
      enter-active-class="transition duration-150 ease-out"
      enter-from-class="opacity-0"
      leave-active-class="transition duration-100 ease-in"
      leave-to-class="opacity-0"
    >
      <div
        v-if="request"
        class="fixed inset-0 z-50 grid place-items-center p-4"
        style="background: color-mix(in oklch, black 45%, transparent)"
        role="dialog"
        aria-modal="true"
        :aria-labelledby="'confirm-title'"
        @keydown="onKey"
        @click.self="resolve(false)"
      >
        <div
          class="w-full max-w-md rounded-xl p-5 shadow-[var(--shadow-overlay)]"
          style="background: var(--surface-overlay); border: 1px solid var(--border)"
        >
          <div class="flex gap-3.5">
            <!-- The icon chip carries the tone, so the severity is legible before the words are. -->
            <div
              class="grid size-9 shrink-0 place-items-center rounded-lg"
              :style="{
                background:
                  request.intent === 'danger'
                    ? 'var(--danger-soft)'
                    : request.intent === 'caution'
                      ? 'var(--warn-soft)'
                      : 'var(--accent-soft)',
                color:
                  request.intent === 'danger'
                    ? 'var(--danger)'
                    : request.intent === 'caution'
                      ? 'var(--warn)'
                      : 'var(--accent)',
              }"
            >
              <svg class="size-4.5" viewBox="0 0 20 20" fill="none" aria-hidden="true">
                <path
                  d="M10 6.5v4M10 13.8h.01"
                  stroke="currentColor"
                  stroke-width="1.8"
                  stroke-linecap="round"
                />
                <path
                  d="M8.6 2.9 1.6 15a1.6 1.6 0 0 0 1.4 2.4h14a1.6 1.6 0 0 0 1.4-2.4l-7-12.1a1.6 1.6 0 0 0-2.8 0Z"
                  stroke="currentColor"
                  stroke-width="1.6"
                  stroke-linejoin="round"
                />
              </svg>
            </div>

            <div class="min-w-0 flex-1">
              <!-- The title is the action. Never "Are you sure?". -->
              <h2 id="confirm-title" class="text-[15px] font-semibold">{{ request.title }}</h2>
              <p v-if="request.body" class="muted mt-1.5 text-sm leading-relaxed">
                {{ request.body }}
              </p>

              <!-- The sub-choice, asked where it is answerable. -->
              <label
                v-if="request.checkbox"
                class="mt-3.5 flex items-start gap-2.5 rounded-lg p-2.5"
                style="background: var(--surface-sunken)"
              >
                <input v-model="checked" type="checkbox" class="mt-0.5 accent-[var(--accent)]" />
                <span class="text-sm">
                  {{ request.checkbox.label }}
                  <span v-if="request.checkbox.hint" class="muted block text-xs">
                    {{ request.checkbox.hint }}
                  </span>
                </span>
              </label>

              <div v-if="request.typeToConfirm" class="mt-3.5">
                <label class="muted mb-1.5 block text-xs">
                  Type
                  <code class="font-mono font-semibold text-[var(--text)]">{{
                    request.typeToConfirm
                  }}</code>
                  to confirm
                </label>
                <input
                  ref="typeInput"
                  v-model="typed"
                  class="field font-mono text-xs"
                  autocomplete="off"
                  spellcheck="false"
                />
              </div>
            </div>
          </div>

          <div class="mt-5 flex justify-end gap-2">
            <BaseButton intent="secondary" size="md" @click="resolve(false)">
              {{ request.cancelLabel ?? 'Cancel' }}
            </BaseButton>
            <BaseButton
              ref="confirmBtn"
              :intent="intent"
              size="md"
              :disabled="blocked"
              @click="resolve(true)"
            >
              {{ request.confirmLabel ?? 'Confirm' }}
            </BaseButton>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>
