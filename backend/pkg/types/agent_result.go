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
// 参数：无（接收者为 *AgentResult）。
// 返回：摘要字符串；nil 接收者返回空串。
func (r *AgentResult) Text() string {
	// 防御 nil 接收者：避免空指针解引用，返回空串。
	if r == nil {
		return ""
	}
	// 优先返回面向用户的总结，这是最直接的结果展示。
	if r.SummaryForUser != "" {
		return r.SummaryForUser
	}
	// 若未生成用户总结，则返回面向 MetaAgent 的调度记忆作为兜底。
	if r.MemoryForMeta != "" {
		return r.MemoryForMeta
	}
	// 若前两者均为空，返回 Error（可能包含失败原因）。
	return r.Error
}

// MetaMemoryEntry 单条 MetaAgent 调度记忆。
// 可沉淀到会话记忆或全局知识库，供后续路由与上下文注入使用。
type MetaMemoryEntry struct {
	// Timestamp 记忆创建时间，用于时间排序与衰减计算。
	Timestamp time.Time `json:"timestamp"`

	// Source 来源 Agent 实例ID或角色名，便于追溯。
	Source string `json:"source"`

	// Content 记忆内容，要求简洁、事实化、无冗余上下文。
	Content string `json:"content"`

	// Tags 标签，用于分类与检索。取值：decision / fact / todo / escalation / summary。
	Tags []string `json:"tags"`
}

// HasTag 检查记忆是否包含指定标签。
// 参数：tag 要检查的标签字符串。
// 返回：true 表示 Tags 中存在该标签；nil 接收者或空切片返回 false。
func (e *MetaMemoryEntry) HasTag(tag string) bool {
	// 防御 nil 接收者：没有 Tags 则不可能包含任何标签。
	if e == nil {
		return false
	}
	// 线性扫描 Tags 切片进行精确匹配。
	for _, t := range e.Tags {
		// 找到相同标签立即返回 true，避免多余遍历。
		if t == tag {
			return true
		}
	}
	// 遍历结束未命中，返回 false。
	return false
}
