package subagent

// task_ledger_test.go 验证任务台账（2026-08-28 旧需求重派事故根治）：
// 派发记进行中、notify 记终态（完成/失败+原因/未验证）、渲染含状态与修改文件、
// 树协调取消/挂起、重启播种、Meta 直派过滤、容量淘汰。

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
)

const (
	ledgerTestSess   = "session-1"
	ledgerTestParent = "session-1" // 顶层 MetaAgent（roleIDFromAgentID 判 meta）
	ledgerTestChild  = "session-1/domain-1"
)

// TestTaskLedger_DoneFlow 派发→完成：渲染含 [完成]、领域、摘要、修改文件与规则头。
func TestTaskLedger_DoneFlow(t *testing.T) {
	l := newTaskLedger()
	l.RecordDispatch(ledgerTestSess, ledgerTestParent, ledgerTestChild, "游戏渲染逻辑", "塔绘制改三层拆分与格子路径", "")
	l.RecordTerminal(ledgerTestSess, ledgerTestParent, ledgerTestChild, "三层渲染与格子路径已落地，9 项验收全部通过", []string{"js/game.js", "js/ui.js"})

	out := l.Render(ledgerTestSess, nil)
	for _, want := range []string{"【任务台账】", "[完成]", "游戏渲染逻辑", "塔绘制改三层拆分", "js/game.js", "摘要:", "禁止重新派发"} {
		if !strings.Contains(out, want) {
			t.Fatalf("render missing %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "#1 [进行中]") {
		t.Fatalf("terminal entry should not render running, got:\n%s", out)
	}
}

// TestTaskLedger_FailureMarker 失败机读标记解析：[failure kind=timeout] → [失败·timeout] + 原因。
func TestTaskLedger_FailureMarker(t *testing.T) {
	l := newTaskLedger()
	l.RecordDispatch(ledgerTestSess, ledgerTestParent, ledgerTestChild, "游戏渲染逻辑", "路径对齐等 5 项", "")
	l.RecordTerminal(ledgerTestSess, ledgerTestParent, ledgerTestChild,
		"[failure kind=timeout retryable=false]\n子 Agent 墙钟预算耗尽（上限 2h0m0s），已被强制收口；产出未完成。",
		[]string{"js/game.js"})

	out := l.Render(ledgerTestSess, nil)
	for _, want := range []string{"[失败·timeout]", "原因:", "墙钟预算耗尽", "已改文件: js/game.js"} {
		if !strings.Contains(out, want) {
			t.Fatalf("render missing %q, got:\n%s", want, out)
		}
	}
}

// TestTaskLedger_UnverifiedMarker verify_missing/unverified 落三态黄态而非失败。
func TestTaskLedger_UnverifiedMarker(t *testing.T) {
	l := newTaskLedger()
	l.RecordDispatch(ledgerTestSess, ledgerTestParent, ledgerTestChild, "游戏渲染逻辑", "任务A", "")
	l.RecordTerminal(ledgerTestSess, ledgerTestParent, ledgerTestChild,
		"[failure kind=verify_missing retryable=false]\n产出已回传但缺少可执行验证证据。", nil)

	out := l.Render(ledgerTestSess, nil)
	if !strings.Contains(out, "[交付未验证·verify_missing]") {
		t.Fatalf("expected delivered-unverified label, got:\n%s", out)
	}
	if strings.Contains(out, "#1 [失败") {
		t.Fatalf("unverified must not render as failed, got:\n%s", out)
	}
}

// TestTaskLedger_RunningShown 仅派发未收口：渲染 [进行中]。
func TestTaskLedger_RunningShown(t *testing.T) {
	l := newTaskLedger()
	l.RecordDispatch(ledgerTestSess, ledgerTestParent, ledgerTestChild, "游戏美术素材", "生成 16 张贴图", "续建#2")

	out := l.Render(ledgerTestSess, nil)
	if !strings.Contains(out, "[进行中]") || !strings.Contains(out, "续建#2") || !strings.Contains(out, "已 ") {
		t.Fatalf("running entry render wrong, got:\n%s", out)
	}
}

// TestTaskLedger_ReconcileFromTree notify 未覆盖的路径（取消/挂起）渲染期用权威树兜底。
func TestTaskLedger_ReconcileFromTree(t *testing.T) {
	tree := orchestrator.NewTree(ledgerTestSess, nil)
	tree.Register(orchestrator.Node{ID: ledgerTestChild, ParentID: ledgerTestParent, Domain: "游戏渲染逻辑", Task: "任务A"})

	l := newTaskLedger()
	l.RecordDispatch(ledgerTestSess, ledgerTestParent, ledgerTestChild, "游戏渲染逻辑", "任务A", "")
	tree.Cancel(ledgerTestChild) // 用户取消：不走 notify

	out := l.Render(ledgerTestSess, tree)
	if !strings.Contains(out, "[已取消]") {
		t.Fatalf("cancelled node should reconcile ledger entry, got:\n%s", out)
	}
}

// TestTaskLedger_SeedFromTree 重启恢复：内存台账为空时从权威树（PG 恢复）播种；
// 重启前仍 Running 的节点视为中断（取消+备注）。
func TestTaskLedger_SeedFromTree(t *testing.T) {
	tree := orchestrator.NewTree(ledgerTestSess, nil)
	tree.Register(orchestrator.Node{ID: "session-1/domain-1", ParentID: ledgerTestParent, Domain: "游戏美术素材", Task: "生成贴图", Started: time.Now().Add(-2 * time.Hour)})
	tree.Finish("session-1/domain-1", "16/16 素材产出", nil)
	tree.Register(orchestrator.Node{ID: "session-1/domain-2", ParentID: ledgerTestParent, Domain: "游戏渲染逻辑", Task: "路径对齐", Started: time.Now().Add(-time.Hour)})
	// domain-2 保持 Running：模拟进程重启时仍在运行的失联节点。

	l := newTaskLedger()
	out := l.Render(ledgerTestSess, tree)
	for _, want := range []string{"重启恢复", "[完成]", "游戏美术素材", "视为中断"} {
		if !strings.Contains(out, want) {
			t.Fatalf("seeded render missing %q, got:\n%s", want, out)
		}
	}
	// 播种后新派发追加而非重播种：seq 应接续。
	l.RecordDispatch(ledgerTestSess, ledgerTestParent, "session-1/domain-3", "游戏渲染逻辑", "续建任务", "续建#1")
	out = l.Render(ledgerTestSess, tree)
	if !strings.Contains(out, "#3 [进行中]") {
		t.Fatalf("post-seed dispatch should append with next seq, got:\n%s", out)
	}
}

// TestTaskLedger_MetaFilter 非 Meta 直派（domain 派叶子助手）不入账，防噪声。
func TestTaskLedger_MetaFilter(t *testing.T) {
	l := newTaskLedger()
	l.RecordDispatch(ledgerTestSess, "session-1/domain-1", "session-1/domain-1/leaf-1", "", "叶子任务", "")
	l.RecordTerminal(ledgerTestSess, "session-1/domain-1", "session-1/domain-1/leaf-1", "done", nil)
	if out := l.Render(ledgerTestSess, nil); out != "" {
		t.Fatalf("leaf dispatches must not enter meta-facing ledger, got:\n%s", out)
	}
}

// TestTaskLedger_Cap 超容量优先淘汰最旧终态条目，进行中保留。
func TestTaskLedger_Cap(t *testing.T) {
	l := newTaskLedger()
	for i := 0; i < ledgerMaxEntries+6; i++ {
		child := fmt.Sprintf("session-1/domain-%d", i+1)
		l.RecordDispatch(ledgerTestSess, ledgerTestParent, child, "领域", fmt.Sprintf("任务%d", i+1), "")
		l.RecordTerminal(ledgerTestSess, ledgerTestParent, child, "完成", nil)
	}
	l.RecordDispatch(ledgerTestSess, ledgerTestParent, "session-1/domain-x", "领域", "未收口任务", "")

	l.mu.Lock()
	total := len(l.bySess[ledgerTestSess])
	l.mu.Unlock()
	if total > ledgerMaxEntries {
		t.Fatalf("ledger should cap at %d entries, got %d", ledgerMaxEntries, total)
	}
	out := l.Render(ledgerTestSess, nil)
	if !strings.Contains(out, "更早") || !strings.Contains(out, "未收口任务") {
		t.Fatalf("render should note skipped old entries and keep the running one, got:\n%s", out)
	}
}

// TestTaskLedger_TerminalWithoutDispatch 台账启用晚于派发/条目被淘汰时，终态补记。
func TestTaskLedger_TerminalWithoutDispatch(t *testing.T) {
	l := newTaskLedger()
	l.RecordTerminal(ledgerTestSess, ledgerTestParent, ledgerTestChild, "补记摘要", nil)
	out := l.Render(ledgerTestSess, nil)
	if !strings.Contains(out, "补记") || !strings.Contains(out, "[完成]") {
		t.Fatalf("terminal without dispatch should be appended with note, got:\n%s", out)
	}
}

// TestTaskLedger_RenderEmpty 无条目且无树：空串（注入层零变化）。
func TestTaskLedger_RenderEmpty(t *testing.T) {
	l := newTaskLedger()
	if out := l.Render("session-none", nil); out != "" {
		t.Fatalf("empty ledger should render empty string, got %q", out)
	}
}

// TestDispatcher_TaskLedgerBrief 端到端接线：Dispatcher 派发登记 + notify 收口 + Brief 渲染；
// 未派发会话返回空串。
func TestDispatcher_TaskLedgerBrief(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, nil)
	if out := d.TaskLedgerBrief(ledgerTestSess); out != "" {
		t.Fatalf("no dispatch should render empty, got %q", out)
	}
	d.ledger.RecordDispatch(ledgerTestSess, ledgerTestParent, ledgerTestChild, "游戏渲染逻辑", "任务A", "")
	d.notify(ledgerTestParent, ledgerTestChild, "任务A 完成摘要", []string{"js/game.js"})
	out := d.TaskLedgerBrief(ledgerTestSess)
	if !strings.Contains(out, "[完成]") || !strings.Contains(out, "js/game.js") {
		t.Fatalf("brief should carry terminal status and files, got:\n%s", out)
	}
}
