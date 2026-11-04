package subagent

// context_audit.go 子 Agent 上下文注入审计（U-02/B-08）：
// 每次派发把"注入了哪些来源、各占多少字符、是否因 fork 硬顶被丢弃、冲突如何裁决"落成
// LiveEventContextInjection（ReactService → stats 事件，detail_json 可查）。
// v2（U-02）由 TaskContextManifest 生成条目，不再用另一个 map 事后拼近似清单；
// manifest=nil（off）时保留 v1 五路计数口径。

import (
	"encoding/json"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/team"
)

// contextAuditVersion 审计信封版本。
const contextAuditVersion = "v2"

// contextAuditEntry 单个上下文来源的注入记录。
type contextAuditEntry struct {
	Source string `json:"source"`           // agents_md/shared_prefix/skill_recall/block_recall/task
	Scope  string `json:"scope"`            // project|task|session|user|role
	Chars  int    `json:"chars"`            // 注入字符数（runes）
	State  string `json:"state"`            // included|dropped
	Reason string `json:"reason,omitempty"` // dropped 原因
	// v2 字段（U-02）：来源、版本与裁决依据。
	EntryID        string `json:"entry_id,omitempty"`
	ConflictKey    string `json:"conflict_key,omitempty"`
	SourceID       string `json:"source_id,omitempty"`
	SourceRevision string `json:"source_revision,omitempty"`
	Trust          string `json:"trust,omitempty"`
	ContentHash    string `json:"content_hash,omitempty"`
	Required       bool   `json:"required,omitempty"`
	Decision       string `json:"decision,omitempty"`
	DropReason     string `json:"drop_reason,omitempty"`
	ArtifactRef    string `json:"artifact_ref,omitempty"`
}

// contextAuditDetail 一次派发的上下文注入审计。
type contextAuditDetail struct {
	Version       string `json:"version"`
	AgentID       string `json:"agent_id"`
	ParentID      string `json:"parent_id"`
	RoleID        string `json:"role_id"`
	TaskRunes     int    `json:"task_runes"`
	Isolated      bool   `json:"isolated"`
	TotalInjected int    `json:"total_injected_runes"`
	// ManifestVersion/Required/Optional 是任务上下文清单的版本化声明（B-08）：
	// Required=目标/任务与共享规格，Optional=项目自述/技能召回/块记忆召回。
	ManifestVersion string              `json:"manifest_version"`
	Required        []string            `json:"required"`
	Optional        []string            `json:"optional"`
	Dropped         []string            `json:"dropped,omitempty"`
	Entries         []contextAuditEntry `json:"entries"`
	// v2 顶层标识（U-02）：与 23 号文档 §3.1 统一标识对齐；legacy 路径 task/attempt 为空。
	Mode             string `json:"mode,omitempty"`
	ExecutionPath    string `json:"execution_path,omitempty"`
	TaskID           string `json:"task_id,omitempty"`
	AttemptID        string `json:"attempt_id,omitempty"`
	PlanVersion      int64  `json:"plan_version,omitempty"`
	SpecVersion      string `json:"spec_version,omitempty"`
	ManifestID       string `json:"manifest_id,omitempty"`
	ManifestHash     string `json:"manifest_hash,omitempty"`
	ActualInputHash  string `json:"actual_model_input_hash,omitempty"`
	ConstraintEvents int    `json:"constraint_events,omitempty"`
}

// emitContextAudit 构造并转发注入审计事件；liveFn 未接线时静默跳过。
// manifest 非 nil 时按其条目输出 v2 清单；否则保留 legacy 五路计数。
func (d *Dispatcher) emitContextAudit(sessionID, subAgentID, parentID, roleID string, injectRunes map[string]int, isolated bool, taskRunes int, manifest *team.TaskContextManifest, actualInputHash string) {
	if d == nil || d.liveFn == nil || sessionID == "" {
		return
	}
	detail := contextAuditDetail{
		Version:         contextAuditVersion,
		AgentID:         subAgentID,
		ParentID:        parentID,
		RoleID:          roleID,
		TaskRunes:       taskRunes,
		Isolated:        isolated,
		ManifestVersion: team.ContextManifestVersion,
		Required:        []string{"task"},
		Mode:            d.contextManifestModeValue(),
		ActualInputHash: actualInputHash,
	}
	if manifest != nil {
		fillManifestAudit(&detail, manifest, isolated)
	} else {
		fillLegacyAudit(&detail, injectRunes, isolated, taskRunes)
	}
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

// fillManifestAudit 从冻结清单生成 v2 审计条目与顶层标识。
func fillManifestAudit(detail *contextAuditDetail, m *team.TaskContextManifest, isolated bool) {
	detail.ExecutionPath = m.ExecutionPath
	detail.TaskID = m.TaskID
	detail.AttemptID = m.AttemptID
	detail.PlanVersion = m.PlanVersion
	detail.SpecVersion = m.SpecVersion
	detail.ManifestID = m.ManifestID
	detail.ManifestHash = m.ContentHash
	detail.ConstraintEvents = len(m.Constraints)

	// 裁决索引：winner→compatible，loser→动作。
	decisionOf := map[string]string{}
	for _, c := range m.Constraints {
		if c.WinnerID != "" {
			decisionOf[c.WinnerID] = team.DecisionCompatible
		}
		for _, id := range c.LoserIDs {
			decisionOf[id] = c.Action
		}
	}
	appendEntry := func(e team.ContextEntry) {
		entry := contextAuditEntry{
			Source: e.Source, Scope: e.Scope, Chars: e.RuneBudget, State: "included",
			EntryID: e.EntryID, ConflictKey: e.ConflictKey, SourceID: e.SourceID,
			SourceRevision: e.SourceRevision, Trust: e.Trust, ContentHash: e.ContentHash,
			Required: e.Required, Decision: decisionOf[e.EntryID], ArtifactRef: e.ArtifactRef,
		}
		if entry.Decision == "" {
			entry.Decision = team.DecisionCompatible
		}
		if isolated && e.Source != team.SourceUserInstruction {
			entry.State = "dropped"
			entry.DropReason = "spawn fork hard cap → isolated"
			entry.Decision = team.DecisionDropped
			detail.Dropped = append(detail.Dropped, e.EntryID)
		} else {
			detail.TotalInjected += e.RuneBudget
		}
		detail.Entries = append(detail.Entries, entry)
		if e.Required {
			detail.Required = append(detail.Required, e.SourceID)
		} else {
			detail.Optional = append(detail.Optional, e.SourceID)
		}
	}
	for _, e := range m.Required {
		appendEntry(e)
	}
	for _, e := range m.Optional {
		appendEntry(e)
	}
}

// fillLegacyAudit 保留 v1 五路计数口径（manifest 未启用时）。
func fillLegacyAudit(detail *contextAuditDetail, injectRunes map[string]int, isolated bool, taskRunes int) {
	order := []struct {
		key   string
		scope string
	}{
		{"agents_md", "project"},
		{"shared_prefix", "task"},
		{"skill_recall", "task"},
		{"block_recall", "session"},
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
			detail.Dropped = append(detail.Dropped, item.key)
		} else {
			detail.TotalInjected += chars
			if item.key == "shared_prefix" {
				detail.Required = append(detail.Required, item.key)
			} else {
				detail.Optional = append(detail.Optional, item.key)
			}
		}
		detail.Entries = append(detail.Entries, entry)
	}
	detail.Entries = append(detail.Entries, contextAuditEntry{
		Source: "task", Scope: "task", Chars: taskRunes, State: "included",
	})
	detail.TotalInjected += taskRunes
}
