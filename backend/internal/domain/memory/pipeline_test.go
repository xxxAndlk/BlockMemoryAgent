package memory

// 导入 testing 包，用于编写单元测试。
import (
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// TestPipeline_WriteAndAssemble 验证 Pipeline 的 Write 与 Assemble 行为。
// 测试场景：先写入一条 tool_call 事件，再调用 Assemble 组装历史消息，
// 期望返回的历史长度比原始历史多一条 system 上下文消息。
func TestPipeline_WriteAndAssemble(t *testing.T) {
	// 创建一个不带持久化存储的 Pipeline 实例。
	pipe := NewPipeline(nil)
	// 向 Pipeline 中写入一条 ReadFile 工具调用事件。
	if err := pipe.Write("agent-1", agent.MemoryEvent{Type: "tool_call", AgentID: "agent-1", ToolName: "ReadFile", Output: "hello"}); err != nil {
		// 写入失败直接终止测试，并输出错误信息。
		t.Fatalf("write failed: %v", err)
	}

	// 构造原始用户历史消息，仅包含一条用户输入。
	history := []agent.ReactMessage{{Role: "user", Content: "read the file"}}
	// 调用 Assemble 注入近期事件上下文。
	out := pipe.Assemble(types.RoleDefinition{}, "agent-1", history)

	// 校验返回消息数量：应为原历史长度加 1 条 system 消息。
	if len(out) != len(history)+1 {
		t.Fatalf("expected %d messages, got %d", len(history)+1, len(out))
	}
	// 校验第一条消息角色为 system，表示上下文注入成功。
	if out[0].Role != "system" {
		t.Fatalf("expected injected system message, got %s", out[0].Role)
	}
	// 校验 system 消息内容非空。
	if out[0].Content == "" {
		t.Fatal("expected non-empty context injection")
	}
}

// TestPipeline_Assemble_NoEvents 验证当没有事件时，Assemble 不会插入空的 system 消息。
func TestPipeline_Assemble_NoEvents(t *testing.T) {
	// 创建一个不带持久化存储的 Pipeline 实例。
	pipe := NewPipeline(nil)
	// 构造原始用户历史消息。
	history := []agent.ReactMessage{{Role: "user", Content: "hi"}}
	// 调用 Assemble，由于未写入任何事件，不应注入额外消息。
	out := pipe.Assemble(types.RoleDefinition{}, "agent-1", history)
	// 校验返回消息数量与原始历史一致。
	if len(out) != len(history) {
		t.Fatalf("expected history unchanged, got %d messages", len(out))
	}
}

// TestInMemoryStore 验证 InMemoryStore 的基本读写能力。
// 测试场景：向同一 agent 写入两条事件，再读取并校验数量与顺序。
func TestInMemoryStore(t *testing.T) {
	// 创建一个新的内存存储实例。
	store := NewInMemoryStore()
	// 从 testing.T 获取一个与测试生命周期绑定的 context。
	ctx := t.Context()
	// 向 agent "a" 写入第一条事件。
	_ = store.SaveEvent(ctx, "a", agent.MemoryEvent{Type: "answer", Content: "one"})
	// 向 agent "a" 写入第二条事件。
	_ = store.SaveEvent(ctx, "a", agent.MemoryEvent{Type: "answer", Content: "two"})
	// 向另一个 agent "b" 写入事件，用于验证数据隔离。
	_ = store.SaveEvent(ctx, "b", agent.MemoryEvent{Type: "answer", Content: "other"})

	// 从 agent "a" 读取最近最多 10 条事件。
	events, err := store.LoadEvents(ctx, "a", 10)
	// 校验读取未报错。
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	// 校验 agent "a" 只有两条事件。
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	// 校验最近一条事件的内容为 "two"，验证追加顺序。
	if events[1].Content != "two" {
		t.Fatalf("expected latest event 'two', got %q", events[1].Content)
	}
}
