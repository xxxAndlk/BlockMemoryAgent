package bootstrap

// memory_consolidate_test.go 覆盖块记忆整理：方案解析、应用守卫（合并/归档的
// 安全下限与校验）、审计流水（PG 真实路径由训练/集成验证）。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// TestParseMemoryConsolidatePlan 围栏剥离 + int64 id 字段映射。
func TestParseMemoryConsolidatePlan(t *testing.T) {
	resp := "噪声前缀\n```json\n{\"merges\":[{\"keep\":1,\"merge\":[2,3],\"merged\":\"合并后表述\",\"note\":\"同类\"}]," +
		"\"archives\":[{\"id\":4,\"reason\":\"零访问琐碎\"}]}\n```"
	plan, err := parseMemoryConsolidatePlan(resp)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(plan.Merges) != 1 || plan.Merges[0].Keep != 1 || len(plan.Merges[0].Merge) != 2 ||
		plan.Merges[0].Merge[1] != 3 || plan.Merges[0].Merged != "合并后表述" {
		t.Fatalf("merges = %+v", plan.Merges)
	}
	if len(plan.Archives) != 1 || plan.Archives[0].ID != 4 {
		t.Fatalf("archives = %+v", plan.Archives)
	}
}

// TestParseMemoryConsolidatePlanNoJSON 无 JSON 对象返回错误。
func TestParseMemoryConsolidatePlanNoJSON(t *testing.T) {
	if _, err := parseMemoryConsolidatePlan("纯文本无对象"); err == nil {
		t.Fatal("expected error for response without JSON")
	}
}

// fakeMemoryStore 块记忆操作桩：记录更新与归档调用。
type fakeMemoryStore struct {
	recs     []*types.KnowledgeRecord
	updated  map[int64]string
	archived map[int64]bool
}

func newFakeMemoryStore(recs ...*types.KnowledgeRecord) *fakeMemoryStore {
	return &fakeMemoryStore{recs: recs, updated: map[int64]string{}, archived: map[int64]bool{}}
}

func (f *fakeMemoryStore) ListAll(context.Context) ([]*types.KnowledgeRecord, error) {
	return f.recs, nil
}

func (f *fakeMemoryStore) UpdateContentEmbedding(_ context.Context, id int64, content string, _ []float32) error {
	f.updated[id] = content
	return nil
}

func (f *fakeMemoryStore) Archive(_ context.Context, id int64) error {
	f.archived[id] = true
	return nil
}

func (f *fakeMemoryStore) Embed(context.Context, string) ([]float32, error) {
	return []float32{0.1, 0.2}, nil
}

// fakeEvoLog 审计流水桩：记录 kind 序列。
type fakeEvoLog struct{ kinds []string }

func (f *fakeEvoLog) AppendEvolutionLog(_ context.Context, kind, _, _, _ string) error {
	f.kinds = append(f.kinds, kind)
	return nil
}

func memRec(id int64, content string, access int) *types.KnowledgeRecord {
	return &types.KnowledgeRecord{ID: id, KnowledgeType: enums.KnowledgeTypeBlockMemory, Content: content, AccessCount: access}
}

// TestMemoryConsolidatorApplyMergeAndArchive 应用守卫全链路：合法合并生效（保留者
// 重写、来源归档）、零访问才可归档、空 merged 组跳过、id 不存在的条目忽略。
func TestMemoryConsolidatorApplyMergeAndArchive(t *testing.T) {
	recs := []*types.KnowledgeRecord{
		memRec(1, "改写教材需满足mermaid≥1且围栏配对", 3),
		memRec(2, "改写教材验收需满足mermaid图≥1且围栏配对", 0),
		memRec(3, "零访问的一次性琐碎细节", 0),
		memRec(4, "被召回过的事实，不可归档", 5),
	}
	fake := newFakeMemoryStore(recs...)
	evo := &fakeEvoLog{}
	c := &memoryConsolidator{mem: fake, evo: evo}

	plan := &memoryConsolidatePlan{}
	plan.Merges = append(plan.Merges, memoryMergeGroup{
		Keep: 1, Merge: []int64{2}, Merged: "改写教材需满足 mermaid≥1 且围栏配对（验收同口径）", Note: "同口径",
	})
	// 空 merged 的组必须跳过（无合成文本不做合并）。
	plan.Merges = append(plan.Merges, memoryMergeGroup{Keep: 3, Merge: []int64{4}, Note: "无合成文本"})
	// 归档：id=3 零访问可归档；id=4 访问>0 必须拒绝。
	plan.Archives = append(plan.Archives,
		memoryArchiveItem{ID: 3, Reason: "零访问琐碎"},
		memoryArchiveItem{ID: 4, Reason: "试图归档热条目"},
	)

	groups, sources, archived := c.apply(context.Background(), recs, plan)
	if groups != 1 || sources != 1 || archived != 1 {
		t.Fatalf("apply = (%d,%d,%d), want (1,1,1)", groups, sources, archived)
	}
	if got := fake.updated[1]; !strings.Contains(got, "mermaid≥1") {
		t.Fatalf("keeper not rewritten with merged content, got %q", got)
	}
	if !fake.archived[2] || !fake.archived[3] {
		t.Fatalf("merged source and zero-access entry must be archived, archived=%v", fake.archived)
	}
	if fake.archived[4] {
		t.Fatal("used entry (access_count>0) must never be archived")
	}
	if fake.archived[1] {
		t.Fatal("keeper must stay active")
	}
	for _, kind := range []string{"block_memory_merge", "block_memory_archive"} {
		found := false
		for _, k := range evo.kinds {
			if k == kind {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing audit kind %s in %v", kind, evo.kinds)
		}
	}
}

// TestMemoryConsolidatorApplyGuards id 不存在 / keep=source / 重复占用全部跳过或拒绝。
func TestMemoryConsolidatorApplyGuards(t *testing.T) {
	recs := []*types.KnowledgeRecord{
		memRec(1, "事实A", 1),
		memRec(2, "事实B", 1),
	}
	fake := newFakeMemoryStore(recs...)
	c := &memoryConsolidator{mem: fake}

	plan := &memoryConsolidatePlan{}
	plan.Merges = append(plan.Merges,
		// keep 不存在。
		memoryMergeGroup{Keep: 99, Merge: []int64{1}, Merged: "x"},
		// keep 混进 merge 列表。
		memoryMergeGroup{Keep: 1, Merge: []int64{1}, Merged: "x"},
	)

	groups, sources, archived := c.apply(context.Background(), recs, plan)
	if groups != 0 || sources != 0 || archived != 0 {
		t.Fatalf("invalid plan must apply nothing, got (%d,%d,%d)", groups, sources, archived)
	}
	if len(fake.updated) != 0 || len(fake.archived) != 0 {
		t.Fatalf("no store mutation expected, updated=%v archived=%v", fake.updated, fake.archived)
	}
}

// TestMemoryConsolidatorListFilter ListAll 结果须过滤为激活 block_memory（其它类型与
// 已归档行不参与整理）。
func TestMemoryConsolidatorListFilter(t *testing.T) {
	archived := memRec(1, "已归档", 0)
	archived.Archived = true
	other := &types.KnowledgeRecord{ID: 2, KnowledgeType: enums.KnowledgeTypeDomainProfile, Content: "领域种子"}
	active := memRec(3, "激活块记忆", 2)
	fake := newFakeMemoryStore(archived, other, active)
	c := &memoryConsolidator{mem: fake}
	got, err := c.listMemories(context.Background())
	if err != nil {
		t.Fatalf("listMemories: %v", err)
	}
	if len(got) != 1 || got[0].ID != 3 {
		t.Fatalf("expected only active block_memory id=3, got %+v", got)
	}
}

// TestMemoryConsolidatorMergeJudgeVeto judge 拒绝部分来源：被拒来源保持激活不归档，
// 确认来源照常合并（keep 重写 + 来源归档），judge 逐来源各调一次。
func TestMemoryConsolidatorMergeJudgeVeto(t *testing.T) {
	recs := []*types.KnowledgeRecord{
		memRec(1, "保留者：mermaid 围栏口径", 2),
		memRec(2, "同口径表述（judge 确认重复）", 0),
		memRec(3, "语义不同的事实（judge 拒并）", 0),
	}
	fake := newFakeMemoryStore(recs...)
	judgeCalls := 0
	c := &memoryConsolidator{mem: fake, judge: func(_ context.Context, _ *types.KnowledgeRecord, src *types.KnowledgeRecord) (duplicateJudgeResult, error) {
		judgeCalls++
		return duplicateJudgeResult{Duplicate: src.ID == 2, Reason: "只认 id=2"}, nil
	}}
	plan := &memoryConsolidatePlan{Merges: []memoryMergeGroup{
		{Keep: 1, Merge: []int64{2, 3}, Merged: "合并后：mermaid 围栏口径", Note: "同口径"},
	}}
	groups, sources, archived := c.apply(context.Background(), recs, plan)
	if groups != 1 || sources != 1 || archived != 0 {
		t.Fatalf("apply = (%d,%d,%d), want (1,1,0)", groups, sources, archived)
	}
	if judgeCalls != 2 {
		t.Fatalf("judge calls = %d, want 2（逐来源复核）", judgeCalls)
	}
	if !fake.archived[2] {
		t.Fatal("judge-confirmed source must be archived")
	}
	if fake.archived[3] {
		t.Fatal("judge-rejected source must stay active")
	}
	if _, ok := fake.updated[1]; !ok {
		t.Fatal("keeper must be rewritten with merged content")
	}
}

// TestMemoryConsolidatorMergeJudgeAllRejected 全部来源被 judge 拒并：整组放弃
//（keep 零副作用——不重写、不归档）。
func TestMemoryConsolidatorMergeJudgeAllRejected(t *testing.T) {
	recs := []*types.KnowledgeRecord{memRec(1, "保留者", 1), memRec(2, "被拒来源", 1)}
	fake := newFakeMemoryStore(recs...)
	c := &memoryConsolidator{mem: fake, judge: func(context.Context, *types.KnowledgeRecord, *types.KnowledgeRecord) (duplicateJudgeResult, error) {
		return duplicateJudgeResult{Duplicate: false, Reason: "工艺不同"}, nil
	}}
	plan := &memoryConsolidatePlan{Merges: []memoryMergeGroup{
		{Keep: 1, Merge: []int64{2}, Merged: "合成文本", Note: "x"},
	}}
	groups, sources, archived := c.apply(context.Background(), recs, plan)
	if groups != 0 || sources != 0 || archived != 0 {
		t.Fatalf("all-rejected group must be aborted, got (%d,%d,%d)", groups, sources, archived)
	}
	if len(fake.updated) != 0 || len(fake.archived) != 0 {
		t.Fatalf("aborted group must have zero side effects, updated=%v archived=%v", fake.updated, fake.archived)
	}
}

// TestMemoryConsolidatorMergeJudgeFailureConservative judge 失败/超时：保守按重复
// 处理（维持合并，与技能库同档策略）。
func TestMemoryConsolidatorMergeJudgeFailureConservative(t *testing.T) {
	recs := []*types.KnowledgeRecord{memRec(1, "保留者", 1), memRec(2, "来源", 0)}
	fake := newFakeMemoryStore(recs...)
	c := &memoryConsolidator{mem: fake, judge: func(context.Context, *types.KnowledgeRecord, *types.KnowledgeRecord) (duplicateJudgeResult, error) {
		return duplicateJudgeResult{}, errors.New("judge timeout")
	}}
	plan := &memoryConsolidatePlan{Merges: []memoryMergeGroup{
		{Keep: 1, Merge: []int64{2}, Merged: "合成文本", Note: "x"},
	}}
	groups, sources, archived := c.apply(context.Background(), recs, plan)
	if groups != 1 || sources != 1 || archived != 0 {
		t.Fatalf("judge failure must conservatively merge, got (%d,%d,%d)", groups, sources, archived)
	}
	if !fake.archived[2] {
		t.Fatal("conservative merge must archive source")
	}
}
