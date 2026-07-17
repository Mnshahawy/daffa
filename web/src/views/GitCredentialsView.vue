<script setup lang="ts">
import { computed, ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { ApiError, daffa, type GitCredential } from '@/lib/api'
import { confirm } from '@/lib/confirm'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import EmptyState from '@/components/ui/EmptyState.vue'

const qc = useQueryClient()
const error = ref('')
const adding = ref(false)

const { data: creds, isLoading } = useQuery({
  queryKey: ['gitcreds'],
  queryFn: daffa.gitCredentials,
})

const blank = () => ({
  name: '',
  kind: 'token' as 'token' | 'ssh',
  username: '',
  token: '',
  ssh_key: '',
  passphrase: '',
  host_key: '',
})
const form = ref(blank())

// ── provider presets ──────────────────────────────────────────────────────────
//
// The friction in adding a git credential is not the token — it is the SSH host key. Pinning one
// means a substituted git server is refused rather than handed a deploy, but getting it means
// running `ssh-keyscan` by hand, so most people skip it and clone over an unverified connection.
//
// A provider card removes that step: it knows the host, so it can FETCH the keys and pin them for
// you. For github.com the keys come back authenticated (from GitHub's published metadata); for the
// others it is the same trust-on-first-use scan you would run yourself, but done, not skipped.
type GitProvider = {
  id: string
  label: string
  host: string // '' ⇒ self-hosted, host typed by hand
  tokenUrl: string
  tokenHint: string
}

const gitProviders: GitProvider[] = [
  { id: 'github', label: 'GitHub', host: 'github.com', tokenUrl: 'https://github.com/settings/tokens', tokenHint: 'Settings → Developer settings → Personal access tokens. A classic token with `repo` scope, or a fine-grained one with Contents: read.' },
  { id: 'gitlab', label: 'GitLab', host: 'gitlab.com', tokenUrl: 'https://gitlab.com/-/user_settings/personal_access_tokens', tokenHint: 'A personal (or project) access token with the `read_repository` scope.' },
  { id: 'bitbucket', label: 'Bitbucket', host: 'bitbucket.org', tokenUrl: 'https://bitbucket.org/account/settings/app-passwords/', tokenHint: 'An app password with Repositories: read. Use your username with it.' },
  { id: 'selfhosted', label: 'Self-hosted', host: '', tokenUrl: '', tokenHint: 'Gitea, Forgejo, or any git server. Enter its host to fetch SSH keys.' },
]

const chosenProvider = ref<GitProvider | null>(null)
const selfHost = ref('')
const keyStatus = ref<{ ok: boolean; verified: boolean; message: string } | null>(null)
const fetchingKeys = ref(false)

function pickProvider(p: GitProvider) {
  chosenProvider.value = chosenProvider.value?.id === p.id ? null : p
  keyStatus.value = null
  if (!chosenProvider.value) return
  if (!form.value.name) form.value.name = p.id === 'selfhosted' ? '' : `${p.label} deploy`
  // A hosted provider knows its host, so pin its keys right away when the SSH path is in view.
  if (p.host && form.value.kind === 'ssh') fetchHostKeys(p.host)
}

async function fetchHostKeys(host: string) {
  if (!host) return
  fetchingKeys.value = true
  keyStatus.value = null
  try {
    const r = await daffa.discoverGitHostKeys(host)
    form.value.host_key = r.known_hosts
    keyStatus.value = {
      ok: true,
      verified: r.verified,
      message: r.verified
        ? `Verified keys from ${host} pinned.`
        : `Keys scanned from ${host} and pinned — trust on first use, so confirm they match if the host is sensitive.`,
    }
  } catch (e) {
    keyStatus.value = {
      ok: false,
      verified: false,
      message: e instanceof ApiError ? e.message : `Could not reach ${host}.`,
    }
  } finally {
    fetchingKeys.value = false
  }
}

// The host to fetch for: a hosted provider's own host, or whatever the self-hosted field holds.
const keyHost = computed(() => chosenProvider.value?.host || selfHost.value.trim())

const create = useMutation({
  mutationFn: () => daffa.createGitCredential(form.value),
  onSuccess: () => {
    form.value = blank()
    adding.value = false
    chosenProvider.value = null
    selfHost.value = ''
    keyStatus.value = null
    error.value = ''
    qc.invalidateQueries({ queryKey: ['gitcreds'] })
  },
  onError: (e) => {
    error.value = e instanceof ApiError ? e.message : 'Could not save the credential.'
  },
})

const remove = useMutation({
  mutationFn: (id: string) => daffa.deleteGitCredential(id),
  onSettled: () => qc.invalidateQueries({ queryKey: ['gitcreds'] }),
  onError: (e) => {
    error.value = e instanceof ApiError ? e.message : 'Could not delete the credential.'
  },
})

async function onRemove(c: GitCredential) {
  if (c.in_use > 0) {
    error.value = `${c.name} is used by ${c.in_use} stack${c.in_use === 1 ? '' : 's'}. Point them elsewhere first.`
    return
  }
  const ok = await confirm({
    title: `Delete the git credential ${c.name}?`,
    body:
      c.kind === 'ssh'
        ? 'Daffa forgets the private key. It is not stored anywhere else and cannot be recovered — you would have to generate a new key and add it to the repository again.'
        : 'Daffa forgets the token. It is not stored anywhere else and cannot be recovered — you would have to issue a new one.',
    confirmLabel: 'Delete',
    intent: 'danger',
  })
  if (!ok) return
  remove.mutate(c.id)
}
</script>

<template>
  <div>
    <div class="mb-5 flex flex-wrap items-center gap-x-3 gap-y-2">
      <div class="min-w-0">
        <h2 class="text-base font-semibold">Git credentials</h2>
        <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
          How Daffa authenticates to your git server when it pulls a stack's compose file.
          Configure one here and pick it when you create a stack — a token pasted into each stack
          is a token you cannot rotate.
        </p>
      </div>

      <div class="ml-auto">
        <BaseButton :intent="adding ? 'secondary' : 'primary'" @click="adding = !adding">
          <AppIcon v-if="!adding" name="plus" class="size-4" />
          {{ adding ? 'Cancel' : 'Add credential' }}
        </BaseButton>
      </div>
    </div>

    <form
      v-if="adding"
      class="surface mb-6 space-y-4 rounded-[var(--radius-card)] p-5"
      @submit.prevent="create.mutate()"
    >
      <!-- Provider cards. Picking one names the credential and, for the SSH path, pins the host
           keys for you — the step everyone otherwise skips. -->
      <div>
        <div class="eyebrow mb-1.5">Provider</div>
        <div class="grid grid-cols-2 gap-2 sm:grid-cols-4">
          <button
            v-for="p in gitProviders"
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

        <!-- Self-hosted has no built-in host, so it asks for one — this is where a Gitea/Forgejo
             server gets named. It drives the host-key fetch below; the keys are what a self-hosted
             card is for, since Daffa cannot know them in advance the way it knows GitHub's. -->
        <div v-if="chosenProvider?.id === 'selfhosted'" class="mt-3">
          <label for="g-host" class="mb-1.5 block text-sm font-medium">Git server host</label>
          <div class="flex flex-wrap items-center gap-2">
            <input
              id="g-host"
              v-model="selfHost"
              placeholder="git.example.com"
              class="field w-auto flex-1 font-mono text-xs"
              data-cursor="text"
              @keydown.enter.prevent="fetchHostKeys(keyHost)"
            />
            <BaseButton
              type="button"
              intent="secondary"
              size="md"
              :loading="fetchingKeys"
              :disabled="!keyHost"
              @click="fetchHostKeys(keyHost)"
            >
              <AppIcon name="download" class="size-4" />
              Fetch host keys
            </BaseButton>
          </div>
          <p class="subtle mt-1 text-xs">
            Your own git server. Daffa scans its SSH host keys and pins them below, so a substituted
            server is refused rather than handed a deploy.
          </p>
          <p
            v-if="keyStatus"
            class="mt-1 text-xs"
            :style="{ color: keyStatus.ok ? (keyStatus.verified ? 'var(--success)' : 'var(--warn)') : 'var(--danger)' }"
          >
            {{ keyStatus.message }}
          </p>
        </div>
      </div>

      <div class="grid gap-4 sm:grid-cols-2">
        <div>
          <label for="g-name" class="mb-1.5 block text-sm font-medium">Name</label>
          <input
            id="g-name"
            v-model="form.name"
            required
            placeholder="forgejo-deploy"
            class="field"
            data-cursor="text"
          />
        </div>
        <div>
          <label for="g-kind" class="mb-1.5 block text-sm font-medium">Type</label>
          <select id="g-kind" v-model="form.kind" class="field">
            <option value="token">Access token (https://)</option>
            <option value="ssh">SSH key (git@host:…)</option>
          </select>
        </div>
      </div>

      <!-- token -->
      <template v-if="form.kind === 'token'">
        <div class="grid gap-4 sm:grid-cols-2">
          <div>
            <label for="g-user" class="mb-1.5 block text-sm font-medium">Username</label>
            <input
              id="g-user"
              v-model="form.username"
              placeholder="usually anything works"
              class="field"
              data-cursor="text"
            />
          </div>
          <div>
            <label for="g-token" class="mb-1.5 block text-sm font-medium">Access token</label>
            <input id="g-token" v-model="form.token" required type="password" class="field" />
          </div>
        </div>
        <p v-if="chosenProvider && chosenProvider.tokenUrl" class="subtle text-xs leading-relaxed">
          {{ chosenProvider.tokenHint }}
          <a
            :href="chosenProvider.tokenUrl"
            target="_blank"
            rel="noopener"
            class="underline transition hover:text-[var(--accent-text)]"
          >
            Create one ↗
          </a>
        </p>
      </template>

      <!-- ssh -->
      <template v-else>
        <div>
          <label for="g-key" class="mb-1.5 block text-sm font-medium">Private key</label>
          <textarea
            id="g-key"
            v-model="form.ssh_key"
            required
            rows="6"
            spellcheck="false"
            placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
            class="field font-mono text-xs"
            data-cursor="text"
          />
          <p class="subtle mt-1 text-xs">
            The private key — the file <em>without</em> <code class="font-mono">.pub</code>. Add its
            public half to the repository as a deploy key.
          </p>
        </div>

        <div class="grid gap-4 sm:grid-cols-2">
          <div>
            <label for="g-pass" class="mb-1.5 block text-sm font-medium">Passphrase</label>
            <input
              id="g-pass"
              v-model="form.passphrase"
              type="password"
              placeholder="if the key has one"
              class="field"
            />
          </div>
        </div>

        <div>
          <div class="mb-1.5 flex flex-wrap items-center gap-2">
            <label for="g-hostkey" class="block text-sm font-medium">
              Host keys <span class="subtle font-normal">(recommended)</span>
            </label>

            <!-- Hosted providers know their own host, so the re-fetch button lives here. A
                 self-hosted server gets its host and fetch control up top, next to the cards. -->
            <template v-if="chosenProvider?.host">
              <BaseButton
                type="button"
                intent="secondary"
                size="xs"
                :loading="fetchingKeys"
                :disabled="!keyHost"
                @click="fetchHostKeys(keyHost)"
              >
                <AppIcon name="download" class="size-3.5" />
                Fetch &amp; pin
              </BaseButton>
              <span
                v-if="keyStatus"
                class="text-xs"
                :style="{ color: keyStatus.ok ? (keyStatus.verified ? 'var(--success)' : 'var(--warn)') : 'var(--danger)' }"
              >
                {{ keyStatus.message }}
              </span>
            </template>
          </div>
          <textarea
            id="g-hostkey"
            v-model="form.host_key"
            rows="3"
            spellcheck="false"
            placeholder="Fetch above, or paste the output of: ssh-keyscan git.example.com"
            class="field font-mono text-xs"
            data-cursor="text"
          />
          <p class="subtle mt-1 text-xs">
            Paste <strong>every</strong> line — a server has several host keys (ed25519, ecdsa,
            rsa) and the client picks which one to use, so pinning just one would reject an honest
            server.
          </p>
        </div>

        <p class="subtle text-xs">
          Without host keys, Daffa trusts whatever answers at that address. Pinning them means a
          substituted git server is refused rather than handed a deploy.
        </p>
      </template>

      <p v-if="error" class="text-sm" :style="{ color: 'var(--danger)' }">{{ error }}</p>

      <BaseButton type="submit" intent="primary" size="md" :loading="create.isPending.value">
        Save credential
      </BaseButton>
    </form>

    <p v-if="error && !adding" class="mb-3 text-sm" :style="{ color: 'var(--danger)' }">
      {{ error }}
    </p>

    <p v-if="isLoading" class="muted text-sm">Loading…</p>

    <EmptyState
      v-else-if="!creds?.length"
      icon="layers"
      title="No git credentials yet"
      body="A credential is how Daffa reaches a private repository — an access token over https, or an SSH deploy key. Public repositories need none."
    >
      <template #action>
        <BaseButton intent="primary" size="md" @click="adding = true">
          <AppIcon name="plus" class="size-4" />
          Add credential
        </BaseButton>
      </template>
    </EmptyState>

    <div v-else class="surface overflow-hidden rounded-[var(--radius-card)]">
      <table class="w-full text-sm">
        <thead>
          <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
            <th class="eyebrow px-4 py-2 text-left font-medium">Credential</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">In use</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">Actions</th>
          </tr>
        </thead>

        <tbody>
          <tr
            v-for="c in creds"
            :key="c.id"
            class="border-b transition last:border-0 hover:bg-[var(--surface-sunken)]"
            :style="{ borderColor: 'var(--border)' }"
          >
            <td class="py-3 pl-4 pr-4">
              <div class="font-medium">{{ c.name }}</div>
              <div class="subtle mt-0.5 text-xs">
                {{ c.kind === 'ssh' ? 'SSH key' : 'Access token' }}
                <span v-if="c.kind === 'ssh' && !c.pinned" :style="{ color: 'var(--warn)' }">
                  · host key not pinned
                </span>
              </div>
            </td>

            <td class="subtle py-3 pr-4 text-right font-mono text-xs">
              {{ c.in_use }} stack{{ c.in_use === 1 ? '' : 's' }}
            </td>

            <td class="py-3 pr-4 text-right">
              <BaseButton
                intent="danger"
                size="xs"
                :disabled="remove.isPending.value"
                @click="onRemove(c)"
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
