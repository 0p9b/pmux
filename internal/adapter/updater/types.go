package updater

import (
	"context"
	"runtime"
	"time"

	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/domain/update"
)

// Ownership identifies which installation method is allowed to replace a component.
type Ownership string

const (
	OwnershipManaged        Ownership = "managed"
	OwnershipAdopted        Ownership = "adopted"
	OwnershipPackageManaged Ownership = "package-managed"
)

// Target identifies the executable format PMux must find after extraction.
type Target struct {
	GOOS string
	Arch string
}

func NativeTarget() Target { return Target{GOOS: runtime.GOOS, Arch: runtime.GOARCH} }

// Release is the complete immutable release description returned by a Source.
type Release struct {
	Component      update.Component
	Version        string
	ArchiveName    string
	ArchiveURL     string
	ChecksumsURL   string
	ExecutableName string
}

// Source performs release-network operations. Engine construction never calls a Source;
// only Check, UpdateSelf, and UpdateProxy do.
type Source interface {
	Resolve(ctx context.Context, component update.Component, version string) (Release, error)
	Download(ctx context.Context, url, destination string) error
}

// Service is the lifecycle subset needed for a verified proxy cutover.
type Service interface {
	Status(ctx context.Context) (service.ServiceStatus, error)
	Start(ctx context.Context) error
	Stop(ctx context.Context, timeout time.Duration) error
}

// ProxyVerifier verifies the newly selected core through machine-readable endpoints.
type ProxyVerifier interface {
	Health(ctx context.Context) error
	Authenticated(ctx context.Context) error
	Models(ctx context.Context) ([]string, error)
}

// SelfVerifier validates the candidate and active executable through PMux's
// canonical version command before and after replacement.
type SelfVerifier interface {
	Preflight(ctx context.Context, candidate, expectedVersion string) error
	Postflight(ctx context.Context, active, expectedVersion string) error
}

type CheckRequest struct {
	Component      update.Component
	CurrentVersion string
	Version        string // empty resolves latest
}

type SelfRequest struct {
	CurrentVersion string
	Version        string // empty resolves latest
	Ownership      Ownership
	ExecutablePath string
	Target         Target
}

type ProxyRequest struct {
	CurrentVersion string
	Version        string // empty resolves latest
	Ownership      Ownership
	VersionsDir    string
	CurrentPointer string
	Target         Target
	StopTimeout    time.Duration
}

type Result struct {
	Component       update.Component `json:"component"`
	PreviousVersion string           `json:"previous_version"`
	Version         string           `json:"version"`
	Changed         bool             `json:"changed"`
	RolledBack      bool             `json:"rolled_back"`
	Warnings        []string         `json:"warnings,omitempty"`
}

// PointerStore atomically reads and selects a managed proxy version.
// The native implementation uses symlinks on Unix and a private pointer file on Windows.
type PointerStore interface {
	Read(pointer string) (string, error)
	Swap(pointer, target string) error
}

// MutationLocker executes a mutation while holding the process-wide OS advisory lock.
type MutationLocker interface {
	WithMutation(ctx context.Context, operation string, mutate func() error) error
}

// Stage names deterministic transaction boundaries and supports focused fault injection.
type Stage string

const (
	StageResolve           Stage = "resolve"
	StageDownloadArchive   Stage = "download-archive"
	StageDownloadChecksums Stage = "download-checksums"
	StageVerifyChecksum    Stage = "verify-checksum"
	StageExtract           Stage = "extract"
	StageVerifyExecutable  Stage = "verify-executable"
	StagePreflight         Stage = "preflight"
	StageStopService       Stage = "stop-service"
	StageInstallVersion    Stage = "install-version"
	StageSwitchPointer     Stage = "switch-pointer"
	StageActivate          Stage = "activate"
	StageStartService      Stage = "start-service"
	StageHealth            Stage = "health"
	StageAuthenticate      Stage = "authenticate"
	StageModels            Stage = "models"
	StagePostflight        Stage = "postflight"
)

type Option func(*Engine)

// WithStageHook is intended for deterministic fault injection and instrumentation.
func WithStageHook(hook func(Stage) error) Option {
	return func(e *Engine) { e.stageHook = hook }
}

func WithPointerStore(store PointerStore) Option {
	return func(e *Engine) { e.pointerStore = store }
}

// WithRecovery stores the single pending update record at statePath and uses
// locker for the whole recovery/update transaction. An empty path selects a
// private component-adjacent default; a nil locker selects an adjacent OS lock.
func WithRecovery(statePath string, locker MutationLocker) Option {
	return func(e *Engine) {
		e.recoveryPath = statePath
		e.recoveryLocker = locker
	}
}
