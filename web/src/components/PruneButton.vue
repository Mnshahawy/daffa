<script setup lang="ts">
import { ref } from 'vue'
import { useQueryClient } from '@tanstack/vue-query'
import { bytes, daffa, type PruneTarget } from '@/lib/api'
import { toast } from '@/lib/toast'
import { Cap } from '@/lib/caps'
import { confirm } from '@/lib/confirm'
import { useSession } from '@/stores/session'
import AppIcon from './ui/AppIcon.vue'
import BaseButton from './ui/BaseButton.vue'

const props = defineProps<{ target: PruneTarget; label: string }>()

const session = useSession()
const qc = useQueryClient()
const busy = ref(false)
const result = ref('')

// system.prune is its own capability, not a side effect of being able to remove one image.
// Sweeping every unused image, volume and network on a host is a different decision from
// deleting a thing you are looking at, so it is a different permission.
const visible = () => session.can(Cap.SystemPrune)

// What each target actually deletes, said plainly, at the moment of deciding.
//
// "Prune" is a word that hides a lot. The difference between "dangling images" and "every
// unused image" is the difference between reclaiming junk and forcing a re-pull of your whole
// registry the next time you need to roll back.
const explanations: Record<PruneTarget, string> = {
  images: 'Deletes untagged (dangling) images. Tagged images are kept, even if nothing uses them.',
  containers: 'Deletes every stopped container. Their logs and writable layers go with them.',
  networks: 'Deletes networks that no container is attached to.',
  volumes:
    'Deletes unused ANONYMOUS volumes. Named volumes are never touched — those hold data you meant to keep.',
  'build-cache': 'Deletes unused build cache layers.',
}

async function run() {
  const ok = await confirm({
    title: `${props.label}?`,
    body: explanations[props.target],
    confirmLabel: props.label,
    // A prune is unrecoverable and it operates on things you are not looking at, which is
    // exactly the combination that deserves a solid red button.
    intent: 'danger',
  })
  if (!ok) return

  busy.value = true
  result.value = ''
  try {
    const r = await daffa.prune(session.envId, props.target)
    // The freed total is the whole point of running this, so it stays put on the row rather
    // than sliding away on a toast timer — it is a value to read, not a receipt to glance at.
    result.value =
      r.deleted === 0
        ? 'Nothing to prune.'
        : `Removed ${r.deleted}${r.freed ? `, freed ${bytes(r.freed)}` : ''}.`
    await qc.invalidateQueries()
  } catch (e) {
    toast.err(e, 'Prune failed.')
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div v-if="visible()" class="flex items-center gap-3">
    <!-- What it freed. The only reason anybody runs this. -->
    <span v-if="result" class="muted text-xs">{{ result }}</span>

    <BaseButton intent="danger" size="sm" :loading="busy" @click="run">
      <AppIcon v-if="!busy" name="trash" class="size-3.5" />
      {{ label }}
    </BaseButton>
  </div>
</template>
