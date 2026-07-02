package graph

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)


// loadHistorySection 读取最近 N 条会话历史，拼成可注入 prompt 的中文段落。
//
// 职责：
//   - 从 HistoryStore 读取最近 5 条历史
//   - 拼成"已知历史"段落，含目标/结果/工具调用路径
//   - 末尾提示 LLM 在用户提到指代词时优先结合历史作答
//
// 参数：
//   - ctx：请求上下文
//
// 返回：拼好的段落；失败/无数据返回空串，不影响主流程。
//
// 副作用：2s 超时读取历史，避免历史查询拖垮主流程。
func (n *MetaAgentNode) loadHistorySection(ctx context.Context) string {
	// 未注入历史读取器则返回空
	if n.history == nil {
		return ""
	}
	// 2s 超时读取，避免历史查询阻塞主流程
	hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	entries, err := n.history.RecentSessionHistories(hctx, 5)
	if err != nil || len(entries) == 0 {
		return ""
	}
	// 拼接段落
	var b strings.Builder
	b.WriteString("\n已知历史（最近会话，倒序）:\n")
	for i, e := range entries {
		// 每条历史：序号 + 会话ID + 目标 + 结果摘要（截断 300 字）
		b.WriteString(fmt.Sprintf("%d. [%s] 目标: %s\n   结果: %s\n", i+1, e.SessionID, e.Goal, truncateStr(e.Summary, 300)))
		// 列出该会话中有意义的工具调用（特别是 WriteFile/RunCommand 的 path）
		for _, tr := range e.ToolResults {
			tool, _ := tr["tool"].(string) // 取工具名
			path, _ := tr["path"].(string) // 取路径
			// 只展示有 path 的工具调用
			if path == "" {
				continue
			}
			// 输出 "工具 -> 路径" 行
			b.WriteString(fmt.Sprintf("   - %s -> %s\n", tool, path))
		}
	}
	// 末尾提示：指代类问题优先结合历史
	b.WriteString("\n当用户提到指代词（在哪/刚才/上次/那个文件）时，请优先结合上述历史作答或检索。\n")
	return b.String()
}
// truncateStr 把字符串截断到 n 个 rune 并加 "..." 后缀。
// 用于摘要展示，避免过长的 LLM 输出污染 prompt。
// 按 rune 截断而非字节，避免在 UTF-8 多字节字符（如中文，每字 3 字节）中间切断产生无效 UTF-8（H3）。
func truncateStr(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n]) + "..."
}
// loadMessagesSection 从 state.Messages 构建对话历史段落，注入 prompt。
//
// 职责：把当前会话的对话消息拼成"对话历史"段落，每条消息截断 300 字。
//
// 参数：
//   - state：图全局状态（取 Messages）
//
// 返回：拼好的段落；无消息返回空串。
func (n *MetaAgentNode) loadMessagesSection(state *types.ThreeLayerState) string {
	// 无消息则返回空
	if len(state.Messages) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n对话历史:\n")
	for _, msg := range state.Messages {
		// 每条消息：角色 + 内容（截断 300 字）
		b.WriteString(fmt.Sprintf("%s: %s\n", msg.Role, truncateStr(msg.Content, 300)))
	}
	b.WriteString("\n")
	return b.String()
}

// metaDirectRoleDef 构造 MetaAgent 直接执行工具时使用的角色定义。
//
// ID="meta" 让 ModelFactory.GetModel 命中 MetaAgent 模型配置；SystemPrompt 取 soul 人格
// （无 soul 则用默认开发助手人格）。executeWithTools 会在此基础上追加环境段与硬性规则。
func (n *MetaAgentNode) metaDirectRoleDef() *types.RoleDefinition {
	basePersona := "你是 BlockMemoryAgent，一个基于大语言模型的本地 AI 开发助手。" +
		"你可以帮助用户：分析代码、操作文件、执行命令、搜索代码、编写程序等。"
	systemPrompt := basePersona
	// 注入 soul 人格（无 soul 时 Inject 返回原串）
	if n.rt != nil && n.rt.Soul != nil {
		systemPrompt = n.rt.Soul.Inject(basePersona)
	}
	return &types.RoleDefinition{
		ID:           "meta",
		Name:         "MetaAgent",
		SystemPrompt: systemPrompt,
		Description:  "主 Agent 直接执行",
		Type:         types.RoleTypeMeta,
	}
}

// executeDirect MetaAgent 直接跑工具循环执行任务（RouteDirectTool / RouteDirectAssistant 共用）。
//
// 职责：用给定 roleDef 调 CommonExecuteAssistantTask 跑 blades ReAct 工具循环，
//   把结果写入 SessionSummary 并置 ActionFinish，不创建 DomainAgent/SubDomainAgent。
//
// 参数：
//   - ctx：请求上下文。
//   - state：图全局状态（写入 SessionSummary / NextAction / Reason）。
//   - roleDef：执行用的角色定义（RouteDirectTool 用 meta 角色；RouteDirectAssistant 用动态助手角色）。
//   - task：任务文本（通常为 state.DomainGoal）。
//
// 返回：更新后的 state。
func (n *MetaAgentNode) executeDirect(ctx context.Context, state *types.ThreeLayerState, roleDef *types.RoleDefinition, task string) (*types.ThreeLayerState, error) {
	if n.modelFactory == nil {
		// 无模型：写占位回答并结束
		state.SessionSummary = "模型不可用，无法直接执行。"
		state.NextAction = enums.ActionFinish
		state.Reason = "no model factory for direct execute"
		return state, nil
	}
	// 走公共执行入口：MetaAgent 不启用写文件门控（直接执行不强制重试）
	result, err := CommonExecuteAssistantTask(ctx, n.modelFactory, n.toolCallback, n.rt,
		roleDef, task, state, "", n.progress, "MetaAgent["+roleDef.Name+"]", 0, false)
	if err != nil || result == "" {
		// 直接执行失败：回退到 RouteCreateDomain 走领域拆分（保底）
		n.emit(ctx, "error", fmt.Sprintf("直接执行失败，回退到领域拆分: %v", err))
		return n.handleInitialCreateDomains(ctx, state, false)
	}
	state.SessionSummary = result
	state.NextAction = enums.ActionFinish
	state.Reason = "direct execute by MetaAgent"
	return state, nil
}

// executeDirectAssistant RouteDirectAssistant 路径：MetaAgent 建一个助手角色后直接跑工具循环。
//
// 不经过 DomainAgent 任务拆分，直接用一个专家助手执行单领域简单任务。
func (n *MetaAgentNode) executeDirectAssistant(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	task := state.DomainGoal
	// 用 factory.CreateAssistant 动态创建一个助手角色定义（无父领域）
	inst, err := n.factory.CreateAssistant(ctx, state.SessionID, task, "", "meta")
	if err != nil || inst == nil {
		// 创建失败：回退到 RouteCreateDomain
		n.emit(ctx, "error", fmt.Sprintf("直接助手创建失败，回退领域拆分: %v", err))
		return n.handleInitialCreateDomains(ctx, state, false)
	}
	def := n.registry.GetRoleDef(inst.RoleDefID)
	if def == nil {
		return n.handleInitialCreateDomains(ctx, state, false)
	}
	n.emitDetail(ctx, "agent_created", fmt.Sprintf("直接创建 Assistant: %s (任务: %s)", inst.ID, task),
		fmt.Sprintf("instID=%s roleDefID=%s", inst.ID, inst.RoleDefID))
	return n.executeDirect(ctx, state, def, task)
}