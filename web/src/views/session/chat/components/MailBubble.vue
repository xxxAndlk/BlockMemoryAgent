<script setup lang="ts">
// MailBubble.vue Agent 间邮件气泡（mailbox 留痕渲染）：一次投递一行，
// 方向可辨（→ 发给谁 / ← 来自谁）、类型可辨（询问/回复/通知/升级）、正文可读。
// 入站邮件在对话区另有 `[mailbox from X]` 注入气泡（Agent 自己的上下文），
// 本组件补充的是**留痕视角**——特别是外发（此前界面上完全看不到）。
import { computed } from 'vue'
import type { AgentMailItem } from '@/api/session'
import { mailBody, mailSubject, mailTypeColor, mailTypeLabel } from '../utils/mails'
import { fmtTime } from '../utils/eventStyles'

const props = defineProps<{
  mail: AgentMailItem
  /** 当前视角 Agent 的实例 ID：from===self 即"我发出的"（右侧/强调），否则"我收到的" */
  selfId?: string
  /** 实例 ID → 展示名（领域名/角色名），缺失回退原值 */
  nameOf?: (id: string) => string
}>()

const outbound = computed(() => !!props.selfId && props.mail.from === props.selfId)
const peerId = computed(() => (outbound.value ? props.mail.to : props.mail.from))
const peerLabel = computed(() => props.nameOf?.(peerId.value) || peerId.value || '(未知)')
const subject = computed(() => mailSubject(props.mail))
const body = computed(() => mailBody(props.mail).trim())
</script>

<template>
  <div class="my-1.5 rounded-lg border border-line bg-page/70 px-3 py-2"
       :class="outbound ? 'border-l-2 border-l-primary' : 'border-l-2 border-l-line'">
    <div class="flex items-center gap-2 text-[11px] text-ink-3 flex-wrap">
      <el-icon class="text-sm" :class="outbound ? 'text-primary' : 'text-ink-2'">
        <Promotion v-if="outbound" /><Message v-else />
      </el-icon>
      <span class="text-ink-2">{{ outbound ? '发给' : '来自' }}</span>
      <span class="text-ink truncate max-w-[220px]" :title="peerId">{{ peerLabel }}</span>
      <el-tag size="small" effect="plain" class="scale-90" :type="mailTypeColor(mail.msg_type)">
        {{ mailTypeLabel(mail.msg_type) }}
      </el-tag>
      <span class="ml-auto shrink-0">{{ fmtTime(mail.at) }}</span>
    </div>
    <div class="text-xs text-ink mt-1 break-words">{{ subject }}</div>
    <div v-if="body" class="text-[11px] text-ink-2 mt-1 whitespace-pre-wrap break-words max-h-40 overflow-auto">{{ body }}</div>
  </div>
</template>
