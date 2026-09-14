package tool

// role_tool_gate_test.go 覆盖角色工具白名单硬门（dormant opt-in，config:
// role_tool_gate_enabled）：默认关闭零行为变化；开启后白名单外工具调用在
// Dispatch 层被拒（Agent 可见错误，不中止循环），白名单内/无 roleID/空白名单放行。

import (
	"context"
	"strings"
	"testing"
)

// gateStubTool 是直接返回成功的测试工具，用于观察硬门放行/拒绝语义。
type gateStubTool struct{ name string }

func (t *gateStubTool) Name() string        { return t.name }
func (t *gateStubTool) Aliases() []string   { return nil }
func (t *gateStubTool) Description() string { return "gate stub" }
func (t *gateStubTool) Execute(context.Context, map[string]any) *Result {
	return &Result{Tool: t.name, Success: true, Output: "ok"}
}

// TestRoleToolGate_DefaultOff 未注入 resolver（默认）时白名单语义不变：
// 任意已注册工具照常执行（Schema 软过滤不被本门影响）。
func TestRoleToolGate_DefaultOff(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.Register(&gateStubTool{name: "probe-gate"})
	ctx := WithRoleID(WithSessionID(context.Background(), "s1"), "code_assistant")

	res, err := r.Dispatch(ctx, "probe-gate", nil)
	if err != nil || !res.Success {
		t.Fatalf("gate off should pass through, got success=%v err=%v", res.Success, err)
	}
}

// TestRoleToolGate_DenyOutsideWhitelist 开启后白名单外工具被拒：
// 返回工具级错误（非 Go error，不中止 ReAct 循环），文案指明角色与工具名。
func TestRoleToolGate_DenyOutsideWhitelist(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.Register(&gateStubTool{name: "probe-gate"})
	r.SetRoleToolGateResolver(func(roleID string) []string {
		if roleID == "code_assistant" {
			return []string{"ReadFile", "WriteFile"}
		}
		return nil
	})
	ctx := WithRoleID(WithSessionID(context.Background(), "s1"), "code_assistant")

	res, err := r.Dispatch(ctx, "probe-gate", nil)
	if err != nil {
		t.Fatalf("deny should return tool-level error without killing loop, got err=%v", err)
	}
	if res.Success {
		t.Fatal("tool outside whitelist should be denied")
	}
	if !strings.Contains(res.Error, "code_assistant") || !strings.Contains(res.Error, "probe-gate") {
		t.Fatalf("deny message should name role and tool, got: %s", res.Error)
	}
}

// TestRoleToolGate_AllowInsideWhitelist 白名单内工具照常执行。
func TestRoleToolGate_AllowInsideWhitelist(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.Register(&gateStubTool{name: "probe-gate"})
	r.SetRoleToolGateResolver(func(string) []string { return []string{"probe-gate"} })
	ctx := WithRoleID(WithSessionID(context.Background(), "s1"), "code_assistant")

	res, err := r.Dispatch(ctx, "probe-gate", nil)
	if err != nil || !res.Success {
		t.Fatalf("whitelisted tool should execute, got success=%v err=%v", res.Success, err)
	}
}

// TestRoleToolGate_PassthroughEdgeCases 三类"不限制"边界全部放行：
// ctx 无 roleID（顶层/旧路径）、resolver 返回空（角色未配白名单）、未知角色。
func TestRoleToolGate_PassthroughEdgeCases(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.Register(&gateStubTool{name: "probe-gate"})
	r.SetRoleToolGateResolver(func(roleID string) []string {
		if roleID == "code_assistant" {
			return []string{"ReadFile"}
		}
		return nil
	})

	// ctx 无 roleID：放行。
	res, err := r.Dispatch(WithSessionID(context.Background(), "s1"), "probe-gate", nil)
	if err != nil || !res.Success {
		t.Fatalf("no roleID in ctx should pass through, got success=%v err=%v", res.Success, err)
	}
	// 未知角色（resolver 返 nil）：放行。
	res, err = r.Dispatch(WithRoleID(WithSessionID(context.Background(), "s1"), "unknown_role"), "probe-gate", nil)
	if err != nil || !res.Success {
		t.Fatalf("unknown role should pass through, got success=%v err=%v", res.Success, err)
	}
}
