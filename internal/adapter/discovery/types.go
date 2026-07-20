package discovery

import (
	"context"
	"time"

	"github.com/0p9b/pmux/internal/domain/install"
	"github.com/0p9b/pmux/internal/domain/service"
)

type EvidenceKind string

const (
	EvidenceBinary    EvidenceKind = "binary"
	EvidenceConfig    EvidenceKind = "config"
	EvidenceProcess   EvidenceKind = "process"
	EvidenceService   EvidenceKind = "service"
	EvidencePort      EvidenceKind = "port"
	EvidenceContainer EvidenceKind = "container"
)

type FileEvidence struct {
	Path    string      `json:"path"`
	Mode    uint32      `json:"mode"`
	Size    int64       `json:"size"`
	ModTime time.Time   `json:"mod_time"`
	SHA256  string      `json:"sha256,omitempty"`
}

type ProcessEvidence struct {
	PID        int      `json:"pid"`
	Executable string   `json:"executable"`
	Argv       []string `json:"argv"`
	WorkingDir string   `json:"working_dir,omitempty"`
	ConfigPath string   `json:"config_path,omitempty"`
}

type ServiceEvidence struct {
	Backend     service.ServiceBackend `json:"backend"`
	Identity    string                 `json:"identity"`
	State        service.ServiceState   `json:"state,omitempty"`
	Definition  string                 `json:"definition,omitempty"`
	Executable  string                 `json:"executable,omitempty"`
	Argv        []string               `json:"argv,omitempty"`
	WorkingDir  string                 `json:"working_dir,omitempty"`
	ConfigPath  string                 `json:"config_path,omitempty"`
	PMuxOwned   bool                   `json:"pmux_owned"`
}

type PortEvidence struct {
	Address     string `json:"address"`
	Healthy     bool   `json:"healthy"`
	HTTPStatus  int    `json:"http_status,omitempty"`
	CoreVersion string `json:"core_version,omitempty"`
}

type MountEvidence struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	ReadOnly    bool   `json:"read_only"`
}
type PublishedPortEvidence struct {
	HostIP        string `json:"host_ip"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Protocol      string `json:"protocol"`
}


type ContainerEvidence struct {
	ID             string                  `json:"id"`
	Name           string                  `json:"name"`
	Image          string                  `json:"image"`
	State          string                  `json:"state"`
	Mounts         []MountEvidence         `json:"mounts,omitempty"`
	Ports          []string                `json:"ports,omitempty"`
	PublishedPorts []PublishedPortEvidence `json:"published_ports,omitempty"`
	ConfigMount    string                  `json:"config_mount,omitempty"`
	Runtime        string                  `json:"runtime"`
	Endpoint       string                  `json:"endpoint,omitempty"`
	Healthy        bool                    `json:"healthy"`
	CoreVersion    string                  `json:"core_version,omitempty"`
}

type VersionSource string

const (
	VersionRunningHeader VersionSource = "running-header"
	VersionMetadata      VersionSource = "metadata"
	VersionIsolatedProbe VersionSource = "isolated-probe"
	VersionUnknown       VersionSource = "unknown"
)

type VersionEvidence struct {
	Version string        `json:"version"`
	Source  VersionSource `json:"source"`
	Commit  string        `json:"commit,omitempty"`
	BuiltAt string        `json:"built_at,omitempty"`
	Warning string        `json:"warning,omitempty"`
}

type Candidate struct {
	Binary    *FileEvidence      `json:"binary,omitempty"`
	Config    *FileEvidence      `json:"config,omitempty"`
	AuthDir   string             `json:"auth_dir,omitempty"`
	Process   *ProcessEvidence   `json:"process,omitempty"`
	Service   *ServiceEvidence   `json:"service,omitempty"`
	Port      *PortEvidence      `json:"port,omitempty"`
	Container *ContainerEvidence `json:"container,omitempty"`
	Version   VersionEvidence    `json:"version"`
	Findings  []string           `json:"findings,omitempty"`
}

func (c Candidate) ToInstallation(id string) install.Installation {
	installation := install.Installation{ID: id, Mode: "adopted", Version: c.Version.Version, Container: c.Container != nil, AuthDir: c.AuthDir}
	if installation.Version == "" {
		installation.Version = "unknown"
	}
	if c.Binary != nil {
		installation.BinaryPath = c.Binary.Path
	}
	if c.Config != nil {
		installation.ConfigPath = c.Config.Path
	}
	return installation
}

type Request struct {
	ProxyPath  string
	ConfigPath string
	AuthDir    string
	Addresses  []string
}

type ProcessEnumerator interface {
	Processes(context.Context) ([]ProcessEvidence, error)
}

type ServiceEnumerator interface {
	Services(context.Context) ([]ServiceEvidence, error)
}

type ContainerEnumerator interface {
	Containers(context.Context) ([]ContainerEvidence, error)
}

type ListenerProber interface {
	Probe(context.Context, string) (PortEvidence, error)
}
