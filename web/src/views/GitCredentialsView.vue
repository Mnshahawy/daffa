<script setup lang="ts">
import { computed, ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { ApiError, daffa, type GitCredential } from '@/lib/api'
import { confirm } from '@/lib/confirm'
import { toast } from '@/lib/toast'
import { useSession } from '@/stores/session'
import { Cap } from '@/lib/caps'
import { RouterLink } from 'vue-router'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import EmptyState from '@/components/ui/EmptyState.vue'
import Select from '@/components/ui/Select.vue'

const qc = useQueryClient()
const session = useSession()
const adding = ref(false)

// Reachable with deploy.git_creds.view; adding and deleting is deploy.git_creds.edit. Gate
// the buttons so a view-only operator does not see a control the route would refuse.
const canEdit = computed(() => session.can(Cap.GitCredsEdit))

const { data: creds, isLoading } = useQuery({
  queryKey: ['gitcreds'],
  queryFn: daffa.gitCredentials,
})

// SSH credentials draw their key from the shared SSH-key store — git creds no longer hold key
// material of their own. The form offers a selection of those keys, not a paste box.
const { data: sshKeys } = useQuery({ queryKey: ['ssh-keys'], queryFn: daffa.sshKeys })
const hasKeys = computed(() => (sshKeys.value?.length ?? 0) > 0)

const blank = () => ({
  name: '',
  kind: 'token' as 'token' | 'ssh',
  username: '',
  token: '',
  ssh_key_id: '',
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

const nameFor = (p: GitProvider) => (p.id === 'selfhosted' ? '' : `${p.label} deploy`)

// What the LAST provider suggested for the name. Without remembering it, switching GitHub→GitLab
// leaves "GitHub deploy" sitting under a GitLab selection (the bug this fixes): the field is no
// longer empty, so a plain fill-if-empty check skips it. Follow the suggestion until it has been
// hand-edited — the same approach as the IdP presets in AuthenticationView.
const providerSuggestion = ref('')

function pickProvider(p: GitProvider) {
  keyStatus.value = null
  // A re-click toggles the selection off; leave the form as-is so a half-typed credential survives.
  if (chosenProvider.value?.id === p.id) {
    chosenProvider.value = null
    return
  }
  chosenProvider.value = p
  // Overwrite the name only when it is still empty or still holds the PREVIOUS provider's
  // suggestion — a name someone typed on purpose is theirs.
  if (!form.value.name || form.value.name === providerSuggestion.value) form.value.name = nameFor(p)
  providerSuggestion.value = nameFor(p)
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
    providerSuggestion.value = ''
    selfHost.value = ''
    keyStatus.value = null
    toast.ok('Credential saved.')
    qc.invalidateQueries({ queryKey: ['gitcreds'] })
  },
  onError: (e) => toast.err(e, 'Could not save the credential.'),
})

const remove = useMutation({
  mutationFn: (id: string) => daffa.deleteGitCredential(id),
  onSettled: () => qc.invalidateQueries({ queryKey: ['gitcreds'] }),
  onSuccess: () => toast.ok('Credential deleted.'),
  onError: (e) => toast.err(e, 'Could not delete the credential.'),
})

// ── test a credential against a repo (ls-remote, no clone) ──────────────────────
// The credential stores no URL, so testing needs one: the operator names any repo the
// credential should be able to read, and Daffa lists its refs with the credential.
const testingId = ref<string | null>(null)
const testUrl = ref('')
const testResult = ref<{ ok: boolean; error?: string } | null>(null)

function openTest(c: GitCredential) {
  testingId.value = testingId.value === c.id ? null : c.id
  testUrl.value = ''
  testResult.value = null
}

const test = useMutation({
  mutationFn: (id: string) => daffa.testGitCredential(id, { url: testUrl.value.trim() }),
  onSuccess: (resp) => {
    testResult.value = resp
  },
  onError: (e) => {
    testResult.value = { ok: false, error: e instanceof ApiError ? e.message : 'Could not run the test.' }
  },
})

async function onRemove(c: GitCredential) {
  if (c.in_use > 0) {
    toast.warn(`${c.name} is used by ${c.in_use} stack${c.in_use === 1 ? '' : 's'}. Point them elsewhere first.`)
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

      <div v-if="canEdit" class="ml-auto">
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
          <Select id="g-kind" v-model="form.kind">
            <option value="token">Access token (https://)</option>
            <option value="ssh">SSH key (git@host:…)</option>
          </Select>
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
          <label for="g-key" class="mb-1.5 block text-sm font-medium">SSH key</label>
          <p v-if="!hasKeys" class="text-sm" :style="{ color: 'var(--warn)' }">
            You have no SSH keys yet.
            <RouterLink :to="{ name: 'settings-ssh' }" class="underline">Add one under SSH keys</RouterLink>,
            then add its public half to the repository as a deploy key.
          </p>
          <template v-else>
            <Select id="g-key" v-model="form.ssh_key_id">
              <option value="" disabled>Choose a key…</option>
              <option v-for="k in sshKeys" :key="k.id" :value="k.id">{{ k.name }} ({{ k.algo }})</option>
            </Select>
            <p class="subtle mt-1 text-xs">
              Keys are managed under
              <RouterLink :to="{ name: 'settings-ssh' }" class="underline">SSH keys</RouterLink>. Add the
              chosen key's public half to the repository as a deploy key.
            </p>
          </template>
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

      <BaseButton
        type="submit"
        intent="primary"
        size="md"
        :loading="create.isPending.value"
        :disabled="form.kind === 'ssh' && !form.ssh_key_id"
      >
        Save credential
      </BaseButton>
    </form>

    <p v-if="isLoading" class="muted text-sm">Loading…</p>

    <EmptyState
      v-else-if="!creds?.length"
      icon="layers"
      title="No git credentials yet"
      body="A credential is how Daffa reaches a private repository — an access token over https, or an SSH deploy key. Public repositories need none."
    >
      <template v-if="canEdit" #action>
        <BaseButton intent="primary" size="md" @click="adding = true">
          <AppIcon name="plus" class="size-4" />
          Add credential
        </BaseButton>
      </template>
    </EmptyState>

    <div v-else class="surface overflow-x-auto rounded-[var(--radius-card)]">
      <table class="w-full text-sm">
        <thead>
          <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
            <th class="eyebrow px-4 py-2 text-left font-medium">Credential</th>
            <th class="eyebrow hidden py-2 pr-4 text-right font-medium md:table-cell">In use</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">Actions</th>
          </tr>
        </thead>

        <tbody>
          <template v-for="c in creds" :key="c.id">
            <tr
              class="border-b transition hover:bg-[var(--surface-sunken)]"
              :class="{ 'last:border-0': testingId !== c.id }"
              :style="{ borderColor: 'var(--border)' }"
            >
              <td class="py-3 pl-4 pr-4">
                <div class="font-medium">{{ c.name }}</div>
                <div class="subtle mt-0.5 text-xs">
                  <template v-if="c.kind === 'ssh'">
                    SSH key<template v-if="c.ssh_key_name"> · {{ c.ssh_key_name }}</template>
                  </template>
                  <template v-else>Access token</template>
                  <span v-if="c.kind === 'ssh' && !c.pinned" :style="{ color: 'var(--warn)' }">
                    · host key not pinned
                  </span>
                </div>
              </td>

              <td class="subtle hidden py-3 pr-4 text-right font-mono text-xs md:table-cell">
                {{ c.in_use }} stack{{ c.in_use === 1 ? '' : 's' }}
              </td>

              <td class="py-3 pr-4 text-right">
                <div class="flex items-center justify-end gap-1">
                  <BaseButton
                    v-if="canEdit"
                    :intent="testingId === c.id ? 'primary' : 'secondary'"
                    size="xs"
                    @click="openTest(c)"
                  >
                    Test
                  </BaseButton>
                  <BaseButton
                    v-if="canEdit"
                    intent="danger"
                    size="xs"
                    :disabled="remove.isPending.value"
                    @click="onRemove(c)"
                  >
                    <AppIcon name="trash" class="size-3.5" />
                    Delete
                  </BaseButton>
                </div>
              </td>
            </tr>

            <!-- Test panel: the credential carries no URL, so name a repo it should be able
                 to read; Daffa runs ls-remote (no clone) with it. -->
            <tr
              v-if="testingId === c.id"
              class="border-b last:border-0"
              :style="{ borderColor: 'var(--border)', background: 'var(--surface-sunken)' }"
            >
              <td colspan="3" class="px-4 py-3">
                <form class="flex flex-wrap items-center gap-2" @submit.prevent="test.mutate(c.id)">
                  <input
                    v-model="testUrl"
                    required
                    placeholder="https://git.example.com/me/repo.git"
                    class="field w-full min-w-0 flex-1 font-mono text-xs sm:w-auto"
                    data-cursor="text"
                  />
                  <BaseButton type="submit" intent="primary" size="sm" :loading="test.isPending.value">
                    Test access
                  </BaseButton>
                </form>
                <p
                  v-if="testResult"
                  class="mt-2 text-xs"
                  :style="{ color: testResult.ok ? 'var(--success)' : 'var(--danger)' }"
                >
                  <template v-if="testResult.ok">
                    ✓ Reachable — the credential can read this repository.
                  </template>
                  <template v-else>✗ {{ testResult.error }}</template>
                </p>
                <p class="subtle mt-1 text-xs">
                  Runs <code class="font-mono">ls-remote</code> with this credential — no clone. Use
                  any repository the credential should be able to read.
                </p>
              </td>
            </tr>
          </template>
        </tbody>
      </table>
    </div>
  </div>
</template>
