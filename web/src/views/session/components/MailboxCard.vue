<script setup lang="ts">
import { fmtDateTime } from '@/utils/date'
import type { MailboxMessage } from '@/api/session'

const props = defineProps<{
  messages: MailboxMessage[]
}>()

function tagType(type: string) {
  switch (type) {
    case 'milestone': return 'success'
    case 'request': return 'primary'
    case 'escalate': return 'danger'
    case 'dependency': return 'warning'
    case 'info': return 'info'
    default: return 'info'
  }
}

function icon(type: string) {
  switch (type) {
    case 'milestone': return 'Trophy'
    case 'request': return 'Connection'
    case 'escalate': return 'WarningFilled'
    case 'dependency': return 'Link'
    case 'info': return 'Document'
    default: return 'Message'
  }
}
</script>

<template>
  <el-card class="!border-line !bg-card">
    <template #header>
      <div class="flex justify-between items-center">
        <div class="font-bold text-sm text-ink">邮箱通知 (Mailbox)</div>
      </div>
    </template>
    <div class="flex justify-between text-xs mb-4">
      <span class="text-ink-2">未读消息 ({{ props.messages.length }})</span>
    </div>
    <div class="space-y-4">
      <div v-for="mail in props.messages.slice(0, 8)" :key="mail.id" class="flex gap-3 text-xs">
        <div class="mt-0.5 rounded-full p-1 shrink-0 bg-page">
          <el-icon class="text-ink-2"><component :is="icon(mail.type)" /></el-icon>
        </div>
        <div class="flex-1 min-w-0">
          <div class="flex justify-between mb-1">
            <span class="font-bold" :class="'text-' + tagType(mail.type) + '-500'">{{ mail.type }}</span>
            <span class="text-ink-2">{{ fmtDateTime(mail.created_at) }}</span>
          </div>
          <div class="text-ink truncate">{{ mail.subject }}</div>
          <div class="text-ink-2 mt-1 truncate">{{ mail.from }} → {{ mail.to }}</div>
        </div>
      </div>
      <div v-if="!props.messages.length" class="text-ink-2 text-xs">暂无未读消息</div>
    </div>
  </el-card>
</template>
