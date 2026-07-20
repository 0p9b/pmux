package mgmtapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/0p9b/pmux/internal/pmuxerr"
	"github.com/0p9b/pmux/internal/redact"
)

const (
	managementPrefix   = "/v0/management"
	defaultTimeout     = 2 * time.Second
	defaultMaxResponse = int64(4 << 20)
)

var credentialPattern = regexp.MustCompile(`(?i)(bearer\s+|sk-)[A-Za-z0-9._~+/=-]{8,}`)

// Options configures the Management API adapter. BaseURL must identify the
// local CLIProxyAPI origin, without a management path suffix.
type Options struct {
	BaseURL         string
	ManagementKey   string
	ProxyKey        string
	HTTPClient      *http.Client
	Timeout         time.Duration
	MaxResponseSize int64
}

// Client implements management.ManagementClient. It deliberately exposes no
// adapter-specific error type; every returned error is a *pmuxerr.Error.
type Client struct {
	base          *url.URL
	http          *http.Client
	managementKey string
	proxyKey      string
	timeout       time.Duration
	maxResponse   int64
}

func New(options Options) (*Client, error) {
	base, err := url.Parse(options.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, pmuxerr.Wrap(safeCause(err, "invalid base URL"), pmuxerr.ConfigValidationFailed, pmuxerr.User, "CLIProxyAPI base URL is invalid")
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "CLIProxyAPI base URL must use HTTP or HTTPS")
	}
	if base.RawQuery != "" || base.Fragment != "" {
		return nil, pmuxerr.New(pmuxerr.ConfigValidationFailed, pmuxerr.User, "CLIProxyAPI base URL must not contain a query or fragment")
	}
	base.Path = strings.TrimSuffix(base.Path, "/")
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	maxResponse := options.MaxResponseSize
	if maxResponse <= 0 {
		maxResponse = defaultMaxResponse
	}
	hc := options.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{
		base: base, http: hc, managementKey: options.ManagementKey,
		proxyKey: options.ProxyKey, timeout: timeout, maxResponse: maxResponse,
	}, nil
}

func (c *Client) endpoint(parts ...string) string {
	u := *c.base
	plain := make([]string, 0, len(parts)+1)
	escaped := make([]string, 0, len(parts)+1)
	for _, segment := range strings.Split(strings.Trim(u.Path, "/"), "/") {
		if segment != "" {
			plain = append(plain, segment)
			escaped = append(escaped, url.PathEscape(segment))
		}
	}
	for _, part := range parts {
		plain = append(plain, part)
		escaped = append(escaped, url.PathEscape(part))
	}
	u.Path = "/" + strings.Join(plain, "/")
	u.RawPath = "/" + strings.Join(escaped, "/")
	return u.String()
}

func (c *Client) managementEndpoint(parts ...string) string {
	all := []string{"v0", "management"}
	all = append(all, parts...)
	return c.endpoint(all...)
}

type authMode uint8

const (
	authNone authMode = iota
	authManagement
	authProxy
)

type requestSpec struct {
	method      string
	url         string
	query       url.Values
	body        []byte
	contentType string
	auth        authMode
	management  bool
	classify404 bool
	responseMax int64
}

func (c *Client) request(ctx context.Context, spec requestSpec) ([]byte, http.Header, int, error) {
	requestURL, err := url.Parse(spec.url)
	if err != nil {
		return nil, nil, 0, pmuxerr.Wrap(safeCause(err, "invalid request URL"), pmuxerr.UnhandledInternal, pmuxerr.Internal, "Could not construct the CLIProxyAPI request")
	}
	if len(spec.query) != 0 {
		requestURL.RawQuery = spec.query.Encode()
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, spec.method, requestURL.String(), bytes.NewReader(spec.body))
	if err != nil {
		return nil, nil, 0, pmuxerr.Wrap(safeCause(err, "request creation failed"), pmuxerr.UnhandledInternal, pmuxerr.Internal, "Could not construct the CLIProxyAPI request")
	}
	if spec.contentType != "" {
		req.Header.Set("Content-Type", spec.contentType)
	} else if spec.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch spec.auth {
	case authManagement:
		if c.managementKey == "" {
			return nil, nil, 0, managementKeyMissing()
		}
		req.Header.Set("Authorization", "Bearer "+c.managementKey)
	case authProxy:
		if c.proxyKey == "" {
			return nil, nil, 0, pmuxerr.New(pmuxerr.ConfigUnreadable, pmuxerr.Environment, "The proxy API key is not configured")
		}
		req.Header.Set("Authorization", "Bearer "+c.proxyKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(requestCtx.Err(), context.Canceled) {
			return nil, nil, 0, pmuxerr.Wrap(context.Canceled, pmuxerr.CodeCanceled, pmuxerr.User, "The CLIProxyAPI request was canceled")
		}
		message := "Could not reach CLIProxyAPI"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
			message = "CLIProxyAPI did not respond within " + c.timeout.String()
		}
		return nil, nil, 0, pmuxerr.Wrap(safeCause(err, "HTTP transport failed"), pmuxerr.ManagementUnreachable, pmuxerr.Environment, message)
	}
	defer func() { _ = resp.Body.Close() }()
	limit := spec.responseMax
	if limit <= 0 || limit > c.maxResponse {
		limit = c.maxResponse
	}
	body, err := readBounded(resp.Body, limit)
	if err != nil {
		if errors.Is(err, errResponseTooLarge) {
			return nil, resp.Header.Clone(), resp.StatusCode, pmuxerr.New(pmuxerr.UnhandledUpstreamShape, pmuxerr.Upstream, fmt.Sprintf("CLIProxyAPI response exceeded the %d-byte safety limit", limit))
		}
		return nil, resp.Header.Clone(), resp.StatusCode, pmuxerr.Wrap(safeCause(err, "response read failed"), pmuxerr.ManagementUnreachable, pmuxerr.Environment, "Could not read the CLIProxyAPI response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.Header.Clone(), resp.StatusCode, c.statusError(ctx, spec, resp.StatusCode)
	}
	return body, resp.Header.Clone(), resp.StatusCode, nil
}

var errResponseTooLarge = errors.New("response exceeds configured bound")

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errResponseTooLarge
	}
	return body, nil
}

func (c *Client) statusError(ctx context.Context, spec requestSpec, status int) error {
	if spec.management && status == http.StatusUnauthorized {
		return &pmuxerr.Error{
			Code: pmuxerr.ManagementAuthRejected, Class: pmuxerr.Upstream,
			Message:     "Management auth failed (HTTP 401). PMux will not retry — 5 failures trigger a 30-minute ban.",
			Explanation: "The stored management key was rejected; PMux made exactly one authenticated attempt.",
			Repair:      []string{"Run `pmux doctor --fix management-key` to regenerate and re-sync the key."},
		}
	}
	if spec.management && status == http.StatusForbidden {
		return &pmuxerr.Error{
			Code: pmuxerr.ManagementAuthRejected, Class: pmuxerr.Upstream,
			Message:     "Management access was refused (HTTP 403); the client address may be banned for 30 minutes.",
			Explanation: "CLIProxyAPI bans an address after five failed management authentication attempts. PMux will not retry.",
			Repair:      []string{"Wait for the 30-minute ban window, then run `pmux doctor --fix management-key`."},
		}
	}
	if spec.management && status == http.StatusNotFound {
		if spec.classify404 {
			enabled, probeErr := c.managementEnabled(ctx)
			if probeErr != nil {
				return probeErr
			}
			if enabled {
				return &pmuxerr.Error{Code: pmuxerr.UnhandledUpstreamShape, Class: pmuxerr.Upstream, Message: "This CLIProxyAPI does not provide the requested management endpoint", Explanation: "Management is enabled, but this individual endpoint returned HTTP 404."}
			}
		}
		return &pmuxerr.Error{Code: pmuxerr.ManagementUnreachable, Class: pmuxerr.Environment, Message: "CLIProxyAPI management is disabled", Explanation: "The management route tree returned HTTP 404. Configure a local management secret before using this operation.", Repair: []string{"Run `pmux doctor --fix management-key`."}}
	}
	if status == http.StatusConflict {
		return &pmuxerr.Error{Code: pmuxerr.ConfigMutationConflict, Class: pmuxerr.Upstream, Message: "CLIProxyAPI rejected the request because its state changed concurrently"}
	}
	if status == http.StatusTooManyRequests {
		return &pmuxerr.Error{Code: pmuxerr.ManagementUnreachable, Class: pmuxerr.Upstream, Message: "CLIProxyAPI rate-limited the request"}
	}
	code := pmuxerr.UnhandledUpstreamShape
	if status >= 500 {
		code = pmuxerr.ManagementUnreachable
	}
	return &pmuxerr.Error{Code: code, Class: pmuxerr.Upstream, Message: fmt.Sprintf("CLIProxyAPI returned HTTP %d", status), Evidence: []string{fmt.Sprintf("http_status=%d", status)}}
}

func (c *Client) managementEnabled(ctx context.Context) (bool, error) {
	_, _, status, err := c.request(ctx, requestSpec{
		method: http.MethodGet, url: c.managementEndpoint("config"), auth: authManagement,
		management: true, classify404: false,
	})
	if err == nil {
		return true, nil
	}
	if status == http.StatusNotFound {
		return false, nil
	}
	return false, err
}

func managementKeyMissing() error {
	return &pmuxerr.Error{Code: pmuxerr.ConfigUnreadable, Class: pmuxerr.Environment, Message: "The management key is not configured", Repair: []string{"Run `pmux doctor --fix management-key`."}}
}

func safeCause(err error, fallback string) error {
	if err == nil {
		return errors.New(fallback)
	}
	return privateCause{err: err, text: fallback}
}

type privateCause struct {
	err  error
	text string
}

func (e privateCause) Error() string { return e.text }

func (c *Client) redact(value string) string {
	value = redact.Known(value, c.managementKey, c.proxyKey)
	return credentialPattern.ReplaceAllStringFunc(value, func(found string) string {
		if i := strings.IndexByte(found, ' '); i >= 0 {
			return found[:i+1] + "********"
		}
		return redact.Mask(found)
	})
}

func fingerprint(value string) string {
	hash := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(hash[:])
}

func marshalBody(value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, pmuxerr.Wrap(safeCause(err, "JSON encoding failed"), pmuxerr.UnhandledInternal, pmuxerr.Internal, "Could not encode the CLIProxyAPI request")
	}
	return body, nil
}

func decodeJSON(body []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return pmuxerr.Wrap(safeCause(err, "invalid JSON response"), pmuxerr.UnhandledUpstreamShape, pmuxerr.Upstream, "CLIProxyAPI returned malformed JSON")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return pmuxerr.New(pmuxerr.UnhandledUpstreamShape, pmuxerr.Upstream, "CLIProxyAPI returned more than one JSON value")
	}
	return nil
}

func queryWith(key, value string) url.Values {
	query := make(url.Values, 1)
	query.Set(key, value)
	return query
}
