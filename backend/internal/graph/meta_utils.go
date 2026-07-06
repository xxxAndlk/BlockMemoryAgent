package graph

import (
	"context"
	"fmt"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
	"strings"
	"time"
	"unicode/utf8"
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
		// P0-1：注入历史会话的 MetaAgent 调度记忆（截断 200 字/条）
		if len(e.MetaMemory) > 0 {
			b.WriteString("   调度记忆:\n")
			for _, mem := range e.MetaMemory {
				content, _ := mem["content"].(string)
				if content == "" {
					continue
				}
				b.WriteString(fmt.Sprintf("     • %s\n", truncateStr(content, 200)))
			}
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

// executeDirectTool RouteDirectTool 路径：MetaAgent 直接跑工具循环，不创建任何 Agent 节点。
//
// 职责：使用 MetaAgent 自身的 "meta" 角色定义，直接调用 CommonExecuteAssistantTask 跑 blades ReAct 工具循环，
// 把结果写入 SessionSummary 并置 ActionFinish。若找不到 meta 角色定义则回退到 RouteDirectAssistant。
func (n *MetaAgentNode) executeDirectTool(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	task := state.DomainGoal
	// 文件/路径存在性问题强制工具验证：
	// 塔防 demo 事故中，用户问"index.html 在哪"，LLM 走 direct_tool 路径直接生成
	// 文本回答，从未真正 ListDir/ReadFile，导致编造"位于项目根目录"。
	// 此处检测文件名/路径模式，命中则在 task 前拼接强制工具验证指令。
	if needsFileVerification(task) {
		task = "[强制工具验证] 此问题涉及文件/路径存在性。回答前必须先调用 ListDir 列出工作目录与可疑子目录,"
		task += "再对候选路径调用 ReadFile 验证存在性。"
		task += "若工具未返回该文件，必须明确回复『未找到』，禁止凭推断/记忆编造路径。"
		task += "\n\n原始问题: " + state.DomainGoal
		n.emit(ctx, "intend", "检测到文件存在性问题，强制工具验证: "+state.DomainGoal)
	}
	n.emit(ctx, "intend", "直接执行工具: "+task)

	// 获取 MetaAgent 自身角色定义
	def := n.registry.GetMetaRoleDef()
	if def == nil {
		n.emit(ctx, "error", "未找到 meta 角色定义，回退到直接助手")
		return n.executeDirectAssistant(ctx, state)
	}
	return n.executeDirect(ctx, state, def, task)
}

// needsFileVerification 判断目标是否涉及文件/路径存在性问题。
// 命中模式：含"在哪/哪里/在哪找/在哪看/路径/位置" + 文件名特征（含 . 后缀 / 含 / 或 \）。
// 用于阻止 LLM 在 direct_tool 路径上凭记忆编造文件路径。
func needsFileVerification(goal string) bool {
	if goal == "" {
		return false
	}
	gl := strings.ToLower(goal)
	// 存在性/定位类关键词
	locPatterns := []string{"在哪", "哪里", "在哪找", "在哪看", "位置", "路径", "存在", "找不到", "没找到", "存不存在", "absolute", "where is", "locate"}
	hitLoc := false
	for _, p := range locPatterns {
		if strings.Contains(gl, p) {
			hitLoc = true
			break
		}
	}
	if !hitLoc {
		return false
	}
	// 文件名特征：含 . 后缀（.html/.js/.go 等）或路径分隔符
	if strings.ContainsAny(gl, "/\\") {
		return true
	}
	// 检测 .ext 模式（2-5 字母后缀）
	dotIdx := strings.IndexByte(gl, '.')
	if dotIdx >= 0 && dotIdx < len(gl)-1 {
		rest := gl[dotIdx+1:]
		cnt := 0
		for _, r := range rest {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				cnt++
			} else {
				break
			}
		}
		if cnt >= 2 && cnt <= 5 {
			return true
		}
	}
	return false
}

// executeDirect MetaAgent 直接执行任务（RouteDirectTool / RouteDirectAssistant 公共执行入口）。
//
// 职责：用给定 roleDef 调 CommonExecuteAssistantTask 跑 blades ReAct 工具循环，
//
//	把结果写入 SessionSummary 并置 ActionFinish，不创建 DomainAgent/SubDomainAgent。
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
		roleDef, task, state, "", n.progress, "MetaAgent["+roleDef.Name+"]", 0, false, n.llmTracker)
	if err != nil || result == nil || (result.SummaryForUser == "" && result.Error != "") {
		// 直接执行失败：回退到 RouteCreateDomain 走领域拆分（保底）
		n.emit(ctx, "error", fmt.Sprintf("直接执行失败，回退到领域拆分: %v", err))
		return n.handleInitialCreateDomains(ctx, state, false)
	}
	// 把执行结果作为本会话结果；若 SummaryForUser 为空则用 MemoryForMeta 兜底
	if result.SummaryForUser == "" && result.MemoryForMeta != "" {
		result.SummaryForUser = result.MemoryForMeta
	}
	state.SessionSummary = result.SummaryForUser
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
