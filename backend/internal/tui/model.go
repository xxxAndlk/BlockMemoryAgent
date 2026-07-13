package tui

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// Model is the top-level bubbletea model for the BlockMemoryAgent TUI.
type Model struct {
	width  int
	height int

	agent      agent.Agent
	dagHandler *server.DAGHandler
	httpAddr   string
	modelName  string

	styles *Styles

	focus int // panelChat / panelInput

	sessions       []*server.Session
	sessionsCursor int

	chatPanel      ChatPanel
	inputBar       InputBar
	overlayPanel   OverlayPanel
	agentTreePanel AgentTreePanel
	taskBriefCache TaskBriefCache

	// accumulated token counts from token_usage events
	totalInputTokens  int
	totalOutputTokens int

	// v2.0 面板开关
	agentPanelVisible bool
	planBarVisible    bool

	// rightPanelForced 用户手动强制显示/隐藏右侧计划/Agent 分栏。
	// 0=自动（按宽度和内容），1=强制显示，-1=强制隐藏。
	rightPanelForced int

	// flash banner
	flash      string
	flashUntil time.Time
	flashMu    *sync.Mutex // 保护 flash/flashUntil 的并发读写（T2 修复：后台 HTTP goroutine 写，主循环 View 读）。用指针避免 bubbletea 值语义 Model 拷贝 Mutex

	// pendingSelectID 由后台 createSession goroutine 写入，tick handler 消费：
	// 成功创建会话后选中它需操作 m.sessions/cursor，不能在后台 goroutine 直接改
	// （与主循环 View 读产生 race），改为 tick 在主循环内执行 refresh+select。
	pendingSelectID string

	// streamEvents 接收当前选中会话的 agent.Stream 事件，用于触发即时刷新。
	streamEvents chan agent.Event
	// streamCancel 关闭当前会话的事件流 goroutine。
	streamCancel context.CancelFunc

	tickCount int
}

// NewModel builds a TUI model wired to backend dependencies.
func NewModel(
	agentFacade agent.Agent,
	dagHandler *server.DAGHandler,
	httpAddr string,
	modelName string,
) *Model {
	m := &Model{
		agent:          agentFacade,
		dagHandler:     dagHandler,
		httpAddr:       httpAddr,
		modelName:      modelName,
		styles:         NewStyles(),
		focus:          panelChat,
		chatPanel:      NewChatPanel(),
		planBarVisible: true,
		inputBar:       NewInputBar(),
		flashMu:        &sync.Mutex{}, // 初始化 flash 互斥锁（T2 修复）
		taskBriefCache: NewTaskBriefCache(),
		streamEvents:   make(chan agent.Event, 16),
	}
	m.refreshSessions()
	if len(m.sessions) > 0 {
		m.selectSession(0)
	} else {
		// No sessions — drop user into the input bar so they can /new one.
		m.focus = panelInput
		m.inputBar.mode = inputNormal
	}
	return m
}

// Init starts background ticks and the agent stream listener.
func (m Model) Init() tea.Cmd {
	return tea.Batch(tickCmd(), streamCmd(m.streamEvents))
}

func tickCmd() tea.Cmd {
	// v2.0：100ms 快速 tick 保证对话区流畅；Agent 面板/顶栏等耗时操作每 10 tick（1s）刷新一次。
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg{} })
}

type tickMsg struct{}

// streamEventMsg is emitted when the agent.Stream channel for the selected
// session delivers a new event. It triggers the same refresh path as tickMsg
// so the TUI stays in sync with live graph output.
type streamEventMsg struct{ event agent.Event }

// streamCmd returns a bubbletea Cmd that waits for the next event on the
// shared streamEvents channel. The goroutine feeding the channel is restarted
// whenever the selected session changes.
func streamCmd(ch <-chan agent.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return streamEventMsg{event: ev}
	}
}

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
	if m.chatPanel.pendingFirstMessage != "" {
		s := m.selectedSession()
		found := false
		if s != nil {
			for _, msg := range s.Messages {
				if msg.Role == enums.ChatRoleUser && strings.TrimSpace(msg.Content) == m.chatPanel.pendingFirstMessage {
					found = true
					break
				}
			}
		}
		if found {
			m.chatPanel.pendingFirstMessage = ""
		} else {
			log.Printf("[tui] selectSession: session %s 尚未同步首条用户消息，保留本地预展示", m.sessions[idx].ID)
		}
	}
	m.chatPanel.cursor = 0
	m.chatPanel.followBottom = true
	m.chatPanel.anchorUser = false
	m.chatPanel.lastItems = 0
	m.chatPanel.lastWidth = 0
	m.rebuildAgents()
	m.startStream()
	m.rebuildChatContent()
	// 内容未撑满视口时回到顶部，确保首条用户消息/欢迎信息可见；
	// 内容超出视口时才滚到底部看最新消息。
	if m.chatPanel.vp.TotalLineCount() <= m.chatPanel.vp.VisibleLineCount() {
		m.chatPanel.vp.GotoTop()
	} else {
		m.chatPanel.vp.GotoBottom()
	}
}

// startStream cancels any previous agent.Stream goroutine for this model and
// starts a new one that follows the currently selected session. Events are
// pushed into streamEvents so the bubbletea message loop can refresh the view.
func (m *Model) startStream() {
	if m.streamCancel != nil {
		m.streamCancel()
	}
	if m.agent == nil || m.sessionsCursor < 0 || m.sessionsCursor >= len(m.sessions) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.streamCancel = cancel
	sessionID := m.sessions[m.sessionsCursor].ID
	go func() {
		ch, err := m.agent.Stream(ctx, sessionID)
		if err != nil {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				select {
				case m.streamEvents <- ev:
				case <-ctx.Done():
					return
				default:
					// Buffer full: drop the event; the next tick will refresh via
					// agent.Get anyway.
				}
			}
		}
	}()
}

func (m *Model) rebuildAgents() {
	m.agentTreePanel.rebuild(m.agent, m.selectedSession())
}

func (m *Model) refreshSessions() {
	if m.agent == nil {
		return
	}
	prevID := ""
	if m.sessionsCursor >= 0 && m.sessionsCursor < len(m.sessions) {
		prevID = m.sessions[m.sessionsCursor].ID
	}
	// List all sessions from the agent facade (including restored history) and
	// keep the cursor on the previously selected session when possible.
	sessions, err := m.agent.List(context.Background(), agent.Filter{})
	if err != nil {
		log.Printf("[tui] refreshSessions: %v", err)
		return
	}
	m.sessions = make([]*server.Session, 0, len(sessions))
	for _, s := range sessions {
		m.sessions = append(m.sessions, toServerSession(s))
	}
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

// toServerSession converts an agent.Session DTO back to the server.Session type
// that the TUI still uses internally for chat rendering and plan panels.
// It delegates to the canonical conversion in the server package to avoid drift.
func toServerSession(a *agent.Session) *server.Session {
	return server.ToServerSession(a)
}

func (m *Model) selectedSession() *server.Session {
	if m.sessionsCursor < 0 || m.sessionsCursor >= len(m.sessions) {
		return nil
	}
	// 测试或降级场景：无 agent facade 时直接返回本地 sessions 的浅拷贝。
	if m.agent == nil {
		s := *m.sessions[m.sessionsCursor]
		return &s
	}
	// 通过 agent.Agent facade 获取会话快照，避免直接依赖 SessionManager 内部方法。
	sess, err := m.agent.Get(context.Background(), m.sessions[m.sessionsCursor].ID)
	if err != nil {
		log.Printf("[tui] selectedSession: %v", err)
		// 降级：返回本地缓存的会话。
		s := *m.sessions[m.sessionsCursor]
		return &s
	}
	return toServerSession(sess)
}

// Update handles messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.chatPanel.vp.Width = m.chatContentWidth()
		m.chatPanel.vp.Height = m.mainContentHeight()
		m.rebuildChatContent()
		if m.chatPanel.followBottom {
			m.chatPanel.vp.GotoBottom()
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
		m.refreshView()
		return m, tickCmd()

	case streamEventMsg:
		m.refreshView()
		return m, streamCmd(m.streamEvents)

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

// refreshView contains the view-refresh logic shared by tickMsg and
// streamEventMsg: pending session selection, scroll-to-user, chat content
// rebuild, and overlay refresh.
func (m *Model) refreshView() {
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
	if m.chatPanel.pendingScrollToUser {
		items := m.collectChatItems()
		for idx := len(items) - 1; idx >= 0; idx-- {
			if strings.HasPrefix(items[idx].title, "> ") {
				m.rebuildChatContent()
				// 把用户问题底部对齐视口底部，保留上方历史可见；
				// 同时锚定，让后续流式输出在下方展开而不把用户问题顶走。
				m.chatPanel.scrollToItemBottom(idx, m.chatPanel.vp.TotalLineCount(), m.chatPanel.vp.VisibleLineCount())
				m.chatPanel.followBottom = false
				m.chatPanel.anchorUser = true
				m.chatPanel.pendingScrollToUser = false
				// 找到真实用户消息后，若其内容与本地预展示一致，清除预展示标记
				if m.chatPanel.pendingFirstMessage != "" && strings.TrimPrefix(items[idx].title, "> ") == m.chatPanel.pendingFirstMessage {
					m.chatPanel.pendingFirstMessage = ""
				}
				break
			}
		}
		// 未找到用户消息时保留 pendingScrollToUser，等待服务端写入或本地兜底展示后再试
	}
	// viewport 内容随会话事件/消息增长而重建；跟随底部时自动滚到最新。
	// 锚定到用户问题时仅重建内容、不自动滚动，避免用户问题被顶出视口。
	if s := m.selectedSession(); s != nil {
		itemsChanged := len(chatItems(s, true)) != m.chatPanel.lastItems
		widthChanged := m.chatContentWidth() != m.chatPanel.lastWidth
		if itemsChanged || widthChanged {
			wasAtBottom := m.chatPanel.vp.AtBottom() || m.chatPanel.followBottom
			m.rebuildChatContent()
			if !m.chatPanel.anchorUser && wasAtBottom {
				m.chatPanel.vp.GotoBottom()
				m.chatPanel.followBottom = true
			}
		}
	}
	if !m.chatPanel.anchorUser && m.chatPanel.followBottom {
		m.chatPanel.vp.GotoBottom()
	}
	// 弹窗打开时刷新动态内容（完整记录面板在末尾时跟随新输出）
	if m.overlayPanel.mode != overlayNone {
		m.refreshOverlay()
	}
}

func (m *Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// When popup is open, wheel scrolls popup contents.
	if m.overlayPanel.mode != overlayNone && m.overlayPanel.mode != overlayHelp {
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			if m.overlayPanel.cursor > 0 {
				m.overlayPanel.cursor--
			}
		case tea.MouseButtonWheelDown:
			m.overlayPanel.cursor++
		}
		m.clampOverlayCursor()
		return m, nil
	}

	// 滚轮始终交给 viewport 处理。
	if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown {
		var cmd tea.Cmd
		m.chatPanel.vp, cmd = m.chatPanel.vp.Update(msg)
		m.chatPanel.followBottom = m.chatPanel.vp.AtBottom()
		m.chatPanel.anchorUser = false
		return m, cmd
	}

	// 滚动条拖动处理。
	sx, sy, sw, sh := m.chatPanel.scrollbarArea(m.chatAreaWidth(), m.mainContentHeight())
	inScrollbar := msg.X >= sx && msg.X < sx+sw && msg.Y >= sy && msg.Y < sy+sh

	if m.chatPanel.scrollbarDragging {
		// 拖动过程中：根据鼠标 Y 位移实时更新 viewport offset。
		// 释放事件（Release）也走这里，先更新位置再结束拖动。
		m.chatPanel.updateDrag(msg.Y, m.mainContentHeight())
		if msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionRelease {
			m.chatPanel.scrollbarDragging = false
		}
		return m, nil
	}

	if msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress && inScrollbar {
		thumbStart, thumbEnd := m.chatPanel.thumbBounds(m.mainContentHeight())
		relY := msg.Y - sy
		if thumbStart >= 0 && relY >= thumbStart && relY <= thumbEnd {
			// 点击滑块：开始拖动。
			m.chatPanel.scrollbarDragging = true
			m.chatPanel.dragStartY = msg.Y
			m.chatPanel.dragStartOffset = m.chatPanel.vp.YOffset
			return m, nil
		}
		// 点击轨道但不在滑块上：跳转（以滑块中心对齐鼠标位置）。
		thumbH := thumbEnd - thumbStart + 1
		m.chatPanel.scrollToThumbY(relY - thumbH/2, m.mainContentHeight())
		return m, nil
	}

	// 默认交给 viewport 处理内容区点击等。
	var cmd tea.Cmd
	m.chatPanel.vp, cmd = m.chatPanel.vp.Update(msg)
	m.chatPanel.followBottom = m.chatPanel.vp.AtBottom()
	m.chatPanel.anchorUser = false
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
	if m.overlayPanel.mode != overlayNone {
		// Keep popup content / cursor in sync with live state.
		m.refreshOverlay()
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc", "q":
			m.overlayPanel.mode = overlayNone
			return m, nil
		case "j", "down":
			m.overlayPanel.cursor++
		case "k", "up":
			if m.overlayPanel.cursor > 0 {
				m.overlayPanel.cursor--
			}
		case "g":
			m.overlayPanel.cursor = 0
		case "G":
			m.overlayPanel.cursor = len(m.overlayPanel.lines) - 1
		case "enter":
			m.handleOverlayEnter()
		case "2":
			m.togglePlanPopup()
		case "3":
			m.toggleAgentsPopup()
		case "4":
			m.toggleLogPopup()
		case "1":
			m.overlayPanel.mode = overlayNone
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
		m.overlayPanel.mode = overlayNone
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
		m.inputBar.mode = inputNormal
	case "/":
		// Focus the input bar empty — for slash commands. Other printable chars
		// fall through to the default case and seed the buffer directly.
		m.focus = panelInput
		m.inputBar.mode = inputNormal
	case "?":
		m.openHelpPopup()
	case "enter":
		m.showChatDetail()
	case "j", "down":
		m.chatPanel.itemDown()
	case "k", "up":
		m.chatPanel.itemUp()
	case "pgup":
		m.chatPanel.vp.HalfViewUp()
		m.chatPanel.followBottom = m.chatPanel.vp.AtBottom()
		m.chatPanel.anchorUser = false
	case "pgdown":
		m.chatPanel.vp.HalfViewDown()
		m.chatPanel.followBottom = m.chatPanel.vp.AtBottom()
		m.chatPanel.anchorUser = false
	case "home":
		m.chatPanel.gotoTop()
	case "end":
		m.chatPanel.gotoBottom()
	default:
		// Any printable rune jumps to input mode and seeds the buffer.
		if len(msg.Runes) > 0 && unicode.IsPrint(msg.Runes[0]) {
			m.focus = panelInput
			m.inputBar.mode = inputNormal
			m.inputBar.runes = append([]rune{}, msg.Runes...)
			m.inputBar.cursor = len(m.inputBar.runes)
		}
	}
	return m, nil
}

// chatCurrentItem 返回当前 viewport 顶部对应的 chatItem 索引。
// chatScrollToItem 滚动到指定 item 顶部，并更新 followBottom 状态。
// chatScrollToItemBottom 滚动到指定 item 完全可见，用于把刚发送的用户问题
// 固定在屏幕内。若 item 高度不超过视口高度，则让 item 顶部对齐视口顶部，
// 避免短消息被后续内容顶出视口；若 item 高于视口，则底部对齐以便看最新部分，
// 同时保留上方历史记录可见。
// rebuildChatContent 根据当前窗口尺寸把当前会话内容渲染成带样式的字符串，
// 并同步到 viewport。ScrollToBottom 由调用方按需执行。
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
	if m.overlayPanel.mode != overlayNone {
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

// refreshOverlay rebuilds popup lines for plan/agents so live state stays in sync.
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
// scrollbarThumbBounds 返回滑块在滚动条区域内的起始/结束行索引（含）。
// 若内容无需滚动则返回 (-1, -1)。
// updateScrollbarDrag 根据当前鼠标 Y 坐标更新 viewport 滚动位置。
// 以 dragStartY/dragStartOffset 为基准，按滑块可移动范围与内容可滚动范围的比率映射。
// scrollToThumbY 将滑块中心对齐到滚动条区域内的指定 Y 坐标（相对于滚动条顶部）。
