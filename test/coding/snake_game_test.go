//go:build integration

package coding_test

import (
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/test/fixtures"
)

// TestSnakeGame 编程主场景端到端：让 Agent 写一个贪吃蛇游戏。
// 用 mock LLM 驱动会话走完整 graph+memory 栈，断言：
//   - 会话能从 running 到达终态（completed/error，不挂起）
//   - 目标确实到达 LLM（证明 goal 经路由派发到模型）
//   - LLM 被调用过
//
// 注：断言"文件确实落盘"需要确定性的工具调用序列（RegisterSequence 已就绪可供后续
// 精细化场景使用）；本测试聚焦端到端会话可走通这一可验证目标。
func TestSnakeGame(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	f.LLM.SetDefaultResponse(fixtures.MockResponse{
		Content: "I'll write a snake game for you. Done.",
	})

	id := createSession(t, f, "write a snake game in Go")
	status := waitForTerminal(t, f, id, 20*time.Second)
	if status == "timeout" {
		t.Fatalf("session %s did not terminate within timeout", id)
	}
	if len(f.LLM.Requests()) == 0 {
		t.Errorf("mock LLM did not receive any requests")
	}
	if !promptContains(f, "snake") {
		t.Errorf("goal 'snake' did not reach the LLM")
	}
}
