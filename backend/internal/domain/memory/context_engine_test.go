package memory

// context_engine_test.go ContextEngine 分档槽位（TODO #22①）：快速档纯裁剪零 LLM、
// 集群档大文件探查摘要、QuarantineEngine 失败隔离降级 daily 留痕。

import (
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// bigToolHistory 构造 n 条大工具输出（每条 bigRunes+10）+ 末尾 keep 条小消息。
func bigToolHistory(n, bigRunes, keep int) []agent.ReactMessage {
	var out []agent.ReactMessage
	out = append(out, agent.ReactMessage{Role: "user", Content: "任务"})
	for i := 0; i < n; i++ {
		out = append(out, agent.ReactMessage{Role: "tool", Content: strings.Repeat("x", bigRunes+10)})
	}
	for i := 0; i < keep; i++ {
		out = append(out, agent.ReactMessage{Role: "assistant", Content: "近况"})
	}
	return out
}

// TestFastTrimEngine_ZeroLLMStubs 验证快速档：旧 tool result >200 字符→stub+截断（零 LLM），
// 近保留段不动；条数不变（契约）；二次执行幂等（不重复改写）。
func TestFastTrimEngine_ZeroLLMStubs(t *testing.T) {
	p := NewPipeline(nil).WithCompression(5) // 显式保留段=5，旧区域可被 stub
	eng := NewFastTrimEngine(p)
	h := bigToolHistory(3, 300, 5)
	out := eng.AfterTurn("a", h)

	if len(out) != len(h) {
		t.Fatalf("contract: message count must not change (%d -> %d)", len(h), len(out))
	}
	// 旧区域被 stub（300+10 > 200）。
	if !strings.Contains(out[1].Content, "工具输出已裁剪") {
		t.Fatalf("old tool result must be stubbed, got %q", truncate(out[1].Content, 80))
	}
	if n := len([]rune(out[1].Content)); n <= 200 || n >= 310 {
		t.Fatalf("stub should keep head 200 runes + marker (~240 total), got %d runes", n)
	}
	// 近保留段（5 条 assistant）不动。
	last := out[len(out)-1]
	if last.Content != "近况" {
		t.Fatalf("recent tail must stay intact, got %q", last.Content)
	}
	// 幂等：二次执行不再改写（前缀缓存稳定）。
	again := eng.AfterTurn("a", out)
	if again[1].Content != out[1].Content {
		t.Fatalf("stub must be idempotent, got %q", again[1].Content)
	}

	// 零 LLM：Compact 不 panic（截断包路径，无摘要器）。
	eng.Compact("a", h)
}

// TestDAGEngine_ProbeBigFiles 验证集群档大文件外置：>25K tok 工具输出换 ~200 tok 探查摘要，
// 条数不变、近保留段不动。
func TestDAGEngine_ProbeBigFiles(t *testing.T) {
	eng := NewDAGEngine(NewPipeline(nil).WithCompression(4))
	h := bigToolHistory(2, dagBigFileRunes+100, 4)
	out := eng.Assemble("a", h)

	if len(out) != len(h) {
		t.Fatalf("contract: count must not change (%d -> %d)", len(h), len(out))
	}
	if !strings.Contains(out[1].Content, "【探查摘要】") || !strings.Contains(out[1].Content, "全文请 ReadFile") {
		t.Fatalf("big file must become probe summary, got %q", truncate(out[1].Content, 120))
	}
	if r := len([]rune(out[1].Content)); r > dagProbeRunes*2 {
		t.Fatalf("probe summary must be ~%d runes, got %d", dagProbeRunes, r)
	}
}

// TestQuarantineEngine_DegradesOnPanic 验证失败隔离：引擎 panic/契约失败→该 Agent 隔离、
// 降级 daily 引擎、engine_quarantine 事件留痕不静默。
func TestQuarantineEngine_DegradesOnPanic(t *testing.T) {
	store := NewInMemoryStore()
	p := NewPipeline(store)
	daily := NewSafeguardEngine(p)

	boom := &panicEngine{}
	q := NewQuarantineEngine(p, func(string) ContextEngine { return boom }, daily)

	h := []agent.ReactMessage{{Role: "user", Content: "hi"}}
	out := q.Assemble("a", h) // panic → 隔离 + fallback
	if len(out) != 1 || out[0].Content != "hi" {
		t.Fatalf("fallback must return original view, got %+v", out)
	}
	if !q.isQuarantined("a") {
		t.Fatal("agent must be quarantined after panic")
	}
	// 二次调用直接走 fallback（不再触碰 boom）。
	out2 := q.AfterTurn("a", h)
	if len(out2) != 1 {
		t.Fatalf("post-quarantine call must use fallback, got %+v", out2)
	}
	// 留痕：engine_quarantine 事件已落 store。
	events, _ := store.LoadEvents(t.Context(), "a", 50)
	found := false
	for _, ev := range events {
		if ev.Type == "engine_quarantine" {
			found = true
		}
	}
	if !found {
		t.Fatal("engine_quarantine event must be persisted (报错留痕不静默)")
	}
}

// TestQuarantineEngine_ContractViolation 验证契约失败（条数变化）同样触发隔离降级。
func TestQuarantineEngine_ContractViolation(t *testing.T) {
	p := NewPipeline(nil)
	daily := NewSafeguardEngine(p)
	q := NewQuarantineEngine(p, func(string) ContextEngine { return &shrinkEngine{} }, daily)

	h := []agent.ReactMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "ok"}}
	out := q.Assemble("a", h)
	if len(out) != len(h) {
		t.Fatalf("fallback must keep count, got %d", len(out))
	}
	if !q.isQuarantined("a") {
		t.Fatal("contract violation must quarantine the agent")
	}
}

// panicEngine 每个视图钩子都 panic。
type panicEngine struct{}

func (panicEngine) Name() string { return "boom" }
func (panicEngine) Assemble(string, []agent.ReactMessage) []agent.ReactMessage {
	panic("boom assemble")
}
func (panicEngine) Compact(string, []agent.ReactMessage) { panic("boom compact") }
func (panicEngine) AfterTurn(string, []agent.ReactMessage) []agent.ReactMessage {
	panic("boom afterTurn")
}
func (panicEngine) PrepareSubagentSpawn(string, string) string { panic("boom spawn") }
func (panicEngine) OnSubagentEnded(string, string) string      { panic("boom end") }

// shrinkEngine 违反条数不变契约（丢消息）。
type shrinkEngine struct{}

func (shrinkEngine) Name() string { return "shrink" }
func (shrinkEngine) Assemble(_ string, h []agent.ReactMessage) []agent.ReactMessage {
	if len(h) > 0 {
		return h[:1]
	}
	return h
}
func (shrinkEngine) Compact(string, []agent.ReactMessage)                      {}
func (shrinkEngine) AfterTurn(_ string, h []agent.ReactMessage) []agent.ReactMessage { return h }
func (shrinkEngine) PrepareSubagentSpawn(string, string) string                { return "" }
func (shrinkEngine) OnSubagentEnded(_, r string) string                        { return r }

// TestEngineAssemble_InjectsThroughPipeline 验证引擎接线：Assemble 走引擎视图变换
// （快速档 stub 进请求视图）且条数契约保持。
func TestEngineAssemble_InjectsThroughPipeline(t *testing.T) {
	p := NewPipeline(nil).WithCompression(3)
	p.WithContextEngine(NewFastTrimEngine(p))
	h := bigToolHistory(2, 300, 3)
	out := p.Assemble(types.RoleDefinition{ID: "code_assistant"}, "a", h)
	// 注入事件/文件地图可能追加 system 消息——只断言旧工具输出已 stub。
	if !strings.Contains(out[1].Content, "工具输出已裁剪") {
		t.Fatalf("engine Assemble must stub old tool output, got %q", truncate(out[1].Content, 80))
	}
}
