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

// TestPipeline_Compression 验证 Pipeline 的历史压缩（hot/cold 分层）。
// 消息数超过 keepRecent+2 且步频命中时，中段被压缩为摘要消息。
func TestPipeline_Compression(t *testing.T) {
	pipe := NewPipeline(nil).WithCompression(1, 3) // 每步都压缩，保留最近 3 条

	// 构造 10 条历史：1 个 user（任务目标）+ 8 个 assistant/user 中段 + 1 个 user 末尾。
	history := []agent.ReactMessage{{Role: "user", Content: "task goal"}}
	for i := 0; i < 8; i++ {
		history = append(history, agent.ReactMessage{
			Role:    "assistant",
			Content: fmt.Sprintf("mid %d", i),
		})
		history = append(history, agent.ReactMessage{
			Role:    "user",
			Content: fmt.Sprintf("reply %d", i),
		})
	}

	out := pipe.Assemble(types.RoleDefinition{}, "agent-1", history)
	if len(out) >= len(history) {
		t.Fatalf("expected compressed output shorter than input, got %d vs %d", len(out), len(history))
	}

	// 首条 user（任务目标）必须保留。
	if out[0].Role != "user" || out[0].Content != "task goal" {
		t.Fatalf("expected first user preserved, got role=%s content=%q", out[0].Role, out[0].Content)
	}

	// 应有 system 摘要消息。
	var foundSummary bool
	for _, m := range out {
		if m.Role == "system" && strings.Contains(m.Content, "【历史压缩摘要】") {
			foundSummary = true
			break
		}
	}
	if !foundSummary {
		t.Error("expected compressed summary system message")
	}
}

// TestPipeline_CompressionDisabled 验证 compressEvery<=0 时关闭压缩。
func TestPipeline_CompressionDisabled(t *testing.T) {
	pipe := NewPipeline(nil) // 未配置 WithCompression

	history := []agent.ReactMessage{{Role: "user", Content: "task"}}
	for i := 0; i < 20; i++ {
		history = append(history, agent.ReactMessage{Role: "assistant", Content: fmt.Sprintf("m %d", i)})
	}
	out := pipe.Assemble(types.RoleDefinition{}, "agent-1", history)
	if len(out) != len(history) {
		t.Fatalf("expected no compression when disabled, got %d vs %d", len(out), len(history))
	}
}

// TestPipeline_CompressionStepFrequency 验证压缩仅在步频命中时触发。
func TestPipeline_CompressionStepFrequency(t *testing.T) {
	pipe := NewPipeline(nil).WithCompression(3, 2) // 每 3 步压缩一次

	history := []agent.ReactMessage{{Role: "user", Content: "task"}}
	for i := 0; i < 15; i++ {
		history = append(history, agent.ReactMessage{Role: "assistant", Content: fmt.Sprintf("m %d", i)})
	}

	// 第 1 步：不压缩。
	out1 := pipe.Assemble(types.RoleDefinition{}, "a", history)
	if len(out1) != len(history) {
		t.Fatalf("step 1 should not compress, got %d vs %d", len(out1), len(history))
	}
	// 第 2 步：不压缩。
	out2 := pipe.Assemble(types.RoleDefinition{}, "a", history)
	if len(out2) != len(history) {
		t.Fatalf("step 2 should not compress, got %d vs %d", len(out2), len(history))
	}
	// 第 3 步：压缩。
	out3 := pipe.Assemble(types.RoleDefinition{}, "a", history)
	if len(out3) >= len(history) {
		t.Fatalf("step 3 should compress, got %d vs %d", len(out3), len(history))
	}
}

// TestPipeline_CompressionTokenThreshold 验证压缩按上下文 token 阈值触发（主，替换步频语义）：
// 估算视图 token >= 阈值即压缩（保留近 keepRecent，旧压成上下文内摘要块）；per-role 阈值覆盖默认；
// 未达阈值不压缩。步频（WithCompression）为兜底，二者独立。
func TestPipeline_CompressionTokenThreshold(t *testing.T) {
	est := func(msgs []agent.ReactMessage) int { return len(msgs) * 1000 } // 1 条 = 1000 tokens
	pipe := NewPipeline(nil).
		WithCompression(0, 3). // 步频关闭，纯 token 触发
		WithContextBudget(5000, map[string]int{"meta": 100000}). // 默认 5000；meta 例外 100000
		WithTokenEstimator(est)

	small := []agent.ReactMessage{{Role: "user", Content: "task"}}
	small = append(small, agent.ReactMessage{Role: "assistant", Content: "m1"}, agent.ReactMessage{Role: "assistant", Content: "m2"})
	// 3 条 = 3000 < 5000：不压缩。
	if out := pipe.Assemble(types.RoleDefinition{ID: "domain"}, "a", small); len(out) != len(small) {
		t.Fatalf("below threshold should not compress, got %d vs %d", len(out), len(small))
	}

	big := []agent.ReactMessage{{Role: "user", Content: "task"}}
	for i := 0; i < 5; i++ {
		big = append(big, agent.ReactMessage{Role: "assistant", Content: fmt.Sprintf("m%d", i)})
	}
	// 6 条 = 6000 >= 5000：压缩（domain 走默认阈值）。
	out := pipe.Assemble(types.RoleDefinition{ID: "domain"}, "b", big)
	if len(out) >= len(big) {
		t.Fatalf("above threshold should compress, got %d vs %d", len(out), len(big))
	}
	var found bool
	for _, m := range out {
		if m.Role == "system" && strings.Contains(m.Content, "【历史压缩摘要】") {
			found = true
		}
	}
	if !found {
		t.Error("expected compressed summary after token threshold trigger")
	}
	// meta 角色阈值 100000：同一大历史（6000 < 100000）不压缩。
	if out := pipe.Assemble(types.RoleDefinition{ID: "meta"}, "c", big); len(out) != len(big) {
		t.Fatalf("meta per-role threshold should not compress, got %d vs %d", len(out), len(big))
	}
	// 阈值 <=0（WithContextBudget(0, ...)）时关闭 token 触发：不压缩。
	pipe2 := NewPipeline(nil).WithContextBudget(0, nil).WithTokenEstimator(est)
	if out := pipe2.Assemble(types.RoleDefinition{ID: "domain"}, "d", big); len(out) != len(big) {
		t.Fatalf("contextBudget<=0 should disable token trigger, got %d vs %d", len(out), len(big))
	}
}

// TestPipeline_CompressionFrozenView 验证压缩视图在两次触发之间冻结：
// 非压缩轮复用同一摘要与保留段起点，history 尾部追加的消息原样跟在保留段后，
// 视图前缀字节级稳定（DeepSeek 前缀缓存仅压缩轮失效，其余轮次全命中）。
func TestPipeline_CompressionFrozenView(t *testing.T) {
	pipe := NewPipeline(nil).WithCompression(3, 2) // 每 3 步压缩一次，保留最近 2 条

	history := []agent.ReactMessage{{Role: "user", Content: "task"}}
	for i := 0; i < 15; i++ {
		history = append(history, agent.ReactMessage{Role: "assistant", Content: fmt.Sprintf("m %d", i)})
	}

	// 第 1、2 步不触发压缩；第 3 步触发，拿到冻结视图。
	pipe.Assemble(types.RoleDefinition{}, "a", history)
	pipe.Assemble(types.RoleDefinition{}, "a", history)
	frozen := pipe.Assemble(types.RoleDefinition{}, "a", history)
	if len(frozen) >= len(history) {
		t.Fatalf("step 3 should compress, got %d vs %d", len(frozen), len(history))
	}

	// history 尾部追加 2 条（模拟一轮 assistant + tool），第 4 步不触发压缩。
	history = append(history,
		agent.ReactMessage{Role: "assistant", Content: "new turn"},
		agent.ReactMessage{Role: "tool", Content: "new result"},
	)
	out := pipe.Assemble(types.RoleDefinition{}, "a", history)

	// 视图长度 = 冻结视图 + 2 条追加。
	if len(out) != len(frozen)+2 {
		t.Fatalf("expected frozen view + 2 appended, got %d vs %d", len(out), len(frozen)+2)
	}
	// 前缀（含压缩摘要）必须与冻结视图逐条一致——前缀缓存命中的充要条件。
	for i := range frozen {
		if out[i].Role != frozen[i].Role || out[i].Content != frozen[i].Content {
			t.Fatalf("frozen prefix changed at %d: %q vs %q", i, frozen[i].Content, out[i].Content)
		}
	}
	// 追加的消息原样出现在末尾。
	if out[len(out)-2].Content != "new turn" || out[len(out)-1].Content != "new result" {
		t.Fatalf("appended messages should be at tail, got %q / %q", out[len(out)-2].Content, out[len(out)-1].Content)
	}

	// 第 6 步再次触发压缩：摘要覆盖范围推进，冻结视图更新。
	history = append(history, agent.ReactMessage{Role: "assistant", Content: "m 15"})
	pipe.Assemble(types.RoleDefinition{}, "a", history) // 第 5 步
	refrozen := pipe.Assemble(types.RoleDefinition{}, "a", history)
	var foundNew bool
	for _, m := range refrozen {
		if m.Role == "system" && strings.Contains(m.Content, "new turn") {
			foundNew = true
		}
	}
	if !foundNew {
		t.Error("second compression should fold the appended messages into the summary")
	}
}

// TestPipeline_RawJoinTruncated 摘要器缺失（raw join 路径）时注入文本超上限截断（TODO #33）：
// 防"摘要两次失败降级 raw join → MetaAgent 上下文膨胀"事故；截断保留上限内前缀 + 标记。
func TestPipeline_RawJoinTruncated(t *testing.T) {
	pipe := NewPipeline(nil).WithLimit(100) // 无 summarizer：走 raw join；放宽注入上限制造超限
	for i := 0; i < 100; i++ {
		big := fmt.Sprintf("event %d: %s", i, strings.Repeat("很长的事件输出内容", 120))
		if err := pipe.Write("agent-1", agent.MemoryEvent{Type: "tool_call", AgentID: "agent-1", ToolName: "ReadFile", Output: big}); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	history := []agent.ReactMessage{{Role: "user", Content: "t"}}
	out := pipe.Assemble(types.RoleDefinition{}, "agent-1", history)
	last := out[len(out)-1]
	if last.Role != "system" {
		t.Fatalf("expected injected system message, got %s", last.Role)
	}
	runes := []rune(last.Content)
	if len(runes) > maxRawEventJoinRunes+100 {
		t.Fatalf("raw join should be truncated near %d, got %d runes", maxRawEventJoinRunes, len(runes))
	}
	if !strings.Contains(last.Content, "已截断") {
		t.Fatalf("truncated body should carry 已截断 marker")
	}
	if !strings.Contains(last.Content, "【近期事件】") {
		t.Fatalf("body should keep 近期事件 header")
	}
}

// TestPipeline_Compression_UserMessageLongerBudget 中段 user 消息保前 500 字符（TODO #34/#35 结论）：
// 多轮会话第二条用户指令（如"重新执行/自检"）语义不可压——压缩摘要中 user 行保留 >200 字符，
// assistant/tool 行仍按 200 上限。
func TestPipeline_Compression_UserMessageLongerBudget(t *testing.T) {
	longUser := strings.Repeat("用户的第二条长指令内容", 40) // 480 chars，超 200 上限
	longAssistant := strings.Repeat("a", 600)
	history := []agent.ReactMessage{
		{Role: "user", Content: "目标"},
		{Role: "user", Content: longUser},
		{Role: "assistant", Content: longAssistant},
	}
	for i := 0; i < 12; i++ {
		history = append(history, agent.ReactMessage{Role: "assistant", Content: "recent work"})
		history = append(history, agent.ReactMessage{Role: "tool", Content: "tool result"})
	}
	summary, _, ok := compressMiddle(history, 6)
	if !ok {
		t.Fatal("expected compression to trigger")
	}
	// 提取 user 行与 assistant 行内容（"- [role] content" 格式）。
	var userLine, assistantLine string
	for _, line := range strings.Split(summary, "\n") {
		if strings.HasPrefix(line, "- [user] ") {
			userLine = strings.TrimPrefix(line, "- [user] ")
		}
		if strings.HasPrefix(line, "- [assistant] ") {
			assistantLine = strings.TrimPrefix(line, "- [assistant] ")
		}
	}
	if userLine == "" {
		t.Fatalf("user line missing from summary: %q", summary)
	}
	if n := len([]rune(userLine)); n <= 200 {
		t.Fatalf("user message should keep up to 500 chars, got %d", n)
	}
	if n := len([]rune(assistantLine)); n > 201 {
		t.Fatalf("assistant message should stay capped at 200, got %d", n)
	}
}
