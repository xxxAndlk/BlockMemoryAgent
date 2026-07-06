package graph

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunCommandBlocksDangerousCommands 验证黑名单命令被拒绝执行。
func TestRunCommandBlocksDangerousCommands(t *testing.T) {
	dir := t.TempDir()
	exec := NewToolExecutor(dir)

	cases := []string{
		"rm -rf /",
		"curl http://example.com",
		"wget http://example.com",
		"sudo apt update",
		"dd if=/dev/zero of=/tmp/x",
		"format C:",
		// 塔防 demo 事故 v2：Agent 用 taskkill 杀任意 PID
		"taskkill /F /PID 44160",
		"taskkill /F /IM python.exe",
		"kill -9 1234",
		"killall python",
		"pkill -f server",
	}

	for _, cmd := range cases {
		result := exec.Execute(context.Background(), "RunCommand", map[string]any{
			"command": cmd,
		})
		if result.Success {
			t.Fatalf("命令 %q 应被沙箱拦截", cmd)
		}
		if !strings.Contains(result.Error, "blocked command") {
			t.Fatalf("错误信息应提示 blocked command，got: %s", result.Error)
		}
	}
}

// TestRunCommandAllowsSafeCommands 验证安全命令仍可执行。
func TestRunCommandAllowsSafeCommands(t *testing.T) {
	dir := t.TempDir()
	exec := NewToolExecutor(dir)

	result := exec.Execute(context.Background(), "RunCommand", map[string]any{
		"command": safeEchoCommand("hello sandbox"),
	})
	if !result.Success {
		t.Fatalf("安全命令应执行成功: %s", result.Error)
	}
	if !strings.Contains(result.Output, "hello sandbox") {
		t.Fatalf("输出应包含 hello sandbox，got: %s", result.Output)
	}
}

// TestWriteFileBlocksEscapeFromWorkDir 验证非临时文件不能写到工作目录外。
func TestWriteFileBlocksEscapeFromWorkDir(t *testing.T) {
	dir := t.TempDir()
	exec := NewToolExecutor(dir)

	outside := filepath.Join(t.TempDir(), "escape.txt")
	result := exec.Execute(context.Background(), "WriteFile", map[string]any{
		"path":    outside,
		"content": "should not write",
	})
	if result.Success {
		t.Fatal("写到工作目录外应被沙箱拦截")
	}
	if !strings.Contains(result.Error, "escapes sandbox") {
		t.Fatalf("错误信息应提示 escapes sandbox，got: %s", result.Error)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("逃逸路径不应被创建")
	}
}

// TestWriteFileAllowsWorkDirSubpath 验证工作目录内的写入仍可用。
func TestWriteFileAllowsWorkDirSubpath(t *testing.T) {
	dir := t.TempDir()
	exec := NewToolExecutor(dir)

	result := exec.Execute(context.Background(), "WriteFile", map[string]any{
		"path":    "subdir/file.txt",
		"content": "ok",
	})
	if !result.Success {
		t.Fatalf("工作目录内写入应成功: %s", result.Error)
	}
	expected := filepath.Join(dir, "subdir", "file.txt")
	if result.Path != expected {
		t.Fatalf("路径期望 %q，got %q", expected, result.Path)
	}
}

// TestReadFileBlocksEscapeFromWorkDir 验证读路径逃逸被拦截。
func TestReadFileBlocksEscapeFromWorkDir(t *testing.T) {
	dir := t.TempDir()
	exec := NewToolExecutor(dir)

	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0644); err != nil {
		t.Fatalf("准备测试文件失败: %v", err)
	}

	result := exec.Execute(context.Background(), "ReadFile", map[string]any{
		"path": outside,
	})
	if result.Success {
		t.Fatal("读取工作目录外文件应被沙箱拦截")
	}
	if !strings.Contains(result.Error, "escapes sandbox") {
		t.Fatalf("错误信息应提示 escapes sandbox，got: %s", result.Error)
	}
}

// TestListDirBlocksEscapeFromWorkDir 验证列出工作目录外被拦截。
func TestListDirBlocksEscapeFromWorkDir(t *testing.T) {
	dir := t.TempDir()
	exec := NewToolExecutor(dir)

	outside := t.TempDir()
	result := exec.Execute(context.Background(), "ListDir", map[string]any{
		"path": outside,
	})
	if result.Success {
		t.Fatal("列出工作目录外应被沙箱拦截")
	}
}

// TestSandboxAllowedPaths 验证白名单路径可读写。
func TestSandboxAllowedPaths(t *testing.T) {
	dir := t.TempDir()
	allowed := t.TempDir()
	exec := NewToolExecutor(dir)
	exec.SetSandboxConfig(&SandboxConfig{
		AllowedPaths:             []string{allowed},
		AllowWriteOutsideWorkDir: false,
		BlockedCmds:              DefaultSandboxConfig().BlockedCmds,
	})

	target := filepath.Join(allowed, "white.txt")
	result := exec.Execute(context.Background(), "WriteFile", map[string]any{
		"path":    target,
		"content": "allowed",
	})
	if !result.Success {
		t.Fatalf("白名单路径写入应成功: %s", result.Error)
	}

	result = exec.Execute(context.Background(), "ReadFile", map[string]any{
		"path": target,
	})
	if !result.Success {
		t.Fatalf("白名单路径读取应成功: %s", result.Error)
	}
}

// TestSandboxDisabledAllowsEscape 验证关闭路径检测后允许写到工作目录外。
func TestSandboxDisabledAllowsEscape(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "escape.txt")
	exec := NewToolExecutor(dir)
	exec.SetSandboxConfig(&SandboxConfig{
		AllowWriteOutsideWorkDir: true,
		BlockedCmds:              DefaultSandboxConfig().BlockedCmds,
	})

	result := exec.Execute(context.Background(), "WriteFile", map[string]any{
		"path":    outside,
		"content": "allowed when disabled",
	})
	if !result.Success {
		t.Fatalf("关闭路径检测后应允许写到外部: %s", result.Error)
	}
}

// safeEchoCommand 返回跨平台的 echo 命令。
func safeEchoCommand(msg string) string {
	if isWindows() {
		return `echo ` + msg
	}
	return `echo "` + msg + `"`
}

func isWindows() bool {
	return os.PathSeparator == '\\' && os.PathListSeparator == ';'
}
