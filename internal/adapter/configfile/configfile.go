// Package configfile implements comment-preserving CLIProxyAPI YAML transactions.
package configfile

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	adapterfs "github.com/0p9b/pmux/internal/adapter/fs"
	adapterplatform "github.com/0p9b/pmux/internal/adapter/platform"
	domainconfig "github.com/0p9b/pmux/internal/domain/config"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/redact"
	"gopkg.in/yaml.v3"
)

// Adapter applies semantic patches to one CLIProxyAPI configuration file. The
// backup directory is the canonical per-instance backup directory.
type Adapter struct {
	backupDir string
	now       func() time.Time
	random    io.Reader

	// afterRename is a fault-injection seam used to prove rollback. Production
	// adapters leave it nil.
	afterRename func(string) error
}

// New constructs a configuration adapter. backupDir must be the private,
// per-instance directory in which canonical config backups are retained.
func New(backupDir string) *Adapter {
	return &Adapter{backupDir: backupDir, now: time.Now, random: rand.Reader}
}

// GenerateProxyKey creates an sk- prefixed key backed by 32 bytes from the
// operating system CSPRNG.
func GenerateProxyKey() (string, error) {
	return generateProxyKey(rand.Reader)
}

func generateProxyKey(random io.Reader) (string, error) {
	if random == nil {
		random = rand.Reader
	}
	material := make([]byte, 32)
	if _, err := io.ReadFull(random, material); err != nil {
		return "", pmuxerr.Wrap(err, pmuxerr.UnhandledInternal, pmuxerr.Internal, "could not generate a secure proxy key")
	}
	return "sk-" + hex.EncodeToString(material), nil
}

// IsTemplateAPIKey reports keys known to put CLIProxyAPI into safe mode, plus
// obvious empty/template shapes. It deliberately does not classify arbitrary
// short or test keys as templates.
func IsTemplateAPIKey(value string) bool {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" {
		return true
	}
	switch v {
	case "example-api-key", "your-api-key", "your-api-key-1",
		"your-api-key-2", "replace-me", "changeme":
		return true
	}
	return strings.Contains(v, "example-api-key") ||
		strings.Contains(v, "placeholder") ||
		strings.Contains(v, "<your-")
}

// Read parses path into the transport-neutral domain snapshot while retaining
// the exact file fingerprint used for optimistic concurrency control.
func (a *Adapter) Read(ctx context.Context, path string) (domainconfig.ConfigSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return domainconfig.ConfigSnapshot{}, wrapCanceled(err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return domainconfig.ConfigSnapshot{}, readError(err, "could not resolve config.yaml path")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return domainconfig.ConfigSnapshot{}, readError(err, "could not resolve config.yaml symlinks")
	}
	absolute = resolved
	body, err := os.ReadFile(absolute)
	if err != nil {
		return domainconfig.ConfigSnapshot{}, readError(err, "could not read config.yaml")
	}
	doc, err := parseDocument(body)
	if err != nil {
		return domainconfig.ConfigSnapshot{}, readError(err, "config.yaml is invalid")
	}
	cfg, err := decodeConfig(doc)
	if err != nil {
		return domainconfig.ConfigSnapshot{}, configError(err, "config.yaml contains invalid managed fields")
	}
	return domainconfig.ConfigSnapshot{Path: absolute, Fingerprint: sha256.Sum256(body), Config: cfg}, nil
}

// Plan rereads the source, rejects a stale snapshot, applies only known
// semantic paths to the YAML AST, and produces validated candidate bytes.
func (a *Adapter) Plan(ctx context.Context, snapshot domainconfig.ConfigSnapshot, ops []domainconfig.PatchOp) (domainconfig.PatchPlan, error) {
	if err := ctx.Err(); err != nil {
		return domainconfig.PatchPlan{}, wrapCanceled(err)
	}
	body, err := os.ReadFile(snapshot.Path)
	if err != nil {
		return domainconfig.PatchPlan{}, readError(err, "could not reread config.yaml")
	}
	if sha256.Sum256(body) != snapshot.Fingerprint {
		return domainconfig.PatchPlan{}, conflictError(snapshot.Path)
	}
	doc, err := parseDocument(body)
	if err != nil {
		return domainconfig.PatchPlan{}, readError(err, "config.yaml is invalid")
	}

	restart := false
	for _, op := range ops {
		if err := ctx.Err(); err != nil {
			return domainconfig.PatchPlan{}, wrapCanceled(err)
		}
		parts, err := validatePatch(op)
		if err != nil {
			return domainconfig.PatchPlan{}, err
		}
		if op.Unset {
			removePath(doc.Content[0], parts)
		} else if err := setPath(doc.Content[0], parts, op.Value); err != nil {
			return domainconfig.PatchPlan{}, configError(err, "could not apply configuration patch")
		}
		if requiresRestart(parts) {
			restart = true
		}
	}

	rendered, err := renderDocument(doc)
	if err != nil {
		return domainconfig.PatchPlan{}, configError(err, "could not render config.yaml")
	}
	candidateDoc, err := parseDocument(rendered)
	if err != nil {
		return domainconfig.PatchPlan{}, configError(err, "rendered config.yaml is invalid")
	}
	candidate, err := decodeConfig(candidateDoc)
	if err != nil {
		return domainconfig.PatchPlan{}, configError(err, "rendered config.yaml contains invalid managed fields")
	}
	if err := validatePolicy(candidate); err != nil {
		return domainconfig.PatchPlan{}, err
	}

	return domainconfig.PatchPlan{
		Snapshot:        snapshot,
		Operations:      append([]domainconfig.PatchOp(nil), ops...),
		Rendered:        rendered,
		RestartRequired: restart,
		Diff:            redactedDiff(ops),
	}, nil
}

// PlanManagedHardening enforces PMux's managed defaults. Real keys are
// preserved, known template keys are removed, and a fresh key is generated
// only when no real key remains. The returned human-facing Diff is redacted.
func (a *Adapter) PlanManagedHardening(ctx context.Context, snapshot domainconfig.ConfigSnapshot) (domainconfig.PatchPlan, error) {
	keys := make([]string, 0, len(snapshot.Config.APIKeys))
	for _, key := range snapshot.Config.APIKeys {
		if !IsTemplateAPIKey(key) {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		key, err := generateProxyKey(a.random)
		if err != nil {
			return domainconfig.PatchPlan{}, err
		}
		keys = append(keys, key)
	}
	return a.Plan(ctx, snapshot, []domainconfig.PatchOp{
		{Path: "host", Value: "127.0.0.1"},
		{Path: "api-keys", Value: keys},
		{Path: "ws-auth", Value: true},
		{Path: "remote-management.allow-remote", Value: false},
		{Path: "remote-management.disable-control-panel", Value: true},
	})
}

// Apply commits a plan using a private canonical backup and a same-directory
// fsynced temporary file. It rechecks the source fingerprint immediately before
// mutation and rolls the original bytes back if post-rename verification fails.
func (a *Adapter) Apply(ctx context.Context, plan domainconfig.PatchPlan) (domainconfig.PatchResult, error) {
	if err := ctx.Err(); err != nil {
		return domainconfig.PatchResult{}, wrapCanceled(err)
	}
	if len(plan.Rendered) == 0 {
		return domainconfig.PatchResult{}, configError(errors.New("empty candidate"), "refusing to write an empty config.yaml")
	}
	candidateDoc, err := parseDocument(plan.Rendered)
	if err != nil {
		return domainconfig.PatchResult{}, configError(err, "refusing to write invalid config.yaml")
	}
	candidate, err := decodeConfig(candidateDoc)
	if err != nil {
		return domainconfig.PatchResult{}, configError(err, "refusing to write config.yaml with invalid managed fields")
	}
	if err := validatePolicy(candidate); err != nil {
		return domainconfig.PatchResult{}, err
	}
	candidateFingerprint := sha256.Sum256(plan.Rendered)
	current, err := os.ReadFile(plan.Snapshot.Path)
	if err != nil {
		return domainconfig.PatchResult{}, readError(err, "could not reread config.yaml before writing")
	}
	if sha256.Sum256(current) != plan.Snapshot.Fingerprint {
		return domainconfig.PatchResult{}, conflictError(plan.Snapshot.Path)
	}
	if a.backupDir == "" || !filepath.IsAbs(a.backupDir) {
		return domainconfig.PatchResult{}, configError(errors.New("backup directory is not absolute"), "backup directory must be an absolute per-instance path")
	}
	if err := adapterfs.EnsurePrivateDir(a.backupDir); err != nil {
		return domainconfig.PatchResult{}, err
	}
	backupPath, err := a.createBackup(current)
	if err != nil {
		return domainconfig.PatchResult{}, err
	}

	if err := adapterfs.AtomicWritePrivate(plan.Snapshot.Path, plan.Rendered); err != nil {
		committed, readErr := os.ReadFile(plan.Snapshot.Path)
		if readErr == nil && sha256.Sum256(committed) == candidateFingerprint {
			if rollbackErr := restorePrior(plan.Snapshot.Path, current); rollbackErr != nil {
				combined := fmt.Errorf("commit: %w; rollback: %v", err, rollbackErr)
				return domainconfig.PatchResult{}, configError(combined, "config update durability failed and rollback also failed")
			}
		}
		return domainconfig.PatchResult{}, err
	}
	verifyErr := error(nil)
	if a.afterRename != nil {
		verifyErr = a.afterRename(plan.Snapshot.Path)
	}
	if verifyErr == nil {
		committed, readErr := os.ReadFile(plan.Snapshot.Path)
		if readErr != nil {
			verifyErr = readErr
		} else if sha256.Sum256(committed) != candidateFingerprint {
			verifyErr = errors.New("committed fingerprint differs from candidate")
		}
	}
	if verifyErr != nil {
		if rollbackErr := restorePrior(plan.Snapshot.Path, current); rollbackErr != nil {
			combined := fmt.Errorf("verify commit: %w; rollback: %v", verifyErr, rollbackErr)
			return domainconfig.PatchResult{}, configError(combined, "config update failed verification and rollback also failed")
		}
		return domainconfig.PatchResult{}, configError(verifyErr, "config update failed verification; the prior config was restored")
	}

	return domainconfig.PatchResult{
		BackupPath:      backupPath,
		Fingerprint:     candidateFingerprint,
		RestartRequired: plan.RestartRequired,
	}, nil
}

// Validate reports managed security-policy diagnostics without exposing key
// values.
func (a *Adapter) Validate(ctx context.Context, snapshot domainconfig.ConfigSnapshot) []domainconfig.Diagnostic {
	if ctx.Err() != nil {
		return []domainconfig.Diagnostic{{ID: "CFG-CANCELED", Severity: "critical", Message: "configuration validation was canceled"}}
	}
	var out []domainconfig.Diagnostic
	if snapshot.Config.Host != "127.0.0.1" && snapshot.Config.Host != "localhost" && snapshot.Config.Host != "::1" {
		out = append(out, domainconfig.Diagnostic{ID: "SEC-EXPOSURE", Severity: "critical", Message: "proxy is not bound to loopback"})
	}
	if !snapshot.Config.WSAuth {
		out = append(out, domainconfig.Diagnostic{ID: "CFG-WSAUTH", Severity: "warning", Message: "ws-auth is not explicitly enabled"})
	}
	if !snapshot.Config.ManagementLocal {
		out = append(out, domainconfig.Diagnostic{ID: "MGMT-LOCAL", Severity: "critical", Message: "management API is not restricted to localhost"})
	}
	if len(snapshot.Config.APIKeys) == 0 {
		out = append(out, domainconfig.Diagnostic{ID: "KEY-SAFEMODE", Severity: "critical", Message: "no proxy API key is configured"})
	} else {
		for _, key := range snapshot.Config.APIKeys {
			if IsTemplateAPIKey(key) {
				out = append(out, domainconfig.Diagnostic{ID: "KEY-SAFEMODE", Severity: "critical", Message: "a template proxy API key would activate safe mode"})
				break
			}
		}
	}
	if info, err := os.Stat(snapshot.Path); err == nil {
		switch runtime.GOOS {
		case "windows":
			if platform, platformErr := adapterplatform.New(""); platformErr == nil {
				if verifyErr := platform.VerifySecurePermissions(snapshot.Path, false); verifyErr != nil {
					out = append(out, domainconfig.Diagnostic{ID: "KEY-PERMS", Severity: "critical", Message: "config.yaml permissions are not private"})
				}
			}
		default:
			if info.Mode().Perm()&0o077 != 0 {
				out = append(out, domainconfig.Diagnostic{ID: "KEY-PERMS", Severity: "critical", Message: "config.yaml permissions are not private"})
			}
		}
	}
	return out
}

func (a *Adapter) createBackup(body []byte) (string, error) {
	stamp := a.now().UTC().Format("20060102T150405Z")
	sum := sha256.Sum256(body)
	name := fmt.Sprintf("config.yaml.%s.%s.bak", stamp, hex.EncodeToString(sum[:4]))
	path := filepath.Join(a.backupDir, name)
	if err := adapterfs.WritePrivateExclusive(path, body); err != nil {
		return "", err
	}
	return path, nil
}
func restorePrior(path string, prior []byte) error {
	if err := adapterfs.AtomicWritePrivate(path, prior); err != nil {
		return err
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if sha256.Sum256(restored) != sha256.Sum256(prior) {
		return errors.New("restored fingerprint differs from prior config")
	}
	return nil
}

func parseDocument(body []byte) (*yaml.Node, error) {
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("config.yaml must contain exactly one YAML document")
		}
		return nil, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("top-level YAML value must be a mapping")
	}
	if err := rejectDuplicateKeys(doc.Content[0], ""); err != nil {
		return nil, err
	}
	return &doc, nil
}

func rejectDuplicateKeys(node *yaml.Node, prefix string) error {
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]struct{}, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			name := key.Value
			if _, ok := seen[name]; ok {
				return fmt.Errorf("duplicate mapping key %q", joinPath(prefix, name))
			}
			seen[name] = struct{}{}
			if err := rejectDuplicateKeys(value, joinPath(prefix, name)); err != nil {
				return err
			}
		}
	} else {
		for _, child := range node.Content {
			if err := rejectDuplicateKeys(child, prefix); err != nil {
				return err
			}
		}
	}
	return nil
}

func decodeConfig(doc *yaml.Node) (domainconfig.Config, error) {
	root := doc.Content[0]
	var cfg domainconfig.Config
	if n := lookupPath(root, []string{"host"}); n != nil {
		if n.Kind != yaml.ScalarNode {
			return cfg, errors.New("host must be a string")
		}
		cfg.Host = n.Value
	}
	if n := lookupPath(root, []string{"port"}); n != nil {
		port, err := strconv.Atoi(n.Value)
		if err != nil || port < 1 || port > 65535 {
			return cfg, errors.New("port must be an integer from 1 to 65535")
		}
		cfg.Port = port
	}
	if n := lookupPath(root, []string{"auth-dir"}); n != nil {
		if n.Kind != yaml.ScalarNode {
			return cfg, errors.New("auth-dir must be a string")
		}
		cfg.AuthDir = n.Value
	}
	if n := lookupPath(root, []string{"ws-auth"}); n != nil {
		if err := n.Decode(&cfg.WSAuth); err != nil {
			return cfg, errors.New("ws-auth must be a boolean")
		}
	}
	if n := lookupPath(root, []string{"api-keys"}); n != nil {
		if n.Kind != yaml.SequenceNode {
			return cfg, errors.New("api-keys must be a sequence")
		}
		if err := n.Decode(&cfg.APIKeys); err != nil {
			return cfg, errors.New("api-keys must contain strings")
		}
	}
	cfg.ManagementLocal = true
	if n := lookupPath(root, []string{"remote-management", "allow-remote"}); n != nil {
		var allow bool
		if err := n.Decode(&allow); err != nil {
			return cfg, errors.New("remote-management.allow-remote must be a boolean")
		}
		cfg.ManagementLocal = !allow
	}
	return cfg, nil
}

func validatePolicy(cfg domainconfig.Config) error {
	if cfg.Port < 1 || cfg.Port > 65535 {
		return configError(errors.New("port outside range"), "refusing to write an invalid proxy port")
	}
	if strings.TrimSpace(cfg.AuthDir) == "" {
		return configError(errors.New("empty auth-dir"), "refusing to write config.yaml without an auth directory")
	}
	if !filepath.IsAbs(cfg.AuthDir) && !strings.HasPrefix(cfg.AuthDir, "~"+string(filepath.Separator)) {
		return configError(errors.New("relative auth-dir"), "refusing to write config.yaml with a relative auth directory")
	}
	if !cfg.ManagementLocal {
		return configError(errors.New("remote management enabled"), "refusing to expose the management API remotely")
	}
	if len(cfg.APIKeys) == 0 {
		return configError(errors.New("empty api-keys"), "refusing to write config.yaml without a proxy API key")
	}
	for _, key := range cfg.APIKeys {
		if IsTemplateAPIKey(key) {
			return &pmuxerr.Error{
				Code: pmuxerr.ConfigSafeMode, Class: pmuxerr.User,
				Message:     "refusing to write a template proxy API key",
				Explanation: "CLIProxyAPI would enter safe mode and reject requests",
				Evidence:    []string{"api-keys contains " + redact.Mask(key)},
				Repair:      []string{"run the managed hardening transaction to generate a private key"},
			}
		}
	}
	return nil
}

func validatePatch(op domainconfig.PatchOp) ([]string, error) {
	path := strings.TrimSpace(op.Path)
	if path == "" || strings.HasPrefix(path, ".") || strings.HasSuffix(path, ".") || strings.Contains(path, "..") {
		return nil, configError(errors.New("invalid patch path"), "configuration path is invalid")
	}
	parts := strings.Split(path, ".")
	if !knownPath(parts) {
		return nil, configError(fmt.Errorf("unknown path %q", path), "unknown configuration path; no changes were planned")
	}
	if op.Unset {
		return parts, nil
	}
	if err := validateValue(parts, op.Value); err != nil {
		return nil, configError(err, "configuration value has the wrong type")
	}
	return parts, nil
}

func knownPath(parts []string) bool {
	path := strings.Join(parts, ".")
	switch path {
	case "host", "port", "auth-dir", "ws-auth", "api-keys",
		"remote-management.allow-remote", "remote-management.disable-control-panel",
		"remote-management.secret-key", "tls", "tls.enable", "tls.cert", "tls.key",
		"pgstore", "objectstore", "gitstore", "plugins":
		return true
	}
	// Token-store and plugin fields are intentionally accepted as subtrees but
	// still classified as restart-required. They are typed by yaml.v3.
	return len(parts) > 1 && (parts[0] == "pgstore" || parts[0] == "objectstore" || parts[0] == "gitstore" || parts[0] == "plugins")
}

func validateValue(parts []string, value any) error {
	path := strings.Join(parts, ".")
	switch path {
	case "host", "auth-dir", "remote-management.secret-key", "tls.cert", "tls.key":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s must be a string", path)
		}
	case "port":
		v, ok := integer(value)
		if !ok || v < 1 || v > 65535 {
			return errors.New("port must be an integer from 1 to 65535")
		}
	case "ws-auth", "remote-management.allow-remote", "remote-management.disable-control-panel", "tls.enable":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be a boolean", path)
		}
	case "api-keys":
		switch value.(type) {
		case []string, []any:
		default:
			return errors.New("api-keys must be a string sequence")
		}
	}
	return nil
}

func integer(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int8:
		return int(v), true
	case int16:
		return int(v), true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case uint:
		if uint64(v) > uint64(^uint(0)>>1) {
			return 0, false
		}
		return int(v), true
	case uint8:
		return int(v), true
	case uint16:
		return int(v), true
	case uint32:
		return int(v), true
	case uint64:
		if v > uint64(^uint(0)>>1) {
			return 0, false
		}
		return int(v), true
	default:
		return 0, false
	}
}

func requiresRestart(parts []string) bool {
	switch parts[0] {
	case "host", "port", "tls", "pgstore", "objectstore", "gitstore", "plugins":
		return true
	default:
		return false
	}
}

func lookupPath(root *yaml.Node, parts []string) *yaml.Node {
	current := root
	for _, part := range parts {
		if current == nil || current.Kind != yaml.MappingNode {
			return nil
		}
		_, value := mappingEntry(current, part)
		current = value
	}
	return current
}

func mappingEntry(mapping *yaml.Node, key string) (int, *yaml.Node) {
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return i, mapping.Content[i+1]
		}
	}
	return -1, nil
}

func setPath(root *yaml.Node, parts []string, value any) error {
	current := root
	for _, part := range parts[:len(parts)-1] {
		idx, child := mappingEntry(current, part)
		if child == nil {
			keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: part}
			child = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			current.Content = append(current.Content, keyNode, child)
		} else if child.Kind != yaml.MappingNode {
			return fmt.Errorf("%s is not a mapping", part)
		} else {
			_ = idx
		}
		current = child
	}
	leaf := parts[len(parts)-1]
	idx, existing := mappingEntry(current, leaf)
	replacement, err := valueNode(value)
	if err != nil {
		return err
	}
	if existing != nil && existing.Kind == replacement.Kind && existing.Kind == yaml.ScalarNode {
		sameTag := existing.Tag == replacement.Tag
		existing.Tag = replacement.Tag
		existing.Value = replacement.Value
		if !sameTag {
			existing.Style = replacement.Style
		}
		return nil
	}
	if existing != nil {
		replacement.HeadComment = existing.HeadComment
		replacement.LineComment = existing.LineComment
		replacement.FootComment = existing.FootComment
		current.Content[idx+1] = replacement
		return nil
	}
	current.Content = append(current.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: leaf}, replacement)
	return nil
}

func removePath(root *yaml.Node, parts []string) bool {
	current := root
	for _, part := range parts[:len(parts)-1] {
		_, current = mappingEntry(current, part)
		if current == nil || current.Kind != yaml.MappingNode {
			return false
		}
	}
	idx, value := mappingEntry(current, parts[len(parts)-1])
	if value == nil {
		return false
	}
	current.Content = append(current.Content[:idx], current.Content[idx+2:]...)
	return true
}

func valueNode(value any) (*yaml.Node, error) {
	body, err := yaml.Marshal(value)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) != 1 {
		return nil, errors.New("value could not be represented as YAML")
	}
	return doc.Content[0], nil
}

func renderDocument(doc *yaml.Node) ([]byte, error) {
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func redactedDiff(ops []domainconfig.PatchOp) string {
	var out strings.Builder
	for _, op := range ops {
		if op.Unset {
			fmt.Fprintf(&out, "- %s\n", op.Path)
			continue
		}
		if sensitivePath(op.Path) {
			fmt.Fprintf(&out, "~ %s: <redacted>\n", op.Path)
		} else {
			fmt.Fprintf(&out, "~ %s: %v\n", op.Path, op.Value)
		}
	}
	return out.String()
}

func sensitivePath(path string) bool {
	path = strings.ToLower(path)
	parts := strings.Split(path, ".")
	switch parts[0] {
	case "api-keys", "pgstore", "objectstore", "gitstore", "plugins":
		return true
	}
	return redact.IsSensitiveKey(parts[len(parts)-1])
}

func conflictError(path string) error {
	return &pmuxerr.Error{
		Code: pmuxerr.ConfigMutationConflict, Class: pmuxerr.Environment,
		Message:     "config.yaml changed on disk since PMux last read it",
		Explanation: "overwriting would discard another writer's change",
		Evidence:    []string{path},
		Repair:      []string{"reload the configuration and preview the change again"},
	}
}

func readError(cause error, message string) error {
	return pmuxerr.Wrap(cause, pmuxerr.ConfigUnreadable, pmuxerr.Environment, message)
}

func configError(cause error, message string) error {
	return pmuxerr.Wrap(cause, pmuxerr.ConfigValidationFailed, pmuxerr.Environment, message)
}

func wrapCanceled(cause error) error {
	return pmuxerr.Wrap(cause, pmuxerr.CodeCanceled, pmuxerr.User, "configuration operation was canceled")
}

func joinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}
