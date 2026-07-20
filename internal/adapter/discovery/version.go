package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/0p9b/pmux/internal/adapter/subproc"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

// NewLocal builds the complete read-only native discovery stack. Docker
// sockets and named pipes are optional; endpoint absence is not an error.
func NewLocal() Discoverer {
	discoverer := Discoverer{
		Processes:  LocalProcessEnumerator{},
		Services:   newLocalServiceEnumerator(),
		Listeners:  HTTPListenerProber{},
		Containers: newLocalContainerEnumerator(),
		Versions: VersionDetector{
			Metadata: SidecarMetadataResolver{},
			Probe: IsolatedBannerProber{Inner: subproc.VersionProbe{
				ParentEnv:  os.Environ(),
				EnvOptions: subproc.EnvironmentOptions{GOOS: runtime.GOOS},
			}},
		},
	}
	return discoverer
}

// IsolatedBannerProber adapts subproc.VersionProbe without exposing subprocess
// implementation types to discovery callers.
type IsolatedBannerProber struct {
	Inner subproc.VersionProbe
}

func (p IsolatedBannerProber) Probe(ctx context.Context, binary string) (ProbeVersion, error) {
	info, err := p.Inner.Probe(ctx, binary)
	if err != nil {
		if errors.Is(err, subproc.ErrUnsafeProbe) {
			return ProbeVersion{}, ErrUnsafeVersionProbe
		}
		return ProbeVersion{}, err
	}
	return ProbeVersion{Version: info.Version, Commit: info.Commit, BuiltAt: info.BuiltAt}, nil
}

// SidecarMetadataResolver trusts metadata only when it names the exact SHA-256
// of the observed binary. The sidecar is <binary>.pmux-release.json.
type SidecarMetadataResolver struct{}

type sidecarMetadata struct {
	Version      string `json:"version"`
	BinarySHA256 string `json:"binary_sha256"`
	Commit       string `json:"commit,omitempty"`
	BuiltAt      string `json:"built_at,omitempty"`
}

func (SidecarMetadataResolver) Version(ctx context.Context, binary string) (VersionEvidence, bool, error) {
	if err := ctx.Err(); err != nil {
		return VersionEvidence{}, false, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "version metadata inspection was canceled")
	}
	metadataBytes, err := os.ReadFile(binary + ".pmux-release.json")
	if os.IsNotExist(err) {
		return VersionEvidence{}, false, nil
	}
	if err != nil {
		return VersionEvidence{}, false, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not read checksum-bound CLIProxyAPI metadata")
	}
	var metadata sidecarMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return VersionEvidence{}, false, pmuxerr.Wrap(err, pmuxerr.ConfigValidationFailed, pmuxerr.Environment, "CLIProxyAPI release metadata is invalid")
	}
	file, err := os.Open(binary)
	if err != nil {
		return VersionEvidence{}, false, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not verify checksum-bound CLIProxyAPI metadata")
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return VersionEvidence{}, false, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "could not hash CLIProxyAPI for release metadata")
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if metadata.Version == "" || !strings.EqualFold(actual, metadata.BinarySHA256) {
		return VersionEvidence{}, false, nil
	}
	return VersionEvidence{Version: metadata.Version, Source: VersionMetadata, Commit: metadata.Commit, BuiltAt: metadata.BuiltAt}, true, nil
}

// HTTPListenerProber probes only a caller-supplied loopback address.
type HTTPListenerProber struct {
	Client *http.Client
}

func (p HTTPListenerProber) Probe(ctx context.Context, address string) (PortEvidence, error) {
	if !isLoopbackAddress(address) {
		return PortEvidence{}, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "discovery listener probe requires a loopback address")
	}
	client := p.Client
	if client == nil {
		transport := &http.Transport{DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext}
		client = &http.Client{Transport: transport, Timeout: 2 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/healthz", nil)
	if err != nil {
		return PortEvidence{}, pmuxerr.Wrap(err, pmuxerr.ManagementUnreachable, pmuxerr.Environment, "could not build the local health probe")
	}
	response, err := client.Do(request)
	if err != nil {
		return PortEvidence{}, pmuxerr.Wrap(err, pmuxerr.ManagementUnreachable, pmuxerr.Environment, "CLIProxyAPI listener is unreachable")
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return PortEvidence{Address: address, Healthy: response.StatusCode == http.StatusOK, HTTPStatus: response.StatusCode, CoreVersion: response.Header.Get("X-CPA-VERSION")}, nil
}

func MetadataPath(binary string) string {
	absolute, _ := filepath.Abs(binary)
	return absolute + ".pmux-release.json"
}
