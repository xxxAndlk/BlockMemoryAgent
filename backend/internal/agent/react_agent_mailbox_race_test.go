package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
)

// midGenMailboxProvider 模拟"子 Agent 在 LLM 生成期间恰好完成"的竞态：
// 首次 Generate 返回前把子 Agent 完成摘要投入 mailbox（此时 pending 计数已归 0），
// 并给出等待型过渡文本（"请稍候。"）；第二次调用才给出真正的最终答复。
type midGenMailboxProvider struct {
	mb    *mailbox.Mailbox
	to    string
	calls int
}

// Generate 首次投递 mailbox 消息并返回过渡文本，之后返回最终答复。
func (m *midGenMailboxProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	m.calls++
	if m.calls == 1 {
		// 子 Agent 于本轮 LLM 生成期间完成：摘要进入 mailbox，PendingChildren 已归 0。
		_, _ = m.mb.Send(&mailbox.Message{From: "session-1/domain-2", To: m.to, Type: mailbox.MsgInfo, Body: "domain-2 完成摘要：实体类三文件交付"})
		return &blades.ModelResponse{Message: blades.AssistantMessage("只差 domain-2 回灌，请稍候。")}, nil
	}
	return &blades.ModelResponse{Message: blades.AssistantMessage("最终交付说明：全部完成")}, nil
}

// Name 返回 mock provider 名称。
func (m *midGenMailboxProvider) Name() string { return "mock-midgen" }

// TestReActAgent_MidGenerationMailboxNotFinal 验证竞态修复：
// 子 Agent 完成摘要在本轮 LLM 生成期间才抵达 mailbox 时，刚生成的过渡文本
// 不得被当作最终答复（实证：塔防 run4 meta 以"请稍候。"提前终结会话，
// domain-2 摘要从未进入终答）。drain 到新消息必须 continue 回主循环，
// 让模型基于完整摘要重新生成答复。
func TestReActAgent_MidGenerationMailboxNotFinal(t *testing.T) {
	mb := mailbox.New()
	llm := &midGenMailboxProvider{mb: mb, to: "test"}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	ag := NewReActAgent("test", types.RoleDefinition{SystemPrompt: ""}, llm, NewToolRegistryAdapter(reg)).
		WithMailbox(mb)

	res, err := ag.Run(context.Background(), "build something")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 终答必须是整合了 mailbox 摘要后的第二轮答复，而非"请稍候"过渡文本。
	if !strings.Contains(res.Text, "最终交付说明") {
		t.Fatalf("过渡文本被误当终答，got %q", res.Text)
	}
	if llm.calls != 2 {
		t.Fatalf("expected 2 LLM calls (mid-generation mailbox 到达后应重跑一轮), got %d", llm.calls)
	}
	// mailbox 摘要必须已进入历史（user 角色），供第二轮整合。
	found := false
	for _, msg := range res.History {
		if strings.Contains(msg.Content, "domain-2 完成摘要") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("mailbox 摘要未注入历史，终答未整合子 Agent 结果")
	}
}
