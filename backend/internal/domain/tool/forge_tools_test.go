package tool

// forge_tools_test.go 覆盖 tool_forge 机制：
//   - 参数校验（非法 name/language/schema（非 object）/超长 description/code → validation_rejected）；
//   - 成功锻造：脚本落盘 .bma/forged/、RegisterForged 簿记、Schema() 可见；
//   - 同名再锻造：同 scope 覆盖、异 scope 拒绝；
//   - 单 scope 锻造数量上限（8 个）；
//   - forged 工具执行：schema 校验失败 → validation_rejected（指明字段）；
//     成功路径用 sh 脚本真实执行（走 runCommand 既有 git-bash 链路），断言 Output 含传入 JSON；
//   - UnregisterForgedByScope 清理：摘除后 Has==false。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forgeTestCtx 构造携带会话/agent 上下文的 ctx（scopeKeyFromCtx = agentID 口径）。
func forgeTestCtx(sessionID, agentID string) context.Context {
	return WithAgentID(WithSessionID(context.Background(), sessionID), agentID)
}

// forgeArgs 组装一次 tool_forge 入参；schema 为 nil 时缺省（无参数工具）。
func forgeArgs(name, language string, schema any, code string) map[string]any {
	return map[string]any{
		"name":        name,
		"description": "测试锻造工具 " + name,
		"language":    language,
		"schema":      schema,
		"code":        code,
	}
}

// TestForgeTool_ValidationRejected 逐字段校验：全部返回 validation_rejected。
func TestForgeTool_ValidationRejected(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	ctx := forgeTestCtx("sess-1", "agent-1")
	longDesc := strings.Repeat("字", maxForgeDescriptionRunes+1)
	longCodeLines := strings.Repeat("echo x\n", maxForgeCodeLines) // 501 行
	longCodeBytes := strings.Repeat("x", maxForgeCodeBytes+1)

	cases := []struct {
		name string
		args map[string]any
		want string // 错误须包含的子串
	}{
		{"name 大写", forgeArgs("Bad-Name", "sh", nil, "echo hi"), "name 非法"},
		{"name 数字开头", forgeArgs("1abc", "sh", nil, "echo hi"), "name 非法"},
		{"name 超长", forgeArgs(strings.Repeat("a", 49), "sh", nil, "echo hi"), "name 非法"},
		{"name 空", forgeArgs("", "sh", nil, "echo hi"), "name 非法"},
		{"description 空", map[string]any{"name": "ok-x", "description": "  ", "language": "sh", "code": "echo hi"}, "description 必填"},
		{"description 超长", map[string]any{"name": "ok-x", "description": longDesc, "language": "sh", "code": "echo hi"}, "description 超长"},
		{"language 非法", forgeArgs("ok-x", "ts", nil, "echo hi"), "language 非法"},
		{"language 空", forgeArgs("ok-x", "", nil, "echo hi"), "language 非法"},
		{"code 空", forgeArgs("ok-x", "sh", nil, "   "), "code 必填"},
		{"code 超行数", forgeArgs("ok-x", "sh", nil, longCodeLines), "code 超长"},
		{"code 超字节", forgeArgs("ok-x", "sh", nil, longCodeBytes), "code 超大"},
		{"schema 非 object", forgeArgs("ok-x", "sh", map[string]any{"type": "string"}, "echo hi"), "schema 必须为 object"},
		{"schema 无类型", forgeArgs("ok-x", "sh", map[string]any{"properties": map[string]any{}}, "echo hi"), "schema 必须为 object"},
		{"schema 非法结构", forgeArgs("ok-x", "sh", map[string]any{"type": 42}, "echo hi"), "schema 非法"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			res, _ := r.Dispatch(ctx, "tool_forge", tt.args)
			if res == nil || res.Success {
				t.Fatalf("应拒绝: %+v", res)
			}
			if res.Category != ResultCategoryValidationRejected {
				t.Fatalf("Category 应为 validation_rejected，got %q", res.Category)
			}
			if !strings.Contains(res.Error, tt.want) {
				t.Fatalf("错误应含 %q，got %q", tt.want, res.Error)
			}
		})
	}
}

// TestForgeTool_Success 成功锻造：脚本落盘、簿记正确、Schema() 可见（描述+入参 schema）。
func TestForgeTool_Success(t *testing.T) {
	workDir := t.TempDir()
	r := NewBuiltinRegistry(workDir, nil, nil)
	ctx := forgeTestCtx("sess-1", "agent-1")
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"who": map[string]any{"type": "string"}},
		"required":   []any{"who"},
	}
	res, _ := r.Dispatch(ctx, "tool_forge", forgeArgs("hello", "sh", schema, "#!/bin/sh\necho \"got:$1\""))
	if res == nil || !res.Success {
		t.Fatalf("锻造应成功: %+v", res)
	}
	if !strings.Contains(res.Output, "forged__hello") {
		t.Fatalf("成功回执应含注册名: %q", res.Output)
	}

	// 脚本落盘 <workDir>/.bma/forged/hello.sh，内容即提交代码。
	script := filepath.Join(workDir, ".bma", "forged", "hello.sh")
	b, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("脚本应落盘: %v", err)
	}
	if !strings.Contains(string(b), "got:$1") {
		t.Fatalf("脚本内容不符: %q", b)
	}

	// 簿记：属主 = agentID（scopeKeyFromCtx 口径），Has 命中。
	owner, ok := r.ForgedScope("forged__hello")
	if !ok || owner != "agent-1" {
		t.Fatalf("簿记属主不符: owner=%q ok=%v", owner, ok)
	}
	if !r.Has("forged__hello") {
		t.Fatal("Has(forged__hello) 应为 true")
	}

	// Schema() 兜底块暴露：名字/描述（含用户描述与锻造后缀）/入参 schema。
	var found bool
	for _, tt := range r.Schema() {
		if tt.Name() != "forged__hello" {
			continue
		}
		found = true
		if !strings.Contains(tt.Description(), "测试锻造工具 hello") || !strings.Contains(tt.Description(), ".bma/forged/hello.sh") {
			t.Fatalf("描述应含用户描述与落盘后缀: %q", tt.Description())
		}
		s := tt.InputSchema()
		if s == nil || s.Type != "object" || s.Properties["who"] == nil || s.Properties["who"].Type != "string" {
			t.Fatalf("入参 schema 透传不符: %+v", s)
		}
	}
	if !found {
		t.Fatal("Schema() 应暴露 forged__hello")
	}
}

// TestForgeTool_Reforge 同名再锻造：同 scope = 覆盖更新；异 scope = 拒绝提示改名。
func TestForgeTool_Reforge(t *testing.T) {
	workDir := t.TempDir()
	r := NewBuiltinRegistry(workDir, nil, nil)
	ctxA := forgeTestCtx("sess-1", "agent-1")
	ctxB := forgeTestCtx("sess-2", "agent-2")

	res, _ := r.Dispatch(ctxA, "tool_forge", forgeArgs("dup", "sh", nil, "echo v1"))
	if res == nil || !res.Success {
		t.Fatalf("首次锻造应成功: %+v", res)
	}

	// 同 scope 覆盖：重写脚本 + 重登记成功。
	res, _ = r.Dispatch(ctxA, "tool_forge", forgeArgs("dup", "sh", nil, "echo v2"))
	if res == nil || !res.Success {
		t.Fatalf("同 scope 再锻造应覆盖成功: %+v", res)
	}
	b, _ := os.ReadFile(filepath.Join(workDir, ".bma", "forged", "dup.sh"))
	if !strings.Contains(string(b), "echo v2") {
		t.Fatalf("覆盖后脚本应更新: %q", b)
	}
	if n := r.ForgedScopeOwnedByCount("agent-1"); n != 1 {
		t.Fatalf("覆盖不应新增名额: count=%d", n)
	}

	// 异 scope 拒绝：名字被其他会话占用。
	res, _ = r.Dispatch(ctxB, "tool_forge", forgeArgs("dup", "sh", nil, "echo v3"))
	if res == nil || res.Success || res.Category != ResultCategoryValidationRejected {
		t.Fatalf("异 scope 再锻造应 validation_rejected: %+v", res)
	}
	if !strings.Contains(res.Error, "占用") {
		t.Fatalf("拒绝文案应提示占用: %q", res.Error)
	}
}

// TestForgeTool_MaxPerSession 单 scope 锻造上限 8 个：第 9 个新名字被拒，覆盖不受限。
func TestForgeTool_MaxPerSession(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	ctx := forgeTestCtx("sess-1", "agent-1")
	for i := 1; i <= maxForgedPerSession; i++ {
		res, _ := r.Dispatch(ctx, "tool_forge", forgeArgs(fmt.Sprintf("cap-%d", i), "sh", nil, "echo ok"))
		if res == nil || !res.Success {
			t.Fatalf("第 %d 个锻造应成功: %+v", i, res)
		}
	}
	// 第 9 个新名字：超限拒绝。
	res, _ := r.Dispatch(ctx, "tool_forge", forgeArgs("cap-9", "sh", nil, "echo ok"))
	if res == nil || res.Success || res.Category != ResultCategoryValidationRejected {
		t.Fatalf("超限应 validation_rejected: %+v", res)
	}
	if !strings.Contains(res.Error, "上限") {
		t.Fatalf("拒绝文案应提示上限: %q", res.Error)
	}
	// 覆盖既有名字不受上限限制。
	res, _ = r.Dispatch(ctx, "tool_forge", forgeArgs("cap-1", "sh", nil, "echo newer"))
	if res == nil || !res.Success {
		t.Fatalf("覆盖更新不应受上限限制: %+v", res)
	}
}

// TestForgedTool_Execute 锻造工具真实执行：sh 脚本 echo $1 回传入参 JSON；
// schema 校验失败 → validation_rejected 且错误指明字段。
func TestForgedTool_Execute(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	ctx := forgeTestCtx("sess-1", "agent-1")
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"who": map[string]any{"type": "string"}},
		"required":   []any{"who"},
	}
	res, _ := r.Dispatch(ctx, "tool_forge", forgeArgs("greeter", "sh", schema, "#!/bin/sh\necho \"got:$1\""))
	if res == nil || !res.Success {
		t.Fatalf("锻造应成功: %+v", res)
	}

	// 成功路径：stdout 含传入 JSON（以 JSON 字符串作 argv[1] 传入）。
	res, _ = r.Dispatch(ctx, "forged__greeter", map[string]any{"who": "world"})
	if res == nil || !res.Success {
		t.Fatalf("执行应成功: %+v", res)
	}
	if !strings.Contains(res.Output, `got:{"who":"world"}`) {
		t.Fatalf("Output 应含传入 JSON，got %q", res.Output)
	}
	if res.Tool != "forged__greeter" {
		t.Fatalf("Result.Tool 应回填锻造名: %q", res.Tool)
	}

	// 缺必填字段：validation_rejected 且指明字段。
	res, _ = r.Dispatch(ctx, "forged__greeter", map[string]any{})
	if res == nil || res.Success || res.Category != ResultCategoryValidationRejected {
		t.Fatalf("缺必填字段应 validation_rejected: %+v", res)
	}
	if !strings.Contains(res.Error, `"who"`) {
		t.Fatalf("错误应指明字段 who: %q", res.Error)
	}

	// 类型不符：validation_rejected 且指明字段。
	res, _ = r.Dispatch(ctx, "forged__greeter", map[string]any{"who": 42})
	if res == nil || res.Success || res.Category != ResultCategoryValidationRejected {
		t.Fatalf("类型不符应 validation_rejected: %+v", res)
	}
	if !strings.Contains(res.Error, `"who"`) {
		t.Fatalf("错误应指明字段 who: %q", res.Error)
	}
}

// TestForgedTool_ExecuteNonZeroExit 非零退出：标失败且 stderr 附在输出里。
func TestForgedTool_ExecuteNonZeroExit(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	ctx := forgeTestCtx("sess-1", "agent-1")
	res, _ := r.Dispatch(ctx, "tool_forge", forgeArgs("boom", "sh", nil, "#!/bin/sh\necho out-line\necho err-line >&2\nexit 3"))
	if res == nil || !res.Success {
		t.Fatalf("锻造应成功: %+v", res)
	}
	res, _ = r.Dispatch(ctx, "forged__boom", map[string]any{})
	if res == nil || res.Success {
		t.Fatalf("非零退出应标失败: %+v", res)
	}
	if !strings.Contains(res.Output, "out-line") || !strings.Contains(res.Output, "err-line") {
		t.Fatalf("stdout/stderr 都应保留: %q", res.Output)
	}
}

// TestUnregisterForgedByScope 会话结束清理：按 scope 摘除全部锻造工具并清簿。
func TestUnregisterForgedByScope(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	ctxA := forgeTestCtx("sess-1", "agent-1")
	ctxB := forgeTestCtx("sess-2", "agent-2")
	for _, n := range []string{"a1", "a2"} {
		if res, _ := r.Dispatch(ctxA, "tool_forge", forgeArgs(n, "sh", nil, "echo ok")); !res.Success {
			t.Fatalf("锻造 %s 应成功: %+v", n, res)
		}
	}
	if res, _ := r.Dispatch(ctxB, "tool_forge", forgeArgs("b1", "sh", nil, "echo ok")); !res.Success {
		t.Fatalf("锻造 b1 应成功: %+v", res)
	}

	removed := r.UnregisterForgedByScope("agent-1")
	if len(removed) != 2 {
		t.Fatalf("应摘除 2 个，got %v", removed)
	}
	for _, name := range []string{"forged__a1", "forged__a2"} {
		if r.Has(name) {
			t.Fatalf("%s 应已摘除", name)
		}
		if _, ok := r.ForgedScope(name); ok {
			t.Fatalf("%s 簿记应已清", name)
		}
	}
	// 其他 scope 的锻造不受影响。
	if !r.Has("forged__b1") {
		t.Fatal("forged__b1 不应被误摘")
	}
	// 重复清理：空结果，无副作用。
	if got := r.UnregisterForgedByScope("agent-1"); len(got) != 0 {
		t.Fatalf("重复清理应为空: %v", got)
	}
}
