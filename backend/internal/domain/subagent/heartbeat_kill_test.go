package subagent

// heartbeat_kill_test.go 验证心跳 kill 增强（2026-08-27 fruit 任务实证链）：
//   - 热驻 domain 活动上报闭包 Load-per-call（Delete→rearm 换 atomic 后仍刷新当前条目）；
//   - kill 时级联停止树内全部 Running/Paused 后代；
//   - kill 回告含「终止前活动摘要」（最近自述 + 最近工具调用，cap 6 新版在前）。

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
)

// TestActivityReporterFn_LoadPerCall 验证闭包在 activity 条目被 Delete 后 rearm
// 重建（热驻 domain 每任务换新 atomic）场景下仍刷新当前条目——捕获指针的旧实现会
// 写进已废弃条目导致巡检误判假死。
func TestActivityReporterFn_LoadPerCall(t *testing.T) {
	d, _, _, _, _ := newSalvageTestEnv(t, &mockProvider{text: "ok"})
	id := "s1/domain-1"
	d.subMeta.Store(id, &subAgentMeta{parentID: "s1", sessionID: "s1"})

	fn := d.activityReporterFn(id)

	// 第一轮任务：rearm 建条目 A，触碰刷新。
	first := newAtomic(time.Now().UnixNano())
	d.activity.Store(id, first)
	fn()
	if first.Load() == 0 {
		t.Fatal("reporter should refresh entry of current task")
	}
	first.Store(1) // 打脏，验证后续写入不再落在此实例

	// enterIdle Delete + 下任务 rearm 重建条目 B。
	d.activity.Delete(id)
	second := newAtomic(0)
	d.activity.Store(id, second)
	fn()
	if second.Load() == 0 {
		t.Fatal("reporter must refresh the rebuilt (current) entry, not the stale one")
	}
	if first.Load() != 1 {
		t.Fatal("stale entry must not be touched by Load-per-call reporter")
	}
}

// TestKillStuckSubAgent_CascadeKillsDescendants 父 domain 被心跳杀时，
// 树内 Running/Paused 后代一并取消（cancel 被调 + activity 清理）。
func TestKillStuckSubAgent_CascadeKillsDescendants(t *testing.T) {
	d, _, tr, _, _ := newSalvageTestEnv(t, &mockProvider{text: "ok"})

	var leafCancel sync2Flag
	var pausedCancel sync2Flag

	domainID := "s1/domain-1"
	leafID := domainID + "/code_assistant-1"
	pausedID := domainID + "/code_assistant-2"

	tr.Register(orchestrator.Node{ID: domainID, ParentID: "s1", Role: "domain", Status: orchestrator.StatusRunning})
	tr.Register(orchestrator.Node{ID: leafID, ParentID: domainID, Role: "code_assistant", Status: orchestrator.StatusRunning})
	tr.Register(orchestrator.Node{ID: pausedID, ParentID: domainID, Role: "code_assistant", Status: orchestrator.StatusPaused})

	stale := time.Now().Add(-time.Hour).UnixNano()
	now := time.Now().UnixNano()
	d.activity.Store(domainID, newAtomic(stale))
	d.activity.Store(leafID, newAtomic(now))
	d.subMeta.Store(domainID, &subAgentMeta{parentID: "s1", sessionID: "s1"})
	d.subMeta.Store(leafID, &subAgentMeta{parentID: domainID, sessionID: "s1", cancel: leafCancel.set})
	d.subMeta.Store(pausedID, &subAgentMeta{parentID: domainID, sessionID: "s1", cancel: pausedCancel.set})

	d.killStuckSubAgent(domainID)

	if !leafCancel.called() {
		t.Fatal("running descendant should be cancelled by cascade")
	}
	if !pausedCancel.called() {
		t.Fatal("paused descendant should be cancelled by cascade")
	}
	for _, id := range []string{domainID, leafID, pausedID} {
		if _, ok := d.activity.Load(id); ok {
			t.Fatalf("activity entry of %s should be cleaned after cascade kill", id)
		}
	}
	for _, id := range []string{domainID, leafID, pausedID} {
		if n, ok := tr.Get(id); ok && n.Status == orchestrator.StatusRunning {
			t.Fatalf("node %s should no longer be running after cascade kill", id)
		}
	}
}

// TestKillStuckSubAgent_NoDescendants 叶子（无后代）被杀不 panic、正常收尾。
func TestKillStuckSubAgent_NoDescendants(t *testing.T) {
	d, mb, tr, _, _ := newSalvageTestEnv(t, &mockProvider{text: "ok"})
	leafID := "s1/code_assistant-9"
	tr.Register(orchestrator.Node{ID: leafID, ParentID: "s1", Role: "code_assistant", Status: orchestrator.StatusRunning})
	stale := time.Now().Add(-time.Hour).UnixNano()
	d.activity.Store(leafID, newAtomic(stale))
	d.subMeta.Store(leafID, &subAgentMeta{parentID: "s1", sessionID: "s1"})

	d.killStuckSubAgent(leafID)

	var got bool
	dl := time.Now().Add(2 * time.Second)
	for time.Now().Before(dl) && !got {
		for _, m := range mb.Drain("s1") {
			if strings.Contains(m.Body, "[failure kind=killed retryable=true]") {
				got = true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !got {
		t.Fatal("parent mailbox should receive killed notification with retryable=true")
	}
}

// TestRecentActivitySummary_KillMsg 归纳段：最近自述 + 最近工具调用随 kill 回告；
// 工具 cap 6 且新版在前；无记录时不追加该段。
func TestRecentActivitySummary_KillMsg(t *testing.T) {
	d, mb, tr, _, _ := newSalvageTestEnv(t, &mockProvider{text: "ok"})
	domainID := "s1/domain-1"
	tr.Register(orchestrator.Node{ID: domainID, ParentID: "s1", Role: "domain", Status: orchestrator.StatusRunning})
	stale := time.Now().Add(-time.Hour).UnixNano()
	d.activity.Store(domainID, newAtomic(stale))
	d.subMeta.Store(domainID, &subAgentMeta{parentID: "s1", sessionID: "s1"})

	d.recordRecentActivity(domainID, agent.LiveEvent{Kind: agent.LiveEventLLMDelta, Text: "正在跑 6 场景截图验证"})
	for i := 1; i <= 8; i++ {
		d.recordRecentActivity(domainID, agent.LiveEvent{
			Kind:  agent.LiveEventToolCall,
			Tool:  "RunCommand",
			Input: `{"cmd":"node probe.tmp.js","n":` + string(rune('0'+i)) + `}`,
		})
	}

	d.killStuckSubAgent(domainID)

	var body string
	dl := time.Now().Add(2 * time.Second)
	for time.Now().Before(dl) && body == "" {
		for _, m := range mb.Drain("s1") {
			body = m.Body
		}
		time.Sleep(10 * time.Millisecond)
	}
	if body == "" {
		t.Fatal("parent mailbox never got kill notification")
	}
	if !strings.Contains(body, "终止前活动摘要") {
		t.Fatalf("kill msg should contain recent activity summary, got: %s", body)
	}
	if !strings.Contains(body, "正在跑 6 场景截图验证") {
		t.Fatalf("summary should contain last LLM text, got: %s", body)
	}
	if !strings.Contains(body, `"n":8`) {
		t.Fatalf("newest tool call should be present, got: %s", body)
	}
	if strings.Contains(body, `"n":1`) || strings.Contains(body, `"n":2`) {
		t.Fatalf("only last 6 tool calls kept, got: %s", body)
	}

	// 无记录 Agent：不追加归纳段。
	leafID := "s1/code_assistant-7"
	tr.Register(orchestrator.Node{ID: leafID, ParentID: "s1", Role: "code_assistant", Status: orchestrator.StatusRunning})
	d.activity.Store(leafID, newAtomic(stale))
	d.subMeta.Store(leafID, &subAgentMeta{parentID: "s1", sessionID: "s1"})
	d.killStuckSubAgent(leafID)
	dl = time.Now().Add(2 * time.Second)
	var leafBody string
	for time.Now().Before(dl) && leafBody == "" {
		for _, m := range mb.Drain("s1") {
			if strings.Contains(m.Body, "code_assistant-7 超过") || strings.Contains(m.Body, leafID) {
				leafBody = m.Body
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if leafBody != "" && strings.Contains(leafBody, "终止前活动摘要") {
		t.Fatalf("no-record agent should not have summary section, got: %s", leafBody)
	}
}

// sync2Flag 是 cancel 是否被调的并发哨兵（context.CancelFunc 兼容）。
type sync2Flag struct {
	mu sync.Mutex
	v  bool
}

func (f *sync2Flag) set() { f.mu.Lock(); f.v = true; f.mu.Unlock() }
func (f *sync2Flag) called() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.v
}
