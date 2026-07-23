<script setup lang="ts">
import { computed, ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { daffa, type SSHKey, type CreatedSSHKey } from '@/lib/api'
import { confirm } from '@/lib/confirm'
import { toast } from '@/lib/toast'
import { useSession } from '@/stores/session'
import { Cap } from '@/lib/caps'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import CopyButton from '@/components/ui/CopyButton.vue'
import EmptyState from '@/components/ui/EmptyState.vue'
import Select from '@/components/ui/Select.vue'

const qc = useQueryClient()
const session = useSession()
const adding = ref(false)

// Reachable with sshkeys.view; generating, importing and deleting is sshkeys.edit (global).
// Gate the controls so a view-only operator does not see a button the route would refuse.
const canEdit = computed(() => session.can(Cap.SshkeysEdit))

const { data: keys, isLoading } = useQuery({
  queryKey: ['ssh-keys'],
  queryFn: daffa.sshKeys,
})

const blank = () => ({
  name: '',
  mode: 'generate' as 'generate' | 'import',
  algo: 'ed25519' as 'ed25519' | 'rsa',
  private_key: '',
  passphrase: '',
})
const form = ref(blank())

// The one moment the operator needs the public key in hand: right after creating it, to paste
// into the target's authorized_keys. Held here until dismissed so it does not vanish on the
// list refetch.
const minted = ref<CreatedSSHKey | null>(null)

const create = useMutation({
  mutationFn: () => daffa.createSSHKey(form.value),
  onSuccess: (resp) => {
    minted.value = resp
    form.value = blank()
    adding.value = false
    toast.ok('SSH key ready.')
    qc.invalidateQueries({ queryKey: ['ssh-keys'] })
  },
  onError: (e) => toast.err(e, 'Could not create the key.'),
})

const remove = useMutation({
  mutationFn: (id: string) => daffa.deleteSSHKey(id),
  onSettled: () => qc.invalidateQueries({ queryKey: ['ssh-keys'] }),
  onSuccess: () => toast.ok('SSH key deleted.'),
  onError: (e) => toast.err(e, 'Could not delete the key.'),
})

async function onRemove(k: SSHKey) {
  if (k.in_use > 0) {
    toast.warn(`${k.name} is in use by ${k.in_use} cluster${k.in_use === 1 ? '' : 's'} or node${k.in_use === 1 ? '' : 's'}. Point them at another key first.`)
    return
  }
  const ok = await confirm({
    title: `Delete the SSH key ${k.name}?`,
    body:
      'Daffa forgets the private key. It is not stored anywhere else and cannot be recovered — you would have to generate a new one and add its public half to every machine again.',
    confirmLabel: 'Delete',
    intent: 'danger',
  })
  if (!ok) return
  remove.mutate(k.id)
}
</script>

<template>
  <div>
    <div class="mb-5 flex flex-wrap items-center gap-x-3 gap-y-2">
      <div class="min-w-0">
        <h2 class="text-base font-semibold">SSH keys</h2>
        <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
          Keys Daffa uses to reach a machine over SSH — a remote cluster, or a node added
          without an agent. Generate one here and add its <strong>public</strong> half to the
          target's <code class="font-mono">authorized_keys</code>; the private half is sealed and
          never leaves the server.
        </p>
      </div>

      <div v-if="canEdit" class="ml-auto">
        <BaseButton :intent="adding ? 'secondary' : 'primary'" @click="adding = !adding">
          <AppIcon v-if="!adding" name="plus" class="size-4" />
          {{ adding ? 'Cancel' : 'Add key' }}
        </BaseButton>
      </div>
    </div>

    <!-- Minted: the public key, shown once so it can be copied straight into the target. It is
         not a secret, so it stays available in the list too — this panel just puts it in hand at
         the moment it is needed. -->
    <div
      v-if="minted"
      class="surface mb-6 rounded-[var(--radius-card)] border p-5"
      :style="{ borderColor: 'var(--accent)' }"
    >
      <div class="mb-2 flex items-center gap-2">
        <AppIcon name="check" class="size-4" :style="{ color: 'var(--success)' }" />
        <span class="font-medium">Key ready — add its public half to the machine</span>
        <BaseButton class="ml-auto" intent="ghost" size="xs" aria-label="Dismiss" @click="minted = null">
          <AppIcon name="x" class="size-4" />
        </BaseButton>
      </div>
      <p class="subtle mb-2 text-xs">
        Append this line to <code class="font-mono">~/.ssh/authorized_keys</code> for the user
        Daffa will connect as. Fingerprint <code class="font-mono">{{ minted.fingerprint }}</code>.
      </p>
      <div class="flex items-start gap-2">
        <code
          class="field min-w-0 flex-1 overflow-x-auto whitespace-pre font-mono text-xs"
          data-cursor="text"
          >{{ minted.public_key }}</code
        >
        <CopyButton intent="secondary" size="sm" :text="minted.public_key" />
      </div>
    </div>

    <form
      v-if="adding"
      class="surface mb-6 space-y-4 rounded-[var(--radius-card)] p-5"
      @submit.prevent="create.mutate()"
    >
      <div class="grid gap-4 sm:grid-cols-2">
        <div>
          <label for="k-name" class="mb-1.5 block text-sm font-medium">Name</label>
          <input
            id="k-name"
            v-model="form.name"
            required
            placeholder="prod-fleet"
            class="field"
            data-cursor="text"
          />
        </div>
        <div>
          <label for="k-mode" class="mb-1.5 block text-sm font-medium">Source</label>
          <Select id="k-mode" v-model="form.mode">
            <option value="generate">Generate a new keypair</option>
            <option value="import">Import an existing private key</option>
          </Select>
        </div>
      </div>

      <!-- generate -->
      <template v-if="form.mode === 'generate'">
        <div class="sm:w-1/2 sm:pr-2">
          <label for="k-algo" class="mb-1.5 block text-sm font-medium">Algorithm</label>
          <Select id="k-algo" v-model="form.algo">
            <option value="ed25519">Ed25519 (recommended)</option>
            <option value="rsa">RSA 4096</option>
          </Select>
          <p class="subtle mt-1 text-xs">
            Daffa keeps the private half sealed and shows you the public half to install on the
            target.
          </p>
        </div>
      </template>

      <!-- import -->
      <template v-else>
        <div>
          <label for="k-key" class="mb-1.5 block text-sm font-medium">Private key</label>
          <textarea
            id="k-key"
            v-model="form.private_key"
            required
            rows="6"
            spellcheck="false"
            placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
            class="field font-mono text-xs"
            data-cursor="text"
          />
          <p class="subtle mt-1 text-xs">
            The private key — the file <em>without</em> <code class="font-mono">.pub</code>. Daffa
            derives the public half and fingerprint from it.
          </p>
        </div>
        <div class="sm:w-1/2 sm:pr-2">
          <label for="k-pass" class="mb-1.5 block text-sm font-medium">Passphrase</label>
          <input
            id="k-pass"
            v-model="form.passphrase"
            type="password"
            placeholder="if the key has one"
            class="field"
          />
        </div>
      </template>

      <BaseButton type="submit" intent="primary" size="md" :loading="create.isPending.value">
        {{ form.mode === 'generate' ? 'Generate key' : 'Import key' }}
      </BaseButton>
    </form>

    <p v-if="isLoading" class="muted text-sm">Loading…</p>

    <EmptyState
      v-else-if="!keys?.length"
      icon="key"
      title="No SSH keys yet"
      body="An SSH key lets Daffa dial out to a machine it does not run on — a remote cluster or a node added over SSH. Generate one, then add its public half to the target."
    >
      <template v-if="canEdit" #action>
        <BaseButton intent="primary" size="md" @click="adding = true">
          <AppIcon name="plus" class="size-4" />
          Add key
        </BaseButton>
      </template>
    </EmptyState>

    <div v-else class="surface overflow-x-auto rounded-[var(--radius-card)]">
      <table class="w-full text-sm">
        <thead>
          <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
            <th class="eyebrow px-4 py-2 text-left font-medium">Key</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Public key</th>
            <th class="eyebrow py-2 pr-4 text-right font-medium">Actions</th>
          </tr>
        </thead>

        <tbody>
          <tr
            v-for="k in keys"
            :key="k.id"
            class="border-b transition last:border-0 hover:bg-[var(--surface-sunken)]"
            :style="{ borderColor: 'var(--border)' }"
          >
            <td class="py-3 pl-4 pr-4 align-top">
              <div class="font-medium">{{ k.name }}</div>
              <div class="subtle mt-0.5 break-all font-mono text-xs">{{ k.algo }} · {{ k.fingerprint }}</div>
            </td>

            <td class="py-3 pr-4 align-top">
              <div class="flex items-center gap-2">
                <!-- The cap must be a fixed unit: a percentage max-width cannot clamp a table
                     column's intrinsic width, so the nowrap key would size the column anyway. -->
                <code class="muted block max-w-[8rem] truncate font-mono text-xs sm:max-w-[24rem]">{{ k.public_key }}</code>
                <CopyButton intent="ghost" size="xs" :text="k.public_key" />
              </div>
            </td>

            <td class="py-3 pr-4 text-right align-top">
              <BaseButton
                v-if="canEdit"
                intent="danger"
                size="xs"
                :disabled="remove.isPending.value"
                @click="onRemove(k)"
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
