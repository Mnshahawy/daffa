<script setup lang="ts">
import { computed, ref } from 'vue'
import { useMutation, useQuery, useQueryClient } from '@tanstack/vue-query'
import { daffa, type APIToken } from '@/lib/api'
import { toast } from '@/lib/toast'
import { Cap } from '@/lib/caps'
import { useSession } from '@/stores/session'
import { confirm } from '@/lib/confirm'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import CopyButton from '@/components/ui/CopyButton.vue'
import EmptyState from '@/components/ui/EmptyState.vue'

const session = useSession()
const qc = useQueryClient()
const isAdmin = computed(() => session.can(Cap.UsersEdit))

const { data: mine } = useQuery({ queryKey: ['tokens'], queryFn: daffa.myTokens })
const { data: all } = useQuery({
  queryKey: ['tokens-all'],
  queryFn: daffa.allTokens,
  enabled: isAdmin,
})

/** Everyone's tokens except my own — mine are already in the first table. */
const others = computed(() =>
  (all.value ?? []).filter((t) => t.user_id !== session.user?.id),
)

function refresh() {
  qc.invalidateQueries({ queryKey: ['tokens'] })
  qc.invalidateQueries({ queryKey: ['tokens-all'] })
}

function when(ts?: string): string {
  return ts ? new Date(ts).toLocaleString() : 'never'
}

// ── creation, and the one-time reveal ───────────────────────────────────────────

const adding = ref(false)
const form = ref({ name: '', expires_days: 0 })

/** The freshly minted secret. Held only until the operator confirms they stored it. */
const created = ref<{ name: string; token: string } | null>(null)
const copied = ref(false)

const create = useMutation({
  mutationFn: () => daffa.createToken({ ...form.value }),
  onSuccess: (res) => {
    created.value = { name: res.name, token: res.token }
    copied.value = false
    form.value = { name: '', expires_days: 0 }
    adding.value = false
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not create the token.'),
})

const remove = useMutation({
  mutationFn: (id: string) => daffa.deleteToken(id),
  onSuccess: () => {
    toast.ok('Token revoked.')
    refresh()
  },
  onError: (e) => toast.err(e, 'Could not revoke the token.'),
})

async function onRevoke(t: APIToken) {
  const ok = await confirm({
    title: `Revoke the token ${t.name}?`,
    body: 'Whatever authenticates with it stops working on its next request. This cannot be undone — a replacement is a new token.',
    confirmLabel: 'Revoke',
    intent: 'danger',
  })
  if (ok) remove.mutate(t.id)
}
</script>

<template>
  <div class="space-y-10">
    <section>
      <div class="mb-4 flex flex-wrap items-center gap-x-3 gap-y-2">
        <div class="min-w-0">
          <h2 class="text-base font-semibold">API tokens</h2>
          <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
            A token lets a script or CI job call the API as you — same permissions, no
            browser, sent as
            <code class="font-mono text-xs">Authorization: Bearer daffa_…</code>. For
            automation that should not be you, create a dedicated user with a narrow role
            and mint its token. Tokens cannot mint other tokens or change passwords.
          </p>
        </div>
        <div class="ml-auto">
          <BaseButton :intent="adding ? 'secondary' : 'primary'" size="sm" @click="adding = !adding">
            <AppIcon v-if="!adding" name="plus" class="size-3.5" />
            {{ adding ? 'Cancel' : 'New token' }}
          </BaseButton>
        </div>
      </div>

      <!-- The one-time reveal. Modal-ish on purpose: only the hash is stored. -->
      <div
        v-if="created"
        class="mb-5 rounded-[var(--radius-card)] p-5"
        :style="{ background: 'var(--warn-soft)', border: '1px solid color-mix(in oklch, var(--warn) 40%, transparent)' }"
      >
        <p class="mb-1 text-sm font-semibold">Copy the token “{{ created.name }}” — now.</p>
        <p class="mb-3 text-sm leading-relaxed">
          This is the only time it will ever be shown. Daffa stores a hash, not the token
          — close this without copying and the token is unusable by anyone, forever. Put
          it straight into your CI's secret storage.
        </p>
        <code class="mb-3 block overflow-x-auto rounded-lg p-3 font-mono text-xs" :style="{ background: 'var(--surface-sunken)' }">{{ created.token }}</code>
        <div class="flex flex-wrap items-center gap-3">
          <CopyButton intent="primary" size="md" label="Copy token" :text="created.token" @copied="copied = true" />
          <BaseButton intent="secondary" size="md" :disabled="!copied" :title="copied ? '' : 'Copy it first'" @click="created = null">
            I have stored it safely
          </BaseButton>
        </div>
      </div>

      <form v-if="adding" class="surface mb-5 rounded-[var(--radius-card)] p-5" @submit.prevent="create.mutate()">
        <div class="grid gap-4 sm:grid-cols-3">
          <div>
            <label for="tok-name" class="mb-1.5 block text-sm font-medium">Name</label>
            <input id="tok-name" v-model="form.name" required placeholder="forgejo-deploy" class="field" data-cursor="text" />
            <p class="subtle mt-1 text-xs">Names the credential in the audit log: “{{ session.user?.label }} (token: {{ form.name || 'name' }})”.</p>
          </div>
          <div>
            <label for="tok-expiry" class="mb-1.5 block text-sm font-medium">Expires after (days)</label>
            <input id="tok-expiry" v-model.number="form.expires_days" type="number" min="0" class="field" data-cursor="text" />
            <p class="subtle mt-1 text-xs">0 = never. CI tokens are usually long-lived on purpose — revoke beats rotate-by-calendar.</p>
          </div>
        </div>
        <BaseButton type="submit" intent="primary" size="md" class="mt-4" :loading="create.isPending.value">
          Create token
        </BaseButton>
      </form>

      <EmptyState
        v-if="!mine?.length && !adding && !created"
        icon="key"
        title="No API tokens yet"
        body="Mint one to let a script, CI job or curl call the API as you — no password in anyone's secret storage, revocable on its own, and every action it takes is attributed to it in the audit log."
      >
        <template #action>
          <BaseButton intent="primary" size="md" @click="adding = true">
            <AppIcon name="plus" class="size-4" />
            New token
          </BaseButton>
        </template>
      </EmptyState>

      <div v-else-if="mine?.length" class="surface overflow-x-auto rounded-[var(--radius-card)]">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
              <th class="eyebrow px-4 py-2 text-left font-medium">Token</th>
              <th class="eyebrow py-2 pr-3 text-left font-medium">Expires</th>
              <th class="eyebrow hidden py-2 pr-3 text-right font-medium md:table-cell">Last used</th>
              <th class="eyebrow py-2 pr-4 text-right font-medium">Actions</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="t in mine" :key="t.id" class="border-b last:border-0" :style="{ borderColor: 'var(--border)' }">
              <td class="max-w-0 py-3 pl-4 pr-3">
                <div class="font-medium">{{ t.name }}</div>
                <div class="subtle mt-0.5 truncate font-mono text-xs">{{ t.prefix }}…</div>
              </td>
              <td class="py-3 pr-3 text-xs" :style="t.expired ? { color: 'var(--danger)' } : {}">
                {{ t.expired ? 'EXPIRED' : t.expires_at ? new Date(t.expires_at).toLocaleDateString() : 'never' }}
              </td>
              <td class="subtle hidden py-3 pr-3 text-right font-mono text-xs md:table-cell">{{ when(t.last_used_at) }}</td>
              <td class="py-3 pr-4 text-right">
                <BaseButton intent="danger" size="xs" :disabled="remove.isPending.value" @click="onRevoke(t)">
                  <AppIcon name="trash" class="size-3.5" />
                </BaseButton>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <!-- Oversight, not impersonation: admins see that a token exists and can kill it;
         the secret is unrecoverable even here. -->
    <section v-if="isAdmin && others.length">
      <div class="mb-4">
        <h2 class="text-base font-semibold">Everyone's tokens</h2>
        <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
          Every token in the system, with its owner. Revoking one here is the kill switch
          for a credential you no longer trust; disabling the user kills all of theirs at
          once.
        </p>
      </div>
      <div class="surface overflow-x-auto rounded-[var(--radius-card)]">
        <table class="w-full text-sm">
          <thead>
            <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
              <th class="eyebrow px-4 py-2 text-left font-medium">Token</th>
              <th class="eyebrow py-2 pr-3 text-left font-medium">Owner</th>
              <th class="eyebrow py-2 pr-3 text-left font-medium">Expires</th>
              <th class="eyebrow hidden py-2 pr-3 text-right font-medium md:table-cell">Last used</th>
              <th class="eyebrow py-2 pr-4 text-right font-medium">Actions</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="t in others" :key="t.id" class="border-b last:border-0" :style="{ borderColor: 'var(--border)' }">
              <td class="max-w-0 py-3 pl-4 pr-3">
                <div class="font-medium">{{ t.name }}</div>
                <div class="subtle mt-0.5 truncate font-mono text-xs">{{ t.prefix }}…</div>
              </td>
              <td class="py-3 pr-3 text-xs">{{ t.user_label || t.user_id }}</td>
              <td class="py-3 pr-3 text-xs" :style="t.expired ? { color: 'var(--danger)' } : {}">
                {{ t.expired ? 'EXPIRED' : t.expires_at ? new Date(t.expires_at).toLocaleDateString() : 'never' }}
              </td>
              <td class="subtle hidden py-3 pr-3 text-right font-mono text-xs md:table-cell">{{ when(t.last_used_at) }}</td>
              <td class="py-3 pr-4 text-right">
                <BaseButton intent="danger" size="xs" :disabled="remove.isPending.value" @click="onRevoke(t)">
                  <AppIcon name="trash" class="size-3.5" />
                </BaseButton>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>
  </div>
</template>
