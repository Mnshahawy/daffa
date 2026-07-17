<script setup lang="ts">
/**
 * The logging-defaults form, shared by the fleet Settings tab and the Host page.
 *
 * The API stores a free-form driver + options (plugin drivers exist and a wrong name
 * fails the deploy with a readable runner log) — but the FORM curates: the drivers Docker
 * ships in a dropdown, and dedicated rotation fields for the two drivers that rotate,
 * because max-size/max-file IS the retention feature and it should not live behind an
 * "advanced" fold.
 */
import { computed, ref, watch } from 'vue'
import type { LogConfig, LogConfigRequest } from '@/lib/api'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'

const props = defineProps<{
  /** The saved config this form edits, or null when unset. */
  modelValue: LogConfig | null
  /** Read-only rendering for holders of view without edit. */
  disabled?: boolean
  busy?: boolean
  /** What the reset action means HERE: "Clear" for the fleet, "Revert to the global default" for a host. */
  clearLabel?: string
  /** Whether there is anything to clear. The Host page seeds the form with the GLOBAL
   * default when no override exists — a "revert" button there would revert nothing. */
  showClear?: boolean
}>()

const emit = defineEmits<{
  save: [body: LogConfigRequest]
  clear: []
}>()

const KNOWN_DRIVERS = ['json-file', 'local', 'journald', 'syslog', 'none'] as const
/** The two drivers whose options rotate — the retention half of this feature. */
const rotates = (d: string) => d === 'json-file' || d === 'local'

const driver = ref('json-file')
const customDriver = ref('')
const maxSize = ref('')
const maxFile = ref('')
const extra = ref<{ key: string; value: string }[]>([])

const isCustom = computed(() => driver.value === 'custom')
const effectiveDriver = computed(() => (isCustom.value ? customDriver.value.trim() : driver.value))

// Re-seed whenever the saved config changes — including the moment the query lands.
watch(
  () => props.modelValue,
  (cfg) => {
    if (!cfg) {
      driver.value = 'json-file'
      customDriver.value = ''
      maxSize.value = ''
      maxFile.value = ''
      extra.value = []
      return
    }
    if ((KNOWN_DRIVERS as readonly string[]).includes(cfg.driver)) {
      driver.value = cfg.driver
      customDriver.value = ''
    } else {
      driver.value = 'custom'
      customDriver.value = cfg.driver
    }
    const opts = { ...cfg.opts }
    // The rotation pair gets its dedicated fields only when the driver honours it;
    // for any other driver the same keys are just options like the rest.
    if (rotates(cfg.driver)) {
      maxSize.value = opts['max-size'] ?? ''
      maxFile.value = opts['max-file'] ?? ''
      delete opts['max-size']
      delete opts['max-file']
    } else {
      maxSize.value = ''
      maxFile.value = ''
    }
    extra.value = Object.entries(opts).map(([key, value]) => ({ key, value }))
  },
  { immediate: true },
)

function request(): LogConfigRequest {
  const opts: Record<string, string> = {}
  if (rotates(effectiveDriver.value)) {
    // max-file is bound to <input type="number">, and Vue's v-model casts a number input to a
    // NUMBER — so maxFile.value can be `3`, not `"3"`, and calling .trim() on it throws a
    // TypeError that aborts the whole submit before the save request is ever sent (the form just
    // silently does nothing). Coerce to string first; the API wants string opts anyway.
    const size = String(maxSize.value).trim()
    const file = String(maxFile.value).trim()
    if (size) opts['max-size'] = size
    if (file) opts['max-file'] = file
  }
  for (const { key, value } of extra.value) {
    const k = String(key).trim()
    if (k) opts[k] = String(value)
  }
  return { driver: effectiveDriver.value, opts }
}

function addRow() {
  extra.value.push({ key: '', value: '' })
}
function dropRow(i: number) {
  extra.value.splice(i, 1)
}
</script>

<template>
  <form class="space-y-4" @submit.prevent="emit('save', request())">
    <div class="grid gap-4 sm:grid-cols-3">
      <div>
        <label for="lc-driver" class="mb-1.5 block text-sm font-medium">Log driver</label>
        <select id="lc-driver" v-model="driver" class="field" :disabled="disabled">
          <option v-for="d in KNOWN_DRIVERS" :key="d" :value="d">{{ d }}</option>
          <option value="custom">custom…</option>
        </select>
      </div>

      <div v-if="isCustom">
        <label for="lc-custom" class="mb-1.5 block text-sm font-medium">Driver name</label>
        <input
          id="lc-custom"
          v-model="customDriver"
          required
          placeholder="loki"
          class="field font-mono text-xs"
          :disabled="disabled"
          data-cursor="text"
        />
        <p class="subtle mt-1.5 text-xs leading-snug">
          A plugin driver must already be installed on the host — a name Docker does not
          know fails the deploy, with the reason in its log.
        </p>
      </div>

      <template v-if="rotates(effectiveDriver)">
        <div>
          <label for="lc-max-size" class="mb-1.5 block text-sm font-medium">
            Rotate at <span class="subtle font-normal">(max-size)</span>
          </label>
          <input
            id="lc-max-size"
            v-model="maxSize"
            placeholder="10m"
            class="field font-mono text-xs"
            :disabled="disabled"
            data-cursor="text"
          />
        </div>
        <div>
          <label for="lc-max-file" class="mb-1.5 block text-sm font-medium">
            Files kept <span class="subtle font-normal">(max-file)</span>
          </label>
          <input
            id="lc-max-file"
            v-model="maxFile"
            type="number"
            min="1"
            placeholder="3"
            class="field font-mono text-xs"
            :disabled="disabled"
          />
        </div>
      </template>
    </div>

    <!-- Unbounded json-file is exactly the disk-filler this feature exists to stop, so an
         empty rotation pair is worth a sentence before the save, not a surprise later. -->
    <p
      v-if="rotates(effectiveDriver) && !maxSize.trim()"
      class="flex items-start gap-2.5 rounded-[var(--radius-control)] px-3 py-2.5 text-sm leading-relaxed"
      :style="{ background: 'var(--warn-soft)' }"
    >
      <AppIcon name="alert" class="mt-0.5 size-4 shrink-0" :style="{ color: 'var(--warn)' }" />
      <span>
        Without <span class="font-mono text-xs">max-size</span>, {{ effectiveDriver }} keeps
        every line a container ever writes — rotation is the retention here, and this leaves
        it off.
      </span>
    </p>

    <div>
      <div class="mb-1.5 flex items-center justify-between">
        <span class="text-sm font-medium">
          Other options <span class="subtle font-normal">(driver-specific)</span>
        </span>
        <BaseButton v-if="!disabled" intent="secondary" size="xs" @click="addRow">
          <AppIcon name="plus" class="size-3.5" />
          Add option
        </BaseButton>
      </div>

      <div v-if="extra.length" class="space-y-1.5">
        <div v-for="(row, i) in extra" :key="i" class="flex items-center gap-1.5">
          <input
            v-model="row.key"
            :aria-label="`Option ${i + 1} name`"
            placeholder="labels"
            class="field w-48 font-mono text-xs"
            :disabled="disabled"
            data-cursor="text"
          />
          <input
            v-model="row.value"
            :aria-label="`Option ${i + 1} value`"
            placeholder="value"
            class="field min-w-0 flex-1 font-mono text-xs"
            :disabled="disabled"
            data-cursor="text"
          />
          <BaseButton
            v-if="!disabled"
            intent="secondary"
            size="xs"
            :aria-label="`Remove option ${i + 1}`"
            @click="dropRow(i)"
          >
            <AppIcon name="trash" class="size-3.5" />
          </BaseButton>
        </div>
      </div>
      <p v-else class="subtle text-xs">None — the driver's own defaults apply.</p>
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
