package model

// provider_anthropic_test.go 验证 max_tokens 超限自愈钳制逻辑
// （2026-08-13 代码助手配 65536 撞 ark /api/coding kimi 硬上限 32768 全挂的根因场景）。

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"        // Anthropic Go SDK
	"github.com/go-kratos/blades"                   // blades 消息抽象
	bladestools "github.com/go-kratos/blades/tools" // blades 工具定义
	"github.com/google/jsonschema-go/jsonschema"    // JSON Schema
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

// TestConvertSystem_CacheControlBreakpoint 验证 prompt cache 断点：
// 仅最后一个 system block 带 cache_control ephemeral，其余 block 不带；
// system 为空/无文本 part 时返回 nil 不 panic。
func TestConvertSystem_CacheControlBreakpoint(t *testing.T) {
	p := &anthropicProvider{}

	// 多文本 part：仅最后一个 block 打断点。
	blocks := p.convertSystem(&blades.Message{Role: blades.RoleSystem, Parts: []blades.Part{
		blades.TextPart{Text: "第一段"},
		blades.TextPart{Text: "第二段"},
	}})
	if len(blocks) != 2 {
		t.Fatalf("应有 2 个 system block, got %d", len(blocks))
	}
	if blocks[0].CacheControl.Type != "" {
		t.Fatalf("非末尾 block 不应带 cache_control: %+v", blocks[0].CacheControl)
	}
	if blocks[1].CacheControl.Type != "ephemeral" {
		t.Fatalf("末尾 block 应带 ephemeral 断点: %+v", blocks[1].CacheControl)
	}

	// 空 system / 无文本 part：nil 安全。
	if got := p.convertSystem(nil); got != nil {
		t.Fatalf("nil system 应返回 nil, got %+v", got)
	}
	if got := p.convertSystem(&blades.Message{Role: blades.RoleSystem}); len(got) != 0 {
		t.Fatalf("无文本 part 应返回空, got %+v", got)
	}
}

// TestConvertTools_CacheControlBreakpoint 验证 prompt cache 断点：
// 仅最后一个工具带 cache_control ephemeral，其余不带；空工具列表返回 nil。
func TestConvertTools_CacheControlBreakpoint(t *testing.T) {
	p := &anthropicProvider{}

	noop := bladestools.HandleFunc(func(context.Context, string) (string, error) { return "", nil })
	schema := &jsonschema.Schema{Type: "object"}
	tools := []bladestools.Tool{
		bladestools.NewTool("t1", "工具1", noop, bladestools.WithInputSchema(schema)),
		bladestools.NewTool("t2", "工具2", noop, bladestools.WithInputSchema(schema)),
	}
	out := p.convertTools(tools)
	if len(out) != 2 {
		t.Fatalf("应有 2 个工具, got %d", len(out))
	}
	if out[0].OfTool == nil || out[0].OfTool.CacheControl.Type != "" {
		t.Fatalf("非末尾工具不应带 cache_control: %+v", out[0].OfTool)
	}
	if out[1].OfTool == nil || out[1].OfTool.CacheControl.Type != "ephemeral" {
		t.Fatalf("末尾工具应带 ephemeral 断点: %+v", out[1].OfTool)
	}

	// 空工具列表：nil 安全。
	if got := p.convertTools(nil); got != nil {
		t.Fatalf("空工具列表应返回 nil, got %+v", got)
	}
}

// RoleTool 消息携带的 DataPart（仅 agent.ToBladesMessages 最新一批）转为同一
// tool_result content 里的 image 块；协议不支持的 MIME（svg 等）静默跳过。
func TestConvertMessages_ToolResultImages(t *testing.T) {
	p := &anthropicProvider{}
	raw := []byte{0x89, 0x50, 0x4E, 0x47}
	msgs := []*blades.Message{
		{Role: blades.RoleUser, Parts: []blades.Part{blades.TextPart{Text: "hi"}}},
		{Role: blades.RoleTool, Parts: []blades.Part{
			blades.ToolPart{ID: "c1", Response: "[image image/png, 4 bytes base64]"},
			blades.DataPart{MIMEType: blades.MIMEImagePNG, Bytes: raw},
		}},
		{Role: blades.RoleTool, Parts: []blades.Part{
			blades.ToolPart{ID: "c2", Response: "[image image/svg+xml, 9 bytes base64]"},
			blades.DataPart{MIMEType: "image/svg+xml", Bytes: raw}, // 协议不支持：跳过
		}},
	}
	out, err := p.convertMessages(msgs)
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	// out = [user, mergedUser(c1+c2 tool_result)]
	if len(out) != 2 {
		t.Fatalf("消息数不符: got %d want 2", len(out))
	}
	merged := out[1]
	if len(merged.Content) != 2 {
		t.Fatalf("合并后应有 2 个 tool_result, got %d", len(merged.Content))
	}
	// c1：text + image 两块，image 为 png base64。
	tr1 := merged.Content[0].OfToolResult
	if tr1 == nil || len(tr1.Content) != 2 {
		t.Fatalf("c1 tool_result 应含 text+image 两块: %+v", tr1)
	}
	img := tr1.Content[1].OfImage
	if img == nil || img.Source.OfBase64 == nil {
		t.Fatalf("c1 第二块应为 base64 image: %+v", tr1.Content[1])
	}
	if img.Source.OfBase64.Data != base64.StdEncoding.EncodeToString(raw) {
		t.Fatalf("image base64 不符: %q", img.Source.OfBase64.Data)
	}
	if img.Source.OfBase64.MediaType != anthropic.Base64ImageSourceMediaTypeImagePNG {
		t.Fatalf("image media_type 不符: %q", img.Source.OfBase64.MediaType)
	}
	// c2：svg 被跳过，仅 text 一块。
	tr2 := merged.Content[1].OfToolResult
	if tr2 == nil || len(tr2.Content) != 1 || tr2.Content[0].OfText == nil {
		t.Fatalf("c2 tool_result 应仅含 text 一块: %+v", tr2)
	}
}

// TestConvertMessage_UserImageBlocks 验证 user 消息携带的图片 DataPart
//（Alt+V 粘贴）转为顶层 image 块；白名单外 MIME 跳过；tool 角色不受影响。
func TestConvertMessage_UserImageBlocks(t *testing.T) {
	p := &anthropicProvider{}
	raw := []byte{0x89, 0x50, 0x4E, 0x47}

	// user 消息：text + png DataPart + svg DataPart（跳过）。
	m := &blades.Message{Role: blades.RoleUser, Parts: []blades.Part{
		blades.TextPart{Text: "看这张图 [image:1]"},
		blades.DataPart{MIMEType: blades.MIMEImagePNG, Bytes: raw},
		blades.DataPart{MIMEType: "image/svg+xml", Bytes: raw},
	}}
	out, err := p.convertMessage(m)
	if err != nil {
		t.Fatalf("convertMessage: %v", err)
	}
	if out.Role != anthropic.MessageParamRoleUser {
		t.Fatalf("role 不符: %v", out.Role)
	}
	if len(out.Content) != 2 {
		t.Fatalf("user 应含 text+image 两块, got %d", len(out.Content))
	}
	if out.Content[0].OfText == nil || out.Content[0].OfText.Text != "看这张图 [image:1]" {
		t.Fatalf("第一块应为 text: %+v", out.Content[0])
	}
	img := out.Content[1].OfImage
	if img == nil || img.Source.OfBase64 == nil {
		t.Fatalf("第二块应为 base64 image: %+v", out.Content[1])
	}
	if img.Source.OfBase64.MediaType != anthropic.Base64ImageSourceMediaTypeImagePNG {
		t.Fatalf("media_type 不符: %q", img.Source.OfBase64.MediaType)
	}
	if img.Source.OfBase64.Data != base64.StdEncoding.EncodeToString(raw) {
		t.Fatalf("image base64 不符: %q", img.Source.OfBase64.Data)
	}

	// assistant 角色携带 DataPart：不挂图（只支持 user 顶层块）。
	am := &blades.Message{Role: blades.RoleAssistant, Parts: []blades.Part{
		blades.TextPart{Text: "回复"},
		blades.DataPart{MIMEType: blades.MIMEImagePNG, Bytes: raw},
	}}
	aout, _ := p.convertMessage(am)
	for _, b := range aout.Content {
		if b.OfImage != nil {
			t.Fatalf("assistant 消息不应挂顶层图片块")
		}
	}
}
