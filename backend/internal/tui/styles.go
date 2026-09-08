package tui

import "github.com/charmbracelet/lipgloss"

// 颜色常量集合：采用 Tokyo Night 主题色板，统一 TUI 各组件的配色。
const (
	// cTitle 是标题/强调色，用于顶部名称、快捷键键名等。
	cTitle = "#7AA2F7"
	// cFocus 是焦点边框色，输入栏/面板获得焦点时使用。
	cFocus = "#7AA2F7"
	// cBlur 是失焦边框/辅助背景色，用于非焦点面板与分隔线。
	cBlur = "#414868"
	// cMeta 是 MetaAgent 主题色，用于 Agent 树顶层节点。
	cMeta = "#F7768E"
	// cDomain 是领域 Agent 主题色，也用于用户消息标签。
	cDomain = "#E0AF68"
	// cSub 是子领域 Agent 主题色，也用于示例文字。
	cSub = "#7AA2F7"
	// cAssist 是辅助/固定 Agent 主题色，表示成功/运行中。
	cAssist = "#9ECE6A"
	// cActive 是活跃状态色，与 cAssist 一致。
	cActive = "#9ECE6A"
	// cDone 是完成/闲置/暗淡文本色。
	cDone = "#565F89"
	// cWarn 是警告/等待状态色。
	cWarn = "#E0AF68"
	// cError 是错误状态色。
	cError = "#F7768E"
	// cInfo 是普通信息文本色。
	cInfo = "#A9B1D6"
	// cValue 是数值/高亮文本色。
	cValue = "#C0CAF5"
	// cHelpBg 是帮助栏背景色。
	cHelpBg = "#1F2335"
	// cHeader 是面板标题/表头强调色。
	cHeader = "#BB9AF7"
	// cOverlayBg 是弹窗背景色。
	cOverlayBg = "#24283B"
	// cPanelBg 是右侧面板背景色。
	cPanelBg = "#16161E"
	// cTopBarBg 是顶部状态栏背景色。
	cTopBarBg = "#1A1B26"
	// cStatusRun 是运行中状态指示色。
	cStatusRun = "#9ECE6A"
	// cStatusIdle 是空闲状态指示色。
	cStatusIdle = "#565F89"
	// cStatusWait 是等待状态指示色。
	cStatusWait = "#E0AF68"
	// cStatusErr 是错误状态指示色。
	cStatusErr = "#F7768E"
	// cStatusDone 是完成态指示色，使用蓝色以区别于 idle 灰色。
	cStatusDone = "#7AA2F7" // 完成态：蓝色，区别于 idle 灰
)

// Styles 聚合 TUI 中所有 lipgloss 样式，避免在渲染逻辑中反复构造。
type Styles struct {
	// Title 用于顶部大标题或强调品牌名。
	Title lipgloss.Style
	// Header 用于弹窗/面板的标题文本。
	Header lipgloss.Style
	// FocusBorder 是获得焦点组件的边框样式。
	FocusBorder lipgloss.Style
	// BlurBorder 是失去焦点组件的边框样式。
	BlurBorder lipgloss.Style
	// TreeMeta 是 Agent 树中 MetaAgent 节点样式。
	TreeMeta lipgloss.Style
	// TreeDomain 是 Agent 树中领域节点样式。
	TreeDomain lipgloss.Style
	// TreeSub 是 Agent 树中子领域节点样式。
	TreeSub lipgloss.Style
	// TreeAssist 是 Agent 树中辅助节点样式。
	TreeAssist lipgloss.Style
	// TreeDone 是 Agent 树中已完成节点样式。
	TreeDone lipgloss.Style
	// TreeActive 是 Agent 树中活跃节点样式。
	TreeActive lipgloss.Style
	// LogInfo 是日志/事件信息文本样式。
	LogInfo lipgloss.Style
	// LogSuccess 是成功日志样式。
	LogSuccess lipgloss.Style
	// LogWarn 是警告日志样式。
	LogWarn lipgloss.Style
	// LogError 是错误日志样式。
	LogError lipgloss.Style
	// LogUser 是用户消息标签样式。
	LogUser lipgloss.Style
	// LogAssistant 是助手消息标签样式。
	LogAssistant lipgloss.Style
	// LogSystem 是系统消息样式。
	LogSystem lipgloss.Style
	// StatLabel 是统计标签样式。
	StatLabel lipgloss.Style
	// StatValue 是统计数值样式。
	StatValue lipgloss.Style
	// HelpBar 是帮助栏样式。
	HelpBar lipgloss.Style
	// CallStack 是调用栈/分隔线样式。
	CallStack lipgloss.Style
	// SessionSum 是会话摘要样式。
	SessionSum lipgloss.Style
	// StepNode 是步骤节点徽章样式。
	StepNode lipgloss.Style
	// Overlay 是弹窗容器样式。
	Overlay lipgloss.Style
	// InputPrompt 是输入栏提示符样式。
	InputPrompt lipgloss.Style
	// InputText 是输入栏文本样式。
	InputText lipgloss.Style
	// Dim 是暗淡/次要文本样式。
	Dim lipgloss.Style
	// Badge 是普通状态徽章样式。
	Badge lipgloss.Style
	// BadgeWarn 是警告状态徽章样式。
	BadgeWarn lipgloss.Style
	// BadgeOk 是成功状态徽章样式。
	BadgeOk lipgloss.Style
	// ScrollbarTrack 是滚动条轨道样式。
	ScrollbarTrack lipgloss.Style
	// ScrollbarThumb 是滚动条滑块样式。
	ScrollbarThumb lipgloss.Style

	// v2.5 新布局样式
	// TopBar 是顶部状态栏整体样式。
	TopBar lipgloss.Style
	// TopBarLabel 是顶部状态栏标签（Model/Memory 等）样式。
	TopBarLabel lipgloss.Style
	// TopBarValue 是顶部状态栏值文本样式。
	TopBarValue lipgloss.Style
	// TopBarStatus 是顶部状态栏状态文本样式。
	TopBarStatus lipgloss.Style
	// TopBarSep 是顶部状态栏分隔符样式。
	TopBarSep lipgloss.Style
	// WelcomeTitle 是欢迎页标题样式。
	WelcomeTitle lipgloss.Style
	// WelcomeSub 是欢迎页副标题样式。
	WelcomeSub lipgloss.Style
	// WelcomeBox 是欢迎页信息框样式。
	WelcomeBox lipgloss.Style
	// WelcomeLabel 是欢迎页标签样式。
	WelcomeLabel lipgloss.Style
	// WelcomeValue 是欢迎页值文本样式。
	WelcomeValue lipgloss.Style
	// WelcomeExample 是欢迎页示例文本样式。
	WelcomeExample lipgloss.Style
	// PanelHeader 是右侧面板标题栏样式。
	PanelHeader lipgloss.Style
	// PanelBox 是右侧面板内容区样式。
	PanelBox lipgloss.Style
	// InputHint 是输入栏空态提示文本样式。
	InputHint lipgloss.Style
}

// NewStyles 构造默认样式集合，所有颜色与尺寸在此集中配置。
func NewStyles() *Styles {
	// rounded 是圆角边框样式，多个组件复用。
	rounded := lipgloss.RoundedBorder()
	return &Styles{
		Title:          lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(cTitle)).Padding(0, 1),
		Header:         lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(cHeader)),
		FocusBorder:    lipgloss.NewStyle().BorderStyle(rounded).BorderForeground(lipgloss.Color(cFocus)),
		BlurBorder:     lipgloss.NewStyle().BorderStyle(rounded).BorderForeground(lipgloss.Color(cBlur)),
		TreeMeta:       lipgloss.NewStyle().Foreground(lipgloss.Color(cMeta)).Bold(true),
		TreeDomain:     lipgloss.NewStyle().Foreground(lipgloss.Color(cDomain)).Bold(true),
		TreeSub:        lipgloss.NewStyle().Foreground(lipgloss.Color(cSub)),
		TreeAssist:     lipgloss.NewStyle().Foreground(lipgloss.Color(cAssist)),
		TreeDone:       lipgloss.NewStyle().Foreground(lipgloss.Color(cDone)),
		TreeActive:     lipgloss.NewStyle().Foreground(lipgloss.Color(cActive)).Bold(true),
		LogInfo:        lipgloss.NewStyle().Foreground(lipgloss.Color(cInfo)),
		LogSuccess:     lipgloss.NewStyle().Foreground(lipgloss.Color(cActive)),
		LogWarn:        lipgloss.NewStyle().Foreground(lipgloss.Color(cWarn)),
		LogError:       lipgloss.NewStyle().Foreground(lipgloss.Color(cError)),
		LogUser:        lipgloss.NewStyle().Foreground(lipgloss.Color(cDomain)).Bold(true).Background(lipgloss.Color(cTopBarBg)),
		LogAssistant:   lipgloss.NewStyle().Foreground(lipgloss.Color(cAssist)).Bold(true),
		LogSystem:      lipgloss.NewStyle().Foreground(lipgloss.Color(cDone)).Italic(true),
		StatLabel:      lipgloss.NewStyle().Foreground(lipgloss.Color(cDone)),
		StatValue:      lipgloss.NewStyle().Foreground(lipgloss.Color(cValue)).Bold(true),
		HelpBar:        lipgloss.NewStyle().Background(lipgloss.Color(cHelpBg)).Foreground(lipgloss.Color(cInfo)).Padding(0, 1),
		CallStack:      lipgloss.NewStyle().Foreground(lipgloss.Color(cFocus)),
		SessionSum:     lipgloss.NewStyle().Foreground(lipgloss.Color(cValue)).Italic(true),
		StepNode:       lipgloss.NewStyle().Background(lipgloss.Color(cBlur)).Foreground(lipgloss.Color(cValue)).Padding(0, 1),
		Overlay:        lipgloss.NewStyle().Background(lipgloss.Color(cOverlayBg)).BorderStyle(rounded).BorderForeground(lipgloss.Color(cFocus)).Padding(1, 2),
		InputPrompt:    lipgloss.NewStyle().Foreground(lipgloss.Color(cActive)).Bold(true),
		InputText:      lipgloss.NewStyle().Foreground(lipgloss.Color(cValue)),
		Dim:            lipgloss.NewStyle().Foreground(lipgloss.Color(cDone)),
		Badge:          lipgloss.NewStyle().Background(lipgloss.Color(cBlur)).Foreground(lipgloss.Color(cValue)).Padding(0, 1),
		BadgeWarn:      lipgloss.NewStyle().Background(lipgloss.Color(cBlur)).Foreground(lipgloss.Color(cWarn)).Padding(0, 1),
		BadgeOk:        lipgloss.NewStyle().Background(lipgloss.Color(cBlur)).Foreground(lipgloss.Color(cActive)).Padding(0, 1),
		ScrollbarTrack: lipgloss.NewStyle().Foreground(lipgloss.Color(cBlur)),
		ScrollbarThumb: lipgloss.NewStyle().Foreground(lipgloss.Color(cFocus)),

		// v2.5 新布局样式
		TopBar:         lipgloss.NewStyle().Background(lipgloss.Color(cTopBarBg)).Foreground(lipgloss.Color(cInfo)),
		TopBarLabel:    lipgloss.NewStyle().Foreground(lipgloss.Color(cDone)),
		TopBarValue:    lipgloss.NewStyle().Foreground(lipgloss.Color(cValue)),
		TopBarStatus:   lipgloss.NewStyle().Foreground(lipgloss.Color(cStatusRun)),
		TopBarSep:      lipgloss.NewStyle().Foreground(lipgloss.Color(cBlur)),
		WelcomeTitle:   lipgloss.NewStyle().Foreground(lipgloss.Color(cTitle)).Bold(true),
		WelcomeSub:     lipgloss.NewStyle().Foreground(lipgloss.Color(cInfo)),
		WelcomeBox:     lipgloss.NewStyle().BorderStyle(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(cBlur)).Padding(1, 2),
		WelcomeLabel:   lipgloss.NewStyle().Foreground(lipgloss.Color(cDone)),
		WelcomeValue:   lipgloss.NewStyle().Foreground(lipgloss.Color(cValue)),
		WelcomeExample: lipgloss.NewStyle().Foreground(lipgloss.Color(cSub)),
		PanelHeader:    lipgloss.NewStyle().Background(lipgloss.Color(cBlur)).Foreground(lipgloss.Color(cValue)).Bold(true).Padding(0, 1),
		PanelBox:       lipgloss.NewStyle().Background(lipgloss.Color(cPanelBg)).BorderStyle(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(cBlur)).Padding(0, 1),
		InputHint:      lipgloss.NewStyle().Foreground(lipgloss.Color(cDone)),
	}
}
