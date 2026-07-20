package discovery

import (
	"context"
	"reflect"
	"testing"

	"github.com/0p9b/pmux/internal/domain/service"
)

func TestWindowsServiceEnumeratorReadsExecActionFromCOMFake(t *testing.T) {
	source := &fakeScheduledTaskSource{tasks: []ScheduledTask{
		{
			Identity:    "PMux CLIProxyAPI (alpha)",
			Definition:  `\PMux CLIProxyAPI (alpha)`,
			Description: windowsOwnershipPrefix + "alpha",
			State:       TaskStateRunning,
			Actions: []ScheduledExecAction{{
				Executable:       `C:\Program Files\PMux\pmux-service-host.exe`,
				Arguments:        `--binary "C:\Program Files\PMux\cli-proxy-api.exe" --config "C:\Users\alice\PMux Data\config.yaml"`,
				WorkingDirectory: `C:\Users\alice\PMux Data\runtime`,
			}},
		},
		{
			Identity:    "Foreign CLIProxyAPI",
			Definition:  `\Vendor\Foreign CLIProxyAPI`,
			Description: "owned elsewhere",
			State:       TaskStateReady,
			Actions: []ScheduledExecAction{{
				Executable:       `D:\tools\cli-proxy-api.exe`,
				Arguments:        `-config D:\proxy\config.yaml`,
				WorkingDirectory: `D:\proxy`,
			}},
		},
		{
			Identity: "Unrelated updater",
			State:    TaskStateReady,
			Actions:  []ScheduledExecAction{{Executable: `C:\Windows\updater.exe`, Arguments: `/quiet`}},
		},
	}}
	enumerator := WindowsServiceEnumerator{Source: source, Limit: 32}

	services, err := enumerator.Services(context.Background())
	if err != nil {
		t.Fatalf("Services() error = %v", err)
	}
	if source.limit != 32 {
		t.Fatalf("COM source limit = %d, want 32", source.limit)
	}
	if len(services) != 2 {
		t.Fatalf("services = %#v, want PMux and foreign CLIProxyAPI tasks", services)
	}
	managed := services[0]
	if managed.Backend != service.BackendWindowsTask || managed.Identity != "PMux CLIProxyAPI (alpha)" || !managed.PMuxOwned {
		t.Fatalf("managed evidence identity/ownership = %#v", managed)
	}
	if managed.Executable != `C:\Program Files\PMux\pmux-service-host.exe` {
		t.Fatalf("managed executable = %q", managed.Executable)
	}
	wantManagedArgv := []string{
		`C:\Program Files\PMux\pmux-service-host.exe`,
		"--binary", `C:\Program Files\PMux\cli-proxy-api.exe`,
		"--config", `C:\Users\alice\PMux Data\config.yaml`,
	}
	if !reflect.DeepEqual(managed.Argv, wantManagedArgv) {
		t.Fatalf("managed argv = %#v, want %#v", managed.Argv, wantManagedArgv)
	}
	if managed.ConfigPath != `C:\Users\alice\PMux Data\config.yaml` || managed.WorkingDir != `C:\Users\alice\PMux Data\runtime` {
		t.Fatalf("managed config/working directory = %#v", managed)
	}
	if managed.State != service.ServiceRunning {
		t.Fatalf("managed state = %q, want running", managed.State)
	}

	foreign := services[1]
	if foreign.PMuxOwned || foreign.State != service.ServiceStopped || foreign.Definition != `\Vendor\Foreign CLIProxyAPI` {
		t.Fatalf("foreign task evidence = %#v", foreign)
	}
	if foreign.ConfigPath != `D:\proxy\config.yaml` {
		t.Fatalf("foreign config path = %q", foreign.ConfigPath)
	}
}

func TestWindowsServiceEnumeratorBoundsCOMEnumeration(t *testing.T) {
	source := &fakeScheduledTaskSource{tasks: make([]ScheduledTask, defaultScheduledTaskLimit+5)}
	enumerator := WindowsServiceEnumerator{Source: source, Limit: defaultScheduledTaskLimit + 500}
	services, err := enumerator.Services(context.Background())
	if err != nil {
		t.Fatalf("Services() error = %v", err)
	}
	if source.limit != defaultScheduledTaskLimit {
		t.Fatalf("COM source limit = %d, want hard cap %d", source.limit, defaultScheduledTaskLimit)
	}
	if len(services) != 0 {
		t.Fatalf("unrelated empty tasks should be skipped, got %d", len(services))
	}
}

func TestParseWindowsCommandLine(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{name: "empty", input: "", want: nil},
		{name: "spaces and quoted paths", input: `--config "C:\Program Files\PMux\config.yaml" --flag`, want: []string{"--config", `C:\Program Files\PMux\config.yaml`, "--flag"}},
		{name: "empty argument", input: `one "" three`, want: []string{"one", "", "three"}},
		{name: "escaped quote", input: `"a\\\"b" c`, want: []string{`a\"b`, "c"}},
		{name: "trailing backslashes", input: `"C:\path\\" tail`, want: []string{`C:\path\`, "tail"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parseWindowsCommandLine(test.input); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("parseWindowsCommandLine(%q) = %#v, want %#v", test.input, got, test.want)
			}
		})
	}
}

func TestWindowsTaskOwnershipRequiresExactCanonicalIdentity(t *testing.T) {
	if !windowsTaskOwned("PMux CLIProxyAPI (alpha)", windowsOwnershipPrefix+"alpha") {
		t.Fatal("canonical task and marker were not recognized")
	}
	if windowsTaskOwned("Renamed task", windowsOwnershipPrefix+"alpha") {
		t.Fatal("renamed task must not be treated as PMux-owned")
	}
	if windowsTaskOwned("PMux CLIProxyAPI (alpha)", windowsOwnershipPrefix+"beta") {
		t.Fatal("mismatched marker must not be treated as PMux-owned")
	}
}

type fakeScheduledTaskSource struct {
	tasks []ScheduledTask
	limit int
}

func (f *fakeScheduledTaskSource) Tasks(_ context.Context, intLimit int) ([]ScheduledTask, error) {
	f.limit = intLimit
	return append([]ScheduledTask(nil), f.tasks...), nil
}
