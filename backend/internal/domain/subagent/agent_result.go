package subagent

// agent_result.go 统一 Agent 结果信封（B-09 / B-03 v2）：
// 终态通知除人读 summary 外，额外携带机器可消费的结构化结果
// （Mailbox Payload JSONB，零表结构变更）。
//
// v2 保留 v1 全部字段（旧读取方忽略新字段仍可工作），新增 task/attempt/spec 标识、
// 逐项 VerificationReport、failure_kind/retryable。版本字段区分 v1/v2；v1 结果读取方
// 必须标 legacy_unproven，不能把默认 passed 当成机器验收通过。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/store"
)

// AgentResultVersion 结果信封版本（字段语义变化时递增）。
const AgentResultVersion = "v2"

// AgentResult 子 Agent 终态结果信封。
type AgentResult struct {
	// v1 字段（保持不变，供旧消费者）。
	Version       string   `json:"version"`
	ResultID      string   `json:"result_id"`
	AgentID       string   `json:"agent_id"`
	ParentID      string   `json:"parent_id"`
	Status        string   `json:"status"` // completed | failed | unverified
	Summary       string   `json:"summary"`
	FilesModified []string `json:"files_modified,omitempty"`
	Verification  string   `json:"verification"` // passed | unverified | unknown

	// v2 字段（B-03）：无 task/attempt 的 legacy 派发允许为空。
	TaskID              string              `json:"task_id,omitempty"`
	AttemptID           string              `json:"attempt_id,omitempty"`
	PlanVersion         int64               `json:"plan_version,omitempty"`
	SpecVersion         string              `json:"spec_version,omitempty"`
	ContextManifestID   string              `json:"context_manifest_id,omitempty"`
	ContextManifestHash string              `json:"context_manifest_hash,omitempty"`
	VerificationReport  *VerificationReport `json:"verification_report,omitempty"`
	Unresolved          []string            `json:"unresolved,omitempty"`
	FailureKind         string              `json:"failure_kind,omitempty"`
	Retryable           bool                `json:"retryable,omitempty"`
	TerminalEventID     string              `json:"terminal_event_id,omitempty"`
	CreatedAt           time.Time           `json:"created_at"`
}

// buildAgentResult 兼容旧调用：构造不带验收报告的 v2 信封。
func (d *Dispatcher) buildAgentResult(parentID, subAgentID, summary string, files []string) *AgentResult {
	return d.buildAgentResultWithReport(parentID, subAgentID, summary, files, nil)
}

// buildAgentResultWithReport 从终态摘要、文件清单与逐项验收报告构造 v2 信封。
// 状态由显式终态标记/未验证文案决定；Verification 只在报告 passed 时为 passed。
func (d *Dispatcher) buildAgentResultWithReport(parentID, subAgentID, summary string, files []string, report *VerificationReport) *AgentResult {
	status := "completed"
	// 无报告时不得默认 passed：v1 legacy 结果按 legacy_unproven 处理。
	verification := "unverified"
	if failureMarkerRe.MatchString(summary) {
		status = "failed"
		verification = "unknown"
	} else if strings.Contains(summary, "未验证") || strings.Contains(summary, "delivered-unverified") {
		status = "unverified"
		verification = "unverified"
	}
	failureKind := ""
	if m := failureMarkerRe.FindStringSubmatch(summary); m != nil {
		failureKind = m[1]
	}
	retryable := failureKind != "" && strings.Contains(summary, "retryable=true")

	var unresolved []string
	if report != nil {
		verification = VerifyStatusUnverified
		if report.Status == VerifyStatusPassed {
			verification = VerifyStatusPassed
		}
		unresolved = report.Unresolved
	}
	resultID := fmt.Sprintf("res-%s-%08x", subAgentID, fnvHash32(summary))
	return &AgentResult{
		Version:            AgentResultVersion,
		ResultID:           resultID,
		AgentID:            subAgentID,
		ParentID:           parentID,
		Status:             status,
		Summary:            summary,
		FilesModified:      files,
		Verification:       verification,
		VerificationReport: report,
		Unresolved:         unresolved,
		FailureKind:        failureKind,
		Retryable:          retryable,
		CreatedAt:          time.Now(),
	}
}

// TeamCandidateInput C-05 团队经验候选输入（只含证据引用，不含密钥）。
type TeamCandidateInput struct {
	ProjectKey                 string
	RoleID                     string
	TaskType                   string
	Content                    string
	SourceSession              string
	SourceTask                 string
	SourceAttempt              string
	SourceResultID             string
	SourceVerificationReportID string
	SpecVersion                string
	ContextManifestHash        string
	SourceStatus               string
	VerificationStatus         string
}

// WithTeamCandidateSink 注入团队经验候选出口（C-05）。
func (d *Dispatcher) WithTeamCandidateSink(fn func(ctx context.Context, c TeamCandidateInput)) *Dispatcher {
	d.candidateSink = fn
	return d
}

// emitTeamCandidate 在绑定任务成功且报告 passed 时提交候选（默认 shadow）。
func (d *Dispatcher) emitTeamCandidate(ctx context.Context, subAgentID, roleID, domain, task string, result agent.ReactResult, report *VerificationReport) {
	if d.candidateSink == nil || report == nil || report.Status != VerifyStatusPassed {
		return
	}
	bnd, ok := taskBindingFromContext(ctx)
	if !ok {
		return // legacy 派发不自动产出团队经验
	}
	pk := store.NormalizeProjectKey(tool.WorkDirFromContext(ctx))
	if pk == "" {
		return
	}
	taskType := strings.TrimSpace(domain)
	if taskType == "" {
		taskType = roleID
	}
	d.candidateSink(ctx, TeamCandidateInput{
		ProjectKey: pk, RoleID: roleID, TaskType: taskType,
		Content:       truncateRunes(result.Text, 1200),
		SourceSession: tool.SessionIDFromContext(ctx), SourceTask: task,
		SourceAttempt: bnd.AttemptID, SpecVersion: bnd.SpecVersion,
		SourceResultID:             fmt.Sprintf("res-%s-%08x", subAgentID, fnvHash32(result.Text)),
		SourceVerificationReportID: report.ReportID,
		SourceStatus:               "completed", VerificationStatus: report.Status,
	})
}

// peekVerificationReport 只读最近一次验收报告（enforce 门使用，不消费）。
func (d *Dispatcher) peekVerificationReport(agentID string) *VerificationReport {
	v, ok := d.verifyReports.Load(agentID)
	if !ok {
		return nil
	}
	r, _ := v.(*VerificationReport)
	return r
}

// takeVerificationReport 取出并移除某实例最近一次验收报告（notify 一次性消费）。
func (d *Dispatcher) takeVerificationReport(agentID string) *VerificationReport {
	v, ok := d.verifyReports.LoadAndDelete(agentID)
	if !ok {
		return nil
	}
	r, _ := v.(*VerificationReport)
	return r
}
