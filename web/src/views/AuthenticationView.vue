<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useQuery, useQueryClient } from '@tanstack/vue-query'
import { ApiError, daffa, type Provider } from '@/lib/api'
import { Cap } from '@/lib/caps'
import { confirm } from '@/lib/confirm'
import { toast } from '@/lib/toast'
import { useSession } from '@/stores/session'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import EmptyState from '@/components/ui/EmptyState.vue'
import Select from '@/components/ui/Select.vue'
import StatusPill from '@/components/ui/StatusPill.vue'

const session = useSession()
const qc = useQueryClient()

const canEdit = computed(() => session.can(Cap.SettingsEdit))

const { data: providers } = useQuery({ queryKey: ['providers'], queryFn: daffa.providers })
const { data: roles } = useQuery({
  queryKey: ['roles'],
  queryFn: daffa.roles,
  enabled: computed(() => session.can(Cap.RolesView)),
})
const { data: envs } = useQuery({ queryKey: ['environments'], queryFn: daffa.environments })

// The role chosen for a new mapping, so the host selector can be hidden for a role that
// cannot be limited to one.
const mappingRole = computed(() => roles.value?.find((r) => r.id === newMapping.value.role_id))

const busy = ref(false)
const editing = ref<Provider | null>(null)
const creating = ref(false)
const testResult = ref<{ id: string; ok: boolean; message: string } | null>(null)
const testing = ref<string | null>(null)

const blank = () => ({
  slug: '',
  name: '',
  issuer: '',
  client_id: '',
  client_secret: '',
  redirect_url: '',
  scopes: 'openid profile email',
  roles_claim: 'groups',
  default_role_id: '',
  enabled: true,
})
const form = ref(blank())

// The redirect URL has to be registered with the provider verbatim, so show the exact
// string rather than leaving someone to assemble it and get it subtly wrong.
const suggestedRedirect = computed(() =>
  form.value.slug ? `${location.origin}/api/auth/callback/${form.value.slug}` : '',
)

// Keep it in step with the slug while it has not been hand-edited, which is the common
// case and saves a copy-paste that is easy to fumble.
watch(suggestedRedirect, (next, prev) => {
  if (!form.value.redirect_url || form.value.redirect_url === prev) {
    form.value.redirect_url = next
  }
})

// ── provider presets ──────────────────────────────────────────────────────────
//
// The flat form is ten fields a first-time admin has to know by heart: the issuer URL, the exact
// scopes, and — the one everybody gets wrong — the NAME of the claim that carries group
// membership, which is different for every provider (Okta `groups`, Entra `roles`). A preset fills
// those three from what the provider documents, so the only things left to type are the client id
// and secret, which are the only things that are actually yours.
//
// `<…>` marks the tenant-specific part the admin must replace; the note says which. Presets are a
// starting point, not a lock — every field stays editable.
type Preset = {
  id: string
  label: string
  name: string
  issuer: string
  scopes: string
  roles_claim: string
  note: string
}

const presets: Preset[] = [
  {
    id: 'google', label: 'Google', name: 'Google',
    issuer: 'https://accounts.google.com',
    scopes: 'openid email profile', roles_claim: '',
    note: 'Groups are not in the token by default. Leave the roles claim empty and everyone maps to the default role, or wire up Workspace group claims first.',
  },
  {
    id: 'entra', label: 'Microsoft Entra', name: 'Microsoft',
    issuer: 'https://login.microsoftonline.com/<tenant-id>/v2.0',
    scopes: 'openid email profile', roles_claim: 'roles',
    note: 'Replace <tenant-id> with your directory (tenant) ID. App roles arrive in the “roles” claim; switch to “groups” if you emit group IDs instead.',
  },
  {
    id: 'okta', label: 'Okta', name: 'Okta',
    issuer: 'https://<your-domain>.okta.com',
    scopes: 'openid email profile groups', roles_claim: 'groups',
    note: 'Replace <your-domain>. Add a “groups” claim to the ID token in the Okta app’s OpenID Connect settings.',
  },
  {
    id: 'auth0', label: 'Auth0', name: 'Auth0',
    issuer: 'https://<tenant>.auth0.com/',
    scopes: 'openid email profile', roles_claim: '',
    note: 'Replace <tenant> (keep the trailing slash). Roles need a custom claim added by an Auth0 Action; put that claim’s full name in the roles field.',
  },
  {
    id: 'keycloak', label: 'Keycloak', name: 'Keycloak',
    issuer: 'https://<host>/realms/<realm>',
    scopes: 'openid email profile', roles_claim: 'groups',
    note: 'Replace <host> and <realm>. Add a group-membership mapper to the client so the “groups” claim is present.',
  },
  {
    id: 'generic', label: 'Generic OIDC', name: '',
    issuer: '', scopes: 'openid profile email', roles_claim: 'groups',
    note: 'Any standards-compliant OIDC provider. Everything is discovered from its /.well-known/openid-configuration.',
  },
]

const chosenPreset = ref<string | null>(null)
const activePreset = computed(() => presets.find((p) => p.id === chosenPreset.value) ?? null)

// What the LAST preset suggested for name/slug. Switching Google→Okta has to move those from
// "Google"/"google" to "Okta"/"okta"; without remembering the previous suggestion the only options
// are to clobber a name the user typed on purpose, or (the bug this fixes) to leave Google's name
// sitting under an Okta selection. So: follow the suggestion until it has been hand-edited, exactly
// as the redirect URL follows the slug.
const presetSuggestion = ref({ name: '', slug: '' })

function slugFor(p: Preset): string {
  return p.id === 'generic' ? '' : p.id
}

function applyPreset(p: Preset) {
  chosenPreset.value = p.id

  // The provider-shaped fields are the whole reason to pick a preset, so they are always taken —
  // switching provider means switching issuer, scopes and the roles claim.
  form.value.issuer = p.issuer
  form.value.scopes = p.scopes
  form.value.roles_claim = p.roles_claim

  // name/slug are the user's to own. Overwrite only when they are still empty or still hold the
  // PREVIOUS preset's suggestion — i.e. the user has not typed their own.
  if (!form.value.name || form.value.name === presetSuggestion.value.name) {
    form.value.name = p.name
  }
  if (!form.value.slug || form.value.slug === presetSuggestion.value.slug) {
    form.value.slug = slugFor(p)
  }
  presetSuggestion.value = { name: p.name, slug: slugFor(p) }
}

function startCreate() {
  creating.value = true
  editing.value = null
  chosenPreset.value = null
  presetSuggestion.value = { name: '', slug: '' }
  form.value = blank()
}

function startEdit(p: Provider) {
  creating.value = false
  editing.value = p
  form.value = { ...p, client_secret: '' } // blank means "keep the stored one"
}

function cancel() {
  creating.value = false
  editing.value = null
}

async function save() {
  busy.value = true
  try {
    if (editing.value) {
      await daffa.updateProvider(editing.value.id, form.value)
    } else {
      await daffa.createProvider(form.value)
    }
    await qc.invalidateQueries({ queryKey: ['providers'] })
    cancel()
    toast.ok('Provider saved.')
  } catch (e) {
    toast.err(e, 'Could not save the provider.')
  } finally {
    busy.value = false
  }
}

async function test(p: Provider) {
  testResult.value = null
  testing.value = p.id
  try {
    const r = await daffa.testProvider(p.id)
    testResult.value = { id: p.id, ok: r.ok, message: r.ok ? (r.message ?? 'OK') : (r.error ?? 'Failed') }
  } catch (e) {
    // The whole point of Test is to find out what is broken. An unreachable issuer is exactly
    // the failure it exists to report, so it has to land in the result rather than as an
    // unhandled rejection in the console.
    testResult.value = {
      id: p.id,
      ok: false,
      message: e instanceof ApiError ? e.message : 'Could not reach the provider.',
    }
  } finally {
    testing.value = null
  }
}

async function remove(p: Provider) {
  const ok = await confirm({
    title: `Delete the provider “${p.name}”?`,
    body:
      'People who sign in through it will no longer be able to. Their accounts and audit history are kept — they simply have no way in until you give them one. The client secret is sealed and cannot be read back, so it has to be re-entered if you add this provider again.',
    confirmLabel: 'Delete provider',
    intent: 'danger',
  })
  if (!ok) return

  try {
    await daffa.deleteProvider(p.id)
    await qc.invalidateQueries({ queryKey: ['providers'] })
    toast.ok('Provider removed.')
  } catch (e) {
    toast.err(e, 'Could not remove the provider.')
  }
}

// ── mappings ──────────────────────────────────────────────────────────────────
const expanded = ref<string | null>(null)
const newMapping = ref({ claim_value: '', role_id: '', env_id: '' })

// The key holds the ref itself, NOT `() => expanded.value`. vue-query unwraps a ref in a key and
// tracks it; it does not call a bare function, so the old form produced a key that never changed —
// every provider shared one cache entry, and expanding the second showed the first one's mappings.
const { data: mappings, refetch: refetchMappings } = useQuery({
  queryKey: ['mappings', expanded],
  queryFn: () => daffa.mappings(expanded.value!),
  enabled: computed(() => !!expanded.value),
})

async function addMapping() {
  if (!expanded.value || !newMapping.value.claim_value || !newMapping.value.role_id) return
  try {
    await daffa.createMapping(expanded.value, newMapping.value)
    newMapping.value = { claim_value: '', role_id: '', env_id: '' }
    await refetchMappings()
    toast.ok('Mapping added.')
  } catch (e) {
    toast.err(e, 'Could not add the mapping.')
  }
}

async function removeMapping(id: string) {
  if (!expanded.value) return

  // One row here, and a whole group loses a role the next time any of them signs in. That is
  // worth a sentence before it happens, even though it is easy enough to add back.
  const m = mappings.value?.find((x) => x.ID === id)
  const ok = await confirm({
    title: m ? `Delete the mapping for “${m.ClaimValue}”?` : 'Delete this mapping?',
    body: m
      ? `Everyone whose claim carries ${m.ClaimValue} stops receiving ${m.RoleName || roleName(m.RoleID)} the next time they sign in. Anyone signed in right now keeps it until their session ends.`
      : 'The group it maps stops receiving its role at the next sign-in.',
    confirmLabel: 'Delete mapping',
    intent: 'danger',
  })
  if (!ok) return

  await daffa.deleteMapping(expanded.value, id)
  await refetchMappings()
  toast.ok('Mapping deleted.')
}

function roleName(id: string): string {
  return roles.value?.find((r) => r.id === id)?.name ?? id
}
</script>

<template>
  <div>
    <!-- A settings sub-page sits under the Settings title, so it gets a section heading rather
         than a second <h1> competing with it. -->
    <div class="mb-5 flex flex-wrap items-start gap-x-3 gap-y-2">
      <div class="min-w-0">
        <h2 class="text-base font-semibold">Authentication</h2>
        <p class="muted mt-0.5 text-sm">
          Identity providers are configured here, not in the environment, and there may be more
          than one. Client secrets are encrypted and cannot be read back.
        </p>
      </div>

      <div class="ml-auto shrink-0">
        <BaseButton v-if="canEdit && !creating && !editing" intent="primary" @click="startCreate">
          <AppIcon name="plus" class="size-4" />
          Add provider
        </BaseButton>
      </div>
    </div>

    <!-- Editor -->
    <form
      v-if="creating || editing"
      class="surface mb-6 rounded-[var(--radius-card)] p-5"
      @submit.prevent="save"
    >
      <p class="mb-4 text-sm font-medium">
        {{ editing ? `Edit “${editing.name}”` : 'New identity provider' }}
      </p>

      <!-- Preset cards. Only when creating: an edit already has its provider chosen, and offering
           to re-template a live provider is a good way to wipe a working config by accident. -->
      <div v-if="!editing" class="mb-4">
        <div class="eyebrow mb-1.5">Start from a provider</div>
        <div class="grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-6">
          <button
            v-for="p in presets"
            :key="p.id"
            type="button"
            class="rounded-[var(--radius-control)] border px-3 py-2 text-left text-sm font-medium transition"
            :style="
              chosenPreset === p.id
                ? { borderColor: 'var(--accent)', background: 'var(--accent-soft)', color: 'var(--accent-text)' }
                : { borderColor: 'var(--border)' }
            "
            @click="applyPreset(p)"
          >
            {{ p.label }}
          </button>
        </div>
        <p v-if="activePreset" class="muted mt-2 max-w-2xl text-xs leading-relaxed">
          {{ activePreset.note }}
        </p>
      </div>

      <div class="grid gap-4 sm:grid-cols-2">
        <div>
          <label for="p-name" class="mb-1.5 block text-sm font-medium">Display name</label>
          <input
            id="p-name"
            v-model="form.name"
            placeholder="Company SSO"
            class="field"
            data-cursor="text"
          />
          <p class="subtle mt-1 text-xs">What the sign-in button says.</p>
        </div>

        <div>
          <label for="p-slug" class="mb-1.5 block text-sm font-medium">Slug</label>
          <input
            id="p-slug"
            v-model="form.slug"
            placeholder="company"
            class="field font-mono text-xs"
            data-cursor="text"
          />
          <p class="subtle mt-1 text-xs">Lowercase. It appears in the sign-in URL.</p>
        </div>

        <div class="sm:col-span-2">
          <label for="p-issuer" class="mb-1.5 block text-sm font-medium">Issuer</label>
          <input
            id="p-issuer"
            v-model="form.issuer"
            placeholder="https://auth.example.com"
            class="field font-mono text-xs"
            data-cursor="text"
          />
          <p class="subtle mt-1 text-xs">
            Everything else is discovered from
            <code class="font-mono">/.well-known/openid-configuration</code>.
          </p>
        </div>

        <div>
          <label for="p-client" class="mb-1.5 block text-sm font-medium">Client ID</label>
          <input
            id="p-client"
            v-model="form.client_id"
            class="field font-mono text-xs"
            data-cursor="text"
          />
        </div>

        <div>
          <label for="p-secret" class="mb-1.5 block text-sm font-medium">Client secret</label>
          <input
            id="p-secret"
            v-model="form.client_secret"
            type="password"
            autocomplete="new-password"
            :placeholder="editing?.has_secret ? '•••••••• (unchanged)' : ''"
            class="field font-mono text-xs"
            data-cursor="text"
          />
          <p v-if="editing?.has_secret" class="subtle mt-1 text-xs">
            Leave blank to keep the current one.
          </p>
        </div>

        <div class="sm:col-span-2">
          <label for="p-redirect" class="mb-1.5 block text-sm font-medium">Redirect URL</label>
          <input
            id="p-redirect"
            v-model="form.redirect_url"
            class="field font-mono text-xs"
            data-cursor="text"
          />
          <p class="subtle mt-1 text-xs">Register this exact string with the provider.</p>
        </div>

        <div>
          <label for="p-scopes" class="mb-1.5 block text-sm font-medium">Scopes</label>
          <input
            id="p-scopes"
            v-model="form.scopes"
            class="field font-mono text-xs"
            data-cursor="text"
          />
          <p class="subtle mt-1 text-xs">Space-separated, as the spec says.</p>
        </div>

        <div>
          <label for="p-claim" class="mb-1.5 block text-sm font-medium">Roles claim</label>
          <input
            id="p-claim"
            v-model="form.roles_claim"
            placeholder="groups"
            class="field font-mono text-xs"
            data-cursor="text"
          />
          <p class="subtle mt-1 text-xs">
            The claim holding group names. Map them to roles below.
          </p>
        </div>

        <div class="sm:col-span-2">
          <label for="p-default" class="mb-1.5 block text-sm font-medium">
            Role for unmapped users
          </label>
          <Select id="p-default" v-model="form.default_role_id">
            <option value="">Refuse them (recommended)</option>
            <option v-for="r in roles" :key="r.id" :value="r.id">{{ r.name }}</option>
          </Select>
          <p class="subtle mt-1 max-w-2xl text-xs">
            What someone gets when their claims match no mapping. Refusing is the safe default:
            signing them in with no capabilities would show them an empty application, which
            reads as a bug rather than as a decision.
          </p>
        </div>

        <label for="p-enabled" class="flex items-center gap-2 text-sm sm:col-span-2">
          <input
            id="p-enabled"
            v-model="form.enabled"
            type="checkbox"
            class="accent-[var(--accent)]"
          />
          Enabled — show a sign-in button for this provider
        </label>
      </div>

      <div class="mt-5 flex gap-2">
        <BaseButton type="submit" intent="primary" size="md" :loading="busy">
          Save provider
        </BaseButton>
        <BaseButton intent="secondary" size="md" @click="cancel">Cancel</BaseButton>
      </div>
    </form>

    <!-- List -->
    <EmptyState
      v-if="!providers?.length && !creating"
      icon="plug"
      title="No identity providers"
      body="Everyone signs in with a username and password. Add an OpenID Connect provider and Daffa will discover its endpoints from the issuer, then map the groups in its token onto roles here."
    >
      <template #action>
        <BaseButton v-if="canEdit" intent="primary" size="md" @click="startCreate">
          <AppIcon name="plus" class="size-4" />
          Add provider
        </BaseButton>
      </template>
    </EmptyState>

    <div v-else-if="providers?.length" class="surface overflow-hidden rounded-[var(--radius-card)]">
      <div class="overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
              <th class="eyebrow px-4 py-2 text-left font-medium">Status</th>
              <th class="eyebrow py-2 pr-4 text-left font-medium">Provider</th>
              <th class="eyebrow hidden py-2 pr-4 text-left font-medium lg:table-cell">Client</th>
              <th class="eyebrow py-2 pr-4 text-right font-medium">Actions</th>
            </tr>
          </thead>

          <tbody>
            <template v-for="p in providers" :key="p.id">
              <tr
                class="border-b transition last:border-0 hover:bg-[var(--surface-sunken)]"
                :style="{ borderColor: 'var(--border)' }"
              >
                <td class="px-4 py-3 align-top">
                  <StatusPill
                    :status="
                      p.enabled
                        ? { tone: 'success', label: 'Enabled' }
                        : { tone: 'neutral', label: 'Disabled' }
                    "
                  />
                </td>

                <td class="py-3 pr-4 align-top">
                  <div class="font-medium">{{ p.name }}</div>
                  <div class="subtle mt-0.5 truncate font-mono text-xs">{{ p.issuer }}</div>

                  <!-- The result of Test. The only reason anybody presses it. -->
                  <p
                    v-if="testResult?.id === p.id"
                    class="mt-2 text-xs"
                    :style="{ color: testResult.ok ? 'var(--success)' : 'var(--danger)' }"
                  >
                    {{ testResult.message }}
                  </p>
                </td>

                <td class="hidden py-3 pr-4 align-top text-xs lg:table-cell">
                  <div class="font-mono">{{ p.client_id }}</div>
                  <div class="mt-0.5">
                    <template v-if="p.has_secret">
                      <span class="subtle">secret set</span>
                    </template>
                    <!-- Was a <template class="text-amber-600">, which styles nothing: a
                         <template> is not an element and never reaches the DOM. The one thing on
                         this row that should have been shouting was rendering as plain text. -->
                    <span v-else :style="{ color: 'var(--warn)' }">no secret</span>
                    <template v-if="p.roles_claim">
                      <span class="subtle"> · claim </span>
                      <code class="font-mono">{{ p.roles_claim }}</code>
                    </template>
                  </div>
                </td>

                <td class="py-3 pr-4 align-top">
                  <div class="flex items-center justify-end gap-1">
                    <BaseButton
                      intent="ghost"
                      size="xs"
                      @click="expanded = expanded === p.id ? null : p.id"
                    >
                      <AppIcon
                        name="chevronRight"
                        class="size-3 transition-transform"
                        :class="expanded === p.id ? 'rotate-90' : ''"
                      />
                      Role mapping
                    </BaseButton>
                    <template v-if="canEdit">
                      <BaseButton
                        intent="secondary"
                        size="xs"
                        :loading="testing === p.id"
                        @click="test(p)"
                      >
                        Test
                      </BaseButton>
                      <BaseButton intent="secondary" size="xs" @click="startEdit(p)">
                        <AppIcon name="pencil" class="size-3" />
                        Edit
                      </BaseButton>
                      <BaseButton intent="danger" size="xs" @click="remove(p)">
                        <AppIcon name="trash" class="size-3" />
                        Delete
                      </BaseButton>
                    </template>
                  </div>
                </td>
              </tr>

              <!-- Claim → role mappings -->
              <tr
                v-if="expanded === p.id"
                class="border-b last:border-0"
                :style="{ borderColor: 'var(--border)', background: 'var(--surface-sunken)' }"
              >
                <td colspan="4" class="px-4 py-4">
                  <p class="text-sm font-medium">
                    Map <code class="font-mono">{{ p.roles_claim || 'the roles claim' }}</code> to
                    roles
                  </p>
                  <p class="muted mt-0.5 mb-3 max-w-2xl text-xs">
                    Someone in several mapped groups gets <strong>all</strong> of those roles —
                    the capabilities add up. There is no “highest” role.
                  </p>

                  <table v-if="mappings?.length" class="mb-3 w-full text-sm">
                    <thead>
                      <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
                        <th class="eyebrow py-1.5 text-left font-medium">Claim value</th>
                        <th class="eyebrow py-1.5 text-left font-medium">Role granted</th>
                        <th v-if="canEdit" class="eyebrow py-1.5 text-right font-medium">
                          Actions
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      <tr
                        v-for="m in mappings"
                        :key="m.ID"
                        class="border-b last:border-0"
                        :style="{ borderColor: 'var(--border)' }"
                      >
                        <td class="py-2 pr-4 font-mono text-xs">{{ m.ClaimValue }}</td>
                        <td class="py-2 pr-4">
                          {{ m.RoleName || roleName(m.RoleID) }}
                          <span v-if="m.EnvName" class="muted">on {{ m.EnvName }}</span>
                        </td>
                        <td v-if="canEdit" class="py-2 text-right">
                          <BaseButton intent="danger" size="xs" @click="removeMapping(m.ID)">
                            Delete
                          </BaseButton>
                        </td>
                      </tr>
                    </tbody>
                  </table>
                  <p v-else class="muted mb-3 text-xs">
                    No mappings. Everyone signing in here gets
                    <template v-if="p.default_role_id">the default role.</template>
                    <template v-else>
                      <strong>refused</strong> — set a default role, or map at least one group.
                    </template>
                  </p>

                  <form v-if="canEdit" class="flex flex-wrap items-end gap-2" @submit.prevent="addMapping">
                    <div class="min-w-48 flex-1">
                      <label :for="`m-claim-${p.id}`" class="eyebrow mb-1 block">Claim value</label>
                      <input
                        :id="`m-claim-${p.id}`"
                        v-model="newMapping.claim_value"
                        placeholder="ops-admins"
                        class="field font-mono text-xs"
                        data-cursor="text"
                      />
                    </div>

                    <div class="w-44">
                      <label :for="`m-role-${p.id}`" class="eyebrow mb-1 block">Role</label>
                      <Select :id="`m-role-${p.id}`" v-model="newMapping.role_id">
                        <option value="">Choose a role…</option>
                        <option v-for="r in roles" :key="r.id" :value="r.id">{{ r.name }}</option>
                      </Select>
                    </div>

                    <!-- A role that administers Daffa itself cannot be limited to one host, so it
                         is not offered one — rather than offering it and having the server refuse. -->
                    <div v-if="mappingRole?.scopable" class="w-40">
                      <label :for="`m-env-${p.id}`" class="eyebrow mb-1 block">Scope</label>
                      <Select :id="`m-env-${p.id}`" v-model="newMapping.env_id">
                        <option value="">Everywhere</option>
                        <option v-for="e in envs" :key="e.id" :value="e.id">on {{ e.name }}</option>
                      </Select>
                    </div>

                    <BaseButton
                      type="submit"
                      intent="primary"
                      size="md"
                      :disabled="!newMapping.claim_value || !newMapping.role_id"
                    >
                      Add mapping
                    </BaseButton>
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
