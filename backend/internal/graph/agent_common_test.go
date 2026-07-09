package graph

import (
	"context"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// TestCommonCollectTaskSummaries 验证摘要收集与截断。
func TestCommonCollectTaskSummaries(t *testing.T) {
	t.Run("nil block", func(t *testing.T) {
		if got := CommonCollectTaskSummaries(nil, 100); len(got) != 0 {
			t.Errorf("nil block should yield no summaries")
		}
	})
	t.Run("truncate long result", func(t *testing.T) {
		long := make([]byte, 200)
		for i := range long {
			long[i] = 'x'
		}
		block := &types.SessionBlock{TaskResults: map[string]string{
			"task1": "short result",
			"task2": string(long),
		}}
		got := CommonCollectTaskSummaries(block, 100)
		if len(got) != 2 {
			t.Fatalf("want 2 summaries, got %d", len(got))
		}
		// 长结果应被截断并带省略号
		foundTrunc := false
		for _, s := range got {
			if len(s) > 100 && endsWithEllipsis(s) {
				foundTrunc = true
			}
		}
		if !foundTrunc {
			t.Errorf("expected a truncated summary with ... suffix")
		}
	})
}

func endsWithEllipsis(s string) bool {
	return len(s) > 3 && s[len(s)-3:] == "..."
}

// TestCommonMatchFixedAssistant_NilRegistry 验证 nil registry 安全返回 nil。
func TestCommonMatchFixedAssistant_NilRegistry(t *testing.T) {
	if got := CommonMatchFixedAssistant(nil, "写代码"); got != nil {
		t.Errorf("nil registry should return nil, got %v", got)
	}
}

// TestCommonExecuteAssistantTask_NoModelFactory 验证无模型工厂回退模拟结果（P0-1：返回 AgentResult）。
func TestCommonExecuteAssistantTask_NoModelFactory(t *testing.T) {
	def := &types.RoleDefinition{ID: "x", Name: "测试助手", SystemPrompt: "sp"}
	state := types.NewThreeLayerState("s1")
	state.CurrentDomain = "demo"
	got, err := CommonExecuteAssistantTask(context.Background(), nil, nil, nil, def, "做某事", state, "", nil, "助手[测试]", 0, false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.SummaryForUser == "" {
		t.Fatalf("expected mock result, got empty")
	}
	// 模拟结果应含领域前缀与任务文本
	if !contains(got.SummaryForUser, "demo") || !contains(got.SummaryForUser, "做某事") {
		t.Errorf("mock result missing domain/task: %s", got.SummaryForUser)
	}
	// MemoryForMeta 应同步填充，供 MetaAgent 调度使用
	if !contains(got.MemoryForMeta, "做某事") {
		t.Errorf("mock result MemoryForMeta missing task: %s", got.MemoryForMeta)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestExecutionPlan_PendingAndMarkDone 验证计划的未完成目标与标记完成（断点续行）。
func TestExecutionPlan_PendingAndMarkDone(t *testing.T) {
	plan := &types.ExecutionPlan{Steps: []*types.PlanStep{
		{Goal: "step1", Status: types.PlanStepDone},
		{Goal: "step2", Status: types.PlanStepPending},
		{Goal: "step3", Status: types.PlanStepPending},
	}}
	pending := plan.PendingGoals()
	if len(pending) != 2 || pending[0] != "step2" || pending[1] != "step3" {
		t.Errorf("unexpected pending goals: %v", pending)
	}
	plan.MarkDone("step2")
	pending = plan.PendingGoals()
	if len(pending) != 1 || pending[0] != "step3" {
		t.Errorf("after markdone step2, unexpected pending: %v", pending)
	}
}
