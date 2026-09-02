<script setup lang="ts">
import { ref, watch } from 'vue'
import { browseFS, type BrowseResult } from '@/api/fs'

const props = defineProps<{ modelValue: string }>()
const emit = defineEmits<{ 'update:modelValue': [string] }>()
const open = ref(false)
const current = ref('')
const parent = ref('')
const dirs = ref<BrowseResult['dirs']>([])
const pick = ref('')

async function load(p: string) {
  const r = await browseFS(p)
  current.value = r.path
  parent.value = r.parent
  dirs.value = r.dirs
  pick.value = ''
}
watch(open, (v) => { if (v) load(current.value || props.modelValue || '') })
function confirm() {
  emit('update:modelValue', pick.value || current.value)
  open.value = false
}
</script>

<template>
  <div class="workdir-picker">
    <el-input :model-value="modelValue" placeholder="工作目录(留空=默认)" clearable
      @update:model-value="$emit('update:modelValue', $event)">
      <template #append><el-button @click="open = true">浏览</el-button></template>
    </el-input>
    <el-dialog v-model="open" title="选择工作目录" width="520px">
      <div class="browse-head">
        <el-button size="small" :disabled="!parent" @click="load(parent)">上级</el-button>
        <span class="cur">{{ current || '选择盘符' }}</span>
      </div>
      <el-scrollbar max-height="320px">
        <div v-for="d in dirs" :key="d.path" class="dir-item" @dblclick="load(d.path)" @click="pick = d.path"
             :class="{ active: pick === d.path }">{{ d.name }}</div>
        <el-empty v-if="!dirs.length" description="无子目录" :image-size="40" />
      </el-scrollbar>
      <template #footer>
        <el-button @click="open = false">取消</el-button>
        <el-button type="primary" :disabled="!pick && !current" @click="confirm">选定 {{ pick || current }}</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.browse-head { display: flex; gap: 8px; align-items: center; margin-bottom: 8px; }
.dir-item { padding: 4px 8px; cursor: pointer; border-radius: 4px; }
.dir-item:hover { background: var(--el-fill-color-light); }
.dir-item.active { background: var(--el-color-primary-light-8); }
</style>
