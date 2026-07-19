<script setup lang="ts">
import { computed, ref } from 'vue'
import { useQuery, useQueryClient } from '@tanstack/vue-query'
import { ApiError, daffa, type Alert, type Monitor } from '@/lib/api'
import { Cap } from '@/lib/caps'
import { confirm } from '@/lib/confirm'
import { useSession } from '@/stores/session'
import { type Status } from '@/lib/status'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import EmptyState from '@/components/ui/EmptyState.vue'
import StatusPill from '@/components/ui/StatusPill.vue'

const session = useSession()
const qc = useQueryClient()

const canEdit = computed(() => session.canAnywhere(Cap.MonitorsEdit))
// Sampling and retention are one setting for the whole fleet, so changing them takes the
// capability EVERYWHERE, not on the host you happen to have selected.
const canEditFleet = computed(() => session.can(Cap.MonitorsEdit, ''))

const { data: monitors } = useQuery({ queryKey: ['monitors'], queryFn: daffa.monitors })
const { data: alerts } = useQuery({
  queryKey: ['alerts'],
  queryFn: daffa.alerts,
  refetchInterval: 30_000,
})
const { data: config } = useQuery({ queryKey: ['monitor-config'], queryFn: daffa.monitorConfig })
const { data: envs } = useQuery({
  queryKey: ['environments'],
  queryFn: daffa.environments,
})

const error = ref('')
const busy = ref(false)

// ── sampling and retention ────────────────────────────────────────────────────

// The three numbers a person can choose, and ONLY those. updated_at belongs to the server, which
// stamps it — sending one back was how this form spent its life failing on every save: an empty
// string is not a timestamp, and the request never got past decoding.
const form = ref({ enabled: true, interval_secs: 30, retention_days: 7 })
const loaded = ref(false)
const _seed = computed(() => {
  if (!loaded.value && config.value) {
    const { enabled, interval_secs, retention_days } = config.value.settings
    form.value = { enabled, interval_secs, retention_days }
    loaded.value = true
  }
  return null
})

// The floor comes from the server, which is the thing that enforces it — a second copy here would
// be a second place to forget. The fallback only covers the moment before the config has landed.
const minInterval = computed(() => config.value?.min_interval ?? 30)
const tooFast = computed(() => form.value.interval_secs < minInterval.value)

async function saveConfig() {
  busy.value = true
  error.value = ''
  try {
    await daffa.saveMonitorConfig(form.value)
    await qc.invalidateQueries({ queryKey: ['monitor-config'] })
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : 'Could not save.'
  } finally {
    busy.value = false
  }
}

// ── the rules ─────────────────────────────────────────────────────────────────

const blank = {
  name: '',
  enabled: true,
  metric: 'mem_pct',
  op: '>',
  threshold: 70,
  duration_secs: 600,
  env_id: '',
  stack: '',
  container: '',
}
const draft = ref({ ...blank })
const editing = ref<string | null>(null)

// ── target suggestions (stack / container comboboxes) ─────────────────────────
// The Stack and Container targets are name filters ("" = any). These feed a <datalist> so an
// operator can pick from what exists without losing the ability to type a name that is not running
// yet (a stopped container, a not-yet-deployed stack, an "Every host" rule). Declared after draft:
// vue-query reads the reactive key/enabled during setup, so draft must already exist.
const { data: stacks } = useQuery({ queryKey: ['stacks'], queryFn: daffa.stacks })

// Containers are listed per host, and the endpoint needs containers.view on it — so the query is
// gated on both a chosen host and the capability. That also means "Every host" fetches nothing (no
// single host to enumerate) and the operator simply types the name there.
const { data: containers } = useQuery({
  queryKey: ['monitor-containers', () => draft.value.env_id],
  queryFn: () => daffa.containers(draft.value.env_id),
  enabled: computed(() => !!draft.value.env_id && session.can(Cap.ContainersView, draft.value.env_id)),
})

// Stack names for the chosen host (all hosts when fleet-wide), de-duped.
const stackOptions = computed(() => {
  const list = stacks.value ?? []
  const scoped = draft.value.env_id ? list.filter((s) => s.env_id === draft.value.env_id) : list
  return [...new Set(scoped.map((s) => s.name))].sort()
})

// Containers on the host, narrowed to the selected stack (its compose project) when one is set.
const containerOptions = computed(() => {
  const list = containers.value ?? []
  const scoped = draft.value.stack ? list.filter((c) => c.project === draft.value.stack) : list
  return [...new Set(scoped.map((c) => c.name))].sort()
})

// ── what a rule is written in, versus what it is stored in ────────────────────
//
// A rule is stored in exactly one unit per metric: percent, or BYTES, or cores. Nobody types
// bytes. So the editor is written in the units a person thinks in — GB, MB, vCPU — and these
// three refs are the translation.
//
// They are deliberately NOT part of `draft`: the number in the box and the number on the wire
// are different quantities, and keeping them in one field is how "512" becomes 512 bytes, or how
// switching MB to GB silently multiplies somebody's threshold by 1024.

type Resource = 'mem' | 'cpu'
type Unit = '%' | 'MB' | 'GB' | 'vCPU'

const MB = 1024 ** 2
const GB = 1024 ** 3

const resource = ref<Resource>('mem')
const unit = ref<Unit>('%')
const amount = ref(70)

const unitsFor: Record<Resource, Unit[]> = {
  mem: ['%', 'MB', 'GB'],
  cpu: ['%', 'vCPU'],
}

/** A sensible line to draw, per unit, so a switch never leaves a nonsense number in the box. */
const defaults: Record<Unit, number> = { '%': 70, MB: 512, GB: 1, vCPU: 1 }

const isBytes = (u: Unit) => u === 'MB' || u === 'GB'
const round2 = (v: number) => Math.round(v * 100) / 100

/** Which metric the (resource, unit) pair means on the wire. */
const metric = computed<string>(() => {
  if (resource.value === 'cpu') return unit.value === '%' ? 'cpu_pct' : 'cpu_cores'
  return unit.value === '%' ? 'mem_pct' : 'mem_bytes'
})

/** The threshold in the unit the server stores — percent, bytes, or cores. */
const threshold = computed(() => {
  if (unit.value === 'MB') return Math.round(amount.value * MB)
  if (unit.value === 'GB') return Math.round(amount.value * GB)
  return amount.value
})

/** The rule as it would be saved. The sentence reads from this, so it describes what you are
 *  about to create rather than what you started from. */
const pending = computed(() => ({
  ...draft.value,
  metric: metric.value,
  threshold: threshold.value,
}))

function pickResource(r: Resource) {
  if (r === resource.value) return
  const wasPercent = unit.value === '%'
  resource.value = r
  // Carry the KIND of rule across. Someone moving a "> 2 GB" memory rule to CPU means "> n vCPU";
  // dropping them back onto a percentage would quietly undo the decision they just made.
  pickUnit(wasPercent ? '%' : r === 'cpu' ? 'vCPU' : 'GB')
}

function pickUnit(u: Unit) {
  if (u === unit.value) return
  // MB → GB is the same quantity said differently, so the number comes with it. Every other move
  // crosses into a different kind of quantity, where the old number means nothing: 70 percent is
  // not 70 GB, and carrying it over would be a nasty little surprise.
  if (isBytes(u) && isBytes(unit.value)) {
    amount.value = u === 'GB' ? round2(amount.value / 1024) : Math.round(amount.value * 1024)
  } else {
    amount.value = defaults[u]
  }
  unit.value = u
}

/** The reverse translation: a stored rule, back into the units it was written in. */
function decompose(m: Monitor) {
  resource.value = m.metric.startsWith('cpu') ? 'cpu' : 'mem'

  // Assigned directly, NOT through pickUnit — that one converts and re-defaults, which is what
  // you want when a person changes their mind and exactly what you do not want when loading a
  // rule that already exists.
  if (m.metric === 'mem_bytes') {
    // Say it back in the unit it was typed in. GB when GB can say it EXACTLY — 1.5 GB comes back
    // as 1.5 GB, not as 1536 MB — and MB otherwise, which is the finer of the two and so the
    // likelier to be exact. Exactness is the constraint that matters: renaming a monitor must
    // not quietly move its threshold, and it can only do that if the number the box shows
    // multiplies back to the number that was stored.
    const exact = (u: number) => round2(m.threshold / u) * u === m.threshold
    const gb = m.threshold >= GB && exact(GB)
    unit.value = gb ? 'GB' : 'MB'
    amount.value = round2(m.threshold / (gb ? GB : MB))
  } else if (m.metric === 'cpu_cores') {
    unit.value = 'vCPU'
    amount.value = round2(m.threshold)
  } else {
    unit.value = '%'
    amount.value = m.threshold
  }
}

function edit(m: Monitor) {
  editing.value = m.id
  draft.value = { ...m }
  decompose(m)
  error.value = ''
}

function reset() {
  editing.value = null
  draft.value = { ...blank }
  resource.value = 'mem'
  unit.value = '%'
  amount.value = 70
  error.value = ''
}

async function save() {
  busy.value = true
  error.value = ''
  try {
    const rule = pending.value
    if (editing.value) await daffa.updateMonitor(editing.value, rule)
    else await daffa.createMonitor(rule)
    await qc.invalidateQueries({ queryKey: ['monitors'] })
    reset()
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : 'Could not save the monitor.'
  } finally {
    busy.value = false
  }
}

async function remove(m: Monitor) {
  const ok = await confirm({
    title: `Delete the monitor ${m.name}?`,
    body: 'Its alert history goes with it — including the record of every time it fired and recovered. Nothing will watch this condition afterwards, and nobody will be emailed about it.',
    confirmLabel: 'Delete',
    intent: 'danger',
  })
  if (!ok) return
  busy.value = true
  try {
    await daffa.deleteMonitor(m.id)
    await qc.invalidateQueries({ queryKey: ['monitors'] })
    await qc.invalidateQueries({ queryKey: ['alerts'] })
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : 'Could not delete.'
  } finally {
    busy.value = false
  }
}

// ── how a rule reads ──────────────────────────────────────────────────────────

const durations = [
  { value: 60, label: '1 minute' },
  { value: 300, label: '5 minutes' },
  { value: 600, label: '10 minutes' },
  { value: 1800, label: '30 minutes' },
  { value: 3600, label: '1 hour' },
]

function metricLabel(metric: string): string {
  return metric.startsWith('cpu') ? 'CPU' : 'Memory'
}

/**
 * A number in the unit its metric is actually in. A rule that reads "above 2147483648" is one
 * somebody has to do arithmetic on at 3am, and "above 1.5" is one they have to guess at — the
 * CPU rule two lines up is measured in percent.
 */
function inUnits(metric: string, v: number): string {
  if (metric === 'mem_bytes') return bytes(v)
  if (metric === 'cpu_cores') return `${round2(v)} vCPU`
  return `${round2(v)}%`
}

function envName(id: string): string {
  if (!id) return 'every host'
  return envs.value?.find((e) => e.id === id)?.name ?? id
}

/** The rule, as a sentence. It is the only reliable way to know you typed what you meant. */
function sentence(m: Monitor | typeof blank): string {
  const dir = m.op === '>' ? 'stays above' : 'stays below'
  const dur = durations.find((d) => d.value === m.duration_secs)?.label ?? `${m.duration_secs}s`
  const where = [
    m.container ? `container ${m.container}` : '',
    m.stack ? `stack ${m.stack}` : '',
    `on ${envName(m.env_id)}`,
  ]
    .filter(Boolean)
    .join(', ')
  return `Alert when ${metricLabel(m.metric)} ${dir} ${inUnits(m.metric, m.threshold)} for ${dur} — ${where}.`
}

function fmtValue(a: Alert, m?: Monitor): string {
  return inUnits(m?.metric ?? 'mem_pct', a.value)
}

function monitorFor(a: Alert): Monitor | undefined {
  return monitors.value?.find((m) => m.id === a.monitor_id)
}

function bytes(v: number): string {
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  // Unary + drops a trailing zero, so a threshold reads "512 MB" rather than "512.0 MB" — while
  // 1.5 GB is still 1.5 GB.
  return `${i === 0 ? v.toFixed(0) : +v.toFixed(1)} ${units[i]}`
}

function ago(iso: string): string {
  const secs = (Date.now() - new Date(iso).getTime()) / 1000
  if (secs < 90) return 'just now'
  if (secs < 3600) return `${Math.round(secs / 60)}m ago`
  if (secs < 86400) return `${Math.round(secs / 3600)}h ago`
  return `${Math.round(secs / 86400)}d ago`
}

const firing = computed(() => alerts.value?.filter((a) => a.state === 'firing') ?? [])
const recent = computed(() => alerts.value?.filter((a) => a.state !== 'firing').slice(0, 10) ?? [])

/**
 * A monitor's own state. A rule that is currently firing is the only thing on this page worth
 * interrupting for, so it says so on the rule itself and not only in the table above — and it
 * pulses, because it is happening right now.
 */
function monitorState(m: Monitor): Status {
  if (!m.enabled) return { tone: 'neutral', label: 'Disabled' }
  const hits = firing.value.filter((a) => a.monitor_id === m.id)
  if (hits.length)
    return {
      tone: 'danger',
      label: 'Firing',
      live: true,
      detail: hits.length === 1 ? hits[0].container_name : `${hits.length} containers`,
    }
  return { tone: 'success', label: 'OK' }
}

const firingStatus: Status = { tone: 'danger', label: 'Firing', live: true }
const recoveredStatus: Status = { tone: 'success', label: 'Recovered' }
</script>

<template>
  <div>
    <span hidden>{{ _seed }}</span>

    <div class="mb-5">
      <h2 class="text-base font-semibold">Resource monitors</h2>
      <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
        Alert when a container stays over the line — not when it merely touches it.
      </p>
    </div>

    <p
      v-if="error"
      role="alert"
      class="mb-4 rounded-[var(--radius-control)] px-3 py-2 text-sm"
      :style="{ background: 'var(--danger-soft)', color: 'var(--danger)' }"
    >
      {{ error }}
    </p>

    <!-- Firing first. It is the only thing on this page that might need doing right now. -->
    <section
      v-if="firing.length"
      class="surface mb-5 overflow-hidden rounded-[var(--radius-card)]"
      :style="{ borderColor: 'var(--danger)' }"
    >
      <h3
        class="eyebrow border-b px-4 py-2.5"
        :style="{ borderColor: 'var(--border)', color: 'var(--danger)' }"
      >
        Firing
      </h3>

      <table class="w-full text-sm">
        <thead>
          <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
            <th class="eyebrow px-4 py-2 text-left font-medium">State</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Container</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Monitor</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">Value</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">Since</th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="a in firing"
            :key="a.id"
            class="border-b last:border-0"
            :style="{ borderColor: 'var(--border)' }"
          >
            <td class="px-4 py-3">
              <StatusPill :status="firingStatus" />
            </td>
            <td class="py-3 pr-4">
              <span class="font-medium">{{ a.container_name }}</span>
              <span v-if="a.stack" class="subtle text-xs"> · {{ a.stack }}</span>
            </td>
            <td class="py-3 pr-4">{{ a.monitor_name }}</td>
            <td class="py-3 pr-4 text-right font-mono text-xs font-medium">
              {{ fmtValue(a, monitorFor(a)) }}
            </td>
            <td class="subtle py-3 pr-4 text-right text-xs">{{ ago(a.started_at) }}</td>
          </tr>
        </tbody>
      </table>
    </section>

    <!-- ── the rules ─────────────────────────────────────────────────────────── -->
    <section class="surface mb-5 rounded-[var(--radius-card)] p-5">
      <h3 class="text-sm font-semibold">Monitors</h3>
      <p class="muted mb-4 mt-1 max-w-[70ch] text-sm leading-relaxed">
        A monitor fires when the condition holds for the <em>whole</em> window — a single sample
        back under the threshold resets the clock. Who gets emailed is set under
        <RouterLink to="/settings/notifications" class="transition hover:text-[var(--accent-text)]">
          Notifications
        </RouterLink>
        , on the <em>Resource monitor fired</em> event.
      </p>

      <table v-if="monitors?.length" class="w-full text-sm">
        <thead>
          <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
            <th class="eyebrow py-2 pr-4 text-left font-medium">State</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Rule</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Host</th>
            <th class="eyebrow py-2 text-right font-medium">Actions</th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="m in monitors"
            :key="m.id"
            class="border-b align-top last:border-0"
            :class="{ 'opacity-60': !m.enabled }"
            :style="{ borderColor: 'var(--border)' }"
          >
            <td class="py-3 pr-4">
              <StatusPill :status="monitorState(m)" />
            </td>

            <td class="py-3 pr-4">
              <div class="font-medium">{{ m.name }}</div>
              <div class="subtle mt-0.5 text-xs leading-snug">{{ sentence(m) }}</div>
            </td>

            <td class="py-3 pr-4">
              <span
                class="subtle inline-block rounded-md border px-1.5 py-0.5 font-mono text-[10px] whitespace-nowrap"
                :style="{ borderColor: 'var(--border)' }"
              >
                {{ envName(m.env_id) }}
              </span>
            </td>

            <td class="py-3">
              <div class="flex items-center justify-end gap-1">
                <BaseButton v-if="canEdit" intent="secondary" size="xs" @click="edit(m)">
                  <AppIcon name="pencil" class="size-3.5" />
                  Edit
                </BaseButton>
                <BaseButton
                  v-if="canEdit"
                  intent="danger"
                  size="xs"
                  :disabled="busy"
                  @click="remove(m)"
                >
                  <AppIcon name="trash" class="size-3.5" />
                  Delete
                </BaseButton>
              </div>
            </td>
          </tr>
        </tbody>
      </table>

      <EmptyState
        v-else
        icon="gauge"
        title="No monitors yet"
        body="A monitor watches CPU or memory for a container and emails you when it stays over the line for a whole window — the difference between a spike, which is normal, and a leak, which is not."
      />

      <!-- ── the editor ───────────────────────────────────────────────────────── -->
      <form
        v-if="canEdit"
        class="mt-5 space-y-4 border-t pt-5"
        :style="{ borderColor: 'var(--border)' }"
        @submit.prevent="save"
      >
        <h4 class="text-sm font-semibold">{{ editing ? 'Edit monitor' : 'New monitor' }}</h4>

        <div class="grid gap-4 sm:grid-cols-2">
          <div>
            <label for="m-name" class="mb-1.5 block text-sm font-medium">Name</label>
            <input
              id="m-name"
              v-model="draft.name"
              required
              placeholder="Memory high"
              class="field"
              data-cursor="text"
            />
          </div>
          <div class="flex items-end">
            <label for="m-enabled" class="mb-2.5 flex items-center gap-2 text-sm">
              <input
                id="m-enabled"
                v-model="draft.enabled"
                type="checkbox"
                class="accent-[var(--color-accent-500)]"
              />
              Enabled
            </label>
          </div>
        </div>

        <div class="grid gap-4 sm:grid-cols-4">
          <div>
            <label for="m-metric" class="mb-1.5 block text-sm font-medium">Watch</label>
            <select
              id="m-metric"
              class="field"
              :value="resource"
              @change="pickResource(($event.target as HTMLSelectElement).value as Resource)"
            >
              <option value="mem">Memory</option>
              <option value="cpu">CPU</option>
            </select>
          </div>
          <div>
            <label for="m-op" class="mb-1.5 block text-sm font-medium">When it</label>
            <select id="m-op" v-model="draft.op" class="field">
              <option value=">">stays above</option>
              <option value="<">stays below</option>
            </select>
          </div>

          <!-- The threshold and its unit are one control, because they are one decision: "70"
               means nothing until you have said whether it is percent, megabytes or cores. -->
          <div>
            <label for="m-threshold" class="mb-1.5 block text-sm font-medium">Threshold</label>
            <div class="flex gap-1.5">
              <input
                id="m-threshold"
                v-model.number="amount"
                type="number"
                step="any"
                min="0"
                required
                class="field min-w-0 flex-1 font-mono text-xs"
              />
              <label :for="'m-unit'" class="sr-only">Unit</label>
              <select
                id="m-unit"
                class="field w-20 shrink-0"
                :value="unit"
                @change="pickUnit(($event.target as HTMLSelectElement).value as Unit)"
              >
                <option v-for="u in unitsFor[resource]" :key="u" :value="u">{{ u }}</option>
              </select>
            </div>
          </div>

          <div>
            <label for="m-duration" class="mb-1.5 block text-sm font-medium">For</label>
            <select id="m-duration" v-model.number="draft.duration_secs" class="field">
              <option v-for="d in durations" :key="d.value" :value="d.value">{{ d.label }}</option>
            </select>
          </div>
        </div>

        <!--
          A percentage of WHAT. This is the one thing about this form that will bite somebody,
          and it bites silently: the monitor looks healthy and simply never fires.
        -->
        <p
          v-if="unit === '%'"
          class="flex items-start gap-2.5 rounded-[var(--radius-control)] px-3 py-2.5 text-sm leading-relaxed"
          :style="{ background: 'var(--warn-soft)' }"
        >
          <AppIcon name="alert" class="mt-0.5 size-4 shrink-0" :style="{ color: 'var(--warn)' }" />
          <span>
            A percentage is measured against the container's <strong>limit</strong> —
            <span class="font-mono text-xs">cpus:</span> and
            <span class="font-mono text-xs">mem_limit:</span> in its compose file. A container
            without one is allowed the whole machine, so the ceiling becomes the host's
            {{ resource === 'cpu' ? 'cores' : 'memory' }} and the rule quietly means
            “{{ amount }}% of the entire node” — a line one container will almost never cross.
            <strong>Set a limit on the service, or write the rule in
              {{ resource === 'cpu' ? 'vCPU' : 'MB / GB' }}.</strong>
          </span>
        </p>

        <div class="grid gap-4 sm:grid-cols-3">
          <div>
            <label for="m-env" class="mb-1.5 block text-sm font-medium">Host</label>
            <select id="m-env" v-model="draft.env_id" class="field">
              <!--
                Every host is a FLEET-WIDE rule, and it takes monitors.edit everywhere. A
                host-scoped holder is not offered it, because the server would refuse it — and
                being told "no" after filling in a form is worse than not being offered.
              -->
              <option v-if="canEditFleet" value="">Every host</option>
              <option v-for="e in envs" :key="e.id" :value="e.id">{{ e.name }}</option>
            </select>
          </div>
          <div>
            <label for="m-stack" class="mb-1.5 block text-sm font-medium">
              Stack <span class="subtle font-normal">(optional)</span>
            </label>
            <input
              id="m-stack"
              v-model="draft.stack"
              list="monitor-stacks"
              placeholder="any"
              class="field font-mono text-xs"
              data-cursor="text"
            />
            <datalist id="monitor-stacks">
              <option v-for="s in stackOptions" :key="s" :value="s" />
            </datalist>
          </div>
          <div>
            <label for="m-container" class="mb-1.5 block text-sm font-medium">
              Container <span class="subtle font-normal">(optional)</span>
            </label>
            <input
              id="m-container"
              v-model="draft.container"
              list="monitor-containers"
              placeholder="any"
              class="field font-mono text-xs"
              data-cursor="text"
            />
            <datalist id="monitor-containers">
              <option v-for="c in containerOptions" :key="c" :value="c" />
            </datalist>
          </div>
        </div>

        <p
          class="rounded-[var(--radius-control)] px-3 py-2 text-sm"
          :style="{ background: 'var(--surface-sunken)' }"
        >
          {{ sentence(pending) }}
        </p>

        <div class="flex items-center gap-2">
          <BaseButton type="submit" intent="primary" size="md" :loading="busy">
            {{ editing ? 'Save' : 'Create monitor' }}
          </BaseButton>
          <BaseButton v-if="editing" intent="secondary" size="md" @click="reset">Cancel</BaseButton>
        </div>
      </form>
    </section>

    <!-- ── sampling ─────────────────────────────────────────────────────────── -->
    <section class="surface mb-5 rounded-[var(--radius-card)] p-5">
      <h3 class="text-sm font-semibold">Sampling</h3>
      <p class="muted mb-4 mt-1 max-w-[70ch] text-sm leading-relaxed">
        Daffa records CPU and memory for every running container. Everything above is built on
        these samples, and so are the charts on the container, stack and host pages.
      </p>

      <form class="space-y-4" @submit.prevent="saveConfig">
        <div class="grid gap-4 sm:grid-cols-3">
          <div class="flex items-end">
            <label for="s-enabled" class="mb-2.5 flex items-center gap-2 text-sm">
              <input
                id="s-enabled"
                v-model="form.enabled"
                type="checkbox"
                :disabled="!canEditFleet"
                class="accent-[var(--color-accent-500)]"
              />
              Record samples
            </label>
          </div>

          <div>
            <label for="s-interval" class="mb-1.5 block text-sm font-medium">
              Every <span class="subtle font-normal">(seconds)</span>
            </label>
            <input
              id="s-interval"
              v-model.number="form.interval_secs"
              type="number"
              :min="minInterval"
              :disabled="!canEditFleet"
              class="field font-mono text-xs"
              :aria-describedby="tooFast ? 's-interval-floor' : undefined"
            />
            <!-- Said before the save, not after it. The server refuses anything under the floor,
                 and a person who has to submit a form to discover the rule has been made to fail
                 for no reason. -->
            <p
              v-if="tooFast"
              id="s-interval-floor"
              class="mt-1.5 text-xs leading-snug"
              :style="{ color: 'var(--warn)' }"
            >
              The minimum is {{ minInterval }} seconds. Sampling faster costs a
              <span class="font-mono">stats</span> call per container per round and tells you
              nothing more — the shortest rule window is 60 seconds.
            </p>
          </div>

          <div>
            <label for="s-retention" class="mb-1.5 block text-sm font-medium">
              Keep for <span class="subtle font-normal">(days)</span>
            </label>
            <input
              id="s-retention"
              v-model.number="form.retention_days"
              type="number"
              min="1"
              :max="config?.max_retention ?? 90"
              :disabled="!canEditFleet"
              class="field font-mono text-xs"
            />
          </div>
        </div>

        <p class="muted max-w-[70ch] text-sm leading-relaxed">
          Samples older than the retention window are expired by dropping whole days at a time, so
          it costs nothing to keep. The ceiling is
          <span class="font-mono">{{ config?.max_retention ?? 90 }}</span> days — Daffa is a
          container console, not a time-series database.
        </p>

        <!-- A feature that quietly grows a database owes you the number. -->
        <p v-if="config?.usage" class="muted text-sm">
          <span class="font-mono font-medium">{{ config.usage.samples.toLocaleString() }}</span>
          samples across
          <span class="font-mono font-medium">{{ config.usage.partitions }}</span>
          {{ config.usage.partitions === 1 ? 'day' : 'days'
          }}<template v-if="config.usage.oldest">, oldest {{ ago(config.usage.oldest) }}</template
          >. Roughly <span class="font-mono">{{ bytes(config.usage.bytes) }}</span> on disk.
        </p>

        <div v-if="canEditFleet">
          <BaseButton type="submit" intent="primary" size="md" :loading="busy">Save</BaseButton>
        </div>
      </form>
    </section>

    <!-- ── history ──────────────────────────────────────────────────────────── -->
    <section v-if="recent.length" class="surface overflow-hidden rounded-[var(--radius-card)]">
      <div class="border-b px-5 pb-4 pt-5" :style="{ borderColor: 'var(--border)' }">
        <h3 class="text-sm font-semibold">Recently recovered</h3>
        <p class="muted mt-1 max-w-[70ch] text-sm leading-relaxed">
          Kept on purpose: “it was in trouble for an hour last night and recovered” is the thing you
          most want to find in the morning.
        </p>
      </div>

      <table class="w-full text-sm">
        <thead>
          <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
            <th class="eyebrow px-4 py-2 text-left font-medium">State</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Container</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Why it cleared</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">Recovered</th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="a in recent"
            :key="a.id"
            class="border-b last:border-0"
            :style="{ borderColor: 'var(--border)' }"
          >
            <td class="px-4 py-3">
              <StatusPill :status="recoveredStatus" />
            </td>
            <td class="py-3 pr-4">
              <span class="font-medium">{{ a.container_name }}</span>
              <span class="subtle text-xs"> · {{ a.monitor_name }}</span>
            </td>
            <td class="muted py-3 pr-4 text-xs">{{ a.resolve_reason }}</td>
            <td class="subtle py-3 pr-4 text-right text-xs">
              {{ a.resolved_at ? ago(a.resolved_at) : '' }}
            </td>
          </tr>
        </tbody>
      </table>
    </section>
  </div>
</template>
