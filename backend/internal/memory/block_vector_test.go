package memory

import (
	"context"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// fakeSearcher 用于测试 SearchBlockMemory 的 scope 过滤与 token 截断。
type fakeSearcher struct {
	records []*types.KnowledgeRecord
	dim     int
}

func (f *fakeSearcher) SearchKnowledgeByTypeAndDomain(ctx context.Context, knowledgeType enums.KnowledgeType, domain string, embedding []float32, topK int) ([]*types.KnowledgeRecord, error) {
	return f.records, nil
}

func (f *fakeSearcher) EmbeddingDim() int { return f.dim }

func (f *fakeSearcher) Embed(ctx context.Context, text string) ([]float32, error) {
	return make([]float32, f.dim), nil
}

func (f *fakeSearcher) SearchBlockMemoryMaxTokens() int { return 800 }

func TestToKnowledgeRecordFacts(t *testing.T) {
	rec := &BlockMemoryRecord{
		SessionID: "s1",
		Domain:    "frontend",
		Goal:      "修复 CSS",
		Summary:   "已修复 header padding",
		Facts: []Fact{
			{Key: "framework", Value: "tailwind", Scope: FactScopeDomain},
			{Key: "error", Value: "padding 不一致", Scope: FactScopeTask},
		},
		CreatedAt: time.Now(),
	}
	kr, err := rec.ToKnowledgeRecord(context.Background(), nil, 768)
	if err != nil {
		t.Fatalf("ToKnowledgeRecord 失败: %v", err)
	}
	if kr.Meta["facts"] == "" {
		t.Fatalf("facts 应被序列化写入 meta")
	}
	if kr.KnowledgeType != enums.KnowledgeTypeBlockMemory {
		t.Fatalf("knowledge_type 应为 block_memory，got %s", kr.KnowledgeType)
	}
}

func TestKnowledgeToBlockMemoryParsesFacts(t *testing.T) {
	rec, err := (&BlockMemoryRecord{
		SessionID: "s1",
		Domain:    "frontend",
		Goal:      "修复 CSS",
		Summary:   "已修复 header padding",
		Facts: []Fact{
			{Key: "framework", Value: "tailwind", Scope: FactScopeDomain},
			{Key: "global_key", Value: "vue3", Scope: FactScopeGlobal},
		},
		CreatedAt: time.Now(),
	}).ToKnowledgeRecord(context.Background(), nil, 768)
	if err != nil {
		t.Fatalf("ToKnowledgeRecord 失败: %v", err)
	}

	out := knowledgeToBlockMemory(rec)
	if out == nil {
		t.Fatal("knowledgeToBlockMemory 不应返回 nil")
	}
	if len(out.Facts) != 2 {
		t.Fatalf("应解析出 2 条 facts，got %d", len(out.Facts))
	}
	if out.Summary != "已修复 header padding" {
		t.Fatalf("summary 解析错误，got %s", out.Summary)
	}
}

func TestSearchBlockMemoryScopeFilter(t *testing.T) {
	rec, err := (&BlockMemoryRecord{
		SessionID: "s1",
		Domain:    "frontend",
		Goal:      "修复 CSS",
		Summary:   "已修复 header padding",
		Facts: []Fact{
			{Key: "framework", Value: "tailwind", Scope: FactScopeDomain},
			{Key: "error", Value: "padding 不一致", Scope: FactScopeTask},
			{Key: "stack", Value: "vue3", Scope: FactScopeGlobal},
		},
		CreatedAt: time.Now(),
	}).ToKnowledgeRecord(context.Background(), nil, 768)
	if err != nil {
		t.Fatalf("ToKnowledgeRecord 失败: %v", err)
	}

	fs := &fakeSearcher{records: []*types.KnowledgeRecord{rec}, dim: 768}
	out, err := SearchBlockMemory(context.Background(), fs, "frontend", "修复 CSS", 5)
	if err != nil {
		t.Fatalf("SearchBlockMemory 失败: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("应返回 1 条记录，got %d", len(out))
	}
	// 默认只保留 domain + task，global 应被过滤掉
	if len(out[0].Facts) != 2 {
		t.Fatalf("应过滤为 2 条 facts，got %d", len(out[0].Facts))
	}
	for _, f := range out[0].Facts {
		if f.Scope == FactScopeGlobal {
			t.Fatalf("global scope 应被过滤掉")
		}
	}
}

func TestFilterFactsByScope(t *testing.T) {
	facts := []Fact{
		{Key: "a", Value: "1", Scope: FactScopeGlobal},
		{Key: "b", Value: "2", Scope: FactScopeDomain},
		{Key: "c", Value: "3", Scope: FactScopeTask},
	}
	filtered := filterFactsByScope(facts, FactScopeDomain, FactScopeTask)
	if len(filtered) != 2 {
		t.Fatalf("应过滤出 2 条，got %d", len(filtered))
	}
}

func TestFormatBlockMemoryFacts(t *testing.T) {
	facts := []Fact{
		{Key: "framework", Value: "tailwind", Scope: FactScopeDomain},
	}
	s := FormatBlockMemoryFacts(facts)
	if s == "" {
		t.Fatal("格式化结果不应为空")
	}
	if !contains(s, "framework") || !contains(s, "tailwind") || !contains(s, "domain") {
		t.Fatalf("格式化结果应包含 key/value/scope，got %s", s)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || findSub(s, sub))
}

func findSub(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
