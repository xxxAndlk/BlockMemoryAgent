// clarifySubmit.ts 澄清答复的韧性提交（2026-09-12 实证回归）。
//
// 背景：后端重启窗口内点「提交全部答案」，前端只弹一句
// 「提交答复失败：Failed to fetch」——用户既不知道答案有没有送出去，也不知道该等还是
// 该重来（本机部署实测重启约 1 分钟：技能库重载 + 插件子进程拉起）。
//
// 两类失败必须分开处理：
//   - NetworkError：请求没到达/响应没回来（服务正在重启、连接被掐）。**可重试**，
//     且重试前先问服务端"到底收到没有"，避免把已落地的答复再发一遍；
//   - APIError（400/409）：服务端明确拒绝（答复数量不符、会话已不在待澄清）。重试无用，
//     但若拒绝原因是"会话已不在待澄清"，说明上一次其实已经落地 → 按已提交处理。
//
// 这样最坏情况也只是"多等一会儿"，不会静默丢答复、也不会重复提交。

import { APIError, NetworkError } from '@/api/client'
import { clarifySession, clarifySessionBatch, getSession } from '@/api/session'

/** 重试节奏（毫秒）：本机部署的一次后端重启实测约 60s（P2 技能库 + 插件加载），
 *  故总窗口按 ~90s 设计；连接被拒是瞬时失败，退避不放大用户等待。 */
const RETRY_DELAYS = [2000, 3000, 5000, 8000, 10000, 10000, 10000, 10000, 10000]
/** 总时长上限：超时即放弃（避免用户面对一个永远转圈的提交按钮）。 */
const MAX_TOTAL_MS = 150000

export type ClarifyOutcome = 'ok' | 'confirmed'

export interface ClarifySubmitHooks {
  /** 即将重试（attempt 从 1 开始）：调用方据此提示「正在重连」。 */
  onRetry?: (attempt: number, total: number) => void
  /** 返回 true 表示调用方已销毁（组件卸载/切走）：停止重试并放弃，不再打扰用户。 */
  aborted?: () => boolean
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

/** 服务端是否已不在待澄清（= 答复已落地）。查询失败按"未知"处理，交给重试兜底。 */
async function alreadyApplied(sessionId: string): Promise<boolean> {
  try {
    const s = await getSession(sessionId)
    return !!s && s.status !== 'awaiting_clarify'
  } catch {
    return false
  }
}

/**
 * 提交澄清答复（单题或批量），网络层失败自动重试。
 *
 * 返回 'ok'=本次提交成功；'confirmed'=请求可能已丢失，但服务端状态显示答复已落地
 *（会话已继续），调用方按成功处理。
 */
export async function submitClarify(
  sessionId: string,
  payload: { answer?: string; answers?: string[] },
  hooks: ClarifySubmitHooks = {},
): Promise<ClarifyOutcome> {
  const post = () =>
    payload.answers && payload.answers.length
      ? clarifySessionBatch(sessionId, payload.answers)
      : clarifySession(sessionId, payload.answer || '')

  const startedAt = Date.now()
  for (let attempt = 0; ; attempt++) {
    try {
      await post()
      return 'ok'
    } catch (e) {
      if (e instanceof APIError) {
        // 业务拒绝：只有"会话已不在待澄清"这一种意味着上一步其实成功了。
        if (await alreadyApplied(sessionId)) return 'confirmed'
        throw e
      }
      if (!(e instanceof NetworkError)) throw e
      // 连接层失败：先确认服务端是否已收到（响应丢失场景下重发会变成二次答复）。
      if (await alreadyApplied(sessionId)) return 'confirmed'
      const outOfRetries = attempt >= RETRY_DELAYS.length
      const timedOut = Date.now() - startedAt > MAX_TOTAL_MS
      if (outOfRetries || timedOut || hooks.aborted?.()) throw e
      hooks.onRetry?.(attempt + 1, RETRY_DELAYS.length)
      await sleep(RETRY_DELAYS[attempt])
      if (hooks.aborted?.()) throw e
    }
  }
}
