import type { Session, SessionEvent, AgentNode } from '@/types'

const API_BASE = '/api'

async function fetchJson<T>(url: string, options?: RequestInit): Promise<T> {
  const r = await fetch(`${API_BASE}${url}`, {
    headers: { 'Content-Type': 'application/json' },
    ...options,
  })
  if (!r.ok) throw new Error(`${r.status} ${r.statusText}`)
  return r.json() as Promise<T>
}

export function listSessions(): Promise<Session[]> {
  return fetchJson('/sessions')
}

export function createSession(goal: string): Promise<Session> {
  return fetchJson('/sessions', {
    method: 'POST',
    body: JSON.stringify({ goal }),
  })
}

export function getSession(id: string): Promise<Session> {
  return fetchJson(`/sessions/${id}`)
}

export function getSessionBoard(id: string): Promise<{ session_id: string; board: any }> {
  return fetchJson(`/sessions/${id}/board`)
}

export function getSessionAgents(id: string): Promise<{ session_id: string; agents: AgentNode[]; tree: AgentNode[] }> {
  return fetchJson(`/sessions/${id}/agents`)
}

export function sendMessage(id: string, content: string): Promise<void> {
  return fetchJson(`/sessions/${id}/message`, {
    method: 'POST',
    body: JSON.stringify({ content }),
  })
}

export function streamSession(
  id: string,
  onEvent: (ev: SessionEvent) => void,
  onDone?: () => void,
  onError?: (err: Error) => void
): () => void {
  const es = new EventSource(`${API_BASE}/sessions/${id}/stream`)
  es.onmessage = (e) => {
    try {
      const d = JSON.parse(e.data)
      if (d.type === 'done') {
        es.close()
        onDone?.()
        return
      }
      onEvent(d as SessionEvent)
    } catch (err) {
      onError?.(err as Error)
    }
  }
  es.onerror = () => {
    onError?.(new Error('SSE error'))
  }
  return () => es.close()
}
