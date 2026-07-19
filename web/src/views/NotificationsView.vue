<script setup lang="ts">
import { computed, ref } from 'vue'
import { useQuery, useQueryClient } from '@tanstack/vue-query'
import { ApiError, daffa, type NotifyEvent } from '@/lib/api'
import { Cap } from '@/lib/caps'
import { useSession } from '@/stores/session'
import { toast } from '@/lib/toast'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'
import Select from '@/components/ui/Select.vue'

const session = useSession()
const qc = useQueryClient()

const canEdit = computed(() => session.can(Cap.SettingsEdit))

const { data: smtp } = useQuery({ queryKey: ['smtp'], queryFn: daffa.smtp })
const { data: events } = useQuery({ queryKey: ['notify-events'], queryFn: daffa.notifyEvents })
const { data: rules } = useQuery({ queryKey: ['notify-rules'], queryFn: daffa.notifyRules })
const { data: failed } = useQuery({ queryKey: ['notify-failed'], queryFn: daffa.failedNotifications })
const { data: roles } = useQuery({
  queryKey: ['roles'],
  queryFn: daffa.roles,
  enabled: computed(() => session.can(Cap.RolesView)),
})

const busy = ref(false)
const testResult = ref<{ ok: boolean; message: string } | null>(null)
const preview = ref<{ event: string; html: string } | null>(null)

// Editing state is seeded from the server and only replaces it on save, so a half-typed
// host cannot be picked up by a background refetch.
const form = ref({
  host: '',
  port: 587,
  username: '',
  password: '',
  from_addr: '',
  from_name: 'Daffa',
  base_url: '',
  enabled: false,
})
const loaded = ref(false)

// Fill the form once, the first time the settings arrive.
const _seed = computed(() => {
  if (!loaded.value && smtp.value) {
    form.value = { ...smtp.value, password: '' }
    loaded.value = true
  }
  return null
})

async function save() {
  busy.value = true
  testResult.value = null
  try {
    // Send only the request fields. The form is seeded from the GET view, which carries the
    // read-only has_password flag; the save endpoint decodes strictly and rejects unknown fields,
    // so spreading the whole form would 400 with `unknown field "has_password"`.
    await daffa.saveSmtp({
      host: form.value.host,
      port: form.value.port,
      username: form.value.username,
      password: form.value.password,
      from_addr: form.value.from_addr,
      from_name: form.value.from_name,
      base_url: form.value.base_url,
      enabled: form.value.enabled,
    })
    await qc.invalidateQueries({ queryKey: ['smtp'] })
    form.value.password = '' // it is stored now; keep it out of the DOM
    toast.ok('Mail server saved.')
  } catch (e) {
    toast.err(e, 'Could not save.')
  } finally {
    busy.value = false
  }
}

async function sendTest() {
  busy.value = true
  testResult.value = null
  try {
    testResult.value = await daffa.testSmtp().then((r) => ({
      ok: r.ok,
      message: r.ok ? (r.message ?? 'Sent.') : (r.error ?? 'Failed.'),
    }))
  } catch (e) {
    // testSmtp rejects on a transport error (the request itself failed), distinct from a
    // delivered-but-negative result. Without this the rejection vanished: the spinner stopped
    // and nothing appeared, which reads as "nothing happened" for the one button whose entire
    // job is to tell you whether something happened.
    testResult.value = { ok: false, message: e instanceof ApiError ? e.message : 'Could not reach the mail server.' }
  } finally {
    busy.value = false
  }
}

async function showPreview(e: NotifyEvent) {
  const r = await daffa.previewNotification(e.event)
  preview.value = { event: e.label, html: r.html }
}

// ── channels ────────────────────────────────────────────────────────────────────
const { data: channels } = useQuery({
  queryKey: ['notify-channels'],
  queryFn: daffa.notifyChannels,
})

// The clickable cards. A "kind" is a preset: it decides the payload shape and the help text, so
// the only thing left for a person to paste is the webhook URL. This is the whole point — the
// common path is a click and a paste, not a form full of decisions.
const channelKinds = [
  {
    kind: 'slack' as const,
    label: 'Slack',
    icon: 'inbox' as const,
    hint: 'Incoming Webhook URL',
    help: 'In Slack: Apps → Incoming Webhooks → Add to a channel. Paste the https://hooks.slack.com/… URL it gives you.',
    placeholder: 'https://hooks.slack.com/services/T…/B…/…',
  },
  {
    kind: 'discord' as const,
    label: 'Discord',
    icon: 'inbox' as const,
    hint: 'Webhook URL',
    help: 'In Discord: Channel → Edit → Integrations → Webhooks → New Webhook → Copy URL.',
    placeholder: 'https://discord.com/api/webhooks/…',
  },
  {
    kind: 'webhook' as const,
    label: 'Webhook',
    icon: 'plug' as const,
    hint: 'Endpoint URL',
    help: 'Any endpoint that accepts a JSON POST. It receives the raw event — shape it however you like on your side.',
    placeholder: 'https://example.com/hooks/daffa',
  },
]

const channelKind = ref<'slack' | 'discord' | 'webhook' | null>(null)
const channelForm = ref({ name: '', url: '' })
const channelBusy = ref(false)
const channelTest = ref<{ id: string; ok: boolean; message: string } | null>(null)

const activeKind = computed(() => channelKinds.find((k) => k.kind === channelKind.value) ?? null)

function pickChannelKind(kind: 'slack' | 'discord' | 'webhook') {
  channelKind.value = channelKind.value === kind ? null : kind
  channelForm.value = { name: kind.charAt(0).toUpperCase() + kind.slice(1), url: '' }
}

async function addChannel() {
  if (!channelKind.value) return
  channelBusy.value = true
  try {
    // The server posts a "Daffa is connected" message to prove the URL works before it saves —
    // so a 400 here means the webhook itself was rejected, and the message says why.
    await daffa.createNotifyChannel({
      kind: channelKind.value,
      name: channelForm.value.name.trim(),
      url: channelForm.value.url.trim(),
    })
    channelKind.value = null
    await qc.invalidateQueries({ queryKey: ['notify-channels'] })
    toast.ok('Channel added.')
  } catch (e) {
    toast.err(e, 'Could not add the channel.')
  } finally {
    channelBusy.value = false
  }
}

async function testChannel(id: string) {
  channelTest.value = null
  try {
    const r = await daffa.testNotifyChannel(id)
    channelTest.value = { id, ok: r.ok, message: r.ok ? (r.message ?? 'Posted.') : (r.error ?? 'Failed.') }
  } catch (e) {
    channelTest.value = { id, ok: false, message: e instanceof ApiError ? e.message : 'Could not reach the channel.' }
  }
}

async function removeChannel(id: string) {
  try {
    await daffa.deleteNotifyChannel(id)
    await qc.invalidateQueries({ queryKey: ['notify-channels'] })
    await qc.invalidateQueries({ queryKey: ['notify-rules'] }) // its routing rules cascade away
    toast.ok('Channel removed.')
  } catch (e) {
    toast.err(e, 'Could not delete the channel.')
  }
}

// ── rules ─────────────────────────────────────────────────────────────────────
const adding = ref<string | null>(null)
const newRule = ref({ role_id: '', address: '', channel_id: '' })

function rulesFor(event: string) {
  return (rules.value ?? []).filter((r) => r.Event === event)
}

async function addRule(event: string) {
  try {
    await daffa.createNotifyRule({
      event,
      role_id: newRule.value.role_id,
      address: newRule.value.address.trim(),
      channel_id: newRule.value.channel_id,
    })
    newRule.value = { role_id: '', address: '', channel_id: '' }
    adding.value = null
    await qc.invalidateQueries({ queryKey: ['notify-rules'] })
    toast.ok('Rule added.')
  } catch (e) {
    toast.err(e, 'Could not add the rule.')
  }
}

async function removeRule(id: string) {
  try {
    await daffa.deleteNotifyRule(id)
    await qc.invalidateQueries({ queryKey: ['notify-rules'] })
    toast.ok('Rule removed.')
  } catch (e) {
    toast.err(e, 'Could not remove the rule.')
  }
}
</script>

<template>
  <div>
    <span class="hidden">{{ _seed }}</span>

    <div class="mb-5">
      <h2 class="text-base font-semibold">Notifications</h2>
      <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
        Daffa emails you when something needs attention. A backup that has been failing quietly
        every night is the thing you find out about on the day you need the backup.
      </p>
    </div>

    <!-- SMTP -->
    <div class="surface mb-6 rounded-[var(--radius-card)] p-5">
      <div class="mb-4 flex items-center justify-between">
        <h3 class="text-sm font-semibold">Mail server</h3>
        <label v-if="canEdit" for="smtp-enabled" class="flex items-center gap-2 text-xs">
          <input
            id="smtp-enabled"
            v-model="form.enabled"
            type="checkbox"
            class="accent-[var(--color-accent-500)]"
          />
          <span class="muted">{{ form.enabled ? 'On' : 'Off' }}</span>
        </label>
      </div>

      <div class="grid gap-4 sm:grid-cols-2">
        <div>
          <label for="smtp-host" class="mb-1.5 block text-sm font-medium">Server</label>
          <input
            id="smtp-host"
            v-model="form.host"
            :disabled="!canEdit"
            placeholder="smtp.postmarkapp.com"
            class="field font-mono text-xs"
            data-cursor="text"
          />
        </div>

        <div>
          <label for="smtp-port" class="mb-1.5 block text-sm font-medium">Port</label>
          <input
            id="smtp-port"
            v-model.number="form.port"
            :disabled="!canEdit"
            type="number"
            class="field font-mono text-xs"
          />
          <p class="subtle mt-1 text-xs">465 uses TLS directly; anything else tries STARTTLS.</p>
        </div>

        <div>
          <label for="smtp-user" class="mb-1.5 block text-sm font-medium">Username</label>
          <input
            id="smtp-user"
            v-model="form.username"
            :disabled="!canEdit"
            autocomplete="off"
            class="field font-mono text-xs"
            data-cursor="text"
          />
        </div>

        <div>
          <label for="smtp-pass" class="mb-1.5 block text-sm font-medium">Password</label>
          <input
            id="smtp-pass"
            v-model="form.password"
            :disabled="!canEdit"
            type="password"
            autocomplete="new-password"
            :placeholder="smtp?.has_password ? '•••••••• (unchanged)' : ''"
            class="field font-mono text-xs"
          />
          <p v-if="smtp?.has_password" class="subtle mt-1 text-xs">
            Stored encrypted and never shown again. Leave blank to keep it.
          </p>
        </div>

        <div>
          <label for="smtp-from" class="mb-1.5 block text-sm font-medium">From address</label>
          <input
            id="smtp-from"
            v-model="form.from_addr"
            :disabled="!canEdit"
            placeholder="daffa@example.com"
            class="field font-mono text-xs"
            data-cursor="text"
          />
        </div>

        <div>
          <label for="smtp-fromname" class="mb-1.5 block text-sm font-medium">From name</label>
          <input
            id="smtp-fromname"
            v-model="form.from_name"
            :disabled="!canEdit"
            class="field"
            data-cursor="text"
          />
        </div>

        <div class="sm:col-span-2">
          <label for="smtp-baseurl" class="mb-1.5 block text-sm font-medium">
            Daffa's public URL
          </label>
          <input
            id="smtp-baseurl"
            v-model="form.base_url"
            :disabled="!canEdit"
            placeholder="https://ops.example.com"
            class="field font-mono text-xs"
            data-cursor="text"
          />
          <p class="subtle mt-1 text-xs">
            Used for the “Open in Daffa” button. Daffa sits behind a proxy and cannot work out its
            own public address, so it has to be told. Leave blank and the button is simply omitted
            — better than a link to nowhere.
          </p>
        </div>
      </div>

      <div v-if="canEdit" class="mt-5 flex flex-wrap items-center gap-2">
        <BaseButton intent="primary" size="md" :loading="busy" @click="save">Save</BaseButton>
        <BaseButton
          intent="primary"
          size="md"
          :disabled="busy || !smtp?.enabled"
          @click="sendTest"
        >
          Send a test to me
        </BaseButton>

        <span
          v-if="testResult"
          class="text-xs"
          :style="{ color: testResult.ok ? 'var(--success)' : 'var(--danger)' }"
        >
          {{ testResult.message }}
        </span>
      </div>
    </div>

    <!-- Channels -->
    <div class="surface mb-6 overflow-hidden rounded-[var(--radius-card)]">
      <div class="border-b px-4 py-3" :style="{ borderColor: 'var(--border)' }">
        <div class="eyebrow">Channels</div>
        <p class="muted mt-1 max-w-2xl text-xs leading-relaxed">
          Send alerts somewhere other than email — a Slack or Discord channel, or your own webhook.
          Add one here, then route events to it below. Channels work even with email switched off.
        </p>
      </div>

      <!-- The existing channels -->
      <div
        v-for="c in channels"
        :key="c.id"
        class="flex items-center gap-3 border-b px-4 py-3 last:border-0"
        :style="{ borderColor: 'var(--border)' }"
      >
        <AppIcon :name="c.kind === 'webhook' ? 'plug' : 'inbox'" class="size-4 shrink-0" />
        <div class="min-w-0 flex-1">
          <div class="text-sm font-medium">{{ c.name }}</div>
          <div class="subtle text-xs capitalize">{{ c.kind }}</div>
        </div>
        <span
          v-if="channelTest?.id === c.id"
          class="text-xs"
          :style="{ color: channelTest.ok ? 'var(--success)' : 'var(--danger)' }"
        >
          {{ channelTest.message }}
        </span>
        <BaseButton v-if="canEdit" intent="ghost" size="xs" @click="testChannel(c.id)">Test</BaseButton>
        <BaseButton v-if="canEdit" intent="ghost" size="xs" @click="removeChannel(c.id)">
          <AppIcon name="trash" class="size-3.5" />
        </BaseButton>
      </div>

      <!-- Add: pick a kind (the clickable cards), then paste the URL -->
      <div v-if="canEdit" class="p-4">
        <div class="grid gap-2 sm:grid-cols-3">
          <button
            v-for="k in channelKinds"
            :key="k.kind"
            type="button"
            class="flex items-center gap-2.5 rounded-[var(--radius-control)] border px-3 py-2.5 text-left transition"
            :style="
              channelKind === k.kind
                ? { borderColor: 'var(--accent)', background: 'var(--accent-soft)' }
                : { borderColor: 'var(--border)' }
            "
            @click="pickChannelKind(k.kind)"
          >
            <AppIcon :name="k.icon" class="size-5 shrink-0" />
            <div class="min-w-0">
              <div class="text-sm font-medium">{{ k.label }}</div>
              <div class="subtle truncate text-xs">{{ k.hint }}</div>
            </div>
          </button>
        </div>

        <!-- The one form, revealed by whichever card is chosen -->
        <div v-if="activeKind" class="mt-3 space-y-3 border-t pt-3" :style="{ borderColor: 'var(--border)' }">
          <p class="muted text-xs leading-relaxed">{{ activeKind.help }}</p>
          <div class="grid gap-2 sm:grid-cols-[1fr_2fr]">
            <div>
              <label for="ch-name" class="mb-1 block text-xs font-medium">Name</label>
              <input id="ch-name" v-model="channelForm.name" class="field py-1 text-sm" data-cursor="text" />
            </div>
            <div>
              <label for="ch-url" class="mb-1 block text-xs font-medium">{{ activeKind.hint }}</label>
              <input
                id="ch-url"
                v-model="channelForm.url"
                :placeholder="activeKind.placeholder"
                class="field py-1 font-mono text-xs"
                data-cursor="text"
              />
            </div>
          </div>
          <BaseButton
            intent="primary"
            size="sm"
            :loading="channelBusy"
            :disabled="!channelForm.name || !channelForm.url"
            @click="addChannel"
          >
            {{ channelBusy ? 'Testing the webhook…' : 'Add channel' }}
          </BaseButton>
          <p class="subtle text-xs">
            Daffa posts a “connected” message to prove the webhook works before saving it.
          </p>
        </div>
      </div>
    </div>

    <!-- Rules -->
    <div class="surface mb-6 overflow-hidden rounded-[var(--radius-card)]">
      <div class="eyebrow border-b px-4 py-2.5" :style="{ borderColor: 'var(--border)' }">
        Who gets told
      </div>

      <div
        v-for="e in events"
        :key="e.event"
        class="border-b p-4 last:border-0"
        :style="{ borderColor: 'var(--border)' }"
      >
        <div class="flex items-start gap-4">
          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-2">
              <span class="text-sm font-medium">{{ e.label }}</span>
              <span v-if="e.noisy" class="subtle text-xs">chatty</span>
            </div>
            <p class="muted mt-0.5 text-xs">{{ e.description }}</p>

            <div class="mt-2 flex flex-wrap items-center gap-1.5">
              <span
                v-for="r in rulesFor(e.event)"
                :key="r.ID"
                class="flex items-center gap-1 rounded-md py-0.5 pl-1.5 pr-0.5 text-xs"
                :style="{ background: 'var(--surface-sunken)' }"
              >
                <AppIcon
                  v-if="r.ChannelID"
                  :name="r.ChannelKind === 'webhook' ? 'plug' : 'inbox'"
                  class="size-3 shrink-0 opacity-70"
                />
                {{ r.ChannelName || r.RoleName || r.Address }}
                <BaseButton
                  v-if="canEdit"
                  intent="ghost"
                  size="xs"
                  icon
                  :label="`Stop telling ${r.ChannelName || r.RoleName || r.Address}`"
                  class="size-5 min-w-0"
                  @click="removeRule(r.ID)"
                >
                  <AppIcon name="x" class="size-3" />
                </BaseButton>
              </span>
              <span v-if="!rulesFor(e.event).length" class="subtle text-xs">nobody — off</span>
            </div>
          </div>

          <div class="flex shrink-0 items-center gap-1">
            <BaseButton v-if="canEdit" intent="ghost" size="xs" @click="showPreview(e)">
              Preview
            </BaseButton>
            <BaseButton
              v-if="canEdit"
              :intent="adding === e.event ? 'secondary' : 'primary'"
              size="xs"
              @click="adding = adding === e.event ? null : e.event"
            >
              {{ adding === e.event ? 'Cancel' : 'Add' }}
            </BaseButton>
          </div>
        </div>

        <!-- A rule is a role OR an address. A role resolves at send time, so it tracks
             membership by itself — and it honours scope, so an operator on staging is not
             paged about prod. -->
        <div v-if="adding === e.event" class="mt-3 flex flex-wrap items-center gap-2">
          <label :for="`rule-role-${e.event}`" class="sr-only">Role to tell</label>
          <Select
            :id="`rule-role-${e.event}`"
            v-model="newRule.role_id"
            class="w-auto"
            select-class="py-1 text-xs"
            @change="((newRule.address = ''), (newRule.channel_id = ''))"
          >
            <option value="">Everyone holding a role…</option>
            <option v-for="r in roles" :key="r.id" :value="r.id">{{ r.name }}</option>
          </Select>

          <span class="subtle text-xs">or</span>

          <label :for="`rule-addr-${e.event}`" class="sr-only">Address to tell</label>
          <input
            :id="`rule-addr-${e.event}`"
            v-model="newRule.address"
            placeholder="oncall@example.com"
            class="field w-auto py-1 font-mono text-xs"
            data-cursor="text"
            @input="((newRule.role_id = ''), (newRule.channel_id = ''))"
          />

          <template v-if="channels?.length">
            <span class="subtle text-xs">or</span>
            <label :for="`rule-chan-${e.event}`" class="sr-only">Channel to post to</label>
            <Select
              :id="`rule-chan-${e.event}`"
              v-model="newRule.channel_id"
              class="w-auto"
              select-class="py-1 text-xs"
              @change="((newRule.role_id = ''), (newRule.address = ''))"
            >
              <option value="">A channel…</option>
              <option v-for="c in channels" :key="c.id" :value="c.id">{{ c.name }}</option>
            </Select>
          </template>

          <BaseButton
            intent="primary"
            size="xs"
            :disabled="!newRule.role_id && !newRule.address && !newRule.channel_id"
            @click="addRule(e.event)"
          >
            Add
          </BaseButton>

          <p class="subtle mt-1 w-full text-xs">
            A role is resolved when the mail is sent, so it follows membership by itself — and it
            respects scope: somebody who only holds the role on one host is not told about another.
          </p>
        </div>
      </div>
    </div>

    <!-- Dead letters. A failed alert that vanished is the worst outcome there is. -->
    <div v-if="failed?.length" class="surface mb-6 overflow-hidden rounded-[var(--radius-card)]">
      <div
        class="eyebrow border-b px-4 py-2.5"
        :style="{ borderColor: 'var(--border)', color: 'var(--danger)' }"
      >
        Notifications that could not be delivered
      </div>
      <div
        v-for="(f, i) in failed"
        :key="i"
        class="border-b p-4 text-sm last:border-0"
        :style="{ borderColor: 'var(--border)' }"
      >
        <div class="font-medium">{{ f.subject }}</div>
        <div class="subtle mt-0.5 text-xs">
          to <span class="font-mono">{{ f.to }}</span> ·
          {{ new Date(f.at).toLocaleString() }}
        </div>
        <div class="mt-1 font-mono text-xs" :style="{ color: 'var(--danger)' }">{{ f.error }}</div>
      </div>
    </div>

    <!-- Preview -->
    <div
      v-if="preview"
      class="fixed inset-0 z-50 flex items-center justify-center p-6"
      style="background: rgba(0, 0, 0, 0.5)"
      @click.self="preview = null"
    >
      <div
        class="surface flex max-h-[85vh] w-full max-w-2xl flex-col overflow-hidden rounded-[var(--radius-card)]"
      >
        <div
          class="flex items-center justify-between border-b px-4 py-2"
          :style="{ borderColor: 'var(--border)' }"
        >
          <span class="text-sm font-medium">{{ preview.event }}</span>
          <BaseButton intent="secondary" size="xs" @click="preview = null">Close</BaseButton>
        </div>
        <iframe :srcdoc="preview.html" class="h-[70vh] w-full border-0 bg-white" sandbox="" />
      </div>
    </div>
  </div>
</template>
