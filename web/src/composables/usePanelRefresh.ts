import { ref, onUnmounted } from 'vue'
import { APP_CONFIG } from '@/config/app'

export function usePanelRefresh() {
  const panelTimer = ref<ReturnType<typeof setInterval> | null>(null)
  let panelEpoch = 0

  function stopPanelTimer() {
    if (panelTimer.value !== null) {
      clearInterval(panelTimer.value)
      panelTimer.value = null
    }
  }

  function invalidate() {
    panelEpoch++
  }

  async function run<T>(fn: () => Promise<T>): Promise<T | undefined> {
    const epoch = ++panelEpoch
    try {
      const result = await fn()
      return epoch === panelEpoch ? result : undefined
    } catch {
      return undefined
    }
  }

  function startAutoRefresh(sessionId: string, refreshFn: (id: string) => Promise<void>) {
    stopPanelTimer()
    refreshFn(sessionId)
    panelTimer.value = setInterval(() => refreshFn(sessionId), APP_CONFIG.panelRefreshInterval)
  }

  onUnmounted(stopPanelTimer)

  return {
    panelTimer,
    stopPanelTimer,
    invalidate,
    run,
    startAutoRefresh,
  }
}
