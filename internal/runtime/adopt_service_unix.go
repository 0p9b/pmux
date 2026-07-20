//go:build !windows

package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"

	"github.com/0p9b/pmux/internal/adapter/discovery"
	adapterfs "github.com/0p9b/pmux/internal/adapter/fs"
	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

type nativeAdoptedServiceCutover struct{}

func (nativeAdoptedServiceCutover) Replace(ctx context.Context, observed discovery.ServiceEvidence) (func(context.Context) error, error) {
	if observed.Definition == "" || observed.Identity == "" {
		return nil, pmuxerr.New(pmuxerr.ServiceForeignOwner, pmuxerr.Environment, "The adopted service definition is not identifiable; hardening cannot replace it safely.")
	}
	body, err := os.ReadFile(observed.Definition)
	if err != nil {
		return nil, pmuxerr.Wrap(err, pmuxerr.ServiceForeignOwner, pmuxerr.Environment, "PMux could not back up the adopted service definition before hardening.")
	}
	active, err := adoptedServiceActive(ctx, observed)
	if err != nil {
		return nil, err
	}
	if active {
		if err := adoptedServiceStop(ctx, observed); err != nil {
			return nil, err
		}
	}
	if err := os.Remove(observed.Definition); err != nil {
		if active {
			_ = adoptedServiceStart(context.Background(), observed)
		}
		return nil, pmuxerr.Wrap(err, pmuxerr.ServiceForeignOwner, pmuxerr.Environment, "PMux could not remove the backed-up foreign service definition during confirmed hardening.")
	}
	if err := adoptedServiceReload(ctx, observed); err != nil {
		_ = adapterfs.AtomicWritePrivate(observed.Definition, body)
		if active {
			_ = adoptedServiceStart(context.Background(), observed)
		}
		return nil, err
	}
	return func(rollbackCtx context.Context) error {
		if err := adapterfs.AtomicWritePrivate(observed.Definition, body); err != nil {
			return err
		}
		if err := adoptedServiceReload(rollbackCtx, observed); err != nil {
			return err
		}
		if active {
			return adoptedServiceStart(rollbackCtx, observed)
		}
		return nil
	}, nil
}

func adoptedServiceActive(ctx context.Context, observed discovery.ServiceEvidence) (bool, error) {
	var command *exec.Cmd
	switch observed.Backend {
	case service.BackendSystemdUser:
		command = exec.CommandContext(ctx, "systemctl", "--user", "is-active", "--quiet", observed.Identity)
	case service.BackendLaunchd:
		command = exec.CommandContext(ctx, "launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/"+observed.Identity)
	default:
		return false, pmuxerr.New(pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "The adopted service backend cannot be replaced by native hardening.")
	}
	err := command.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return false, nil
	}
	return false, pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "PMux could not inspect the adopted service state before hardening.")
}

func adoptedServiceStop(ctx context.Context, observed discovery.ServiceEvidence) error {
	var command *exec.Cmd
	switch observed.Backend {
	case service.BackendSystemdUser:
		command = exec.CommandContext(ctx, "systemctl", "--user", "stop", observed.Identity)
	case service.BackendLaunchd:
		command = exec.CommandContext(ctx, "launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+observed.Identity)
	default:
		return pmuxerr.New(pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "The adopted service backend cannot be stopped safely.")
	}
	if output, err := command.CombinedOutput(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "PMux could not stop the adopted service before replacement: "+safeCommandDetail(output))
	}
	return nil
}

func adoptedServiceReload(ctx context.Context, observed discovery.ServiceEvidence) error {
	if observed.Backend != service.BackendSystemdUser {
		return nil
	}
	if output, err := exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "PMux could not reload the systemd user manager after service replacement: "+safeCommandDetail(output))
	}
	return nil
}

func adoptedServiceStart(ctx context.Context, observed discovery.ServiceEvidence) error {
	var command *exec.Cmd
	switch observed.Backend {
	case service.BackendSystemdUser:
		command = exec.CommandContext(ctx, "systemctl", "--user", "start", observed.Identity)
	case service.BackendLaunchd:
		command = exec.CommandContext(ctx, "launchctl", "bootstrap", "gui/"+strconv.Itoa(os.Getuid()), observed.Definition)
	default:
		return pmuxerr.New(pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "The adopted service backend cannot be restored safely.")
	}
	if output, err := command.CombinedOutput(); err != nil {
		return pmuxerr.Wrap(err, pmuxerr.ServiceStartFailed, pmuxerr.Environment, "PMux could not restore the adopted service after rollback: "+safeCommandDetail(output))
	}
	return nil
}

func safeCommandDetail([]byte) string {
	return "native command returned a nonzero status"
}
