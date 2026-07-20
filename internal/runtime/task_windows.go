//go:build windows

package runtime

import (
	"github.com/0p9b/pmux/internal/adapter/service/health"
	"github.com/0p9b/pmux/internal/adapter/service/windowstask"
	domainplatform "github.com/0p9b/pmux/internal/domain/platform"
	"github.com/0p9b/pmux/internal/domain/service"
)

func newWindowsTaskManager(spec service.ServiceSpec, platform domainplatform.Platform, checker health.Checker) (service.ServiceManager, error) {
	return windowstask.New(spec, windowstask.NewNativeCOM(), platform, windowstask.NewNativeLogReader(), checker)
}
