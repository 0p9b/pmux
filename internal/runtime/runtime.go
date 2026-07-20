// Package runtime constructs PMux's native single-host application runtime.
package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0p9b/pmux/internal/adapter/auth"
	"github.com/0p9b/pmux/internal/adapter/bundle"
	clientclaude "github.com/0p9b/pmux/internal/adapter/client/claude"
	"github.com/0p9b/pmux/internal/adapter/configfile"
	"github.com/0p9b/pmux/internal/adapter/discovery"
	adapterdoctor "github.com/0p9b/pmux/internal/adapter/doctor"
	adapterfs "github.com/0p9b/pmux/internal/adapter/fs"
	"github.com/0p9b/pmux/internal/adapter/installer"
	"github.com/0p9b/pmux/internal/adapter/mgmtapi"
	adaptermodels "github.com/0p9b/pmux/internal/adapter/models"
	adapterplatform "github.com/0p9b/pmux/internal/adapter/platform"
	"github.com/0p9b/pmux/internal/adapter/service/foreground"
	"github.com/0p9b/pmux/internal/adapter/service/health"
	"github.com/0p9b/pmux/internal/adapter/service/launchd"
	"github.com/0p9b/pmux/internal/adapter/service/systemd"
	"github.com/0p9b/pmux/internal/adapter/updater"
	"github.com/0p9b/pmux/internal/app"
	domainclient "github.com/0p9b/pmux/internal/domain/client"
	domainconfig "github.com/0p9b/pmux/internal/domain/config"
	domaindoctor "github.com/0p9b/pmux/internal/domain/doctor"
	domaininstall "github.com/0p9b/pmux/internal/domain/install"
	"github.com/0p9b/pmux/internal/domain/management"
	domainmodel "github.com/0p9b/pmux/internal/domain/model"
	domainplatform "github.com/0p9b/pmux/internal/domain/platform"
	"github.com/0p9b/pmux/internal/domain/provider"
	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/domain/update"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/state"
	pmuxversion "github.com/0p9b/pmux/internal/version"
)

// Options are process-owned presentation boundaries. Network clients are
// constructed but never contacted until an explicit command requests it.
type Options struct {
	ConfigDir      string
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	HTTPClient     *http.Client
}

// NewNative loads canonical roots and versioned state, then wires the concrete
// app router to native adapters. Construction performs no network request.
func NewNative(options Options) (app.UseCases, error) {
	platform, err := adapterplatform.New(options.ConfigDir)
	if err != nil {
		return nil, ensureTyped(err, "PMux could not initialize the native platform.")
	}
	roots, err := loadRoots(platform)
	if err != nil {
		return nil, err
	}
	store, err := state.New(state.Paths{Config: filepath.Join(roots.Config, "config.json"), State: filepath.Join(roots.State, "state.json"), Secrets: filepath.Join(roots.State, "secrets.json")})
	if err != nil {
		return nil, ensureTyped(err, "PMux could not initialize its versioned state store.")
	}
	// Validate schema compatibility now; this is local read-only I/O.
	if _, err := store.LoadConfig(); err != nil {
		return nil, ensureTyped(err, "PMux configuration could not be loaded.")
	}
	if _, err := store.LoadState(); err != nil {
		return nil, ensureTyped(err, "PMux state could not be loaded.")
	}
	if _, err := store.LoadSecretReferences(); err != nil {
		return nil, ensureTyped(err, "PMux secret references could not be loaded.")
	}

	native := &nativeRuntime{platform: platform, roots: roots, store: store, http: options.HTTPClient, stdin: valueReader(options.Stdin, os.Stdin), stdout: valueWriter(options.Stdout, os.Stdout), stderr: valueWriter(options.Stderr, os.Stderr), foreground: make(map[string]*foreground.Manager)}
	installAdapter, err := installer.New(installer.Options{
		Target:         domaininstall.Target{OS: goruntime.GOOS, Arch: goruntime.GOARCH},
		DataRoot:       roots.Data,
		HTTPClient:     options.HTTPClient,
		RestoreService: native.restoreManagedServiceCheckpoint,
	})
	if err != nil {
		return nil, ensureTyped(err, "PMux could not initialize the managed installer.")
	}
	native.installer = installAdapter
	if err := native.installer.Recover(context.Background()); err != nil {
		return nil, ensureTyped(err, "PMux could not recover an interrupted managed installation.")
	}

	deps := app.Dependencies{
		Roots: roots, Store: store, Setup: native,
		Management: native.management, Models: native.models, Services: native.service,
		Configs: native.config, Auth: native.auth, Launcher: native.launcher,
		Secrets: native.loadProxyKey, KnownSecrets: native.loadKnownSecrets, ModelTester: native, ConfigFiles: native, PMuxConfig: native, Doctor: native, Bundle: native,
		Updates: native, Input: native.stdin, ReadPassword: native.readPassword, VerifyPrivateFile: func(path string) error { return platform.VerifySecurePermissions(path, false) }, WorkingDir: os.Getwd, Now: time.Now,
		Output: native.stdout,
	}
	router, err := app.NewRouter(deps)
	if err != nil {
		return nil, err
	}
	return NewGovernedUseCases(router, roots.State)
}

// New is the default native composition used by cmd/pmux.
func New() (app.UseCases, error) { return NewNative(Options{}) }

type nativeRuntime struct {
	platform        domainplatform.Platform
	roots           domainplatform.Roots
	store           *state.Store
	installer       *installer.Adapter
	http            *http.Client
	stdin           io.Reader
	stdout, stderr  io.Writer
	mu              sync.Mutex
	foreground      map[string]*foreground.Manager
	serviceFactory  app.ServiceFactory
	discover        func() discovery.Discoverer
	adoptedServices adoptedServiceCutover
}

func loadRoots(platform domainplatform.Platform) (domainplatform.Roots, error) {
	config, err := platform.ConfigDir()
	if err != nil {
		return domainplatform.Roots{}, ensureTyped(err, "PMux could not resolve its config root.")
	}
	stateRoot, err := platform.StateDir()
	if err != nil {
		return domainplatform.Roots{}, ensureTyped(err, "PMux could not resolve its state root.")
	}
	cache, err := platform.CacheDir()
	if err != nil {
		return domainplatform.Roots{}, ensureTyped(err, "PMux could not resolve its cache root.")
	}
	data, err := platform.DataDir()
	if err != nil {
		return domainplatform.Roots{}, ensureTyped(err, "PMux could not resolve its data root.")
	}
	return domainplatform.Roots{Config: config, State: stateRoot, Cache: cache, Data: data}, nil
}

func (n *nativeRuntime) Setup(ctx context.Context, request app.SetupRequest) (out app.SetupOutcome, retErr error) {
	if request.Mode == "adopt" {
		return n.adopt(ctx, request)
	}
	return n.managed(ctx, request)
}

func (n *nativeRuntime) managed(ctx context.Context, request app.SetupRequest) (out app.SetupOutcome, retErr error) {
	if !request.Interactive && !request.Yes {
		return out, usage("Noninteractive managed setup requires `--yes`; no changes were made.")
	}
	if err := n.installer.Recover(ctx); err != nil {
		return out, err
	}
	current, err := n.store.LoadState()
	if err != nil {
		return out, err
	}
	for _, installation := range current.Installations {
		if installation.Kind == "managed" {
			return app.SetupOutcome{Installation: installation, CoreComplete: true, NextActions: onboardingActions()}, nil
		}
	}
	previousState := current
	previousState.Installations = append([]state.Installation(nil), current.Installations...)
	previousRefs, err := n.store.LoadSecretReferences()
	if err != nil {
		return out, err
	}
	refs := cloneSecretReferences(previousRefs)
	staging, err := os.MkdirTemp(n.roots.Cache, "pmux-install-")
	if err != nil {
		if mkErr := os.MkdirAll(n.roots.Cache, 0o700); mkErr != nil {
			return out, pmuxerr.Wrap(mkErr, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "PMux could not create its cache root.")
		}
		staging, err = os.MkdirTemp(n.roots.Cache, "pmux-install-")
	}
	if err != nil {
		return out, pmuxerr.Wrap(err, pmuxerr.InstallDownloadFailed, pmuxerr.Environment, "PMux could not create private installation staging.")
	}
	defer os.RemoveAll(staging)
	release, err := n.installer.Resolve(ctx, installer.ManagedDefaultVersion)
	if err != nil {
		return out, err
	}
	if err := n.installer.BeginSetup(ctx, release); err != nil {
		return out, err
	}
	setupOpen := true
	defer func() {
		if !setupOpen {
			return
		}
		_ = n.installer.Recover(context.Background())
	}()
	asset, err := n.installer.Download(ctx, release, staging)
	if err != nil {
		return out, err
	}
	checksums, err := n.installer.DownloadChecksums(ctx, release)
	if err != nil {
		return out, err
	}
	if err := n.installer.VerifyArchive(ctx, asset, checksums); err != nil {
		return out, err
	}
	extracted, err := n.installer.Extract(ctx, asset, filepath.Join(staging, "extracted"))
	if err != nil {
		return out, err
	}
	if err := n.installer.VerifyExecutable(ctx, extracted, domaininstall.Target{OS: goruntime.GOOS, Arch: goruntime.GOARCH}); err != nil {
		return out, err
	}
	if err := n.installer.Install(ctx, extracted); err != nil {
		return out, err
	}
	var instanceRoot string
	var manager service.ServiceManager
	serviceInstalled := false
	committed := false
	defer func() {
		if retErr == nil || committed {
			return
		}
		if serviceInstalled && manager != nil {
			_ = manager.Stop(context.Background(), 15*time.Second)
			_ = manager.Uninstall(context.Background())
		}
		_ = n.store.SaveState(previousState)
		_ = n.store.SaveSecretReferences(previousRefs)
		if instanceRoot != "" {
			_ = os.RemoveAll(instanceRoot)
		}
		_ = n.installer.Rollback(context.Background())
	}()

	id := "default"
	instanceRoot = filepath.Join(n.roots.Data, "instances", id)
	configPath := filepath.Join(instanceRoot, "config.yaml")
	authDir := filepath.Join(instanceRoot, "auth")
	runtimeDir := filepath.Join(instanceRoot, "runtime")
	sidecar := filepath.Join(instanceRoot, "api-key.txt")
	for _, path := range []string{instanceRoot, authDir, runtimeDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return out, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "PMux could not create the managed instance layout.")
		}
		if err := n.platform.SecurePermissions(path, true); err != nil {
			return out, err
		}
	}
	proxyKey, err := configfile.GenerateProxyKey()
	if err != nil {
		return out, err
	}
	managementKey, err := randomSecret()
	if err != nil {
		return out, err
	}
	managementPath := filepath.Join(n.roots.State, "management", id+".key")
	if err := writeSecret(sidecar, []byte(proxyKey+"\n"), n.platform); err != nil {
		return out, err
	}
	if err := writeSecret(managementPath, []byte(managementKey+"\n"), n.platform); err != nil {
		return out, err
	}
	configBody := []byte(fmt.Sprintf("host: 127.0.0.1\nport: 8317\nauth-dir: %s\napi-keys:\n  - %s\nremote-management:\n  allow-remote: false\n  disable-control-panel: true\n  secret-key: %s\nws-auth: true\n", yamlQuote(authDir), yamlQuote(proxyKey), yamlQuote(managementKey)))
	if err := writeSecret(configPath, configBody, n.platform); err != nil {
		return out, err
	}
	configBackupDir := filepath.Join(n.roots.State, "backups", id)
	if err := os.MkdirAll(configBackupDir, 0o700); err != nil {
		return out, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "PMux could not create managed config backup storage.")
	}
	configBackupPath := filepath.Join(configBackupDir, "config.yaml.setup.bak")
	if err := writeSecret(configBackupPath, configBody, n.platform); err != nil {
		return out, err
	}
	if err := n.installer.CheckpointConfig(ctx, installer.ConfigCheckpoint{Path: configPath, BackupPath: configBackupPath}); err != nil {
		return out, err
	}
	executable := "cli-proxy-api"
	if goruntime.GOOS == "windows" {
		executable += ".exe"
	}
	binaryPath := filepath.Join(n.roots.Data, "cli-proxy-api", "versions", release.Version, executable)
	binarySHA256, err := fileSHA256(binaryPath)
	if err != nil {
		return out, err
	}
	installation := state.Installation{ID: id, Kind: "managed", BinaryPath: binaryPath, BinarySHA256: binarySHA256, ConfigPath: configPath, ProxyKeyRef: secretReference(sidecar, proxyKey), AuthDir: authDir, RuntimeDir: runtimeDir, Host: "127.0.0.1", Port: 8317, ServiceBackend: defaultBackend(ctx), CoreVersionSeen: release.Version, ObservedAt: time.Now().UTC()}
	current.Installations = append(append([]state.Installation(nil), current.Installations...), installation)
	if err := n.store.SaveState(current); err != nil {
		return out, err
	}
	refs.Management[id] = secretReference(managementPath, managementKey)
	if err := n.store.SaveSecretReferences(refs); err != nil {
		return out, err
	}
	manager, err = n.service(ctx, installation, false)
	if err != nil {
		return out, err
	}
	backend := service.ServiceBackend(installation.ServiceBackend)
	spec := appServiceSpec(n.roots, installation, backend)
	if err := n.installer.CheckpointService(ctx, installer.ServiceCheckpoint{Identity: spec.Identity}); err != nil {
		return out, err
	}
	if err := manager.Install(ctx, spec); err != nil {
		return out, err
	}
	serviceInstalled = true
	if err := manager.Start(ctx); err != nil {
		return out, err
	}
	status, err := manager.Status(ctx)
	if err != nil {
		return out, err
	}
	if !status.Healthy {
		return out, pmuxerr.New(pmuxerr.ServiceHealthDeadline, pmuxerr.Environment, "CLIProxyAPI did not become healthy after managed setup.")
	}
	if err := n.installer.Complete(ctx); err != nil {
		return out, err
	}
	setupOpen = false
	committed = true
	return app.SetupOutcome{Installation: installation, CoreComplete: true, NextActions: onboardingActions()}, nil
}

func (n *nativeRuntime) adopt(ctx context.Context, request app.SetupRequest) (app.SetupOutcome, error) {
	discoverer := discovery.NewLocal()
	if n.discover != nil {
		discoverer = n.discover()
	}
	candidates, err := discoverer.Discover(ctx, discovery.Request{ProxyPath: request.ProxyPath, ConfigPath: request.ConfigPath})
	if err != nil {
		return app.SetupOutcome{}, err
	}
	if len(candidates) == 0 {
		return app.SetupOutcome{}, pmuxerr.New(pmuxerr.ConfigUnreadable, pmuxerr.Environment, "No importable CLIProxyAPI installation was found at the supplied paths.")
	}
	candidate := candidates[0]
	if request.ProxyPath != "" || request.ConfigPath != "" {
		proxyPath, _ := filepath.Abs(request.ProxyPath)
		configPath, _ := filepath.Abs(request.ConfigPath)
		for _, discovered := range candidates {
			if discovered.Binary != nil && discovered.Config != nil &&
				(request.ProxyPath == "" || discovered.Binary.Path == proxyPath) &&
				(request.ConfigPath == "" || discovered.Config.Path == configPath) {
				candidate = discovered
				break
			}
		}
	}
	if candidate.Container != nil {
		if request.Harden {
			return app.SetupOutcome{}, pmuxerr.New(pmuxerr.ServiceForeignOwner, pmuxerr.Environment, "This CLIProxyAPI runs in Docker; its lifecycle and configuration are owned by the container runtime. Manage it with Docker; PMux mutation actions are disabled.")
		}
		if candidate.Config == nil || candidate.Container.ConfigMount == "" {
			return app.SetupOutcome{}, pmuxerr.New(pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "Container adoption requires a readable known host config bind mount.")
		}
		if candidate.Port == nil || !candidate.Port.Healthy || candidate.Container.Endpoint == "" {
			return app.SetupOutcome{}, pmuxerr.New(pmuxerr.ServiceHealthDeadline, pmuxerr.Environment, "Container adoption requires a healthy CLIProxyAPI endpoint published on loopback.")
		}
		host, portText, splitErr := net.SplitHostPort(candidate.Container.Endpoint)
		port, portErr := strconv.Atoi(portText)
		if splitErr != nil || portErr != nil {
			return app.SetupOutcome{}, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.Environment, "Container adoption discovered an invalid loopback endpoint.")
		}
		installation := state.Installation{
			ID: "default", Kind: "container", ConfigPath: candidate.Config.Path,
			Host: host, Port: port, ServiceBackend: string(service.BackendDockerUnmanaged),
			CoreVersionSeen: valueOr(candidate.Port.CoreVersion, "unknown"), ObservedAt: time.Now().UTC(),
			Container: &state.ContainerMetadata{
				Runtime: candidate.Container.Runtime, ID: candidate.Container.ID,
				Name: candidate.Container.Name, Image: candidate.Container.Image,
				Endpoint: candidate.Container.Endpoint, ConfigMount: candidate.Container.ConfigMount,
			},
		}
		current, loadErr := n.store.LoadState()
		if loadErr != nil {
			return app.SetupOutcome{}, loadErr
		}
		current.Installations = replaceInstallation(current.Installations, installation)
		if saveErr := n.store.SaveState(current); saveErr != nil {
			return app.SetupOutcome{}, saveErr
		}
		return app.SetupOutcome{Installation: installation, CoreComplete: true, NextActions: onboardingActions()}, nil
	}
	if candidate.Binary == nil || candidate.Config == nil {
		return app.SetupOutcome{}, pmuxerr.New(pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "Adoption requires an unambiguous binary and config path.")
	}
	adapter := configfile.New(filepath.Join(n.roots.State, "backups", "default"))
	snapshot, err := adapter.Read(ctx, candidate.Config.Path)
	if err != nil {
		return app.SetupOutcome{}, err
	}
	masked, fingerprint := "********", fingerprintOf("")
	if len(snapshot.Config.APIKeys) > 0 {
		masked = mask(snapshot.Config.APIKeys[0])
		fingerprint = fingerprintOf(snapshot.Config.APIKeys[0])
	}
	backend := defaultBackend(ctx)
	if candidate.Service != nil {
		backend = string(candidate.Service.Backend)
	}
	installation := state.Installation{
		ID: "default", Kind: "adopted", BinaryPath: candidate.Binary.Path,
		ConfigPath:  candidate.Config.Path,
		ProxyKeyRef: state.SecretReference{Path: candidate.Config.Path, Masked: masked, Fingerprint: fingerprint},
		AuthDir:     snapshot.Config.AuthDir, RuntimeDir: filepath.Join(n.roots.Data, "instances", "default", "runtime"),
		Host: snapshot.Config.Host, Port: snapshot.Config.Port, ServiceBackend: backend,
		CoreVersionSeen: valueOr(candidate.Version.Version, "unknown"), ObservedAt: time.Now().UTC(),
	}
	current, err := n.store.LoadState()
	if err != nil {
		return app.SetupOutcome{}, err
	}
	current.Installations = replaceInstallation(current.Installations, installation)
	if err := n.store.SaveState(current); err != nil {
		return app.SetupOutcome{}, err
	}
	if !request.Harden {
		return app.SetupOutcome{Installation: installation, CoreComplete: true, NextActions: onboardingActions()}, nil
	}
	if !request.Yes {
		return app.SetupOutcome{}, usage("Adopted hardening is a separate transaction and requires explicit confirmation; no proxy files or services were changed.")
	}
	hardened, err := n.hardenAdoption(ctx, adapter, snapshot, installation, candidate.Service)
	if err != nil {
		return app.SetupOutcome{}, err
	}
	return app.SetupOutcome{Installation: hardened, CoreComplete: true, Hardened: true, NextActions: onboardingActions()}, nil
}

func (n *nativeRuntime) hardenAdoption(ctx context.Context, adapter *configfile.Adapter, snapshot domainconfig.ConfigSnapshot, installation state.Installation, observedService *discovery.ServiceEvidence) (_ state.Installation, retErr error) {
	originalConfig, err := os.ReadFile(installation.ConfigPath)
	if err != nil {
		return state.Installation{}, err
	}
	originalState, err := n.store.LoadState()
	if err != nil {
		return state.Installation{}, err
	}
	originalRefs, err := n.store.LoadSecretReferences()
	if err != nil {
		return state.Installation{}, err
	}
	basePlan, err := adapter.PlanManagedHardening(ctx, snapshot)
	if err != nil {
		return state.Installation{}, err
	}
	managementKey, err := randomSecret()
	if err != nil {
		return state.Installation{}, err
	}
	ops := append([]domainconfig.PatchOp(nil), basePlan.Operations...)
	ops = append(ops, domainconfig.PatchOp{Path: "remote-management.secret-key", Value: managementKey})
	plan, err := adapter.Plan(ctx, snapshot, ops)
	if err != nil {
		return state.Installation{}, err
	}
	instanceRoot := filepath.Dir(installation.RuntimeDir)
	sidecar := filepath.Join(instanceRoot, "api-key.txt")
	managementPath := filepath.Join(n.roots.State, "management", installation.ID+".key")
	runtimeExisted := pathExists(installation.RuntimeDir)
	sidecarBody, sidecarExisted := readExisting(sidecar)
	managementBody, managementExisted := readExisting(managementPath)
	manager, err := n.service(ctx, installation, false)
	if err != nil {
		return state.Installation{}, err
	}
	priorStatus, err := manager.Detect(ctx)
	if err != nil {
		return state.Installation{}, err
	}
	cutover := n.adoptedServices
	if cutover == nil {
		cutover = nativeAdoptedServiceCutover{}
	}
	var foreignRollback func(context.Context) error
	if observedService != nil && !observedService.PMuxOwned &&
		observedService.Identity != service.Identity(service.ServiceBackend(installation.ServiceBackend), installation.ID) {
		foreignRollback, err = cutover.Replace(ctx, *observedService)
		if err != nil {
			return state.Installation{}, err
		}
	}
	servicePath, serviceBody, serviceExisted := n.serviceArtifact(installation)
	serviceInstalled := false
	committed := false
	defer func() {
		if retErr == nil || committed {
			return
		}
		_ = manager.Stop(context.Background(), 15*time.Second)
		if serviceInstalled {
			_ = manager.Uninstall(context.Background())
		}
		if servicePath != "" {
			if serviceExisted {
				_ = adapterfs.AtomicWritePrivate(servicePath, serviceBody)
			} else {
				_ = os.Remove(servicePath)
			}
		}
		_ = adapterfs.AtomicWritePrivate(installation.ConfigPath, originalConfig)
		restoreExisting(sidecar, sidecarBody, sidecarExisted)
		restoreExisting(managementPath, managementBody, managementExisted)
		_ = n.store.SaveState(originalState)
		_ = n.store.SaveSecretReferences(originalRefs)
		if !runtimeExisted {
			_ = os.RemoveAll(instanceRoot)
		}
		if foreignRollback != nil {
			_ = foreignRollback(context.Background())
		}
		if priorStatus.State == service.ServiceRunning {
			_ = manager.Start(context.Background())
		}
	}()
	if err := os.MkdirAll(installation.RuntimeDir, 0o700); err != nil {
		return state.Installation{}, err
	}
	if err := n.platform.SecurePermissions(installation.RuntimeDir, true); err != nil {
		return state.Installation{}, err
	}
	if _, err := adapter.Apply(ctx, plan); err != nil {
		return state.Installation{}, err
	}
	hardenedSnapshot, err := adapter.Read(ctx, installation.ConfigPath)
	if err != nil {
		return state.Installation{}, err
	}
	if len(hardenedSnapshot.Config.APIKeys) == 0 {
		return state.Installation{}, actionable(pmuxerr.ConfigSafeMode, "Hardening produced no usable proxy key.", "Restore the config backup and retry.")
	}
	proxyKey := hardenedSnapshot.Config.APIKeys[0]
	if err := writeSecret(sidecar, []byte(proxyKey+"\n"), n.platform); err != nil {
		return state.Installation{}, err
	}
	if err := writeSecret(managementPath, []byte(managementKey+"\n"), n.platform); err != nil {
		return state.Installation{}, err
	}
	installation.ProxyKeyRef = secretReference(sidecar, proxyKey)
	installation.Host = hardenedSnapshot.Config.Host
	installation.Port = hardenedSnapshot.Config.Port
	installation.AuthDir = hardenedSnapshot.Config.AuthDir
	current, err := n.store.LoadState()
	if err != nil {
		return state.Installation{}, err
	}
	current.Installations = replaceInstallation(current.Installations, installation)
	if err := n.store.SaveState(current); err != nil {
		return state.Installation{}, err
	}
	refs := cloneSecretReferences(originalRefs)
	refs.Management[installation.ID] = secretReference(managementPath, managementKey)
	if err := n.store.SaveSecretReferences(refs); err != nil {
		return state.Installation{}, err
	}
	if err := manager.Install(ctx, appServiceSpec(n.roots, installation, service.ServiceBackend(installation.ServiceBackend))); err != nil {
		return state.Installation{}, err
	}
	serviceInstalled = priorStatus.State == service.ServiceNotInstalled
	status, err := manager.Restart(ctx)
	if err != nil {
		return state.Installation{}, err
	}
	if !status.Healthy {
		return state.Installation{}, actionable(pmuxerr.ServiceHealthDeadline, "Adopted CLIProxyAPI did not become healthy after service hardening.", "The hardening transaction will restore the prior config and service.")
	}
	if err := n.verifyProxyKey(ctx, installation, proxyKey); err != nil {
		return state.Installation{}, err
	}
	client, err := n.management(ctx, installation)
	if err != nil {
		return state.Installation{}, err
	}
	if _, err := client.AuthFiles(ctx); err != nil {
		return state.Installation{}, err
	}
	committed = true
	return installation, nil
}

func (n *nativeRuntime) management(ctx context.Context, installation state.Installation) (management.ManagementClient, error) {
	refs, err := n.store.LoadSecretReferences()
	if err != nil {
		return nil, err
	}
	managementRef, ok := refs.Management[installation.ID]
	if !ok {
		return nil, actionable(pmuxerr.ConfigUnreadable, "The management secret reference is missing.", "Run `pmux doctor --fix management-key` to generate and synchronize it.")
	}
	managementKey, err := n.loadSecret(ctx, managementRef)
	if err != nil {
		return nil, err
	}
	defer clear(managementKey)
	proxyKey, err := n.loadProxyKey(ctx, installation)
	if err != nil {
		return nil, err
	}
	defer clear(proxyKey)
	return mgmtapi.New(mgmtapi.Options{BaseURL: endpoint(installation), ManagementKey: string(managementKey), ProxyKey: string(proxyKey), HTTPClient: n.http})
}

func (n *nativeRuntime) models(_ context.Context, installation state.Installation) (domainmodel.ModelCatalog, error) {
	cache := &modelCache{path: filepath.Join(n.roots.Cache, "models", installation.ID+".json"), platform: n.platform}
	return adaptermodels.New(lazyManagement{runtime: n, installation: installation}, cache, favorites{store: n.store}, adaptermodels.Options{CacheKey: installation.ID}), nil
}

func (n *nativeRuntime) config(_ context.Context, installation state.Installation) (domainconfig.ConfigFile, error) {
	adapter := configfile.New(filepath.Join(n.roots.State, "backups", installation.ID))
	if installation.Kind == "container" {
		return readOnlyContainerConfig{inner: adapter}, nil
	}
	return adapter, nil
}

func (n *nativeRuntime) auth(ctx context.Context, installation state.Installation, providerID management.ProviderID) (provider.ProviderAuthenticator, error) {
	if installation.Kind == "container" {
		return nil, containerMutationError()
	}
	client, err := n.management(ctx, installation)
	if err != nil {
		return nil, err
	}
	return auth.New(providerID, client, nil, nil)
}

func (n *nativeRuntime) launcher(_ context.Context, installation state.Installation, invocation app.Invocation) (domainclient.ClientLauncher, error) {
	preflight := func(ctx context.Context, exactID string) error {
		catalog, err := n.models(ctx, installation)
		if err != nil {
			return err
		}
		entries, err := catalog.Refresh(ctx)
		if err != nil {
			return err
		}
		return requireLiveModel(entries, exactID)
	}
	launcher := clientclaude.New(clientclaude.Options{Stdin: n.stdin, Stdout: n.stdout, Stderr: n.stderr, JSONMode: invocation.JSON, ModelPreflight: preflight, SettingsPath: filepath.Join(userHome(), ".claude", "settings.json"), PersistenceStatePath: filepath.Join(n.roots.State, "claude-persistence.json")})
	if installation.Kind == "container" {
		return readOnlyContainerLauncher{inner: launcher}, nil
	}
	return launcher, nil
}

func (n *nativeRuntime) Test(ctx context.Context, installation state.Installation, exactID string, timeout time.Duration) (any, error) {
	catalog, err := n.models(ctx, installation)
	if err != nil {
		return nil, err
	}
	entries, err := catalog.Refresh(ctx)
	if err != nil {
		return nil, err
	}
	if err := requireLiveModel(entries, exactID); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	testCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	key, err := n.loadProxyKey(testCtx, installation)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	payload, err := json.Marshal(map[string]any{
		"model": exactID, "max_tokens": 1, "stream": false,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.UnhandledInternal, pmuxerr.Internal, "PMux could not encode the local model test.")
	}
	request, err := http.NewRequestWithContext(testCtx, http.MethodPost, endpoint(installation)+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigValidationFailed, pmuxerr.Internal, "PMux could not construct the local model test.")
	}
	request.Header.Set("Authorization", "Bearer "+string(key))
	request.Header.Set("Content-Type", "application/json")
	started := time.Now()
	response, err := httpClient(n.http).Do(request)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ManagementUnreachable, pmuxerr.Environment, "The local model test request failed.")
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ManagementUnreachable, pmuxerr.Environment, "The local model test response could not be read.")
	}
	if safeMode := response.Header.Get("X-Cpa-Safe-Mode"); safeMode != "" {
		return nil, actionable(pmuxerr.ConfigSafeMode, "CLIProxyAPI is in safe mode.", "Run `pmux doctor --fix KEY-SAFEMODE`.")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, actionable(pmuxerr.ServiceHealthDeadline, fmt.Sprintf("Model test failed with HTTP %d.", response.StatusCode), "Run `pmux doctor` and inspect provider status.")
	}
	var envelope struct {
		Choices []json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.Choices) == 0 {
		if err == nil {
			err = errors.New("completion response has no choices")
		}
		return nil, pmuxerr.Wrap(err, pmuxerr.ServiceHealthDeadline, pmuxerr.Upstream, "The model test returned a malformed completion response.")
	}
	return map[string]any{"model": exactID, "http_status": response.StatusCode, "latency_ms": time.Since(started).Milliseconds()}, nil
}

func requireLiveModel(entries []domainmodel.CatalogEntry, exactID string) error {
	for _, entry := range entries {
		if entry.ID != exactID {
			continue
		}
		if entry.Stale {
			return actionable(pmuxerr.ManagementUnreachable, fmt.Sprintf("Live availability for model %q could not be verified; the matching catalog entry is stale.", exactID), "Restore CLIProxyAPI connectivity, then run `pmux models list --refresh` before launching or testing the model.")
		}
		if entry.Available {
			return nil
		}
		return actionable(pmuxerr.ServiceHealthDeadline, fmt.Sprintf("Model %q is not currently available.", exactID), "Run `pmux providers verify --refresh-models`, then refresh the model catalog.")
	}
	return actionable(pmuxerr.ServiceHealthDeadline, fmt.Sprintf("Model %q is not in the current live catalog.", exactID), "Run `pmux models list --refresh` and choose an exact returned ID.")
}

func (n *nativeRuntime) service(ctx context.Context, installation state.Installation, forceForeground bool) (service.ServiceManager, error) {
	if installation.Kind == "container" {
		if installation.Container == nil {
			return nil, containerMutationError()
		}
		return discovery.DockerServiceManager{
			Container: discovery.ContainerEvidence{
				Runtime: installation.Container.Runtime, ID: installation.Container.ID,
				Name: installation.Container.Name, Image: installation.Container.Image,
				State: "running", Endpoint: installation.Container.Endpoint,
				ConfigMount: installation.Container.ConfigMount, CoreVersion: installation.CoreVersionSeen,
			},
			Probe: discovery.HTTPListenerProber{Client: n.http},
		}, nil
	}
	if n.serviceFactory != nil {
		return n.serviceFactory(ctx, installation, forceForeground)
	}
	checker := health.NewPoller(health.HTTPProbe{BaseURL: endpoint(installation), Client: n.http})
	backend := installation.ServiceBackend
	if forceForeground {
		manager := foreground.NewAttachedPersistent(
			foreground.OSRunner{},
			checker,
			filepath.Join(n.roots.State, "foreground-"+installation.ID+".json"),
			foreground.Streams{Stdin: n.stdin, Stdout: n.stdout, Stderr: n.stderr},
		)
		if err := manager.Install(ctx, appServiceSpec(n.roots, installation, service.BackendForeground)); err != nil {
			return nil, err
		}
		return manager, nil
	}
	switch service.ServiceBackend(backend) {
	case service.BackendForeground, "":
		n.mu.Lock()
		defer n.mu.Unlock()
		if existing := n.foreground[installation.ID]; existing != nil {
			return existing, nil
		}
		manager := foreground.NewPersistent(foreground.OSRunner{}, checker, filepath.Join(n.roots.State, "foreground-"+installation.ID+".json"))
		if err := manager.Install(ctx, appServiceSpec(n.roots, installation, service.BackendForeground)); err != nil {
			return nil, err
		}
		n.foreground[installation.ID] = manager
		return manager, nil
	case service.BackendSystemdUser:
		return systemd.New(installation.ID, filepath.Join(userHome(), ".config", "systemd", "user"), systemd.OSRunner{}, checker), nil
	case service.BackendLaunchd:
		return launchd.New(launchd.Config{InstanceID: installation.ID, PlistDir: filepath.Join(userHome(), "Library", "LaunchAgents"), UID: currentUID(), Health: checker})
	case service.BackendWindowsTask:
		return newWindowsTaskManager(appServiceSpec(n.roots, installation, service.BackendWindowsTask), n.platform, checker)
	default:
		return nil, actionable(pmuxerr.ServiceBackendUnavailable, fmt.Sprintf("Recorded service backend %q is unsupported.", backend), "Run `pmux doctor` and select a supported backend.")
	}
}

func (n *nativeRuntime) loadSecret(ctx context.Context, reference state.SecretReference) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reference.Path == "" || !filepath.IsAbs(reference.Path) {
		return nil, actionable(pmuxerr.ConfigUnreadable, "A private secret source path is missing or not absolute.", "Run `pmux doctor` to repair the installation record.")
	}
	body, err := os.ReadFile(reference.Path)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "PMux could not read a private secret source.")
	}
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return nil, actionable(pmuxerr.ConfigUnreadable, "A private secret source is empty.", "Run `pmux doctor` to repair the installation secret.")
	}
	return body, nil
}

func (n *nativeRuntime) loadProxyKey(ctx context.Context, installation state.Installation) ([]byte, error) {
	if installation.ProxyKeyRef.Path != installation.ConfigPath {
		return n.loadSecret(ctx, installation.ProxyKeyRef)
	}
	adapter := configfile.New(filepath.Join(n.roots.State, "backups", installation.ID))
	snapshot, err := adapter.Read(ctx, installation.ConfigPath)
	if err != nil {
		return nil, err
	}
	if len(snapshot.Config.APIKeys) == 0 {
		return nil, actionable(pmuxerr.ConfigUnreadable, "CLIProxyAPI has no configured proxy key.", "Run `pmux doctor --fix KEY-SAFEMODE`.")
	}
	for _, key := range snapshot.Config.APIKeys {
		if f := fingerprintOf(key); f == installation.ProxyKeyRef.Fingerprint {
			return []byte(key), nil
		}
	}
	return nil, actionable(pmuxerr.ConfigMutationConflict, "The recorded adopted proxy-key fingerprint no longer matches config.yaml.", "Run `pmux doctor` to refresh the adopted installation record.")
}
func (n *nativeRuntime) loadKnownSecrets(ctx context.Context, installation state.Installation) ([][]byte, error) {
	if installation.Kind == "container" {
		return nil, nil
	}
	proxy, err := n.loadProxyKey(ctx, installation)
	if err != nil {
		return nil, err
	}
	refs, err := n.store.LoadSecretReferences()
	if err != nil {
		clear(proxy)
		return nil, err
	}
	reference, ok := refs.Management[installation.ID]
	if !ok {
		return [][]byte{proxy}, nil
	}
	managementKey, err := n.loadSecret(ctx, reference)
	if err != nil {
		clear(proxy)
		return nil, err
	}
	return [][]byte{proxy, managementKey}, nil
}

func (n *nativeRuntime) verifyProxyKey(ctx context.Context, installation state.Installation, key string) error {
	if _, err := (health.HTTPProbe{BaseURL: endpoint(installation), Client: n.http}).Probe(ctx); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint(installation)+"/v1/models", nil)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigValidationFailed, pmuxerr.Internal, "PMux could not construct authenticated proxy verification.")
	}
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := httpClient(n.http).Do(request)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ManagementUnreachable, pmuxerr.Environment, "Authenticated proxy verification could not reach CLIProxyAPI.")
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.Header.Get("X-Cpa-Safe-Mode") != "" {
		return actionable(pmuxerr.ConfigSafeMode, "CLIProxyAPI remained in safe mode after the proxy-key change.", "Restore the prior configuration and inspect `pmux doctor`.")
	}
	if response.StatusCode != http.StatusOK {
		return actionable(pmuxerr.ManagementAuthRejected, fmt.Sprintf("Authenticated proxy verification returned HTTP %d.", response.StatusCode), "Restore the prior key and inspect `pmux doctor`.")
	}
	return nil
}

func (n *nativeRuntime) Backup(_ context.Context, path string) (string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	instance := installationIDForPath(n.roots.Data, path)
	backups, err := adapterfs.NewBackups(filepath.Join(n.roots.State, "backups"), 10)
	if err != nil {
		return "", err
	}
	return backups.Create(instance, filepath.Base(path), body)
}
func (n *nativeRuntime) Restore(_ context.Context, target, backup string) (any, error) {
	if n.isContainerConfigPath(target) {
		return nil, containerMutationError()
	}
	if !filepath.IsAbs(backup) {
		backup = filepath.Join(n.roots.State, "backups", installationIDForPath(n.roots.Data, target), backup)
	}
	if !within(filepath.Join(n.roots.State, "backups"), backup) {
		return nil, actionable(pmuxerr.ServiceForeignOwner, "PMux will not restore a backup outside its canonical backup root.", "Choose a backup returned by `pmux config backup`.")
	}
	body, err := os.ReadFile(backup)
	if err != nil {
		return nil, err
	}
	if err := writeSecret(target, body, n.platform); err != nil {
		return nil, err
	}
	return map[string]any{"path": target, "backup": backup}, nil
}

func (n *nativeRuntime) Run(ctx context.Context, installation state.Installation, checks, fixes []string, fixing, yes, online bool) (any, bool, error) {
	if fixing && installation.Kind == "container" {
		return nil, true, containerMutationError()
	}
	source := &doctorSource{runtime: n, installation: installation, online: online}
	registry, err := adapterdoctor.NewDefaultRegistry(source)
	if err != nil {
		return nil, true, err
	}
	if err := registry.RegisterFix(&safeModeFix{
		adapter:      configfile.New(filepath.Join(n.roots.State, "backups", installation.ID)),
		installation: installation,
		platform:     n.platform,
		store:        n.store,
		verify: func(ctx context.Context, key string) error {
			return n.verifyProxyKey(ctx, installation, key)
		},
	}); err != nil {
		return nil, true, err
	}
	if fixing {
		report, err := (adapterdoctor.FixRunner{Registry: registry}).Run(ctx, fixes, adapterdoctor.FixOptions{Yes: yes})
		if err != nil {
			return report, true, err
		}
		return report, report.Report.Summary.ExitCode != 0, nil
	}
	report, err := (adapterdoctor.Runner{Registry: registry}).Run(ctx, checks...)
	if err != nil {
		return nil, true, err
	}
	return report, report.Summary.ExitCode != 0, nil
}

type safeModeFix struct {
	adapter              *configfile.Adapter
	installation         state.Installation
	platform             domainplatform.Platform
	store                *state.Store
	verify               func(context.Context, string) error
	wait                 func(context.Context, time.Duration) error
	verifyInterval       time.Duration
	verifyTimeout        time.Duration
	originalConfig       []byte
	originalKey          []byte
	originalInstallation state.Installation
	keyExisted           bool
	stateChanged         bool
	changed              bool
}

func (f *safeModeFix) ID() string      { return "fix-safe-mode" }
func (f *safeModeFix) CheckID() string { return adapterdoctor.CheckSafeMode }

func (f *safeModeFix) Apply(ctx context.Context, dryRun bool) (domaindoctor.FixResult, error) {
	snapshot, err := f.adapter.Read(ctx, f.installation.ConfigPath)
	if err != nil {
		return domaindoctor.FixResult{}, err
	}
	keys := make([]string, 0, len(snapshot.Config.APIKeys))
	for _, key := range snapshot.Config.APIKeys {
		if !configfile.IsTemplateAPIKey(key) {
			keys = append(keys, key)
		}
	}
	if len(keys) != 0 {
		return domaindoctor.FixResult{Summary: "No template proxy key remains.", Verified: true}, nil
	}
	key, err := configfile.GenerateProxyKey()
	if err != nil {
		return domaindoctor.FixResult{}, err
	}
	plan, err := f.adapter.Plan(ctx, snapshot, []domainconfig.PatchOp{{Path: "api-keys", Value: []string{key}}})
	if err != nil {
		return domaindoctor.FixResult{}, err
	}
	if dryRun {
		return domaindoctor.FixResult{Summary: "Generate and install a private non-placeholder proxy key."}, nil
	}
	f.originalConfig, err = os.ReadFile(f.installation.ConfigPath)
	if err != nil {
		return domaindoctor.FixResult{}, err
	}
	sidecar := f.installation.ProxyKeyRef.Path != f.installation.ConfigPath
	if sidecar {
		f.originalKey, err = os.ReadFile(f.installation.ProxyKeyRef.Path)
		if err == nil {
			f.keyExisted = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return domaindoctor.FixResult{}, err
		}
	}
	f.changed = true
	if sidecar {
		if err := writeSecret(f.installation.ProxyKeyRef.Path, []byte(key+"\n"), f.platform); err != nil {
			return domaindoctor.FixResult{}, err
		}
	}
	if _, err := f.adapter.Apply(ctx, plan); err != nil {
		return domaindoctor.FixResult{}, err
	}
	if err := f.platform.SecurePermissions(f.installation.ConfigPath, false); err != nil {
		return domaindoctor.FixResult{}, err
	}
	verified, err := f.adapter.Read(ctx, f.installation.ConfigPath)
	if err != nil {
		return domaindoctor.FixResult{}, err
	}
	for _, configured := range verified.Config.APIKeys {
		if configfile.IsTemplateAPIKey(configured) {
			return domaindoctor.FixResult{Changed: true, Summary: "The replacement key did not clear safe mode."}, nil
		}
	}
	if f.verify == nil {
		return domaindoctor.FixResult{Changed: true, Summary: "The replacement key could not be authenticated because no verifier is configured."}, pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "safe-mode fix requires authenticated proxy verification")
	}
	if err := f.verifyWithRetry(ctx, key); err != nil {
		return domaindoctor.FixResult{Changed: true, Summary: "The replacement key failed authenticated proxy verification."}, err
	}
	f.installation.ProxyKeyRef = secretReference(f.installation.ProxyKeyRef.Path, key)
	if f.store != nil {
		current, err := f.store.LoadState()
		if err != nil {
			return domaindoctor.FixResult{}, err
		}
		for _, installed := range current.Installations {
			if installed.ID == f.installation.ID {
				f.originalInstallation = installed
				break
			}
		}
		current.Installations = replaceInstallation(current.Installations, f.installation)
		if err := f.store.SaveState(current); err != nil {
			return domaindoctor.FixResult{}, err
		}
		f.stateChanged = true
	}
	return domaindoctor.FixResult{Changed: true, Verified: true, Summary: "Safe mode proxy key replaced and authenticated proxy access verified."}, nil
}

func (f *safeModeFix) verifyWithRetry(ctx context.Context, key string) error {
	interval := f.verifyInterval
	if interval <= 0 {
		interval = time.Second
	}
	timeout := f.verifyTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	wait := f.wait
	if wait == nil {
		wait = func(ctx context.Context, duration time.Duration) error {
			timer := time.NewTimer(duration)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}
	}
	verifyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	attempts := int(timeout/interval) + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if err := f.verify(verifyCtx, key); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if attempt+1 == attempts {
			break
		}
		if err := wait(verifyCtx, interval); err != nil {
			if lastErr != nil {
				return lastErr
			}
			return err
		}
	}
	return lastErr
}

func (f *safeModeFix) Rollback(context.Context) error {
	if !f.changed {
		return nil
	}
	if err := adapterfs.AtomicWritePrivate(f.installation.ConfigPath, f.originalConfig); err != nil {
		return err
	}
	if f.installation.ProxyKeyRef.Path != f.installation.ConfigPath {
		if f.keyExisted {
			if err := adapterfs.AtomicWritePrivate(f.installation.ProxyKeyRef.Path, f.originalKey); err != nil {
				return err
			}
		} else if err := os.Remove(f.installation.ProxyKeyRef.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if f.stateChanged {
		current, err := f.store.LoadState()
		if err != nil {
			return err
		}
		current.Installations = replaceInstallation(current.Installations, f.originalInstallation)
		if err := f.store.SaveState(current); err != nil {
			return err
		}
	}
	return nil
}

func (n *nativeRuntime) Bundle(ctx context.Context, installation state.Installation, destination string) (any, error) {
	if destination == "" || destination == "<default>" {
		destination = filepath.Join(n.roots.State, "pmux-bundle-"+time.Now().UTC().Format("20060102T150405Z")+".zip")
	}
	entries := make([]bundle.Entry, 0, 8)
	stateBody, _ := json.Marshal(map[string]any{"installation": safeInstallation(installation), "roots": n.roots})
	entries = append(entries, bundle.Entry{ArchivePath: "state.json", Kind: bundle.KindJSON, Data: stateBody})

	source := &doctorSource{runtime: n, installation: installation}
	if registry, err := adapterdoctor.NewDefaultRegistry(source); err == nil {
		if report, reportErr := (adapterdoctor.Runner{Registry: registry}).Run(ctx); reportErr == nil {
			if body, marshalErr := report.JSON(); marshalErr == nil {
				entries = append(entries, bundle.Entry{ArchivePath: "doctor.json", Kind: bundle.KindJSON, Data: body})
			}
		}
	}
	configAdapter := configfile.New(filepath.Join(n.roots.State, "backups", installation.ID))
	if snapshot, err := configAdapter.Read(ctx, installation.ConfigPath); err == nil {
		configSummary, _ := json.Marshal(map[string]any{
			"path": snapshot.Path, "host": snapshot.Config.Host, "port": snapshot.Config.Port,
			"auth_dir": snapshot.Config.AuthDir, "ws_auth": snapshot.Config.WSAuth,
			"api_key_count": len(snapshot.Config.APIKeys), "management_local": snapshot.Config.ManagementLocal,
		})
		entries = append(entries, bundle.Entry{ArchivePath: "config-summary.json", Kind: bundle.KindJSON, Data: configSummary})
	}
	if serviceFact, err := source.Service(ctx); err == nil {
		serviceBody, _ := json.Marshal(map[string]any{
			"backend": serviceFact.Backend, "identity": service.Identity(service.ServiceBackend(installation.ServiceBackend), installation.ID),
			"installed": serviceFact.Installed, "running": serviceFact.Running, "crash_loop": serviceFact.CrashLoop,
			"definition_owned": serviceFact.DefinitionOwned, "identity_matches": serviceFact.IdentityMatches,
			"absolute_config": serviceFact.DefinitionUsesConfig, "environment_scrubbed": serviceFact.EnvironmentScrubbed,
			"runtime_dir_clean": serviceFact.RuntimeDirClean, "detail": serviceFact.Detail,
		})
		entries = append(entries, bundle.Entry{ArchivePath: "service.json", Kind: bundle.KindJSON, Data: serviceBody})
	}
	if manager, err := n.service(ctx, installation, false); err == nil {
		if logs, logErr := manager.Logs(ctx, 200, false); logErr == nil {
			logBody, readErr := io.ReadAll(io.LimitReader(logs, 256<<10))
			closeErr := logs.Close()
			if readErr == nil && closeErr == nil && len(logBody) > 0 {
				entries = append(entries, bundle.Entry{ArchivePath: "logs/service.log", Kind: bundle.KindText, Data: logBody})
			}
		}
	}
	for _, record := range []struct{ archive, path string }{
		{"logs/pmux.log", filepath.Join(n.roots.State, "logs", "pmux.log")},
		{"audit.jsonl", filepath.Join(n.roots.State, "audit.jsonl")},
		{"journal.jsonl", filepath.Join(n.roots.State, "journal.jsonl")},
	} {
		if body, err := readBoundedTail(record.path, 256<<10); err == nil && len(body) > 0 {
			entries = append(entries, bundle.Entry{ArchivePath: record.archive, SourcePath: record.path, Kind: bundle.KindText, Data: body})
		}
	}
	knownSecrets, secretBuffers := n.bundleKnownSecrets(ctx, installation)
	defer func() {
		for _, buffer := range secretBuffers {
			clear(buffer)
		}
		for index := range knownSecrets {
			knownSecrets[index] = ""
		}
	}()
	builder := bundle.Builder{
		AuthRoots: []string{installation.AuthDir}, KnownSecrets: knownSecrets,
		SecurePermissions: n.platform.SecurePermissions, VerifySecurePermissions: n.platform.VerifySecurePermissions,
	}
	result, err := builder.Build(ctx, destination, entries)
	builder.KnownSecrets = nil
	return result, err
}
func (n *nativeRuntime) bundleKnownSecrets(ctx context.Context, installation state.Installation) ([]string, [][]byte) {
	var secrets []string
	var buffers [][]byte
	adapter := configfile.New(filepath.Join(n.roots.State, "backups", installation.ID))
	if snapshot, err := adapter.Read(ctx, installation.ConfigPath); err == nil {
		for _, key := range snapshot.Config.APIKeys {
			buffer := []byte(key)
			buffers = append(buffers, buffer)
			secrets = append(secrets, string(buffer))
		}
	}
	if references, err := n.store.LoadSecretReferences(); err == nil {
		if reference, ok := references.Management[installation.ID]; ok {
			if buffer, readErr := os.ReadFile(reference.Path); readErr == nil {
				buffer = bytes.TrimSpace(buffer)
				if len(buffer) > 0 {
					buffers = append(buffers, buffer)
					secrets = append(secrets, string(buffer))
				}
			}
		}
	}
	return secrets, buffers
}

func readBoundedTail(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	offset := info.Size() - limit
	if offset < 0 {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(file, limit))
}
func (n *nativeRuntime) Check(ctx context.Context, component string, installation state.Installation) (any, error) {
	source := updater.NewGitHubSource(n.http, updater.NativeTarget())
	engine := updater.New(source, nil, nil, updater.CommandSelfVerifier{})
	components := []update.Component{update.Self, update.Proxy}
	if component == "self" {
		components = []update.Component{update.Self}
	}
	if component == "proxy" {
		components = []update.Component{update.Proxy}
	}
	results := make([]update.Release, 0, len(components))
	for _, item := range components {
		current := pmuxversion.Current().Version
		if item == update.Proxy {
			current = installation.CoreVersionSeen
		}
		release, err := engine.Check(ctx, updater.CheckRequest{Component: item, CurrentVersion: current})
		if err != nil {
			return nil, err
		}
		results = append(results, release)
	}
	return map[string]any{"releases": results, "changed": false}, nil
}
func (n *nativeRuntime) Self(ctx context.Context, requested string) (any, error) {
	current := pmuxversion.Current().Version
	if current == "" || current == "dev" {
		return nil, actionable(pmuxerr.ConfigMutationConflict, "Development builds are not eligible for self-update.", "Install a checksum-verified PMux release before using `pmux update self`.")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "PMux could not resolve its active executable.")
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "PMux could not resolve its active executable.")
	}
	source := updater.NewGitHubSource(n.http, updater.NativeTarget())
	engine := updater.New(source, nil, nil, updater.CommandSelfVerifier{})
	return engine.UpdateSelf(ctx, updater.SelfRequest{
		CurrentVersion: current,
		Version:        requested,
		Ownership:      selfOwnership(executable),
		ExecutablePath: executable,
		Target:         updater.NativeTarget(),
	})
}
func (n *nativeRuntime) Proxy(ctx context.Context, installation state.Installation, requested string) (any, error) {
	if installation.Kind == "container" {
		return nil, containerMutationError()
	}
	if installation.Kind != "managed" {
		return nil, pmuxerr.New(pmuxerr.ConfigMutationConflict, pmuxerr.User, "CLIProxyAPI is adopted, not PMux-managed; update it with its owning installation method.")
	}
	manager, err := n.service(ctx, installation, false)
	if err != nil {
		return nil, err
	}
	client, err := n.management(ctx, installation)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(n.roots.Data, "cli-proxy-api")
	currentPointer := filepath.Join(root, "current")
	updateService := &proxyUpdateService{
		runtime:      n,
		installation: installation,
		manager:      manager,
		current:      currentPointer,
	}
	engine := updater.New(
		updater.NewGitHubSource(n.http, updater.NativeTarget()),
		updateService,
		proxyUpdateVerifier{client: client},
		updater.CommandSelfVerifier{},
	)
	return engine.UpdateProxy(ctx, updater.ProxyRequest{
		CurrentVersion: installation.CoreVersionSeen,
		Version:        requested,
		Ownership:      updater.OwnershipManaged,
		VersionsDir:    filepath.Join(root, "versions"),
		CurrentPointer: currentPointer,
		Target:         updater.NativeTarget(),
		StopTimeout:    15 * time.Second,
	})
}

func selfOwnership(executable string) updater.Ownership {
	path := strings.ToLower(strings.ReplaceAll(filepath.ToSlash(executable), `\`, "/"))
	packageRoots := []string{"/usr/", "/opt/", "/nix/", "/snap/", "/home/linuxbrew/", "/cellar/", "/windowsapps/", "/scoop/apps/", "/go/bin/"}
	for _, marker := range packageRoots {
		if strings.Contains(path, marker) {
			return updater.OwnershipPackageManaged
		}
	}
	return updater.OwnershipManaged
}

type proxyUpdateService struct {
	runtime      *nativeRuntime
	installation state.Installation
	manager      service.ServiceManager
	current      string
}

func (s *proxyUpdateService) Status(ctx context.Context) (service.ServiceStatus, error) {
	return s.manager.Status(ctx)
}

func (s *proxyUpdateService) Stop(ctx context.Context, timeout time.Duration) error {
	return s.manager.Stop(ctx, timeout)
}

func (s *proxyUpdateService) Start(ctx context.Context) error {
	binary, version, err := selectedProxyBinary(s.current)
	if err != nil {
		return err
	}
	next := s.installation
	next.BinaryPath = binary
	next.CoreVersionSeen = version
	manager, err := s.runtime.service(ctx, next, false)
	if err != nil {
		return err
	}
	if err := manager.Uninstall(ctx); err != nil {
		return err
	}
	if err := manager.Install(ctx, appServiceSpec(s.runtime.roots, next, service.ServiceBackend(next.ServiceBackend))); err != nil {
		return err
	}
	if err := manager.Start(ctx); err != nil {
		return err
	}
	current, err := s.runtime.store.LoadState()
	if err != nil {
		return err
	}
	current.Installations = replaceInstallation(current.Installations, next)
	if err := s.runtime.store.SaveState(current); err != nil {
		_ = manager.Stop(context.Background(), 15*time.Second)
		return err
	}
	s.installation = next
	s.manager = manager
	return nil
}

type proxyUpdateVerifier struct {
	client management.ManagementClient
}

func (v proxyUpdateVerifier) Health(ctx context.Context) error {
	_, err := v.client.Health(ctx)
	return err
}

func (v proxyUpdateVerifier) Authenticated(ctx context.Context) error {
	_, err := v.client.PublicModels(ctx)
	return err
}

func (v proxyUpdateVerifier) Models(ctx context.Context) ([]string, error) {
	refs, err := v.client.PublicModels(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.ID)
	}
	return ids, nil
}

func selectedProxyBinary(current string) (string, string, error) {
	var target string
	if goruntime.GOOS == "windows" {
		body, err := os.ReadFile(current)
		if err != nil {
			return "", "", pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Managed proxy current pointer is unreadable.")
		}
		target = strings.TrimSpace(string(body))
	} else {
		value, err := os.Readlink(current)
		if err != nil {
			return "", "", pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Managed proxy current pointer is unreadable.")
		}
		target = value
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(current), target)
		}
	}
	target, err := filepath.Abs(target)
	if err != nil {
		return "", "", pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Managed proxy current pointer is invalid.")
	}
	executable := "cli-proxy-api"
	if goruntime.GOOS == "windows" {
		executable += ".exe"
	}
	binary := filepath.Join(target, executable)
	if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = errors.New("selected proxy binary is not a regular file")
		}
		return "", "", pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Environment, "Managed proxy current pointer does not select a valid executable.")
	}
	return binary, filepath.Base(target), nil
}

// modelCache is a private, non-secret cache used only for explicitly requested
// refreshes and cache-only list operations.
type modelCache struct {
	path     string
	platform domainplatform.Platform
}

func (c *modelCache) Load(_ context.Context, _ string) (adaptermodels.Snapshot, error) {
	var snapshot adaptermodels.Snapshot
	body, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, adaptermodels.ErrCacheMiss
	}
	if err != nil {
		return snapshot, err
	}
	if err := json.Unmarshal(body, &snapshot); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}
func (c *modelCache) Store(_ context.Context, _ string, snapshot adaptermodels.Snapshot) error {
	body, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	return writeSecret(c.path, append(body, '\n'), c.platform)
}

type favorites struct{ store *state.Store }

func (f favorites) FavoriteIDs(context.Context) ([]string, error) {
	value, err := f.store.LoadState()
	return append([]string(nil), value.Favorites...), err
}

// doctorSource keeps ordinary doctor local/loopback-only and never contacts a
// release service unless the caller explicitly selected --online.
type doctorSource struct {
	runtime      *nativeRuntime
	installation state.Installation
	online       bool
}

func (s *doctorSource) Binary(context.Context) (adapterdoctor.BinaryFact, error) {
	if s.installation.Kind == "container" {
		return adapterdoctor.BinaryFact{Version: s.installation.CoreVersionSeen, NotApplicable: containerEvidenceSummary(s.installation)}, nil
	}
	info, statErr := os.Stat(s.installation.BinaryPath)
	fact := adapterdoctor.BinaryFact{
		Path: s.installation.BinaryPath, Exists: statErr == nil,
		Executable:     statErr == nil && (goruntime.GOOS == "windows" || info.Mode()&0o111 != 0),
		ArchitectureOK: statErr == nil, Managed: s.installation.Kind == "managed",
		ChecksumOK: s.installation.Kind != "managed", Version: s.installation.CoreVersionSeen,
	}
	if statErr != nil || s.installation.Kind != "managed" {
		return fact, nil
	}
	actual, err := fileSHA256(s.installation.BinaryPath)
	if err != nil {
		return fact, err
	}
	fact.ChecksumOK = s.installation.BinarySHA256 != "" && strings.EqualFold(actual, strings.TrimPrefix(s.installation.BinarySHA256, "sha256:"))
	return fact, nil
}
func (s *doctorSource) AbsoluteConfig(ctx context.Context) (adapterdoctor.AbsoluteConfigFact, error) {
	fact := adapterdoctor.AbsoluteConfigFact{ConfigPath: s.installation.ConfigPath}
	if _, err := os.ReadFile(s.installation.ConfigPath); err != nil {
		fact.ParseDetail = err.Error()
	} else {
		fact.ConfigReadable = true
		adapter := configfile.New(filepath.Join(s.runtime.roots.State, "backups", s.installation.ID))
		snapshot, err := adapter.Read(ctx, s.installation.ConfigPath)
		if err != nil {
			fact.ParseDetail = err.Error()
		} else {
			fact.ConfigParsed = true
			fact.WSAuthEnabled = snapshot.Config.WSAuth
		}
	}
	if s.installation.Kind == "container" {
		fact.ProcessContractNotApplicable = containerEvidenceSummary(s.installation)
		return fact, nil
	}
	body, definitionErr := s.runtime.effectiveServiceDefinition(s.installation)
	if definitionErr != nil {
		return fact, nil
	}
	usesConfig := (bytes.Contains(body, []byte("--config")) || bytes.Contains(body, []byte("-config"))) &&
		bytes.Contains(body, []byte(s.installation.ConfigPath))
	usesRuntime := bytes.Contains(body, []byte(s.installation.RuntimeDir))
	if usesRuntime {
		fact.RuntimeDir = s.installation.RuntimeDir
	}
	_, dotEnvErr := os.Stat(filepath.Join(s.installation.RuntimeDir, ".env"))
	fact.RuntimeContainsDotEnv = dotEnvErr == nil
	for _, line := range strings.FieldsFunc(string(body), func(r rune) bool {
		return r == '\n' || r == '\r' || r == '"' || r == ' ' || r == '='
	}) {
		name := strings.TrimSpace(line)
		if strings.HasPrefix(name, "PGSTORE_") || strings.HasPrefix(name, "OBJECTSTORE_") || strings.HasPrefix(name, "GITSTORE_") {
			fact.StoreOverrides = append(fact.StoreOverrides, name)
		}
	}
	slices.Sort(fact.StoreOverrides)
	fact.StoreOverrides = slices.Compact(fact.StoreOverrides)
	fact.ArgvUsesAbsolutePath = filepath.IsAbs(s.installation.ConfigPath) && usesConfig
	return fact, nil
}
func (s *doctorSource) SafeMode(ctx context.Context) (adapterdoctor.SafeModeFact, error) {
	adapter := configfile.New(filepath.Join(s.runtime.roots.State, "backups", s.installation.ID))
	snapshot, err := adapter.Read(ctx, s.installation.ConfigPath)
	if err != nil {
		return adapterdoctor.SafeModeFact{}, err
	}
	if s.installation.Kind == "container" {
		placeholder := len(snapshot.Config.APIKeys) == 0
		for _, key := range snapshot.Config.APIKeys {
			placeholder = placeholder || configfile.IsTemplateAPIKey(key)
		}
		fact := adapterdoctor.SafeModeFact{PlaceholderConfigured: placeholder}
		if placeholder || len(snapshot.Config.APIKeys) == 0 {
			return fact, nil
		}
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, endpoint(s.installation)+"/v1/models", nil)
		if requestErr != nil {
			return fact, requestErr
		}
		request.Header.Set("Authorization", "Bearer "+snapshot.Config.APIKeys[0])
		response, requestErr := httpClient(s.runtime.http).Do(request)
		if requestErr != nil {
			return fact, requestErr
		}
		defer func() { _ = response.Body.Close() }()
		fact.HTTPStatus = response.StatusCode
		fact.Header = response.Header.Get("X-Cpa-Safe-Mode")
		fact.Authenticated = response.StatusCode >= 200 && response.StatusCode < 300 && fact.Header == ""
		return fact, nil
	}
	placeholder := len(snapshot.Config.APIKeys) == 0
	for _, key := range snapshot.Config.APIKeys {
		placeholder = placeholder || configfile.IsTemplateAPIKey(key)
	}
	return adapterdoctor.SafeModeFact{PlaceholderConfigured: placeholder, Authenticated: !placeholder}, nil
}
func (s *doctorSource) Permissions(context.Context) (adapterdoctor.PermissionsFact, error) {
	if s.installation.Kind == "container" {
		evidence := containerEvidenceSummary(s.installation)
		return adapterdoctor.PermissionsFact{SecretNotApplicable: evidence, AuthNotApplicable: evidence}, nil
	}
	targets := []adapterdoctor.PermissionTarget{
		{Path: s.installation.ConfigPath},
		{Path: s.installation.ProxyKeyRef.Path},
		{Path: s.installation.AuthDir, Auth: true},
	}
	if entries, err := os.ReadDir(s.installation.AuthDir); err == nil {
		for _, entry := range entries {
			targets = append(targets, adapterdoctor.PermissionTarget{Path: filepath.Join(s.installation.AuthDir, entry.Name()), Auth: true, RedactName: true})
		}
	}
	fact := adapterdoctor.PermissionsFact{}
	for _, target := range targets {
		if target.Path == "" {
			continue
		}
		info, err := os.Stat(target.Path)
		target.Secure = err == nil && s.runtime.platform.VerifySecurePermissions(target.Path, info.IsDir()) == nil
		if err != nil {
			target.Detail = err.Error()
		}
		fact.Targets = append(fact.Targets, target)
	}
	return fact, nil
}

func (s *doctorSource) Service(ctx context.Context) (adapterdoctor.ServiceFact, error) {
	if s.installation.Kind == "container" {
		manager, err := s.runtime.service(ctx, s.installation, false)
		if err != nil {
			return adapterdoctor.ServiceFact{}, err
		}
		status, err := manager.Status(ctx)
		if err != nil {
			return adapterdoctor.ServiceFact{}, err
		}
		return adapterdoctor.ServiceFact{
			Backend: string(status.Backend), Installed: true,
			Running: status.State == service.ServiceRunning, CrashLoop: status.State == service.ServiceFailed,
			ExternallyManaged: true, Detail: containerEvidenceSummary(s.installation) + "; " + status.Detail,
		}, nil
	}
	manager, err := s.runtime.service(ctx, s.installation, false)
	if err != nil {
		return adapterdoctor.ServiceFact{}, err
	}
	status, err := manager.Status(ctx)
	if err != nil {
		return adapterdoctor.ServiceFact{}, err
	}
	body, _ := s.runtime.effectiveServiceDefinition(s.installation)
	hasConfig := (bytes.Contains(body, []byte("--config")) || bytes.Contains(body, []byte("-config"))) && bytes.Contains(body, []byte(s.installation.ConfigPath))
	hasForbiddenEnvironment := bytes.Contains(body, []byte("PGSTORE_")) || bytes.Contains(body, []byte("OBJECTSTORE_")) || bytes.Contains(body, []byte("GITSTORE_"))
	_, dotEnvErr := os.Stat(filepath.Join(s.installation.RuntimeDir, ".env"))
	return adapterdoctor.ServiceFact{
		Backend: string(status.Backend), Installed: status.State != service.ServiceNotInstalled,
		Required: s.installation.ServiceBackend != string(service.BackendForeground),
		Running:  status.State == service.ServiceRunning, CrashLoop: status.State == service.ServiceFailed,
		DefinitionOwned: len(body) > 0, IdentityMatches: s.installation.ID != "",
		DefinitionUsesConfig: hasConfig, EnvironmentScrubbed: !hasForbiddenEnvironment,
		RuntimeDirClean: dotEnvErr != nil, Detail: status.Detail,
	}, nil
}

type lazyManagement struct {
	runtime      *nativeRuntime
	installation state.Installation
}

func (m lazyManagement) client(ctx context.Context) (management.ManagementClient, error) {
	return m.runtime.management(ctx, m.installation)
}

func (m lazyManagement) AuthFiles(ctx context.Context) ([]management.AuthFile, error) {
	client, err := m.client(ctx)
	if err != nil {
		return nil, err
	}
	return client.AuthFiles(ctx)
}

func (m lazyManagement) AuthFileModels(ctx context.Context, name string) ([]management.ModelRef, error) {
	client, err := m.client(ctx)
	if err != nil {
		return nil, err
	}
	return client.AuthFileModels(ctx, name)
}

func (m lazyManagement) ModelDefinitions(ctx context.Context, channel string) ([]management.ModelDef, error) {
	client, err := m.client(ctx)
	if err != nil {
		return nil, err
	}
	return client.ModelDefinitions(ctx, channel)
}

func (m lazyManagement) PublicModels(ctx context.Context) ([]management.ModelRef, error) {
	client, err := m.client(ctx)
	if err != nil {
		return nil, err
	}
	return client.PublicModels(ctx)
}
func (s *doctorSource) Health(ctx context.Context) (adapterdoctor.HealthFact, error) {
	before := time.Now()
	result, err := (health.HTTPProbe{BaseURL: endpoint(s.installation), Client: s.runtime.http}).Probe(ctx)
	if err != nil {
		return adapterdoctor.HealthFact{}, err
	}
	return adapterdoctor.HealthFact{HTTPStatus: http.StatusOK, Version: result.Version, Endpoint: endpoint(s.installation) + "/healthz", LatencyMS: time.Since(before).Milliseconds()}, nil
}
func (s *doctorSource) Providers(ctx context.Context) (adapterdoctor.ProviderFact, error) {
	if s.installation.Kind == "container" {
		return adapterdoctor.ProviderFact{NotApplicable: "management credentials are owned by the external container; " + containerEvidenceSummary(s.installation)}, nil
	}
	client, err := s.runtime.management(ctx, s.installation)
	if err != nil {
		return adapterdoctor.ProviderFact{}, err
	}
	files, err := client.AuthFiles(ctx)
	if err != nil {
		return adapterdoctor.ProviderFact{}, err
	}
	fact := adapterdoctor.ProviderFact{Configured: len(files)}
	for _, file := range files {
		if !file.Disabled && (file.Status == "" || file.Status == "ok" || file.Status == "usable") {
			fact.Usable++
		} else {
			fact.Unavailable++
		}
	}
	return fact, nil
}
func (s *doctorSource) Models(ctx context.Context) (adapterdoctor.ModelsFact, error) {
	if s.installation.Kind == "container" {
		return adapterdoctor.ModelsFact{NotApplicable: "management credentials are owned by the external container; " + containerEvidenceSummary(s.installation)}, nil
	}
	catalog, err := s.runtime.models(ctx, s.installation)
	if err != nil {
		return adapterdoctor.ModelsFact{}, err
	}
	entries, err := catalog.List(ctx)
	if errors.Is(err, adaptermodels.ErrCacheMiss) {
		return adapterdoctor.ModelsFact{DiscoverySucceeded: true, Count: 0, Source: "cache", Cause: "no cached live discovery"}, nil
	}
	if err != nil {
		return adapterdoctor.ModelsFact{}, err
	}
	source := "cache"
	if len(entries) > 0 {
		source = entries[0].Source
	}
	return adapterdoctor.ModelsFact{DiscoverySucceeded: true, Count: len(entries), Source: source}, nil
}
func (s *doctorSource) Claude(ctx context.Context) (adapterdoctor.ClaudeFact, error) {
	launcher, _ := s.runtime.launcher(ctx, s.installation, app.Invocation{})
	detected, err := launcher.Detect(ctx)
	if err != nil {
		return adapterdoctor.ClaudeFact{Found: false}, nil
	}
	return adapterdoctor.ClaudeFact{Found: true, VersionKnown: detected.Version != "", Supported: detected.Supported, Version: detected.Version, Path: detected.Path}, nil
}
func (s *doctorSource) Compatibility(context.Context) (adapterdoctor.CompatibilityFact, error) {
	version := s.installation.CoreVersionSeen
	return adapterdoctor.CompatibilityFact{VersionKnown: version != "" && version != "unknown", DetectedVersion: valueOr(version, "unknown"), MinimumVersion: "7.2.91", FloorSatisfied: versionAtLeast(version, "7.2.91")}, nil
}
func (s *doctorSource) UpdateState(context.Context) (adapterdoctor.UpdateStateFact, error) {
	cfg, _ := s.runtime.store.LoadConfig()
	return adapterdoctor.UpdateStateFact{AutomaticChecksEnabled: cfg.UpdateCheck, UnexpectedEgress: false, LastCheckExplicit: false}, nil
}

func (s *doctorSource) Port(ctx context.Context) (adapterdoctor.PortFact, error) {
	host := valueOr(s.installation.Host, "127.0.0.1")
	fact := adapterdoctor.PortFact{Host: host, Port: s.installation.Port}
	connection, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(s.installation.Port)))
	if err != nil {
		return fact, nil
	}
	fact.Listening = true
	_ = connection.Close()
	if _, err := (health.HTTPProbe{BaseURL: endpoint(s.installation), Client: s.runtime.http}).Probe(ctx); err == nil {
		fact.ExpectedOwner = true
		fact.Owner = "CLIProxyAPI health endpoint"
	}
	return fact, nil
}

func (s *doctorSource) ManagementLocal(ctx context.Context) (adapterdoctor.ManagementLocalFact, error) {
	if s.installation.Kind == "container" {
		return adapterdoctor.ManagementLocalFact{NotApplicable: containerEvidenceSummary(s.installation)}, nil
	}
	adapter := configfile.New(filepath.Join(s.runtime.roots.State, "backups", s.installation.ID))
	snapshot, err := adapter.Read(ctx, s.installation.ConfigPath)
	if err != nil {
		return adapterdoctor.ManagementLocalFact{}, err
	}
	fact := adapterdoctor.ManagementLocalFact{Required: true, AllowRemote: !snapshot.Config.ManagementLocal}
	body, _ := os.ReadFile(s.installation.ConfigPath)
	fact.ControlPanelDisabled = bytes.Contains(body, []byte("disable-control-panel: true"))
	client, err := s.runtime.management(ctx, s.installation)
	if err != nil {
		return fact, err
	}
	fact.Enabled = true
	if _, err := client.Config(ctx); err != nil {
		return fact, err
	}
	fact.Authenticated = true
	return fact, nil
}

func (s *doctorSource) Exposure(ctx context.Context) (adapterdoctor.ExposureFact, error) {
	adapter := configfile.New(filepath.Join(s.runtime.roots.State, "backups", s.installation.ID))
	snapshot, err := adapter.Read(ctx, s.installation.ConfigPath)
	if err != nil {
		return adapterdoctor.ExposureFact{}, err
	}
	host := snapshot.Config.Host
	if s.installation.Kind == "container" {
		host = s.installation.Host
	}
	return adapterdoctor.ExposureFact{
		Host: host, Loopback: isLoopbackHost(host), ManagementLocal: snapshot.Config.ManagementLocal,
		RealProxyKey: hasRealProxyKey(snapshot.Config.APIKeys),
	}, nil
}

func (s *doctorSource) StateLock(context.Context) (adapterdoctor.StateLockFact, error) {
	_, err := s.runtime.store.LoadState()
	return adapterdoctor.StateLockFact{ReadOnlyStateAccessible: err == nil, MutationRequested: false, MutationLockAvailable: false}, nil
}

func isLoopbackHost(host string) bool {
	trimmed := strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(trimmed, "localhost") {
		return true
	}
	ip := net.ParseIP(trimmed)
	return ip != nil && ip.IsLoopback()
}

func hasRealProxyKey(keys []string) bool {
	for _, key := range keys {
		if !configfile.IsTemplateAPIKey(key) {
			return true
		}
	}
	return false
}

func containerEvidenceSummary(installation state.Installation) string {
	if installation.Container == nil {
		return "container metadata is unavailable"
	}
	return fmt.Sprintf(
		"external container runtime=%s image=%s id=%s endpoint=%s config_mount=%s",
		installation.Container.Runtime,
		installation.Container.Image,
		installation.Container.ID,
		installation.Container.Endpoint,
		valueOr(installation.Container.ConfigMount, "unknown"),
	)
}
func writeSecret(path string, body []byte, platform domainplatform.Platform) error {
	if !filepath.IsAbs(path) {
		return actionable(pmuxerr.ConfigValidationFailed, "Private file path must be absolute.", "Use canonical platform roots.")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := platform.SecurePermissions(filepath.Dir(path), true); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".pmux-")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if err := platform.SecurePermissions(name, false); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(body); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return platform.VerifySecurePermissions(path, false)
}
func randomSecret() (string, error) {
	body := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, body); err != nil {
		return "", pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Internal, "PMux could not generate a management secret.")
	}
	return hex.EncodeToString(body), nil
}
func secretReference(path, secret string) state.SecretReference {
	return state.SecretReference{Path: path, Masked: mask(secret), Fingerprint: fingerprintOf(secret)}
}
func fingerprintOf(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}
func mask(value string) string {
	if len(value) < 12 {
		return "********"
	}
	return value[:7] + "…" + value[len(value)-4:]
}
func yamlQuote(value string) string { body, _ := json.Marshal(value); return string(body) }
func (n *nativeRuntime) effectiveServiceDefinition(installation state.Installation) ([]byte, error) {
	path, body, exists := n.serviceArtifact(installation)
	if exists {
		return body, nil
	}
	if path == "" {
		return nil, actionable(pmuxerr.ServiceBackendUnavailable, "The selected service backend has no inspectable effective definition.", "Install or repair the PMux service before rerunning doctor.")
	}
	return nil, pmuxerr.Wrap(os.ErrNotExist, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "PMux could not inspect the effective service definition at "+path+".")
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readExisting(path string) ([]byte, bool) {
	body, err := os.ReadFile(path)
	return body, err == nil
}

func restoreExisting(path string, body []byte, existed bool) {
	if existed {
		_ = adapterfs.AtomicWritePrivate(path, body)
		return
	}
	_ = os.Remove(path)
}

func (n *nativeRuntime) serviceArtifact(installation state.Installation) (string, []byte, bool) {
	var path string
	switch service.ServiceBackend(installation.ServiceBackend) {
	case service.BackendSystemdUser:
		path = filepath.Join(userHome(), ".config", "systemd", "user", service.Identity(service.BackendSystemdUser, installation.ID))
	case service.BackendLaunchd:
		path = filepath.Join(userHome(), "Library", "LaunchAgents", service.Identity(service.BackendLaunchd, installation.ID)+".plist")
	case service.BackendForeground:
		path = filepath.Join(n.roots.State, "foreground-"+installation.ID+".json")
	}
	if path == "" {
		return "", nil, false
	}
	body, exists := readExisting(path)
	return path, body, exists
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Environment, "PMux could not hash the installed CLIProxyAPI executable.")
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", pmuxerr.Wrap(err, pmuxerr.InstallIntegrityFailed, pmuxerr.Environment, "PMux could not hash the installed CLIProxyAPI executable.")
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func httpClient(client *http.Client) *http.Client {
	if client != nil {
		return client
	}
	return http.DefaultClient
}

func defaultBackend(ctx context.Context) string {
	return string(adapterplatform.DefaultServiceBackend(ctx))
}
func cloneSecretReferences(value state.SecretReferences) state.SecretReferences {
	cloned := value
	cloned.Management = make(map[string]state.SecretReference, len(value.Management))
	for id, reference := range value.Management {
		cloned.Management[id] = reference
	}
	return cloned
}

func endpoint(installation state.Installation) string {
	host := installation.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + host + ":" + strconv.Itoa(installation.Port)
}
func (n *nativeRuntime) restoreManagedServiceCheckpoint(ctx context.Context, checkpoint installer.ServiceCheckpoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := n.store.LoadState()
	if err != nil {
		return err
	}
	for _, installation := range current.Installations {
		if installation.Kind != "managed" {
			continue
		}
		backend := service.ServiceBackend(installation.ServiceBackend)
		if service.Identity(backend, installation.ID) != checkpoint.Identity {
			continue
		}
		manager, managerErr := n.service(ctx, installation, false)
		if managerErr != nil {
			return managerErr
		}
		_ = manager.Stop(ctx, 15*time.Second)
		if err := manager.Uninstall(ctx); err != nil {
			return err
		}
		return nil
	}
	// No matching installation remains; the interrupted service definition is gone.
	return nil
}

func appServiceSpec(roots domainplatform.Roots, installation state.Installation, backend service.ServiceBackend) service.ServiceSpec {
	executable, _ := os.Executable()
	return service.ServiceSpec{InstanceID: installation.ID, Identity: service.Identity(backend, installation.ID), PMuxPath: executable, BinaryPath: installation.BinaryPath, ConfigPath: installation.ConfigPath, RuntimeDir: installation.RuntimeDir, LogDir: filepath.Join(roots.State, "logs"), Environment: foreground.AllowlistedEnvironment(os.Environ())}
}
func safeInstallation(installation state.Installation) map[string]any {
	return map[string]any{"id": installation.ID, "kind": installation.Kind, "binary_path": installation.BinaryPath, "config_path": installation.ConfigPath, "auth_dir": installation.AuthDir, "runtime_dir": installation.RuntimeDir, "host": installation.Host, "port": installation.Port, "service_backend": installation.ServiceBackend, "core_version": installation.CoreVersionSeen, "proxy_key": installation.ProxyKeyRef.Masked}
}
func replaceInstallation(values []state.Installation, replacement state.Installation) []state.Installation {
	for index := range values {
		if values[index].ID == replacement.ID {
			values[index] = replacement
			return values
		}
	}
	return append(values, replacement)
}
func onboardingActions() []string {
	return []string{"pmux providers login <provider>", "pmux models list --refresh", "pmux launch --client claude --model <id>"}
}
func installationIDForPath(dataRoot, path string) string {
	relative, err := filepath.Rel(filepath.Join(dataRoot, "instances"), path)
	if err == nil {
		parts := strings.Split(relative, string(filepath.Separator))
		if len(parts) > 1 && parts[0] != ".." {
			return parts[0]
		}
	}
	return "default"
}
func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
func userHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}
func valueReader(value, fallback io.Reader) io.Reader {
	if value != nil {
		return value
	}
	return fallback
}
func valueWriter(value, fallback io.Writer) io.Writer {
	if value != nil {
		return value
	}
	return fallback
}
func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
func clear(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
func versionAtLeast(value, floor string) bool {
	if value == "" || value == "unknown" {
		return false
	}
	parse := func(raw string) []int {
		parts := strings.Split(strings.TrimPrefix(raw, "v"), ".")
		out := make([]int, 3)
		for index := 0; index < len(out) && index < len(parts); index++ {
			out[index], _ = strconv.Atoi(parts[index])
		}
		return out
	}
	left, right := parse(value), parse(floor)
	return slices.Compare(left, right) >= 0
}
func actionable(code, message, repair string) *pmuxerr.Error {
	err := pmuxerr.New(code, pmuxerr.Environment, message)
	err.Repair = []string{repair}
	return err
}
func containerMutationError() *pmuxerr.Error {
	return actionable(pmuxerr.ServiceForeignOwner, "This CLIProxyAPI runs in Docker; its lifecycle and configuration are owned by the container runtime.", "Manage it with Docker; PMux mutation actions are disabled.")
}
func (n *nativeRuntime) isContainerConfigPath(path string) bool {
	current, err := n.store.LoadState()
	if err != nil {
		return false
	}
	target := filepath.Clean(path)
	for _, installation := range current.Installations {
		if installation.Kind == "container" && filepath.Clean(installation.ConfigPath) == target {
			return true
		}
	}
	return false
}

func usage(message string) *pmuxerr.Error {
	return pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, message)
}
func ensureTyped(err error, message string) error {
	var typed *pmuxerr.Error
	if errors.As(err, &typed) {
		return typed
	}
	return pmuxerr.Wrap(err, pmuxerr.UnhandledInternal, pmuxerr.Internal, message)
}

var _ app.SetupService = (*nativeRuntime)(nil)
var _ app.ConfigMaintenance = (*nativeRuntime)(nil)
var _ app.DoctorService = (*nativeRuntime)(nil)
var _ app.BundleService = (*nativeRuntime)(nil)
var _ app.UpdateService = (*nativeRuntime)(nil)
