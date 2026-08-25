package subagent

// verify_template_test.go 测试 TODO #63 证据模板同源注入：
// verify_kind=executable 角色的任务尾部注入 tool.VerificationEvidenceTemplate()
// （与识别器同一份词表），verify_kind=none 不注入。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/go-kratos/blades"
)

// captureTextProvider 记录每次 Generate 请求的全部消息文本，返回固定文本。
type captureTextProvider struct {
	mu   sync.Mutex
	seen []string
}

func (p *captureTextProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var sb strings.Builder
	for _, m := range req.Messages {
		for _, part := range m.Parts {
			fmt.Fprintf(&sb, "%v\n", part)
		}
	}
	p.seen = append(p.seen, sb.String())
	return &blades.ModelResponse{Message: blades.AssistantMessage("done")}, nil
}
func (p *captureTextProvider) Name() string { return "capture-text" }

// firstRequest 返回首次请求的消息文本（任务注入在首轮即应可见）。
func (p *captureTextProvider) firstRequest() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.seen) == 0 {
		return ""
	}
	return p.seen[0]
}

// TestDispatch_InjectsVerificationEvidenceTemplate 证据模板随任务注入：
// executable 角色首轮请求的任务尾部含【验证证据格式】与 EXIT_CODE=0 口径；
// none 角色不注入（模板只服务 L0 可执行校验路径）。
func TestDispatch_InjectsVerificationEvidenceTemplate(t *testing.T) {
	exec := &captureTextProvider{}
	_, mb, toolsReg := newVerifyTestEnv(t, &mockModelFactory{provider: exec})

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":     "code_assistant",
		"task":        "写文件",
		"verify_kind": "executable",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	waitMailboxBody(t, mb, "s1")

	first := exec.firstRequest()
	if !strings.Contains(first, "写文件") {
		t.Fatalf("task text missing in first request: %q", first)
	}
	if !strings.Contains(first, "【验证证据格式】") || !strings.Contains(first, "EXIT_CODE=0") {
		t.Fatalf("executable role should carry evidence template, got: %q", first)
	}

	noneP := &captureTextProvider{}
	_, mb2, toolsReg2 := newVerifyTestEnv(t, &mockModelFactory{provider: noneP})

	res, err = toolsReg2.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":     "code_assistant",
		"task":        "写文件",
		"verify_kind": "none",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	waitMailboxBody(t, mb2, "s1")

	if got := noneP.firstRequest(); strings.Contains(got, "【验证证据格式】") {
		t.Fatalf("verify_kind=none should not inject evidence template, got: %q", got)
	}
}
