<script setup lang="ts">
import { computed, nextTick, ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { daffa, type Environment } from '@/lib/api'
import { useSession } from '@/stores/session'
import { hostStatus } from '@/lib/status'
import { toast } from '@/lib/toast'
import DropdownMenu from './DropdownMenu.vue'
import AppIcon from './ui/AppIcon.vue'
import StatusPill from './ui/StatusPill.vue'
import { Cap } from '@/lib/caps'

// The environment is the context that every page in the rail below is scoped to, so the switcher
// sits at the top of the rail rather than in a corner of a toolbar. Get it wrong and you are
// looking at the right screen for the wrong machine.
defineProps<{ collapsed?: boolean }>()

// What an environment IS, in one word.
//
// This used to read the daemon's kind straight off the environment ('socket' or 'agent'), which
// worked only while an environment WAS a daemon. It is now made of them: a standalone environment
// has one, so its badge is still that daemon's kind; a swarm has however many it has, and the
// useful thing to say about it is how many.
function envBadge(env: Environment): string {
  if (env.swarm) {
    return env.nodes.length === 1 ? 'swarm' : `swarm · ${env.nodes.length}`
  }
  return env.nodes[0]?.kind === 'local' ? 'socket' : 'agent'
}

const session = useSession()
const qc = useQueryClient()

const { data: environments } = useQuery({
  queryKey: ['environments'],
  queryFn: daffa.environments,
  // The only poll in the app: a host going offline produces no event to react to.
  refetchInterval: 15_000,
})

const current = computed(() => environments.value?.find((e) => e.id === session.envId))

// ── renaming ──────────────────────────────────────────────────────────────────
// "Local" and "agent-3" are names Daffa invents. "web-1" and "prod-eu" are what people
// actually think in, so let them say so — inline, where the name already is.
const editing = ref<string | null>(null)
const draft = ref('')
const input = ref<HTMLInputElement>()

const rename = useMutation({
  mutationFn: ({ id, name }: { id: string; name: string }) => daffa.renameEnvironment(id, name),
  onSuccess: () => {
    toast.ok('Host renamed.')
    editing.value = null
    qc.invalidateQueries({ queryKey: ['environments'] })
  },
  onError: (e) => toast.err(e, 'Could not rename.'),
})

async function startEdit(env: Environment, e: Event) {
  e.stopPropagation() // do not let the click select the host and close the menu
  editing.value = env.id
  draft.value = env.name
  await nextTick()
  input.value?.focus()
  input.value?.select()
}

function commit() {
  const name = draft.value.trim()
  if (!editing.value || !name) return (editing.value = null)
  if (name === environments.value?.find((e) => e.id === editing.value)?.name) {
    editing.value = null
    return
  }
  rename.mutate({ id: editing.value, name })
}

function select(env: Environment) {
  if (editing.value) return
  session.envId = env.id
}
</script>

<template>
  <DropdownMenu v-if="current" align="left" block>
    <template #trigger="{ open }">
      <!-- Collapsed, this used to be a bare coloured dot: a green circle, floating in the rail,
           saying nothing about WHAT was online or that it could be clicked at all. It is the
           control that decides which machine every page below it is talking about, so it has to
           look like a host — the server glyph says what it is, and the status dot rides on its
           corner rather than standing in for it. -->
      <span
        class="flex w-full items-center gap-2 rounded-[var(--radius-control)] border text-sm transition hover:border-[var(--border-strong)]"
        :class="collapsed ? 'justify-center p-1.5' : 'px-2.5 py-1.5'"
        :style="{ borderColor: 'var(--border)', background: 'var(--surface-raised)' }"
        :title="collapsed ? `${current.name} — ${hostStatus(current.status).label}` : undefined"
      >
        <span class="relative flex shrink-0" :class="collapsed ? '' : 'items-center'">
          <AppIcon
            v-if="collapsed"
            name="server"
            class="size-[18px]"
            :style="{ color: 'var(--text-muted)' }"
          />
          <StatusPill v-else :status="hostStatus(current.status)" variant="dot" />

          <!-- Ringed in the surface it sits on, so the dot reads as a badge fixed to the icon
               rather than as a stray pixel touching it. A box-shadow rather than Tailwind's
               `ring`, because the ring utility resolves its colour through --tw-ring-color and
               reaching into that from an inline style is a dependency on an internal. -->
          <span
            v-if="collapsed"
            class="absolute -bottom-0.5 -right-0.5 size-2 rounded-full"
            :style="{
              background:
                hostStatus(current.status).tone === 'success' ? 'var(--success)' : 'var(--danger)',
              boxShadow: '0 0 0 2px var(--surface-raised)',
            }"
          />
        </span>

        <template v-if="!collapsed">
          <span class="min-w-0 flex-1 truncate text-left font-medium">{{ current.name }}</span>
          <AppIcon
            name="chevronDown"
            class="size-3.5 shrink-0 opacity-50 transition"
            :class="open ? 'rotate-180' : ''"
          />
        </template>
      </span>
    </template>

    <div class="eyebrow px-2 py-1.5">Hosts</div>

    <div
      v-for="env in environments"
      :key="env.id"
      class="flex items-center gap-2 rounded-lg px-2 py-1.5 text-sm transition hover:bg-[var(--surface-sunken)]"
      :class="env.id === session.envId ? 'bg-[var(--surface-sunken)]' : ''"
      @click.stop="select(env)"
    >
      <StatusPill :status="hostStatus(env.status)" variant="dot" />

      <input
        v-if="editing === env.id"
        ref="input"
        v-model="draft"
        class="field min-w-0 flex-1 px-1.5 py-0.5 text-sm"
        @click.stop
        @keydown.enter.stop="commit"
        @keydown.escape.stop="editing = null"
        @blur="commit"
      />

      <template v-else>
        <button class="min-w-0 flex-1 truncate text-left">{{ env.name }}</button>
        <span class="subtle shrink-0 font-mono text-[10px]">
          {{ envBadge(env) }}
        </span>
        <button
          v-if="session.can(Cap.HostsEdit)"
          class="subtle shrink-0 rounded p-0.5 transition hover:text-[var(--accent-text)]"
          aria-label="Rename host"
          title="Rename"
          @click.stop="startEdit(env, $event)"
        >
          <AppIcon name="pencil" class="size-3" />
        </button>
      </template>
    </div>

    <div class="mt-1 border-t pt-1" :style="{ borderColor: 'var(--border)' }">
      <RouterLink
        v-if="session.can(Cap.HostsEdit)"
        :to="{ name: 'settings-agents' }"
        class="muted flex items-center gap-2 rounded-lg px-2 py-1.5 text-sm transition hover:bg-[var(--surface-sunken)] hover:text-[var(--text)]"
      >
        <AppIcon name="plus" class="size-3.5" />
        Add a host…
      </RouterLink>
    </div>
  </DropdownMenu>
</template>
