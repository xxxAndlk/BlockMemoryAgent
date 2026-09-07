// context_slim_test.go 覆盖 TODO 第9项②③ + 第10项⑦ 的 agent 侧单测：
//   - evictStaleToolResults 陈旧只读工具结果驱逐（请求期视图变换）；
//   - condenseToolResult 工具结果统一收口（超限落盘 + 头部摘录 + 全文路径）；
//   - buildEnvBlock 项目自述（AGENTS.md/CLAUDE.md）注入。
package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
)

// TestEvictStaleToolResults_EvictsOldReads 驱逐 N 轮前的 ReadFile，保留 RunCommand
//（验证证据）与近轮 ReadFile；占位符为合法 ToolResultJSON 且含"内容已驱逐"标记。
func TestEvictStaleToolResults_EvictsOldReads(t *testing.T) {
	oldRead := "line1\nline2\nline3"
	cmdOut := `$ go test ./...`
	freshRead := "fresh content"
	msgs := []ReactMessage{
		{Role: "user", Content: "task"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "a1", Name: "ReadFile"},
			{ID: "a2", Name: "RunCommand"},
		}},
		{Role: "tool", ToolCallID: "a1", Content: ToolResultJSON(ToolResult{Tool: "ReadFile", Success: true, Output: oldRead})},
		{Role: "tool", ToolCallID: "a2", Content: ToolResultJSON(ToolResult{Tool: "RunCommand", Success: true, Output: cmdOut})},
		{Role: "assistant", Content: "thinking"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "a3", Name: "ReadFile"}}},
		{Role: "tool", ToolCallID: "a3", Content: ToolResultJSON(ToolResult{Tool: "ReadFile", Success: true, Output: freshRead})},
		{Role: "assistant", Content: "almost done"},
	}
	out := evictStaleToolResults(msgs, 2)
	// canonical history 不被改动。
	if msgs[2].Content != ToolResultJSON(ToolResult{Tool: "ReadFile", Success: true, Output: oldRead}) {
		t.Fatalf("canonical history must be untouched, got: %s", msgs[2].Content)
	}
	// a1：4 轮前（cur=3, ord=0）的 ReadFile → 驱逐。
	if !strings.Contains(out[2].Content, "内容已驱逐") || !strings.Contains(out[2].Content, "重新执行 ReadFile") {
		t.Fatalf("old ReadFile should be evicted with re-read hint, got: %s", out[2].Content)
	}
	var r ToolResult
	if err := json.Unmarshal([]byte(out[2].Content), &r); err != nil {
		t.Fatalf("evicted placeholder must be valid ToolResultJSON: %v", err)
	}
	if r.Tool != "ReadFile" || !r.Success {
		t.Fatalf("placeholder must keep tool name and success, got: %+v", r)
	}
	if out[2].ToolCallID != "a1" {
		t.Fatalf("placeholder must keep ToolCallID, got: %q", out[2].ToolCallID)
	}
	// a2：RunCommand（验证证据）不驱逐。
	if out[3].Content != msgs[3].Content {
		t.Fatalf("RunCommand result must not be evicted, got: %s", out[3].Content)
	}
	// a3：1 轮前，不驱逐。
	if out[6].Content != msgs[6].Content {
		t.Fatalf("recent ReadFile must not be evicted, got: %s", out[6].Content)
	}
	// 幂等：再次驱逐不重写占位符。
	second := evictStaleToolResults(out, 2)
	if second[2].Content != out[2].Content {
		t.Fatalf("placeholder must stay stable across rounds, got: %s", second[2].Content)
	}
}

// TestEvictStaleToolResults_DisabledOrEmpty keepRounds<=0 与无 assistant 时零行为。
func TestEvictStaleToolResults_DisabledOrEmpty(t *testing.T) {
	msgs := []ReactMessage{
		{Role: "user", Content: "task"},
		{Role: "tool", ToolCallID: "a1", Content: `{"tool":"ReadFile"}`},
	}
	if out := evictStaleToolResults(msgs, 0); len(out) != 2 || &out[0] == &msgs[0] && false {
		t.Fatalf("disabled eviction must return input as-is")
	}
	// 无 assistant 消息（cur<0）零行为。
	if out := evictStaleToolResults(msgs, 5); out[1].Content != msgs[1].Content {
		t.Fatalf("no-assistant history must not be evicted, got: %s", out[1].Content)
	}
}

// TestReActAgent_ToolResultDump 收口端到端：ReadFile 输出超阈值时全文落盘
// <workDir>/.bma/tool_outputs/，历史 tool 消息只留头部摘录 + 【全文已落盘】路径。
func TestReActAgent_ToolResultDump(t *testing.T) {
	dir := t.TempDir()
	reg := tool.NewBuiltinRegistry(dir, nil, nil)
	big := strings.Repeat("hello world ", 40) // 480 runes
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatalf("write big.txt: %v", err)
	}
	llm := &mockModelProvider{
		responses: []*blades.Message{
			{
				Role: blades.RoleAssistant,
				Parts: []blades.Part{
					blades.ToolPart{Name: "ReadFile", Request: string(mustJSON(map[string]any{"path": "big.txt"}))},
				},
			},
			blades.AssistantMessage("done"),
		},
	}
	ag := NewReActAgent("session-1", types.RoleDefinition{SystemPrompt: "t"}, llm, NewToolRegistryAdapter(reg)).
		WithWorkDir(dir).
		WithLoopConfig(LoopConfig{ToolResultDumpRunes: 100, ToolResultDigestRunes: 30, ToolOutputMaxRunes: 5000})
	ctx := tool.WithWorkDir(context.Background(), dir)
	res, err := ag.Run(ctx, "read big file")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// 历史：user, assistant(tool), tool, assistant。
	var toolMsg *ReactMessage
	for i := range res.History {
		if res.History[i].Role == "tool" {
			toolMsg = &res.History[i]
		}
	}
	if toolMsg == nil {
		t.Fatalf("no tool message in history: %d msgs", len(res.History))
	}
	if !strings.Contains(toolMsg.Content, "【全文已落盘】") {
		t.Fatalf("tool message should contain dump path marker, got: %s", toolMsg.Content)
	}
	// 解析占位里的路径，校验全文文件存在且内容完整。
	var r ToolResult
	if err := json.Unmarshal([]byte(toolMsg.Content), &r); err != nil {
		t.Fatalf("tool message content must be valid ToolResultJSON: %v", err)
	}
	idx := strings.Index(r.Output, "【全文已落盘】")
	path := strings.TrimSpace(strings.SplitN(r.Output[idx:], "（", 2)[0][len("【全文已落盘】"):])
	if path == "" {
		t.Fatalf("dump path missing in output: %s", r.Output)
	}
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("dumped file unreadable: %v", err)
	}
	if !strings.Contains(string(full), big) {
		t.Fatalf("dumped file must contain full output, got %d bytes", len(full))
	}
	// 摘录段（【全文已落盘】之前）应≈digest 阈值：30 runes + truncateRunes 的省略尾注。
	digestPart := strings.SplitN(r.Output, "【全文已落盘】", 2)[0]
	if n := len([]rune(strings.TrimSpace(digestPart))); n > 50 {
		t.Fatalf("digest excerpt should stay near the 30-rune cap, got %d", n)
	}
	if !strings.Contains(digestPart, "(truncated)") {
		t.Fatalf("digest excerpt should carry truncation marker, got %q", digestPart)
	}
}

// TestBuildEnvBlock_InjectsProjectBrief 验证 AGENTS.md 优先、CLAUDE.md 兜底、关闭时零注入。
func TestBuildEnvBlock_InjectsProjectBrief(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# Agent 规范\n写代码前先跑 lint。"), 0o644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	env := buildEnvBlock(dir, 4000)
	if !strings.Contains(env, "【项目自述】") || !strings.Contains(env, "写代码前先跑 lint") {
		t.Fatalf("AGENTS.md should be injected, got: %s", env)
	}
	// 关闭（<=0）零注入。
	if env := buildEnvBlock(dir, 0); strings.Contains(env, "【项目自述】") {
		t.Fatalf("disabled brief must not be injected, got: %s", env)
	}
	// 仅 CLAUDE.md 时兜底。
	dir2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir2, "CLAUDE.md"), []byte("claude notes"), 0o644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}
	if env := buildEnvBlock(dir2, 4000); !strings.Contains(env, "claude notes") {
		t.Fatalf("CLAUDE.md should be injected as fallback, got: %s", env)
	}
	// 两者并存时 AGENTS.md 优先。
	if err := os.WriteFile(filepath.Join(dir2, "AGENTS.md"), []byte("agents wins"), 0o644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	if env := buildEnvBlock(dir2, 4000); !strings.Contains(env, "agents wins") || strings.Contains(env, "claude notes") {
		t.Fatalf("AGENTS.md must take precedence, got: %s", env)
	}
}
