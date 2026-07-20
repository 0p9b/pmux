package discovery

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

// DockerSocketEnumerator uses Docker's read-only container-list API. It never
// invokes the Docker CLI and never sends a lifecycle mutation.
type DockerSocketEnumerator struct {
	SocketPath string
	Client     *http.Client
	IsAbsent   func(error) bool
}
type unavailableContainerEnumerator struct {
	cause error
}

func (e unavailableContainerEnumerator) Containers(context.Context) ([]ContainerEvidence, error) {
	return nil, pmuxerr.Wrap(e.cause, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "Docker endpoint exists but cannot be inspected")
}

type dockerContainer struct {
	ID     string   `json:"Id"`
	Names  []string `json:"Names"`
	Image  string   `json:"Image"`
	State  string   `json:"State"`
	Mounts []struct {
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		Mode        string `json:"Mode"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
	Ports []struct {
		IP          string `json:"IP"`
		PrivatePort int    `json:"PrivatePort"`
		PublicPort  int    `json:"PublicPort"`
		Type        string `json:"Type"`
	} `json:"Ports"`
}

func (e DockerSocketEnumerator) Containers(ctx context.Context) ([]ContainerEvidence, error) {
	client := e.Client
	if client == nil {
		socket := e.SocketPath
		if socket == "" {
			socket = "/var/run/docker.sock"
		}
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", socket)
		}}
		client = &http.Client{Transport: transport, Timeout: 2 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/v1.41/containers/json?all=1", nil)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not build the Docker discovery request")
	}
	response, err := client.Do(request)
	if err != nil {
		if e.IsAbsent != nil && e.IsAbsent(err) {
			return nil, nil
		}
		return nil, pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "Docker is unavailable for read-only discovery")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, pmuxerr.New(pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "Docker rejected the read-only container inventory request")
	}
	var payload []dockerContainer
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
	if err := decoder.Decode(&payload); err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.UnhandledUpstreamShape, pmuxerr.Upstream, "Docker returned an unrecognized container inventory")
	}
	containers := make([]ContainerEvidence, 0)
	for _, item := range payload {
		if !looksLikeCore(item.Image, nil) {
			continue
		}
		container := ContainerEvidence{ID: item.ID, Image: item.Image, State: item.State, Runtime: "docker"}
		if len(item.Names) > 0 {
			container.Name = strings.TrimPrefix(item.Names[0], "/")
		}
		for _, mount := range item.Mounts {
			container.Mounts = append(container.Mounts, MountEvidence{Source: mount.Source, Destination: mount.Destination, ReadOnly: !mount.RW})
			if mount.Destination == "/CLIProxyAPI/config.yaml" {
				container.ConfigMount = mount.Source
			}
		}
		for _, port := range item.Ports {
			container.Ports = append(container.Ports, dockerPort(port.IP, port.PublicPort, port.PrivatePort, port.Type))
			if port.PublicPort > 0 {
				container.PublishedPorts = append(container.PublishedPorts, PublishedPortEvidence{HostIP: port.IP, HostPort: port.PublicPort, ContainerPort: port.PrivatePort, Protocol: port.Type})
			}
		}
		containers = append(containers, container)
	}
	return containers, nil
}

func dockerPort(ip string, public, private int, protocol string) string {
	if public == 0 {
		return strings.TrimSpace(strings.Join([]string{itoa(private), protocol}, "/"))
	}
	return ip + ":" + itoa(public) + "->" + itoa(private) + "/" + protocol
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	buffer := [20]byte{}
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		index--
		buffer[index] = '-'
	}
	return string(buffer[index:])
}

// DockerServiceManager exposes diagnosis/status only. Every lifecycle mutation
// fails with the ownership-conflict condition and cannot reach a container API.
type DockerServiceManager struct {
	Container ContainerEvidence
	Probe     ListenerProber
}

func (DockerServiceManager) Backend() service.ServiceBackend { return service.BackendDockerUnmanaged }
func (m DockerServiceManager) Detect(ctx context.Context) (service.ServiceStatus, error) {
	return m.status(ctx), nil
}
func (m DockerServiceManager) Status(ctx context.Context) (service.ServiceStatus, error) {
	return m.status(ctx), nil
}
func (DockerServiceManager) Install(context.Context, service.ServiceSpec) error {
	return dockerMutationError()
}
func (DockerServiceManager) Uninstall(context.Context) error           { return dockerMutationError() }
func (DockerServiceManager) Start(context.Context) error               { return dockerMutationError() }
func (DockerServiceManager) Stop(context.Context, time.Duration) error { return dockerMutationError() }
func (DockerServiceManager) Restart(context.Context) (service.ServiceStatus, error) {
	return service.ServiceStatus{}, dockerMutationError()
}
func (DockerServiceManager) Logs(context.Context, int, bool) (io.ReadCloser, error) {
	return nil, pmuxerr.New(pmuxerr.ServiceBackendUnavailable, pmuxerr.Upstream, "Docker logs are available only through the detected Management API; container runtime logs remain externally managed")
}

func (m DockerServiceManager) status(ctx context.Context) service.ServiceStatus {
	state := service.ServiceStopped
	if strings.EqualFold(m.Container.State, "running") {
		state = service.ServiceRunning
	}
	status := service.ServiceStatus{
		Backend:     service.BackendDockerUnmanaged,
		State:       state,
		Detail:      "Docker lifecycle is externally managed",
		CoreVersion: m.Container.CoreVersion,
		Healthy:     m.Container.Healthy,
	}
	if status.CoreVersion == "" {
		status.CoreVersion = "unknown"
	}
	if state == service.ServiceRunning && m.Probe != nil && m.Container.Endpoint != "" {
		port, err := m.Probe.Probe(ctx, m.Container.Endpoint)
		if err != nil {
			status.Healthy = false
			status.Warning = "container endpoint is not answering on loopback"
		} else {
			status.Healthy = port.Healthy
			if port.CoreVersion != "" {
				status.CoreVersion = port.CoreVersion
			}
		}
	}
	return status
}

func dockerMutationError() error {
	return pmuxerr.New(pmuxerr.ServiceForeignOwner, pmuxerr.Environment, "This CLIProxyAPI runs in Docker; its lifecycle is owned by the container runtime. Manage it with Docker; PMux service actions are disabled.")
}
