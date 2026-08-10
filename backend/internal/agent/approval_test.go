// Package agent 审批流单元测试：破坏性工具生产边界确认（TODO #17 P1）。
package agent

// approval_test.go 验证 ApprovalHook 全链路：
//   - 生产目录下 WriteFile 触发确认，会话暂停（awaiting_clarify + PendingClarify 透出）；
//   - 用户答复「确认」→ 放行 → 文件写入 → 会话正常完成；
//   - 用户答复「拒绝」→ 拒绝 → 文件不写 → 会话正常完成；
//   - 会话取消时审批阻塞被 ctx 解除（不永久挂起）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/go-kratos/blades"
)

// newApprovalTestService 构造带生产目录配置 + 审批钩子接线的 ReactService。
func newApprovalTestService(t *testing.T, provider ModelProvider, workDir, prodDir string) *ReactService {
	t.Helper()
	roleRegistry := role.NewRegistry(&pkgconfig.RoleConfigFile{})
	toolRegistry := tool.NewBuiltinRegistry(workDir, &config.AgentConfig{SafetyConfig: config.SafetyConfig{ProductionWorkDir: prodDir}}, nil)
	s := NewReactService(roleRegistry, nil, toolRegistry, nil, NopMemoryPipeline{}, nil)
	s.testProvider = provider
	s.store.workDir = workDir
	toolRegistry.SetApprovalHook(s.ApprovalHook())
	return s
}

// TestReactService_ApprovalFlow_AllowAndDeny 验证确认/拒绝两路闭环。
func TestReactService_ApprovalFlow_AllowAndDeny(t *testing.T) {
	cases := []struct {
		name      string
		answer    string
		wantWrite bool
	}{
		{"allow", "确认", true},
		{"deny", "拒绝", false},
		{"ambiguous", "随便吧", false}, // 不明确答复 fail-closed
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			llm := &mockReactModelProvider{responses: []*blades.Message{
				{
					Role: blades.RoleAssistant,
					Parts: []blades.Part{
						blades.ToolPart{Name: "WriteFile", Request: string(mustJSON(map[string]any{"path": "out.txt", "content": "42"}))},
					},
				},
				blades.AssistantMessage("done"),
			}}
			svc := newApprovalTestService(t, llm, dir, dir)
			ctx := context.Background()

			created, err := svc.CreateSession(ctx, CreateRequest{Goal: "write out.txt"})
			if err != nil {
				t.Fatalf("CreateSession: %v", err)
			}

			// 等待会话进入待审批（Agent goroutine 阻塞在 ApprovalHook，会话转 awaiting_clarify）。
			deadline := time.Now().Add(5 * time.Second)
			var sess *Session
			for time.Now().Before(deadline) {
				sess, err = svc.Get(ctx, created.ID)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				if sess.PendingClarify != nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if sess == nil || sess.PendingClarify == nil {
				t.Fatal("expected approval request within timeout")
			}
			if !strings.Contains(sess.PendingClarify.Question, "需确认") {
				t.Fatalf("expected 需确认 question, got: %q", sess.PendingClarify.Question)
			}
			if sess.Status != string(enums.SessionStatusAwaitingClarify) {
				t.Fatalf("expected awaiting_clarify while approval pending, got %q", sess.Status)
			}

			// 用户答复走 sendMessage（TUI 普通输入路径）。
			if err := svc.sendMessage(ctx, created.ID, tc.answer); err != nil {
				t.Fatalf("sendMessage: %v", err)
			}

			// 等待会话完成。
			deadline = time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				sess, _ = svc.Get(ctx, created.ID)
				if sess.Status == string(enums.SessionStatusCompleted) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if sess.Status != string(enums.SessionStatusCompleted) {
				t.Fatalf("expected completed, got %q", sess.Status)
			}

			// 文件写入断言。
			_, err = os.Stat(filepath.Join(dir, "out.txt"))
			if tc.wantWrite && err != nil {
				t.Fatalf("file should be written after approval: %v", err)
			}
			if !tc.wantWrite && err == nil {
				t.Fatal("file must not be written when denied")
			}
		})
	}
}

// TestReactService_ApprovalFlow_CancelUnblocks 验证审批等待中取消会话：
// ApprovalHook 的 ctx 取消解除阻塞，Agent 循环退出，会话置 error，不永久挂起。
func TestReactService_ApprovalFlow_CancelUnblocks(t *testing.T) {
	dir := t.TempDir()
	llm := &mockReactModelProvider{responses: []*blades.Message{
		{
			Role: blades.RoleAssistant,
			Parts: []blades.Part{
				blades.ToolPart{Name: "WriteFile", Request: string(mustJSON(map[string]any{"path": "out.txt", "content": "42"}))},
			},
		},
	}}
	svc := newApprovalTestService(t, llm, dir, dir)
	ctx := context.Background()
	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "write out.txt"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// 等待进入待审批。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sess, _ := svc.Get(ctx, created.ID)
		if sess.PendingClarify != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 审批等待中取消会话：应解除阻塞并正常收敛。
	if err := svc.cancel(ctx, created.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	var sess *Session
	for time.Now().Before(deadline) {
		sess, _ = svc.Get(ctx, created.ID)
		if sess.Status == string(enums.SessionStatusError) || sess.Status == string(enums.SessionStatusCompleted) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sess == nil || (sess.Status != string(enums.SessionStatusError) && sess.Status != string(enums.SessionStatusCompleted)) {
		t.Fatalf("expected session terminated after cancel during approval, got %+v", sess)
	}
}

// TestParseApproval 验证答复解析：明确同意 → true，其余 fail-closed。
func TestParseApproval(t *testing.T) {
	cases := map[string]bool{
		"确认": true, "同意": true, "yes": true, "YES": true, "ok": true, "y": true, "允许": true,
		"拒绝": false, "no": false, "取消": false, "随便": false, "": false, " 确认 ": true,
	}
	for in, want := range cases {
		if got := parseApproval(in); got != want {
			t.Errorf("parseApproval(%q) = %v, want %v", in, got, want)
		}
	}
}
