package subagent

// agent_result.go 统一 Agent 结果信封（B-09）：终态通知除人读 summary 外，
// 额外携带机器可消费的结构化结果（Mailbox Payload JSONB，零表结构变更）。
// 状态口径与任务台账/验收分层一致：completed / failed / unverified。

import (
	"fmt"
	"strings"
)

// AgentResultVersion 结果信封版本（字段语义变化时递增）。
const AgentResultVersion = "v1"

// AgentResult 子 Agent 终态结果信封。
type AgentResult struct {
	Version       string   `json:"version"`
	ResultID      string   `json:"result_id"`
	AgentID       string   `json:"agent_id"`
	ParentID      string   `json:"parent_id"`
	Status        string   `json:"status"` // completed | failed | unverified
	Summary       string   `json:"summary"`
	FilesModified []string `json:"files_modified,omitempty"`
	Verification  string   `json:"verification"` // passed | unverified | unknown
}

// buildAgentResult 从终态摘要与文件清单构造结果信封。
// 失败机读标记 [failure kind=...] 优先；其次识别未验证文案；否则视为完成。
func (d *Dispatcher) buildAgentResult(parentID, subAgentID, summary string, files []string) *AgentResult {
	status := "completed"
	verification := "passed"
	if failureMarkerRe.MatchString(summary) {
		status = "failed"
		verification = "unknown"
	} else if strings.Contains(summary, "未验证") || strings.Contains(summary, "delivered-unverified") {
		status = "unverified"
		verification = "unverified"
	}
	resultID := fmt.Sprintf("res-%s-%08x", subAgentID, fnvHash32(summary))
	return &AgentResult{
		Version:       AgentResultVersion,
		ResultID:      resultID,
		AgentID:       subAgentID,
		ParentID:      parentID,
		Status:        status,
		Summary:       summary,
		FilesModified: files,
		Verification:  verification,
	}
}
