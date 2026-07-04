package memory

import (
	"context"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/pkg/types"
)

func TestSaveFromState_MixedSummaryStrategy(t *testing.T) {
	rs := &fakeRedisSnapshotStore{}
	ss := &fakeSnapshotStore{}
	cfg := &config.AgentConfig{SnapshotSummaryCount: 5}
	m := NewSnapshotManager(rs, ss, cfg)

	// 构造 10 条 Episode：最近 5 条重要性低，旧的有 2 条高重要性
	episodes := make([]*types.Episode, 10)
	for i := 0; i < 10; i++ {
		episodes[i] = &types.Episode{
			StepID:             fmtStepID(i),
			Timestamp:          time.Now().Add(-time.Duration(10-i) * time.Minute),
			ObservationSummary: "step content",
			Importance:         0.3,
		}
	}
	episodes[0].Importance = 0.9 // 很旧但很重要
	episodes[2].Importance = 0.9 // 很旧但很重要

	output := &types.AgentOutput{AgentID: "a1", Version: 1}
	if err := m.SaveFromState(context.Background(), "a1", "t1", output, episodes); err != nil {
		t.Fatalf("SaveFromState failed: %v", err)
	}

	if len(ss.saved) != 1 {
		t.Fatalf("expected 1 saved snapshot, got %d", len(ss.saved))
	}
	snap := ss.saved[0]
	if len(snap.KeySummaries) != 7 {
		t.Fatalf("expected 7 summaries (5 recent + 2 high-importance), got %d", len(snap.KeySummaries))
	}
}

func TestSaveFromState_OpenIssuesOnlyUnresolved(t *testing.T) {
	rs := &fakeRedisSnapshotStore{}
	ss := &fakeSnapshotStore{}
	cfg := &config.AgentConfig{SnapshotOpenIssueThreshold: 0.7}
	m := NewSnapshotManager(rs, ss, cfg)

	now := time.Now()
	episodes := []*types.Episode{
		{
			StepID:             "s1",
			Timestamp:          now,
			ObservationSummary: "成功完成文件写入",
			Action:             "WriteFile",
			Importance:         0.8,
		},
		{
			StepID:             "s2",
			Timestamp:          now,
			ObservationSummary: "命令执行失败: exit status 1",
			Action:             "RunCommand",
			Importance:         0.8,
		},
	}

	output := &types.AgentOutput{AgentID: "a1", Version: 1}
	if err := m.SaveFromState(context.Background(), "a1", "t1", output, episodes); err != nil {
		t.Fatalf("SaveFromState failed: %v", err)
	}

	snap := ss.saved[0]
	if len(snap.OpenIssues) != 1 {
		t.Fatalf("expected 1 open issue (only failed command), got %d", len(snap.OpenIssues))
	}
	if snap.OpenIssues[0].ID != "s2" {
		t.Fatalf("expected open issue id s2, got %s", snap.OpenIssues[0].ID)
	}
}

func fmtStepID(i int) string {
	return "step_" + string(rune('0'+i))
}
