package memory

// 导入 testing 包，用于编写单元测试。
import (
	"fmt"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// TestPipeline_WriteAndAssemble 验证 Pipeline 的 Write 与 Assemble 行为。
// 测试场景：先写入一条 tool_call 事件，再调用 Assemble 组装历史消息，
// 期望返回的历史长度比原始历史多一条 system 上下文消息（位于末尾，不破坏前缀缓存）。
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
	// 校验原始历史仍在前位（构成稳定前缀，供前缀缓存命中）。
	if out[0].Role != "user" || out[0].Content != "read the file" {
		t.Fatalf("expected original history at front, got role=%s content=%q", out[0].Role, out[0].Content)
	}
	// 校验末尾消息角色为 system，表示上下文注入成功。
	last := out[len(out)-1]
	if last.Role != "system" {
		t.Fatalf("expected injected system message at end, got %s", last.Role)
	}
	// 校验 system 消息内容非空。
	if last.Content == "" {
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

// TestPipeline_MaxEventsPerAgent 验证 Pipeline 的每 agent 事件容量上限。
// 测试场景：容量上限配置为 25，写入 30 条事件后，
// 期望内存中仅保留最新 25 条，最旧的 5 条被丢弃，且剩余事件顺序不变。
func TestPipeline_MaxEventsPerAgent(t *testing.T) {
	// 创建一个容量上限为 25 的 Pipeline 实例。
	pipe := NewPipeline(nil).WithMaxEventsPerAgent(25)
	// 连续写入 30 条事件，内容依次编号为 ev-0 到 ev-29。
	for i := 0; i < 30; i++ {
		if err := pipe.Write("agent-1", agent.MemoryEvent{Type: "answer", AgentID: "agent-1", Content: fmt.Sprintf("ev-%d", i)}); err != nil {
			// 写入失败直接终止测试，并输出错误信息。
			t.Fatalf("write failed: %v", err)
		}
	}

	// 取出内存中的事件副本，校验数量不超过容量上限。
	events := pipe.Events("agent-1")
	if len(events) != 25 {
		t.Fatalf("expected %d events after trim, got %d", 25, len(events))
	}
	// 校验最旧的 5 条已被丢弃，保留的第一条应为 ev-5。
	if events[0].Content != "ev-5" {
		t.Fatalf("expected oldest retained event 'ev-5', got %q", events[0].Content)
	}
	// 校验最新一条为 ev-29，验证追加顺序未被破坏。
	if events[24].Content != "ev-29" {
		t.Fatalf("expected latest event 'ev-29', got %q", events[24].Content)
	}
}

// TestPipeline_MaxEventsPerAgent_AssembleLatest 验证容量裁减后 Assemble 仍能注入最新事件。
// 测试场景：容量上限 25、注入上限 3，写入 30 条事件后调用 Assemble，
// 期望注入的 system 消息（位于末尾）包含最新事件 ev-29，且不包含已被丢弃的 ev-0。
func TestPipeline_MaxEventsPerAgent_AssembleLatest(t *testing.T) {
	// 创建一个容量上限为 25、注入上限为 3 的 Pipeline 实例。
	pipe := NewPipeline(nil).WithMaxEventsPerAgent(25).WithLimit(3)
	// 连续写入 30 条事件，触发容量裁减。
	for i := 0; i < 30; i++ {
		if err := pipe.Write("agent-1", agent.MemoryEvent{Type: "answer", AgentID: "agent-1", Content: fmt.Sprintf("ev-%d", i)}); err != nil {
			// 写入失败直接终止测试，并输出错误信息。
			t.Fatalf("write failed: %v", err)
		}
	}

	// 构造原始用户历史消息。
	history := []agent.ReactMessage{{Role: "user", Content: "hi"}}
	// 调用 Assemble 注入近期事件上下文。
	out := pipe.Assemble(types.RoleDefinition{}, "agent-1", history)

	// 校验返回消息数量：应为原历史长度加 1 条 system 消息。
	if len(out) != len(history)+1 {
		t.Fatalf("expected %d messages, got %d", len(history)+1, len(out))
	}
	// 注入的 system 消息位于末尾（不破坏前缀缓存）。
	last := out[len(out)-1]
	// 校验注入的上下文包含最新事件 ev-29。
	if !strings.Contains(last.Content, "ev-29") {
		t.Fatalf("expected injected context to contain latest event 'ev-29', got %q", last.Content)
	}
	// 校验注入的上下文不包含注入窗口之前的 ev-26（注入上限为 3，只应包含 ev-27 及之后）。
	if strings.Contains(last.Content, "ev-26") {
		t.Fatalf("expected injected context to exclude event 'ev-26' beyond limit, got %q", last.Content)
	}
	// 校验已被容量裁减丢弃的 ev-0 不出现在上下文中。
	if strings.Contains(last.Content, "ev-0") {
		t.Fatalf("expected injected context to exclude trimmed event 'ev-0', got %q", last.Content)
	}
}

// TestPipeline_WithMaxEventsPerAgent_Fallback 验证容量上限的非法配置回退逻辑。
// 测试场景：分别传入 0 与 DefaultEventLimit，均不大于 DefaultEventLimit，
// 期望均回退到 DefaultMaxEventsPerAgent，维持容量上限大于注入上限的不变式。
func TestPipeline_WithMaxEventsPerAgent_Fallback(t *testing.T) {
	// 传入 0 时应回退到默认容量上限。
	if got := NewPipeline(nil).WithMaxEventsPerAgent(0).maxEventsPerAgent; got != DefaultMaxEventsPerAgent {
		t.Fatalf("expected fallback to %d, got %d", DefaultMaxEventsPerAgent, got)
	}
	// 传入 DefaultEventLimit 时同样应回退，保证 maxEventsPerAgent 始终大于 DefaultEventLimit。
	if got := NewPipeline(nil).WithMaxEventsPerAgent(DefaultEventLimit).maxEventsPerAgent; got != DefaultMaxEventsPerAgent {
		t.Fatalf("expected fallback to %d, got %d", DefaultMaxEventsPerAgent, got)
	}
	// 默认构造的 Pipeline 应直接使用 DefaultMaxEventsPerAgent。
	if got := NewPipeline(nil).maxEventsPerAgent; got != DefaultMaxEventsPerAgent {
		t.Fatalf("expected default %d, got %d", DefaultMaxEventsPerAgent, got)
	}
}

// TestInMemoryStore_MaxEventsPerAgent 验证 InMemoryStore 的每代理事件容量上限。
// 测试场景：容量上限配置为 25，写入 30 条事件后，
// 期望 LoadEvents 最多返回 25 条，且保留的是最新事件。
func TestInMemoryStore_MaxEventsPerAgent(t *testing.T) {
	// 创建一个容量上限为 25 的内存存储实例。
	store := NewInMemoryStore().WithMaxEventsPerAgent(25)
	// 从 testing.T 获取一个与测试生命周期绑定的 context。
	ctx := t.Context()
	// 连续写入 30 条事件，内容依次编号为 ev-0 到 ev-29。
	for i := 0; i < 30; i++ {
		_ = store.SaveEvent(ctx, "a", agent.MemoryEvent{Type: "answer", AgentID: "a", Content: fmt.Sprintf("ev-%d", i)})
	}

	// 读取全部事件，校验数量不超过容量上限。
	events, err := store.LoadEvents(ctx, "a", 100)
	// 校验读取未报错。
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	// 校验存储中仅保留最新 25 条事件。
	if len(events) != 25 {
		t.Fatalf("expected %d events after trim, got %d", 25, len(events))
	}
	// 校验最旧的 5 条已被丢弃，保留的第一条应为 ev-5。
	if events[0].Content != "ev-5" {
		t.Fatalf("expected oldest retained event 'ev-5', got %q", events[0].Content)
	}
	// 校验最新一条为 ev-29，验证追加顺序未被破坏。
	if events[24].Content != "ev-29" {
		t.Fatalf("expected latest event 'ev-29', got %q", events[24].Content)
	}
}
