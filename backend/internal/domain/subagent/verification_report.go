package subagent

// verification_report.go 逐项机器验收报告（B-03 首批）：
// 复用现有证据采集（verify_levels.go / agent.verify_evidence.go），把“哪条验收对应
// 哪份证据、缺什么、最终裁决”结构化输出，随 AgentResult v2 进 Mailbox Payload。
//
// 本报告只读本 attempt 的 History：证据来自成功工具调用与真实写入文件，
// 不接受 Agent 自述。legacy 无标记验收行标 legacy_unproven，不得当作机器通过。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/team"
)

// VerificationReportVersion 报告版本（与 AgentResult 版本独立）。
const VerificationReportVersion = "v1"

// 报告/条目状态。
const (
	VerifyStatusPassed       = "passed"
	VerifyStatusFailed       = "failed"
	VerifyStatusUnverified   = "unverified"
	VerifyItemMissing        = "missing"
	VerifyItemManualPending  = "manual_pending"
	VerifyItemManualApproved = "manual_approved"
	VerifyItemLegacyUnproven = "legacy_unproven"
)

// VerificationReport 一次 attempt 的逐项机器验收报告。
type VerificationReport struct {
	Version        string             `json:"version"`
	ReportID       string             `json:"report_id"`
	TaskID         string             `json:"task_id,omitempty"`
	AttemptID      string             `json:"attempt_id,omitempty"`
	SpecVersion    string             `json:"spec_version,omitempty"`
	Risk           string             `json:"risk,omitempty"`
	Status         string             `json:"status"` // passed/failed/unverified
	Items          []VerificationItem `json:"items,omitempty"`
	RequiredLevels []LevelResult      `json:"required_levels,omitempty"`
	Unresolved     []string           `json:"unresolved,omitempty"`
	PolicyHash     string             `json:"policy_hash,omitempty"`
}

// VerificationItem 单条验收的机器裁决。
type VerificationItem struct {
	AcceptanceID string        `json:"acceptance_id"`
	Text         string        `json:"text"`
	TextHash     string        `json:"text_hash"`
	EvidenceKind string        `json:"evidence_kind"`
	Layer        string        `json:"layer,omitempty"`
	Status       string        `json:"status"`
	EvidenceRefs []EvidenceRef `json:"evidence_refs,omitempty"`
	Reason       string        `json:"reason,omitempty"`
}

// EvidenceRef 证据引用（跨 attempt 校验用 attempt_id；本版恒为当前 attempt）。
type EvidenceRef struct {
	EvidenceID  string `json:"evidence_id"`
	Kind        string `json:"kind"`
	ToolCallID  string `json:"tool_call_id,omitempty"`
	Command     string `json:"command,omitempty"`
	ExitCode    *int   `json:"exit_code,omitempty"`
	ArtifactRef string `json:"artifact_ref,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
}

// LevelResult 验收层级结论。
type LevelResult struct {
	Level  string `json:"level"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// WithVerificationPolicyMode 设置验收强制模式（off|shadow|enforce）。
// 仅对带 task_id 绑定的新路径 enforce；legacy 派发始终只记录。
func (d *Dispatcher) WithVerificationPolicyMode(mode string) *Dispatcher {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "off":
		d.verificationPolicyMode = "off"
	case "enforce":
		d.verificationPolicyMode = "enforce"
	default:
		d.verificationPolicyMode = "shadow"
	}
	return d
}

// verificationFor 统一构造报告（含 C-03 task/attempt/risk 标识）。
func (d *Dispatcher) verificationFor(ctx context.Context, parentID, domain, agentID string, result agent.ReactResult, runErr error) *VerificationReport {
	var rec *parentSpecRecord
	var acceptance []string
	if rec = d.parentSpecRecordOf(parentID, domain); rec != nil {
		acceptance = rec.acceptance
	}
	taskID, attemptID, risk := "", "", ""
	if bnd, ok := taskBindingFromContext(ctx); ok {
		taskID, attemptID, risk = bnd.TaskID, bnd.AttemptID, bnd.Risk
		// 冻结计划中的验收清单是该 attempt 的权威输入，优先于 parentSpec 缓存。
		if len(bnd.Acceptance) > 0 {
			acceptance = bnd.Acceptance
		}
	}
	return buildVerificationReport(agentID, taskID, attemptID, risk, result.History, acceptance, rec, runErr)
}

// refreshVerificationReport 按当前出口的最终 History 强制重建报告。
// 暂停、恢复、强制收口都会复用同一 Agent ID，不能复用上一阶段缓存的旧裁决。
func (d *Dispatcher) refreshVerificationReport(ctx context.Context, parentID, domain, agentID string, result agent.ReactResult, runErr error) *VerificationReport {
	rep := d.verificationFor(ctx, parentID, domain, agentID, result, runErr)
	d.verifyReports.Store(agentID, rep)
	return rep
}

// ensureVerificationReport 在首次完成门禁前按需生成报告，避免“门禁先跑、报告后生成”的时序倒置。
func (d *Dispatcher) ensureVerificationReport(ctx context.Context, parentID, domain, agentID string, result agent.ReactResult) *VerificationReport {
	if rep := d.peekVerificationReport(agentID); rep != nil {
		return rep
	}
	return d.refreshVerificationReport(ctx, parentID, domain, agentID, result, nil)
}

// enforceVerificationGate 在绑定任务完成前检查报告；不通过返回回告文案。
func (d *Dispatcher) enforceVerificationGate(ctx context.Context, agentID string) (string, bool) {
	if d.verificationPolicyMode != "enforce" {
		return "", false
	}
	if _, ok := taskBindingFromContext(ctx); !ok {
		return "", false // legacy 只记录，不阻断
	}
	rep := d.peekVerificationReport(agentID)
	if rep == nil {
		// 绑定任务 + enforce：缺报告不得默认放行（时序倒置曾导致漏拦）。
		return "【机器验收缺失】绑定任务未生成验收报告，不得标记 done", true
	}
	if rep.Status == VerifyStatusPassed {
		return "", false
	}
	msg := fmt.Sprintf("【机器验收未通过】报告 %s 状态=%s，未完成项：%s", rep.ReportID, rep.Status, strings.Join(rep.Unresolved, "；"))
	return msg, true
}

// buildVerificationReport 从本 attempt 历史与验收清单构建报告。
// terminalErr 为 runSubAgentOnce 的出口错误（nil=正常完成），用于区分 failed/unverified。
func buildVerificationReport(agentID, taskID, attemptID, risk string, history []agent.ReactMessage, acceptance []string, rec *parentSpecRecord, terminalErr error) *VerificationReport {
	report := &VerificationReport{
		Version:  VerificationReportVersion,
		ReportID: fmt.Sprintf("vr-%s-%d", agentID, len(history)),
		TaskID:   taskID, AttemptID: attemptID, Risk: risk,
		Status: VerifyStatusPassed,
	}
	var scenes []string
	var levels, probes []string
	if rec != nil {
		scenes, levels, probes = rec.scenes, rec.verifyLevels, rec.probes
	}
	_ = probes

	for i, line := range acceptance {
		text, evidence, layer := tool.ParseAcceptanceLine(line)
		item := VerificationItem{
			AcceptanceID: fmt.Sprintf("A%d", i+1),
			Text:         text,
			TextHash:     team.HashContextContent(text),
			EvidenceKind: evidence,
			Layer:        layer,
		}
		switch evidence {
		case "command":
			item.EvidenceRefs = collectCommandEvidence(history)
			item.Status = boolStatus(len(item.EvidenceRefs) > 0)
		case "screenshot":
			ok, reason := visualEvidenceCheck(history, scenes)
			item.EvidenceRefs = collectToolEvidence(history, agent.ScreenshotToolNames)
			item.Status = boolStatus(ok)
			if !ok {
				item.Reason = reason
			}
		case "probe":
			ok := agent.HasRuntimeProbeEvidence(history)
			item.EvidenceRefs = collectRuntimeProbeEvidence(history)
			item.Status = boolStatus(ok)
		case "file":
			item.EvidenceRefs = collectFileEvidence(history)
			item.Status = boolStatus(len(item.EvidenceRefs) > 0)
		case "":
			item.Status = VerifyItemLegacyUnproven
			item.Reason = "legacy acceptance line lacks evidence tag; not machine-verifiable"
		case "manual":
			item.Status = VerifyItemManualPending
			item.Reason = "manual acceptance requires reviewer; not machine-verified"
		default:
			item.Status = VerifyItemMissing
			item.Reason = "unknown evidence kind"
		}
		if item.Status != VerifyStatusPassed {
			report.Unresolved = append(report.Unresolved, fmt.Sprintf("%s [%s] %s", item.AcceptanceID, item.EvidenceKind, item.Reason))
		}
		report.Items = append(report.Items, item)
	}
	if len(acceptance) == 0 {
		report.Unresolved = append(report.Unresolved, "no structured acceptance criteria declared")
		report.Status = VerifyStatusUnverified
	}
	if report.Status == VerifyStatusPassed {
		for _, item := range report.Items {
			if item.Status != VerifyStatusPassed {
				report.Status = VerifyStatusUnverified
				break
			}
		}
	}

	for _, lv := range levels {
		res := LevelResult{Level: lv}
		switch lv {
		case "runtime":
			res.Status = boolStatus(agent.HasRuntimeProbeEvidence(history))
		case "visual":
			ok, reason := visualEvidenceCheck(history, scenes)
			res.Status = boolStatus(ok)
			if !ok {
				res.Reason = reason
			}
		case "existence":
			res.Status = boolStatus(len(agent.FilesModifiedFromHistory(history)) > 0)
		default:
			res.Status = boolStatus(agent.HasExecutableVerification(history))
		}
		report.RequiredLevels = append(report.RequiredLevels, res)
		if res.Status != VerifyStatusPassed {
			report.Unresolved = append(report.Unresolved, "level "+lv+": "+res.Reason)
			report.Status = VerifyStatusUnverified
		}
	}

	report.Status = applyTerminalStatus(report.Status, terminalErr)
	report.PolicyHash = team.HashContextContent(report.riskPolicyString())
	return report
}

// applyTerminalStatus 用出口错误校正报告状态（执行失败优先于证据缺口）。
func applyTerminalStatus(status string, err error) string {
	if err == nil {
		return status
	}
	switch {
	case errors.Is(err, errUnverified), errors.Is(err, errVerifyMissing),
		errors.Is(err, errVisualEvidenceMissing), errors.Is(err, errPaused),
		errors.Is(err, errPartialReturn):
		return VerifyStatusUnverified
	default:
		return VerifyStatusFailed
	}
}

func (r *VerificationReport) riskPolicyString() string {
	if r == nil {
		return ""
	}
	return r.Version + "|" + r.Risk + "|" + r.Status
}

func boolStatus(ok bool) string {
	if ok {
		return VerifyStatusPassed
	}
	return VerifyItemMissing
}

// collectCommandEvidence 收集成功执行的验证类命令证据。
func collectCommandEvidence(history []agent.ReactMessage) []EvidenceRef {
	calls := map[string]string{}
	for _, m := range history {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Name != "RunCommand" {
				continue
			}
			cmd, _ := tc.Input["command"].(string)
			if cmd != "" && tool.IsVerificationCommand(cmd) {
				calls[tc.ID] = cmd
			}
		}
	}
	var out []EvidenceRef
	for _, m := range history {
		if m.Role != "tool" {
			continue
		}
		cmd, ok := calls[m.ToolCallID]
		if !ok || !toolResultSuccess(m.Content) {
			continue
		}
		out = append(out, EvidenceRef{
			EvidenceID: "cmd-" + m.ToolCallID, Kind: "command", ToolCallID: m.ToolCallID,
			Command: cmd, ContentHash: team.HashContextContent(m.Content),
		})
	}
	return out
}

// collectToolEvidence 收集指定工具名的成功调用证据（截图等）。
func collectToolEvidence(history []agent.ReactMessage, names []string) []EvidenceRef {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	var out []EvidenceRef
	for _, m := range history {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if !want[tool.BareToolName(tc.Name)] || !toolResultSuccessByID(history, tc.ID) {
				continue
			}
			out = append(out, EvidenceRef{
				EvidenceID: "tool-" + tc.ID, Kind: "screenshot", ToolCallID: tc.ID,
			})
		}
	}
	return out
}

// collectRuntimeProbeEvidence 收集 navigate/evaluate/console 成功证据。
func collectRuntimeProbeEvidence(history []agent.ReactMessage) []EvidenceRef {
	var out []EvidenceRef
	for _, m := range history {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			bare := tool.BareToolName(tc.Name)
			if !agent.RuntimeProbeToolNames[bare] || !toolResultSuccessByID(history, tc.ID) {
				continue
			}
			out = append(out, EvidenceRef{EvidenceID: "probe-" + tc.ID, Kind: bare, ToolCallID: tc.ID})
		}
	}
	return out
}

// collectFileEvidence 收集实际写入文件证据。
func collectFileEvidence(history []agent.ReactMessage) []EvidenceRef {
	var out []EvidenceRef
	for _, p := range agent.FilesModifiedFromHistory(history) {
		out = append(out, EvidenceRef{
			EvidenceID: "file-" + p, Kind: "file", ArtifactRef: p, ContentHash: team.HashContextContent(p),
		})
	}
	return out
}

func toolResultSuccessByID(history []agent.ReactMessage, callID string) bool {
	for _, m := range history {
		if m.Role == "tool" && m.ToolCallID == callID && toolResultSuccess(m.Content) {
			return true
		}
	}
	return false
}

func toolResultSuccess(content string) bool {
	var r agent.ToolResult
	return json.Unmarshal([]byte(content), &r) == nil && r.Success
}
