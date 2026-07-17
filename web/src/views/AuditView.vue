<script setup lang="ts">
import { computed, ref } from 'vue'
import { useQuery } from '@tanstack/vue-query'
import { daffa, type AuditEntry } from '@/lib/api'
import { type Status } from '@/lib/status'
import EmptyState from '@/components/ui/EmptyState.vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import SearchInput from '@/components/SearchInput.vue'
import StatusPill from '@/components/ui/StatusPill.vue'

// Every mutating action lands here. A tool holding a root-equivalent socket that
// cannot tell you who restarted what, and when, is not one you should trust.
const { data: entries, isLoading } = useQuery({
  queryKey: ['audit'],
  queryFn: daffa.audit,
})

const filter = ref('')

// The audit log is the longest list in the app and the one you arrive at with a specific
// question — "who removed that volume", "what did the failed deploy say". Searching the
// outcome too means `denied` and `error` work as filters, which is how you find trouble.
const shown = computed(() => {
  const q = filter.value.trim().toLowerCase()
  if (!q) return entries.value ?? []
  return (entries.value ?? []).filter(
    (e) =>
      e.action.toLowerCase().includes(q) ||
      e.user.toLowerCase().includes(q) ||
      e.target.toLowerCase().includes(q) ||
      e.outcome.toLowerCase().includes(q),
  )
})

function when(iso: string): string {
  return new Date(iso).toLocaleString()
}

/**
 * A refused action is the security-relevant row on this page, and it used to read exactly like
 * every other one — amber, the same colour the UI uses for "restarting". Someone probing what
 * they are not allowed to do is not a warning, it is the thing you came here to find, so it is
 * `danger` and the whole row is tinted.
 *
 * `error` moves down to amber in exchange: a command that ran and failed is an operational
 * problem, not a security one.
 *
 * The labels stay the words the filter matches — you can read "denied" on the row and then type
 * it into the box above.
 */
function outcomeStatus(e: AuditEntry): Status {
  switch (e.outcome) {
    case 'denied':
      return { tone: 'danger', label: 'denied' }
    case 'error':
      return { tone: 'warn', label: 'error' }
    default:
      return { tone: 'success', label: 'ok' }
  }
}

/** Why a row has nobody in the Who column. Hover text, so the dash stops reading as a gap. */
function noActorReason(e: AuditEntry): string {
  if (e.action === 'auth.login' && e.outcome === 'denied')
    return 'Nobody was signed in — the sign-in was refused. The username that was tried is the target.'
  if (e.action.startsWith('stack.') && e.detail?.includes('webhook'))
    return 'Started by a webhook, not by a person.'
  return 'No signed-in user was attached to this action.'
}

/**
 * The detail is stored as a JSON object (store.AuditDetail), and it was being printed as one:
 *
 *     {"ip":"::1","reason":"unknown_or_disabled"}
 *
 * That is the database's rendering of the fact, not a person's. Unpack it into pairs.
 *
 * `reason` leads, because on a refused row it is the entire point — everything else is
 * provenance. The values stay exactly as they were written (`bad_password`, not "bad password"),
 * because the reason you are reading this page is usually to grep it, and a word you can read
 * but cannot type into the filter box above is a word that has been taken away from you.
 */
const KEY_ORDER = ['reason', 'error', 'event', 'container', 'agent_id', 'version', 'ip']

function detailPairs(detail?: string): [string, string][] {
  if (!detail) return []

  let parsed: unknown
  try {
    parsed = JSON.parse(detail)
  } catch {
    // Not JSON after all — show whatever it is rather than nothing.
    return [['', detail]]
  }
  if (typeof parsed !== 'object' || parsed === null) return [['', detail]]

  return Object.entries(parsed as Record<string, unknown>)
    .filter(([, v]) => v !== '' && v != null)
    .map(([k, v]) => [k, String(v)] as [string, string])
    .sort(([a], [b]) => {
      const ia = KEY_ORDER.indexOf(a)
      const ib = KEY_ORDER.indexOf(b)
      return (ia < 0 ? KEY_ORDER.length : ia) - (ib < 0 ? KEY_ORDER.length : ib)
    })
}
</script>

<template>
  <div>
    <PageHeader
      title="Audit"
      :count="entries ? (filter ? `${shown.length} of ${entries.length}` : entries.length) : undefined"
      description="Every mutating action, and every one refused."
    >
      <template #actions>
        <SearchInput
          v-model="filter"
          placeholder="Action, user, target, or outcome…"
          class="w-80"
        />
      </template>
    </PageHeader>

    <p v-if="isLoading" class="muted text-sm">Loading…</p>

    <EmptyState
      v-else-if="!entries?.length"
      icon="scroll"
      title="Nothing recorded yet"
      body="Every action that changes something — a deploy, a restart, a removed volume — is written here with who did it, and so is every action Daffa refused. Nothing has happened yet."
    />

    <p v-else-if="!shown.length" class="muted text-sm">Nothing matches “{{ filter }}”.</p>

    <div v-else class="surface overflow-hidden rounded-[var(--radius-card)]">
      <table class="w-full text-sm">
        <thead>
          <tr class="border-b" :style="{ borderColor: 'var(--border)' }">
            <th class="eyebrow px-4 py-2 text-left font-medium">When</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Who</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Action</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Target</th>
            <th class="eyebrow py-2 pr-4 text-left font-medium">Outcome</th>
          </tr>
        </thead>

        <tbody>
          <!-- A refused row carries a red rule down its left edge and the faintest wash of red.
               It is the row you came to this page to find, and it should be findable without
               being read — but the wash has to stay FAINT, because a page of refusals is the
               normal shape of a brute-force attempt, and at any real strength the rows fuse into
               one red slab with no borders and no countable edges. The rule is what you scan;
               the wash only groups. -->
          <tr
            v-for="(e, i) in shown"
            :key="i"
            class="border-b last:border-0"
            :style="
              e.outcome === 'denied'
                ? {
                    borderColor: 'var(--border)',
                    background: 'color-mix(in oklch, var(--danger) 7%, transparent)',
                    boxShadow: 'inset 2px 0 0 var(--danger)',
                  }
                : { borderColor: 'var(--border)' }
            "
          >
            <td class="subtle whitespace-nowrap px-4 py-2 font-mono text-xs">
              <time :title="e.at">{{ when(e.at) }}</time>
            </td>

            <!-- A blank Who is a fact, not a gap — but only for the rows where nobody was
                 signed in. A refused login has no authenticated user by definition (the username
                 that was TRIED is the target, not the actor), and a webhook deploy has no person
                 behind it at all. Say which, rather than leaving a dash to be read as missing
                 data. Actions a named person took now carry their name; that was a real bug, and
                 it was in the store. -->
            <td class="whitespace-nowrap py-2 pr-4">
              <template v-if="e.user">{{ e.user }}</template>
              <span v-else class="subtle" :title="noActorReason(e)">—</span>
            </td>

            <td class="whitespace-nowrap py-2 pr-4 font-mono text-xs">{{ e.action }}</td>

            <td class="subtle max-w-0 truncate py-2 pr-4 font-mono text-xs" :title="e.target">
              {{ e.target || '—' }}
            </td>

            <!-- The pill and the reason sit on ONE line. Stacked, the refused rows grew a second
                 line and every row on the page ended up a different height, which is the other
                 half of why this list read as a mess. -->
            <td class="py-2 pr-4">
              <div class="flex min-w-0 items-center gap-2.5">
                <StatusPill :status="outcomeStatus(e)" />

                <!-- Recorded all along, and never shown. On a refused row it is the only thing
                     that says what they were actually turned away from. -->
                <div
                  v-if="e.outcome !== 'ok' && detailPairs(e.detail).length"
                  class="flex min-w-0 items-center gap-x-3 gap-y-0.5 overflow-hidden"
                  :title="e.detail"
                >
                  <span
                    v-for="[k, v] in detailPairs(e.detail)"
                    :key="k"
                    class="flex min-w-0 items-baseline gap-1 whitespace-nowrap text-xs"
                  >
                    <span v-if="k" class="subtle">{{ k }}</span>
                    <span class="muted truncate font-mono">{{ v }}</span>
                  </span>
                </div>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
