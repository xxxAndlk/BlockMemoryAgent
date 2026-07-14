package types

import "time"

// AgentResult 下级 Agent（Assistant / DomainAgent / SubDomainAgent）完成任务后
// 返回给 MetaAgent 的结构化结果。
//
// 设计意图（P0-1）：把"给用户看的总结"与"给 MetaAgent 的调度记忆"解耦，
// 让 MetaAgent 不记录繁琐上下文，只保留足够调度用的轻量记忆。
type AgentResult struct {
	// SummaryForUser 面向用户的自然语言总结。
	// MetaAgent 在 finalizeSession 时把多个 SummaryForUser 合并为最终回复。
	SummaryForUser string `json:"summary_for_user"`

	// MemoryForMeta 面向 MetaAgent 的调度记忆。
	// 包含关键结论、决策依据、未决事项、领域状态等，供后续路由与注入使用。
	MemoryForMeta string `json:"memory_for_meta"`

	// Facts 关键事实列表（键值对或短句）。
	// 可沉淀到块记忆或全局知识，供跨任务/跨会话召回。
	Facts []string `json:"facts,omitempty"`

	// Error 非空表示任务执行失败，MetaAgent 可据此决定是否重试或升级。
	Error string `json:"error,omitempty"`
}

// Text 兼容辅助：快速取面向用户的文本摘要。
// 当调用方只需要字符串结果时，返回 SummaryForUser；空结果时返回 Error。
func (r *AgentResult) Text() string {
	if r == nil {
		return ""
	}
	if r.SummaryForUser != "" {
		return r.SummaryForUser
	}
	if r.MemoryForMeta != "" {
		return r.MemoryForMeta
	}
	return r.Error
}

// MetaMemoryEntry 单条 MetaAgent 调度记忆。
// 可沉淀到会话记忆或全局知识库，供后续路由与上下文注入使用。
type MetaMemoryEntry struct {
	// Timestamp 记忆创建时间。
	Timestamp time.Time `json:"timestamp"`

	// Source 来源 Agent 实例ID或角色名，便于追溯。
	Source string `json:"source"`

	// Content 记忆内容，要求简洁、事实化、无冗余上下文。
	Content string `json:"content"`

	// Tags 标签，用于分类与检索。取值：decision / fact / todo / escalation / summary。
	Tags []string `json:"tags"`
}

// HasTag 检查记忆是否包含指定标签。
func (e *MetaMemoryEntry) HasTag(tag string) bool {
	for _, t := range e.Tags {
		if t == tag {
			return true
		}
	}
	return false
}
