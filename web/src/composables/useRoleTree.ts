import { computed, unref } from 'vue'
import type { MaybeRef } from 'vue'
import type { AgentNode } from '@/types'

export interface RoleTreeNode {
  label: string
  status: string
  statusType: string
  active: boolean
  isUser: boolean
  iconColor: string
  /** 活动证据小字（TODO 第10项②展示面）："in ReadFile · active 12s ago"，仅运行节点携带 */
  activity: string
  children: RoleTreeNode[]
}

// formatActivityEvidence 把活动证据渲染为节点小字（TODO 第10项②展示面）：
// tool:<名> → "in <名> · active Xs ago"；llm_start/stream → thinking。
function formatActivityEvidence(kind?: string, ago?: string): string {
  const k = (kind ?? '').trim()
  const t = (ago ?? '').trim()
  if (!k || !t) return ''
  if (k.startsWith('tool:')) return `in ${k.slice(5)} · active ${t} ago`
  if (k === 'llm_start' || k === 'stream') return `thinking · active ${t} ago`
  if (k === 'user_wait') return `awaiting user · ${t} ago`
  return `${k} · active ${t} ago`
}

export function useRoleTree(agents: MaybeRef<AgentNode[]>) {
  const roleTree = computed<RoleTreeNode[]>(() => {
    const list = unref(agents)
    const root: RoleTreeNode[] = []
    const map = new Map<string, RoleTreeNode>()

    list.forEach((a) => {
      // 热驻（Idle）节点不在编排树展示（任务 114 对齐 TUI）：任务完结转 Idle 待复用，
      // 复用清单只供 MetaAgent 注入，对用户是噪音。
      if (a.status === 'idle') return
      const node: RoleTreeNode = {
        label: a.name,
        status: a.status,
        statusType:
          a.status === 'active'
            ? 'success'
            : a.status === 'running'
              ? 'warning'
              : a.status === 'delivered-unverified'
                ? 'warning'
                : a.status === 'failed'
                  ? 'danger'
                  : 'info',
        active: a.status === 'active' || a.status === 'running',
        isUser: a.type === 'domain' || a.type === 'subdomain',
        iconColor:
          a.status === 'active'
            ? 'text-green-500'
            : a.status === 'running'
              ? 'text-yellow-500'
              : a.status === 'delivered-unverified'
                ? 'text-yellow-500'
                : a.status === 'failed'
                  ? 'text-red-500'
                  : 'text-gray-500',
        activity: formatActivityEvidence(a.activity_kind, a.last_activity_ago),
        children: [],
      }
      map.set(a.inst_id, node)
      if (!a.parent_id) root.push(node)
    })

    list.forEach((a) => {
      if (!a.parent_id) return // 根节点已在第一轮挂上
      if (map.has(a.parent_id)) {
        map.get(a.parent_id)!.children.push(map.get(a.inst_id)!)
      } else {
        // 父节点缺失（后端 parent_id 与实例 ID 口径漂移的兜底）：挂顶层而非静默丢弃，
        // 否则整棵子树从编排面板消失（2026-09-11 实证：只剩 MetaAgent）。
        root.push(map.get(a.inst_id)!)
      }
    })

    return root
  })

  const defaultProps = { children: 'children', label: 'label' }

  return { roleTree, defaultProps }
}
