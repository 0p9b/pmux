//go:build windows

package runtime

import (
	"context"

	"github.com/0p9b/pmux/internal/adapter/discovery"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

type nativeAdoptedServiceCutover struct{}

func (nativeAdoptedServiceCutover) Replace(context.Context, discovery.ServiceEvidence) (func(context.Context) error, error) {
	return nil, pmuxerr.New(pmuxerr.ServiceBackendUnavailable, pmuxerr.Environment, "Replacing a foreign adopted Windows task is unavailable; PMux will not report hardening success without a Task Scheduler ownership transfer.")
}
