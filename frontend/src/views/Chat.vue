<script setup lang="ts">
import { ref, onMounted, nextTick } from 'vue'
import type { Session, SessionEvent } from '../types'
import { renderMd, esc } from '../utils/markdown'

const sessions = ref<Session[]>([])
const activeSession = ref<Session | null>(null)
const input = ref('')
const loading = ref(false)
const messagesContainer = ref<HTMLDivElement | null>(null)
const evtSource = ref<EventSource | null>(null)

interface DisplayMessage {
  role: 'user' | 'assistant' | 'system'
  content: string
  timestamp: string
  events?: SessionEvent[]
  // 用于标记执行过程是否仍在进行（显示展开/收起）
  running?: boolean
}

const displayMessages = ref<DisplayMessage[]>([])

onMounted(loadSessions)

async function loadSessions() {
  try {
    const r = await fetch('/api/sessions')
    sessions.value = await r.json()
  } catch {}
}

function connectStream(session: Session) {
  if (evtSource.value) evtSource.value.close()
  evtSource.value = new EventSource(`/api/sessions/${session.id}/stream`)

  let initial = true
  let stepsBubble: DisplayMessage | null = null

  // 获取或创建当前用户消息后的执行步骤气泡
  const ensureStepsBubble = (ev: SessionEvent): DisplayMessage => {
    if (stepsBubble && !stepsBubble.running === false) {
      // 若已结束则新建
    }
    if (!stepsBubble) {
      stepsBubble = {
        role: 'system',
        content: '',
        timestamp: ev.timestamp,
        events: [],
        running: true,
      }
      displayMessages.value.push(stepsBubble)
    }
    return stepsBubble
  }

  evtSource.value.onmessage = (e) => {
    const d: SessionEvent = JSON.parse(e.data)

    if (d.type === 'done') {
      evtSource.value?.close()
      loading.value = false
      if (stepsBubble) stepsBubble.running = false
      loadSessions()
      return
    }

    if (initial && (d as any).id) {
      initial = false
      activeSession.value = d as any
      buildDisplayMessages(d as any)
      return
    }

    if (!activeSession.value) return
    activeSession.value.events.push(d)

    // MetaAgent 启动事件不进 steps 气泡
    if (d.type === 'system' && d.agent === 'MetaAgent' && d.message.startsWith('会话启动')) {
      return
    }

    // 会话完成 → 作为最终 assistant 消息
    if (d.type === 'system' && d.agent === 'MetaAgent' && d.message.startsWith('会话完成')) {
      if (stepsBubble) stepsBubble.running = false
      displayMessages.value.push({
        role: 'assistant',
        content: d.message.replace(/^会话完成:?\s*/, ''),
        timestamp: d.timestamp,
      })
      stepsBubble = null
      scrollToBottom()
      return
    }

    // agent_done / stats / error → 作为最终系统消息
    if (d.type === 'agent_done' || d.type === 'stats' || d.type === 'error') {
      if (stepsBubble) stepsBubble.running = false
      displayMessages.value.push({
        role: 'system',
        content: `[${d.agent}] ${d.message}`,
        timestamp: d.timestamp,
      })
      stepsBubble = null
      scrollToBottom()
      return
    }

    // progress / tool_exec → 进执行步骤气泡
    if (d.type === 'progress' || d.type === 'tool_exec') {
      const bubble = ensureStepsBubble(d)
      bubble.events = bubble.events || []
      bubble.events.push(d)
      scrollToBottom()
      return
    }

    // 其他未识别事件直接忽略
  }
}

function buildDisplayMessages(session: Session) {
  displayMessages.value = []
  if (!session.messages) return

  // 把已有 events 按 timestamp 与 messages 合并展示
  const events = session.events || []
  let evIdx = 0

  for (const msg of session.messages) {
    if (msg.role === 'system' && msg.content.startsWith('Goal:')) continue
    // 在 user 消息后、下一条 user 消息前的 progress/tool_exec 事件聚合成一个 steps 气泡
    if (msg.role === 'user') {
      displayMessages.value.push({
        role: 'user',
        content: msg.content,
        timestamp: msg.timestamp,
      })
      const steps: SessionEvent[] = []
      while (evIdx < events.length) {
        const ev = events[evIdx]
        if (ev.type === 'system' && ev.agent === 'MetaAgent' && ev.message.startsWith('会话启动')) {
          evIdx++
          continue
        }
        if (ev.type === 'progress' || ev.type === 'tool_exec') {
          steps.push(ev)
          evIdx++
          continue
        }
        break
      }
      if (steps.length) {
        displayMessages.value.push({
          role: 'system',
          content: '',
          timestamp: steps[0].timestamp,
          events: steps,
          running: session.status === 'running',
        })
      }
    } else if (msg.role === 'assistant') {
      displayMessages.value.push({
        role: 'assistant',
        content: msg.content,
        timestamp: msg.timestamp,
      })
    }
  }

  // 剩余 events（如 stats / agent_done）追加为系统消息
  while (evIdx < events.length) {
    const ev = events[evIdx++]
    if (ev.type === 'system' && ev.agent === 'MetaAgent' && ev.message.startsWith('会话完成')) {
      displayMessages.value.push({
        role: 'assistant',
        content: ev.message.replace(/^会话完成:?\s*/, ''),
        timestamp: ev.timestamp,
      })
    } else if (ev.type === 'agent_done' || ev.type === 'stats' || ev.type === 'error') {
      displayMessages.value.push({
        role: 'system',
        content: `[${ev.agent}] ${ev.message}`,
        timestamp: ev.timestamp,
      })
    }
  }
}

async function send() {
  const text = input.value.trim()
  if (!text || loading.value) return
  input.value = ''

  if (!activeSession.value || activeSession.value.status !== 'running') {
    loading.value = true
    displayMessages.value.push({
      role: 'user',
      content: text,
      timestamp: new Date().toISOString(),
    })

    try {
      const r = await fetch('/api/sessions', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ goal: text }),
      })
      const session: Session = await r.json()
      activeSession.value = session
      sessions.value.unshift(session)
      connectStream(session)
      scrollToBottom()
    } catch (e) {
      loading.value = false
      displayMessages.value.push({
        role: 'system',
        content: '创建会话失败: ' + String(e),
        timestamp: new Date().toISOString(),
      })
    }
  } else {
    displayMessages.value.push({
      role: 'user',
      content: text,
      timestamp: new Date().toISOString(),
    })

    try {
      await fetch(`/api/sessions/${activeSession.value.id}/message`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ content: text }),
      })
      connectStream(activeSession.value)
      scrollToBottom()
    } catch (e) {
      displayMessages.value.push({
        role: 'system',
        content: '发送消息失败: ' + String(e),
        timestamp: new Date().toISOString(),
      })
    }
  }
}

function selectSession(s: Session) {
  activeSession.value = s
  buildDisplayMessages(s)
  if (s.status === 'running') {
    loading.value = true
    connectStream(s)
  } else {
    loading.value = false
    if (evtSource.value) {
      evtSource.value.close()
      evtSource.value = null
    }
  }
  scrollToBottom()
}

function newChat() {
  activeSession.value = null
  displayMessages.value = []
  loading.value = false
  if (evtSource.value) {
    evtSource.value.close()
    evtSource.value = null
  }
}

function scrollToBottom() {
  nextTick(() => {
    messagesContainer.value?.scrollTo({ top: messagesContainer.value.scrollHeight, behavior: 'smooth' })
  })
}

function formatTime(ts: string) {
  return new Date(ts).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', second: '2-digit' })
}

function kindLabel(k?: string) {
  return ({
    think: '思考', intend: '决策', llm: 'LLM调用', tool_call: '工具调用', tool_result: '工具结果',
    wait: '等待', error: '错误', prompt: 'Prompt', agent_created: '智能体创建',
    token_usage: 'Token统计', graph_step: '图步骤',
  } as Record<string, string>)[k || ''] || k || ''
}

function eventIcon(ev: SessionEvent) {
  if (ev.type === 'tool_exec') return ev.success ? '✓' : '✗'
  const m: Record<string, string> = {
    think: '💭', intend: '🎯', llm: '🤖', tool_call: '🔧', tool_result: '🔧',
    wait: '⏳', error: '⚠', prompt: '📝', agent_created: '✨',
    token_usage: '📊', graph_step: '→',
  }
  return m[ev.kind || ''] || '•'
}
</script>

<template>
  <div class="chat-layout">
    <div class="session-list">
      <div class="list-header">
        <h3>会话历史</h3>
        <button class="new-btn" @click="newChat">+ 新对话</button>
      </div>
      <div class="list-items">
        <div
          v-for="s in sessions"
          :key="s.id"
          class="session-item"
          :class="{ active: activeSession?.id === s.id }"
          @click="selectSession(s)"
        >
          <div class="session-title">{{ s.goal }}</div>
          <div class="session-meta">
            <span :class="'status-dot ' + s.status"></span>
            <span class="time">{{ formatTime(s.started_at) }}</span>
          </div>
        </div>
      </div>
    </div>

    <div class="chat-main">
      <div v-if="!activeSession && displayMessages.length === 0" class="welcome">
        <div class="welcome-icon">◆</div>
        <h2>BlockMemory Agent</h2>
        <p>输入你的问题或任务，Agent 将自动分析并执行</p>
        <div class="suggestions">
          <button @click="input = '分析代码结构'; send()">分析代码结构</button>
          <button @click="input = '编写新模块'; send()">编写新模块</button>
          <button @click="input = '运行测试'; send()">运行测试</button>
          <button @click="input = '搜索知识库'; send()">搜索知识库</button>
        </div>
      </div>

      <div ref="messagesContainer" class="messages">
        <template v-for="(msg, i) in displayMessages" :key="i">
          <!-- 用户消息 -->
          <div v-if="msg.role === 'user'" class="message user">
            <div class="message-content">
              <div class="message-text" v-html="esc(msg.content)"></div>
            </div>
            <div class="message-time">{{ formatTime(msg.timestamp) }}</div>
          </div>

          <!-- 助手最终回答 -->
          <div v-else-if="msg.role === 'assistant'" class="message assistant">
            <div class="message-content">
              <div class="message-text markdown-body" v-html="renderMd(msg.content)"></div>
            </div>
            <div class="message-time">{{ formatTime(msg.timestamp) }}</div>
          </div>

          <!-- 执行步骤气泡 -->
          <div v-else-if="msg.role === 'system' && msg.events && msg.events.length" class="message system steps">
            <div class="message-content">
              <div class="steps-header">
                <span class="steps-title">
                  执行过程 · {{ msg.events.length }} 步
                  <span v-if="msg.running" class="running-tag">进行中…</span>
                </span>
              </div>
              <div class="steps-list">
                <div
                  v-for="(ev, j) in msg.events"
                  :key="j"
                  class="step-item"
                  :class="'kind-'+ev.kind"
                >
                  <span class="step-icon">{{ eventIcon(ev) }}</span>
                  <span class="step-time">{{ formatTime(ev.timestamp) }}</span>
                  <span class="step-agent" v-if="ev.agent">{{ ev.agent }}</span>
                  <span v-if="ev.kind" :class="'step-kind kind-'+ev.kind">{{ kindLabel(ev.kind) }}</span>
                  <span v-else-if="ev.type === 'tool_exec'" class="step-kind kind-tool_exec">工具</span>
                  <div class="step-msg" v-html="esc(ev.message)"></div>
                  <details v-if="ev.tool_output || ev.tool_error || ev.detail_json || ev.prompt" class="step-detail">
                    <summary>详情</summary>
                    <pre v-if="ev.tool_output">{{ ev.tool_output }}</pre>
                    <pre v-if="ev.tool_error" class="error">{{ ev.tool_error }}</pre>
                    <pre v-if="ev.detail_json">{{ ev.detail_json }}</pre>
                    <pre v-if="ev.prompt">{{ ev.prompt }}</pre>
                  </details>
                </div>
              </div>
            </div>
            <div class="message-time">{{ formatTime(msg.timestamp) }}</div>
          </div>

          <!-- 纯系统消息（stats / error / agent_done） -->
          <div v-else class="message system">
            <div class="message-content">
              <div class="message-text" v-html="esc(msg.content)"></div>
            </div>
            <div class="message-time">{{ formatTime(msg.timestamp) }}</div>
          </div>
        </template>

        <div v-if="loading" class="message assistant typing">
          <div class="message-content">
            <div class="typing-indicator">
              <span></span><span></span><span></span>
            </div>
          </div>
        </div>
      </div>

      <div class="input-area">
        <textarea
          v-model="input"
          :placeholder="activeSession?.status === 'running' ? 'Agent 正在执行中，请稍候...' : '输入消息...'"
          :disabled="loading && activeSession?.status === 'running'"
          @keydown.enter.prevent="send"
          rows="1"
        />
        <button
          class="send-btn"
          :disabled="!input.trim() || (loading && activeSession?.status === 'running')"
          @click="send"
        >
          发送
        </button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.chat-layout {
  display: grid;
  grid-template-columns: 260px 1fr;
  gap: 0;
  height: 100%;
  overflow: hidden;
}

.session-list {
  background: #111827;
  border-right: 1px solid #243447;
  display: flex;
  flex-direction: column;
  overflow: hidden;
}

.list-header {
  padding: 12px 14px;
  border-bottom: 1px solid #243447;
  display: flex;
  justify-content: space-between;
  align-items: center;
}

.list-header h3 {
  font-size: 13px;
  font-weight: 700;
  margin: 0;
  color: #e2e8f0;
}

.new-btn {
  background: #2563eb;
  border: none;
  border-radius: 4px;
  padding: 4px 10px;
  color: white;
  font-size: 11px;
  cursor: pointer;
  font-family: inherit;
}

.new-btn:hover { background: #3b82f6; }

.list-items { flex: 1; overflow-y: auto; padding: 8px; }

.session-item {
  padding: 10px;
  border-radius: 6px;
  margin-bottom: 4px;
  cursor: pointer;
  border: 1px solid transparent;
  transition: all 0.15s;
}

.session-item:hover { background: #1e293b; }
.session-item.active { background: rgba(37, 99, 235, 0.15); border-color: #2563eb; }

.session-title {
  font-size: 12px;
  color: #e2e8f0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  margin-bottom: 4px;
}

.session-meta {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 10px;
  color: #64748b;
}

.status-dot {
  width: 6px; height: 6px; border-radius: 50%; display: inline-block;
}
.status-dot.running { background: #3b82f6; animation: pulse 1.5s infinite; }
.status-dot.completed { background: #22c55e; }
.status-dot.error { background: #ef4444; }

@keyframes pulse {
  0%, 100% { opacity: 1; }
  50% { opacity: 0.4; }
}

.chat-main {
  display: flex;
  flex-direction: column;
  overflow: hidden;
  background: #0a0e17;
}

.welcome {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  padding: 40px;
  text-align: center;
}

.welcome-icon {
  width: 48px; height: 48px;
  background: #2563eb;
  border-radius: 8px;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 24px;
  color: white;
  margin-bottom: 16px;
}

.welcome h2 { font-size: 20px; font-weight: 700; color: #e2e8f0; margin: 0 0 8px; }
.welcome p { font-size: 13px; color: #64748b; margin: 0 0 24px; }
.suggestions { display: flex; gap: 8px; flex-wrap: wrap; justify-content: center; }
.suggestions button {
  background: #1a2332;
  border: 1px solid #243447;
  border-radius: 6px;
  padding: 8px 14px;
  color: #94a3b8;
  font-size: 12px;
  cursor: pointer;
  font-family: inherit;
}
.suggestions button:hover { border-color: #3b82f6; color: #e2e8f0; }

.messages {
  flex: 1;
  overflow-y: auto;
  padding: 16px 20px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.message { display: flex; flex-direction: column; max-width: 80%; }
.message.user { align-self: flex-end; }
.message.assistant, .message.system { align-self: flex-start; }
.message.system.steps { max-width: 90%; }

.message-content {
  padding: 10px 14px;
  border-radius: 12px;
  font-size: 13px;
  line-height: 1.5;
}

.message.user .message-content {
  background: #2563eb;
  color: white;
  border-bottom-right-radius: 4px;
}

.message.assistant .message-content {
  background: #161f2e;
  color: #e2e8f0;
  border: 1px solid #243447;
  border-bottom-left-radius: 4px;
}

.message.system .message-content {
  background: #1a2332;
  color: #94a3b8;
  border: 1px solid #243447;
  border-bottom-left-radius: 4px;
  font-size: 12px;
}

.message.system.steps .message-content {
  padding: 10px 12px;
}

.steps-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 8px;
  padding-bottom: 6px;
  border-bottom: 1px solid #243447;
}

.steps-title {
  font-size: 12px;
  font-weight: 700;
  color: #3b82f6;
}

.running-tag {
  font-size: 10px;
  color: #3b82f6;
  margin-left: 6px;
  animation: pulse 1.5s infinite;
}

.steps-list {
  display: flex;
  flex-direction: column;
  gap: 4px;
  max-height: 320px;
  overflow-y: auto;
}

.step-item {
  display: grid;
  grid-template-columns: 18px 60px auto auto auto 1fr;
  gap: 6px;
  align-items: start;
  padding: 4px 6px;
  border-radius: 3px;
  font-size: 11px;
  border-left: 2px solid #243447;
  background: #0a0e17;
}

.step-item:hover { background: #161f2e; }

.step-icon { font-size: 11px; line-height: 1.5; }
.step-time { color: #64748b; font-size: 10px; line-height: 1.5; }
.step-agent { color: #3b82f6; font-weight: 600; font-size: 10px; line-height: 1.5; max-width: 140px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

.step-kind {
  font-size: 9px;
  padding: 1px 5px;
  border-radius: 3px;
  font-weight: 700;
  line-height: 1.5;
  height: fit-content;
}

.kind-think, .kind-think { background: rgba(234,179,8,0.15); color: #eab308; }
.kind-intend, .kind-intend { background: rgba(59,130,246,0.15); color: #3b82f6; }
.kind-llm, .kind-llm { background: rgba(168,85,247,0.15); color: #a855f7; }
.kind-tool_call, .kind-tool_call { background: rgba(34,197,94,0.15); color: #22c55e; }
.kind-tool_result, .kind-tool_result { background: rgba(6,182,212,0.15); color: #06b6d4; }
.kind-tool_exec { background: rgba(249,115,22,0.15); color: #f97316; }
.kind-wait { background: rgba(100,116,139,0.15); color: #64748b; }
.kind-error { background: rgba(239,68,68,0.15); color: #ef4444; }
.kind-prompt { background: rgba(139,92,246,0.15); color: #a78bfa; }
.kind-agent_created { background: rgba(99,102,241,0.15); color: #818cf8; }
.kind-token_usage { background: rgba(100,116,139,0.1); color: #64748b; font-size: 9px; }
.kind-graph_step { background: rgba(59,130,246,0.15); color: #3b82f6; }

.step-msg {
  color: #94a3b8;
  white-space: pre-wrap;
  word-break: break-all;
  line-height: 1.5;
}

.step-detail {
  grid-column: 1 / -1;
  margin-top: 4px;
  background: #0a0e17;
  border: 1px solid #243447;
  border-radius: 3px;
  font-size: 10px;
}

.step-detail summary {
  padding: 3px 8px;
  cursor: pointer;
  color: #64748b;
}

.step-detail pre {
  padding: 6px 8px;
  margin: 0;
  white-space: pre-wrap;
  overflow-x: auto;
  color: #94a3b8;
  border-top: 1px solid #243447;
  font-family: 'JetBrains Mono', monospace;
}

.step-detail pre.error { color: #ef4444; }

.message-time {
  font-size: 10px;
  color: #64748b;
  margin-top: 4px;
  padding: 0 4px;
}

.message.user .message-time { align-self: flex-end; }

.typing-indicator { display: flex; gap: 4px; padding: 4px 0; }
.typing-indicator span {
  width: 6px; height: 6px;
  background: #64748b;
  border-radius: 50%;
  animation: bounce 1.4s infinite ease-in-out;
}
.typing-indicator span:nth-child(1) { animation-delay: 0s; }
.typing-indicator span:nth-child(2) { animation-delay: 0.2s; }
.typing-indicator span:nth-child(3) { animation-delay: 0.4s; }

@keyframes bounce {
  0%, 80%, 100% { transform: translateY(0); }
  40% { transform: translateY(-4px); }
}

.input-area {
  display: flex;
  gap: 8px;
  padding: 12px 20px;
  border-top: 1px solid #243447;
  background: #111827;
}

.input-area textarea {
  flex: 1;
  background: #161f2e;
  border: 1px solid #243447;
  border-radius: 8px;
  padding: 10px 14px;
  color: #e2e8f0;
  font-size: 13px;
  font-family: inherit;
  resize: none;
  outline: none;
  line-height: 1.5;
}

.input-area textarea:focus { border-color: #3b82f6; }
.input-area textarea:disabled { opacity: 0.5; }

.send-btn {
  background: #2563eb;
  border: none;
  border-radius: 8px;
  padding: 10px 20px;
  color: white;
  font-size: 13px;
  font-weight: 600;
  cursor: pointer;
  font-family: inherit;
  align-self: flex-end;
}

.send-btn:hover:not(:disabled) { background: #3b82f6; }
.send-btn:disabled { opacity: 0.5; cursor: not-allowed; }
</style>
