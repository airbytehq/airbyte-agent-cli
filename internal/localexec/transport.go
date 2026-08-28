package localexec

// transport.go creates an ISOLATED, hardened HTTP client for connector-origin
// requests and executes a compiled+authenticated request plan against it.
//
// This client is deliberately NOT internal/client.Client: that client injects
// the Airbyte bearer token, organization ID, and CLI headers on every request.
// A connector-origin request must carry NONE of that material, so this package
// builds its own http.Client + http.Transport from scratch and only ever sets
// the headers/cookies present in the request plan.
//
// Hardening applied here:
//   - HTTPS-only destinations in production (an HTTP allowance exists solely for
//     tests and is never set by production callers).
//   - Bounded redirects, with HTTPS->HTTP redirects refused and Authorization /
//     Cookie headers stripped on cross-origin redirects (defense in depth).
//   - Bounded response body size, per-request timeout, and context cancellation.
//   - Retries for safe idempotent methods (GET/HEAD) only, never for mutations.
//   - Non-2xx responses converted to a sanitized connector_execution_error that
//     exposes only the status and a redacted origin/path — never the body.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// TypeConnectorExecution marks a failure that occurred while talking to the
// connector's origin (DNS/TLS/timeout/redirect-violation/non-2xx/response
// transform). It maps to exit code 1 via Error.ExitCode's default arm.
const TypeConnectorExecution = "connector_execution_error"

// Transport defaults. All are conservative and overridable via executor options.
const (
	defaultTimeout        = 30 * time.Second
	defaultMaxBodyBytes   = 10 << 20 // 10 MiB
	defaultMaxRedirects   = 5
	hardMaxRetries        = 5
	tlsHandshakeTimeout   = 10 * time.Second
	expectContinueTimeout = 1 * time.Second
	idleConnTimeout       = 30 * time.Second
)

// connectorError constructs a sanitized connector_execution_error. Messages
// passed here MUST already be redaction-safe (status + redacted origin/path
// only) — never a raw remote body, header, or wrapped transport error string.
func connectorError(message string) *Error {
	return &Error{ErrType: TypeConnectorExecution, Message: message}
}

// transportConfig carries the tunable + injectable transport seams.
type transportConfig struct {
	timeout      time.Duration
	maxBodyBytes int64
	maxRedirects int
	maxRetries   int
	// allowHTTP relaxes the HTTPS-only rule. It is TEST-ONLY: production callers
	// never set it, so httptest.Server (plain HTTP) can be exercised without
	// weakening production defaults.
	allowHTTP bool
	// roundTripper, when non-nil, replaces the hardened default transport. Used
	// by tests to inject a custom RoundTripper.
	roundTripper http.RoundTripper
	// backoff maps a 1-based retry attempt to a sleep duration. Defaults to no
	// sleep so tests are fast and deterministic.
	backoff func(attempt int) time.Duration
}

// httpResponse is the decoded, bounded result of a successful (2xx) exchange.
type httpResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	FinalURL   *url.URL
}

// httpTransport is the isolated client wrapper.
type httpTransport struct {
	client *http.Client
	cfg    transportConfig
}

// newTransport builds the isolated hardened client from cfg, filling defaults.
func newTransport(cfg transportConfig) *httpTransport {
	if cfg.timeout <= 0 {
		cfg.timeout = defaultTimeout
	}
	if cfg.maxBodyBytes <= 0 {
		cfg.maxBodyBytes = defaultMaxBodyBytes
	}
	if cfg.maxRedirects <= 0 {
		cfg.maxRedirects = defaultMaxRedirects
	}
	if cfg.maxRetries < 0 {
		cfg.maxRetries = 0
	}
	if cfg.maxRetries > hardMaxRetries {
		cfg.maxRetries = hardMaxRetries
	}
	if cfg.backoff == nil {
		cfg.backoff = func(int) time.Duration { return 0 }
	}
	rt := cfg.roundTripper
	if rt == nil {
		rt = hardenedRoundTripper()
	}
	client := &http.Client{
		Transport:     rt,
		Timeout:       cfg.timeout,
		CheckRedirect: makeCheckRedirect(cfg),
	}
	return &httpTransport{client: client, cfg: cfg}
}

// hardenedRoundTripper builds the default *http.Transport. It carries no
// Airbyte credentials or headers of any kind.
func hardenedRoundTripper() http.RoundTripper {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
	}
}

// makeCheckRedirect enforces the redirect policy: a bounded hop count, refusal
// of non-HTTPS redirect targets (unless the test HTTP allowance is set), and
// stripping of Authorization / Cookie headers on cross-origin hops.
func makeCheckRedirect(cfg transportConfig) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= cfg.maxRedirects {
			return connectorError(fmt.Sprintf("connector request exceeded the maximum of %d redirects", cfg.maxRedirects))
		}
		if req.URL.Scheme != "https" && !cfg.allowHTTP {
			return connectorError("connector redirect to a non-HTTPS destination is not allowed")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			// Cross-origin: never forward credentials. Go strips some of these
			// itself; this is belt-and-suspenders.
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
		}
		return nil
	}
}

// isIdempotent reports whether a method is safe to retry.
func isIdempotent(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

// isRetryableStatus reports whether a non-2xx status is worth retrying (only for
// idempotent methods).
func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// do executes plan against the isolated client, applying URL validation, safe
// retries, body bounds, and non-2xx sanitization. The response body is always
// read to completion and closed.
func (t *httpTransport) do(ctx context.Context, plan *RequestPlan) (*httpResponse, error) {
	if err := validateDestination(plan.URL, t.cfg.allowHTTP); err != nil {
		return nil, err
	}
	retries := 0
	if isIdempotent(plan.Method) {
		retries = t.cfg.maxRetries
	}
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, connectorError("connector request was canceled")
			case <-time.After(t.cfg.backoff(attempt)):
			}
		}
		resp, retryable, err := t.attempt(ctx, plan)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !(retryable && attempt < retries) {
			return nil, err
		}
	}
	return nil, lastErr
}

// attempt performs a single request exchange. It returns (resp, false, nil) on
// a 2xx, or (nil, retryable, err) otherwise. The body is always closed.
func (t *httpTransport) attempt(ctx context.Context, plan *RequestPlan) (*httpResponse, bool, error) {
	req, err := buildHTTPRequest(ctx, plan)
	if err != nil {
		return nil, false, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, true, sanitizeTransportError(plan.URL, err)
	}
	defer resp.Body.Close()

	// Read at most maxBodyBytes+1 so we can detect an over-limit body.
	limited := io.LimitReader(resp.Body, t.cfg.maxBodyBytes+1)
	body, readErr := io.ReadAll(limited)
	if readErr != nil {
		return nil, true, sanitizeTransportError(plan.URL, readErr)
	}
	if int64(len(body)) > t.cfg.maxBodyBytes {
		return nil, false, connectorError("connector response body exceeds the maximum allowed size")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, isRetryableStatus(resp.StatusCode), connectorError(
			fmt.Sprintf("connector returned HTTP %d for %s", resp.StatusCode, redactURL(plan.URL)))
	}
	finalURL := resp.Request.URL
	return &httpResponse{
		StatusCode: resp.StatusCode,
		Header:     resp.Header.Clone(),
		Body:       body,
		FinalURL:   finalURL,
	}, false, nil
}

// buildHTTPRequest constructs an *http.Request from a plan. It sets ONLY the
// plan's headers and cookies plus a neutral User-Agent; no Airbyte material is
// ever added here.
func buildHTTPRequest(ctx context.Context, plan *RequestPlan) (*http.Request, error) {
	var bodyReader io.Reader
	if len(plan.Body) > 0 {
		bodyReader = bytes.NewReader(plan.Body)
	}
	req, err := http.NewRequestWithContext(ctx, plan.Method, plan.URL, bodyReader)
	if err != nil {
		return nil, connectorError(fmt.Sprintf("connector request could not be constructed for %s", redactURL(plan.URL)))
	}
	// Re-readable body for redirects.
	if len(plan.Body) > 0 {
		body := plan.Body
		req.ContentLength = int64(len(body))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	}
	for _, h := range plan.Headers {
		req.Header.Add(h.Name, h.Value)
	}
	for _, c := range plan.Cookies {
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "airbyte-agent-local/1")
	}
	return req, nil
}

// validateDestination enforces the HTTPS-only rule (relaxed only by the
// test-only allowHTTP flag) and requires an absolute URL with a host.
func validateDestination(rawURL string, allowHTTP bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return connectorError("connector request URL is not valid")
	}
	if u.Host == "" {
		return connectorError("connector request URL is missing a host")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if allowHTTP {
			return nil
		}
		return connectorError("connector request URL must use HTTPS")
	default:
		return connectorError("connector request URL must use HTTPS")
	}
}

// sanitizeTransportError converts a transport-level error into a redacted
// connector_execution_error. It never includes the wrapped error string (which
// can embed the full URL with query) — only a redacted origin/path. A redirect
// policy error raised by CheckRedirect (already a typed *Error) is surfaced
// verbatim.
func sanitizeTransportError(rawURL string, err error) error {
	if e, ok := AsError(err); ok {
		return e
	}
	return connectorError(fmt.Sprintf("connector request failed for %s", redactURL(rawURL)))
}

// redactURL renders scheme://host/path, dropping any query string (which may
// carry signed parameters) and userinfo.
func redactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "connector"
	}
	return u.Scheme + "://" + u.Host + u.Path
}
