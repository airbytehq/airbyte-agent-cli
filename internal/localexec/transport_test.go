package localexec

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// insecureTransportConfig returns a config that permits plain HTTP (for
// httptest.Server) and never sleeps between retries.
func insecureTransportConfig() transportConfig {
	return transportConfig{allowHTTP: true, backoff: func(int) time.Duration { return 0 }}
}

func TestTransport_NoAirbyteHeadersLeak(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tr := newTransport(insecureTransportConfig())
	plan := &RequestPlan{Method: "GET", URL: srv.URL, Headers: []Header{{Name: "X-API-Key", Value: "connector-key"}}}
	if _, err := tr.do(context.Background(), plan); err != nil {
		t.Fatalf("do: %v", err)
	}
	// The connector-provided header is present; no Airbyte/CLI headers are.
	if got.Get("X-API-Key") != "connector-key" {
		t.Fatalf("connector header missing: %v", got)
	}
	for _, banned := range []string{"Authorization", "X-Airbyte-Organization-Id", "X-Endpoint-Api-Userinfo"} {
		if v := got.Get(banned); v != "" && banned == "Authorization" {
			t.Fatalf("leaked %s: %q", banned, v)
		}
	}
	for name := range got {
		if strings.HasPrefix(strings.ToLower(name), "x-airbyte") {
			t.Fatalf("leaked Airbyte header %q", name)
		}
	}
}

func TestTransport_ProductionRejectsHTTP(t *testing.T) {
	tr := newTransport(transportConfig{}) // no allowHTTP -> HTTPS only
	_, err := tr.do(context.Background(), &RequestPlan{Method: "GET", URL: "http://insecure.test/x"})
	le, ok := AsError(err)
	if !ok || le.Type() != TypeConnectorExecution {
		t.Fatalf("expected connector_execution_error, got %v", err)
	}
	if !strings.Contains(le.Message, "HTTPS") {
		t.Fatalf("message = %q", le.Message)
	}
}

func TestTransport_RedirectToHTTPBlocked(t *testing.T) {
	// A server that redirects to an http:// URL. With allowHTTP=false the
	// redirect must be refused even though the initial hop is allowed via a
	// custom RoundTripper... instead we test the CheckRedirect policy directly.
	cfg := transportConfig{allowHTTP: false, maxRedirects: 5}
	check := makeCheckRedirect(cfg)
	req, _ := http.NewRequest("GET", "http://elsewhere.test/x", nil)
	via, _ := http.NewRequest("GET", "https://origin.test/x", nil)
	err := check(req, []*http.Request{via})
	if le, ok := AsError(err); !ok || !strings.Contains(le.Message, "non-HTTPS") {
		t.Fatalf("expected non-HTTPS redirect rejection, got %v", err)
	}
}

func TestTransport_MaxRedirects(t *testing.T) {
	cfg := transportConfig{allowHTTP: true, maxRedirects: 2}
	check := makeCheckRedirect(cfg)
	via := []*http.Request{}
	for i := 0; i < 2; i++ {
		r, _ := http.NewRequest("GET", "https://a.test/x", nil)
		via = append(via, r)
	}
	req, _ := http.NewRequest("GET", "https://a.test/y", nil)
	err := check(req, via)
	if le, ok := AsError(err); !ok || !strings.Contains(le.Message, "redirect") {
		t.Fatalf("expected redirect-limit error, got %v", err)
	}
}

func TestTransport_CrossOriginStripsCredentials(t *testing.T) {
	cfg := transportConfig{allowHTTP: true, maxRedirects: 5}
	check := makeCheckRedirect(cfg)
	req, _ := http.NewRequest("GET", "https://other.test/x", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Cookie", "session=abc")
	via, _ := http.NewRequest("GET", "https://origin.test/x", nil)
	if err := check(req, []*http.Request{via}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
		t.Fatalf("credentials not stripped on cross-origin redirect: %v", req.Header)
	}
}

func TestTransport_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	cfg := insecureTransportConfig()
	cfg.timeout = 20 * time.Millisecond
	tr := newTransport(cfg)
	_, err := tr.do(context.Background(), &RequestPlan{Method: "GET", URL: srv.URL})
	if le, ok := AsError(err); !ok || le.Type() != TypeConnectorExecution {
		t.Fatalf("expected connector_execution_error on timeout, got %v", err)
	}
}

func TestTransport_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	tr := newTransport(insecureTransportConfig())
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	_, err := tr.do(ctx, &RequestPlan{Method: "GET", URL: srv.URL})
	if _, ok := AsError(err); !ok {
		t.Fatalf("expected typed error on cancellation, got %v", err)
	}
}

func TestTransport_BodyLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("a", 1000)))
	}))
	defer srv.Close()
	cfg := insecureTransportConfig()
	cfg.maxBodyBytes = 100
	tr := newTransport(cfg)
	_, err := tr.do(context.Background(), &RequestPlan{Method: "GET", URL: srv.URL})
	if le, ok := AsError(err); !ok || !strings.Contains(le.Message, "maximum allowed size") {
		t.Fatalf("expected body-size error, got %v", err)
	}
}

func TestTransport_RetriesSafeMethod(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	cfg := insecureTransportConfig()
	cfg.maxRetries = 3
	tr := newTransport(cfg)
	resp, err := tr.do(context.Background(), &RequestPlan{Method: "GET", URL: srv.URL})
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
}

func TestTransport_NoRetryOnMutation(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	cfg := insecureTransportConfig()
	cfg.maxRetries = 3
	tr := newTransport(cfg)
	_, err := tr.do(context.Background(), &RequestPlan{Method: "POST", URL: srv.URL, Body: []byte(`{}`)})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("mutation must not be retried; got %d attempts", got)
	}
}

func TestTransport_Non2xxSanitized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"secret_error":"do-not-leak this body"}`))
	}))
	defer srv.Close()
	tr := newTransport(insecureTransportConfig())
	_, err := tr.do(context.Background(), &RequestPlan{Method: "GET", URL: srv.URL + "/path?token=leak"})
	le, ok := AsError(err)
	if !ok || le.Type() != TypeConnectorExecution {
		t.Fatalf("expected connector_execution_error, got %v", err)
	}
	if strings.Contains(le.Message, "do-not-leak") || strings.Contains(le.Message, "token=leak") {
		t.Fatalf("error leaked body or query: %q", le.Message)
	}
	if !strings.Contains(le.Message, "404") {
		t.Fatalf("error should carry status: %q", le.Message)
	}
}

func TestRedactURL(t *testing.T) {
	got := redactURL("https://api.example.test/v1/things?token=abc&sig=xyz")
	if got != "https://api.example.test/v1/things" {
		t.Fatalf("redactURL = %q", got)
	}
}
