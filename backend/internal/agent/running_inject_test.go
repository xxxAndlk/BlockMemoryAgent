package agent

// running_inject_test.go 覆盖"运行中重新输入"与派发事件解析（2026-09-12 用户实证）：
//
//   - 运行中的会话收到用户消息必须**投进 MetaAgent 邮箱**：此前只写 session.Messages，
//     而运行中的 ReAct 主循环从不读该字段（只在 resumeSession 建新轮时当输入用），
//     用户看着"已发送"、Agent 毫无反应——"任务全部派发出去、正在等子 Agent"时最刺眼。
//   - 派发事件解析：一次批量派发（call_sub_agents）要落 N 条事件（对话栏子 Agent 列表
//     一人一行），并把中文领域名带出（事件 Tool 字段只放得下角色 ID，domain 恒为
//     "domain"，九个领域会同名）。
//   - 用户注入的邮箱消息不是子 Agent 完成，不得推 sub_agent_done 假完成事件。

import (
	"context"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
)

// blockingProvider 卡住首次 LLM 调用，让会话稳定停在"运行中"，便于测试注入路径；
// release 关闭后返回普通答复，会话正常收尾。
type blockingProvider struct {
	entered chan struct{}
	release chan struct{}
}

func (p *blockingProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	<-p.release
	return &blades.ModelResponse{Message: blades.AssistantMessage("done")}, nil
}

func (p *blockingProvider) Name() string { return "blocking" }

// TestSendMessage_RunningInjectsIntoMailbox 运行中发消息 → 投递到该会话的 MetaAgent 邮箱
// （而不是只躺在 session.Messages 里）。
func TestSendMessage_RunningInjectsIntoMailbox(t *testing.T) {
	prov := &blockingProvider{entered: make(chan struct{}, 1), release: make(chan struct{})}
	svc := newReactServiceForTest(prov, "")
	fm := &fakeMessenger{}
	svc.SetAgentMessenger(fm)
	defer svc.Shutdown(context.Background())

	sess, err := svc.CreateSession(context.Background(), CreateRequest{Goal: "g"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	select {
	case <-prov.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("LLM 未开始，无法构造运行中态")
	}

	if err := svc.sendMessage(context.Background(), sess.ID, "改用中文领域名重派，并停掉第 3 章那个域"); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	if len(fm.injected) != 1 || fm.injected[0] != sess.ID {
		t.Fatalf("运行中消息应投进本会话 MetaAgent 邮箱（%q），got %v", sess.ID, fm.injected)
	}
	if len(fm.contents) != 1 || fm.contents[0] != "改用中文领域名重派，并停掉第 3 章那个域" {
		t.Fatalf("注入内容应与用户输入一致, got %v", fm.contents)
	}

	close(prov.release)
	waitCompleted(t, svc, sess.ID)
}

// TestEnqueue_NonRunningSeedsInput 未运行会话的 enqueue 必须把内容补进对话消息：
// resumeSession 以"最后一条 user 消息"为本轮输入，不补就同样丢失（原实现两处都没投递）。
func TestEnqueue_NonRunningSeedsInput(t *testing.T) {
	prov := &multiInstructionProvider{text: "done"}
	svc := newReactServiceForTest(prov, "")
	defer svc.Shutdown(context.Background())

	sess, err := svc.CreateSession(context.Background(), CreateRequest{Goal: "g1"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	waitCompleted(t, svc, sess.ID)

	if err := svc.enqueue(context.Background(), sess.ID, "补一条补充说明"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, err := svc.Get(context.Background(), sess.ID)
		if err == nil {
			for _, m := range got.Messages {
				if m.Content == "补一条补充说明" {
					return // 已补进对话消息，下一轮能作为输入取到
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("enqueue 的内容未进会话消息（会被 resumeSession 丢弃）")
}

// TestParseSubAgentDispatch 派发事件解析：单派发 1 项、批量逐项、坏 JSON 兜底不丢展示。
func TestParseSubAgentDispatch(t *testing.T) {
	single := `{"role_id":"domain","domain":"文档修订-第3章","task":"第 3 章:\n补充过时知识点并修正示例","responsibility":"只改第 3 章"}`
	got := parseSubAgentDispatch(single)
	if len(got) != 1 {
		t.Fatalf("单派发应 1 项, got %d", len(got))
	}
	if got[0].Domain != "文档修订-第3章" || got[0].RoleID != "domain" {
		t.Fatalf("单派发字段解析错: %+v", got[0])
	}
	if got[0].Task != "第 3 章: 补充过时知识点并修正示例" {
		t.Fatalf("task 应单行化, got %q", got[0].Task)
	}

	batch := `{"tasks":[{"role_id":"domain","domain":"第1章修订","task":"a"},{"role_id":"domain","domain":"第2章修订","task":"b"},{"role_id":"code_assistant","task":"c"}]}`
	got = parseSubAgentDispatch(batch)
	if len(got) != 3 {
		t.Fatalf("批量应逐项 3 条（对话栏一人一行）, got %d", len(got))
	}
	if got[0].Domain != "第1章修订" || got[2].Domain != "" || got[2].RoleID != "code_assistant" {
		t.Fatalf("批量字段解析错: %+v", got)
	}

	got = parseSubAgentDispatch(`{不是 JSON`)
	if len(got) != 1 || got[0].Task == "" {
		t.Fatalf("坏 JSON 应兜底为原文摘要一项, got %+v", got)
	}
}

// TestDispatchDomainJSON 领域名只在非空时落 detail_json（不给事件加噪声）。
func TestDispatchDomainJSON(t *testing.T) {
	if dispatchDomainJSON("") != "" {
		t.Fatal("空领域名不应写 detail_json")
	}
	if got := dispatchDomainJSON("文档修订-第3章"); got != `{"domain":"文档修订-第3章"}` {
		t.Fatalf("领域名 JSON 错: %s", got)
	}
}

// TestDrainMailbox_UserMessageNoSubAgentDone 用户注入（From=user）不从邮箱冒泡
// sub_agent_done 完成事件——否则对话栏会多出一条"子Agent 完成: user"的假完成。
func TestDrainMailbox_UserMessageNoSubAgentDone(t *testing.T) {
	mb := mailbox.New()
	ag := NewReActAgent("session-1", types.RoleDefinition{SystemPrompt: "t"}, nil, nil).WithMailbox(mb)
	var kinds []string
	ag.WithLiveEvents(func(ev LiveEvent) { kinds = append(kinds, string(ev.Kind)) })

	if _, err := mb.Send(&mailbox.Message{From: "user", To: "session-1", Type: mailbox.MsgRequest, Subject: "用户新指令", Body: "补充一句"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	messages := []ReactMessage{}
	history, n := ag.drainMailbox(messages)
	if n != 1 || len(history) != 1 {
		t.Fatalf("用户消息应入史 1 条, n=%d history=%d", n, len(history))
	}
	for _, k := range kinds {
		if k == string(LiveEventSubAgentDone) {
			t.Fatalf("用户注入不得推子 Agent 完成事件, got %v", kinds)
		}
	}
}

// TestDrainMailbox_PeerAskEvent 跨 Agent 询问（MsgRequest/MsgEscalate）单列 peer_ask
// 事件（用户能看到问答在用）；普通通知（MsgInfo）仍走 sub_agent_done。
func TestDrainMailbox_PeerAskEvent(t *testing.T) {
	cases := []struct {
		msgType mailbox.MessageType
		want    string
	}{
		{mailbox.MsgRequest, string(LiveEventPeerAsk)},
		{mailbox.MsgEscalate, string(LiveEventPeerAsk)},
		{mailbox.MsgInfo, string(LiveEventSubAgentDone)},
	}
	for _, c := range cases {
		mb := mailbox.New()
		ag := NewReActAgent("session-1", types.RoleDefinition{SystemPrompt: "t"}, nil, nil).WithMailbox(mb)
		var kinds []string
		ag.WithLiveEvents(func(ev LiveEvent) { kinds = append(kinds, string(ev.Kind)) })

		if _, err := mb.Send(&mailbox.Message{From: "domain-2", To: "session-1", Type: c.msgType, Subject: "接口口径", Body: "x"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		ag.drainMailbox([]ReactMessage{})
		if len(kinds) != 1 || kinds[0] != c.want {
			t.Fatalf("type=%s 应推 %s, got %v", c.msgType, c.want, kinds)
		}
	}
}
