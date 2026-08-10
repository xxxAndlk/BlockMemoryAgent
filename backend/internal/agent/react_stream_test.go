// Package agent 包含 ReActAgent 流式生成与实时事件的单元测试。
package agent

import (
	"context"
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
