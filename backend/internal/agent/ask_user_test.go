// Package agent ask_user 人在回路测试（TODO #24）。
package agent

// ask_user_test.go 验证 AskUserHook 全链路：
//   - meta Agent 调 ask_user 提问 → 会话暂停（awaiting_clarify + PendingClarify 透出问题文本）；
//   - 用户答复（sendMessage 普通输入路径）→ 原始答复作为工具结果带回 ReAct 循环 → 会话完成；
//   - 超时未答复 → 工具返回"用户未答复，自行决策"，Agent 继续；
//   - 未接线 hook 时工具返回未配置错误（tool 包单测覆盖）。

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/internal/userprofile"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/go-kratos/blades"
)

// newAskUserTestService 构造带 AskUserHook 接线的 ReactService。
func newAskUserTestService(t *testing.T, provider ModelProvider) *ReactService {
	t.Helper()
	roleRegistry := role.NewRegistry(&pkgconfig.RoleConfigFile{})
	toolRegistry := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	s := NewReactService(roleRegistry, nil, toolRegistry, nil, NopMemoryPipeline{}, nil)
	s.testProvider = provider
	s.store.workDir = t.TempDir()
	toolRegistry.SetAskUserHook(s.AskUserHook())
	return s
}

// askCaptureProvider 记录最后一次 Generate 的请求，供断言工具结果是否进入下一轮上下文。
type askCaptureProvider struct {
	responses []*blades.Message
	lastReq   *blades.ModelRequest
}

func (p *askCaptureProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	p.lastReq = req
	if len(p.responses) == 0 {
		return &blades.ModelResponse{Message: blades.AssistantMessage("done")}, nil
	}
	r := p.responses[0]
	p.responses = p.responses[1:]
	return &blades.ModelResponse{Message: r}, nil
}
func (p *askCaptureProvider) Name() string { return "ask-capture" }

// requestContains 报告最后一次请求的任意消息是否包含 substr。
func (p *askCaptureProvider) requestContains(substr string) bool {
	if p.lastReq == nil {
		return false
	}
	for _, m := range p.lastReq.Messages {
		for _, part := range m.Parts {
			if strings.Contains(fmt.Sprintf("%v", part), substr) {
				return true
			}
		}
	}
	return false
}

func askUserToolCall(question string) *blades.Message {
	return &blades.Message{
		Role: blades.RoleAssistant,
		Parts: []blades.Part{
			blades.ToolPart{Name: "ask_user", Request: string(mustJSON(map[string]any{"question": question}))},
		},
	}
}

// TestAskUserFlow 用户答复闭环：提问 -> 暂停 -> 答复 -> 工具结果入下一轮上下文 -> 完成。
func TestAskUserFlow(t *testing.T) {
	llm := &askCaptureProvider{responses: []*blades.Message{
		askUserToolCall("游戏配色用深色还是浅色？"),
		blades.AssistantMessage("完成，已按用户答复用深色实现"),
	}}
	svc := newAskUserTestService(t, llm)
	ctx := context.Background()

	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "实现塔防游戏"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// 等会话进入待答复（ask_user 阻塞，会话转 awaiting_clarify）。
	deadline := time.Now().Add(5 * time.Second)
	var sess *Session
	for time.Now().Before(deadline) {
		sess, _ = svc.Get(ctx, created.ID)
		if sess != nil && sess.PendingClarify != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sess == nil || sess.PendingClarify == nil {
		t.Fatal("expected ask_user pending within timeout")
	}
	if !strings.Contains(sess.PendingClarify.Question, "深色还是浅色") {
		t.Fatalf("expected question text in PendingClarify, got: %q", sess.PendingClarify.Question)
	}
	if sess.Status != string(enums.SessionStatusAwaitingClarify) {
		t.Fatalf("expected awaiting_clarify while ask pending, got %q", sess.Status)
	}

	// 用户答复走 sendMessage（TUI 普通输入路径）：原始文本（非审批裁决）。
	if err := svc.sendMessage(ctx, created.ID, "用深色"); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}

	// 等会话完成，断言用户答复作为工具结果进入下一轮 LLM 请求。
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sess, _ = svc.Get(ctx, created.ID)
		if sess != nil && sess.Status == string(enums.SessionStatusCompleted) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sess == nil || sess.Status != string(enums.SessionStatusCompleted) {
		t.Fatalf("session did not complete after ask answer, status=%v", sess.Status)
	}
	if !llm.requestContains("用户答复: 用深色") {
		t.Fatalf("ask_user tool result with user answer should reach the next LLM request")
	}
	if !strings.Contains(sess.Result, "深色") {
		t.Fatalf("final answer should reflect user preference, got: %s", sess.Result)
	}
}

// TestAskUser_TimeoutSelfDecision 超时未答复：工具返回"用户未答复，自行决策"，Agent 继续完成。
func TestAskUser_TimeoutSelfDecision(t *testing.T) {
	llm := &askCaptureProvider{responses: []*blades.Message{
		{
			Role: blades.RoleAssistant,
			Parts: []blades.Part{
				blades.ToolPart{Name: "ask_user", Request: string(mustJSON(map[string]any{
					"question":    "配色？",
					"timeout_sec": float64(1),
				}))},
			},
		},
		blades.AssistantMessage("自行决定用深色"),
	}}
	svc := newAskUserTestService(t, llm)
	ctx := context.Background()

	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "实现游戏"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// 等 PendingClarify 出现（确保提问已挂起），然后不答复。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sess, _ := svc.Get(ctx, created.ID)
		if sess != nil && sess.PendingClarify != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 等会话完成（timeout 后自行决策路径）。
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		sess, _ := svc.Get(ctx, created.ID)
		if sess != nil && sess.Status == string(enums.SessionStatusCompleted) {
			if !llm.requestContains("用户未答复，自行决策") {
				t.Fatalf("timeout path should produce 自行决策 tool result in next request")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("session did not complete after ask_user timeout")
}

// ---- TODO #28 用户画像 ----

// TestUserProfile_MetaPromptInjection 画像注入 MetaAgent 系统提示词（【用户画像】前缀）。
func TestUserProfile_MetaPromptInjection(t *testing.T) {
	dir := t.TempDir()
	store := userprofile.NewStore(filepath.Join(dir, "profile.md"))
	_ = store.Load()
	_ = store.Append("偏好", "直接改别问")
	svc := newAskUserTestService(t, &askCaptureProvider{responses: []*blades.Message{blades.AssistantMessage("done")}})
	svc.SetUserProfileStore(store)
	svc.SetPersonaInjector(nil)

	llm := &askCaptureProvider{responses: []*blades.Message{blades.AssistantMessage("done")}}
	_ = llm
	svc2 := newAskUserTestService(t, &askCaptureProvider{responses: []*blades.Message{blades.AssistantMessage("done")}})
	svc2.SetUserProfileStore(store)
	svc2.SetPersonaInjector(nil)
	// metaPersona 组合注入器应含画像前缀。
	inj := svc2.metaPersona()
	out := inj.Inject("system")
	if !strings.Contains(out, "【用户画像】") || !strings.Contains(out, "直接改别问") {
		t.Fatalf("meta prompt should carry profile, got: %s", out)
	}
	_ = svc
	_ = store
}

// TestUserProfile_ExplicitWriteAndSave 显式写入（remember 通道）+ SaveProfile 覆盖闭环。
func TestUserProfile_ExplicitWriteAndSave(t *testing.T) {
	dir := t.TempDir()
	store := userprofile.NewStore(filepath.Join(dir, "profile.md"))
	_ = store.Load()
	svc := newAskUserTestService(t, &askCaptureProvider{responses: []*blades.Message{blades.AssistantMessage("done")}})
	svc.SetUserProfileStore(store)

	if err := svc.SaveProfile(context.Background(), "# 用户画像\n\n## 偏好\n- 手动编辑\n"); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	p, err := svc.Profile(context.Background())
	if err != nil || !strings.Contains(p.Content, "手动编辑") {
		t.Fatalf("Profile should reflect saved content, err=%v content=%q", err, p.Content)
	}
}

// TestUserProfile_ExtractionAtCompletion 会话完成自动提取：偏好增量追加进反馈记录。
func TestUserProfile_ExtractionAtCompletion(t *testing.T) {
	dir := t.TempDir()
	store := userprofile.NewStore(filepath.Join(dir, "profile.md"))
	_ = store.Load()
	svc := newAskUserTestService(t, &askCaptureProvider{responses: []*blades.Message{blades.AssistantMessage("done")}})
	svc.SetUserProfileStore(store)
	svc.SetProfileExtractor(func(ctx context.Context, text string) ([]string, error) {
		return []string{"沟通风格: 直接给结论"}, nil
	})

	created, err := svc.CreateSession(context.Background(), CreateRequest{Goal: "记住：我偏好直接给结论"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sess, _ := svc.Get(context.Background(), created.ID)
		if sess != nil && sess.Status == string(enums.SessionStatusCompleted) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(store.Current().Content, "沟通风格: 直接给结论") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("profile should gain extracted preference after session completion, got: %s", store.Current().Content)
}

// TestUserProfile_ExtractFailureNoSideEffect 提取失败零副作用。
func TestUserProfile_ExtractFailureNoSideEffect(t *testing.T) {
	dir := t.TempDir()
	store := userprofile.NewStore(filepath.Join(dir, "profile.md"))
	_ = store.Load()
	svc := newAskUserTestService(t, &askCaptureProvider{responses: []*blades.Message{blades.AssistantMessage("done")}})
	svc.SetUserProfileStore(store)
	svc.SetProfileExtractor(func(ctx context.Context, text string) ([]string, error) {
		return nil, errors.New("llm down")
	})

	created, err := svc.CreateSession(context.Background(), CreateRequest{Goal: "任务"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sess, _ := svc.Get(context.Background(), created.ID)
		if sess != nil && sess.Status == string(enums.SessionStatusCompleted) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if store.Current().Content != "" {
		t.Fatalf("extract failure must not write profile, got: %s", store.Current().Content)
	}
}
