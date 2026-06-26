package tui

import (
	"context"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/blockmemory/agent/backend/internal/graph"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/internal/store"
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
	chatCursor       int
	chatFollowBottom bool

	// agents tree (built every tick)
	agentsNodes []agentTreeNode

	// input bar
	inputMode    int
	inputRunes   []rune
	inputCursor  int
	inputHistory []string
	inputHistIdx int

	// overlay
	overlayTitle  string
	overlayLines  []string
	overlayCursor int
	overlayKind   int // overlayPlan / overlayAgents / overlayDetail / overlayHelp

	// flash banner
	flash      string
	flashUntil time.Time

	tickCount int
}

// agentTreeNode is one flattened row in the agent topology.
type agentTreeNode struct {
	depth     int
	instID    string
	name      string
	domain    string
	roleType  types.RoleType
	status    types.RoleStatus
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
		sessionMgr: sessionMgr,
		registry:   registry,
		rt:         rt,
		dagHandler: dagHandler,
		pgStore:    pgStore,
		httpAddr:   httpAddr,
		modelName:  modelName,
		styles:     NewStyles(),
		focus:      panelChat,
		chatFollowBottom: true,
		inputHistIdx:     -1,
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
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg{} })
}

type tickMsg struct{}

func (m *Model) selectSession(idx int) {
	if idx < 0 || idx >= len(m.sessions) {
		return
	}
	m.sessionsCursor = idx
	m.chatCursor = 0
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
	m.sessions = m.sessionMgr.ListSessions()
	if m.pgStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if recs, err := m.pgStore.RecentSessionHistories(ctx, 200); err == nil {
			seen := make(map[string]bool)
			for _, s := range m.sessions {
				seen[s.ID] = true
			}
			for _, rec := range recs {
				if seen[rec.SessionID] {
					continue
				}
				endedAt := rec.CreatedAt
				m.sessions = append(m.sessions, &server.Session{
					ID:        rec.SessionID,
					Goal:      rec.Goal,
					Status:    "completed",
					Result:    rec.Summary,
					StartedAt: rec.CreatedAt,
					EndedAt:   &endedAt,
					Events:    make([]server.SessionEvent, 0),
					Messages:  make([]types.ChatMessage, 0),
				})
			}
		}
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

func (m *Model) rebuildAgents() {
	m.agentsNodes = nil
	s := m.selectedSession()
	if s == nil {
		return
	}

	metaStatus := types.RoleStatusIdle
	switch s.Status {
	case "running":
		metaStatus = types.RoleStatusActive
	case "completed":
		metaStatus = types.RoleStatusDone
	case "error":
		metaStatus = types.RoleStatusError
	case "awaiting_clarify":
		metaStatus = types.RoleStatusWaiting
	}
	m.agentsNodes = append(m.agentsNodes, agentTreeNode{
		depth:    0,
		instID:   "MetaAgent",
		name:     "MetaAgent",
		roleType: types.RoleTypeMeta,
		status:   metaStatus,
	})

	insts := m.registry.GetInstancesBySession(s.ID)
	byID := make(map[string]*types.RoleInstance)
	for _, inst := range insts {
		byID[inst.ID] = inst
	}
	for _, inst := range insts {
		if inst.Type != types.RoleTypeDomain {
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
			if child.Type == types.RoleTypeSubDomain {
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
			} else if child.Type == types.RoleTypeFixed || child.Type == types.RoleTypeDynamic {
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
			roleType:  types.RoleTypeMeta,
			status:    types.RoleStatusWaiting,
			isClarify: true,
		})
	}
}

func instName(r *graph.RoleRegistry, inst *types.RoleInstance) string {
	if inst == nil {
		return "unknown"
	}
	if inst.Type == types.RoleTypeDomain || inst.Type == types.RoleTypeSubDomain {
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
	return m.sessions[m.sessionsCursor]
}

// Update handles messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tickMsg:
		m.tickCount++
		if m.tickCount%25 == 0 {
			m.refreshSessions()
		}
		m.rebuildAgents()
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
		m.chatFollowBottom = false
		m.moveChatCursor(-3)
	case tea.MouseButtonWheelDown:
		m.moveChatCursor(3)
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
	case "ctrl+c":
		return m, tea.Quit
	case "1":
		// already on chat
	case "2":
		m.togglePlanPopup()
	case "3":
		m.toggleAgentsPopup()
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
	case "g":
		m.moveChatCursor(-99999)
	case "G":
		m.moveChatCursor(99999)
	case "tab":
		m.focus = panelInput
		m.inputMode = inputNormal
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

func (m *Model) moveChatCursor(delta int) {
	s := m.selectedSession()
	if s == nil {
		return
	}
	items := len(s.Messages) + len(s.Events)
	m.chatCursor = clamp(m.chatCursor+delta, 0, items-1)
	if delta < 0 {
		m.chatFollowBottom = false
	}
	if m.chatCursor >= items-1 {
		m.chatFollowBottom = true
	}
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

func (m *Model) openOverlay(title string, lines []string) {
	m.overlay = overlayDetail
	m.overlayKind = overlayDetail
	m.overlayTitle = title
	m.overlayLines = lines
	m.overlayCursor = 0
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
