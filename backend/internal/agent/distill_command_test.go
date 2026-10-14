package agent

// distill_command_test.go 覆盖三件事：
//   - ①自进化准则注入：buildSystemPrompt 设/不设 evolutionBlock 两态（顺序在 memoryIndex 之后）；
//   - ②/提炼 快捷指令拦截：parseSlashCommand 解析、sendMessageFull 命中/未知/透传三分支；
//   - ③distillSession 手动提炼：事件数闸门 / 全量成功 / evolver 失败但块记忆成功。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// TestBuildSystemPromptEvolutionBlock 自进化准则注入：不设时零注入；设置时追加在
// memoryIndex 之后（尾部槽位），且计入 promptStatsSegs 分段计量。
func TestBuildSystemPromptEvolutionBlock(t *testing.T) {
	roleDef := types.RoleDefinition{ID: "meta", Name: "MetaAgent", SystemPrompt: "BASE"}

	// 不设 evolutionBlock：提示词不含准则块，分段为 0。
	a := NewReActAgent("s-noevo", roleDef, nil, nil).
		WithSkillBlock("SKILL_BLOCK").
		WithMemoryIndex("MEM_INDEX")
	p := a.systemPrompt()
	if strings.Contains(p, "自进化准则") {
		t.Fatalf("未设 evolutionBlock 不应注入准则块:\n%s", p)
	}
	if got := a.promptStatsSegs["evolution"]; got != 0 {
		t.Fatalf("未设时 evolution 分段应为 0, got %d", got)
	}

	// 设置 evolutionBlock：追加在 memoryIndex 之后，分段计量正确。
	evo := "【自进化准则】本角色在历史使用中沉淀的抽象行为准则（可经 update_evolution_prompt 全量更新，≤300字）：\n先验证再宣称完成"
	b := NewReActAgent("s-evo", roleDef, nil, nil).
		WithSkillBlock("SKILL_BLOCK").
		WithMemoryIndex("MEM_INDEX").
		WithEvolutionBlock(evo)
	p2 := b.systemPrompt()
	if !strings.Contains(p2, evo) {
		t.Fatalf("设置后应注入准则块:\n%s", p2)
	}
	mi, ev := strings.Index(p2, "MEM_INDEX"), strings.Index(p2, evo)
	if mi < 0 || ev < 0 || ev < mi {
		t.Fatalf("准则块应在 memoryIndex 之后: memIdx=%d evoIdx=%d", mi, ev)
	}
	if got, want := b.promptStatsSegs["evolution"], utf8.RuneCountInString(evo); got != want {
		t.Fatalf("evolution 分段 = %d, want %d", got, want)
	}
}

// TestParseSlashCommand 仅单 token 斜杠指令被识别；/提炼 与 /distill 归一为 distill。
func TestParseSlashCommand(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/提炼", "distill"},
		{"/distill", "distill"},
		{"/Distill", "distill"},
		{"  /提炼  ", "distill"},
		{"/xxx", "xxx"},
		{"/Foo", "foo"},
		{"多行\n/提炼", ""},
		{"- /列表", ""},
		{"/提炼 全部", ""},
		{"/ ", ""},
		{"", ""},
		{"普通消息", ""},
	}
	for _, c := range cases {
		if got := parseSlashCommand(c.in); got != c.want {
			t.Errorf("parseSlashCommand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// newDistillTestService 构造带 mock provider 的最小服务（指令拦截不触发 LLM，
// provider 仅供透传用例的 resume 路径使用）。
func newDistillTestService(t *testing.T) *ReactService {
	t.Helper()
	return newDedupTestService(t, &mockReactModelProvider{})
}

// seedDistillSession 建一个已完成会话并补 events 条内部事件（供提炼事件数闸门）。
func seedDistillSession(svc *ReactService, events int) *reactInternalSession {
	sess := svc.store.createSession("提炼测试目标", "")
	svc.store.mu.Lock()
	sess.Status = enums.SessionStatusCompleted
	sess.Result = "已完成：写了爬虫并验证"
	for i := 0; i < events; i++ {
		sess.Events = append(sess.Events, internalEvent{
			Type: eventkind.Message, Agent: "MetaAgent",
			Message: fmt.Sprintf("第%d轮事件", i), Timestamp: time.Now(),
		})
	}
	svc.store.mu.Unlock()
	return sess
}

// waitForSessionEvent 轮询会话事件流直到出现满足条件的事件（带超时防挂）。
func waitForSessionEvent(t *testing.T, svc *ReactService, sess *reactInternalSession, timeout time.Duration, match func(internalEvent) bool) internalEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		svc.store.mu.RLock()
		for _, ev := range sess.Events {
			if match(ev) {
				svc.store.mu.RUnlock()
				return ev
			}
		}
		svc.store.mu.RUnlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待事件超时（%s）", timeout)
	return internalEvent{}
}

// TestSendMessageDistillCommand /提炼 命中：返回 nil、落用户气泡事件、指令不进 LLM
// 对话历史、会话不置 Running；异步提炼完成后 System 回执含技能与块记忆条数。
func TestSendMessageDistillCommand(t *testing.T) {
	svc := newDistillTestService(t)
	sess := seedDistillSession(svc, minEvolveEvents+1)

	var mu sync.Mutex
	var sinkSkills []EvolvedSkill
	var sinkOutcome string
	var distillText string
	svc.evolver = func(ctx context.Context, in EvolveInput) (*EvolveOutput, error) {
		if in.Outcome != "mixed" {
			t.Errorf("evolve outcome = %q, want mixed", in.Outcome)
		}
		if in.Goal == "" || in.UserMessages == "" || in.EventsDigest == "" {
			t.Errorf("evolve input incomplete: goal=%q msgs=%d digest=%d", in.Goal, len(in.UserMessages), len(in.EventsDigest))
		}
		return &EvolveOutput{Skills: []EvolvedSkill{{Name: "web-scrape", Title: "网页抓取", WhenToUse: "抓网页时"}}}, nil
	}
	svc.skillSink = func(ctx context.Context, sessionID string, skills []EvolvedSkill, outcome string) error {
		mu.Lock()
		sinkSkills, sinkOutcome = skills, outcome
		mu.Unlock()
		return nil
	}
	svc.SetBlockMemoryDistiller(func(ctx context.Context, sessionID, text string) (int, error) {
		mu.Lock()
		distillText = text
		mu.Unlock()
		return 2, nil
	})

	if err := svc.sendMessageFull(context.Background(), sess.ID, "/提炼", nil, nil); err != nil {
		t.Fatalf("sendMessageFull: %v", err)
	}

	// 同步断言：指令不进 Messages、会话不置 Running、用户气泡事件已落。
	svc.store.mu.RLock()
	if sess.Status == enums.SessionStatusRunning {
		t.Error("指令拦截不应置会话 Running")
	}
	for _, m := range sess.Messages {
		if strings.Contains(m.Content, "/提炼") {
			t.Errorf("指令不应进 LLM 对话历史: %+v", m)
		}
	}
	hasUserEv := false
	for _, ev := range sess.Events {
		if ev.Type == eventkind.UserMessage && ev.Message == "/提炼" {
			hasUserEv = true
		}
	}
	svc.store.mu.RUnlock()
	if !hasUserEv {
		t.Fatal("应落 user_message 用户气泡事件")
	}

	// 异步提炼完成：System 回执含技能 title 与块记忆条数。
	ev := waitForSessionEvent(t, svc, sess, 3*time.Second, func(ev internalEvent) bool {
		return ev.Type == eventkind.System && strings.Contains(ev.Message, "手动提炼完成")
	})
	if !strings.Contains(ev.Message, "网页抓取") || !strings.Contains(ev.Message, "块记忆 +2 条") {
		t.Fatalf("回执应含技能与块记忆条数: %q", ev.Message)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sinkSkills) != 1 || sinkSkills[0].Name != "web-scrape" || sinkOutcome != "mixed" {
		t.Fatalf("skillSink 应收 1 技能且 outcome=mixed: skills=%+v outcome=%q", sinkSkills, sinkOutcome)
	}
	if !strings.Contains(distillText, "提炼测试目标") || !strings.Contains(distillText, "已完成") {
		t.Fatalf("distiller 文本应拼接 goal/result: %q", distillText)
	}
}

// TestSendMessageUnknownSlashCommand 未识别指令：用户气泡 + System 未知指令提示，
// 不进 LLM 历史、不触发续跑。
func TestSendMessageUnknownSlashCommand(t *testing.T) {
	svc := newDistillTestService(t)
	sess := seedDistillSession(svc, 1)

	if err := svc.sendMessageFull(context.Background(), sess.ID, "/foo", nil, nil); err != nil {
		t.Fatalf("sendMessageFull: %v", err)
	}

	// 未知分支同步落两条事件（无异步），直接断言。
	svc.store.mu.RLock()
	defer svc.store.mu.RUnlock()
	var userEv, sysEv bool
	for _, ev := range sess.Events {
		if ev.Type == eventkind.UserMessage && ev.Message == "/foo" {
			userEv = true
		}
		if ev.Type == eventkind.System && strings.Contains(ev.Message, "未知指令 /foo") && strings.Contains(ev.Message, "/提炼") {
			sysEv = true
		}
	}
	if !userEv || !sysEv {
		t.Fatalf("应有用户气泡与未知指令提示: userEv=%v sysEv=%v", userEv, sysEv)
	}
	for _, m := range sess.Messages {
		if strings.Contains(m.Content, "/foo") {
			t.Fatal("未知指令不应进 LLM 对话历史")
		}
	}
	if sess.Status == enums.SessionStatusRunning {
		t.Fatal("未知指令不应触发续跑")
	}
}

// TestSendMessageSlashWithSpacePassthrough "/提炼 xxx"（带空格）非单 token：
// 不拦截，走正常用户消息路径（进 Messages、无未知指令事件）。
func TestSendMessageSlashWithSpacePassthrough(t *testing.T) {
	svc := newDistillTestService(t)
	sess := seedDistillSession(svc, 1)

	if err := svc.sendMessageFull(context.Background(), sess.ID, "/提炼 一下", nil, nil); err != nil {
		t.Fatalf("sendMessageFull: %v", err)
	}

	svc.store.mu.RLock()
	found := false
	for _, m := range sess.Messages {
		if m.Role == string(enums.ChatRoleUser) && strings.Contains(m.Content, "/提炼 一下") {
			found = true
		}
	}
	for _, ev := range sess.Events {
		if ev.Type == eventkind.System && strings.Contains(ev.Message, "未知指令") {
			t.Errorf("透传路径不应有未知指令事件: %q", ev.Message)
		}
	}
	svc.store.mu.RUnlock()
	if !found {
		t.Fatal("带空格文本应走正常用户消息路径进 Messages")
	}
}

// TestDistillSessionGateOnEvents 事件 < minEvolveEvents：回执"会话内容太少"，evolver 不调用。
func TestDistillSessionGateOnEvents(t *testing.T) {
	svc := newEvolverTestService(t)
	sess := newEvolverTestSession(minEvolveEvents - 1)
	called := false
	svc.evolver = func(ctx context.Context, in EvolveInput) (*EvolveOutput, error) {
		called = true
		return &EvolveOutput{}, nil
	}
	svc.distillSession(sess, distillSnapshot{
		goal: sess.Goal, result: sess.Result,
		events: sess.Events, userMessages: userMessagesText(sess),
	})
	if called {
		t.Fatal("事件不足不应调用 evolver")
	}
	last := sess.Events[len(sess.Events)-1]
	if last.Type != eventkind.System || !strings.Contains(last.Message, "会话内容太少") {
		t.Fatalf("应回执会话内容太少: %+v", last)
	}
}

// TestDistillSessionFullSuccess 全量成功：fake evolver 出 1 技能（落 skillSink）
// + fake distiller 返 2，回执含技能 title 与块记忆条数。
func TestDistillSessionFullSuccess(t *testing.T) {
	svc := newEvolverTestService(t)
	sess := newEvolverTestSession(minEvolveEvents + 1)
	var sinkSkills []EvolvedSkill
	svc.skillSink = func(ctx context.Context, sessionID string, skills []EvolvedSkill, outcome string) error {
		sinkSkills = skills
		return nil
	}
	svc.evolver = func(ctx context.Context, in EvolveInput) (*EvolveOutput, error) {
		return &EvolveOutput{Skills: []EvolvedSkill{{Name: "api-retry", Title: "接口重试"}}}, nil
	}
	var gotText string
	svc.SetBlockMemoryDistiller(func(ctx context.Context, sessionID, text string) (int, error) {
		gotText = text
		return 2, nil
	})
	svc.distillSession(sess, distillSnapshot{
		goal: sess.Goal, result: sess.Result,
		events: sess.Events, userMessages: userMessagesText(sess),
	})
	if len(sinkSkills) != 1 || sinkSkills[0].Name != "api-retry" {
		t.Fatalf("skillSink 应收 1 技能: %+v", sinkSkills)
	}
	if strings.TrimSpace(gotText) == "" {
		t.Fatal("distiller 文本不应为空")
	}
	last := sess.Events[len(sess.Events)-1]
	if last.Type != eventkind.System || !strings.Contains(last.Message, "手动提炼完成") ||
		!strings.Contains(last.Message, "接口重试") || !strings.Contains(last.Message, "块记忆 +2 条") {
		t.Fatalf("回执应含技能与块记忆条数: %q", last.Message)
	}
}

// TestDistillSessionEvolverFailsButBlocksOK evolver 报错但 distiller 成功：
// 回执含部分失败文案且块记忆条数正确。
func TestDistillSessionEvolverFailsButBlocksOK(t *testing.T) {
	svc := newEvolverTestService(t)
	sess := newEvolverTestSession(minEvolveEvents + 1)
	svc.evolver = func(ctx context.Context, in EvolveInput) (*EvolveOutput, error) {
		return nil, errors.New("llm boom")
	}
	svc.SetBlockMemoryDistiller(func(ctx context.Context, sessionID, text string) (int, error) {
		return 3, nil
	})
	svc.distillSession(sess, distillSnapshot{
		goal: sess.Goal, result: sess.Result,
		events: sess.Events, userMessages: userMessagesText(sess),
	})
	last := sess.Events[len(sess.Events)-1]
	if !strings.Contains(last.Message, "技能提炼失败") || !strings.Contains(last.Message, "llm boom") ||
		!strings.Contains(last.Message, "块记忆 +3 条") {
		t.Fatalf("回执应含部分失败原因与块记忆条数: %q", last.Message)
	}
}
