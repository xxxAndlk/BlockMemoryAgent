/**
 * mails.ts Agent 间邮件（mailbox 留痕）的展示解析与回合归属。
 *
 * 背景：邮件此前只存在两处——收件方上下文里的 `[mailbox from X]` 注入（面板上显示为气泡），
 * 和 agent_events 的 mailbox 留痕（前端从未渲染）。于是"我怎么回上级的"在界面上完全不可见：
 * 外发只是一次被折叠的工具调用（2026-09-17 用户实证）。现在把留痕按时间挂到回合上渲染。
 *
 * 归属规则：一封邮件按 `at` 时间落到"开始时间不晚于它的最后一个回合"——邮件不是事件流成员，
 * 没有回合归属信息，只能按时间就近；比首回合还早的落到首回合（会话启动时的派发邮件）。
 */
import type { AgentMailItem } from '@/api/session'
import type { Turn } from './turns'

/** 邮件类型中文标签（后端 message_type）。 */
export function mailTypeLabel(t: string): string {
  switch (t) {
    case 'request': return '询问'
    case 'reply': return '回复'
    case 'info': return '通知'
    case 'escalate': return '升级'
    default: return t || '消息'
  }
}

/** 类型配色（el-tag 语义色）：询问=蓝、回复=绿、通知=灰、升级=红。 */
export function mailTypeColor(t: string): 'primary' | 'success' | 'info' | 'danger' {
  switch (t) {
    case 'request': return 'primary'
    case 'reply': return 'success'
    case 'escalate': return 'danger'
    default: return 'info'
  }
}

/** 邮件正文拆行：后端 content = subject\nbody。 */
export function mailSubject(m: AgentMailItem): string {
  return m.subject || m.body || '(无主题)'
}

export function mailBody(m: AgentMailItem): string {
  return m.subject ? m.body : ''
}

/**
 * 把邮件挂到回合上（就地写入 turn.mails，按时间正序）。
 * `keep` 决定哪些邮件参与（主对话栏收双向；子 Agent 面板只收外发，入站已有气泡）。
 */
export function attachMailsToTurns(turns: Turn[], mails: AgentMailItem[], keep: (m: AgentMailItem) => boolean) {
  if (!turns.length || !mails.length) return
  const sorted = [...mails].filter(keep).sort((a, b) => (a.at || '').localeCompare(b.at || ''))
  let cursor = 0
  for (const m of sorted) {
    const at = m.at || ''
    // 推进到"开始时间不晚于该邮件"的最后一个回合
    while (cursor + 1 < turns.length && (turns[cursor + 1].startedAt || '') <= at) cursor++
    turns[cursor].mails.push(m)
  }
}
