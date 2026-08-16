// Package agent 结构化问答（TODO #53）单元测试：
// 选项答复解析 / 确认裁决 / ApprovalHook 选项化 / ask_user hook 选项透传。
package agent

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

// TestParseClarifyAnswer 验证选项答复解析（TODO #53）：
// 选项 ID / Label / 数字序号三种命中、多选分隔（逗号/空格/顿号）、去重、
// 未命中回退自由文本、无选项（nil/空）原样透传。
func TestParseClarifyAnswer(t *testing.T) {
	pc := &ClarifyRequest{Kind: "choice", Options: []ClarifyOption{
		{ID: "dark", Label: "深色", Description: "护眼"},
		{ID: "light", Label: "浅色"},
		{ID: "auto", Label: "跟随系统"},
	}}
	cases := []struct {
		name     string
		answer   string
		wantText string
		wantIDs  []string
	}{
		{"option id hit", "light", "浅色", []string{"light"}},
		{"option label hit", "深色", "深色", []string{"dark"}},
		{"numeric hit", "2", "浅色", []string{"light"}},
		{"multi comma", "1,3", "深色、跟随系统", []string{"dark", "auto"}},
		{"multi chinese comma", "1，3", "深色、跟随系统", []string{"dark", "auto"}},
		{"multi space", "light auto", "浅色、跟随系统", []string{"light", "auto"}},
		{"multi 顿号", "1、2", "深色、浅色", []string{"dark", "light"}},
		{"dedupe", "1,1", "深色", []string{"dark"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, ids := parseClarifyAnswer(tc.answer, pc)
			if text != tc.wantText {
				t.Fatalf("want text %q, got %q", tc.wantText, text)
			}
			if len(ids) != len(tc.wantIDs) {
				t.Fatalf("want ids %v, got %v", tc.wantIDs, ids)
			}
			for i := range ids {
				if ids[i] != tc.wantIDs[i] {
					t.Fatalf("want ids %v, got %v", tc.wantIDs, ids)
				}
			}
		})
	}

	// 未命中回退自由文本（含部分命中）。
	for _, fallback := range []string{"随便", "1, 随便", "4", "0", "-1"} {
		text, ids := parseClarifyAnswer(fallback, pc)
		if text != fallback || ids != nil {
			t.Fatalf("answer %q 应回退自由文本 (text=%q ids=%v)", fallback, text, ids)
		}
	}
	// 无选项：原样透传。
	if text, ids := parseClarifyAnswer("x", nil); text != "x" || ids != nil {
		t.Fatalf("nil pc 应原样透传, got %q %v", text, ids)
	}
	if text, ids := parseClarifyAnswer("x", &ClarifyRequest{}); text != "x" || ids != nil {
		t.Fatalf("空选项 pc 应原样透传, got %q %v", text, ids)
	}
}

// TestResolveApproval 验证确认裁决（TODO #53 选项化）：
// confirm/reject 选项 ID/数字序号直接裁决优先于关键词；未命中回退 parseApproval；fail-closed。
func TestResolveApproval(t *testing.T) {
	pc := &ClarifyRequest{Kind: "confirm", Options: []ClarifyOption{
		{ID: "confirm", Label: "确认执行"},
		{ID: "reject", Label: "拒绝取消"},
	}}
	cases := map[string]bool{
		// 选项 ID / 序号 / Label 直接裁决。
		"confirm":   true,
		"1":         true,
		"确认执行":   true,
		"reject":    false,
		"2":         false,
		"拒绝取消":   false,
		// 未命中回退关键词（自由文本兑底）。
		"确认":       true,
		"yes":       true,
		"拒绝":       false,
		"随便吧":     false,
		"":          false,
	}
	for answer, want := range cases {
		if got := resolveApproval(answer, pc); got != want {
			t.Fatalf("resolveApproval(%q) = %v, want %v", answer, got, want)
		}
	}
	// 无选项时退化为纯关键词判定。
	if got := resolveApproval("确认", nil); !got {
		t.Fatal("nil pc 时应走 parseApproval 关键词：确认 → true")
	}
	if got := resolveApproval("no", &ClarifyRequest{}); got {
		t.Fatal("空选项 pc 时应走 parseApproval 关键词：no → false")
	}
}

// TestRecordClarifyAnswer 验证答复写回 pendingClarify：Answer/AnswerOptionIDs/AnsweredAt。
func TestRecordClarifyAnswer(t *testing.T) {
	pc := &ClarifyRequest{Options: []ClarifyOption{
		{ID: "a", Label: "方案A"},
		{ID: "b", Label: "方案B"},
	}}
	text, ids := recordClarifyAnswer(pc, "2")
	if text != "方案B" || len(ids) != 1 || ids[0] != "b" {
		t.Fatalf("recordClarifyAnswer: text=%q ids=%v", text, ids)
	}
	if pc.Answer != "方案B" || len(pc.AnswerOptionIDs) != 1 || pc.AnswerOptionIDs[0] != "b" {
		t.Fatalf("pendingClarify 应记录回传文本与选项 ID: %+v", pc)
	}
	if pc.AnsweredAt == nil {
		t.Fatal("AnsweredAt 应被记录")
	}
	// 自由文本回退：原文写入 Answer，无选项 ID。
	pc2 := &ClarifyRequest{Options: []ClarifyOption{{ID: "a", Label: "方案A"}}}
	text2, ids2 := recordClarifyAnswer(pc2, "按第三方案来")
	if text2 != "按第三方案来" || ids2 != nil || pc2.Answer != "按第三方案来" || pc2.AnswerOptionIDs != nil {
		t.Fatalf("自由文本应原样记录: text=%q ids=%v pc=%+v", text2, ids2, pc2)
	}
}

// TestApprovalHook_StructuredOptions 验证破坏性确认选项化（TODO #53）：
// pendingClarify 含 Kind=confirm 与确认/拒绝两选项；数字序号答复 1/2 直接裁决放行/拒绝。
func TestApprovalHook_StructuredOptions(t *testing.T) {
	for _, tc := range []struct {
		answer    string
		wantWrite bool
	}{
		{"1", true},   // 序号 1 = 确认执行
		{"2", false},  // 序号 2 = 拒绝取消
		{"reject", false},
	} {
		t.Run(tc.answer, func(t *testing.T) {
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
			roleRegistry := role.NewRegistry(&pkgconfig.RoleConfigFile{})
			toolRegistry := tool.NewBuiltinRegistry(dir, &config.AgentConfig{SafetyConfig: config.SafetyConfig{ProductionWorkDir: dir}}, nil)
			svc := NewReactService(roleRegistry, nil, toolRegistry, nil, NopMemoryPipeline{}, nil)
			svc.testProvider = llm
			svc.store.workDir = dir
			toolRegistry.SetApprovalHook(svc.ApprovalHook())
			ctx := context.Background()

			created, err := svc.CreateSession(ctx, CreateRequest{Goal: "write out.txt"})
			if err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			deadline := time.Now().Add(5 * time.Second)
			var sess *Session
			for time.Now().Before(deadline) {
				sess, _ = svc.Get(ctx, created.ID)
				if sess != nil && sess.PendingClarify != nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if sess == nil || sess.PendingClarify == nil {
				t.Fatal("expected approval request within timeout")
			}
			pc := sess.PendingClarify
			if pc.Kind != "confirm" {
				t.Fatalf("approval 的 pendingClarify 应为 Kind=confirm, got %q", pc.Kind)
			}
			if len(pc.Options) != 2 || pc.Options[0].ID != "confirm" || pc.Options[1].ID != "reject" {
				t.Fatalf("approval 应带 确认/拒绝 两选项, got %+v", pc.Options)
			}
			if pc.Options[0].Label != "确认执行" || pc.Options[1].Label != "拒绝取消" {
				t.Fatalf("approval 选项文案不符, got %+v", pc.Options)
			}

			if err := svc.sendMessage(ctx, created.ID, tc.answer); err != nil {
				t.Fatalf("sendMessage: %v", err)
			}
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
			_, err = os.Stat(filepath.Join(dir, "out.txt"))
			if tc.wantWrite && err != nil {
				t.Fatalf("file should be written after option 1: %v", err)
			}
			if !tc.wantWrite && err == nil {
				t.Fatal("file must not be written when denied via option")
			}
		})
	}
}

// TestAskUserHook_OptionsPassthrough 验证 ask_user hook 透传结构化选项（TODO #53）：
// 模型传 options/multi_select → pendingClarify Kind=choice + Options + MultiSelect；
// 用户按数字序号答复 → 工具结果回传选项 Label，进入下一轮 LLM 请求。
func TestAskUserHook_OptionsPassthrough(t *testing.T) {
	llm := &askCaptureProvider{responses: []*blades.Message{
		{
			Role: blades.RoleAssistant,
			Parts: []blades.Part{
				blades.ToolPart{Name: "ask_user", Request: string(mustJSON(map[string]any{
					"question":     "配色方向？",
					"multi_select": true,
					"options": []map[string]any{
						{"id": "dark", "label": "深色", "description": "护眼"},
						{"id": "light", "label": "浅色"},
					},
				}))},
			},
		},
		blades.AssistantMessage("完成，按用户选择（浅色）实现"),
	}}
	svc := newAskUserTestService(t, llm)
	ctx := context.Background()

	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "实现登录页"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var sess *Session
	for time.Now().Before(deadline) {
		sess, _ = svc.Get(ctx, created.ID)
		if sess != nil && sess.PendingClarify != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sess == nil || sess.PendingClarify == nil {
		t.Fatal("expected ask_user pending within timeout")
	}
	pc := sess.PendingClarify
	if pc.Kind != "choice" {
		t.Fatalf("带选项提问应为 Kind=choice, got %q", pc.Kind)
	}
	if !pc.MultiSelect {
		t.Fatal("multi_select=true 应透传到 pendingClarify")
	}
	if len(pc.Options) != 2 || pc.Options[0].ID != "dark" || pc.Options[0].Label != "深色" || pc.Options[1].ID != "light" {
		t.Fatalf("options 应透传到 pendingClarify, got %+v", pc.Options)
	}

	// 数字序号答复：第 2 项 = 浅色，工具结果回传 Label。
	if err := svc.sendMessage(ctx, created.ID, "2"); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sess, _ = svc.Get(ctx, created.ID)
		if sess != nil && sess.Status == string(enums.SessionStatusCompleted) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sess == nil || sess.Status != string(enums.SessionStatusCompleted) {
		t.Fatalf("session did not complete after option answer, status=%v", sess.Status)
	}
	if !llm.requestContains("用户答复: 浅色") {
		t.Fatalf("option label 应作为工具结果进入下一轮 LLM 请求")
	}
	if !strings.Contains(sess.Result, "浅色") {
		t.Fatalf("final answer should reflect chosen option, got: %s", sess.Result)
	}
}
