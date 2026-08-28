package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/airbytehq/airbyte-agent-cli/internal/auth"
	"github.com/airbytehq/airbyte-agent-cli/internal/config"
)

const (
	maxRetries     = 3
	baseRetryDelay = 1 * time.Second
	requestTimeout = 30 * time.Second
)

type Client struct {
	apiHost          string
	organizationID   string
	defaultWorkspace string
	allowDestructive bool
	userAgent        string
	version          string
	tokenManager     *auth.TokenManager
	httpClient       *http.Client
	debug            bool
	debugFunc        func() bool

	executionConfigFunc func() (config.ExecutionConfig, error)
}

type Option func(*Client)

func WithDebug(debug bool) Option {
	return func(c *Client) {
		c.debug = debug
	}
}

func WithDebugFunc(debugFunc func() bool) Option {
	return func(c *Client) {
		c.debugFunc = debugFunc
	}
}

// WithExecutionConfigFunc registers a deferred resolver for the runtime
// execution configuration. Following the WithDebugFunc pattern, the function is
// called lazily (at request time), NOT at client construction, so it observes
// the values Cobra parsed from argv. The resolver applies precedence and
// validation and MAY return an error (e.g. an invalid --execution-mode); the
// error is surfaced to the caller so it can be mapped to a validation exit
// code rather than being silently swallowed into a hosted fallback.
func WithExecutionConfigFunc(fn func() (config.ExecutionConfig, error)) Option {
	return func(c *Client) {
		c.executionConfigFunc = fn
	}
}

// ExecutionConfig returns the resolved runtime execution configuration,
// reading it lazily via the registered resolver. It is nil-safe and defaults
// to hosted mode (with a nil error) when no resolver is registered (or the
// receiver is nil). A non-nil error from the resolver is propagated unchanged
// so callers can surface invalid runtime configuration as a validation error.
func (c *Client) ExecutionConfig() (config.ExecutionConfig, error) {
	if c == nil || c.executionConfigFunc == nil {
		return config.ExecutionConfig{Mode: config.ModeHosted}, nil
	}
	return c.executionConfigFunc()
}

// WithDefaultWorkspace sets the workspace name used as a fallback when a
// command needs `workspace` but the caller didn't supply one. Empty string
// is allowed (resources fall back to a hardcoded "default").
func WithDefaultWorkspace(name string) Option {
	return func(c *Client) {
		c.defaultWorkspace = name
	}
}

// DefaultWorkspace returns the configured fallback workspace name, or "" if
// none was set.
func (c *Client) DefaultWorkspace() string {
	return c.defaultWorkspace
}

// OrganizationID returns the configured organization ID, or "" if none was
// set. Safe to call on a nil receiver (returns "").
func (c *Client) OrganizationID() string {
	if c == nil {
		return ""
	}
	return c.organizationID
}

// WithAllowDestructive grants permission for destructive operations (e.g.
// `connectors delete`) to run without prompting for interactive
// confirmation. Sourced from `allow_destructive` in settings.json.
func WithAllowDestructive(allow bool) Option {
	return func(c *Client) {
		c.allowDestructive = allow
	}
}

// AllowDestructive reports whether destructive operations may skip the
// interactive confirmation prompt. Safe to call on a nil receiver
// (returns false).
func (c *Client) AllowDestructive() bool {
	if c == nil {
		return false
	}
	return c.allowDestructive
}

func New(apiHost, organizationID, version string, tm *auth.TokenManager, opts ...Option) *Client {
	c := &Client{
		apiHost:        apiHost,
		organizationID: organizationID,
		userAgent:      "airbyte-agent/" + version,
		version:        version,
		tokenManager:   tm,
		httpClient:     &http.Client{Timeout: requestTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Client) Get(ctx context.Context, path string, params map[string]string) (json.RawMessage, error) {
	u, err := url.Parse(c.apiHost + path)
	if err != nil {
		return nil, fmt.Errorf("parsing URL: %w", err)
	}

	if len(params) > 0 {
		q := u.Query()
		for k, v := range params {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}

	return c.do(ctx, http.MethodGet, u.String(), nil)
}

func (c *Client) Post(ctx context.Context, path string, body any) (json.RawMessage, error) {
	return c.doWithBody(ctx, http.MethodPost, path, body)
}

func (c *Client) Patch(ctx context.Context, path string, body any) (json.RawMessage, error) {
	return c.doWithBody(ctx, http.MethodPatch, path, body)
}

func (c *Client) Delete(ctx context.Context, path string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodDelete, c.apiHost+path, nil)
}

func (c *Client) GetURL(ctx context.Context, rawURL string) (json.RawMessage, error) {
	safeURL, err := c.resolveAPIURL(rawURL)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodGet, safeURL, nil)
}

func (c *Client) doWithBody(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, fmt.Errorf("encoding request body: %w", err)
		}
	}
	return c.do(ctx, method, c.apiHost+path, &buf)
}

func (c *Client) do(ctx context.Context, method, rawURL string, body io.Reader) (json.RawMessage, error) {
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = io.ReadAll(body)
		if err != nil {
			return nil, fmt.Errorf("reading request body: %w", err)
		}
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := baseRetryDelay * time.Duration(math.Pow(2, float64(attempt-1)))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		var bodyReader io.Reader
		if bodyBytes != nil {
			bodyReader = bytes.NewReader(bodyBytes)
		}

		result, retryable, err := c.doOnce(ctx, method, rawURL, bodyReader)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}

		if c.isDebug() {
			log.Printf("[DEBUG] request %s %s attempt %d failed: %s", method, redactURL(rawURL), attempt+1, debugError(err))
		}
	}

	return nil, lastErr
}

func (c *Client) doOnce(ctx context.Context, method, rawURL string, body io.Reader) (json.RawMessage, bool, error) {
	token, err := c.tokenManager.GetToken()
	if err != nil {
		return nil, false, fmt.Errorf("getting auth token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, false, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("X-ADP-Agent-CLI", c.version)
	if c.organizationID != "" {
		req.Header.Set("X-Organization-Id", c.organizationID)
	}

	if c.isDebug() {
		log.Printf("[DEBUG] %s %s", method, redactURL(rawURL))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("executing request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("reading response body: %w", err)
	}

	if c.isDebug() {
		log.Printf("[DEBUG] response status=%d bytes=%d", resp.StatusCode, len(respBody))
	}

	if resp.StatusCode >= 400 {
		message := extractErrorMessage(respBody, resp.StatusCode)
		apiErr := newAPIError(resp.StatusCode, message, respBody)
		return nil, apiErr.Retryable, apiErr
	}

	if len(respBody) == 0 {
		return json.RawMessage("null"), false, nil
	}

	return json.RawMessage(respBody), false, nil
}

func (c *Client) isDebug() bool {
	return c.debug || (c.debugFunc != nil && c.debugFunc())
}

func (c *Client) resolveAPIURL(rawURL string) (string, error) {
	apiBase, err := url.Parse(c.apiHost)
	if err != nil {
		return "", fmt.Errorf("parsing API host: %w", err)
	}
	nextURL, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parsing URL: %w", err)
	}
	resolvedURL := nextURL
	if !nextURL.IsAbs() {
		resolvedURL = apiBase.ResolveReference(nextURL)
	}
	if resolvedURL.Scheme != apiBase.Scheme || resolvedURL.Host != apiBase.Host {
		return "", &APIError{
			Type:       "validation_error",
			Message:    "pagination URL points outside the configured API host",
			StatusCode: 400,
		}
	}
	return resolvedURL.String(), nil
}

func redactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := u.Query()
	for key := range q {
		if isSensitiveKey(key) {
			q.Set(key, "[REDACTED]")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func debugError(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("%s status=%d retryable=%t", apiErr.Type, apiErr.StatusCode, apiErr.Retryable)
	}
	return redactText(err.Error())
}

func redactText(s string) string {
	parts := strings.Fields(s)
	for i, part := range parts {
		if strings.Contains(part, "=") {
			key, _, _ := strings.Cut(part, "=")
			if isSensitiveKey(strings.Trim(key, `"'`)) {
				parts[i] = key + "=[REDACTED]"
			}
		}
	}
	return strings.Join(parts, " ")
}

func isSensitiveKey(key string) bool {
	key = strings.ToLower(key)
	return strings.Contains(key, "token") ||
		strings.Contains(key, "secret") ||
		strings.Contains(key, "password") ||
		strings.Contains(key, "credential") ||
		strings.Contains(key, "api_key") ||
		strings.Contains(key, "apikey")
}

func extractErrorMessage(body []byte, statusCode int) string {
	var parsed struct {
		Detail  string `json:"detail"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		if parsed.Detail != "" {
			return parsed.Detail
		}
		if parsed.Message != "" {
			return parsed.Message
		}
	}
	if len(body) > 0 {
		return string(body)
	}
	return fmt.Sprintf("HTTP %d", statusCode)
}
