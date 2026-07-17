<script setup lang="ts">
import { ref, watch } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import { daffa, type EnvVarItem } from '@/lib/api'
import AppIcon from './ui/AppIcon.vue'
import BaseButton from './ui/BaseButton.vue'

const props = defineProps<{ stackId: string; canWrite: boolean }>()
const emit = defineEmits<{ save: [EnvVarItem[]] }>()

const { data } = useQuery({
  queryKey: ['stackenv', () => props.stackId],
  queryFn: () => daffa.stackEnv(props.stackId),
})

const vars = ref<EnvVarItem[]>([])
const saved = ref(false)

watch(
  data,
  (d) => {
    vars.value = (d ?? []).map((v) => ({ ...v }))
  },
  { immediate: true },
)

function add() {
  vars.value.push({ key: '', value: '', is_secret: false })
}

function remove(i: number) {
  vars.value.splice(i, 1)
}

async function save() {
  emit(
    'save',
    vars.value.filter((v) => v.key.trim()),
  )
  saved.value = true
  setTimeout(() => (saved.value = false), 2000)
}
</script>

<template>
  <div class="surface overflow-hidden rounded-[var(--radius-card)]">
    <div
      class="flex items-center justify-between border-b px-4 py-2"
      :style="{ borderColor: 'var(--border)' }"
    >
      <span class="text-sm font-medium">Environment variables</span>
      <span v-if="saved" class="text-xs" :style="{ color: 'var(--success)' }">Saved</span>
    </div>

    <div class="p-4">
      <p v-if="!vars.length" class="muted mb-3 text-sm">
        None. These are written into the stack's <code class="font-mono">.env</code> and
        substituted into the compose file.
      </p>

      <!-- The columns say what they are. Two unlabelled boxes and a tickbox is a form you have to
           guess at, and KEY/value is exactly the pair people transpose. -->
      <div v-else class="mb-1.5 flex items-center gap-2">
        <span class="eyebrow w-56 shrink-0">Key</span>
        <span class="eyebrow min-w-0 flex-1">Value</span>
        <span class="eyebrow w-20 shrink-0">Secret</span>
        <span v-if="canWrite" class="w-[1.625rem] shrink-0" />
      </div>

      <div v-for="(v, i) in vars" :key="i" class="mb-2 flex items-center gap-2">
        <div class="w-56 shrink-0">
          <label :for="`env-key-${i}`" class="sr-only">Variable name</label>
          <input
            :id="`env-key-${i}`"
            v-model="v.key"
            :disabled="!canWrite"
            placeholder="KEY"
            class="field py-1.5 font-mono text-xs"
            data-cursor="text"
          />
        </div>

        <div class="min-w-0 flex-1">
          <label :for="`env-value-${i}`" class="sr-only">Value</label>
          <input
            :id="`env-value-${i}`"
            v-model="v.value"
            :disabled="!canWrite"
            :type="v.is_secret ? 'password' : 'text'"
            :placeholder="v.is_secret && !v.value ? '•••••• (unchanged)' : 'value'"
            class="field py-1.5 font-mono text-xs"
            data-cursor="text"
          />
        </div>

        <label
          :for="`env-secret-${i}`"
          class="muted flex w-20 shrink-0 items-center gap-1.5 text-xs"
          :title="'A secret is never shown again once saved'"
        >
          <input
            :id="`env-secret-${i}`"
            v-model="v.is_secret"
            :disabled="!canWrite"
            type="checkbox"
            class="accent-[var(--accent)]"
          />
          secret
        </label>

        <!-- Nothing is destroyed until Save, so this is a row affordance and not a red button. -->
        <BaseButton
          v-if="canWrite"
          intent="ghost"
          size="xs"
          icon
          :label="`Remove ${v.key || 'this variable'}`"
          @click="remove(i)"
        >
          <AppIcon name="x" class="size-3.5" />
        </BaseButton>
      </div>

      <div v-if="canWrite" class="mt-3 flex items-center gap-2">
        <BaseButton @click="add">
          <AppIcon name="plus" class="size-3.5" />
          Add variable
        </BaseButton>
        <BaseButton intent="primary" @click="save">Save</BaseButton>
        <span class="muted text-xs">Changes apply on the next deploy.</span>
      </div>
    </div>
  </div>
</template>
