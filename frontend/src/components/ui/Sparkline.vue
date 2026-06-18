<script setup lang="ts">
import { computed } from 'vue'

const props = defineProps<{
  data: number[]
  color?: string
  fill?: boolean
  height?: number
  strokeWidth?: number
}>()

const h = computed(() => props.height || 40)
const w = computed(() => h.value * 4)
const pad = 4
const min = computed(() => Math.min(...props.data, 0))
const max = computed(() => Math.max(...props.data, 1))
const pts = computed(() => {
  const n = props.data.length
  if (n === 0) return ''
  const step = (w.value - pad * 2) / (n - 1)
  const range = max.value - min.value || 1
  return props.data.map((v, i) => {
    const x = pad + i * step
    const y = h.value - pad - ((v - min.value) / range) * (h.value - pad * 2)
    return `${x},${y}`
  }).join(' ')
})
const area = computed(() => {
  if (!props.fill || !pts.value) return ''
  return `${pts.value} ${w.value - pad},${h.value} ${pad},${h.value}`
})
</script>

<template>
  <svg class="sparkline" :width="w" :height="h" viewBox="0 0 160 40" preserveAspectRatio="none">
    <polygon v-if="fill" :points="area" :fill="color || '#3b82f6'" fill-opacity="0.12" />
    <polyline
      :points="pts"
      :stroke="color || '#3b82f6'"
      :stroke-width="strokeWidth || 2"
      fill="none"
      stroke-linecap="round"
      stroke-linejoin="round"
    />
  </svg>
</template>

<style scoped>
.sparkline { display: block; }
</style>
