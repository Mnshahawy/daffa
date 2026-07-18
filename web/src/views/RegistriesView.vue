<script setup lang="ts">
import { ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { ApiError, daffa, type RegistryItem } from '@/lib/api'
import { confirm } from '@/lib/confirm'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import EmptyState from '@/components/ui/EmptyState.vue'

const qc = useQueryClient()
const error = ref('')

const { data: registries, isLoading } = useQuery({
  queryKey: ['registries'],
  queryFn: daffa.registries,
})

const form = ref({ name: '', url: '', username: '', password: '' })

// ── provider presets ──────────────────────────────────────────────────────────
//
// The same cards as the git credentials view, for the same reason: the friction is not the
// password, it is knowing WHICH host to type (docker.io? index.docker.io? registry-1?) and
// which of a provider's several token kinds the registry actually accepts. A card knows
// both, so picking one fills the host and says where the right token comes from.
type RegistryProvider = {
  id: string
  label: string
  host: string // '' ⇒ self-hosted, host typed by hand
  usernameHint: string
  tokenUrl: string
  tokenHint: string
}

const registryProviders: RegistryProvider[] = [
  { id: 'dockerhub', label: 'Docker Hub', host: 'docker.io', usernameHint: 'your Docker ID', tokenUrl: 'https://app.docker.com/settings/personal-access-tokens', tokenHint: 'Account settings → Personal access tokens. Read-only access is enough for pulls; your account password also works, but a token can be revoked on its own.' },
  { id: 'ghcr', label: 'GHCR', host: 'ghcr.io', usernameHint: 'your GitHub username', tokenUrl: 'https://github.com/settings/tokens', tokenHint: 'A classic personal access token with the `read:packages` scope — fine-grained tokens do not work against ghcr.io.' },
  { id: 'gitlab', label: 'GitLab', host: 'registry.gitlab.com', usernameHint: 'username or token name', tokenUrl: 'https://gitlab.com/-/user_settings/personal_access_tokens', tokenHint: 'A personal access token with `read_registry` — or a project deploy token, in which case the username is the deploy token’s own name.' },
  { id: 'selfhosted', label: 'Self-hosted', host: '', usernameHint: '', tokenUrl: '', tokenHint: 'Harbor, a Forgejo/Gitea registry, or a plain registry:2 — whatever `docker login` takes there works here. Type the bare host; add an http:// prefix only for a plain-HTTP registry. A registry fronted by a certificate from a CA Daffa manages is trusted automatically.' },
]

const chosenProvider = ref<RegistryProvider | null>(null)

function pickProvider(p: RegistryProvider) {
  chosenProvider.value = chosenProvider.value?.id === p.id ? null : p
  if (!chosenProvider.value) return
  // Fill, don't overwrite: a host or name someone already typed is theirs.
  if (p.host && !form.value.url) form.value.url = p.host
  if (!form.value.name) form.value.name = p.id === 'selfhosted' ? '' : p.id
}

const create = useMutation({
  // verify:true probes the registry first; verify:false stores the credential without probing —
  // used for "save anyway" after an advisory unreachable, since the deploy pull runs from the host
  // daemon, not from Daffa.
  mutationFn: (verify: boolean) => daffa.createRegistry({ ...form.value, verify }),
  onSuccess: async (resp) => {
    if (resp.unreachable) {
      const ok = await confirm({
        title: 'Daffa could not reach this registry',
        body: `${resp.reason} — but deploys pull from the host daemon, not from Daffa, so a registry Daffa cannot reach from here may still work at deploy time. Save the credential anyway?`,
        confirmLabel: 'Save anyway',
      })
      if (ok) create.mutate(false)
      return
    }
    form.value = { name: '', url: '', username: '', password: '' }
    chosenProvider.value = null
    error.value = ''
    qc.invalidateQueries({ queryKey: ['registries'] })
  },
  onError: (e) => {
    error.value = e instanceof ApiError ? e.message : 'Could not save the registry.'
  },
})

const remove = useMutation({
  mutationFn: (id: string) => daffa.deleteRegistry(id),
  onSettled: () => qc.invalidateQueries({ queryKey: ['registries'] }),
  onError: (e) => {
    error.value = e instanceof ApiError ? e.message : 'Could not delete the registry.'
  },
})

async function onRemove(r: RegistryItem) {
  const ok = await confirm({
    title: `Delete the credential for ${r.url}?`,
    body: 'Stacks that pull from it will pull anonymously from then on, which fails for a private image. The password is encrypted at rest and cannot be read back, so it has to be re-entered.',
    confirmLabel: 'Delete',
    intent: 'danger',
  })
  if (!ok) return
  remove.mutate(r.id)
}
</script>

<template>
  <div>
    <div class="mb-5 flex flex-wrap items-center gap-x-3 gap-y-2">
      <div class="min-w-0">
        <h2 class="text-base font-semibold">Registries</h2>
        <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
          Credentials for pulling private images. The password is encrypted at rest and is only
          ever written inside the ephemeral container that runs the deploy.
        </p>
      </div>
    </div>

    <form
      class="surface mb-6 rounded-[var(--radius-card)] p-5"
      @submit.prevent="create.mutate(true)"
    >
      <!-- Provider cards. Picking one fills the host — the part people guess wrong — and
           points at the token kind the registry actually accepts. -->
      <div class="mb-4">
        <div class="eyebrow mb-1.5">Provider</div>
        <div class="grid grid-cols-2 gap-2 sm:grid-cols-4">
          <button
            v-for="p in registryProviders"
            :key="p.id"
            type="button"
            class="rounded-[var(--radius-control)] border px-3 py-2 text-sm font-medium transition"
            :style="
              chosenProvider?.id === p.id
                ? { borderColor: 'var(--accent)', background: 'var(--accent-soft)', color: 'var(--accent-text)' }
                : { borderColor: 'var(--border)' }
            "
            @click="pickProvider(p)"
          >
            {{ p.label }}
          </button>
        </div>
      </div>

      <div class="grid gap-4 sm:grid-cols-4">
        <div>
          <label for="r-name" class="mb-1.5 block text-sm font-medium">Name</label>
          <input
            id="r-name"
            v-model="form.name"
            required
            placeholder="ghcr"
            class="field"
            data-cursor="text"
          />
        </div>
        <div>
          <label for="r-url" class="mb-1.5 block text-sm font-medium">Registry host</label>
          <input
            id="r-url"
            v-model="form.url"
            required
            placeholder="ghcr.io"
            class="field font-mono text-xs"
            data-cursor="text"
          />
        </div>
        <div>
          <label for="r-user" class="mb-1.5 block text-sm font-medium">
            Username <span class="subtle font-normal">(optional)</span>
          </label>
          <input
            id="r-user"
            v-model="form.username"
            :placeholder="chosenProvider?.usernameHint || undefined"
            class="field"
            data-cursor="text"
          />
          <p class="subtle mt-1 text-xs">
            Leave empty if the credential is a bearer token — it is then sent as a token rather than
            a username/password pair.
          </p>
        </div>
        <div>
          <label for="r-pass" class="mb-1.5 block text-sm font-medium">Password or token</label>
          <input id="r-pass" v-model="form.password" type="password" class="field" />
        </div>
      </div>

      <p v-if="chosenProvider" class="subtle mt-3 text-xs leading-relaxed">
        {{ chosenProvider.tokenHint }}
        <a
          v-if="chosenProvider.tokenUrl"
          :href="chosenProvider.tokenUrl"
          target="_blank"
          rel="noopener"
          class="underline transition hover:text-[var(--accent-text)]"
        >
          Create one ↗
        </a>
      </p>

      <p v-if="error" class="mt-3 text-sm" :style="{ color: 'var(--danger)' }">{{ error }}</p>

      <BaseButton
        type="submit"
        intent="primary"
        size="md"
        class="mt-4"
        :loading="create.isPending.value"
      >
        {{ create.isPending.value ? 'Signing in to the registry…' : 'Add registry' }}
      </BaseButton>
      <p class="subtle mt-2 text-xs">
        Daffa signs in to the registry before saving, so a wrong password is caught now rather than
        by a deploy failing to pull a private image. If Daffa cannot reach the registry from here,
        you can still save — the deploy pull runs from the host daemon, not from Daffa.
      </p>
    </form>

    <p v-if="isLoading" class="muted text-sm">Loading…</p>

    <EmptyState
      v-else-if="!registries?.length"
      icon="disc"
      title="No registry credentials yet"
      body="Daffa pulls anonymously, which is all a public image needs. Add a registry here and every stack that pulls from that host is authenticated — the password never leaves the deploy container."
    />

    <div v-else class="surface overflow-hidden rounded-[var(--radius-card)]">
      <table class="w-full text-sm">
        <thead>
          <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
            <th class="eyebrow px-4 py-2 text-left font-medium">Name</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Registry</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Username</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">Actions</th>
          </tr>
        </thead>

        <tbody>
          <tr
            v-for="r in registries"
            :key="r.id"
            class="border-b transition last:border-0 hover:bg-[var(--surface-sunken)]"
            :style="{ borderColor: 'var(--border)' }"
          >
            <td class="py-3 pl-4 pr-4 font-medium">{{ r.name }}</td>
            <td class="subtle py-3 pr-4 font-mono text-xs">{{ r.url }}</td>
            <td class="subtle py-3 pr-4 text-xs">{{ r.username || '—' }}</td>
            <td class="py-3 pr-4 text-right">
              <BaseButton
                intent="danger"
                size="xs"
                :disabled="remove.isPending.value"
                @click="onRemove(r)"
              >
                <AppIcon name="trash" class="size-3.5" />
                Delete
              </BaseButton>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
