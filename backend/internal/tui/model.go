package tui

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	term "github.com/charmbracelet/x/term"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/logger"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// Model 是 BlockMemoryAgent TUI 的顶层 bubbletea 模型，持有全部状态与依赖。
type Model struct {
	// width 与 height 是当前终端尺寸。
	width  int
	height int

	// agent 是后端 Agent facade，用于会话管理、事件流等。
	agent agent.Agent
	// dagHandler 处理 DAG 相关请求（预留）。
	dagHandler *server.DAGHandler
	// httpAddr 是本地 TUI 后端地址，postJSON/getJSON 使用。
	httpAddr string
	// modelName 是当前使用的模型名称，用于欢迎页与顶栏展示。
	modelName string
	// workDir 是进程工作目录，用于欢迎页与顶栏 Workspace 展示（替代原硬编码 "~/demo"）。
	workDir string

	// styles 是全局样式集合。
	styles *Styles

	// focus 是当前焦点面板，取值 panelChat 或 panelInput。
	focus int

	// sessions 是会话列表。
	sessions []*server.Session
	// sessionsCursor 是当前选中会话的索引；-1 表示无选中（启动初始状态，
	// 避免零值 0 隐式指向第一个恢复的历史会话而被当作记忆渲染出来）。
	sessionsCursor int

	// chatPanel 是对话面板。
	chatPanel ChatPanel
	// inputBar 是底部输入栏。
	inputBar InputBar
	// overlayPanel 是弹窗状态。
	overlayPanel OverlayPanel
	// agentTreePanel 是 Agent 编排面板状态。
	agentTreePanel AgentTreePanel
	// taskBriefCache 是任务标题摘要缓存。
	taskBriefCache TaskBriefCache

	// totalInputTokens 与 totalOutputTokens 分别累计输入/输出 Token 数量。
	totalInputTokens  int
	totalOutputTokens int
	// totalCacheHit / totalCacheMiss 累计缓存命中/未命中 token（TODO #40 可观测），
	// 命中率 = hit/(hit+miss) 展示在 Token 栏。
	totalCacheHit  int
	totalCacheMiss int

	// v2.0 面板开关
	// agentPanelVisible 与 planBarVisible 控制旧版面板显示（部分已弃用）。
	agentPanelVisible bool
	planBarVisible    bool

	// rightPanelForced 用户手动强制显示/隐藏右侧计划/Agent 分栏。
	// 0=自动（按宽度和内容），1=强制显示，-1=强制隐藏。
	rightPanelForced int

	// planScroll 是右侧计划面板任务列表的滚动偏移（行）：
	// 滚轮悬停在计划面板区域时增减（handleMouse），渲染时按此偏移开窗（formatPlanSnapshot）。
	planScroll int
	// agentScroll 是右侧 Agent 编排面板树内容的滚动偏移（行）：
	// 滚轮悬停在编排面板区域时增减（handleMouse），渲染时按此偏移开窗（renderAgentsPanel）。
	agentScroll int

	// startedAt 是 TUI 启动时间，用于计划面板底部"总计时"展示（TUI 开启至今的累计时长）。
	startedAt time.Time

	// shared 是跨 bubbletea 值拷贝共享的可变状态（#47 修复），见 sharedState。
	shared *sharedState

	// quitArmedUntil 是 Ctrl+C 退出确认的武装截止时间：
	// 仍有运行中会话时，首次按 Ctrl+C 只提示，3 秒内再按才真正退出（防误杀长任务）。
	quitArmedUntil time.Time
	// stopArmedUntil 是软停止（TODO #37）双击 ESC 的武装截止时间：
	// 当前会话 Running 时首次 ESC 进入 2s 窗（不清空输入栏），窗内再按 → POST /stop。
	stopArmedUntil time.Time
	// tokenWarnLevel 记录已提醒过的输入 Token 成本预警档位（每 50 万为一档）。
	tokenWarnLevel int

	// clarifySel 是澄清多选模式下已选选项 ID 集合（TODO #53），
	// 数字键 1-9 / 空格切换选择，Enter 提交时 join "," 作为答复；
	// 会话离开 awaiting_clarify 后由 syncInputMode 清空。
	clarifySel []string
	// clarifyCursor 是问答面板中当前高亮的选项下标（↑/↓ 移动，回车提交）。
	clarifyCursor int
	// clarifyID 记录当前已进入澄清模式的 PendingClarify.ID，
	// 同一 session 多轮 ask_user 时据此重置 clarifyCursor/clarifySel。
	clarifyID string

	// streamEvents 接收当前选中会话的 agent.Stream 事件，用于触发即时刷新。
	streamEvents chan agent.Event
	// streamCancel 关闭当前会话的事件流 goroutine。
	streamCancel context.CancelFunc

	// tickCount 记录 tick 次数，用于按周期执行不同刷新任务。
	tickCount int

	// dirty 标记有待上屏的流式变更：streamEventMsg 只置脏而不立即 refreshView，
	// 由 tickMsg 按 100ms 粒度合并刷新，避免每个流式 delta 都全量重算重绘造成卡顿。
	dirty bool

	// log 是结构化日志器，由 SetLogger 注入；nil 时回退标准库 log。
	log *logger.Logger

	// 模型切换弹窗状态（overlayModel 两段式）：
	// modelStage 0=选角色 1=选预设；modelSelRole 是 stage1 的目标角色；
	// modelSwitching 标记异步切换进行中（探测最长 60s，期间忽略 Enter）。
	modelStage     int
	modelSelRole   string
	modelSwitching bool
	modelRoles     []agent.RoleModelStatus
	modelPresets   []types.ModelPreset
}

// sharedState 是跨 bubbletea 值拷贝共享的可变状态（#47 修复）。
// Model 采用值语义：每次 Update 返回一份拷贝，后台 goroutine（createSession/postJSON）
// 若直接写 Model 字段，写入会落到已废弃的副本上，主循环永远看不到——T2 的互斥锁
// 只消除了数据竞争，没解决值拷贝导致的写入丢失。因此把「后台 goroutine 写、主循环读」
// 的字段集中到该指针共享结构体内：NewModel 构造一次，之后所有拷贝共享同一实例。
type sharedState struct {
	mu sync.Mutex
	// flash 是临时闪屏提示文本，flashUntil 是其过期时间。
	flash      string
	flashUntil time.Time
	// pendingSelectID 由后台 createSession goroutine 写入，tick handler 消费：
	// 成功创建会话后选中它需操作 m.sessions/cursor，不能在后台 goroutine 直接改
	// （与主循环 View 读产生 race），改为 tick 在主循环内执行 refresh+select。
	pendingSelectID string
}

func newSharedState() *sharedState { return &sharedState{} }

// setFlash 设置一条 2 秒后过期的闪屏提示。
func (s *sharedState) setFlash(msg string) {
	s.mu.Lock()
	s.flash = msg
	s.flashUntil = time.Now().Add(2 * time.Second)
	s.mu.Unlock()
}

// getFlash 读取未过期的闪屏提示；过期或为空返回 ""。nil 接收者安全（测试字面量未初始化时）。
func (s *sharedState) getFlash() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.flash == "" || time.Now().After(s.flashUntil) {
		return ""
	}
	return s.flash
}

func (s *sharedState) setPendingSelect(id string) {
	s.mu.Lock()
	s.pendingSelectID = id
	s.mu.Unlock()
}

// takePendingSelect 取出并清空待选中会话 ID；无待处理项返回 ""。nil 接收者安全。
func (s *sharedState) takePendingSelect() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.pendingSelectID
	s.pendingSelectID = ""
	return id
}

// hasPendingSelect 仅检查是否有待选中会话（不消费）。nil 接收者安全。
func (s *sharedState) hasPendingSelect() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingSelectID != ""
}

// ensureShared 惰性补齐 shared（测试中以 Model 字面量构造时可能未初始化）。
func (m *Model) ensureShared() *sharedState {
	if m.shared == nil {
		m.shared = newSharedState()
	}
	return m.shared
}

// NewModel 构造一个 TUI Model，连接后端依赖，初始化默认状态并加载会话列表。
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
		workDir:        currentWorkDir(),
		styles:         NewStyles(),
		focus:          panelChat,
		sessionsCursor: -1, // 启动不选中任何会话：聊天区保持空白新会话状态
		chatPanel:      NewChatPanel(),
		planBarVisible: true,
		inputBar:       NewInputBar(),
		shared:         newSharedState(), // 跨值拷贝共享的可变状态（#47 修复）
		taskBriefCache: NewTaskBriefCache(),
		streamEvents:   make(chan agent.Event, 16),
		startedAt:      time.Now(), // TUI 启动时间：计划面板底部"总计时"的计时起点
	}
	m.refreshSessions()
	// 启动时不自动选中任何历史会话：保持空白新会话状态（无选中会话），
	// 避免旧会话内容被当作"记忆"加载；用户直接输入即创建全新会话。
	// 历史会话仍列在侧边栏，可手动切换查看。
	m.focus = panelInput
	m.inputBar.mode = inputNormal
	return m
}

// currentWorkDir 返回进程当前工作目录，用于顶栏与欢迎页的 Workspace 展示。
// 获取失败时回退 "."，保证展示始终有值。
func currentWorkDir() string {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return "."
	}
	return wd
}

// Init 启动后台 tick、终端尺寸轮询（Windows 补偿，见 sizePollCmd）与 agent 事件流监听器。
func (m Model) Init() tea.Cmd {
	return tea.Batch(tickCmd(), sizePollCmd(), streamCmd(m.streamEvents))
}

// SetLogger 注入结构化日志器，使后端交互错误以 [ERRO] 级别输出。
// 参数 l：已初始化的 Logger 指针；未注入时回退标准库 log（级别固定 INFO）。
func (m *Model) SetLogger(l *logger.Logger) {
	m.log = l
}

// logInfo 记录信息类日志；未注入 logger 时回退标准库 log，保持旧行为。
func (m *Model) logInfo(msg string) {
	if m.log != nil {
		m.log.Info(context.Background(), msg)
		return
	}
	log.Print(msg)
}

// logError 记录错误类日志；未注入 logger 时回退标准库 log，保持旧行为。
func (m *Model) logError(msg string, err error) {
	if m.log != nil {
		m.log.Error(context.Background(), msg, err)
		return
	}
	log.Printf("%s: %v", msg, err)
}

// tickCmd 返回每 100ms 触发一次的 tick 命令。
// v2.0：100ms 快速 tick 保证对话区流畅；Agent 面板/顶栏等耗时操作每 10 tick（1s）刷新一次。
func tickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg{} })
}

// tickMsg 是 tick 命令产生的消息类型。
type tickMsg struct{}

// sizePollCmd 每秒查询一次真实终端尺寸，产出 sizePollMsg。
// Windows 平台没有 SIGWINCH，bubbletea v1 的 listenForResize 是空实现，
// 启动后窗口最大化/还原/全屏切换都不会再上报 WindowSizeMsg，模型只能按
// 启动时的尺寸渲染：帧比物理屏幕高时整帧上滚、顶栏被顶出屏幕。
// 这里以轮询补偿：尺寸变化时合成 WindowSizeMsg 走正常 resize 路径
// （该消息同时喂给渲染器，渲染器的裁剪宽度同步更新）。
// 非 TTY（测试/CI）下 GetSize 报错，返回 nil 消息，不产生任何影响。
func sizePollCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg {
		w, h, err := term.GetSize(os.Stdout.Fd())
		if err != nil || w <= 0 || h <= 0 {
			return nil
		}
		return sizePollMsg{width: w, height: h}
	})
}

// sizePollMsg 是 sizePollCmd 产生的尺寸轮询结果。
type sizePollMsg struct {
	width  int
	height int
}

// reconcileSize 对比轮询尺寸与当前缓存尺寸：一致返回 nil；不一致时返回一个
// 产出合成 WindowSizeMsg 的命令，使模型与渲染器经同一 resize 路径更新。
func (m *Model) reconcileSize(w, h int) tea.Cmd {
	if w == m.width && h == m.height {
		return nil
	}
	return func() tea.Msg { return tea.WindowSizeMsg{Width: w, Height: h} }
}

// streamEventMsg 在选中会话的 agent.Stream 通道产生新事件时发出，
// 触发与 tickMsg 相同的刷新路径，保证 TUI 与实时输出同步。
type streamEventMsg struct{ event agent.Event }

// streamCmd 返回一个 bubbletea 命令，等待 streamEvents 通道的下一个事件。
// 选中会话改变时会重新启动向通道写入事件的 goroutine。
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
				// 两侧都去空白比较，容忍输入带尾随换行/空格造成的差异。
				if msg.Role == enums.ChatRoleUser && strings.TrimSpace(msg.Content) == strings.TrimSpace(m.chatPanel.pendingFirstMessage) {
					found = true
					break
				}
			}
		}
		if found {
			m.chatPanel.pendingFirstMessage = ""
		} else {
			m.logInfo(fmt.Sprintf("[tui] selectSession: session %s 尚未同步首条用户消息，保留本地预展示", m.sessions[idx].ID))
		}
	}
	// 重置对话面板滚动状态。
	m.chatPanel.cursor = 0
	m.chatPanel.followBottom = true
	m.chatPanel.anchorUser = false
	m.chatPanel.lastItems = 0
	m.chatPanel.lastWidth = 0
	// 重置计划面板滚动偏移：不同会话的任务列表长度不同，旧偏移可能越界。
	m.planScroll = 0
	// 同理重置 Agent 编排面板滚动偏移。
	m.agentScroll = 0
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

// startStream 取消之前的事件流 goroutine，并启动一个监听当前选中会话的新 goroutine。
// 事件被写入 streamEvents，由 bubbletea 消息循环消费以刷新视图。
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
					// 事件流缓冲区已满，丢弃该事件；
					// 下次 tick 会通过 agent.Get 重新刷新。
				}
			}
		}
	}()
}

// rebuildAgents 根据当前选中会话重建 Agent 树。
func (m *Model) rebuildAgents() {
	m.agentTreePanel.rebuild(m.agent, m.selectedSession())
}

// refreshSessions 从 agent facade 重新加载会话列表，并尽量保持对原选中会话的光标位置。
func (m *Model) refreshSessions() {
	if m.agent == nil {
		return
	}
	// 记录当前选中的会话 ID，用于刷新后恢复光标。
	prevID := ""
	if m.sessionsCursor >= 0 && m.sessionsCursor < len(m.sessions) {
		prevID = m.sessions[m.sessionsCursor].ID
	}
	// 从 agent facade 加载全部会话（包括已恢复的历史会话），
	// 并尽可能保持光标停留在之前选中的会话上。
	sessions, err := m.agent.List(context.Background(), agent.Filter{})
	if err != nil {
		m.logError("[tui] refreshSessions", err)
		return
	}
	m.sessions = make([]*server.Session, 0, len(sessions))
	for _, s := range sessions {
		m.sessions = append(m.sessions, toServerSession(s))
	}
	// 在原列表中查找 previously selected session。
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
		// 原光标越界且仍有会话时，移到末尾并重建 Agent 树。
		m.sessionsCursor = len(m.sessions) - 1
		m.rebuildAgents()
	}
}

// syncInputMode 根据当前选中会话的待澄清状态同步输入栏模式（TODO #53）：
// 会话处于 awaiting_clarify 且存在 PendingClarify 时置为 inputClarify（已处于则保持），
// 否则若当前是 inputClarify 则回退 inputNormal，并顺带清空多选选择集 clarifySel
// 与选项光标（答复已提交、会话恢复运行后不再需要残留状态）；
// 同一 session 出现新的 PendingClarify（ID 变化）时也重置光标与选择集。
func (m *Model) syncInputMode() {
	if pc := m.pendingClarify(); pc != nil {
		if m.inputBar.mode != inputClarify {
			m.inputBar.mode = inputClarify
		}
		if m.clarifyID != pc.ID {
			m.clarifyID = pc.ID
			m.clarifyCursor = 0
			m.clarifySel = nil
		}
		return
	}
	if m.inputBar.mode == inputClarify {
		m.inputBar.mode = inputNormal
	}
	m.clarifyID = ""
	m.clarifyCursor = 0
	m.clarifySel = nil
}

// toServerSession 将 agent.Session DTO 转换为 TUI 内部仍在使用的 server.Session 类型。
func toServerSession(a *agent.Session) *server.Session {
	return server.ToServerSession(a)
}

// selectedSession 返回当前选中的会话；若 agent 不可用则返回本地缓存的浅拷贝作为降级。
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
		m.logError("[tui] selectedSession", err)
		// 降级：返回本地缓存的会话。
		s := *m.sessions[m.sessionsCursor]
		return &s
	}
	return toServerSession(sess)
}

// Update 是 bubbletea 消息处理入口，根据消息类型更新 Model 并返回命令。
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// 终端尺寸变化时更新尺寸、viewport 尺寸并重建对话内容。
		m.width = msg.Width
		m.height = msg.Height
		m.chatPanel.vp.Width = m.chatContentWidth()
		m.syncChatBodyHeight()
		m.rebuildChatContent()
		if m.chatPanel.followBottom {
			m.chatPanel.vp.GotoBottom()
		}

	case sizePollMsg:
		// 终端尺寸轮询（Windows 下 resize 事件的补偿）：尺寸变化时合成
		// WindowSizeMsg 走正常 resize 路径；无论是否变化都重新武装轮询。
		if cmd := m.reconcileSize(msg.width, msg.height); cmd != nil {
			return m, tea.Batch(sizePollCmd(), cmd)
		}
		return m, sizePollCmd()

	case tickMsg:
		m.tickCount++
		// 每 5 tick（0.5s）刷新会话列表；每 10 tick（1s）刷新 Agent 面板与 Token 统计。
		if m.tickCount%5 == 0 {
			m.refreshSessions()
			// 会话列表刷新后同步输入栏模式（awaiting_clarify ↔ inputClarify），
			// 并清理多选选择集；依赖会话状态，放在 refreshSessions 之后。
			m.syncInputMode()
		}
		if m.tickCount%10 == 0 {
			m.rebuildAgents()
			m.accumulateTokens()
		}
		// 运行中会话每 2 tick（200ms）强制刷新：思考中动效条目（转圈帧+省略号，
		// 相位取墙钟）不产生流式事件，只有强制重绘才能让帧前进。
		if m.tickCount%2 == 0 {
			if s := m.selectedSession(); s != nil && s.Status == enums.SessionStatusRunning {
				m.dirty = true
			}
		}
		// 有待消费的会话选中/滚动锚定请求时立即刷新，不等流式事件驱动，
		// 保证发送消息后视图在一个 tick 内响应。
		if m.shared.hasPendingSelect() || m.chatPanel.pendingScrollToUser {
			m.dirty = true
		}
		// 流式事件已在 streamEventMsg 中置脏，这里按 tick 粒度合并刷新；
		// 每 10 tick 额外强制刷新一次，保证 rebuildAgents/refreshSessions 带来的
		// 变化（不经过 dirty 标记）也能及时上屏。
		if m.dirty || m.tickCount%10 == 0 {
			m.dirty = false
			m.refreshView()
		}
		return m, tickCmd()

	case streamEventMsg:
		// 只置脏标记并继续监听事件流；实际刷新合并到下一个 tick，
		// 避免高频流式 delta 每个都触发全量 refreshView。
		m.dirty = true
		return m, streamCmd(m.streamEvents)

	case clipImageMsg:
		// Alt+V 剪贴板图片读取完成：校验限流后暂存并在输入栏插入 [image:N] 占位符。
		m.applyClipImage(msg)
		m.dirty = true

	case clipVideoMsg:
		// Alt+V 剪贴板视频文件识别完成：限流后暂存路径引用并插入 [video:N] 占位符。
		m.applyClipVideo(msg)
		m.dirty = true

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case modelSwitchDoneMsg:
		m.applyModelSwitchDone(msg)
		m.dirty = true

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

// refreshView 是 tickMsg 驱动的视图刷新逻辑（streamEventMsg 只置 dirty 标记，由 tick 合并触发）：
// 待处理会话选中、滚动到用户消息、重建对话内容、刷新弹窗。
func (m *Model) refreshView() {
	// 目标栏随首条消息/会话切换/看板目标出现或消失，先于下方一切滚动计算
	// 同步持久化的对话体高度，否则滚动偏移上限按过期高度钳制。
	m.syncChatBodyHeight()
	// 消费后台 createSession 写入的 pendingSelectID：在主循环内 refresh+select
	// 避免后台 goroutine 直接改 m.sessions/cursor 与 View 产生 race（T2 修复；
	// #47 修复：经 sharedState 指针共享，写入不再落到废弃的 Model 副本上）
	if id := m.shared.takePendingSelect(); id != "" {
		m.refreshSessions()
		for i, s := range m.sessions {
			if s.ID == id {
				m.selectSession(i)
				break
			}
		}
	}
	// 对话条目只全量收集一次，供下方滚动锚定与内容重建两个分支复用
	// （原先各算一次 collectChatItems，长会话下是双倍开销）。
	items := m.collectChatItems()
	// 发送消息后，优先滚动到最后一条用户问题，确保用户能看到自己的输入。
	// 该逻辑必须排在内容重建/自动跟随底部之前，防止新内容把用户问题顶出视口。
	if m.chatPanel.pendingScrollToUser {
		for idx := len(items) - 1; idx >= 0; idx-- {
			if strings.HasPrefix(items[idx].title, "> ") {
				m.chatPanel.rebuildContent(items, m.styles, m.chatContentWidth())
				// 把用户问题底部对齐视口底部，保留上方历史可见；
				// 同时锚定，让后续流式输出在下方展开而不把用户问题顶走。
				m.chatPanel.scrollToItemBottom(idx, m.chatPanel.vp.TotalLineCount(), m.chatPanel.vp.VisibleLineCount())
				m.chatPanel.followBottom = false
				m.chatPanel.anchorUser = true
				m.chatPanel.pendingScrollToUser = false
				// 注意：此处不得清除 pendingFirstMessage——命中的可能只是本地兜底
				// 条目（自我匹配），提前清除会让首条问题在服务端消息同步前消失。
				// 清除只在 selectSession 确认服务端已含该消息后进行。
				break
			}
		}
		// 未找到用户消息时保留 pendingScrollToUser，等待服务端写入或本地兜底展示后再试
	}
	// viewport 内容随会话事件/消息增长而重建；跟随底部时自动滚到最新。
	// 锚定到用户问题时仅重建内容、不自动滚动，避免用户问题被顶出视口。
	if s := m.selectedSession(); s != nil {
		itemsChanged := len(items) != m.chatPanel.lastItems
		widthChanged := m.chatContentWidth() != m.chatPanel.lastWidth
		// 条目数不变但末条内容变化（流式文本增长、思考中/执行中状态切换）也需重建。
		lastTitle := ""
		if len(items) > 0 {
			lastTitle = items[len(items)-1].title
		}
		liveChanged := lastTitle != m.chatPanel.lastLiveTitle
		if itemsChanged || widthChanged || liveChanged {
			wasAtBottom := m.chatPanel.vp.AtBottom() || m.chatPanel.followBottom
			m.chatPanel.rebuildContent(items, m.styles, m.chatContentWidth())
			if !m.chatPanel.anchorUser && wasAtBottom {
				m.chatPanel.vp.GotoBottom()
				m.chatPanel.followBottom = true
			}
		}
	}
	// 内容已溢出视口时解除用户问题锚定并跟随底部：
	// 锚定只在全部内容可见时有意义（问题不被顶走）；一旦流式输出使内容超过
	// 视口高度，必须恢复自动跟随，否则流式尾部与最终结果永远停留在视口外。
	if m.chatPanel.anchorUser && m.chatPanel.vp.TotalLineCount() > m.chatPanel.vp.VisibleLineCount() {
		m.chatPanel.anchorUser = false
		m.chatPanel.followBottom = true
		m.chatPanel.vp.GotoBottom()
	}
	if !m.chatPanel.anchorUser && m.chatPanel.followBottom {
		m.chatPanel.vp.GotoBottom()
	}
	// 弹窗打开时刷新动态内容（完整记录面板在末尾时跟随新输出）
	if m.overlayPanel.mode != overlayNone {
		m.refreshOverlay()
	}
}

// handleMouse 处理鼠标消息，包括弹窗滚轮、对话区滚轮、滚动条拖动等。
func (m *Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// 弹窗打开时，滚轮用于滚动弹窗内容。
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

	// 滚轮悬停在右侧计划面板区域时滚动计划任务列表（执行计划改为滚动查看，不再省略截断）。
	if (msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown) && m.rightPanelVisible() && m.overPlanPanel(msg.X, msg.Y) {
		delta := 3
		if msg.Button == tea.MouseButtonWheelUp {
			delta = -3
		}
		m.planScroll = clamp(m.planScroll+delta, 0, m.planMaxScroll())
		return m, nil
	}

	// 滚轮悬停在右侧 Agent 编排面板区域时滚动编排树（与计划面板一致：滚动查看，不再省略截断）。
	if (msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown) && m.rightPanelVisible() && m.overAgentsPanel(msg.X, msg.Y) {
		delta := 3
		if msg.Button == tea.MouseButtonWheelUp {
			delta = -3
		}
		m.agentScroll = clamp(m.agentScroll+delta, 0, m.agentMaxScroll())
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
		m.chatPanel.scrollToThumbY(relY-thumbH/2, m.mainContentHeight())
		return m, nil
	}

	// 默认交给 viewport 处理内容区点击等。
	var cmd tea.Cmd
	m.chatPanel.vp, cmd = m.chatPanel.vp.Update(msg)
	m.chatPanel.followBottom = m.chatPanel.vp.AtBottom()
	m.chatPanel.anchorUser = false
	return m, cmd
}

// handleKey 处理键盘消息，包括全局快捷键、弹窗导航、输入栏路由、对话区导航。
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

	// 弹窗模式：导航或关闭。
	if m.overlayPanel.mode != overlayNone {
		// 保持弹窗内容与光标和实时状态同步。
		m.refreshOverlay()
		switch msg.String() {
		case "ctrl+c":
			return m.ctrlCQuit()
		case "esc", "q":
			// 模型弹窗两段式：stage 1 的 Esc 退回角色列表，stage 0 才关闭。
			if m.overlayPanel.mode == overlayModel && m.modelStage == 1 {
				m.modelStage = 0
				m.overlayPanel.title = "Switch Model · Select Role"
				m.overlayPanel.lines = m.buildModelRoleLines()
				m.clampOverlayCursor()
				return m, nil
			}
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
			// 模型弹窗 Enter 可能发起异步切换（返回 tea.Cmd）。
			if m.overlayPanel.mode == overlayModel {
				return m, m.handleModelEnter()
			}
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

	// 输入栏模式：将所有按键交给输入处理器。
	if m.focus == panelInput {
		return m.handleInputKey(msg)
	}

	// 对话区导航模式。
	switch msg.String() {
	case "ctrl+c":
		return m.ctrlCQuit()
	case "q", "Q":
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
		m.toggleModelPopup()
	case "K":
		m.focus = panelInput
		m.inputBar.mode = inputNormal
	case "/":
		// "/" 聚焦到空输入栏，用于输入斜杠命令；其他可打印字符
		// 直接进入 default 分支并填充到输入缓冲区。
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
		// 任意可打印字符都会进入输入模式并填充缓冲区。
		if len(msg.Runes) > 0 && unicode.IsPrint(msg.Runes[0]) {
			m.focus = panelInput
			m.inputBar.mode = inputNormal
			m.inputBar.runes = append([]rune{}, msg.Runes...)
			m.inputBar.cursor = len(m.inputBar.runes)
		}
	}
	return m, nil
}

// ctrlCQuit 处理退出请求：仍有会话在运行时，首次按下只提示，
// 3 秒内再次按下 Ctrl+C 才真正退出——避免误关终端把执行了数小时的长任务杀掉
// （退出时 app.Agent.Shutdown 会取消全部运行中会话）。
func (m *Model) ctrlCQuit() (tea.Model, tea.Cmd) {
	running := 0
	for _, s := range m.sessions {
		if s != nil && s.Status == enums.SessionStatusRunning {
			running++
		}
	}
	if running > 0 && time.Now().After(m.quitArmedUntil) {
		m.quitArmedUntil = time.Now().Add(3 * time.Second)
		m.flashMsg(fmt.Sprintf("仍有 %d 个会话在运行，3 秒内再按一次 Ctrl+C 确认退出", running))
		return m, nil
	}
	return m, tea.Quit
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

// mainContentHeight 返回中间主内容区高度（已扣除顶栏、问答面板、Token 栏、输入栏、底部快捷键栏、弹窗占位）。
func (m *Model) mainContentHeight() int {
	topH := 1
	// Token 用量栏：输入栏上方 1 行实时展示当前会话累计 token。
	tokenBarH := 1
	// 输入栏实际是 5 行：带边框样式的 Height(3) 只算内容区，上下边框再加 2 行。
	// 预算必须按真实高度计算，否则整页比终端高出 2 行，
	// alt-screen 只保留底部 N 行，顶栏与对话区首行（首个问题）会被顶出屏幕。
	inputH := 5
	shortcutH := 1
	overlayH := 0
	if m.overlayPanel.mode != overlayNone {
		overlayH = m.height / 3
		if overlayH < 6 {
			overlayH = 6
		}
	}
	clarifyH := m.clarifyPanelHeight()
	h := m.height - topH - tokenBarH - inputH - shortcutH - overlayH - clarifyH
	if h < 4 {
		h = 4
	}
	return h
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

// overPlanPanel 命中测试：屏幕坐标是否落在右侧计划面板区域内。
// 布局：顶栏占 1 行，其下为主内容区；计划面板为右栏上段（rightPanelHeights 的 topH），
// x 起点为对话区宽度（renderRightPanels 紧跟 renderChat 水平拼接）。
func (m *Model) overPlanPanel(x, y int) bool {
	topH, _ := rightPanelHeights(m.mainContentHeight())
	return x >= m.chatAreaWidth() && y >= 1 && y < 1+topH
}

// planMaxScroll 返回计划面板任务列表当前的最大滚动偏移：
// 任务数超出可视行数（面板高度扣除标题/边框与底部"总体进度/计时"3 行）的部分。
func (m *Model) planMaxScroll() int {
	s := m.selectedSession()
	if s == nil {
		return 0
	}
	snap := m.boardSnapshot(s)
	topH, _ := rightPanelHeights(m.mainContentHeight())
	budget := topH - 3 - 3 // PanelBox 边框/标题 3 行 + footer 3 行
	if budget < 0 {
		budget = 0
	}
	if maxOff := len(snap.Tasks) - budget; maxOff > 0 {
		return maxOff
	}
	return 0
}

// overAgentsPanel 命中测试：屏幕坐标是否落在右侧 Agent 编排面板区域内。
// 布局：顶栏占 1 行；编排面板为右栏下段（紧跟计划面板，rightPanelHeights 的 bottomH）。
func (m *Model) overAgentsPanel(x, y int) bool {
	topH, bottomH := rightPanelHeights(m.mainContentHeight())
	return x >= m.chatAreaWidth() && y >= 1+topH && y < 1+topH+bottomH
}

// agentMaxScroll 返回 Agent 编排面板树内容当前的最大滚动偏移：
// 完整内容行数超出可视行数（面板高度扣除标题/边框 3 行与底部固定图例 1 行）的部分。
// 无分支时回退网格布局内容很短，不可滚动，返回 0。
func (m *Model) agentMaxScroll() int {
	_, branches, _ := groupAgentTree(m.agentTreePanel.nodes)
	if len(branches) == 0 {
		return 0
	}
	_, bottomH := rightPanelHeights(m.mainContentHeight())
	budget := bottomH - 3 - 1 // PanelBox 边框/标题 3 行 + 底部图例 1 行
	if budget < 1 {
		return 0
	}
	w := m.rightPanelWidth()
	if w < 20 {
		w = 20
	}
	innerW := w - 4
	if innerW < 10 {
		innerW = 10
	}
	if maxOff := len(m.buildAgentTreeContent(innerW)) - budget; maxOff > 0 {
		return maxOff
	}
	return 0
}

// toggleLogPopup 打开/关闭"完整记录"面板：把整段对话铺成可滚动行列表，
// 用户可用 j/k/g/G 翻阅全部 LLM 输出 / 工具调用 / 思考，不受对话区高度限制。
// 内容每次渲染实时刷新（refreshOverlay），保证新输出立即可见。
// accumulateTokens 累加 token_usage 事件的输入/输出 Token 数量。
func (m *Model) accumulateTokens() {
	s := m.selectedSession()
	if s == nil {
		return
	}
	in, out := 0, 0
	cacheHit, cacheMiss := 0, 0
	for _, ev := range s.Events {
		in += ev.InputTokens
		out += ev.OutputTokens
		cacheHit += ev.CacheHitTokens
		cacheMiss += ev.CacheMissTokens
	}
	m.totalInputTokens = in
	m.totalOutputTokens = out
	m.totalCacheHit = cacheHit
	m.totalCacheMiss = cacheMiss
	// 成本预警：输入 Token 每跨过 50 万一档提醒一次（仅提示，不阻断执行）。
	const warnStep = 500000
	if level := in / warnStep; level > m.tokenWarnLevel {
		m.tokenWarnLevel = level
		m.flashMsg(fmt.Sprintf("本会话输入 Token 已达 %d 万，注意模型成本", in/10000))
	}
}

// clamp 将整数 v 限制在 [lo, hi] 范围内；若 lo > hi 则返回 lo。
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
// thumbBounds 返回滑块在滚动条区域内的起始/结束行索引（含）。
// 若内容无需滚动则返回 (-1, -1)。
// updateDrag 根据当前鼠标 Y 坐标更新 viewport 滚动位置。
// 以 dragStartY/dragStartOffset 为基准，按滑块可移动范围与内容可滚动范围的比率映射。
// scrollToThumbY 将滑块中心对齐到滚动条区域内的指定 Y 坐标（相对于滚动条顶部）。
