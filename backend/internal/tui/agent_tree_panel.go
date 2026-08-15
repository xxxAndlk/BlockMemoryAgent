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
	// parentID 是父节点实例 ID（orchestrator.Node.ParentID）：
	// depth=1 节点的父为会话根（rootID），MetaAgent 节点为空。
	parentID string
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
			parentID:  n.ParentID,
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

// renderAgentsPanel 渲染右侧 Agent 编排面板（TODO #48 真树形）：
// 顶部为居中的 MetaAgent 卡片，经连接线按实际分支数分叉引出领域卡片，
// 每个领域卡片下挂其派发的助手子分支（树状连接符），底部为状态图例。
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
	// 内容区最大行数（PanelBox Height(h-3)），超出部分按行裁剪。
	maxBody := h - 3
	if maxBody < 1 {
		maxBody = 1
	}

	// 真树形分组：meta 下按分支（depth=1 节点，通常是领域 Agent）数量动态分叉，
	// 每个分支下再挂其派发的助手子分支（TODO #48 子项 2）。
	meta, branches, _ := groupAgentTree(m.agentTreePanel.nodes)
	if meta == nil && len(branches) == 0 {
		body := m.styles.Dim.Render("(no agents)")
		return lipgloss.JoinVertical(lipgloss.Top, header, m.styles.PanelBox.Width(w-2).Height(h-3).Render(body))
	}

	// 无分支（仅 MetaAgent 或只有散节点）时回退旧网格布局。
	if len(branches) == 0 {
		return m.renderAgentsGridFallback(w, h, header, meta, maxBody, innerW)
	}

	// 分支列参数：宽度足够时每行最多 3 列（分三叉），否则 2 列/1 列（窄面板退化为竖向树）。
	rowCols := 3
	if innerW < 66 {
		rowCols = 2
	}
	if innerW < 40 {
		rowCols = 1
	}
	if rowCols > len(branches) {
		rowCols = len(branches)
	}
	gap := 2
	colW := (innerW - gap*(rowCols-1)) / rowCols
	if colW < 10 {
		colW = 10
	}

	// Meta 卡片：居中；运行中且有待完成分支时标注"谁在等谁"（TODO #48 子项 1）。
	var lines []string
	if meta != nil {
		// 等待标注按面板宽度截断，防窄面板下卡片超出内宽导致边框折行。
		metaCard := m.buildMetaCard(*meta, truncate(waitingBranchNames(branches), innerW-6))
		padLeft := (innerW - lipgloss.Width(metaCard)) / 2
		if padLeft < 0 {
			padLeft = 0
		}
		pad := strings.Repeat(" ", padLeft)
		for _, l := range strings.Split(metaCard, "\n") {
			lines = append(lines, pad+l)
		}
	}

	// 逐行组装分支：连接线 + 分支卡片行 + 各分支子节点行，整行粒度裁剪（行放不下整行丢弃）。
	omitted := 0
	firstRow := true
	for row := 0; row < len(branches); row += rowCols {
		end := row + rowCols
		if end > len(branches) {
			end = len(branches)
		}
		rowBranches := branches[row:end]
		centers := branchCenters(len(rowBranches), colW, gap, innerW)

		// 连接线：首行从 Meta 引出（1/2/3 分叉形态按实际分支数生成），
		// 后续行左缘竖线延续，表示仍是 Meta 的分支。
		var rowLines []string
		if firstRow && meta != nil {
			rowLines = append(rowLines, m.agentBranchConnectorLines(centers, innerW/2)...)
		} else {
			rowLines = append(rowLines, m.styles.Dim.Render("  │"))
		}
		// 分支卡片行：每个分支一张卡片（含等待标注/当前任务行）。
		cards := make([]string, 0, len(rowBranches))
		for _, b := range rowBranches {
			cards = append(cards, m.buildAgentCard(b.node, colW, waitingChildNames(b.children)))
		}
		rowLines = append(rowLines, strings.Split(joinHorizontalWithGap(cards, gap), "\n")...)
		// 各分支子节点行：树状连接符（├─ / └─），助手挂在所属分支下。
		nodesInRow := len(rowBranches)
		for _, b := range rowBranches {
			cl := m.branchChildLines(b, colW)
			rowLines = append(rowLines, cl...)
			nodesInRow += len(b.children)
		}

		if len(lines)+len(rowLines) > maxBody {
			omitted += nodesInRow
			break
		}
		lines = append(lines, rowLines...)
		firstRow = false
	}

	// 图例：全部放下且还有余量时显示并贴底对齐。
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

// agentTreeBranch 是编排树的一个直接分支：depth=1 节点（领域 Agent/直接助手）+ 其下挂载的子节点。
type agentTreeBranch struct {
	node     agentTreeNode
	children []agentTreeNode
}

// groupAgentTree 把扁平渲染节点按树结构分组为 meta + 直接分支（TODO #48 子项 2 数据源）。
// 子孙归属：沿 ParentID 链找到首个 depth=1 祖先；孤儿（找不到祖先）挂到最后一个分支兜底。
// 顺带应用拥挤过滤：depth>=2 且无目标且已终结/空闲的节点不参与渲染（与旧网格同口径）。
// 待澄清占位节点作为独立分支追加（挂在 meta 下）。
func groupAgentTree(nodes []agentTreeNode) (meta *agentTreeNode, branches []agentTreeBranch, loose []agentTreeNode) {
	byID := make(map[string]int, len(nodes))
	for i := range nodes {
		byID[nodes[i].instID] = i
	}
	level1Idx := make(map[string]int, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		if n.isClarify {
			continue
		}
		switch n.depth {
		case 0:
			if meta == nil {
				meta = n
			}
		case 1:
			level1Idx[n.instID] = len(branches)
			branches = append(branches, agentTreeBranch{node: *n})
		}
	}
	// 第二遍：depth>=2 节点沿父链挂到所属分支。
	for i := range nodes {
		n := &nodes[i]
		if n.depth < 2 {
			continue
		}
		if n.goal == "" && (n.status == enums.RoleStatusDone || n.status == enums.RoleStatusIdle) {
			continue
		}
		attached := false
		pid := n.parentID
		for range nodes {
			if pid == "" {
				break
			}
			idx, ok := byID[pid]
			if !ok {
				break
			}
			anc := &nodes[idx]
			if anc.depth == 1 {
				if bi, ok2 := level1Idx[anc.instID]; ok2 {
					branches[bi].children = append(branches[bi].children, *n)
					attached = true
				}
				break
			}
			pid = anc.parentID
		}
		if !attached {
			loose = append(loose, *n)
		}
	}
	// 待澄清占位作为独立分支。
	for i := range nodes {
		if nodes[i].isClarify {
			branches = append(branches, agentTreeBranch{node: nodes[i]})
		}
	}
	// 孤儿挂最后一个分支兜底，保证不丢节点。
	if len(loose) > 0 && len(branches) > 0 {
		branches[len(branches)-1].children = append(branches[len(branches)-1].children, loose...)
	}
	return meta, branches, loose
}

// waitingChildNames 返回分支下仍处于活动/等待状态的子节点展示名摘要（TODO #48 子项 1）：
// 1 个返回"⏳ 等待 X 完成"，2 个"⏳ 等待 X、Y 完成"，更多返回"⏳ 等待 N 个子 Agent 完成"。
func waitingChildNames(children []agentTreeNode) string {
	var names []string
	for _, c := range children {
		if c.status == enums.RoleStatusActive || c.status == enums.RoleStatusWaiting {
			names = append(names, c.name)
		}
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return "⏳ 等待 " + names[0] + " 完成"
	case 2:
		return "⏳ 等待 " + names[0] + "、" + names[1] + " 完成"
	default:
		return fmt.Sprintf("⏳ 等待 %d 个子 Agent 完成", len(names))
	}
}

// waitingBranchNames 返回 meta 正在等待的分支名摘要：分支自身处于活动/等待态即视为未回传。
func waitingBranchNames(branches []agentTreeBranch) string {
	children := make([]agentTreeNode, 0, len(branches))
	for _, b := range branches {
		children = append(children, b.node)
	}
	return waitingChildNames(children)
}

// branchCenters 计算一行内各分支卡片列的中心横坐标（行整体居中）。
func branchCenters(n, colW, gap, innerW int) []int {
	rowW := n*colW + (n-1)*gap
	start := (innerW - rowW) / 2
	if start < 0 {
		start = 0
	}
	centers := make([]int, n)
	for i := range n {
		centers[i] = start + i*(colW+gap) + colW/2
	}
	return centers
}

// renderAgentsGridFallback 回退旧网格布局：仅 MetaAgent（无分支派发）或散节点场景。
func (m Model) renderAgentsGridFallback(w, h int, header string, meta *agentTreeNode, maxBody, innerW int) string {
	var lines []string
	if meta != nil {
		metaCard := m.buildMetaCard(*meta, "")
		padLeft := (innerW - lipgloss.Width(metaCard)) / 2
		if padLeft < 0 {
			padLeft = 0
		}
		pad := strings.Repeat(" ", padLeft)
		for _, l := range strings.Split(metaCard, "\n") {
			lines = append(lines, pad+l)
		}
	}
	for len(lines)+1 < maxBody {
		lines = append(lines, "")
	}
	if len(lines) < maxBody {
		lines = append(lines, m.agentLegendLine())
	}
	body := strings.Join(lines, "\n")
	return lipgloss.JoinVertical(lipgloss.Top, header, m.styles.PanelBox.Width(w-2).Height(h-3).Render(body))
}

// buildMetaCard 渲染顶部 MetaAgent 卡片：名称 +（可选）等待标注行。
// 边框颜色仍随状态变化（运行绿/错误红），宽度自适应内容，由调用方居中。
func (m Model) buildMetaCard(node agentTreeNode, waiting string) string {
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
	// 等待标注：MetaAgent 空等子 Agent 时直接读出"谁在等谁"（TODO #48 子项 1）。
	if waiting != "" {
		name += "\n" + m.styles.Dim.Render(waiting)
	}
	return lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(borderColor)).
		Padding(0, 2).
		Render(name)
}

// buildAgentCard 把子 Agent 节点渲染为紧凑的圆角边框卡片：
// 彩色加粗名称（如"游戏渲染领域"）+ 状态色点文本（如"● Running"）；
// 运行中且有待完成子节点时追加"⏳ 等待 XX 完成"标注行（TODO #48 子项 1），
// 运行中无子节点时展示当前任务摘要（进度感，TODO #48 子项 3 验收 (d)）。
// 卡片总宽恒为 cardW：lipgloss Width 含左右内边距（各 1），边框另加 2 列，保证列对齐。
func (m Model) buildAgentCard(node agentTreeNode, cardW int, waiting string) string {
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
	// 附加行：等待标注优先，其次运行中展示当前任务（进度感）。
	extra := ""
	if waiting != "" {
		extra = m.styles.Dim.Render(truncate(waiting, inner))
	} else if node.status == enums.RoleStatusActive && node.goal != "" {
		extra = m.styles.Dim.Render(truncate("📋 "+node.goal, inner))
	}
	// 运行中/错误的 Agent 用状态色边框突出，其余用普通暗色边框。
	borderColor := cBlur
	switch node.status {
	case enums.RoleStatusActive:
		borderColor = cStatusRun
	case enums.RoleStatusError:
		borderColor = cStatusErr
	}
	content := name + "\n" + stLine
	if extra != "" {
		content += "\n" + extra
	}
	return lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(borderColor)).
		Padding(0, 1).
		Width(cardW - 2).
		Render(content)
}

// branchChildLines 渲染一个分支下的子节点行：树状连接符（├─ / └─）+ 名称 + 状态色点。
func (m Model) branchChildLines(b agentTreeBranch, colW int) []string {
	var lines []string
	for i, c := range b.children {
		glyph := "├─"
		if i == len(b.children)-1 {
			glyph = "└─"
		}
		name := lipgloss.NewStyle().Foreground(lipgloss.Color(agentRoleColor(c.roleType))).
			Render(truncate(c.name, colW-6))
		st := lipgloss.NewStyle().Foreground(lipgloss.Color(statusColor(string(c.status)))).
			Render(statusIcon(string(c.status)) + " " + roleStatusText(c.status))
		lines = append(lines, "  "+m.styles.Dim.Render(glyph)+" "+name+" "+st)
	}
	return lines
}

// agentBranchConnectorLines 生成 MetaAgent 卡片到分支卡片行之间的连接线（TODO #48 子项 2）：
// 竖干（meta 中心）→ 分流横线（按实际分支列中心生成：1 个分支一条竖线、2 个两叉、3 个三叉）→ 各分支竖线。
func (m Model) agentBranchConnectorLines(centers []int, metaCenter int) []string {
	// 单分支且与 meta 同轴：三条竖线（无横线分叉）。
	if len(centers) == 1 && centers[0] == metaCenter {
		v := strings.Repeat(" ", metaCenter) + m.styles.Dim.Render("│")
		return []string{v, v, v}
	}
	lo, hi := centers[0], centers[len(centers)-1]
	if metaCenter < lo {
		lo = metaCenter
	}
	if metaCenter > hi {
		hi = metaCenter
	}
	bar := []rune(strings.Repeat(" ", hi+1))
	for i := lo; i <= hi; i++ {
		bar[i] = '─'
	}
	bar[metaCenter] = '┴'
	for _, c := range centers {
		if c == metaCenter {
			bar[c] = '┼'
		} else {
			bar[c] = '┬'
		}
	}
	// 横线端点美化：未与连接点重合的端点画 ┌ ┐。
	if lo < hi {
		if lo != metaCenter {
			bar[lo] = '┌'
		}
		if hi != metaCenter {
			bar[hi] = '┐'
		}
	}
	stub := []rune(strings.Repeat(" ", hi+1))
	for _, c := range centers {
		stub[c] = '│'
	}
	return []string{
		strings.Repeat(" ", metaCenter) + m.styles.Dim.Render("│"),
		m.styles.Dim.Render(string(bar)),
		m.styles.Dim.Render(string(stub)),
	}
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
