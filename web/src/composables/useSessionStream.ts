import { ref, onUnmounted } from 'vue'
import type { Session, SessionEvent } from '@/types'
import { streamSession, type LiveTextFrame } from '@/api/session'

function isSnapshot(ev: SessionEvent): boolean {
  return (
    typeof ev === 'object' &&
    ev !== null &&
    'id' in ev &&
    'goal' in ev &&
    'events' in ev
  )
}

export interface StreamHandlers {
  onSnapshot?: (snap: Session) => void
  onEvent?: (ev: SessionEvent) => void
  onLive?: (d: LiveTextFrame) => void
  onDone?: (finalStatus?: string) => void
  onError?: (err: Error) => void
}

export function useSessionStream() {
  const closeStream = ref<(() => void) | null>(null)

  function startStream(sessionId: string, handlers: StreamHandlers) {
    closeStream.value?.()
    closeStream.value = streamSession(
      sessionId,
      (ev) => {
        if (isSnapshot(ev)) {
          handlers.onSnapshot?.(ev as unknown as Session)
        } else {
          handlers.onEvent?.(ev)
        }
      },
      handlers.onDone,
      handlers.onError,
      handlers.onLive
    )
  }

  function close() {
    closeStream.value?.()
    closeStream.value = null
  }

  onUnmounted(close)

  return { closeStream, startStream, close }
}
