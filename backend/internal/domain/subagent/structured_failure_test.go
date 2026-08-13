package subagent

// structured_failure_test.go 验证 TODO #23 结构化失败：
//   - 机读失败标记 [failure kind=X retryable=Y] 随父 mailbox 失败消息透出；
//   - 叶子助手 kind=error 失败自动重派一次（同任务同前缀），成功后正常回灌；
//   - domain 与不可重试 kind 不自动重试；
//   - send_message escalate 类型 + 死信可见（目标已销毁返"消息未送达"）。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/go-kratos/blades"
)

// countedProvider 前 failCalls 次 Generate 返回错误，之后返回固定文本。
// 用于驱动"首轮失败 -> 自动重派 -> 次轮成功"的确定性序列。
type countedProvider struct {
	failCalls int
	text      string
	calls     int
}

func (p *countedProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	if p.calls < p.failCalls {
		p.calls++
		return nil, errors.New("mock transient failure")
	}
	p.calls++
	return &blades.ModelResponse{Message: blades.AssistantMessage(p.text)}, nil
}
func (p *countedProvider) Name() string { return "counted" }

// TestFailureKindOf 失败错误 -> 结构化 kind 分类。
func TestFailureKindOf(t *testing.T) {
	expiredCtx, cancel := context.WithTimeout(context.Background(), -time.Second)
	defer cancel()
	cases := []struct {
		name string
		err  error
		want FailureKind
	}{
		{"timeout", context.DeadlineExceeded, FailureKindTimeout},
		{"expired ctx err", expiredCtx.Err(), FailureKindTimeout},
		{"loop guard", tool.ErrLoopExit, FailureKindLoopGuard},
		{"budget partial", errPartialReturn, FailureKindBudget},
		{"generic", errors.New("boom"), FailureKindError},
	}
	for _, c := range cases {
		if got := failureKindOf(c.err); got != c.want {
			t.Fatalf("%s: failureKindOf=%s want=%s", c.name, got, c.want)
		}
	}
}

// TestFailureMarker 机读标记格式。
func TestFailureMarker(t *testing.T) {
	if got := failureMarker(FailureKindTimeout, false); got != "[failure kind=timeout retryable=false]" {
		t.Fatalf("unexpected marker: %s", got)
	}
	if got := failureMarker(FailureKindError, true); got != "[failure kind=error retryable=true]" {
		t.Fatalf("unexpected marker: %s", got)
	}
}

// TestDispatcher_AutoRetry_LeafErrorSucceedsOnSecondAttempt 叶子助手 kind=error 失败
// 自动重派一次，二次成功：父 mailbox 收正常完成消息（非失败）。
func TestDispatcher_AutoRetry_LeafErrorSucceedsOnSecondAttempt(t *testing.T) {
	d, mb, _, toolsReg, _ := newSalvageTestEnv(t, &countedProvider{failCalls: 1, text: "done"})
	d.WithDispatchRetryCount(1)

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":     "code_assistant",
		"task":        "写文件",
		"verify_kind": "none", // 本测试验证自动重派机制，校验分层无关
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}

	var got *mailbox.Message
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msgs := mb.Drain("s1")
		if len(msgs) > 0 {
			got = msgs[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("parent mailbox got no message after auto-retry")
	}
	if strings.Contains(got.Body, "failure kind") {
		t.Fatalf("retried success should not carry failure marker, got: %s", got.Body)
	}
	if got.Body != "done" {
		t.Fatalf("expected successful summary 'done', got: %q", got.Body)
	}
}

// TestDispatcher_AutoRetry_LeafErrorFailsAfterRetry 重派后仍失败：父收
// [failure kind=error retryable=true] 机读标记 + 人读文案。
func TestDispatcher_AutoRetry_LeafErrorFailsAfterRetry(t *testing.T) {
	d, mb, _, toolsReg, _ := newSalvageTestEnv(t, &countedProvider{failCalls: 99, text: "x"})
	d.WithDispatchRetryCount(1)

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":     "code_assistant",
		"task":        "写文件",
		"verify_kind": "none", // 本测试验证自动重派机制，校验分层无关
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}

	var got *mailbox.Message
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msgs := mb.Drain("s1")
		if len(msgs) > 0 {
			got = msgs[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("parent mailbox got no failure message after retry exhausted")
	}
	if !strings.Contains(got.Body, "[failure kind=error retryable=true]") {
		t.Fatalf("expected structured failure marker, got: %s", got.Body)
	}
	if !strings.Contains(got.Body, "sub-agent failed") {
		t.Fatalf("human-readable failure text should follow marker, got: %s", got.Body)
	}
}

// TestDispatcher_NoAutoRetryForDomain domain 失败不自动重派：标记 retryable=false。
func TestDispatcher_NoAutoRetryForDomain(t *testing.T) {
	d, mb, _, toolsReg, _ := newSalvageTestEnv(t, &countedProvider{failCalls: 99, text: "x"})
	d.WithDispatchRetryCount(1)

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"domain":         "配置",
		"task":           "实现 config.js",
		"responsibility": "负责配置",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}

	var got *mailbox.Message
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msgs := mb.Drain("s1")
		if len(msgs) > 0 {
			got = msgs[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("parent mailbox got no failure message")
	}
	if !strings.Contains(got.Body, "[failure kind=error retryable=false]") {
		t.Fatalf("domain failure should be non-retryable, got: %s", got.Body)
	}
}

// TestSendMessageTool_Escalate send_message 支持 message_type=escalate -> MsgEscalate。
func TestSendMessageTool_Escalate(t *testing.T) {
	d, mb, _, toolsReg, _ := newSalvageTestEnv(t, &mockProvider{text: "x"})
	d.RegisterMessagingTool(toolsReg)

	res, err := toolsReg.Dispatch(agent.WithAgentID(context.Background(), "s1/domain-1"), "send_message", map[string]any{
		"to_agent_id":  "s1",
		"subject":      "无法自治",
		"body":         "测试脚本无法编写",
		"message_type": "escalate",
	})
	if err != nil || !res.Success {
		t.Fatalf("escalate dispatch failed: err=%v res=%+v", err, res)
	}
	msgs := mb.Drain("s1")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 escalate message, got %d", len(msgs))
	}
	if msgs[0].Type != mailbox.MsgEscalate {
		t.Fatalf("expected MsgEscalate, got %q", msgs[0].Type)
	}
}

// TestSendMessageTool_DeadLetter 目标已销毁（Purge 过）时返回"消息未送达"。
func TestSendMessageTool_DeadLetter(t *testing.T) {
	d, mb, _, toolsReg, _ := newSalvageTestEnv(t, &mockProvider{text: "x"})
	d.RegisterMessagingTool(toolsReg)
	mb.Purge("s1/domain-gone")

	res, err := toolsReg.Dispatch(agent.WithAgentID(context.Background(), "s1"), "send_message", map[string]any{
		"to_agent_id": "s1/domain-gone",
		"subject":     "hello",
	})
	if err != nil {
		t.Fatalf("dispatch should not error: %v", err)
	}
	if res.Success {
		t.Fatalf("send to purged recipient should fail, got: %+v", res)
	}
	if !strings.Contains(res.Error, "消息未送达") {
		t.Fatalf("expected dead-letter error text, got: %s", res.Error)
	}
}
