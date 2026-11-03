package team

// context.go TaskContextManifest v2 中立契约（U-02）：
// 只放数据结构与纯函数，不 import subagent/board/agent，避免 Team 契约与执行器循环依赖。
// 该 Manifest 的目标是统一“实际给执行者看了什么”：来源、版本、冲突裁决与必传保真。
// 首版（U02-1）只做 shadow 对拍，模型输入不变；enforce 由后续 PR 接入。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ContextManifestVersion 当前清单版本。
const ContextManifestVersion = "v2"

// 上下文来源标识（与 23 号文档 §4.3 优先级表对应）。
const (
	SourceUserInstruction = "user_instruction"
	SourceTaskSpec        = "task_spec"
	SourceAgentsMD        = "agents_md"
	SourceProjectMemory   = "project_memory"
	SourceUserMemory      = "user_memory"
	SourceSkill           = "skill"
	SourceSharedFact      = "shared_fact"
	SourceBlockRecall     = "block_recall"
	SourceToolResult      = "tool_result"
)

// 信任等级。
const (
	TrustAuthoritative = "authoritative"
	TrustVerified      = "verified"
	TrustUntrusted     = "untrusted"
)

// 作用域。
const (
	ScopeTask    = "task"
	ScopeProject = "project"
	ScopeUser    = "user"
	ScopeSession = "session"
	ScopeRole    = "role"
)

// 裁决动作（ConstraintDecision.Action）。
const (
	DecisionCompatible = "compatible"
	DecisionSuperseded = "superseded"
	DecisionShadowed   = "shadowed"
	DecisionUnresolved = "unresolved"
	DecisionRevoked    = "revoked"
	DecisionDropped    = "dropped_budget"
	DecisionDuplicate  = "duplicate"
)

// ExecutionPathLegacyDelegate 无 task/attempt 绑定的旧派发路径（C-03 前唯一路径）。
const ExecutionPathLegacyDelegate = "legacy_delegate"

// TaskContextManifest 一次派发的不可变上下文清单。
// 字段名可按项目风格微调，语义不可删除（23 号文档 §4.2）。
type TaskContextManifest struct {
	Version           string               `json:"version"`
	ManifestID        string               `json:"manifest_id"`
	SessionID         string               `json:"session_id"`
	TaskID            string               `json:"task_id,omitempty"`
	AttemptID         string               `json:"attempt_id,omitempty"`
	PlanVersion       int64                `json:"plan_version"`
	SpecVersion       string               `json:"spec_version,omitempty"`
	RoleID            string               `json:"role_id"`
	Owner             string               `json:"owner"`
	ProjectKey        string               `json:"project_key"`
	EffectiveRevision int64                `json:"effective_revision"`
	ExecutionPath     string               `json:"execution_path"`
	Required          []ContextEntry       `json:"required"`
	Optional          []ContextEntry       `json:"optional"`
	Constraints       []ConstraintDecision `json:"constraints,omitempty"`
	ContentHash       string               `json:"content_hash"`
	FrozenAt          time.Time            `json:"frozen_at"`
}

// ContextEntry 单个上下文来源条目。
type ContextEntry struct {
	EntryID        string `json:"entry_id"`
	ConflictKey    string `json:"conflict_key,omitempty"` // 仅同 scope、同主题才允许冲突裁决
	Source         string `json:"source"`                 // user_instruction/task_spec/agents_md/...
	SourceID       string `json:"source_id,omitempty"`    // M-xxx、Spec key、skill name、fact id 等
	SourceRevision string `json:"source_revision,omitempty"`
	Scope          string `json:"scope"` // task/project/user/session/role
	Trust          string `json:"trust"` // authoritative/verified/untrusted
	ContentHash    string `json:"content_hash"`
	Content        string `json:"content,omitempty"` // required 短文本冻结；optional 只留 hash
	ArtifactRef    string `json:"artifact_ref,omitempty"`
	RuneBudget     int    `json:"rune_budget,omitempty"`
	Required       bool   `json:"required"`
}

// ConstraintDecision 冲突裁决结果。
type ConstraintDecision struct {
	ConflictKey string   `json:"conflict_key"`
	Scope       string   `json:"scope"`
	WinnerID    string   `json:"winner_id,omitempty"`
	LoserIDs    []string `json:"loser_ids,omitempty"`
	Action      string   `json:"action"` // compatible/superseded/shadowed/unresolved/revoked/dropped_budget
	Reason      string   `json:"reason,omitempty"`
}

// SourceRank 返回来源优先级（1 最高，7 最低）。未知来源按 untrusted 处理。
func SourceRank(source string) int {
	switch source {
	case SourceUserInstruction:
		return 1
	case SourceTaskSpec:
		return 2
	case SourceAgentsMD:
		return 3
	case SourceProjectMemory:
		return 4
	case SourceUserMemory:
		return 5
	case SourceSkill:
		return 6
	default:
		return 7
	}
}

// TrustRank 信任等级排序（数值越小越可信）。
func TrustRank(trust string) int {
	switch trust {
	case TrustAuthoritative:
		return 0
	case TrustVerified:
		return 1
	default:
		return 2
	}
}

// HashContextContent 计算规范化文本的 sha256（hex）。
func HashContextContent(s string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(s)))
	return hex.EncodeToString(sum[:])
}

// EntryCanonical 用于稳定 hash 的比较视图（不含展示/预算字段）。
type entryCanonical struct {
	EntryID        string `json:"entry_id"`
	ConflictKey    string `json:"conflict_key"`
	Source         string `json:"source"`
	SourceID       string `json:"source_id"`
	SourceRevision string `json:"source_revision"`
	Scope          string `json:"scope"`
	Trust          string `json:"trust"`
	ContentHash    string `json:"content_hash"`
	ArtifactRef    string `json:"artifact_ref"`
	Required       bool   `json:"required"`
}

func canonicalEntries(entries []ContextEntry) []entryCanonical {
	out := make([]entryCanonical, 0, len(entries))
	for _, e := range entries {
		out = append(out, entryCanonical{
			EntryID: e.EntryID, ConflictKey: e.ConflictKey, Source: e.Source,
			SourceID: e.SourceID, SourceRevision: e.SourceRevision, Scope: e.Scope,
			Trust: e.Trust, ContentHash: e.ContentHash, ArtifactRef: e.ArtifactRef,
			Required: e.Required,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		if out[i].SourceID != out[j].SourceID {
			return out[i].SourceID < out[j].SourceID
		}
		return out[i].ContentHash < out[j].ContentHash
	})
	return out
}

// ComputeContentHash 计算 Manifest 内容 hash：渲染顺序固定，同一输入重复构建 hash 相同。
func (m *TaskContextManifest) ComputeContentHash() string {
	if m == nil {
		return ""
	}
	payload := struct {
		Version     string           `json:"version"`
		RoleID      string           `json:"role_id"`
		ProjectKey  string           `json:"project_key"`
		Execution   string           `json:"execution_path"`
		Required    []entryCanonical `json:"required"`
		Optional    []entryCanonical `json:"optional"`
		Constraints []ConstraintDecision `json:"constraints"`
	}{
		Version:     m.Version,
		RoleID:      m.RoleID,
		ProjectKey:  m.ProjectKey,
		Execution:   m.ExecutionPath,
		Required:    canonicalEntries(m.Required),
		Optional:    canonicalEntries(m.Optional),
		Constraints: m.Constraints,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// NewManifestID 生成清单 ID（内容 hash 前 16 位 + 冻结时间，便于审计定位）。
func NewManifestID(contentHash string, frozenAt time.Time) string {
	h := contentHash
	if len(h) > 16 {
		h = h[:16]
	}
	return fmt.Sprintf("ctx-%s-%d", h, frozenAt.UnixNano())
}
