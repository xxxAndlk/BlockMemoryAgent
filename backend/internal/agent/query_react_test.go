package agent

import (
	"context"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// TestQueryBoard_TreeSynthesis_StatusMapping 验证 QueryKindBoard：
// 无 write_plan 看板时回退权威树合成，状态映射含 Idle 翻绿 / Unverified 黄 /
// Paused 阻塞 / Failed 红（任务 113/89 语义服务端同步）。
func TestQueryBoard_TreeSynthesis_StatusMapping(t *testing.T) {
	svc := newReactServiceForTest(nil, "")
	sess := svc.store.createSession("goal-x")
	tree := svc.TreeFor(sess.ID)

	now := time.Now()
	tree.Register(orchestrator.Node{ID: "domain-1", Role: "domain", Domain: "d1", Status: orchestrator.StatusIdle, Started: now})
	tree.Register(orchestrator.Node{ID: "domain-2", Role: "domain", Domain: "d2", Status: orchestrator.StatusUnverified, Started: now})
	tree.Register(orchestrator.Node{ID: "domain-3", Role: "domain", Domain: "d3", Status: orchestrator.StatusPaused, Started: now})
	tree.Register(orchestrator.Node{ID: "domain-4", Role: "domain", Domain: "d4", Status: orchestrator.StatusFailed, Started: now})
	tree.Register(orchestrator.Node{ID: "domain-5", Role: "domain", Domain: "d5", Status: orchestrator.StatusRunning, Started: now})

	res, err := svc.Query(context.Background(), sess.ID, Query{Kind: QueryKindBoard})
	if err != nil {
		t.Fatalf("Query board: %v", err)
	}
	snap, ok := res.Data.(*board.Snapshot)
	if !ok {
		t.Fatalf("board query Data 类型不符: %T", res.Data)
	}
	if len(snap.Tasks) != 5 {
		t.Fatalf("期望 5 条合成任务, got %d", len(snap.Tasks))
	}
	want := map[string]board.TaskStatus{
		"domain-1": board.TaskDone,        // Idle 翻绿（任务 113）
		"domain-2": board.TaskUnverified,  // 标黄不标红（任务 89）
		"domain-3": board.TaskBlocked,     // Paused 展示为阻塞
		"domain-4": board.TaskFailed,
		"domain-5": board.TaskInProgress,
	}
	for _, task := range snap.Tasks {
		if want[task.ID] != task.Status {
			t.Errorf("task %s status = %q, want %q", task.ID, task.Status, want[task.ID])
		}
	}
}

// TestQueryBoard_AuthoritativePlanWins 验证 write_plan 权威看板优先于树合成。
func TestQueryBoard_AuthoritativePlanWins(t *testing.T) {
	svc := newReactServiceForTest(nil, "")
	sess := svc.store.createSession("goal-y")
	svc.SetBoard(func(sessionID string) *board.TaskBoard {
		b := board.NewTaskBoard("topic-1", "计划目标")
		_ = b.SetPlan("计划目标", []board.PlanTask{{ID: "t1", Title: "权威任务", Domain: "d1"}})
		_ = b.Assign("t1", "domain-1")
		return b
	})

	res, err := svc.Query(context.Background(), sess.ID, Query{Kind: QueryKindBoard})
	if err != nil {
		t.Fatalf("Query board: %v", err)
	}
	snap, ok := res.Data.(*board.Snapshot)
	if !ok {
		t.Fatalf("board query Data 类型不符: %T", res.Data)
	}
	if len(snap.Tasks) != 1 || snap.Tasks[0].Title != "权威任务" {
		t.Fatalf("权威看板未生效: %+v", snap.Tasks)
	}
	if snap.Goal != "计划目标" {
		t.Errorf("Goal = %q, want 计划目标", snap.Goal)
	}
}

// TestQueryBoard_FiltersPrevRoundTerminalNodes 验证上一轮终态节点被过滤、
// 本轮节点保留（多轮会话不稀释看板）。
func TestQueryBoard_FiltersPrevRoundTerminalNodes(t *testing.T) {
	svc := newReactServiceForTest(nil, "")
	sess := svc.store.createSession("goal-z")
	// 注入一条用户消息作为本轮起点。
	svc.store.getSession(sess.ID).Messages = []Message{{Role: string(enums.ChatRoleUser), Content: "第二轮", Timestamp: time.Now()}}
	tree := svc.TreeFor(sess.ID)

	old := time.Now().Add(-time.Hour)
	tree.Register(orchestrator.Node{ID: "old-done", Role: "domain", Status: orchestrator.StatusDone, Started: old})
	tree.Register(orchestrator.Node{ID: "old-idle", Role: "domain", Status: orchestrator.StatusIdle, Started: old})
	tree.Register(orchestrator.Node{ID: "new-running", Role: "domain", Status: orchestrator.StatusRunning, Started: time.Now()})

	res, err := svc.Query(context.Background(), sess.ID, Query{Kind: QueryKindBoard})
	if err != nil {
		t.Fatalf("Query board: %v", err)
	}
	snap := res.Data.(*board.Snapshot)
	// 上一轮终态节点（old-done）被过滤；Idle 不在过滤名单（热驻跨轮保留供复用，
	// 与 tui/agent_tree_panel.go filterPrevRoundNodes 同构）；本轮节点保留。
	if len(snap.Tasks) != 2 {
		t.Fatalf("前轮过滤结果不符: %+v", snap.Tasks)
	}
	seen := map[string]bool{}
	for _, task := range snap.Tasks {
		seen[task.ID] = true
	}
	if seen["old-done"] {
		t.Errorf("上一轮 done 节点未被过滤")
	}
	if !seen["old-idle"] || !seen["new-running"] {
		t.Errorf("Idle/本轮节点不应被过滤: %+v", snap.Tasks)
	}
}

// TestQueryMetrics_TimeoutEventCounting 验证 QueryKindMetrics：
// pgStore 缺席时调用数/token 归零、超时数从 error 事件关键词计数。
func TestQueryMetrics_TimeoutEventCounting(t *testing.T) {
	svc := newReactServiceForTest(nil, "")
	sess := svc.store.createSession("goal-m")
	svc.store.addEvent(sess, eventkind.Error, "meta", "run: llm generate: context DeadlineExceeded", eventkind.Error, "", "", "", "context deadline exceeded", false)

	res, err := svc.Query(context.Background(), sess.ID, Query{Kind: QueryKindMetrics})
	if err != nil {
		t.Fatalf("Query metrics: %v", err)
	}
	m, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("metrics query Data 类型不符: %T", res.Data)
	}
	if m["calls"] != 0 {
		t.Errorf("calls = %v, want 0", m["calls"])
	}
	if m["timeouts"] != 1 {
		t.Errorf("timeouts = %v, want 1", m["timeouts"])
	}
}

// TestQueryWatchdogAndMailbox_EmptyCompat 验证退役的 watchdog 返回空决策、
// mailbox 在无消息/无邮箱依赖时返回空列表而非 nil Data。
func TestQueryWatchdogAndMailbox_EmptyCompat(t *testing.T) {
	svc := newReactServiceForTest(nil, "")
	sess := svc.store.createSession("goal-w")
	ctx := context.Background()

	res, err := svc.Query(ctx, sess.ID, Query{Kind: QueryKindWatchdog})
	if err != nil {
		t.Fatalf("Query watchdog: %v", err)
	}
	if decisions, ok := res.Data.([]any); !ok || len(decisions) != 0 {
		t.Fatalf("watchdog Data 应为空数组, got %#v", res.Data)
	}

	res, err = svc.Query(ctx, sess.ID, Query{Kind: QueryKindMailbox})
	if err != nil {
		t.Fatalf("Query mailbox: %v", err)
	}
	if msgs, ok := res.Data.([]*mailbox.Message); !ok || len(msgs) != 0 {
		t.Fatalf("mailbox Data 应为空列表, got %#v", res.Data)
	}
}
