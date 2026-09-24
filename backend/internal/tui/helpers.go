package tui

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// ansiRegex 匹配 ANSI 转义序列（CSI/OSC 等）。T10 修复：
// tool_output 含 ANSI（如 colored log）会让 bubbletea 渲染错位。
var ansiRegex = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b[@-Z\\-_]")

// stripANSI 移除字符串中的 ANSI 转义序列，避免渲染错位。
func stripANSI(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}

// sanitizeToolText 净化工具输出/错误文本：剥离 ANSI，并把 CRLF 与孤立 \r 归一为 \n。
// 工具输出夹带 \r（CRLF 文件内容、命令进度条）时，终端会把行内 \r 当回车、
// 后续文本覆盖本行造成重叠乱码；归一后按普通换行渲染。
func sanitizeToolText(s string) string {
	s = stripANSI(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// flashMsg 设置一条 2 秒后过期的闪屏提示。
// 经 sharedState 指针共享写入（#47 修复）：后台 goroutine 调用时 Model 可能已被
// tick 拷贝多轮，直接写字段会落到废弃副本上导致提示丢失。
func (m *Model) flashMsg(msg string) {
	m.ensureShared().setFlash(msg)
}

// hasPlan 判断当前是否可展示计划弹窗；只要有激活会话即认为有计划（包括 direct_tool 的合成计划）。
func (m *Model) hasPlan() bool {
	// 只要有激活会话就认为有计划可展示（包括 direct_tool 的合成计划），
	// 避免计划弹窗在大多数会话里被禁用。
	return m.selectedSession() != nil
}

// showChatDetail 打开当前选中对话条目的详情弹窗，展示完整内容。
func (m *Model) showChatDetail() {
	s := m.selectedSession()
	if s == nil {
		return
	}
	items := chatItemsResolved(s, true, m.subAgentNameResolver())
	idx := m.chatPanel.currentItem()
	if idx < 0 || idx >= len(items) {
		return
	}
	item := items[idx]
	// 弹窗展示完整详情（rawDetail），不截断工具输出
	detail := item.rawDetail
	if detail == "" {
		detail = item.detail
	}
	m.overlayPanel.open(item.title, strings.Split(detail, "\n"))
}

// currentRoundStart 返回会话当前轮任务的开始时间：最后一条用户消息的时间；
// 无用户消息或消息时间缺失时回退到会话开始时间。多轮会话中用它界定"当前轮"，
// 编排面板与计划面板据此过滤上一轮残留的终态 Agent 节点。
func currentRoundStart(s *server.Session) time.Time {
	if s == nil {
		return time.Time{}
	}
	for i := len(s.Messages) - 1; i >= 0; i-- {
		if s.Messages[i].Role == enums.ChatRoleUser {
			if !s.Messages[i].Timestamp.IsZero() {
				return s.Messages[i].Timestamp
			}
			break
		}
	}
	return s.StartedAt
}

// boardSnapshot 返回指定会话的看板快照（TODO #22 Phase 2）：
// 优先读真实执行计划（agent.Board，write_plan 写入的 board.TaskBoard 权威快照）；
// 无计划（未 write_plan / 旧会话）回退旧路径——从权威 Agent 树合成：
// 每个树节点映射为一条任务，状态直接取节点的实时状态；树为空或查询失败时返回空快照。
// 只统计当前轮任务：上一轮已终结的节点被过滤，不再稀释进度条与任务列表。
func (m *Model) boardSnapshot(s *server.Session) board.Snapshot {
	if m.agent == nil || s == nil || s.ID == "" {
		return board.Snapshot{}
	}
	// 真实计划优先：board.Snapshot 是 write_plan 的权威真相源（含依赖/验收/领域）。
	if snap, err := m.agent.Board(context.Background(), s.ID); err == nil && snap != nil && len(snap.Tasks) > 0 {
		return *snap
	}
	nodes, err := m.agent.Tree(context.Background(), s.ID)
	if err != nil || len(nodes) == 0 {
		return board.Snapshot{}
	}
	nodes = filterPrevRoundNodes(nodes, currentRoundStart(s))
	tasks := make([]board.SubTask, 0, len(nodes))
	for i, n := range nodes {
		st := board.TaskInProgress
		switch n.Status {
		case orchestrator.StatusDone, orchestrator.StatusCancelled, orchestrator.StatusIdle:
			// Idle=任务完结热驻留态（Tree.Idle），合成计划任务应翻绿；Wake 复用会翻回 Running。
			st = board.TaskDone
		case orchestrator.StatusFailed:
			st = board.TaskFailed
		case orchestrator.StatusUnverified:
			// 已交付未验证：看板标黄不标红。
			st = board.TaskUnverified
		case orchestrator.StatusPaused:
			// 触达 token 上限暂停（待用户"继续"）：展示为 Blocked 而非 Running。
			st = board.TaskBlocked
		}
		title := "派发 " + agentNodeName(n)
		if n.Task != "" {
			title += ": " + n.Task
		}
		// 完成/失败的任务用 Finished 作为结束时间，使时长统计正确；缺失时回退 Started。
		updated := n.Started
		if !n.Finished.IsZero() {
			updated = n.Finished
		}
		tasks = append(tasks, board.SubTask{
			ID:        fmt.Sprintf("sub-%d", i+1),
			Title:     title,
			Status:    st,
			CreatedAt: n.Started,
			UpdatedAt: updated,
		})
	}
	return board.Snapshot{Tasks: tasks}
}

// showPlanDetailByIndex 打开指定索引计划任务的详情弹窗。
func (m *Model) showPlanDetailByIndex(idx int) {
	s := m.selectedSession()
	if s == nil {
		return
	}
	snap := m.boardSnapshot(s)
	if len(snap.Tasks) == 0 {
		return
	}
	if idx < 0 || idx >= len(snap.Tasks) {
		return
	}
	t := snap.Tasks[idx]
	lines := []string{
		fmt.Sprintf("ID: %s", t.ID),
		fmt.Sprintf("Title: %s", t.Title),
		fmt.Sprintf("Status: %s", t.Status),
		fmt.Sprintf("Assignee: %s", t.Assignee),
	}
	if t.Result != "" {
		lines = append(lines, fmt.Sprintf("Result: %s", t.Result))
	}
	m.overlayPanel.open("Plan Task", lines)
}

// showAgentDetailByIndex 打开指定索引 Agent 节点的详情弹窗。
func (m *Model) showAgentDetailByIndex(idx int) {
	if idx < 0 || idx >= len(m.agentTreePanel.nodes) {
		return
	}
	node := m.agentTreePanel.nodes[idx]
	lines := []string{
		fmt.Sprintf("Instance: %s", node.instID),
		fmt.Sprintf("Name: %s", node.name),
		fmt.Sprintf("Type: %s", node.roleType),
		fmt.Sprintf("Status: %s", node.status),
	}
	if node.domain != "" {
		lines = append(lines, fmt.Sprintf("Domain: %s", node.domain))
	}
	if node.goal != "" {
		lines = append(lines, fmt.Sprintf("Goal: %s", node.goal))
	}
	// 终态节点的完整结果摘要/错误信息（卡片上只显示截断版）。
	if node.summary != "" {
		lines = append(lines, fmt.Sprintf("Summary: %s", node.summary))
	}
	if node.err != "" {
		lines = append(lines, fmt.Sprintf("Error: %s", node.err))
	}
	m.overlayPanel.open("Agent", lines)
}

// chatItem 表示对话区可展示的一项，可以是消息或事件。
type chatItem struct {
	// title 是主对话区展示的主要文本。
	title string
	// detail 是主对话区展示的 compact 详情（工具输出可能被截断）。
	detail string
	// rawDetail 是完整详情，用于弹窗/完整记录面板。
	rawDetail string
	// timestamp 是消息/事件发生时间，用于排序与时间戳显示。
	timestamp time.Time
	// isEvent 为 true 表示来自 SessionEvent，false 表示来自 ChatMessage。
	isEvent bool
	// role 仅对 ChatMessage 有效，标识消息角色。
	role enums.ChatRole
}

// buildPlanLines 将当前会话的任务看板渲染为弹窗用的扁平行列表。
func (m *Model) buildPlanLines() []string {
	s := m.selectedSession()
	var snap board.Snapshot
	if s != nil {
		snap = m.boardSnapshot(s)
	}

	// 没有看板时（如 direct_tool），用会话目标生成最小计划视图，
	// 避免弹窗只显示空白的 "(no plan)"。
	if len(snap.Tasks) == 0 {
		goal := ""
		status := board.TaskDone
		if s != nil {
			goal = s.Goal
			if goal == "" && len(s.Messages) > 0 {
				for _, msg := range s.Messages {
					if msg.Role == enums.ChatRoleUser {
						goal = strings.TrimSpace(msg.Content)
						break
					}
				}
			}
			switch s.Status {
			case enums.SessionStatusRunning:
				status = board.TaskInProgress
			case enums.SessionStatusError:
				status = board.TaskFailed
			}
		}
		if goal == "" {
			goal = "(no plan)"
		}
		snap = board.Snapshot{
			Goal:  goal,
			Tasks: []board.SubTask{{ID: "direct", Title: "直接执行", Status: status}},
		}
	}
	// Tree 路径的看板快照不带 Goal：用会话目标补充，避免弹窗 Goal 行空白。
	if snap.Goal == "" && s != nil {
		snap.Goal = s.Goal
	}

	// 统计完成数量。
	done, total := 0, len(snap.Tasks)
	for _, t := range snap.Tasks {
		if t.Status == board.TaskDone {
			done++
		}
	}
	lines := []string{
		fmt.Sprintf("Goal: %s", snap.Goal),
		fmt.Sprintf("Progress: %d/%d", done, total),
		"",
	}
	// 每条任务一行，当前光标处加上选中标记。
	for i, t := range snap.Tasks {
		marker := " "
		if i == m.overlayPanel.cursor {
			marker = "▸"
		}
		lines = append(lines, fmt.Sprintf("%s %s  %s", marker, statusIcon(string(t.Status)), t.Title))
	}
	return lines
}

// buildAgentsLines 将 Agent 拓扑渲染为弹窗用的扁平行列表。
func (m *Model) buildAgentsLines() []string {
	if len(m.agentTreePanel.nodes) == 0 {
		return []string{"(no agents)"}
	}
	var lines []string
	for i, node := range m.agentTreePanel.nodes {
		// 缩进由深度决定。
		prefix := strings.Repeat("  ", node.depth)
		var icon string
		switch node.roleType {
		case enums.RoleTypeMeta:
			icon = "◆"
		case enums.RoleTypeDomain:
			icon = "◆"
		case enums.RoleTypeSubDomain:
			icon = "◇"
		default:
			icon = "▸"
		}
		marker := " "
		if i == m.overlayPanel.cursor {
			marker = "▸"
		}
		name := node.name
		if node.goal != "" {
			name += " — " + truncate(node.goal, 40)
		}
		if node.isClarify {
			name = "Clarify pending"
		}
		lines = append(lines, fmt.Sprintf("%s %s %s %s  %s", marker, prefix, icon, name, statusIcon(string(node.status))))
	}
	return lines
}

// chatItems 将会话消息与关键事件合并为可展示的 chatItem 列表，按时间戳稳定排序，
// 并在 compact=true 时进行聚合与去重处理。
func chatItems(s *server.Session, compact bool) []chatItem {
	return chatItemsResolved(s, compact, nil)
}

// chatItemsResolved 是 chatItems 的完整版本：resolver 把子 Agent 实例 ID
// （如 session-1/code_assistant-5）映射为展示名，用于 llm_result 事件（子 Agent 结果摘要）
// 的归属展示；resolver 为 nil 时保持事件原始 Agent 字段。
func chatItemsResolved(s *server.Session, compact bool, resolver func(childID string) string) []chatItem {
	// TUI 对话区按时间戳交错展示会话消息与关键 Agent 事件
	// （tool_call、LLM/思考输出、错误等），使 ReAct 序列
	// （思考 → 工具 → 结果 → 下一步思考）清晰可见。调试类事件
	// （token_usage/graph_step/agent_created/prompt/stats）已被过滤，
	// 以保持界面可读。
	var items []chatItem
	// 遍历 ChatMessage，根据角色生成 title。
	for _, msg := range s.Messages {
		// v2.5：用户输入前缀 ">"，助手回复普通文本，
		// 初始 "Goal: ..." 系统提示作为噪声过滤。
		content := strings.TrimSpace(msg.Content)
		if msg.Role == enums.ChatRoleSystem && strings.HasPrefix(content, "Goal: ") {
			continue
		}

		var title string
		var detail string
		switch msg.Role {
		case enums.ChatRoleUser:
			// 澄清答复的内部标记前缀不展示，保持用户问题干净。
			title = "> " + strings.TrimPrefix(content, "[澄清答复] ")
		case enums.ChatRoleAssistant:
			title = content
			detail = ""
		default:
			// system / tool 等角色按原格式展示
			title = fmt.Sprintf("[%s] %s", msg.Role, msg.Timestamp.Format("15:04:05"))
			detail = formatMarkdown(content)
		}
		items = append(items, chatItem{
			title:     title,
			detail:    detail,
			timestamp: msg.Timestamp,
			isEvent:   false,
			role:      msg.Role,
		})
	}
	// P2-1（文档 §2.2）：内联 🧠 recalled 最多 2 条，避免淹没主对话。
	// 找出最后 2 条 memory_recall 事件的索引，其余在下面循环中跳过。
	recallKeep := map[int]bool{}
	var recallIdxs []int
	for i, ev := range s.Events {
		if ev.Kind == "memory_recall" && strings.TrimSpace(ev.Message) != "" {
			recallIdxs = append(recallIdxs, i)
		}
	}
	keepFrom := 0
	if len(recallIdxs) > 2 {
		keepFrom = len(recallIdxs) - 2 // 仅保留最后 2 条
	}
	for k := keepFrom; k < len(recallIdxs); k++ {
		recallKeep[recallIdxs[k]] = true
	}

	// 遍历事件，转换为 chatItem。
	for i, ev := range s.Events {
		// 超出限额的 memory_recall 不内联展示（仍在右侧 Agent 面板的 Memory 区可见）
		if ev.Kind == "memory_recall" && !recallKeep[i] {
			continue
		}
		// agent_done 是大模型最终答复：转为 assistant 条目，与 Assistant 消息同样式渲染
		// （Markdown + Assistant 标签），避免 "MetaAgent 完成: " 前缀噪音；
		// 恢复历史会话时同一答复已有 Assistant 消息，由后面的相邻去重消除重复。
		if ev.Type == "agent_done" {
			text := strings.TrimSpace(ev.Message)
			if text == "" {
				continue
			}
			items = append(items, chatItem{title: text, timestamp: ev.Timestamp, isEvent: false, role: enums.ChatRoleAssistant})
			continue
		}
		// 子 Agent 结果摘要事件（llm_result，Tool=实例 ID）：把 "SubAgent" 替换为实例对应的
		// 展示名（如"游戏渲染领域"），让用户直接看到是谁的产出。
		if ev.Kind == "llm_result" && resolver != nil && ev.Tool != "" {
			if name := resolver(ev.Tool); name != "" {
				ev.Agent = name
			}
		}
		title, detail, rawDetail, ok := eventChatItem(ev, compact)
		if !ok {
			continue
		}
		items = append(items, chatItem{title: title, detail: detail, rawDetail: rawDetail, timestamp: ev.Timestamp, isEvent: true})
	}
	// 稳定排序：同时间戳时保持插入顺序（message 先于其后触发的事件）
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].timestamp.Before(items[j].timestamp)
	})

	// 合并：把相邻的 [●] 工具调用行与同工具的 [✓]/[✗] 结果行合并为单条，
	// 让每个工具调用在对话区只占一行；未等到结果的 [●] 行保留，表示仍在执行。
	items = mergeToolCallPairs(items)

	// 聚合：把连续的无详情非 verbose 工具完成事件合并为 [✓] ToolName × N，
	// 大幅减少 ListDir/ReadFile 等高频工具在对话区的刷屏。
	if compact {
		items = aggregateToolEvents(items)
	}

	// 去重：删除所有与最后一条 Assistant 消息内容重复的 "MetaAgent 总结: 会话完成: ..."
	// 事件，避免直接回答等场景下同一段答案出现多次。
	if len(items) >= 2 {
		var lastAssistantIdx int = -1
		for i := len(items) - 1; i >= 0; i-- {
			if !items[i].isEvent && items[i].role == enums.ChatRoleAssistant {
				lastAssistantIdx = i
				break
			}
		}
		if lastAssistantIdx >= 0 {
			assistantText := normalizeChatText(items[lastAssistantIdx].title)
			// 从后往前删除所有重复总结事件（可能因 LLM/事件重复产生多条）
			for i := len(items) - 1; i > lastAssistantIdx; i-- {
				if !items[i].isEvent {
					continue
				}
				// 新格式: "✓ 会话已结束 — MetaAgent 总结: 会话完成: ..."
				// 旧格式: "MetaAgent 总结: 会话完成: ..."
				var summary string
				if strings.HasPrefix(items[i].title, "✓ 会话已结束 — MetaAgent 总结: 会话完成: ") {
					summary = strings.TrimPrefix(items[i].title, "✓ 会话已结束 — MetaAgent 总结: 会话完成: ")
				} else if strings.HasPrefix(items[i].title, "MetaAgent 总结: 会话完成: ") {
					summary = strings.TrimPrefix(items[i].title, "MetaAgent 总结: 会话完成: ")
				} else {
					continue
				}
				if normalizeChatText(summary) == assistantText {
					items = append(items[:i], items[i+1:]...)
				}
			}
		}
	}

	// 去重：相邻且内容相同的 assistant 条目只保留第一条。
	// 恢复历史会话时，最终答复会同时以 Assistant 消息（历史）与 agent_done 转换条目出现；
	// 遇到新的用户消息则重置比较，避免误删多轮对话中的正当重复。
	{
		deduped := make([]chatItem, 0, len(items))
		lastAssistant := ""
		for _, it := range items {
			if !it.isEvent && it.role == enums.ChatRoleAssistant {
				norm := normalizeChatText(it.title)
				if norm != "" && norm == lastAssistant {
					continue
				}
				lastAssistant = norm
			} else if !it.isEvent && it.role == enums.ChatRoleUser {
				lastAssistant = ""
			}
			deduped = append(deduped, it)
		}
		items = deduped
	}
	// 运行中会话追加实时状态条目（不走事件流，避免 token 级事件淹没事件列表）：
	// 有流式文本时展示"正在输出"的助手条目；否则按是否有未完成的工具调用
	// 分别展示"工具执行中"与"思考中"等待状态，保证等待期界面始终有反馈。
	if s.Status == enums.SessionStatusRunning {
		items = appendLiveItems(items, s, resolver)
	}

	return items
}

// appendLiveItems 为运行中会话追加一条实时状态条目（均为瞬时展示，不进事件流）：
// 答复流式输出 > 思考过程 > 工具执行中 > 等待子 Agent > 思考中，任一时刻只展示一条。
// 等待子 Agent 是次要行（主任务目标由对话区顶部固定目标栏常驻展示，TODO #48 子项 3）；
// resolver 把实例 ID 解析为展示名（领域名/角色中文名），nil 时回退原始 ID。
func appendLiveItems(items []chatItem, s *server.Session, resolver func(childID string) string) []chatItem {
	now := time.Now()
	// 有答复流式文本：作为助手条目展示截至当前的累积输出，末尾加光标符提示仍在生成。
	if strings.TrimSpace(s.StreamingText) != "" {
		return append(items, chatItem{
			title:     s.StreamingText + " ▍",
			timestamp: now,
			isEvent:   false,
			role:      enums.ChatRoleAssistant,
		})
	}
	// 有思考过程文本：暗色 💭 展示（瞬时，答复开始或会话结束时消失）。
	if strings.TrimSpace(s.ThinkingText) != "" {
		return append(items, chatItem{
			title:     "💭 " + strings.TrimSpace(s.ThinkingText) + " ▍",
			timestamp: now,
			isEvent:   true,
		})
	}
	// 无流式文本：有未完成的工具调用则提示工具执行中。
	if tool := inflightTool(s.Events); tool != "" {
		return append(items, chatItem{
			title:     "⚙ 正在执行工具: " + tool,
			timestamp: now,
			isEvent:   true,
		})
	}
	// 有已派发但未完成的子 Agent：提示等待子 Agent（展示名优先，读不出"谁在等谁"）。
	if sub := waitingSubAgent(s.Events); sub != "" {
		name := sub
		if resolver != nil {
			if r := resolver(sub); r != "" {
				name = r
			}
		}
		return append(items, chatItem{
			title:     "⏳ 等待子 Agent 执行: " + name,
			timestamp: now,
			isEvent:   true,
		})
	}
	// 否则处于 LLM 调用等待期，提示思考中。转圈帧 + 动态省略号（相位取墙钟），
	// 由 Model tick 的 200ms 强制刷新驱动重绘；thinking 模型首块前可静默数分钟，
	// 动效是等待期唯一的"还活着"反馈（2026-08-20：MetaAgent 流静默 15min+ 用户无从分辨）。
	return append(items, chatItem{
		title:     thinkingWaitTitle(now),
		timestamp: now,
		isEvent:   true,
	})
}

// spinnerFrames 是思考等待期的盲文转圈帧序列。
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// thinkingWaitTitle 依据墙钟相位生成"转圈 + 思考中 + 渐增省略号"标题：
// 帧周期 200ms（与 Model 的强制刷新节奏一致），省略号步进 600ms。
func thinkingWaitTitle(now time.Time) string {
	p := now.UnixMilli() / 200
	frame := spinnerFrames[p%int64(len(spinnerFrames))]
	dots := strings.Repeat("·", int((p/3)%4))
	return frame + " MetaAgent 思考中" + dots
}

// waitingSubAgent 返回当前仍处于"已派发未回传"状态的子 Agent ID；没有时返回空串。
// 判定：tool_exec(Tool=call_sub_agent) 次数多于 sub_agent_done 事件数，
// 最近一个未完成 ID 取自对应 tool_exec 事件的 ToolPath。
func waitingSubAgent(events []server.SessionEvent) string {
	dispatched := []string{}
	done := 0
	for _, ev := range events {
		switch {
		case ev.Type == "tool_exec" && ev.Tool == "call_sub_agent" && ev.Success:
			dispatched = append(dispatched, ev.ToolPath)
		case ev.Kind == "sub_agent_done":
			done++
		}
	}
	if len(dispatched) <= done {
		return ""
	}
	last := dispatched[len(dispatched)-1]
	if last == "" {
		last = "sub-agent"
	}
	return last
}

// subAgentRoleFromID 从子 Agent ID（形如 session-1/code_assistant-1）还原角色 ID。
func subAgentRoleFromID(id string) string {
	seg := id
	if idx := strings.LastIndex(seg, "/"); idx >= 0 {
		seg = seg[idx+1:]
	}
	// 去掉尾部序号（"-N"）。
	if dash := strings.LastIndex(seg, "-"); dash > 0 {
		return seg[:dash]
	}
	return seg
}

// subAgentRoleLabel 返回角色的人类可读类别标签：domain 为"领域 Agent"，其余为"助手"。
func subAgentRoleLabel(role string) string {
	if role == "domain" || strings.HasPrefix(role, "domain") {
		return "领域 Agent"
	}
	return "助手"
}

// subAgentNameResolver 构建子 Agent 实例 ID → 展示名的解析器：
// 优先查 Agent 树（instID→node.name，如 "游戏渲染领域"/"代码助手"），
// 未命中回退角色类别标签（领域 Agent/助手）。树为空返回 nil（调用方保持事件原始 Agent）。
func (m *Model) subAgentNameResolver() func(childID string) string {
	nodes := m.agentTreePanel.nodes
	if len(nodes) == 0 {
		return nil
	}
	byID := make(map[string]string, len(nodes))
	for _, n := range nodes {
		if n.instID != "" {
			byID[n.instID] = n.name
		}
	}
	return func(childID string) string {
		if name, ok := byID[childID]; ok {
			return name
		}
		return subAgentRoleLabel(subAgentRoleFromID(childID))
	}
}

// inflightTool 返回当前仍处于"已调用未返回"状态的工具名；没有时返回空串。
func inflightTool(events []server.SessionEvent) string {
	pending := map[string]int{}
	last := ""
	for _, ev := range events {
		switch {
		case ev.Kind == "tool_call":
			pending[ev.Tool]++
			last = ev.Tool
		case ev.Type == "tool_exec":
			if pending[ev.Tool] > 0 {
				pending[ev.Tool]--
			}
		}
	}
	if last != "" && pending[last] > 0 {
		return last
	}
	for t, n := range pending {
		if n > 0 {
			return t
		}
	}
	return ""
}

// mergeToolCallPairs 把相邻的 [●] 工具调用行与同工具的 [✓]/[✗] 结果行合并为单条结果行，
// 让每个工具调用在对话区只占一行（标题随结果状态确定）；未等到结果的 [●] 行保留，
// 表示工具仍在执行。
func mergeToolCallPairs(items []chatItem) []chatItem {
	out := make([]chatItem, 0, len(items))
	for i := 0; i < len(items); i++ {
		tool, ok := toolRowStatus(items[i], "●")
		if ok && i+1 < len(items) {
			if resTool, done := toolRowAnyResult(items[i+1]); done && resTool == tool {
				// 丢弃 [●] 行，只保留结果行（含详情）。
				out = append(out, items[i+1])
				i++
				continue
			}
		}
		out = append(out, items[i])
	}
	return out
}

// toolRowStatus 提取工具行的工具名，仅当条目是指定状态的事件工具行时返回 true。
func toolRowStatus(it chatItem, status string) (string, bool) {
	if !it.isEvent {
		return "", false
	}
	rest, ok := strings.CutPrefix(it.title, "["+status+"] ")
	if !ok {
		return "", false
	}
	tool, _, _ := strings.Cut(rest, ":")
	tool = strings.TrimSpace(tool)
	return tool, tool != ""
}

// toolRowAnyResult 提取工具结果行（[✓] 或 [✗]）的工具名。
func toolRowAnyResult(it chatItem) (string, bool) {
	if tool, ok := toolRowStatus(it, "✓"); ok {
		return tool, true
	}
	return toolRowStatus(it, "✗")
}

// normalizeChatText 把聊天文本归一化，用于去重比较：
// 移除 ANSI、Markdown 标记、列表符号，并压平空白。
func normalizeChatText(s string) string {
	s = stripANSI(s)
	s = strings.ReplaceAll(s, "**", "")
	s = strings.ReplaceAll(s, "*", "")
	s = strings.ReplaceAll(s, "`", "")
	s = strings.ReplaceAll(s, "#", "")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}

// aggregateToolEvents 把连续的无详情非 verbose 工具完成事件聚合成一条
// [✓] ToolName × N，显著减少 ListDir/ReadFile 等高频工具刷屏。
func aggregateToolEvents(items []chatItem) []chatItem {
	if len(items) < 2 {
		return items
	}
	var out []chatItem
	for i := 0; i < len(items); {
		it := items[i]
		tool, ok := collapsibleToolTitle(it)
		if !ok {
			out = append(out, it)
			i++
			continue
		}
		paths := []string{}
		j := i
		// 向后查找连续的同一工具完成事件。
		for j < len(items) {
			nextTool, ok := collapsibleToolTitle(items[j])
			if !ok || nextTool != tool {
				break
			}
			path := strings.TrimSpace(strings.TrimPrefix(items[j].title, "[✓] "+tool+":"))
			paths = append(paths, path)
			j++
		}
		if len(paths) > 1 {
			aggTitle := fmt.Sprintf("[✓] %s × %d", tool, len(paths))
			out = append(out, chatItem{
				title:     aggTitle,
				detail:    aggregateToolPaths(paths),
				timestamp: it.timestamp,
				isEvent:   true,
			})
			i = j
			continue
		}
		out = append(out, it)
		i++
	}
	return out
}

// collapsibleToolTitle 判断 item 是否可被聚合：非 verbose 工具的 [✓] 完成事件且无 detail。
func collapsibleToolTitle(it chatItem) (tool string, ok bool) {
	if !it.isEvent || it.detail != "" {
		return "", false
	}
	if !strings.HasPrefix(it.title, "[✓] ") {
		return "", false
	}
	rest := strings.TrimPrefix(it.title, "[✓] ")
	idx := strings.Index(rest, ":")
	if idx <= 0 {
		return "", false
	}
	tool = strings.TrimSpace(rest[:idx])
	if verboseTools[tool] {
		return "", false
	}
	return tool, true
}

// aggregateToolPaths 把路径列表折叠成短 detail，最多展示前 5 条。
func aggregateToolPaths(paths []string) string {
	const maxShow = 5
	var b strings.Builder
	n := len(paths)
	if n <= maxShow {
		for _, p := range paths {
			b.WriteString("  ")
			b.WriteString(p)
			b.WriteByte('\n')
		}
		return strings.TrimRight(b.String(), "\n")
	}
	for _, p := range paths[:maxShow] {
		b.WriteString("  ")
		b.WriteString(p)
		b.WriteByte('\n')
	}
	b.WriteString(fmt.Sprintf("  ... 还有 %d 条", n-maxShow))
	return b.String()
}

// buildTranscriptLines 把整段对话（消息 + 关键事件）铺成可滚动弹窗用的扁平行列表。
// 每行按弹窗内宽截断，避免自动换行打乱 overlay 的行级滚动计数。
// 用途：Ctrl+L / 4 打开的"完整记录"面板，让用户不受窗口高度与输入栏焦点限制，
// 用 j/k 翻阅全部 LLM 输出 / 工具调用 / 思考过程。
func (m *Model) buildTranscriptLines() []string {
	s := m.selectedSession()
	if s == nil {
		return []string{"(no active session)"}
	}
	items := chatItemsResolved(s, false, m.subAgentNameResolver())
	if len(items) == 0 {
		return []string{"(empty — send a message below)"}
	}
	// 弹窗内容宽度：boxW = width*4/5，减去 padding(4) + border(2) + 余量
	maxW := m.width*4/5 - 8
	if maxW < 40 {
		maxW = 80
	}
	var out []string
	for _, it := range items {
		out = append(out, truncate(it.title, maxW))
		detail := it.rawDetail
		if detail == "" {
			detail = it.detail
		}
		for _, l := range strings.Split(detail, "\n") {
			out = append(out, "    "+truncate(l, maxW-4))
		}
		out = append(out, m.styles.Dim.Render("─"))
	}
	return out
}

// verboseTools 默认在 TUI 对话区展开完整 ToolOutput 的工具。
// 其余工具（ReadFile/SearchInFiles/ListDir/HTTPGet/HTTPPost）只显示工具名和路径，
// 隐藏 output 内容以减少视觉噪声（P2-3）。
var verboseTools = map[string]bool{
	"WriteFile":  true,
	"RunCommand": true,
}

// compactToolOutputLines 是 verbose 工具输出在主对话区最多展示的行数，
// 超出部分折叠，可在弹窗/Ctrl+L 完整记录中查看。
const compactToolOutputLines = 5

// truncateToolOutput 截断工具输出到指定行数，超出部分显示 "  ..." 提示。
func truncateToolOutput(output string, maxLines int) string {
	if maxLines <= 0 {
		return output
	}
	lines := strings.Split(output, "\n")
	if len(lines) <= maxLines {
		return output
	}
	return strings.Join(lines[:maxLines], "\n") + "\n  ..."
}

// eventChatItem 把一个 SessionEvent 映射为对话区的一行（title + detail + rawDetail）。
// compact=true 时会对 verbose 工具的长输出做截断，用于主对话区；
// compact=false 时 rawDetail 保留完整输出，用于弹窗/完整记录面板。
// 返回 ok=false 表示该事件类型不展示（调试噪声）。
func eventChatItem(ev server.SessionEvent, compact bool) (title, detail, rawDetail string, ok bool) {
	switch {
	// 工具执行/调用事件。
	case ev.Type == "tool_exec" || ev.Kind == "tool_call":
		tool := ev.Tool
		if tool == "" {
			tool = "tool"
		}
		// 非 verbose 工具只保留执行完成的事件，隐藏调用前 pending 事件，
		// 避免 ListDir/ReadFile/HTTPGet 等高频工具在对话区产生大量 [●] 行。
		if ev.Kind == "tool_call" && !verboseTools[tool] {
			return "", "", "", false
		}
		status := "✓"
		if ev.Kind == "tool_call" {
			status = "●"
		}
		if !ev.Success && ev.Type == "tool_exec" {
			status = "✗"
		}
		title = fmt.Sprintf("[%s] %s", status, tool)
		if strings.TrimSpace(ev.ToolPath) != "" {
			title += ": " + ev.ToolPath
		}
		// 子 Agent 产生的工具事件在标题末尾追加归属（如 "[✓] WriteFile · 代码助手"），
		// 让用户能区分主流程与子 Agent 的工具调用；主会话 MetaAgent 与空归属不标注，
		// 保持主流程标题干净（Agent 展示名由后端 role.Name 提供：MetaAgent/代码助手/领域Agent:xxx）。
		if ev.Agent != "" && ev.Agent != "MetaAgent" {
			title += " · " + ev.Agent
		}
		// 工具注册表产生的固定文案（"调用工具 X"/"工具结果 X"）是噪声，不展示；
		// 其他来源的 Message 仍保留在完整记录中。
		msg := ev.Message
		if strings.HasPrefix(msg, "调用工具 ") || strings.HasPrefix(msg, "工具结果 ") {
			msg = ""
		}
		// rawDetail 始终保留完整信息，用于弹窗/完整记录面板
		var full strings.Builder
		if msg != "" {
			full.WriteString(msg)
			full.WriteByte('\n')
		}
		if ev.ToolOutput != "" {
			full.WriteString("结果:\n")
			full.WriteString(sanitizeToolText(ev.ToolOutput))
			full.WriteByte('\n')
		}
		if ev.ToolError != "" {
			full.WriteString("错误: ")
			full.WriteString(sanitizeToolText(ev.ToolError))
			full.WriteByte('\n')
		}
		rawDetail = strings.TrimRight(full.String(), "\n")

		// compact detail：verbose 工具只展示截断后的输出（完整内容见完整记录）；
		// 非 verbose 工具仅展示 Error，避免输出刷屏但保证错误可见。
		var compactDetail strings.Builder
		if verboseTools[tool] && ev.ToolOutput != "" {
			compactDetail.WriteString(truncateToolOutput(sanitizeToolText(ev.ToolOutput), compactToolOutputLines))
			compactDetail.WriteByte('\n')
		}
		if ev.ToolError != "" {
			compactDetail.WriteString("错误: ")
			compactDetail.WriteString(sanitizeToolText(ev.ToolError))
			compactDetail.WriteByte('\n')
		}
		detail = strings.TrimRight(compactDetail.String(), "\n")
		if !compact {
			detail = rawDetail
		}
		return title, detail, rawDetail, true
	// 记忆召回事件。
	case ev.Kind == "memory_recall":
		title = "🧠 recalled: " + strings.TrimSpace(ev.Message)
		return title, "", title, true
	// 话题切换事件。
	case ev.Kind == "topic_switch":
		msg := ev.Message
		if msg == "" {
			msg = "切换话题"
		}
		title = "─── " + msg + " ───"
		return title, "", title, true
	// 思考过程（中间推理步骤）事件：以 💭 前缀标识，渲染时用暗色区别于正式答复。
	case ev.Kind == "think":
		msg := strings.TrimSpace(ev.Message)
		if msg == "" {
			return "", "", "", false
		}
		title = "💭 " + msg
		return title, "", title, true
	// 子 Agent 派发事件：Tool 携带角色 ID，Message 为任务全文（后端不再截断）。
	// 标题按显示宽度截断防刷屏；完整任务文本放 rawDetail 供完整记录查看。
	case ev.Kind == "sub_agent_dispatch":
		role := ev.Tool
		if role == "" {
			role = "sub-agent"
		}
		title = "🚀 派发" + subAgentRoleLabel(role) + ": " + role
		rawDetail = strings.TrimSpace(ev.Message)
		if rawDetail != "" {
			title += " — " + truncate(rawDetail, 200)
		}
		return title, "", rawDetail, true
	// 子 Agent 完成事件：Message 携带子 Agent ID（形如 session-1/code_assistant-1）。
	case ev.Kind == "sub_agent_done":
		id := strings.TrimSpace(ev.Message)
		if id == "" {
			id = "sub-agent"
		}
		role := subAgentRoleFromID(id)
		title = "✓ " + subAgentRoleLabel(role) + "执行完成: " + id
		return title, "", title, true
	// 澄清问答事件（TODO #53）：提问（Agent 非 User）与答复（Agent 为 User）。
	// 后端原始 Message 带 "Agent 提问: " / "提问答复: " / "审批答复: " 前缀，
	// 在此剥离后以 ❓/✅ 前缀展示；空消息不展示。
	case ev.Type == "clarify":
		msg := strings.TrimSpace(ev.Message)
		if msg == "" {
			return "", "", "", false
		}
		if ev.Agent != "User" {
			msg = strings.TrimSpace(strings.TrimPrefix(msg, "Agent 提问: "))
			title = "❓ " + msg
		} else {
			msg = strings.TrimSpace(strings.TrimPrefix(msg, "提问答复: "))
			msg = strings.TrimSpace(strings.TrimPrefix(msg, "审批答复: "))
			title = "✅ 答复: " + msg
		}
		if msg == "" {
			return "", "", "", false
		}
		return title, "", title, true
	// LLM/思考/等待等事件。
	case ev.Kind == "llm_result" || ev.Kind == "llm" || ev.Kind == "intend" || ev.Kind == "wait":
		agent := ev.Agent
		if agent == "" {
			agent = "Assistant"
		}
		title = agent + ": " + ev.Message
		return title, "", title, true
	// 系统事件：只展示会话完成总结与轮数暂停提示。
	case ev.Type == "system":
		// 轮数上限暂停（非错误）：提示用户发送消息即可续跑。
		if strings.Contains(ev.Message, "已达最大轮数") {
			title = "⏸ " + ev.Message
			return title, "", title, true
		}
		// 只展示会话完成总结，避免 "会话启动/继续执行" 等噪声淹没对话。
		if strings.Contains(ev.Message, "会话完成") {
			agent := ev.Agent
			if agent == "" {
				agent = "MetaAgent"
			}
			// 加 ✓ 已结束 前缀，让用户一眼看出会话已结束
			title = "✓ 会话已结束 — " + agent + " 总结: " + ev.Message
			return title, "", title, true
		}
		return "", "", "", false
	// 错误事件。
	case ev.Kind == "error" || ev.Type == "error":
		title = "✗ Error: " + ev.Message
		return title, "", title, true
	}
	return "", "", "", false
}

// formatMarkdown 对聊天文本做轻量 Markdown 格式化，支持标题、加粗、斜体、行内代码、
// 代码块、列表、引用等。
// 行级状态机：按行扫描，遇 ``` 切换 inCodeBlock；代码块内原样累积，块外按行类型渲染。
func formatMarkdown(text string) string {
	var out []string
	var inCodeBlock bool
	var codeBlock []string

	// 预定义样式：避免在循环内反复构造，提升长文本渲染性能
	mdHeader := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(cHeader))
	mdBold := lipgloss.NewStyle().Bold(true)
	mdItalic := lipgloss.NewStyle().Italic(true)
	mdCode := lipgloss.NewStyle().Foreground(lipgloss.Color(cWarn))
	mdCodeBlock := lipgloss.NewStyle().Background(lipgloss.Color(cBlur)).Foreground(lipgloss.Color(cValue)).Padding(0, 1)
	mdDim := lipgloss.NewStyle().Foreground(lipgloss.Color(cDone))

	// flushCode 把累积的代码块行渲染为单个带背景的代码块并清空缓存
	flushCode := func() {
		if len(codeBlock) == 0 {
			return
		}
		out = append(out, mdCodeBlock.Render(strings.Join(codeBlock, "\n")))
		codeBlock = nil
	}

	for _, raw := range strings.Split(text, "\n") {
		line := raw
		trimmed := strings.TrimSpace(line)

		// Code fence：``` 切换代码块状态；行本身不输出
		if strings.HasPrefix(trimmed, "```") {
			if inCodeBlock {
				flushCode()
				inCodeBlock = false
			} else {
				inCodeBlock = true
			}
			continue
		}
		// 代码块内：原样累积，跳过其他 Markdown 解析，避免 ** 被误识别
		if inCodeBlock {
			codeBlock = append(codeBlock, line)
			continue
		}

		// Header：# ~ ###### 后跟空格或行尾；level 不限但渲染样式统一
		if strings.HasPrefix(trimmed, "#") {
			level := 0
			for level < len(trimmed) && trimmed[level] == '#' {
				level++
			}
			if level > 0 && (level == len(trimmed) || trimmed[level] == ' ') {
				content := strings.TrimSpace(trimmed[level:])
				content = applyInlineMarkdown(content, mdBold, mdItalic, mdCode)
				out = append(out, mdHeader.Render(content))
				continue
			}
		}

		// Blockquote：> 前缀，用 ┃ 替换以适配等宽字体对齐
		if strings.HasPrefix(trimmed, ">") {
			content := strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))
			content = applyInlineMarkdown(content, mdBold, mdItalic, mdCode)
			out = append(out, mdDim.Render("┃ "+content))
			continue
		}

		// List item：- / * / + 后跟空格才识别（避免把 *bold* 误判为列表项）
		if strings.HasPrefix(trimmed, "-") || strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "+") {
			if len(trimmed) > 1 && trimmed[1] == ' ' {
				content := strings.TrimSpace(trimmed[1:])
				content = applyInlineMarkdown(content, mdBold, mdItalic, mdCode)
				out = append(out, mdDim.Render("• ")+content)
				continue
			}
		}

		// Plain line：仅做内联格式化
		line = applyInlineMarkdown(line, mdBold, mdItalic, mdCode)
		out = append(out, line)
	}
	// 文本结束时若仍在代码块中，flush 兜底输出
	flushCode()

	return strings.Join(out, "\n")
}

// applyInlineMarkdown 处理行内 Markdown：**粗体**、*斜体*、`行内代码`。
// 顺序很重要：先处理 `code`（避免其中 ** 被吃掉），再 **bold，最后 *italic。
// 这样 ** 会优先于 * 被匹配，避免 bold 内容被识别成 italic。
func applyInlineMarkdown(text string, bold, italic, code lipgloss.Style) string {
	// 行内代码
	text = replacePairs(text, "`", func(s string) string { return code.Render(s) })
	// 粗体
	text = replacePairs(text, "**", func(s string) string { return bold.Render(s) })
	// 斜体（未被粗体消耗的单星号）
	text = replacePairs(text, "*", func(s string) string { return italic.Render(s) })
	return text
}

// replacePairs 简易成对替换：找到首个 marker 作为开标记，再找其后第一个 marker 作为闭标记，
// 把中间内容传给 f 渲染；循环到没有配对为止。不支持嵌套。
func replacePairs(text, marker string, f func(string) string) string {
	for {
		start := strings.Index(text, marker)
		if start == -1 {
			break
		}
		end := strings.Index(text[start+len(marker):], marker)
		if end == -1 {
			break // 只有开标记无闭标记：保持原样，避免吞字符
		}
		end += start + len(marker)
		content := text[start+len(marker) : end]
		text = text[:start] + f(content) + text[end+len(marker):]
	}
	return text
}

// planStatusText 把看板任务状态转换为短文本标签，用于右侧面板展示。
func planStatusText(status board.TaskStatus) string {
	switch status {
	case board.TaskDone:
		return " Done"
	case board.TaskInProgress:
		return " Running"
	case board.TaskBlocked:
		return " Blocked"
	case board.TaskFailed:
		return " Failed"
	case board.TaskUnverified:
		return " Unverified"
	default:
		return " Waiting"
	}
}

// taskElapsed 估算任务已用时长：已完成/失败用 UpdatedAt-CreatedAt，
// 进行中用 Now-CreatedAt，待处理返回 0。
func taskElapsed(t board.SubTask) time.Duration {
	if t.Status == board.TaskPending {
		return 0
	}
	end := t.UpdatedAt
	if t.Status == board.TaskInProgress || t.Status == board.TaskBlocked {
		end = time.Now()
	}
	if end.Before(t.CreatedAt) {
		end = t.CreatedAt
	}
	return end.Sub(t.CreatedAt)
}

// taskElapsedHMS 格式化任务已用时长为 hh:mm:ss；看板时间戳缺失时返回占位符而非异常大值。
func taskElapsedHMS(t board.SubTask) string {
	if t.CreatedAt.IsZero() {
		return "--:--:--"
	}
	if (t.Status == board.TaskDone || t.Status == board.TaskFailed || t.Status == board.TaskUnverified) && t.UpdatedAt.IsZero() {
		return "--:--:--"
	}
	return formatDurationHMS(taskElapsed(t))
}

// formatDurationHMS 把时长固定格式化为 hh:mm:ss（不足 1 小时也补前导零），
// 对齐"新TUI页.png"设计稿中执行计划面板的时长列。
func formatDurationHMS(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

// statusIcon 将状态字符串映射为展示图标，用于计划栏、Agent 栏等。
func statusIcon(status string) string {
	switch status {
	case "running", string(enums.RoleStatusActive), string(board.TaskInProgress):
		return "●"
	case "awaiting_clarify", string(enums.RoleStatusWaiting), string(board.TaskBlocked):
		return "◐"
	case "completed", string(board.TaskDone):
		return "✓"
	case "error", string(board.TaskFailed):
		return "✗"
	case string(board.TaskPending), string(enums.RoleStatusIdle):
		return "◦"
	default:
		return "◦"
	}
}

// formatTopBar 按 v2.0 格式渲染顶部状态栏：
// 示例输出：BlockMemoryAgent > {sessionID}  ●running  {N} agents active  in:{in} out:{out}
func formatTopBar(styles *Styles, sessionID, status string, agentCount, inTokens, outTokens int) string {
	if sessionID == "" {
		sessionID = "-"
	}
	if status == "" {
		status = "idle"
	}
	icon := statusIcon(status)
	agents := ""
	if agentCount > 0 {
		agents = fmt.Sprintf("  %d agents active", agentCount)
	}
	tokenStr := ""
	if inTokens > 0 || outTokens > 0 {
		tokenStr = fmt.Sprintf("  in:%d out:%d", inTokens, outTokens)
	}
	left := styles.Title.Render("BlockMemoryAgent") + styles.StatLabel.Render(" > ") + styles.StatValue.Render(sessionID)
	mid := styles.StatValue.Render("  "+icon+status) + styles.StatValue.Render(agents)
	right := styles.StatLabel.Render(tokenStr)
	return left + mid + right
}

// formatPlanBar 渲染计划进度栏。有任务看板时显示当前计划步骤与进度；无计划时显示 [direct] 直接回答。
func formatPlanBar(styles *Styles, snap board.Snapshot, toolLabel string) string {
	if len(snap.Tasks) == 0 {
		// 无 plan 时：若仍有工具在执行，展示工具状态（文档 §2.3 "plan 进度+工具状态"）
		if toolLabel != "" {
			return styles.Dim.Render("[tool] " + toolLabel + " [●]")
		}
		return styles.Dim.Render("[direct] 直接回答")
	}
	// 统计已完成数与当前步骤索引。
	done := 0
	current := 0
	for i, t := range snap.Tasks {
		if t.Status == board.TaskDone {
			done++
		}
		if t.Status == board.TaskInProgress || t.Status == board.TaskBlocked {
			current = i + 1
		}
	}
	if current == 0 && done < len(snap.Tasks) {
		current = done + 1
	}
	var steps []string
	for i, t := range snap.Tasks {
		step := fmt.Sprintf("%d.%s", i+1, t.Title)
		if i+1 == current {
			step = styles.StatValue.Render(step)
		} else if i+1 <= done {
			step = styles.Dim.Render(step)
		}
		steps = append(steps, step)
	}
	progressIcon := "●"
	if done == len(snap.Tasks) {
		progressIcon = "✓"
	}
	return fmt.Sprintf("[plan] %s   [%d/%d] %s",
		strings.Join(steps, " → "),
		current,
		len(snap.Tasks),
		styles.StatValue.Render(progressIcon),
	)
}

// lastToolLabel 返回当前会话中"进行中"的工具标签（"Name: path" 或 "Name"）。
// 判定：遍历事件，最后一个 tool_call（●）若未被后续 tool_exec（✓/✗）关闭，则视为进行中。
// 无进行中工具返回空串。用于 plan 栏展示工具状态（P2-1，文档 §2.3）。
func lastToolLabel(s *server.Session) string {
	if s == nil {
		return ""
	}
	inFlight := false
	var tool, path string
	for _, ev := range s.Events {
		if ev.Kind == "tool_call" {
			inFlight = true
			tool = ev.Tool
			path = ev.ToolPath
		} else if ev.Type == "tool_exec" {
			inFlight = false // 工具执行完成（成功或失败），关闭进行中标记
		}
	}
	if !inFlight {
		return ""
	}
	if tool == "" {
		tool = "tool"
	}
	if path != "" {
		return tool + ": " + path
	}
	return tool
}

// renderAgentPanel 渲染右侧 Agent 面板。
// recentMemoryRecalls 从会话事件中抽取最近召回的记忆片段。
func recentMemoryRecalls(s *server.Session, limit int) []string {
	if limit <= 0 {
		limit = 3
	}
	var out []string
	// 从后向前遍历，最多取 limit 条非空记忆召回。
	for i := len(s.Events) - 1; i >= 0 && len(out) < limit; i-- {
		ev := s.Events[i]
		if ev.Kind == "memory_recall" && strings.TrimSpace(ev.Message) != "" {
			out = append([]string{strings.TrimSpace(ev.Message)}, out...)
		}
	}
	return out
}
