package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/0p9b/pmux/internal/adapter/configfile"
	"github.com/0p9b/pmux/internal/app"
	domainconfig "github.com/0p9b/pmux/internal/domain/config"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/state"
)

// Edit opens exactly one private temporary copy with a directly executed editor
// binary. No shell parses the editor name, target path, or edited contents.
func (n *nativeRuntime) Edit(ctx context.Context, request app.ConfigEditRequest) (app.ConfigEditResult, error) {
	if request.Scope != "proxy" && request.Scope != "pmux" {
		return app.ConfigEditResult{}, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Configuration edit scope must be proxy or pmux.")
	}
	if request.Target == "" || !filepath.IsAbs(request.Target) {
		return app.ConfigEditResult{}, pmuxerr.New(pmuxerr.ConfigPathMismatch, pmuxerr.User, "Configuration edit target must be an absolute path.")
	}
	if request.Scope == "proxy" && n.isContainerConfigPath(request.Target) {
		return app.ConfigEditResult{}, containerMutationError()
	}
	editor := strings.TrimSpace(request.Editor)
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("VISUAL"))
	}
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if editor == "" {
		return app.ConfigEditResult{}, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Configuration edit requires --editor <executable> or a VISUAL/EDITOR executable.")
	}

	before, err := os.ReadFile(request.Target)
	if errors.Is(err, os.ErrNotExist) && request.Scope == "pmux" {
		cfg, loadErr := n.store.LoadConfig()
		if loadErr != nil {
			return app.ConfigEditResult{}, loadErr
		}
		before, err = json.MarshalIndent(cfg, "", "  ")
		before = append(before, '\n')
	}
	if err != nil {
		return app.ConfigEditResult{}, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "PMux could not read the selected configuration scope.")
	}
	if err := os.MkdirAll(filepath.Dir(request.Target), 0o700); err != nil {
		return app.ConfigEditResult{}, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "PMux could not prepare the private configuration directory.")
	}
	temp, err := os.CreateTemp(filepath.Dir(request.Target), ".pmux-edit-*")
	if err != nil {
		return app.ConfigEditResult{}, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "PMux could not create a private temporary configuration copy.")
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if chmodErr := temp.Chmod(0o600); chmodErr == nil {
		_, err = temp.Write(before)
	} else {
		err = chmodErr
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return app.ConfigEditResult{}, pmuxerr.Wrap(err, pmuxerr.ConfigInsecurePermissions, pmuxerr.Environment, "PMux could not write the private temporary configuration copy.")
	}

	path, err := exec.LookPath(editor)
	if err != nil {
		return app.ConfigEditResult{}, pmuxerr.Wrap(err, pmuxerr.CodeExecutableMissing, pmuxerr.Environment, "The requested editor executable was not found.")
	}
	command := exec.CommandContext(ctx, path, tempPath)
	command.Stdin, command.Stdout, command.Stderr = n.stdin, n.stdout, n.stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return app.ConfigEditResult{}, pmuxerr.Wrap(ctx.Err(), pmuxerr.CodeCanceled, pmuxerr.User, "Configuration editing was canceled; no changes were made.")
		}
		return app.ConfigEditResult{}, pmuxerr.Wrap(err, pmuxerr.CodeCanceled, pmuxerr.User, "The editor exited without committing a configuration change.")
	}
	after, err := os.ReadFile(tempPath)
	if err != nil {
		return app.ConfigEditResult{}, pmuxerr.Wrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "PMux could not read the edited temporary configuration.")
	}
	if bytes.Equal(before, after) {
		return app.ConfigEditResult{}, pmuxerr.New(pmuxerr.CodeCanceled, pmuxerr.User, "Configuration was unchanged; no changes were made.")
	}

	if request.Scope == "pmux" {
		return n.commitPMuxEdit(request, before, after)
	}
	return n.commitProxyEdit(ctx, request, before, tempPath)
}

func (n *nativeRuntime) commitPMuxEdit(request app.ConfigEditRequest, before, after []byte) (app.ConfigEditResult, error) {
	candidate, err := decodePMuxConfig(after)
	if err != nil {
		return app.ConfigEditResult{}, pmuxerr.Wrap(err, pmuxerr.ConfigValidationFailed, pmuxerr.User, "Edited PMux settings are invalid JSON; no changes were written.")
	}
	diff := textDiff(before, after, filepath.Base(request.Target))
	if n.stdout != nil {
		_, _ = fmt.Fprintln(n.stdout, diff)
	}
	ok, err := confirmEdit(request, diff)
	if err != nil {
		return app.ConfigEditResult{}, err
	}
	if !ok {
		return app.ConfigEditResult{}, pmuxerr.New(pmuxerr.CodeCanceled, pmuxerr.User, "Configuration edit was canceled; no changes were made.")
	}
	current, readErr := os.ReadFile(request.Target)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return app.ConfigEditResult{}, readErr
	}
	if readErr == nil && !bytes.Equal(current, before) {
		return app.ConfigEditResult{}, pmuxerr.New(pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "PMux settings changed while the editor was open; no changes were written.")
	}
	backupID, err := n.BackupPMux(context.Background())
	if err != nil {
		return app.ConfigEditResult{}, pmuxerr.Wrap(err, pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "Could not create a private PMux settings backup; no changes were written.")
	}
	if err := n.store.SaveConfig(candidate); err != nil {
		return app.ConfigEditResult{}, err
	}
	return app.ConfigEditResult{Path: request.Target, Diff: diff, BackupPath: backupID}, nil
}

func (n *nativeRuntime) commitProxyEdit(ctx context.Context, request app.ConfigEditRequest, before []byte, tempPath string) (app.ConfigEditResult, error) {
	adapter := configfile.New(filepath.Join(n.roots.State, "backups", installationIDForPath(n.roots.Data, request.Target)))
	original, err := adapter.Read(ctx, request.Target)
	if err != nil {
		return app.ConfigEditResult{}, err
	}
	edited, err := adapter.Read(ctx, tempPath)
	if err != nil {
		return app.ConfigEditResult{}, pmuxerr.Wrap(err, pmuxerr.ConfigValidationFailed, pmuxerr.User, "Edited proxy configuration is invalid; no changes were written.")
	}
	ops := configPatchDifference(original.Config, edited.Config)
	if len(ops) == 0 {
		return app.ConfigEditResult{}, pmuxerr.New(pmuxerr.CodeCanceled, pmuxerr.User, "The edit changed no supported proxy configuration values; no changes were written.")
	}
	plan, err := adapter.Plan(ctx, original, ops)
	if err != nil {
		return app.ConfigEditResult{}, err
	}
	if n.stdout != nil {
		_, _ = fmt.Fprintln(n.stdout, plan.Diff)
	}
	ok, err := confirmEdit(request, plan.Diff)
	if err != nil {
		return app.ConfigEditResult{}, err
	}
	if !ok {
		return app.ConfigEditResult{}, pmuxerr.New(pmuxerr.CodeCanceled, pmuxerr.User, "Configuration edit was canceled; no changes were made.")
	}
	current, err := os.ReadFile(request.Target)
	if err != nil || !bytes.Equal(current, before) {
		if err == nil {
			err = errors.New("configuration fingerprint changed")
		}
		return app.ConfigEditResult{}, pmuxerr.Wrap(err, pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "Proxy configuration changed while the editor was open; no changes were written.")
	}
	result, err := adapter.Apply(ctx, plan)
	if err != nil {
		return app.ConfigEditResult{}, err
	}
	return app.ConfigEditResult{Path: request.Target, Diff: plan.Diff, BackupPath: result.BackupPath, RestartRequired: result.RestartRequired}, nil
}

func confirmEdit(request app.ConfigEditRequest, diff string) (bool, error) {
	if request.Confirm == nil {
		return false, pmuxerr.New(pmuxerr.CodeUsage, pmuxerr.User, "Configuration edit requires explicit confirmation; no changes were made.")
	}
	return request.Confirm(diff)
}

func configPatchDifference(before, after domainconfig.Config) []domainconfig.PatchOp {
	ops := make([]domainconfig.PatchOp, 0, 6)
	if before.Host != after.Host {
		ops = append(ops, domainconfig.PatchOp{Path: "host", Value: after.Host})
	}
	if before.Port != after.Port {
		ops = append(ops, domainconfig.PatchOp{Path: "port", Value: after.Port})
	}
	if before.AuthDir != after.AuthDir {
		ops = append(ops, domainconfig.PatchOp{Path: "auth-dir", Value: after.AuthDir})
	}
	if before.WSAuth != after.WSAuth {
		ops = append(ops, domainconfig.PatchOp{Path: "ws-auth", Value: after.WSAuth})
	}
	if before.ManagementLocal != after.ManagementLocal {
		ops = append(ops, domainconfig.PatchOp{Path: "remote-management.allow-remote", Value: !after.ManagementLocal})
	}
	if !reflect.DeepEqual(before.APIKeys, after.APIKeys) {
		ops = append(ops, domainconfig.PatchOp{Path: "api-keys", Value: after.APIKeys})
	}
	return ops
}

func textDiff(before, after []byte, name string) string {
	return fmt.Sprintf("--- %s\n+++ %s\n@@ before @@\n%s@@ after @@\n%s", name, name, before, after)
}

func (n *nativeRuntime) BackupPMux(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	body, _, err := n.currentPMuxConfigBytes()
	if err != nil {
		return "", err
	}
	return n.writePMuxBackup(body)
}

func (n *nativeRuntime) PlanRestorePMux(ctx context.Context, id string, current state.Config) (app.PMuxConfigRestorePlan, error) {
	if err := ctx.Err(); err != nil {
		return app.PMuxConfigRestorePlan{}, err
	}
	if filepath.Base(id) != id || !strings.HasPrefix(id, "config.json.") || !strings.HasSuffix(id, ".bak") {
		return app.PMuxConfigRestorePlan{}, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "PMux settings backup ID is invalid.")
	}
	body, err := os.ReadFile(filepath.Join(n.pmuxBackupDir(), id))
	if err != nil {
		return app.PMuxConfigRestorePlan{}, err
	}
	candidate, err := decodePMuxConfig(body)
	if err != nil {
		return app.PMuxConfigRestorePlan{}, err
	}
	sum := sha256.Sum256(body)
	parts := strings.Split(id, ".")
	if len(parts) < 5 || parts[len(parts)-2] != fmt.Sprintf("%x", sum[:4]) {
		return app.PMuxConfigRestorePlan{}, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "PMux settings backup checksum does not match its backup ID.")
	}
	before, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return app.PMuxConfigRestorePlan{}, err
	}
	return app.PMuxConfigRestorePlan{
		ID: id, Fingerprint: sum, Config: candidate,
		Diff: textDiff(append(before, '\n'), body, "config.json"),
	}, nil
}

func (n *nativeRuntime) RestorePMux(ctx context.Context, plan app.PMuxConfigRestorePlan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := filepath.Join(n.pmuxBackupDir(), plan.ID)
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if sha256.Sum256(body) != plan.Fingerprint {
		return pmuxerr.New(pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "PMux settings backup changed after preview; no changes were written.")
	}
	candidate, err := decodePMuxConfig(body)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(candidate, plan.Config) {
		return pmuxerr.New(pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "PMux settings backup no longer matches its validated restore plan.")
	}
	current, exists, err := n.currentPMuxConfigBytes()
	if err != nil {
		return err
	}
	if exists {
		if _, err := n.writePMuxBackup(current); err != nil {
			return err
		}
	}
	target := filepath.Join(n.roots.Config, "config.json")
	if err := replacePrivateFile(target, body); err != nil {
		return err
	}
	loaded, err := n.store.LoadConfig()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(loaded, candidate) {
		return pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.Internal, "Restored PMux settings did not match the validated backup.")
	}
	return nil
}

func (n *nativeRuntime) currentPMuxConfigBytes() ([]byte, bool, error) {
	path := filepath.Join(n.roots.Config, "config.json")
	body, err := os.ReadFile(path)
	if err == nil {
		if _, decodeErr := decodePMuxConfig(body); decodeErr != nil {
			return nil, true, decodeErr
		}
		return body, true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	value, err := n.store.LoadConfig()
	if err != nil {
		return nil, false, err
	}
	body, err = json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(body, '\n'), false, nil
}

func (n *nativeRuntime) pmuxBackupDir() string {
	return filepath.Join(n.roots.State, "backups", "pmux")
}

func (n *nativeRuntime) writePMuxBackup(body []byte) (string, error) {
	sum := sha256.Sum256(body)
	id := fmt.Sprintf("config.json.%s.%x.bak", time.Now().UTC().Format("20060102T150405Z"), sum[:4])
	dir := n.pmuxBackupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	file, err := os.OpenFile(filepath.Join(dir, id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err = file.Write(body); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

func decodePMuxConfig(body []byte) (state.Config, error) {
	var value state.Config
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return state.Config{}, pmuxerr.Wrap(err, pmuxerr.ConfigValidationFailed, pmuxerr.User, "PMux settings backup is invalid JSON.")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return state.Config{}, pmuxerr.Wrap(err, pmuxerr.ConfigValidationFailed, pmuxerr.User, "PMux settings backup contains trailing data.")
	}
	if value.Version != state.SchemaVersion {
		return state.Config{}, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, fmt.Sprintf("PMux settings backup schema version %d is unsupported.", value.Version))
	}
	return value, nil
}

func replacePrivateFile(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".pmux-config-restore-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if chmodErr := temp.Chmod(0o600); chmodErr == nil {
		_, err = temp.Write(body)
		if err == nil {
			err = temp.Sync()
		}
	} else {
		err = chmodErr
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
