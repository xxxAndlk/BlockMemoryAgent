package subagent

// blackboard_uptake_test.go 测试黑板模式（TODO #42）：
// - injectScopedRecall scope 确定性过滤 + 语义回退
// - siblingUptakePipeline 每轮摄取（命中注入/去重/排他自身/fail-open/seedSeen）
// - withPriorSalvage 黑板优先读
//
// mockBlackboardSearcher 同时实现 BlockMemorySearcher + BlackboardSearcher，供 scope 路径测试。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// mockBlackboardSearcher 同时实现 BlockMemorySearcher（SearchBlockMemoryByGoal）与
// BlackboardSearcher（Query），记录调用参数供断言 scope 过滤行为。
type mockBlackboardSearcher struct {
	recs      []*types.KnowledgeRecord // SearchBlockMemoryByGoal 返回
	err       error
	lastGoal  string
	bumpCalls int

	queryRecs        []*types.KnowledgeRecord // Query 返回
	queryErr         error
	lastQueryParent  string
	lastQueryDomain  string
	lastQueryText    string
	lastQueryExclude string
	queryCalls       int
}

func (m *mockBlackboardSearcher) SearchBlockMemoryByGoal(ctx context.Context, sessionID, goal string, topK int) ([]*types.KnowledgeRecord, error) {
	m.lastGoal = goal
	return m.recs, m.err
}

func (m *mockBlackboardSearcher) BumpReuse(ctx context.Context, id int64) error {
	m.bumpCalls++
	return nil
}

func (m *mockBlackboardSearcher) Query(ctx context.Context, sessionID, parentID, taskDomain, query string, topK int, excludeSubAgentID string) ([]*types.KnowledgeRecord, error) {
	m.lastQueryParent = parentID
	m.lastQueryDomain = taskDomain
	m.lastQueryText = query
	m.lastQueryExclude = excludeSubAgentID
	m.queryCalls++
	return m.queryRecs, m.queryErr
}

// TestInjectScopedRecall_FiltersScope 验证 BlackboardSearcher.Query 被调用且 scope 参数透传。
func TestInjectScopedRecall_FiltersScope(t *testing.T) {
	mock := &mockBlackboardSearcher{
		queryRecs: []*types.KnowledgeRecord{{ID: 10, Content: "兄弟结论A", Meta: map[string]any{"outcome": "success"}}},
	}
	d := NewDispatcher(nil, nil, nil, nil, nil).WithBlockMemorySearcher(mock)
	ctx := tool.WithSessionID(context.Background(), "s1")

	got, recs := d.injectScopedRecall(ctx, "s1", "渲染", "查询", "原始任务")
	if mock.queryCalls != 1 {
		t.Fatalf("expected 1 Query call, got %d", mock.queryCalls)
	}
	if mock.lastQueryParent != "s1" || mock.lastQueryDomain != "渲染" {
		t.Fatalf("scope not passed: parent=%s domain=%s", mock.lastQueryParent, mock.lastQueryDomain)
	}
	if !strings.Contains(got, "兄弟结论A") {
		t.Fatalf("expected recall content injected, got: %s", got)
	}
	if !strings.Contains(got, "【当前任务】\n原始任务") {
		t.Fatalf("expected task appended after recall, got: %s", got)
	}
	if len(recs) != 1 || recs[0].ID != 10 {
		t.Fatalf("expected 1 rec returned, got %v", recs)
	}
}

// TestInjectScopedRecall_FallbackSemantic 验证 searcher 未实现 BlackboardSearcher（mock 无 Query）
// 时回退 SearchBlockMemoryByGoal 语义召回（向后兼容旧数据与现有测试）。
func TestInjectScopedRecall_FallbackSemantic(t *testing.T) {
	mock := &mockBlockMemorySearcher{recs: []*types.KnowledgeRecord{{Content: "语义命中"}}}
	d := NewDispatcher(nil, nil, nil, nil, nil).WithBlockMemorySearcher(mock)
	ctx := tool.WithSessionID(context.Background(), "s1")

	got, _ := d.injectScopedRecall(ctx, "s1", "渲染", "查询", "任务")
	if mock.lastGoal != "查询" {
		t.Fatalf("expected fallback to SearchBlockMemoryByGoal with query, got goal=%s", mock.lastGoal)
	}
	if !strings.Contains(got, "语义命中") {
		t.Fatalf("expected fallback recall content, got: %s", got)
	}
}

// TestSiblingUptake_AppendsOnHits 验证 Query 命中时尾部追加【兄弟产出】+ excludeSelf 透传。
func TestSiblingUptake_AppendsOnHits(t *testing.T) {
	mock := &mockBlackboardSearcher{
		queryRecs: []*types.KnowledgeRecord{{ID: 1, Content: "兄弟完成: 渲染引擎", Meta: map[string]any{"outcome": "success"}}},
	}
	p := newSiblingUptakePipeline(agent.NopMemoryPipeline{}, mock, "s1", "s1", "渲染", "s1/domain-1")

	out := p.Assemble(types.RoleDefinition{}, "s1/domain-1", []agent.ReactMessage{{Role: "user", Content: "task"}})
	if len(out) != 2 {
		t.Fatalf("expected 2 msgs (history + sibling), got %d", len(out))
	}
	if !strings.Contains(out[1].Content, "【兄弟产出】") || !strings.Contains(out[1].Content, "渲染引擎") {
		t.Fatalf("expected sibling output msg, got: %s", out[1].Content)
	}
	if mock.lastQueryExclude != "s1/domain-1" {
		t.Fatalf("expected excludeSelf=selfID, got %s", mock.lastQueryExclude)
	}
}

// TestSiblingUptake_NoHitsNoInject 验证 Query 无命中时不注入。
func TestSiblingUptake_NoHitsNoInject(t *testing.T) {
	mock := &mockBlackboardSearcher{queryRecs: nil}
	p := newSiblingUptakePipeline(agent.NopMemoryPipeline{}, mock, "s1", "s1", "渲染", "s1/domain-1")

	out := p.Assemble(types.RoleDefinition{}, "s1/domain-1", []agent.ReactMessage{{Role: "user", Content: "t"}})
	if len(out) != 1 {
		t.Fatalf("no hits should not inject, got %d msgs", len(out))
	}
}

// TestSiblingUptake_DedupSeen 验证已注入的 rec 下一轮不重复注入。
func TestSiblingUptake_DedupSeen(t *testing.T) {
	rec := []*types.KnowledgeRecord{{ID: 1, Content: "兄弟结论", Meta: map[string]any{"outcome": "success"}}}
	mock := &mockBlackboardSearcher{queryRecs: rec}
	p := newSiblingUptakePipeline(agent.NopMemoryPipeline{}, mock, "s1", "s1", "渲染", "s1/domain-1")

	if out := p.Assemble(types.RoleDefinition{}, "s1/domain-1", []agent.ReactMessage{{Role: "user", Content: "t"}}); len(out) != 2 {
		t.Fatalf("first Assemble should inject, got %d msgs", len(out))
	}
	// 第二轮同 rec -> 已见，不重复注入。
	if out := p.Assemble(types.RoleDefinition{}, "s1/domain-1", []agent.ReactMessage{{Role: "user", Content: "t"}}); len(out) != 1 {
		t.Fatalf("second Assemble should dedup, got %d msgs", len(out))
	}
}

// TestSiblingUptake_SeedSeen 验证播种召回 seedSeen 的 rec 不被每轮摄取重复注入。
func TestSiblingUptake_SeedSeen(t *testing.T) {
	seed := []*types.KnowledgeRecord{{ID: 5, Content: "已见结论", Meta: map[string]any{"outcome": "success"}}}
	mock := &mockBlackboardSearcher{queryRecs: seed}
	p := newSiblingUptakePipeline(agent.NopMemoryPipeline{}, mock, "s1", "s1", "渲染", "s1/domain-1")
	p.seedSeen(seed)

	out := p.Assemble(types.RoleDefinition{}, "s1/domain-1", []agent.ReactMessage{{Role: "user", Content: "t"}})
	if len(out) != 1 {
		t.Fatalf("seeded rec should be deduped, got %d msgs", len(out))
	}
}

// TestSiblingUptake_FailOpen 验证 Query 出错时不注入不崩。
func TestSiblingUptake_FailOpen(t *testing.T) {
	mock := &mockBlackboardSearcher{queryErr: errors.New("db down")}
	p := newSiblingUptakePipeline(agent.NopMemoryPipeline{}, mock, "s1", "s1", "渲染", "s1/domain-1")

	out := p.Assemble(types.RoleDefinition{}, "s1/domain-1", []agent.ReactMessage{{Role: "user", Content: "t"}})
	if len(out) != 1 {
		t.Fatalf("fail-open should not inject, got %d msgs", len(out))
	}
}

// TestSiblingUptake_RosterRealtimeRefresh 验证拓扑名册实时刷新（全量 diff 语义）：
// 与已播报快照无变化时零注入；任何变化（新增或消失）注入**全量当前名册**一次；
// 持续无变化后不再重复注入。
func TestSiblingUptake_RosterRealtimeRefresh(t *testing.T) {
	mock := &mockBlackboardSearcher{queryRecs: nil}
	p := newSiblingUptakePipeline(agent.NopMemoryPipeline{}, mock, "s1", "s1", "渲染", "s1/domain-1")

	current := []rosterEntry{{id: "s1/domain-2", label: "核心层", task: "任务A"}}
	// seeded = 首注名册（domain-2 系统提示词里已列出）。
	p = p.WithRoster(func() []rosterEntry { return current }, map[string]bool{"s1/domain-2": true})

	// 轮1：无变化 -> 不注入。
	out := p.Assemble(types.RoleDefinition{}, "s1/domain-1", []agent.ReactMessage{{Role: "user", Content: "t"}})
	if len(out) != 1 {
		t.Fatalf("无变化时应零注入, got %d msgs", len(out))
	}
	// 新兄弟出现 -> 注入全量（含已知兄弟，替换旧认知）。
	current = []rosterEntry{{id: "s1/domain-2", label: "核心层", task: "任务A"}, {id: "s1/domain-9", label: "命令层", task: "任务B"}}
	out = p.Assemble(types.RoleDefinition{}, "s1/domain-1", []agent.ReactMessage{{Role: "user", Content: "t"}})
	if len(out) != 2 || !strings.Contains(out[1].Content, "【拓扑名册更新】") ||
		!strings.Contains(out[1].Content, "s1/domain-9") || !strings.Contains(out[1].Content, "s1/domain-2") {
		t.Fatalf("名册变化应注入全量当前名册, got: %v", out[1:])
	}
	// 下一轮无变化 -> 不重复。
	out = p.Assemble(types.RoleDefinition{}, "s1/domain-1", []agent.ReactMessage{{Role: "user", Content: "t"}})
	if len(out) != 1 {
		t.Fatalf("已播报快照无变化不得重复注入, got %d msgs", len(out))
	}
	// 兄弟消失（domain-2 完成）-> 变化触发，注入剩余全量（domain-2 不再出现）。
	current = []rosterEntry{{id: "s1/domain-9", label: "命令层", task: "任务B"}}
	out = p.Assemble(types.RoleDefinition{}, "s1/domain-1", []agent.ReactMessage{{Role: "user", Content: "t"}})
	if len(out) != 2 || strings.Contains(out[1].Content, "s1/domain-2") || !strings.Contains(out[1].Content, "s1/domain-9") {
		t.Fatalf("兄弟消失应注入不含该节点的全量名册, got: %v", out[1:])
	}
}

// TestSiblingUptake_RosterNilFn 验证未装配 rosterFn（旧调用形态）时零行为。
func TestSiblingUptake_RosterNilFn(t *testing.T) {
	mock := &mockBlackboardSearcher{queryRecs: nil}
	p := newSiblingUptakePipeline(agent.NopMemoryPipeline{}, mock, "s1", "s1", "渲染", "s1/domain-1")
	out := p.Assemble(types.RoleDefinition{}, "s1/domain-1", []agent.ReactMessage{{Role: "user", Content: "t"}})
	if len(out) != 1 {
		t.Fatalf("nil rosterFn should not inject, got %d msgs", len(out))
	}
}

// TestWithPriorSalvage_ReadsBlackboard 验证 withPriorSalvage 黑板优先读（替 slot）。
func TestWithPriorSalvage_ReadsBlackboard(t *testing.T) {
	d, _, _, _, _ := newSalvageTestEnv(t, &mockProvider{text: "ok"})
	mock := &mockBlackboardSearcher{
		queryRecs: []*types.KnowledgeRecord{{Content: "黑板打捞: 已读 config.js", Meta: map[string]any{"outcome": "fail"}}},
	}
	d.WithBlockMemorySearcher(mock)

	task := d.withPriorSalvage(dispatchCtx(), "s1", "配置", "domain", "实现 config.js")
	if !strings.Contains(task, "【前序探索摘要】") || !strings.Contains(task, "config.js") {
		t.Fatalf("expected blackboard salvage injected, got: %s", task)
	}
	if mock.queryCalls == 0 || mock.lastQueryDomain != "配置" {
		t.Fatalf("expected blackboard Query by scope, calls=%d domain=%s", mock.queryCalls, mock.lastQueryDomain)
	}
}
