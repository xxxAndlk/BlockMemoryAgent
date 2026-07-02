package tui

import (
	"strings"
	"sync"
	"time"
	"unicode"

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
	chatScrollLine   int // viewport top line (absolute)
	chatCursor       int
	chatFollowBottom bool

	// accumulated token counts from token_usage events
	totalInputTokens  int
	totalOutputTokens int

	// agents tree (built every tick)
	agentsNodes []agentTreeNode

	// v2.0 面板开关
	agentPanelVisible bool
	planBarVisible    bool

	// input bar
	inputMode    int
	inputRunes   []rune
	inputCursor  int
	inputHistory map[string][]string // sessionID -> 历史输入
	inputHistIdx int

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
		sessionMgr:        sessionMgr,
		registry:          registry,
		rt:                rt,
		dagHandler:        dagHandler,
		pgStore:           pgStore,
		httpAddr:          httpAddr,
		modelName:         modelName,
		styles:            NewStyles(),
		focus:             panelChat,
		chatFollowBottom:  true,
		planBarVisible:    true,
		inputHistIdx:      -1,
		inputHistory:      make(map[string][]string),
		flashMu:           &sync.Mutex{}, // 初始化 flash 互斥锁（T2 修复）
	}
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

func (m *Model) selectSession(idx int) {
	if idx < 0 || idx >= len(m.sessions) {
		return
	}
	m.sessionsCursor = idx
	m.chatCursor = 0
	m.chatScrollLine = 0
	m.chatFollowBottom = true
	m.rebuildAgents()
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
	m.agentsNodes = append(m.agentsNodes, agentTreeNode{
		depth:    0,
		instID:   "MetaAgent",
		name:     "MetaAgent",
		roleType: enums.RoleTypeMeta,
		status:   metaStatus,
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
		// chatFollowBottom alone triggers bottom-clamping in renderChat;
		// no sentinel needed.
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
		return m, nil
	}
	// Otherwise wheel scrolls the chat panel (wherever the pointer is).
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if m.chatFollowBottom {
			m.chatFollowBottom = false
			m.chatScrollLine = m.chatBottomLine()
		}
		m.chatScrollLine -= 3
		if m.chatScrollLine < 0 {
			m.chatScrollLine = 0
		}
	case tea.MouseButtonWheelDown:
		m.chatScrollLine += 3
		// Re-attach follow-bottom when scrolled past content end.
		if m.chatScrollLine >= m.chatBottomLine() {
			m.chatFollowBottom = true
		}
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Ctrl+L 全局开关"完整记录"面板：无论当前焦点在输入栏还是对话区、
	// 无论是否已打开其它弹窗，都可随时翻阅全部输出。这是查看长输出的主入口，
	// 绕开"发送后焦点锁在输入栏导致 j/k 无法滚动对话区"的问题。
	if msg.String() == "ctrl+l" {
		m.toggleLogPopup()
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
	case "1":
		// already on chat
	case "2":
		m.togglePlanPopup()
	case "3":
		m.toggleAgentsPopup()
	case "4":
		m.toggleLogPopup()
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
		m.moveChatCursor(1)
	case "k", "up":
		m.moveChatCursor(-1)
	case "pgup":
		m.scrollChat(-5)
	case "pgdown":
		m.scrollChat(5)
	case "g":
		m.moveChatCursor(-99999)
	case "G":
		m.moveChatCursor(99999)
	case "tab":
		m.agentPanelVisible = !m.agentPanelVisible
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

// scrollChat 按行数滚动对话区（PgUp/PgDn 用）。
func (m *Model) scrollChat(delta int) {
	s := m.selectedSession()
	if s == nil {
		return
	}
	items := chatItems(s)
	totalLines := 0
	for _, item := range items {
		totalLines += 1 + len(displayDetailLines(item.title, item.detail))
	}
	viewportH := m.height - 8
	if viewportH < 1 {
		viewportH = 1
	}
	m.chatScrollLine += delta
	maxScroll := totalLines - viewportH
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.chatScrollLine < 0 {
		m.chatScrollLine = 0
	}
	if m.chatScrollLine > maxScroll {
		m.chatScrollLine = maxScroll
		m.chatFollowBottom = true
	} else {
		m.chatFollowBottom = false
	}
}

func (m *Model) moveChatCursor(delta int) {
	s := m.selectedSession()
	if s == nil {
		return
	}
	items := chatItems(s)
	if len(items) == 0 {
		return
	}
	// Compute item start lines (same as renderChat viewport calc).
	itemStartLine := make([]int, len(items))
	totalLines := 0
	for i, item := range items {
		itemStartLine[i] = totalLines
		totalLines += 1 + len(displayDetailLines(item.title, item.detail))
	}
	// Find current item from chatScrollLine (or chatCursor if not scrolled).
	curItem := m.chatCursor
	if m.chatScrollLine > 0 {
		for i := len(items) - 1; i >= 0; i-- {
			if itemStartLine[i] <= m.chatScrollLine {
				curItem = i
				break
			}
		}
	}
	newItem := clamp(curItem+delta, 0, len(items)-1)
	m.chatCursor = newItem
	m.chatScrollLine = itemStartLine[newItem]
	if delta < 0 {
		m.chatFollowBottom = false
	}
	if m.chatCursor >= len(items)-1 {
		m.chatFollowBottom = true
	}
}

// chatBottomLine returns the scrollLine that places the last content line at
// the bottom of the chat viewport. Used by mouse-wheel to transition out of
// follow-bottom mode without jumping.
func (m *Model) chatBottomLine() int {
	s := m.selectedSession()
	if s == nil {
		return 0
	}
	items := chatItems(s)
	totalLines := 0
	for _, item := range items {
		totalLines += 1 + len(displayDetailLines(item.title, item.detail))
	}
	contentH := m.height - 8 // topH(3)+inputH(4)+tabsH(1)
	viewportH := contentH - 2
	if viewportH < 1 {
		viewportH = 1
	}
	bottom := totalLines - viewportH
	if bottom < 0 {
		bottom = 0
	}
	return bottom
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
