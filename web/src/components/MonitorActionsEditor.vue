<script setup lang="ts">
/**
 * What a monitor DOES when it fires, besides telling someone: restart the service, run a
 * command in it (a heap dump, a thread dump), scale a swarm service, or POST the alert to a
 * URL. In order, once per firing, and never more often than the cooldown — the floor that
 * keeps restart → recover → refire from becoming a restart loop. See .ai/monitor-actions.md.
 *
 * Each kind is offered only to someone holding the power it wields on this cluster: arming a
 * restart is restarting, later, unattended.
 */
import { computed } from 'vue'
import type { MonitorAction } from '@/lib/api'
import { Cap } from '@/lib/caps'
import { useSession } from '@/stores/session'
import { AppIcon, BaseButton, Select } from '@mnshahawy/daffa-console-ui'

const actions = defineModel<MonitorAction[]>({ required: true })
const cooldown = defineModel<number>('cooldown', { required: true })
const props = defineProps<{ envId: string }>()

const session = useSession()

const kinds = computed(() => [
  {
    kind: 'restart',
    label: 'Restart',
    hint: 'Restart the containers one at a time, or force-update the swarm service.',
    allowed: session.can(Cap.ContainersEdit, props.envId),
    needs: 'containers.edit',
  },
  {
    kind: 'exec',
    label: 'Run a command',
    hint: 'In every replica — a heap or thread dump before the restart that loses it.',
    allowed: session.can(Cap.ContainersExec, props.envId),
    needs: 'containers.exec',
  },
  {
    kind: 'scale',
    label: 'Scale',
    hint: 'Change a swarm service’s replicas, never past a limit.',
    allowed: session.can(Cap.ServicesEdit, props.envId),
    needs: 'services.edit',
  },
  {
    kind: 'http',
    label: 'Call a URL',
    hint: 'POST the alert as JSON — a runbook, a pager, a ticket.',
    allowed: session.can(Cap.SettingsEdit, ''),
    needs: 'settings.edit fleet-wide',
  },
])
const label = (k: string) => kinds.value.find((x) => x.kind === k)?.label ?? k

const cooldowns = [
  { value: 300, label: '5 minutes' },
  { value: 900, label: '15 minutes' },
  { value: 1800, label: '30 minutes' },
  { value: 3600, label: '1 hour' },
  { value: 21600, label: '6 hours' },
  { value: 86400, label: '1 day' },
]

function add(kind: string) {
  const a: MonitorAction = { kind }
  if (kind === 'exec') a.command = ['sh', '-c', '']
  if (kind === 'scale') Object.assign(a, { by: 1, limit: 3 })
  if (kind === 'http') a.url = ''
  actions.value = [...actions.value, a]
}

function removeAt(i: number) {
  actions.value = actions.value.filter((_, j) => j !== i)
}

function move(i: number, d: -1 | 1) {
  const next = [...actions.value]
  ;[next[i], next[i + d]] = [next[i + d], next[i]]
  actions.value = next
}

// The form edits a command as one line run under `sh -c` — how people write these. A command
// saved as a bare argv (by the API or a manifest) is shown joined, and rewritten under sh -c
// only if someone edits it here.
function commandText(a: MonitorAction): string {
  const c = a.command ?? []
  return c[0] === 'sh' && c[1] === '-c' ? (c[2] ?? '') : c.join(' ')
}
function setCommand(a: MonitorAction, text: string) {
  a.command = ['sh', '-c', text]
}
</script>

<template>
  <div>
    <div class="mb-1.5 text-sm font-medium">
      When it fires <span class="subtle font-normal">(optional)</span>
    </div>

    <p v-if="!envId" class="subtle text-xs">
      Pick a cluster to add actions — a rule that restarts things on every host at once is not one
      Daffa will run.
    </p>

    <template v-else>
      <ol v-if="actions.length" class="mb-3 space-y-2">
        <li
          v-for="(a, i) in actions"
          :key="i"
          class="rounded-[var(--radius-control)] border p-3"
          :style="{ borderColor: 'var(--border)' }"
        >
          <div class="mb-2 flex items-center justify-between gap-2">
            <span class="text-sm font-medium">{{ i + 1 }}. {{ label(a.kind) }}</span>
            <div class="flex gap-1">
              <BaseButton intent="ghost" size="xs" :disabled="i === 0" aria-label="Move up" @click="move(i, -1)">
                ↑
              </BaseButton>
              <BaseButton
                intent="ghost"
                size="xs"
                :disabled="i === actions.length - 1"
                aria-label="Move down"
                @click="move(i, 1)"
              >
                ↓
              </BaseButton>
              <BaseButton intent="ghost" size="xs" aria-label="Remove" @click="removeAt(i)">
                <AppIcon name="x" class="size-3.5" />
              </BaseButton>
            </div>
          </div>

          <p v-if="a.kind === 'restart'" class="subtle text-xs">
            Containers restart one at a time; a swarm service is force-updated and rolls its tasks
            under its own update policy. The image is not re-pulled.
          </p>

          <div v-else-if="a.kind === 'exec'" class="grid gap-2 sm:grid-cols-[1fr_8rem]">
            <div>
              <input
                :value="commandText(a)"
                required
                placeholder="jcmd 1 GC.heap_dump /dumps/heap-$(date +%s).hprof"
                class="field font-mono text-xs"
                data-cursor="text"
                @input="setCommand(a, ($event.target as HTMLInputElement).value)"
              />
              <p class="subtle mt-1 text-xs">
                Runs under <code class="font-mono">sh -c</code> in each replica. Write dumps to a
                mounted volume — the container's own filesystem goes with the restart.
              </p>
            </div>
            <label class="text-xs">
              <span class="subtle mb-1 block">Timeout (s)</span>
              <input v-model.number="a.timeout_secs" type="number" min="1" max="600" placeholder="60" class="field font-mono text-xs" />
            </label>
          </div>

          <div v-else-if="a.kind === 'scale'" class="grid gap-2 sm:grid-cols-2">
            <label class="text-xs">
              <span class="subtle mb-1 block">Change replicas by</span>
              <input v-model.number="a.by" type="number" min="-10" max="10" class="field font-mono text-xs" />
            </label>
            <label class="text-xs">
              <span class="subtle mb-1 block">{{ (a.by ?? 0) < 0 ? 'But never below' : 'But never above' }}</span>
              <input v-model.number="a.limit" type="number" min="1" class="field font-mono text-xs" />
            </label>
            <p class="subtle text-xs sm:col-span-2">Swarm services only.</p>
          </div>

          <div v-else-if="a.kind === 'http'" class="grid gap-2 sm:grid-cols-2">
            <input
              v-model="a.url"
              required
              type="url"
              placeholder="https://runbooks.internal/hooks/memory"
              class="field font-mono text-xs"
              data-cursor="text"
            />
            <input
              v-model="a.secret"
              type="password"
              autocomplete="new-password"
              :placeholder="a.has_secret ? '•••••••• (unchanged)' : 'Signing secret (optional)'"
              class="field font-mono text-xs"
            />
            <p class="subtle text-xs sm:col-span-2">
              The body is signed as <code class="font-mono">X-Daffa-Signature: sha256=…</code> when a
              secret is set — the GitHub webhook shape.
            </p>
          </div>
        </li>
      </ol>

      <div class="flex flex-wrap gap-2">
        <BaseButton
          v-for="k in kinds"
          :key="k.kind"
          intent="secondary"
          size="xs"
          :disabled="!k.allowed || actions.length >= 5"
          :title="k.allowed ? k.hint : `Needs ${k.needs}`"
          @click="add(k.kind)"
        >
          <AppIcon name="plus" class="size-3.5" />
          {{ k.label }}
        </BaseButton>
      </div>

      <div v-if="actions.length" class="mt-3 flex items-center gap-2 text-xs">
        <label for="m-cooldown" class="subtle">At most once per target every</label>
        <Select id="m-cooldown" v-model.number="cooldown" class="w-40">
          <option v-for="c in cooldowns" :key="c.value" :value="c.value">{{ c.label }}</option>
        </Select>
      </div>
    </template>
  </div>
</template>
