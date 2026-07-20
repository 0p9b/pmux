//go:build !windows

package runtime

import (
	"github.com/0p9b/pmux/internal/adapter/service/health"
	domainplatform "github.com/0p9b/pmux/internal/domain/platform"
	"github.com/0p9b/pmux/internal/domain/service"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

func newWindowsTaskManager(service.ServiceSpec, domainplatform.Platform, health.Checker) (service.ServiceManager, error) {
	return nil, pmuxerr.New(pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "Windows Task Scheduler is available only in Windows builds.")
}
