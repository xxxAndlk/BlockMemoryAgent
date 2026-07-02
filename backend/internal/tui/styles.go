package tui

import "github.com/charmbracelet/lipgloss"

// Colors — Tokyo Night palette.
const (
	cTitle     = "#7AA2F7"
	cFocus     = "#7AA2F7"
	cBlur      = "#414868"
	cMeta      = "#F7768E"
	cDomain    = "#E0AF68"
	cSub       = "#7AA2F7"
	cAssist    = "#9ECE6A"
	cActive    = "#9ECE6A"
	cDone      = "#565F89"
	cWarn      = "#E0AF68"
	cError     = "#F7768E"
	cInfo      = "#A9B1D6"
	cValue     = "#C0CAF5"
	cHelpBg    = "#1F2335"
	cHeader    = "#BB9AF7"
	cOverlayBg = "#24283B"
)

// Styles holds all lipgloss styles used by the TUI.
type Styles struct {
	Title        lipgloss.Style
	Header       lipgloss.Style
	FocusBorder  lipgloss.Style
	BlurBorder   lipgloss.Style
	TreeMeta     lipgloss.Style
	TreeDomain   lipgloss.Style
	TreeSub      lipgloss.Style
	TreeAssist   lipgloss.Style
	TreeDone     lipgloss.Style
	TreeActive   lipgloss.Style
	LogInfo      lipgloss.Style
	LogSuccess   lipgloss.Style
	LogWarn      lipgloss.Style
	LogError     lipgloss.Style
	LogUser      lipgloss.Style
	LogAssistant lipgloss.Style
	LogSystem    lipgloss.Style
	StatLabel    lipgloss.Style
	StatValue    lipgloss.Style
	HelpBar      lipgloss.Style
	CallStack    lipgloss.Style
	SessionSum   lipgloss.Style
	StepNode     lipgloss.Style
	Overlay      lipgloss.Style
	InputPrompt  lipgloss.Style
	InputText    lipgloss.Style
	Dim          lipgloss.Style
	Badge        lipgloss.Style
	BadgeWarn    lipgloss.Style
	BadgeOk      lipgloss.Style
}

// NewStyles constructs the default style set.
func NewStyles() *Styles {
	rounded := lipgloss.RoundedBorder()
	return &Styles{
		Title:        lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(cTitle)).Padding(0, 1),
		Header:       lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(cHeader)),
		FocusBorder:  lipgloss.NewStyle().BorderStyle(rounded).BorderForeground(lipgloss.Color(cFocus)),
		BlurBorder:   lipgloss.NewStyle().BorderStyle(rounded).BorderForeground(lipgloss.Color(cBlur)),
		TreeMeta:     lipgloss.NewStyle().Foreground(lipgloss.Color(cMeta)).Bold(true),
		TreeDomain:   lipgloss.NewStyle().Foreground(lipgloss.Color(cDomain)).Bold(true),
		TreeSub:      lipgloss.NewStyle().Foreground(lipgloss.Color(cSub)),
		TreeAssist:   lipgloss.NewStyle().Foreground(lipgloss.Color(cAssist)),
		TreeDone:     lipgloss.NewStyle().Foreground(lipgloss.Color(cDone)),
		TreeActive:   lipgloss.NewStyle().Foreground(lipgloss.Color(cActive)).Bold(true),
		LogInfo:      lipgloss.NewStyle().Foreground(lipgloss.Color(cInfo)),
		LogSuccess:   lipgloss.NewStyle().Foreground(lipgloss.Color(cActive)),
		LogWarn:      lipgloss.NewStyle().Foreground(lipgloss.Color(cWarn)),
		LogError:     lipgloss.NewStyle().Foreground(lipgloss.Color(cError)),
		LogUser:      lipgloss.NewStyle().Foreground(lipgloss.Color(cInfo)).Bold(true),
		LogAssistant: lipgloss.NewStyle().Foreground(lipgloss.Color(cAssist)).Bold(true),
		LogSystem:    lipgloss.NewStyle().Foreground(lipgloss.Color(cDone)).Italic(true),
		StatLabel:    lipgloss.NewStyle().Foreground(lipgloss.Color(cDone)),
		StatValue:    lipgloss.NewStyle().Foreground(lipgloss.Color(cValue)).Bold(true),
		HelpBar:      lipgloss.NewStyle().Background(lipgloss.Color(cHelpBg)).Foreground(lipgloss.Color(cInfo)).Padding(0, 1),
		CallStack:    lipgloss.NewStyle().Foreground(lipgloss.Color(cFocus)),
		SessionSum:   lipgloss.NewStyle().Foreground(lipgloss.Color(cValue)).Italic(true),
		StepNode:     lipgloss.NewStyle().Background(lipgloss.Color(cBlur)).Foreground(lipgloss.Color(cValue)).Padding(0, 1),
		Overlay:      lipgloss.NewStyle().Background(lipgloss.Color(cOverlayBg)).BorderStyle(rounded).BorderForeground(lipgloss.Color(cFocus)).Padding(1, 2),
		InputPrompt:  lipgloss.NewStyle().Foreground(lipgloss.Color(cActive)).Bold(true),
		InputText:    lipgloss.NewStyle().Foreground(lipgloss.Color(cValue)),
		Dim:          lipgloss.NewStyle().Foreground(lipgloss.Color(cDone)),
		Badge:        lipgloss.NewStyle().Background(lipgloss.Color(cBlur)).Foreground(lipgloss.Color(cValue)).Padding(0, 1),
		BadgeWarn:    lipgloss.NewStyle().Background(lipgloss.Color(cBlur)).Foreground(lipgloss.Color(cWarn)).Padding(0, 1),
		BadgeOk:      lipgloss.NewStyle().Background(lipgloss.Color(cBlur)).Foreground(lipgloss.Color(cActive)).Padding(0, 1),
	}
}
