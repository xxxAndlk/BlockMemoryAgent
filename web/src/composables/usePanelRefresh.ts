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

  /** 开启一个新刷新周期：并发取数面板共享同一 epoch（各自 run 会互相掐掉）。 */
  function cycle(): number {
    return ++panelEpoch
  }

  async function run<T>(fn: () => Promise<T>, epoch?: number): Promise<T | undefined> {
    const e = epoch ?? ++panelEpoch
    try {
      const result = await fn()
      return e === panelEpoch ? result : undefined
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
    cycle,
    run,
    startAutoRefresh,
  }
}
