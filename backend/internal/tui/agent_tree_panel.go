package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// agentTreeNode 表示 Agent 拓扑 flattened 后的一行节点。
type agentTreeNode struct {
	// depth 是节点在树中的深度，0 为顶层 MetaAgent。
	depth int
	// instID 是 Agent 实例 ID。
	instID string
	// name 是 Agent 显示名称。
	name string
	// domain 是 Agent 所属领域，用于与看板任务状态对齐。
	domain string
	// roleType 是 Agent 角色类型（Meta/Domain/SubDomain/Fixed/Dynamic）。
	roleType enums.RoleType
	// role 是 Agent 担任的角色名称（如 Designer/Developer），MetaAgent 固定为 Orchestrator。
	role string
	// status 是 Agent 当前状态。
	status enums.RoleStatus
	// goal 是 Agent 的目标描述，可能为空。
	goal string
	// summary 是终态 Agent 的结果摘要（来自 orchestrator.Node.Summary），可能为空。
	summary string
	// err 是失败 Agent 的错误信息（来自 orchestrator.Node.Err），可能为空。
	err string
	// isClarify 标识该节点是否为待澄清占位节点。
	isClarify bool
	// createdAt 是 Agent 创建时间，用于显示时间戳。
	createdAt time.Time
}

// AgentTreePanel 维护从 agent.Agent 数据构建的扁平 Agent 树渲染状态。
type AgentTreePanel struct {
	// nodes 是扁平化后的 Agent 树节点列表。
	nodes []agentTreeNode
}

// NewAgentTreePanel 构造一个空的 AgentTreePanel。
func NewAgentTreePanel() AgentTreePanel {
	return AgentTreePanel{}
}

// rebuild 根据会话与 agent facade 重建扁平 Agent 树，包含 MetaAgent、领域 Agent、
// 子领域 Agent 及固定/动态 Agent，并可能追加待澄清占位节点。
func (at *AgentTreePanel) rebuild(agentFacade agent.Agent, s *server.Session) {
	// 清空旧节点。
	at.nodes = nil
	// 无会话或 facade 时不构建。
	if s == nil || agentFacade == nil {
		return
	}

	// 根据会话状态推导 MetaAgent 状态。
	metaStatus := enums.RoleStatusIdle
	switch s.Status {
	case "running":
		metaStatus = enums.RoleStatusActive
	case "completed":
		metaStatus = enums.RoleStatusDone
	case "error":
		metaStatus = enums.RoleStatusError
	case "awaiting_clarify":
		metaStatus = enums.RoleStatusWaiting
	}
	// 推导 MetaAgent 目标：展示当前轮任务——取最后一条用户消息（多轮会话中反映最新任务），
	// 没有用户消息时回退到会话 Goal；当前轮开始时间取该消息时间（新一轮的开始时刻）。
	roundStart := currentRoundStart(s)
	metaGoal := ""
	for i := len(s.Messages) - 1; i >= 0; i-- {
		if s.Messages[i].Role == enums.ChatRoleUser {
			metaGoal = strings.TrimSpace(s.Messages[i].Content)
			break
		}
	}
	if metaGoal == "" {
		metaGoal = s.Goal
	}
	// 添加顶层 MetaAgent 节点。
	at.nodes = append(at.nodes, agentTreeNode{
		depth:     0,
		instID:    "MetaAgent",
		name:      "MetaAgent",
		roleType:  enums.RoleTypeMeta,
		role:      "Orchestrator",
		status:    metaStatus,
		goal:      metaGoal,
		createdAt: roundStart,
	})

	// 读取权威 Agent 树(Dispatcher 维护,已 PG 持久化,重启后 lazy 恢复)。
	// 替代旧事件流派生(deriveSubAgentNodes):树是单一真相源,事件流派生有竞态/遗漏。
	// Tree() 返回 []orchestrator.Node,按启动时间升序;MetaAgent 非节点,已在上方作为根加入。
	// 过滤上一轮已终结的节点:旧完成卡片排在最前会一直占位,把新一轮派发的 Agent
	// 挤出可视区(实证:第二轮运行时编排面板看似不刷新,全是上一轮 Done 卡片)。
	nodes, err := agentFacade.Tree(context.Background(), s.ID)
	if err == nil {
		at.nodes = append(at.nodes, orchestratorNodesToTreeNodes(filterPrevRoundNodes(nodes, roundStart), s.ID)...)
	}

	// 若会话处于待澄清状态，追加一个占位节点提示用户（含问题文本，TODO #24）。
	if s.State != nil && s.State.PendingClarify != nil {
		label := "Clarify pending"
		if q := strings.TrimSpace(s.State.PendingClarify.Question); q != "" {
			label = "待答复: " + truncate(q, 40)
		}
		at.nodes = append(at.nodes, agentTreeNode{
			depth:     1,
			instID:    "clarify",
			name:      label,
			roleType:  enums.RoleTypeMeta,
			status:    enums.RoleStatusWaiting,
			isClarify: true,
		})
	}
}

// orchestratorNodesToTreeNodes 把权威树节点 []orchestrator.Node 映射为扁平渲染节点,
// 按 ParentID 链计算 depth:根(MetaAgent,rootID)的直接子节点 depth=1,逐级 +1。
// 状态映射:Running->Active / Done->Done / Failed->Error / Cancelled->Done(终态)。
func orchestratorNodesToTreeNodes(nodes []orchestrator.Node, rootID string) []agentTreeNode {
	byID := make(map[string]int, len(nodes))
	for i, n := range nodes {
		byID[n.ID] = i
	}
	out := make([]agentTreeNode, 0, len(nodes))
	for _, n := range nodes {
		roleType := enums.RoleTypeFixed
		if n.Role == "domain" || strings.HasPrefix(n.Role, "domain") {
			roleType = enums.RoleTypeDomain
		}
		out = append(out, agentTreeNode{
			depth:     orchestratorNodeDepth(n, nodes, byID, rootID),
			instID:    n.ID,
			name:      agentNodeName(n),
			domain:    n.Domain,
			roleType:  roleType,
			role:      n.Role,
			status:    orchestratorStatusToRole(n.Status),
			goal:      n.Task,
			summary:   n.Summary,
			err:       n.Err,
			createdAt: n.Started,
		})
	}
	return out
}

// agentNodeName 返回树节点的展示名。
// domain 角色优先用 LLM 提供的 Domain 字段拼"XX领域"（如 游戏渲染 -> 游戏渲染领域），
// 顾名思义，替代原来所有领域 Agent 都叫 "domain"（对话流里则是裸 ID "session-N/domain-2"）
// 无法区分的命名；固定助手映射为 roles.yaml 里的中文角色名；其余兜底返回 Role。
func agentNodeName(n orchestrator.Node) string {
	role := strings.TrimSpace(n.Role)
	if role == "domain" || strings.HasPrefix(role, "domain") {
		// TrimSuffix 防 LLM 已填"XX领域"时拼出"XX领域领域"。
		if d := strings.TrimSuffix(strings.TrimSpace(n.Domain), "领域"); d != "" {
			return d + "领域"
		}
		return "领域Agent"
	}
	if name, ok := fixedRoleDisplayNames[role]; ok {
		return name
	}
	return role
}

// fixedRoleDisplayNames 固定助手 role_id -> 中文展示名（与 config/roles.yaml 的 name 对齐）。
var fixedRoleDisplayNames = map[string]string{
	"code_assistant":  "代码助手",
	"ui_assistant":    "UI助手",
	"test_assistant":  "测试助手",
	"doc_assistant":   "文档助手",
	"code_reviewer":   "代码审查助手",
	"prompt_reviewer": "提示词审查助手",
}

// filterPrevRoundNodes 丢弃上一轮次已终结的 Agent 节点：
// Started 早于 roundStart 且处于终态（Done/Failed/Cancelled）的节点被过滤；
// 仍在 Running/Paused 的节点无论起始时间都保留（跨轮未完结的工作仍可见）。
// roundStart 为零值（无用户消息时间戳，如恢复的历史会话）时不过滤。
func filterPrevRoundNodes(nodes []orchestrator.Node, roundStart time.Time) []orchestrator.Node {
	if roundStart.IsZero() {
		return nodes
	}
	out := make([]orchestrator.Node, 0, len(nodes))
	for _, n := range nodes {
		switch n.Status {
		case orchestrator.StatusDone, orchestrator.StatusFailed, orchestrator.StatusCancelled:
			if n.Started.Before(roundStart) {
				continue
			}
		}
		out = append(out, n)
	}
	return out
}

// orchestratorNodeDepth 沿 ParentID 链向上数祖先数:直接子节点(rootID 为父)depth=1。
// 循环/缺失父节点兜底 depth=1。深度上限为节点总数,防环。
func orchestratorNodeDepth(n orchestrator.Node, nodes []orchestrator.Node, byID map[string]int, rootID string) int {
	depth := 1
	pid := n.ParentID
	for i := 0; i < len(nodes); i++ {
		if pid == "" || pid == rootID {
			break
		}
		idx, ok := byID[pid]
		if !ok {
			break
		}
		depth++
		pid = nodes[idx].ParentID
	}
	return depth
}

// orchestratorStatusToRole 把 orchestrator.Status 映射为 TUI RoleStatus。
// Cancelled 归入 Done(终态,非错误)；Paused(触达 token 上限待恢复)归入 Waiting。
func orchestratorStatusToRole(s orchestrator.Status) enums.RoleStatus {
	switch s {
	case orchestrator.StatusRunning:
		return enums.RoleStatusActive
	case orchestrator.StatusDone:
		return enums.RoleStatusDone
	case orchestrator.StatusFailed:
		return enums.RoleStatusError
	case orchestrator.StatusCancelled:
		return enums.RoleStatusDone
	case orchestrator.StatusPaused:
		return enums.RoleStatusWaiting
	}
	return enums.RoleStatusIdle
}

// deriveDomainTaskStatuses 从 Agent 树中汇总每个领域的真实状态，用于覆盖看板中可能滞后的状态。
// 优先级：Failed > InProgress > Done > Pending。
func (at *AgentTreePanel) deriveDomainTaskStatuses() map[string]board.TaskStatus {
	status := make(map[string]board.TaskStatus)
	for _, node := range at.nodes {
		// 无领域信息的节点不参与计划状态汇总。
		if node.domain == "" {
			continue
		}
		var st board.TaskStatus
		// 将 Agent 状态映射为看板任务状态。
		switch node.status {
		case enums.RoleStatusError:
			st = board.TaskFailed
		case enums.RoleStatusActive:
			st = board.TaskInProgress
		case enums.RoleStatusDone:
			st = board.TaskDone
		default:
			st = board.TaskPending
		}
		// 同一领域多个 Agent 时取优先级更高的状态。
		cur := status[node.domain]
		status[node.domain] = strongerTaskStatus(cur, st)
	}
	return status
}

// buildLines 将 Agent 树渲染为弹窗可用的扁平行列表。
func (at *AgentTreePanel) buildLines() []string {
	if len(at.nodes) == 0 {
		return []string{"(no agents)"}
	}
	var lines []string
	for _, node := range at.nodes {
		// 根据深度生成缩进。
		prefix := strings.Repeat("  ", node.depth)
		// 根据角色类型选择图标。
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
		// 名称后追加目标摘要（如有）。
		name := node.name
		if node.goal != "" {
			name += " — " + truncate(node.goal, 40)
		}
		// 待澄清节点显示固定文本。
		if node.isClarify {
			name = "Clarify pending"
		}
		lines = append(lines, fmt.Sprintf("%s %s %s %s  %s", " ", prefix, icon, name, statusIcon(string(node.status))))
	}
	return lines
}

// renderAgentsPanel 渲染右侧 Agent 编排面板。
// 布局参考"新TUI页.png"：顶部为居中的 MetaAgent 卡片，经连接线引出
// 子 Agent 卡片网格（宽度足够时两列），底部为状态图例。
func (m Model) renderAgentsPanel(w, h int) string {
	// 保证最小宽度，避免卡片过度压缩。
	if w < 20 {
		w = 20
	}

	// 构建标题栏：左侧标题 + 右侧关闭提示，中间用空格填充。
	titleLeft := "🧠 Agent 编排"
	titleRight := "[A] 关闭"
	titlePadding := w - lipgloss.Width(titleLeft) - lipgloss.Width(titleRight) - 2
	if titlePadding < 1 {
		titlePadding = 1
	}
	headerText := titleLeft + strings.Repeat(" ", titlePadding) + titleRight
	header := m.styles.PanelHeader.Width(w).Render(headerText)
	// 内容区可用宽度需扣除边框与内边距。
	innerW := w - 4
	if innerW < 10 {
		innerW = 10
	}
	// 内容区最大行数（PanelBox Height(h-3)），超出部分按卡片行粒度裁剪。
	maxBody := h - 3
	if maxBody < 1 {
		maxBody = 1
	}

	// 分离 MetaAgent 与子 Agent 节点；过滤掉二级以下、无目标且已完成/空闲的节点，
	// 避免面板过于拥挤。
	var meta *agentTreeNode
	var children []agentTreeNode
	for i := range m.agentTreePanel.nodes {
		node := m.agentTreePanel.nodes[i]
		if node.depth == 0 && !node.isClarify {
			n := node
			meta = &n
			continue
		}
		if node.depth >= 2 && node.goal == "" &&
			(node.status == enums.RoleStatusDone || node.status == enums.RoleStatusIdle) {
			continue
		}
		children = append(children, node)
	}

	if meta == nil && len(children) == 0 {
		body := m.styles.Dim.Render("(no agents)")
		return lipgloss.JoinVertical(lipgloss.Top, header, m.styles.PanelBox.Width(w-2).Height(h-3).Render(body))
	}

	// 网格布局参数：宽度足够时两列（更宽时三列），否则单列。
	cols := 1
	if innerW >= 72 {
		cols = 3
	} else if innerW >= 44 {
		cols = 2
	}
	gap := 2
	cardW := (innerW - gap*(cols-1)) / cols
	if cardW < 14 {
		cardW = 14
	}

	// 分段组装：MetaAgent 卡片（居中）、连接线、子 Agent 卡片行、图例。
	// 裁剪时优先丢图例、再丢末尾卡片行，保证 MetaAgent 始终可见。
	var metaLines []string
	if meta != nil {
		// MetaAgent 卡片只写名称（用户要求不带目标/角色等额外信息），宽度自适应内容。
		metaCard := m.buildMetaCard(*meta)
		padLeft := (innerW - lipgloss.Width(metaCard)) / 2
		if padLeft < 0 {
			padLeft = 0
		}
		pad := strings.Repeat(" ", padLeft)
		for _, l := range strings.Split(metaCard, "\n") {
			metaLines = append(metaLines, pad+l)
		}
	}
	// 子 Agent 卡片按行拼接（行内 JoinHorizontal）；放不下的整行丢弃。
	connLines := m.agentConnectorLines(cols, cardW, gap, innerW)
	rowBudget := maxBody - len(metaLines) - len(connLines)
	var rowLines []string
	omitted := 0
	for row := 0; row < len(children); row += cols {
		end := row + cols
		if end > len(children) {
			end = len(children)
		}
		var cards []string
		for _, n := range children[row:end] {
			cards = append(cards, m.buildAgentCard(n, cardW))
		}
		rl := strings.Split(joinHorizontalWithGap(cards, gap), "\n")
		if len(rowLines)+len(rl) > rowBudget {
			omitted = len(children) - row
			break
		}
		rowLines = append(rowLines, rl...)
	}
	// 一个卡片行也放不下时，不画悬空的连接线。
	if len(children) == 0 || (omitted == len(children) && len(children) > 0) {
		connLines = nil
	}
	lines := append(metaLines, connLines...)
	lines = append(lines, rowLines...)

	// 图例：仅在所有卡片都放下且还有余量时显示，并插入空行贴底对齐。
	if omitted == 0 && len(lines)+1 <= maxBody {
		for len(lines)+1 < maxBody {
			lines = append(lines, "")
		}
		lines = append(lines, m.agentLegendLine())
	} else if omitted > 0 && len(lines) < maxBody {
		lines = append(lines, m.styles.Dim.Render(fmt.Sprintf("… 还有 %d 个 Agent", omitted)))
	}
	// 兜底硬裁剪，防止极端高度下内容溢出面板挤乱整体布局。
	if len(lines) > maxBody {
		lines = lines[:maxBody]
	}

	body := strings.Join(lines, "\n")
	return lipgloss.JoinVertical(lipgloss.Top, header, m.styles.PanelBox.Width(w-2).Height(h-3).Render(body))
}

// buildMetaCard 渲染顶部 MetaAgent 卡片：只写名称，不带角色/目标/时间等额外信息。
// 边框颜色仍随状态变化（运行绿/错误红），宽度自适应内容，由调用方居中。
func (m Model) buildMetaCard(node agentTreeNode) string {
	name := lipgloss.NewStyle().Foreground(lipgloss.Color(agentRoleColor(enums.RoleTypeMeta))).Bold(true).
		Render("MetaAgent")
	// 运行中/错误用状态色边框突出，其余用普通暗色边框。
	borderColor := cBlur
	switch node.status {
	case enums.RoleStatusActive:
		borderColor = cStatusRun
	case enums.RoleStatusError:
		borderColor = cStatusErr
	}
	return lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(borderColor)).
		Padding(0, 2).
		Render(name)
}

// buildAgentCard 把子 Agent 节点渲染为紧凑的圆角边框卡片，固定 2 行内容：
// 彩色加粗名称（如"游戏渲染领域"）+ 状态色点文本（如"● Running"）。
// 任务/结果/时间等详情不再上卡片（简洁展示要求），仍可在 [A] 编排弹窗中查看。
// 卡片总宽恒为 cardW：lipgloss Width 含左右内边距（各 1），边框另加 2 列，保证网格列对齐。
func (m Model) buildAgentCard(node agentTreeNode, cardW int) string {
	// 文本区宽度 = 总宽 - 边框 2 - 内边距 2。
	inner := cardW - 4
	if inner < 4 {
		inner = 4
	}
	// 名称：按角色类型着色并加粗。
	name := lipgloss.NewStyle().Foreground(lipgloss.Color(agentRoleColor(node.roleType))).Bold(true).
		Render(truncate(node.name, inner))
	// 状态行：彩色图标 + 英文状态文本。
	stLine := lipgloss.NewStyle().Foreground(lipgloss.Color(statusColor(string(node.status)))).
		Render(statusIcon(string(node.status)) + " " + roleStatusText(node.status))
	// 运行中/错误的 Agent 用状态色边框突出，其余用普通暗色边框。
	borderColor := cBlur
	switch node.status {
	case enums.RoleStatusActive:
		borderColor = cStatusRun
	case enums.RoleStatusError:
		borderColor = cStatusErr
	}
	return lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(borderColor)).
		Padding(0, 1).
		Width(cardW - 2).
		Render(name + "\n" + stLine)
}

// agentConnectorLines 生成 MetaAgent 卡片与子 Agent 网格之间的连接线。
// 单列时为居中的竖线；多列时为带 ┌ ┐ ┬ ┴ 的分流横线，对齐各列中心。
func (m Model) agentConnectorLines(cols, cardW, gap, innerW int) []string {
	center := innerW / 2
	vline := strings.Repeat(" ", center) + m.styles.Dim.Render("│")
	if cols <= 1 {
		return []string{vline, vline}
	}
	// 计算各列中心位置。
	centers := make([]int, cols)
	for i := 0; i < cols; i++ {
		centers[i] = i*(cardW+gap) + cardW/2
	}
	bar := []rune(strings.Repeat(" ", centers[cols-1]+1))
	for i := centers[0]; i <= centers[cols-1]; i++ {
		bar[i] = '─'
	}
	mid := (centers[0] + centers[cols-1]) / 2
	bar[mid] = '┴'
	for i, c := range centers {
		switch {
		case c == mid:
			bar[c] = '┼'
		case i == 0:
			bar[c] = '┌'
		case i == cols-1:
			bar[c] = '┐'
		default:
			bar[c] = '┬'
		}
	}
	return []string{vline, m.styles.Dim.Render(string(bar))}
}

// agentLegendLine 渲染 Agent 编排面板底部的状态图例。
func (m Model) agentLegendLine() string {
	item := func(icon, label, color string) string {
		return lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render(icon) + " " + m.styles.Dim.Render(label)
	}
	return item("✓", "Done", cStatusDone) + "   " +
		item("●", "Running", cStatusRun) + "   " +
		item("◐", "Waiting", cStatusWait)
}

// joinHorizontalWithGap 以固定空格间隔水平拼接多个同高文本块。
func joinHorizontalWithGap(blocks []string, gap int) string {
	if len(blocks) == 1 {
		return blocks[0]
	}
	parts := make([]string, 0, len(blocks)*2-1)
	for i, b := range blocks {
		if i > 0 {
			parts = append(parts, strings.Repeat(" ", gap))
		}
		parts = append(parts, b)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}

// roleStatusText 把 Agent 状态映射为英文短文本，用于卡片状态行。
func roleStatusText(s enums.RoleStatus) string {
	switch s {
	case enums.RoleStatusActive:
		return "Running"
	case enums.RoleStatusDone:
		return "Done"
	case enums.RoleStatusWaiting:
		return "Waiting"
	case enums.RoleStatusError:
		return "Error"
	default:
		return "Idle"
	}
}

// agentTreePrefix 根据节点深度与兄弟关系生成树状连接符前缀（如 ├─ / └─ / │  ）。
func agentTreePrefix(nodes []agentTreeNode, idx int) string {
	// 索引越界时返回空。
	if idx < 0 || idx >= len(nodes) {
		return ""
	}
	depth := nodes[idx].depth
	// 顶层节点无前缀。
	if depth == 0 {
		return ""
	}

	var parts []string
	// 从第 1 层到当前深度逐层判断连接符。
	for d := 1; d <= depth; d++ {
		// 向上查找当前节点在第 d 层的祖先节点索引。
		ancestorIdx := -1
		for j := idx; j >= 0; j-- {
			if nodes[j].depth == d {
				ancestorIdx = j
				break
			}
		}
		// 若找不到祖先，用空格占位。
		if ancestorIdx == -1 {
			parts = append(parts, "   ")
			continue
		}
		// 判断该祖先是否是其父节点的最后一个子节点。
		isLast := true
		for j := ancestorIdx + 1; j < len(nodes); j++ {
			if nodes[j].depth < d {
				break
			}
			if nodes[j].depth == d {
				isLast = false
				break
			}
		}
		// 当前深度使用 ├─ / └─，上层使用 │  / 空格。
		if d == depth {
			if isLast {
				parts = append(parts, "└─ ")
			} else {
				parts = append(parts, "├─ ")
			}
		} else {
			if isLast {
				parts = append(parts, "   ")
			} else {
				parts = append(parts, "│  ")
			}
		}
	}
	return strings.Join(parts, "")
}

// agentRoleColor 返回不同 Agent 角色类型对应的主题色。
func agentRoleColor(roleType enums.RoleType) string {
	switch roleType {
	case enums.RoleTypeMeta:
		return cMeta
	case enums.RoleTypeDomain:
		return cDomain
	case enums.RoleTypeSubDomain:
		return cSub
	case enums.RoleTypeFixed, enums.RoleTypeDynamic:
		return cAssist
	default:
		return cInfo
	}
}

// renderAgentPanel 是旧的右侧 Agent 面板渲染器，保留以兼容旧调用点；主布局已使用 renderAgentsPanel。
func renderAgentPanel(m *Model, w int) string {
	s := m.selectedSession()
	if s == nil {
		return m.styles.BlurBorder.Width(w).Render(m.styles.Dim.Render("No active session"))
	}

	var lines []string
	lines = append(lines, m.styles.Header.Render("Agents"))

	if len(m.agentTreePanel.nodes) == 0 {
		lines = append(lines, m.styles.Dim.Render("  (no agents)"))
	} else {
		for _, node := range m.agentTreePanel.nodes {
			prefix := strings.Repeat("  ", node.depth)
			icon := statusIcon(string(node.status))
			name := node.name
			if node.goal != "" {
				name += " " + m.styles.Dim.Render(truncate(node.goal, w-12))
			}
			lines = append(lines, fmt.Sprintf("%s%s %s %s", prefix, icon, name, m.styles.Dim.Render("")))
		}
	}

	// 追加 Memory 区域：展示最近召回的记忆。
	lines = append(lines, "", m.styles.Header.Render("Memory"))
	recalls := recentMemoryRecalls(s, 3)
	if len(recalls) == 0 {
		lines = append(lines, m.styles.Dim.Render("  (none)"))
	} else {
		for _, r := range recalls {
			lines = append(lines, m.styles.Dim.Render("  • "+truncate(r, w-4)))
		}
	}

	// 追加 Tokens 区域：展示当前累计的输入/输出 Token 数与缓存命中率（TODO #40 可观测）。
	lines = append(lines, "", m.styles.Header.Render("Tokens"))
	cacheLine := fmt.Sprintf("  in:%d out:%d", m.totalInputTokens, m.totalOutputTokens)
	if hit, miss := m.totalCacheHit, m.totalCacheMiss; hit+miss > 0 {
		cacheLine += fmt.Sprintf("  缓存命中率 %.0f%%", float64(hit)/float64(hit+miss)*100)
	}
	lines = append(lines, cacheLine)

	content := strings.Join(lines, "\n")
	return m.styles.BlurBorder.Width(w).Height(m.height - 2).Render(content)
}
