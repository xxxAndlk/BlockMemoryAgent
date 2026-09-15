import { ref } from 'vue'
import type { SessionSummary } from '@/types'
import { listSessions } from '@/api/session'

export function useSessionList() {
  const sessions = ref<SessionSummary[]>([])

  async function loadSessions() {
    try {
      sessions.value = (await listSessions()) || []
    } catch {
      sessions.value = []
    }
  }

  return { sessions, loadSessions }
}
