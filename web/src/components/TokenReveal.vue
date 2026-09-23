<script setup lang="ts">
import { ref } from 'vue'
import { BaseButton } from '@mnshahawy/daffa-console-ui'
import { CopyButton } from '@mnshahawy/daffa-console-ui'

// The one-time reveal of a freshly minted API token. Modal-ish on purpose: only the hash
// is stored, so closing this without copying leaves a token nobody can ever use. The
// dismiss button stays disabled until the copy button has been pressed.
defineProps<{ name: string; token: string; owner?: string }>()
const emit = defineEmits<{ done: [] }>()

const copied = ref(false)
</script>

<template>
  <div
    class="rounded-[var(--radius-card)] p-5"
    :style="{ background: 'var(--warn-soft)', border: '1px solid color-mix(in oklch, var(--warn) 40%, transparent)' }"
  >
    <p class="mb-1 text-sm font-semibold">
      Copy the token “{{ name }}”<template v-if="owner"> for {{ owner }}</template> — now.
    </p>
    <p class="mb-3 text-sm leading-relaxed">
      This is the only time it will ever be shown. Daffa stores a hash, not the token
      — close this without copying and the token is unusable by anyone, forever. Put
      it straight into your CI's secret storage.
    </p>
    <code class="mb-3 block overflow-x-auto rounded-lg p-3 font-mono text-xs" :style="{ background: 'var(--surface-sunken)' }">{{ token }}</code>
    <div class="flex flex-wrap items-center gap-3">
      <CopyButton intent="primary" size="md" label="Copy token" :text="token" @copied="copied = true" />
      <BaseButton intent="secondary" size="md" :disabled="!copied" :title="copied ? '' : 'Copy it first'" @click="emit('done')">
        I have stored it safely
      </BaseButton>
    </div>
  </div>
</template>
