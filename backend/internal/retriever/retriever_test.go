package retriever

// retriever_test.go 验证 TODO #27 外部知识库检索层：
// RRF 融合排序、分块器、混合检索（fake 后端）、摄入管线（fake saver/embedder）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

func rec(id int64, content string) *types.KnowledgeRecord {
	return &types.KnowledgeRecord{ID: id, KnowledgeType: enums.KnowledgeTypeExternalKB, Content: content, Meta: map[string]any{}}
}

// TestRRFMerge 融合：两路各自排序的结果按 RRF 融合去重保序。
func TestRRFMerge(t *testing.T) {
	vector := []*types.KnowledgeRecord{rec(1, "v1"), rec(2, "v2"), rec(3, "v3")}
	keyword := []*types.KnowledgeRecord{rec(3, "k3"), rec(2, "k2"), rec(4, "k4")}
	merged := rrfMerge([][]*types.KnowledgeRecord{vector, keyword}, 3)
	if len(merged) != 3 {
		t.Fatalf("expected 3 merged, got %d", len(merged))
	}
	// id=3：keyword rank1（1/61）+ vector rank3（1/63）= 0.032267
	// id=2：两路都 rank2（2/62）= 0.032258 → id3 险胜排第一，id2 第二，id1 第三（单路 1/61）。
	if merged[0].ID != 3 {
		t.Fatalf("id=3 should rank first, got %d", merged[0].ID)
	}
	if merged[1].ID != 2 {
		t.Fatalf("id=2 should rank second, got %d", merged[1].ID)
	}
	if merged[2].ID != 1 {
		t.Fatalf("id=1 should rank third, got %d", merged[2].ID)
	}
}

// TestChunk_BasicSplits 分块：段落/标题边界收块 + 重叠。
func TestChunk_BasicSplits(t *testing.T) {
	doc := "# 标题A\n\n段落一内容。\n\n# 标题B\n\n段落二内容。\n"
	chunks := Chunk(doc, 200, 20)
	if len(chunks) < 3 {
		t.Fatalf("expected >=3 chunks, got %d: %v", len(chunks), chunks)
	}
	if !strings.Contains(chunks[0], "标题A") {
		t.Fatalf("chunk0 should carry heading A: %v", chunks)
	}
	joined := strings.Join(chunks, "")
	if !strings.Contains(joined, "标题B") || !strings.Contains(joined, "段落二") {
		t.Fatalf("chunks should carry heading B content: %v", chunks)
	}
}

// TestChunk_SingleHugeLine 单行超长强制截断 + 不丢内容。
func TestChunk_SingleHugeLine(t *testing.T) {
	line := strings.Repeat("数据", 500) // 1000 runes
	chunks := Chunk(line, 200, 0)
	if len(chunks) < 5 {
		t.Fatalf("huge line should split into multiple chunks, got %d", len(chunks))
	}
	joined := strings.Join(chunks, "")
	if len([]rune(joined)) < 900 {
		t.Fatalf("content lost across chunks, joined len=%d", len([]rune(joined)))
	}
}

// fakeHybrid 是混合检索后端 fake。
type fakeHybrid struct {
	vec  []*types.KnowledgeRecord
	kw   []*types.KnowledgeRecord
	call int
}

func (f *fakeHybrid) SearchByType(ctx context.Context, kt enums.KnowledgeType, emb []float32, topK int) ([]*types.KnowledgeRecord, error) {
	f.call++
	return f.vec, nil
}
func (f *fakeHybrid) SearchKeywords(ctx context.Context, kt enums.KnowledgeType, query string, topK int) ([]*types.KnowledgeRecord, error) {
	f.call++
	return f.kw, nil
}

// fakeEmbedder 返回固定向量。
type fakeEmbedder struct{}

func (fakeEmbedder) Embed(ctx context.Context, text string) ([]float32, error) { return []float32{1, 2, 3}, nil }

// TestSearchHybrid_RRFAndType 混合检索走 RRF + 类型隔离（external_kb）。
func TestSearchHybrid_RRFAndType(t *testing.T) {
	hy := &fakeHybrid{
		vec: []*types.KnowledgeRecord{rec(1, "vec1")},
		kw:  []*types.KnowledgeRecord{rec(1, "kw1"), rec(2, "kw2")},
	}
	r := NewGlobalKnowledgeRetriever(fakeEmbedder{})
	r.SetHybridBackend(hy)
	hits, err := r.SearchHybrid(context.Background(), "塔防游戏怎么实现", "", 3)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 unique hits, got %d", len(hits))
	}
	if hits[0].ID != 1 {
		t.Fatalf("id=1 (both lists) should rank first, got %d", hits[0].ID)
	}
	if hy.call != 2 {
		t.Fatalf("both backends should be called, got %d", hy.call)
	}
}

// fakeSaver 记录 Save 调用。
type fakeSaver struct {
	saved []*types.KnowledgeRecord
	err   error
}

func (f *fakeSaver) Save(ctx context.Context, rec *types.KnowledgeRecord) error {
	if f.err != nil {
		return f.err
	}
	f.saved = append(f.saved, rec)
	return nil
}

// TestIngestDir 摄入目录：切块 → embed → 落库，meta 带 namespace/source。
func TestIngestDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wiki.md"), []byte("# 渲染引擎\n\nCanvas 2D 渲染管线说明。\n\n# 配置\n\n配置文件格式说明。\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("一段纯文本知识。\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skip.bin"), []byte("not text"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	saver := &fakeSaver{}
	n, err := IngestDir(context.Background(), dir, saver, fakeEmbedder{}, IngestOptions{})
	if err != nil {
		t.Fatalf("IngestDir: %v", err)
	}
	if n != len(saver.saved) || n < 3 {
		t.Fatalf("expected >=3 chunks ingested, got %d saved=%d", n, len(saver.saved))
	}
	for _, r := range saver.saved {
		if r.KnowledgeType != enums.KnowledgeTypeExternalKB {
			t.Fatalf("type should be external_kb, got %s", r.KnowledgeType)
		}
		if r.Meta["namespace"] != "external" {
			t.Fatalf("meta.namespace missing, got %v", r.Meta)
		}
		if len(r.Embedding) == 0 {
			t.Fatalf("embedding should be set")
		}
		if !strings.Contains(r.Meta["source"].(string), ".md") && !strings.Contains(r.Meta["source"].(string), ".txt") {
			t.Fatalf("source should carry file path, got %v", r.Meta["source"])
		}
	}
}
