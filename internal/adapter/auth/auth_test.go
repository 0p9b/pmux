package auth

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0p9b/pmux/internal/domain/management"
	"github.com/0p9b/pmux/internal/domain/provider"
	"github.com/0p9b/pmux/internal/pmuxerr"
)

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)}
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Sleep(ctx context.Context, duration time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	clock.mu.Lock()
	clock.sleeps = append(clock.sleeps, duration)
	clock.now = clock.now.Add(duration)
	clock.mu.Unlock()
	return nil
}

func (clock *fakeClock) advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	clock.mu.Unlock()
}

type fakeManagement struct {
	management.ManagementClient

	challenge            management.OAuthChallenge
	beginErr             error
	beginProvider        management.ProviderID
	beginWebUI           bool
	status               management.OAuthStatus
	statusErr            error
	statusCalls          int
	callback             string
	callbackErr          error
	cancelStates         []string
	authResponses        [][]management.AuthFile
	authCalls            int
	providerKeys         []management.ProviderKey
	providerKeyErr       error
	putKind              management.ProviderKeyKind
	putValues            []management.ProviderKey
	putErr               error
	importRequest        management.VertexImportRequest
	importResult         management.VertexImportResult
	importErr            error
	providerKeyResponses [][]management.ProviderKey
	providerKeyErrors    []error
	providerKeyCalls     int
	deleteIDs            []string
	deleteErr            error
}

func (fake *fakeManagement) BeginOAuth(_ context.Context, providerID management.ProviderID, webUI bool) (management.OAuthChallenge, error) {
	fake.beginProvider = providerID
	fake.beginWebUI = webUI
	return fake.challenge, fake.beginErr
}

func (fake *fakeManagement) OAuthStatus(_ context.Context, state string) (management.OAuthStatus, error) {
	fake.statusCalls++
	result := fake.status
	result.State = state
	return result, fake.statusErr
}

func (fake *fakeManagement) SubmitOAuthCallback(_ context.Context, callbackURL string) error {
	fake.callback = callbackURL
	return fake.callbackErr
}

func (fake *fakeManagement) CancelOAuth(_ context.Context, state string) error {
	fake.cancelStates = append(fake.cancelStates, state)
	return nil
}

func (fake *fakeManagement) AuthFiles(context.Context) ([]management.AuthFile, error) {
	if len(fake.authResponses) == 0 {
		return nil, nil
	}
	index := fake.authCalls
	if index >= len(fake.authResponses) {
		index = len(fake.authResponses) - 1
	}
	fake.authCalls++
	return append([]management.AuthFile(nil), fake.authResponses[index]...), nil
}

func (fake *fakeManagement) ProviderKeys(_ context.Context, _ management.ProviderKeyKind) ([]management.ProviderKey, error) {
	index := fake.providerKeyCalls
	fake.providerKeyCalls++
	if index < len(fake.providerKeyErrors) && fake.providerKeyErrors[index] != nil {
		return nil, fake.providerKeyErrors[index]
	}
	if len(fake.providerKeyResponses) > 0 {
		if index >= len(fake.providerKeyResponses) {
			index = len(fake.providerKeyResponses) - 1
		}
		return cloneFakeProviderKeys(fake.providerKeyResponses[index]), nil
	}
	if fake.providerKeyErr != nil {
		return nil, fake.providerKeyErr
	}
	if fake.putValues != nil {
		return cloneFakeProviderKeys(fake.putValues), nil
	}
	return cloneFakeProviderKeys(fake.providerKeys), nil
}

func (fake *fakeManagement) CreateProviderKey(ctx context.Context, kind management.ProviderKeyKind, value management.ProviderKey) (management.ProviderKey, error) {
	fake.putKind = kind
	if fake.putErr != nil {
		return management.ProviderKey{}, fake.putErr
	}
	fake.putValues = append(cloneFakeProviderKeys(fake.providerKeys), cloneFakeProviderKeys([]management.ProviderKey{value})...)
	observed, err := fake.ProviderKeys(ctx, kind)
	if err == nil {
		for _, entry := range observed {
			if entry.ID == value.ID {
				return value, nil
			}
		}
	}
	if deleteErr := fake.DeleteProviderKey(ctx, kind, value.ID); deleteErr != nil {
		return management.ProviderKey{}, pmuxerr.Wrap(deleteErr, pmuxerr.ConfigMutationConflict, pmuxerr.Upstream, "Provider-key verification failed and the new entry could not be removed.")
	}
	if err != nil {
		return management.ProviderKey{}, err
	}
	return management.ProviderKey{}, pmuxerr.New(pmuxerr.OAuthNoUsableCredential, pmuxerr.Upstream, "Provider-key verification failed; the new entry was removed.")
}

func (fake *fakeManagement) PatchProviderKeys(_ context.Context, kind management.ProviderKeyKind, patch management.ProviderKeyPatch) error {
	fake.putKind = kind
	if fake.putErr != nil {
		return fake.putErr
	}
	var value management.ProviderKey
	if err := json.Unmarshal(patch, &value); err != nil {
		return err
	}
	fake.putValues = append(cloneFakeProviderKeys(fake.providerKeys), value)
	return nil
}
func (fake *fakeManagement) DeleteProviderKey(_ context.Context, _ management.ProviderKeyKind, id string) error {
	fake.deleteIDs = append(fake.deleteIDs, id)
	if fake.deleteErr != nil {
		return fake.deleteErr
	}
	filtered := fake.putValues[:0]
	for _, entry := range fake.putValues {
		if entry.ID != id {
			filtered = append(filtered, entry)
		}
	}
	fake.putValues = filtered
	return nil
}

func (fake *fakeManagement) ImportVertex(_ context.Context, request management.VertexImportRequest) (management.VertexImportResult, error) {
	fake.importRequest = request
	return fake.importResult, fake.importErr
}

func cloneFakeProviderKeys(keys []management.ProviderKey) []management.ProviderKey {
	cloned := make([]management.ProviderKey, len(keys))
	for index, key := range keys {
		cloned[index] = key
		cloned[index].Fields = make(map[string]string, len(key.Fields))
		for field, value := range key.Fields {
			cloned[index].Fields[field] = value
		}
	}
	return cloned
}

type fakeFallback struct {
	provider management.ProviderID
	flow     provider.AuthFlow
	flags    []string
	err      error
}

func (fallback *fakeFallback) RunAuth(_ context.Context, providerID management.ProviderID, flow provider.AuthFlow, flags []string) error {
	fallback.provider = providerID
	fallback.flow = flow
	fallback.flags = append([]string(nil), flags...)
	return fallback.err
}

type fakeVerifier struct {
	before      CredentialSnapshot
	snapshotErr error
	credential  management.AuthFile
	verifyErr   error
	verified    bool
}

func (verifier *fakeVerifier) Snapshot(context.Context, management.ProviderID) (CredentialSnapshot, error) {
	return cloneSnapshot(verifier.before), verifier.snapshotErr
}

func (verifier *fakeVerifier) Verify(_ context.Context, _ management.ProviderID, before CredentialSnapshot) (management.AuthFile, error) {
	verifier.verified = true
	if !reflect.DeepEqual(before, verifier.before) {
		return management.AuthFile{}, errors.New("snapshot handoff changed")
	}
	return verifier.credential, verifier.verifyErr
}

type protectedBytes struct {
	value []byte
	err   error
}

func (input *protectedBytes) ReadSecret(context.Context) ([]byte, error) {
	return input.value, input.err
}

func TestEveryProviderDefinitionConstructsWithExactFlows(t *testing.T) {
	for _, definition := range provider.Registry() {
		definition := definition
		t.Run(string(definition.ID), func(t *testing.T) {
			authenticator, err := New(definition.ID, &fakeManagement{}, nil, &fakeVerifier{})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if authenticator.Provider() != definition.ID {
				t.Fatalf("provider = %q, want %q", authenticator.Provider(), definition.ID)
			}
			if !reflect.DeepEqual(authenticator.Flows(), definition.Flows) {
				t.Fatalf("flows = %#v, want %#v", authenticator.Flows(), definition.Flows)
			}
		})
	}
}

func TestManagementFirstCallbackSuccessAndCredentialHandoff(t *testing.T) {
	for _, providerID := range []management.ProviderID{"codex", "claude", "antigravity"} {
		providerID := providerID
		t.Run(string(providerID), func(t *testing.T) {
			clock := newFakeClock()
			fake := &fakeManagement{
				challenge: management.OAuthChallenge{State: "state-1", URL: "https://provider.example/authorize"},
				status:    management.OAuthStatus{Status: "complete"},
				authResponses: [][]management.AuthFile{
					{},
					{{Name: "credential.json", Provider: providerID, Status: "ready"}},
				},
			}
			authenticator, err := New(providerID, fake, nil, nil, WithClock(clock))
			if err != nil {
				t.Fatal(err)
			}
			session, err := authenticator.Begin(context.Background(), provider.FlowBrowser)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			status, err := authenticator.Poll(context.Background(), session)
			if err != nil {
				t.Fatalf("Poll: %v", err)
			}
			if status.Status != "complete" || fake.authCalls != 2 {
				t.Fatalf("status=%#v auth calls=%d", status, fake.authCalls)
			}
			if len(clock.sleeps) != 1 || clock.sleeps[0] < time.Second {
				t.Fatalf("poll sleeps = %#v; polling must be at least one second", clock.sleeps)
			}
			if fake.beginProvider != providerID || !fake.beginWebUI {
				t.Fatalf("management BeginOAuth provider=%q webUI=%v", fake.beginProvider, fake.beginWebUI)
			}
		})
	}
}

func TestManagementFirstDeviceSuccessForEveryDeviceProvider(t *testing.T) {
	for _, providerID := range []management.ProviderID{"codex", "kimi", "xai"} {
		providerID := providerID
		t.Run(string(providerID), func(t *testing.T) {
			clock := newFakeClock()
			canary := "DEVICE-CODE-CANARY"
			verifier := &fakeVerifier{credential: management.AuthFile{Name: "new.json", Provider: providerID}}
			fake := &fakeManagement{
				challenge: management.OAuthChallenge{State: "device-state", VerificationURI: "https://verify.example/", UserCode: canary, Interval: 50 * time.Millisecond},
				status:    management.OAuthStatus{Status: "authenticated", Message: "authorized " + canary},
			}
			authenticator, err := New(providerID, fake, nil, verifier, WithClock(clock))
			if err != nil {
				t.Fatal(err)
			}
			session, err := authenticator.Begin(context.Background(), provider.FlowDeviceCode)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if session.Flow != provider.FlowDeviceCode || fake.beginWebUI {
				t.Fatalf("flow=%q webUI=%v", session.Flow, fake.beginWebUI)
			}
			status, err := authenticator.Poll(context.Background(), session)
			if err != nil {
				t.Fatalf("Poll: %v", err)
			}
			if strings.Contains(status.Message, canary) || strings.Contains(status.State, canary) {
				t.Fatal("terminal status leaked device code")
			}
			if !verifier.verified {
				t.Fatal("credential verification handoff was not called")
			}
		})
	}
}

func TestHeadlessPasteCallbackDoesNotExposeCallback(t *testing.T) {
	const callbackCanary = "CALLBACK-SECRET-CANARY"
	clock := newFakeClock()
	fake := &fakeManagement{
		challenge: management.OAuthChallenge{State: "paste-state", URL: "https://provider.example/authorize"},
		status:    management.OAuthStatus{Status: "complete", Message: callbackCanary},
	}
	verifier := &fakeVerifier{credential: management.AuthFile{Name: "credential.json", Provider: "claude"}}
	authenticator, err := New("claude", fake, nil, verifier, WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	session, err := authenticator.Begin(context.Background(), provider.FlowPasteCallback)
	if err != nil {
		t.Fatal(err)
	}
	callback := "http://127.0.0.1:54545/callback?state=paste-state&code=" + callbackCanary
	status, err := authenticator.CompletePaste(context.Background(), session, callback)
	if err != nil {
		t.Fatalf("CompletePaste: %v", err)
	}
	if fake.callback != callback {
		t.Fatal("callback was not forwarded exactly to the management client")
	}
	if strings.Contains(status.Message, callbackCanary) || strings.Contains(status.State, callbackCanary) {
		t.Fatal("terminal status leaked callback data")
	}
}

func TestCallbackValidationFailsClosedWithoutForwarding(t *testing.T) {
	fake := &fakeManagement{challenge: management.OAuthChallenge{State: "expected", URL: "https://provider.example/authorize"}}
	authenticator, err := New("claude", fake, nil, &fakeVerifier{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := authenticator.Begin(context.Background(), provider.FlowPasteCallback)
	if err != nil {
		t.Fatal(err)
	}
	for _, callback := range []string{
		"https://attacker.example/callback?state=expected",
		"http://127.0.0.1:54545/wrong?state=expected",
		"http://127.0.0.1:54545/callback?state=wrong",
	} {
		_, err := authenticator.CompletePaste(context.Background(), session, callback)
		if err == nil {
			t.Fatalf("callback %q was accepted", callback)
		}
		if strings.Contains(err.Error(), callback) {
			t.Fatalf("error leaked callback URL: %v", err)
		}
	}
	if fake.callback != "" {
		t.Fatal("invalid callback reached management client")
	}
}

func TestXAIShapeDiscrimination(t *testing.T) {
	tests := []struct {
		name      string
		challenge management.OAuthChallenge
		wantFlow  provider.AuthFlow
		wantErr   bool
	}{
		{name: "device", challenge: management.OAuthChallenge{State: "x-device", VerificationURI: "https://x.ai/device", UserCode: "ABCD-EFGH"}, wantFlow: provider.FlowDeviceCode},
		{name: "callback", challenge: management.OAuthChallenge{State: "x-callback", URL: "https://x.ai/oauth"}, wantFlow: provider.FlowPasteCallback},
		{name: "unknown empty", challenge: management.OAuthChallenge{State: "x-unknown"}, wantErr: true},
		{name: "unknown mixed", challenge: management.OAuthChallenge{State: "x-mixed", URL: "https://x.ai/oauth", VerificationURI: "https://x.ai/device", UserCode: "ABCD"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeManagement{challenge: test.challenge}
			authenticator, err := New("xai", fake, nil, &fakeVerifier{})
			if err != nil {
				t.Fatal(err)
			}
			session, err := authenticator.Begin(context.Background(), provider.FlowDeviceCode)
			if test.wantErr {
				if err == nil {
					t.Fatal("unknown xAI shape was accepted")
				}
				var structured *pmuxerr.Error
				if !errors.As(err, &structured) || structured.Code != pmuxerr.UnhandledUpstreamShape {
					t.Fatalf("error = %#v", err)
				}
				if len(fake.cancelStates) != 1 || fake.cancelStates[0] != test.challenge.State {
					t.Fatalf("unknown xAI session cleanup = %#v", fake.cancelStates)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if session.Flow != test.wantFlow {
				t.Fatalf("flow=%q want=%q", session.Flow, test.wantFlow)
			}
		})
	}
}

func TestCancelAndFiveMinuteTimeoutCleanUp(t *testing.T) {
	cancelCases := []struct {
		providerID management.ProviderID
		flow       provider.AuthFlow
		challenge  management.OAuthChallenge
	}{
		{providerID: "codex", flow: provider.FlowBrowser, challenge: management.OAuthChallenge{State: "codex-cancel", URL: "https://provider.example/auth"}},
		{providerID: "claude", flow: provider.FlowBrowser, challenge: management.OAuthChallenge{State: "claude-cancel", URL: "https://provider.example/auth"}},
		{providerID: "antigravity", flow: provider.FlowBrowser, challenge: management.OAuthChallenge{State: "antigravity-cancel", URL: "https://provider.example/auth"}},
		{providerID: "kimi", flow: provider.FlowDeviceCode, challenge: management.OAuthChallenge{State: "kimi-cancel", VerificationURI: "https://verify.example", UserCode: "KIMI"}},
		{providerID: "xai", flow: provider.FlowDeviceCode, challenge: management.OAuthChallenge{State: "xai-cancel", VerificationURI: "https://verify.example", UserCode: "XAI"}},
	}
	for _, test := range cancelCases {
		test := test
		t.Run("cancel "+string(test.providerID), func(t *testing.T) {
			fake := &fakeManagement{challenge: test.challenge}
			authenticator, err := New(test.providerID, fake, nil, &fakeVerifier{})
			if err != nil {
				t.Fatal(err)
			}
			session, err := authenticator.Begin(context.Background(), test.flow)
			if err != nil {
				t.Fatal(err)
			}
			if err := authenticator.Cancel(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fake.cancelStates, []string{test.challenge.State}) {
				t.Fatalf("cancel cleanup count=%d", len(fake.cancelStates))
			}
			if _, err := authenticator.Poll(context.Background(), session); err == nil {
				t.Fatal("canceled session remained pollable")
			}
		})
	}

	t.Run("timeout", func(t *testing.T) {
		clock := newFakeClock()
		fake := &fakeManagement{challenge: management.OAuthChallenge{State: "timeout-state", VerificationURI: "https://verify.example", UserCode: "CODE"}}
		authenticator, err := New("kimi", fake, nil, &fakeVerifier{}, WithClock(clock))
		if err != nil {
			t.Fatal(err)
		}
		session, err := authenticator.Begin(context.Background(), provider.FlowDeviceCode)
		if err != nil {
			t.Fatal(err)
		}
		clock.advance(5 * time.Minute)
		_, err = authenticator.Poll(context.Background(), session)
		var structured *pmuxerr.Error
		if !errors.As(err, &structured) || structured.Code != pmuxerr.OAuthTimeout {
			t.Fatalf("timeout error=%#v", err)
		}
		if !reflect.DeepEqual(fake.cancelStates, []string{"timeout-state"}) {
			t.Fatalf("timeout cleanup=%#v", fake.cancelStates)
		}
		if fake.statusCalls != 0 {
			t.Fatalf("status was polled after budget: %d", fake.statusCalls)
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		clock := newFakeClock()
		fake := &fakeManagement{challenge: management.OAuthChallenge{State: "context-state", URL: "https://provider.example/auth"}}
		authenticator, err := New("codex", fake, nil, &fakeVerifier{}, WithClock(clock))
		if err != nil {
			t.Fatal(err)
		}
		session, err := authenticator.Begin(context.Background(), provider.FlowBrowser)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = authenticator.Poll(ctx, session)
		var structured *pmuxerr.Error
		if !errors.As(err, &structured) || structured.Code != pmuxerr.CodeCanceled {
			t.Fatalf("cancellation error=%#v", err)
		}
		if !reflect.DeepEqual(fake.cancelStates, []string{"context-state"}) {
			t.Fatalf("context cancellation cleanup=%#v", fake.cancelStates)
		}
	})
}

func TestClosedFallbackFlagsExactForEveryDefinition(t *testing.T) {
	tests := []struct {
		provider management.ProviderID
		flow     provider.AuthFlow
		options  FallbackOptions
		want     []string
	}{
		{provider: "codex", flow: provider.FlowBrowser, want: []string{"-codex-login"}},
		{provider: "codex", flow: provider.FlowPasteCallback, options: FallbackOptions{Headless: true}, want: []string{"-codex-login", "-no-browser"}},
		{provider: "codex", flow: provider.FlowDeviceCode, want: []string{"-codex-device-login"}},
		{provider: "claude", flow: provider.FlowBrowser, want: []string{"-claude-login"}},
		{provider: "antigravity", flow: provider.FlowBrowser, want: []string{"-antigravity-login"}},
		{provider: "kimi", flow: provider.FlowDeviceCode, want: []string{"-kimi-login"}},
		{provider: "xai", flow: provider.FlowDeviceCode, want: []string{"-xai-login"}},
		{provider: "vertex", flow: provider.FlowVertexImport, options: FallbackOptions{VertexPath: absTestPath("/private/vertex.json")}, want: []string{"-vertex-import", absTestPath("/private/vertex.json")}},
		{provider: "vertex", flow: provider.FlowVertexImport, options: FallbackOptions{VertexPath: absTestPath("/private/vertex.json"), VertexPrefix: "team"}, want: []string{"-vertex-import", absTestPath("/private/vertex.json"), "-vertex-import-prefix", "team"}},
	}
	for _, test := range tests {
		flags, err := ClosedFallbackFlags(test.provider, test.flow, test.options)
		if err != nil {
			t.Fatalf("%s/%s: %v", test.provider, test.flow, err)
		}
		if !reflect.DeepEqual(flags, test.want) {
			t.Fatalf("%s/%s flags=%#v want=%#v", test.provider, test.flow, flags, test.want)
		}
	}
	if _, err := ClosedFallbackFlags("vertex", provider.FlowVertexImport, FallbackOptions{VertexPath: "relative.json"}); err == nil {
		t.Fatal("relative Vertex fallback path was accepted")
	}
	for _, definition := range provider.Registry() {
		for _, flow := range definition.Flows {
			_, mapped := definition.SubprocessFlags[flow]
			if mapped || flow == provider.FlowVertexImport {
				continue
			}
			if _, err := ClosedFallbackFlags(definition.ID, flow, FallbackOptions{}); err == nil {
				t.Fatalf("%s/%s unexpectedly gained a fallback", definition.ID, flow)
			}
		}
	}
}

func TestFallbackRunsOnlyForUnavailableManagementAndVerifiesCredential(t *testing.T) {
	verifier := &fakeVerifier{credential: management.AuthFile{Name: "new.json", Provider: "claude"}}
	fallback := &fakeFallback{}
	fake := &fakeManagement{beginErr: ErrManagementUnavailable}
	authenticator, err := New("claude", fake, fallback, verifier)
	if err != nil {
		t.Fatal(err)
	}
	session, err := authenticator.Begin(context.Background(), provider.FlowPasteCallback)
	if err != nil {
		t.Fatal(err)
	}
	if fallback.provider != "claude" || fallback.flow != provider.FlowBrowser || !reflect.DeepEqual(fallback.flags, []string{"-claude-login", "-no-browser"}) {
		t.Fatalf("fallback call provider=%q flow=%q flags=%#v", fallback.provider, fallback.flow, fallback.flags)
	}
	if !verifier.verified || session.Flow != provider.FlowSubprocess {
		t.Fatalf("verified=%v session=%#v", verifier.verified, session)
	}
	status, err := authenticator.Poll(context.Background(), session)
	if err != nil || status.Status != "complete" {
		t.Fatalf("fallback terminal status=%#v err=%v", status, err)
	}

	missingFallback := &fakeFallback{}
	missingFake := &fakeManagement{beginErr: &pmuxerr.Error{Code: pmuxerr.UnhandledUpstreamShape, Class: pmuxerr.Upstream, Explanation: "Individual endpoint returned HTTP 404."}}
	missingAuth, err := New("claude", missingFake, missingFallback, &fakeVerifier{credential: management.AuthFile{Name: "new.json", Provider: "claude"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missingAuth.Begin(context.Background(), provider.FlowBrowser); err != nil {
		t.Fatalf("missing management endpoint did not use fallback: %v", err)
	}
	if !reflect.DeepEqual(missingFallback.flags, []string{"-claude-login"}) {
		t.Fatalf("missing endpoint fallback flags=%#v", missingFallback.flags)
	}

	networkFallback := &fakeFallback{}
	networkFake := &fakeManagement{beginErr: errors.New("network down")}
	networkAuth, err := New("claude", networkFake, networkFallback, &fakeVerifier{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := networkAuth.Begin(context.Background(), provider.FlowBrowser); err == nil {
		t.Fatal("network error unexpectedly fell back")
	}
	if len(networkFallback.flags) != 0 {
		t.Fatalf("fallback ran for network failure: %#v", networkFallback.flags)
	}
}

func TestCredentialVerificationFailureIsTerminal(t *testing.T) {
	clock := newFakeClock()
	canary := "AUTH-FILE-CONTENT-CANARY"
	fake := &fakeManagement{
		challenge: management.OAuthChallenge{State: "state", URL: "https://provider.example/auth"},
		status:    management.OAuthStatus{Status: "complete", Message: canary},
	}
	verifier := &fakeVerifier{verifyErr: errors.New(canary)}
	authenticator, err := New("codex", fake, nil, verifier, WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	session, err := authenticator.Begin(context.Background(), provider.FlowBrowser)
	if err != nil {
		t.Fatal(err)
	}
	_, err = authenticator.Poll(context.Background(), session)
	if err == nil || strings.Contains(err.Error(), canary) {
		t.Fatal("credential verification error leaked secret or was nil")
	}
	var structured *pmuxerr.Error
	if !errors.As(err, &structured) || structured.Code != pmuxerr.OAuthNoUsableCredential {
		t.Fatalf("error=%#v", err)
	}
}

func TestProtectedAPIKeyContractAndCanaryAbsence(t *testing.T) {
	const canary = "sk-API-KEY-CANARY-1234567890"
	for _, test := range []struct {
		provider management.ProviderID
		kind     management.ProviderKeyKind
	}{
		{provider: "gemini", kind: management.ProviderGemini},
		{provider: "codex", kind: management.ProviderCodex},
		{provider: "codex-compatible", kind: management.ProviderCodex},
		{provider: "claude", kind: management.ProviderClaude},
		{provider: "claude-compatible", kind: management.ProviderClaude},
		{provider: "xai", kind: management.ProviderXAI},
		{provider: "interactions", kind: management.ProviderInteractions},
		{provider: "vertex", kind: management.ProviderVertex},
		{provider: "openrouter", kind: management.ProviderOpenAICompatible},
		{provider: "openai-compatible", kind: management.ProviderOpenAICompatible},
	} {
		t.Run(string(test.provider), func(t *testing.T) {
			inputBytes := []byte("  " + canary + "\n")
			input := &protectedBytes{value: inputBytes}
			fake := &fakeManagement{}
			authenticator, err := New(test.provider, fake, nil, &fakeVerifier{})
			if err != nil {
				t.Fatal(err)
			}
			result, err := authenticator.ApplyAPIKey(context.Background(), APIKeyApplication{Input: input, Label: "test", Fields: map[string]string{"base-url": "https://api.example"}})
			if err != nil {
				t.Fatal(err)
			}
			if fake.putKind != test.kind || len(fake.putValues) != 1 || fake.putValues[0].Fields["api-key"] != canary {
				t.Fatal("protected API-key application did not reach the expected management resource")
			}
			if _, exists := result.Fields["api-key"]; exists || strings.Contains(result.Mask, canary) {
				t.Fatal("safe provider-key result leaked the key")
			}
			for _, value := range inputBytes {
				if value != 0 {
					t.Fatal("protected input buffer was not cleared")
				}
			}
		})
	}

	input := &protectedBytes{value: []byte(canary)}
	fake := &fakeManagement{putErr: errors.New("request failed with " + canary)}
	authenticator, err := New("gemini", fake, nil, &fakeVerifier{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = authenticator.ApplyAPIKey(context.Background(), APIKeyApplication{Input: input})
	if err == nil || strings.Contains(err.Error(), canary) {
		t.Fatal("secret-bearing management error escaped")
	}
	var structured *pmuxerr.Error
	if errors.As(err, &structured) && structured.Cause != nil && strings.Contains(structured.Cause.Error(), canary) {
		t.Fatal("secret-bearing verbose cause escaped")
	}
}

func TestProtectedAPIKeyVerificationFailureCompensatesNewEntry(t *testing.T) {
	const canary = "sk-rollback-canary-1234567890"
	fake := &fakeManagement{
		providerKeyResponses: [][]management.ProviderKey{{}, {}, {}},
	}
	authenticator, err := New("gemini", fake, nil, &fakeVerifier{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = authenticator.ApplyAPIKey(context.Background(), APIKeyApplication{Input: &protectedBytes{value: []byte(canary)}})
	if err == nil || len(fake.deleteIDs) != 1 || len(fake.putValues) != 0 {
		t.Fatalf("verification rollback: err=%v deletes=%#v values=%#v", err, fake.deleteIDs, fake.putValues)
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatal("verification rollback error disclosed API key")
	}
}

func TestProtectedAPIKeyRefusesLossyExistingEntryUpdate(t *testing.T) {
	fake := &fakeManagement{providerKeys: []management.ProviderKey{{ID: "existing", Mask: "********"}}}
	authenticator, err := New("codex", fake, nil, &fakeVerifier{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = authenticator.ApplyAPIKey(context.Background(), APIKeyApplication{
		ID:    "existing",
		Input: &protectedBytes{value: []byte("sk-replacement-canary-123456")},
	})
	if err == nil || fake.putValues != nil || len(fake.deleteIDs) != 0 {
		t.Fatalf("lossy existing update was not blocked before mutation: err=%v", err)
	}
}

func TestProtectedAPIKeyReportsCompensationFailureWithoutSecret(t *testing.T) {
	const canary = "sk-delete-failure-canary-123456"
	fake := &fakeManagement{
		providerKeyResponses: [][]management.ProviderKey{{}, {}},
		deleteErr:            errors.New("delete rejected " + canary),
	}
	authenticator, err := New("vertex", fake, nil, &fakeVerifier{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = authenticator.ApplyAPIKey(context.Background(), APIKeyApplication{Input: &protectedBytes{value: []byte(canary)}})
	if err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("unsafe compensation failure error: %v", err)
	}
	var typed *pmuxerr.Error
	if !errors.As(err, &typed) || typed.Code != pmuxerr.ConfigMutationConflict {
		t.Fatalf("compensation failure type = %#v", err)
	}
}

func TestVertexManagementFirstAndFallback(t *testing.T) {
	t.Run("management", func(t *testing.T) {
		verifier := &fakeVerifier{credential: management.AuthFile{Name: "vertex-project.json", Provider: "vertex"}}
		fake := &fakeManagement{importResult: management.VertexImportResult{Name: "vertex-project.json"}}
		authenticator, err := New("vertex", fake, &fakeFallback{}, verifier)
		if err != nil {
			t.Fatal(err)
		}
		path := absTestPath("/private/service-account.json")
		result, err := authenticator.ImportVertex(context.Background(), VertexImport{Path: path, Prefix: "team"})
		if err != nil {
			t.Fatal(err)
		}
		if result.Name != "vertex-project.json" || fake.importRequest.Path != path || fake.importRequest.Prefix != "team" || !verifier.verified {
			t.Fatalf("result=%#v request=%#v verified=%v", result, fake.importRequest, verifier.verified)
		}
	})

	t.Run("closed fallback", func(t *testing.T) {
		verifier := &fakeVerifier{credential: management.AuthFile{Name: "vertex-team-project.json", Provider: "vertex"}}
		fallback := &fakeFallback{}
		fake := &fakeManagement{importErr: ErrManagementUnavailable}
		authenticator, err := New("vertex", fake, fallback, verifier)
		if err != nil {
			t.Fatal(err)
		}
		path := absTestPath("/private/service-account.json")
		result, err := authenticator.ImportVertex(context.Background(), VertexImport{Path: path, Prefix: "team"})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"-vertex-import", path, "-vertex-import-prefix", "team"}
		if !reflect.DeepEqual(fallback.flags, want) || fallback.provider != "vertex" || fallback.flow != provider.FlowVertexImport {
			t.Fatalf("fallback provider=%q flow=%q flags=%#v", fallback.provider, fallback.flow, fallback.flags)
		}
		if result.Name != "vertex-team-project.json" {
			t.Fatalf("result=%#v", result)
		}
	})
}
func absTestPath(path string) string {
	abs, _ := filepath.Abs(path)
	return abs
}
