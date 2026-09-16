package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
)

// imageRejectErr 是 provider 侧"模型不支持图片输入"错误的仿真形状（ark 网关实证文案）。
var imageRejectErr = errors.New(`anthropic messages POST https://ark.example/api/plan: ` +
	`400 Bad Request {"error":{"code":"InvalidParameter","message":"Model do not support image input.","type":"BadRequest"}}`)

// noVisionProvider 命名 provider：首个 failures 次调用返回图片拒绝错误，之后正常作答。
// requestsWithImg 记录每次请求是否携带图片（DataPart），供断言剥图生效。
type noVisionProvider struct {
	modelName       string
	failures        int
	requestsWithImg []bool
}

func (p *noVisionProvider) Generate(_ context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	p.requestsWithImg = append(p.requestsWithImg, reqHasImage(req))
	if p.failures > 0 {
		p.failures--
		return nil, imageRejectErr
	}
	return &blades.ModelResponse{Message: blades.AssistantMessage("done")}, nil
}

func (p *noVisionProvider) Name() string      { return "novision" }
func (p *noVisionProvider) ModelName() string { return p.modelName }

// reqHasImage 判定请求消息中是否存在图片 DataPart。
func reqHasImage(req *blades.ModelRequest) bool {
	for _, m := range req.Messages {
		if m == nil {
			continue
		}
		for _, part := range m.Parts {
			if _, ok := part.(blades.DataPart); ok {
				return true
			}
		}
	}
	return false
}

func testPNGImage() tool.ResultImage {
	return tool.ResultImage{MIMEType: "image/png", Data: []byte(base64.StdEncoding.EncodeToString([]byte("png-bytes")))}
}

// TestReActAgent_ImageUnsupportedDegradesNotFatal 验证：模型不支持图片输入时 run 不判死——
// 首次 400 后剥图降级继续（第二次请求已无图片），说明注入历史，且该模型被进程级记住。
func TestReActAgent_ImageUnsupportedDegradesNotFatal(t *testing.T) {
	const modelName = "test-novision-degrade"
	llm := &noVisionProvider{modelName: modelName, failures: 1}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	a := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "You are a tester."}, llm, NewToolRegistryAdapter(reg))

	ctx := WithUserImages(context.Background(), []tool.ResultImage{testPNGImage()})
	res, err := a.RunWithHistory(ctx, "看这张设计图", nil)
	if err != nil {
		t.Fatalf("图片拒绝不应判死, got error: %v", err)
	}
	if res.Text != "done" {
		t.Fatalf("expected 'done', got %q", res.Text)
	}
	if !modelNoImage(modelName) {
		t.Fatalf("模型 %s 应被进程级记为无视觉", modelName)
	}
	if len(llm.requestsWithImg) < 2 {
		t.Fatalf("expected >=2 LLM 调用, got %d", len(llm.requestsWithImg))
	}
	if !llm.requestsWithImg[0] {
		t.Fatalf("首次请求应携带图片（尚未学习到能力缺失）")
	}
	if llm.requestsWithImg[1] {
		t.Fatalf("降级后的请求不应再携带图片")
	}
	var foundNotice bool
	for _, m := range res.History {
		if strings.Contains(m.Content, "【视觉能力缺失】") {
			foundNotice = true
		}
	}
	if !foundNotice {
		t.Fatalf("历史中应注入视觉能力缺失说明")
	}
}

// TestReActAgent_ImageUnsupportedRepeatedErrorStillFails 验证防空转：同一 run 内
// 已因图片降级过仍复现同错（剥图路径漏了）时按普通错误上报，不无限重试。
func TestReActAgent_ImageUnsupportedRepeatedErrorStillFails(t *testing.T) {
	llm := &noVisionProvider{modelName: "test-novision-repeat", failures: 99}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	a := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "You are a tester."}, llm, NewToolRegistryAdapter(reg))

	_, err := a.Run(context.Background(), "hi")
	if err == nil {
		t.Fatalf("重复同错应上报失败，不得空转")
	}
	if !strings.Contains(err.Error(), "llm generate") {
		t.Fatalf("错误应保留 llm generate 前缀, got %v", err)
	}
	if len(llm.requestsWithImg) != 2 {
		t.Fatalf("应恰好 2 次调用（首错→降级重试→复错终止）, got %d", len(llm.requestsWithImg))
	}
}

// TestReActAgent_ImageSentForDifferentModel 验证无粘性误伤：另一模型名不受此前缓存影响，
// 图片照常进入请求。
func TestReActAgent_ImageSentForDifferentModel(t *testing.T) {
	llm := &noVisionProvider{modelName: "test-novision-clean"}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	a := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "You are a tester."}, llm, NewToolRegistryAdapter(reg))

	ctx := WithUserImages(context.Background(), []tool.ResultImage{testPNGImage()})
	if _, err := a.RunWithHistory(ctx, "看图", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(llm.requestsWithImg) != 1 || !llm.requestsWithImg[0] {
		t.Fatalf("模型未被标记为无视觉时应携带图片, got %v", llm.requestsWithImg)
	}
}

// TestImageUnsupportedMatcher 验证错误识别器（model 包）的正例与反例。
func TestImageUnsupportedMatcher(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"ark 文案", imageRejectErr, true},
		{"措辞变体+400", errors.New(`openai-chat 400: {"message":"does not support image input"}`), true},
		{"普通 400", errors.New("openai-chat 400: invalid_request_error"), false},
		{"超时", context.DeadlineExceeded, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := model.IsImageInputUnsupported(c.err); got != c.want {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// TestShouldRetryLLMCall 验证 agent 层重试判定：图片拒绝不重试，其他瞬时错误重试。
func TestShouldRetryLLMCall(t *testing.T) {
	ctx := context.Background()
	if shouldRetryLLMCall(ctx, imageRejectErr) {
		t.Fatalf("图片拒绝不应重试")
	}
	if !shouldRetryLLMCall(ctx, errors.New("connection reset by peer")) {
		t.Fatalf("瞬时错误应重试")
	}
	if shouldRetryLLMCall(ctx, context.DeadlineExceeded) {
		t.Fatalf("单次调用超时不应重试")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if shouldRetryLLMCall(cancelled, errors.New("boom")) {
		t.Fatalf("会话取消不应重试")
	}
}
