<script setup lang="ts">
import { computed, ref } from 'vue'
import { useQuery, useQueryClient } from '@tanstack/vue-query'
import { ApiError, daffa, type LogConfigRequest } from '@/lib/api'
import { Cap } from '@/lib/caps'
import { useSession } from '@/stores/session'
import LogConfigForm from '@/components/LogConfigForm.vue'

const session = useSession()
const qc = useQueryClient()

// The fleet default is one setting, so changing it takes logging.edit EVERYWHERE —
// the same reasoning as the monitor sampling settings.
const canEditFleet = computed(() => session.can(Cap.LoggingEdit, ''))

const { data: config, isLoading } = useQuery({
  queryKey: ['log-config'],
  queryFn: daffa.globalLogConfig,
})

const error = ref('')
const busy = ref(false)

async function save(body: LogConfigRequest) {
  busy.value = true
  error.value = ''
  try {
    await daffa.saveGlobalLogConfig(body)
    await qc.invalidateQueries({ queryKey: ['log-config'] })
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : 'Could not save.'
  } finally {
    busy.value = false
  }
}

async function clear() {
  busy.value = true
  error.value = ''
  try {
    await daffa.clearGlobalLogConfig()
    await qc.invalidateQueries({ queryKey: ['log-config'] })
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : 'Could not clear.'
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div>
    <div class="mb-5">
      <h2 class="text-base font-semibold">Container logs</h2>
      <p class="muted mt-0.5 max-w-2xl text-sm leading-relaxed">
        The default log driver and rotation for deployed stacks — Docker's own
        <span class="font-mono text-xs">max-size</span> /
        <span class="font-mono text-xs">max-file</span> rotation is what keeps container
        logs from filling the host's disk.
      </p>
    </div>

    <p
      v-if="error"
      role="alert"
      class="mb-4 rounded-[var(--radius-control)] px-3 py-2 text-sm"
      :style="{ background: 'var(--danger-soft)', color: 'var(--danger)' }"
    >
      {{ error }}
    </p>

    <section class="surface rounded-[var(--radius-card)] p-5">
      <h3 class="text-sm font-semibold">Fleet default</h3>
      <p class="muted mb-4 mt-1 max-w-[70ch] text-sm leading-relaxed">
        Applied to services that don't declare their own
        <span class="font-mono text-xs">logging:</span>, at their next deploy — a service's
        own block always wins, and nothing restarts just because this changed. A host can
        override it on its Host page. When unset, the daemon's default applies, which is
        typically unbounded.
      </p>

      <p v-if="isLoading" class="muted text-sm">Loading…</p>
      <LogConfigForm
        v-else
        :model-value="config ?? null"
        :disabled="!canEditFleet"
        :busy="busy"
        clear-label="Unset the default"
        @save="save"
        @clear="clear"
      />
    </section>
  </div>
</template>
