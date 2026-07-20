package state

import (
	"encoding/hex"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/0p9b/pmux/internal/pmuxerr"
)

type Config struct {
	Version                int               `json:"version"`
	Theme                  string            `json:"theme,omitempty"`
	UpdateCheck            bool              `json:"update_check,omitempty"`
	DefaultInstallation    string            `json:"default_installation,omitempty"`
	DefaultClient          string            `json:"default_client,omitempty"`
	DefaultModel           string            `json:"default_model,omitempty"`
	HighLatencyMode        *bool             `json:"high_latency_mode,omitempty"`
	LogLineLimit           int               `json:"log_line_limit,omitempty"`
	Visual                 map[string]string `json:"visual,omitempty"`
	PersistentClaudeModels map[string]string `json:"persistent_claude_models,omitempty"`
}

type State struct {
	Version       int            `json:"version"`
	Installations []Installation `json:"installations,omitempty"`
	Favorites     []string       `json:"favorites,omitempty"`
	RecentModels  []string       `json:"recent_models,omitempty"`
}

type Installation struct {
	ID              string             `json:"id"`
	Kind            string             `json:"kind"`
	BinaryPath      string             `json:"binary_path"`
	BinarySHA256    string             `json:"binary_sha256,omitempty"`
	ConfigPath      string             `json:"config_path"`
	ProxyKeyRef     SecretReference    `json:"proxy_key_ref"`
	AuthDir         string             `json:"auth_dir"`
	RuntimeDir      string             `json:"runtime_dir"`
	Host            string             `json:"host"`
	Port            int                `json:"port"`
	ServiceBackend  string             `json:"service_backend"`
	CoreVersionSeen string             `json:"core_version_seen,omitempty"`
	ObservedAt      time.Time          `json:"observed_at,omitempty"`
	Container       *ContainerMetadata `json:"container,omitempty"`
}

type ContainerMetadata struct {
	Runtime     string `json:"runtime"`
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Image       string `json:"image"`
	Endpoint    string `json:"endpoint"`
	ConfigMount string `json:"config_mount,omitempty"`
}

// SecretReference is deliberately incapable of storing a plaintext secret.
// Fingerprint is a one-way digest, Masked is display-safe, and Path points to
// the private canonical source.
type SecretReference struct {
	Path        string `json:"path"`
	Masked      string `json:"masked"`
	Fingerprint string `json:"fingerprint"`
}

// SecretReferences persists references to platform-protected secret sources;
// it never persists their values.
type SecretReferences struct {
	Version    int                        `json:"version"`
	Management map[string]SecretReference `json:"management,omitempty"`
}

func validateState(value State) error {
	seen := make(map[string]struct{}, len(value.Installations))
	for _, installation := range value.Installations {
		if installation.ID == "" {
			return validationError("installation ID cannot be empty")
		}
		if _, exists := seen[installation.ID]; exists {
			return validationError("installation IDs must be unique")
		}
		seen[installation.ID] = struct{}{}
		if installation.Kind == "container" {
			if err := validateContainerMetadata(installation.Container); err != nil {
				return validationError(fmt.Sprintf("installation %q: %v", installation.ID, err))
			}
			continue
		}
		for name, path := range map[string]string{
			"binary":  installation.BinaryPath,
			"config":  installation.ConfigPath,
			"auth":    installation.AuthDir,
			"runtime": installation.RuntimeDir,
		} {
			if path == "" || !filepath.IsAbs(path) {
				return validationError(fmt.Sprintf("installation %q %s path must be absolute", installation.ID, name))
			}
		}
		if err := validateSecretReference(installation.ProxyKeyRef); err != nil {
			return err
		}
	}
	return nil
}

func validateContainerMetadata(value *ContainerMetadata) error {
	if value == nil || strings.TrimSpace(value.Runtime) == "" || strings.TrimSpace(value.ID) == "" || strings.TrimSpace(value.Image) == "" {
		return fmt.Errorf("container runtime, ID, and image are required")
	}
	if value.ConfigMount != "" && !filepath.IsAbs(value.ConfigMount) {
		return fmt.Errorf("container config mount must be absolute")
	}
	host, _, err := net.SplitHostPort(value.Endpoint)
	if err != nil || host == "" {
		return fmt.Errorf("container endpoint must be a loopback host and port")
	}
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("container endpoint must be loopback")
	}
	return nil
}

func validateSecretReferences(value SecretReferences) error {
	for id, reference := range value.Management {
		if strings.TrimSpace(id) == "" {
			return validationError("secret reference installation ID cannot be empty")
		}
		if err := validateSecretReference(reference); err != nil {
			return err
		}
	}
	return nil
}

func validateSecretReference(reference SecretReference) error {
	if reference.Path == "" || !filepath.IsAbs(reference.Path) {
		return validationError("secret reference path must be absolute")
	}
	const fingerprintPrefix = "sha256:"
	digest := strings.TrimPrefix(reference.Fingerprint, fingerprintPrefix)
	if len(digest) != 64 || !strings.HasPrefix(reference.Fingerprint, fingerprintPrefix) {
		return validationError("secret reference fingerprint must be a one-way sha256 reference")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return validationError("secret reference fingerprint must contain a hexadecimal sha256 digest")
	}
	if reference.Masked == "" {
		return validationError("secret reference mask cannot be empty")
	}
	if reference.Masked != "********" {
		parts := strings.Split(reference.Masked, "…")
		if len(parts) != 2 || utf8.RuneCountInString(parts[0]) > 7 || utf8.RuneCountInString(parts[1]) > 4 {
			return validationError("secret reference mask may contain at most the first seven and last four characters")
		}
	}
	return nil
}

func validationError(message string) error {
	return pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, message)
}
