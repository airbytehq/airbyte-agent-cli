package resources

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/airbytehq/airbyte-agent-cli/internal/auth"
	"github.com/airbytehq/airbyte-agent-cli/internal/client"
	"github.com/airbytehq/airbyte-agent-cli/internal/config"
	"github.com/airbytehq/airbyte-agent-cli/internal/localexec"
	"github.com/airbytehq/airbyte-agent-cli/internal/secrets"
)

// --- test seams ---------------------------------------------------------

// fakeProvider is a recording secrets.Provider. It counts Resolve calls (to
// prove validation precedes hydration) and can be configured to fail with a
// typed *secrets.Error (to exercise the error-translation paths).
type fakeExecProvider struct {
	calls   int32
	value   string
	failErr error
}

func (p *fakeExecProvider) Resolve(ctx context.Context, coordinate string) (string, error) {
	atomic.AddInt32(&p.calls, 1)
	if p.failErr != nil {
		return "", p.failErr
	}
	if p.value != "" {
		return p.value, nil
	}
	return "resolved", nil
}

func (p *fakeExecProvider) callCount() int32 { return atomic.LoadInt32(&p.calls) }

// fixedExecClock advances by step on each call so execution_time_ms is
// deterministic (start + one end call => exactly one step of elapsed time).
func fixedExecClock(step time.Duration) func() time.Time {
	base := time.Unix(1_700_000_000, 0)
	var n int64
	var mu sync.Mutex
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t := base.Add(time.Duration(n) * step)
		n++
		return t
	}
}

// execDefYAML is a minimal connector definition exposing widget/list. It sends
// the hydrated api_key as the X-API-Key header so hydration is observable.
func execDefYAML(baseURL string) string {
	return `
openapi: 3.0.0
servers:
  - url: ` + baseURL + `
components:
  securitySchemes:
    apiKey:
      type: apiKey
      in: header
      name: X-API-Key
      x-airbyte-auth:
        config_key: credentials.api_key
security:
  - apiKey: []
paths:
  /widgets:
    get:
      x-airbyte-entity: widget
      x-airbyte-action: list
      x-airbyte-record-selector: "$.data[*]"
      x-airbyte-record-meta:
        next: "$.meta.next"
      responses:
        "200":
          content:
            application/json:
              schema: {type: object}
`
}

// newLocalModeClient builds a client pointed at apiServer whose execution
// config resolves to local mode.
func newLocalModeClient(t *testing.T, apiServer *httptest.Server) (*client.Client, func()) {
	t.Helper()
	tokenServer := newTestTokenServer(t)
	creds := &auth.Credentials{ClientID: "id", ClientSecret: "secret"}
	tm := auth.NewTokenManager(tokenServer.URL, "", creds)
	c := client.New(apiServer.URL, "org-123", "test", tm,
		client.WithExecutionConfigFunc(func() (config.ExecutionConfig, error) {
			return config.ExecutionConfig{Mode: config.ModeLocal}, nil
		}),
	)
	return c, func() { tokenServer.Close() }
}

// installExecSeams overrides the provider/executor factories for the duration
// of a test and returns a restore func. The executor is wired to hit connSrv
// via its client transport with insecure HTTP allowed and a deterministic
// clock.
func installExecSeams(provider secrets.Provider, connSrv *httptest.Server) func() {
	origProvider := newSecretsProvider
	origExecutor := newLocalExecutor
	newSecretsProvider = func(ec config.ExecutionConfig) secrets.Provider { return provider }
	newLocalExecutor = func(p secrets.Provider) *localexec.Executor {
		return localexec.NewExecutor(p,
			localexec.WithRoundTripper(connSrv.Client().Transport),
			localexec.WithInsecureHTTP(),
			localexec.WithClock(fixedExecClock(5*time.Millisecond)),
		)
	}
	return func() {
		newSecretsProvider = origProvider
		newLocalExecutor = origExecutor
	}
}

// prepareEnvelope builds a valid prepare-response envelope JSON carrying a
// bundle whose definition points at connURL.
func prepareEnvelope(connURL string, mutate func(env map[string]any)) []byte {
	bundle := map[string]any{
		"connector_definition_id": "def-1",
		"definition_yaml":         execDefYAML(connURL),
		"entity":                  "widget",
		"action":                  "list",
		"source_config": map[string]any{
			"credentials": map[string]any{"api_key": secrets.CoordinatePrefix + "ref/api-key"},
		},
	}
	env := map[string]any{
		"status":             "success",
		"connector_metadata": map[string]any{"source": "widget"},
		"execution_metadata": map[string]any{"connector_instance_id": "ci-123", "execution_time_ms": 999},
		"warning":            "heads up",
		"bundle":             bundle,
	}
	if mutate != nil {
		mutate(env)
	}
	b, _ := json.Marshal(env)
	return b
}

const (
	execExactPath   = "/api/v1/integrations/connectors/conn-1/execute"
	preparePath     = "/api/v1/integrations/connectors/conn-1/execute/prepare"
	recordsResponse = `{"data":[{"id":1,"name":"a"},{"id":2,"name":"b"}],"meta":{"next":"c2"}}`
)

// --- hosted default -----------------------------------------------------

func TestConnectorsExecute_HostedDefault(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","result":{"data":[]}}`))
	}))
	defer apiServer.Close()

	// Default client (no execution-config resolver) => hosted.
	c, cleanup := newTestClient(t, apiServer)
	defer cleanup()

	result, err := connectorsExecute(context.Background(), c, map[string]any{
		"id":     "conn-1",
		"entity": "contacts",
		"action": "list",
		"intent": "why",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != execExactPath {
		t.Errorf("hosted path = %q, want %q", gotPath, execExactPath)
	}
	if gotBody["intent"] != "why" {
		t.Errorf("hosted body intent = %v, want why", gotBody["intent"])
	}
	if _, ok := result.(json.RawMessage); !ok {
		t.Fatalf("hosted result type = %T, want json.RawMessage (raw passthrough)", result)
	}
}

// --- local prepare path + body ------------------------------------------

func TestConnectorsExecute_LocalPreparePathAndBody(t *testing.T) {
	connSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(recordsResponse))
	}))
	defer connSrv.Close()

	var gotPreparePath string
	var gotBody map[string]any
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == execExactPath {
			t.Errorf("hosted /execute must NOT be called in local mode")
		}
		gotPreparePath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(prepareEnvelope(connSrv.URL, nil))
	}))
	defer apiServer.Close()

	provider := &fakeExecProvider{value: "top-secret-key"}
	defer installExecSeams(provider, connSrv)()

	c, cleanup := newLocalModeClient(t, apiServer)
	defer cleanup()

	_, err := connectorsExecute(context.Background(), c, map[string]any{
		"id":            "conn-1",
		"entity":        "widget",
		"action":        "list",
		"select_fields": []string{"name"},
		"intent":        "audit-me",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPreparePath != preparePath {
		t.Errorf("prepare path = %q, want %q", gotPreparePath, preparePath)
	}
	// Same body, including intent, is sent to prepare.
	if gotBody["entity"] != "widget" || gotBody["action"] != "list" {
		t.Errorf("prepare body entity/action = %v/%v", gotBody["entity"], gotBody["action"])
	}
	if gotBody["intent"] != "audit-me" {
		t.Errorf("prepare body intent = %v, want audit-me", gotBody["intent"])
	}
	if gotBody["select_fields"] == nil {
		t.Error("prepare body missing select_fields")
	}
}

// --- returned envelope --------------------------------------------------

func TestConnectorsExecute_LocalReturnedEnvelope(t *testing.T) {
	var gotAPIKey string
	connSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(recordsResponse))
	}))
	defer connSrv.Close()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(prepareEnvelope(connSrv.URL, nil))
	}))
	defer apiServer.Close()

	provider := &fakeExecProvider{value: "top-secret-key"}
	defer installExecSeams(provider, connSrv)()

	c, cleanup := newLocalModeClient(t, apiServer)
	defer cleanup()

	result, err := connectorsExecute(context.Background(), c, map[string]any{
		"id": "conn-1", "entity": "widget", "action": "list",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAPIKey != "top-secret-key" {
		t.Fatalf("connector did not receive hydrated key, got %q", gotAPIKey)
	}

	// The result must marshal cleanly and expose the standard envelope.
	out, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("local result type = %T, want map[string]any", result)
	}
	if _, present := out["bundle"]; present {
		t.Error("bundle must be removed from the returned envelope")
	}
	if out["status"] != "success" {
		t.Errorf("status = %v, want success", out["status"])
	}
	if out["warning"] != "heads up" {
		t.Errorf("warning = %v, want preserved", out["warning"])
	}

	execMeta, _ := out["execution_metadata"].(map[string]any)
	if execMeta["connector_instance_id"] != "ci-123" {
		t.Errorf("connector_instance_id = %v, want preserved ci-123", execMeta["connector_instance_id"])
	}
	// execution_time_ms replaced with the local total (5ms), not the prepare's 999.
	if got := execMeta["execution_time_ms"]; got != int64(5) {
		t.Errorf("execution_time_ms = %v (type %T), want local total int64(5)", got, got)
	}

	res, _ := out["result"].(map[string]any)
	if res["record_count"] != 2 {
		t.Errorf("result.record_count = %v, want 2", res["record_count"])
	}
	records, _ := res["records"].([]any)
	if len(records) != 2 {
		t.Errorf("result.records len = %d, want 2", len(records))
	}
	if meta, _ := res["metadata"].(map[string]any); meta["next"] != "c2" {
		t.Errorf("result.metadata.next = %v, want c2", meta)
	}

	// Confirm the whole envelope marshals to JSON without error.
	if _, err := json.Marshal(out); err != nil {
		t.Fatalf("returned envelope not JSON-marshalable: %v", err)
	}
}

// --- missing bundle -----------------------------------------------------

func TestConnectorsExecute_LocalMissingBundle(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Valid envelope JSON but no bundle.
		_, _ = w.Write([]byte(`{"status":"success","connector_metadata":{},"execution_metadata":{"connector_instance_id":"ci","execution_time_ms":1},"bundle":null}`))
	}))
	defer apiServer.Close()

	provider := &fakeExecProvider{}
	connSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer connSrv.Close()
	defer installExecSeams(provider, connSrv)()

	c, cleanup := newLocalModeClient(t, apiServer)
	defer cleanup()

	_, err := connectorsExecute(context.Background(), c, map[string]any{"id": "conn-1", "entity": "widget", "action": "list"})
	assertAPIExit(t, err, client.TypeValidation, client.ExitValidation)
	if provider.callCount() != 0 {
		t.Errorf("provider must not be called when bundle is missing, got %d calls", provider.callCount())
	}
}

// --- failure stages -----------------------------------------------------

func TestConnectorsExecute_LocalPrepareError(t *testing.T) {
	var hostedHit bool
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == execExactPath {
			hostedHit = true
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"prepare boom"}`))
	}))
	defer apiServer.Close()

	provider := &fakeExecProvider{}
	connSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer connSrv.Close()
	defer installExecSeams(provider, connSrv)()

	c, cleanup := newLocalModeClient(t, apiServer)
	defer cleanup()

	_, err := connectorsExecute(context.Background(), c, map[string]any{"id": "conn-1", "entity": "widget", "action": "list"})
	if err == nil {
		t.Fatal("expected prepare error")
	}
	apiErr, ok := err.(*client.APIError)
	if !ok {
		t.Fatalf("expected *client.APIError from prepare, got %T", err)
	}
	if apiErr.StatusCode != 500 {
		t.Errorf("prepare error StatusCode = %d, want 500", apiErr.StatusCode)
	}
	if hostedHit {
		t.Error("no-fallback violated: hosted /execute was called after prepare failed")
	}
	if provider.callCount() != 0 {
		t.Error("provider must not be called when prepare fails")
	}
}

func TestConnectorsExecute_LocalDecodeError(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{bad json`))
	}))
	defer apiServer.Close()

	provider := &fakeExecProvider{}
	connSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer connSrv.Close()
	defer installExecSeams(provider, connSrv)()

	c, cleanup := newLocalModeClient(t, apiServer)
	defer cleanup()

	_, err := connectorsExecute(context.Background(), c, map[string]any{"id": "conn-1", "entity": "widget", "action": "list"})
	assertAPIExit(t, err, client.TypeValidation, client.ExitValidation)
	if provider.callCount() != 0 {
		t.Error("provider must not be called when decode fails")
	}
}

func TestConnectorsExecute_LocalProviderErrorPreservesHint(t *testing.T) {
	connSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(recordsResponse))
	}))
	defer connSrv.Close()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(prepareEnvelope(connSrv.URL, nil))
	}))
	defer apiServer.Close()

	const hint = "aws sso login --profile prod"
	provider := &fakeExecProvider{failErr: &secrets.Error{
		Type:    secrets.ErrAuthentication,
		Message: "cached SSO token expired",
		Hint:    hint,
	}}
	defer installExecSeams(provider, connSrv)()

	c, cleanup := newLocalModeClient(t, apiServer)
	defer cleanup()

	_, err := connectorsExecute(context.Background(), c, map[string]any{"id": "conn-1", "entity": "widget", "action": "list"})
	apiErr := assertAPIExit(t, err, client.TypeSecretManagerAuthentication, client.ExitAuth)
	if apiErr.Hint != hint {
		t.Errorf("hint = %q, want %q (SSO remediation must pass through)", apiErr.Hint, hint)
	}
	if provider.callCount() == 0 {
		t.Error("provider should have been called during hydration")
	}
}

func TestConnectorsExecute_LocalUnsupportedIsExit4AndValidationBeforeHydration(t *testing.T) {
	connSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("connector must not be reached for an unsupported bundle")
	}))
	defer connSrv.Close()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A bundle whose action is not defined => unsupported before hydration.
		_, _ = w.Write(prepareEnvelope(connSrv.URL, func(env map[string]any) {
			env["bundle"].(map[string]any)["action"] = "download"
		}))
	}))
	defer apiServer.Close()

	provider := &fakeExecProvider{}
	defer installExecSeams(provider, connSrv)()

	c, cleanup := newLocalModeClient(t, apiServer)
	defer cleanup()

	_, err := connectorsExecute(context.Background(), c, map[string]any{"id": "conn-1", "entity": "widget", "action": "download"})
	if err == nil {
		t.Fatal("expected unsupported error")
	}
	apiErr, ok := err.(*client.APIError)
	if !ok {
		t.Fatalf("expected *client.APIError, got %T", err)
	}
	if apiErr.ExitCode() != client.ExitValidation {
		t.Errorf("exit = %d, want %d", apiErr.ExitCode(), client.ExitValidation)
	}
	// Validation-before-hydration: the provider is never consulted.
	if provider.callCount() != 0 {
		t.Errorf("provider called %d times; static validation must precede hydration", provider.callCount())
	}
}

func TestConnectorsExecute_LocalConnectorTransportErrorIsExit1(t *testing.T) {
	connSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"upstream down"}`))
	}))
	defer connSrv.Close()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(prepareEnvelope(connSrv.URL, nil))
	}))
	defer apiServer.Close()

	provider := &fakeExecProvider{value: "k"}
	defer installExecSeams(provider, connSrv)()

	c, cleanup := newLocalModeClient(t, apiServer)
	defer cleanup()

	_, err := connectorsExecute(context.Background(), c, map[string]any{"id": "conn-1", "entity": "widget", "action": "list"})
	assertAPIExit(t, err, client.TypeConnectorExecutionError, client.ExitGeneral)
}

// --- cancellation -------------------------------------------------------

func TestConnectorsExecute_LocalCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	connSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Cancel the client context mid-flight, then wait for the client to
		// abort so the transport surfaces a cancellation.
		cancel()
		<-r.Context().Done()
	}))
	defer connSrv.Close()

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(prepareEnvelope(connSrv.URL, nil))
	}))
	defer apiServer.Close()

	provider := &fakeExecProvider{value: "k"}
	defer installExecSeams(provider, connSrv)()

	c, cleanup := newLocalModeClient(t, apiServer)
	defer cleanup()

	_, err := connectorsExecute(ctx, c, map[string]any{"id": "conn-1", "entity": "widget", "action": "list"})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
}

// --- no fallback on executor failure ------------------------------------

func TestConnectorsExecute_LocalNoFallbackToHosted(t *testing.T) {
	connSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer connSrv.Close()

	var hostedHit bool
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == execExactPath {
			hostedHit = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(prepareEnvelope(connSrv.URL, nil))
	}))
	defer apiServer.Close()

	provider := &fakeExecProvider{value: "k"}
	defer installExecSeams(provider, connSrv)()

	c, cleanup := newLocalModeClient(t, apiServer)
	defer cleanup()

	_, err := connectorsExecute(context.Background(), c, map[string]any{"id": "conn-1", "entity": "widget", "action": "list"})
	if err == nil {
		t.Fatal("expected connector execution error")
	}
	if hostedHit {
		t.Error("no-fallback violated: hosted /execute was called after a local failure")
	}
}

// --- validation error before hydration (invalid execution mode) ---------

func TestConnectorsExecute_InvalidExecutionModeIsValidationError(t *testing.T) {
	var apiHit bool
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiHit = true
	}))
	defer apiServer.Close()

	tokenServer := newTestTokenServer(t)
	defer tokenServer.Close()
	creds := &auth.Credentials{ClientID: "id", ClientSecret: "secret"}
	tm := auth.NewTokenManager(tokenServer.URL, "", creds)
	c := client.New(apiServer.URL, "org-123", "test", tm,
		client.WithExecutionConfigFunc(func() (config.ExecutionConfig, error) {
			return config.ExecutionConfig{}, &config.ValidationError{Message: "invalid execution mode"}
		}),
	)

	_, err := connectorsExecute(context.Background(), c, map[string]any{"id": "conn-1", "entity": "widget", "action": "list"})
	assertAPIExit(t, err, client.TypeValidation, client.ExitValidation)
	if apiHit {
		t.Error("no API call should be made when execution config is invalid")
	}
}

// --- helper -------------------------------------------------------------

func assertAPIExit(t *testing.T, err error, wantType string, wantExit int) *client.APIError {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	apiErr, ok := err.(*client.APIError)
	if !ok {
		t.Fatalf("expected *client.APIError, got %T (%v)", err, err)
	}
	if apiErr.Type != wantType {
		t.Errorf("Type = %q, want %q", apiErr.Type, wantType)
	}
	if got := apiErr.ExitCode(); got != wantExit {
		t.Errorf("ExitCode = %d, want %d", got, wantExit)
	}
	if apiErr.Retryable {
		t.Error("local errors must not be retryable")
	}
	return apiErr
}
