package tool

// loop_guard_test.go 验证三层循环守卫剩余两层（TODO #20 第一层）命中时返回包装
// ErrLoopExit 哨兵的非 nil error：连读死循环（maxConsecutiveSameRead）与校验拒绝超限
// （maxConsecutiveValidationRejections）。探索预算与连杀指纹已退役（TODO #44），不再有守卫。

import (
	"context"
	"errors"
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

	// 执行失败不再触发任何守卫（连杀指纹已退役，TODO #44）：连续失败仅返回工具错误。
	exec := &rejectTool{name: "probe-exec", cat: ResultCategoryExecutionFailed, msg: "boom"}
	r.Register(exec)
	for i := 1; i <= 5; i++ {
		res, err := r.Dispatch(ctx, "probe-exec", nil)
		if res.Success || err != nil {
			t.Fatalf("exec failure %d should be ordinary after retirement: success=%v err=%v", i, res.Success, err)
		}
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

// scriptedTool 按预设步骤依次返回结果（越界重复最后一步），模拟"拒绝→纠正→成功"节奏。
type scriptedTool struct {
	name  string
	steps []Result
	calls int
}

func (t *scriptedTool) Name() string        { return t.name }
func (t *scriptedTool) Aliases() []string   { return nil }
func (t *scriptedTool) Description() string { return "test tool" }
func (t *scriptedTool) Execute(context.Context, map[string]any) *Result {
	i := t.calls
	if i >= len(t.steps) {
		i = len(t.steps) - 1
	}
	t.calls++
	res := t.steps[i]
	return &res
}

// TestLoopGuard_ValidationCountResetsOnSuccess 成功即清零（2026-09-09 事故回归）：
// 4 次拒绝 → 成功 1 次（模型已补齐前置条件）→ 后续拒绝计数从 1 重计；
// 旧实现 reset 无调用点，成功后的第 1 次拒绝即累计 5 次误杀。
func TestLoopGuard_ValidationCountResetsOnSuccess(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)
	st := &scriptedTool{name: "WriteSpec", steps: []Result{
		{Tool: "WriteSpec", Error: "goal/acceptance 命中还原类诉求但 baseline 为空", Category: ResultCategoryValidationRejected},
		{Tool: "WriteSpec", Error: "goal/acceptance 命中还原类诉求但 baseline 为空", Category: ResultCategoryValidationRejected},
		{Tool: "WriteSpec", Error: "goal/acceptance 命中还原类诉求但 baseline 为空", Category: ResultCategoryValidationRejected},
		{Tool: "WriteSpec", Error: "goal/acceptance 命中还原类诉求但 baseline 为空", Category: ResultCategoryValidationRejected},
		{Tool: "WriteSpec", Success: true, Output: "spec written (key=spec:wave1)"},
		{Tool: "WriteSpec", Error: "goal is required", Category: ResultCategoryValidationRejected},
	}}
	r.Register(st)
	ctx := WithSessionID(context.Background(), "s1")

	// 前 4 次拒绝：普通校验拒绝，不终止。
	for i := 1; i <= 4; i++ {
		res, err := r.Dispatch(ctx, "WriteSpec", map[string]any{"acceptance": []any{"a"}})
		if res.Success || err != nil {
			t.Fatalf("reject %d should be ordinary: success=%v err=%v", i, res.Success, err)
		}
	}
	// 第 5 步成功：计数必须清零。
	if res, err := r.Dispatch(ctx, "WriteSpec", map[string]any{"acceptance": []any{"a"}}); !res.Success || err != nil {
		t.Fatalf("5th call should succeed: success=%v err=%v", res.Success, err)
	}
	// 成功后的第 1 次拒绝：连续语义下计数=1，不得终止（旧实现此处累计 5 次直接 ErrLoopExit）。
	res, err := r.Dispatch(ctx, "WriteSpec", map[string]any{"acceptance": []any{"a"}})
	if res.Success || err != nil {
		t.Fatalf("post-success reject must be ordinary after reset, got err=%v", err)
	}
	if !strings.Contains(res.Error, "goal is required") {
		t.Fatalf("original validation error must surface, got: %s", res.Error)
	}
}

// TestLoopGuard_ValidationCountScopeIsolated 计数按 scope 隔离（2026-09-09 事故回归）：
// 会话 A 的历史校验拒绝不得继承到会话 B——B 的首次拒绝从 1 起计。
func TestLoopGuard_ValidationCountScopeIsolated(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)
	r.Register(&rejectTool{name: "probe", cat: ResultCategoryValidationRejected, msg: "bad params"})
	ctxA := WithSessionID(context.Background(), "sA")
	ctxB := WithSessionID(context.Background(), "sB")

	for i := 1; i <= 4; i++ {
		if res, err := r.Dispatch(ctxA, "probe", nil); res.Success || err != nil {
			t.Fatalf("session A reject %d should be ordinary: err=%v", i, err)
		}
	}
	// 会话 B 首次拒绝：不得继承 A 的 4 次历史（旧实现按工具名全局计数，此处即误杀）。
	if res, err := r.Dispatch(ctxB, "probe", nil); res.Success || err != nil {
		t.Fatalf("session B first rejection must not inherit session A count, got err=%v", err)
	}
	// 会话 A 第 5 次拒绝：本会话连续达阈值，正常终止。
	if _, err := r.Dispatch(ctxA, "probe", nil); !errors.Is(err, ErrLoopExit) {
		t.Fatalf("session A 5th consecutive rejection should kill, err=%v", err)
	}
}

// TestLoopGuard_ValidationLoopExitKeepsOriginalError LoopExit 文案保留原始校验错误
//（2026-09-09 事故：守卫消息整条覆盖 result.Error，真实拒绝原因在日志/事件流丢失）。
func TestLoopGuard_ValidationLoopExitKeepsOriginalError(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)
	r.Register(&rejectTool{name: "probe", cat: ResultCategoryValidationRejected, msg: "baseline 引用文件不存在: ref/img.png"})
	ctx := WithSessionID(context.Background(), "s1")
	var res *Result
	var err error
	for i := 1; i <= 5; i++ {
		res, err = r.Dispatch(ctx, "probe", nil)
	}
	if !errors.Is(err, ErrLoopExit) {
		t.Fatalf("expected ErrLoopExit at 5th, got: %v", err)
	}
	if !strings.Contains(res.Error, "校验拒绝") || !strings.Contains(res.Error, "baseline 引用文件不存在") {
		t.Fatalf("LoopExit result must keep guard message AND original error, got: %s", res.Error)
	}
}
