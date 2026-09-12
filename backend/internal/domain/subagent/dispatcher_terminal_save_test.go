package subagent

// dispatcher_terminal_save_test.go 验证子 Agent 终态（成功/部分回灌）把完整 history
// 落 agent_messages（编排页对话视图 PG 全量源），复用 pause 测试的 captureMessagesStore。

import (
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
)

// TestRunSubAgent_TerminalSavesHistory 叶子助手成功收尾时应有一次 SaveMessages 落库
//（区别于既有 pause 路径——这是新增的终态落库）。
func TestRunSubAgent_TerminalSavesHistory(t *testing.T) {
	d, _, _, msgStore, tr, toolsReg := newPauseTestEnv(t, &tokenUsageProvider{text: "leaf done"})
	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":     "code_assistant",
		"task":        "write x",
		"verify_kind": "none",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	subID := res.Output
	waitForCond(t, "leaf terminal", func() bool {
		n, ok := tr.Get(subID)
		return ok && (n.Status == orchestrator.StatusDone)
	})
	found := false
	for _, s := range msgStore.saved {
		if s.agentID == subID {
			found = true
		}
	}
	if !found {
		t.Fatalf("终态应落库 history, saved=%+v", msgStore.saved)
	}
	_ = d
}
