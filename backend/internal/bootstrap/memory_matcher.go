package bootstrap

// memory_matcher.go —— 长期记忆语义匹配器（可选，2026-10-24 长期记忆撤回与污染修复）。
//
// 确定性规则（ID/精确文本/同主题键）能覆盖绝大多数撤回；剩下"用户换了措辞、指代含糊"
// 的情况交给轻量模型只做**判断**：
//   - 模型只能输出结构化操作（retract / quarantine / supersede），不能输出 SQL、
//     Markdown 整段或新文件状态；
//   - 执行前由 agent.MemoryLifecycle 逐条校验条目 ID 是否存在、scope/key 是否匹配；
//   - 不确定的目标一律建议 uncertain → 走"先隔离"（立即停止注入），不做永久撤回。
// 模型失败/超时 → 返回错误，调用方留 reconcile 提示，不假报成功。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/model"
)

// memoryMatcherPromptVersion 匹配提示词版本（语义改动时手动 bump，便于日志归因）。
const memoryMatcherPromptVersion = "20261024-1"

// newMemoryMatcher 构造语义匹配器（注入给 agent.MemoryLifecycle.SetMatcher）。
func newMemoryMatcher(f *model.ModelFactory) agent.MemoryMatcherFunc {
	if f == nil {
		return nil
	}
	return func(ctx context.Context, req agent.MemoryMatchRequest) (*agent.MemoryMatchResult, error) {
		prompt := buildMemoryMatcherPrompt(req)
		out, err := f.CallLightweightWithRetry(ctx, prompt)
		if err != nil {
			return nil, err
		}
		return parseMemoryMatcherOutput(out)
	}
}

// buildMemoryMatcherPrompt 构造匹配提示词（只读判断，禁写）。
func buildMemoryMatcherPrompt(req agent.MemoryMatchRequest) string {
	var b strings.Builder
	b.WriteString("你是长期记忆撤回判定器。用户刚发来一句话，需要判断它是否在**撤回/替换**已有的长期记忆条目。\n")
	b.WriteString("规则：\n")
	b.WriteString("1. 只判断与长期记忆有关的内容；一次性任务选择（\"这次\"\"当前这个角色\"）与产物修改反馈（\"幅度太小\"）不算。\n")
	b.WriteString("2. 只有在你能确定对应哪一条时才给 retract/supersede；不确定就给 uncertain（该条目会先被隔离，稍后由用户确认）。\n")
	b.WriteString("3. supersede 必须给 new_content（用户给出的新表述）且 memory_key 必须与旧条目一致。\n")
	b.WriteString("4. 禁止编造条目 ID；只能从候选里选。禁止输出操作以外的文本。\n")
	b.WriteString("5. 只输出 JSON，形如：{\"ops\":[{\"action\":\"retract\",\"item_id\":\"M-123\",\"reason\":\"用户明确不要该做法\"}],\"uncertain\":[],\"note\":\"\"}\n\n")
	fmt.Fprintf(&b, "用户原话：%s\n", strings.TrimSpace(req.Text))
	if req.ScopeHint != "" {
		fmt.Fprintf(&b, "作用域提示：%s\n", req.ScopeHint)
	}
	b.WriteString("\n候选长期记忆条目：\n")
	if len(req.Candidates) == 0 {
		b.WriteString("（无）\n")
	}
	for _, c := range req.Candidates {
		fmt.Fprintf(&b, "- id=%s scope=%s key=%s content=%s\n", c.ID, c.Scope, c.MemoryKey, c.Content)
	}
	b.WriteString("\nJSON：")
	return b.String()
}

// parseMemoryMatcherOutput 解析模型输出（容忍 ```json 代码围栏与前后噪声）。
func parseMemoryMatcherOutput(raw string) (*agent.MemoryMatchResult, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "{"); i >= 0 {
		s = s[i:]
	}
	if j := strings.LastIndex(s, "}"); j >= 0 {
		s = s[:j+1]
	}
	if s == "" {
		return nil, fmt.Errorf("empty matcher output")
	}
	var res agent.MemoryMatchResult
	if err := json.Unmarshal([]byte(s), &res); err != nil {
		return nil, fmt.Errorf("parse matcher output: %w", err)
	}
	return &res, nil
}
