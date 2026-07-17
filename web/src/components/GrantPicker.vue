<script setup lang="ts">
import type { Environment, Grant, Role } from '@/lib/api'

const props = defineProps<{
  roles: Role[]
  envs: Environment[]
  modelValue: Grant[]
  disabled?: boolean
}>()

const emit = defineEmits<{ 'update:modelValue': [Grant[]] }>()

// A grant is a role AND a scope. The same role held on two hosts is two grants, and
// toggling one must not disturb the other — so both fields are the key.
function held(roleId: string, envId: string): boolean {
  return props.modelValue.some((g) => g.role_id === roleId && g.env_id === envId)
}

function toggle(roleId: string, envId: string, on: boolean) {
  const without = props.modelValue.filter((g) => !(g.role_id === roleId && g.env_id === envId))
  emit('update:modelValue', on ? [...without, { role_id: roleId, env_id: envId }] : without)
}

// Granting a role everywhere makes any per-host grant of it redundant — and leaving both
// ticked would show a state that reads as "and also on staging", which is meaningless.
function toggleGlobal(roleId: string, on: boolean) {
  const others = props.modelValue.filter((g) => g.role_id !== roleId)
  emit('update:modelValue', on ? [...others, { role_id: roleId, env_id: '' }] : others)
}

// A row that grants something looks like it grants something. Colour alone would not do it.
function rowStyle(roleId: string) {
  const on = props.modelValue.some((g) => g.role_id === roleId)
  return {
    borderColor: 'var(--border)',
    background: on ? 'var(--accent-soft)' : undefined,
  }
}
</script>

<template>
  <div class="overflow-x-auto">
    <table class="w-full text-sm">
      <thead>
        <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
          <th class="eyebrow pb-2 text-left font-medium">Role</th>
          <th class="eyebrow w-24 pb-2 text-center font-medium">Everywhere</th>
          <th v-for="e in envs" :key="e.id" class="eyebrow w-24 pb-2 text-center font-medium">
            {{ e.name }}
          </th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="r in roles" :key="r.id" class="border-b last:border-0" :style="rowStyle(r.id)">
          <td class="py-2 pr-4">
            <span class="font-medium">{{ r.name }}</span>
            <span
              v-if="r.is_admin"
              class="ml-1.5 rounded-md px-1.5 py-0.5 text-xs font-medium"
              :style="{ background: 'var(--accent-soft)', color: 'var(--accent-text)' }"
            >
              everything
            </span>
            <p v-if="r.description" class="muted mt-0.5 text-xs">{{ r.description }}</p>
          </td>

          <td class="text-center align-top">
            <input
              type="checkbox"
              :checked="held(r.id, '')"
              :disabled="disabled"
              class="mt-1 accent-[var(--accent)]"
              :aria-label="`${r.name} everywhere`"
              @change="toggleGlobal(r.id, ($event.target as HTMLInputElement).checked)"
            />
          </td>

          <!-- A role carrying a capability that has no meaning on one host can only be
               granted everywhere. Rather than let someone tick a box that the server will
               then refuse, the box is not there — and the reason is. -->
          <td v-for="e in envs" :key="e.id" class="text-center align-top">
            <input
              v-if="r.scopable"
              type="checkbox"
              :checked="held(r.id, e.id) || held(r.id, '')"
              :disabled="disabled || held(r.id, '')"
              class="mt-1 accent-[var(--accent)]"
              :title="held(r.id, '') ? 'Already granted everywhere' : `${r.name} on ${e.name}`"
              :aria-label="`${r.name} on ${e.name}`"
              @change="toggle(r.id, e.id, ($event.target as HTMLInputElement).checked)"
            />
            <span
              v-else
              class="subtle text-xs"
              :title="`This role carries ${r.global_only?.join(', ')}, which administers Daffa itself and cannot be limited to one host.`"
            >
              —
            </span>
          </td>
        </tr>
      </tbody>
    </table>
  </div>

  <p v-if="roles.some((r) => !r.scopable)" class="muted mt-3 text-xs">
    A role marked <span class="font-mono">—</span> can only be granted everywhere: it carries a
    capability that administers Daffa itself (users, roles, settings), and Daffa is not
    per-host. Hover to see which.
  </p>
</template>
