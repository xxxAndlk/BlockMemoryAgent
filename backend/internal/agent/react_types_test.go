package agent

// react_types_test.go 验证 react_types.go 的纯函数 helper。

import (
	"path/filepath"
	"reflect"
	"testing"
)

// TestFilesModifiedFromHistory 验证从 ReAct 历史扫 WriteFile 调用收集修改文件路径。
func TestFilesModifiedFromHistory(t *testing.T) {
	history := []ReactMessage{
		{Role: "user", Content: "fix bug"},
		{Role: "assistant", Content: "I will edit two files", ToolCalls: []ToolCall{
			{ID: "t1", Name: "WriteFile", Input: map[string]any{"path": "src/a.go"}},
		}},
		{Role: "tool", ToolCallID: "t1", Content: "wrote 10 bytes"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "t2", Name: "ReadFile", Input: map[string]any{"path": "src/b.go"}}, // 非 WriteFile，忽略
			{ID: "t3", Name: "WriteFile", Input: map[string]any{"path": "doc/b.md"}},
		}},
		{Role: "assistant", Content: "done", ToolCalls: []ToolCall{
			{ID: "t4", Name: "WriteFile", Input: map[string]any{"path": "src/a.go"}}, // 重复路径，去重
			{ID: "t5", Name: "WriteFile", Input: map[string]any{}},                  // 无 path 字段，跳过
			{ID: "t6", Name: "WriteFile", Input: map[string]any{"path": "  "}},      // 空白 path，跳过
		}},
	}
	got := FilesModifiedFromHistory(history)
	want := []string{filepath.Clean("src/a.go"), filepath.Clean("doc/b.md")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FilesModifiedFromHistory = %v, want %v", got, want)
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
