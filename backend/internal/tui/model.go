package tui

import (
	"log"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/blockmemory/agent/backend/internal/graph"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// Model is the top-level bubbletea model for the BlockMemoryAgent TUI.
type Model struct {
	width  int
	height int

	sessionMgr *server.SessionManager
	registry   *graph.RoleRegistry
	rt         *runtime.Runtime
	dagHandler *server.DAGHandler
	pgStore    *store.PostgresStore
	httpAddr   string
	modelName  string

	styles *Styles

	focus   int // panelChat / panelInput
	overlay int

	sessions       []*server.Session
	sessionsCursor int

	// chat panel
	chatVP              viewport.Model
	chatCursor          int
	chatFollowBottom    bool
	chatLastItems       int    // 用于检测会话内容变化，决定是否重建 viewport content
	chatLastWidth       int    // 上次生成 content 时的宽度
	chatItemOffsets     []int  // 每个 chatItem 在 viewport content 中的起始行偏移
	pendingScrollToUser bool   // 发送消息后优先滚动到用户问题
	chatAnchorUser      bool   // 已锚定到用户问题，禁止自动跟随底部
	pendingFirstMessage string // 无会话时用户发送的首条消息，用于立即切换到对话视图并高亮展示

	// scrollbar drag state
	scrollbarDragging bool // 是否正在拖动聊天区滚动条滑块
	dragStartY        int  // 拖动开始时鼠标 Y 坐标
	dragStartOffset   int  // 拖动开始时 viewport YOffset

	// accumulated token counts from token_usage events
	totalInputTokens  int
	totalOutputTokens int

	// agents tree (built every tick)
	agentsNodes []agentTreeNode

	// v2.0 面板开关
	agentPanelVisible bool
	planBarVisible    bool

	// rightPanelForced 用户手动强制显示/隐藏右侧计划/Agent 分栏。
	// 0=自动（按宽度和内容），1=强制显示，-1=强制隐藏。
	rightPanelForced int

	// input bar
	inputMode    int
	inputRunes   []rune
	inputCursor  int
	inputHistory map[string][]string // sessionID -> 历史输入
	inputHistIdx int
	lastKeyTime  time.Time           // 上次按键时间，用于区分快速粘贴与手动回车

	// overlay
	overlayTitle  string
	overlayLines  []string
	overlayCursor int
	overlayKind   int // overlayPlan / overlayAgents / overlayDetail / overlayHelp

	// flash banner
	flash      string
	flashUntil time.Time
	flashMu    *sync.Mutex // 保护 flash/flashUntil 的并发读写（T2 修复：后台 HTTP goroutine 写，主循环 View 读）。用指针避免 bubbletea 值语义 Model 拷贝 Mutex

	// pendingSelectID 由后台 createSession goroutine 写入，tick handler 消费：
	// 成功创建会话后选中它需操作 m.sessions/cursor，不能在后台 goroutine 直接改
	// （与主循环 View 读产生 race），改为 tick 在主循环内执行 refresh+select。
	pendingSelectID string

	tickCount int
}

// agentTreeNode is one flattened row in the agent topology.
type agentTreeNode struct {
	depth     int
	instID    string
	name      string
	domain    string
	roleType  enums.RoleType
	status    enums.RoleStatus
	goal      string
	isClarify bool
}

// NewModel builds a TUI model wired to backend dependencies.
func NewModel(
	sessionMgr *server.SessionManager,
	registry *graph.RoleRegistry,
	rt *runtime.Runtime,
	dagHandler *server.DAGHandler,
	pgStore *store.PostgresStore,
	httpAddr string,
	modelName string,
) *Model {
	m := &Model{
		sessionMgr:       sessionMgr,
		registry:         registry,
		rt:               rt,
		dagHandler:       dagHandler,
		pgStore:          pgStore,
		httpAddr:         httpAddr,
		modelName:        modelName,
		styles:           NewStyles(),
		focus:            panelChat,
		chatFollowBottom: true,
		planBarVisible:   true,
		inputHistIdx:     -1,
		inputHistory:     make(map[string][]string),
		flashMu:          &sync.Mutex{}, // 初始化 flash 互斥锁（T2 修复）
		chatVP:           viewport.New(0, 0),
	}
	m.chatVP.SetContent("")
	m.refreshSessions()
	if len(m.sessions) > 0 {
		m.selectSession(0)
	} else {
		// No sessions — drop user into the input bar so they can /new one.
		m.focus = panelInput
		m.inputMode = inputNormal
	}
	return m
}

// Init starts background ticks.
func (m Model) Init() tea.Cmd {
	return tickCmd()
}

func tickCmd() tea.Cmd {
	// v2.0：100ms 快速 tick 保证对话区流畅；Agent 面板/顶栏等耗时操作每 10 tick（1s）刷新一次。
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg{} })
}

type tickMsg struct{}

// selectSession 仅在显式切换会话（NewModel 初始化 / pendingSelectID 自动选中）时调用。
// 不重置 pendingScrollToUser：第一条用户消息触发 createSession → tick 消费 pendingSelectID
// → selectSession，此时 pendingScrollToUser 仍需保留以便后续滚动到用户问题，
// 否则 GotoBottom 会把用户消息顶出视口（chatVP 内容含会话启动/agent_created 等后续事件）。
func (m *Model) selectSession(idx int) {
	if idx < 0 || idx >= len(m.sessions) {
		return
	}
	m.sessionsCursor = idx
	// 只有在确认服务端会话的 Messages 中已包含同一条首条用户消息时，
	// 才清除本地预展示；否则保留 pendingFirstMessage，由 buildChatContent
	// 继续展示，避免选中后首条消息"消失"的竞态错觉。
	if m.pendingFirstMessage != "" {
		s := m.selectedSession()
		found := false
		if s != nil {
			for _, msg := range s.Messages {
				if msg.Role == enums.ChatRoleUser && strings.TrimSpace(msg.Content) == m.pendingFirstMessage {
					found = true
					break
				}
			}
		}
		if found {
			m.pendingFirstMessage = ""
		} else {
			log.Printf("[tui] selectSession: session %s 尚未同步首条用户消息，保留本地预展示", m.sessions[idx].ID)
		}
	}
	m.chatCursor = 0
	m.chatFollowBottom = true
	m.chatAnchorUser = false
	m.chatLastItems = 0
	m.chatLastWidth = 0
	m.rebuildAgents()
	m.rebuildChatContent()
	// 内容未撑满视口时回到顶部，确保首条用户消息/欢迎信息可见；
	// 内容超出视口时才滚到底部看最新消息。
	if m.chatVP.TotalLineCount() <= m.chatVP.VisibleLineCount() {
		m.chatVP.GotoTop()
	} else {
		m.chatVP.GotoBottom()
	}
}

func (m *Model) refreshSessions() {
	if m.sessionMgr == nil {
		return
	}
	prevID := ""
	if m.sessionsCursor >= 0 && m.sessionsCursor < len(m.sessions) {
		prevID = m.sessions[m.sessionsCursor].ID
	}
	// TUI only shows sessions created during this TUI run. Historical sessions
	// are kept for Agent internal retrieval; they are not surfaced here.
	m.sessions = m.sessionMgr.ListSessions()
	found := -1
	for i, s := range m.sessions {
		if s.ID == prevID {
			found = i
			break
		}
	}
	if found >= 0 {
		m.sessionsCursor = found
	} else if m.sessionsCursor >= len(m.sessions) && len(m.sessions) > 0 {
		m.sessionsCursor = len(m.sessions) - 1
		m.rebuildAgents()
	}
}

func (m *Model) rebuildAgents() {
	m.agentsNodes = nil
	s := m.selectedSession()
	if s == nil {
		return
	}

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
	m.agentsNodes = append(m.agentsNodes, agentTreeNode{
		depth:    0,
		instID:   "MetaAgent",
		name:     "MetaAgent",
		roleType: enums.RoleTypeMeta,
		status:   metaStatus,
		goal:     metaGoal,
	})

	insts := m.registry.GetInstancesBySession(s.ID)
	byID := make(map[string]*types.RoleInstance)
	for _, inst := range insts {
		byID[inst.ID] = inst
	}
	for _, inst := range insts {
		if inst.Type != enums.RoleTypeDomain {
			continue
		}
		goal := ""
		if s.State != nil {
			for _, b := range s.State.ActiveBlocks {
				if b.Domain == inst.Domain {
					goal = b.Goal
					break
				}
			}
		}
		m.agentsNodes = append(m.agentsNodes, agentTreeNode{
			depth:    1,
			instID:   inst.ID,
			name:     instName(m.registry, inst),
			domain:   inst.Domain,
			roleType: inst.Type,
			status:   inst.Status,
			goal:     goal,
		})
		for _, childID := range inst.Children {
			child := byID[childID]
			if child == nil {
				continue
			}
			depth := 2
			if child.Type == enums.RoleTypeSubDomain {
				m.agentsNodes = append(m.agentsNodes, agentTreeNode{
					depth:    depth,
					instID:   child.ID,
					name:     instName(m.registry, child),
					domain:   child.Domain,
					roleType: child.Type,
					status:   child.Status,
				})
				for _, subID := range child.Children {
					sub := byID[subID]
					if sub == nil {
						continue
					}
					m.agentsNodes = append(m.agentsNodes, agentTreeNode{
						depth:    3,
						instID:   sub.ID,
						name:     instName(m.registry, sub),
						domain:   sub.Domain,
						roleType: sub.Type,
						status:   sub.Status,
					})
				}
			} else if child.Type == enums.RoleTypeFixed || child.Type == enums.RoleTypeDynamic {
				m.agentsNodes = append(m.agentsNodes, agentTreeNode{
					depth:    depth,
					instID:   child.ID,
					name:     instName(m.registry, child),
					domain:   child.Domain,
					roleType: child.Type,
					status:   child.Status,
				})
			}
		}
	}

	if s.State != nil && s.State.PendingClarify != nil {
		m.agentsNodes = append(m.agentsNodes, agentTreeNode{
			depth:     1,
			instID:    "clarify",
			name:      "Clarify pending",
			roleType:  enums.RoleTypeMeta,
			status:    enums.RoleStatusWaiting,
			isClarify: true,
		})
	}
}

func instName(r *graph.RoleRegistry, inst *types.RoleInstance) string {
	if inst == nil {
		return "unknown"
	}
	if inst.Type == enums.RoleTypeDomain || inst.Type == enums.RoleTypeSubDomain {
		if inst.Domain != "" {
			return inst.Domain
		}
	}
	if def := r.GetRoleDef(inst.RoleDefID); def != nil {
		return def.Name
	}
	return inst.RoleDefID
}

func (m *Model) selectedSession() *server.Session {
	if m.sessionsCursor < 0 || m.sessionsCursor >= len(m.sessions) {
		return nil
	}
	// 测试或降级场景：无 SessionManager 时直接返回本地 sessions 的浅拷贝。
	if m.sessionMgr == nil {
		s := *m.sessions[m.sessionsCursor]
		return &s
	}
	// 返回持锁深拷贝（T1 修复：原直接返回 *Session 指针，TUI 在 tea 主 goroutine
	// 无锁读 Events/Messages/State，与后台 runSession/addEventDebug 的并发写产生
	// data race，事件量大时可能 slice 迭代越界 panic 或读取半更新 State 指针）。
	return m.sessionMgr.SnapshotSession(m.sessions[m.sessionsCursor].ID)
}

// Update handles messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.chatVP.Width = m.chatContentWidth()
		m.chatVP.Height = m.mainContentHeight()
		m.rebuildChatContent()
		if m.chatFollowBottom {
			m.chatVP.GotoBottom()
		}

	case tickMsg:
		m.tickCount++
		// 每 5 tick（0.5s）刷新会话列表；每 10 tick（1s）刷新 Agent 面板与 Token 统计。
		if m.tickCount%5 == 0 {
			m.refreshSessions()
		}
		if m.tickCount%10 == 0 {
			m.rebuildAgents()
			m.accumulateTokens()
		}
		// Auto-scroll to bottom when following.
		// 清理过期闪屏提示（持锁，T2 修复）
		m.flashMu.Lock()
		if m.flash != "" && time.Now().After(m.flashUntil) {
			m.flash = ""
		}
		m.flashMu.Unlock()
		// 消费后台 createSession 写入的 pendingSelectID：在主循环内 refresh+select
		// 避免后台 goroutine 直接改 m.sessions/cursor 与 View 产生 race（T2 修复）
		if m.pendingSelectID != "" {
			id := m.pendingSelectID
			m.pendingSelectID = ""
			m.refreshSessions()
			for i, s := range m.sessions {
				if s.ID == id {
					m.selectSession(i)
					break
				}
			}
		}
		// 发送消息后，优先滚动到最后一条用户问题，确保用户能看到自己的输入。
		// 该逻辑必须排在内容重建/自动跟随底部之前，防止新内容把用户问题顶出视口。
		if m.pendingScrollToUser {
			items := m.collectChatItems()
			for idx := len(items) - 1; idx >= 0; idx-- {
				if strings.HasPrefix(items[idx].title, "> ") {
					m.rebuildChatContent()
					// 把用户问题底部对齐视口底部，保留上方历史可见；
					// 同时锚定，让后续流式输出在下方展开而不把用户问题顶走。
					m.chatScrollToItemBottom(idx)
					m.chatFollowBottom = false
					m.chatAnchorUser = true
					m.pendingScrollToUser = false
					// 找到真实用户消息后，若其内容与本地预展示一致，清除预展示标记
					if m.pendingFirstMessage != "" && strings.TrimPrefix(items[idx].title, "> ") == m.pendingFirstMessage {
						m.pendingFirstMessage = ""
					}
					break
				}
			}
			// 未找到用户消息时保留 pendingScrollToUser，等待服务端写入或本地兜底展示后再试
		}
		// viewport 内容随会话事件/消息增长而重建；跟随底部时自动滚到最新。
		// 锚定到用户问题时仅重建内容、不自动滚动，避免用户问题被顶出视口。
		if s := m.selectedSession(); s != nil {
			itemsChanged := len(chatItems(s, true)) != m.chatLastItems
			widthChanged := m.chatContentWidth() != m.chatLastWidth
			if itemsChanged || widthChanged {
				wasAtBottom := m.chatVP.AtBottom() || m.chatFollowBottom
				m.rebuildChatContent()
				if !m.chatAnchorUser && wasAtBottom {
					m.chatVP.GotoBottom()
					m.chatFollowBottom = true
				}
			}
		}
		if !m.chatAnchorUser && m.chatFollowBottom {
			m.chatVP.GotoBottom()
		}
		// 弹窗打开时刷新动态内容（完整记录面板在末尾时跟随新输出）
		if m.overlay != overlayNone {
			m.refreshOverlay()
		}
		return m, tickCmd()

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

func (m *Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// When popup is open, wheel scrolls popup contents.
	if m.overlay != overlayNone && m.overlay != overlayHelp {
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			if m.overlayCursor > 0 {
				m.overlayCursor--
			}
		case tea.MouseButtonWheelDown:
			m.overlayCursor++
		}
		m.clampOverlayCursor()
		return m, nil
	}

	// 滚轮始终交给 viewport 处理。
	if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown {
		var cmd tea.Cmd
		m.chatVP, cmd = m.chatVP.Update(msg)
		m.chatFollowBottom = m.chatVP.AtBottom()
		m.chatAnchorUser = false
		return m, cmd
	}

	// 滚动条拖动处理。
	sx, sy, sw, sh := m.scrollbarArea()
	inScrollbar := msg.X >= sx && msg.X < sx+sw && msg.Y >= sy && msg.Y < sy+sh

	if m.scrollbarDragging {
		// 拖动过程中：根据鼠标 Y 位移实时更新 viewport offset。
		// 释放事件（Release）也走这里，先更新位置再结束拖动。
		m.updateScrollbarDrag(msg.Y)
		if msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionRelease {
			m.scrollbarDragging = false
		}
		return m, nil
	}

	if msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress && inScrollbar {
		thumbStart, thumbEnd := m.scrollbarThumbBounds()
		relY := msg.Y - sy
		if thumbStart >= 0 && relY >= thumbStart && relY <= thumbEnd {
			// 点击滑块：开始拖动。
			m.scrollbarDragging = true
			m.dragStartY = msg.Y
			m.dragStartOffset = m.chatVP.YOffset
			return m, nil
		}
		// 点击轨道但不在滑块上：跳转（以滑块中心对齐鼠标位置）。
		thumbH := thumbEnd - thumbStart + 1
		m.scrollToThumbY(relY - thumbH/2)
		return m, nil
	}

	// 默认交给 viewport 处理内容区点击等。
	var cmd tea.Cmd
	m.chatVP, cmd = m.chatVP.Update(msg)
	m.chatFollowBottom = m.chatVP.AtBottom()
	m.chatAnchorUser = false
	return m, cmd
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Ctrl+L 全局开关"完整记录"面板：无论当前焦点在输入栏还是对话区、
	// 无论是否已打开其它弹窗，都可随时翻阅全部输出。这是查看长输出的主入口，
	// 绕开"发送后焦点锁在输入栏导致 j/k 无法滚动对话区"的问题。
	if msg.String() == "ctrl+l" {
		m.toggleLogPopup()
		return m, nil
	}
	// Ctrl+B 全局切换右侧计划/Agent 分栏显示/隐藏。
	if msg.String() == "ctrl+b" {
		m.toggleRightPanel()
		return m, nil
	}

	// Overlay mode: navigate or close.
	if m.overlay != overlayNone {
		// Keep popup content / cursor in sync with live state.
		m.refreshOverlay()
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc", "q":
			m.overlay = overlayNone
			return m, nil
		case "j", "down":
			m.overlayCursor++
		case "k", "up":
			if m.overlayCursor > 0 {
				m.overlayCursor--
			}
		case "g":
			m.overlayCursor = 0
		case "G":
			m.overlayCursor = len(m.overlayLines) - 1
		case "enter":
			m.handleOverlayEnter()
		case "2":
			m.togglePlanPopup()
		case "3":
			m.toggleAgentsPopup()
		case "4":
			m.toggleLogPopup()
		case "1":
			m.overlay = overlayNone
		}
		m.clampOverlayCursor()
		return m, nil
	}

	// Input bar: route all keys to input handler.
	if m.focus == panelInput {
		return m.handleInputKey(msg)
	}

	// Chat navigation mode.
	switch msg.String() {
	case "ctrl+c", "q", "Q":
		return m, tea.Quit
	case "1", "esc":
		m.overlay = overlayNone
	case "2", "p":
		m.togglePlanPopup()
	case "3", "a":
		m.toggleAgentsPopup()
	case "4", "l":
		m.toggleLogPopup()
	case "m":
		m.flashMsg("Memory panel: not implemented in TUI")
	case "g":
		m.flashMsg("Git Diff: not implemented in TUI")
	case "s":
		m.flashMsg("Settings: not implemented in TUI")
	case "K":
		m.focus = panelInput
		m.inputMode = inputNormal
	case "/":
		// Focus the input bar empty — for slash commands. Other printable chars
		// fall through to the default case and seed the buffer directly.
		m.focus = panelInput
		m.inputMode = inputNormal
	case "?":
		m.openHelpPopup()
	case "enter":
		m.showChatDetail()
	case "j", "down":
		m.chatItemDown()
	case "k", "up":
		m.chatItemUp()
	case "pgup":
		m.chatVP.HalfViewUp()
		m.chatFollowBottom = m.chatVP.AtBottom()
		m.chatAnchorUser = false
	case "pgdown":
		m.chatVP.HalfViewDown()
		m.chatFollowBottom = m.chatVP.AtBottom()
		m.chatAnchorUser = false
	case "home":
		m.chatGotoTop()
	case "end":
		m.chatGotoBottom()
	default:
		// Any printable rune jumps to input mode and seeds the buffer.
		if len(msg.Runes) > 0 && unicode.IsPrint(msg.Runes[0]) {
			m.focus = panelInput
			m.inputMode = inputNormal
			m.inputRunes = append([]rune{}, msg.Runes...)
			m.inputCursor = len(m.inputRunes)
		}
	}
	return m, nil
}

// chatCurrentItem 返回当前 viewport 顶部对应的 chatItem 索引。
func (m *Model) chatCurrentItem() int {
	if len(m.chatItemOffsets) == 0 {
		return 0
	}
	offset := m.chatVP.YOffset
	idx := 0
	for i := len(m.chatItemOffsets) - 1; i >= 0; i-- {
		if m.chatItemOffsets[i] <= offset {
			idx = i
			break
		}
	}
	return idx
}

// chatScrollToItem 滚动到指定 item 顶部，并更新 followBottom 状态。
func (m *Model) chatScrollToItem(idx int) {
	s := m.selectedSession()
	if s == nil || len(m.chatItemOffsets) == 0 {
		return
	}
	idx = clamp(idx, 0, len(m.chatItemOffsets)-1)
	m.chatVP.SetYOffset(m.chatItemOffsets[idx])
	m.chatFollowBottom = idx == len(m.chatItemOffsets)-1
}

// chatScrollToItemBottom 滚动到指定 item 完全可见，用于把刚发送的用户问题
// 固定在屏幕内。若 item 高度不超过视口高度，则让 item 顶部对齐视口顶部，
// 避免短消息被后续内容顶出视口；若 item 高于视口，则底部对齐以便看最新部分，
// 同时保留上方历史记录可见。
func (m *Model) chatScrollToItemBottom(idx int) {
	s := m.selectedSession()
	if s == nil || len(m.chatItemOffsets) == 0 {
		return
	}
	idx = clamp(idx, 0, len(m.chatItemOffsets)-1)
	startOffset := m.chatItemOffsets[idx]
	var endOffset int
	if idx+1 < len(m.chatItemOffsets) {
		endOffset = m.chatItemOffsets[idx+1]
	} else {
		endOffset = m.chatVP.TotalLineCount()
	}
	visible := m.chatVP.VisibleLineCount()
	itemH := endOffset - startOffset
	var target int
	if itemH <= visible {
		// 短消息优先完整展示在视口顶部，防止顶部被后续内容遮挡
		target = startOffset
	} else {
		// item 高度超过视口高度时，底部对齐以便看最新部分
		target = endOffset - visible
	}
	if target < 0 {
		target = 0
	}
	maxOffset := m.chatVP.TotalLineCount() - visible
	if maxOffset < 0 {
		maxOffset = 0
	}
	if target > maxOffset {
		target = maxOffset
	}
	m.chatVP.SetYOffset(target)
	m.chatFollowBottom = target >= maxOffset
}

func (m *Model) chatItemUp() {
	m.chatScrollToItem(m.chatCurrentItem() - 1)
	m.chatAnchorUser = false
}
func (m *Model) chatItemDown() {
	m.chatScrollToItem(m.chatCurrentItem() + 1)
	m.chatAnchorUser = false
}

func (m *Model) chatGotoTop() {
	m.chatScrollToItem(0)
	m.chatAnchorUser = false
}

func (m *Model) chatGotoBottom() {
	n := len(m.chatItemOffsets)
	if n == 0 {
		return
	}
	m.chatScrollToItem(n - 1)
	m.chatVP.GotoBottom()
	m.chatAnchorUser = false
}

// rebuildChatContent 根据当前窗口尺寸把当前会话内容渲染成带样式的字符串，
// 并同步到 viewport。ScrollToBottom 由调用方按需执行。
func (m *Model) rebuildChatContent() {
	items := m.collectChatItems()
	if len(items) == 0 {
		m.chatVP.SetContent("")
		m.chatLastItems = 0
		return
	}
	w := m.chatContentWidth()
	if w < 4 {
		w = 4
	}
	content := m.buildChatContent(w)
	m.chatVP.SetContent(content)
	m.chatLastItems = len(items)
	m.chatLastWidth = w
}

// rightPanelVisible 返回是否显示右侧计划/Agent 分栏。
// 显示条件（满足其一即可）：
//   - 用户手动强制显示（ctrl+b）
//   - 有活动会话且终端宽度≥80（参考设计：右侧面板为会话视图的固定组成部分）
func (m *Model) rightPanelVisible() bool {
	if m.rightPanelForced == 1 {
		return true
	}
	if m.rightPanelForced == -1 {
		return false
	}
	if m.selectedSession() == nil || m.width < 80 {
		return false
	}
	return true
}

// chatAreaWidth 返回左侧对话区总宽度（含滚动条与间隔）。
func (m *Model) chatAreaWidth() int {
	if !m.rightPanelVisible() {
		return m.width
	}
	w := m.width * 65 / 100
	if w < 50 {
		w = 50
	}
	return w
}

// rightPanelWidth 返回右侧面板可用宽度。
func (m *Model) rightPanelWidth() int {
	return m.width - m.chatAreaWidth()
}

// chatContentWidth 返回 viewport 内文本可用宽度（已扣除滚动条与间隔）。
func (m *Model) chatContentWidth() int {
	const scrollbarW = 1
	gap := 1
	w := m.chatAreaWidth() - scrollbarW - gap
	if w < 20 {
		w = 20
	}
	return w
}

// mainContentHeight 返回中间主内容区高度（已扣除顶栏、输入栏、底部快捷键栏、弹窗占位）。
func (m *Model) mainContentHeight() int {
	topH := 1
	inputH := 3
	shortcutH := 1
	overlayH := 0
	if m.overlay != overlayNone {
		overlayH = m.height / 3
		if overlayH < 6 {
			overlayH = 6
		}
	}
	h := m.height - topH - inputH - shortcutH - overlayH
	if h < 4 {
		h = 4
	}
	return h
}

func (m *Model) handleOverlayEnter() {
	switch m.overlay {
	case overlayPlan:
		m.showPlanDetailByIndex(m.overlayCursor)
	case overlayAgents:
		m.showAgentDetailByIndex(m.overlayCursor)
	}
}

// refreshOverlay rebuilds popup lines for plan/agents so live state stays in sync.
func (m *Model) refreshOverlay() {
	switch m.overlay {
	case overlayPlan:
		m.overlayLines = m.buildPlanLines()
	case overlayAgents:
		m.overlayLines = m.buildAgentsLines()
	case overlayLog:
		// 实时刷新完整记录：若用户当前停在末尾（跟读最新输出），新行加入后自动跟随到尾；
		// 用户已上滚浏览历史时不打断其位置。
		wasAtEnd := len(m.overlayLines) > 0 && m.overlayCursor >= len(m.overlayLines)-1
		m.overlayLines = m.buildTranscriptLines()
		if wasAtEnd {
			m.overlayCursor = len(m.overlayLines) - 1
		}
	case overlayHelp:
		m.overlayLines = strings.Split(strings.Trim(fullHelpText, "\n"), "\n")
	}
}

func (m *Model) clampOverlayCursor() {
	if len(m.overlayLines) == 0 {
		m.overlayCursor = 0
		return
	}
	if m.overlayCursor < 0 {
		m.overlayCursor = 0
	}
	if m.overlayCursor >= len(m.overlayLines) {
		m.overlayCursor = len(m.overlayLines) - 1
	}
}

func (m *Model) openHelpPopup() {
	m.overlay = overlayHelp
	m.overlayKind = overlayHelp
	m.overlayTitle = "Help"
	m.overlayLines = strings.Split(strings.Trim(fullHelpText, "\n"), "\n")
	m.overlayCursor = 0
}

func (m *Model) togglePlanPopup() {
	if !m.hasPlan() {
		m.flashMsg("no plan available")
		return
	}
	if m.overlay == overlayPlan {
		m.overlay = overlayNone
		return
	}
	lines := m.buildPlanLines()
	m.overlay = overlayPlan
	m.overlayKind = overlayPlan
	m.overlayTitle = "Execution Plan"
	m.overlayLines = lines
	m.overlayCursor = clamp(m.overlayCursor, 0, len(lines)-1)
}

func (m *Model) toggleAgentsPopup() {
	if len(m.agentsNodes) == 0 {
		m.flashMsg("no agents available")
		return
	}
	if m.overlay == overlayAgents {
		m.overlay = overlayNone
		return
	}
	lines := m.buildAgentsLines()
	m.overlay = overlayAgents
	m.overlayKind = overlayAgents
	m.overlayTitle = "Agent Topology"
	m.overlayLines = lines
	m.overlayCursor = clamp(m.overlayCursor, 0, len(lines)-1)
}

// toggleRightPanel 切换右侧计划/Agent 分栏的强制显示/隐藏状态。
// 循环：自动 → 强制显示 → 强制隐藏 → 自动。
func (m *Model) toggleRightPanel() {
	switch m.rightPanelForced {
	case 0:
		m.rightPanelForced = 1
		m.flashMsg("right panel: forced visible")
	case 1:
		m.rightPanelForced = -1
		m.flashMsg("right panel: forced hidden")
	default:
		m.rightPanelForced = 0
		m.flashMsg("right panel: auto")
	}
}

// toggleLogPopup 打开/关闭"完整记录"面板：把整段对话铺成可滚动行列表，
// 用户可用 j/k/g/G 翻阅全部 LLM 输出 / 工具调用 / 思考，不受对话区高度限制。
// 内容每次渲染实时刷新（refreshOverlay），保证新输出立即可见。
func (m *Model) toggleLogPopup() {
	s := m.selectedSession()
	if s == nil {
		m.flashMsg("no active session")
		return
	}
	if m.overlay == overlayLog {
		m.overlay = overlayNone
		return
	}
	lines := m.buildTranscriptLines()
	// 打开时默认滚到末尾，方便先看最新输出；用户可 g 回到顶部
	cursor := len(lines) - 1
	if cursor < 0 {
		cursor = 0
	}
	m.overlay = overlayLog
	m.overlayKind = overlayLog
	m.overlayTitle = "Full Transcript"
	m.overlayLines = lines
	m.overlayCursor = cursor
}

func (m *Model) openOverlay(title string, lines []string) {
	m.overlay = overlayDetail
	m.overlayKind = overlayDetail
	m.overlayTitle = title
	m.overlayLines = lines
	m.overlayCursor = 0
}

// accumulateTokens sums token_usage events and updates total counts.
func (m *Model) accumulateTokens() {
	s := m.selectedSession()
	if s == nil {
		return
	}
	in, out := 0, 0
	for _, ev := range s.Events {
		in += ev.InputTokens
		out += ev.OutputTokens
	}
	m.totalInputTokens = in
	m.totalOutputTokens = out
}

func clamp(v, lo, hi int) int {
	if lo > hi {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// scrollbarArea 返回聊天区滚动条在屏幕上的范围（x, y, w, h）。
// 顶栏占 1 行，滚动条位于对话区最右侧，宽度 1。
func (m *Model) scrollbarArea() (x, y, w, h int) {
	x = m.chatAreaWidth() - 2 // 扣除 gap(1) + scrollbar 宽度(1)
	if x < 0 {
		x = 0
	}
	y = 1 // 顶栏占 1 行
	w = 1
	h = m.mainContentHeight()
	return
}

// scrollbarThumbBounds 返回滑块在滚动条区域内的起始/结束行索引（含）。
// 若内容无需滚动则返回 (-1, -1)。
func (m *Model) scrollbarThumbBounds() (start, end int) {
	totalLines := m.chatVP.TotalLineCount()
	viewportH := m.chatVP.VisibleLineCount()
	h := m.mainContentHeight()
	if totalLines <= viewportH || viewportH <= 0 || h < 1 {
		return -1, -1
	}
	scrollable := totalLines - viewportH
	if scrollable < 1 {
		scrollable = 1
	}
	thumbH := h * viewportH / totalLines
	if thumbH < 1 {
		thumbH = 1
	}
	if thumbH > h {
		thumbH = h
	}
	thumbPos := m.chatVP.YOffset * (h - thumbH) / scrollable
	if thumbPos < 0 {
		thumbPos = 0
	}
	if thumbPos+thumbH > h {
		thumbPos = h - thumbH
	}
	return thumbPos, thumbPos + thumbH - 1
}

// updateScrollbarDrag 根据当前鼠标 Y 坐标更新 viewport 滚动位置。
// 以 dragStartY/dragStartOffset 为基准，按滑块可移动范围与内容可滚动范围的比率映射。
func (m *Model) updateScrollbarDrag(mouseY int) {
	totalLines := m.chatVP.TotalLineCount()
	viewportH := m.chatVP.VisibleLineCount()
	h := m.mainContentHeight()
	if totalLines <= viewportH || viewportH <= 0 || h < 1 {
		return
	}
	scrollable := totalLines - viewportH
	thumbH := h * viewportH / totalLines
	if thumbH < 1 {
		thumbH = 1
	}
	if thumbH > h {
		thumbH = h
	}
	maxThumbTravel := h - thumbH
	if maxThumbTravel < 1 {
		maxThumbTravel = 1
	}
	deltaY := mouseY - m.dragStartY
	deltaOffset := deltaY * scrollable / maxThumbTravel
	newOffset := m.dragStartOffset + deltaOffset
	if newOffset < 0 {
		newOffset = 0
	}
	if newOffset > scrollable {
		newOffset = scrollable
	}
	m.chatVP.YOffset = newOffset
	m.chatFollowBottom = m.chatVP.AtBottom()
	m.chatAnchorUser = false
}

// scrollToThumbY 将滑块中心对齐到滚动条区域内的指定 Y 坐标（相对于滚动条顶部）。
func (m *Model) scrollToThumbY(thumbCenterY int) {
	totalLines := m.chatVP.TotalLineCount()
	viewportH := m.chatVP.VisibleLineCount()
	h := m.mainContentHeight()
	if totalLines <= viewportH || viewportH <= 0 || h < 1 {
		return
	}
	scrollable := totalLines - viewportH
	thumbH := h * viewportH / totalLines
	if thumbH < 1 {
		thumbH = 1
	}
	if thumbH > h {
		thumbH = h
	}
	maxPos := h - thumbH
	if maxPos < 1 {
		maxPos = 1
	}
	pos := thumbCenterY
	if pos < 0 {
		pos = 0
	}
	if pos > maxPos {
		pos = maxPos
	}
	m.chatVP.YOffset = pos * scrollable / maxPos
	if m.chatVP.YOffset > scrollable {
		m.chatVP.YOffset = scrollable
	}
	m.chatFollowBottom = m.chatVP.AtBottom()
	m.chatAnchorUser = false
}
