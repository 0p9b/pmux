package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// SetupStage identifies one presentation step in the guided first-run journey.
// Infrastructure work remains behind SetupFacade; the model only owns choices.
type SetupStage string

const (
	SetupChooseMode     SetupStage = "choose-mode"
	SetupAdoptPaths     SetupStage = "adopt-paths"
	SetupCore           SetupStage = "core"
	SetupProviderOffer  SetupStage = "provider-offer"
	SetupProviderSelect SetupStage = "provider-select"
	SetupProviderAuth   SetupStage = "provider-auth"
	SetupModelSelect    SetupStage = "model-select"
	SetupClientPreflight SetupStage = "client-preflight"
	SetupLaunchOffer    SetupStage = "launch-offer"
	SetupComplete       SetupStage = "complete"
)

type SetupActionKind string

const (
	SetupActionCore          SetupActionKind = "core"
	SetupActionLoadProviders SetupActionKind = "load-providers"
	SetupActionLoginProvider SetupActionKind = "login-provider"
	SetupActionPreflight     SetupActionKind = "preflight"
)

// SetupAction carries only user choices. It contains no credentials and is
// interpreted by the command-layer facade through existing application use cases.
type SetupAction struct {
	Kind       SetupActionKind
	Mode       string
	ProxyPath  string
	ConfigPath string
	Harden     bool
	Provider   string
	Model      string
}

type SetupChoice struct {
	ID    string
	Label string
	Note  string
}

type SetupProgress struct {
	Stage         SetupStage
	Mode          string
	CoreComplete  bool
	Providers     []SetupChoice
	Models        []SetupChoice
	SelectedModel string
	ClientPath    string
	ClientVersion string
	Messages      []string
	NextActions   []string
}

type SetupFacade interface {
	Execute(context.Context, SetupAction) (SetupProgress, error)
}

type SetupStart struct {
	Mode       string
	ProxyPath  string
	ConfigPath string
	Harden     bool
}

// SetupModel is a finite terminal model for first-run setup. A successful core
// transaction is never rolled back merely because onboarding is skipped or
// canceled; Complete reports the exact resumable commands instead.
type SetupModel struct {
	ctx      context.Context
	facade   SetupFacade
	options  Options
	progress SetupProgress
	start    SetupStart
	selected int
	field    int
	busy     bool
	cancel   context.CancelFunc
	lastErr  string
	canceled bool
	launch   bool
}

type setupProgressMsg struct {
	progress SetupProgress
	err      error
}

func NewSetup(facade SetupFacade, start SetupStart, options Options) *SetupModel {
	if options.Context == nil {
		options.Context = context.Background()
	}
	m := &SetupModel{ctx: options.Context, facade: facade, options: options, start: start}
	switch start.Mode {
	case "managed":
		m.progress = SetupProgress{Stage: SetupCore, Mode: "managed"}
		m.busy = true
	case "adopt":
		if start.ProxyPath == "" {
			m.progress = SetupProgress{Stage: SetupAdoptPaths, Mode: "adopt"}
		} else {
			m.progress = SetupProgress{Stage: SetupCore, Mode: "adopt"}
			m.busy = true
		}
	default:
		m.progress = SetupProgress{Stage: SetupChooseMode}
	}
	return m
}

func (m *SetupModel) Init() tea.Cmd {
	if !m.busy {
		return nil
	}
	return m.execute(SetupAction{Kind: SetupActionCore, Mode: m.progress.Mode, ProxyPath: m.start.ProxyPath, ConfigPath: m.start.ConfigPath, Harden: m.start.Harden})
}

func (m *SetupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case setupProgressMsg:
		m.busy = false
		m.cancel = nil
		if msg.err != nil {
			m.lastErr = safeText(msg.err.Error())
			return m, nil
		}
		m.lastErr = ""
		m.progress = msg.progress
		m.selected = 0
		return m, nil
	case tea.KeyMsg:
		return m.updateKey(msg)
	default:
		return m, nil
	}
}

func (m *SetupModel) updateKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.busy {
		if key.Type == tea.KeyCtrlC || key.Type == tea.KeyEsc {
			return m.cancelJourney()
		}
		return m, nil
	}
	if key.Type == tea.KeyCtrlC || key.String() == "q" || key.Type == tea.KeyEsc {
		return m.cancelJourney()
	}
	if m.progress.Stage == SetupComplete {
		return m, tea.Quit
	}
	if m.progress.Stage == SetupAdoptPaths {
		return m.updateAdoptPaths(key)
	}
	if key.Type == tea.KeyUp || key.String() == "k" {
		m.move(-1)
		return m, nil
	}
	if key.Type == tea.KeyDown || key.String() == "j" {
		m.move(1)
		return m, nil
	}
	if len(key.Runes) == 1 && key.Runes[0] >= '1' && key.Runes[0] <= '9' {
		index := int(key.Runes[0] - '1')
		if index < m.choiceCount() {
			m.selected = index
		}
		return m, nil
	}
	if key.Type != tea.KeyEnter {
		return m, nil
	}
	return m.activate()
}

func (m *SetupModel) activate() (tea.Model, tea.Cmd) {
	switch m.progress.Stage {
	case SetupChooseMode:
		if m.selected == 0 {
			m.progress = SetupProgress{Stage: SetupCore, Mode: "managed"}
			m.start.Mode = "managed"
			m.busy = true
			return m, m.execute(SetupAction{Kind: SetupActionCore, Mode: "managed"})
		}
		m.progress = SetupProgress{Stage: SetupAdoptPaths, Mode: "adopt"}
		m.start.Mode = "adopt"
		return m, nil
	case SetupProviderOffer:
		if m.selected == 1 {
			m.finish(false, "Provider authentication was skipped.", "pmux providers login <provider>", "pmux models list --refresh", "pmux launch --client claude --model <id>")
			return m, tea.Quit
		}
		m.busy = true
		return m, m.execute(SetupAction{Kind: SetupActionLoadProviders})
	case SetupProviderSelect:
		if len(m.progress.Providers) == 0 {
			return m, nil
		}
		m.busy = true
		m.progress.Stage = SetupProviderAuth
		return m, m.execute(SetupAction{Kind: SetupActionLoginProvider, Provider: m.progress.Providers[m.selected].ID})
	case SetupModelSelect:
		if len(m.progress.Models) == 0 {
			return m, nil
		}
		model := m.progress.Models[m.selected].ID
		m.progress.SelectedModel = model
		m.progress.Stage = SetupClientPreflight
		m.busy = true
		return m, m.execute(SetupAction{Kind: SetupActionPreflight, Model: model})
	case SetupLaunchOffer:
		if m.selected == 0 {
			m.launch = true
			m.finish(false, "Setup is ready to launch Claude Code.")
		} else {
			m.finish(false, "Claude Code launch was skipped.", "pmux launch --client claude --model "+m.progress.SelectedModel)
		}
		return m, tea.Quit
	default:
		return m, nil
	}
}

func (m *SetupModel) updateAdoptPaths(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if key.Type == tea.KeyTab {
		m.field = (m.field + 1) % 2
		return m, nil
	}
	if key.Type == tea.KeyBackspace || key.Type == tea.KeyDelete {
		target := &m.start.ProxyPath
		if m.field == 1 {
			target = &m.start.ConfigPath
		}
		runes := []rune(*target)
		if len(runes) > 0 {
			*target = string(runes[:len(runes)-1])
		}
		return m, nil
	}
	if key.Type == tea.KeyEnter {
		if m.field == 0 {
			if strings.TrimSpace(m.start.ProxyPath) == "" {
				m.lastErr = "An absolute CLIProxyAPI executable path is required."
				return m, nil
			}
			m.field = 1
			return m, nil
		}
		m.busy = true
		m.progress.Stage = SetupCore
		return m, m.execute(SetupAction{Kind: SetupActionCore, Mode: "adopt", ProxyPath: m.start.ProxyPath, ConfigPath: m.start.ConfigPath, Harden: m.start.Harden})
	}
	target := &m.start.ProxyPath
	if m.field == 1 {
		target = &m.start.ConfigPath
	}
	for _, r := range key.Runes {
		if r >= 0x20 && r != 0x7f {
			*target += string(r)
		}
	}
	return m, nil
}

func (m *SetupModel) cancelJourney() (tea.Model, tea.Cmd) {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.canceled = true
	if m.progress.CoreComplete {
		next := []string{"pmux providers login <provider>", "pmux models list --refresh", "pmux launch --client claude --model <id>"}
		if m.progress.SelectedModel != "" {
			next = []string{"pmux launch --client claude --model " + m.progress.SelectedModel}
		}
		m.finish(true, "Onboarding was canceled; the completed CLIProxyAPI setup remains valid.", next...)
	} else {
		m.progress = SetupProgress{Stage: SetupComplete, Mode: m.progress.Mode, NextActions: []string{"pmux setup"}}
	}
	return m, tea.Quit
}

func (m *SetupModel) finish(canceled bool, message string, next ...string) {
	m.canceled = canceled
	m.progress.Stage = SetupComplete
	m.progress.Messages = append(m.progress.Messages, message)
	m.progress.NextActions = append([]string(nil), next...)
}

func (m *SetupModel) move(delta int) {
	count := m.choiceCount()
	if count == 0 {
		return
	}
	m.selected = (m.selected + delta + count) % count
}

func (m *SetupModel) choiceCount() int {
	switch m.progress.Stage {
	case SetupChooseMode, SetupProviderOffer, SetupLaunchOffer:
		return 2
	case SetupProviderSelect:
		return len(m.progress.Providers)
	case SetupModelSelect:
		return len(m.progress.Models)
	default:
		return 0
	}
}

func (m *SetupModel) execute(action SetupAction) tea.Cmd {
	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	return func() tea.Msg {
		if m.facade == nil {
			return setupProgressMsg{err: fmt.Errorf("setup application services are unavailable")}
		}
		progress, err := m.facade.Execute(ctx, action)
		return setupProgressMsg{progress: progress, err: err}
	}
}

func (m *SetupModel) View() string {
	var b strings.Builder
	b.WriteString("PMux setup\n\n")
	if m.busy {
		b.WriteString("Working: ")
		b.WriteString(stageLabel(m.progress.Stage))
		b.WriteByte('\n')
	}
	if m.lastErr != "" {
		b.WriteString("Error: ")
		b.WriteString(safeText(m.lastErr))
		b.WriteByte('\n')
	}
	for _, message := range m.progress.Messages {
		if message != "" {
			b.WriteString(safeText(message))
			b.WriteByte('\n')
		}
	}
	switch m.progress.Stage {
	case SetupChooseMode:
		b.WriteString("Choose how to configure CLIProxyAPI:\n")
		m.renderChoices(&b, []SetupChoice{{ID: "managed", Label: "Install a managed copy (recommended)"}, {ID: "adopt", Label: "Adopt an existing installation"}})
	case SetupAdoptPaths:
		b.WriteString("Adopt an existing CLIProxyAPI installation (read-only by default).\n")
		m.renderField(&b, 0, "Proxy executable", m.start.ProxyPath)
		m.renderField(&b, 1, "Config path (optional when discoverable)", m.start.ConfigPath)
		b.WriteString("Tab switches fields; Enter continues.\n")
	case SetupProviderOffer:
		b.WriteString("CLIProxyAPI core and service are ready. Authenticate a provider now?\n")
		m.renderChoices(&b, []SetupChoice{{ID: "yes", Label: "Authenticate a provider"}, {ID: "no", Label: "Skip for now"}})
	case SetupProviderSelect:
		b.WriteString("Choose a provider to authenticate:\n")
		m.renderChoices(&b, m.progress.Providers)
	case SetupProviderAuth:
		b.WriteString("Waiting for provider authentication. Follow the displayed verification instructions.\n")
	case SetupModelSelect:
		b.WriteString("Choose one exact dynamically discovered model:\n")
		m.renderChoices(&b, m.progress.Models)
	case SetupClientPreflight:
		b.WriteString("Checking Claude Code v2 and the selected model.\n")
	case SetupLaunchOffer:
		fmt.Fprintf(&b, "Claude Code %s (%s) is ready with exact model %s. Launch now?\n", safeText(m.progress.ClientVersion), safeText(m.progress.ClientPath), safeText(m.progress.SelectedModel))
		m.renderChoices(&b, []SetupChoice{{ID: "yes", Label: "Launch Claude Code now"}, {ID: "no", Label: "Finish without launching"}})
	case SetupComplete:
		if m.progress.CoreComplete {
			b.WriteString("CLIProxyAPI core setup is complete.\n")
		}
		for _, action := range m.progress.NextActions {
			b.WriteString("Next: ")
			b.WriteString(safeText(action))
			b.WriteByte('\n')
		}
	}
	if m.progress.Stage != SetupComplete {
		b.WriteString("\nEnter select  Up/Down move  q cancel\n")
	}
	return b.String()
}

func (m *SetupModel) renderChoices(b *strings.Builder, choices []SetupChoice) {
	for index, choice := range choices {
		marker := "  "
		if index == m.selected {
			marker = "> "
		}
		fmt.Fprintf(b, "%s%d. %s", marker, index+1, safeText(choice.Label))
		if choice.Note != "" {
			fmt.Fprintf(b, " — %s", safeText(choice.Note))
		}
		b.WriteByte('\n')
	}
}

func (m *SetupModel) renderField(b *strings.Builder, index int, label, value string) {
	marker := "  "
	if index == m.field {
		marker = "> "
	}
	fmt.Fprintf(b, "%s%s: %s\n", marker, label, safeText(value))
}

func stageLabel(stage SetupStage) string {
	switch stage {
	case SetupCore:
		return "installing or adopting CLIProxyAPI and verifying its service"
	case SetupProviderAuth:
		return "authenticating the selected provider"
	case SetupClientPreflight:
		return "checking Claude Code and the selected model"
	default:
		return string(stage)
	}
}

func (m *SetupModel) Progress() SetupProgress { return m.progress }
func (m *SetupModel) LaunchRequested() bool { return m.launch }
func (m *SetupModel) Canceled() bool { return m.canceled }
