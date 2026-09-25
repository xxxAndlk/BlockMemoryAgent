/**
 * notifications.ts 浏览器通知（TODO #18-5 T32）：
 * 页面在后台（document.hidden）时收到会话终态 → 系统级 Notification 提醒，
 * 用户切回来就能看到结果；前台时不需要（页面自己就是焦点）。
 * 权限申请按钮在设置页；未授权/被拒绝时静默跳过，零打扰。
 */

/**
 * 终态集合：会话真正结束的状态（运行中/等待澄清/挂起等子/暂停于子都不算）。
 * 两处判定共用：终态通知（这些状态值得提醒，挂起态不打扰）、
 * SSE `done` 帧收口（api/session.ts：非终态 done 是旧后端的挂起态误报，必须续连而非收口）。
 */
export const TERMINAL_STATUSES = new Set(['completed', 'error', 'cancelled'])

/** 浏览器是否支持 Notification API。 */
export function notificationSupported(): boolean {
  return typeof window !== 'undefined' && 'Notification' in window
}

/** 当前权限状态：unsupported / default / granted / denied。 */
export function notificationPermission(): 'unsupported' | NotificationPermission {
  if (!notificationSupported()) return 'unsupported'
  return Notification.permission
}

/** 申请通知权限（设置页按钮调用）；返回申请后的权限状态。 */
export async function requestNotificationPermission(): Promise<'unsupported' | NotificationPermission> {
  if (!notificationSupported()) return 'unsupported'
  try {
    return await Notification.requestPermission()
  } catch {
    return Notification.permission
  }
}

/**
 * 会话终态提醒（session/index.vue onDone / onSnapshot 终态时调用）：
 * 仅当页面在后台 ∧ 状态是终态 ∧ 已授权 时弹出，tag=会话 ID 天然去重。
 */
export function maybeNotifySessionDone(status: string | undefined, goal: string | undefined) {
  if (!notificationSupported()) return
  if (Notification.permission !== 'granted') return
  if (!document.hidden) return
  if (!status || !TERMINAL_STATUSES.has(status)) return
  const title = status === 'completed' ? '✅ 会话已完成' : status === 'error' ? '❌ 会话执行失败' : '⏹ 会话已终止'
  const body = (goal || '').slice(0, 120) || '点击回到页面查看结果'
  try {
    const n = new Notification(title, { body, tag: 'bma-session-done', silent: false })
    // 点击聚焦页面（焦点事件本身会把面板刷一遍，无需携带路由）
    n.onclick = () => {
      window.focus()
      n.close()
    }
  } catch {
    // 个别浏览器在无 Service Worker 时构造可能抛错：通知是锦上添花，静默失败
  }
}

/**
 * 待澄清提醒（2026-09-20）：Agent 挂起等用户回答时页面在后台 → 系统级提醒，
 * 避免 ask_user 超时自行决策用户毫无感知（此前只有会话终态才通知）。
 * 同一 questionId 只弹一次（tag 去重）；前台不打扰。
 */
export function maybeNotifyClarifyWaiting(questionId: string, question: string) {
  if (!notificationSupported()) return
  if (Notification.permission !== 'granted') return
  if (!document.hidden) return
  if (!questionId) return
  const body = (question || '').replace(/^Agent 提问[:：]\s*/, '').slice(0, 80) || '点击回到页面回答'
  try {
    const n = new Notification('❓ AI 等待你的回答', { body, tag: 'bma-clarify-' + questionId, silent: false })
    n.onclick = () => {
      window.focus()
      n.close()
    }
  } catch {
    // 同 maybeNotifySessionDone：通知是锦上添花，静默失败
  }
}
