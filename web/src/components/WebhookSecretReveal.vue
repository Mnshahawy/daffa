<script setup lang="ts">
/**
 * The one moment a webhook secret is visible: right after it is minted (create, rotate, or
 * "create one for this stack"). It is sealed afterwards and cannot be read back, so this
 * says so, next to the URL it belongs with — the two things the git server needs.
 */
import { CopyButton } from '@mnshahawy/daffa-console-ui'

defineProps<{ path: string; secret: string }>()

const url = (path: string) => `${location.origin}${path}`
</script>

<template>
  <div
    class="rounded-[var(--radius-control)] border p-3"
    :style="{
      background: 'var(--accent-soft)',
      borderColor: 'color-mix(in oklch, var(--accent) 40%, transparent)',
    }"
  >
    <p class="mb-1.5 text-xs font-medium">Payload URL</p>
    <div
      class="mb-3 flex items-start gap-2 rounded-[var(--radius-control)] p-2 font-mono text-xs"
      :style="{ background: 'var(--surface)' }"
    >
      <code class="flex-1 break-all">{{ url(path) }}</code>
      <CopyButton intent="ghost" size="xs" :text="url(path)" />
    </div>
    <p class="mb-1.5 text-xs font-medium">Secret — copy it now</p>
    <div
      class="flex items-start gap-2 rounded-[var(--radius-control)] p-2 font-mono text-xs"
      :style="{ background: 'var(--surface)' }"
    >
      <code class="flex-1 break-all">{{ secret }}</code>
      <CopyButton intent="ghost" size="xs" :text="secret" />
    </div>
    <p class="muted mt-1.5 text-xs leading-relaxed">
      Add both to your repository's webhook settings — content type
      <code class="font-mono">application/json</code>, the push event. The secret is shown once:
      it is stored encrypted and cannot be read back. Lose it, and rotate it.
    </p>
  </div>
</template>
