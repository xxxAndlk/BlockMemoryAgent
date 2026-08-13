package agent

// verify_evidence.go 实现 TODO #43 L0 可执行校验（证据扫描）：
// 扫描子 Agent 历史中"验证类命令（测试/lint/--check/verify）执行成功"的客观证据，
// 零 LLM、零额外执行、零新 Agent——堵"没跑测试就声称完成"。
//
// tool 消息 Content 是 ToolResultJSON（react_types.go:351，含 success 字段）；
// 历史截断只切 Output 不破 JSON 信封（react_agent.go:552-554），解析可靠。

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// HasExecutableVerification 返回 history 中是否存在"验证类命令执行成功"的证据：
// assistant 消息携带 Name=="RunCommand" 且 command 命中 tool.IsVerificationCommand 的
// 工具调用，且对应 tool 结果（ToolCallID 匹配）Success==true。
func HasExecutableVerification(history []ReactMessage) bool {
	verifyCallIDs := make(map[string]bool)
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
				verifyCallIDs[tc.ID] = true
			}
		}
	}
	if len(verifyCallIDs) == 0 {
		return false
	}
	for _, m := range history {
		if m.Role != "tool" || !verifyCallIDs[m.ToolCallID] {
			continue
		}
		var r ToolResult
		if json.Unmarshal([]byte(m.Content), &r) == nil && r.Success {
			return true
		}
	}
	return false
}

// RecentVerificationOutputs 返回最近 n 条验证类 RunCommand 的工具结果摘要
// （命令 + 成功标记 + 输出截断），供 judge prompt 的【验证证据】段引用。
// 按历史顺序返回（越新越靠后）；无验证类命令时返回 nil。
func RecentVerificationOutputs(history []ReactMessage, n int) []string {
	callCmds := make(map[string]string)
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
				callCmds[tc.ID] = cmd
			}
		}
	}
	var out []string
	for _, m := range history {
		if m.Role != "tool" {
			continue
		}
		cmd, ok := callCmds[m.ToolCallID]
		if !ok {
			continue
		}
		var r ToolResult
		if json.Unmarshal([]byte(m.Content), &r) != nil {
			continue
		}
		line := "命令: " + truncateRunes(cmd, 200) + "\n成功: " + strconv.FormatBool(r.Success)
		if outText := truncateRunes(strings.TrimSpace(r.Output), 400); outText != "" {
			line += "\n输出: " + outText
		}
		if r.Error != "" {
			line += "\n错误: " + truncateRunes(r.Error, 200)
		}
		out = append(out, line)
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}
