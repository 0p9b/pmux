package claude

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	domainclient "github.com/0p9b/pmux/internal/domain/client"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/redact"
)

const (
	baseURLEnv   = "ANTHROPIC_BASE_URL"
	authTokenEnv = "ANTHROPIC_AUTH_TOKEN"
	opusEnv      = "ANTHROPIC_DEFAULT_OPUS_MODEL"
	sonnetEnv    = "ANTHROPIC_DEFAULT_SONNET_MODEL"
	haikuEnv     = "ANTHROPIC_DEFAULT_HAIKU_MODEL"
)

var versionPattern = regexp.MustCompile(`(?m)(?:^|\s)v?(\d+)\.(\d+)\.(\d+)(?:[-+][0-9A-Za-z.-]+)?(?:\s|$)`)

// ModelPreflight verifies that an exact, case-sensitive model ID is currently
// available. The adapter never substitutes or normalizes the supplied ID.
type ModelPreflight func(context.Context, string) error

// Process is the complete, shell-free Claude process invocation.
type Process struct {
	Path   string
	Args   []string
	Env    []string
	Dir    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Runner is injectable so detection and launch can be tested without spawning
// a real Claude process.
type Runner interface {
	Output(context.Context, string, ...string) ([]byte, error)
	Run(context.Context, Process) error
}

type execRunner struct{}

func (execRunner) Output(ctx context.Context, path string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, path, args...).CombinedOutput()
}

func (execRunner) Run(ctx context.Context, process Process) error {
	cmd := exec.CommandContext(ctx, process.Path, process.Args...)
	cmd.Env = process.Env
	cmd.Dir = process.Dir
	cmd.Stdin = process.Stdin
	cmd.Stdout = process.Stdout
	cmd.Stderr = process.Stderr
	return cmd.Run()
}

// Options provides process and persistence boundaries. Zero values select the
// real process environment, standard streams, and ~/.claude/settings.json.
type Options struct {
	Executable           string
	LookPath             func(string) (string, error)
	Runner               Runner
	Environment          func() []string
	WorkingDirectory     func() (string, error)
	ModelPreflight       ModelPreflight
	Stdin                io.Reader
	Stdout               io.Writer
	Stderr               io.Writer
	JSONMode             bool
	SettingsPath         string
	PersistenceStatePath string
	Now                  func() time.Time
}

type Launcher struct {
	executable       string
	lookPath         func(string) (string, error)
	runner           Runner
	environment      func() []string
	workingDirectory func() (string, error)
	preflight        ModelPreflight
	stdin            io.Reader
	stdout           io.Writer
	stderr           io.Writer
	jsonMode         bool
	settingsPath     string
	statePath        string
	now              func() time.Time
}

func New(options Options) *Launcher {
	lookPath := options.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	runner := options.Runner
	if runner == nil {
		runner = execRunner{}
	}
	environment := options.Environment
	if environment == nil {
		environment = os.Environ
	}
	workingDirectory := options.WorkingDirectory
	if workingDirectory == nil {
		workingDirectory = os.Getwd
	}
	stdin := options.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	stdout := options.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := options.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	settingsPath := options.SettingsPath
	if settingsPath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			settingsPath = filepath.Join(home, ".claude", "settings.json")
		}
	}
	statePath := options.PersistenceStatePath
	if statePath == "" && settingsPath != "" {
		statePath = settingsPath + ".pmux-persistence.json"
	}
	return &Launcher{
		executable: options.Executable, lookPath: lookPath, runner: runner,
		environment: environment, workingDirectory: workingDirectory,
		preflight: options.ModelPreflight, stdin: stdin, stdout: stdout,
		stderr: stderr, jsonMode: options.JSONMode, settingsPath: settingsPath,
		statePath: statePath, now: now,
	}
}

var _ domainclient.ClientLauncher = (*Launcher)(nil)

func (l *Launcher) Client() domainclient.ClientID { return domainclient.Claude }

func (l *Launcher) resolveExecutable() (string, error) {
	name := l.executable
	if name == "" {
		name = "claude"
	}
	path, err := l.lookPath(name)
	if err != nil {
		return "", &pmuxerr.Error{
			Code: pmuxerr.ClientBinaryMissing, Class: pmuxerr.Environment,
			Message: "Claude Code was not found on PATH. Install Claude Code v2.0.0 or newer, then run \"pmux doctor\".",
			Cause:   err,
		}
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", pmuxerr.Wrap(err, pmuxerr.ClientBinaryMissing, pmuxerr.Environment, "Could not resolve the Claude Code executable path.")
	}
	return path, nil
}

func (l *Launcher) Detect(ctx context.Context) (domainclient.ClientInstall, error) {
	path, err := l.resolveExecutable()
	if err != nil {
		return domainclient.ClientInstall{}, err
	}
	output, err := l.runner.Output(ctx, path, "--version")
	if err != nil {
		return domainclient.ClientInstall{}, pmuxerr.Wrap(err, pmuxerr.ClientBinaryMissing, pmuxerr.Environment, "Could not determine the Claude Code version; PMux requires a verified version of v2.0.0 or newer.")
	}
	version, major, ok := parseVersion(string(output))
	if !ok {
		return domainclient.ClientInstall{Path: path, Version: "unknown", Supported: false}, &pmuxerr.Error{
			Code: pmuxerr.ClientBinaryMissing, Class: pmuxerr.Environment,
			Message: "Could not determine the Claude Code version; PMux requires a verified version of v2.0.0 or newer.",
		}
	}
	return domainclient.ClientInstall{Path: path, Version: version, Supported: major >= 2}, nil
}

func parseVersion(output string) (version string, major int, ok bool) {
	match := versionPattern.FindStringSubmatch(output)
	if match == nil {
		return "", 0, false
	}
	major, err := strconv.Atoi(match[1])
	if err != nil {
		return "", 0, false
	}
	return match[1] + "." + match[2] + "." + match[3], major, true
}

var conflictingClaudeEnv = map[string]struct{}{
	baseURLEnv: {}, authTokenEnv: {}, "ANTHROPIC_API_KEY": {},
	"ANTHROPIC_MODEL": {}, "ANTHROPIC_SMALL_FAST_MODEL": {},
	opusEnv: {}, sonnetEnv: {}, haikuEnv: {},
}

func (l *Launcher) Env(spec domainclient.LaunchSpec) ([]string, error) {
	if spec.BaseURL == "" {
		return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "Claude launch requires a proxy base URL.")
	}
	if spec.Token == "" {
		return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "Claude launch requires a proxy authentication token.")
	}
	parent := l.environment()
	env := make([]string, 0, len(parent)+2)
	for _, value := range parent {
		name := value
		if index := strings.IndexByte(value, '='); index >= 0 {
			name = value[:index]
		}
		if _, remove := conflictingClaudeEnv[name]; !remove {
			env = append(env, value)
		}
	}
	env = append(env, baseURLEnv+"="+spec.BaseURL, authTokenEnv+"="+spec.Token)
	return env, nil
}

func (l *Launcher) Launch(ctx context.Context, spec domainclient.LaunchSpec) (domainclient.LaunchResult, error) {
	if spec.Client != "" && spec.Client != domainclient.Claude {
		return domainclient.LaunchResult{}, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Claude launcher received a different client ID.")
	}
	if spec.Model == "" {
		return domainclient.LaunchResult{}, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Claude launch requires an exact model ID.")
	}
	for _, arg := range spec.Args {
		if arg == "--model" || strings.HasPrefix(arg, "--model=") {
			return domainclient.LaunchResult{}, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Client arguments must not contain '--model'; use 'pmux launch --model <id>'.")
		}
	}
	install, err := l.Detect(ctx)
	if err != nil {
		return domainclient.LaunchResult{}, err
	}
	if !install.Supported {
		return domainclient.LaunchResult{}, &pmuxerr.Error{
			Code: pmuxerr.ClientBinaryMissing, Class: pmuxerr.Environment,
			Message: fmt.Sprintf("Claude Code %s is unsupported; PMux requires Claude Code v2.0.0 or newer.", install.Version),
		}
	}
	if l.preflight == nil {
		return domainclient.LaunchResult{}, pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "Claude model preflight is not configured.")
	}
	if err := l.preflight(ctx, spec.Model); err != nil {
		return domainclient.LaunchResult{}, err
	}
	env, err := l.Env(spec)
	if err != nil {
		return domainclient.LaunchResult{}, err
	}
	dir := spec.WorkingDir
	if dir == "" {
		dir, err = l.workingDirectory()
		if err != nil {
			return domainclient.LaunchResult{}, pmuxerr.Wrap(err, pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "Could not determine the caller working directory.")
		}
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return domainclient.LaunchResult{}, pmuxerr.Wrap(err, pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "Could not resolve the caller working directory.")
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = errors.New("not a directory")
		}
		return domainclient.LaunchResult{}, pmuxerr.Wrap(err, pmuxerr.ConfigPathMismatch, pmuxerr.Environment, "Working directory is not accessible: "+dir+".")
	}
	stdout := l.stdout
	if l.jsonMode {
		stdout = l.stderr
	}
	args := make([]string, 0, 2+len(spec.Args))
	args = append(args, "--model", spec.Model)
	args = append(args, spec.Args...)
	err = l.runner.Run(ctx, Process{
		Path: install.Path, Args: args, Env: env, Dir: dir,
		Stdin: l.stdin, Stdout: stdout, Stderr: l.stderr,
	})
	if err == nil {
		return domainclient.LaunchResult{}, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 1 && code <= 125 {
			return domainclient.LaunchResult{ExitCode: code}, nil
		}
		if result, ok := signalLaunchResult(exitErr.ProcessState); ok {
			return result, nil
		}
		return domainclient.LaunchResult{}, pmuxerr.Wrap(err, pmuxerr.ClientLaunchFailed, pmuxerr.Upstream, "Claude Code returned an unsupported exit status after launch.")
	}
	return domainclient.LaunchResult{}, pmuxerr.Wrap(err, pmuxerr.ClientLaunchFailed, pmuxerr.Environment, "Claude Code could not be started after successful preflight.")
}

type SlotAction = domainclient.SlotAction

const (
	SlotUnchanged = domainclient.SlotUnchanged
	SlotSet       = domainclient.SlotSet
	SlotUnmanaged = domainclient.SlotUnmanaged
)

type SlotUpdate = domainclient.SlotUpdate
type PersistentSlots = domainclient.PersistentSlots
type PersistentSpec = domainclient.PersistSpec

// PlanPersist plans an explicit update to independently managed Claude model
// slots. It never infers a slot from the normal launch model.
func (l *Launcher) PlanPersist(ctx context.Context, spec domainclient.PersistSpec) (domainclient.PersistPlan, error) {
	return l.planPersistent(ctx, spec)
}

// PlanPersistent is retained as the concrete adapter convenience used by
// callers that need the Claude-specific name.
func (l *Launcher) PlanPersistent(ctx context.Context, spec PersistentSpec) (domainclient.PersistPlan, error) {
	return l.planPersistent(ctx, spec)
}

func (l *Launcher) planPersistent(ctx context.Context, spec domainclient.PersistSpec) (domainclient.PersistPlan, error) {
	if l.settingsPath == "" {
		return domainclient.PersistPlan{}, pmuxerr.New(pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Claude settings path is unavailable.")
	}
	if spec.BaseURL == "" || spec.Token == "" {
		return domainclient.PersistPlan{}, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "Persistent Claude settings require a proxy base URL and authentication token.")
	}
	before, err := readOptional(l.settingsPath)
	if err != nil {
		return domainclient.PersistPlan{}, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Could not read Claude settings.")
	}
	root := make(map[string]json.RawMessage)
	if len(bytes.TrimSpace(before)) != 0 {
		if err := json.Unmarshal(before, &root); err != nil {
			return domainclient.PersistPlan{}, pmuxerr.Wrap(err, pmuxerr.ConfigValidationFailed, pmuxerr.Environment, "Claude settings are not valid JSON; no changes were planned.")
		}
	}
	if root == nil {
		root = make(map[string]json.RawMessage)
	}
	env := make(map[string]string)
	if raw, found := root["env"]; found && len(bytes.TrimSpace(raw)) != 0 && string(bytes.TrimSpace(raw)) != "null" {
		if err := json.Unmarshal(raw, &env); err != nil {
			return domainclient.PersistPlan{}, pmuxerr.Wrap(err, pmuxerr.ConfigValidationFailed, pmuxerr.Environment, "Claude settings env is not a string map; no changes were planned.")
		}
	}
	env[baseURLEnv] = spec.BaseURL
	env[authTokenEnv] = spec.Token
	updates := []struct {
		name string
		item SlotUpdate
	}{{opusEnv, spec.Slots.Opus}, {sonnetEnv, spec.Slots.Sonnet}, {haikuEnv, spec.Slots.Haiku}}
	for _, update := range updates {
		switch update.item.Action {
		case "", SlotUnchanged:
		case SlotUnmanaged:
			delete(env, update.name)
		case SlotSet:
			if update.item.Model == "" {
				return domainclient.PersistPlan{}, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Persistent Claude model slots require an exact non-empty model ID.")
			}
			if l.preflight == nil {
				return domainclient.PersistPlan{}, pmuxerr.New(pmuxerr.UnhandledInternal, pmuxerr.Internal, "Claude model preflight is not configured.")
			}
			if err := l.preflight(ctx, update.item.Model); err != nil {
				return domainclient.PersistPlan{}, err
			}
			env[update.name] = update.item.Model
		default:
			return domainclient.PersistPlan{}, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Unknown persistent Claude slot action.")
		}
	}
	rawEnv, err := json.Marshal(env)
	if err != nil {
		return domainclient.PersistPlan{}, pmuxerr.Wrap(err, pmuxerr.UnhandledInternal, pmuxerr.Internal, "Could not encode persistent Claude environment.")
	}
	root["env"] = rawEnv
	after, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return domainclient.PersistPlan{}, pmuxerr.Wrap(err, pmuxerr.UnhandledInternal, pmuxerr.Internal, "Could not encode Claude settings.")
	}
	after = append(after, '\n')
	return domainclient.PersistPlan{
		Path: l.settingsPath, Before: before, After: after,
		Diff: redactedDiff(before, after, spec.Token),
	}, nil
}

type persistenceRecord struct {
	Version         int    `json:"version"`
	SettingsPath    string `json:"settings_path"`
	BackupPath      string `json:"backup_path"`
	OriginalExisted bool   `json:"original_existed"`
	BeforeSHA256    string `json:"before_sha256"`
	AfterSHA256     string `json:"after_sha256"`
}

// Upsert commits either the first persistence transaction or a later
// independently planned slot update. The first backup remains the uninstall
// source of truth; every later update is fingerprint-checked against the
// previously committed settings.
func (l *Launcher) Upsert(ctx context.Context, plan domainclient.PersistPlan) error {
	if err := ctx.Err(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.CodeCanceled, pmuxerr.User, "Persistent Claude settings were canceled before commit.")
	}
	if plan.Path != l.settingsPath || plan.Path == "" {
		return pmuxerr.New(pmuxerr.ClientSettingsConflict, pmuxerr.User, "Persistent Claude settings plan targets an unexpected path.")
	}
	current, existed, err := readOptionalWithExistence(plan.Path)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Could not re-read Claude settings before persistence.")
	}
	if !bytes.Equal(current, plan.Before) {
		return pmuxerr.New(pmuxerr.ClientSettingsConflict, pmuxerr.Environment, "Claude settings changed since PMux planned the persistent update; no changes were written.")
	}
	if err := os.MkdirAll(filepath.Dir(plan.Path), 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Could not create the private Claude settings directory.")
	}
	if err := os.Chmod(filepath.Dir(plan.Path), 0o700); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, "Could not secure the Claude settings directory.")
	}

	var record persistenceRecord
	recordBytes, stateErr := os.ReadFile(l.statePath)
	newRecord := errors.Is(stateErr, os.ErrNotExist)
	if stateErr != nil && !newRecord {
		return pmuxerr.Wrap(stateErr, pmuxerr.ClientSettingsConflict, pmuxerr.Environment, "Could not verify persistent Claude settings ownership.")
	}
	if newRecord {
		beforeHash := hashBytes(current)
		stamp := l.now().UTC().Format("20060102T150405.000000000Z")
		backupPath := fmt.Sprintf("%s.pmux.%s.%s.bak", plan.Path, stamp, beforeHash[:8])
		if err := writeExclusive(backupPath, current, 0o600); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.ClientSettingsConflict, pmuxerr.Environment, "Could not create the private Claude settings backup; no settings were changed.")
		}
		record = persistenceRecord{Version: 1, SettingsPath: plan.Path, BackupPath: backupPath, OriginalExisted: existed, BeforeSHA256: beforeHash}
	} else {
		if err := json.Unmarshal(recordBytes, &record); err != nil || record.Version != 1 || record.SettingsPath != l.settingsPath {
			if err == nil {
				err = errors.New("invalid persistence record")
			}
			return pmuxerr.Wrap(err, pmuxerr.ClientSettingsConflict, pmuxerr.Environment, "Persistent Claude settings ownership record is invalid; no settings were changed.")
		}
		if record.AfterSHA256 != hashBytes(current) {
			return pmuxerr.New(pmuxerr.ClientSettingsConflict, pmuxerr.Environment, "Claude settings changed since PMux last persisted them; no settings were changed.")
		}
	}
	if err := ctx.Err(); err != nil {
		if newRecord {
			_ = os.Remove(record.BackupPath)
		}
		return pmuxerr.Wrap(err, pmuxerr.CodeCanceled, pmuxerr.User, "Persistent Claude settings were canceled before commit.")
	}
	if plan.After == nil {
		err = os.Remove(plan.Path)
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	} else {
		err = writeAtomic(plan.Path, plan.After, 0o600)
	}
	if err != nil {
		if newRecord {
			_ = os.Remove(record.BackupPath)
		}
		return pmuxerr.Wrap(err, pmuxerr.ClientSettingsConflict, pmuxerr.Environment, "Could not write persistent Claude settings.")
	}
	record.AfterSHA256 = hashBytes(plan.After)
	recordBytes, err = json.MarshalIndent(record, "", "  ")
	if err == nil {
		recordBytes = append(recordBytes, '\n')
		err = os.MkdirAll(filepath.Dir(l.statePath), 0o700)
	}
	if err == nil {
		err = writeAtomic(l.statePath, recordBytes, 0o600)
	}
	if err != nil {
		if existed {
			_ = writeAtomic(plan.Path, current, 0o600)
		} else {
			_ = os.Remove(plan.Path)
		}
		if newRecord {
			_ = os.Remove(record.BackupPath)
		}
		return pmuxerr.Wrap(err, pmuxerr.ClientSettingsConflict, pmuxerr.Environment, "Could not record persistent Claude settings ownership; the settings change was restored.")
	}
	return nil
}

// Persist remains a compatibility spelling for concrete adapter callers.
func (l *Launcher) Persist(ctx context.Context, plan domainclient.PersistPlan) error {
	return l.Upsert(ctx, plan)
}

func (l *Launcher) Unpersist(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.CodeCanceled, pmuxerr.User, "Removing persistent Claude settings was canceled before commit.")
	}
	recordBytes, err := os.ReadFile(l.statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Could not read persistent Claude settings ownership.")
	}
	var record persistenceRecord
	if err := json.Unmarshal(recordBytes, &record); err != nil || record.Version != 1 || record.SettingsPath != l.settingsPath {
		if err == nil {
			err = errors.New("invalid persistence record")
		}
		return pmuxerr.Wrap(err, pmuxerr.ClientSettingsConflict, pmuxerr.Environment, "Persistent Claude settings ownership record is invalid; no settings were changed.")
	}
	current, err := os.ReadFile(record.SettingsPath)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ClientSettingsConflict, pmuxerr.Environment, "Claude settings changed or disappeared; PMux will not overwrite them.")
	}
	if hashBytes(current) != record.AfterSHA256 {
		return pmuxerr.New(pmuxerr.ClientSettingsConflict, pmuxerr.Environment, "Claude settings changed since PMux persisted them; PMux will not overwrite unrelated work.")
	}
	backup, err := os.ReadFile(record.BackupPath)
	if err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Persistent Claude settings backup is unavailable; no settings were changed.")
	}
	if hashBytes(backup) != record.BeforeSHA256 {
		return pmuxerr.New(pmuxerr.ClientSettingsConflict, pmuxerr.Environment, "Persistent Claude settings backup fingerprint does not match; no settings were changed.")
	}
	if record.OriginalExisted {
		if err := writeAtomic(record.SettingsPath, backup, 0o600); err != nil {
			return pmuxerr.Wrap(err, pmuxerr.ClientSettingsConflict, pmuxerr.Environment, "Could not restore the original Claude settings.")
		}
	} else if err := os.Remove(record.SettingsPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return pmuxerr.Wrap(err, pmuxerr.ClientSettingsConflict, pmuxerr.Environment, "Could not remove PMux-created Claude settings.")
	}
	if err := os.Remove(l.statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return pmuxerr.Wrap(err, pmuxerr.ClientSettingsConflict, pmuxerr.Environment, "Claude settings were restored, but PMux could not remove its ownership record.")
	}
	if err := os.Remove(record.BackupPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return pmuxerr.Wrap(err, pmuxerr.ClientSettingsConflict, pmuxerr.Environment, "Claude settings were restored, but PMux could not remove its backup.")
	}
	return nil
}

func readOptional(path string) ([]byte, error) {
	body, _, err := readOptionalWithExistence(path)
	return body, err
}

func readOptionalWithExistence(path string) ([]byte, bool, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return body, err == nil, err
}

func hashBytes(body []byte) string {
	hash := sha256.Sum256(body)
	return hex.EncodeToString(hash[:])
}

func redactedDiff(before, after []byte, token string) string {
	return "--- settings.json\n+++ settings.json\n@@ before @@\n" +
		redactedSettings(before, token) + "@@ after @@\n" + redactedSettings(after, token)
}

func redactedSettings(body []byte, knownSecret string) string {
	if len(bytes.TrimSpace(body)) == 0 {
		return ""
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return redact.Known(string(body), knownSecret)
	}
	sanitizeJSON(value)
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "<redacted settings>\n"
	}
	return redact.Known(string(encoded)+"\n", knownSecret)
}

func sanitizeJSON(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if redact.IsSensitiveKey(key) {
				if text, ok := child.(string); ok {
					typed[key] = redact.Mask(text)
				} else {
					typed[key] = "<redacted>"
				}
				continue
			}
			if strings.Contains(strings.ToLower(key), "url") {
				if text, ok := child.(string); ok {
					typed[key] = redact.URL(text)
					continue
				}
			}
			sanitizeJSON(child)
		}
	case []any:
		for _, child := range typed {
			sanitizeJSON(child)
		}
	}
}

func writeExclusive(path string, body []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(body); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	ok = true
	return syncDir(filepath.Dir(path))
}

func writeAtomic(path string, body []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".pmux-claude-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(body); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
