export type SessionStatus =
  | 'running'
  | 'completed'
  | 'error'
  | 'awaiting_clarify'
  | 'paused_on_child'
  | 'awaiting_child'

/** 用户消息图片的 HTTP 线型（与后端 agent.WireImage 对齐）。
 *  data 为 base64 编码内容（不带 data: 前缀）；后端 Go []byte JSON 编解码即 base64。 */
export interface WireImage {
  mime_type: string
  data: string
}

/** 待澄清问题的选项（来自 SSE awaiting_clarify 帧） */
export interface ClarifyOption {
  id: string
  label: string
  description?: string
}

/** 批量待澄清的单题（任务 140，SSE awaiting_clarify 帧 questions 数组项；与后端 ClarifyQuestionItem 对齐） */
export interface ClarifyQuestionItem {
  question: string
  multi_select?: boolean
  options?: ClarifyOption[]
}

/** SSE awaiting_clarify 帧驱动的待澄清状态（不 push 进 events，答复后复位） */
export interface ClarifyPending {
  options: ClarifyOption[]
  multiSelect: boolean
  questionId: string
  /** 长上下文（进度盘点/计划全文）：展示在问题之前（任务 140 问题①） */
  detail?: string
  /** 批量模式题目列表；长度>1 为批量（同屏分页改选、统一提交），缺失/≤1 为单题 */
  questions?: ClarifyQuestionItem[]
  /** 澄清附带的产物（演示视频等，SSE awaiting_clarify 帧 artifacts 字段）：问答卡内嵌展示 */
  artifacts?: ArtifactRef[]
  /** 答复剩余秒数（SSE 帧 timeout_sec，服务端现算；存在即显示倒计时，超时工具侧自行决策） */
  timeoutSec?: number
}

export interface ChatMessage {
  role: 'user' | 'assistant' | 'system'
  content: string
  timestamp: string
}

/** 信任模式（TODO 第10⑥ 三级信任，对标 Codex）：suggest=变更逐条审批 / auto-edit=命令与破坏性工具审批 / full-auto=全自主。 */
export type TrustMode = 'suggest' | 'auto-edit' | 'full-auto'

/** 会话执行档位（TODO #14 三档全手动，2026-09-16）：fast=文档助手顶层直达 / daily=DomainAgent 顶层直接执行（默认）/ cluster=Meta 全装编排。 */
export type SessionGear = 'fast' | 'daily' | 'cluster'

/** 会话级思考强度（2026-09-16）：off|low|medium|high；空串 = 跟随角色默认。只影响本会话顶层 Agent。 */
export type SessionThinking = '' | 'off' | 'low' | 'medium' | 'high'

/**
 * 会话摘要（GET /sessions 列表线型，与后端 server.sessionSummary 对齐）。
 * 列表不带 events/messages：单会话事件流可达数 MB，整包下发会把首屏拖慢；
 * 需要明细的页面走 GET /sessions/{id}（返回完整 Session）。
 */
export interface SessionSummary {
  id: string
  goal: string
  status: SessionStatus
  result?: string
  started_at: string
  ended_at?: string
  /** 每会话工作目录：空/缺省 = 后端默认目录。 */
  work_dir?: string
  /** 临时工作目录（可选）。 */
  temp_dir?: string
  /** 信任模式（TODO 第10⑥）：suggest=变更逐条审批 / auto-edit=命令与破坏性工具审批 / full-auto=全自主；空 = 后端回退现网语义。 */
  trust_mode?: TrustMode | ''
  /** 执行档位（TODO #14 三档全手动）：fast|daily|cluster；空 = 未设置（按集群档现行为兜底）。 */
  gear?: SessionGear | ''
  /** 会话级思考强度（2026-09-16）：off|low|medium|high；空/缺省 = 跟随角色默认。 */
  thinking?: SessionThinking
}

export interface Session extends SessionSummary {
  /** 软停止销毁倒计时截止时间（TODO #37）：软停止后非空，续跑/到期后清空。 */
  destroy_at?: string | null
  /** 当前正在流式生成的助手文本（仅运行中有值，SSE 快照/live 帧携带），对齐 TUI 实时汇报展示。 */
  streaming_text?: string
  /** 当前思考阶段过程文本（瞬时，仅运行中有值）。 */
  thinking_text?: string
  events: SessionEvent[]
  messages: ChatMessage[]
}

export interface BaseSessionEvent {
  type: string
  agent: string
  message: string
  timestamp: string
  kind?: string
  tool?: string
  tool_path?: string
  tool_output?: string
  tool_error?: string
  tool_args?: string
  success?: boolean
  prompt?: string
  input_tokens?: number
  output_tokens?: number
  detail_json?: string
}

export interface UserMessageEvent extends BaseSessionEvent {
  type: 'user_message'
}

export interface ToolCallEvent extends BaseSessionEvent {
  type: 'tool_call'
  kind: 'tool_call'
}

export interface ToolExecEvent extends BaseSessionEvent {
  type: 'tool_exec'
}

export interface LLMEvent extends BaseSessionEvent {
  kind: 'llm' | 'llm_result' | 'llm_response' | 'think' | 'intend' | 'prompt' | 'agent_done' | 'wait' | 'graph_step'
}

export interface ErrorEvent extends BaseSessionEvent {
  type: 'error'
  kind?: 'error'
}

export interface TokenUsageEvent extends BaseSessionEvent {
  kind: 'token_usage'
}

export type SessionEvent =
  | UserMessageEvent
  | ToolCallEvent
  | ToolExecEvent
  | LLMEvent
  | ErrorEvent
  | TokenUsageEvent
  | BaseSessionEvent

export function isUserMessageEvent(ev: SessionEvent): ev is UserMessageEvent {
  return ev.type === 'user_message'
}

export function isToolCallEvent(ev: SessionEvent): ev is ToolCallEvent {
  return ev.type === 'tool_call' || ev.kind === 'tool_call'
}

export function isToolExecEvent(ev: SessionEvent): ev is ToolExecEvent {
  return ev.type === 'tool_exec'
}

/** 工具调用之间的中间正文（后端 kind=assistant_text）：该事件到达即表示上一轮流式正文已落盘，
 *  live 行可以清掉——不清会与正文块同屏重复。 */
export function isAssistantTextEvent(ev: SessionEvent): boolean {
  return ev.kind === 'assistant_text'
}

export function isLLMEvent(ev: SessionEvent): ev is LLMEvent {
  const kinds = new Set([
    'llm',
    'llm_result',
    'llm_response',
    'think',
    'intend',
    'prompt',
    'agent_done',
    'wait',
    'graph_step',
  ])
  return !!(ev.kind && kinds.has(ev.kind))
}

export function isErrorEvent(ev: SessionEvent): ev is ErrorEvent {
  return ev.type === 'error' || ev.kind === 'error' || ev.success === false
}

export function isTokenUsageEvent(ev: SessionEvent): ev is TokenUsageEvent {
  return ev.kind === 'token_usage'
}

export interface AgentNode {
  inst_id: string
  role_def_id: string
  name: string
  type: string
  domain: string
  status: string
  parent_id: string
  goal?: string
  block_id?: string
  /** 活动证据：最近活动种类（llm_start/tool:<名>/stream 等），空=无监控条目 */
  activity_kind?: string
  /** 活动证据：最近活动距今时长（"5s" 格式） */
  last_activity_ago?: string
}

/**
 * Agent 可视成果引用（ShowArtifact 工具产出 / 工具输出里的 .bma 产物路径）。
 * 只带工作区相对路径，媒体本体走 GET /api/sessions/:id/workspace/*path 流式读取。
 */
export interface ArtifactRef {
  kind: 'image' | 'video' | 'audio' | 'html' | string
  /** 工作区相对路径（正斜杠） */
  path: string
  title?: string
  caption?: string
  mime?: string
  /** 自动兜底识别出来的（非工具显式登记）：用于文案区分 */
  inferred?: boolean
}

/** 从 tool_exec 事件的 detail_json 里取出 ShowArtifact 登记的成果。 */
export function artifactsFromDetail(detailJson?: string): ArtifactRef[] {
  if (!detailJson) return []
  try {
    const d = JSON.parse(detailJson) as { artifacts?: ArtifactRef[] }
    if (!Array.isArray(d.artifacts)) return []
    return d.artifacts.filter((a) => a && typeof a.path === 'string' && a.path.length > 0)
  } catch {
    return [] // detail_json 是自由字段，解析失败按无成果处理
  }
}

/**
 * 从提问事件的 detail_json 里取出「提问前的答复正文」快照。
 *
 * 模型常见「先输出正文、再调 ask_user」：正文只活在流式瞬时字段里，不落事件，
 * 待澄清态一切换就把上一段输出整段吞掉（用户只看到思考链）。后端在提问事件上挂了
 * `{"report_text": ...}`，前端把它渲染在问答卡上方——刷新/回放/重启后仍在。
 */
export function clarifyReportFromDetail(detailJson?: string): string {
  if (!detailJson) return ''
  try {
    const d = JSON.parse(detailJson) as { report_text?: string }
    return typeof d.report_text === 'string' ? d.report_text : ''
  } catch {
    return '' // detail_json 是自由字段，解析失败按无正文处理
  }
}

/**
 * 从派发事件（kind=sub_agent_dispatch）的 detail_json 里取出中文领域名。
 *
 * 事件 Tool 字段只放得下角色 ID（domain 角色恒为 "domain"），一波 9 个领域会同名，
 * 展示名因此随 detail_json 带出。域名为空时返回空串（调用方回退角色 ID）。
 */
export function domainFromDetail(detailJson?: string): string {
  if (!detailJson) return ''
  try {
    const d = JSON.parse(detailJson) as { domain?: string }
    return typeof d.domain === 'string' ? d.domain : ''
  } catch {
    return '' // detail_json 是自由字段，解析失败按无领域名处理
  }
}

/** 媒体扩展名 → 展示类型（自动兜底识别用）。 */
const MEDIA_EXT_KIND: Record<string, string> = {
  png: 'image', jpg: 'image', jpeg: 'image', gif: 'image', webp: 'image', svg: 'image',
  mp4: 'video', webm: 'video', mov: 'video', mkv: 'video',
  mp3: 'audio', wav: 'audio', m4a: 'audio', ogg: 'audio', flac: 'audio',
  html: 'html', htm: 'html',
}

/**
 * 从工具输出文本里兜底识别可视成果路径（Agent 没调 ShowArtifact 也能出卡）。
 *
 * 只认 `.bma/` 下的产物目录：该前缀是 Agent 产出媒体/截图的统一落点
 * （images / od-artifacts / ui-artifacts / videos / artifacts），按 `/.bma/` 锚点截取
 * 就能同时兼容"绝对路径"与"工作区相对路径"两种写法，也避免把普通源码里的
 * 路径字符串误判成媒体。
 */
export function inferArtifactsFromOutput(output?: string): ArtifactRef[] {
  if (!output) return []
  const out: ArtifactRef[] = []
  const seen = new Set<string>()
  // 按空白与常见标点切词后逐词检查（比"路径前缀正则"稳：不用猜路径前面是什么，
  // 中英文标点/尖括号包裹的路径都能切出来）。
  for (const token of output.split(/[\s"'`()=<>,;:!?|\[\]{}、。：，；！？（）、【】「」《》…—～·]+/)) {
    const norm = token.replace(/\\/g, '/')
    const i = norm.indexOf('.bma/')
    if (i < 0) continue
    const path = norm.slice(i)
    // 只认产物目录下的媒体文件（.bma/shared、.bma/tool_outputs 等非展示内容不在此列）。
    if (!/^(images|od-artifacts|ui-artifacts|videos|artifacts)\//.test(path.slice(5))) continue
    const ext = path.split('.').pop()?.toLowerCase() || ''
    const kind = MEDIA_EXT_KIND[ext]
    if (!kind || seen.has(path)) continue
    seen.add(path)
    out.push({ kind, path, inferred: true })
  }
  return out
}

const ARTIFACT_KINDS = new Set(['image', 'video', 'audio', 'html'])

/**
 * 清洗 SSE awaiting_clarify 帧的 artifacts 字段（后端契约外的脏数据不放进问答卡）：
 * 非数组按无产物处理；元素缺 kind/path 丢弃；kind 不在四值枚举内时按 path 扩展名
 * 推断（复用 MEDIA_EXT_KIND），推断不出同样丢弃。
 */
export function clarifyArtifactsFromFrame(raw: unknown): ArtifactRef[] {
  if (!Array.isArray(raw)) return []
  const out: ArtifactRef[] = []
  for (const a of raw) {
    if (!a || typeof a !== 'object') continue
    const item = a as ArtifactRef
    if (typeof item.path !== 'string' || !item.path) continue
    let kind = typeof item.kind === 'string' ? item.kind : ''
    if (!ARTIFACT_KINDS.has(kind)) {
      const ext = item.path.split('.').pop()?.toLowerCase() || ''
      kind = MEDIA_EXT_KIND[ext] || ''
    }
    if (!kind) continue
    out.push({ ...item, kind })
  }
  return out
}

export interface SubTask {
  id: string
  title: string
  assignee?: string
  status: string
  result?: string
  depends_on?: string[]
  created_at: string
  updated_at: string
}

export interface TaskBoardData {
  topic_id: string
  goal: string
  status: string
  constraints?: Record<string, string>
  tasks: SubTask[]
  updated_at: string
}

export interface Skill {
  skill_id: string
  name: string
  description: string
  domain: string
  tool_ref: string
  cost: number
  tags: string[]
}

export interface HealthStatus {
  name: string
  online: boolean
  latency_ms?: number
  detail: string
}
