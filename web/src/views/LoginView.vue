<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useSession } from '@/stores/session'
import { ApiError } from '@/lib/api'
import DaffaMark from '@/components/brand/DaffaMark.vue'
import BaseButton from '@/components/ui/BaseButton.vue'

const session = useSession()
const router = useRouter()

const username = ref('')
const password = ref('')
const error = ref('')
const busy = ref(false)

const providers = computed(() => session.authConfig?.providers ?? [])

// The login page renders whatever the server says it supports — a password form, one button
// per identity provider, or both. Nothing about a deployment's auth is baked into the bundle.
async function submit() {
  error.value = ''
  busy.value = true
  try {
    await session.login(username.value, password.value)
    // Not a fixed route: a role that cannot see stacks would land on a page it is immediately
    // bounced off. Send them home and let the guard pick.
    router.push({ path: '/' })
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : 'Sign-in failed.'
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <main class="relative grid min-h-dvh place-items-center overflow-hidden p-6">
    <!-- The one decorative thing in the entire product, and it is on the one screen with
         nothing to operate. Light coming up through water: the mark's own two hues, blooming
         out from behind the mark itself rather than from the top of the window — a glow
         anchored to the viewport edge while the content sits centred reads as a stray vignette,
         not as light with a source.

         Everywhere past this door, colour means state. Here it is allowed to just mean Daffa. -->
    <div
      aria-hidden="true"
      class="pointer-events-none absolute inset-0"
      style="
        background:
          radial-gradient(
            ellipse 42rem 26rem at 50% 30%,
            color-mix(in oklch, var(--color-accent-500) 26%, transparent),
            transparent 70%
          ),
          radial-gradient(
            ellipse 26rem 16rem at 50% 22%,
            color-mix(in oklch, var(--color-marine-500) 14%, transparent),
            transparent 70%
          );
      "
    />

    <div class="relative w-full max-w-sm">
      <div class="mb-8 flex flex-col items-center text-center">
        <DaffaMark class="mb-5 size-16" />
        <h1 class="text-3xl font-semibold tracking-[-0.02em]">Daffa</h1>
        <!-- دفّة is the helm — the thing you steer with. Say so once, here, where there is room
             for it. It is the only place in the product that explains its own name. -->
        <p class="muted mt-1.5 text-sm">
          <span class="font-medium">دفّة</span> · the helm
        </p>
      </div>

      <div class="surface rounded-[var(--radius-card)] p-6 shadow-[var(--shadow-raised)]">
        <form v-if="session.authConfig?.local_auth" class="space-y-4" @submit.prevent="submit">
          <div>
            <label for="username" class="mb-1.5 block text-sm font-medium">Username</label>
            <input
              id="username"
              v-model="username"
              autocomplete="username"
              autofocus
              required
              class="field"
              data-cursor="text"
            />
          </div>

          <div>
            <label for="password" class="mb-1.5 block text-sm font-medium">Password</label>
            <input
              id="password"
              v-model="password"
              type="password"
              autocomplete="current-password"
              required
              class="field"
              data-cursor="text"
            />
          </div>

          <p v-if="error" class="text-sm" :style="{ color: 'var(--danger)' }" role="alert">
            {{ error }}
          </p>

          <BaseButton type="submit" intent="primary" size="md" block :loading="busy">
            {{ busy ? 'Signing in…' : 'Sign in' }}
          </BaseButton>
        </form>

        <div
          v-if="session.authConfig?.local_auth && providers.length"
          class="my-5 flex items-center gap-3"
        >
          <div class="h-px flex-1" :style="{ background: 'var(--border)' }" />
          <span class="eyebrow">or</span>
          <div class="h-px flex-1" :style="{ background: 'var(--border)' }" />
        </div>

        <!-- One button per identity provider. There may be several — a company IdP and a
             contractor one, say — so this is a list, not a single "Sign in with SSO". -->
        <BaseButton
          v-for="p in providers"
          :key="p.slug"
          intent="secondary"
          size="md"
          block
          class="mb-2 last:mb-0"
          :href="`/api/auth/oidc/start/${p.slug}`"
        >
          Sign in with {{ p.name }}
        </BaseButton>

        <p
          v-if="session.authConfig && !session.authConfig.local_auth && !providers.length"
          class="rounded-[var(--radius-control)] px-3 py-2 text-sm"
          :style="{
            background: 'var(--warn-soft)',
            border: '1px solid color-mix(in oklch, var(--warn) 30%, transparent)',
          }"
        >
          No sign-in method is configured on this server. An administrator can recover with
          <code class="font-mono text-xs">daffa admin-token</code>.
        </p>

        <p v-if="!session.authConfig" class="muted text-sm">Loading…</p>
      </div>
    </div>
  </main>
</template>
