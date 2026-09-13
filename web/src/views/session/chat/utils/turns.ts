import type { ArtifactRef, SessionEvent } from '@/types'
import {
  isUserMessageEvent,
  isToolCallEvent,
  isToolExecEvent,
  isTokenUsageEvent,
  isErrorEvent,
  artifactsFromDetail,
  inferArtifactsFromOutput,
  clarifyReportFromDetail,
  domainFromDetail,
} from '@/types'

/** 一次工具调用：把 tool_call (intent) + tool_exec (结果) 配对 */
export interface ToolCallGroup {
  id: string
  tool: string
  agent: string
  call?: SessionEvent // kind === 'tool_call'
  result?: SessionEvent // type === 'tool_exec'
  success: boolean
  pending: boolean
  /** 该次工具产出/引用的可视成果（ShowArtifact 登记 + 工具输出兜底识别） */
  artifacts?: ArtifactRef[]
}

/**
 * 一次子 Agent 派发（对话栏子 Agent 列表一行）：领域名 + 任务简要。
 * 状态不在这里定——由展示层用 agents 接口的实时实例状态叠加（派发瞬间实例还没进树）。
 */
export interface SubAgentRef {
  /** 事件时间戳（回合内唯一，作为列表 key） */
  key: string
  /** 中文领域名（后端随派发事件 detail_json 带出；缺失回退角色 ID） */
  name: string
  /** 任务简要（派发事件 message，后端已单行化） */
  task: string
  ts: string
}

/** 回合内一个按时间顺序的步骤：一段思考 / 一段中间正文 / 一次工具调用 */
export interface TurnStep {
  kind: 'think' | 'narrate' | 'tool'
  event?: SessionEvent // think / narrate 步骤对应的事件
  group?: ToolCallGroup // tool 步骤对应的工具调用组
}

/** 一个对话回合：用户消息 → 助手处理过程 → 最终答案 */
export interface Turn {
  id: string
  userMessage?: SessionEvent
  /** 按时间交错的步骤序列，保留 ReAct 时序（思考↔工具↔结果↔下一轮思考） */
  steps: TurnStep[]
  thinkChain: SessionEvent[] // 兼容字段：所有思考事件扁平集合
  toolCalls: ToolCallGroup[] // 兼容字段：所有工具调用组
  errors: SessionEvent[]
  /** 本回合派发出去的子 Agent（按派发顺序，一人一行） */
  subAgents: SubAgentRef[]
  clarifyQuestion?: SessionEvent
  /** 提问附带的长上下文（任务 140，kind=clarify_detail 事件）：展示在问题之前 */
  clarifyDetails: SessionEvent[]
  /** Agent 发言块（按时间顺序）：中间正文（kind=assistant_text，"口播一句→调工具"的那段话）、
   *  子 Agent 结果摘要（llm_result）、Meta 中继文本（message）等。它们按**正文**展示（与最终
   *  答复同档），只有模型私有推理（think/intend）才进思考链盒（2026-09-13 用户实证）。 */
  narrations: SessionEvent[]
  /** 提问前模型输出的正文快照（后端挂在提问事件 detail_json.report_text 上）：
   *  不落事件的话，待澄清态一切换就把上一段输出整段吞掉，只剩思考链（2026-09-12 实证）。 */
  clarifyReport?: string
  finalAnswer?: SessionEvent
  /** 合成最终答复：运行中会话的新用户消息接替当前回合时，收编接替时刻的流式汇报文本
   *  （流式文本只存于 SSE live 帧，不落事件；不收编则上一回合的答复内容丢失）。 */
  finalText?: string
  status: 'running' | 'completed' | 'error' | 'awaiting_clarify' | 'awaiting_child' | 'cancelled'
  startedAt: string
  endedAt?: string
  tokens: { in: number; out: number }
  agents: string[]
}

type EventCategory =
  | 'user_message'
  | 'system_start'
  | 'system_resume'
  | 'tool_call'
  | 'tool_exec'
  | 'tool_result_legacy'
  | 'completion'
  | 'sub_agent_dispatch'
  | 'assistant_text'
  | 'clarify'
  | 'clarify_detail'
  | 'error'
  | 'think'
  | 'agent_message'
  | 'debug'

/**
 * 推理类 kind：**模型自己的思考链**（只进「思考链路」盒）。
 * 市场对齐（DeepSeek「深度思考」/ ChatGPT agent / Claude Code）：盒里只放模型的私有推理；
 * 说给用户的话、子 Agent 的产出都不是思考（2026-09-13 用户实证：链盒里混着
 * 「【交付结论】…」这类交付内容）。
 */
const REASONING_KINDS = new Set(['think', 'intend'])

/**
 * Agent 发言类 kind：模型/子 Agent **说给用户的话**，按正文展示（与最终答复同档）。
 * - `llm_result`：子 Agent 完成时的结果摘要（`LiveEventSubAgentDone` 落事件，Tool=子 Agent ID）
 * - `message`：Meta 中继文本等普通消息（`suspendOnChildWait` 注释即"前端按正常发言展示"）
 * - `llm` / `llm_response` / `notify`：LLM 通用输出事件
 */
const SPEECH_KINDS = new Set(['llm', 'llm_result', 'llm_response', 'message', 'notify'])

/**
 * 调试/活动类 kind：既不是推理也不是发言，折叠在思考盒里（简洁模式只计「已折叠 +N」）。
 * 完整事件流看监控页（?view=monitor 的执行日志），对话栏不铺这些管线细节。
 * 判别按 kind||type（`prompt`/`system` 这类事件只有 type，没有 kind——只认 kind 会把
 * 「输入补全: gate_skip」当发言展示出来）。
 */
const DEBUG_KINDS = new Set([
  'prompt',
  'token_usage',
  'graph_step',
  'agent_created',
  'wait',
  'stats',
  'sub_agent_done',
  'memory_recall',
  'topic_switch',
  'interrupt',
  'enqueue',
])

/** 将事件归类到单一语义类别，作为后续路由的依据 */
export function classifyEvent(ev: SessionEvent): EventCategory {
  if (isUserMessageEvent(ev)) return 'user_message'
  // 用户对澄清/审批的答复事件（"提问答复: …"/"审批答复: …"，type=clarify + agent=User）
  // 本质是用户发言：归入 user_message 开新回合。否则会命中下方 clarify 分支——把已答复
  // 的问题卡覆盖成答复文本、把回合重新标回 awaiting_clarify，且问答流卡在"待澄清"态直到
  // 完成事件到来（2026-09-08 web 端 ask_user 答复显示修复）。
  if ((ev.type === 'clarify' || ev.kind === 'clarify') && ev.agent === 'User') return 'user_message'
  // 会话启动事件（后端 addEvent agent=System）：仅作时间线锚点，不开回合不进思考链
  if (ev.type === 'system' && ev.message?.startsWith('会话启动')) return 'system_start'
  if (ev.type === 'system' && ev.message?.startsWith('继续会话')) return 'system_resume'
  if (isCompletion(ev)) return 'completion'
  // 提问附带的长上下文（任务 140 问题①）：kind=clarify_detail，先于问题展示；
  // 必须在通用 clarify 判断之前分类（其 type 也是 clarify）。
  if (ev.kind === 'clarify_detail') return 'clarify_detail'
  if (ev.type === 'clarify' || ev.kind === 'clarify') return 'clarify'
  // 子 Agent 派发（后端 kind=sub_agent_dispatch）：对话栏子 Agent 列表用，
  // 必须在通用 think 兜底之前分类——否则会落进思考链，用户看到一串参数 JSON。
  if (ev.kind === 'sub_agent_dispatch') return 'sub_agent_dispatch'
  // 工具调用之间的中间正文（后端 kind=assistant_text）：按时间顺序渲染成正文块，
  // 同样必须在 think 兜底之前分类——否则这段"人话"会掉进思考链里。
  if (ev.kind === 'assistant_text') return 'assistant_text'
  if (isToolCallEvent(ev)) return 'tool_call'
  if (isToolExecEvent(ev)) return 'tool_exec'
  if (ev.kind === 'tool_result') return 'tool_result_legacy'
  if (isErrorEvent(ev)) return 'error'
  if (isReasoningEvent(ev)) return 'think'
  if (isSpeechEvent(ev)) return 'agent_message'
  if (isDebugEvent(ev)) return 'debug'
  // 未知 kind + 系统提示（"因服务重启中断，可发送消息继续"之类）：一律按正文展示。
  // 旧的兜底是把未知 kind 塞进思考链——结果是「交付结论」这类输出被当成思考
  // （2026-09-13 用户实证）；宁可在对话栏多显示，也不把内容藏进推理盒。
  return 'agent_message'
}

/** 事件的判别标识：后端多数事件 kind 与 type 同名（prompt/system 只有 type），统一取 kind||type。 */
function eventKind(ev: SessionEvent): string {
  return ev.kind || ev.type || ''
}

function isReasoningEvent(ev: SessionEvent): boolean {
  return REASONING_KINDS.has(eventKind(ev))
}

function isSpeechEvent(ev: SessionEvent): boolean {
  return SPEECH_KINDS.has(eventKind(ev))
}

function isDebugEvent(ev: SessionEvent): boolean {
  if (DEBUG_KINDS.has(eventKind(ev))) return true
  // progress 型管线事件（llm_result 是发言，已在 classifyEvent 里先被拦走）
  return ev.type === 'progress'
}

/**
 * 终结回合的 system 文案（agent=System）——漏一条就会让回合永远"处理中"假转圈
 * （2026-09-11 实证：点"终止"后后端发了「会话已被用户取消」，前端不认，转圈不停）。
 * 文案来源：`service_react.go` 的 pauseMessage / cancel / 墙钟强杀 / 软停止销毁。
 */
const TERMINAL_SYSTEM_MESSAGES = [
  '已暂停', // pauseMessage（token/轮数触顶）
  '会话暂停',
  '任务已停止', // 软停止（可续跑）
  '会话已被用户取消', // 硬取消：用户点"终止"
  '已强制终止', // 会话超全局时限
  '任务已销毁', // 软停止倒计时到期
]

/** 用户主动终止（"终止"按钮）：回合终态是"已终止"而非"失败"。 */
export function isUserCancel(ev: SessionEvent): boolean {
  return ev.type === 'system' && ev.agent === 'System' && (ev.message || '').includes('用户取消')
}

export function isCompletion(ev: SessionEvent): boolean {
  // 当前合同：后端会话完成只发 type=agent_done + agent=MetaAgent（消息即最终答复正文，无前缀）。
  // 旧合同（system + "会话完成:"前缀）已无发送方，保留兼容历史库重放事件。
  if (ev.type === 'agent_done' && ev.agent === 'MetaAgent') return true
  if (ev.type === 'system' && ev.agent === 'System') {
    const msg = ev.message || ''
    return TERMINAL_SYSTEM_MESSAGES.some((m) => msg.includes(m))
  }
  return (
    ev.type === 'system' &&
    ev.agent === 'MetaAgent' &&
    (ev.message?.startsWith('会话完成') || ev.message?.startsWith('执行失败'))
  )
}

/**
 * 按会话终态兜底收口最后一个回合。
 *
 * 分组只认事件流里的终结事件；但会话可能因**事件流里没有对应文案**的路径停下
 * （服务重启中断、异常退出等）——此时 session 已是终态而回合仍停在 'running'，
 * 表现为永久"处理中 + 正在生成回答…"假转圈。会话状态是权威的兜底信号：
 * 只要会话不在运行/待澄清，就不该有任何回合处于 'running'。
 */
export function settleTurnsBySessionStatus(
  turns: Turn[],
  sessionStatus: string | undefined,
): Turn[] {
  if (!sessionStatus) return turns
  if (sessionStatus === 'running' || sessionStatus === 'awaiting_clarify') return turns
  // 挂起等子（Meta 已派完子任务，挂起等子 Agent 回传唤醒）：非终态，回合不能收口；
  // 把仍 running 的回合标成 awaiting_child，展示「挂起等子」而非永久"处理中"转圈。
  if (sessionStatus === 'awaiting_child') {
    for (let i = turns.length - 1; i >= 0; i--) {
      if (turns[i].status === 'running') turns[i].status = 'awaiting_child'
    }
    return turns
  }
  for (let i = turns.length - 1; i >= 0; i--) {
    // 'awaiting_clarify'/'awaiting_child' 一并收口：会话已终态就不该还挂着待澄清卡/挂起态
    // （终止发生在提问/等子期间时，卡片会带着可点选项留着，点了只会报错——会话已经不接受了）。
    if (turns[i].status === 'running' || turns[i].status === 'awaiting_clarify' || turns[i].status === 'awaiting_child') {
      turns[i].status = sessionStatus === 'error' ? 'cancelled' : 'completed'
    }
  }
  return turns
}

export function isFailedCompletion(ev: SessionEvent): boolean {
  // 仅旧合同带 "执行失败" 前缀；agent_done 恒为成功路径（失败走 error 事件）。
  return ev.type === 'system' && ev.agent === 'MetaAgent' && ev.message?.startsWith('执行失败') === true
}

export function isError(ev: SessionEvent): boolean {
  return isErrorEvent(ev)
}

export function createToolGroup(callEv: SessionEvent): ToolCallGroup {
  return {
    id: callEv.timestamp,
    tool: callEv.tool || extractToolName(callEv.message) || 'unknown',
    agent: callEv.agent,
    call: callEv,
    success: false,
    pending: true,
  }
}

export function finalizeTurn(turn: Turn, finalEvent?: SessionEvent): Turn {
  if (!finalEvent) return turn
  // 必须原地修改：调用方把 turn 引用存进了 turns[]，返回扩散副本会丢掉完成状态
  //（实证 2026-09-11：agent_done 落事件流但对话栏回合永久"处理中"、最终交付不渲染）。
  turn.finalAnswer = finalEvent
  // 用户主动终止单列一档：这是预期内的中止，不该和"失败"共用红标与文案。
  turn.status = isUserCancel(finalEvent)
    ? 'cancelled'
    : isFailedCompletion(finalEvent)
      ? 'error'
      : 'completed'
  turn.endedAt = finalEvent.timestamp
  return turn
}

function tokenIn(ev: SessionEvent): number {
  return ev.input_tokens || 0
}

function tokenOut(ev: SessionEvent): number {
  return ev.output_tokens || 0
}

/**
 * 把扁平的 SessionEvent[] 分组成 Turn[]。
 *
 * 规则：
 * - 每条 type='user_message' 开启新回合；或会话初始的 system "会话启动" 事件视为第一个回合的"用户消息"。
 * - 回合内事件按时间顺序路由到 steps[]，保留 ReAct 时序：
 *   think/intend/llm/llm_result/wait/... → think 步骤；
 *   tool_call → 新建工具组 + tool 步骤；tool_exec → 匹配同名 pending 组填 result（不新增步骤）；
 *   error → errors；system "会话完成"/"执行失败" → finalAnswer + 改变 status。
 * - 工具类事件判断优先于 error，避免失败的工具调用被误归 errors 而永久 pending。
 * - 运行中会话（MetaAgent 单循环常驻）的新用户消息接替当前回合：上一回合标 completed，
 *   并从 priorReplies 收编接替时刻的流式文本作 finalText——否则上一回合永远"处理中"
 *   且继续渲染全局 live 帧（与当前回合重复展示同一份流式汇报，2026-09-10 实证）。
 *   priorReplies 由 SSE 层在 user_message 事件到达时快照 liveStreaming 构建（key=事件时间戳）。
 */
export function groupEventsToTurns(events: SessionEvent[], priorReplies?: Record<string, string>): Turn[] {
  const turns: Turn[] = []
  let current: Turn | null = null

  const openTurn = (ev?: SessionEvent, startedAt?: string) => {
    current = {
      id: ev?.timestamp || `turn-${turns.length}`,
      userMessage: ev,
      steps: [],
      thinkChain: [],
      toolCalls: [],
      errors: [],
      subAgents: [],
      clarifyDetails: [],
      narrations: [],
      status: 'running',
      startedAt: startedAt || ev?.timestamp || new Date().toISOString(),
      tokens: { in: 0, out: 0 },
      agents: [],
    }
    turns.push(current)
  }

  for (const ev of events) {
    const category = classifyEvent(ev)

    // 启动 / 用户消息 → 新回合
    if (category === 'user_message') {
      // 接替仍在运行的上一回合（awaiting_clarify 例外：澄清卡依赖该状态展示，保持原样）。
      if (current && current.status === 'running') {
        current.status = 'completed'
        current.endedAt = ev.timestamp
        current.finalText = priorReplies?.[ev.timestamp] || ''
      }
      openTurn(ev)
      continue
    }
    if (category === 'system_start') {
      // 锚点事件：跳过，不作为回合内容（回合由首个 user_message / 首个事件兜底开启）
      continue
    }
    if (category === 'system_resume') {
      // 接续上一个回合（用户已 push 过 user_message），忽略
      continue
    }

    if (!current) openTurn(undefined, ev.timestamp)

    // 收集参与的 agent
    if (ev.agent && current!.agents.indexOf(ev.agent) === -1 && ev.agent !== 'System') {
      current!.agents.push(ev.agent)
    }

    // Token 累计
    if (isTokenUsageEvent(ev)) {
      current!.tokens.in += tokenIn(ev)
      current!.tokens.out += tokenOut(ev)
    }

    // 完成事件
    if (category === 'completion') {
      current = finalizeTurn(current!, ev)
      continue
    }

    // 待澄清：Agent 请求用户澄清，挂起会话；区别于错误，单独标记
    if (category === 'clarify') {
      current!.clarifyQuestion = ev
      current!.status = 'awaiting_clarify'
      current!.endedAt = ev.timestamp
      // 提问前的正文快照（批量题只挂在第一条提问事件上）：任一事件带即取，
      // 首段非空优先——它必须活到问答卡上方，否则用户只能靠思考链回忆上下文。
      if (!current!.clarifyReport) {
        const r = clarifyReportFromDetail(ev.detail_json)
        if (r) current!.clarifyReport = r
      }
      continue
    }

    // 提问附带的长上下文（任务 140 问题①）：只收集不改回合状态，
    // 由问答卡渲染在问题之前
    if (category === 'clarify_detail') {
      current!.clarifyDetails.push(ev)
      continue
    }

    // 子 Agent 派发：收集成列表（一次批量派发落多条事件，一人一行）
    if (category === 'sub_agent_dispatch') {
      current!.subAgents.push({
        key: ev.timestamp,
        // 领域名优先；旧事件（后端未带 domain，Tool 恒为 "domain"）回退通用标签，
        // 不把角色 ID "domain" 当名字展示给用户。
        name: domainFromDetail(ev.detail_json) || (ev.tool && ev.tool !== 'domain' ? ev.tool : '') || '子 Agent',
        task: (ev.message || '').trim(),
        ts: ev.timestamp,
      })
      continue
    }

    // 中间正文（工具调用之间的口播）：按时间顺序收集，渲染成持久正文块
    if (category === 'assistant_text') {
      current!.narrations.push(ev)
      current!.steps.push({ kind: 'narrate', event: ev })
      continue
    }

    // Agent 发言（子 Agent 结果摘要 / Meta 中继文本 / 未知 kind）：与中间正文同一档展示——
    // 它们是"需要展示的东西"，不是模型的私有推理。
    if (category === 'agent_message') {
      current!.narrations.push(ev)
      current!.steps.push({ kind: 'narrate', event: ev })
      continue
    }

    // 工具调用意图（pending）：新建组并占一个 tool 步骤
    if (category === 'tool_call') {
      const group = createToolGroup(ev)
      current!.toolCalls.push(group)
      current!.steps.push({ kind: 'tool', group })
      continue
    }

    // 工具执行结果（type=tool_exec）：配对到同名 pending 组填 result，不新增步骤
    if (category === 'tool_exec') {
      const toolName = ev.tool || extractToolName(ev.message) || 'unknown'
      // 可视成果：ShowArtifact 显式登记（detail_json.artifacts）优先；
      // 没登记时从工具输出里兜底识别 .bma/ 产物路径（Agent 忘了调工具也看得到）。
      const arts = collectArtifacts(ev)
      const matched = [...current!.toolCalls].reverse().find((g) => g.tool === toolName && g.pending)
      if (matched) {
        matched.result = ev
        matched.success = ev.success !== false
        matched.pending = false
        if (arts.length) matched.artifacts = arts
      } else {
        // 没有配对的 call（历史回放/孤儿），作为自包含组新建一个 tool 步骤
        const group: ToolCallGroup = {
          id: ev.timestamp,
          tool: toolName,
          agent: ev.agent,
          result: ev,
          success: ev.success !== false,
          pending: false,
          artifacts: arts.length ? arts : undefined,
        }
        current!.toolCalls.push(group)
        current!.steps.push({ kind: 'tool', group })
      }
      continue
    }

    // 旧版后端可能仍发 tool_result kind（已废弃），忽略避免重复卡片
    if (category === 'tool_result_legacy') {
      continue
    }

    // 错误（工具失败已由 tool_exec.success=false 体现，此处只收真正的错误事件）
    if (category === 'error') {
      current!.errors.push(ev)
      current!.status = 'error'
      continue
    }

    // 推理 + 调试（prompt/token_usage/子Agent完成…）→ think 步骤（兼容字段 thinkChain）；
    // ThinkChain 只显示推理，调试类折叠成「已折叠 +N」，展开/verbose 才看得到。
    if (category === 'think' || category === 'debug') {
      current!.thinkChain.push(ev)
      current!.steps.push({ kind: 'think', event: ev })
      continue
    }
  }

  return turns
}

/** 从消息文本里推断工具名（如 "调用工具 ReadFile" / "执行工具: ReadFile" / "工具 ReadFile 执行成功"） */
function extractToolName(msg?: string): string | undefined {
  if (!msg) return undefined
  const patterns = [
    /调用工具\s+([A-Za-z_][\w]*)/,
    /执行工具[:：]\s*([A-Za-z_][\w]*)/,
    /工具\s+([A-Za-z_][\w]*)\s+执行/,
  ]
  for (const re of patterns) {
    const m = msg.match(re)
    if (m) return m[1]
  }
  return undefined
}

/** 从调用参数 JSON 中提取一行关键入参摘要（读了哪个文件 / 跑了什么命令 / 请求哪个 URL） */
export function toolHeadline(group: ToolCallGroup): string {
  const raw = group.call?.detail_json || group.call?.tool_args || ''
  let args: Record<string, unknown> = {}
  if (raw) {
    try {
      args = JSON.parse(raw)
    } catch {
      args = {}
    }
  }
  switch (group.tool) {
    case 'ReadFile':
    case 'WriteFile':
    case 'EditFile':
    case 'ListDir':
      return String(args.path || group.result?.tool_path || '')
    case 'RunCommand':
      return String(args.command || '')
    case 'HTTPGet':
    case 'HTTPPost':
      return String(args.url || '')
    case 'SearchInFiles':
      return String(args.pattern || '')
    default:
      return group.result?.tool_path || ''
  }
}

/** 简洁模式过滤：剔除偏调试用的事件 */
export function filterTurnForConcise(turn: Turn): Turn {
  return {
    ...turn,
    thinkChain: turn.thinkChain.filter((ev) => {
      const k = ev.kind || ev.type
      return k === 'think' || k === 'intend' || k === 'llm' || k === 'wait'
    }),
  }
}


/** 收集一次工具结果携带的可视成果：显式登记优先，工具输出兜底识别（去重）。 */
export function collectArtifacts(ev: SessionEvent): ArtifactRef[] {
  const explicit = artifactsFromDetail(ev.detail_json)
  const inferred = inferArtifactsFromOutput(ev.tool_output).filter(
    (a) => !explicit.some((e) => e.path === a.path),
  )
  return [...explicit, ...inferred]
}

/** 回合内全部可视成果（按出现顺序去重）——对话栏"本轮产出"媒体 rail 用。 */
export function turnArtifacts(turn: Turn): ArtifactRef[] {
  const out: ArtifactRef[] = []
  const seen = new Set<string>()
  for (const g of turn.toolCalls) {
    for (const a of g.artifacts || []) {
      if (seen.has(a.path)) continue
      seen.add(a.path)
      out.push(a)
    }
  }
  return out
}
