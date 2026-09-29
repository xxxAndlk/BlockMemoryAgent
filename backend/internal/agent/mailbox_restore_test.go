package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// 恢复重投：Restore 进箱的未读消息能被 drainMailbox 正常注入历史。
func TestDrainMailboxAfterRestore(t *testing.T) {
	mb := mailbox.New()
	// 照 running_inject_test.go 的既有构造方式：NewReActAgent 默认装 NopMemoryPipeline，
	// 裸 struct 字面量会让 drainMailbox 内 a.memory.Write 空接口调用 panic。
	ag := NewReActAgent("sess-1", types.RoleDefinition{SystemPrompt: "t"}, nil, nil).WithMailbox(mb)
	if err := mb.Restore(&mailbox.Message{
		ID: "msg_r1", From: "sess-1/domain-1", To: "sess-1",
		Type: mailbox.MsgRequest, Subject: "恢复的问题", ThreadID: "msg_r1",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	history, n := ag.drainMailbox(nil)
	if n != 1 || len(history) != 1 {
		t.Fatalf("恢复消息未被 drain：n=%d history=%d", n, len(history))
	}
	if got := history[0].Content; !strings.Contains(got, "msg_r1") {
		t.Fatalf("注入文本应含消息 id：%q", got)
	}
}
