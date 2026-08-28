package localexec

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/airbytehq/airbyte-agent-cli/internal/secrets"
)

// fakeProvider is a recording secrets.Provider. It counts Resolve calls (to
// prove validation precedes hydration) and returns canned values keyed by
// coordinate suffix.
type fakeProvider struct {
	mu     sync.Mutex
	calls  int32
	values map[string]string
}

func newFakeProvider(values map[string]string) *fakeProvider {
	return &fakeProvider{values: values}
}

func (p *fakeProvider) Resolve(ctx context.Context, coordinate string) (string, error) {
	atomic.AddInt32(&p.calls, 1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := p.values[coordinate]; ok {
		return v, nil
	}
	return "resolved:" + coordinate, nil
}

func (p *fakeProvider) callCount() int32 { return atomic.LoadInt32(&p.calls) }

// fixedClock returns a clock that advances by step on each call.
func fixedClock(step time.Duration) func() time.Time {
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

func TestExecutor_EndToEnd(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":1,"name":"a"},{"id":2,"name":"b"}],"meta":{"next":"c2"}}`))
	}))
	defer srv.Close()

	provider := newFakeProvider(map[string]string{"ref/api-key": "top-secret-key"})
	exec := NewExecutor(provider,
		WithInsecureHTTP(),
		WithRoundTripper(srv.Client().Transport),
		WithClock(fixedClock(5*time.Millisecond)),
	)

	bundle := &ExecuteBundle{
		ConnectorDefinitionID: "def-1",
		DefinitionYAML:        e2eDefYAML(srv.URL),
		Entity:                "widget",
		Action:                "list",
		SourceConfig: map[string]any{
			"credentials": map[string]any{"api_key": secrets.CoordinatePrefix + "ref/api-key"},
		},
	}

	res, err := exec.Execute(context.Background(), bundle)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotHeader != "top-secret-key" {
		t.Fatalf("connector did not receive hydrated key, got %q", gotHeader)
	}
	if res.RecordCount != 2 {
		t.Fatalf("record count = %d", res.RecordCount)
	}
	if res.Metadata["next"] != "c2" {
		t.Fatalf("meta next = %v", res.Metadata["next"])
	}
	if res.ExecutionTimeMs <= 0 {
		t.Fatalf("execution_time_ms should be positive, got %d", res.ExecutionTimeMs)
	}
	if provider.callCount() != 1 {
		t.Fatalf("expected exactly 1 provider call, got %d", provider.callCount())
	}
}

func TestExecutor_ValidationBeforeHydration_ZeroProviderCalls(t *testing.T) {
	cases := []struct {
		name   string
		bundle *ExecuteBundle
	}{
		{
			name: "unsupported action",
			bundle: &ExecuteBundle{
				ConnectorDefinitionID: "d", DefinitionYAML: e2eDefYAML("https://api.example.test"),
				Entity: "widget", Action: "download",
				SourceConfig: map[string]any{"credentials": map[string]any{"api_key": secrets.CoordinatePrefix + "ref/x"}},
			},
		},
		{
			name: "unknown entity/action",
			bundle: &ExecuteBundle{
				ConnectorDefinitionID: "d", DefinitionYAML: e2eDefYAML("https://api.example.test"),
				Entity: "ghost", Action: "list",
				SourceConfig: map[string]any{"credentials": map[string]any{"api_key": secrets.CoordinatePrefix + "ref/x"}},
			},
		},
		{
			name: "malformed record-selector JSONPath",
			bundle: &ExecuteBundle{
				ConnectorDefinitionID: "d", DefinitionYAML: badSelectorDefYAML("https://api.example.test"),
				Entity: "widget", Action: "list",
				SourceConfig: map[string]any{"credentials": map[string]any{"api_key": secrets.CoordinatePrefix + "ref/x"}},
			},
		},
		{
			name: "refreshable OAuth",
			bundle: &ExecuteBundle{
				ConnectorDefinitionID: "d", DefinitionYAML: refreshOAuthDefYAML(),
				Entity: "widget", Action: "list",
				SourceConfig: map[string]any{"credentials": map[string]any{"api_key": secrets.CoordinatePrefix + "ref/x"}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := newFakeProvider(nil)
			exec := NewExecutor(provider, WithInsecureHTTP())
			_, err := exec.Execute(context.Background(), tc.bundle)
			if err == nil {
				t.Fatal("expected error")
			}
			if _, ok := AsError(err); !ok {
				t.Fatalf("expected typed *Error, got %T: %v", err, err)
			}
			if provider.callCount() != 0 {
				t.Fatalf("provider must NOT be called for invalid input; got %d calls", provider.callCount())
			}
		})
	}
}

func TestExecutor_ErrorClassification(t *testing.T) {
	// unsupported action -> exit 4
	provider := newFakeProvider(nil)
	exec := NewExecutor(provider)
	_, err := exec.Execute(context.Background(), &ExecuteBundle{
		ConnectorDefinitionID: "d", DefinitionYAML: e2eDefYAML("https://api.example.test"),
		Entity: "widget", Action: "download",
	})
	if le, _ := AsError(err); le == nil || le.ExitCode() != 4 {
		t.Fatalf("unsupported action should be exit 4, got %v", err)
	}
}

func TestExecutor_ConnectorErrorRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"leak-this-secret-detail"}`))
	}))
	defer srv.Close()
	provider := newFakeProvider(nil)
	exec := NewExecutor(provider, WithInsecureHTTP(), WithRoundTripper(srv.Client().Transport))
	_, err := exec.Execute(context.Background(), &ExecuteBundle{
		ConnectorDefinitionID: "d", DefinitionYAML: noAuthDefYAML(srv.URL),
		Entity: "widget", Action: "list",
	})
	le, ok := AsError(err)
	if !ok || le.Type() != TypeConnectorExecution || le.ExitCode() != 1 {
		t.Fatalf("expected connector_execution_error exit 1, got %v", err)
	}
	if strings.Contains(le.Message, "leak-this-secret-detail") {
		t.Fatalf("connector error leaked body: %q", le.Message)
	}
}

func TestExecutor_Cancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	provider := newFakeProvider(nil)
	exec := NewExecutor(provider, WithInsecureHTTP(), WithRoundTripper(srv.Client().Transport))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	_, err := exec.Execute(ctx, &ExecuteBundle{
		ConnectorDefinitionID: "d", DefinitionYAML: noAuthDefYAML(srv.URL),
		Entity: "widget", Action: "list",
	})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestExecutor_ConcurrentInvocations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Echo back the received key so a shared-state bug would surface.
		_, _ = w.Write([]byte(`{"data":[{"key":"` + r.Header.Get("X-API-Key") + `"}]}`))
	}))
	defer srv.Close()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			provider := newFakeProvider(map[string]string{"ref/k": "key-shared"})
			exec := NewExecutor(provider, WithInsecureHTTP(), WithRoundTripper(srv.Client().Transport))
			bundle := &ExecuteBundle{
				ConnectorDefinitionID: "d", DefinitionYAML: e2eDefYAML(srv.URL),
				Entity: "widget", Action: "list",
				SourceConfig: map[string]any{"credentials": map[string]any{"api_key": secrets.CoordinatePrefix + "ref/k"}},
			}
			res, err := exec.Execute(context.Background(), bundle)
			if err != nil {
				t.Errorf("Execute: %v", err)
				return
			}
			rec := res.Records[0].(map[string]any)
			if rec["key"] != "key-shared" {
				t.Errorf("unexpected key %v", rec["key"])
			}
		}(i)
	}
	wg.Wait()
}

// --- test definitions -------------------------------------------------------

func e2eDefYAML(baseURL string) string {
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

func noAuthDefYAML(baseURL string) string {
	return `
openapi: 3.0.0
servers:
  - url: ` + baseURL + `
paths:
  /widgets:
    get:
      x-airbyte-entity: widget
      x-airbyte-action: list
      x-airbyte-record-selector: "$.data[*]"
      responses:
        "200":
          content:
            application/json:
              schema: {type: object}
`
}

func badSelectorDefYAML(baseURL string) string {
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
      x-airbyte-record-selector: "$..bad"
      responses:
        "200":
          content:
            application/json:
              schema: {type: object}
`
}

func refreshOAuthDefYAML() string {
	return `
openapi: 3.0.0
servers:
  - url: https://api.example.test
components:
  securitySchemes:
    oauth:
      type: oauth2
      flows:
        authorizationCode:
          authorizationUrl: https://a.test/auth
          tokenUrl: https://a.test/token
          refreshUrl: https://a.test/refresh
security:
  - oauth: []
paths:
  /widgets:
    get:
      x-airbyte-entity: widget
      x-airbyte-action: list
      responses:
        "200":
          content:
            application/json:
              schema: {type: object}
`
}
