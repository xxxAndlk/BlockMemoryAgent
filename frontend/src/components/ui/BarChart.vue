<script setup lang="ts">
import { computed } from 'vue'

interface BarItem {
  label: string
  value: number
  color?: string
}

const props = defineProps<{
  data: BarItem[]
  height?: number
}>()

const h = computed(() => props.height || 80)
const max = computed(() => Math.max(...props.data.map(d => d.value), 1))
const rowHeight = computed(() => h.value / props.data.length)
</script>

<template>
  <svg class="bar-chart" width="100%" :height="h">
    <g v-for="(d, i) in data" :key="d.label" :transform="`translate(0, ${i * rowHeight})`">
      <rect
        :x="0" :y="2" :height="rowHeight - 4"
        :width="`${(d.value / max) * 100}%`"
        :fill="d.color || '#3b82f6'"
        rx="2"
      />
      <text :x="4" :y="rowHeight/2 + 4" fill="#e2e8f0" font-size="10">{{ d.label }}</text>
      <text
        :x="`${Math.max((d.value / max) * 100, 12)}%`"
        :y="rowHeight/2 + 4"
        fill="#94a3b8"
        font-size="9"
        dx="4"
      >{{ d.value }}</text>
    </g>
  </svg>
</template>

<style scoped>
.bar-chart { display: block; }
text { font-family: inherit; }
</style>
