package subagent

// dispatcher_batch_test.go 验证 call_sub_agents 批量派发工具：
//   - 同波多任务一次原子派出：全部成功 + Output 汇总 subAgentID。
//   - 逐项校验：某项参数非法（缺 responsibility）整批拒绝。
//   - 逐项容错：某项派发失败（unknown role）不阻塞其他项，Output 汇总成功/失败清单。
//   - 批量上限：>6 项拒绝。
//
// 工具形态引导"同波一次派出"，对治 MetaAgent 分波串行（v6 实证第二波晚 24 分钟判负）。

import (
	"strings"
	"testing"
)

// TestCallSubAgents_BatchDispatch 验证同波批量派发：两个不同领域一次派出，均成功。
func TestCallSubAgents_BatchDispatch(t *testing.T) {
	d, _, _, _, tr, toolsReg := newPauseTestEnv(t, &mockProvider{text: "ok"})
	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agents", map[string]any{
		"tasks": []any{
			map[string]any{"role_id": "domain", "task": "实现 config.js", "domain": "配置", "responsibility": "负责 config.js"},
			map[string]any{"role_id": "domain", "task": "实现 renderer.js", "domain": "渲染", "responsibility": "负责 renderer.js"},
		},
	})
	if err != nil {
		t.Fatalf("batch dispatch returned err: %v", err)
	}
	if !res.Success {
		t.Fatalf("batch dispatch should succeed, got %+v", res)
	}
	// Output 汇总两个 subAgentID。
	if !strings.Contains(res.Output, "已并行派出 2 个子 Agent") {
		t.Fatalf("output should summarize 2 dispatched, got %q", res.Output)
	}
	if !strings.Contains(res.Output, "s1/domain-") {
		t.Fatalf("output should contain sub ids, got %q", res.Output)
	}
	// 两个节点都注册进树。
	nodes := tr.Snapshot()
	count := 0
	for _, n := range nodes {
		if n.ParentID == "s1" && n.Role == "domain" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("tree should have 2 domain children, got %d", count)
	}
	// 父未决计数 = 2（异步在飞或已完成都会最终归零，此处只校验 >=0 的 track 一致性）。
	if got := d.PendingChildren("s1"); got < 0 {
		t.Fatalf("pending should never be negative, got %d", got)
	}
}

// TestCallSubAgents_ItemValidation 验证逐项参数校验：缺 responsibility 的 domain 项整批拒绝。
func TestCallSubAgents_ItemValidation(t *testing.T) {
	_, _, _, _, _, toolsReg := newPauseTestEnv(t, &mockProvider{text: "ok"})
	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agents", map[string]any{
		"tasks": []any{
			map[string]any{"role_id": "domain", "task": "实现 config.js", "domain": "配置"},
		},
	})
	if err != nil {
		t.Fatalf("dispatch returned err: %v", err)
	}
	if res.Success || !strings.Contains(res.Error, "responsibility") {
		t.Fatalf("missing responsibility should be rejected, got %+v", res)
	}
	// tasks 缺失/为空拒绝。
	res2, _ := toolsReg.Dispatch(dispatchCtx(), "call_sub_agents", map[string]any{})
	if res2.Success || !strings.Contains(res2.Error, "tasks is required") {
		t.Fatalf("empty tasks should be rejected, got %+v", res2)
	}
	// 超批量上限拒绝。
	big := make([]any, 7)
	for i := range big {
		big[i] = map[string]any{"role_id": "code_assistant", "task": "t"}
	}
	res3, _ := toolsReg.Dispatch(dispatchCtx(), "call_sub_agents", map[string]any{"tasks": big})
	if res3.Success || !strings.Contains(res3.Error, "batch too large") {
		t.Fatalf("oversized batch should be rejected, got %+v", res3)
	}
}

// TestCallSubAgents_PartialFailure 验证逐项容错：unknown role 项失败不阻塞另一项派出。
func TestCallSubAgents_PartialFailure(t *testing.T) {
	_, _, _, _, _, toolsReg := newPauseTestEnv(t, &mockProvider{text: "ok"})
	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agents", map[string]any{
		"tasks": []any{
			map[string]any{"role_id": "code_assistant", "task": "写 config.js"},
			map[string]any{"role_id": "nonexistent_role", "task": "不存在"},
		},
	})
	if err != nil {
		t.Fatalf("dispatch returned err: %v", err)
	}
	if res.Success {
		t.Fatalf("partial failure should mark Success=false, got %+v", res)
	}
	if !strings.Contains(res.Output, "已并行派出 1 个子 Agent") || !strings.Contains(res.Output, "未派出 1 个") {
		t.Fatalf("output should summarize 1 ok + 1 failed, got %q", res.Output)
	}
	if !strings.Contains(res.Output, "unknown role") {
		t.Fatalf("failure detail should mention unknown role, got %q", res.Output)
	}
}
