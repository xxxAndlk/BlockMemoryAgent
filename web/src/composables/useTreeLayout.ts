import { computed, unref } from 'vue'
import type { ComputedRef, MaybeRef } from 'vue'
import type { AgentNode } from '@/types'

// ===== 编排树图几何常量（单位 px，画布逻辑坐标）=====
export const NODE_W = 208
export const NODE_H = 76
export const H_GAP = 28
export const V_GAP = 64

// 画布四周留白：最右/最深节点不贴边，"适应"缩放也留呼吸空间。
const CANVAS_PAD = 48

/** LayoutNode 是树图渲染单元：x/y 为节点左上角的画布逻辑坐标，depth 为层级（根=0）。 */
export interface LayoutNode {
  id: string
  name: string
  type: string
  status: string
  activityKind: string
  /** 已格式化好的活动小字（见 formatActivityText），空串=无活动（卡片留空占位） */
  activityText: string
  x: number
  y: number
  depth: number
}

/** LayoutEdge 是父子连线：(x1,y1)=父底边中点，(x2,y2)=子顶边中点。 */
export interface LayoutEdge {
  from: string
  to: string
  x1: number
  y1: number
  x2: number
  y2: number
}

export interface TreeLayout {
  /**
   * 森林的扁平化结果（含全部层级节点，非仅顶层）：LayoutNode 不带 children 字段，
   * 画布 v-for 直接渲染即可，层级关系由 edges 与 depth 表达。
   */
  roots: ComputedRef<LayoutNode[]>
  edges: ComputedRef<LayoutEdge[]>
  width: ComputedRef<number>
  height: ComputedRef<number>
}

/**
 * isChildWaiting 判定"运行中且在等下级返回"（后端 subagent 上报的 child_wait 是纯展示态：
 * 只更新 lastKind 不刷新 lastTS，故不能只看 activity_kind 的时间新鲜度）。
 * 同时兼容两种字段口径：AgentNode（activity_kind，snake_case，HTTP 线型）与
 * LayoutNode（activityKind）——节点卡用后者，对话面板用前者，避免调用点各写一遍条件。
 */
export function isChildWaiting(
  n: { status?: string; activityKind?: string; activity_kind?: string } | null | undefined,
): boolean {
  // 'active' 是 MetaAgent 的会话级运行态（RoleStatusActive），它与子 Agent 的 'running'
  // 同属"在跑"，等下级时同样该显示"等下级返回"（meta 的发送框另有 meta 分支禁用）。
  if (!n || (n.status !== 'running' && n.status !== 'active')) return false
  return (n.activityKind || n.activity_kind || '') === 'child_wait'
}

/**
 * formatActivityText 与 useRoleTree.formatActivityEvidence 同口径（Task 13 删除该文件后由本文件承接）：
 * `tool:<名>` → "in <名> · active Xs ago"、`llm_start/stream` → "thinking · active Xs ago"、
 * `user_wait` → "awaiting user · Xs ago"、其余原样。
 * child_wait 额外特判为"等下级返回"中文小字：ago 缺失时也要出字，否则等子节点看着像假死。
 */
function formatActivityText(kind?: string, ago?: string): string {
  const k = (kind ?? '').trim()
  const t = (ago ?? '').trim()
  if (!k) return ''
  if (k === 'child_wait') return t ? `等下级返回 · ${t} ago` : '等下级返回'
  if (!t) return ''
  if (k.startsWith('tool:')) return `in ${k.slice(5)} · active ${t} ago`
  if (k === 'llm_start' || k === 'stream') return `thinking · active ${t} ago`
  if (k === 'user_wait') return `awaiting user · ${t} ago`
  return `${k} · active ${t} ago`
}

interface BuildResult {
  roots: LayoutNode[]
  edges: LayoutEdge[]
  width: number
  height: number
}

/**
 * buildLayout 用 tidy-tree 两遍法（后序算单位宽、前序分配水平区间）布局整片森林。
 * 纯函数：每次调用都重建新对象，不共享可变状态（computed 重算安全）。
 */
function buildLayout(list: AgentNode[]): BuildResult {
  const nodes: LayoutNode[] = []
  const byId = new Map<string, LayoutNode>()
  const instById = new Map<string, AgentNode>()

  for (const a of list) {
    // 热驻（idle）节点不入树：任务完结转 Idle 待复用，复用清单只供 MetaAgent 派发决策，
    // 对用户是噪音（与 useRoleTree 同口径，Task 13 后由本文件承接该约定）。
    if (a.status === 'idle') continue
    const node: LayoutNode = {
      id: a.inst_id,
      name: a.name || a.inst_id,
      type: a.type,
      status: a.status,
      activityKind: a.activity_kind ?? '',
      activityText: formatActivityText(a.activity_kind, a.last_activity_ago),
      x: 0,
      y: 0,
      depth: 0,
    }
    byId.set(node.id, node)
    instById.set(node.id, a)
    nodes.push(node)
  }
  if (!nodes.length) return { roots: [], edges: [], width: 0, height: 0 }

  // 建森林：按 parent_id 挂子。父缺失（被 idle 过滤或后端 parent_id 与实例 ID 口径漂移）
  // 一律升为根而非静默丢弃——否则整棵子树从树图消失（2026-09-11 实证：只剩 MetaAgent）。
  // 自指 parent_id 也按根处理，避免自环把后序递归拖死。
  const childrenOf = new Map<string, LayoutNode[]>()
  const roots: LayoutNode[] = []
  for (const [id, node] of byId) {
    const parentId = (instById.get(id)?.parent_id ?? '').trim()
    const parent = parentId && parentId !== id ? byId.get(parentId) : undefined
    if (!parent) {
      roots.push(node)
      continue
    }
    const kids = childrenOf.get(parent.id)
    if (kids) kids.push(node)
    else childrenOf.set(parent.id, [node])
  }

  // 稳定排序（根与同级子节点都按 name）：同一份数据每次渲染布局一致，连线不跳。
  const byName = (a: LayoutNode, b: LayoutNode) => a.name.localeCompare(b.name)
  roots.sort(byName)
  for (const kids of childrenOf.values()) kids.sort(byName)

  // 第一遍（后序）：子树"单位宽度"= max(1, Σ 子子树单位宽)，叶子=1。
  const units = new Map<string, number>()
  const measured = new Set<string>()
  const measure = (n: LayoutNode): number => {
    if (measured.has(n.id)) return units.get(n.id) ?? 1
    measured.add(n.id)
    let sum = 0
    for (const k of childrenOf.get(n.id) ?? []) sum += measure(k)
    const u = Math.max(1, sum)
    units.set(n.id, u)
    return u
  }

  // 第二遍（前序）：按单位宽分配水平区间，节点居中于自身区间（区间中点 - 半宽）。
  // 有子节点时 parents 区间宽 = Σ 子区间宽，子节点正好铺满父区间，天然居中对齐。
  const unitW = NODE_W + H_GAP
  const placed = new Set<string>()
  const place = (n: LayoutNode, left: number, depth: number) => {
    if (placed.has(n.id)) return
    placed.add(n.id)
    const u = units.get(n.id) ?? 1
    n.depth = depth
    n.y = depth * (NODE_H + V_GAP)
    n.x = left + (u * unitW) / 2 - NODE_W / 2
    let cursor = left
    for (const k of childrenOf.get(n.id) ?? []) {
      if (placed.has(k.id)) continue
      place(k, cursor, depth + 1)
      cursor += (units.get(k.id) ?? 1) * unitW
    }
  }

  let cursor = 0
  const placeRoots = (list2: LayoutNode[]) => {
    for (const r of list2) {
      if (placed.has(r.id)) continue
      measure(r)
      place(r, cursor, 0)
      cursor += (units.get(r.id) ?? 1) * unitW
    }
  }
  placeRoots(roots)
  // 兜底：环状脏数据下节点既非根也非任何根的后代，补挂为根——保证"从树里消失"的唯一原因是 idle。
  const orphan = nodes.filter((n) => !placed.has(n.id))
  if (orphan.length) {
    roots.push(...orphan)
    placeRoots(orphan)
  }

  // 边：父底边中点 → 子顶边中点（父不在布局里则不画，如父被 idle 过滤）。
  const edges: LayoutEdge[] = []
  for (const n of nodes) {
    if (!placed.has(n.id)) continue
    const parentId = (instById.get(n.id)?.parent_id ?? '').trim()
    const p = parentId && parentId !== n.id ? byId.get(parentId) : undefined
    if (!p || !placed.has(p.id)) continue
    edges.push({
      from: p.id,
      to: n.id,
      x1: p.x + NODE_W / 2,
      y1: p.y + NODE_H,
      x2: n.x + NODE_W / 2,
      y2: n.y,
    })
  }

  // 画布尺寸：最右/最深节点边界 + 留白（供 SVG 尺寸与"适应"缩放计算）。
  let maxX = 0
  let maxY = 0
  for (const n of nodes) {
    if (!placed.has(n.id)) continue
    maxX = Math.max(maxX, n.x + NODE_W)
    maxY = Math.max(maxY, n.y + NODE_H)
  }

  // 渲染顺序按 (depth, x) 定序：父先于子、左先于右，DOM 顺序与视觉顺序一致（截图/叠放不随输入顺序漂）。
  const flat = nodes.filter((n) => placed.has(n.id)).sort((a, b) => a.depth - b.depth || a.x - b.x)

  return { roots: flat, edges, width: maxX + CANVAS_PAD, height: maxY + CANVAS_PAD }
}

/**
 * useTreeLayout 把 Agent 实例列表（/api/sessions/{id}/agents 的 agents）算成树图布局：
 * 输出扁平节点、父子连线与画布尺寸，全部 computed，agents 变化即重排。
 */
export function useTreeLayout(agents: MaybeRef<AgentNode[]>): TreeLayout {
  const layout = computed(() => buildLayout(unref(agents) ?? []))
  const roots = computed(() => layout.value.roots)
  const edges = computed(() => layout.value.edges)
  const width = computed(() => layout.value.width)
  const height = computed(() => layout.value.height)
  return { roots, edges, width, height }
}
