package agent

// escalate_gear_test.go 验证 TODO #14 T7 升档流全链路（fake provider 驱动真实状态机）：
//   - 确认路：chat/fast 档调 escalate_gear → 确认卡挂起（awaiting_clarify）→ 用户答复
//     「确认」→ 切集群档（D-2）→ 旧 run 落定 → 种子消息经 sendMessage 重启为 meta 全装；
//   - 拒绝路：答复「拒绝」→ 档位不变，快速档对话继续完成。
//
// 会话目标用闲聊信号（"你好呀"）保证 fixGearForRun 选 fast 档——escalate 只在 fast 档有意义。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/go-kratos/blades"
)

// newEscalateTestService 构造接线了 escalate_gear 钩子的最小 ReactService。
func newEscalateTestService(t *testing.T, provider ModelProvider) *ReactService {
	t.Helper()
	svc := newReactServiceForTest(provider, t.TempDir())
	svc.toolRegistry.SetEscalateGearHook(svc.EscalateGearHook())
	return svc
}

// waitSessionEventual 轮询会话直到满足条件或超时；返回最后的快照。
func waitSessionEventual(t *testing.T, svc *ReactService, ctx context.Context, id string, cond func(*Session) bool) *Session {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var sess *Session
	for time.Now().Before(deadline) {
		sess, _ = svc.Get(ctx, id)
		if sess != nil && cond(sess) {
			return sess
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session condition not met within timeout, last: %+v", sess)
	return nil
}

// TestReactService_EscalateGearFlow_Confirm 确认路：升级 → 集群档新 run 接手。
func TestReactService_EscalateGearFlow_Confirm(t *testing.T) {
	llm := &mockReactModelProvider{responses: []*blades.Message{
		{
			Role: blades.RoleAssistant,
			Parts: []blades.Part{
				blades.ToolPart{Name: "escalate_gear", Request: string(mustJSON(map[string]any{
					"reason":     "需要改三个文件",
					"task_brief": "做一个塔防游戏，验收：npm test 通过",
					"files":      []any{"main.go", "game.go"},
				}))},
			},
		},
	}}
	svc := newEscalateTestService(t, llm)
	ctx := context.Background()

	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "你好呀"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// 闲聊目标应自动选 fast 档（T3 规则选档，runSession 异步固化），升档有前提。
	gearDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(gearDeadline) && svc.SessionGear(created.ID) != tool.GearFast {
		time.Sleep(10 * time.Millisecond)
	}
	if got := svc.SessionGear(created.ID); got != tool.GearFast {
		t.Fatalf("chatty goal should fix fast gear, got %q", got)
	}

	// 等待升级确认卡挂起。
	sess := waitSessionEventual(t, svc, ctx, created.ID, func(s *Session) bool {
		return s.PendingClarify != nil
	})
	if !strings.Contains(sess.PendingClarify.Question, "集群档") {
		t.Fatalf("confirm card question should mention 集群档, got %q", sess.PendingClarify.Question)
	}

	// 用户确认（sendMessage 路由进 askUser 通道）。
	if err := svc.sendMessage(ctx, created.ID, "确认"); err != nil {
		t.Fatalf("sendMessage confirm: %v", err)
	}

	// 终态：档位已切集群、种子消息入列、新 run 完成收尾。
	sess = waitSessionEventual(t, svc, ctx, created.ID, func(s *Session) bool {
		if s.Status != string(enums.SessionStatusCompleted) {
			return false
		}
		for _, m := range s.Messages {
			if m.Role == "user" && strings.Contains(m.Content, "【档位升级续跑】") {
				return true
			}
		}
		return false
	})
	if got := svc.SessionGear(created.ID); got != tool.GearCluster {
		t.Fatalf("gear should be cluster after escalation, got %q", got)
	}
	if !strings.Contains(sess.Messages[len(sess.Messages)-1].Content, "任务简报") {
		t.Fatalf("seed message should carry task brief, got %q", sess.Messages[len(sess.Messages)-1].Content)
	}
}

// TestReactService_EscalateGearFlow_Reject 拒绝路：档位不变，快速档继续完成。
func TestReactService_EscalateGearFlow_Reject(t *testing.T) {
	llm := &mockReactModelProvider{responses: []*blades.Message{
		{
			Role: blades.RoleAssistant,
			Parts: []blades.Part{
				blades.ToolPart{Name: "escalate_gear", Request: string(mustJSON(map[string]any{
					"reason":     "需要改代码",
					"task_brief": "重构配置模块",
				}))},
			},
		},
	}}
	svc := newEscalateTestService(t, llm)
	ctx := context.Background()

	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "你好呀"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	waitSessionEventual(t, svc, ctx, created.ID, func(s *Session) bool {
		return s.PendingClarify != nil
	})
	if err := svc.sendMessage(ctx, created.ID, "拒绝"); err != nil {
		t.Fatalf("sendMessage reject: %v", err)
	}

	waitSessionEventual(t, svc, ctx, created.ID, func(s *Session) bool {
		return s.Status == string(enums.SessionStatusCompleted)
	})
	if got := svc.SessionGear(created.ID); got != tool.GearFast {
		t.Fatalf("gear must stay fast after rejection, got %q", got)
	}
}

// TestReactService_SetSessionGear_RecordsEvent 手动切档落 D-2 系统事件（T7 留痕契约）。
func TestReactService_SetSessionGear_RecordsEvent(t *testing.T) {
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	ctx := context.Background()
	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "你好呀"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := svc.SetSessionGear(created.ID, tool.GearCluster); err != nil {
		t.Fatalf("SetSessionGear: %v", err)
	}
	sess := waitSessionEventual(t, svc, ctx, created.ID, func(s *Session) bool {
		for _, ev := range s.Events {
			if ev.Type == "system" && strings.Contains(ev.Message, "档位切换") && strings.Contains(ev.Message, "手动") {
				return true
			}
		}
		return false
	})
	found := false
	for _, ev := range sess.Events {
		if strings.Contains(ev.Message, "auto → cluster（手动）") {
			found = true
		}
	}
	if !found {
		t.Fatal("manual gear event should record direction and source")
	}
}
