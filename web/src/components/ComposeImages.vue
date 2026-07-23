<script setup lang="ts">
// The Images tab for an inline compose editor. It parses the YAML server-side into its unique
// images and lets the operator set a new tag per image, validating that each tag actually exists
// in its registry before it is ever deployed. See .ai/image-upgrades.md.
//
// Phase 1: list + per-image tag validation. The latest-tag hint (phase 2) and Apply (phase 3)
// slot into the row and the footer respectively.
import { ref, computed, onMounted } from 'vue'
import { ApiError, daffa, type PreviewImage } from '@/lib/api'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'

const props = defineProps<{ envId: string; yaml: string; canEdit: boolean }>()
// Apply hands the rewritten YAML back to the editor that owns it; this component never persists.
const emit = defineEmits<{ applied: [yaml: string] }>()

type State = 'idle' | 'checking' | 'ok' | 'missing' | 'error'
interface Row extends PreviewImage {
  newTag: string
  state: State
  message: string
  hint: string // a newer tag the registry suggests, or '' — best-effort, never blocks anything
}

const rows = ref<Row[]>([])
const loading = ref(false)
const loadError = ref('')

async function load() {
  loading.value = true
  loadError.value = ''
  try {
    const res = await daffa.previewComposeImages({ env_id: props.envId, inline_yaml: props.yaml })
    rows.value = res.images.map((img) => ({ ...img, newTag: img.tag ?? '', state: 'idle', message: '', hint: '' }))
    void fetchHints()
  } catch (e) {
    loadError.value = e instanceof ApiError ? e.message : String(e)
    rows.value = []
  } finally {
    loading.value = false
  }
}

// Hints are fetched after the table is already up, in parallel, and any failure is swallowed:
// "the newest looks like…" is a nicety, never a precondition for validating or applying.
async function fetchHints() {
  await Promise.all(
    rows.value
      .filter((r) => r.kind === 'tag')
      .map(async (row) => {
        try {
          const res = await daffa.latestImageTag({ env_id: props.envId, image: row.ref })
          if (res.latest && res.latest !== row.tag) row.hint = res.latest
        } catch {
          /* best-effort */
        }
      }),
  )
}

function applyHint(row: Row) {
  row.newTag = row.hint
  validate(row)
}

// A row counts as "changed" when its tag differs from what is in the file. Apply is offered only
// when there is at least one such row AND every changed row has been validated as existing — a
// red or unchecked field cannot reach a deploy.
const changedRows = computed(() =>
  rows.value.filter((r) => r.kind === 'tag' && r.newTag.trim() && r.newTag.trim() !== r.tag),
)
const allChangedValid = computed(() => changedRows.value.every((r) => r.state === 'ok'))
const canApply = computed(
  () => props.canEdit && changedRows.value.length > 0 && allChangedValid.value,
)

const applying = ref(false)
const applied = ref(false)

async function apply() {
  applying.value = true
  applied.value = false
  loadError.value = ''
  try {
    const res = await daffa.rewriteComposeImages({
      env_id: props.envId,
      inline_yaml: props.yaml,
      changes: changedRows.value.map((r) => ({ old_ref: r.ref, new_tag: r.newTag.trim() })),
    })
    emit('applied', res.inline_yaml)
    // Reflect the new state in place: the changed tags are now the current ones.
    changedRows.value.forEach((r) => {
      r.tag = r.newTag.trim()
      r.hint = ''
    })
    applied.value = true
  } catch (e) {
    loadError.value = e instanceof ApiError ? e.message : String(e)
  } finally {
    applying.value = false
  }
}

async function validate(row: Row) {
  if (row.kind !== 'tag') return
  const tag = row.newTag.trim()
  if (!tag) {
    row.state = 'idle'
    row.message = ''
    return
  }
  row.state = 'checking'
  row.message = ''
  try {
    const res = await daffa.checkImageTag({ env_id: props.envId, image: row.ref, tag })
    if (res.error) {
      row.state = 'error'
      row.message = res.error
    } else if (res.exists) {
      row.state = 'ok'
      row.message = ''
    } else {
      row.state = 'missing'
      row.message = 'No such tag in the registry.'
    }
  } catch (e) {
    row.state = 'error'
    row.message = e instanceof ApiError ? e.message : String(e)
  }
}

const glyph: Record<State, string> = { idle: '', checking: '…', ok: '✓', missing: '✕', error: '!' }
const glyphColor: Record<State, string> = {
  idle: 'var(--text-subtle)',
  checking: 'var(--text-subtle)',
  ok: 'var(--success)',
  missing: 'var(--danger)',
  error: 'var(--warn)',
}

onMounted(load)
</script>

<template>
  <div>
    <div class="mb-3 flex items-center justify-between gap-3">
      <p class="muted text-xs">
        Set a tag and Daffa checks it exists in the registry before you deploy. Digest-pinned and
        variable tags are shown but not editable.
      </p>
      <BaseButton intent="ghost" size="sm" class="shrink-0" :loading="loading" @click="load">
        <AppIcon name="history" class="size-3.5" />
        Refresh
      </BaseButton>
    </div>

    <p v-if="loadError" class="text-sm" :style="{ color: 'var(--danger)' }">{{ loadError }}</p>

    <p v-else-if="!loading && !rows.length" class="muted text-sm">
      No images found in this compose file.
    </p>

    <div v-else class="surface overflow-hidden rounded-[var(--radius-card)]">
      <div
        v-for="row in rows"
        :key="row.ref"
        class="flex flex-wrap items-center gap-x-4 gap-y-2 border-b border-[var(--border)] p-3 last:border-b-0"
      >
        <!-- Image + services -->
        <div class="min-w-0 flex-1">
          <div class="truncate font-mono text-xs">{{ row.repo || row.ref }}</div>
          <div class="muted mt-0.5 truncate text-[11px]">{{ row.services.join(', ') }}</div>
        </div>

        <!-- Tag control -->
        <div v-if="row.kind === 'tag'" class="flex flex-wrap items-center gap-2">
          <input
            v-model="row.newTag"
            spellcheck="false"
            class="field w-40 font-mono text-xs"
            :placeholder="row.tag"
            data-cursor="text"
            @blur="validate(row)"
            @keyup.enter="validate(row)"
          />
          <span
            class="inline-block w-4 text-center font-mono text-sm"
            :style="{ color: glyphColor[row.state] }"
            :title="row.message"
            >{{ glyph[row.state] }}</span
          >
          <button
            v-if="row.hint"
            type="button"
            class="rounded-full px-2 py-0.5 font-mono text-[11px]"
            :style="{ color: 'var(--accent-text)', background: 'var(--accent-soft)' }"
            :title="`Use ${row.hint}`"
            @click="applyHint(row)"
          >
            ↑ {{ row.hint }}
          </button>
        </div>
        <div v-else class="muted text-xs">
          <span v-if="row.kind === 'digest'" title="pinned by digest">pinned · {{ (row.digest || '').slice(0, 19) }}…</span>
          <span v-else title="the tag comes from a variable">set by a variable</span>
        </div>
      </div>
    </div>

    <p
      v-for="row in rows.filter((r) => r.message)"
      :key="'msg-' + row.ref"
      class="mt-2 text-xs"
      :style="{ color: 'var(--warn)' }"
    >
      {{ row.repo || row.ref }}: {{ row.message }}
    </p>

    <div v-if="rows.length" class="mt-3 flex flex-wrap items-center gap-3">
      <BaseButton intent="primary" size="sm" :disabled="!canApply || applying" :loading="applying" @click="apply">
        Apply
      </BaseButton>
      <span v-if="!canEdit" class="muted text-xs">Editing the stack takes stacks.edit.</span>
      <span v-else-if="applied && !changedRows.length" class="text-xs" :style="{ color: 'var(--success)' }">
        Compose file updated.
      </span>
      <span
        v-else-if="changedRows.length && !allChangedValid"
        class="muted text-xs"
      >
        Validate the changed tags before applying.
      </span>
    </div>
  </div>
</template>
