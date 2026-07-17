<script setup lang="ts">
import { computed, ref } from 'vue'
import { useQuery, useQueryClient } from '@tanstack/vue-query'
import { ApiError, daffa, type Role } from '@/lib/api'
import { Cap } from '@/lib/caps'
import { confirm } from '@/lib/confirm'
import { useSession } from '@/stores/session'
import CapabilityMatrix from '@/components/CapabilityMatrix.vue'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import EmptyState from '@/components/ui/EmptyState.vue'

const session = useSession()
const qc = useQueryClient()

const canEdit = computed(() => session.can(Cap.RolesEdit))

const { data: roles } = useQuery({ queryKey: ['roles'], queryFn: daffa.roles })
const { data: capabilities } = useQuery({
  queryKey: ['capabilities'],
  queryFn: daffa.capabilities,
})

const editing = ref<Role | null>(null)
const creating = ref(false)
const error = ref('')
const busy = ref(false)

const form = ref({ name: '', description: '', cap_names: [] as string[] })

function startCreate() {
  creating.value = true
  editing.value = null
  error.value = ''
  form.value = { name: '', description: '', cap_names: [] }
}

function startEdit(r: Role) {
  creating.value = false
  editing.value = r
  error.value = ''
  form.value = { name: r.name, description: r.description, cap_names: [...r.cap_names] }
}

function cancel() {
  creating.value = false
  editing.value = null
  error.value = ''
}

async function save() {
  busy.value = true
  error.value = ''
  try {
    if (editing.value) {
      await daffa.updateRole(editing.value.id, form.value)
    } else {
      await daffa.createRole(form.value)
    }
    await qc.invalidateQueries({ queryKey: ['roles'] })
    // The person editing may have just changed their OWN permissions. Re-read them, or
    // the UI keeps showing buttons they no longer have.
    await session.refresh()
    cancel()
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : 'Could not save the role.'
  } finally {
    busy.value = false
  }
}

async function remove(r: Role) {
  // Who actually loses something is the whole question, and it is knowable — so say it, and
  // say it in people rather than in rows. A role nobody holds is a different act from one
  // that will strip four people of everything it grants, and only the second is worth making
  // someone type the name out.
  const ok = await confirm({
    title: `Delete the role “${r.name}”?`,
    body:
      r.members > 0
        ? `${r.members} ${r.members === 1 ? 'person holds' : 'people hold'} it and will lose everything it grants, on every host, the moment you delete it. Their accounts stay; their access does not.`
        : 'Nobody holds it, so nobody loses access. It cannot be brought back — the capabilities would have to be ticked again from scratch.',
    confirmLabel: 'Delete role',
    intent: 'danger',
    typeToConfirm: r.members > 0 ? r.name : undefined,
  })
  if (!ok) return

  error.value = ''
  try {
    await daffa.deleteRole(r.id)
    await qc.invalidateQueries({ queryKey: ['roles'] })
    await session.refresh()
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : 'Could not delete the role.'
  }
}
</script>

<template>
  <div>
    <!-- A settings sub-page sits under the Settings title, so it gets a section heading rather
         than a second <h1> competing with it. -->
    <div class="mb-5 flex flex-wrap items-start gap-x-3 gap-y-2">
      <div class="min-w-0">
        <h2 class="text-base font-semibold">Roles</h2>
        <p class="muted mt-0.5 text-sm">
          A role is a set of capabilities. What someone may do is the union of every role they
          hold.
        </p>
      </div>

      <div class="ml-auto shrink-0">
        <BaseButton v-if="canEdit && !creating && !editing" intent="primary" @click="startCreate">
          <AppIcon name="plus" class="size-4" />
          New role
        </BaseButton>
      </div>
    </div>

    <p v-if="error" class="mb-4 text-sm" :style="{ color: 'var(--danger)' }">{{ error }}</p>

    <!-- Editor -->
    <div v-if="creating || editing" class="surface mb-6 rounded-[var(--radius-card)] p-5">
      <p class="mb-4 text-sm font-medium">
        {{ editing ? `Edit “${editing.name}”` : 'New role' }}
      </p>

      <div class="mb-5 grid gap-4 sm:grid-cols-2">
        <div>
          <label for="role-name" class="mb-1.5 block text-sm font-medium">Name</label>
          <input
            id="role-name"
            v-model="form.name"
            placeholder="Deployers"
            class="field"
            data-cursor="text"
          />
        </div>
        <div>
          <label for="role-desc" class="mb-1.5 block text-sm font-medium">Description</label>
          <input
            id="role-desc"
            v-model="form.description"
            placeholder="Can deploy stacks, but not touch backups"
            class="field"
            data-cursor="text"
          />
        </div>
      </div>

      <CapabilityMatrix
        v-if="capabilities"
        v-model="form.cap_names"
        :areas="capabilities.areas"
        :capabilities="capabilities.capabilities"
      />

      <div class="mt-5 flex gap-2">
        <BaseButton
          intent="primary"
          size="md"
          :disabled="!form.name"
          :loading="busy"
          @click="save"
        >
          Save role
        </BaseButton>
        <BaseButton intent="secondary" size="md" @click="cancel">Cancel</BaseButton>
      </div>
    </div>

    <!-- List -->
    <EmptyState
      v-if="roles && !roles.length && !creating"
      icon="check"
      title="No roles yet"
      body="A role bundles the capabilities someone is trusted with — deploy stacks, read logs, open a shell. Nobody can be granted anything until one exists."
    >
      <template #action>
        <BaseButton v-if="canEdit" intent="primary" size="md" @click="startCreate">
          <AppIcon name="plus" class="size-4" />
          New role
        </BaseButton>
      </template>
    </EmptyState>

    <div v-else-if="roles?.length" class="surface overflow-hidden rounded-[var(--radius-card)]">
      <div class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
              <th class="eyebrow px-4 py-2 text-left font-medium">Role</th>
              <th class="eyebrow hidden py-2 pr-4 text-left font-medium sm:table-cell">Members</th>
              <th class="eyebrow py-2 pr-4 text-left font-medium">Grants</th>
              <th v-if="canEdit" class="eyebrow py-2 pr-4 text-right font-medium">Actions</th>
            </tr>
          </thead>

          <tbody>
            <tr
              v-for="r in roles"
              :key="r.id"
              class="border-b transition last:border-0 hover:bg-[var(--surface-sunken)]"
              :style="{ borderColor: 'var(--border)' }"
            >
              <td class="px-4 py-3">
                <div class="flex flex-wrap items-center gap-2">
                  <span class="font-medium">{{ r.name }}</span>
                  <span
                    v-if="r.is_admin"
                    class="rounded-md px-1.5 py-0.5 text-xs font-medium"
                    :style="{ background: 'var(--accent-soft)', color: 'var(--accent-text)' }"
                  >
                    every capability
                  </span>
                  <span v-if="r.builtin" class="subtle text-xs">built in</span>
                </div>
                <p v-if="r.description" class="muted mt-0.5 text-sm">{{ r.description }}</p>
              </td>

              <td class="muted hidden py-3 pr-4 font-mono text-xs sm:table-cell">
                {{ r.members }}
              </td>

              <td class="py-3 pr-4 text-xs">
                <template v-if="r.is_admin">
                  <span class="muted">
                    everything, including capabilities added in future versions
                  </span>
                </template>
                <template v-else-if="r.cap_names.length">
                  <span class="muted font-mono">{{ r.cap_names.length }} capabilities</span>
                </template>
                <template v-else>
                  <span :style="{ color: 'var(--warn)' }">
                    no capabilities — this role grants nothing
                  </span>
                </template>
              </td>

              <td v-if="canEdit" class="py-3 pr-4">
                <div class="flex items-center justify-end gap-1">
                  <!-- The Admin role is all-capabilities by definition; a checkbox grid for it
                       would be a grid that does nothing. -->
                  <BaseButton v-if="!r.is_admin" intent="secondary" size="xs" @click="startEdit(r)">
                    <AppIcon name="pencil" class="size-3" />
                    Edit
                  </BaseButton>
                  <BaseButton v-if="!r.builtin" intent="danger" size="xs" @click="remove(r)">
                    <AppIcon name="trash" class="size-3" />
                    Delete
                  </BaseButton>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</template>
