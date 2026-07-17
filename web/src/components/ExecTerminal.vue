<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, useTemplateRef, watch } from 'vue'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { openExec } from '@/lib/stream'
import { resolved } from '@/lib/theme'
import type { Status } from '@/lib/status'
import StatusPill from './ui/StatusPill.vue'

// node: which machine the container is on.
//
// THIS IS CROSS-NODE EXEC. A manager cannot exec into a container on another machine — that is
// Docker, not Daffa, and no tool escapes it. But the server holds a tunnel per node, so naming the
// node routes the shell straight to the daemon that can serve it. Portainer needed a gossiping
// agent mesh and an X-PortainerAgent-Target header to do the same thing.
const props = defineProps<{ env: string; container: string; node?: string }>()

const host = useTemplateRef<HTMLDivElement>('host')
const status = ref<'connecting' | 'open' | 'closed' | 'error'>('connecting')
const error = ref('')

// The socket's state, in the app's one status vocabulary. Connecting pulses because it is
// genuinely in flight; a closed session is neutral, not red — the shell ending is what
// happens when you type `exit`.
const pill = computed<Status>(() => {
  switch (status.value) {
    case 'connecting':
      return { tone: 'accent', label: 'Connecting', live: true }
    case 'open':
      return { tone: 'success', label: 'Connected' }
    case 'error':
      return { tone: 'danger', label: 'Failed' }
    default:
      return { tone: 'neutral', label: 'Session ended' }
  }
})

let term: Terminal | undefined
let ws: WebSocket | undefined
let observer: ResizeObserver | undefined

const encoder = new TextEncoder()
const decoder = new TextDecoder()

// The terminal is a canvas, not a DOM tree, so it cannot inherit the CSS variables — its
// colours have to be read out and handed over. Which also means they do not update on
// their own when the theme changes; see the watch below.
//
// Only opaque tokens go in here. xterm parses a colour it does not recognise by painting it
// to a 1x1 canvas and reading the pixel back, and it THROWS if that pixel is not fully
// opaque — so a color-mix() token like --accent-soft would take the terminal down with it.
function surfaceTheme() {
  const style = getComputedStyle(document.documentElement)
  const token = (name: string, fallback: string) =>
    style.getPropertyValue(name).trim() || fallback

  return {
    background: token('--surface-sunken', '#0f1115'),
    foreground: token('--text', '#e6e6e6'),
    // The cursor is the one piece of brand in the terminal, exactly as it is everywhere else.
    cursor: token('--accent', '#7c5cff'),
    cursorAccent: token('--surface-sunken', '#0f1115'),
    selectionBackground: token('--border-strong', '#3a3f4b'),
  }
}

onMounted(() => {
  term = new Terminal({
    fontFamily: 'ui-monospace, "SF Mono", "JetBrains Mono", monospace',
    fontSize: 13,
    cursorBlink: true,
    // Match the app's surfaces so the terminal reads as part of the page, not as an
    // applet dropped into it.
    theme: surfaceTheme(),
    scrollback: 5000,
  })

  // Switching to light while a shell is open should not leave a black hole in the page.
  watch(resolved, () => {
    if (term) term.options.theme = surfaceTheme()
  })

  const fit = new FitAddon()
  term.loadAddon(fit)
  term.open(host.value!)
  fit.fit()

  ws = openExec(props.env, props.container, term.rows, term.cols, props.node)

  ws.onopen = () => {
    status.value = 'open'
    term!.focus()
  }

  // Keystrokes go out as raw bytes. Nothing interprets them on the way — the shell on
  // the far end is the only thing that should.
  term.onData((data) => {
    if (ws?.readyState === WebSocket.OPEN) ws.send(encoder.encode(data))
  })

  ws.onmessage = (e) => {
    if (e.data instanceof ArrayBuffer) {
      term!.write(decoder.decode(new Uint8Array(e.data)))
    }
  }

  ws.onerror = () => {
    status.value = 'error'
    error.value = 'The connection failed.'
  }

  ws.onclose = (e) => {
    if (status.value !== 'error') status.value = 'closed'
    // 1000 is a clean exit (the shell ended). Anything else is worth naming: an HTTP
    // error during the upgrade (403 for a viewer, 400 for a shell-less image) closes
    // the socket before it ever opens, and without this the terminal would just sit
    // there blank and silent.
    if (e.code !== 1000 && !error.value) {
      error.value = e.reason || 'The shell could not be opened. Distroless images have no shell, and viewers may not exec.'
    }
    term?.write('\r\n\x1b[90m— session ended —\x1b[0m\r\n')
  }

  // Keep the far-end TTY the same shape as the div; otherwise vim and top draw
  // themselves at 80x24 inside a much larger box.
  observer = new ResizeObserver(() => {
    try {
      fit.fit()
      if (ws?.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'resize', rows: term!.rows, cols: term!.cols }))
      }
    } catch {
      // fit() throws if the element is hidden (a collapsed tab); nothing to do.
    }
  })
  observer.observe(host.value!)
})

onBeforeUnmount(() => {
  observer?.disconnect()
  ws?.close(1000, 'closed by user')
  term?.dispose()
})
</script>

<template>
  <div class="surface overflow-hidden rounded-[var(--radius-card)]">
    <div
      class="flex items-center justify-between border-b px-4 py-2"
      :style="{ borderColor: 'var(--border)' }"
    >
      <span class="text-sm font-medium">Shell</span>
      <StatusPill :status="pill" />
    </div>

    <p
      v-if="error"
      class="border-b px-4 py-2 text-xs"
      :style="{
        borderColor: 'var(--border)',
        background: 'var(--danger-soft)',
        color: 'var(--danger)',
      }"
    >
      {{ error }}
    </p>

    <div
      ref="host"
      class="h-[60vh] p-2"
      :style="{ background: 'var(--surface-sunken)' }"
    />
  </div>
</template>
