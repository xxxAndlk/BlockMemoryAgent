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

// TestDetectPortConflictHintAddressInUse 验证：输出含 "Address already in use" 时
// detectPortConflictHint 返回换端口提示。
func TestDetectPortConflictHintAddressInUse(t *testing.T) {
	cmd := "python -m http.server 8000"
	out := "Traceback (most recent call last):\n" +
		"  File \"<stdin>\", line 1, in <module>\n" +
		"OSError: [Errno 98] Address already in use"
	hint := detectPortConflictHint(cmd, out)
	if hint == "" {
		t.Fatal("应检测到端口占用并返回提示")
	}
	if !strings.Contains(hint, "8000") {
		t.Fatalf("提示应含端口号 8000，got: %q", hint)
	}
	if !strings.Contains(hint, "8001") {
		t.Fatalf("提示应建议换用 8001，got: %q", hint)
	}
}

// TestDetectPortConflictHintEADDRINUSE 验证：Node.js 风格 EADDRINUSE 错误检测。
func TestDetectPortConflictHintEADDRINUSE(t *testing.T) {
	cmd := "node server.js --port 3000"
	out := "Error: listen EADDRINUSE: address already in use :::3000"
	hint := detectPortConflictHint(cmd, out)
	if hint == "" {
		t.Fatal("应检测到 EADDRINUSE")
	}
	if !strings.Contains(hint, "3000") {
		t.Fatalf("提示应含端口号 3000，got: %q", hint)
	}
}

// TestDetectPortConflictHintWindowsChinese 验证：Windows 中文端口占用错误检测。
func TestDetectPortConflictHintWindowsChinese(t *testing.T) {
	cmd := "python -m http.server 8000"
	out := "通常每个套接字地址(协议/网络地址/端口)只允许使用一次。"
	hint := detectPortConflictHint(cmd, out)
	if hint == "" {
		t.Fatal("应检测到中文端口占用错误")
	}
}

// TestDetectPortConflictHintNoConflict 验证：无端口占用模式时返回空串。
func TestDetectPortConflictHintNoConflict(t *testing.T) {
	cmd := "python -m http.server 8000"
	out := "Serving HTTP on 0.0.0.0 port 8000 (http://0.0.0.0:8000/)..."
	hint := detectPortConflictHint(cmd, out)
	if hint != "" {
		t.Fatalf("非端口占用输出不应触发提示，got: %q", hint)
	}
}

// TestDetectPortConflictHintEmptyOutput 验证：空输出返回空串。
func TestDetectPortConflictHintEmptyOutput(t *testing.T) {
	if hint := detectPortConflictHint("cmd", ""); hint != "" {
		t.Fatalf("空输出应返回空串，got: %q", hint)
	}
}

// TestExtractPortFromCommandDoubleDashPort 验证：--port=8000 / --port 8000 提取。
func TestExtractPortFromCommandDoubleDashPort(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		{"node server.js --port=8000", "8000"},
		{"node server.js --port 8000", "8000"},
		{"flask run --port 5000", "5000"},
	}
	for _, c := range cases {
		got := extractPortFromCommand(c.cmd)
		if got != c.want {
			t.Errorf("cmd=%q 期望 %q got %q", c.cmd, c.want, got)
		}
	}
}

// TestExtractPortFromCommandDashP 验证：-p 8000 提取。
func TestExtractPortFromCommandDashP(t *testing.T) {
	got := extractPortFromCommand("python -m http.server -p 8000")
	if got != "8000" {
		t.Fatalf("期望 8000 got %q", got)
	}
}

// TestExtractPortFromCommandColonPort 验证：:8000 提取（URL/host:port）。
func TestExtractPortFromCommandColonPort(t *testing.T) {
	got := extractPortFromCommand("curl http://localhost:8000/api")
	if got != "8000" {
		t.Fatalf("期望 8000 got %q", got)
	}
}

// TestDetectLongRunningServerGoRun 验证：go run 被拦截（Agent 写 server.go 后 go run 启动）。
func TestDetectLongRunningServerGoRun(t *testing.T) {
	if desc := detectLongRunningServer("go run workspace/server.go"); desc == "" {
		t.Fatal("应拦截 go run")
	}
}

// TestDetectLongRunningServerStartServerExe 验证：start "" server.exe 被拦截。
func TestDetectLongRunningServerStartServerExe(t *testing.T) {
	cases := []string{
		`cd /d workspace && start "" server.exe`,
		`start server.exe`,
		`start "" app.exe`,
		`workspace/server.exe`,
	}
	for _, cmd := range cases {
		if desc := detectLongRunningServer(cmd); desc == "" {
			t.Errorf("应拦截 %q", cmd)
		}
	}
}

// TestDetectLongRunningServerAllowsGoBuild 验证：go build（仅编译不运行）不被误拦。
func TestDetectLongRunningServerAllowsGoBuild(t *testing.T) {
	if desc := detectLongRunningServer("go build -o nul workspace/main.go"); desc != "" {
		t.Fatalf("go build 不应被拦截，got: %q", desc)
	}
}

// TestDetectLongRunningServerAllowsStartNotepad 验证：start notepad 等无害命令不被误拦。
func TestDetectLongRunningServerAllowsStartNotepad(t *testing.T) {
	if desc := detectLongRunningServer(`start notepad.exe`); desc != "" {
		t.Fatalf("start notepad 不应被拦截，got: %q", desc)
	}
}

// TestDetectFileHelperScriptSysStdoutWrite 验证：Python 脚本用 sys.stdout.write 绕过
// print 检测的脚本被拦截（塔防 demo 事故 v2：_read_tail.py）。
func TestDetectFileHelperScriptSysStdoutWrite(t *testing.T) {
	content := `import sys
with open('game_core.js', 'r', encoding='utf-8') as f:
    lines = f.readlines()
for line in lines[-50:]:
    sys.stdout.write(line)
sys.stdout.flush()
`
	if reason := detectFileHelperScript("workspace/_read_tail.py", content); reason == "" {
		t.Fatal("应拦截 sys.stdout.write + readlines 的读文件脚本")
	}
}

// TestDetectFileHelperScriptSysStdoutBufferWrite 验证：sys.stdout.buffer.write 也被拦截。
func TestDetectFileHelperScriptSysStdoutBufferWrite(t *testing.T) {
	content := `import sys
with open('game.js', 'rb') as f:
    data = f.read()
sys.stdout.buffer.write(data)
`
	if reason := detectFileHelperScript("workspace/_dump.py", content); reason == "" {
		t.Fatal("应拦截 sys.stdout.buffer.write + read 的读文件脚本")
	}
}
