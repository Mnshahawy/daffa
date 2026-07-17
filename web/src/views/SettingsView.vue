<script setup lang="ts">
import { computed } from 'vue'
import { RouterLink, RouterView, useRoute } from 'vue-router'
import { settingsGroups } from '@/lib/nav'
import { useSession } from '@/stores/session'
import AppIcon from '@/components/ui/AppIcon.vue'
import PageHeader from '@/components/ui/PageHeader.vue'

const route = useRoute()
const session = useSession()

// Settings are the things you configure once and then forget: who may use Daffa, the hosts it
// can reach, the registries it can pull from, the buckets it can write to. None of them belong
// in a navigation you look at every day.
//
// They used to be nine flat tabs — a list, not a structure, with "Users" and "Storage" side by
// side as though they were the same kind of decision. The groups are the three questions
// Settings actually answers: who gets in, what Daffa can reach, and when it should speak up.
//
// canAnywhere, not can: a settings page is not about the host you happen to have selected. An
// operator scoped to staging who can see git credentials should find the tab regardless of
// where the switcher is pointing.
const groups = computed(() =>
  settingsGroups
    .map((g) => ({ ...g, items: g.items.filter((t) => session.canAnywhere(t.cap)) }))
    .filter((g) => g.items.length),
)
</script>

<template>
  <div>
    <PageHeader title="Settings" description="Configured once, then forgotten about." />

    <div class="flex flex-col gap-6 md:flex-row">
      <nav class="shrink-0 space-y-5 md:w-60">
        <div v-for="g in groups" :key="g.title">
          <div class="eyebrow px-2.5 pb-1.5">{{ g.title }}</div>

          <RouterLink
            v-for="t in g.items"
            :key="t.name"
            :to="{ name: t.name }"
            class="relative flex items-start gap-2.5 rounded-[var(--radius-control)] px-2.5 py-2 transition"
            :class="
              route.name === t.name
                ? 'font-medium'
                : 'muted hover:bg-[var(--surface-sunken)] hover:text-[var(--text)]'
            "
            :style="
              route.name === t.name
                ? { background: 'var(--accent-soft)', color: 'var(--accent-text)' }
                : undefined
            "
          >
            <AppIcon :name="t.icon" class="mt-0.5 size-4 shrink-0" />
            <span class="min-w-0">
              <span class="block text-sm">{{ t.label }}</span>
              <span class="subtle block text-xs leading-snug">{{ t.hint }}</span>
            </span>
          </RouterLink>
        </div>
      </nav>

      <div class="min-w-0 flex-1">
        <RouterView />
      </div>
    </div>
  </div>
</template>
