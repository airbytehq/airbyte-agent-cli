package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/airbytehq/airbyte-agent-cli/internal/auth"
	"github.com/airbytehq/airbyte-agent-cli/internal/config"
)

func newTestTokenServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "test-bearer-token",
			"token_type":   "bearer",
			"expires_in":   1200,
		})
	}))
}

func newTestClient(t *testing.T, apiServer *httptest.Server) (c *Client, cleanup func()) {
	t.Helper()
	tokenServer := newTestTokenServer(t)
	creds := &auth.Credentials{ClientID: "id", ClientSecret: "secret"}
	tm := auth.NewTokenManager(tokenServer.URL, "", creds)

	c = New(apiServer.URL, "org-123", "1.0.0-test", tm)
	return c, func() {
		tokenServer.Close()
	}
}

func TestClient_Get_AuthHeaders(t *testing.T) {
	var gotHeaders http.Header
	var gotPath string
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": "ok"}`))
	}))
	defer apiServer.Close()

	c, cleanup := newTestClient(t, apiServer)
	defer cleanup()

	result, err := c.Get(context.Background(), "/v1/test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotPath != "/v1/test" {
		t.Errorf("path = %q, want %q", gotPath, "/v1/test")
	}
	if auth := gotHeaders.Get("Authorization"); auth != "Bearer test-bearer-token" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer test-bearer-token")
	}
	if ua := gotHeaders.Get("User-Agent"); ua != "airbyte-agent/1.0.0-test" {
		t.Errorf("User-Agent = %q, want %q", ua, "airbyte-agent/1.0.0-test")
	}
	if org := gotHeaders.Get("X-Organization-Id"); org != "org-123" {
		t.Errorf("X-Organization-Id = %q, want %q", org, "org-123")
	}
	if cli := gotHeaders.Get("X-Adp-Agent-Cli"); cli != "1.0.0-test" {
		t.Errorf("X-ADP-Agent-CLI = %q, want %q", cli, "1.0.0-test")
	}

	var parsed map[string]string
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("parsing result: %v", err)
	}
	if parsed["data"] != "ok" {
		t.Errorf("data = %q, want %q", parsed["data"], "ok")
	}
}

func TestClient_Get_QueryParams(t *testing.T) {
	var gotQuery string
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer apiServer.Close()

	c, cleanup := newTestClient(t, apiServer)
	defer cleanup()

	_, err := c.Get(context.Background(), "/v1/test", map[string]string{
		"limit":  "10",
		"offset": "0",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotQuery == "" {
		t.Error("expected query params, got empty")
	}
}

func TestClient_Post_SendsBody(t *testing.T) {
	var gotBody map[string]string
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id": "created"}`))
	}))
	defer apiServer.Close()

	c, cleanup := newTestClient(t, apiServer)
	defer cleanup()

	body := map[string]string{"name": "test-resource"}
	_, err := c.Post(context.Background(), "/v1/resources", body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotBody["name"] != "test-resource" {
		t.Errorf("body name = %q, want %q", gotBody["name"], "test-resource")
	}
}

func TestClient_Delete(t *testing.T) {
	var gotMethod string
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer apiServer.Close()

	c, cleanup := newTestClient(t, apiServer)
	defer cleanup()

	_, err := c.Delete(context.Background(), "/v1/resources/123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", gotMethod)
	}
}

func TestClient_APIError(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail": "resource not found"}`))
	}))
	defer apiServer.Close()

	c, cleanup := newTestClient(t, apiServer)
	defer cleanup()

	_, err := c.Get(context.Background(), "/v1/missing", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.StatusCode != 404 {
		t.Errorf("StatusCode = %d, want 404", apiErr.StatusCode)
	}
	if apiErr.Type != "not_found" {
		t.Errorf("Type = %q, want %q", apiErr.Type, "not_found")
	}
	if apiErr.Message != "resource not found" {
		t.Errorf("Message = %q, want %q", apiErr.Message, "resource not found")
	}
	if apiErr.ExitCode() != ExitNotFound {
		t.Errorf("ExitCode = %d, want %d", apiErr.ExitCode(), ExitNotFound)
	}
}

func TestClient_NoRetryOn401(t *testing.T) {
	var callCount atomic.Int32
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail": "unauthorized"}`))
	}))
	defer apiServer.Close()

	c, cleanup := newTestClient(t, apiServer)
	defer cleanup()

	_, err := c.Get(context.Background(), "/v1/test", nil)
	if err == nil {
		t.Fatal("expected error")
	}

	if count := callCount.Load(); count != 1 {
		t.Errorf("server called %d times, want 1 (no retry on 401)", count)
	}
}

func TestClient_NoRetryOn403(t *testing.T) {
	var callCount atomic.Int32
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"detail": "forbidden"}`))
	}))
	defer apiServer.Close()

	c, cleanup := newTestClient(t, apiServer)
	defer cleanup()

	_, err := c.Get(context.Background(), "/v1/test", nil)
	if err == nil {
		t.Fatal("expected error")
	}

	if count := callCount.Load(); count != 1 {
		t.Errorf("server called %d times, want 1 (no retry on 403)", count)
	}
}

func TestClient_RetryOn429(t *testing.T) {
	var callCount atomic.Int32
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := callCount.Add(1)
		if count <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"detail": "rate limited"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success": true}`))
	}))
	defer apiServer.Close()

	c, cleanup := newTestClient(t, apiServer)
	defer cleanup()

	result, err := c.Get(context.Background(), "/v1/test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]bool
	_ = json.Unmarshal(result, &parsed)
	if !parsed["success"] {
		t.Error("expected success response after retry")
	}

	if count := callCount.Load(); count != 3 {
		t.Errorf("server called %d times, want 3 (2 retries + success)", count)
	}
}

func TestClient_RetryOn502(t *testing.T) {
	var callCount atomic.Int32
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := callCount.Add(1)
		if count == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"detail": "bad gateway"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	defer apiServer.Close()

	c, cleanup := newTestClient(t, apiServer)
	defer cleanup()

	_, err := c.Get(context.Background(), "/v1/test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if count := callCount.Load(); count != 2 {
		t.Errorf("server called %d times, want 2", count)
	}
}

func TestClient_MaxRetriesExhausted(t *testing.T) {
	var callCount atomic.Int32
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"detail": "service unavailable"}`))
	}))
	defer apiServer.Close()

	c, cleanup := newTestClient(t, apiServer)
	defer cleanup()

	_, err := c.Get(context.Background(), "/v1/test", nil)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}

	if count := callCount.Load(); count != maxRetries+1 {
		t.Errorf("server called %d times, want %d", count, maxRetries+1)
	}
}

func TestAPIError_ExitCodes(t *testing.T) {
	tests := []struct {
		status   int
		wantCode int
	}{
		{401, ExitAuth},
		{403, ExitAuth},
		{404, ExitNotFound},
		{400, ExitValidation},
		{422, ExitValidation},
		{500, ExitGeneral},
		{429, ExitGeneral},
	}

	for _, tt := range tests {
		apiErr := newAPIError(tt.status, "test", nil)
		if got := apiErr.ExitCode(); got != tt.wantCode {
			t.Errorf("status %d: ExitCode = %d, want %d", tt.status, got, tt.wantCode)
		}
	}
}

func TestAPIError_JSONSerializable(t *testing.T) {
	apiErr := &APIError{
		Type:       "not_found",
		Message:    "resource missing",
		StatusCode: 404,
		Retryable:  false,
	}

	data, err := json.Marshal(apiErr)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}

	var parsed APIError
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshaling: %v", err)
	}

	if parsed.Type != apiErr.Type {
		t.Errorf("Type = %q, want %q", parsed.Type, apiErr.Type)
	}
	if parsed.Message != apiErr.Message {
		t.Errorf("Message = %q, want %q", parsed.Message, apiErr.Message)
	}
	if parsed.StatusCode != apiErr.StatusCode {
		t.Errorf("StatusCode = %d, want %d", parsed.StatusCode, apiErr.StatusCode)
	}
}

func TestLocalErrorConstructors_ExitCodesAndJSON(t *testing.T) {
	tests := []struct {
		name     string
		err      *APIError
		wantType string
		wantExit int
	}{
		{"validation", NewLocalValidationError("bad input", "fix it"), TypeValidation, ExitValidation},
		{"unsupported", NewLocalExecutionUnsupportedError("nope", ""), TypeLocalExecutionUnsupported, ExitValidation},
		{"sm-auth", NewSecretManagerAuthenticationError("expired", "aws sso login --profile p"), TypeSecretManagerAuthentication, ExitAuth},
		{"sm-access", NewSecretManagerAccessError("denied", ""), TypeSecretManagerAccess, ExitAuth},
		{"not-found", NewSecretNotFoundError("missing", ""), TypeSecretNotFound, ExitNotFound},
		{"hydration", NewSecretHydrationError("bad payload", ""), TypeSecretHydration, ExitGeneral},
		{"connector", NewConnectorExecutionError("transport failed", ""), TypeConnectorExecutionError, ExitGeneral},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", tt.err.Type, tt.wantType)
			}
			if tt.err.Retryable {
				t.Error("local errors must not be retryable")
			}
			if got := tt.err.ExitCode(); got != tt.wantExit {
				t.Errorf("ExitCode = %d, want %d", got, tt.wantExit)
			}

			data, err := json.Marshal(tt.err)
			if err != nil {
				t.Fatalf("marshaling: %v", err)
			}
			var parsed APIError
			if err := json.Unmarshal(data, &parsed); err != nil {
				t.Fatalf("unmarshaling: %v", err)
			}
			if parsed.Type != tt.wantType {
				t.Errorf("round-tripped Type = %q, want %q", parsed.Type, tt.wantType)
			}
			if parsed.Message != tt.err.Message {
				t.Errorf("round-tripped Message = %q, want %q", parsed.Message, tt.err.Message)
			}
			if parsed.Hint != tt.err.Hint {
				t.Errorf("round-tripped Hint = %q, want %q", parsed.Hint, tt.err.Hint)
			}
		})
	}
}

func TestClient_RejectsExternalPaginationURL(t *testing.T) {
	creds := &auth.Credentials{ClientID: "id", ClientSecret: "secret"}
	tm := auth.NewTokenManager("https://api.example.com", "", creds)
	c := New("https://api.example.com", "", "test", tm)

	_, err := c.GetURL(context.Background(), "https://evil.example.com/api/v1/workspaces?cursor=next")
	if err == nil {
		t.Fatal("expected error for external pagination URL")
	}

	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.Type != "validation_error" {
		t.Errorf("Type = %q, want validation_error", apiErr.Type)
	}
}

func TestClient_RejectsSchemeRelativePaginationURL(t *testing.T) {
	creds := &auth.Credentials{ClientID: "id", ClientSecret: "secret"}
	tm := auth.NewTokenManager("https://api.example.com", "", creds)
	c := New("https://api.example.com", "", "test", tm)

	_, err := c.GetURL(context.Background(), "//evil.example.com/api/v1/workspaces?cursor=next")
	if err == nil {
		t.Fatal("expected error for scheme-relative external pagination URL")
	}

	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.Type != "validation_error" {
		t.Errorf("Type = %q, want validation_error", apiErr.Type)
	}
}

func TestClient_DebugFuncIsEvaluatedAtRequestTime(t *testing.T) {
	debug := false
	c := New("https://api.example.com", "", "test", nil, WithDebugFunc(func() bool { return debug }))

	if c.isDebug() {
		t.Fatal("debug should initially be false")
	}

	debug = true
	if !c.isDebug() {
		t.Fatal("debug should reflect updated flag state")
	}
}

func TestClient_ExecutionConfigDefaultsToHosted(t *testing.T) {
	// No resolver registered: defaults to hosted with no error.
	c := New("https://api.example.com", "", "test", nil)
	cfg, err := c.ExecutionConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Mode != config.ModeHosted {
		t.Errorf("Mode = %q, want hosted (default)", cfg.Mode)
	}

	// Nil receiver is safe and also hosted.
	var nilClient *Client
	nilCfg, err := nilClient.ExecutionConfig()
	if err != nil {
		t.Fatalf("unexpected nil-client error: %v", err)
	}
	if nilCfg.Mode != config.ModeHosted {
		t.Errorf("nil-client Mode = %q, want hosted", nilCfg.Mode)
	}
}

func TestClient_ExecutionConfigFuncIsEvaluatedAtRequestTime(t *testing.T) {
	mode := config.ModeHosted
	c := New("https://api.example.com", "", "test", nil,
		WithExecutionConfigFunc(func() (config.ExecutionConfig, error) {
			return config.ExecutionConfig{Mode: mode, AWSProfile: "p"}, nil
		}),
	)

	cfg, err := c.ExecutionConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Mode != config.ModeHosted {
		t.Fatalf("Mode = %q, want hosted before flag change", cfg.Mode)
	}

	// Simulate a flag parsed after client construction.
	mode = config.ModeLocal
	cfg, err = c.ExecutionConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Mode != config.ModeLocal {
		t.Errorf("Mode = %q, want local after deferred read", cfg.Mode)
	}
	if cfg.AWSProfile != "p" {
		t.Errorf("AWSProfile = %q, want p", cfg.AWSProfile)
	}
}

func TestClient_ExecutionConfigFuncPropagatesError(t *testing.T) {
	wantErr := &APIError{Type: "validation_error", Message: "bad mode", StatusCode: 400}
	c := New("https://api.example.com", "", "test", nil,
		WithExecutionConfigFunc(func() (config.ExecutionConfig, error) {
			return config.ExecutionConfig{}, wantErr
		}),
	)

	_, err := c.ExecutionConfig()
	if err == nil {
		t.Fatal("expected error to be propagated, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T", err)
	}
}

func TestRedactURL(t *testing.T) {
	got := redactURL("https://api.example.com/path?token=secret&client_secret=hidden&safe=value")
	if strings.Contains(got, "token=secret") || strings.Contains(got, "client_secret=hidden") {
		t.Fatalf("expected sensitive query values to be redacted, got %s", got)
	}
	if !strings.Contains(got, "safe=value") {
		t.Fatalf("expected non-sensitive query value to remain, got %s", got)
	}
}
