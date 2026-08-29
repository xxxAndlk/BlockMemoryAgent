package model

import (
	"context"       // 上下文传递
	"encoding/base64" // tool_result 图片块 base64 编码
	"encoding/json" // JSON 序列化
	"fmt"           // 错误格式化
	"log"           // 钳制日志
	"regexp"        // max_tokens 上限值提取
	"strconv"       // 上限值解析
	"strings"       // 字符串拼接
	"sync/atomic"   // maxTokens 并发安全（多子 Agent 共享 provider 实例）

	"github.com/anthropics/anthropic-sdk-go"         // Anthropic Go SDK
	"github.com/anthropics/anthropic-sdk-go/option"  // Anthropic 客户端选项
	"github.com/blockmemory/agent/backend/pkg/types" // 共享配置类型
	"github.com/go-kratos/blades"                    // ModelProvider 抽象
	bladestools "github.com/go-kratos/blades/tools"  // blades 工具定义
	"github.com/google/jsonschema-go/jsonschema"     // JSON Schema 处理
)

// anthropicProvider 基于 Anthropic Go SDK 原生 Messages API 的 provider 封装。
type anthropicProvider struct {
	client      anthropic.Client // Anthropic SDK 客户端
	modelName   string           // 模型名称
	maxTokens   atomic.Int64     // 最大输出 token 数（运行期可被端点上限钳制，需并发安全）
	temperature float64          // 采样温度
	baseURL     string           // .env 配置的完整端点（实际请求 URL）
}

// newAnthropicProvider 构造一个 Anthropic 原生 provider。
//
// 参数：
//   - cfg: 模型配置
//
// 返回：blades.ModelProvider 实例。
func newAnthropicProvider(cfg types.AgentModelConfig) blades.ModelProvider {
	// 若未配置 baseURL，使用 Anthropic 官方默认端点
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	// 解析 MaxTokens，未配置或非法时回退到 4096
	maxTokens := int64(cfg.MaxTokens)
	if maxTokens <= 0 {
		maxTokens = 4096
	}

	// 创建 Anthropic 客户端并封装为 provider
	p := &anthropicProvider{
		client: anthropic.NewClient(
			option.WithAPIKey(cfg.APIKey),
			option.WithBaseURL(baseURL),
		),
		modelName:   cfg.Model,
		temperature: cfg.Temperature,
		baseURL:     baseURL,
	}
	p.maxTokens.Store(maxTokens)
	return p
}

// Name 返回 provider 使用的模型名称。
func (p *anthropicProvider) Name() string { return p.modelName }

// maxTokensLimitRe 匹配端点 max_tokens 超限错误中声明的上限值：
//   - ark/火山: "expected a value <= 32768, but got 65536 instead"
//   - OpenAI 系: "must be less than or equal to 32768"
var maxTokensLimitRe = regexp.MustCompile(`(?:<=|less than or equal to)\s*(\d+)`)

// ClampMaxTokensOnError 检测 max_tokens 超限错误并把 p.maxTokens 钳制到端点声明的上限，
// 返回是否已钳制（调用方据此安全重试一次）。满足 retryProvider 的 maxTokensClamper 接口。
//
// 2026-08-13 实证：ark /api/coding 对 kimi 系模型硬上限 32768，roles.yaml 配 65536 导致
// 代码助手全部调用 400 InvalidParameter、重派全挂；此前只能人手调配置，新用户直接踩死。
// 钳制后 max_tokens 配置退化为软偏好，超限自动对齐端点能力。
func (p *anthropicProvider) ClampMaxTokensOnError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if !strings.Contains(msg, "max_tokens") {
		return false
	}
	m := maxTokensLimitRe.FindStringSubmatch(msg)
	if len(m) < 2 {
		return false
	}
	limit, perr := strconv.ParseInt(m[1], 10, 64)
	if perr != nil || limit <= 0 {
		return false
	}
	old := p.maxTokens.Load()
	if old <= limit {
		return false
	}
	p.maxTokens.Store(limit)
	log.Printf("[model] clamp max_tokens %d -> %d (endpoint limit, model=%s)", old, limit, p.modelName)
	return true
}

// maxContinueRounds 限制 stop_reason=max_tokens 时的自动续写轮数上限。
// 每轮允许 maxTokens 输出；8 轮 = 8*maxTokens 总输出（128K 时即 1M），覆盖任意单次响应需求。
// 超过仍返回截断结果，由上层 ReAct 循环处理（不会死循环）。
const maxContinueRounds = 8

// Generate 调用 Anthropic Messages API 完成生成。
//
// stop_reason=max_tokens 时自动续写：把本轮 assistant 内容（text+tool_use）追加到 messages
// 再发一次，模型从截断处继续。跨轮文本拼接、tool_use 按 ID 去重（续写轮返回完整 input 覆盖
// 前轮部分），thinking 累积。达到 maxContinueRounds 仍 max_tokens 时返回截断结果。
//
// 该机制防 LLM 单次写大文件（如 game.js 1000+行）被 max_tokens 截断 mid-WriteFile JSON
// 导致 tool parser fail -> 空响应 -> ReAct nudge 死循环（实证见 logs/tui/2026-07-28.log）。
func (p *anthropicProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	// 转换 messages
	messages, err := p.convertMessages(req.Messages)
	if err != nil {
		return nil, fmt.Errorf("convert messages: %w", err)
	}
	system := p.convertSystem(req.Instruction)
	tools := p.convertTools(req.Tools)

	var (
		accText    strings.Builder
		toolByID   = map[string]anthropic.ToolUseBlock{}
		toolOrder  []string
		inTok      int64
		outTok     int64
		stopReason string
		// TODO #40 缓存可观测：CacheRead 作 hit、InputTokens+CacheCreation 作 miss（第 0 轮值）。
		cacheHit  int64
		cacheMiss int64
	)

	for round := 0; round < maxContinueRounds; round++ {
		params := anthropic.MessageNewParams{
			Model:       anthropic.Model(p.modelName),
			MaxTokens:   p.maxTokens.Load(),
			Messages:    messages,
			System:      system,
			Tools:       tools,
			Temperature: anthropic.Float(p.temperature),
		}
		resp, err := p.client.Messages.New(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("anthropic messages POST %s: %w", p.baseURL, err)
		}

		// 累积本轮 text 与 tool_use；tool_use 按 ID 去重（续写轮返回完整 input 覆盖前轮部分）。
		for _, block := range resp.Content {
			switch v := block.AsAny().(type) {
			case anthropic.TextBlock:
				accText.WriteString(v.Text)
			case anthropic.ToolUseBlock:
				if _, ok := toolByID[v.ID]; !ok {
					toolOrder = append(toolOrder, v.ID)
				}
				toolByID[v.ID] = v
			}
		}
		// 输入 token 只取第 0 轮（= 原始会话输入大小）；输出 token 累加（每轮新增输出）。
		if round == 0 {
			inTok = resp.Usage.InputTokens + resp.Usage.CacheCreationInputTokens + resp.Usage.CacheReadInputTokens
			cacheHit = int64(resp.Usage.CacheReadInputTokens)
			// 未命中 = 普通 input_tokens（未缓存部分）+ cache_creation（写缓存开销）。
			// 只计 creation 会在端点不报 creation 时 miss 恒 0（glm 中继实测），命中率失真。
			cacheMiss = int64(resp.Usage.InputTokens) + int64(resp.Usage.CacheCreationInputTokens)
		}
		outTok += resp.Usage.OutputTokens
		stopReason = string(resp.StopReason)

		// 非 max_tokens 表示正常结束（end_turn/tool_use/stop_sequence），停止续写。
		if stopReason != "max_tokens" {
			break
		}
		// 最后一轮仍 max_tokens：不再续写，返回截断结果让上层处理。
		if round == maxContinueRounds-1 {
			break
		}

		// 构造本轮 assistant 消息追加到 messages，让下一轮从截断处续写。
		// 必须含本轮全部 text + tool_use 块，模型据此恢复上下文继续输出。
		assistantMsg := anthropic.MessageParam{Role: anthropic.MessageParamRoleAssistant}
		for _, block := range resp.Content {
			switch v := block.AsAny().(type) {
			case anthropic.TextBlock:
				if v.Text != "" {
					assistantMsg.Content = append(assistantMsg.Content, anthropic.ContentBlockParamUnion{
						OfText: &anthropic.TextBlockParam{Text: v.Text},
					})
				}
			case anthropic.ToolUseBlock:
				// 部分截断的 tool_use JSON 也按原样传回，Anthropic 后端会续写补全。
				input := json.RawMessage(v.Input)
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				assistantMsg.Content = append(assistantMsg.Content, anthropic.ContentBlockParamUnion{
					OfToolUse: &anthropic.ToolUseBlockParam{
						ID: v.ID, Name: v.Name, Input: input,
					},
				})
			}
		}
		// 本轮无任何 content 块（如 MaxTokens=1 命中 max_tokens 前未产出任何 token）：
		// 追加空 content 的 assistant 消息会触发 Ark/Anthropic 端点 400 MissingParameter
		// messages[i].content。无内容可续写，直接跳出返回截断结果。
		if len(assistantMsg.Content) == 0 {
			break
		}
		messages = append(messages, assistantMsg)
	}

	msg := blades.NewAssistantMessage(blades.StatusCompleted)
	parts := make([]blades.Part, 0, len(toolOrder)+1)
	if accText.Len() > 0 {
		parts = append(parts, blades.TextPart{Text: accText.String()})
	}
	// stop_reason=max_tokens 且续写轮耗尽：本轮 tool_use input 可能是半截 JSON，
	// 执行会把文件写残（实证 monster.js 被截断到 64 行）。整轮丢弃工具调用，
	// 文本保留，由 ReAct 下一轮让模型重试。正常 tool_use 结束不受影响。
	if stopReason != "max_tokens" {
		for _, id := range toolOrder {
			t := toolByID[id]
			parts = append(parts, blades.NewToolPart(t.ID, t.Name, string(t.Input)))
		}
	}
	msg.Parts = parts
	msg.TokenUsage = blades.TokenUsage{
		InputTokens:  inTok,
		OutputTokens: outTok,
		TotalTokens:  inTok + outTok,
	}
	// TODO #40 缓存可观测：CacheRead 作 hit、InputTokens+CacheCreation 作 miss（第 0 轮值）。
	setCacheUsageMeta(msg, cacheHit, cacheMiss)
	msg.FinishReason = stopReason
	return &blades.ModelResponse{Message: msg}, nil
}

// NewStreaming 创建真正的 SSE 流式生成器：
// 中间产出增量文本块（驱动 UI 逐 token 渲染），最后一个产出值是累积完整的响应。
//
// stop_reason=max_tokens 时自动续写：把本轮 assistant 内容追加到 messages 再开下一轮流，
// 跨轮 text/tool_use/thinking 累积，增量 deltas 继续向 yield 推送。
// 上限 maxContinueRounds 轮，仍 max_tokens 则返回截断结果。
func (p *anthropicProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	return func(yield func(*blades.ModelResponse, error) bool) {
		// 转换 messages
		messages, err := p.convertMessages(req.Messages)
		if err != nil {
			yield(nil, fmt.Errorf("convert messages: %w", err))
			return
		}
		system := p.convertSystem(req.Instruction)
		tools := p.convertTools(req.Tools)

		type toolAcc struct {
			id, name, input string
		}
		var (
			accText     strings.Builder
			accThinking strings.Builder
			// toolByID 跨轮去重：续写轮返回完整 tool_use input 覆盖前轮部分截断。
			toolByID  = map[string]*toolAcc{}
			toolOrder []string
			// 跨轮累积：输出 token 累加（每轮新增输出），输入 token 只取第 0 轮。
			inputTokens, outputTokens int64
			// TODO #40 缓存可观测：跨轮累积 CacheRead（hit）/InputTokens+CacheCreation（miss）。
			cacheHit, cacheMiss int64
			stopReason                string
		)

		for round := 0; round < maxContinueRounds; round++ {
			// 构造与 Generate 一致的请求参数
			params := anthropic.MessageNewParams{
				Model:       anthropic.Model(p.modelName),
				MaxTokens:   p.maxTokens.Load(),
				Messages:    messages,
				System:      system,
				Tools:       tools,
				Temperature: anthropic.Float(p.temperature),
			}

			stream := p.client.Messages.NewStreaming(ctx, params)

			// 本轮局部累积：用于续写时构造 assistant 消息追加到 messages。
			var roundText strings.Builder
			var roundTools []toolAcc

			for stream.Next() {
				event := stream.Current()
				switch ev := event.AsAny().(type) {
				case anthropic.MessageStartEvent:
					// input_tokens 只取第 0 轮（= 原始会话输入大小；后续轮含追加的 assistant 输出会膨胀）。
					// 三字段合计还原真实输入（cache 命中时 input_tokens 恒 0，真实值在 cache_read）。
					if round == 0 {
						inputTokens = ev.Message.Usage.InputTokens +
							ev.Message.Usage.CacheCreationInputTokens +
							ev.Message.Usage.CacheReadInputTokens
					}
				case anthropic.ContentBlockStartEvent:
					// 工具调用块开始：记录 id/name，后续 input_json_delta 累积入参。
					if ev.ContentBlock.Type == "tool_use" {
						roundTools = append(roundTools, toolAcc{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name})
						if _, ok := toolByID[ev.ContentBlock.ID]; !ok {
							toolOrder = append(toolOrder, ev.ContentBlock.ID)
						}
					}
				case anthropic.ContentBlockDeltaEvent:
					switch ev.Delta.Type {
					case "text_delta":
						// 文本增量：累积并产出增量块（消费方停止则终止流）。
						roundText.WriteString(ev.Delta.Text)
						accText.WriteString(ev.Delta.Text)
						msg := blades.NewAssistantMessage(blades.StatusCompleted)
						msg.Parts = []blades.Part{blades.TextPart{Text: ev.Delta.Text}}
						if !yield(&blades.ModelResponse{Message: msg}, nil) {
							stream.Close()
							return
						}
					case "thinking_delta":
						// 思考过程增量：累积并产出携带累积思考文本的中间块（无文本 part），
						// 消费方据此实时展示思考过程；思考内容不进入答复文本。
						accThinking.WriteString(ev.Delta.Thinking)
						msg := blades.NewAssistantMessage(blades.StatusInProgress)
						msg.Metadata = map[string]any{"thinking": accThinking.String()}
						if !yield(&blades.ModelResponse{Message: msg}, nil) {
							stream.Close()
							return
						}
					case "input_json_delta":
						// 工具入参增量：追加到最近开始的工具调用块。
						if len(roundTools) > 0 {
							roundTools[len(roundTools)-1].input += ev.Delta.PartialJSON
						}
					}
				case anthropic.MessageDeltaEvent:
					stopReason = string(ev.Delta.StopReason)
					outputTokens += ev.Usage.OutputTokens
					// 部分网关在 message_start 不填 usage，仅在 message_delta 提供；
					// 且 cache 命中时 input_tokens 恒为 0，真实输入在 cache_read 字段。
					// 第 0 轮累计 cache tokens 补全 inputTokens（已是累积值，直接覆盖）。
					if round == 0 {
						if u := ev.Usage; u.InputTokens+u.CacheCreationInputTokens+u.CacheReadInputTokens > 0 {
							inputTokens = u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
							cacheHit += u.CacheReadInputTokens
							// 未命中 = 普通 input_tokens + cache_creation（口径同非流式路径）。
							cacheMiss += u.InputTokens + u.CacheCreationInputTokens
						}
					}
				}
			}
			if err := stream.Err(); err != nil {
				stream.Close()
				yield(nil, fmt.Errorf("anthropic messages stream POST %s: %w", p.baseURL, err))
				return
			}
			stream.Close()

			// 跨轮累积 tool_use：续写轮返回完整 input 覆盖前轮部分截断。
			for i := range roundTools {
				t := roundTools[i]
				if existing, ok := toolByID[t.id]; ok {
					if t.input != "" {
						existing.input = t.input
					}
				} else {
					tc := t
					toolByID[t.id] = &tc
				}
			}

			// 非 max_tokens 表示正常结束（end_turn/tool_use/stop_sequence），停止续写。
			if stopReason != "max_tokens" {
				break
			}
			// 最后一轮仍 max_tokens：不再续写，进入最终响应构造。
			if round == maxContinueRounds-1 {
				break
			}

			// 构造本轮 assistant 消息追加到 messages，让下一轮从截断处续写。
			// 必须含本轮全部 text + tool_use 块，模型据此恢复上下文继续输出。
			assistantMsg := anthropic.MessageParam{Role: anthropic.MessageParamRoleAssistant}
			if roundText.Len() > 0 {
				assistantMsg.Content = append(assistantMsg.Content, anthropic.ContentBlockParamUnion{
					OfText: &anthropic.TextBlockParam{Text: roundText.String()},
				})
			}
			for _, t := range roundTools {
				// 部分截断的 tool_use JSON 也按原样传回，Anthropic 后端会续写补全。
				input := json.RawMessage(t.input)
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				assistantMsg.Content = append(assistantMsg.Content, anthropic.ContentBlockParamUnion{
					OfToolUse: &anthropic.ToolUseBlockParam{
						ID: t.id, Name: t.name, Input: input,
					},
				})
			}
			// 本轮无任何 content 块（如 MaxTokens=1 命中 max_tokens 前未产出任何 token）：
			// 追加空 content 的 assistant 消息会触发 Ark/Anthropic 端点 400 MissingParameter
			// messages[i].content。无内容可续写，直接跳出进入最终响应构造。
			if len(assistantMsg.Content) == 0 {
				break
			}
			messages = append(messages, assistantMsg)
		}

		// 产出累积完整的最终响应（文本 + 工具调用 + 用量 + 结束原因）。
		msg := blades.NewAssistantMessage(blades.StatusCompleted)
		parts := make([]blades.Part, 0, len(toolOrder)+1)
		if accText.Len() > 0 {
			parts = append(parts, blades.TextPart{Text: accText.String()})
		}
		// 同 Generate：续写轮耗尽仍 max_tokens 时丢弃 tool_use（半截 JSON 会写残文件）。
		if stopReason != "max_tokens" {
			for _, id := range toolOrder {
				t := toolByID[id]
				parts = append(parts, blades.NewToolPart(t.id, t.name, t.input))
			}
		}
		msg.Parts = parts
		msg.TokenUsage = blades.TokenUsage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			TotalTokens:  inputTokens + outputTokens,
		}
		msg.FinishReason = stopReason
		// 思考过程经 Metadata 传递（blades 无对应 Part 类型），截断防止超长。
		if accThinking.Len() > 0 {
			msg.Metadata = map[string]any{"thinking": truncateThinking(accThinking.String())}
		}
		// TODO #40 缓存可观测：CacheRead 作 hit、InputTokens+CacheCreation 作 miss。
		setCacheUsageMeta(msg, cacheHit, cacheMiss)
		yield(&blades.ModelResponse{Message: msg}, nil)
	}
}

// thinkingMaxRunes 限制单次响应携带的思考过程文本长度，防止超长思考挤占展示与存储。
const thinkingMaxRunes = 4000

// truncateThinking 按 rune 数截断思考过程文本并追加省略提示。
func truncateThinking(s string) string {
	runes := []rune(s)
	if len(runes) <= thinkingMaxRunes {
		return s
	}
	return string(runes[:thinkingMaxRunes]) + "...(truncated)"
}

// convertSystem 将 blades 的 Instruction 消息转换为 Anthropic system blocks。
//
// 参数：
//   - inst: blades system 消息
//
// 返回：Anthropic system text blocks 切片。
func (p *anthropicProvider) convertSystem(inst *blades.Message) []anthropic.TextBlockParam {
	// 空 system 消息直接返回 nil
	if inst == nil {
		return nil
	}
	// 收集所有文本 part
	var blocks []anthropic.TextBlockParam
	for _, part := range inst.Parts {
		// 仅处理文本 part
		text, ok := part.(blades.TextPart)
		if !ok {
			continue
		}
		blocks = append(blocks, anthropic.TextBlockParam{Text: text.Text})
	}
	// 给最后一个 system block 打 cache_control ephemeral 断点：system prompt
	// 已按实例冻结（react_agent.go sysPromptOnce），是请求中最大最稳定的前缀，
	// 此前完全依赖中继隐式缓存，实测命中率仅 24%；显式断点让 Anthropic 协议
	// 把 system 前缀纳入 prompt cache，后续轮次命中 cache_read。
	if len(blocks) > 0 {
		blocks[len(blocks)-1].CacheControl = anthropic.NewCacheControlEphemeralParam()
	}
	return blocks
}

// convertMessages 将 blades 消息列表转换为 Anthropic 消息参数列表。
//
// 参数：
//   - messages: blades 消息切片
//
// 返回：
//   - []anthropic.MessageParam: Anthropic 消息参数
//   - error: 转换错误
func (p *anthropicProvider) convertMessages(messages []*blades.Message) ([]anthropic.MessageParam, error) {
	// 预分配等长切片
	out := make([]anthropic.MessageParam, 0, len(messages))
	// 逐条转换
	for i := 0; i < len(messages); i++ {
		// 跳过 nil 消息
		if messages[i] == nil {
			continue
		}
		// Anthropic 协议要求：若上一条 assistant 含 N 个 tool_use，下一条 user 消息
		// 必须包含全部 N 个 tool_result。我们的 ReactMessage 把每个 tool 结果拆成
		// 独立 RoleTool 消息；不合并会导致 Ark/Anthropic 端点 400 InvalidParameter。
		// 这里把连续的 RoleTool 消息合并为单条 user 消息，content 含全部 ToolResultBlock。
		if messages[i].Role == blades.RoleTool {
			merged := anthropic.MessageParam{Role: anthropic.MessageParamRoleUser}
			for i < len(messages) && messages[i] != nil && messages[i].Role == blades.RoleTool {
				// 每条 RoleTool 消息 = 一个 ToolPart（文本结果）+ 可选 DataPart（图片，
				// 仅 image_passthrough 插件的最新一批，见 agent.ToBladesMessages）。
				// 图片挂在同一 tool_result 的 content 里（anthropic 视觉协议）。
				var images []anthropic.ToolResultBlockParamContentUnion
				for _, part := range messages[i].Parts {
					if dp, ok := part.(blades.DataPart); ok {
						if img := toolResultImageBlock(dp); img != nil {
							images = append(images, *img)
						}
					}
				}
				for _, part := range messages[i].Parts {
					tp, ok := part.(blades.ToolPart)
					if !ok {
						continue
					}
					if tp.Response == "" {
						continue
					}
					content := make([]anthropic.ToolResultBlockParamContentUnion, 0, 1+len(images))
					content = append(content, anthropic.ToolResultBlockParamContentUnion{
						OfText: &anthropic.TextBlockParam{Text: tp.Response},
					})
					content = append(content, images...)
					merged.Content = append(merged.Content, anthropic.ContentBlockParamUnion{
						OfToolResult: &anthropic.ToolResultBlockParam{
							ToolUseID: tp.ID,
							Content:   content,
						},
					})
				}
				i++
			}
			// 回退一位，外层 for 会再 +1，确保下一条非 RoleTool 消息正常处理。
			i--
			if len(merged.Content) > 0 {
				out = append(out, merged)
			}
			continue
		}
		param, err := p.convertMessage(messages[i])
		if err != nil {
			return nil, err
		}
		out = append(out, param)
	}
	return out, nil
}

// imageMediaType 校验并归一图片 MIME 类型，白名单外返回空串。
// Anthropic 图片协议仅接受 jpeg/png/gif/webp 的 base64 来源。
func imageMediaType(mime blades.MIMEType) anthropic.Base64ImageSourceMediaType {
	switch anthropic.Base64ImageSourceMediaType(strings.ToLower(string(mime))) {
	case anthropic.Base64ImageSourceMediaTypeImagePNG:
		return anthropic.Base64ImageSourceMediaTypeImagePNG
	case anthropic.Base64ImageSourceMediaTypeImageJPEG:
		return anthropic.Base64ImageSourceMediaTypeImageJPEG
	case anthropic.Base64ImageSourceMediaTypeImageGIF:
		return anthropic.Base64ImageSourceMediaTypeImageGIF
	case anthropic.Base64ImageSourceMediaTypeImageWebP:
		return anthropic.Base64ImageSourceMediaTypeImageWebP
	default:
		return ""
	}
}

// imageDataBlock 把 blades.DataPart 转为 anthropic 顶层图片块（user 消息多模态）。
// 不支持/无法转换时返回 nil（静默跳过，文本占位符仍保留其存在痕迹）。
func imageDataBlock(dp blades.DataPart) *anthropic.ImageBlockParam {
	if len(dp.Bytes) == 0 {
		return nil
	}
	mediaType := imageMediaType(dp.MIMEType)
	if mediaType == "" {
		return nil
	}
	return &anthropic.ImageBlockParam{
		Source: anthropic.ImageBlockParamSourceUnion{
			OfBase64: &anthropic.Base64ImageSourceParam{
				Data:      base64.StdEncoding.EncodeToString(dp.Bytes),
				MediaType: mediaType,
			},
		},
	}
}

// toolResultImageBlock 把 blades.DataPart 转为 anthropic tool_result content 的图片块。
// 不支持/无法转换时返回 nil（静默跳过，Response 文本占位符仍保留其存在痕迹）。
func toolResultImageBlock(dp blades.DataPart) *anthropic.ToolResultBlockParamContentUnion {
	ib := imageDataBlock(dp)
	if ib == nil {
		return nil
	}
	return &anthropic.ToolResultBlockParamContentUnion{OfImage: ib}
}

// convertMessage 将单条 blades.Message 转换为 Anthropic MessageParam。
//
// 参数：
//   - m: blades 消息
//
// 返回：
//   - anthropic.MessageParam: Anthropic 消息参数
//   - error: 转换错误（当前不会返回错误，保留签名以兼容未来扩展）
func (p *anthropicProvider) convertMessage(m *blades.Message) (anthropic.MessageParam, error) {
	// 根据 blades 角色映射到 Anthropic 角色
	var role anthropic.MessageParamRole
	switch m.Role {
	case blades.RoleUser, blades.RoleTool:
		role = anthropic.MessageParamRoleUser
	case blades.RoleAssistant:
		role = anthropic.MessageParamRoleAssistant
	case blades.RoleSystem:
		role = anthropic.MessageParamRoleUser
	default:
		role = anthropic.MessageParamRoleUser
	}

	// 预分配 content blocks
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(m.Parts))
	// 逐个 part 转换
	for _, part := range m.Parts {
		switch v := part.(type) {
		case blades.TextPart:
			// 文本块直接追加
			blocks = append(blocks, anthropic.ContentBlockParamUnion{
				OfText: &anthropic.TextBlockParam{Text: v.Text},
			})
		case blades.ToolPart:
			if v.Response != "" {
				// 工具结果块，必须以 user message 形式返回给模型
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolResult: &anthropic.ToolResultBlockParam{
						ToolUseID: v.ID,
						Content: []anthropic.ToolResultBlockParamContentUnion{
							{OfText: &anthropic.TextBlockParam{Text: v.Response}},
						},
					},
				})
			} else if m.Role == blades.RoleAssistant {
				// assistant 发起的 tool_use 调用
				input := json.RawMessage(v.Request)
				// 若请求为空，使用空对象占位
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolUse: &anthropic.ToolUseBlockParam{
						ID:    v.ID,
						Name:  v.Name,
						Input: input,
					},
				})
			}
		default:
			// 忽略不支持的 part 类型
		}
	}

	// user 消息携带的图片 DataPart（Alt+V 粘贴 / 工具透传）转顶层图片块；
	// tool 角色的图片由上方 ToolPart 分支经 toolResultImageBlock 处理。
	if m.Role == blades.RoleUser {
		for _, part := range m.Parts {
			if dp, ok := part.(blades.DataPart); ok {
				if ib := imageDataBlock(dp); ib != nil {
					blocks = append(blocks, anthropic.ContentBlockParamUnion{OfImage: ib})
				}
			}
		}
	}

	return anthropic.MessageParam{Role: role, Content: blocks}, nil
}

// convertTools 将 blades 工具列表转换为 Anthropic 工具参数列表。
//
// 参数：
//   - tools: blades 工具切片
//
// 返回：Anthropic 工具参数切片。
func (p *anthropicProvider) convertTools(tools []bladestools.Tool) []anthropic.ToolUnionParam {
	// 无工具时返回 nil，避免序列化为 "tools": [] 触发端点 InvalidParameter
	if len(tools) == 0 {
		return nil
	}
	// 预分配等长切片
	out := make([]anthropic.ToolUnionParam, 0, len(tools))
	// 逐个工具转换
	for _, t := range tools {
		// 将 JSON Schema 转为 map
		schemaMap, _ := schemaToMap(t.InputSchema())
		// ToolInputSchemaParam.Properties 只接受内部 properties 映射，
		// 传入整个 schema 会导致 input_schema 结构错乱（嵌套一层 type/properties/required），
		// 触发 Ark 端点 400 InvalidParameter。
		var properties any
		if schemaMap != nil {
			if props, ok := schemaMap["properties"]; ok {
				properties = props
			} else {
				properties = map[string]any{}
			}
		} else {
			properties = map[string]any{}
		}
		out = append(out, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        t.Name(),
				Description: anthropic.String(t.Description()),
				InputSchema: anthropic.ToolInputSchemaParam{
					Properties: properties,
					Required:   t.InputSchema().Required,
				},
			},
		})
	}
	// 给最后一个工具打 cache_control ephemeral 断点：tools 列表按实例稳定，
	// 与 system 同为请求头部的大段稳定前缀；断点让 tools+system 整体可被缓存，
	// 避免每轮 32K tokens 全量 cache_miss（此前依赖中继隐式缓存命中率仅 24%）。
	// len(tools) == 0 时上方已返回 nil，此处必然非空。
	out[len(out)-1].OfTool.CacheControl = anthropic.NewCacheControlEphemeralParam()
	return out
}

// convertResponse 已移除：Generate 改为带续写的累积逻辑，单轮响应转换被内联。
// 如需单次转换可参考 Generate 中 round 循环内的 text/tool_use 累积分支。

// schemaToMap 将 jsonschema.Schema 序列化为 map[string]any。
//
// 参数：
//   - s: JSON Schema 指针
//
// 返回：
//   - map[string]any: 转换后的 map
//   - error: 序列化/反序列化错误
func schemaToMap(s *jsonschema.Schema) (map[string]any, error) {
	// nil schema 直接返回 nil
	if s == nil {
		return nil, nil
	}
	// 序列化为 JSON
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	// 反序列化为 map
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	// jsonschema-go 在序列化时去掉了 Type/Types 字段（json:"-"），
	// 手动补回顶层 type，方便 Anthropic SDK 生成合法的 input_schema。
	if s.Type != "" {
		m["type"] = s.Type
	} else if len(s.Types) > 0 {
		m["type"] = s.Types[0]
	}
	return m, nil
}
