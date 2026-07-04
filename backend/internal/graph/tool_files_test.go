package graph

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteFilePermanentWritesToWorkDir 验证：默认情况下 WriteFile 把文件写到 workDir。
func TestWriteFilePermanentWritesToWorkDir(t *testing.T) {
	dir := t.TempDir()
	exec := NewToolExecutor(dir)

	ctx := WithSessionID(context.Background(), "session-1")
	result := exec.Execute(ctx, "WriteFile", map[string]any{
		"path":    "hello.txt",
		"content": "world",
	})
	if !result.Success {
		t.Fatalf("WriteFile 失败: %s", result.Error)
	}

	expected := filepath.Join(dir, "hello.txt")
	if result.Path != expected {
		t.Fatalf("路径期望 %q，got %q", expected, result.Path)
	}
	if result.IsTemporary {
		t.Fatal("非临时文件不应标记 IsTemporary")
	}
	data, err := os.ReadFile(expected)
	if err != nil {
		t.Fatalf("读文件失败: %v", err)
	}
	if string(data) != "world" {
		t.Fatalf("文件内容期望 world，got %q", string(data))
	}
}

// TestWriteFileTemporaryWritesToSessionTempDir 验证：temporary=true 时文件写入
// 会话级临时目录，且 ToolResult 正确标记。
func TestWriteFileTemporaryWritesToSessionTempDir(t *testing.T) {
	dir := t.TempDir()
	exec := NewToolExecutor(dir)

	ctx := WithSessionID(context.Background(), "session-1")
	result := exec.Execute(ctx, "WriteFile", map[string]any{
		"path":      "subdir/script.py",
		"content":   "print('ok')",
		"temporary": true,
	})
	if !result.Success {
		t.Fatalf("WriteFile 失败: %s", result.Error)
	}

	expectedDir := filepath.Join(dir, ".bma", "tmp", "session-1")
	expectedFile := filepath.Join(expectedDir, "subdir", "script.py")
	if result.Path != expectedFile {
		t.Fatalf("临时文件路径期望 %q，got %q", expectedFile, result.Path)
	}
	if !result.IsTemporary {
		t.Fatal("临时文件应标记 IsTemporary=true")
	}
	if result.TempDir != expectedDir {
		t.Fatalf("TempDir 期望 %q，got %q", expectedDir, result.TempDir)
	}

	data, err := os.ReadFile(expectedFile)
	if err != nil {
		t.Fatalf("读临时文件失败: %v", err)
	}
	if string(data) != "print('ok')" {
		t.Fatalf("临时文件内容不匹配，got %q", string(data))
	}
}

// TestWriteFileTemporaryAbsolutePathConvergesToTempDir 验证：temporary=true 且传入
// 绝对路径时，文件仍被收敛到会话临时目录，防止写到预期外位置。
func TestWriteFileTemporaryAbsolutePathConvergesToTempDir(t *testing.T) {
	dir := t.TempDir()
	exec := NewToolExecutor(dir)

	// 构造一个跨平台的绝对路径作为输入，验证其被收敛到临时目录
	absInput := filepath.Join(os.TempDir(), "should-not-escape.txt")
	ctx := WithSessionID(context.Background(), "session-2")
	result := exec.Execute(ctx, "WriteFile", map[string]any{
		"path":      absInput,
		"content":   "should not escape",
		"temporary": true,
	})
	if !result.Success {
		t.Fatalf("WriteFile 失败: %s", result.Error)
	}

	expected := filepath.Join(dir, ".bma", "tmp", "session-2", "should-not-escape.txt")
	if result.Path != expected {
		t.Fatalf("绝对路径应被收敛到临时目录，期望 %q，got %q", expected, result.Path)
	}
	if _, err := os.Stat(absInput); !os.IsNotExist(err) {
		t.Fatal("临时文件不应写到会话临时目录之外")
	}
}

// TestWriteFileTemporaryRequiresSessionID 验证：temporary=true 但无 sessionID 时返回错误。
func TestWriteFileTemporaryRequiresSessionID(t *testing.T) {
	dir := t.TempDir()
	exec := NewToolExecutor(dir)

	result := exec.Execute(context.Background(), "WriteFile", map[string]any{
		"path":      "x.py",
		"content":   "x",
		"temporary": true,
	})
	if result.Success {
		t.Fatal("无 sessionID 的临时写入应失败")
	}
	if !strings.Contains(result.Error, "session context") {
		t.Fatalf("错误信息应提示 session context，got %q", result.Error)
	}
}
