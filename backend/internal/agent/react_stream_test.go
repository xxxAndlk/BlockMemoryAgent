// Package agent 包含 ReActAgent 流式生成与实时事件的单元测试。
package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
)

// mockStreamProvider 同时实现 Generate 与 NewStreaming 的 mock provider，
// 用于验证 ReActAgent 的流式路径与 llm_delta 实时事件。
type mockStreamProvider struct {
	// chunks 是 NewStreaming 依次产出的增量文本块。
	chunks []string
	// final 是 NewStreaming 最后产出的完整响应。
	final *blades.Message
}

// Name 返回 mock provider 名称。
func (m *mockStreamProvider) Name() string { return "mock-stream" }

// Generate 返回完整响应（非流式路径，本测试不使用）。
func (m *mockStreamProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	return &blades.ModelResponse{Message: m.final}, nil
}

// NewStreaming 依次产出增量块，最后产出完整响应（模拟 contrib/openai 的累积器行为）。
func (m *mockStreamProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	return func(yield func(*blades.ModelResponse, error) bool) {
		for _, c := range m.chunks {
			if !yield(&blades.ModelResponse{Message: blades.AssistantMessage(c)}, nil) {
				return
			}
		}
		yield(&blades.ModelResponse{Message: m.final}, nil)
	}
}

// TestReActAgent_Streaming_Deltas 验证增量式流式块被累积为逐步增长的 llm_delta 事件，
// 且最终结果与历史均取完整响应。
func TestReActAgent_Streaming_Deltas(t *testing.T) {
	llm := &mockStreamProvider{
		chunks: []string{"Hello", ", ", "world"},
		final:  blades.AssistantMessage("Hello, world"),
	}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	agent := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "s"}, llm, NewToolRegistryAdapter(reg))

	// 收集实时事件。
	var deltas []string
	agent.WithLiveEvents(func(ev LiveEvent) {
		if ev.Kind == LiveEventLLMDelta {
			deltas = append(deltas, ev.Text)
		}
	})

	res, err := agent.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Text != "Hello, world" {
		t.Fatalf("expected final text %q, got %q", "Hello, world", res.Text)
	}
	// 至少产出 3 次增量事件，且首次为首个块、末次为完整文本。
	if len(deltas) < 3 {
		t.Fatalf("expected >=3 delta events, got %d", len(deltas))
	}
	if deltas[0] != "Hello" {
		t.Fatalf("expected first delta %q, got %q", "Hello", deltas[0])
	}
	if deltas[len(deltas)-1] != "Hello, world" {
		t.Fatalf("expected last delta %q, got %q", "Hello, world", deltas[len(deltas)-1])
	}
}

// TestReActAgent_Streaming_CumulativeChunks 验证全量（累积）式流式块不会造成文本重复：
// 后一块以前缀包含前一块时直接替换而非追加。
func TestReActAgent_Streaming_CumulativeChunks(t *testing.T) {
	llm := &mockStreamProvider{
		chunks: []string{"Hello", "Hello, world"},
		final:  blades.AssistantMessage("Hello, world"),
	}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	agent := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "s"}, llm, NewToolRegistryAdapter(reg))

	var last string
	agent.WithLiveEvents(func(ev LiveEvent) {
		if ev.Kind == LiveEventLLMDelta {
			last = ev.Text
		}
	})

	res, err := agent.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Text != "Hello, world" {
		t.Fatalf("expected final text %q, got %q", "Hello, world", res.Text)
	}
	if last != "Hello, world" {
		t.Fatalf("cumulative chunks should replace, not append; got %q", last)
	}
}

// TestReActAgent_Streaming_ReasoningContent 验证 OpenAI 系 provider 的思考键
// reasoning_content 被消费为 think_delta：此前只认 Anthropic 的 "thinking" 键，
// deepseek/ark/glm 等 openai 兼容端点的实时思考与 think 事件恒空（2026-09-26 实证）。
// 同时验证这类 provider 单帧可同时携带累积正文，思考分支不得跳过本帧文本。
func TestReActAgent_Streaming_ReasoningContent(t *testing.T) {
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	a := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "s"},
		&reasoningStreamProvider{}, NewToolRegistryAdapter(reg))

	var thinks, texts []string
	a.WithLiveEvents(func(ev LiveEvent) {
		switch ev.Kind {
		case LiveEventThinkDelta:
			thinks = append(thinks, ev.Text)
		case LiveEventLLMDelta:
			texts = append(texts, ev.Text)
		}
	})

	res, err := a.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Text != "答" {
		t.Fatalf("final text = %q, want %q", res.Text, "答")
	}
	// reasoning_content 应逐帧产生 think_delta（累积快照），末帧为全量推理。
	if len(thinks) == 0 {
		t.Fatal("reasoning_content 应产生 think_delta 事件，got 0 条")
	}
	if thinks[len(thinks)-1] != "想清楚了" {
		t.Fatalf("末帧思考 = %q, want %q", thinks[len(thinks)-1], "想清楚了")
	}
	// 携带正文的同帧不得被思考分支跳过（不 continue）：正文增量仍要推送。
	if len(texts) == 0 || texts[len(texts)-1] != "答" {
		t.Fatalf("正文增量被跳过: texts = %v", texts)
	}
}

// reasoningStreamProvider 模拟 openai 兼容端点的流式行为：思考经 Metadata
// ["reasoning_content"] 累积传递，且单帧可同时携带累积正文（provider_openai_chat.go
// 的 reasoningBuf + contentBuf 同帧 yield）。第二帧即"思考+正文同帧"关键场景。
type reasoningStreamProvider struct{}

func (p *reasoningStreamProvider) Name() string { return "mock-reasoning" }

func (p *reasoningStreamProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	return &blades.ModelResponse{Message: blades.AssistantMessage("答")}, nil
}

func (p *reasoningStreamProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	return func(yield func(*blades.ModelResponse, error) bool) {
		// 帧1：仅推理。
		m1 := blades.AssistantMessage("")
		m1.Metadata = map[string]any{"reasoning_content": "想"}
		yield(&blades.ModelResponse{Message: m1}, nil)
		// 帧2：推理 + 正文同帧（关键：思考分支不得 continue 吃掉正文）。
		m2 := blades.AssistantMessage("答")
		m2.Metadata = map[string]any{"reasoning_content": "想清楚了"}
		yield(&blades.ModelResponse{Message: m2}, nil)
		// 末帧：全量响应。
		final := blades.AssistantMessage("答")
		final.Metadata = map[string]any{"reasoning_content": "想清楚了"}
		yield(&blades.ModelResponse{Message: final}, nil)
	}
}

// TestReActAgent_SubAgentDoneEvent 验证 mailbox 收到子 Agent 摘要时会推送
// LiveEventSubAgentDone 实时事件（Tool 字段为子 Agent ID）。
func TestReActAgent_SubAgentDoneEvent(t *testing.T) {
	llm := &mockStreamProvider{
		chunks: []string{"done"},
		final:  blades.AssistantMessage("done"),
	}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()
	agent := NewReActAgent("meta", types.RoleDefinition{SystemPrompt: "s"}, llm, NewToolRegistryAdapter(reg)).
		WithMailbox(mb)

	// 预置一封发给 "meta" 的子 Agent 完成通知。
	_, _ = mb.Send(&mailbox.Message{
		From:    "meta/code_assistant-1",
		To:      "meta",
		Type:    mailbox.MsgInfo,
		Subject: "子 Agent 完成",
		Body:    "写入完成",
	})

	var got LiveEvent
	fired := false
	agent.WithLiveEvents(func(ev LiveEvent) {
		if ev.Kind == LiveEventSubAgentDone {
			fired = true
			got = ev
		}
	})

	if _, err := agent.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fired {
		t.Fatal("mailbox 有子 Agent 摘要时应推送 LiveEventSubAgentDone")
	}
	if got.Tool != "meta/code_assistant-1" {
		t.Fatalf("Tool 应为子 Agent ID, got %q", got.Tool)
	}
}

// TestReActAgent_MilestoneEvent 验证 mailbox 收到 subject 前缀「里程碑:」的 info
// 消息时推送 LiveEventMilestone（而非 sub_agent_done——里程碑是中途播报不是完成）。
func TestReActAgent_MilestoneEvent(t *testing.T) {
	llm := &mockStreamProvider{
		chunks: []string{"done"},
		final:  blades.AssistantMessage("done"),
	}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()
	agent := NewReActAgent("meta", types.RoleDefinition{SystemPrompt: "s"}, llm, NewToolRegistryAdapter(reg)).
		WithMailbox(mb)

	// 预置一封里程碑播报 + 一封普通完成通知。
	_, _ = mb.Send(&mailbox.Message{
		From: "meta/domain-1", To: "meta", Type: mailbox.MsgInfo,
		Subject: "里程碑: 渲染链路已打通", Body: "三层拆分完成，js/render.js",
	})
	_, _ = mb.Send(&mailbox.Message{
		From: "meta/code_assistant-2", To: "meta", Type: mailbox.MsgInfo,
		Subject: "子 Agent 完成", Body: "写入完成",
	})

	var events []LiveEvent
	agent.WithLiveEvents(func(ev LiveEvent) {
		if ev.Kind == LiveEventMilestone || ev.Kind == LiveEventSubAgentDone {
			events = append(events, ev)
		}
	})

	if _, err := agent.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var milestone, done bool
	for _, ev := range events {
		switch ev.Kind {
		case LiveEventMilestone:
			milestone = ev.Tool == "meta/domain-1" && strings.Contains(ev.Text, "渲染链路已打通")
		case LiveEventSubAgentDone:
			done = ev.Tool == "meta/code_assistant-2"
		}
	}
	if !milestone {
		t.Fatalf("里程碑: 前缀 info 应推送 LiveEventMilestone, got %+v", events)
	}
	if !done {
		t.Fatalf("普通完成通知仍应推送 LiveEventSubAgentDone, got %+v", events)
	}
}
