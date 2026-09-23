<script setup lang="ts">
import { computed, ref } from 'vue'
import { useQuery, useQueryClient } from '@tanstack/vue-query'
import { daffa, type APIToken, type Grant, type User } from '@/lib/api'
import { Cap } from '@/lib/caps'
import { confirm } from '@mnshahawy/daffa-console-ui'
import { useSession } from '@/stores/session'
import { toast } from '@mnshahawy/daffa-console-ui'
import GrantPicker from '@/components/GrantPicker.vue'
import TokenReveal from '@/components/TokenReveal.vue'
import { AppIcon } from '@mnshahawy/daffa-console-ui'
import { BaseButton } from '@mnshahawy/daffa-console-ui'
import { EmptyState } from '@mnshahawy/daffa-console-ui'
import { StatusPill } from '@mnshahawy/daffa-console-ui'

const session = useSession()
const qc = useQueryClient()

const canEdit = computed(() => session.can(Cap.UsersEdit))

const { data: users } = useQuery({ queryKey: ['users'], queryFn: daffa.users })
// Needed to offer roles when creating or granting. Someone with users.edit but not
// roles.view cannot see the list — so they cannot grant anything, and the form says so
// rather than showing an empty dropdown.
const { data: roles } = useQuery({
  queryKey: ['roles'],
  queryFn: daffa.roles,
  enabled: computed(() => session.can(Cap.RolesView)),
})
// The hosts a role can be limited to. Only the ones this admin can see — you cannot grant
// access to a host you cannot see yourself.
const { data: envs } = useQuery({ queryKey: ['environments'], queryFn: daffa.environments })
// A bot's tokens live on the oversight list (users.edit, like everything a bot needs).
const { data: allTokens } = useQuery({
  queryKey: ['tokens-all'],
  queryFn: daffa.allTokens,
  enabled: canEdit,
})

const busy = ref(false)
const adding = ref(false)
const editingRoles = ref<User | null>(null)
const selectedGrants = ref<Grant[]>([])

// Setting a password used to be a window.prompt(): unstyleable, untypeable into on a phone,
// and it echoed the new password in plain text in a box you cannot mark as a password field.
const settingPassword = ref<User | null>(null)
const newPassword = ref('')

const editingEmail = ref<User | null>(null)
const newEmail = ref('')

type Kind = 'local' | 'bot'
const blankForm = (kind: Kind) => ({ kind, username: '', email: '', password: '', grants: [] as Grant[] })
const form = ref(blankForm('local'))

function startAdding(kind: Kind) {
  form.value = blankForm(kind)
  adding.value = true
}

async function afterChange() {
  await qc.invalidateQueries({ queryKey: ['users'] })
  await qc.invalidateQueries({ queryKey: ['roles'] }) // member counts moved
  // We may have just changed our own access.
  await session.refresh()
}

async function create() {
  busy.value = true
  try {
    const { password, ...rest } = form.value
    // A bot has no password, and the server refuses one rather than ignoring it.
    const created = await daffa.createUser(rest.kind === 'bot' ? rest : { ...rest, password })
    await afterChange()
    adding.value = false
    form.value = blankForm('local')
    if (created.kind === 'bot') {
      toast.ok('Bot created. Mint it a token.')
      startTokens(created)
    } else {
      toast.ok('User created.')
    }
  } catch (e) {
    toast.err(e, form.value.kind === 'bot' ? 'Could not create the bot.' : 'Could not create the user.')
  } finally {
    busy.value = false
  }
}

function startRoles(u: User) {
  editingRoles.value = editingRoles.value?.id === u.id ? null : u
  settingPassword.value = null
  editingEmail.value = null
  managingTokens.value = null
  // Only the locally granted ones are ours to change. The provider's are re-synced from
  // its claims on every login.
  selectedGrants.value = u.roles
    .filter((r) => r.source === 'local')
    .map((r) => ({ role_id: r.role_id, env_id: r.env_id ?? '' }))
}

// "Operator" or "Operator on staging" — the scope is part of what the grant IS, so showing
// the role alone would be a half-truth.
function describe(name: string, envName?: string): string {
  return envName ? `${name} on ${envName}` : name
}

async function saveRoles() {
  if (!editingRoles.value) return
  busy.value = true
  try {
    await daffa.setUserRoles(editingRoles.value.id, selectedGrants.value)
    await afterChange()
    editingRoles.value = null
    toast.ok('Roles saved.')
  } catch (e) {
    toast.err(e, 'Could not save the roles.')
  } finally {
    busy.value = false
  }
}

async function toggleDisabled(u: User) {
  // Enabling is safe and needs no ceremony. Disabling ends their session and locks them out —
  // recoverable, so amber rather than red, but not something to do by mis-clicking a row.
  if (!u.disabled) {
    const ok = await confirm({
      title: `Disable ${u.label}?`,
      body:
        u.kind === 'bot'
          ? 'Every token this bot holds stops working on its next request. Its roles, tokens and audit history are kept, and enabling it again brings the same tokens back.'
          : 'They can no longer sign in, and any session they have open stops working. Their roles and their audit history are kept, and you can enable the account again at any time.',
      confirmLabel: 'Disable',
      intent: 'caution',
    })
    if (!ok) return
  }

  try {
    await daffa.updateUser(u.id, { disabled: !u.disabled })
    await afterChange()
    toast.ok('Account updated.')
  } catch (e) {
    toast.err(e, 'Could not change the account.')
  }
}

async function remove(u: User) {
  const ok = await confirm({
    title: `Delete ${u.label}?`,
    body:
      u.kind === 'bot'
        ? 'The bot, its roles and every token it holds go — whatever authenticates with them stops working. Its audit entries are kept. To pause it instead, disable it.'
        : 'The account and every role granted to it go. Their entries in the audit log are kept — what they did is still on the record, it simply no longer has an account attached. If you only want to lock them out, disable the account instead.',
    confirmLabel: u.kind === 'bot' ? 'Delete bot' : 'Delete user',
    intent: 'danger',
  })
  if (!ok) return

  try {
    await daffa.deleteUser(u.id)
    await afterChange()
    toast.ok('User deleted.')
  } catch (e) {
    toast.err(e, 'Could not delete the user.')
  }
}

function startPassword(u: User) {
  settingPassword.value = settingPassword.value?.id === u.id ? null : u
  editingRoles.value = null
  editingEmail.value = null
  managingTokens.value = null
  newPassword.value = ''
}

function startEmail(u: User) {
  editingEmail.value = editingEmail.value?.id === u.id ? null : u
  editingRoles.value = null
  settingPassword.value = null
  managingTokens.value = null
  newEmail.value = u.email
}

async function saveEmail() {
  const u = editingEmail.value
  if (!u) return
  busy.value = true
  try {
    await daffa.updateUser(u.id, { email: newEmail.value })
    await afterChange()
    editingEmail.value = null
    toast.ok(`Email updated for ${u.label}.`)
  } catch (e) {
    toast.err(e, 'Could not update the email.')
  } finally {
    busy.value = false
  }
}

async function resetPassword() {
  const u = settingPassword.value
  if (!u || !newPassword.value) return
  busy.value = true
  try {
    await daffa.setUserPassword(u.id, newPassword.value)
    // Nothing on the row changes when a password is set, so without this the click looks
    // like it did nothing at all.
    toast.ok(`New password set for ${u.label}.`)
    settingPassword.value = null
    newPassword.value = ''
  } catch (e) {
    toast.err(e, 'Could not set the password.')
  } finally {
    busy.value = false
  }
}

// ── a bot's tokens ──────────────────────────────────────────────────────────────
// A bot cannot sign in to mint its own, so they are minted here. The secret is shown once.

const managingTokens = ref<User | null>(null)
const tokenForm = ref({ name: '', expires_days: 0 })
const minted = ref<{ name: string; token: string; owner: string } | null>(null)

function startTokens(u: User) {
  managingTokens.value = managingTokens.value?.id === u.id ? null : u
  editingRoles.value = null
  settingPassword.value = null
  editingEmail.value = null
  tokenForm.value = { name: '', expires_days: 0 }
}

function tokensOf(u: User): APIToken[] {
  return (allTokens.value ?? []).filter((t) => t.user_id === u.id)
}

/** A bot never signs in; the nearest equivalent is the last time any of its tokens was used. */
function lastTokenUse(u: User): string | undefined {
  return tokensOf(u)
    .map((t) => t.last_used_at)
    .filter((t): t is string => !!t)
    .sort()
    .at(-1)
}

async function mintToken() {
  const u = managingTokens.value
  if (!u) return
  busy.value = true
  try {
    const res = await daffa.createBotToken(u.id, { ...tokenForm.value })
    minted.value = { name: res.name, token: res.token, owner: u.label }
    tokenForm.value = { name: '', expires_days: 0 }
    await qc.invalidateQueries({ queryKey: ['tokens-all'] })
  } catch (e) {
    toast.err(e, 'Could not mint the token.')
  } finally {
    busy.value = false
  }
}

async function revokeToken(u: User, t: APIToken) {
  const ok = await confirm({
    title: `Revoke ${u.label}'s token ${t.name}?`,
    body: 'Whatever authenticates with it stops working on its next request. This cannot be undone — a replacement is a new token.',
    confirmLabel: 'Revoke',
    intent: 'danger',
  })
  if (!ok) return
  try {
    await daffa.deleteToken(t.id)
    await qc.invalidateQueries({ queryKey: ['tokens-all'] })
    toast.ok('Token revoked.')
  } catch (e) {
    toast.err(e, 'Could not revoke the token.')
  }
}
</script>

<template>
  <div>
    <!-- A settings sub-page sits under the Settings title, so it gets a section heading rather
         than a second <h1> competing with it. -->
    <div class="mb-5 flex flex-wrap items-start gap-x-3 gap-y-2">
      <div class="min-w-0">
        <h2 class="text-base font-semibold">Users</h2>
        <p class="muted mt-0.5 text-sm">
          Accounts that sign in through an identity provider appear here after their first
          sign-in. Bots are for automation: they cannot sign in, and act only through the
          API tokens minted for them here.
        </p>
      </div>

      <div v-if="canEdit" class="ml-auto flex shrink-0 gap-2">
        <BaseButton v-if="adding" intent="secondary" @click="adding = false">Cancel</BaseButton>
        <template v-else>
          <BaseButton intent="secondary" @click="startAdding('bot')">
            <AppIcon name="plus" class="size-4" />
            New bot
          </BaseButton>
          <BaseButton intent="primary" @click="startAdding('local')">
            <AppIcon name="plus" class="size-4" />
            New user
          </BaseButton>
        </template>
      </div>
    </div>

    <!-- New local user -->
    <form
      v-if="adding"
      class="surface mb-6 rounded-[var(--radius-card)] p-5"
      @submit.prevent="create"
    >
      <p class="mb-1 text-sm font-medium">{{ form.kind === 'bot' ? 'New bot' : 'New user' }}</p>
      <p v-if="form.kind === 'bot'" class="muted mb-4 max-w-2xl text-xs leading-relaxed">
        An account for CI or a script, not a person: no password, no sign-in. It holds roles
        like anyone else, so give it the narrowest one that does the job — then mint its token.
        It keeps working when the person who set it up leaves.
      </p>
      <div v-else class="mb-4" />

      <div class="mb-5 grid gap-4 sm:grid-cols-3">
        <div>
          <label for="u-username" class="mb-1.5 block text-sm font-medium">Username</label>
          <input
            id="u-username"
            v-model="form.username"
            autocomplete="off"
            class="field font-mono text-xs"
            data-cursor="text"
          />
        </div>
        <div>
          <label for="u-email" class="mb-1.5 block text-sm font-medium">
            {{ form.kind === 'bot' ? 'Contact email' : 'Email' }}
          </label>
          <input
            id="u-email"
            v-model="form.email"
            autocomplete="off"
            class="field"
            data-cursor="text"
          />
        </div>
        <div v-if="form.kind !== 'bot'">
          <label for="u-password" class="mb-1.5 block text-sm font-medium">Password</label>
          <input
            id="u-password"
            v-model="form.password"
            type="password"
            autocomplete="new-password"
            class="field"
            data-cursor="text"
          />
          <p class="subtle mt-1 text-xs">At least 12 characters.</p>
        </div>
      </div>

      <p class="mb-1.5 text-sm font-medium">Roles</p>
      <p v-if="!roles?.length" class="muted mb-4 text-xs">
        You cannot see the list of roles, so you cannot grant one. Someone with the
        <code class="font-mono">roles.view</code> capability can.
      </p>
      <div v-else class="mb-4">
        <GrantPicker v-model="form.grants" :roles="roles" :envs="envs ?? []" />
      </div>

      <BaseButton
        type="submit"
        intent="primary"
        size="md"
        :disabled="!form.username || (form.kind !== 'bot' && !form.password) || !form.grants.length"
        :loading="busy"
      >
        {{ form.kind === 'bot' ? 'Create bot' : 'Create user' }}
      </BaseButton>
      <p class="muted mt-2 text-xs">
        {{
          form.kind === 'bot'
            ? 'A bot with no role holds tokens that can do nothing, so at least one is required.'
            : 'A user with no role can sign in and see nothing, so at least one is required.'
        }}
      </p>
    </form>

    <!-- List -->
    <EmptyState
      v-if="users && !users.length && !adding"
      icon="users"
      title="Nobody can sign in yet"
      body="Users are the accounts that can reach Daffa at all. Create one with a password, or configure an identity provider under Authentication and people will appear here after their first sign-in."
    >
      <template #action>
        <BaseButton v-if="canEdit" intent="primary" size="md" @click="startAdding('local')">
          <AppIcon name="plus" class="size-4" />
          New user
        </BaseButton>
      </template>
    </EmptyState>

    <div v-else-if="users?.length" class="surface overflow-hidden rounded-[var(--radius-card)]">
      <div class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
              <th class="eyebrow px-4 py-2 text-left font-medium">User</th>
              <th class="eyebrow py-2 pr-4 text-left font-medium">Roles</th>
              <th class="eyebrow hidden py-2 pr-4 text-left font-medium md:table-cell">
                Last signed in
              </th>
              <th v-if="canEdit" class="eyebrow py-2 pr-4 text-right font-medium">Actions</th>
            </tr>
          </thead>

          <tbody>
            <template v-for="u in users" :key="u.id">
              <tr
                class="border-b transition last:border-0 hover:bg-[var(--surface-sunken)]"
                :style="{ borderColor: 'var(--border)' }"
              >
                <td class="px-4 py-3">
                  <div class="flex flex-wrap items-center gap-2">
                    <span class="font-medium" :class="u.disabled ? 'line-through opacity-60' : ''">
                      {{ u.label }}
                    </span>
                    <span
                      class="subtle rounded-md border px-1.5 py-0.5 font-mono text-[10px]"
                      :style="{ borderColor: 'var(--border)' }"
                    >
                      {{ u.kind === 'oidc' ? 'SSO' : u.kind === 'bot' ? 'bot' : 'password' }}
                    </span>
                    <StatusPill
                      v-if="u.disabled"
                      :status="{
                        tone: 'warn',
                        label: 'Disabled',
                        detail: u.kind === 'bot' ? 'its tokens are refused' : 'cannot sign in',
                      }"
                    />
                  </div>
                </td>

                <td class="py-3 pr-4">
                  <div class="flex flex-wrap items-center gap-1.5">
                    <span
                      v-for="r in u.roles"
                      :key="`${r.role_id}:${r.env_id ?? ''}`"
                      class="flex items-center gap-1 rounded-md px-1.5 py-0.5 text-xs"
                      :style="
                        r.is_admin
                          ? { background: 'var(--accent-soft)', color: 'var(--accent-text)' }
                          : { background: 'var(--surface-sunken)' }
                      "
                    >
                      {{ describe(r.name, r.env_name) }}
                      <!-- Locked: the provider grants it, and it comes back on the next login. -->
                      <span v-if="r.source === 'oidc'" title="Granted by the identity provider">
                        🔒
                      </span>
                    </span>
                    <span v-if="!u.roles.length" class="text-xs" :style="{ color: 'var(--warn)' }">
                      {{
                        u.kind === 'bot'
                          ? 'no roles — its tokens can do nothing'
                          : 'no roles — this account can sign in and see nothing'
                      }}
                    </span>
                  </div>
                </td>

                <td class="subtle hidden py-3 pr-4 text-xs md:table-cell">
                  <template v-if="u.kind === 'bot'">
                    <time v-if="lastTokenUse(u)" :title="lastTokenUse(u)">
                      token used {{ new Date(lastTokenUse(u)!).toLocaleString() }}
                    </time>
                    <template v-else>never signs in</template>
                  </template>
                  <time v-else-if="u.last_login_at" :title="u.last_login_at">
                    {{ new Date(u.last_login_at).toLocaleString() }}
                  </time>
                  <template v-else>never</template>
                </td>

                <td v-if="canEdit" class="py-3 pr-4">
                  <div class="flex flex-wrap items-center justify-end gap-1">
                    <BaseButton intent="ghost" size="xs" @click="startRoles(u)">Roles</BaseButton>
                    <BaseButton
                      v-if="u.kind === 'bot'"
                      intent="ghost"
                      size="xs"
                      @click="startTokens(u)"
                    >
                      Tokens ({{ tokensOf(u).length }})
                    </BaseButton>
                    <!-- Email and password are for local accounts only. An SSO account's
                         email is the provider's and is re-synced on every sign-in. -->
                    <BaseButton
                      v-if="u.kind === 'local'"
                      intent="ghost"
                      size="xs"
                      @click="startEmail(u)"
                    >
                      Email
                    </BaseButton>
                    <BaseButton
                      v-if="u.kind === 'local'"
                      intent="ghost"
                      size="xs"
                      @click="startPassword(u)"
                    >
                      Password
                    </BaseButton>
                    <!-- Disable is amber: they lose access, and they get it back the moment you
                         click Enable. Delete is red: the account is gone. -->
                    <BaseButton
                      :intent="u.disabled ? 'secondary' : 'caution'"
                      size="xs"
                      @click="toggleDisabled(u)"
                    >
                      {{ u.disabled ? 'Enable' : 'Disable' }}
                    </BaseButton>
                    <BaseButton intent="danger" size="xs" @click="remove(u)">
                      <AppIcon name="trash" class="size-3" />
                      Delete
                    </BaseButton>
                  </div>
                </td>
              </tr>

              <!-- Role editor for this user -->
              <tr
                v-if="editingRoles?.id === u.id"
                class="border-b last:border-0"
                :style="{ borderColor: 'var(--border)', background: 'var(--surface-sunken)' }"
              >
                <td :colspan="canEdit ? 4 : 3" class="px-4 py-4">
                  <p class="mb-2 text-sm font-medium">Roles granted in Daffa</p>

                  <div v-if="roles?.length" class="mb-3">
                    <GrantPicker v-model="selectedGrants" :roles="roles" :envs="envs ?? []" />
                  </div>
                  <p v-else class="muted mb-3 text-xs">You cannot see the list of roles.</p>

                  <p v-if="u.roles.some((r) => r.source === 'oidc')" class="muted mb-3 text-xs">
                    🔒 Roles from the identity provider are not listed here. They are re-applied
                    from its claims every time this person signs in, so removing one would only
                    last until they did.
                  </p>

                  <div class="flex gap-2">
                    <BaseButton intent="primary" size="sm" :loading="busy" @click="saveRoles">
                      Save roles
                    </BaseButton>
                    <BaseButton intent="secondary" size="sm" @click="editingRoles = null">
                      Cancel
                    </BaseButton>
                  </div>
                </td>
              </tr>

              <!-- A bot's tokens: minted here, because a bot cannot sign in to mint its own -->
              <tr
                v-if="managingTokens?.id === u.id"
                class="border-b last:border-0"
                :style="{ borderColor: 'var(--border)', background: 'var(--surface-sunken)' }"
              >
                <td :colspan="canEdit ? 4 : 3" class="space-y-4 px-4 py-4">
                  <TokenReveal
                    v-if="minted"
                    :name="minted.name"
                    :token="minted.token"
                    :owner="minted.owner"
                    @done="minted = null"
                  />

                  <div v-if="tokensOf(u).length" class="overflow-x-auto">
                    <table class="w-full text-sm">
                      <tbody>
                        <tr
                          v-for="t in tokensOf(u)"
                          :key="t.id"
                          class="border-b last:border-0"
                          :style="{ borderColor: 'var(--border)' }"
                        >
                          <td class="max-w-0 py-2 pr-3">
                            <div class="font-medium">{{ t.name }}</div>
                            <div class="subtle mt-0.5 truncate font-mono text-xs">{{ t.prefix }}…</div>
                          </td>
                          <td class="py-2 pr-3 text-xs" :style="t.expired ? { color: 'var(--danger)' } : {}">
                            {{
                              t.expired
                                ? 'EXPIRED'
                                : t.expires_at
                                  ? `expires ${new Date(t.expires_at).toLocaleDateString()}`
                                  : 'does not expire'
                            }}
                          </td>
                          <td class="subtle hidden py-2 pr-3 text-right font-mono text-xs md:table-cell">
                            {{ t.last_used_at ? `used ${new Date(t.last_used_at).toLocaleString()}` : 'never used' }}
                          </td>
                          <td class="py-2 text-right">
                            <BaseButton intent="danger" size="xs" @click="revokeToken(u, t)">
                              <AppIcon name="trash" class="size-3.5" />
                            </BaseButton>
                          </td>
                        </tr>
                      </tbody>
                    </table>
                  </div>
                  <p v-else-if="!minted" class="muted text-xs">
                    No tokens yet. Mint one and put it in your CI's secret storage as
                    <code class="font-mono">DAFFA_TOKEN</code>.
                  </p>

                  <form class="flex flex-wrap items-end gap-3" @submit.prevent="mintToken">
                    <div>
                      <label :for="`tok-name-${u.id}`" class="mb-1.5 block text-sm font-medium">New token</label>
                      <input
                        :id="`tok-name-${u.id}`"
                        v-model="tokenForm.name"
                        required
                        placeholder="forgejo-ci"
                        class="field"
                        data-cursor="text"
                      />
                    </div>
                    <div>
                      <label :for="`tok-exp-${u.id}`" class="mb-1.5 block text-sm font-medium">
                        Expires after (days)
                      </label>
                      <input
                        :id="`tok-exp-${u.id}`"
                        v-model.number="tokenForm.expires_days"
                        type="number"
                        min="0"
                        class="field w-32"
                        data-cursor="text"
                      />
                    </div>
                    <BaseButton type="submit" intent="primary" size="sm" :disabled="!tokenForm.name" :loading="busy">
                      Mint token
                    </BaseButton>
                    <BaseButton intent="secondary" size="sm" @click="managingTokens = null">Close</BaseButton>
                  </form>
                  <p class="subtle text-xs">
                    0 days = never expires. Actions taken with a token are audited as
                    “{{ u.label }} (token: name)”.
                  </p>
                </td>
              </tr>

              <!-- Email, for a local account -->
              <tr
                v-if="editingEmail?.id === u.id"
                class="border-b last:border-0"
                :style="{ borderColor: 'var(--border)', background: 'var(--surface-sunken)' }"
              >
                <td :colspan="canEdit ? 4 : 3" class="px-4 py-4">
                  <form class="max-w-sm" @submit.prevent="saveEmail">
                    <label :for="`email-${u.id}`" class="mb-1.5 block text-sm font-medium">
                      Email for {{ u.label }}
                    </label>
                    <input
                      :id="`email-${u.id}`"
                      v-model="newEmail"
                      type="email"
                      autocomplete="off"
                      class="field"
                      data-cursor="text"
                    />
                    <p class="subtle mt-1 text-xs">
                      Used for notifications and to identify the account. Leave blank to clear it.
                    </p>

                    <div class="mt-3 flex gap-2">
                      <BaseButton type="submit" intent="primary" size="sm" :loading="busy">
                        Save email
                      </BaseButton>
                      <BaseButton intent="secondary" size="sm" @click="editingEmail = null">
                        Cancel
                      </BaseButton>
                    </div>
                  </form>
                </td>
              </tr>

              <!-- Password, for a local account -->
              <tr
                v-if="settingPassword?.id === u.id"
                class="border-b last:border-0"
                :style="{ borderColor: 'var(--border)', background: 'var(--surface-sunken)' }"
              >
                <td :colspan="canEdit ? 4 : 3" class="px-4 py-4">
                  <form class="max-w-sm" @submit.prevent="resetPassword">
                    <label :for="`pw-${u.id}`" class="mb-1.5 block text-sm font-medium">
                      New password for {{ u.label }}
                    </label>
                    <input
                      :id="`pw-${u.id}`"
                      v-model="newPassword"
                      type="password"
                      autocomplete="new-password"
                      class="field"
                      data-cursor="text"
                    />
                    <p class="subtle mt-1 text-xs">
                      At least 12 characters. Any session they have open keeps working.
                    </p>

                    <div class="mt-3 flex gap-2">
                      <BaseButton
                        type="submit"
                        intent="primary"
                        size="sm"
                        :disabled="!newPassword"
                        :loading="busy"
                      >
                        Set password
                      </BaseButton>
                      <BaseButton intent="secondary" size="sm" @click="settingPassword = null">
                        Cancel
                      </BaseButton>
                    </div>
                  </form>
                </td>
              </tr>
            </template>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</template>
