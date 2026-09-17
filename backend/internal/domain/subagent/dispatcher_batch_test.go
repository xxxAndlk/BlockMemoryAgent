package subagent

// dispatcher_batch_test.go 验证 call_sub_agents 批量派发工具：
//   - 同波多任务一次原子派出：全部成功 + Output 汇总 subAgentID。
//   - 逐项校验：某项参数非法（缺 responsibility）整批拒绝。
//   - 逐项容错：某项派发失败（unknown role）不阻塞其他项，Output 汇总成功/失败清单。
//   - 批量上限：>6 项拒绝。
//
// 工具形态引导"同波一次派出"，对治 MetaAgent 分波串行（v6 实证第二波晚 24 分钟判负）。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/mailbox"
)

// unthrottledLoop 放开 pause env 的 TokenBudget=10（domain 子 Agent 会因
// est_tokens>budget 暂停而非完成，波聚合测试需要子 Agent 正常跑完回传）。
func unthrottledLoop(d *Dispatcher) {
	d.WithLoopConfigByRole(func(string) agent.LoopConfig {
		return agent.LoopConfig{TokenBudget: 100000, MaxIterations: 50}
	})
}

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

// stubWaveMerger 测试用整合纪要合成器：成功版返回固定文本，失败版恒报错。
type stubWaveMerger struct{ text string; err error }

func (m *stubWaveMerger) Merge(ctx context.Context, goal string, entries []DigestEntry) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	return m.text, nil
}

// dispatchThreeDomains 派 3 个 domain 的同波批量（波聚合标准入参）。
func dispatchThreeDomains(t *testing.T, toolsReg *tool.Registry) *tool.Result {
	t.Helper()
	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agents", map[string]any{
		"tasks": []any{
			map[string]any{"role_id": "domain", "task": "实现 config.js", "domain": "配置", "responsibility": "负责 config.js"},
			map[string]any{"role_id": "domain", "task": "实现 renderer.js", "domain": "渲染", "responsibility": "负责 renderer.js"},
			map[string]any{"role_id": "domain", "task": "实现 audio.js", "domain": "音效", "responsibility": "负责 audio.js"},
		},
	})
	if err != nil {
		t.Fatalf("dispatch returned err: %v", err)
	}
	return res
}

// findDigestMsg 在父邮箱里找【整合纪要】消息；返回消息与是否找到。
func findDigestMsg(mb *mailbox.Mailbox) (*mailbox.Message, bool) {
	for _, m := range mb.Peek("s1") {
		if strings.Contains(m.Body, "【整合纪要】") {
			return m, true
		}
	}
	return nil, false
}

// TestCallSubAgents_WaveDigest 三领域同波：全部完成汇成一条【整合纪要】（无合成器，
// 回退逐领域拼接），父邮箱不再收到逐条子 Agent 完成消息。
func TestCallSubAgents_WaveDigest(t *testing.T) {
	d, _, mb, _, _, toolsReg := newPauseTestEnv(t, &mockProvider{text: "ok"})
	unthrottledLoop(d)
	res := dispatchThreeDomains(t, toolsReg)
	if !res.Success {
		t.Fatalf("3-domain wave should succeed, got %+v", res)
	}
	if !strings.Contains(res.Output, "【整合纪要】") {
		t.Fatalf("output should mention wave digest, got %q", res.Output)
	}
	waitForCond(t, "wave digest message", func() bool {
		_, ok := findDigestMsg(mb)
		return ok
	})
	msg, _ := findDigestMsg(mb)
	for _, domain := range []string{"配置", "渲染", "音效"} {
		if !strings.Contains(msg.Body, domain) {
			t.Fatalf("digest should cover domain %q, body: %q", domain, msg.Body)
		}
	}
	if !strings.Contains(msg.Body, "共 3 个领域：完成 3 / 失败 0") {
		t.Fatalf("digest should summarize 3 ok, body: %q", msg.Body)
	}
	// 不再有逐条直发（From=子 Agent id 的完成消息）。
	for _, m := range mb.Peek("s1") {
		if strings.HasPrefix(m.From, "s1/domain-") && strings.HasPrefix(m.Subject, "子 Agent 完成: ") {
			t.Fatalf("per-item completion should be suppressed under wave digest, got %+v", m)
		}
	}
}

// TestCallSubAgents_WaveDigestMerger 合成器优先：注入成功合成器时纪要用其产出；
// 合成器失败时回退逐领域拼接（fail-open）。
func TestCallSubAgents_WaveDigestMerger(t *testing.T) {
	t.Run("merger ok", func(t *testing.T) {
		d, _, mb, _, _, toolsReg := newPauseTestEnv(t, &mockProvider{text: "ok"})
		unthrottledLoop(d)
		d.WithSummaryMerger(&stubWaveMerger{text: "MERGED-DIGEST-BY-LLM"})
		res := dispatchThreeDomains(t, toolsReg)
		if !res.Success {
			t.Fatalf("wave should succeed, got %+v", res)
		}
		waitForCond(t, "merged digest", func() bool {
			m, ok := findDigestMsg(mb)
			return ok && strings.Contains(m.Body, "MERGED-DIGEST-BY-LLM")
		})
	})
	t.Run("merger fails fallback", func(t *testing.T) {
		d, _, mb, _, _, toolsReg := newPauseTestEnv(t, &mockProvider{text: "ok"})
		unthrottledLoop(d)
		d.WithSummaryMerger(&stubWaveMerger{err: errors.New("llm down")})
		res := dispatchThreeDomains(t, toolsReg)
		if !res.Success {
			t.Fatalf("wave should succeed, got %+v", res)
		}
		waitForCond(t, "fallback digest", func() bool {
			m, ok := findDigestMsg(mb)
			return ok && strings.Contains(m.Body, "渲染") && strings.Contains(m.Body, "共 3 个领域")
		})
	})
}

// TestCallSubAgents_WaveDigestDisabled batch_digest_enabled=false：回退逐条回传，
// 不产生【整合纪要】。
func TestCallSubAgents_WaveDigestDisabled(t *testing.T) {
	d, _, mb, _, _, toolsReg := newPauseTestEnv(t, &mockProvider{text: "ok"})
	unthrottledLoop(d)
	d.WithBatchDigest(false)
	res := dispatchThreeDomains(t, toolsReg)
	if !res.Success {
		t.Fatalf("wave should succeed, got %+v", res)
	}
	if strings.Contains(res.Output, "【整合纪要】") {
		t.Fatalf("disabled digest should not mention digest, got %q", res.Output)
	}
	// 3 条逐条完成消息（From=s1/domain-N）。
	waitForCond(t, "3 per-item completions", func() bool {
		count := 0
		for _, m := range mb.Peek("s1") {
			if strings.HasPrefix(m.From, "s1/domain-") && strings.HasPrefix(m.Subject, "子 Agent 完成: ") {
				count++
			}
		}
		return count == 3
	})
	if _, ok := findDigestMsg(mb); ok {
		t.Fatal("digest should be absent when batch digest disabled")
	}
}

// TestCallSubAgents_WaveAbandonOnLowOk domainCount>=2 但成功派出的 domain <2：
// 放弃聚合回退逐条直发（防单条纪要无意义），无【整合纪要】产生。
func TestCallSubAgents_WaveAbandonOnLowOk(t *testing.T) {
	d, _, mb, _, tr, toolsReg := newPauseTestEnv(t, &mockProvider{text: "ok"})
	unthrottledLoop(d)
	// 预注册活跃同名节点使「配置」派发被拒（跨调用活跃同名闸门）。
	tr.Register(orchestrator.Node{ID: "s1/domain-90", ParentID: "s1", Role: "domain", Domain: "配置", Status: orchestrator.StatusRunning})
	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agents", map[string]any{
		"tasks": []any{
			map[string]any{"role_id": "domain", "task": "实现 config.js", "domain": "配置", "responsibility": "负责 config.js"},
			map[string]any{"role_id": "domain", "task": "实现 renderer.js", "domain": "渲染", "responsibility": "负责 renderer.js"},
		},
	})
	if err != nil {
		t.Fatalf("dispatch returned err: %v", err)
	}
	if res.Success || !strings.Contains(res.Output, "未派出 1 个") || !strings.Contains(res.Output, "同名活跃实例") {
		t.Fatalf("one rejected + one ok should summarize, got %+v", res)
	}
	// 「渲染」逐条直发到达父邮箱（聚合被放弃，flush 直发；From=领域名）。
	waitForCond(t, "abandoned-wave direct delivery", func() bool {
		for _, m := range mb.Peek("s1") {
			if m.From == "渲染" && m.Subject == "子 Agent 完成: 渲染" {
				return true
			}
		}
		return false
	})
	// 直发到达后短候再确认无纪要。
	time.Sleep(100 * time.Millisecond)
	if _, ok := findDigestMsg(mb); ok {
		t.Fatal("wave with <2 ok domains should not produce digest")
	}
}
