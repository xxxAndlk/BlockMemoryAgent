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
  children: RoleTreeNode[]
}

export function useRoleTree(agents: MaybeRef<AgentNode[]>) {
  const roleTree = computed<RoleTreeNode[]>(() => {
    const list = unref(agents)
    const root: RoleTreeNode[] = []
    const map = new Map<string, RoleTreeNode>()

    list.forEach((a) => {
      const node: RoleTreeNode = {
        label: a.name,
        status: a.status,
        statusType:
          a.status === 'active' ? 'success' : a.status === 'running' ? 'warning' : 'info',
        active: a.status === 'active' || a.status === 'running',
        isUser: a.type === 'domain' || a.type === 'subdomain',
        iconColor:
          a.status === 'active' ? 'text-green-500' : a.status === 'running' ? 'text-yellow-500' : 'text-gray-500',
        children: [],
      }
      map.set(a.inst_id, node)
      if (!a.parent_id) root.push(node)
    })

    list.forEach((a) => {
      if (a.parent_id && map.has(a.parent_id)) {
        map.get(a.parent_id)!.children.push(map.get(a.inst_id)!)
      }
    })

    return root
  })

  const defaultProps = { children: 'children', label: 'label' }

  return { roleTree, defaultProps }
}
