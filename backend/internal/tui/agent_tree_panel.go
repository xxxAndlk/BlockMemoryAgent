package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/board"
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
	// status 是 Agent 当前状态。
	status enums.RoleStatus
	// goal 是 Agent 的目标描述，可能为空。
	goal string
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
	// 推导 MetaAgent 目标：优先用会话 Goal，否则取第一条用户消息内容。
	metaGoal := ""
	if s.Goal != "" {
		metaGoal = s.Goal
	} else if len(s.Messages) > 0 {
		for _, msg := range s.Messages {
			if msg.Role == enums.ChatRoleUser {
				metaGoal = strings.TrimSpace(msg.Content)
				break
			}
		}
	}
	// 添加顶层 MetaAgent 节点。
	at.nodes = append(at.nodes, agentTreeNode{
		depth:     0,
		instID:    "MetaAgent",
		name:      "MetaAgent",
		roleType:  enums.RoleTypeMeta,
		status:    metaStatus,
		goal:      metaGoal,
		createdAt: s.StartedAt,
	})

	// 获取会话的 Agent 实例列表。
	insts, err := agentFacade.ListAgents(context.Background(), s.ID)
	if err != nil {
		return
	}
	// 构建 ModuleID 到实例的映射，便于查找父子关系。
	byID := make(map[string]agent.AgentInstance)
	for _, inst := range insts {
		byID[inst.ModuleID] = inst
	}
	// 遍历实例，仅处理领域 Agent 作为一级子节点。
	for _, inst := range insts {
		if inst.RoleType != enums.RoleTypeDomain {
			continue
		}
		// 从会话状态中查找该领域当前的目标。
		goal := ""
		if s.State != nil {
			for _, b := range s.State.ActiveBlocks {
				if b.Domain == inst.Domain {
					goal = b.Goal
					break
				}
			}
		}
		// 添加领域 Agent 节点。
		at.nodes = append(at.nodes, agentTreeNode{
			depth:     1,
			instID:    inst.ModuleID,
			name:      inst.Name,
			domain:    inst.Domain,
			roleType:  inst.RoleType,
			status:    enums.RoleStatus(inst.Status),
			goal:      goal,
			createdAt: inst.CreatedAt,
		})
		// 处理领域 Agent 的子节点。
		for _, childID := range inst.Children {
			child := byID[childID]
			// 子节点不存在时跳过。
			if child.ModuleID == "" {
				continue
			}
			depth := 2
			// 子领域 Agent 继续展开其子节点。
			if child.RoleType == enums.RoleTypeSubDomain {
				at.nodes = append(at.nodes, agentTreeNode{
					depth:     depth,
					instID:    child.ModuleID,
					name:      child.Name,
					domain:    child.Domain,
					roleType:  child.RoleType,
					status:    enums.RoleStatus(child.Status),
					createdAt: child.CreatedAt,
				})
				for _, subID := range child.Children {
					sub := byID[subID]
					if sub.ModuleID == "" {
						continue
					}
					at.nodes = append(at.nodes, agentTreeNode{
						depth:     3,
						instID:    sub.ModuleID,
						name:      sub.Name,
						domain:    sub.Domain,
						roleType:  sub.RoleType,
						status:    enums.RoleStatus(sub.Status),
						createdAt: sub.CreatedAt,
					})
				}
			} else if child.RoleType == enums.RoleTypeFixed || child.RoleType == enums.RoleTypeDynamic {
				// 固定/动态 Agent 直接作为二级节点。
				at.nodes = append(at.nodes, agentTreeNode{
					depth:     depth,
					instID:    child.ModuleID,
					name:      child.Name,
					domain:    child.Domain,
					roleType:  child.RoleType,
					status:    enums.RoleStatus(child.Status),
					createdAt: child.CreatedAt,
				})
			}
		}
	}

	// 若会话处于待澄清状态，追加一个占位节点提示用户。
	if s.State != nil && s.State.PendingClarify != nil {
		at.nodes = append(at.nodes, agentTreeNode{
			depth:     1,
			instID:    "clarify",
			name:      "Clarify pending",
			roleType:  enums.RoleTypeMeta,
			status:    enums.RoleStatusWaiting,
			isClarify: true,
		})
	}
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

// renderAgentsPanel 渲染右侧 Agent 编排面板，包含标题栏与 Agent 卡片列表。
func (m Model) renderAgentsPanel(w, h int) string {
	// 保证最小宽度，避免卡片过度压缩。
	if w < 20 {
		w = 20
	}

	// 构建标题栏：左侧标题 + 右侧关闭提示，中间用空格填充。
	titleLeft := "Agent 编排"
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

	// 构建 Agent 卡片行。
	var lines []string
	if len(m.agentTreePanel.nodes) == 0 {
		lines = append(lines, "(no agents)")
	} else {
		for i, node := range m.agentTreePanel.nodes {
			// 过滤掉二级以下、无目标且已完成/空闲的节点，避免面板过于拥挤。
			if node.depth >= 2 && node.goal == "" &&
				(node.status == enums.RoleStatusDone || node.status == enums.RoleStatusIdle) {
				continue
			}
			card := m.agentCardLine(node, i, innerW)
			lines = append(lines, card...)
		}
	}

	// 拼接标题栏与内容区。
	body := strings.Join(lines, "\n")
	return lipgloss.JoinVertical(lipgloss.Top, header, m.styles.PanelBox.Width(w-2).Height(h-3).Render(body))
}

// agentCardLine 渲染单个 Agent 卡片，返回一行或两行字符串（第二行展示目标）。
func (m Model) agentCardLine(node agentTreeNode, idx, innerW int) []string {
	// 根据节点在树中的位置生成树状前缀并计算其显示宽度。
	prefix := agentTreePrefix(m.agentTreePanel.nodes, idx)
	prefixW := runewidth.StringWidth(prefix)
	// 可用宽度需扣除前缀占位。
	avail := innerW - prefixW
	if avail < 10 {
		avail = 10
	}

	// 状态图标与颜色。
	icon := statusIcon(string(node.status))
	iconStyled := lipgloss.NewStyle().Foreground(lipgloss.Color(statusColor(string(node.status)))).Render(icon)
	// 根据角色类型选择名称颜色并加粗。
	nameColor := agentRoleColor(node.roleType)
	nameStyled := lipgloss.NewStyle().Foreground(lipgloss.Color(nameColor)).Bold(true).Render(node.name)
	// 状态徽章。
	badge := agentStatusBadge(m.styles, node.status)

	// 创建时间戳，非零时显示在右侧。
	ts := ""
	if !node.createdAt.IsZero() {
		ts = node.createdAt.Format("15:04:05")
	}
	tsStyled := m.styles.Dim.Render(ts)

	// 拼接第一行：前缀 + 图标 + 名称 + 徽章。
	line := fmt.Sprintf("%s%s %s %s", prefix, iconStyled, nameStyled, badge)
	// 若时间戳非空，则右对齐放置。
	if ts != "" {
		gap := avail - lipgloss.Width(line) + prefixW - lipgloss.Width(tsStyled)
		if gap < 1 {
			gap = 1
		}
		line += strings.Repeat(" ", gap) + tsStyled
	}
	// 按可用宽度截断，防止溢出。
	line = truncate(line, innerW)

	lines := []string{line}

	// 若节点有目标且不是待澄清占位，则在第二行展示目标摘要。
	if node.goal != "" && !node.isClarify {
		goal := strings.ReplaceAll(node.goal, "\n", " ")
		goalPrefix := strings.Repeat(" ", prefixW) + "  "
		goalAvail := innerW - runewidth.StringWidth(goalPrefix)
		if goalAvail < 10 {
			goalAvail = 10
		}
		goalLine := goalPrefix + m.styles.Dim.Render(truncate(goal, goalAvail))
		lines = append(lines, goalLine)
	}

	return lines
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

// agentStatusBadge 将 Agent 状态渲染为短标签徽章，用于 Agent 编排栏。
func agentStatusBadge(styles *Styles, status enums.RoleStatus) string {
	switch status {
	case enums.RoleStatusActive:
		return styles.BadgeWarn.Render(" 运行 ")
	case enums.RoleStatusDone:
		return styles.BadgeOk.Render(" 完成 ")
	case enums.RoleStatusError:
		return styles.BadgeWarn.Render(" 错误 ")
	case enums.RoleStatusWaiting:
		return styles.Badge.Render(" 等待 ")
	default:
		return styles.Badge.Render(" 空闲 ")
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

	// 追加 Tokens 区域：展示当前累计的输入/输出 Token 数。
	lines = append(lines, "", m.styles.Header.Render("Tokens"))
	lines = append(lines, fmt.Sprintf("  in:%d out:%d", m.totalInputTokens, m.totalOutputTokens))

	content := strings.Join(lines, "\n")
	return m.styles.BlurBorder.Width(w).Height(m.height - 2).Render(content)
}
