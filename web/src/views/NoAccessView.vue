<script setup lang="ts">
import { useSession } from '@/stores/session'
import AppIcon from '@/components/ui/AppIcon.vue'
import BaseButton from '@/components/ui/BaseButton.vue'

// Somebody signed in successfully and holds no capability at all. That is a real state —
// an administrator can revoke every role from an account — and it needs a page, because
// the alternative is bouncing them between views they cannot see until the router gives
// up and shows them a blank screen or an error.
//
// Say what happened, say what to do about it, and give them the way out.
//
// The way out is not just "sign out". The person reading this is stuck behind someone else's
// decision, and the only thing that unsticks them is an administrator granting them a role.
// So the page hands them the exact thing that administrator will ask for — the identifier their
// account is filed under — rather than leaving them to guess at it from an email signature.
const session = useSession()

// A reload rather than a route push: the rail, the command palette and the router's landing
// fallback are all built from the capabilities read at boot, so the honest way to pick up a role
// granted thirty seconds ago is to boot again.
function checkAgain() {
  location.reload()
}
</script>

<template>
  <div class="mx-auto max-w-lg py-20">
    <div class="surface rounded-[var(--radius-card)] p-8 text-center">
      <div
        class="mx-auto mb-4 grid size-11 place-items-center rounded-xl"
        :style="{ background: 'var(--surface-sunken)', color: 'var(--text-subtle)' }"
      >
        <AppIcon name="users" class="size-5" />
      </div>

      <h1 class="text-base font-semibold">You have not been given a role yet</h1>

      <p class="muted mx-auto mt-2 max-w-md text-sm leading-relaxed">
        You are signed in, so your password and your account are both fine. But what you may do
        in Daffa comes from the roles you hold, and you hold none — so there is not a single page
        here you are allowed to open. This is not a fault you can fix from your side.
      </p>

      <!-- What an administrator will ask for, in mono, ready to be copied into the box on
           Settings → Access → Users. -->
      <div
        class="mt-5 rounded-[var(--radius-control)] px-4 py-3 text-left"
        :style="{ background: 'var(--surface-sunken)' }"
      >
        <p class="eyebrow">Signed in as</p>
        <p class="mt-1 font-mono text-sm">{{ session.user?.label }}</p>
        <p v-if="session.user?.email" class="subtle font-mono text-xs">
          {{ session.user.email }}
        </p>
      </div>

      <p class="muted mt-5 text-sm leading-relaxed">
        Ask an administrator to grant you a role under
        <span class="font-medium">Settings → Access → Users</span>. Tell them which cluster and what
        you need to do on it — the roles are built out of capabilities, and they can hand you
        exactly those and nothing more.
      </p>

      <div class="mt-6 flex items-center justify-center gap-2">
        <BaseButton intent="secondary" @click="checkAgain">
          <AppIcon name="restart" class="size-3.5" />
          Check again
        </BaseButton>

        <BaseButton intent="ghost" @click="session.logout()">
          <AppIcon name="logOut" class="size-3.5" />
          Sign out
        </BaseButton>
      </div>
    </div>
  </div>
</template>
