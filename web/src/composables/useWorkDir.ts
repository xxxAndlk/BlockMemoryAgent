import { ref } from 'vue'

const KEY = 'bma:last-workdir'
const workDir = ref(localStorage.getItem(KEY) ?? '')

export function useWorkDir() {
  const set = (v: string) => {
    workDir.value = v
    localStorage.setItem(KEY, v)
  }
  return { workDir, setWorkDir: set }
}
