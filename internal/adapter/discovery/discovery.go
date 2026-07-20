package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/0p9b/pmux/internal/pmuxerr"
)

// VersionDetector applies the mandatory order: running header, checksum-bound
// metadata, isolated generated-config banner probe, then unknown.
type VersionDetector struct {
	Metadata MetadataResolver
	Probe    BannerProber
}

type MetadataResolver interface {
	Version(context.Context, string) (VersionEvidence, bool, error)
}

type BannerProber interface {
	Probe(context.Context, string) (ProbeVersion, error)
}

type ProbeVersion struct {
	Version string
	Commit  string
	BuiltAt string
}

var ErrUnsafeVersionProbe = pmuxerr.New(pmuxerr.InstallUnsupportedTarget, pmuxerr.Environment, "isolated version probe is unsafe")

func (d VersionDetector) Detect(ctx context.Context, candidate Candidate) VersionEvidence {
	if candidate.Port != nil && candidate.Port.CoreVersion != "" {
		return VersionEvidence{Version: candidate.Port.CoreVersion, Source: VersionRunningHeader}
	}
	if candidate.Binary == nil || candidate.Binary.Path == "" || candidate.Container != nil {
		return VersionEvidence{Version: "unknown", Source: VersionUnknown, Warning: "no safe local binary is available for isolated version detection"}
	}
	if d.Metadata != nil {
		metadata, ok, err := d.Metadata.Version(ctx, candidate.Binary.Path)
		if err == nil && ok && metadata.Version != "" {
			metadata.Source = VersionMetadata
			return metadata
		}
	}
	if d.Probe != nil {
		probed, err := d.Probe.Probe(ctx, candidate.Binary.Path)
		if err == nil && probed.Version != "" {
			return VersionEvidence{Version: probed.Version, Source: VersionIsolatedProbe, Commit: probed.Commit, BuiltAt: probed.BuiltAt}
		}
	}
	return VersionEvidence{Version: "unknown", Source: VersionUnknown, Warning: "version could not be detected safely"}
}

// Discoverer performs observation only. It has no lifecycle or mutation port.
type Discoverer struct {
	Processes ProcessEnumerator
	Services  ServiceEnumerator
	Containers ContainerEnumerator
	Listeners ListenerProber
	Versions  VersionDetector
	LookPath  func(string) (string, error)
}

func (d Discoverer) Discover(ctx context.Context, request Request) ([]Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "installation discovery was canceled")
	}
	candidates := make([]Candidate, 0, 8)
	if request.ProxyPath != "" || request.ConfigPath != "" {
		candidate, err := explicitCandidate(request)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	lookPath := d.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if request.ProxyPath == "" {
		if path, err := lookPath("cli-proxy-api"); err == nil {
			if binary, fileErr := observeFile(path, true); fileErr == nil {
				candidates = append(candidates, Candidate{Binary: binary})
			}
		}
	}

	if d.Processes != nil {
		processes, err := d.Processes.Processes(ctx)
		if err != nil {
			return nil, pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "could not inspect running CLIProxyAPI processes")
		}
		for i := range processes {
			process := processes[i]
			if !looksLikeCore(process.Executable, process.Argv) {
				continue
			}
			candidate := Candidate{Process: &process}
			if process.Executable != "" {
				candidate.Binary, _ = observeFile(process.Executable, true)
			}
			configPath, explicit := configFromArgv(process.Argv, process.WorkingDir)
			process.ConfigPath = configPath
			candidate.Process = &process
			if configPath != "" {
				candidate.Config, _ = observeFile(configPath, false)
			}
			if !explicit {
				candidate.Findings = append(candidate.Findings, "process depends on its working directory for config.yaml")
			}
			candidates = append(candidates, candidate)
		}
	}

	if d.Services != nil {
		services, err := d.Services.Services(ctx)
		if err != nil {
			return nil, pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not inspect CLIProxyAPI service definitions")
		}
		for i := range services {
			observed := services[i]
			if !looksLikeCore(observed.Executable, observed.Argv) && !strings.Contains(strings.ToLower(observed.Identity), "cliproxy") {
				continue
			}
			candidate := Candidate{Service: &observed}
			if observed.Executable != "" {
				candidate.Binary, _ = observeFile(observed.Executable, true)
			}
			configPath, explicit := configFromArgv(observed.Argv, observed.WorkingDir)
			observed.ConfigPath = configPath
			candidate.Service = &observed
			if configPath != "" {
				candidate.Config, _ = observeFile(configPath, false)
			}
			if !explicit {
				candidate.Findings = append(candidate.Findings, "service depends on its working directory for config.yaml")
			}
			candidates = append(candidates, candidate)
		}
	}

	if d.Containers != nil {
		containers, err := d.Containers.Containers(ctx)
		if err != nil {
			return nil, pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "could not inspect containerized CLIProxyAPI installations")
		}
		for i := range containers {
			container := containers[i]
			if !looksLikeCore(container.Image, nil) {
				continue
			}
			candidate := Candidate{Container: &container, Findings: []string{"container lifecycle is externally managed and read-only in PMux v1"}}
			if container.ConfigMount != "" {
				if config, configErr := observeFile(container.ConfigMount, false); configErr == nil {
					candidate.Config = config
				} else {
					candidate.Findings = append(candidate.Findings, "known container config bind mount is not readable from the host")
				}
			}
			if address, exposed := containerLoopbackEndpoint(container); address != "" {
				if exposed {
					candidate.Findings = append(candidate.Findings, "container port is published beyond loopback")
				}
				port := PortEvidence{Address: address}
				container.Endpoint = address
				if d.Listeners != nil {
					if observed, probeErr := d.Listeners.Probe(ctx, address); probeErr == nil {
						port = observed
					} else {
						candidate.Findings = append(candidate.Findings, "container loopback endpoint is not healthy")
					}
				}
				candidate.Port = &port
				container.Healthy = port.Healthy
				container.CoreVersion = port.CoreVersion
				candidate.Container = &container
			} else {
				candidate.Findings = append(candidate.Findings, "container has no supported loopback published port")
			}
			candidates = append(candidates, candidate)
		}
	}

	if d.Listeners != nil {
		for _, address := range request.Addresses {
			if !isLoopbackAddress(address) {
				continue
			}
			evidence, err := d.Listeners.Probe(ctx, address)
			if err != nil {
				continue
			}
			candidates = mergePort(candidates, evidence)
		}
	}

	candidates = mergeCandidates(candidates)
	for index := range candidates {
		if candidates[index].AuthDir == "" && request.AuthDir != "" && len(candidates) == 1 {
			candidates[index].AuthDir, _ = filepath.Abs(request.AuthDir)
		}
		candidates[index].Version = d.Versions.Detect(ctx, candidates[index])
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidateKey(candidates[i]) < candidateKey(candidates[j]) })
	return candidates, nil
}

func explicitCandidate(request Request) (Candidate, error) {
	var candidate Candidate
	if request.ProxyPath != "" {
		binary, err := observeFile(request.ProxyPath, true)
		if err != nil {
			return Candidate{}, err
		}
		candidate.Binary = binary
	}
	if request.ConfigPath != "" {
		config, err := observeFile(request.ConfigPath, false)
		if err != nil {
			return Candidate{}, err
		}
		candidate.Config = config
	}
	if request.AuthDir != "" {
		authDir, err := filepath.Abs(request.AuthDir)
		if err != nil {
			return Candidate{}, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not resolve the adopted auth directory")
		}
		candidate.AuthDir = authDir
	}
	return candidate, nil
}

func observeFile(path string, executable bool) (*FileEvidence, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not resolve an adopted installation path")
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not inspect an adopted installation path")
	}
	if !info.Mode().IsRegular() {
		return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.Environment, fmt.Sprintf("adopted path is not a regular file: %s", absolute))
	}
	if executable && info.Mode().Perm()&0o111 == 0 {
		return nil, pmuxerr.New(pmuxerr.InstallUnsupportedTarget, pmuxerr.Environment, fmt.Sprintf("adopted CLIProxyAPI binary is not executable: %s", absolute))
	}
	bytes, err := os.ReadFile(absolute)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not fingerprint an adopted installation path")
	}
	hash := sha256.Sum256(bytes)
	return &FileEvidence{Path: absolute, Mode: uint32(info.Mode()), Size: info.Size(), ModTime: info.ModTime(), SHA256: hex.EncodeToString(hash[:])}, nil
}

func configFromArgv(argv []string, workingDir string) (string, bool) {
	for index, arg := range argv {
		if arg == "-config" && index+1 < len(argv) {
			return resolveProcessPath(argv[index+1], workingDir), true
		}
		if value, ok := strings.CutPrefix(arg, "-config="); ok {
			return resolveProcessPath(value, workingDir), true
		}
	}
	if workingDir == "" {
		return "", false
	}
	return filepath.Join(workingDir, "config.yaml"), false
}

func resolveProcessPath(path, workingDir string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	if workingDir != "" {
		return filepath.Clean(filepath.Join(workingDir, path))
	}
	absolute, _ := filepath.Abs(path)
	return absolute
}

func looksLikeCore(executable string, argv []string) bool {
	values := append([]string{executable}, argv...)
	for _, value := range values {
		base := strings.ToLower(filepath.Base(value))
		if strings.Contains(base, "cli-proxy-api") || strings.Contains(base, "cliproxyapi") {
			return true
		}
		if strings.Contains(strings.ToLower(value), "eceasy/cli-proxy-api") {
			return true
		}
	}
	return false
}

func isLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
}

func containerLoopbackEndpoint(container ContainerEvidence) (address string, exposed bool) {
	var selected *PublishedPortEvidence
	for index := range container.PublishedPorts {
		port := &container.PublishedPorts[index]
		if port.Protocol != "" && !strings.EqualFold(port.Protocol, "tcp") {
			continue
		}
		if selected == nil || port.ContainerPort == 8317 {
			selected = port
		}
		if port.ContainerPort == 8317 {
			break
		}
	}
	if selected == nil || selected.HostPort <= 0 {
		return "", false
	}
	host := strings.Trim(selected.HostIP, "[]")
	switch host {
	case "", "0.0.0.0", "::":
		return net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", selected.HostPort)), true
	}
	ip := net.ParseIP(host)
	if strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback()) {
		return net.JoinHostPort(host, fmt.Sprintf("%d", selected.HostPort)), false
	}
	return "", false
}

func mergePort(candidates []Candidate, port PortEvidence) []Candidate {
	eligible := -1
	for index := range candidates {
		if candidates[index].Port == nil && candidates[index].Container == nil {
			if eligible != -1 {
				eligible = -2
				break
			}
			eligible = index
		}
	}
	copy := port
	if eligible >= 0 {
		candidates[eligible].Port = &copy
		return candidates
	}
	return append(candidates, Candidate{Port: &copy})
}

func mergeCandidates(input []Candidate) []Candidate {
	output := make([]Candidate, 0, len(input))
	for _, candidate := range input {
		merged := false
		for index := range output {
			if sameCandidate(output[index], candidate) {
				output[index] = combine(output[index], candidate)
				merged = true
				break
			}
		}
		if !merged {
			output = append(output, candidate)
		}
	}
	return output
}

func sameCandidate(left, right Candidate) bool {
	if left.Container != nil || right.Container != nil {
		return left.Container != nil && right.Container != nil &&
			left.Container.Runtime == right.Container.Runtime && left.Container.ID == right.Container.ID
	}
	if left.Config != nil && right.Config != nil && left.Config.Path == right.Config.Path {
		return true
	}
	if left.Binary != nil && right.Binary != nil && left.Binary.Path == right.Binary.Path {
		return true
	}
	return left.Process != nil && right.Process != nil && left.Process.PID == right.Process.PID
}

func candidateKey(candidate Candidate) string {
	if candidate.Container != nil {
		return "container:" + candidate.Container.Runtime + ":" + candidate.Container.ID
	}
	if candidate.Config != nil {
		return "config:" + candidate.Config.Path
	}
	if candidate.Binary != nil {
		return "binary:" + candidate.Binary.Path
	}
	if candidate.Process != nil {
		return fmt.Sprintf("process:%d", candidate.Process.PID)
	}
	if candidate.Port != nil {
		return "port:" + candidate.Port.Address
	}
	return ""
}

func combine(left, right Candidate) Candidate {
	if left.Binary == nil { left.Binary = right.Binary }
	if left.Config == nil { left.Config = right.Config }
	if left.AuthDir == "" { left.AuthDir = right.AuthDir }
	if left.Process == nil { left.Process = right.Process }
	if left.Service == nil { left.Service = right.Service }
	if left.Port == nil { left.Port = right.Port }
	if left.Container == nil { left.Container = right.Container }
	left.Findings = append(left.Findings, right.Findings...)
	return left
}
