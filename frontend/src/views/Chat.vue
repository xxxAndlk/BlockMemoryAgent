<script setup lang="ts">
import { ref, onMounted, nextTick } from 'vue'
import type { Session, SessionEvent } from '../types'

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
  evtSource.value.onmessage = (e) => {
    const d = JSON.parse(e.data)

    if (d.type === 'done') {
      evtSource.value?.close()
      loading.value = false
      loadSessions()
      return
    }

    if (initial && d.id) {
      initial = false
      activeSession.value = d
      buildDisplayMessages(d)
      return
    }

    if (activeSession.value) {
      activeSession.value.events.push(d)
      if (d.type === 'progress') {
        addOrUpdateProgressEvent(d)
      } else if (d.type === 'system' && d.agent === 'MetaAgent') {
        if (d.message.startsWith('会话完成')) {
          displayMessages.value.push({
            role: 'assistant',
            content: d.message.replace('会话完成: ', ''),
            timestamp: d.timestamp,
          })
        }
      } else if (d.type === 'tool_exec') {
        const last = displayMessages.value[displayMessages.value.length - 1]
        if (last && last.role === 'system') {
          last.events = last.events || []
          last.events.push(d)
        } else {
          displayMessages.value.push({
            role: 'system',
            content: '',
            timestamp: d.timestamp,
            events: [d],
          })
        }
      }
      scrollToBottom()
    }
  }
}

function buildDisplayMessages(session: Session) {
  displayMessages.value = []
  if (!session.messages) return

  for (const msg of session.messages) {
    if (msg.role === 'system' && msg.content.startsWith('Goal:')) continue
    displayMessages.value.push({
      role: msg.role as 'user' | 'assistant' | 'system',
      content: msg.content,
      timestamp: msg.timestamp,
    })
  }
}

function addOrUpdateProgressEvent(ev: SessionEvent) {
  if (ev.kind === 'think' || ev.kind === 'intend' || ev.kind === 'llm') {
    const last = displayMessages.value[displayMessages.value.length - 1]
    if (last && last.role === 'assistant' && !last.content) {
      return
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
  return new Date(ts).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })
}

function esc(s: string) {
  const d = document.createElement('div'); d.textContent = s; return d.innerHTML
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
        <div
          v-for="(msg, i) in displayMessages"
          :key="i"
          class="message"
          :class="msg.role"
        >
          <div class="message-content">
            <div class="message-text" v-html="esc(msg.content)"></div>
            <div v-if="msg.events && msg.events.length" class="tool-events">
              <details v-for="(ev, j) in msg.events" :key="j">
                <summary>{{ ev.tool }} {{ ev.success ? '✓' : '✗' }}</summary>
                <pre v-if="ev.tool_output">{{ ev.tool_output }}</pre>
                <pre v-if="ev.tool_error" class="error">{{ ev.tool_error }}</pre>
              </details>
            </div>
          </div>
          <div class="message-time">{{ formatTime(msg.timestamp) }}</div>
        </div>

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

.new-btn:hover {
  background: #3b82f6;
}

.list-items {
  flex: 1;
  overflow-y: auto;
  padding: 8px;
}

.session-item {
  padding: 10px;
  border-radius: 6px;
  margin-bottom: 4px;
  cursor: pointer;
  border: 1px solid transparent;
  transition: all 0.15s;
}

.session-item:hover {
  background: #1e293b;
}

.session-item.active {
  background: rgba(37, 99, 235, 0.15);
  border-color: #2563eb;
}

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
  width: 6px;
  height: 6px;
  border-radius: 50%;
  display: inline-block;
}

.status-dot.running {
  background: #3b82f6;
  animation: pulse 1.5s infinite;
}

.status-dot.completed {
  background: #22c55e;
}

.status-dot.error {
  background: #ef4444;
}

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
  width: 48px;
  height: 48px;
  background: #2563eb;
  border-radius: 8px;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 24px;
  color: white;
  margin-bottom: 16px;
}

.welcome h2 {
  font-size: 20px;
  font-weight: 700;
  color: #e2e8f0;
  margin: 0 0 8px;
}

.welcome p {
  font-size: 13px;
  color: #64748b;
  margin: 0 0 24px;
}

.suggestions {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  justify-content: center;
}

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

.suggestions button:hover {
  border-color: #3b82f6;
  color: #e2e8f0;
}

.messages {
  flex: 1;
  overflow-y: auto;
  padding: 16px 20px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.message {
  display: flex;
  flex-direction: column;
  max-width: 80%;
}

.message.user {
  align-self: flex-end;
}

.message.assistant,
.message.system {
  align-self: flex-start;
}

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

.message-time {
  font-size: 10px;
  color: #64748b;
  margin-top: 4px;
  padding: 0 4px;
}

.message.user .message-time {
  align-self: flex-end;
}

.typing-indicator {
  display: flex;
  gap: 4px;
  padding: 4px 0;
}

.typing-indicator span {
  width: 6px;
  height: 6px;
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

.tool-events {
  margin-top: 8px;
}

.tool-events details {
  background: #0a0e17;
  border: 1px solid #243447;
  border-radius: 4px;
  margin-bottom: 4px;
  font-size: 11px;
}

.tool-events summary {
  padding: 6px 10px;
  cursor: pointer;
  color: #94a3b8;
}

.tool-events pre {
  padding: 8px 10px;
  margin: 0;
  white-space: pre-wrap;
  overflow-x: auto;
  color: #64748b;
  border-top: 1px solid #243447;
}

.tool-events pre.error {
  color: #ef4444;
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

.input-area textarea:focus {
  border-color: #3b82f6;
}

.input-area textarea:disabled {
  opacity: 0.5;
}

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

.send-btn:hover:not(:disabled) {
  background: #3b82f6;
}

.send-btn:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
</style>
