package agent

// react_types_test.go 验证 react_types.go 的纯函数 helper。

import (
	"encoding/base64"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/go-kratos/blades"
)

// TestAssistantMessageFromBlades_DropsBrokenToolCall 验证 ToolPart.Request 为
// 半截/损坏 JSON（max_tokens 截断后端点补齐引号括号的假合法参数）时，该工具
// 调用被丢弃而非以 nil 入参追加执行（nil 入参写文件会静默写坏内容）。
func TestAssistantMessageFromBlades_DropsBrokenToolCall(t *testing.T) {
	m := blades.NewAssistantMessage(blades.StatusCompleted)
	m.Parts = []blades.Part{
		blades.TextPart{Text: "先写文件"},
		blades.NewToolPart("c1", "WriteFile", `{"path":"a.js","content":"`),     // 半截 JSON
		blades.NewToolPart("c2", "WriteFile", `{"path":"b.js","content":"ok"}`), // 完整 JSON
	}
	msg := AssistantMessageFromBlades(m)
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1 (broken JSON dropped)", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].ID != "c2" {
		t.Errorf("kept tool call = %+v, want c2", msg.ToolCalls[0])
	}
	if msg.ToolCalls[0].Input == nil {
		t.Error("kept tool call Input must not be nil")
	}
	if msg.Content != "先写文件" {
		t.Errorf("content = %q", msg.Content)
	}
}

// TestAssistantMessageFromBlades_EmptyRequestKept 验证空对象 {} 是合法入参，不丢弃。
func TestAssistantMessageFromBlades_EmptyRequestKept(t *testing.T) {
	m := blades.NewAssistantMessage(blades.StatusCompleted)
	m.Parts = []blades.Part{
		blades.NewToolPart("c1", "noop", `{}`),
	}
	msg := AssistantMessageFromBlades(m)
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Input == nil || len(msg.ToolCalls[0].Input) != 0 {
		t.Fatalf("ToolCalls = %+v, want 1 with empty map", msg.ToolCalls)
	}
}

// TestFilesModifiedFromHistory 验证从 ReAct 历史扫 WriteFile/EditFile 调用收集修改文件路径。
func TestFilesModifiedFromHistory(t *testing.T) {
	history := []ReactMessage{
		{Role: "user", Content: "fix bug"},
		{Role: "assistant", Content: "I will edit two files", ToolCalls: []ToolCall{
			{ID: "t1", Name: "WriteFile", Input: map[string]any{"path": "src/a.go"}},
		}},
		{Role: "tool", ToolCallID: "t1", Content: "wrote 10 bytes"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "t2", Name: "ReadFile", Input: map[string]any{"path": "src/b.go"}}, // 非写工具，忽略
			{ID: "t3", Name: "EditFile", Input: map[string]any{"path": "doc/b.md"}}, // EditFile 同列修改文件
		}},
		{Role: "assistant", Content: "done", ToolCalls: []ToolCall{
			{ID: "t4", Name: "WriteFile", Input: map[string]any{"path": "src/a.go"}}, // 重复路径，去重
			{ID: "t5", Name: "WriteFile", Input: map[string]any{}},                   // 无 path 字段，跳过
			{ID: "t6", Name: "EditFile", Input: map[string]any{"path": "  "}},        // 空白 path，跳过
		}},
	}
	got := FilesModifiedFromHistory(history)
	want := []string{filepath.Clean("src/a.go"), filepath.Clean("doc/b.md")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FilesModifiedFromHistory = %v, want %v", got, want)
	}
}

// TestFilesModifiedFromHistory_Empty 验证空历史或无 WriteFile 返回 nil。
func TestFilesModifiedFromHistory_Empty(t *testing.T) {
	if got := FilesModifiedFromHistory(nil); got != nil {
		t.Errorf("FilesModifiedFromHistory(nil) = %v, want nil", got)
	}
	noWrite := []ReactMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "done"},
	}
	if got := FilesModifiedFromHistory(noWrite); got != nil {
		t.Errorf("FilesModifiedFromHistory(no WriteFile) = %v, want nil", got)
	}
}

// TestFilesModifiedFromHistory_PathCleaning 验证路径被 filepath.Clean 规范化（去 ./ 等）。
func TestFilesModifiedFromHistory_PathCleaning(t *testing.T) {
	history := []ReactMessage{
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "t1", Name: "WriteFile", Input: map[string]any{"path": "./src/../src/a.go"}},
		}},
	}
	got := FilesModifiedFromHistory(history)
	// filepath.Clean("./src/../src/a.go") = "src/a.go"（跨平台用 Clean 算期望）
	want := []string{filepath.Clean("src/a.go")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FilesModifiedFromHistory path cleaning = %v, want %v", got, want)
	}
}

// TestToBladesMessages_ToolImagesLatestBatchOnly 验证图片透传边界：
// 仅历史末尾最新一批 tool 结果挂载 DataPart（越过 mailbox 注入的 user 尾巴），
// 更早批次不挂（其 Content 文本占位符仍在），防 base64 反复进上下文烧毁前缀缓存；
// 非法 base64 / 空 MIME 静默跳过。
func TestToBladesMessages_ToolImagesLatestBatchOnly(t *testing.T) {
	pngB64 := base64.StdEncoding.EncodeToString([]byte{0x89, 0x50, 0x4E, 0x47})
	screenshot := func(id string) []tool.ResultImage {
		return []tool.ResultImage{{MIMEType: "image/png", Data: []byte(pngB64)}}
	}
	history := []ReactMessage{
		{Role: "user", Content: "u"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "browser_take_screenshot", Input: map[string]any{}}}},
		{Role: "tool", ToolCallID: "c1", Content: "[image image/png, 4 bytes base64]", Images: screenshot("c1")},
		{Role: "assistant", Content: "看到了旧图"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c2", Name: "browser_take_screenshot", Input: map[string]any{}}}},
		{Role: "tool", ToolCallID: "c2", Content: "[image image/png, 4 bytes base64]", Images: []tool.ResultImage{
			{MIMEType: "image/png", Data: []byte(pngB64)},
			{MIMEType: "image/png", Data: []byte("!!!not-base64!!!")}, // 非法：跳过
			{MIMEType: "", Data: []byte(pngB64)},                      // 空 MIME：跳过
		}},
		{Role: "user", Content: "[mailbox] 子任务完成"}, // mailbox 尾巴：不影响最新批次定位
	}
	msgs := ToBladesMessages(history)
	if len(msgs) != len(history) {
		t.Fatalf("消息数不符: got %d want %d", len(msgs), len(history))
	}
	// 旧批次（索引 2）：只有 ToolPart，无 DataPart。
	for _, p := range msgs[2].Parts {
		if _, ok := p.(blades.DataPart); ok {
			t.Fatalf("旧批次不应挂载图片: %+v", p)
		}
	}
	// 最新批次（索引 5）：ToolPart + 恰好 1 个合法 DataPart。
	var dataParts []blades.DataPart
	for _, p := range msgs[5].Parts {
		if dp, ok := p.(blades.DataPart); ok {
			dataParts = append(dataParts, dp)
		}
	}
	if len(dataParts) != 1 {
		t.Fatalf("最新批次应挂载 1 张合法图片, got %d", len(dataParts))
	}
	if dataParts[0].MIMEType != blades.MIMEImagePNG || len(dataParts[0].Bytes) != 4 {
		t.Fatalf("DataPart 内容错误: %+v", dataParts[0])
	}
}
