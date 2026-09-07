package tool

// trust_mode_test.go 验证三级信任模式（TODO 第10⑥，对标 Codex）：
//   - full-auto：全自主，破坏性操作（含危险命令/生产目录写）hook 零调用；
//   - auto-edit：文件编辑直通，RunCommand（含危险命令）与动态 Destructive 审批；
//   - suggest：全部变更类动作（WriteFile/RunCommand）逐条审批，读类直通；
//   - 模式经 ctx 读取器实时读取——会话中途切换下一工具调用即生效；
//   - ctx 未携带值回退现网语义（approvalDisabled 全信任开关 / 生产边界 + 危险命令规则）。

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/blockmemory/agent/backend/internal/config"
)

// newTrustTestRegistry 构造带审批计数器的生产目录 Registry（dir 即 production_workdir，
// 生产边界语义下 WriteFile 命中；hook 拒绝并计数，被拒操作不执行）。
func newTrustTestRegistry(t *testing.T, dir string) (*Registry, *int) {
	t.Helper()
	r := NewBuiltinRegistry(dir, &config.AgentConfig{SafetyConfig: config.SafetyConfig{ProductionWorkDir: dir}}, nil)
	calls := 0
	r.SetApprovalHook(func(ctx context.Context, name string, args map[string]any) (bool, error) {
		calls++
		return false, nil
	})
	return r, &calls
}

// TestTrustMode_FullAuto 验证 full-auto：危险命令与生产目录 WriteFile 均直接执行，hook 零调用。
func TestTrustMode_FullAuto(t *testing.T) {
	dir := t.TempDir()
	r, calls := newTrustTestRegistry(t, dir)
	ctx := WithTrustModeFunc(WithAgentID(context.Background(), "meta"), func() string { return TrustModeFullAuto })

	if _, err := r.Dispatch(ctx, "RunCommand", map[string]any{"command": "git push origin main"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	res, err := r.Dispatch(ctx, "WriteFile", map[string]any{"path": "a.txt", "content": "42"})
	if err != nil || res == nil || !res.Success {
		t.Fatalf("full-auto write should succeed, res=%+v err=%v", res, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatalf("file should be written without approval: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("full-auto must never trigger approval, got %d calls", *calls)
	}
}

// TestTrustMode_AutoEdit 验证 auto-edit：WriteFile 直通（不问），RunCommand 审批（含危险命令），
// 读类工具直通。
func TestTrustMode_AutoEdit(t *testing.T) {
	dir := t.TempDir()
	r, calls := newTrustTestRegistry(t, dir)
	ctx := WithTrustModeFunc(WithAgentID(context.Background(), "meta"), func() string { return TrustModeAutoEdit })

	// 文件编辑直通：hook 不被调，文件真实写入。
	res, err := r.Dispatch(ctx, "WriteFile", map[string]any{"path": "a.txt", "content": "42"})
	if err != nil || res == nil || !res.Success {
		t.Fatalf("auto-edit write should pass through, res=%+v err=%v", res, err)
	}
	if *calls != 0 {
		t.Fatalf("auto-edit must not gate file writes, got %d calls", *calls)
	}

	// RunCommand 审批：普通命令也问（危险命令自然恒问）；拒绝则不执行。
	res2, err := r.Dispatch(ctx, "RunCommand", map[string]any{"command": "node --check x.js"})
	if err != nil {
		t.Fatalf("denial should not surface as dispatch error: %v", err)
	}
	if res2 == nil || res2.Success {
		t.Fatalf("command should be denied in auto-edit, got %+v", res2)
	}
	if *calls != 1 {
		t.Fatalf("auto-edit must gate RunCommand, got %d calls", *calls)
	}

	// 读类直通：ReadFile 不进审批链（文件不存在返回工具级错误，但 hook 不被调）。
	if _, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt", "offset": float64(1), "limit": float64(10)}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("auto-edit must not gate reads, got %d calls", *calls)
	}
}

// TestTrustMode_Suggest 验证 suggest：WriteFile 与 RunCommand 全部逐条审批（拒绝则不执行），
// 读类直通。
func TestTrustMode_Suggest(t *testing.T) {
	dir := t.TempDir()
	r, calls := newTrustTestRegistry(t, dir)
	ctx := WithTrustModeFunc(WithAgentID(context.Background(), "meta"), func() string { return TrustModeSuggest })

	// 写文件审批：拒绝则不写。
	res, err := r.Dispatch(ctx, "WriteFile", map[string]any{"path": "a.txt", "content": "42"})
	if err != nil {
		t.Fatalf("denial should not surface as dispatch error: %v", err)
	}
	if res == nil || res.Success {
		t.Fatalf("suggest should gate WriteFile, got %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err == nil {
		t.Fatal("file must not be written when denied")
	}

	// 命令审批（含普通命令）。
	if _, err := r.Dispatch(ctx, "RunCommand", map[string]any{"command": "node --check x.js"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if *calls != 2 {
		t.Fatalf("suggest must gate every mutating call, got %d calls", *calls)
	}

	// 读类直通。
	if _, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt", "offset": float64(1), "limit": float64(10)}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if *calls != 2 {
		t.Fatalf("suggest must not gate reads, got %d calls", *calls)
	}
}

// TestTrustMode_MidSessionSwitch 验证模式经读取器实时读取：同一 ctx 下切换模式，
// 下一工具调用即按新模式裁决（会话中途切换语义）。
func TestTrustMode_MidSessionSwitch(t *testing.T) {
	dir := t.TempDir()
	r, calls := newTrustTestRegistry(t, dir)
	mode := TrustModeFullAuto
	ctx := WithTrustModeFunc(WithAgentID(context.Background(), "meta"), func() string { return mode })

	// full-auto：危险命令直通。
	if _, err := r.Dispatch(ctx, "RunCommand", map[string]any{"command": "git push origin main"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("full-auto phase must not gate, got %d calls", *calls)
	}

	// 中途切到 auto-edit：下一次同类调用立即被拦。
	mode = TrustModeAutoEdit
	if _, err := r.Dispatch(ctx, "RunCommand", map[string]any{"command": "git push origin main"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("mode switch must take effect on next tool call, got %d calls", *calls)
	}
}

// TestTrustMode_NoCtxValueFallsBack 验证 ctx 未携带模式时回退现网语义：
// approvalDisabled=true 全放行；false 时生产边界 + 危险命令规则照旧生效。
func TestTrustMode_NoCtxValueFallsBack(t *testing.T) {
	dir := t.TempDir()
	r, calls := newTrustTestRegistry(t, dir)
	r.SetApprovalDisabled(true)
	// 无 WithTrustModeFunc：回退 approvalDisabled 语义，危险命令直通。
	ctx := WithAgentID(context.Background(), "meta")
	if _, err := r.Dispatch(ctx, "RunCommand", map[string]any{"command": "git push origin main"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("approvalDisabled fallback must not gate, got %d calls", *calls)
	}

	// 读取器返回空串同义回退：恢复生产边界规则后危险命令恒触发。
	r2, calls2 := newTrustTestRegistry(t, dir)
	ctx2 := WithTrustModeFunc(WithAgentID(context.Background(), "meta"), func() string { return "" })
	if _, err := r2.Dispatch(ctx2, "RunCommand", map[string]any{"command": "git push origin main"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if *calls2 != 1 {
		t.Fatalf("empty mode fallback must apply legacy dangerous-command rule, got %d calls", *calls2)
	}
}

// TestValidTrustMode 验证枚举校验。
func TestValidTrustMode(t *testing.T) {
	for _, m := range []string{TrustModeSuggest, TrustModeAutoEdit, TrustModeFullAuto} {
		if !ValidTrustMode(m) {
			t.Errorf("ValidTrustMode(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"", "auto", "full_auto", "SUGGEST"} {
		if ValidTrustMode(m) {
			t.Errorf("ValidTrustMode(%q) = true, want false", m)
		}
	}
}
