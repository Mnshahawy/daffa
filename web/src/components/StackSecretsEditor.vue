<script setup lang="ts">
import { ref, watch } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import { daffa, type StackSecretItem } from '@/lib/api'
import AppIcon from './ui/AppIcon.vue'
import BaseButton from './ui/BaseButton.vue'
import SecretField from './SecretField.vue'

// canReveal gates the unmask control on a secret's content (the server enforces it too).
const props = defineProps<{ stackId: string; canWrite: boolean; canReveal: boolean }>()
const emit = defineEmits<{ save: [StackSecretItem[]] }>()

const { data } = useQuery({
  queryKey: ['stacksecrets', () => props.stackId],
  queryFn: () => daffa.stackSecrets(props.stackId),
})

// `existing` tracks which rows came back from the server with a name and no content — its content is
// masked, revealed on demand, and editable in place (blank on an existing row means "keep it").
const secrets = ref<{ name: string; content: string; existing: boolean }[]>([])
const saved = ref(false)

watch(
  data,
  (d) => {
    secrets.value = (d ?? []).map((s) => ({ name: s.name, content: '', existing: true }))
  },
  { immediate: true },
)

function add() {
  secrets.value.push({ name: '', content: '', existing: false })
}

function remove(i: number) {
  secrets.value.splice(i, 1)
}

function reveal(name: string): Promise<string> {
  return daffa.revealStackSecret(props.stackId, name).then((r) => r.value)
}

async function save() {
  emit(
    'save',
    secrets.value
      .filter((s) => s.name.trim())
      .map((s) => ({ name: s.name.trim(), content: s.content })),
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
      <span class="text-sm font-medium">Secrets</span>
      <span v-if="saved" class="text-xs" :style="{ color: 'var(--success)' }">Saved</span>
    </div>

    <div class="p-4">
      <!-- File secrets are a Swarm-only feature: docker stack deploy reads the file and turns it
           into a raft secret. Say it once, with the exact snippet to paste — a secrets block is
           easy to get subtly wrong. (Compose stacks never see this tab; they use secret env vars.) -->
      <div
        class="mb-4 rounded-[var(--radius-control)] border px-3 py-2.5 text-xs"
        :style="{ borderColor: 'var(--border)', background: 'var(--surface-sunken)' }"
      >
        <p class="muted">
          Each secret is a file Daffa keeps beside the stack. On deploy it becomes a Swarm raft
          secret, delivered to whatever node runs the task. Declare it in your compose file:
        </p>
        <pre
          class="mt-1.5 overflow-x-auto font-mono text-[11px]"
          :style="{ color: 'var(--text)' }"
        ><code>secrets:
  &lt;name&gt;:
    file: ./daffa-secrets/&lt;name&gt;</code></pre>
        <p class="muted mt-1.5">
          Reference it in a service with <code class="font-mono">secrets: [&lt;name&gt;]</code>; it
          mounts at <code class="font-mono">/run/secrets/&lt;name&gt;</code>. Its content is immutable
          once deployed — rotating it means giving it a new name.
        </p>
      </div>

      <p v-if="!secrets.length" class="muted mb-3 text-sm">
        None. Add one for a TLS key, a service-account JSON, or anything else a container should read
        from a file rather than an environment variable.
      </p>

      <div v-else class="mb-1.5 hidden items-start gap-2 sm:flex">
        <span class="eyebrow w-56 shrink-0">Name</span>
        <span class="eyebrow min-w-0 flex-1">Content</span>
        <span v-if="canWrite" class="w-[1.625rem] shrink-0" />
      </div>

      <div
        v-for="(s, i) in secrets"
        :key="i"
        class="mb-3 flex flex-wrap items-start gap-2 sm:mb-2 sm:flex-nowrap"
      >
        <div class="w-full shrink-0 sm:w-56">
          <label :for="`secret-name-${i}`" class="sr-only">Secret name</label>
          <input
            :id="`secret-name-${i}`"
            v-model="s.name"
            :disabled="!canWrite"
            placeholder="name"
            class="field py-1.5 font-mono text-xs"
            data-cursor="text"
          />
        </div>

        <div class="min-w-0 flex-1">
          <label :for="`secret-content-${i}`" class="sr-only">Secret content</label>
          <!-- One editor: an existing secret's content is masked and revealed on demand (its bytes
               were never sent), then editable in place; a new one is authored in the clear. These
               are FILES — a PEM key or a JSON blob — so it is a textarea with the eye at the corner. -->
          <SecretField
            v-model="s.content"
            :input-id="`secret-content-${i}`"
            :existing="s.existing"
            :can-write="canWrite"
            :can-reveal="canReveal"
            :reveal="() => reveal(s.name)"
            multiline
          />
        </div>

        <!-- Nothing is destroyed until Save, so this is a row affordance and not a red button. -->
        <BaseButton
          v-if="canWrite"
          intent="ghost"
          size="xs"
          icon
          :label="`Remove ${s.name || 'this secret'}`"
          @click="remove(i)"
        >
          <AppIcon name="x" class="size-3.5" />
        </BaseButton>
      </div>

      <div v-if="canWrite" class="mt-3 flex flex-wrap items-center gap-2">
        <BaseButton @click="add">
          <AppIcon name="plus" class="size-3.5" />
          Add secret
        </BaseButton>
        <BaseButton intent="primary" @click="save">Save</BaseButton>
        <span class="muted text-xs">Changes apply on the next deploy.</span>
      </div>
    </div>
  </div>
</template>
