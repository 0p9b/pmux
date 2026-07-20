package tui

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type recordingFacade struct {
	calls        []ActionRequest
	result       Snapshot
	err          error
	secretAtCall string
}

func (f *recordingFacade) Execute(_ context.Context, request ActionRequest) (Snapshot, error) {
	f.secretAtCall = string(request.Secret)
	f.calls = append(f.calls, request)
	return f.result, f.err
}

type callbackInputFacade struct {
	result   Snapshot
	callback string
}

func (facade *callbackInputFacade) Execute(_ context.Context, request ActionRequest) (Snapshot, error) {
	request.Events <- ActionEvent{Type: "protected_input_required", Message: "Paste callback URL"}
	value, err := io.ReadAll(request.ProtectedInput)
	if err != nil {
		return facade.result, err
	}
	facade.callback = string(value)
	return facade.result, nil
}

func cachedFixture() Snapshot {
	return Snapshot{
		Version: "test",
		Context: "Local",
		Dashboard: DashboardSnapshot{
			Installation: "Managed",
			CoreVersion:  "runtime-version",
			Service:      Status{Kind: StatusHealthy, Text: "Running"},
			Health:       Status{Kind: StatusHealthy, Text: "Healthy"},
			Claude:       Status{Kind: StatusHealthy, Text: "Installed"},
			Providers:    1,
			Accounts:     1,
			Models:       1,
		},
		Providers: []ProviderRow{{ID: "runtime-provider", Name: "Runtime Provider", Kind: "Device code", Enabled: true, Status: Status{Kind: StatusHealthy, Text: "Authenticated"}, Accounts: "1/1", Models: 1}},
		Models:    []ModelRow{{ID: "runtime-model-from-cache", Owner: "runtime-owner", Provider: "runtime-provider", Available: true, Favorite: true, Latency: 12 * time.Millisecond}},
		Launch:    LaunchSnapshot{ClientPath: "/bin/claude", ClientVersion: "2.0.0", ModelID: "runtime-model-from-cache", Provider: "runtime-provider", BaseURL: "http://127.0.0.1:8317", Token: MaskSecret("sk-0123456789abcdefghijklmnopqrstuvwxyz"), WorkingDir: "/work", Ready: true},
		Doctor:    []DoctorRow{{ID: "CFG-CWD", Status: Status{Kind: StatusHealthy, Text: "Pass"}, Severity: "critical", Summary: "absolute config", Fixable: false}},
		Service:   ServiceSnapshot{Backend: "foreground", Identity: "instance", Status: Status{Kind: StatusHealthy, Text: "Running"}, PID: 42, BinaryPath: "/data/core", ConfigPath: "/data/config.yaml", RuntimeDir: "/data/runtime", CoreVersion: "runtime-version"},
		Config:    []ConfigRow{{Key: "host", Value: "127.0.0.1", Activation: "Restart required"}},
		Settings:  []SettingRow{{Key: "theme", Value: "high-contrast"}},
		Logs:      []LogRow{{Timestamp: time.Unix(1, 0), Source: "pmux", Level: "info", Message: "ready"}},
	}
}

func key(value string) tea.KeyMsg {
	if value == "enter" {
		return tea.KeyMsg{Type: tea.KeyEnter}
	}
	if value == "esc" {
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	if value == "tab" {
		return tea.KeyMsg{Type: tea.KeyTab}
	}
	if value == "up" {
		return tea.KeyMsg{Type: tea.KeyUp}
	}
	if value == "down" {
		return tea.KeyMsg{Type: tea.KeyDown}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(value)}
}

func update(t *testing.T, shell *Shell, msg tea.Msg) (*Shell, tea.Cmd) {
	t.Helper()
	model, cmd := shell.Update(msg)
	updated, ok := model.(*Shell)
	if !ok {
		t.Fatalf("Update returned %T, want *Shell", model)
	}
	return updated, cmd
}

func TestLaunchIsDeferredUntilAfterProgramExit(t *testing.T) {
	fixture := cachedFixture()
	fixture.Launch.Arguments = []string{"--permission-mode", "plan", "argument with spaces"}
	facade := &recordingFacade{result: fixture}
	shell := New(facade, fixture, Options{Plain: true, InitialScreen: Launch})

	shell, quit := update(t, shell, key("enter"))
	if quit == nil {
		t.Fatal("launch did not request Bubble Tea exit")
	}
	_ = quit()
	if len(facade.calls) != 0 {
		t.Fatalf("launch facade ran before Program.Run could restore the terminal: %#v", facade.calls)
	}
	if shell.Busy() {
		t.Fatal("deferred launch incorrectly entered facade busy state")
	}

	request, ok := shell.TakeHandoff()
	if !ok {
		t.Fatal("deferred launch request was not available after exit")
	}
	if request.ID != ActionLaunchRun {
		t.Fatalf("handoff action = %s, want %s", request.ID, ActionLaunchRun)
	}
	want := []string{"runtime-model-from-cache", "--permission-mode", "plan", "argument with spaces"}
	if !reflect.DeepEqual(request.Arguments, want) {
		t.Fatalf("handoff arguments = %#v, want %#v", request.Arguments, want)
	}
	if request.Secret != nil || request.Events != nil {
		t.Fatal("launch handoff carried a secret or streaming channel")
	}
	if _, ok := shell.TakeHandoff(); ok {
		t.Fatal("launch handoff was not consumed exactly once")
	}
}

func TestModelSelectorPreservesInitialPassthroughArguments(t *testing.T) {
	fixture := cachedFixture()
	fixture.Launch.Arguments = []string{"--permission-mode", "plan", "argument with spaces"}
	facade := &recordingFacade{result: fixture}
	shell := New(facade, fixture, Options{Plain: true, InitialScreen: Models})

	shell, quit := update(t, shell, key("l"))
	if quit == nil {
		t.Fatal("selected model did not request Bubble Tea exit")
	}
	_ = quit()
	if len(facade.calls) != 0 {
		t.Fatal("model selector invoked launch before terminal cleanup")
	}
	request, ok := shell.TakeHandoff()
	if !ok || request.ID != ActionModelLaunch {
		t.Fatalf("model handoff = %#v, available=%v", request, ok)
	}
	want := []string{"runtime-model-from-cache", "--permission-mode", "plan", "argument with spaces"}
	if !reflect.DeepEqual(request.Arguments, want) {
		t.Fatalf("model handoff arguments = %#v, want %#v", request.Arguments, want)
	}
}

func TestInitAndFirstModelRenderUseOnlyCachedState(t *testing.T) {
	facade := &recordingFacade{result: cachedFixture()}
	shell := New(facade, cachedFixture(), Options{Plain: true})
	if cmd := shell.Init(); cmd != nil {
		t.Fatal("Init returned a command; startup must be cache-only")
	}
	shell, _ = update(t, shell, key("3"))
	view := shell.View()
	if !strings.Contains(view, "runtime-model-from-cache") {
		t.Fatalf("cached model missing from first model render:\n%s", view)
	}
	if len(facade.calls) != 0 {
		t.Fatalf("first render called facade %d time(s)", len(facade.calls))
	}
}

func TestNumberKeysNavigateEveryPrimaryScreen(t *testing.T) {
	shell := New(nil, cachedFixture(), Options{Plain: true})
	for i := 1; i <= 9; i++ {
		shell, _ = update(t, shell, key(string(rune('0'+i))))
		if shell.Screen() != Screen(i-1) {
			t.Fatalf("key %d selected %s", i, shell.Screen())
		}
		if !strings.Contains(shell.View(), screenNames[i-1]) {
			t.Fatalf("screen %d did not render its title", i)
		}
	}
}

func TestListKeyboardFocusAndSearch(t *testing.T) {
	fixture := cachedFixture()
	fixture.Models = append(fixture.Models, ModelRow{ID: "second-runtime-model", Available: true})
	shell := New(nil, fixture, Options{Plain: true})
	shell, _ = update(t, shell, key("3"))
	shell, _ = update(t, shell, key("down"))
	if shell.selected[Models] != 1 {
		t.Fatalf("down selected %d, want 1", shell.selected[Models])
	}
	shell, _ = update(t, shell, key("/"))
	shell, _ = update(t, shell, key("second"))
	if shell.SearchQuery() != "second" {
		t.Fatalf("query = %q", shell.SearchQuery())
	}
	if strings.Contains(shell.View(), "runtime-model-from-cache") {
		t.Fatal("local search did not filter first model")
	}
	shell, _ = update(t, shell, key("esc"))
	if shell.SearchQuery() != "" {
		t.Fatal("first escape did not clear search")
	}
}

func TestTinyTerminalGuardIsOnlyContent(t *testing.T) {
	shell := New(nil, cachedFixture(), Options{})
	shell, _ = update(t, shell, tea.WindowSizeMsg{Width: 79, Height: 23})
	want := "terminal too small (need 80x24, have 79x23)\n"
	if got := shell.View(); got != want {
		t.Fatalf("tiny view = %q, want %q", got, want)
	}
}

func TestPlainNoColorAndReducedMotion(t *testing.T) {
	shell := New(nil, cachedFixture(), Options{Plain: true, NoColor: true, HighContrast: true, ReducedMotion: true})
	shell.screen = Providers
	shell.busy = true
	view := shell.View()
	if strings.Contains(view, "\x1b[") {
		t.Fatalf("plain/no-color output contains ANSI: %q", view)
	}
	if strings.Contains(view, "⠋") || strings.Contains(view, "⠙") {
		t.Fatalf("reduced-motion output contains animated spinner: %q", view)
	}
	if !strings.Contains(view, "✓ Authenticated") || !strings.Contains(view, ">") {
		t.Fatal("status/focus must remain textual without color")
	}
}

func TestEnvironmentPresentationModes(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("TERM", "dumb")
	t.Setenv("PMUX_NO_ANIMATION", "1")
	shell := New(nil, cachedFixture(), Options{})
	if !shell.options.NoColor || !shell.options.Plain || !shell.options.ReducedMotion {
		t.Fatalf("environment modes not applied: %#v", shell.options)
	}
	if strings.Contains(shell.View(), "\x1b[") {
		t.Fatal("environment-selected plain mode emitted ANSI")
	}
}

func TestSecretsAndTerminalControlsAreNeverRendered(t *testing.T) {
	const proxy = "sk-supersecretproxykeyvalue"
	const bearer = "Bearer complete-management-secret"
	const provider = "provider_api_key=complete-provider-key"
	const callbackCode = "complete-callback-code"
	fixture := cachedFixture()
	fixture.Dashboard.Warnings = []string{proxy, bearer, provider, "http://127.0.0.1/callback?code=" + callbackCode + "&state=secret-state", "\x1b]52;c;attack\x07clipboard"}
	fixture.Config = append(fixture.Config, ConfigRow{Key: "api-key", Value: "complete-config-key", Sensitive: true})
	fixture.Logs = append(fixture.Logs, LogRow{Source: "proxy", Message: bearer + " " + provider})
	for _, screen := range []Screen{Dashboard, Config, Logs} {
		shell := New(nil, fixture, Options{Plain: true})
		shell.screen = screen
		view := shell.View()
		for _, secret := range []string{proxy, "complete-management-secret", "complete-provider-key", "complete-config-key", callbackCode, "secret-state"} {
			if strings.Contains(view, secret) {
				t.Fatalf("%s rendered complete secret %q:\n%s", screen, secret, view)
			}
		}
		if strings.Contains(view, "\x1b]") {
			t.Fatalf("%s rendered terminal control sequence", screen)
		}
	}
}

func TestEveryOperationalActionHasCanonicalCLIParityMetadata(t *testing.T) {
	seen := make(map[ActionID]bool)
	for _, action := range Actions() {
		if action.ID == "" || action.Label == "" || action.Key == "" {
			t.Fatalf("incomplete action metadata: %#v", action)
		}
		if len(action.Command) < 1 || action.Command[0] != "pmux" {
			t.Fatalf("action %s has noncanonical command %#v", action.ID, action.Command)
		}
		machine := action.MachineCommand()
		if len(machine) < 2 || machine[0] != "pmux" || machine[1] != "--json" {
			t.Fatalf("action %s has no canonical machine command: %#v", action.ID, machine)
		}
		command := strings.Join(action.Command, " ")
		for _, forbidden := range []string{" pmux env", " pmux keys", " pmux fleet", " pmux use", "--repair"} {
			if strings.Contains(" "+command, forbidden) {
				t.Fatalf("action %s exposes forbidden command %q", action.ID, command)
			}
		}
		if seen[action.ID] {
			t.Fatalf("duplicate action ID %s", action.ID)
		}
		seen[action.ID] = true
	}
	for screen := Dashboard; screen <= Logs; screen++ {
		if len(ActionsFor(screen)) == 0 {
			t.Fatalf("%s has no command parity metadata", screen)
		}
	}
}

func TestReadActionCallsOnlyInjectedFacadeAfterInput(t *testing.T) {
	facade := &recordingFacade{result: cachedFixture()}
	shell := New(facade, cachedFixture(), Options{Plain: true})
	var cmd tea.Cmd
	shell, cmd = update(t, shell, key("r"))
	if cmd == nil {
		t.Fatal("refresh did not return a facade command")
	}
	if len(facade.calls) != 0 {
		t.Fatal("facade called synchronously from Update")
	}
	msg := cmd()
	if len(facade.calls) != 1 || facade.calls[0].ID != ActionDashboardRefresh {
		t.Fatalf("facade calls = %#v", facade.calls)
	}
	shell, _ = update(t, shell, msg)
	if shell.Busy() {
		t.Fatal("shell remained busy after facade result")
	}
}

func TestMutationRequiresExactConfirmation(t *testing.T) {
	facade := &recordingFacade{result: cachedFixture()}
	shell := New(facade, cachedFixture(), Options{Plain: true})
	shell, _ = update(t, shell, key("6"))
	shell, cmd := update(t, shell, key("x"))
	if cmd != nil || shell.pending == nil {
		t.Fatal("stop should open confirmation without executing")
	}
	shell, cmd = update(t, shell, key("wrong"))
	if cmd != nil {
		t.Fatal("typing wrong confirmation executed mutation")
	}
	shell, cmd = update(t, shell, key("enter"))
	if cmd != nil {
		t.Fatal("wrong phrase submitted mutation")
	}
	shell.confirm = "stop"
	_, cmd = update(t, shell, key("enter"))
	if cmd == nil {
		t.Fatal("exact confirmation did not schedule mutation")
	}
	_ = cmd()
	if len(facade.calls) != 1 || facade.calls[0].ID != ActionServiceStop {
		t.Fatalf("unexpected calls: %#v", facade.calls)
	}
}

func TestValueEntryPrecedesMutationConfirmation(t *testing.T) {
	facade := &recordingFacade{result: cachedFixture()}
	shell := New(facade, cachedFixture(), Options{Plain: true})
	shell, _ = update(t, shell, key("7"))
	shell, cmd := update(t, shell, key("w"))
	if cmd != nil || shell.input == nil || shell.pending != nil {
		t.Fatal("config set must collect a value before confirmation")
	}
	if !strings.Contains(shell.View(), "New value for host") {
		t.Fatalf("value prompt is not visible: %s", shell.View())
	}
	shell, _ = update(t, shell, key("127.0.0.1"))
	shell, cmd = update(t, shell, key("enter"))
	if cmd != nil || shell.input != nil || shell.pending == nil {
		t.Fatal("entered value did not advance to confirmation")
	}
	shell.confirm = "apply"
	_, cmd = update(t, shell, key("enter"))
	if cmd == nil {
		t.Fatal("confirmed value mutation did not schedule facade work")
	}
	_ = cmd()
	if len(facade.calls) != 1 || facade.calls[0].ID != ActionConfigSet {
		t.Fatalf("facade calls = %#v", facade.calls)
	}
	if got := facade.calls[0].Arguments; len(got) != 2 || got[0] != "host" || got[1] != "127.0.0.1" {
		t.Fatalf("config set arguments = %#v", got)
	}
}

func TestValueEntryCancellationDoesNotCallFacade(t *testing.T) {
	facade := &recordingFacade{result: cachedFixture()}
	shell := New(facade, cachedFixture(), Options{Plain: true})
	shell, _ = update(t, shell, key("9"))
	shell, _ = update(t, shell, key("e"))
	if shell.input == nil {
		t.Fatal("log export did not open a path prompt")
	}
	shell, cmd := update(t, shell, key("esc"))
	if cmd != nil || shell.input != nil || len(facade.calls) != 0 {
		t.Fatal("canceling value entry performed work")
	}
}

func TestSensitiveConfigValueEntryIsUnavailable(t *testing.T) {
	fixture := cachedFixture()
	fixture.Config = []ConfigRow{{Key: "remote-management.secret-key", Value: "abc...wxyz", Sensitive: true}}
	facade := &recordingFacade{result: fixture}
	shell := New(facade, fixture, Options{Plain: true, InitialScreen: Config})
	shell, cmd := update(t, shell, key("w"))
	if cmd != nil || shell.input != nil || shell.pending != nil || len(facade.calls) != 0 {
		t.Fatal("sensitive config field opened a generic value path")
	}
	if !strings.Contains(shell.lastErr, "protected provider login") {
		t.Fatalf("guard guidance = %q", shell.lastErr)
	}
}

func TestProtectedProviderInputIsMaskedAndEphemeral(t *testing.T) {
	const secret = "provider-secret-canary"
	fixture := cachedFixture()
	fixture.Providers = []ProviderRow{{ID: "gemini", Name: "Gemini", Flows: []string{"api_key"}}}
	facade := &recordingFacade{result: fixture}
	shell := New(facade, fixture, Options{Plain: true, InitialScreen: Providers})
	shell, cmd := update(t, shell, key("a"))
	if cmd != nil || shell.input == nil || !shell.input.secret {
		t.Fatal("API-key provider did not open protected input")
	}
	shell, _ = update(t, shell, key(secret))
	if strings.Contains(shell.View(), secret) {
		t.Fatal("protected input was rendered in full")
	}
	shell, _ = update(t, shell, key("enter"))
	if shell.pending == nil || strings.Contains(shell.View(), secret) {
		t.Fatal("protected input did not advance safely to confirmation")
	}
	shell.confirm = "apply"
	_, cmd = update(t, shell, key("enter"))
	if cmd == nil {
		t.Fatal("protected provider action was not scheduled")
	}
	_ = cmd()
	if facade.secretAtCall != secret {
		t.Fatalf("protected input was not delivered to facade: %q", facade.secretAtCall)
	}
	if len(facade.calls) != 1 || len(facade.calls[0].Secret) != len(secret) {
		t.Fatalf("unexpected protected request: %#v", facade.calls)
	}
	for _, value := range facade.calls[0].Secret {
		if value != 0 {
			t.Fatal("protected request was not cleared after facade execution")
		}
	}
}

func TestPostChallengeCallbackUsesProtectedInputChannel(t *testing.T) {
	const callback = "http://127.0.0.1:54545/callback?state=session-state&code=callback-canary"
	fixture := cachedFixture()
	facade := &callbackInputFacade{result: fixture}
	shell := New(facade, fixture, Options{Plain: true, InitialScreen: Providers})

	model, start := shell.executeWithArguments(ActionProviderLogin, []string{"runtime-provider", "paste_callback"}, nil)
	shell = model.(*Shell)
	if start == nil {
		t.Fatal("callback flow was not scheduled")
	}
	shell, wait := update(t, shell, start())
	if shell.input == nil || !shell.input.secret || wait == nil {
		t.Fatal("protected callback prompt did not open after the challenge event")
	}
	shell, _ = update(t, shell, key(callback))
	if strings.Contains(shell.View(), "callback-canary") || strings.Contains(shell.View(), "session-state") {
		t.Fatal("callback URL or OAuth state was rendered")
	}
	shell, submit := update(t, shell, key("enter"))
	if submit == nil {
		t.Fatal("protected callback was not submitted")
	}
	if message := submit(); message != nil {
		shell, _ = update(t, shell, message)
	}
	shell, _ = update(t, shell, wait())
	if facade.callback != callback {
		t.Fatalf("protected callback delivered %q", facade.callback)
	}
	if shell.input != nil || strings.Contains(shell.View(), "callback-canary") {
		t.Fatal("callback prompt or secret remained after completion")
	}
}

func TestSafeTextUsesSharedMaskShape(t *testing.T) {
	secret := "sk-0123456789abcdefghijklmnopqrstuvwxyz"
	masked := MaskSecret(secret).String()
	if !strings.Contains(safeText(secret), masked) {
		t.Fatalf("safeText mask %q does not match shared mask %q", safeText(secret), masked)
	}
	if strings.Contains(safeText(secret), secret) {
		t.Fatal("safeText retained complete secret")
	}
}
