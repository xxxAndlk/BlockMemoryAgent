import { ref } from 'vue'
import type { Session } from '@/types'
import { listSessions } from '@/api/session'

export function useSessionList() {
  const sessions = ref<Session[]>([])

  async function loadSessions() {
    try {
      sessions.value = (await listSessions()) || []
    } catch {
      sessions.value = []
    }
  }

  return { sessions, loadSessions }
}
