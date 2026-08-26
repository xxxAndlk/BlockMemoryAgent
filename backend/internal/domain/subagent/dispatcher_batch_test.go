package subagent

// dispatcher_batch_test.go 验证 call_sub_agents 批量派发工具：
//   - 同波多任务一次原子派出：全部成功 + Output 汇总 subAgentID。
//   - 逐项校验：某项参数非法（缺 responsibility）整批拒绝。
//   - 逐项容错：某项派发失败（unknown role）不阻塞其他项，Output 汇总成功/失败清单。
//   - 批量上限：>6 项拒绝。
//
// 工具形态引导"同波一次派出"，对治 MetaAgent 分波串行（v6 实证第二波晚 24 分钟判负）。

import (
	"errors"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
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

// TestCallSubAgents_DupDomainRejected 验证同波批内 domain 重名：整批拒绝且树中无残留节点。
func TestCallSubAgents_DupDomainRejected(t *testing.T) {
	_, _, _, _, tr, toolsReg := newPauseTestEnv(t, &mockProvider{text: "ok"})
	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agents", map[string]any{
		"tasks": []any{
			map[string]any{"role_id": "domain", "task": "实现 fib.js", "domain": "CLI工具", "responsibility": "负责 fib.js"},
			map[string]any{"role_id": "domain", "task": "实现 prime.js", "domain": "CLI工具", "responsibility": "负责 prime.js"},
		},
	})
	if err != nil {
		t.Fatalf("dispatch returned err: %v", err)
	}
	if res.Success || !strings.Contains(res.Error, "重复 domain") {
		t.Fatalf("same-wave dup domain should be rejected, got %+v", res)
	}
	for _, n := range tr.Snapshot() {
		if n.Role == "domain" {
			t.Fatalf("rejected batch should leave no domain node, got %+v", n)
		}
	}
}

// TestCallSubAgent_CrossCallDupDomainRejected 验证跨调用查重（dispatchOne 活跃同名闸门）：
// 同一轮多次 call_sub_agent 单派同名 domain 时第二个被拦（批内查重管不到跨调用）；
// 同名节点终结（Done/Failed/Cancelled）后重派放行（保留打捞/新任务路径）。
func TestCallSubAgent_CrossCallDupDomainRejected(t *testing.T) {
	_, _, _, _, tr, toolsReg := newPauseTestEnv(t, &mockProvider{text: "ok"})
	dispatch := func(domain string) *tool.Result {
		res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
			"role_id": "domain", "task": "实现某个文件", "domain": domain, "responsibility": "负责该文件",
		})
		if err != nil {
			t.Fatalf("dispatch returned err: %v", err)
		}
		return res
	}
	// 手工注册活跃同名节点（避免 mock 子 Agent 秒完成带来的状态竞态）。
	tr.Register(orchestrator.Node{ID: "s1/domain-90", ParentID: "s1", Role: "domain", Domain: "CLI工具", Status: orchestrator.StatusRunning})
	if res := dispatch("CLI工具"); res.Success || !strings.Contains(res.Error, "同名活跃实例") {
		t.Fatalf("active same-name domain should be rejected, got %+v", res)
	}
	// 不同名放行。
	if res := dispatch("数学工具"); !res.Success {
		t.Fatalf("distinct domain should pass, got %+v", res)
	}
	// 同名节点转终态后重派放行：Failed（打捞重派）与 Done（完结后新任务）各验一次。
	tr.Finish("s1/domain-90", "failed", errors.New("mock failure"))
	if res := dispatch("CLI工具"); !res.Success {
		t.Fatalf("re-dispatch after Failed should pass, got %+v", res)
	}
	// 上一次成功派发的节点可能仍在跑或已暂停（mock 行为不定）：把所有非终态同名节点
	// 置为 Done 再派，验证"终态后同名重派放行"。
	for _, n := range tr.Snapshot() {
		if n.ParentID == "s1" && n.Role == "domain" && n.Domain == "CLI工具" &&
			n.Status != orchestrator.StatusDone && n.Status != orchestrator.StatusFailed && n.Status != orchestrator.StatusCancelled {
			tr.Finish(n.ID, "done", nil)
		}
	}
	if res := dispatch("CLI工具"); !res.Success {
		t.Fatalf("re-dispatch after Done should pass, got %+v", res)
	}
}

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
	// 插入一次成功派发，重置同一工具的连续失败计数（连续失败 ×3 会触发 LoopExit 守卫终止）。
	resOk, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agents", map[string]any{
		"tasks": []any{
			map[string]any{"role_id": "code_assistant", "task": "写 config.js"},
		},
	})
	if err != nil || !resOk.Success {
		t.Fatalf("valid batch should succeed, err=%v res=%+v", err, resOk)
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
