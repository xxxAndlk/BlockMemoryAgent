package memory

// pyramid_test.go 验证层级压缩（压缩金字塔）：增量压缩包累积、超限合并最老一半、
// 截断降级、状态落库与重启懒加载、事件懒加载。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// makeHistory 构造 1 条 user 任务目标 + n 条 assistant 消息的测试历史。
func makeHistory(n int) []agent.ReactMessage {
	history := []agent.ReactMessage{{Role: "user", Content: "task goal"}}
	for i := 0; i < n; i++ {
		history = append(history, agent.ReactMessage{Role: "assistant", Content: fmt.Sprintf("m %d", i)})
	}
	return history
}

// findSummary 返回视图中的压缩摘要 system 消息正文；未找到返回 ("", false)。
func findSummary(out []agent.ReactMessage) (string, bool) {
	for _, m := range out {
		if m.Role == "system" && strings.Contains(m.Content, "【历史压缩摘要】") {
			return m.Content, true
		}
	}
	return "", false
}

// TestPipeline_PyramidIncrementalBundles 验证增量层级压缩：
// 每次触发只把"新滑出保留段"的中段压成一个新压缩包（不重复压旧段），
// 多个压缩包按时间从旧到新同时出现在摘要消息中。
func TestPipeline_PyramidIncrementalBundles(t *testing.T) {
	var inputs []string
	call := 0
	// LLM 摘要桩：记录输入文本，返回可识别的包标记。
	stub := func(_ context.Context, text string, merge bool) (string, error) {
		call++
		if merge {
			t.Fatalf("unexpected merge call")
		}
		inputs = append(inputs, text)
		return fmt.Sprintf("PACK%d", call), nil
	}
	pipe := NewPipeline(nil).WithCompression(1, 3).WithHistorySummarizer(stub) // 每步压缩，保留最近 3 条

	// 第 1 次触发：10 条历史，保留 3 条，压缩段 = m0..m5。
	history := makeHistory(9)
	out := pipe.Assemble(types.RoleDefinition{}, "a", history)
	sum, ok := findSummary(out)
	if !ok {
		t.Fatalf("expected compressed summary after first trigger")
	}
	if !strings.Contains(sum, "PACK1") {
		t.Fatalf("summary should contain PACK1, got %q", sum)
	}
	if !strings.Contains(inputs[0], "m 0") || !strings.Contains(inputs[0], "m 5") {
		t.Fatalf("first segment should cover m0..m5, got %q", inputs[0])
	}

	// history 尾部追加 5 条（m9..m13），第 2 次触发：压缩段 = m6..m10（不含已压过的旧段）。
	for i := 9; i < 14; i++ {
		history = append(history, agent.ReactMessage{Role: "assistant", Content: fmt.Sprintf("m %d", i)})
	}
	out = pipe.Assemble(types.RoleDefinition{}, "a", history)
	sum, ok = findSummary(out)
	if !ok {
		t.Fatalf("expected compressed summary after second trigger")
	}
	if !strings.Contains(sum, "PACK1") || !strings.Contains(sum, "PACK2") {
		t.Fatalf("summary should contain both PACK1 and PACK2, got %q", sum)
	}
	if call != 2 {
		t.Fatalf("expected 2 summarizer calls, got %d", call)
	}
	if strings.Contains(inputs[1], "m 0") {
		t.Fatalf("second segment should NOT re-cover old messages, got %q", inputs[1])
	}
	if !strings.Contains(inputs[1], "m 6") || !strings.Contains(inputs[1], "m 10") {
		t.Fatalf("second segment should cover m6..m10, got %q", inputs[1])
	}

	// 视图应保持紧凑：首条 user + 摘要 + 保留段 ≪ 全量历史。
	if len(out) >= len(history) {
		t.Fatalf("compressed view should be shorter than history, got %d vs %d", len(out), len(history))
	}
	if out[0].Role != "user" || out[0].Content != "task goal" {
		t.Fatalf("first user goal must be preserved, got %q", out[0].Content)
	}
}

// TestPipeline_PyramidMergeOldestHalf 验证压缩包超上限时合并最老的一半：
// 上限 2 时第 3 个包触发合并（merge=true），包数收敛回上限内，且下一轮继续收敛。
func TestPipeline_PyramidMergeOldestHalf(t *testing.T) {
	call := 0
	mergeCalls := 0
	stub := func(_ context.Context, text string, merge bool) (string, error) {
		call++
		if merge {
			mergeCalls++
			return fmt.Sprintf("MERGED%d", mergeCalls), nil
		}
		return fmt.Sprintf("PACK%d", call), nil
	}
	pipe := NewPipeline(nil).WithCompression(1, 3).WithMaxBundles(2).WithHistorySummarizer(stub)

	// 逐轮追加 3 条并触发压缩，共 4 轮 → 产生 4 个包的压缩需求，上限 2。
	history := makeHistory(6)
	for round := 0; round < 4; round++ {
		base := len(history) - 1 // 已用 assistant 条数
		for i := base; i < base+3; i++ {
			history = append(history, agent.ReactMessage{Role: "assistant", Content: fmt.Sprintf("m %d", i)})
		}
		pipe.Assemble(types.RoleDefinition{}, "a", history)
	}

	pipe.mu.RLock()
	st := pipe.compressStates["a"]
	pipe.mu.RUnlock()
	if len(st.Bundles) > 2 {
		t.Fatalf("bundles should converge to <= 2, got %d", len(st.Bundles))
	}
	if mergeCalls == 0 {
		t.Fatalf("expected at least one merge call")
	}
	// 最老的内容经合并保留在首包（更粗粒度），最新的包保持原样。
	if !strings.Contains(st.Bundles[0], "MERGED") {
		t.Fatalf("oldest bundle should be a merged one, got %q", st.Bundles[0])
	}
}

// TestPipeline_PyramidFallbackMultiBundles 验证无 LLM 摘要器时的截断降级：
// 每次触发产生一个截断压缩段，多段同时出现在摘要消息中（不再互相覆盖）。
func TestPipeline_PyramidFallbackMultiBundles(t *testing.T) {
	pipe := NewPipeline(nil).WithCompression(1, 3) // 每步压缩，保留最近 3 条，无摘要器

	history := makeHistory(9)
	pipe.Assemble(types.RoleDefinition{}, "a", history)
	for i := 9; i < 14; i++ {
		history = append(history, agent.ReactMessage{Role: "assistant", Content: fmt.Sprintf("m %d", i)})
	}
	out := pipe.Assemble(types.RoleDefinition{}, "a", history)

	sum, ok := findSummary(out)
	if !ok {
		t.Fatalf("expected compressed summary")
	}
	if !strings.Contains(sum, "▶ 第 1 段") || !strings.Contains(sum, "▶ 第 2 段") {
		t.Fatalf("summary should render two bundles, got %q", sum)
	}
	// 截断降级保留各段内容的可识别前缀。
	if !strings.Contains(sum, "m 0") || !strings.Contains(sum, "m 6") {
		t.Fatalf("fallback bundles should retain truncated content, got %q", sum)
	}
}

// TestPipeline_PyramidMergeFallbackConcat 验证合并的降级路径（无摘要器）：
// 超上限时最老的一半直接拼接为一个包，包数收敛、内容不丢（截断形式保留）。
func TestPipeline_PyramidMergeFallbackConcat(t *testing.T) {
	pipe := NewPipeline(nil).WithCompression(1, 3).WithMaxBundles(2) // 无摘要器

	history := makeHistory(6)
	for round := 0; round < 4; round++ {
		base := len(history) - 1
		for i := base; i < base+3; i++ {
			history = append(history, agent.ReactMessage{Role: "assistant", Content: fmt.Sprintf("m %d", i)})
		}
		pipe.Assemble(types.RoleDefinition{}, "a", history)
	}

	pipe.mu.RLock()
	st := pipe.compressStates["a"]
	pipe.mu.RUnlock()
	if len(st.Bundles) > 2 {
		t.Fatalf("bundles should converge to <= 2 without summarizer, got %d", len(st.Bundles))
	}
	// 拼接降级：首包应同时含最老两段的可识别内容。
	if !strings.Contains(st.Bundles[0], "m 0") {
		t.Fatalf("concat fallback should retain oldest content, got %q", st.Bundles[0])
	}
}

// TestPipeline_CompressStatePersistReload 验证压缩状态落库与重启懒加载：
// pipe1 压缩后状态写入 store；新 pipe2（模拟进程重启）同 store 首次 Assemble
// 即恢复压缩视图（无需摘要器、无需重新触发压缩）。
func TestPipeline_CompressStatePersistReload(t *testing.T) {
	store := NewInMemoryStore()
	stub := func(_ context.Context, _ string, _ bool) (string, error) { return "PACK-RESTART", nil }
	pipe1 := NewPipeline(store).WithCompression(1, 3).WithHistorySummarizer(stub)

	history := makeHistory(9)
	out1 := pipe1.Assemble(types.RoleDefinition{}, "a", history)
	if _, ok := findSummary(out1); !ok {
		t.Fatalf("pipe1 should compress")
	}

	// 模拟重启：全新 Pipeline，内存无状态，经 store 懒加载恢复。
	pipe2 := NewPipeline(store).WithCompression(0, 3) // 步频关闭，仅靠懒加载出视图
	out2 := pipe2.Assemble(types.RoleDefinition{}, "a", history)
	sum, ok := findSummary(out2)
	if !ok {
		t.Fatalf("pipe2 should restore compressed view from store")
	}
	if !strings.Contains(sum, "PACK-RESTART") {
		t.Fatalf("restored summary should contain persisted bundle, got %q", sum)
	}
	if len(out2) != len(out1) {
		t.Fatalf("restored view should match pre-restart view, got %d vs %d", len(out2), len(out1))
	}
}

// TestPipeline_EventsLazyReload 验证重启后事件懒加载：
// pipe1 写入的事件落 store；新 pipe2（模拟重启）内存为空时首次 Assemble
// 即注入【近期事件】，不再重启即失忆。
func TestPipeline_EventsLazyReload(t *testing.T) {
	store := NewInMemoryStore()
	pipe1 := NewPipeline(store)
	if err := pipe1.Write("agent-1", agent.MemoryEvent{Type: "answer", AgentID: "agent-1", Content: "hello restart"}); err != nil {
		t.Fatalf("write: %v", err)
	}

	pipe2 := NewPipeline(store)
	out := pipe2.Assemble(types.RoleDefinition{}, "agent-1", []agent.ReactMessage{{Role: "user", Content: "t"}})
	last := out[len(out)-1]
	if last.Role != "system" || !strings.Contains(last.Content, "【近期事件】") {
		t.Fatalf("expected injected events after lazy reload, got %+v", last)
	}
	if !strings.Contains(last.Content, "hello restart") {
		t.Fatalf("reloaded events should contain pre-restart content, got %q", last.Content)
	}
}

// TestFormatSegmentText_TruncatesLongMessages 验证压缩输入的单条截断：user/assistant
// 超 1000 runes、tool 超 800 runes 被截断并带省略号（防 lightweight 摘要模型 400，
// 2026-08-28 doubao-seed-2.0-mini "Total tokens of image and text exceed max message tokens" 实证）。
func TestFormatSegmentText_TruncatesLongMessages(t *testing.T) {
	long := strings.Repeat("字", 3000)
	segment := []agent.ReactMessage{
		{Role: "user", Content: long},
		{Role: "assistant", Content: long},
		{Role: "tool", Content: long},
	}
	out := formatSegmentText(segment)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	if n := len([]rune(lines[0])); n > len("[user] ")+segmentTextMsgRunes+1 {
		t.Fatalf("user line should be truncated to %d runes, got %d", segmentTextMsgRunes, n)
	}
	if n := len([]rune(lines[2])); n > len("[tool] ")+segmentTextToolMsgRunes+1 {
		t.Fatalf("tool line should be truncated to %d runes, got %d", segmentTextToolMsgRunes, n)
	}
	if !strings.Contains(lines[0], "…") {
		t.Fatalf("truncated line should carry ellipsis, got %q", lines[0])
	}
}

// TestFormatSegmentText_TotalCapDropsOldest 验证段总量上限：超限从最老消息开始丢弃，
// 保留最新并加省略标记（20000 runes/条 × 30 条/段 ≈ 60 万字符必超 lightweight 模型上下文）。
func TestFormatSegmentText_TotalCapDropsOldest(t *testing.T) {
	segment := make([]agent.ReactMessage, 0, 30)
	for i := 0; i < 30; i++ {
		segment = append(segment, agent.ReactMessage{Role: "assistant", Content: fmt.Sprintf("MSG%02d-", i) + strings.Repeat("x", 900)})
	}
	out := formatSegmentText(segment)
	if n := len([]rune(out)); n > maxSegmentTextRunes+200 { // 省略标记占少量额度
		t.Fatalf("total output should be capped near %d runes, got %d", maxSegmentTextRunes, n)
	}
	if !strings.Contains(out, "省略") {
		t.Fatalf("expected elision marker for dropped oldest messages, got prefix %q", string([]rune(out)[:60]))
	}
	if !strings.Contains(out, "MSG29-") {
		t.Fatalf("newest message must be kept")
	}
	if strings.Contains(out, "MSG00-") {
		t.Fatalf("oldest message should have been dropped")
	}
}
