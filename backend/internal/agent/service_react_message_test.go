package agent

// service_react_message_test.go 覆盖编排页用户直连的状态机路由（TODO 第12项 Task 7）：
// running+child_wait → 注入唤醒；running → ErrAgentBusy；终态 → 复活重跑；
// Paused/Idle/meta → 拒绝；成功分支留 System 会话事件。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/internal/server/eventkind"
)

// fakeMessenger 记录直连分支调用（注入 / 复活）。
type fakeMessenger struct {
	injected  []string
	revived   []orchestrator.Node
	contents  []string
	injectErr error
	reviveErr error
	// reviveCtxSession 记录复活调用 ctx 中的会话标识（须等于目标会话 ID）。
	reviveCtxSession string
}

func (f *fakeMessenger) InjectUserMessage(agentID, content string) error {
	f.injected = append(f.injected, agentID)
	f.contents = append(f.contents, content)
	return f.injectErr
}

func (f *fakeMessenger) ReviveWithMessage(ctx context.Context, node orchestrator.Node, userMsg string) error {
	f.revived = append(f.revived, node)
	f.contents = append(f.contents, userMsg)
	// 记录复活 ctx 里的会话标识：HTTP 请求 ctx 不带 sessionID，缺了它 dispatcher
	// 会取到空树、复活必然失败（回归护栏）。
	f.reviveCtxSession = tool.SessionIDFromContext(ctx)
	return f.reviveErr
}

// fakeActivityEvidence 固定返回某 kind 的活动证据（模拟 waitForChildren 上报的 child_wait）。
type fakeActivityEvidence struct{ kind string }

func (f *fakeActivityEvidence) ActivityEvidenceOf(string) (string, time.Duration, bool) {
	return f.kind, time.Second, true
}

// newMessageAgentEnv 构造一个带内存会话 + 树节点的 ReactService。
func newMessageAgentEnv(t *testing.T) (*ReactService, *fakeMessenger, *orchestrator.Tree, string) {
	t.Helper()
	svc := NewReactService(nil, nil, nil, mailbox.New(), NopMemoryPipeline{}, nil)
	sess := svc.store.createSession("编排页直连测试", "")
	tr := svc.TreeFor(sess.ID)
	fm := &fakeMessenger{}
	svc.SetAgentMessenger(fm)
	return svc, fm, tr, sess.ID
}

// registerNode 在树中注册一个指定状态的子节点。
func registerNode(t *testing.T, tr *orchestrator.Tree, sessID, instID string, status orchestrator.Status) orchestrator.Node {
	t.Helper()
	tr.Register(orchestrator.Node{
		ID: instID, ParentID: sessID, Role: "domain", Domain: "配置",
		Task: "实现 config.js", Status: orchestrator.StatusRunning,
	})
	switch status {
	case orchestrator.StatusRunning:
	case orchestrator.StatusDone:
		tr.Finish(instID, "上一轮完成", nil)
	case orchestrator.StatusFailed:
		tr.Finish(instID, "失败了", errors.New("boom"))
	case orchestrator.StatusUnverified:
		tr.FinishUnverified(instID, "缺证据", "no evidence")
	case orchestrator.StatusPaused:
		tr.Pause(instID, "token budget exhausted")
	case orchestrator.StatusIdle:
		tr.Idle(instID, "done", nil)
	case orchestrator.StatusCancelled:
		tr.Cancel(instID)
	}
	n, _ := tr.Get(instID)
	return n
}

// TestMessageAgentStateRouting 逐分支断言状态机路由与留痕。
func TestMessageAgentStateRouting(t *testing.T) {
	ctx := context.Background()

	// 分支 1：running + child_wait → 注入唤醒，content 透传。
	t.Run("waiting_inject", func(t *testing.T) {
		svc, fm, tr, sid := newMessageAgentEnv(t)
		svc.SetActivityEvidenceProvider(&fakeActivityEvidence{kind: "child_wait"})
		registerNode(t, tr, sid, "child-1", orchestrator.StatusRunning)
		if err := svc.MessageAgent(ctx, sid, "child-1", "看看进度"); err != nil {
			t.Fatalf("waiting 注入应成功: %v", err)
		}
		if len(fm.injected) != 1 || fm.injected[0] != "child-1" || fm.contents[0] != "看看进度" {
			t.Fatalf("注入分支不符: %+v", fm)
		}
	})

	// 分支 2：running（非 child_wait）→ ErrAgentBusy。
	t.Run("running_busy", func(t *testing.T) {
		svc, fm, tr, sid := newMessageAgentEnv(t)
		svc.SetActivityEvidenceProvider(&fakeActivityEvidence{kind: "tool:ReadFile"})
		registerNode(t, tr, sid, "child-2", orchestrator.StatusRunning)
		err := svc.MessageAgent(ctx, sid, "child-2", "在吗")
		if !errors.Is(err, ErrAgentBusy) {
			t.Fatalf("running 应 ErrAgentBusy, got %v", err)
		}
		if len(fm.injected) != 0 || len(fm.revived) != 0 {
			t.Fatal("running 分支不应触达 messenger")
		}
	})

	// 分支 3：终态（done/failed/cancelled/unverified）→ 复活重跑，node 透传。
	for _, tc := range []struct {
		name   string
		status orchestrator.Status
	}{
		{"done_revive", orchestrator.StatusDone},
		{"failed_revive", orchestrator.StatusFailed},
		{"unverified_revive", orchestrator.StatusUnverified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, fm, tr, sid := newMessageAgentEnv(t)
			registerNode(t, tr, sid, "child-3", tc.status)
			if err := svc.MessageAgent(ctx, sid, "child-3", "返工一下"); err != nil {
				t.Fatalf("终态复活应成功: %v", err)
			}
			if len(fm.revived) != 1 || fm.revived[0].ID != "child-3" || fm.contents[0] != "返工一下" {
				t.Fatalf("复活分支不符: %+v", fm)
			}
			if fm.revived[0].Task != "实现 config.js" {
				t.Fatalf("复活种子需带原任务, got %q", fm.revived[0].Task)
			}
			// 复活 ctx 必须带会话标识：否则 dispatcher treeFn("") 取到空树，复活永远失败。
			if fm.reviveCtxSession != sid {
				t.Fatalf("复活 ctx 会话标识 = %q, want %q", fm.reviveCtxSession, sid)
			}
		})
	}

	// 分支 4：Paused / Idle → ErrInvalidSessionState（走监控页恢复 / 经 MetaAgent 派发）。
	for _, tc := range []struct {
		name   string
		status orchestrator.Status
	}{
		{"paused_reject", orchestrator.StatusPaused},
		{"idle_reject", orchestrator.StatusIdle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, fm, tr, sid := newMessageAgentEnv(t)
			registerNode(t, tr, sid, "child-4", tc.status)
			err := svc.MessageAgent(ctx, sid, "child-4", "喂")
			if !errors.Is(err, ErrInvalidSessionState) {
				t.Fatalf("%s 应 ErrInvalidSessionState, got %v", tc.name, err)
			}
			if len(fm.injected) != 0 || len(fm.revived) != 0 {
				t.Fatal("拒绝分支不应触达 messenger")
			}
		})
	}

	// 分支 5：meta 实例与空 content → 拒绝。
	t.Run("meta_and_empty_reject", func(t *testing.T) {
		svc, fm, tr, sid := newMessageAgentEnv(t)
		registerNode(t, tr, sid, "child-5", orchestrator.StatusDone)
		if err := svc.MessageAgent(ctx, sid, "meta", "喂"); !errors.Is(err, ErrInvalidSessionState) {
			t.Fatalf("meta 应拒绝, got %v", err)
		}
		if err := svc.MessageAgent(ctx, sid, "child-5", "   "); err == nil {
			t.Fatal("空 content 应参数错")
		}
		if len(fm.injected) != 0 || len(fm.revived) != 0 {
			t.Fatal("拒绝分支不应触达 messenger")
		}
	})

	// 分支 6：成功分支留痕 —— 会话事件流出现 System 事件。
	t.Run("success_writes_event", func(t *testing.T) {
		svc, _, tr, sid := newMessageAgentEnv(t)
		svc.SetActivityEvidenceProvider(&fakeActivityEvidence{kind: "child_wait"})
		registerNode(t, tr, sid, "child-6", orchestrator.StatusRunning)
		if err := svc.MessageAgent(ctx, sid, "child-6", "留痕检查"); err != nil {
			t.Fatalf("注入应成功: %v", err)
		}
		svc.store.mu.RLock()
		sess := svc.store.sessions[sid]
		svc.store.mu.RUnlock()
		found := false
		for _, ev := range sess.Events {
			if ev.Type == eventkind.System && strings.Contains(ev.Message, "用户直连") {
				found = true
			}
		}
		if !found {
			t.Fatalf("成功分支应写 System 留痕事件, events=%+v", sess.Events)
		}
	})
}
