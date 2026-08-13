package model

// provider_anthropic_test.go 验证 max_tokens 超限自愈钳制逻辑
// （2026-08-13 代码助手配 65536 撞 ark /api/coding kimi 硬上限 32768 全挂的根因场景）。

import (
	"errors"
	"testing"
)

// newClampTestProvider 构造一个 maxTokens=指定值的 anthropicProvider（无真实客户端）。
func newClampTestProvider(maxTokens int64) *anthropicProvider {
	p := &anthropicProvider{modelName: "kimi-k2.7-code"}
	p.maxTokens.Store(maxTokens)
	return p
}

// TestClampMaxTokensOnError_ArkFormat 验证 ark 端点错误格式解析：
// 钳制生效、上限正确、日志路径可达。
func TestClampMaxTokensOnError_ArkFormat(t *testing.T) {
	p := newClampTestProvider(65536)
	err := errors.New(`POST "https://ark.cn-beijing.volces.com/api/coding/v1/messages": 400 Bad Request {"error":{"code":"InvalidParameter","message":"The parameter ` + "`max_tokens`" + ` specified in the request is not valid: integer above maximum value, expected a value <= 32768, but got 65536 instead. Request id: 021786588908781795aa3a4ba36fe885727fc31a219ca25b6d293"}}`)
	if !p.ClampMaxTokensOnError(err) {
		t.Fatal("超限错误应触发钳制")
	}
	if got := p.maxTokens.Load(); got != 32768 {
		t.Fatalf("钳制后 maxTokens 应为 32768，got %d", got)
	}
	// 已钳制到上限内，同错误再次出现不再钳制（避免误读）。
	if p.ClampMaxTokensOnError(err) {
		t.Fatal("已在上限内不应再次钳制")
	}
}

// TestClampMaxTokensOnError_OpenAIFormat 验证 OpenAI 系错误文案（"less than or equal to"）同样可解析。
func TestClampMaxTokensOnError_OpenAIFormat(t *testing.T) {
	p := newClampTestProvider(65536)
	err := errors.New(`400 Bad Request: max_tokens must be less than or equal to 32768`)
	if !p.ClampMaxTokensOnError(err) {
		t.Fatal("OpenAI 系超限错误应触发钳制")
	}
	if got := p.maxTokens.Load(); got != 32768 {
		t.Fatalf("钳制后 maxTokens 应为 32768，got %d", got)
	}
}

// TestClampMaxTokensOnError_IgnoresUnrelated 验证非 max_tokens 错误与无上限值错误不触发钳制。
func TestClampMaxTokensOnError_IgnoresUnrelated(t *testing.T) {
	p := newClampTestProvider(65536)
	cases := []error{
		nil,
		errors.New(`400 Bad Request {"error":{"message":"tool_call_ids did not have response messages"}}`),
		errors.New("connection refused"),
		errors.New(`max_tokens 配置错误但无上限值`),
		// 上限大于当前值时不应钳制（当前值本身合法）。
		errors.New(`max_tokens: expected a value <= 128000, but got 65536 instead`),
	}
	for _, err := range cases {
		if p.ClampMaxTokensOnError(err) {
			t.Fatalf("不应钳制：%v", err)
		}
	}
	if got := p.maxTokens.Load(); got != 65536 {
		t.Fatalf("未触发钳制时 maxTokens 应保持 65536，got %d", got)
	}
}
