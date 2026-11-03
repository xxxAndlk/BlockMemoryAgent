package subagent

// context_audit.go 子 Agent 上下文注入审计（U-02/B-08 最小落地）：
// 每次派发把"注入了哪些来源、各占多少字符、是否因 fork 硬顶被丢弃"落成
// LiveEventContextInjection（ReactService → stats 事件，detail_json 可查）。
// 完整 TaskContextManifest（版本化必传/选传/不传）仍需后续专项建设。

import (
	"encoding/json"

	"github.com/blockmemory/agent/backend/internal/agent"
)

// contextAuditVersion 审计信封版本。
const contextAuditVersion = "v1"

// contextAuditEntry 单个上下文来源的注入记录。
type contextAuditEntry struct {
	Source string `json:"source"`           // agents_md/shared_prefix/skill_recall/block_recall/task
	Scope  string `json:"scope"`            // project|task|session
	Chars  int    `json:"chars"`            // 注入字符数（runes）
	State  string `json:"state"`            // included|dropped
	Reason string `json:"reason,omitempty"` // dropped 原因
}

// contextAuditDetail 一次派发的上下文注入审计。
type contextAuditDetail struct {
	Version       string              `json:"version"`
	AgentID       string              `json:"agent_id"`
	ParentID      string              `json:"parent_id"`
	RoleID        string              `json:"role_id"`
	TaskRunes     int                 `json:"task_runes"`
	Isolated      bool                `json:"isolated"`
	TotalInjected int                 `json:"total_injected_runes"`
	Entries       []contextAuditEntry `json:"entries"`
}

// emitContextAudit 构造并转发注入审计事件；liveFn 未接线时静默跳过。
func (d *Dispatcher) emitContextAudit(sessionID, subAgentID, parentID, roleID string, injectRunes map[string]int, isolated bool, taskRunes int) {
	if d == nil || d.liveFn == nil || sessionID == "" {
		return
	}
	order := []struct {
		key   string
		scope string
	}{
		{"agents_md", "project"},
		{"shared_prefix", "task"},
		{"skill_recall", "task"},
		{"block_recall", "session"},
	}
	detail := contextAuditDetail{
		Version:   contextAuditVersion,
		AgentID:   subAgentID,
		ParentID:  parentID,
		RoleID:    roleID,
		TaskRunes: taskRunes,
		Isolated:  isolated,
	}
	for _, item := range order {
		chars := injectRunes[item.key]
		if chars <= 0 {
			continue
		}
		entry := contextAuditEntry{Source: item.key, Scope: item.scope, Chars: chars, State: "included"}
		if isolated {
			entry.State = "dropped"
			entry.Reason = "spawn fork hard cap → isolated"
		} else {
			detail.TotalInjected += chars
		}
		detail.Entries = append(detail.Entries, entry)
	}
	detail.Entries = append(detail.Entries, contextAuditEntry{
		Source: "task", Scope: "task", Chars: taskRunes, State: "included",
	})
	detail.TotalInjected += taskRunes
	payload, err := json.Marshal(detail)
	if err != nil {
		return
	}
	d.liveFn(sessionID, agent.LiveEvent{
		Kind:    agent.LiveEventContextInjection,
		Agent:   roleID,
		AgentID: subAgentID,
		Detail:  string(payload),
	})
}
