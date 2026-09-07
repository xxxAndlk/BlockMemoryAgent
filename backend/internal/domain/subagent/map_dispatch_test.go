package subagent

// map_dispatch_test.go 验证 map_sub_agents 同构批量派发工具（TODO 第七项⑤）：
//   - scout（spec_exempt）单波 32 项上限、全部派出。
//   - 重型角色单波 6 项上限、超限拒绝。
//   - 参数校验：缺 {{item}} 占位、空 items、非字符串 item、未知角色、不可调用角色。
//   - aggregate 聚合：N 项完成汇成单条父邮箱消息，按派发顺序排列。

import (
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// TestMapSubAgents_ScoutBatch32 验证 spec_exempt 角色单波 32 项派出与 33 项拒绝。
func TestMapSubAgents_ScoutBatch32(t *testing.T) {
	d, _, _, _, tr, toolsReg := newPauseTestEnv(t, &mockProvider{text: "ok"})
	if err := d.registry.Register(&types.RoleDefinition{
		ID: "scout", Type: enums.RoleTypeDynamic, CanBeCalled: true,
		SystemPrompt: "scout", SpecExempt: true,
	}); err != nil {
		t.Fatalf("register scout: %v", err)
	}
	items := make([]any, 32)
	for i := range items {
		items[i] = "符号" + string(rune('a'+i%26)) + string(rune('0'+i/26))
	}
	res, err := toolsReg.Dispatch(dispatchCtx(), "map_sub_agents", map[string]any{
		"role_id":       "scout",
		"task_template": "定位 {{item}} 的定义位置",
		"items":         items,
	})
	if err != nil {
		t.Fatalf("map dispatch returned err: %v", err)
	}
	if !res.Success {
		t.Fatalf("map dispatch should succeed, got %+v", res)
	}
	if !strings.Contains(res.Output, "已派出 32 项") {
		t.Fatalf("output should summarize 32 items, got %q", res.Output)
	}
	count := 0
	for _, n := range tr.Snapshot() {
		if n.ParentID == "s1" && n.Role == "scout" {
			count++
		}
	}
	if count != 32 {
		t.Fatalf("tree should have 32 scout children, got %d", count)
	}
	// 33 项拒绝。
	items33 := append(append([]any{}, items...), "extra")
	res2, _ := toolsReg.Dispatch(dispatchCtx(), "map_sub_agents", map[string]any{
		"role_id": "scout", "task_template": "定位 {{item}}", "items": items33,
	})
	if res2.Success || !strings.Contains(res2.Error, "上限 32") {
		t.Fatalf("33 scout items should be rejected, got %+v", res2)
	}
}

// TestMapSubAgents_HeavyLimit6 验证非 spec_exempt 角色单波 6 项上限（对齐 call_sub_agents maxBatch）。
func TestMapSubAgents_HeavyLimit6(t *testing.T) {
	_, _, _, _, _, toolsReg := newPauseTestEnv(t, &mockProvider{text: "ok"})
	items := make([]any, 7)
	for i := range items {
		items[i] = "文件"
	}
	res, _ := toolsReg.Dispatch(dispatchCtx(), "map_sub_agents", map[string]any{
		"role_id": "code_assistant", "task_template": "写 {{item}}", "items": items,
	})
	if res.Success || !strings.Contains(res.Error, "上限 6") {
		t.Fatalf("7 heavy items should be rejected, got %+v", res)
	}
}

// TestMapSubAgents_ArgValidation 验证参数校验：占位符/items 元素/未知角色/不可调用角色。
// 每用例独立环境：同 scope 连续 5 次校验拒绝会触发 LoopExit 守卫干扰断言。
func TestMapSubAgents_ArgValidation(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"missing placeholder", map[string]any{"role_id": "scout", "task_template": "无占位", "items": []any{"a"}}, "{{item}}"},
		{"empty items", map[string]any{"role_id": "scout", "task_template": "{{item}}", "items": []any{}}, "items 均必填"},
		{"non-string item", map[string]any{"role_id": "scout", "task_template": "{{item}}", "items": []any{42}}, "非空字符串"},
		{"unknown role", map[string]any{"role_id": "nope", "task_template": "{{item}}", "items": []any{"a"}}, "unknown role"},
		{"not callable", map[string]any{"role_id": "private_role", "task_template": "{{item}}", "items": []any{"a"}}, "cannot be called"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _, _, _, toolsReg := newPauseTestEnv(t, &mockProvider{text: "ok"})
			if err := d.registry.Register(&types.RoleDefinition{
				ID: "scout", Type: enums.RoleTypeDynamic, CanBeCalled: true,
				SystemPrompt: "scout", SpecExempt: true,
			}); err != nil {
				t.Fatalf("register scout: %v", err)
			}
			if err := d.registry.Register(&types.RoleDefinition{
				ID: "private_role", Type: enums.RoleTypeDynamic, CanBeCalled: false,
				SystemPrompt: "hidden",
			}); err != nil {
				t.Fatalf("register private_role: %v", err)
			}
			res, err := toolsReg.Dispatch(dispatchCtx(), "map_sub_agents", tc.args)
			if err != nil {
				t.Fatalf("dispatch err: %v", err)
			}
			if res.Success || !strings.Contains(res.Error, tc.want) {
				t.Fatalf("want error containing %q, got %+v", tc.want, res)
			}
		})
	}
}

// TestMapSubAgents_AggregateSingleMessage 验证 aggregate=true：两项完成汇成单条邮箱消息且按派发顺序排列。
func TestMapSubAgents_AggregateSingleMessage(t *testing.T) {
	d, _, mb, _, tr, toolsReg := newPauseTestEnv(t, &mockProvider{text: "ok"})
	if err := d.registry.Register(&types.RoleDefinition{
		ID: "scout", Type: enums.RoleTypeDynamic, CanBeCalled: true,
		SystemPrompt: "scout", SpecExempt: true,
	}); err != nil {
		t.Fatalf("register scout: %v", err)
	}
	res, err := toolsReg.Dispatch(dispatchCtx(), "map_sub_agents", map[string]any{
		"role_id":       "scout",
		"task_template": "定位 {{item}}",
		"items":         []any{"第一个符号", "第二个符号"},
		"aggregate":     true,
	})
	if err != nil {
		t.Fatalf("map dispatch returned err: %v", err)
	}
	if !res.Success || !strings.Contains(res.Output, "aggregate=true") {
		t.Fatalf("aggregate dispatch should succeed with batch note, got %+v", res)
	}
	// 等聚合消息单条送达父邮箱。
	var got []*mailbox.Message
	waitForCond(t, "aggregate message", func() bool {
		got = mb.Drain("s1")
		return len(got) == 1
	})
	body := got[0].Body
	if !strings.Contains(body, "聚合回传") {
		t.Fatalf("aggregate message should carry header, got %q", body)
	}
	// 按派发顺序：第一个符号在前。
	i0, i1 := strings.Index(body, "第一个符号"), strings.Index(body, "第二个符号")
	if i0 < 0 || i1 < 0 || i0 > i1 {
		t.Fatalf("aggregate message should list items in dispatch order, got %q", body)
	}
	if !strings.Contains(body, "完成 2 / 失败 0") {
		t.Fatalf("aggregate message should summarize counts, got %q", body)
	}
	// 树中两个 scout 节点。
	count := 0
	for _, n := range tr.Snapshot() {
		if n.Role == "scout" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("tree should have 2 scout children, got %d", count)
	}
}
