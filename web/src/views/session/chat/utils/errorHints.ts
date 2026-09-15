/**
 * errorHints.ts 失败说人话（TODO #15 翻译层⑤ T11）：
 * 把工具/请求的原始报错翻译成可行动的提示。识别超时、限流、网络、鉴权等
 * 高频失败模式，给出"下一步怎么办"；集群档建议（escalate）仅在耗时敏感的
 * 快速档失败场景出现——升集群档换更完整的多步执行。
 */

export interface ErrorHint {
  /** 给用户看的可行动提示（中文，说人话） */
  text: string
  /** true 时 UI 追加"升集群档重试"按钮（联动 setSessionGear(id, 'cluster')） */
  escalate?: boolean
}

/** 从原始报错文本推断可行动提示；未识别返回 null（按原样展示）。 */
export function errorHint(raw: string | undefined | null): ErrorHint | null {
  const s = (raw || '').toLowerCase()
  if (!s) return null

  // 限流/配额：等一等比立刻重试有效。
  if (s.includes('429') || s.includes('rate limit') || s.includes('quota') || s.includes('too many requests')) {
    return { text: '模型服务限流了，稍等一两分钟再继续即可；频繁出现可在设置里更换模型。' }
  }
  // 超时：任务可能太重，建议升集群档（多步拆解比单次长等待更稳）。
  if (s.includes('timeout') || s.includes('timed out') || s.includes('deadline') || s.includes('超时')) {
    return { text: '这一步超时了。任务偏重时单次等待容易失败——升到集群档后由多个子 Agent 分步完成更稳。', escalate: true }
  }
  // 网络层失败：请求根本没到服务端（本机部署重启窗口 ≈1 分钟）。
  if (s.includes('failed to fetch') || s.includes('networkerror') || s.includes('econnrefused') ||
      s.includes('connection refused') || s.includes('dial tcp') || s.includes('no such host')) {
    return { text: '网络请求没有到达服务端。如果是本机部署，服务可能正在重启（约 1 分钟），稍候自动重连即可。' }
  }
  // 鉴权：key 缺失/失效。
  if (s.includes('401') || s.includes('unauthorized') || s.includes('api key') || s.includes('invalid api')) {
    return { text: '模型鉴权失败（API Key 缺失或失效）。请到设置里检查模型 Key，或运行安装向导补填。' }
  }
  // 上游 5xx：服务端临时故障。
  if (s.includes('502') || s.includes('503') || s.includes('504') || s.includes('bad gateway') || s.includes('service unavailable')) {
    return { text: '模型服务暂时不可用（上游 5xx），一般几分钟内恢复；可稍后重试或先切换备胎模型。' }
  }
  return null
}
