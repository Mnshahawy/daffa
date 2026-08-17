<script setup lang="ts">
import { computed, nextTick, ref, useId, watch } from 'vue'
import AppIcon from './AppIcon.vue'
import BaseButton from './BaseButton.vue'
import Modal from './Modal.vue'
import type { IconName } from '../lib/icons'

/**
 * A step's own primary button, replacing Next.
 *
 * Use it when the step *does* something rather than just collecting fields — mints a key,
 * tests a connection, exchanges a token. It does NOT advance the wizard: work like that
 * decides for itself where the reader lands (a rejected credential stays put, a generated
 * secret has its own screen), so the caller drives `step` and this only says which button
 * to draw. `run` still fires behind the browser's own validation, like Next does.
 */
export interface WizardAction {
  label: string
  icon?: IconName
  /** Spinner + disabled, for a button whose work is in flight. */
  busy?: boolean
  /** Extra gate on top of the step's `complete`. */
  disabled?: boolean
  run: () => void | Promise<void>
}

export interface WizardStep {
  /** Stable key. Also the name of the slot this step's fields come from. */
  id: string
  /** Short label for the rail — two or three words, not a sentence. */
  title: string
  /** One line above the step's fields. Optional. */
  description?: string
  /**
   * Gate on leaving this step, for what the browser cannot check itself: a choice that has
   * no valid option yet, a pair of fields that only makes sense together. Plain `required`
   * on an input needs nothing here — Next submits the form, so native validation already
   * runs and points at the offending field. Default true.
   */
  complete?: boolean
  /**
   * Drop the step entirely. For a step only some paths need — git credentials on a stack
   * whose compose file is typed inline. The numbering, the rail and the counter all close
   * up around it, so the reader never sees a step they cannot reach.
   */
  skip?: boolean
  /** This step's own primary button instead of Next. See WizardAction. */
  action?: WizardAction
  /**
   * Drop Back on this step. For a point of no return — a key already minted, a record
   * already written — where going back would offer to redo something that cannot be redone.
   */
  hideBack?: boolean
}

/**
 * A modal that splits one long form into ordered steps.
 *
 * Reach for it when a form's fields no longer fit a screen and *have an order* — what to
 * back up, then where it goes, then who can decrypt it. Height is the tell: a form that
 * scrolls has hidden half of itself, and widening it past two columns only trades a tall
 * wall for a wide one. What it is not is a way to hide an unordered form; a flat set of
 * fields belongs in a plain `Modal`, where the reader can see the whole thing at once.
 *
 * The rail on the left is the map — it says how many steps there are *before* the reader
 * commits to the first one, which is the other half of what makes a long form feel long.
 * Below `sm` it collapses to a one-line counter, because on a phone the rail would cost
 * more room than the fields it indexes.
 *
 * Each step's fields come from a slot named after its id:
 *
 *   <WizardModal title="New backup job" :steps="steps" submit-label="Create job"
 *                @close="adding = false" @submit="create.mutate()">
 *     <template #source>…</template>
 *     <template #destination>…</template>
 *   </WizardModal>
 *
 * There is no `open` prop, and none of the React version's reopen bookkeeping: like every
 * dialog in this kit it is mounted with `v-if`, so opening it IS constructing it and a
 * second visit starts on step one by construction.
 */
const props = withDefaults(
  defineProps<{
    title: string
    description?: string
    steps: WizardStep[]
    /** Final button. Name the thing that happens — "Create job", not "Finish". */
    submitLabel?: string
    /** Spinner on the primary button, and Back goes inert. */
    submitting?: boolean
    /** Extra gate on the final button, on top of the last step's `complete`. */
    canSubmit?: boolean
    /** Pinned under the fields — survives whatever the step body is doing. */
    error?: string
    /**
     * Every step already has an answer, so the rail is open from the first screen rather
     * than earned one Next at a time. For EDITING an existing record: someone who opened
     * this to change one field on the last step should not have to walk through three
     * screens of their own answers to reach it.
     */
    unlocked?: boolean
    size?: 'lg' | 'xl' | '2xl'
  }>(),
  { submitLabel: 'Save', canSubmit: true, size: '2xl' },
)

const emit = defineEmits<{ close: []; submit: [] }>()

/** Index into the VISIBLE steps. Bind it only when the caller drives the flow itself. */
const step = defineModel<number>('step', { default: 0 })

// The footer lives outside the body's scroll area, so its buttons reach the form by id —
// the same contract every other Modal caller uses, minted here so callers cannot collide.
const formId = useId()
const formEl = ref<HTMLFormElement>()

// Skipped steps never enter the visible list, so every index below — the rail, the counter,
// `last` — counts the same thing.
const visible = computed(() => props.steps.filter((s) => !s.skip))

// A step can disappear under the reader (a choice on step 1 removes step 3), so the index
// is clamped on read rather than trusted, and written back so the caller's copy agrees.
const index = computed(() => Math.min(Math.max(step.value, 0), Math.max(visible.value.length - 1, 0)))
watch(index, (i) => {
  if (i !== step.value) step.value = i
})

const current = computed(() => visible.value[index.value])
const last = computed(() => index.value >= visible.value.length - 1)

// How far the reader has got. Without it, stepping back would re-lock every step ahead —
// they would have to click Next through work they had already done just to return to where
// they were, and the ticks on finished steps would vanish even though the answers are still
// in the form.
const furthest = ref(props.unlocked ? props.steps.length : 0)
watch(index, (i) => {
  if (i > furthest.value) furthest.value = i
})

const complete = computed(() => current.value?.complete !== false)
// Every step before this one has to be complete too, or Submit would let the reader past a
// gate that a skip-back-then-forward rail click had opened.
const priorComplete = computed(() =>
  visible.value.slice(0, index.value).every((s) => s.complete !== false),
)

const busy = computed(() => props.submitting || current.value?.action?.busy)

const primaryDisabled = computed(() => {
  if (busy.value || !complete.value) return true
  const action = current.value?.action
  if (action) return !!action.disabled
  return last.value ? !priorComplete.value || !props.canSubmit : false
})

function goTo(next: number) {
  const clamped = Math.min(Math.max(next, 0), visible.value.length - 1)
  if (!visible.value[clamped]) return
  step.value = clamped
}

/**
 * A rail click. Going forward has to clear the same gate Next does — otherwise emptying a
 * required field and then clicking ahead on the rail is a way around the validation, and
 * the reader finds out from the server instead. Going back is always free: nobody is being
 * walked past anything.
 */
function jump(i: number) {
  if (i > index.value && formEl.value && !formEl.value.reportValidity()) return
  goTo(i)
}

/**
 * One handler for all three primary buttons, and it is a form submit on purpose: the
 * browser validates the fields of the step you are leaving and puts the cursor on the bad
 * one. A wizard that walks you forward past an empty required field and only complains at
 * the end has wasted every step in between.
 */
function primary() {
  const action = current.value?.action
  if (action) void action.run()
  else if (last.value) emit('submit')
  else goTo(index.value + 1)
}

// Arriving on a step should put the cursor in it. Otherwise the reader clicks Next and then
// has to find the first field with the mouse — on every step, in a dialog whose whole point
// is that there are several. Skipped when the step leads with a radio group or a checkbox:
// focusing one of those looks like it has been chosen.
watch(index, () => {
  void nextTick(() => {
    formEl.value?.closest('[data-modal-body]')?.scrollTo({ top: 0 })
    const first = formEl.value?.querySelector<HTMLElement>(
      'input:not([type=radio]):not([type=checkbox]):not([disabled]), select:not([disabled]), textarea:not([disabled])',
    )
    first?.focus()
  })
})
</script>

<template>
  <Modal :title="title" :description="description" :size="size" @close="emit('close')">
    <div class="flex flex-col gap-5 sm:flex-row sm:gap-7">
      <!-- Phone: the rail's information without its footprint. -->
      <p class="eyebrow sm:hidden">
        Step {{ index + 1 }} of {{ visible.length }} · {{ current?.title }}
      </p>

      <!-- Sticky because the rail is the map: it scrolls away with the fields on a short
           screen, which is exactly when a reader wants to know how much is left. -->
      <ol class="hidden w-44 shrink-0 flex-col gap-1 self-start sm:sticky sm:top-0 sm:flex">
        <li v-for="(s, i) in visible" :key="s.id">
          <!--
            A tick means "you have been here and it is filled in", so it survives stepping
            back — the answers did not disappear when the reader did. Anywhere already
            visited is one click away, EXCEPT past a step that has since become incomplete:
            that is the gate Next enforces, and the rail must not be a way around it.
          -->
          <button
            type="button"
            class="flex w-full items-center gap-2.5 rounded-[var(--radius-control)] px-2 py-1.5 text-left text-sm transition"
            :class="[
              i === index && 'font-medium',
              i <= furthest && !visible.slice(0, i).some((p) => p.complete === false)
                ? 'cursor-pointer'
                : 'cursor-default',
            ]"
            :style="{
              background: i === index ? 'var(--accent-soft)' : undefined,
              color:
                i === index
                  ? 'var(--accent-text)'
                  : i <= furthest
                    ? 'var(--text-muted)'
                    : 'var(--text-subtle)',
            }"
            :disabled="i > furthest || visible.slice(0, i).some((p) => p.complete === false)"
            :aria-current="i === index ? 'step' : undefined"
            @click="jump(i)"
          >
            <span
              class="grid size-5 shrink-0 place-items-center rounded-full text-[10px] font-semibold"
              :style="
                i === index || (i < index && s.complete !== false && i <= furthest)
                  ? { background: 'var(--accent)', color: 'var(--accent-contrast)' }
                  : { border: '1px solid var(--border-strong)' }
              "
            >
              <AppIcon
                v-if="i !== index && i <= furthest && s.complete !== false"
                name="check"
                class="size-3"
              />
              <template v-else>{{ i + 1 }}</template>
            </span>
            <span class="min-w-0 flex-1 truncate">{{ s.title }}</span>
          </button>
        </li>
      </ol>

      <form
        :id="formId"
        ref="formEl"
        class="min-w-0 flex-1 space-y-4 pb-1"
        @submit.prevent="primary"
      >
        <p v-if="current?.description" class="muted max-w-[70ch] text-sm leading-relaxed">
          {{ current.description }}
        </p>

        <slot v-if="current" :name="current.id" />

        <p v-if="error" class="text-sm leading-relaxed" :style="{ color: 'var(--danger)' }">
          {{ error }}
        </p>
      </form>
    </div>

    <template #footer>
      <span class="subtle mr-auto text-xs">Step {{ index + 1 }} of {{ visible.length }}</span>

      <BaseButton
        v-if="index > 0 && !current?.hideBack"
        intent="secondary"
        size="md"
        :disabled="submitting"
        @click="goTo(index - 1)"
      >
        <AppIcon name="chevronLeft" class="size-4" />
        Back
      </BaseButton>

      <BaseButton
        type="submit"
        :form="formId"
        intent="primary"
        size="md"
        :loading="busy"
        :disabled="primaryDisabled"
      >
        <AppIcon v-if="current?.action?.icon && !busy" :name="current.action.icon" class="size-4" />
        {{ current?.action ? current.action.label : last ? submitLabel : 'Next' }}
        <AppIcon v-if="!current?.action && !last" name="chevronRight" class="size-4" />
      </BaseButton>
    </template>
  </Modal>
</template>
