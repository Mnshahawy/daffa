<script setup lang="ts">
import { computed, ref } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import {
  daffa,
  type ManifestApplyView,
  type ManifestReport,
  type ManifestResourceView,
} from '@/lib/api'
import { ago, absolute } from '@/lib/format'
import { type Status } from '@/lib/status'
import { EmptyState } from '@mnshahawy/daffa-console-ui'
import { PageHeader } from '@mnshahawy/daffa-console-ui'
import { StatusPill } from '@mnshahawy/daffa-console-ui'

// The manifest apply history. Manifests are a CLI-first feature — `daffa plan` and
// `daffa apply` do the work — so this page is the paper trail: which document was
// applied, by whom, and what each apply decided about each resource. The report is the
// interesting half; the document rides along verbatim so "was THIS file ever applied"
// has an answer you can diff.

const { data: applies, isLoading } = useQuery({
  queryKey: ['manifest-applies'],
  queryFn: daffa.listManifestApplies,
})

const selectedId = ref('')

const { data: detail } = useQuery({
  queryKey: ['manifest-apply', selectedId],
  queryFn: () => daffa.getManifestApply(selectedId.value),
  enabled: computed(() => !!selectedId.value),
})

// The generated type says `report` is a string (it is json.RawMessage on the wire, and
// raw JSON arrives as the object itself, not a quoted string) — accept either shape
// rather than trusting the annotation over the runtime.
const report = computed<ManifestReport | null>(() => {
  const raw: unknown = detail.value?.report
  if (!raw) return null
  if (typeof raw === 'string') {
    try {
      return JSON.parse(raw) as ManifestReport
    } catch {
      return null
    }
  }
  return raw as ManifestReport
})

/** "sha256:9f2c…" carries no news at full width; twelve hex chars identify a file. */
function shortHash(h: string): string {
  return h.replace(/^sha256:/, '').slice(0, 12)
}

/** Plan vs apply, at a glance. A plan touched nothing; an apply is the real event. */
function modeStatus(a: ManifestApplyView): Status {
  return a.dry_run ? { tone: 'neutral', label: 'plan' } : { tone: 'accent', label: 'apply' }
}

/**
 * The verdict palette follows what each one asks of the operator. in-sync is the goal
 * state, green. create and update are intended changes — accent and info, not alarms.
 * unfilled is amber: the apply succeeded but a secret slot still needs a value before
 * the stack can boot. blocked and drifted are red — blocked means the document asked
 * for something that cannot happen, and drifted means reality disagrees with the
 * declaration in a way apply deliberately refuses to converge (trust material is never
 * rotated by a file edit).
 */
function verdictStatus(v: ManifestResourceView['verdict']): Status {
  switch (v) {
    case 'in-sync':
      return { tone: 'success', label: 'in sync' }
    case 'create':
      return { tone: 'accent', label: 'create' }
    case 'update':
      return { tone: 'info', label: 'update' }
    case 'unfilled':
      return { tone: 'warn', label: 'unfilled' }
    case 'blocked':
      return { tone: 'danger', label: 'blocked' }
    case 'drifted':
      return { tone: 'danger', label: 'drifted' }
  }
}

/** Summary chips, skipping the zeros — a row of "0 blocked, 0 drifted" is noise. */
const summaryChips = computed<Status[]>(() => {
  const s = report.value?.summary
  if (!s) return []
  const chips: [number, Status['tone'], string][] = [
    [s.create, 'accent', 'to create'],
    [s.update, 'info', 'to update'],
    [s.in_sync, 'success', 'in sync'],
    [s.unfilled, 'warn', 'unfilled'],
    [s.blocked, 'danger', 'blocked'],
    [s.drifted, 'danger', 'drifted'],
  ]
  return chips.filter(([n]) => n > 0).map(([n, tone, label]) => ({ tone, label: `${n} ${label}` }))
})

function select(id: string) {
  selectedId.value = selectedId.value === id ? '' : id
}
</script>

<template>
  <div>
    <PageHeader
      title="Manifests"
      :count="applies?.length"
      description="Declared infrastructure, applied and diffed — every plan and apply, with what each one decided."
    />

    <p v-if="isLoading" class="muted text-sm">Loading…</p>

    <EmptyState
      v-else-if="!applies?.length"
      icon="file"
      title="No manifest has been applied yet"
      body="A manifest is one YAML file declaring the resources that should exist — stacks, certificates, deliveries — applied with `daffa plan -f manifest.yaml` to see what would change and `daffa apply -f manifest.yaml` to make it so. Every run lands here with its full report."
    />

    <template v-else>
      <div class="surface overflow-x-auto rounded-[var(--radius-card)]">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
              <th class="eyebrow px-4 py-2 text-left font-medium">Mode</th>
              <th class="eyebrow py-2 pr-4 text-left font-medium">Manifest</th>
              <th class="eyebrow hidden py-2 pr-4 text-left font-medium md:table-cell">Document</th>
              <th class="eyebrow hidden py-2 pr-4 text-left font-medium sm:table-cell">Who</th>
              <th class="eyebrow py-2 pr-4 text-right font-medium">When</th>
            </tr>
          </thead>

          <tbody>
            <!-- The whole row toggles the report below — an apply's interesting half is
                 its verdicts, not this line. -->
            <tr
              v-for="a in applies"
              :key="a.id"
              class="cursor-pointer border-b transition last:border-0 hover:bg-[var(--surface-sunken)]"
              :style="
                a.id === selectedId
                  ? { borderColor: 'var(--border)', background: 'var(--surface-sunken)' }
                  : { borderColor: 'var(--border)' }
              "
              @click="select(a.id)"
            >
              <td class="px-4 py-3"><StatusPill :status="modeStatus(a)" /></td>
              <td class="py-3 pr-4 font-medium">{{ a.name || '—' }}</td>
              <td class="subtle hidden py-3 pr-4 font-mono text-xs md:table-cell" :title="a.doc_hash">
                {{ shortHash(a.doc_hash) }}
              </td>
              <td class="muted hidden py-3 pr-4 text-xs sm:table-cell">{{ a.applied_by }}</td>
              <td class="py-3 pr-4 text-right">
                <time class="subtle text-xs whitespace-nowrap" :title="absolute(a.applied_at)">
                  {{ ago(a.applied_at) }}
                </time>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <div v-if="selectedId && detail" class="surface mt-4 rounded-[var(--radius-card)] p-4">
        <div class="flex flex-wrap items-center gap-x-3 gap-y-2">
          <StatusPill :status="modeStatus(detail)" />
          <span class="font-medium">{{ detail.name || 'unnamed manifest' }}</span>
          <span class="subtle font-mono text-xs" :title="detail.doc_hash">{{ shortHash(detail.doc_hash) }}</span>
          <span class="muted text-xs">{{ detail.applied_by }} · {{ absolute(detail.applied_at) }}</span>
        </div>

        <div v-if="summaryChips.length" class="mt-3 flex flex-wrap gap-1.5">
          <StatusPill v-for="(c, i) in summaryChips" :key="i" :status="c" />
        </div>

        <div v-if="report?.resources?.length" class="mt-4 overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
                <th class="eyebrow py-2 pr-4 text-left font-medium">Kind</th>
                <th class="eyebrow py-2 pr-4 text-left font-medium">Name</th>
                <th class="eyebrow hidden py-2 pr-4 text-left font-medium sm:table-cell">Cluster</th>
                <th class="eyebrow py-2 pr-4 text-left font-medium">Verdict</th>
                <th class="eyebrow hidden py-2 pr-4 text-left font-medium md:table-cell">Detail</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="(res, i) in report.resources"
                :key="i"
                class="border-b last:border-0"
                :style="{ borderColor: 'var(--border)' }"
              >
                <td class="subtle whitespace-nowrap py-2 pr-4 font-mono text-xs">{{ res.kind }}</td>
                <td class="py-2 pr-4 font-medium">{{ res.name }}</td>
                <td class="muted hidden py-2 pr-4 text-xs sm:table-cell">{{ res.cluster || '—' }}</td>
                <td class="py-2 pr-4"><StatusPill :status="verdictStatus(res.verdict)" /></td>
                <!-- The detail is the operator-facing sentence — on blocked and drifted
                     rows it is the entire point, so it must not hide behind a hover. -->
                <td class="muted hidden py-2 pr-4 text-xs md:table-cell">{{ res.detail || '' }}</td>
              </tr>
            </tbody>
          </table>
        </div>

        <!-- Unfilled slots are the "what do I still have to do" list: the apply is done,
             and these secrets still need values before their stacks can boot. -->
        <div v-if="report?.unfilled?.length" class="mt-4">
          <p class="eyebrow mb-1.5 font-medium">Unfilled secret slots</p>
          <ul class="space-y-1 text-sm">
            <li v-for="(u, i) in report.unfilled" :key="i" class="flex items-center gap-2">
              <StatusPill :status="{ tone: 'warn', label: 'unfilled' }" />
              <span class="font-mono text-xs">{{ u.name }}</span>
              <span v-if="u.stack" class="muted text-xs">
                on stack {{ u.stack }}<template v-if="u.cluster"> ({{ u.cluster }})</template>
              </span>
            </li>
          </ul>
        </div>

        <details v-if="detail.document" class="mt-4">
          <summary class="muted cursor-pointer text-sm">The document, as submitted</summary>
          <pre
            class="mt-2 overflow-x-auto rounded-[var(--radius-card)] p-3 font-mono text-xs"
            :style="{ background: 'var(--surface-sunken)' }"
            >{{ detail.document }}</pre
          >
        </details>
      </div>
    </template>
  </div>
</template>
