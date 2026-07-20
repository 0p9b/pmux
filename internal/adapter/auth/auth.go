package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/0p9b/pmux/internal/domain/management"
	"github.com/0p9b/pmux/internal/domain/provider"
	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/redact"
)

const (
	sessionBudget = 5 * time.Minute
	minimumPoll   = time.Second
	cleanupBudget = 2 * time.Second
)

// ErrManagementUnavailable is the only non-PMux error that authorizes the
// explicit subprocess fallback. Transport and authentication failures never do.
var ErrManagementUnavailable = errors.New("management authentication capability unavailable")

// FallbackRunner executes an already selected, closed-map CLIProxyAPI fallback.
// Args always come from ClosedFallbackFlags; implementations must use argv, not a
// shell command.
type FallbackRunner interface {
	RunAuth(context.Context, management.ProviderID, provider.AuthFlow, []string) error
}

// CredentialVerifier is the structured handoff used after OAuth, subprocess,
// and Vertex flows. Implementations may use Management API auth metadata or a
// filename-only auth-directory snapshot, but never auth-file contents.
type CredentialVerifier interface {
	Snapshot(context.Context, management.ProviderID) (CredentialSnapshot, error)
	Verify(context.Context, management.ProviderID, CredentialSnapshot) (management.AuthFile, error)
}

type CredentialSnapshot struct {
	Files []management.AuthFile
}

// These aliases keep the adapter API concise while the transport-neutral
// contracts remain owned by the provider domain.
type ProtectedInput = provider.ProtectedInput
type APIKeyApplication = provider.APIKeyApplication
type VertexImport = provider.VertexImport

type FallbackOptions struct {
	Headless     bool
	VertexPath   string
	VertexPrefix string
}

type Clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

type Option func(*Authenticator)

func WithClock(clock Clock) Option {
	return func(a *Authenticator) {
		if clock != nil {
			a.clock = clock
		}
	}
}

type Authenticator struct {
	definition provider.Definition
	management management.ManagementClient
	fallback   FallbackRunner
	verifier   CredentialVerifier
	clock      Clock

	mu       sync.Mutex
	sessions map[string]*activeSession
}
var _ provider.ProviderAuthenticator = (*Authenticator)(nil)


type activeSession struct {
	pollMu     sync.Mutex
	flow       provider.AuthFlow
	before     CredentialSnapshot
	deadline   time.Time
	lastPoll   time.Time
	interval   time.Duration
	fallbackOK bool
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func New(
	providerID management.ProviderID,
	client management.ManagementClient,
	fallback FallbackRunner,
	verifier CredentialVerifier,
	options ...Option,
) (*Authenticator, error) {
	definition, ok := definitionFor(providerID)
	if !ok {
		return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, fmt.Sprintf("Provider %q is not supported by mainline CLIProxyAPI.", providerID))
	}
	if client == nil {
		return nil, pmuxerr.New(pmuxerr.ManagementUnreachable, pmuxerr.Environment, "Management API client is not configured.")
	}
	if verifier == nil {
		verifier = &managementVerifier{client: client}
	}
	authenticator := &Authenticator{
		definition: definition,
		management: client,
		fallback:   fallback,
		verifier:   verifier,
		clock:      realClock{},
		sessions:   make(map[string]*activeSession),
	}
	for _, option := range options {
		option(authenticator)
	}
	return authenticator, nil
}

func (a *Authenticator) Provider() management.ProviderID { return a.definition.ID }

func (a *Authenticator) Flows() []provider.AuthFlow {
	return append([]provider.AuthFlow(nil), a.definition.Flows...)
}

func (a *Authenticator) Begin(ctx context.Context, flow provider.AuthFlow) (provider.AuthSession, error) {
	if !a.supportsOAuth(flow) {
		return provider.AuthSession{}, unsupportedFlow(a.definition.ID, flow)
	}
	startedAt := a.clock.Now()
	overallDeadline := startedAt.Add(sessionBudget)
	authContext, cancel := context.WithTimeout(ctx, sessionBudget)
	defer cancel()

	before, err := a.verifier.Snapshot(authContext, a.definition.ID)
	if err != nil {
		if managementFallbackAllowed(err) {
			return a.beginFallback(authContext, flow, CredentialSnapshot{}, flow == provider.FlowPasteCallback, overallDeadline)
		}
		if authContext.Err() != nil {
			return provider.AuthSession{}, authContextError(ctx)
		}
		return provider.AuthSession{}, safeWrap(err, pmuxerr.ManagementUnreachable, pmuxerr.Environment, "Could not read credential status before authentication.")
	}

	challenge, err := a.management.BeginOAuth(authContext, a.definition.ID, flow != provider.FlowDeviceCode)
	if err != nil {
		if managementFallbackAllowed(err) {
			return a.beginFallback(authContext, flow, before, flow == provider.FlowPasteCallback, overallDeadline)
		}
		if authContext.Err() != nil {
			return provider.AuthSession{}, authContextError(ctx)
		}
		return provider.AuthSession{}, safeWrap(err, pmuxerr.ProviderUnreachable, pmuxerr.Upstream, "Could not start provider authentication.")
	}

	actualFlow, err := discriminateChallenge(a.definition.ID, flow, challenge)
	if err != nil {
		if challenge.State != "" {
			a.cancelState(challenge.State)
		}
		return provider.AuthSession{}, err
	}

	now := a.clock.Now()
	deadline := overallDeadline
	if !challenge.ExpiresAt.IsZero() && challenge.ExpiresAt.Before(deadline) {
		deadline = challenge.ExpiresAt
	}
	interval := challenge.Interval
	if interval < minimumPoll {
		interval = minimumPoll
	}

	a.mu.Lock()
	a.sessions[challenge.State] = &activeSession{
		flow:     actualFlow,
		before:   cloneSnapshot(before),
		deadline: deadline,
		lastPoll: now,
		interval: interval,
	}
	a.mu.Unlock()

	return provider.AuthSession{Provider: a.definition.ID, Flow: actualFlow, Challenge: challenge}, nil
}

func (a *Authenticator) Poll(ctx context.Context, session provider.AuthSession) (management.OAuthStatus, error) {
	active, err := a.session(session)
	if err != nil {
		return management.OAuthStatus{}, err
	}
	active.pollMu.Lock()
	defer active.pollMu.Unlock()
	if active.fallbackOK {
		a.deleteSession(session.Challenge.State)
		return terminalStatus(session.Challenge.State), nil
	}

	if err := a.waitForPoll(ctx, session.Challenge.State, active); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			a.cancelState(session.Challenge.State)
			return management.OAuthStatus{}, pmuxerr.New(pmuxerr.CodeCanceled, pmuxerr.User, "Authentication was canceled; no active session remains.")
		}
		return management.OAuthStatus{}, err
	}
	return a.pollNow(ctx, session, active)
}

func (a *Authenticator) CompletePaste(ctx context.Context, session provider.AuthSession, callbackURL string) (management.OAuthStatus, error) {
	active, err := a.session(session)
	if err != nil {
		return management.OAuthStatus{}, err
	}
	active.pollMu.Lock()
	defer active.pollMu.Unlock()
	if active.flow != provider.FlowPasteCallback && active.flow != provider.FlowBrowser {
		return management.OAuthStatus{}, unsupportedFlow(a.definition.ID, provider.FlowPasteCallback)
	}
	if err := validateCallback(callbackURL, session.Challenge.State); err != nil {
		return management.OAuthStatus{}, err
	}
	callbackContext, cancel := a.operationContext(ctx, active)
	defer cancel()
	if err := a.management.SubmitOAuthCallback(callbackContext, callbackURL); err != nil {
		if callbackContext.Err() != nil {
			a.cancelState(session.Challenge.State)
			if ctx.Err() != nil {
				return management.OAuthStatus{}, pmuxerr.New(pmuxerr.CodeCanceled, pmuxerr.User, "Authentication was canceled; no active session remains.")
			}
			return management.OAuthStatus{}, pmuxerr.New(pmuxerr.OAuthTimeout, pmuxerr.Upstream, "Authentication timed out after five minutes; the session was canceled.")
		}
		return management.OAuthStatus{}, safeWrap(err, pmuxerr.ProviderUnreachable, pmuxerr.Upstream, "The provider callback could not be submitted.")
	}
	return a.pollNow(callbackContext, session, active)
}

func (a *Authenticator) Cancel(_ context.Context, session provider.AuthSession) error {
	if _, err := a.session(session); err != nil {
		return err
	}
	a.cancelState(session.Challenge.State)
	return nil
}

// ApplyAPIKey consumes protected input, patches one provider entry without
// rewriting existing redacted entries, and verifies the management read-back.
type providerKeyCreator interface {
	CreateProviderKey(context.Context, management.ProviderKeyKind, management.ProviderKey) (management.ProviderKey, error)
}

func (a *Authenticator) ApplyAPIKey(ctx context.Context, application provider.APIKeyApplication) (management.ProviderKey, error) {
	if !hasFlow(a.definition, provider.FlowAPIKey) {
		return management.ProviderKey{}, unsupportedFlow(a.definition.ID, provider.FlowAPIKey)
	}
	if application.Input == nil {
		return management.ProviderKey{}, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "A protected API-key input is required.")
	}
	for key := range application.Fields {
		if redact.IsSensitiveKey(key) {
			return management.ProviderKey{}, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "Secret fields must be supplied through protected input.")
		}
	}

	secret, err := application.Input.ReadSecret(ctx)
	if err != nil {
		return management.ProviderKey{}, safeWrap(err, pmuxerr.ConfigUnreadable, pmuxerr.Environment, "Could not read the protected API-key input.")
	}
	defer clearBytes(secret)
	secret = trimSecret(secret)
	if len(secret) == 0 {
		return management.ProviderKey{}, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "The API key is empty.")
	}

	kind, ok := providerKeyKind(a.definition.ID)
	if !ok {
		return management.ProviderKey{}, unsupportedFlow(a.definition.ID, provider.FlowAPIKey)
	}
	creator, ok := a.management.(providerKeyCreator)
	if !ok {
		return management.ProviderKey{}, pmuxerr.New(pmuxerr.CodeDependencyMissing, pmuxerr.Environment, "Lossless provider-key creation is unavailable.")
	}
	before, err := a.management.ProviderKeys(ctx, kind)
	if err != nil {
		return management.ProviderKey{}, safeWrap(err, pmuxerr.ManagementUnreachable, pmuxerr.Environment, "Could not read the provider-key collection.")
	}

	id := application.ID
	if id == "" {
		digest := sha256.Sum256(secret)
		id = "pmux-" + hex.EncodeToString(digest[:6])
	}
	for _, entry := range before {
		if entry.ID == id {
			return management.ProviderKey{}, pmuxerr.New(pmuxerr.ConfigMutationConflict, pmuxerr.Environment, "The provider-key ID already exists; PMux cannot update a redacted entry without a lossless rollback.")
		}
	}
	fields := make(map[string]string, len(application.Fields)+1)
	for key, value := range application.Fields {
		fields[key] = value
	}
	fields["api-key"] = string(secret)
	candidate := management.ProviderKey{ID: id, Label: application.Label, Fields: fields}
	created, createErr := creator.CreateProviderKey(ctx, kind, candidate)
	fields["api-key"] = ""
	if createErr != nil {
		return management.ProviderKey{}, safeWrap(createErr, pmuxerr.ProviderUnreachable, pmuxerr.Upstream, "The provider API key was not applied.")
	}
	return safeProviderKey(created), nil
}

func (a *Authenticator) ImportVertex(ctx context.Context, request provider.VertexImport) (management.VertexImportResult, error) {
	if a.definition.ID != "vertex" || !hasFlow(a.definition, provider.FlowVertexImport) {
		return management.VertexImportResult{}, unsupportedFlow(a.definition.ID, provider.FlowVertexImport)
	}
	if strings.TrimSpace(request.Path) == "" || !filepath.IsAbs(request.Path) {
		return management.VertexImportResult{}, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "An absolute private service-account file path is required.")
	}

	before, err := a.verifier.Snapshot(ctx, a.definition.ID)
	if err != nil {
		return management.VertexImportResult{}, safeWrap(err, pmuxerr.ManagementUnreachable, pmuxerr.Environment, "Could not read credential status before Vertex import.")
	}
	result, err := a.management.ImportVertex(ctx, management.VertexImportRequest{Path: request.Path, Prefix: request.Prefix})
	if err != nil {
		if !managementFallbackAllowed(err) {
			return management.VertexImportResult{}, safeWrap(err, pmuxerr.ProviderUnreachable, pmuxerr.Upstream, "Vertex service-account import failed.")
		}
		flags, flagErr := ClosedFallbackFlags(a.definition.ID, provider.FlowVertexImport, FallbackOptions{VertexPath: request.Path, VertexPrefix: request.Prefix})
		if flagErr != nil {
			return management.VertexImportResult{}, flagErr
		}
		if a.fallback == nil {
			return management.VertexImportResult{}, pmuxerr.New(pmuxerr.UnhandledUpstreamShape, pmuxerr.Upstream, "Vertex import is unavailable and no verified fallback runner is configured.")
		}
		if runErr := a.fallback.RunAuth(ctx, a.definition.ID, provider.FlowVertexImport, flags); runErr != nil {
			return management.VertexImportResult{}, safeWrap(runErr, pmuxerr.ProviderUnreachable, pmuxerr.Upstream, "Vertex import fallback failed.")
		}
	}

	credential, verifyErr := a.verifier.Verify(ctx, a.definition.ID, before)
	if verifyErr != nil {
		return management.VertexImportResult{}, safeWrap(verifyErr, pmuxerr.OAuthNoUsableCredential, pmuxerr.Upstream, "Vertex import completed, but no usable credential was verified.")
	}
	if result.Name == "" {
		result.Name = credential.Name
	}
	return result, nil
}

func ClosedFallbackFlags(providerID management.ProviderID, flow provider.AuthFlow, options FallbackOptions) ([]string, error) {
	definition, ok := definitionFor(providerID)
	if !ok {
		return nil, unsupportedFlow(providerID, flow)
	}
	mappedFlow := flow
	if flow == provider.FlowPasteCallback {
		mappedFlow = provider.FlowBrowser
	}
	base, ok := definition.SubprocessFlags[mappedFlow]
	if !ok || len(base) == 0 {
		return nil, pmuxerr.New(pmuxerr.UnhandledUpstreamShape, pmuxerr.Upstream, fmt.Sprintf("Provider %q has no supported CLIProxyAPI subprocess flow.", providerID))
	}
	flags := append([]string(nil), base...)
	if mappedFlow == provider.FlowBrowser && options.Headless {
		flags = append(flags, "-no-browser")
	}
	if mappedFlow == provider.FlowVertexImport {
		if len(flags) != 1 || flags[0] != "-vertex-import" || !filepath.IsAbs(options.VertexPath) {
			return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "Vertex fallback requires an absolute service-account path.")
		}
		flags = append(flags, options.VertexPath)
		if options.VertexPrefix != "" {
			flags = append(flags, "-vertex-import-prefix", options.VertexPrefix)
		}
	}
	return flags, nil
}

func (a *Authenticator) beginFallback(ctx context.Context, flow provider.AuthFlow, before CredentialSnapshot, headless bool, deadline time.Time) (provider.AuthSession, error) {
	flags, err := ClosedFallbackFlags(a.definition.ID, flow, FallbackOptions{Headless: headless})
	if err != nil {
		return provider.AuthSession{}, err
	}
	if a.fallback == nil {
		return provider.AuthSession{}, pmuxerr.New(pmuxerr.UnhandledUpstreamShape, pmuxerr.Upstream, "Management authentication is unavailable and no verified fallback runner is configured.")
	}
	mappedFlow := flow
	if flow == provider.FlowPasteCallback {
		mappedFlow = provider.FlowBrowser
	}
	if err := a.fallback.RunAuth(ctx, a.definition.ID, mappedFlow, flags); err != nil {
		if ctx.Err() != nil {
			return provider.AuthSession{}, authContextError(ctx)
		}
		return provider.AuthSession{}, safeWrap(err, pmuxerr.ProviderUnreachable, pmuxerr.Upstream, "CLIProxyAPI authentication fallback failed.")
	}
	if _, err := a.verifier.Verify(ctx, a.definition.ID, before); err != nil {
		if ctx.Err() != nil {
			return provider.AuthSession{}, authContextError(ctx)
		}
		return provider.AuthSession{}, safeWrap(err, pmuxerr.OAuthNoUsableCredential, pmuxerr.Upstream, "Authentication fallback completed, but no usable credential was verified.")
	}
	state, err := randomState()
	if err != nil {
		return provider.AuthSession{}, safeWrap(err, pmuxerr.UnhandledInternal, pmuxerr.Internal, "Could not create a local authentication result.")
	}
	now := a.clock.Now()
	a.mu.Lock()
	a.sessions[state] = &activeSession{flow: provider.FlowSubprocess, before: cloneSnapshot(before), deadline: deadline, lastPoll: now, interval: minimumPoll, fallbackOK: true}
	a.mu.Unlock()
	return provider.AuthSession{Provider: a.definition.ID, Flow: provider.FlowSubprocess, Challenge: management.OAuthChallenge{State: state}}, nil
}

func (a *Authenticator) supportsOAuth(flow provider.AuthFlow) bool {
	if flow == provider.FlowPasteCallback {
		return hasFlow(a.definition, provider.FlowBrowser)
	}
	if flow != provider.FlowBrowser && flow != provider.FlowDeviceCode {
		return false
	}
	return hasFlow(a.definition, flow)
}

func (a *Authenticator) session(session provider.AuthSession) (*activeSession, error) {
	if session.Provider != a.definition.ID || session.Challenge.State == "" {
		return nil, pmuxerr.New(pmuxerr.OAuthStateMismatch, pmuxerr.User, "Authentication session does not match this provider.")
	}
	a.mu.Lock()
	active := a.sessions[session.Challenge.State]
	a.mu.Unlock()
	if active == nil {
		return nil, pmuxerr.New(pmuxerr.OAuthStateMismatch, pmuxerr.User, "Authentication session is no longer active.")
	}
	return active, nil
}

func (a *Authenticator) waitForPoll(ctx context.Context, state string, active *activeSession) error {
	now := a.clock.Now()
	if !now.Before(active.deadline) {
		a.timeoutState(state)
		return pmuxerr.New(pmuxerr.OAuthTimeout, pmuxerr.Upstream, "Authentication timed out after five minutes; the session was canceled.")
	}
	wait := active.interval - now.Sub(active.lastPoll)
	if wait <= 0 {
		return nil
	}
	remaining := active.deadline.Sub(now)
	if wait > remaining {
		wait = remaining
	}
	if err := a.clock.Sleep(ctx, wait); err != nil {
		return err
	}
	if !a.clock.Now().Before(active.deadline) {
		a.timeoutState(state)
		return pmuxerr.New(pmuxerr.OAuthTimeout, pmuxerr.Upstream, "Authentication timed out after five minutes; the session was canceled.")
	}
	return nil
}

func (a *Authenticator) pollNow(ctx context.Context, session provider.AuthSession, active *activeSession) (management.OAuthStatus, error) {
	pollContext, cancel := a.operationContext(ctx, active)
	defer cancel()
	status, err := a.management.OAuthStatus(pollContext, session.Challenge.State)
	active.lastPoll = a.clock.Now()
	if err != nil {
		if pollContext.Err() != nil {
			a.cancelState(session.Challenge.State)
			if ctx.Err() != nil {
				return management.OAuthStatus{}, pmuxerr.New(pmuxerr.CodeCanceled, pmuxerr.User, "Authentication was canceled; no active session remains.")
			}
			return management.OAuthStatus{}, pmuxerr.New(pmuxerr.OAuthTimeout, pmuxerr.Upstream, "Authentication timed out after five minutes; the session was canceled.")
		}
		return management.OAuthStatus{}, safeWrap(err, pmuxerr.ProviderUnreachable, pmuxerr.Upstream, "Could not read provider authentication status.")
	}
	status.Message = redact.Known(status.Message, session.Challenge.State, session.Challenge.UserCode, session.Challenge.URL, session.Challenge.VerificationURI)
	switch normalizeStatus(status.Status) {
	case "pending":
		return status, nil
	case "success":
		if _, err := a.verifier.Verify(ctx, a.definition.ID, active.before); err != nil {
			a.deleteSession(session.Challenge.State)
			return management.OAuthStatus{}, safeWrap(err, pmuxerr.OAuthNoUsableCredential, pmuxerr.Upstream, "Authentication completed, but no usable credential was verified.")
		}
		a.deleteSession(session.Challenge.State)
		return terminalStatus(session.Challenge.State), nil
	case "expired":
		a.timeoutState(session.Challenge.State)
		return management.OAuthStatus{}, pmuxerr.New(pmuxerr.OAuthTimeout, pmuxerr.Upstream, "Authentication expired; the session was canceled.")
	case "failed":
		a.cancelState(session.Challenge.State)
		return management.OAuthStatus{}, pmuxerr.New(pmuxerr.OAuthNoUsableCredential, pmuxerr.Upstream, "Authentication did not complete; no usable credential was recorded.")
	default:
		a.cancelState(session.Challenge.State)
		return management.OAuthStatus{}, pmuxerr.New(pmuxerr.UnhandledUpstreamShape, pmuxerr.Upstream, "Provider authentication returned an unrecognized status; the session was canceled.")
	}
}
func (a *Authenticator) operationContext(ctx context.Context, active *activeSession) (context.Context, context.CancelFunc) {
	remaining := active.deadline.Sub(a.clock.Now())
	if remaining <= 0 {
		remaining = time.Nanosecond
	}
	return context.WithTimeout(ctx, remaining)
}


func (a *Authenticator) timeoutState(state string) {
	a.cancelState(state)
}

func (a *Authenticator) cancelState(state string) {
	a.deleteSession(state)
	ctx, cancel := context.WithTimeout(context.Background(), cleanupBudget)
	defer cancel()
	_ = a.management.CancelOAuth(ctx, state)
}

func (a *Authenticator) deleteSession(state string) {
	a.mu.Lock()
	delete(a.sessions, state)
	a.mu.Unlock()
}

func discriminateChallenge(providerID management.ProviderID, requested provider.AuthFlow, challenge management.OAuthChallenge) (provider.AuthFlow, error) {
	if challenge.State == "" {
		return "", pmuxerr.New(pmuxerr.UnhandledUpstreamShape, pmuxerr.Upstream, "Provider authentication response did not include a session state; no credentials were changed.")
	}
	isDevice := challenge.URL == "" && challenge.VerificationURI != "" && challenge.UserCode != ""
	isCallback := challenge.URL != "" && challenge.VerificationURI == "" && challenge.UserCode == ""
	if isDevice == isCallback {
		return "", pmuxerr.New(pmuxerr.UnhandledUpstreamShape, pmuxerr.Upstream, fmt.Sprintf("%s authentication response is not recognized; no credentials were changed.", displayName(providerID)))
	}
	if providerID == "xai" {
		if isDevice {
			return provider.FlowDeviceCode, nil
		}
		return provider.FlowPasteCallback, nil
	}
	if requested == provider.FlowDeviceCode && isDevice {
		return provider.FlowDeviceCode, nil
	}
	if (requested == provider.FlowBrowser || requested == provider.FlowPasteCallback) && isCallback {
		return requested, nil
	}
	return "", pmuxerr.New(pmuxerr.UnhandledUpstreamShape, pmuxerr.Upstream, "Provider authentication response did not match the requested flow; no credentials were changed.")
}

func validateCallback(raw, expectedState string) error {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return pmuxerr.New(pmuxerr.OAuthStateMismatch, pmuxerr.User, "Callback URL was rejected because it is not a valid local callback URL.")
	}
	host := parsed.Hostname()
	if host != "localhost" && net.ParseIP(host) == nil {
		return pmuxerr.New(pmuxerr.OAuthStateMismatch, pmuxerr.User, "Callback URL was rejected because it is not local.")
	}
	if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() {
		return pmuxerr.New(pmuxerr.OAuthStateMismatch, pmuxerr.User, "Callback URL was rejected because it is not local.")
	}
	if !strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/callback") {
		return pmuxerr.New(pmuxerr.OAuthStateMismatch, pmuxerr.User, "Callback URL was rejected because its route is not recognized.")
	}
	if parsed.Query().Get("state") != expectedState {
		return pmuxerr.New(pmuxerr.OAuthStateMismatch, pmuxerr.User, "Callback URL was rejected because it does not match the active authentication session.")
	}
	return nil
}

func normalizeStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "pending", "waiting", "authorizing", "in_progress":
		return "pending"
	case "success", "complete", "completed", "authenticated":
		return "success"
	case "expired", "timeout", "timed_out":
		return "expired"
	case "failed", "error", "denied", "rejected", "canceled", "cancelled":
		return "failed"
	default:
		return ""
	}
}

func terminalStatus(state string) management.OAuthStatus {
	return management.OAuthStatus{State: state, Status: "complete", Message: "Authentication complete."}
}

type managementVerifier struct {
	client management.ManagementClient
}

func (v *managementVerifier) Snapshot(ctx context.Context, _ management.ProviderID) (CredentialSnapshot, error) {
	files, err := v.client.AuthFiles(ctx)
	if err != nil {
		return CredentialSnapshot{}, err
	}
	return CredentialSnapshot{Files: append([]management.AuthFile(nil), files...)}, nil
}

func (v *managementVerifier) Verify(ctx context.Context, providerID management.ProviderID, before CredentialSnapshot) (management.AuthFile, error) {
	after, err := v.client.AuthFiles(ctx)
	if err != nil {
		return management.AuthFile{}, err
	}
	previous := make(map[string]management.AuthFile, len(before.Files))
	for _, file := range before.Files {
		previous[file.Name] = file
	}
	for _, file := range after {
		if file.Provider != providerID || !usable(file) {
			continue
		}
		prior, existed := previous[file.Name]
		if !existed || prior.Disabled || !usable(prior) {
			return file, nil
		}
	}
	return management.AuthFile{}, errors.New("no new usable credential metadata")
}

func usable(file management.AuthFile) bool {
	if file.Disabled {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(file.Status)) {
	case "", "ok", "ready", "usable", "active", "authenticated", "success":
		return true
	default:
		return false
	}
}

func managementFallbackAllowed(err error) bool {
	if errors.Is(err, ErrManagementUnavailable) {
		return true
	}
	var structured *pmuxerr.Error
	if !errors.As(err, &structured) {
		return false
	}
	if structured.Code == pmuxerr.CodeDependencyMissing {
		return true
	}
	explanation := strings.ToLower(structured.Explanation)
	if !strings.Contains(explanation, "http 404") {
		return false
	}
	return structured.Code == pmuxerr.UnhandledUpstreamShape || structured.Code == pmuxerr.ManagementUnreachable
}

func definitionFor(providerID management.ProviderID) (provider.Definition, bool) {
	for _, definition := range provider.Registry() {
		if definition.ID == providerID {
			return definition, true
		}
	}
	return provider.Definition{}, false
}

func (a *Authenticator) rollbackNewProviderKey(ctx context.Context, kind management.ProviderKeyKind, id string) error {
	if err := a.management.DeleteProviderKey(ctx, kind, id); err != nil {
		return err
	}
	entries, err := a.management.ProviderKeys(ctx, kind)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.ID == id {
			return errors.New("provider-key entry remained after compensating deletion")
		}
	}
	return nil
}

func hasFlow(definition provider.Definition, flow provider.AuthFlow) bool {
	for _, candidate := range definition.Flows {
		if candidate == flow {
			return true
		}
	}
	return false
}

func providerKeyKind(providerID management.ProviderID) (management.ProviderKeyKind, bool) {
	switch providerID {
	case "gemini":
		return management.ProviderGemini, true
	case "interactions":
		return management.ProviderInteractions, true
	case "codex", "codex-compatible":
		return management.ProviderCodex, true
	case "claude", "claude-compatible":
		return management.ProviderClaude, true
	case "xai":
		return management.ProviderXAI, true
	case "vertex":
		return management.ProviderVertex, true
	case "openrouter", "openai-compatible":
		return management.ProviderOpenAICompatible, true
	default:
		return "", false
	}
}

func cloneSnapshot(snapshot CredentialSnapshot) CredentialSnapshot {
	return CredentialSnapshot{Files: append([]management.AuthFile(nil), snapshot.Files...)}
}


func safeProviderKey(key management.ProviderKey) management.ProviderKey {
	safe := management.ProviderKey{ID: key.ID, Label: key.Label, Mask: key.Mask, Fields: make(map[string]string)}
	for field, value := range key.Fields {
		if !redact.IsSensitiveKey(field) {
			safe.Fields[field] = value
		}
	}
	return safe
}

func trimSecret(secret []byte) []byte {
	start := 0
	for start < len(secret) && (secret[start] == ' ' || secret[start] == '\t' || secret[start] == '\r' || secret[start] == '\n') {
		start++
	}
	end := len(secret)
	for end > start && (secret[end-1] == ' ' || secret[end-1] == '\t' || secret[end-1] == '\r' || secret[end-1] == '\n') {
		end--
	}
	return secret[start:end]
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func randomState() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "fallback-" + hex.EncodeToString(value[:]), nil
}

func authContextError(ctx context.Context) *pmuxerr.Error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return pmuxerr.New(pmuxerr.CodeCanceled, pmuxerr.User, "Authentication was canceled; no active session remains.")
	}
	return pmuxerr.New(pmuxerr.OAuthTimeout, pmuxerr.Upstream, "Authentication timed out after five minutes; no usable credential was recorded.")
}


func safeWrap(err error, code string, class pmuxerr.Class, message string) *pmuxerr.Error {
	// The underlying management/subprocess error may contain a callback URL,
	// device code, provider key, or service-account content. Preserve only a
	// typed error's stable classification; always replace its presentation and
	// cause before it can reach normal or verbose rendering.
	var typed *pmuxerr.Error
	if errors.As(err, &typed) {
		code, class = typed.Code, typed.Class
	}
	return &pmuxerr.Error{Code: code, Class: class, Message: message, Cause: errors.New("redacted adapter error")}
}

func unsupportedFlow(providerID management.ProviderID, flow provider.AuthFlow) *pmuxerr.Error {
	return pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, fmt.Sprintf("Provider %q does not support authentication flow %q.", providerID, flow))
}

func displayName(providerID management.ProviderID) string {
	if definition, ok := definitionFor(providerID); ok {
		return definition.Name
	}
	return string(providerID)
}
