<script setup lang="ts">
import { useId } from 'vue'

// The rudder. See public/mark.svg for what it is and why it survives 16px.
//
// The gradient and the mask need ids, and two of these on one page with the same ids would
// have the second one silently reference the first one's defs. useId() is per-instance.
const props = withDefaults(
  defineProps<{
    /**
     * `brand` paints the mark in the depth gradient — turquoise at the waterline, indigo
     * below. For the places the product introduces itself: the rail, the sign-in page.
     *
     * `current` paints it in currentColor, for anywhere it is just an icon and has to agree
     * with the text beside it.
     */
    tone?: 'brand' | 'current'
  }>(),
  { tone: 'brand' },
)

const uid = useId()
const gradientId = `daffa-depth-${uid}`
const maskId = `daffa-waterline-${uid}`
</script>

<template>
  <svg viewBox="0 0 32 32" fill="none" role="img" aria-label="Daffa">
    <defs>
      <linearGradient
        v-if="props.tone === 'brand'"
        :id="gradientId"
        x1="4"
        y1="2"
        x2="26"
        y2="30"
        gradientUnits="userSpaceOnUse"
      >
        <stop offset="0" stop-color="var(--color-marine-400)" />
        <stop offset="0.55" stop-color="var(--color-accent-600)" />
        <stop offset="1" stop-color="var(--color-accent-700)" />
      </linearGradient>

      <mask :id="maskId">
        <rect width="32" height="32" fill="#fff" />
        <rect y="14.9" width="32" height="2.2" fill="#000" />
      </mask>
    </defs>

    <path
      :fill="props.tone === 'brand' ? `url(#${gradientId})` : 'currentColor'"
      :mask="`url(#${maskId})`"
      fill-rule="evenodd"
      d="M5 3h11c7.2 0 12.5 5.6 12.5 13S23.2 29 16 29H5zm6 6v14h4.5c4 0 7-2.8 7-7s-3-7-7-7z"
    />
  </svg>
</template>
