package memory

// compaction_test.go 压缩生命周期钩子（TODO #20②+#21）：压缩前快照落底账、
// 热改动回灌冻结进视图、compaction 审计事件直写 store、空摘要质量门重试。

import (
	"context"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// TestCompactionHooks_SnapshotAndReinject 验证钩子语义：
// 压缩前快照随 compaction 事件落 store（Input=快照、Content=摘要本体、Output=体积指标）；
// 热改动回灌文本冻结进视图（稳定前缀段）；compaction 事件不进内存事件流（不渲染进【近期事件】）。
func TestCompactionHooks_SnapshotAndReinject(t *testing.T) {
	store := NewInMemoryStore()
	snapCalls, reinjectCalls := 0, 0
	snapshot := func(agentID string) string {
		snapCalls++
		return "【任务账本】demo-ledger for " + agentID
	}
	reinject := func(agentID string, files []string) string {
		reinjectCalls++
		return "【热改动回灌】files=" + strings.Join(files, ",")
	}
	pipe := NewPipeline(store).
		WithCompression(3).
		WithContextBudget(1, nil). // 必超阈值每次装配都触发
		WithTokenEstimator(alwaysOverEstimator).
		WithHistorySummarizer(func(_ context.Context, text string, merge bool) (string, error) {
			return "PACK", nil
		}).
		WithCompactionHooks(snapshot, reinject)

	history := makeHistory(9)
	out := pipe.Assemble(types.RoleDefinition{}, "a", history)

	if snapCalls == 0 || reinjectCalls == 0 {
		t.Fatalf("hooks must fire on compaction: snap=%d reinject=%d", snapCalls, reinjectCalls)
	}
	// 热改动回灌进视图（稳定前缀段，history 尾部之前）。
	joined := ""
	for _, m := range out {
		joined += m.Content + "\n"
	}
	if !strings.Contains(joined, "【热改动回灌】") {
		t.Fatalf("hot reinject must be frozen into view, got:\n%s", joined)
	}
	if !strings.Contains(joined, "files=") {
		t.Fatalf("reinject provider output must appear, got:\n%s", joined)
	}
	// compaction 事件直写 store（审计），不进内存事件流（不污染【近期事件】/前缀）。
	events, err := store.LoadEvents(context.Background(), "a", 100)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == "compaction" {
			found = true
			if !strings.Contains(ev.Input, "demo-ledger") {
				t.Errorf("compaction Input should carry pre-compact snapshot, got %q", ev.Input)
			}
			if ev.Content != "PACK" {
				t.Errorf("compaction Content should carry summary body, got %q", ev.Content)
			}
			if !strings.Contains(ev.Output, "before_msgs=") {
				t.Errorf("compaction Output should carry size audit, got %q", ev.Output)
			}
		}
	}
	if !found {
		t.Fatal("compaction event must be persisted to store")
	}
	if len(pipe.Events("a")) != 0 {
		t.Fatalf("compaction event must NOT enter in-memory event stream, got %d", len(pipe.Events("a")))
	}
}

// TestSummarizeSegment_EmptyRetry 验证质量门（TODO #21③）：空摘要重试一次，
// 第二次返非空即采纳（不落截断降级）；两次全空才降级。
func TestSummarizeSegment_EmptyRetry(t *testing.T) {
	calls := 0
	stub := func(_ context.Context, text string, merge bool) (string, error) {
		calls++
		if calls == 1 {
			return "   ", nil // 空摘要：触发重试
		}
		return "RECOVERED", nil
	}
	pipe := NewPipeline(nil).WithHistorySummarizer(stub)
	got := pipe.summarizeSegment("a", makeHistory(5))
	if got != "RECOVERED" {
		t.Fatalf("empty-first should retry and adopt second result, got %q", got)
	}
	if calls != 2 {
		t.Fatalf("expected exactly 1 retry (2 calls), got %d", calls)
	}

	// 两次全空：降级截断包（非 RECOVERED）。
	calls = 0
	stub2 := func(_ context.Context, text string, merge bool) (string, error) {
		calls++
		return "", nil
	}
	pipe2 := NewPipeline(nil).WithHistorySummarizer(stub2)
	got2 := pipe2.summarizeSegment("a", makeHistory(5))
	if calls != 2 {
		t.Fatalf("all-empty should still retry once (2 calls), got %d", calls)
	}
	if got2 == "" || got2 == "RECOVERED" {
		t.Fatalf("all-empty should fall back to truncation bundle, got %q", got2)
	}
}

// TestCompaction_SummaryAppearsInView 验证交接文本合同（KC v0.15.0 同款）：
// 摘要本体必须出现在压缩后的视图中（renderBundles 渲染）。
func TestCompaction_SummaryAppearsInView(t *testing.T) {
	pipe := NewPipeline(nil).WithCompression(3).
		WithContextBudget(1, nil).
		WithTokenEstimator(alwaysOverEstimator).
		WithHistorySummarizer(func(_ context.Context, text string, merge bool) (string, error) {
			if merge {
				return "MERGED", nil
			}
			return "HANDOFF-SUMMARY", nil
		})
	out := pipe.Assemble(types.RoleDefinition{}, "a", makeHistory(9))
	sum, ok := findSummary(out)
	if !ok {
		t.Fatal("compressed view must contain summary block")
	}
	if !strings.Contains(sum, "HANDOFF-SUMMARY") {
		t.Fatalf("summary body must appear in handoff text, got %q", sum)
	}
}
