package tui

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	MinimumWidth  = 80
	MinimumHeight = 24
)

type Options struct {
	InitialScreen Screen
	Context       context.Context
	Setup         SetupFacade
	NoColor       bool
	HighContrast  bool
	Plain         bool
	ReducedMotion bool
}

type Shell struct {
	ctx             context.Context
	facade          Facade
	snapshot        Snapshot
	screen          Screen
	width           int
	height          int
	selected        [9]int
	focus           int
	help            bool
	search          bool
	query           string
	input           *inputPrompt
	busy            bool
	cancel          context.CancelFunc
	lastErr         string
	actionEvent     ActionEvent
	pending         *ActionMeta
	pendingArgs     []string
	pendingSecret   []byte
	handoff         *ActionRequest
	launchClient    string
	protectedWriter *io.PipeWriter
	confirm         string
	palette         palette
	options         Options
}

type facadeResultMsg struct {
	snapshot Snapshot
	err      error
}

type facadeEventMsg struct {
	event   ActionEvent
	events  <-chan ActionEvent
	results <-chan facadeResultMsg
}
type protectedInputSubmittedMsg struct{ err error }

func New(facade Facade, cached Snapshot, options Options) *Shell {
	if options.Context == nil {
		options.Context = context.Background()
	}
	if _, present := os.LookupEnv("NO_COLOR"); present {
		options.NoColor = true
	}
	if os.Getenv("TERM") == "dumb" {
		options.Plain = true
		options.NoColor = true
	}
	if os.Getenv("PMUX_NO_ANIMATION") == "1" {
		options.ReducedMotion = true
	}
	return &Shell{
		ctx:      options.Context,
		facade:   facade,
		snapshot: cached,
		screen:   options.InitialScreen,
		width:    MinimumWidth,
		height:   MinimumHeight,
		palette:  makePalette(options.NoColor, options.HighContrast, options.Plain),
		options:  options,
	}
}

// Init intentionally returns no command. Startup is cache-only and therefore
// cannot perform network, filesystem, service, or subprocess work.
func (m *Shell) Init() tea.Cmd { return nil }

func (m *Shell) Screen() Screen      { return m.screen }
func (m *Shell) Snapshot() Snapshot  { return m.snapshot }
func (m *Shell) Busy() bool          { return m.busy }
func (m *Shell) SearchQuery() string { return m.query }

// TakeHandoff returns the launch request recorded by the final TUI action.
// The composition root must call it only after tea.Program.Run has returned,
// so Bubble Tea has restored the terminal before a launcher may exec.
func (m *Shell) TakeHandoff() (ActionRequest, bool) {
	if m.handoff == nil {
		return ActionRequest{}, false
	}
	request := ActionRequest{
		ID:        m.handoff.ID,
		Arguments: append([]string(nil), m.handoff.Arguments...),
		Options:   maps.Clone(m.handoff.Options),
	}
	m.handoff = nil
	return request, true
}

func (m *Shell) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case facadeEventMsg:
		m.actionEvent = msg.event
		delay := time.Duration(0)
		if msg.event.Type == "log" {
			delay = 250 * time.Millisecond
		}
		if msg.event.Type == "protected_input_required" {
			m.input = &inputPrompt{
				action:   ActionProviderLogin,
				label:    valueOrText(msg.event.Message, "Paste the full callback URL from your browser's address bar:"),
				secret:   true,
				response: m.protectedWriter,
			}
		}
		return m, waitFacadeMessage(msg.events, msg.results, delay)
	case protectedInputSubmittedMsg:
		m.protectedWriter = nil
		if msg.err != nil {
			if m.cancel != nil {
				m.cancel()
			}
			m.lastErr = safeText(msg.err.Error())
		}
		return m, nil
	case facadeResultMsg:
		m.busy = false
		m.cancel = nil
		if m.protectedWriter != nil {
			_ = m.protectedWriter.Close()
			m.protectedWriter = nil
		}
		m.actionEvent = ActionEvent{}
		if msg.err != nil {
			m.lastErr = safeText(msg.err.Error())
			return m, nil
		}
		m.lastErr = ""
		m.snapshot = msg.snapshot
		m.clampSelection()
		return m, nil
	case tea.KeyMsg:
		return m.updateKey(msg)
	default:
		return m, nil
	}
}

func (m *Shell) updateKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.input != nil {
		return m.updateInput(key)
	}
	if key.Type == tea.KeyCtrlC {
		if m.pending != nil {
			clearSecret(m.pendingSecret)
			m.pendingSecret = nil
			m.pending = nil
			m.pendingArgs = nil
			m.confirm = ""
			m.lastErr = "Canceled; no changes were made."
			return m, nil
		}
		if m.busy && m.cancel != nil {
			m.cancel()
			m.lastErr = "Cancel requested; waiting for the operation to stop."
			return m, nil
		}
		return m, tea.Quit
	}
	if key.Type == tea.KeyEsc && m.busy && m.cancel != nil {
		m.cancel()
		if m.protectedWriter != nil {
			_ = m.protectedWriter.CloseWithError(context.Canceled)
			m.protectedWriter = nil
		}
		m.lastErr = "Cancel requested; waiting for the operation to stop."
		return m, nil
	}
	if m.pending != nil {
		return m.updateConfirmation(key)
	}
	if m.help {
		if key.Type == tea.KeyEsc || key.String() == "?" || key.String() == "q" {
			m.help = false
		}
		return m, nil
	}
	if m.search {
		return m.updateSearch(key)
	}
	if m.tooSmall() {
		if key.String() == "q" {
			return m, tea.Quit
		}
		return m, nil
	}

	switch key.String() {
	case "?":
		m.help = true
		return m, nil
	case "/":
		m.search = true
		m.query = ""
		return m, nil
	case "tab":
		m.focus = (m.focus + 1) % 2
		return m, nil
	case "shift+tab":
		m.focus = (m.focus + 1) % 2
		return m, nil
	case "esc":
		m.query = ""
		return m, nil
	case "q":
		return m, tea.Quit
	case "up", "k":
		m.move(-1)
		return m, nil
	case "down", "j":
		m.move(1)
		return m, nil
	case "home", "g":
		m.selected[m.screen] = 0
		return m, nil
	case "end", "G":
		m.selected[m.screen] = max(0, m.rowCount()-1)
		return m, nil
	}
	if len(key.Runes) == 1 && key.Runes[0] >= '1' && key.Runes[0] <= '9' {
		m.screen = Screen(key.Runes[0] - '1')
		m.focus = 0
		m.query = ""
		return m, nil
	}
	id, ok := m.actionForKey(key.String())
	if !ok {
		return m, nil
	}
	return m.request(id)
}

func (m *Shell) updateSearch(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.Type {
	case tea.KeyEsc:
		if m.query != "" {
			m.query = ""
		} else {
			m.search = false
		}
		return m, nil
	case tea.KeyEnter:
		m.search = false
		m.selected[m.screen] = 0
		return m, nil
	case tea.KeyBackspace, tea.KeyDelete:
		r := []rune(m.query)
		if len(r) > 0 {
			m.query = string(r[:len(r)-1])
		}
		return m, nil
	}
	for _, r := range key.Runes {
		if unicode.IsPrint(r) {
			m.query += string(r)
		}
	}
	return m, nil
}

func (m *Shell) updateConfirmation(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if key.Type == tea.KeyEsc {
		m.pending = nil
		clearSecret(m.pendingSecret)
		m.pendingSecret = nil
		m.pendingArgs = nil
		m.confirm = ""
		m.lastErr = "Canceled; no changes were made."
		return m, nil
	}
	if key.Type == tea.KeyBackspace || key.Type == tea.KeyDelete {
		r := []rune(m.confirm)
		if len(r) > 0 {
			m.confirm = string(r[:len(r)-1])
		}
		return m, nil
	}
	if key.Type == tea.KeyEnter {
		if m.confirm != confirmationPhrase(m.pending.ID) {
			return m, nil
		}
		id := m.pending.ID
		arguments := append([]string(nil), m.pendingArgs...)
		secret := append([]byte(nil), m.pendingSecret...)
		clearSecret(m.pendingSecret)
		m.pending = nil
		m.pendingArgs = nil
		m.pendingSecret = nil
		m.confirm = ""
		return m.executeWithArguments(id, arguments, secret)
	}
	for _, r := range key.Runes {
		if unicode.IsPrint(r) {
			m.confirm += string(r)
		}
	}
	return m, nil
}

// launchClients is the fixed cycle order for the Launch screen client picker.
var launchClients = []string{"claude", "codex", "gemini", "opencode"}

func (m *Shell) launchClientOrDefault() string {
	if m.launchClient != "" {
		return m.launchClient
	}
	if m.snapshot.Launch.Client != "" {
		return m.snapshot.Launch.Client
	}
	return "claude"
}

func (m *Shell) cycleLaunchClient() {
	current := m.launchClientOrDefault()
	next := launchClients[0]
	for index, candidate := range launchClients {
		if candidate == current {
			next = launchClients[(index+1)%len(launchClients)]
			break
		}
	}
	m.launchClient = next
	m.lastErr = ""
}

func (m *Shell) request(id ActionID) (tea.Model, tea.Cmd) {
	meta, ok := Action(id)
	if !ok {
		m.lastErr = "This action has no canonical CLI equivalent."
		return m, nil
	}
	if id == ActionSetup {
		if m.options.Setup == nil {
			m.lastErr = "Setup is unavailable: no setup application facade is configured."
			return m, nil
		}
		setup := NewSetup(m.options.Setup, SetupStart{}, m.options)
		return setup, setup.Init()
	}
	if id == ActionConfigSet && m.selectedConfigSensitive() {
		m.lastErr = "Sensitive proxy fields cannot be entered here; use the protected provider login flow."
		return m, nil
	}
	if id == ActionLaunchClient {
		m.cycleLaunchClient()
		return m, nil
	}
	if id == ActionProviderLogin {
		if prompt, needsInput := m.providerLoginInput(); needsInput {
			m.input = &prompt
			m.lastErr = ""
			return m, nil
		}
	}
	arguments := m.selectedArguments(id)
	if isLaunchHandoff(id) && len(arguments) == 0 {
		m.lastErr = "No model is available. Authenticate a provider and refresh Models before launching."
		return m, nil
	}
	if prompt, needsInput := inputFor(id, arguments); needsInput {
		m.input = &prompt
		m.lastErr = ""
		return m, nil
	}
	return m.confirmOrExecute(meta, arguments, nil)
}

func (m *Shell) confirmOrExecute(meta ActionMeta, arguments []string, secret []byte) (tea.Model, tea.Cmd) {
	if meta.Mutating {
		m.pending = &meta
		m.pendingArgs = append([]string(nil), arguments...)
		m.pendingSecret = append([]byte(nil), secret...)
		clearSecret(secret)
		m.confirm = ""
		return m, nil
	}
	return m.executeWithArguments(meta.ID, arguments, secret)
}

func (m *Shell) executeWithArguments(id ActionID, arguments []string, secret []byte) (tea.Model, tea.Cmd) {
	if m.busy {
		m.lastErr = "Already running: " + safeText(string(id)) + "."
		clearSecret(secret)
		return m, nil
	}
	if isLaunchHandoff(id) {
		request := ActionRequest{ID: id, Arguments: append([]string(nil), arguments...), Options: map[string]string{"client": m.launchClientOrDefault()}}
		clearSecret(secret)
		m.handoff = &request
		m.lastErr = ""
		return m, tea.Quit
	}
	if m.facade == nil {
		m.lastErr = "This action is unavailable: no application facade is configured."
		return m, nil
	}
	events := make(chan ActionEvent, 16)
	results := make(chan facadeResultMsg, 1)
	var protectedReader io.Reader
	if id == ActionProviderLogin && len(arguments) > 1 && (arguments[1] == "browser" || arguments[1] == "paste_callback") {
		reader, writer := io.Pipe()
		protectedReader = reader
		m.protectedWriter = writer
	}
	request := ActionRequest{
		ID:             id,
		Arguments:      append([]string(nil), arguments...),
		Secret:         append([]byte(nil), secret...),
		Events:         events,
		ProtectedInput: protectedReader,
	}
	clearSecret(secret)
	operationContext, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	m.busy = true
	m.actionEvent = ActionEvent{Type: "started", Message: "Starting " + string(id)}
	m.lastErr = ""
	return m, func() tea.Msg {
		go func() {
			snapshot, err := m.facade.Execute(operationContext, request)
			clearSecret(request.Secret)
			close(events)
			results <- facadeResultMsg{snapshot: snapshot, err: err}
		}()
		return waitFacadeMessage(events, results, 0)()
	}
}

func isLaunchHandoff(id ActionID) bool {
	return id == ActionModelLaunch || id == ActionLaunchRun
}

func waitFacadeMessage(events <-chan ActionEvent, results <-chan facadeResultMsg, delay time.Duration) tea.Cmd {
	return func() tea.Msg {
		if delay > 0 {
			timer := time.NewTimer(delay)
			<-timer.C
			timer.Stop()
		}
		select {
		case event, ok := <-events:
			if ok {
				return facadeEventMsg{event: event, events: events, results: results}
			}
		default:
		}
		select {
		case event, ok := <-events:
			if ok {
				return facadeEventMsg{event: event, events: events, results: results}
			}
			return <-results
		case result := <-results:
			return result
		}
	}
}
func valueOrText(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func (m *Shell) selectedArguments(id ActionID) []string {
	switch m.screen {
	case Providers:
		rows := m.filteredProviders()
		if len(rows) > 0 {
			return []string{rows[min(m.selected[m.screen], len(rows)-1)].ID}
		}
	case Models:
		rows := m.filteredModels()
		if len(rows) > 0 {
			modelID := rows[min(m.selected[m.screen], len(rows)-1)].ID
			if id == ActionModelLaunch {
				arguments := make([]string, 0, len(m.snapshot.Launch.Arguments)+1)
				arguments = append(arguments, modelID)
				arguments = append(arguments, m.snapshot.Launch.Arguments...)
				return arguments
			}
			return []string{modelID}
		}
	case Launch:
		if m.snapshot.Launch.ModelID != "" {
			arguments := make([]string, 0, len(m.snapshot.Launch.Arguments)+1)
			arguments = append(arguments, m.snapshot.Launch.ModelID)
			arguments = append(arguments, m.snapshot.Launch.Arguments...)
			return arguments
		}
	case Doctor:
		rows := m.filteredDoctor()
		if len(rows) > 0 {
			return []string{rows[min(m.selected[m.screen], len(rows)-1)].ID}
		}
	case Config:
		rows := m.filteredConfig()
		if len(rows) > 0 {
			return []string{rows[min(m.selected[m.screen], len(rows)-1)].Key}
		}
	case Settings:
		rows := m.filteredSettings()
		if len(rows) > 0 {
			return []string{rows[min(m.selected[m.screen], len(rows)-1)].Key}
		}
	case Logs:
		rows := m.filteredLogs()
		if len(rows) > 0 {
			return []string{rows[min(m.selected[m.screen], len(rows)-1)].Source}
		}
	}
	return nil
}
func (m *Shell) selectedConfigSensitive() bool {
	rows := m.filteredConfig()
	if len(rows) == 0 {
		return false
	}
	return rows[min(m.selected[Config], len(rows)-1)].Sensitive
}

func confirmationPhrase(id ActionID) string {
	switch id {
	case ActionProviderDisable:
		return "disable"
	case ActionProviderRemove:
		return "remove"
	case ActionServiceStop:
		return "stop"
	case ActionServiceRestart:
		return "restart"
	case ActionServiceUninstall:
		return "uninstall"
	case ActionDoctorFix, ActionDoctorFixAll:
		return "fix"
	case ActionLogsClear:
		return "clear-logs"
	case ActionConfigRestore, ActionSettingsRestore:
		return "restore"
	default:
		return "apply"
	}
}

func (m *Shell) actionForKey(key string) (ActionID, bool) {
	switch m.screen {
	case Dashboard:
		if key == "r" {
			return ActionDashboardRefresh, true
		}
		if key == "enter" && m.snapshot.Dashboard.Recommended != "" {
			return m.snapshot.Dashboard.Recommended, true
		}
	case Providers:
		switch key {
		case "r":
			return ActionProvidersList, true
		case "enter":
			return ActionProviderDetails, true
		case "a":
			return ActionProviderLogin, true
		case "v":
			return ActionProviderVerify, true
		case "e":
			return ActionProviderEnable, true
		case "x":
			return ActionProviderDisable, true
		case "delete":
			return ActionProviderRemove, true
		}
	case Models:
		switch key {
		case "r":
			return ActionModelsList, true
		case "enter":
			return ActionModelDetails, true
		case "t":
			return ActionModelTest, true
		case "l":
			return ActionModelLaunch, true
		case "f":
			rows := m.filteredModels()
			if len(rows) > 0 && rows[min(m.selected[m.screen], len(rows)-1)].Favorite {
				return ActionModelUnfavorite, true
			}
			return ActionModelFavorite, true
		}
	case Launch:
		switch key {
		case "enter":
			return ActionLaunchRun, true
		case "c":
			return ActionLaunchClient, true
		case "p":
			return ActionLaunchPersist, true
		case "d":
			return ActionLaunchDoctor, true
		}
	case Doctor:
		switch key {
		case "r":
			return ActionDoctorRun, true
		case "enter":
			return ActionDoctorDetails, true
		case "f":
			return ActionDoctorFix, true
		case "a":
			return ActionDoctorFixAll, true
		case "b":
			return ActionDoctorBundle, true
		}
	case Service:
		switch key {
		case "v":
			return ActionServiceStatus, true
		case "s":
			return ActionServiceStart, true
		case "x":
			return ActionServiceStop, true
		case "r":
			return ActionServiceRestart, true
		case "i":
			return ActionServiceInstall, true
		case "u":
			return ActionServiceUninstall, true
		case "l":
			return ActionServiceLogs, true
		case "f":
			return ActionServiceForeground, true
		}
	case Config:
		switch key {
		case "enter":
			return ActionConfigGet, true
		case "w":
			return ActionConfigSet, true
		case "e":
			return ActionConfigEdit, true
		case "b":
			return ActionConfigBackup, true
		case "r":
			return ActionConfigRestore, true
		case "v":
			return ActionConfigShow, true
		}
	case Settings:
		switch key {
		case "enter":
			return ActionSettingsGet, true
		case "p":
			return ActionSettingsSet, true
		case "b":
			return ActionSettingsBackup, true
		case "r":
			return ActionSettingsRestore, true
		case "v":
			return ActionSettingsShow, true
		}
	case Logs:
		switch key {
		case "r":
			return ActionLogsList, true
		case " ":
			return ActionLogsFollow, true
		case "e":
			return ActionLogsExport, true
		case "delete":
			return ActionLogsClear, true
		}
	}
	return "", false
}

func (m *Shell) move(delta int) {
	count := m.rowCount()
	if count == 0 {
		return
	}
	next := m.selected[m.screen] + delta
	if next < 0 {
		next = count - 1
	}
	if next >= count {
		next = 0
	}
	m.selected[m.screen] = next
}

func (m *Shell) clampSelection() {
	for screen := Dashboard; screen <= Logs; screen++ {
		old := m.screen
		m.screen = screen
		count := m.rowCount()
		if count == 0 {
			m.selected[screen] = 0
		} else if m.selected[screen] >= count {
			m.selected[screen] = count - 1
		}
		m.screen = old
	}
}

func (m *Shell) rowCount() int {
	switch m.screen {
	case Providers:
		return len(m.filteredProviders())
	case Models:
		return len(m.filteredModels())
	case Doctor:
		return len(m.filteredDoctor())
	case Config:
		return len(m.filteredConfig())
	case Settings:
		return len(m.filteredSettings())
	case Logs:
		return len(m.filteredLogs())
	default:
		return 1
	}
}

func (m *Shell) tooSmall() bool { return m.width < MinimumWidth || m.height < MinimumHeight }

func (m *Shell) View() string {
	if m.tooSmall() {
		return fmt.Sprintf("terminal too small (need %dx%d, have %dx%d)\n", MinimumWidth, MinimumHeight, m.width, m.height)
	}
	if m.options.Plain {
		return m.plainView()
	}
	var b strings.Builder
	b.WriteString(m.header())
	b.WriteByte('\n')
	if m.search {
		b.WriteString("Search: " + safeText(m.query) + "\n")
	}
	if m.input != nil {
		b.WriteString(m.inputView() + "\n")
	}
	if m.busy {
		b.WriteString(Status{Kind: StatusWorking, Text: "Working"}.Label(m.options.ReducedMotion) + "\n")
	}
	if m.busy && m.actionEvent.Type != "" {
		b.WriteString(m.progressView() + "\n")
	}
	if m.lastErr != "" {
		b.WriteString(m.palette.error.Render("× Error: "+safeText(m.lastErr)) + "\n")
	}
	b.WriteString(m.screenView())
	if m.help {
		b.WriteString("\n" + m.helpView())
	}
	if m.pending != nil {
		b.WriteString("\n" + m.confirmationView())
	}
	b.WriteString("\n" + m.footer())
	return b.String()
}

func (m *Shell) plainView() string {
	var b strings.Builder
	b.WriteString("PMux ")
	b.WriteString(safeText(m.snapshot.Version))
	b.WriteString(" - ")
	b.WriteString(m.screen.String())
	b.WriteByte('\n')
	if m.lastErr != "" {
		b.WriteString("Error: " + safeText(m.lastErr) + "\n")
	}
	if m.input != nil {
		b.WriteString(m.inputView() + "\n")
	}
	if m.busy && m.actionEvent.Type != "" {
		b.WriteString(m.progressView() + "\n")
	}
	b.WriteString(m.screenView())
	if m.help {
		b.WriteString("\n" + m.helpView())
	}
	if m.pending != nil {
		b.WriteString("\n" + m.confirmationView())
	}
	return b.String()
}

func (m *Shell) progressView() string {
	event := m.actionEvent
	var lines []string
	if event.Message != "" {
		lines = append(lines, safeText(event.Message))
	} else {
		lines = append(lines, safeText(strings.ReplaceAll(event.Type, "_", " ")))
	}
	for _, key := range []string{"provider", "flow", "url", "verification_uri", "user_code", "status", "source", "level"} {
		if value := event.Fields[key]; value != "" {
			lines = append(lines, safeText(strings.ReplaceAll(key, "_", " "))+": "+safeText(value))
		}
	}
	lines = append(lines, "Esc or Ctrl+C cancels this operation.")
	return strings.Join(lines, "\n")
}

func (m *Shell) header() string {
	var tabs []string
	for screen := Dashboard; screen <= Logs; screen++ {
		label := strconv.Itoa(int(screen)+1) + " " + screen.String()
		if screen == m.screen {
			label = "▸ " + label
			label = m.palette.active.Render(label)
		}
		tabs = append(tabs, label)
	}
	version := safeText(m.snapshot.Version)
	if version == "" {
		version = "dev"
	}
	contextName := safeText(m.snapshot.Context)
	if contextName == "" {
		contextName = "Local"
	}
	return m.palette.title.Render("PMux "+version+" — "+m.screen.String()+" — "+contextName) + "\n" + strings.Join(tabs, "  ")
}

func (m *Shell) screenView() string {
	switch m.screen {
	case Dashboard:
		return m.dashboardView()
	case Providers:
		return m.providersView()
	case Models:
		return m.modelsView()
	case Launch:
		return m.launchView()
	case Doctor:
		return m.doctorView()
	case Service:
		return m.serviceView()
	case Config:
		return m.configView()
	case Settings:
		return m.settingsView()
	case Logs:
		return m.logsView()
	default:
		return "Unknown screen."
	}
}

func (m *Shell) dashboardView() string {
	d := m.snapshot.Dashboard
	var b strings.Builder
	line(&b, "CLIProxyAPI", joinNonempty(d.Installation, d.CoreVersion))
	line(&b, "Service", d.Service.Label(m.options.ReducedMotion))
	line(&b, "Config path", d.ConfigPath)
	line(&b, "Auth directory", d.AuthDir)
	line(&b, "Bind", d.Bind)
	line(&b, "Proxy health", d.Health.Label(m.options.ReducedMotion))
	line(&b, "Claude Code", d.Claude.Label(m.options.ReducedMotion))
	line(&b, "Providers", strconv.Itoa(d.Providers))
	line(&b, "Accounts", strconv.Itoa(d.Accounts))
	line(&b, "Available models", strconv.Itoa(d.Models))
	if len(d.RecentErrors) == 0 {
		line(&b, "Recent errors", "No recent errors.")
	} else {
		line(&b, "Recent errors", strings.Join(d.RecentErrors, "; "))
	}
	if len(d.Warnings) == 0 {
		line(&b, "Security warnings", "No security warnings detected.")
	} else {
		line(&b, "Security warnings", strings.Join(d.Warnings, "; "))
	}
	if d.Recommended == "" {
		line(&b, "> Recommended action", "No action required")
	} else if meta, ok := Action(d.Recommended); ok {
		line(&b, "> Recommended action", meta.Label)
	}
	return b.String()
}

func (m *Shell) providersView() string {
	rows := m.filteredProviders()
	if len(rows) == 0 {
		return "No providers are configured. Press a to add or authenticate a provider.\n"
	}
	var b strings.Builder
	b.WriteString("  Provider              Type              Enabled  Status                 Accounts  Models\n")
	for i, row := range rows {
		focus := rowFocus(i == m.selected[m.screen])
		fmt.Fprintf(&b, "%s %-20s %-17s %-8s %-22s %-9s %d\n", focus, crop(row.Name, 20), crop(row.Kind, 17), yesNo(row.Enabled), crop(row.Status.Label(m.options.ReducedMotion), 22), safeText(row.Accounts), row.Models)
	}
	return b.String()
}

func (m *Shell) modelsView() string {
	rows := m.filteredModels()
	if len(rows) == 0 {
		return "No models are available. Authenticate a provider, then press r to refresh.\n"
	}
	var b strings.Builder
	b.WriteString("  Fav Model ID                        Owner          Provider        Status       Test\n")
	for i, row := range rows {
		favorite := "☆"
		if row.Favorite {
			favorite = "★"
		}
		status := "Available"
		if !row.Available {
			status = "Unavailable"
		}
		if row.Stale {
			status = "Cached stale"
		}
		latency := "—"
		if row.Latency > 0 {
			latency = row.Latency.Round(time.Millisecond).String()
		}
		fmt.Fprintf(&b, "%s %s   %-31s %-14s %-15s %-12s %s\n", rowFocus(i == m.selected[m.screen]), favorite, crop(row.ID, 31), crop(row.Owner, 14), crop(defaultText(row.Provider, "Unknown"), 15), status, latency)
	}
	return b.String()
}

func (m *Shell) launchView() string {
	v := m.snapshot.Launch
	var b strings.Builder
	clientLabel := m.launchClientOrDefault()
	if detail := joinNonempty(v.ClientVersion, v.ClientPath); detail != "" {
		clientLabel += " (" + detail + ")"
	}
	line(&b, "Client", clientLabel)
	line(&b, "Model", v.ModelID)
	line(&b, "Provider", defaultText(v.Provider, "Unknown"))
	line(&b, "Proxy", v.BaseURL)
	line(&b, "Auth token", v.Token.String())
	line(&b, "Working directory", v.WorkingDir)
	line(&b, "Arguments", strings.Join(v.Arguments, " "))
	line(&b, "Persistence", "Process only")
	if v.Ready {
		line(&b, "> Launch", Status{Kind: StatusHealthy, Text: "Ready"}.Label(m.options.ReducedMotion))
	} else {
		line(&b, "> Launch", Status{Kind: StatusError, Text: defaultText(v.Reason, "Not ready")}.Label(m.options.ReducedMotion))
	}
	return b.String()
}

func (m *Shell) doctorView() string {
	rows := m.filteredDoctor()
	if len(rows) == 0 {
		return "Doctor found no problems.\n"
	}
	var b strings.Builder
	b.WriteString("  Status                 Check                    Severity    Summary\n")
	for i, row := range rows {
		fmt.Fprintf(&b, "%s %-22s %-24s %-11s %s\n", rowFocus(i == m.selected[m.screen]), crop(row.Status.Label(m.options.ReducedMotion), 22), crop(row.ID, 24), crop(row.Severity, 11), crop(row.Summary, 40))
	}
	return b.String()
}

func (m *Shell) serviceView() string {
	v := m.snapshot.Service
	var b strings.Builder
	line(&b, "Backend", v.Backend)
	line(&b, "Identity", v.Identity)
	line(&b, "> State", v.Status.Label(m.options.ReducedMotion))
	if v.PID > 0 {
		line(&b, "PID", strconv.Itoa(v.PID))
	}
	line(&b, "Executable", v.BinaryPath)
	line(&b, "Absolute config", v.ConfigPath)
	line(&b, "Runtime directory", v.RuntimeDir)
	line(&b, "Core version", defaultText(v.CoreVersion, "unknown"))
	if v.Warning != "" {
		line(&b, "Warning", v.Warning)
	}
	return b.String()
}

func (m *Shell) configView() string {
	rows := m.filteredConfig()
	if len(rows) == 0 {
		return "Configuration is empty. Press d to run Doctor.\n"
	}
	var b strings.Builder
	b.WriteString("  Key                                  Value                         Activation\n")
	for i, row := range rows {
		value := row.Value
		if row.Sensitive {
			value = "********"
		}
		fmt.Fprintf(&b, "%s %-36s %-29s %s\n", rowFocus(i == m.selected[m.screen]), crop(row.Key, 36), crop(value, 29), safeText(row.Activation))
	}
	return b.String()
}

func (m *Shell) settingsView() string {
	rows := m.filteredSettings()
	if len(rows) == 0 {
		return "PMux settings do not exist yet; defaults are active.\n"
	}
	var b strings.Builder
	b.WriteString("  Setting                                      Value\n")
	for i, row := range rows {
		fmt.Fprintf(&b, "%s %-44s %s\n", rowFocus(i == m.selected[m.screen]), crop(row.Key, 44), safeText(row.Value))
	}
	line(&b, "Telemetry", "None")
	return b.String()
}

func (m *Shell) logsView() string {
	rows := m.filteredLogs()
	if len(rows) == 0 {
		return "No log entries are available for the selected sources and time range.\n"
	}
	var b strings.Builder
	b.WriteString("  Timestamp             Source        Level   Message\n")
	for i, row := range rows {
		ts := row.Timestamp.Format(time.RFC3339)
		if row.Timestamp.IsZero() {
			ts = "—"
		}
		fmt.Fprintf(&b, "%s %-20s %-13s %-7s %s\n", rowFocus(i == m.selected[m.screen]), ts, crop(row.Source, 13), crop(row.Level, 7), crop(row.Message, 55))
	}
	return b.String()
}

func (m *Shell) footer() string {
	actions := ActionsFor(m.screen)
	parts := []string{"? help", "/ search", "q quit"}
	seen := make(map[string]bool)
	for _, action := range actions {
		item := action.Key + " " + action.Label
		if !seen[item] {
			parts = append(parts, item)
			seen[item] = true
		}
	}
	return strings.Join(parts, "  ")
}

func (m *Shell) helpView() string {
	var b strings.Builder
	b.WriteString("Help — every operational action has a canonical CLI equivalent:\n")
	for _, action := range ActionsFor(m.screen) {
		fmt.Fprintf(&b, "  %-8s %-20s %s\n", action.Key, safeText(action.Label), strings.Join(action.Command, " "))
	}
	b.WriteString("  1-9      primary screens      pmux <bare parent command>\n")
	b.WriteString("  tab      move focus           presentation only\n")
	b.WriteString("  /        local search         presentation only; no network\n")
	return b.String()
}

func (m *Shell) confirmationView() string {
	phrase := confirmationPhrase(m.pending.ID)
	return fmt.Sprintf("Confirm %s\nCommand: %s\nType %s to confirm: %s", safeText(m.pending.Label), strings.Join(m.pending.Command, " "), phrase, safeText(m.confirm))
}

func line(b *strings.Builder, label, value string) {
	fmt.Fprintf(b, "%-24s %s\n", safeText(label), safeText(value))
}

func rowFocus(focused bool) string {
	if focused {
		return ">"
	}
	return " "
}
func yesNo(value bool) string {
	if value {
		return "Yes"
	}
	return "No"
}
func defaultText(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
func joinNonempty(values ...string) string {
	var out []string
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return strings.Join(out, " ")
}
func crop(value string, width int) string {
	value = safeText(value)
	r := []rune(value)
	if len(r) <= width {
		return value
	}
	if width <= 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}

func (m *Shell) matches(values ...string) bool {
	needle := strings.ToLower(m.query)
	if needle == "" {
		return true
	}
	for _, value := range values {
		if strings.Contains(strings.ToLower(safeText(value)), needle) {
			return true
		}
	}
	return false
}

func (m *Shell) filteredProviders() []ProviderRow {
	out := make([]ProviderRow, 0, len(m.snapshot.Providers))
	for _, row := range m.snapshot.Providers {
		if m.matches(row.Name, row.ID, row.Kind, row.Status.Text) {
			out = append(out, row)
		}
	}
	return out
}
func (m *Shell) filteredModels() []ModelRow {
	out := make([]ModelRow, 0, len(m.snapshot.Models))
	for _, row := range m.snapshot.Models {
		if m.matches(row.ID, row.Owner, row.Provider) {
			out = append(out, row)
		}
	}
	return out
}
func (m *Shell) filteredDoctor() []DoctorRow {
	out := make([]DoctorRow, 0, len(m.snapshot.Doctor))
	for _, row := range m.snapshot.Doctor {
		if m.matches(row.ID, row.Summary, row.Severity) {
			out = append(out, row)
		}
	}
	return out
}
func (m *Shell) filteredConfig() []ConfigRow {
	out := make([]ConfigRow, 0, len(m.snapshot.Config))
	for _, row := range m.snapshot.Config {
		if m.matches(row.Key, row.Activation) {
			out = append(out, row)
		}
	}
	return out
}
func (m *Shell) filteredSettings() []SettingRow {
	out := make([]SettingRow, 0, len(m.snapshot.Settings))
	for _, row := range m.snapshot.Settings {
		if m.matches(row.Key, row.Value) {
			out = append(out, row)
		}
	}
	return out
}
func (m *Shell) filteredLogs() []LogRow {
	out := make([]LogRow, 0, len(m.snapshot.Logs))
	for _, row := range m.snapshot.Logs {
		if m.matches(row.Source, row.Level, row.Message, row.RequestID) {
			out = append(out, row)
		}
	}
	return out
}
