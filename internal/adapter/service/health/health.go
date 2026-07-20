package health

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/0p9b/pmux/internal/pmuxerr"
)

const (
	DefaultInterval       = time.Second
	DefaultDeadline       = 15 * time.Second
	DefaultRequestTimeout = 2 * time.Second
	UnknownVersion        = "unknown"
	UnknownVersionWarning = "CLIProxyAPI is healthy, but X-CPA-VERSION was not returned; version is unknown."
)

// Result is the verified result of a successful /healthz request.
type Result struct {
	Version string
	Warning string
}

// Checker verifies lifecycle readiness using the canonical PMux health policy.
type Checker interface {
	WaitReady(ctx context.Context) (Result, error)
}

// Probe performs one bounded health request. Poller supplies the request context.
type Probe interface {
	Probe(ctx context.Context) (Result, error)
}

// Clock makes cadence and deadline behavior deterministic in tests.
type Clock interface {
	Now() time.Time
	Wait(ctx context.Context, duration time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Poller performs an immediate probe, then at most one probe per Interval until
// Deadline has elapsed. Requests never overlap.
type Poller struct {
	Probe          Probe
	Clock          Clock
	Interval       time.Duration
	Deadline       time.Duration
	RequestTimeout time.Duration
}

func NewPoller(probe Probe) *Poller {
	return &Poller{
		Probe:          probe,
		Clock:          realClock{},
		Interval:       DefaultInterval,
		Deadline:       DefaultDeadline,
		RequestTimeout: DefaultRequestTimeout,
	}
}

func (p *Poller) WaitReady(ctx context.Context) (Result, error) {
	if p == nil || p.Probe == nil {
		return Result{}, pmuxerr.New(pmuxerr.ServiceBackendUnavailable, pmuxerr.Internal, "service health checker is not configured")
	}
	clock := p.Clock
	if clock == nil {
		clock = realClock{}
	}
	interval := p.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	deadlineDuration := p.Deadline
	if deadlineDuration <= 0 {
		deadlineDuration = DefaultDeadline
	}
	requestTimeout := p.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = DefaultRequestTimeout
	}

	deadline := clock.Now().Add(deadlineDuration)
	var lastErr error
	for {
		remaining := deadline.Sub(clock.Now())
		if remaining <= 0 {
			break
		}
		probeTimeout := requestTimeout
		if remaining < probeTimeout {
			probeTimeout = remaining
		}
		requestCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		result, err := p.Probe.Probe(requestCtx)
		cancel()
		if err == nil {
			if result.Version == "" {
				result.Version = UnknownVersion
				result.Warning = UnknownVersionWarning
			}
			return result, nil
		}
		lastErr = err

		now := clock.Now()
		if !now.Before(deadline) || !now.Add(interval).Before(deadline) {
			break
		}
		if err := clock.Wait(ctx, interval); err != nil {
			return Result{}, pmuxerr.Wrap(err, pmuxerr.ServiceHealthDeadline, pmuxerr.Environment, "CLIProxyAPI health verification was interrupted")
		}
	}

	message := fmt.Sprintf("CLIProxyAPI did not become healthy within %s", deadlineDuration)
	return Result{}, &pmuxerr.Error{
		Code:        pmuxerr.ServiceHealthDeadline,
		Class:       pmuxerr.Environment,
		Message:     message,
		Explanation: "the service process did not return HTTP 200 from /healthz before the lifecycle deadline",
		Repair:      []string{"Run `pmux doctor` to diagnose the service."},
		Cause:       lastErr,
	}
}

// HTTPProbe checks a CLIProxyAPI /healthz endpoint. BaseURL may contain a path;
// /healthz is appended after trimming its trailing slash.
type HTTPProbe struct {
	BaseURL string
	Client  *http.Client
}

func (p HTTPProbe) Probe(ctx context.Context) (Result, error) {
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.BaseURL, "/")+"/healthz", nil)
	if err != nil {
		return Result{}, pmuxerr.Wrap(err, pmuxerr.ConfigValidationFailed, pmuxerr.Internal, "could not construct CLIProxyAPI health request")
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("health endpoint returned HTTP %d", resp.StatusCode)
	}
	version := strings.TrimSpace(resp.Header.Get("X-CPA-VERSION"))
	if version == "" {
		return Result{Version: UnknownVersion, Warning: UnknownVersionWarning}, nil
	}
	return Result{Version: version}, nil
}
