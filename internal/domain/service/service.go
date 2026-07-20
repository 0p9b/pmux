package service

import (
	"context"
	"io"
	"time"
)

type ServiceBackend string

const (
	BackendSystemdUser     ServiceBackend = "systemd-user"
	BackendLaunchd         ServiceBackend = "launchd"
	BackendWindowsTask     ServiceBackend = "windows-task"
	BackendForeground      ServiceBackend = "foreground"
	BackendDockerUnmanaged ServiceBackend = "docker-unmanaged"
)

type ServiceState string

const (
	ServiceUnknown      ServiceState = "unknown"
	ServiceNotInstalled ServiceState = "not-installed"
	ServiceStopped      ServiceState = "stopped"
	ServiceStarting     ServiceState = "starting"
	ServiceRunning      ServiceState = "running"
	ServiceStopping     ServiceState = "stopping"
	ServiceFailed       ServiceState = "failed"
)

type ServiceSpec struct {
	InstanceID  string
	Identity    string
	PMuxPath    string
	BinaryPath  string
	ConfigPath  string
	RuntimeDir  string
	LogDir      string
	Environment []string
}

type ServiceStatus struct {
	Backend     ServiceBackend `json:"backend"`
	State       ServiceState   `json:"state"`
	PID         int            `json:"pid,omitempty"`
	Since       time.Time      `json:"since,omitempty"`
	Detail      string         `json:"detail,omitempty"`
	Healthy     bool           `json:"healthy"`
	CoreVersion string         `json:"core_version"`
	Warning     string         `json:"warning,omitempty"`
}

type ServiceManager interface {
	Backend() ServiceBackend
	Detect(ctx context.Context) (ServiceStatus, error)
	Install(ctx context.Context, spec ServiceSpec) error
	Uninstall(ctx context.Context) error
	Start(ctx context.Context) error
	Stop(ctx context.Context, timeout time.Duration) error
	Restart(ctx context.Context) (ServiceStatus, error)
	Status(ctx context.Context) (ServiceStatus, error)
	Logs(ctx context.Context, tail int, follow bool) (io.ReadCloser, error)
}

func Identity(backend ServiceBackend, instanceID string) string {
	switch backend {
	case BackendSystemdUser:
		return "pmux-cliproxyapi@" + instanceID + ".service"
	case BackendLaunchd:
		return "dev.pmux.cliproxyapi." + instanceID
	case BackendWindowsTask:
		return "PMux CLIProxyAPI (" + instanceID + ")"
	default:
		return "pmux-cliproxyapi-" + instanceID
	}
}
