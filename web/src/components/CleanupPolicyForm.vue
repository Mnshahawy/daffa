<script setup lang="ts">
/**
 * The automatic-cleanup form, shared by the fleet Settings tab and the Host page.
 *
 * Two things carry the weight here and neither is an "advanced" option: WHAT gets swept,
 * and how old it has to be first. The age floor is the whole difference between this and
 * `docker system prune -a` — the sweep takes the release from three weeks ago and leaves
 * the one currently running — so it gets a labelled field and a sentence, not a default
 * nobody reads.
 */
import { computed, ref, watch } from 'vue'
import type { CleanupPolicy, CleanupPolicyRequest } from '@/lib/api'
import { AppIcon } from '@mnshahawy/daffa-console-ui'
import { BaseButton } from '@mnshahawy/daffa-console-ui'

const props = defineProps<{
  /** The saved policy this form edits, or null when unset. */
  modelValue: CleanupPolicy | null
  /** Read-only rendering for a holder who can see the page but not press prune. */
  disabled?: boolean
  busy?: boolean
  /** What the reset action means HERE: "Clear" for the fleet, "Revert…" for a host. */
  clearLabel?: string
  /** Whether there is anything to clear — the Host page seeds this form with the FLEET
   * policy when no override exists, and a "revert" there would revert nothing. */
  showClear?: boolean
}>()

const emit = defineEmits<{
  save: [body: CleanupPolicyRequest]
  clear: []
}>()

/**
 * Volumes are deliberately not on this list, and the server refuses them too. A pruned
 * image is a re-pull; a pruned volume is deleted data, and "anonymous" versus "the
 * database of a stack that happens to be stopped" is one label nobody checks at 03:30.
 */
const TARGETS = [
  {
    id: 'containers',
    label: 'Stopped containers',
    hint: 'Old deployments — swarm keeps the last few task containers per service, writable layers and all. Swept first, so the images they pin can go in the same pass.',
  },
  {
    id: 'images',
    label: 'Unused images',
    hint: 'Every image no container references, not just the untagged ones. The superseded image of each release stays tagged forever otherwise — usually the single biggest reclaim.',
  },
  { id: 'networks', label: 'Unused networks', hint: 'Cheap, and mostly tidiness.' },
  {
    id: 'build-cache',
    label: 'Build cache',
    hint: "BuildKit's layer cache, for stacks that build from a Dockerfile. Grows without limit.",
  },
] as const

const enabled = ref(false)
const schedule = ref('30 3 * * *')
const keepHours = ref<number | string>(168)
const targets = ref<string[]>(['containers', 'images', 'build-cache'])

const keepNumber = computed(() => Number(keepHours.value) || 0)
const keepDays = computed(() => Math.round((keepNumber.value / 24) * 10) / 10)

// Re-seed whenever the saved policy changes — including the moment the query lands.
watch(
  () => props.modelValue,
  (p) => {
    if (!p) return // keep the defaults above: a sane starting policy, not an empty form
    enabled.value = p.enabled
    schedule.value = p.schedule || '30 3 * * *'
    keepHours.value = p.keep_hours
    targets.value = [...p.targets]
  },
  { immediate: true },
)

function toggle(id: string) {
  targets.value = targets.value.includes(id)
    ? targets.value.filter((t) => t !== id)
    : [...targets.value, id]
}

function request(): CleanupPolicyRequest {
  return {
    enabled: enabled.value,
    schedule: String(schedule.value).trim(),
    // Bound to <input type="number">, so Vue hands back a number — but an emptied field
    // gives '' , and the API wants a number either way.
    keep_hours: keepNumber.value,
    targets: targets.value,
  }
}
</script>

<template>
  <form class="space-y-4" @submit.prevent="emit('save', request())">
    <label class="flex items-start gap-2.5 text-sm" for="cp-enabled">
      <input
        id="cp-enabled"
        v-model="enabled"
        type="checkbox"
        class="mt-0.5 accent-[var(--color-accent-500)]"
        :disabled="disabled"
      />
      <span>
        <span class="font-medium">Sweep this host on a schedule</span>
        <span class="subtle block text-xs leading-snug">
          Off means nothing is ever deleted automatically — the buttons on the disk card
          still work.
        </span>
      </span>
    </label>

    <div class="grid gap-4 sm:grid-cols-2">
      <div>
        <label for="cp-schedule" class="mb-1.5 block text-sm font-medium">Schedule</label>
        <input
          id="cp-schedule"
          v-model="schedule"
          placeholder="30 3 * * *"
          class="field font-mono text-xs"
          :disabled="disabled"
          data-cursor="text"
        />
        <p class="subtle mt-1 text-xs">Cron, in UTC. Pick an hour nothing deploys in.</p>
      </div>

      <div>
        <label for="cp-keep" class="mb-1.5 block text-sm font-medium">
          Keep anything newer than <span class="subtle font-normal">(hours)</span>
        </label>
        <input
          id="cp-keep"
          v-model="keepHours"
          type="number"
          min="0"
          placeholder="168"
          class="field font-mono text-xs"
          :disabled="disabled"
        />
        <p class="subtle mt-1 text-xs">
          <template v-if="keepNumber > 0">
            {{ keepDays }} days. Nothing created inside that window is touched, so the last
            few releases stay rollback-able.
          </template>
          <template v-else>No age floor.</template>
        </p>
      </div>
    </div>

    <!-- Zero here is `docker system prune -a`: it takes the image of the release that went
         out this morning the moment nothing is running it. Worth a sentence before saving. -->
    <p
      v-if="enabled && keepNumber <= 0"
      class="flex items-start gap-2.5 rounded-[var(--radius-control)] px-3 py-2.5 text-sm leading-relaxed"
      :style="{ background: 'var(--warn-soft)' }"
    >
      <AppIcon name="alert" class="mt-0.5 size-4 shrink-0" :style="{ color: 'var(--warn)' }" />
      <span>
        With no age floor this is <span class="font-mono text-xs">docker system prune -a</span>:
        the image a stopped stack needs to start again is gone the same night, and rolling
        back means pulling it from the registry.
      </span>
    </p>

    <div>
      <div class="mb-2 text-sm font-medium">What to prune</div>
      <div class="space-y-2">
        <label
          v-for="t in TARGETS"
          :key="t.id"
          class="flex items-start gap-2.5 text-sm"
          :for="'cp-' + t.id"
        >
          <input
            :id="'cp-' + t.id"
            type="checkbox"
            class="mt-0.5 accent-[var(--color-accent-500)]"
            :checked="targets.includes(t.id)"
            :disabled="disabled"
            @change="toggle(t.id)"
          />
          <span>
            <span class="font-medium">{{ t.label }}</span>
            <span class="subtle block text-xs leading-snug">{{ t.hint }}</span>
          </span>
        </label>
      </div>
      <p class="subtle mt-2 text-xs leading-relaxed">
        Volumes are never swept automatically, by design — a pruned volume is deleted data,
        not a re-pull. Remove one from the volumes page when you mean it.
      </p>
    </div>

    <div v-if="!disabled" class="flex items-center gap-2">
      <BaseButton type="submit" intent="primary" size="md" :loading="busy">Save</BaseButton>
      <BaseButton
        v-if="showClear ?? !!modelValue"
        intent="secondary"
        size="md"
        :disabled="busy"
        @click="emit('clear')"
      >
        {{ clearLabel ?? 'Clear' }}
      </BaseButton>
    </div>
  </form>
</template>
