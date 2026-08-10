package tool

// loop_guard_test.go 验证 TODO #20 第一层：三层循环守卫（连读死循环/探索预算耗尽/
// 连续失败）命中时返回包装 ErrLoopExit 哨兵的非 nil error，不再写 blades ActionLoopExit
// 上下文信号（无注入方，静默丢弃成死代码）；ReAct 主循环据此终止。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoopGuard_ConsecutiveSameRead_ReturnsErrLoopExit 连读守卫第 3 次命中：
// Result 文案不变（含"死循环"），err 非 nil 且 errors.Is(ErrLoopExit)。
func TestLoopGuard_ConsecutiveSameRead_ReturnsErrLoopExit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s1")

	for i := 1; i <= 2; i++ {
		res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt"})
		if err != nil || !res.Success {
			t.Fatalf("read %d should succeed: err=%v success=%v", i, err, res.Success)
		}
	}
	res3, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt"})
	if res3.Success {
		t.Fatal("3rd identical read should be blocked")
	}
	if !strings.Contains(res3.Error, "死循环") {
		t.Fatalf("LLM 文案应含死循环提示, got: %s", res3.Error)
	}
	if !errors.Is(err, ErrLoopExit) {
		t.Fatalf("expected ErrLoopExit, got: %v", err)
	}
}

// TestLoopGuard_ExploreBudget_ReturnsErrLoopExit 探索预算耗尽（写前 8 次）：
// 第 9 次探索类调用返回 ErrLoopExit；非探索类 RunCommand（验证/动作）不受影响。
func TestLoopGuard_ExploreBudget_ReturnsErrLoopExit(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := 0; i < 60; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(strings.Join(lines, "\n")), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s-budget")

	for i := 1; i <= exploreBudget; i++ {
		res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt", "offset": float64(i), "limit": float64(1)})
		if err != nil || !res.Success {
			t.Fatalf("read %d within budget should succeed: err=%v", i, err)
		}
	}
	res9, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "a.txt", "offset": float64(30), "limit": float64(1)})
	if res9.Success {
		t.Fatal("read beyond budget should be blocked")
	}
	if !strings.Contains(res9.Error, "探索预算耗尽") {
		t.Fatalf("expected budget error, got: %s", res9.Error)
	}
	if !errors.Is(err, ErrLoopExit) {
		t.Fatalf("expected ErrLoopExit on explore budget exhaustion, got: %v", err)
	}

	// 回归（#16）：验证/动作类 RunCommand 不计探索预算，预算耗尽后仍可执行。
	cmdRes, cmdErr := r.Dispatch(ctx, "RunCommand", map[string]any{"command": "echo hello"})
	if cmdErr != nil || !cmdRes.Success {
		t.Fatalf("RunCommand verify should survive explore budget exhaustion: err=%v res=%+v", cmdErr, cmdRes)
	}
	// 只读型 RunCommand（cat）按探索计费：预算已耗尽，命中守卫。
	if catRes, catErr := r.Dispatch(ctx, "RunCommand", map[string]any{"command": "cat a.txt"}); catRes.Success || !errors.Is(catErr, ErrLoopExit) {
		t.Fatalf("read-like RunCommand beyond budget should hit ErrLoopExit: res=%+v err=%v", catRes, catErr)
	}
}

// TestLoopGuard_ConsecutiveFailures_ReturnsErrLoopExit 单工具连续失败 ×3：
// 第 3 次返回 ErrLoopExit；期间成功调用重置计数，不会误触发。
func TestLoopGuard_ConsecutiveFailures_ReturnsErrLoopExit(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s1")

	// 前 2 次失败（offset 各不相同，避开连读同参守卫）：返回工具错误，不触发守卫。
	for i := 1; i <= 2; i++ {
		res, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "missing.txt", "offset": float64(i), "limit": float64(1)})
		if res.Success {
			t.Fatalf("read missing file should fail (attempt %d)", i)
		}
		if err != nil {
			t.Fatalf("first %d failures should not return error, got: %v", i, err)
		}
	}
	// 第 3 次：连续失败达阈值，返回 ErrLoopExit。
	res3, err := r.Dispatch(ctx, "ReadFile", map[string]any{"path": "missing.txt", "offset": float64(3), "limit": float64(1)})
	if res3.Success {
		t.Fatal("3rd consecutive failure should be blocked")
	}
	if !strings.Contains(res3.Error, "无效重试死循环") {
		t.Fatalf("expected guard message, got: %s", res3.Error)
	}
	if !errors.Is(err, ErrLoopExit) {
		t.Fatalf("expected ErrLoopExit, got: %v", err)
	}
	// 其他工具失败不共享计数。
	otherRes, otherErr := r.Dispatch(ctx, "ListDir", map[string]any{"path": "no-such-dir"})
	if otherRes.Success || otherErr != nil {
		t.Fatalf("other tool failure should be ordinary, res=%+v err=%v", otherRes, otherErr)
	}
}

// rejectTool 是返回指定失败分类的测试工具，用于验证循环守卫分级（TODO #32）。
type rejectTool struct {
	name string
	cat  string
	msg  string
}

func (t *rejectTool) Name() string        { return t.name }
func (t *rejectTool) Aliases() []string   { return nil }
func (t *rejectTool) Description() string { return "test tool" }
func (t *rejectTool) Execute(context.Context, map[string]any) *Result {
	return &Result{Tool: t.name, Error: t.msg, Category: t.cat}
}

// TestLoopGuard_ValidationRejections_SeparateCounter 校验拒绝单独计数（阈值 5）：
// 前 4 次不终止，第 5 次 ErrLoopExit 且文案明示"校验拒绝"；与执行失败互不共享计数。
func TestLoopGuard_ValidationRejections_SeparateCounter(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)
	ctx := WithSessionID(context.Background(), "s1")

	// 用 WriteSpec 缺 goal 触发校验拒绝（Category 已打 validation_rejected）。
	r.SetSharedMemory(newFakeSharedMemoryStore())
	for i := 1; i <= 4; i++ {
		res, err := r.Dispatch(ctx, "WriteSpec", map[string]any{"acceptance": []any{"a"}})
		if res.Success {
			t.Fatalf("WriteSpec missing goal should fail (attempt %d)", i)
		}
		if err != nil {
			t.Fatalf("validation rejection %d should not kill yet, got: %v", i, err)
		}
		if res.Category != ResultCategoryValidationRejected {
			t.Fatalf("expected validation_rejected category, got: %q", res.Category)
		}
	}
	// 第 5 次：校验拒绝达阈值，ErrLoopExit + 文案区分。
	res5, err := r.Dispatch(ctx, "WriteSpec", map[string]any{"acceptance": []any{"a"}})
	if res5.Success {
		t.Fatal("5th validation rejection should be blocked")
	}
	if !strings.Contains(res5.Error, "校验拒绝") {
		t.Fatalf("expected 校验拒绝 message, got: %s", res5.Error)
	}
	if !errors.Is(err, ErrLoopExit) {
		t.Fatalf("expected ErrLoopExit, got: %v", err)
	}

	// 校验拒绝计数不污染执行失败计数：另一个执行失败工具仍需连续 3 次才触发。
	exec := &rejectTool{name: "probe-exec", cat: ResultCategoryExecutionFailed, msg: "boom"}
	r.Register(exec)
	for i := 1; i <= 2; i++ {
		res, err := r.Dispatch(ctx, "probe-exec", nil)
		if res.Success || err != nil {
			t.Fatalf("exec failure %d should be ordinary: success=%v err=%v", i, res.Success, err)
		}
	}
	res3, err3 := r.Dispatch(ctx, "probe-exec", nil)
	if res3.Success || !errors.Is(err3, ErrLoopExit) {
		t.Fatalf("3rd exec failure should still kill: res=%+v err=%v", res3, err3)
	}
}

// TestLoopGuard_MetaCallSubAgentExempt MetaAgent 的 call_sub_agent 校验拒绝豁免连杀终止：
// 连续 6 次校验拒绝不 ErrLoopExit，错误文案保持原始校验提示（模型可继续纠偏）。
func TestLoopGuard_MetaCallSubAgentExempt(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)
	metaTool := &rejectTool{name: "call_sub_agent", cat: ResultCategoryValidationRejected, msg: "task too long: 5000 runes (max 2000)"}
	r.Register(metaTool)

	// meta 角色上下文（与 ReActAgent 注入一致）。
	ctx := WithRoleID(WithSessionID(context.Background(), "s1"), "meta")
	for i := 1; i <= 6; i++ {
		res, err := r.Dispatch(ctx, "call_sub_agent", map[string]any{"task": "x"})
		if res.Success {
			t.Fatalf("validation reject should fail (attempt %d)", i)
		}
		if err != nil {
			t.Fatalf("meta dispatch validation rejection %d must not kill, got: %v", i, err)
		}
		if !strings.Contains(res.Error, "task too long") {
			t.Fatalf("original validation message should be preserved, got: %q", res.Error)
		}
	}

	// 非 meta 角色（domain）的 call_sub_agent 校验拒绝不豁免：达 5 次仍终止。
	r2 := NewBuiltinRegistry(dir, nil, nil)
	r2.Register(&rejectTool{name: "call_sub_agent", cat: ResultCategoryValidationRejected, msg: "task too long"})
	ctx2 := WithRoleID(WithSessionID(context.Background(), "s2"), "domain")
	for i := 1; i <= 4; i++ {
		if res, err := r2.Dispatch(ctx2, "call_sub_agent", map[string]any{"task": "x"}); res.Success || err != nil {
			t.Fatalf("domain reject %d should be ordinary: success=%v err=%v", i, res.Success, err)
		}
	}
	res5, err5 := r2.Dispatch(ctx2, "call_sub_agent", map[string]any{"task": "x"})
	if res5.Success || !errors.Is(err5, ErrLoopExit) {
		t.Fatalf("domain call_sub_agent 5th validation rejection should kill: res=%+v err=%v", res5, err5)
	}
}

// TestLoopGuard_MetaExemptOnlyForDispatch 豁免仅限 meta 角色的派发工具：
// meta 角色下其他工具的校验拒绝仍计数（防豁免被滥用绕过守卫）。
func TestLoopGuard_MetaExemptOnlyForDispatch(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)
	r.Register(&rejectTool{name: "WriteSpec", cat: ResultCategoryValidationRejected, msg: "goal is required"})
	ctx := WithRoleID(WithSessionID(context.Background(), "s1"), "meta")
	for i := 1; i <= 5; i++ {
		res, err := r.Dispatch(ctx, "WriteSpec", map[string]any{"acceptance": []any{"a"}})
		if i < 5 {
			if res.Success || err != nil {
				t.Fatalf("reject %d should be ordinary: success=%v err=%v", i, res.Success, err)
			}
			continue
		}
		if res.Success || !errors.Is(err, ErrLoopExit) {
			t.Fatalf("meta non-dispatch validation rejection should still kill at 5: res=%+v err=%v", res, err)
		}
	}
}
