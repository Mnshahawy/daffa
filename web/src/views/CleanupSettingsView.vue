<script setup lang="ts">
import { computed, ref } from 'vue'
import { useQuery, useQueryClient } from '@tanstack/vue-query'
import { daffa, type CleanupPolicyRequest } from '@/lib/api'
import { Cap } from '@/lib/caps'
import { useSession } from '@/stores/session'
import { toast } from '@mnshahawy/daffa-console-ui'
import CleanupPolicyForm from '@/components/CleanupPolicyForm.vue'

const session = useSession()
const qc = useQueryClient()

// The fleet policy is one setting that deletes things on every host, so changing it takes
// system.prune EVERYWHERE — the same reasoning as the fleet log defaults.
const canEditFleet = computed(() => session.can(Cap.SystemPrune, ''))

const { data: policy, isLoading } = useQuery({
  queryKey: ['cleanup-policy'],
  queryFn: daffa.globalCleanup,
})

const busy = ref(false)

async function save(body: CleanupPolicyRequest) {
  busy.value = true
  try {
    await daffa.saveGlobalCleanup(body)
    await qc.invalidateQueries({ queryKey: ['cleanup-policy'] })
    toast.ok('Cleanup policy saved.')
  } catch (e) {
    toast.err(e, 'Could not save.')
  } finally {
    busy.value = false
  }
}

async function clear() {
  busy.value = true
  try {
    await daffa.clearGlobalCleanup()
    await qc.invalidateQueries({ queryKey: ['cleanup-policy'] })
    toast.ok('Cleanup policy cleared.')
  } catch (e) {
    toast.err(e, 'Could not clear.')
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div>
    <div class="mb-5">
      <h2 class="text-base font-semibold">Automatic cleanup</h2>
      <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
        A host you deploy to accumulates the superseded image of every release, the stopped
        containers of previous deployments and a build cache that never shrinks. This sweeps
        them on a schedule — with an age floor, so what the last few releases need survives.
      </p>
    </div>

    <section class="surface rounded-[var(--radius-card)] p-5">
      <h3 class="text-sm font-semibold">Fleet default</h3>
      <p class="muted mb-4 mt-1 max-w-[70ch] text-sm leading-relaxed">
        Applied to every host that has no policy of its own; a host can override it — or opt
        out of sweeping entirely — on its Host page. When unset, nothing is ever deleted
        automatically anywhere.
      </p>

      <p v-if="isLoading" class="muted text-sm">Loading…</p>
      <CleanupPolicyForm
        v-else
        :model-value="policy ?? null"
        :disabled="!canEditFleet"
        :busy="busy"
        clear-label="Unset the default"
        @save="save"
        @clear="clear"
      />
    </section>
  </div>
</template>
