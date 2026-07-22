package tui

// Screen is a stable primary-screen identifier. Its order defines the 1-9
// keyboard shortcuts from the product contract.
type Screen int

const (
	Dashboard Screen = iota
	Providers
	Models
	Launch
	Doctor
	Service
	Config
	Settings
	Logs
)

var screenNames = [...]string{
	"Dashboard", "Providers", "Models", "Launch", "Doctor",
	"Service", "Config", "Settings", "Logs",
}

func (s Screen) String() string {
	if int(s) < 0 || int(s) >= len(screenNames) {
		return "Unknown"
	}
	return screenNames[s]
}

type ActionID string

const (
	ActionDashboardRefresh ActionID = "dashboard.refresh"
	ActionRecommended      ActionID = "dashboard.recommended"
	ActionSetup            ActionID = "setup.start"

	ActionProvidersList   ActionID = "providers.list"
	ActionProviderDetails ActionID = "providers.details"
	ActionProviderLogin   ActionID = "providers.login"
	ActionProviderVerify  ActionID = "providers.verify"
	ActionProviderEnable  ActionID = "providers.enable"
	ActionProviderDisable ActionID = "providers.disable"
	ActionProviderRemove  ActionID = "providers.remove"

	ActionModelsList      ActionID = "models.list"
	ActionModelDetails    ActionID = "models.details"
	ActionModelTest       ActionID = "models.test"
	ActionModelFavorite   ActionID = "models.favorite"
	ActionModelUnfavorite ActionID = "models.unfavorite"
	ActionModelLaunch     ActionID = "models.launch"

	ActionLaunchRun     ActionID = "launch.run"
	ActionLaunchClient  ActionID = "launch.client"
	ActionLaunchPersist ActionID = "launch.persist"
	ActionLaunchDoctor  ActionID = "launch.doctor"

	ActionDoctorRun     ActionID = "doctor.run"
	ActionDoctorDetails ActionID = "doctor.details"
	ActionDoctorFix     ActionID = "doctor.fix"
	ActionDoctorFixAll  ActionID = "doctor.fix-all"
	ActionDoctorBundle  ActionID = "doctor.bundle"

	ActionServiceStatus     ActionID = "service.status"
	ActionServiceStart      ActionID = "service.start"
	ActionServiceStop       ActionID = "service.stop"
	ActionServiceRestart    ActionID = "service.restart"
	ActionServiceInstall    ActionID = "service.install"
	ActionServiceUninstall  ActionID = "service.uninstall"
	ActionServiceLogs       ActionID = "service.logs"
	ActionServiceForeground ActionID = "service.foreground"

	ActionConfigShow    ActionID = "config.show"
	ActionConfigGet     ActionID = "config.get"
	ActionConfigSet     ActionID = "config.set"
	ActionConfigEdit    ActionID = "config.edit"
	ActionConfigBackup  ActionID = "config.backup"
	ActionConfigRestore ActionID = "config.restore"

	ActionSettingsShow    ActionID = "settings.show"
	ActionSettingsGet     ActionID = "settings.get"
	ActionSettingsSet     ActionID = "settings.set"
	ActionSettingsBackup  ActionID = "settings.backup"
	ActionSettingsRestore ActionID = "settings.restore"

	ActionLogsList   ActionID = "logs.list"
	ActionLogsFollow ActionID = "logs.follow"
	ActionLogsExport ActionID = "logs.export"
	ActionLogsClear  ActionID = "logs.clear"
)

// ActionMeta is rendered in contextual help. Command is the canonical §15
// equivalent and is suitable for documentation; placeholders use angle
// brackets and are never shell-expanded by the TUI.
type ActionMeta struct {
	ID        ActionID
	Screen    Screen
	Key       string
	Label     string
	Command   []string
	Mutating  bool
	Streaming bool
}

func (a ActionMeta) MachineCommand() []string {
	if len(a.Command) == 0 {
		return nil
	}
	out := make([]string, 0, len(a.Command)+1)
	out = append(out, a.Command[0], "--json")
	out = append(out, a.Command[1:]...)
	return out
}

var actionCatalog = []ActionMeta{
	{ActionDashboardRefresh, Dashboard, "r", "refresh", []string{"pmux"}, false, false},
	{ActionRecommended, Dashboard, "enter", "recommended action", []string{"pmux"}, false, false},
	{ActionSetup, Dashboard, "enter", "start setup", []string{"pmux", "setup"}, false, false},
	{ActionProvidersList, Providers, "r", "refresh", []string{"pmux", "providers", "list", "--refresh"}, false, false},
	{ActionProviderDetails, Providers, "enter", "details", []string{"pmux", "providers", "list"}, false, false},
	{ActionProviderLogin, Providers, "a", "add/login", []string{"pmux", "providers", "login", "<provider>"}, true, true},
	{ActionProviderVerify, Providers, "v", "verify", []string{"pmux", "providers", "verify", "<provider>"}, false, false},
	{ActionProviderEnable, Providers, "e", "enable", []string{"pmux", "providers", "enable", "<provider>"}, true, false},
	{ActionProviderDisable, Providers, "x", "disable", []string{"pmux", "providers", "disable", "<provider>"}, true, false},
	{ActionProviderRemove, Providers, "delete", "remove", []string{"pmux", "providers", "remove", "<provider>"}, true, false},
	{ActionModelsList, Models, "r", "refresh", []string{"pmux", "models", "list", "--refresh"}, false, false},
	{ActionModelDetails, Models, "enter", "details", []string{"pmux", "models", "list"}, false, false},
	{ActionModelTest, Models, "t", "test", []string{"pmux", "models", "test", "<model>"}, false, false},
	{ActionModelFavorite, Models, "f", "favorite", []string{"pmux", "models", "favorite", "<model>"}, true, false},
	{ActionModelUnfavorite, Models, "f", "unfavorite", []string{"pmux", "models", "unfavorite", "<model>"}, true, false},
	{ActionModelLaunch, Models, "l", "launch", []string{"pmux", "launch", "--client", "<client>", "--model", "<model>"}, false, true},
	{ActionLaunchRun, Launch, "enter", "launch", []string{"pmux", "launch", "--client", "<client>", "--model", "<model>"}, false, true},
	{ActionLaunchClient, Launch, "c", "client", []string{"pmux", "launch", "--client", "<claude|codex|gemini|opencode>", "--model", "<model>"}, false, false},
	{ActionLaunchPersist, Launch, "p", "persistent slots", []string{"pmux", "config", "--scope", "pmux", "set", "integrations.claude.persistent-models.<slot>", "<model|unmanaged>"}, true, false},
	{ActionLaunchDoctor, Launch, "d", "doctor", []string{"pmux", "doctor"}, false, false},
	{ActionDoctorRun, Doctor, "r", "rerun", []string{"pmux", "doctor"}, false, false},
	{ActionDoctorDetails, Doctor, "enter", "details", []string{"pmux", "doctor"}, false, false},
	{ActionDoctorFix, Doctor, "f", "fix", []string{"pmux", "doctor", "--fix", "<check>"}, true, false},
	{ActionDoctorFixAll, Doctor, "a", "fix all", []string{"pmux", "doctor", "--fix"}, true, false},
	{ActionDoctorBundle, Doctor, "b", "bundle", []string{"pmux", "doctor", "--bundle"}, true, false},
	{ActionServiceStatus, Service, "v", "refresh", []string{"pmux", "service", "status"}, false, false},
	{ActionServiceStart, Service, "s", "start", []string{"pmux", "service", "start"}, true, false},
	{ActionServiceStop, Service, "x", "stop", []string{"pmux", "service", "stop"}, true, false},
	{ActionServiceRestart, Service, "r", "restart", []string{"pmux", "service", "restart"}, true, false},
	{ActionServiceInstall, Service, "i", "install", []string{"pmux", "service", "install"}, true, false},
	{ActionServiceUninstall, Service, "u", "uninstall", []string{"pmux", "service", "uninstall"}, true, false},
	{ActionServiceLogs, Service, "l", "logs", []string{"pmux", "service", "logs"}, false, true},
	{ActionServiceForeground, Service, "f", "foreground", []string{"pmux", "service", "start", "--foreground"}, true, true},
	{ActionConfigShow, Config, "v", "validate/reload", []string{"pmux", "config", "--scope", "proxy", "show"}, false, false},
	{ActionConfigGet, Config, "enter", "inspect", []string{"pmux", "config", "--scope", "proxy", "get", "<key>"}, false, false},
	{ActionConfigSet, Config, "w", "write", []string{"pmux", "config", "--scope", "proxy", "set", "<key>", "<value>"}, true, false},
	{ActionConfigEdit, Config, "e", "edit", []string{"pmux", "config", "--scope", "proxy", "edit"}, true, false},
	{ActionConfigBackup, Config, "b", "backup", []string{"pmux", "config", "--scope", "proxy", "backup"}, true, false},
	{ActionConfigRestore, Config, "r", "restore", []string{"pmux", "config", "--scope", "proxy", "restore", "<backup>"}, true, false},
	{ActionSettingsShow, Settings, "v", "reload", []string{"pmux", "config", "--scope", "pmux", "show"}, false, false},
	{ActionSettingsGet, Settings, "enter", "inspect", []string{"pmux", "config", "--scope", "pmux", "get", "<key>"}, false, false},
	{ActionSettingsSet, Settings, "p", "set", []string{"pmux", "config", "--scope", "pmux", "set", "<key>", "<value>"}, true, false},
	{ActionSettingsBackup, Settings, "b", "backup", []string{"pmux", "config", "--scope", "pmux", "backup"}, true, false},
	{ActionSettingsRestore, Settings, "r", "restore", []string{"pmux", "config", "--scope", "pmux", "restore", "<backup>"}, true, false},
	{ActionLogsList, Logs, "r", "reload", []string{"pmux", "service", "logs"}, false, false},
	{ActionLogsFollow, Logs, "space", "follow", []string{"pmux", "service", "logs", "--follow"}, false, true},
	{ActionLogsExport, Logs, "e", "export", []string{"pmux", "service", "logs", "--output", "<path>"}, true, false},
	{ActionLogsClear, Logs, "delete", "clear", []string{"pmux", "service", "logs", "--clear", "<source>"}, true, false},
}

func Actions() []ActionMeta {
	out := make([]ActionMeta, len(actionCatalog))
	copy(out, actionCatalog)
	return out
}

func ActionsFor(screen Screen) []ActionMeta {
	out := make([]ActionMeta, 0, 8)
	for _, action := range actionCatalog {
		if action.Screen == screen {
			out = append(out, action)
		}
	}
	return out
}

func Action(id ActionID) (ActionMeta, bool) {
	for _, action := range actionCatalog {
		if action.ID == id {
			return action, true
		}
	}
	return ActionMeta{}, false
}
