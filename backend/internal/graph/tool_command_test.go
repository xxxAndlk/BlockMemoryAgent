package graph

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRunCommandInjectsSessionTempDirEnv 验证：RunCommand 执行时会注入
// BMA_SESSION_TEMP_DIR 环境变量，指向本会话的临时目录。
func TestRunCommandInjectsSessionTempDirEnv(t *testing.T) {
	dir := t.TempDir()
	exec := NewToolExecutor(dir)

	ctx := WithSessionID(context.Background(), "session-env")
	result := exec.Execute(ctx, "RunCommand", map[string]any{
		"command": envDumpCommand(),
	})
	if !result.Success {
		t.Fatalf("RunCommand 失败: %s", result.Error)
	}

	expected := filepath.Join(dir, ".bma", "tmp", "session-env")
	want := "BMA_SESSION_TEMP_DIR=" + expected
	if !strings.Contains(result.Output, want) {
		t.Fatalf("命令输出应包含 %q，got:\n%s", want, result.Output)
	}
}

// TestRunCommandWithoutSessionIDNoEnv 验证：无 sessionID 时不注入临时目录环境变量。
func TestRunCommandWithoutSessionIDNoEnv(t *testing.T) {
	dir := t.TempDir()
	exec := NewToolExecutor(dir)

	result := exec.Execute(context.Background(), "RunCommand", map[string]any{
		"command": envDumpCommand(),
	})
	if !result.Success {
		t.Fatalf("RunCommand 失败: %s", result.Error)
	}
	if strings.Contains(result.Output, "BMA_SESSION_TEMP_DIR=") {
		t.Fatal("无 sessionID 时不应注入 BMA_SESSION_TEMP_DIR")
	}
}

// envDumpCommand 返回一个跨平台的打印环境变量命令。
func envDumpCommand() string {
	if runtime.GOOS == "windows" {
		return "set"
	}
	return "env"
}
