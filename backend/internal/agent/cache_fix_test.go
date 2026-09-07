package agent

// cache_fix_test.go 验证 TODO #40 前缀缓存修复（块 1 时间移出 / 块 2 可观测 / 块 4 头部冻结）：
//   - buildEnvBlock 稳定段跨调用字节一致（无动态时间）；
//   - RunWithHistory 发往模型的最后一条消息为【当前时间】尾部 system 消息；
//   - systemPrompt 按实例冻结（persona 每次返回不同内容也不影响首轮后的前缀）。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
)

// recordingProvider 记录每次请求的 Messages，便于断言尾部注入。
type recordingProvider struct {
	responses []*blades.Message
	reqs      []*blades.ModelRequest
}

func (m *recordingProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	m.reqs = append(m.reqs, req)
	if len(m.responses) == 0 {
		return &blades.ModelResponse{Message: blades.AssistantMessage("done")}, nil
	}
	r := m.responses[0]
	m.responses = m.responses[1:]
	return &blades.ModelResponse{Message: r}, nil
}

func (m *recordingProvider) Name() string { return "recording" }

// TestBuildEnvBlock_StableNoTime 环境块跨调用字节一致且不含动态时间（TODO #40 块 1）。
func TestBuildEnvBlock_StableNoTime(t *testing.T) {
	dir := t.TempDir()
	first := buildEnvBlock(dir, 0)
	second := buildEnvBlock(dir, 0)
	if first != second {
		t.Fatalf("envBlock must be byte-stable across calls:\n%q\nvs\n%q", first, second)
	}
	if strings.Contains(first, "当前时间") || strings.Contains(first, "2006") {
		t.Fatalf("envBlock must not contain dynamic time, got: %q", first)
	}
}

// TestBuildTimeMessage_InjectedAtTail RunWithHistory 发给模型的最后一条消息
// 是【当前时间】system 消息（尾部不可缓存位，不破坏前缀）。
func TestBuildTimeMessage_InjectedAtTail(t *testing.T) {
	llm := &recordingProvider{
		responses: []*blades.Message{blades.AssistantMessage("hello")},
	}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	ag := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "You are a tester."}, llm, NewToolRegistryAdapter(reg))

	if _, err := ag.Run(context.Background(), "say hi"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(llm.reqs) == 0 {
		t.Fatal("no request recorded")
	}
	msgs := llm.reqs[0].Messages
	if len(msgs) == 0 {
		t.Fatal("no messages in request")
	}
	last := msgs[len(msgs)-1]
	if !strings.Contains(messageLogText(last), "【当前时间】") {
		t.Fatalf("last message must be 【当前时间】 tail message, got: %q", messageLogText(last))
	}
}

// dynamicPersona 每次 Inject 返回按实例序号变化的内容——无冻结时每次调用结果必变。
type dynamicPersona struct{ n int }

func (p dynamicPersona) Inject(systemPrompt string) string {
	return "【人格】v" + fmt.Sprintf("%d", p.n) + "\n\n" + systemPrompt
}

// TestSystemPrompt_FrozenPerInstance systemPrompt 按实例冻结：
// persona 每次返回不同内容，同一实例两次调用结果仍字节一致（TODO #40 块 4）。
func TestSystemPrompt_FrozenPerInstance(t *testing.T) {
	ag := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "You are a tester."}, nil, nil)
	ag.persona = dynamicPersona{n: 1}

	first := ag.systemPrompt()
	second := ag.systemPrompt()
	if first != second {
		t.Fatalf("systemPrompt must be frozen per instance, got:\n%q\nvs\n%q", first, second)
	}
}

// TestSystemPrompt_CrossInstanceFresh 冻结是实例级的：新实例（resume 语义）读到最新内容。
func TestSystemPrompt_CrossInstanceFresh(t *testing.T) {
	ag1 := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "You are a tester."}, nil, nil)
	ag1.persona = dynamicPersona{n: 1}
	p1 := ag1.systemPrompt()

	ag2 := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "You are a tester."}, nil, nil)
	ag2.persona = dynamicPersona{n: 2}
	p2 := ag2.systemPrompt()

	if p1 == p2 {
		t.Fatal("fresh instance must rebuild systemPrompt (resume freshness)")
	}
}
