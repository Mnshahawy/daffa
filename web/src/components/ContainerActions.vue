<script setup lang="ts">
import { computed, ref } from 'vue'
import type { ContainerAction } from '@/lib/api'
import { confirm } from '@/lib/confirm'
import DropdownMenu from './DropdownMenu.vue'
import AppIcon from './ui/AppIcon.vue'
import BaseButton from './ui/BaseButton.vue'
import type { IconName } from '@/lib/icons'

/**
 * What you can do to a container, coloured by what it costs you.
 *
 * Three tiers, and they are not decoration:
 *
 *   primary   Start, Resume     — brings it back. Safe.
 *   caution   Stop, Restart     — takes it away, and gives it back. Amber.
 *   danger    Remove            — takes it away for good, with its logs. Red.
 *
 * Portainer paints Start and Kill in the same grey, adjacent, in a toolbar of seven identical
 * buttons that are all disabled until you tick a checkbox. Dokploy paints Deploy solid black
 * and Kill Process in 10%-opacity red — so the irreversible action is the *quieter* one on the
 * screen. Both make the same mistake: the pixels do not know what the button does.
 *
 * The two most common actions are inline, because reaching them should not cost a hover. The
 * rest live behind a kebab, which is the row-level menu Portainer never built — there, acting
 * on one container means checking its box and then travelling to a toolbar at the top of the
 * page, for every single container, every time.
 */
const props = defineProps<{ state: string; name?: string; disabled?: boolean; canExec?: boolean }>()
const emit = defineEmits<{ action: [ContainerAction] }>()

const busy = ref<ContainerAction | null>(null)

interface Action {
  id: ContainerAction
  label: string
  icon: IconName
  intent: 'primary' | 'caution' | 'danger' | 'ghost'
  /** Inline on the row. Everything else folds into the kebab. */
  inline?: boolean
}

// Only offer what the container's current state actually permits. A "start" button on a running
// container is a control that lies; we remove it rather than grey it out and annotate it.
const actions = computed<Action[]>(() => {
  switch (props.state) {
    case 'running':
      return [
        { id: 'restart', label: 'Restart', icon: 'restart', intent: 'caution', inline: true },
        { id: 'stop', label: 'Stop', icon: 'stop', intent: 'caution', inline: true },
        { id: 'pause', label: 'Pause', icon: 'pause', intent: 'caution' },
        { id: 'kill', label: 'Kill', icon: 'stop', intent: 'danger' },
      ]
    case 'paused':
      return [{ id: 'unpause', label: 'Resume', icon: 'play', intent: 'primary', inline: true }]
    case 'exited':
    case 'created':
    case 'dead':
      return [
        { id: 'start', label: 'Start', icon: 'play', intent: 'primary', inline: true },
        { id: 'remove', label: 'Remove', icon: 'trash', intent: 'danger' },
      ]
    default:
      return []
  }
})

const inline = computed(() => actions.value.filter((a) => a.inline))
const overflow = computed(() => actions.value.filter((a) => !a.inline))

// The confirmations, and only where they are earned.
//
// Stop and restart get none: they are recoverable, they are the whole job, and a console that
// asks "are you sure?" every time you restart something teaches you to stop reading the
// dialogs — which spends the attention you were saving for the one that matters.
const prompts: Partial<Record<ContainerAction, () => Parameters<typeof confirm>[0]>> = {
  remove: () => ({
    title: `Remove ${props.name ?? 'this container'}?`,
    body: 'The container goes, and its logs and writable layer go with it. Anything not on a volume is lost.',
    confirmLabel: 'Remove',
    intent: 'danger',
    checkbox: {
      label: 'Also remove its anonymous volumes',
      hint: 'Named volumes are never touched — those hold data you meant to keep.',
    },
  }),
  kill: () => ({
    title: `Kill ${props.name ?? 'this container'}?`,
    body: 'SIGKILL, immediately. The process gets no chance to flush or shut down cleanly — prefer Stop unless it is already wedged.',
    confirmLabel: 'Kill',
    intent: 'danger',
  }),
}

async function run(a: Action) {
  const prompt = prompts[a.id]
  if (prompt && !(await confirm(prompt()))) return

  busy.value = a.id
  try {
    emit('action', a.id)
  } finally {
    busy.value = null
  }
}
</script>

<template>
  <div v-if="!disabled && actions.length" class="flex items-center justify-end gap-1">
    <BaseButton
      v-for="a in inline"
      :key="a.id"
      :intent="a.intent"
      size="xs"
      :loading="busy === a.id"
      @click="run(a)"
    >
      <AppIcon v-if="busy !== a.id" :name="a.icon" class="size-3" />
      {{ a.label }}
    </BaseButton>

    <DropdownMenu v-if="overflow.length" align="right">
      <template #trigger>
        <span class="btn btn-ghost btn-xs btn-icon" aria-label="More actions" title="More actions">
          <AppIcon name="more" class="size-3.5" />
        </span>
      </template>

      <button
        v-for="a in overflow"
        :key="a.id"
        class="flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left text-sm transition hover:bg-[var(--surface-sunken)]"
        :style="
          a.intent === 'danger'
            ? { color: 'var(--danger)' }
            : a.intent === 'caution'
              ? { color: 'var(--warn)' }
              : undefined
        "
        @click="run(a)"
      >
        <AppIcon :name="a.icon" class="size-3.5" />
        {{ a.label }}
      </button>
    </DropdownMenu>
  </div>
</template>
