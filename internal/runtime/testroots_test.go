package runtime

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"time"

	domainplatform "github.com/0p9b/pmux/internal/domain/platform"
	"github.com/0p9b/pmux/internal/domain/service"
)

func testRoots(root string) domainplatform.Roots {
	return domainplatform.Roots{
		Config: filepath.Join(root, "config"),
		State:  filepath.Join(root, "state"),
		Cache:  filepath.Join(root, "cache"),
		Data:   filepath.Join(root, "data"),
	}
}

func normalizeTestPath(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return path
}

type hardeningService struct {
	installed  service.ServiceSpec
	restarts   int
	restartErr error
}

func (*hardeningService) Backend() service.ServiceBackend { return service.BackendForeground }
func (*hardeningService) Detect(context.Context) (service.ServiceStatus, error) {
	return service.ServiceStatus{Backend: service.BackendForeground, State: service.ServiceStopped}, nil
}
func (s *hardeningService) Install(_ context.Context, spec service.ServiceSpec) error {
	s.installed = spec
	return nil
}
func (*hardeningService) Uninstall(context.Context) error           { return nil }
func (*hardeningService) Start(context.Context) error               { return nil }
func (*hardeningService) Stop(context.Context, time.Duration) error { return nil }
func (s *hardeningService) Restart(context.Context) (service.ServiceStatus, error) {
	s.restarts++
	if s.restartErr != nil {
		return service.ServiceStatus{}, s.restartErr
	}
	return service.ServiceStatus{Backend: service.BackendForeground, State: service.ServiceRunning, Healthy: true}, nil
}
func (*hardeningService) Status(context.Context) (service.ServiceStatus, error) {
	return service.ServiceStatus{Backend: service.BackendForeground, State: service.ServiceRunning, Healthy: true}, nil
}
func (*hardeningService) Logs(context.Context, int, bool) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
