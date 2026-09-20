// Package agent 包含 think/子 Agent 摘要落会话事件流的单元测试。
package agent

import (
	"encoding/json"
	"testing"

	"github.com/go-kratos/blades"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// agentIDFromDetail 解析事件 DetailJSON（{"agent_id":"..."}）中的实例 ID；失败返回空串。
func agentIDFromDetail(detailJSON string) string {
	var d struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal([]byte(detailJSON), &d); err != nil {
		return ""
	}
	return d.AgentID
}

// newLiveEventTestSession 构造一个带运行中会话的 ReactService，供 handleLiveEvent 单测使用。
func newLiveEventTestSession(t *testing.T) (*ReactService, *reactInternalSession) {
	t.Helper()
	svc := newReactServiceForTest(nil, t.TempDir())
	sess := svc.store.createSession("test", "")
	return svc, sess
}

// findEventByKind 返回会话事件中第一条 Kind 匹配的事件；不存在返回 nil。
func findEventByKind(events []internalEvent, kind string) *internalEvent {
	for i := range events {
		if events[i].Kind == kind {
			return &events[i]
		}
	}
	return nil
}

// TestHandleLiveEvent_ThinkPersistedOnLLMDelta 验证答复文本开始输出时，
// 累积的思考过程落为 think 事件（剥子 Agent 前缀、带 agent_id 归属），瞬时字段清空。
func TestHandleLiveEvent_ThinkPersistedOnLLMDelta(t *testing.T) {
	svc, sess := newLiveEventTestSession(t)
	svc.store.setThinkingText(sess, "【代码助手】\n分析文件结构…")

	svc.handleLiveEvent(sess, LiveEvent{
		Kind:    LiveEventLLMDelta,
		Agent:   "代码助手",
		AgentID: "session-1/code_assistant-5",
		Text:    "最终答复",
	})

	think := findEventByKind(sess.Events, eventkind.Think)
	if think == nil {
		t.Fatalf("思考结束时应落 think 事件，events=%v", sess.Events)
	}
	if think.Agent != "代码助手" {
		t.Fatalf("think 事件 Agent 应为展示名，got %q", think.Agent)
	}
	if think.Message != "分析文件结构…" {
		t.Fatalf("think 事件应剥离【Agent】前缀，got %q", think.Message)
	}
	if agentIDFromDetail(think.DetailJSON) != "session-1/code_assistant-5" {
		t.Fatalf("think 事件 DetailJSON 应含实例 ID，got %q", think.DetailJSON)
	}
	if sess.ThinkingText != "" || sess.StreamingText != "最终答复" {
		t.Fatalf("瞬时字段应更新：thinking=%q streaming=%q", sess.ThinkingText, sess.StreamingText)
	}
}

// TestHandleLiveEvent_ThinkPersistedOnToolCall 验证思考后直接调工具（无文本输出）时，
// 工具调用开始同样落 think 事件。
func TestHandleLiveEvent_ThinkPersistedOnToolCall(t *testing.T) {
	svc, sess := newLiveEventTestSession(t)
	svc.store.setThinkingText(sess, "先读文件再决定")

	svc.handleLiveEvent(sess, LiveEvent{
		Kind:    LiveEventToolCall,
		Agent:   "MetaAgent",
		AgentID: "session-1",
		Tool:    "ReadFile",
	})

	think := findEventByKind(sess.Events, eventkind.Think)
	if think == nil {
		t.Fatal("工具调用开始时应落 think 事件")
	}
	if think.Message != "先读文件再决定" || think.Agent != "MetaAgent" {
		t.Fatalf("think 事件内容不符，got agent=%q msg=%q", think.Agent, think.Message)
	}
	if sess.ThinkingText != "" {
		t.Fatalf("think 落事件后瞬时字段应清空，got %q", sess.ThinkingText)
	}
}

// TestHandleLiveEvent_ThinkEmptyNoEvent 验证无思考内容时不落空 think 事件。
func TestHandleLiveEvent_ThinkEmptyNoEvent(t *testing.T) {
	svc, sess := newLiveEventTestSession(t)
	svc.handleLiveEvent(sess, LiveEvent{Kind: LiveEventLLMDelta, Agent: "MetaAgent", Text: "直接答复"})
	if ev := findEventByKind(sess.Events, eventkind.Think); ev != nil {
		t.Fatalf("无思考内容不应落 think 事件，got %q", ev.Message)
	}
}

// TestHandleLiveEvent_ClusterTopThinkDropped 验证集群档顶层 Meta 的思考链被丢弃：
// 不进 ThinkingText（live 思考盒）也不落 think 事件（与中间轮口播同治理）；
// 子 Agent 思考与日常档顶层思考不受影响。
func TestHandleLiveEvent_ClusterTopThinkDropped(t *testing.T) {
	svc, sess := newLiveEventTestSession(t)
	sess.ID = "session-1" // 顶层 Meta 实例 ID = 会话 ID
	sess.setGear(tool.GearCluster)

	// 顶层 Meta 思考：丢弃。
	svc.handleLiveEvent(sess, LiveEvent{
		Kind: LiveEventThinkDelta, Agent: "MetaAgent", AgentID: "session-1", Text: "编排推理：先派侦察…",
	})
	if sess.ThinkingText != "" {
		t.Fatalf("集群顶层 Meta 思考不应进 ThinkingText，got %q", sess.ThinkingText)
	}
	// 思考结束边界（答复输出）：不得落 think 事件，正文只进轮缓冲。
	svc.handleLiveEvent(sess, LiveEvent{
		Kind: LiveEventLLMDelta, Agent: "MetaAgent", AgentID: "session-1", Text: "终答",
	})
	if ev := findEventByKind(sess.Events, eventkind.Think); ev != nil {
		t.Fatalf("集群顶层 Meta 思考不应落 think 事件，got %q", ev.Message)
	}
	if sess.StreamingText != "" {
		t.Fatalf("集群顶层 Meta 正文应只进轮缓冲不进 StreamingText，got %q", sess.StreamingText)
	}

	// 子 Agent 思考：照常展示。
	svc.handleLiveEvent(sess, LiveEvent{
		Kind: LiveEventThinkDelta, Agent: "代码助手", AgentID: "session-1/code_assistant-5", Text: "【代码助手】\n分析文件结构…",
	})
	if sess.ThinkingText != "【代码助手】\n分析文件结构…" {
		t.Fatalf("子 Agent 思考应照常进 ThinkingText，got %q", sess.ThinkingText)
	}
}

// TestHandleLiveEvent_DailyGearTopThinkKept 验证日常档顶层思考展示不受影响。
func TestHandleLiveEvent_DailyGearTopThinkKept(t *testing.T) {
	svc, sess := newLiveEventTestSession(t)
	sess.ID = "session-1"
	sess.setGear(tool.GearDaily)
	svc.handleLiveEvent(sess, LiveEvent{
		Kind: LiveEventThinkDelta, Agent: "MetaAgent", AgentID: "session-1", Text: "日常档思考",
	})
	if sess.ThinkingText != "日常档思考" {
		t.Fatalf("日常档顶层思考应照常进 ThinkingText，got %q", sess.ThinkingText)
	}
}

// TestHandleLiveEvent_SubAgentDoneLLMResult 验证子 Agent 完成时摘要落 llm_result 事件
// （Tool 承载实例 ID）；摘要为空时不落。
func TestHandleLiveEvent_SubAgentDoneLLMResult(t *testing.T) {
	svc, sess := newLiveEventTestSession(t)
	svc.handleLiveEvent(sess, LiveEvent{
		Kind: LiveEventSubAgentDone,
		Tool: "session-1/domain-3",
		Text: "游戏渲染完成：场景+3 个怪物模型",
	})

	llm := findEventByKind(sess.Events, eventkind.LLMResult)
	if llm == nil {
		t.Fatal("子 Agent 带摘要完成时应落 llm_result 事件")
	}
	if llm.Message != "游戏渲染完成：场景+3 个怪物模型" || llm.Tool != "session-1/domain-3" {
		t.Fatalf("llm_result 内容不符，got tool=%q msg=%q", llm.Tool, llm.Message)
	}
	if agentIDFromDetail(llm.DetailJSON) != "session-1/domain-3" {
		t.Fatalf("llm_result DetailJSON 应含实例 ID，got %q", llm.DetailJSON)
	}
	if done := findEventByKind(sess.Events, "sub_agent_done"); done == nil {
		t.Fatal("仍应保留 sub_agent_done 生命周期事件")
	}

	// 空摘要：不落 llm_result。
	svc2, sess2 := newLiveEventTestSession(t)
	svc2.handleLiveEvent(sess2, LiveEvent{Kind: LiveEventSubAgentDone, Tool: "session-1/domain-4"})
	if ev := findEventByKind(sess2.Events, eventkind.LLMResult); ev != nil {
		t.Fatalf("空摘要不应落 llm_result，got %q", ev.Message)
	}
}

// TestHandleToolEvent_AgentIDDetail 验证子 Agent 工具事件携带 agent_id DetailJSON。
func TestHandleToolEvent_AgentIDDetail(t *testing.T) {
	svc, sess := newLiveEventTestSession(t)
	ctx := WithAgentID(t.Context(), "session-1/domain-3")
	svc.handleToolEvent(ctx, tool.ProgressEvent{
		SessionID: sess.ID,
		Kind:      "tool_result",
		Tool:      "ReadFile",
		Message:   "工具结果 ReadFile",
		Detail:    `{"output":"line1","path":"src/a.js"}`,
	})

	found := false
	for _, ev := range sess.Events {
		if ev.Type == eventkind.ToolExec && ev.Tool == "ReadFile" {
			found = true
			if agentIDFromDetail(ev.DetailJSON) != "session-1/domain-3" {
				t.Fatalf("工具事件 DetailJSON 应含实例 ID，got %q", ev.DetailJSON)
			}
		}
	}
	if !found {
		t.Fatal("未找到 ReadFile 工具事件")
	}
}

// TestEmitLive_FillsAgentID 验证 emitLive 自动填充 AgentID 为 Agent 实例 ID（a.name）。
func TestEmitLive_FillsAgentID(t *testing.T) {
	llm := &mockStreamProvider{chunks: []string{"done"}, final: blades.AssistantMessage("done")}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	agent := NewReActAgent("meta", types.RoleDefinition{SystemPrompt: "s", Name: "MetaAgent"}, llm, NewToolRegistryAdapter(reg))

	var got LiveEvent
	agent.WithLiveEvents(func(ev LiveEvent) {
		if ev.Kind == LiveEventLLMDelta {
			got = ev
		}
	})
	if _, err := agent.Run(t.Context(), "hi"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Kind == "" {
		t.Fatal("未收到任何 LiveEvent")
	}
	if got.AgentID != "meta" {
		t.Fatalf("emitLive 应填 AgentID=实例 ID（a.name），got %q", got.AgentID)
	}
	if got.Agent != "MetaAgent" {
		t.Fatalf("emitLive 应填 Agent=role.Name，got %q", got.Agent)
	}
}
