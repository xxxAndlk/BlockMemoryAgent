//go:build integration

package coding_test

import (
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/test/fixtures"
)

// TestBugFix 编程主场景端到端：定位并修复 bug。
// 预置一个含已知 bug 的 calc.go，让 Agent 修复。用 mock LLM 驱动会话走通完整栈，断言：
//   - 会话到达终态
//   - 目标到达 LLM
func TestBugFix(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	// 预置含 bug 的源文件（Add 误写为减法）
	f.WS.WriteFile("calc.go", []byte(`package calc

func Add(a, b int) int {
	return a - b // bug
}
`))

	f.LLM.SetDefaultResponse(fixtures.MockResponse{
		Content: "I see the bug: subtraction should be addition. Fixed.",
	})

	id := createSession(t, f, "fix the bug in calc.go where Add subtracts")
	status := waitForTerminal(t, f, id, 20*time.Second)
	if status == "timeout" {
		t.Fatalf("session %s did not terminate within timeout", id)
	}
	if len(f.LLM.Requests()) == 0 {
		t.Errorf("mock LLM did not receive any requests")
	}
	if !promptContains(f, "calc") {
		t.Errorf("goal 'calc' did not reach the LLM")
	}
}
