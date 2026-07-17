<script setup lang="ts">
import { computed } from 'vue'
import { RouterLink } from 'vue-router'
import type { RouteLocationRaw } from 'vue-router'

/**
 * Every button in Daffa. The `intent` says what KIND of action this is, and the colour
 * follows from that — see the action grammar in style.css.
 *
 * `loading` is a prop rather than something each call site reinvents, because thirty
 * hand-rolled spinners is thirty chances for one of them to forget to also disable the
 * button, and a deploy fired twice because the first click looked like it did nothing is
 * a real outage.
 */
const props = withDefaults(
  defineProps<{
    intent?: 'primary' | 'secondary' | 'ghost' | 'caution' | 'danger' | 'danger-solid' | 'link'
    size?: 'xs' | 'sm' | 'md'
    /** Square. For a button whose whole content is one icon. Needs `label` for a11y. */
    icon?: boolean
    /** Required when `icon` — an icon-only button with no accessible name is a button nobody can use. */
    label?: string
    loading?: boolean
    disabled?: boolean
    block?: boolean
    type?: 'button' | 'submit' | 'reset'
    /** Renders a RouterLink instead, styled identically. A link that looks like a button should BE a link. */
    to?: RouteLocationRaw
    /** Renders an <a>. For the OIDC start URLs, which leave the SPA. */
    href?: string
  }>(),
  { intent: 'secondary', size: 'sm', type: 'button' },
)

const classes = computed(() => [
  'btn',
  `btn-${props.intent}`,
  `btn-${props.size}`,
  props.icon && 'btn-icon',
  props.block && 'w-full',
])

const isDisabled = computed(() => props.disabled || props.loading)
</script>

<template>
  <RouterLink v-if="to && !isDisabled" :to="to" :class="classes" :aria-label="label">
    <slot />
  </RouterLink>

  <a v-else-if="href && !isDisabled" :href="href" :class="classes" :aria-label="label">
    <slot />
  </a>

  <button
    v-else
    :type="type"
    :class="classes"
    :disabled="isDisabled"
    :aria-label="label"
    :aria-busy="loading || undefined"
  >
    <!-- The spinner replaces the icon slot rather than joining it, so the button does not
         change width mid-action and shove the row around. -->
    <svg
      v-if="loading"
      class="size-3.5 shrink-0 animate-spin"
      viewBox="0 0 16 16"
      fill="none"
      aria-hidden="true"
    >
      <circle cx="8" cy="8" r="6.5" stroke="currentColor" stroke-width="2" opacity="0.25" />
      <path
        d="M8 1.5A6.5 6.5 0 0 1 14.5 8"
        stroke="currentColor"
        stroke-width="2"
        stroke-linecap="round"
      />
    </svg>
    <slot />
  </button>
</template>
