<script setup lang="ts">
import { computed, watchEffect } from 'vue'
import { useRoute } from 'vue-router'
import { useQuery } from '@tanstack/vue-query'
import { daffa } from '@/lib/api'
import { setTitle } from '@/lib/title'
import { useSession } from '@/stores/session'
import { containerStatus } from '@/lib/status'
import PageHeader from '@/components/ui/PageHeader.vue'
import StatusPill from '@/components/ui/StatusPill.vue'
import ContainerPanel from '@/components/ContainerPanel.vue'

// The page is a header around ContainerPanel, which owns the stats/logs/shell surface —
// the same panel a stack's Containers tab embeds. This wrapper is what has a URL.
const route = useRoute()
const session = useSession()
const id = computed(() => route.params.id as string)

// WHICH MACHINE this container is on.
//
// A container id is unique per DAEMON, not per cluster, so on a Swarm every request about one has
// to say which node it means. The list tags each row with its node and puts it in the link, so the
// value is simply here — and it is absent on a standalone environment (and a single-node swarm),
// where there is one possible answer and the server fills it in.
//
// This is also the whole of cross-node exec: the server holds a tunnel per node, so naming the node
// IS the routing. Portainer needed a gossiping agent mesh and two HTTP headers for the same trick.
const node = computed(() => (route.query.node as string) || undefined)

// Deduplicated with the panel's identical query by key — one request serves both.
const { data: inspect } = useQuery({
  queryKey: ['container', () => session.envId, () => id.value, () => node.value],
  queryFn: () => daffa.container(session.envId, id.value, node.value),
  enabled: computed(() => !!session.envId && !!id.value),
})

const name = computed(() => {
  const n = (inspect.value?.Name as string) ?? ''
  return n.replace(/^\//, '') || id.value.slice(0, 12)
})

// Name the tab after the container. This is the page most likely to be one of six open at once
// while something is on fire, and "Daffa" on all six is no help at all.
watchEffect(() => setTitle(name.value, 'Containers'))

const state = computed(
  () => ((inspect.value?.State as Record<string, unknown>)?.Status as string) ?? '',
)

// Inspect gives the exit code as a number; containerStatus reads it out of Docker's
// "Exited (137)" phrasing, so hand it that shape. It is the difference between "Completed"
// and "Exited · code 137", which is the difference between a finished migration and an
// OOM kill.
const status = computed(() => {
  const code = (inspect.value?.State as Record<string, unknown>)?.ExitCode
  return containerStatus(state.value, typeof code === 'number' ? `(${code})` : undefined)
})
</script>

<template>
  <div>
    <!-- A detail page with no way back up is a dead end: this container is reachable from the
         list, from the overview and from a stack, and it used to lead back to none of them. -->
    <PageHeader :title="name" :crumbs="[{ label: 'Containers', to: { name: 'containers' } }]">
      <template #actions>
        <StatusPill v-if="state" :status="status" />
      </template>
    </PageHeader>

    <ContainerPanel :env="session.envId" :id="id" :node="node" />
  </div>
</template>
