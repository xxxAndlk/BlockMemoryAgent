import { fetchJson } from './client'

/** 经验技能（learned_skills 表元数据，与后端 store.LearnedSkill 对齐）。 */
export interface LearnedSkill {
  name: string
  title: string
  when_to_use: string
  content_path: string
  enabled: boolean
  use_count: number
  source_session?: string
  outcome?: string
  /** 是否携带配套可执行脚本（frontmatter tools 清单非空，阶段 C）。 */
  has_tools?: boolean
  /** 最近一次 load_skill 加载时间（冷淘汰判定依据）；从未使用为空。 */
  last_used_at?: string
  created_at: string
  updated_at: string
}

/** 技能配套脚本条目（frontmatter tools 清单项；PUT 编辑不支持变更，仅只读展示）。 */
export interface SkillTool {
  path: string
  desc: string
  run?: string
}

export interface LearnedSkillDetail {
  skill: LearnedSkill
  content: string
}

/** 进化审计流水（evolution_log 表）。 */
export interface EvolutionLogEntry {
  id: number
  kind: string // user_pref|project_lesson|skill_create|skill_update
  target: string
  summary: string
  source_session?: string
  created_at: string
}

export function listLearnedSkills(): Promise<{ skills: LearnedSkill[] }> {
  return fetchJson('/skills/learned')
}

export function getLearnedSkill(name: string): Promise<LearnedSkillDetail> {
  return fetchJson(`/skills/learned/${encodeURIComponent(name)}`)
}

export function saveLearnedSkill(
  name: string,
  body: { title: string; when_to_use: string; content: string; tools?: SkillTool[] }
): Promise<{ ok: boolean }> {
  return fetchJson(`/skills/learned/${encodeURIComponent(name)}`, {
    method: 'PUT',
    body: JSON.stringify(body),
  })
}

export function enableLearnedSkill(name: string): Promise<{ ok: boolean }> {
  return fetchJson(`/skills/learned/${encodeURIComponent(name)}/enable`, { method: 'POST' })
}

export function disableLearnedSkill(name: string): Promise<{ ok: boolean }> {
  return fetchJson(`/skills/learned/${encodeURIComponent(name)}/disable`, { method: 'POST' })
}

/** 手动删除经验技能（物理删）：PG 行 + 技能文件 + 技能池退池，写 evolution_log（skill_delete）。 */
export function deleteSkill(name: string): Promise<{ ok: boolean }> {
  return fetchJson(`/skills/learned/${encodeURIComponent(name)}`, { method: 'DELETE' })
}

/**
 * 重铸单个存量技能：decision 模型按当前产出契约重写正文（背景/≥3 条带锚点步骤/坑点/验证/示例）。
 * 结构门或评分不过时后端返回 422，错误消息即拒绝理由（旧内容保留）。
 */
export function reforgeSkill(name: string): Promise<{ ok: boolean; summary: string }> {
  return fetchJson(`/skills/learned/${encodeURIComponent(name)}/reforge`, {
    method: 'POST',
    timeoutMs: 150000,
  })
}

export function listEvolutionLog(limit = 200): Promise<{ entries: EvolutionLogEntry[] }> {
  return fetchJson(`/evolution/log?limit=${limit}`)
}

/** 手动触发一轮技能库整理（合并语义重复技能 + 归档零使用技能）；轻量模型调用，耗时可达约 2 分钟。 */
export function consolidateSkills(): Promise<{ ok: boolean; summary: string }> {
  return fetchJson('/skills/consolidate', { method: 'POST', timeoutMs: 150000 })
}
