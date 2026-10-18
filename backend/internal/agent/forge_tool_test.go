package agent

// forge_tool_test.go 覆盖 tool_forge 的 agent 层收口：
//   - adapter 可见性：锻造工具仅属主 scope 可见——白名单非空、白名单为空、
//     无过滤配置三条路径都要过滤（白名单空 = 角色见全部内置，但锻造工具仍须
//     按属主过滤，这是关键陷阱）；
//   - adapter.Dispatch 向 ctx 注入 scope：ctx 未携带 agentID 时以 adapter.scope
//     补齐，锻造登记属主与可见性过滤同口径；
//   - finalizeSession 清理：会话终态摘除本会话锻造工具，Has==false。

import (
	"context"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// forgeCall 组装一次 tool_forge 调用（无参数 sh 脚本，最小合法入参）。
func forgeCall(name string) ToolCall {
	return ToolCall{Name: "tool_forge", Input: map[string]any{
		"name":        name,
		"description": "锻造工具 " + name,
		"language":    "sh",
		"code":        "#!/bin/sh\necho ok",
	}}
}

// TestAdapterForgedVisibilityWhitelistBranches 三条 Schema() 路径都按属主过滤：
// 白名单非空 / 白名单为空（角色见全部内置，关键陷阱分支）/ 无过滤配置。
func TestAdapterForgedVisibilityWhitelistBranches(t *testing.T) {
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	owner := NewToolRegistryAdapterForRole(reg, "session-1", []string{"ReadFile", "tool_forge"}, "meta", nil)
	// ctx 不带 agentID：Dispatch 应以 adapter.scope 注入，锻造属主 = session-1。
	if _, err := owner.Dispatch(context.Background(), forgeCall("mine")); err != nil {
		t.Fatalf("tool_forge 调用失败: %v", err)
	}
	if owner, ok := reg.ForgedScope("forged__mine"); !ok || owner != "session-1" {
		t.Fatalf("锻造属主应为 session-1: owner=%q ok=%v", owner, ok)
	}

	// 1) 白名单非空：属主可见；其他 scope 不可见（即使白名单含 tool_forge）。
	otherWL := NewToolRegistryAdapterForRole(reg, "session-2", []string{"ReadFile", "tool_forge"}, "meta", nil)
	if !schemaNames(owner.Schema())["forged__mine"] {
		t.Fatal("白名单非空：属主 scope 应可见锻造工具")
	}
	if schemaNames(otherWL.Schema())["forged__mine"] {
		t.Fatal("白名单非空：其他 scope 不应可见锻造工具")
	}

	// 2) 白名单为空：角色见全部内置，但锻造工具仍须按属主过滤（关键陷阱）。
	ownerNoWL := NewToolRegistryAdapterForRole(reg, "session-1", nil, "meta", nil)
	otherNoWL := NewToolRegistryAdapterForRole(reg, "session-2", nil, "domain", nil)
	if !schemaNames(ownerNoWL.Schema())["forged__mine"] {
		t.Fatal("白名单为空：属主 scope 应可见锻造工具")
	}
	if schemaNames(otherNoWL.Schema())["forged__mine"] {
		t.Fatal("白名单为空：其他 scope 不应可见锻造工具")
	}
	// 内置工具在白名单为空时照常全量可见（非锻造逻辑零行为变化）。
	if !schemaNames(otherNoWL.Schema())["ReadFile"] {
		t.Fatal("白名单为空：内置工具应全量可见")
	}

	// 3) 无过滤配置（NewToolRegistryAdapter）：scope 为空，锻造工具不可见。
	plain := NewToolRegistryAdapter(reg)
	if schemaNames(plain.Schema())["forged__mine"] {
		t.Fatal("无 scope 的裸 adapter 不应暴露锻造工具")
	}
}

// TestAdapterForgedVisibilityWithVisibilityCallback pluginVisibility 非 nil 时
// 锻造过滤与插件过滤并存不互相干扰。
func TestAdapterForgedVisibilityWithVisibilityCallback(t *testing.T) {
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	reg.Register(&visTool{name: "web_search"})
	vis := func(roleID, name string) (bool, bool) { return name == "web_search", roleID == "meta" }
	owner := NewToolRegistryAdapterForRole(reg, "session-1", []string{"ReadFile", "tool_forge"}, "meta", vis)
	if _, err := owner.Dispatch(context.Background(), forgeCall("cb")); err != nil {
		t.Fatalf("tool_forge 调用失败: %v", err)
	}

	names := schemaNames(owner.Schema())
	if !names["forged__cb"] {
		t.Fatal("属主 scope 应可见锻造工具")
	}
	if names["web_search"] {
		t.Fatal("未挂载插件工具不应可见（TODO #52 语义不变）")
	}
	sibling := NewToolRegistryAdapterForRole(reg, "session-1/domain-1", []string{"ReadFile", "tool_forge"}, "meta", vis)
	if schemaNames(sibling.Schema())["forged__cb"] {
		t.Fatal("子 Agent scope 不应可见父会话锻造工具")
	}
}

// TestFinalizeSessionUnregistersForgedTools 会话终态 finalizeSession 摘除本会话
// 锻造工具（脚本文件留档不删）；暂停态不摘，续跑仍可用。
func TestFinalizeSessionUnregistersForgedTools(t *testing.T) {
	svc := newReactServiceForTest(nil, t.TempDir())
	sess := svc.store.createSession("test-forge", "")
	ctx := tool.WithAgentID(tool.WithSessionID(context.Background(), sess.ID), sess.ID)

	res, err := svc.toolRegistry.Dispatch(ctx, "tool_forge", map[string]any{
		"name": "sess-tool", "description": "会话锻造", "language": "sh", "code": "#!/bin/sh\necho hi",
	})
	if err != nil || res == nil || !res.Success {
		t.Fatalf("tool_forge 应成功: res=%+v err=%v", res, err)
	}
	if !svc.toolRegistry.Has("forged__sess-tool") {
		t.Fatal("锻造后应已注册")
	}

	// 终态（completed）：finalizeSession 摘除。
	sess.Status = enums.SessionStatusCompleted
	svc.finalizeSession(sess)
	if svc.toolRegistry.Has("forged__sess-tool") {
		t.Fatal("会话终态后锻造工具应被摘除")
	}
	if _, ok := svc.toolRegistry.ForgedScope("forged__sess-tool"); ok {
		t.Fatal("会话终态后锻造簿记应被清理")
	}

	// 暂停态（awaiting_clarify）：不摘，续跑仍可用。
	sess2 := svc.store.createSession("test-forge-pause", "")
	ctx2 := tool.WithAgentID(tool.WithSessionID(context.Background(), sess2.ID), sess2.ID)
	res3, err := svc.toolRegistry.Dispatch(ctx2, "tool_forge", map[string]any{
		"name": "pause-tool", "description": "暂停态锻造", "language": "sh", "code": "#!/bin/sh\necho hi",
	})
	if err != nil || res3 == nil || !res3.Success {
		t.Fatalf("暂停会话锻造应成功: res=%+v err=%v", res3, err)
	}
	sess2.Status = enums.SessionStatusAwaitingClarify
	svc.finalizeSession(sess2)
	if !svc.toolRegistry.Has("forged__pause-tool") {
		t.Fatal("暂停态 finalizeSession 不应摘除锻造工具（续跑仍可用）")
	}
	if _, ok := svc.toolRegistry.ForgedScope("forged__pause-tool"); !ok {
		t.Fatal("暂停态 finalizeSession 不应清理锻造簿记")
	}
}
