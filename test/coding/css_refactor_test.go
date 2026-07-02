//go:build integration

package coding_test

import (
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/test/fixtures"
)

// TestCSSRefactor 编程主场景端到端：CSS 重构任务。
// 预置一个 styles.css，让 Agent 做重构。用 mock LLM 驱动会话走通完整栈，断言：
//   - 会话到达终态
//   - 目标到达 LLM
func TestCSSRefactor(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	// 预置待重构的 CSS 文件（供 Agent 读取/改写；本测试用 mock 不实际落盘改写）
	f.WS.WriteFile("styles.css", []byte(".old { padding: 1px; }"))

	f.LLM.SetDefaultResponse(fixtures.MockResponse{
		Content: "CSS refactored to use tailwind utilities.",
	})

	id := createSession(t, f, "refactor styles.css to use tailwind")
	status := waitForTerminal(t, f, id, 20*time.Second)
	if status == "timeout" {
		t.Fatalf("session %s did not terminate within timeout", id)
	}
	if len(f.LLM.Requests()) == 0 {
		t.Errorf("mock LLM did not receive any requests")
	}
	if !promptContains(f, "tailwind") {
		t.Errorf("goal 'tailwind' did not reach the LLM")
	}
}
