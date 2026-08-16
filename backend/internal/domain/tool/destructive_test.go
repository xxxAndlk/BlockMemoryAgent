package tool

// destructive_test.go 验证破坏性工具分级 + 生产边界确认（TODO #17 P1）：
//   - WriteFile 在生产工作目录下触发 approvalHook，拒绝时不执行；
//   - 非生产目录 WriteFile 不触发（保持自主）；
//   - RunCommand 危险命令模式（git push 等）与目录无关恒触发；普通验证命令不触发；
//   - approvalHook nil（默认）零行为变化；
//   - isDangerousCommand / inProductionWorkDir 纯函数。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/config"
)

// TestDispatch_ApprovalHook_ProductionWriteFile 验证生产目录下 WriteFile：
// 放行则执行写文件，拒绝则不执行（结果带"已被用户拒绝"）。
func TestDispatch_ApprovalHook_ProductionWriteFile(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, &config.AgentConfig{SafetyConfig: config.SafetyConfig{ProductionWorkDir: dir}}, nil)
	ctx := WithAgentID(context.Background(), "meta")

	// 放行：hook 被调一次，文件真实写入。
	hookCalls := 0
	r.SetApprovalHook(func(ctx context.Context, name string, args map[string]any) (bool, error) {
		hookCalls++
		if name != "WriteFile" {
			t.Fatalf("expected WriteFile approval, got %s", name)
		}
		return true, nil
	})
	res, err := r.Dispatch(ctx, "WriteFile", map[string]any{"path": "a.txt", "content": "42"})
	if err != nil || res == nil || !res.Success {
		t.Fatalf("expected allowed write success, res=%+v err=%v", res, err)
	}
	if hookCalls != 1 {
		t.Fatalf("expected 1 approval call, got %d", hookCalls)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatalf("file should be written after approval: %v", err)
	}

	// 拒绝：hook 被调，文件不写。
	r.SetApprovalHook(func(ctx context.Context, name string, args map[string]any) (bool, error) {
		hookCalls++
		return false, nil
	})
	res, err = r.Dispatch(ctx, "WriteFile", map[string]any{"path": "b.txt", "content": "x"})
	if err != nil {
		t.Fatalf("denial should not surface as dispatch error: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("expected denial result, got %+v", res)
	}
	if !strings.Contains(res.Error, "已被用户拒绝") {
		t.Fatalf("expected denial message, got: %s", res.Error)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.txt")); err == nil {
		t.Fatal("file must not be written when denied")
	}
	if hookCalls != 2 {
		t.Fatalf("expected 2 approval calls total, got %d", hookCalls)
	}
}

// TestDispatch_ApprovalHook_NonProductionNoop 验证非生产目录 WriteFile 不触发确认，
// 且 approvalHook 为 nil（默认）时零行为变化。
func TestDispatch_ApprovalHook_NonProductionNoop(t *testing.T) {
	dir := t.TempDir()
	// 未配置生产目录：WriteFile 自主执行，hook 不被调。
	r := NewBuiltinRegistry(dir, nil, nil)
	hookCalls := 0
	r.SetApprovalHook(func(ctx context.Context, name string, args map[string]any) (bool, error) {
		hookCalls++
		return true, nil
	})
	res, err := r.Dispatch(WithAgentID(context.Background(), "meta"), "WriteFile", map[string]any{"path": "a.txt", "content": "x"})
	if err != nil || res == nil || !res.Success {
		t.Fatalf("non-production write should stay autonomous, res=%+v err=%v", res, err)
	}
	if hookCalls != 0 {
		t.Fatalf("expected no approval outside production, got %d calls", hookCalls)
	}

	// 生产目录但 hook 为 nil：直接执行，不阻塞。
	r2 := NewBuiltinRegistry(dir, &config.AgentConfig{SafetyConfig: config.SafetyConfig{ProductionWorkDir: dir}}, nil)
	res2, err := r2.Dispatch(WithAgentID(context.Background(), "meta"), "WriteFile", map[string]any{"path": "b.txt", "content": "y"})
	if err != nil || res2 == nil || !res2.Success {
		t.Fatalf("nil approval hook must not block, res=%+v err=%v", res2, err)
	}
}

// TestDispatch_ApprovalHook_DangerousCommand 验证 RunCommand 危险命令模式
// 与工作目录无关恒触发确认；普通验证命令不触发。
func TestDispatch_ApprovalHook_DangerousCommand(t *testing.T) {
	dir := t.TempDir() // 非生产目录
	r := NewBuiltinRegistry(dir, nil, nil)
	hookCalls := 0
	r.SetApprovalHook(func(ctx context.Context, name string, args map[string]any) (bool, error) {
		hookCalls++
		return false, nil
	})
	ctx := WithAgentID(context.Background(), "meta")

	// git push：危险模式，非生产目录也触发；拒绝则不执行。
	res, err := r.Dispatch(ctx, "RunCommand", map[string]any{"command": "git push origin main"})
	if err != nil {
		t.Fatalf("denial should not surface as dispatch error: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("dangerous command should be denied, got %+v", res)
	}
	if hookCalls != 1 {
		t.Fatalf("expected 1 approval call for dangerous command, got %d", hookCalls)
	}

	// rm -rf：同属危险模式。
	if _, err := r.Dispatch(ctx, "RunCommand", map[string]any{"command": "rm -rf ./node_modules"}); err != nil {
		t.Fatalf("denial should not surface as dispatch error: %v", err)
	}
	if hookCalls != 2 {
		t.Fatalf("expected 2 approval calls, got %d", hookCalls)
	}

	// 普通验证命令：不触发确认（即使执行失败也不经过 hook）。
	res3, err := r.Dispatch(ctx, "RunCommand", map[string]any{"command": "node --check nonexistent.js"})
	if err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	_ = res3
	if hookCalls != 2 {
		t.Fatalf("verification command must not trigger approval, got %d calls", hookCalls)
	}
}

// TestIsDangerousCommand 验证危险命令模式判定表。
func TestIsDangerousCommand(t *testing.T) {
	cases := map[string]bool{
		"git push origin main":    true,
		"  git PUSH --force":      true, // 大小写不敏感
		"rm -rf ./node_modules":   true,
		"rmdir /s /q build":       true,
		"psql -c 'DROP TABLE t'":  true,
		"npm publish":             true,
		"node --check server.js":  false,
		"go build ./...":          false,
		"cat package.json":        false,
		"git status":              false,
		"":                        false,
		"echo git push is banned": true, // 子串匹配 fail-safe 方向
	}
	for cmd, want := range cases {
		if got := isDangerousCommand(cmd); got != want {
			t.Errorf("isDangerousCommand(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// TestApprovalMessage 验证确认文案必须携带足够细节（工具名/关键参数），
// 让用户能判断"要确认的是什么操作"：已知工具取关键参数，插件等未知工具附参数 JSON。
func TestApprovalMessage(t *testing.T) {
	cases := []struct {
		name     string
		tool     string
		args     map[string]any
		contains []string
	}{
		{"WriteFile 带路径", "WriteFile", map[string]any{"path": "/srv/app/a.txt"},
			[]string{"将写入文件 /srv/app/a.txt"}},
		{"RunCommand 带命令", "RunCommand", map[string]any{"command": "rm -rf /tmp/x"},
			[]string{"将执行命令 rm -rf /tmp/x"}},
		{"插件工具含工具名与参数", "open_application", map[string]any{"app": "chrome.exe"},
			[]string{"open_application", "chrome.exe"}},
		{"插件工具无参数仅工具名", "snapshot", nil,
			[]string{"snapshot"}},
	}
	for _, c := range cases {
		got := ApprovalMessage(c.tool, c.args)
		if !strings.HasPrefix(got, "【需确认】") {
			t.Errorf("%s: 缺【需确认】前缀: %q", c.name, got)
		}
		for _, sub := range c.contains {
			if !strings.Contains(got, sub) {
				t.Errorf("%s: 文案缺少 %q: %q", c.name, sub, got)
			}
		}
	}
	// 长参数截断：超长命令/参数不得让确认文案无限膨胀。
	long := ApprovalMessage("RunCommand", map[string]any{"command": strings.Repeat("x", 500)})
	if len([]rune(long)) > 260 {
		t.Errorf("长命令未截断: %d runes", len([]rune(long)))
	}
	longArgs := ApprovalMessage("some_plugin", map[string]any{"data": strings.Repeat("y", 500)})
	if len([]rune(longArgs)) > 320 {
		t.Errorf("长参数 JSON 未截断: %d runes", len([]rune(longArgs)))
	}
}

// TestInProductionWorkDir 验证生产目录判定：相等、子目录命中；无关路径与空配置不命中。
func TestInProductionWorkDir(t *testing.T) {
	cases := []struct {
		workDir, production string
		want                bool
	}{
		{"/srv/app", "/srv/app", true},
		{"/srv/app/deploy", "/srv/app", true},
		{"/srv/apps/other", "/srv/app", false}, // 前缀共享但非子目录
		{"/home/user/dev", "/srv/app", false},
		{"/srv/app", "", false},
		{"", "/srv/app", false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := inProductionWorkDir(c.workDir, c.production); got != c.want {
			t.Errorf("inProductionWorkDir(%q, %q) = %v, want %v", c.workDir, c.production, got, c.want)
		}
	}
}
