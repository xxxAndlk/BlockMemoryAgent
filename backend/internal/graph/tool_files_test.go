package graph

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMarshalNoHTMLEscape 验证：marshalNoHTMLEscape 不转义 < > &，
// 避免 TUI/日志展示 ArgsJSON 时出现 < > & 乱码。
func TestMarshalNoHTMLEscape(t *testing.T) {
	args := map[string]any{
		"command": "cd workspace && python -m http.server 8000",
		"html":    "<!DOCTYPE html>",
	}
	out, err := marshalNoHTMLEscape(args)
	if err != nil {
		t.Fatalf("marshal 失败: %v", err)
	}
	s := string(out)
	// 不应含 HTML 转义序列
	if strings.Contains(s, "\\u003c") || strings.Contains(s, "\\u003e") || strings.Contains(s, "\\u0026") {
		t.Fatalf("不应含 HTML 转义序列，got: %s", s)
	}
	// 应含原始字符
	if !strings.Contains(s, "&&") {
		t.Fatalf("应含 && 原始字符，got: %s", s)
	}
	if !strings.Contains(s, "<!DOCTYPE") {
		t.Fatalf("应含 <!DOCTYPE 原始字符，got: %s", s)
	}
}

// TestMarshalNoHTMLEscapeEmptyAndNil 验证边界：空 map 和 nil 不报错。
func TestMarshalNoHTMLEscapeEmptyAndNil(t *testing.T) {
	if _, err := marshalNoHTMLEscape(map[string]any{}); err != nil {
		t.Fatalf("空 map 不应报错，got: %v", err)
	}
	if _, err := marshalNoHTMLEscape(nil); err != nil {
		t.Fatalf("nil 不应报错，got: %v", err)
	}
}

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

// TestWriteFileRejectsMailboxJSONFile 验证：WriteFile 拒绝写 mailbox/to-*.json 类
// 伪造邮箱消息文件。
func TestWriteFileRejectsMailboxJSONFile(t *testing.T) {
	dir := t.TempDir()
	exec := NewToolExecutor(dir)

	ctx := WithSessionID(context.Background(), "s1")
	result := exec.Execute(ctx, "WriteFile", map[string]any{
		"path":    "workspace/mailbox/to-combat.json",
		"content": `{"from":"ai","msg":"x"}`,
	})
	if result.Success {
		t.Fatal("应拒绝写 mailbox/*.json 伪造邮箱文件")
	}
	if !strings.Contains(result.Error, "禁止用 WriteFile 写邮箱消息文件") {
		t.Fatalf("错误应提示邮箱文件化拦截，got %q", result.Error)
	}
}

// TestWriteFileRejectsFileHelperPythonScript 验证：WriteFile 拒绝写仅用于读文件/列目录
// 的 Python 辅助脚本。
func TestWriteFileRejectsFileHelperPythonScript(t *testing.T) {
	dir := t.TempDir()
	exec := NewToolExecutor(dir)

	ctx := WithSessionID(context.Background(), "s1")
	// 命中 open(...).read() + print 模式
	result := exec.Execute(ctx, "WriteFile", map[string]any{
		"path":    "workspace/read_game.py",
		"content": "data = open('game.js').read()\nprint(data)",
	})
	if result.Success {
		t.Fatal("应拒绝写读文件类 Python 脚本")
	}
	if !strings.Contains(result.Error, "禁止写脚本做文件读取") {
		t.Fatalf("错误应提示文件辅助脚本拦截，got %q", result.Error)
	}
}

// TestWriteFileRejectsFileHelperShellScript 验证：WriteFile 拒绝写 cat/ls/grep+echo
// 类 Shell 辅助脚本。
func TestWriteFileRejectsFileHelperShellScript(t *testing.T) {
	dir := t.TempDir()
	exec := NewToolExecutor(dir)

	ctx := WithSessionID(context.Background(), "s1")
	result := exec.Execute(ctx, "WriteFile", map[string]any{
		"path":    "workspace/list.sh",
		"content": "#!/bin/sh\nls -la\necho done",
	})
	if result.Success {
		t.Fatal("应拒绝写 ls+echo 类 Shell 脚本")
	}
	if !strings.Contains(result.Error, "禁止写脚本做文件读取") {
		t.Fatalf("错误应提示文件辅助脚本拦截，got %q", result.Error)
	}
}

// TestWriteFileRejectsMailboxGoProgram 验证：WriteFile 拒绝写 Go 程序绕过邮箱投递
// （Agent 写 send_interface.go 调 runtime.Mailbox.Send 的反模式）。
func TestWriteFileRejectsMailboxGoProgram(t *testing.T) {
	dir := t.TempDir()
	exec := NewToolExecutor(dir)

	ctx := WithSessionID(context.Background(), "s1")
	result := exec.Execute(ctx, "WriteFile", map[string]any{
		"path": "workspace/send_interface.go",
		"content": `package main

import "github.com/blockmemory/agent/backend/internal/runtime"

func main() {
	runtime.Mailbox.Send("combat", "attack")
}
`,
	})
	if result.Success {
		t.Fatal("应拒绝写 Go 程序绕过邮箱投递")
	}
	if !strings.Contains(result.Error, "禁止写 Go 程序伪造邮箱投递") {
		t.Fatalf("错误应提示 Go 邮箱程序拦截，got %q", result.Error)
	}
}

// TestWriteFileAllowsNormalGoFile 验证：正常 Go 源码（不引用 runtime.Mailbox）不被误拦。
func TestWriteFileAllowsNormalGoFile(t *testing.T) {
	dir := t.TempDir()
	exec := NewToolExecutor(dir)

	ctx := WithSessionID(context.Background(), "s1")
	result := exec.Execute(ctx, "WriteFile", map[string]any{
		"path": "workspace/snake.go",
		"content": `package main

import "fmt"

func main() {
	fmt.Println("snake game")
}
`,
	})
	if !result.Success {
		t.Fatalf("正常 Go 文件不应被拦截，got error: %s", result.Error)
	}
}

// TestWriteFileRejectsProtectedPath 验证：WriteFile 拒绝写受保护的项目目录
// （backend/、test/、config/ 等）。
func TestWriteFileRejectsProtectedPath(t *testing.T) {
	dir := t.TempDir()
	exec := NewToolExecutor(dir)

	ctx := WithSessionID(context.Background(), "s1")
	result := exec.Execute(ctx, "WriteFile", map[string]any{
		"path":    "backend/internal/evil/evil.go",
		"content": "package evil",
	})
	if result.Success {
		t.Fatal("应拒绝写 backend/ 受保护目录")
	}
	if !strings.Contains(result.Error, "受保护目录") {
		t.Fatalf("错误应提示受保护目录拦截，got %q", result.Error)
	}
}

// TestWriteFileRejectsRootGoMod 验证：WriteFile 拒绝写根 go.mod（污染 module 配置）。
func TestWriteFileRejectsRootGoMod(t *testing.T) {
	dir := t.TempDir()
	exec := NewToolExecutor(dir)

	ctx := WithSessionID(context.Background(), "s1")
	result := exec.Execute(ctx, "WriteFile", map[string]any{
		"path":    "go.mod",
		"content": "module fake",
	})
	if result.Success {
		t.Fatal("应拒绝写根 go.mod")
	}
	if !strings.Contains(result.Error, "go.mod") {
		t.Fatalf("错误应提示 go.mod 拦截，got %q", result.Error)
	}
}

// TestWriteFileRejectsSpaceInPathSegment 验证：WriteFile 拒绝路径段含空格
// （"docs workspace" 应被拦截）。
func TestWriteFileRejectsSpaceInPathSegment(t *testing.T) {
	dir := t.TempDir()
	exec := NewToolExecutor(dir)

	ctx := WithSessionID(context.Background(), "s1")
	result := exec.Execute(ctx, "WriteFile", map[string]any{
		"path":    "docs workspace/file.txt",
		"content": "x",
	})
	if result.Success {
		t.Fatal("应拒绝路径段含空格")
	}
	if !strings.Contains(result.Error, "space") {
		t.Fatalf("错误应提示空格拦截，got %q", result.Error)
	}
}

// TestWriteFileAllowSpacesOverride 验证：allow_spaces=true 时放行含空格路径。
func TestWriteFileAllowSpacesOverride(t *testing.T) {
	dir := t.TempDir()
	exec := NewToolExecutor(dir)

	ctx := WithSessionID(context.Background(), "s1")
	result := exec.Execute(ctx, "WriteFile", map[string]any{
		"path":         "workspace/my dir/file.txt",
		"content":      "x",
		"allow_spaces": true,
	})
	if !result.Success {
		t.Fatalf("allow_spaces=true 应放行，got error: %s", result.Error)
	}
}
