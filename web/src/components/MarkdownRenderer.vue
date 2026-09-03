<script setup lang="ts">
import { computed } from 'vue'
import { renderMd } from '@/utils/markdown'

const props = defineProps<{
  content: string | null | undefined
}>()

const html = computed(() => renderMd(props.content))
</script>

<template>
  <div class="markdown-body" v-html="html"></div>
</template>

<style scoped>
.markdown-body :deep(p) { margin: 0; }
.markdown-body :deep(pre) {
  background: var(--bma-page);
  padding: 8px;
  border-radius: 4px;
  margin-top: 4px;
  overflow-x: auto;
  color: var(--bma-text);
}
.markdown-body :deep(code) { font-family: monospace; }
.markdown-body :deep(a) { color: var(--bma-primary); }
</style>
