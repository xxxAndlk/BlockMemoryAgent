import { ref } from 'vue'

const KEY = 'bma:last-workdir'
const workDir = ref(localStorage.getItem(KEY) ?? '')
// 服务端进程默认工作目录（GET /api/capabilities 的 default_work_dir）：新建会话不选目录时
// 文件就落这里。模块级 ref，会话页挂载时拉一次共享给目录选择器占位文案——否则用户在
// 新建会话面板上无从知道文件会写到哪个目录（后端 config agent.default_workdir 解析而来）。
const defaultDir = ref('')

export function useWorkDir() {
  const set = (v: string) => {
    workDir.value = v
    localStorage.setItem(KEY, v)
  }
  const setDefaultDir = (v: string) => {
    defaultDir.value = v
  }
  return { workDir, setWorkDir: set, defaultDir, setDefaultDir }
}
