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
