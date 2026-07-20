package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeClock struct {
	now   time.Time
	waits []time.Duration
}

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Wait(_ context.Context, duration time.Duration) error {
	c.waits = append(c.waits, duration)
	c.now = c.now.Add(duration)
	return nil
}

type sequenceProbe struct {
	calls     int
	succeedAt int
	deadlines []time.Duration
}

func (p *sequenceProbe) Probe(ctx context.Context) (Result, error) {
	p.calls++
	if deadline, ok := ctx.Deadline(); ok {
		p.deadlines = append(p.deadlines, time.Until(deadline))
	}
	if p.calls == p.succeedAt {
		return Result{Version: "7.2.92"}, nil
	}
	return Result{}, errors.New("not ready")
}

func TestPollerUsesCanonicalCadenceAndRequestTimeout(t *testing.T) {
	clock := &fakeClock{now: time.Unix(0, 0)}
	probe := &sequenceProbe{succeedAt: 15}
	poller := &Poller{Probe: probe, Clock: clock, Interval: time.Second, Deadline: 15 * time.Second, RequestTimeout: 2 * time.Second}

	result, err := poller.WaitReady(context.Background())
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if result.Version != "7.2.92" {
		t.Fatalf("version = %q", result.Version)
	}
	if probe.calls != 15 {
		t.Fatalf("probe calls = %d, want 15 (t=0 through t=14)", probe.calls)
	}
	if len(clock.waits) != 14 {
		t.Fatalf("wait count = %d, want 14", len(clock.waits))
	}
	for _, wait := range clock.waits {
		if wait != time.Second {
			t.Fatalf("wait = %s, want 1s", wait)
		}
	}
	for _, deadline := range probe.deadlines {
		if deadline <= 0 || deadline > 2*time.Second {
			t.Fatalf("request deadline = %s, want (0,2s]", deadline)
		}
	}
}

func TestHTTPProbeAcceptsMissingVersionHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	result, err := (HTTPProbe{BaseURL: server.URL, Client: server.Client()}).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.Version != UnknownVersion || result.Warning != UnknownVersionWarning {
		t.Fatalf("result = %#v", result)
	}
}

func TestHTTPProbeCapturesVersionHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-CPA-VERSION", "7.2.91")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	result, err := (HTTPProbe{BaseURL: server.URL, Client: server.Client()}).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.Version != "7.2.91" || result.Warning != "" {
		t.Fatalf("result = %#v", result)
	}
}
