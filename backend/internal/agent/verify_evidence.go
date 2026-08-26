package agent

// verify_evidence.go 实现 TODO #43 L0 可执行校验（证据扫描）：
// 扫描子 Agent 历史中"验证类命令（测试/lint/--check/verify）执行成功"的客观证据，
// 零 LLM、零额外执行、零新 Agent——堵"没跑测试就声称完成"。
//
// tool 消息 Content 是 ToolResultJSON（react_types.go:351，含 success 字段）；
// 历史截断只切 Output 不破 JSON 信封（react_agent.go:552-554），解析可靠。

import (
	"encoding/json"
	"slices"
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

// ScreenshotToolNames 是视觉层证据认可的截图工具名（TODO #59 验收分层 visual 层）。
// ui_preview 插件的浏览器截图工具；可扩展（如 computer_use 的截图变体）。
var ScreenshotToolNames = []string{"browser_take_screenshot"}

// HasScreenshotEvidence 返回 history 中是否存在成功的截图工具调用证据（TODO #59 视觉层）：
// assistant 消息携带 Name ∈ ScreenshotToolNames 的调用，且对应 tool 结果 Success==true。
// UI/游戏类任务 spec verify_levels 含 visual 时，dispatcher 强制该证据——
// 缺截图回显即"未验证"（delivered-unverified 黄态），杜绝"文件生成了但运行时一张没用"
// 类验收盲区（实证 2026-08-24 塔防 23 张贴图全走手绘兜底）。
func HasScreenshotEvidence(history []ReactMessage) bool {
	shotIDs := make(map[string]bool)
	for _, m := range history {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if slices.Contains(ScreenshotToolNames, tc.Name) {
				shotIDs[tc.ID] = true
			}
		}
	}
	if len(shotIDs) == 0 {
		return false
	}
	for _, m := range history {
		if m.Role != "tool" || !shotIDs[m.ToolCallID] {
			continue
		}
		var r ToolResult
		if json.Unmarshal([]byte(m.Content), &r) == nil && r.Success {
			return true
		}
	}
	return false
}

// RuntimeProbeToolNames 是 runtime 层证据认可的浏览器探针工具名（TODO #67）。
// ui_preview MCP 插件：navigate（打开页面）/ evaluate（JS 断言）/ console_messages（错误回读）。
var RuntimeProbeToolNames = map[string]bool{
	"browser_navigate":        true,
	"browser_evaluate":        true,
	"browser_console_messages": true,
}

// HasRuntimeProbeEvidence 返回 history 中是否存在完整的运行时行为探针证据（TODO #67 runtime 层）：
// ① browser_navigate 成功（页面被真实打开）；
// ② browser_evaluate 成功（至少一次断言/交互脚本被执行）；
// ③ browser_console_messages 回读且无 error/severe 级条目（console 干净）。
// 三者齐备才算"运行行为被验证"——缺任一或 console 有 error 均判证据不足
// （实证 2026-08-25 水果忍者：贴图 key 不匹配缺陷活过三轮"已验证"交付，
// 全程零 browser_navigate/evaluate 探针，仅靠同图连拍菜单截图充数）。
func HasRuntimeProbeEvidence(history []ReactMessage) bool {
	hasNavigate, hasEvaluate, consoleRead := false, false, false
	consoleHasError := false
	for _, m := range history {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if !RuntimeProbeToolNames[tc.Name] {
				continue
			}
			switch tc.Name {
			case "browser_navigate":
				if toolResultSuccess(history, tc.ID) {
					hasNavigate = true
				}
			case "browser_evaluate":
				if toolResultSuccess(history, tc.ID) {
					hasEvaluate = true
				}
			case "browser_console_messages":
				out := toolResultOutput(history, tc.ID)
				if out == "" {
					continue
				}
				consoleRead = true
				if runtimeConsoleHasError(out) {
					consoleHasError = true
				}
			}
		}
	}
	return hasNavigate && hasEvaluate && consoleRead && !consoleHasError
}

// toolResultSuccess 查找 ToolCallID 对应的 tool 消息并解析 Success。
func toolResultSuccess(history []ReactMessage, callID string) bool {
	for _, m := range history {
		if m.Role != "tool" || m.ToolCallID != callID {
			continue
		}
		var r ToolResult
		if json.Unmarshal([]byte(m.Content), &r) == nil && r.Success {
			return true
		}
	}
	return false
}

// toolResultOutput 查找 ToolCallID 对应 tool 消息的 Output 文本（失败/缺失返回空串）。
func toolResultOutput(history []ReactMessage, callID string) string {
	for _, m := range history {
		if m.Role != "tool" || m.ToolCallID != callID {
			continue
		}
		var r ToolResult
		if json.Unmarshal([]byte(m.Content), &r) == nil {
			return r.Output
		}
	}
	return ""
}

// runtimeConsoleErrorPatterns 是 console 回读文本中判 error/severe 的模式（小写匹配）。
// browser_console_messages 输出格式为逐条日志行（level+文本），这里按保守口径扫常见 error 标记。
var runtimeConsoleErrorPatterns = []string{
	`"level": "error"`, `"level":"error"`, `"level": "severe"`, `"level":"severe"`,
	`level=error`, `[error]`, ` uncaught `, `uncaughtreferenceerror`, `referenceerror`,
}

// runtimeConsoleHasError 判断 console 回读文本是否含 error/severe 级条目。
func runtimeConsoleHasError(out string) bool {
	lc := strings.ToLower(out)
	for _, p := range runtimeConsoleErrorPatterns {
		if strings.Contains(lc, p) {
			return true
		}
	}
	// 去空白后的宽匹配：`"level" : "error"` 类空格变体。
	norm := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, lc)
	return strings.Contains(norm, `"level":"error"`) || strings.Contains(norm, `"level":"severe"`)
}

// SceneEvidenceReport 是场景化截图证据扫描结果（TODO #69）。
type SceneEvidenceReport struct {
	// DistinctShots 去重后的成功截图次数（按输出内容指纹）。
	DistinctShots int
	// TotalShots 成功截图总次数（含重复）。
	TotalShots int
	// HasAdjacent 每张截图前是否有 navigate/evaluate 邻接证据（任一截图命中即 true）。
	HasAdjacent bool
}

// HasSceneEvidence 场景化截图判定（TODO #69 visual 层）：
// ① 截图按输出内容指纹去重（防同图连拍充数——实证 2026-08-25 水果忍者 10 张截图
// 7 张字节相同菜单图，从未拍到游玩画面）；
// ② 去重后数量 ≥ wantScenes；
// ③ 存在 navigate/evaluate → screenshot 的时序邻接（截图对应真实页面状态而非空页面）。
// wantScenes <= 0 时退化为"去重后至少 1 张 + 有邻接"（替代单截图判定）。
func HasSceneEvidence(history []ReactMessage, wantScenes int) SceneEvidenceReport {
	// 按历史顺序收集事件序列：(kind, callID)。kind: shot/nav/eval。
	type ev struct {
		kind    string
		callID  string
		success bool
	}
	var seq []ev
	for _, m := range history {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			switch {
			case slices.Contains(ScreenshotToolNames, tc.Name):
				seq = append(seq, ev{kind: "shot", callID: tc.ID})
			case tc.Name == "browser_navigate":
				seq = append(seq, ev{kind: "nav", callID: tc.ID})
			case tc.Name == "browser_evaluate":
				seq = append(seq, ev{kind: "eval", callID: tc.ID})
			}
		}
	}
	if len(seq) == 0 {
		return SceneEvidenceReport{}
	}
	// 第一遍：标记成功事件 + 计录成功截图的输出指纹。
	success := make(map[string]bool, len(seq))
	fingerprints := make(map[string]bool)
	var shotOrder []string // 成功截图 callID 按时序
	totalShots := 0
	for _, e := range seq {
		ok := toolResultSuccess(history, e.callID)
		success[e.callID] = ok
		if e.kind == "shot" && ok {
			totalShots++
			fp := shotFingerprint(history, e.callID)
			if !fingerprints[fp] {
				fingerprints[fp] = true
			}
			shotOrder = append(shotOrder, e.callID)
		}
	}
	// 第二遍：时序邻接——成功截图前最近一次 nav/eval 事件（成功）即算邻接。
	hasAdjacent := false
	prevEvidence := ""
	for _, e := range seq {
		switch e.kind {
		case "nav", "eval":
			if success[e.callID] {
				prevEvidence = e.kind
			}
		case "shot":
			if success[e.callID] && prevEvidence != "" {
				hasAdjacent = true
			}
		}
	}
	return SceneEvidenceReport{
		DistinctShots: len(fingerprints),
		TotalShots:    totalShots,
		HasAdjacent:   hasAdjacent,
	}
}

// shotFingerprint 提取截图工具结果的输出指纹（内容哈希的代理：截图结果
// Output 文本 + Images 计数）。ui_preview 截图结果回传 base64 图片时 Output
// 常为保存路径/确认文本，同一画面重复截图的 Output 字节一致，可作为去重指纹。
func shotFingerprint(history []ReactMessage, callID string) string {
	for _, m := range history {
		if m.Role != "tool" || m.ToolCallID != callID {
			continue
		}
		var r ToolResult
		if json.Unmarshal([]byte(m.Content), &r) != nil {
			return m.Content
		}
		return r.Output + "|" + strconv.Itoa(len(r.Images))
	}
	return callID
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
