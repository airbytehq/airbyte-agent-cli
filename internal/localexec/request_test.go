package localexec

import (
	"reflect"
	"testing"
)

// buildFromFixture parses a fixture, resolves (entity, action), and compiles a
// request plan from the given inputs.
func buildFromFixture(t *testing.T, fixture, entity, action string, in Inputs) *RequestPlan {
	t.Helper()
	def, err := ParseDefinition(loadFixture(t, fixture))
	if err != nil {
		t.Fatalf("parse %s: %v", fixture, err)
	}
	r, err := def.ResolveOperation(entity, action)
	if err != nil {
		t.Fatalf("resolve %s/%s: %v", entity, action, err)
	}
	plan, err := BuildRequest(r, in)
	if err != nil {
		t.Fatalf("build %s/%s: %v", entity, action, err)
	}
	return plan
}

func TestRequestAPIKeyListGolden(t *testing.T) {
	plan := buildFromFixture(t, "apikey_list.yaml", "widget", "list", Inputs{
		Config: map[string]any{"version": "v1"},
		Params: map[string]any{
			"page_size": float64(100),
			"status":    "active",
			"tags":      []any{"a", "b"},
			"filter":    map[string]any{"priority": float64(2), "status": "open"},
		},
	})
	if plan.Method != "GET" {
		t.Errorf("method = %s", plan.Method)
	}
	wantURL := "https://api.example.test/v1/widgets?filter[priority]=2&filter[status]=open&page_size=100&status=active&tags=a&tags=b"
	if plan.URL != wantURL {
		t.Errorf("url =\n  %s\nwant\n  %s", plan.URL, wantURL)
	}
	if plan.Body != nil {
		t.Errorf("expected no body, got %q", plan.Body)
	}
	if len(plan.Headers) != 0 {
		t.Errorf("expected no headers, got %v", plan.Headers)
	}
}

func TestRequestGraphQLSearchGolden(t *testing.T) {
	plan := buildFromFixture(t, "graphql_search.yaml", "issue", "api_search", Inputs{
		Params: map[string]any{"term": "bug"},
	})
	if plan.Method != "POST" || plan.ContentType != "application/json" {
		t.Errorf("method/ct = %s %s", plan.Method, plan.ContentType)
	}
	if plan.URL != "https://graph.example.test/graphql" {
		t.Errorf("url = %s", plan.URL)
	}
	wantBody := `{"query":"query Search($term: String!) { search(term: $term) { id title } }","variables":{"term":"bug"}}`
	if string(plan.Body) != wantBody {
		t.Errorf("body =\n  %s\nwant\n  %s", plan.Body, wantBody)
	}
}

func TestRequestGraphQLExplicitVariables(t *testing.T) {
	plan := buildFromFixture(t, "graphql_search.yaml", "issue", "api_search", Inputs{
		Params: map[string]any{"variables": map[string]any{"term": "crash"}},
	})
	wantBody := `{"query":"query Search($term: String!) { search(term: $term) { id title } }","variables":{"term":"crash"}}`
	if string(plan.Body) != wantBody {
		t.Errorf("body = %s", plan.Body)
	}
}

func TestRequestBasicAuthCreateGolden(t *testing.T) {
	plan := buildFromFixture(t, "basicauth_write.yaml", "account", "create", Inputs{
		Params: map[string]any{"name": "Acme", "seats": float64(5)},
	})
	if plan.Method != "POST" || plan.ContentType != "application/json" {
		t.Errorf("method/ct = %s %s", plan.Method, plan.ContentType)
	}
	if plan.URL != "https://api.example.test/accounts" {
		t.Errorf("url = %s", plan.URL)
	}
	// plan defaults to "free"; keys are sorted by json.Marshal.
	wantBody := `{"name":"Acme","plan":"free","seats":5}`
	if string(plan.Body) != wantBody {
		t.Errorf("body = %s want %s", plan.Body, wantBody)
	}
}

func TestRequestBasicAuthUpdateGolden(t *testing.T) {
	plan := buildFromFixture(t, "basicauth_write.yaml", "account", "update", Inputs{
		Params: map[string]any{"account_id": "acc-123", "name": "Renamed"},
	})
	if plan.Method != "PATCH" {
		t.Errorf("method = %s", plan.Method)
	}
	if plan.URL != "https://api.example.test/accounts/acc-123" {
		t.Errorf("url = %s", plan.URL)
	}
	if string(plan.Body) != `{"name":"Renamed"}` {
		t.Errorf("body = %s", plan.Body)
	}
}

func TestRequestBasicAuthDeleteGolden(t *testing.T) {
	plan := buildFromFixture(t, "basicauth_write.yaml", "account", "delete", Inputs{
		Params: map[string]any{"account_id": "acc-123"},
	})
	if plan.Method != "DELETE" {
		t.Errorf("method = %s", plan.Method)
	}
	if plan.URL != "https://api.example.test/accounts/acc-123" {
		t.Errorf("url = %s", plan.URL)
	}
	if plan.Body != nil || plan.ContentType != "" {
		t.Errorf("expected empty body, got %q / %q", plan.Body, plan.ContentType)
	}
}

func TestRequestScopedServerHeaderCookieGolden(t *testing.T) {
	plan := buildFromFixture(t, "scoped_server.yaml", "record", "list", Inputs{
		Config: map[string]any{
			"credentials": map[string]any{"account": map[string]any{"subdomain": "acme"}},
			"settings":    map[string]any{"region": "eu"},
		},
		Params: map[string]any{"X-Trace-Id": "trace-1", "session": "sess-1"},
	})
	if plan.URL != "https://acme.example.test/eu/records" {
		t.Errorf("url = %s", plan.URL)
	}
	wantHeaders := []Header{{Name: "X-Trace-Id", Value: "trace-1"}}
	if !reflect.DeepEqual(plan.Headers, wantHeaders) {
		t.Errorf("headers = %v want %v", plan.Headers, wantHeaders)
	}
	wantCookies := []Cookie{{Name: "session", Value: "sess-1"}}
	if !reflect.DeepEqual(plan.Cookies, wantCookies) {
		t.Errorf("cookies = %v want %v", plan.Cookies, wantCookies)
	}
}

func TestRequestFormBodyGolden(t *testing.T) {
	plan := buildFromFixture(t, "form_body.yaml", "session", "authorize", Inputs{
		Params: map[string]any{"scope": "read write"},
	})
	if plan.Method != "POST" || plan.ContentType != "application/x-www-form-urlencoded" {
		t.Errorf("method/ct = %s %s", plan.Method, plan.ContentType)
	}
	wantBody := "grant_type=client_credentials&scope=read+write"
	if string(plan.Body) != wantBody {
		t.Errorf("body = %s want %s", plan.Body, wantBody)
	}
}

func TestRequestMultipartUploadGolden(t *testing.T) {
	plan := buildFromFixture(t, "multipart_upload.yaml", "upload", "create", Inputs{
		Params: map[string]any{"filename": "a.txt", "description": "hello"},
	})
	if plan.ContentType != "multipart/form-data; boundary="+multipartBoundary {
		t.Errorf("ct = %s", plan.ContentType)
	}
	b := multipartBoundary
	wantBody := "--" + b + "\r\nContent-Disposition: form-data; name=\"content_type\"\r\n\r\ntext/plain\r\n" +
		"--" + b + "\r\nContent-Disposition: form-data; name=\"description\"\r\n\r\nhello\r\n" +
		"--" + b + "\r\nContent-Disposition: form-data; name=\"filename\"\r\n\r\na.txt\r\n" +
		"--" + b + "--\r\n"
	if string(plan.Body) != wantBody {
		t.Errorf("body =\n%q\nwant\n%q", plan.Body, wantBody)
	}
}

func TestRequestValidationErrors(t *testing.T) {
	// Missing required body property.
	_, err := buildErr(t, "basicauth_write.yaml", "account", "create", Inputs{Params: map[string]any{}})
	assertValidationError(t, err)

	// Enum violation on a body property.
	_, err = buildErr(t, "basicauth_write.yaml", "account", "create", Inputs{
		Params: map[string]any{"name": "x", "plan": "enterprise"},
	})
	assertValidationError(t, err)

	// Type mismatch on a query parameter.
	_, err = buildErr(t, "apikey_list.yaml", "widget", "list", Inputs{
		Config: map[string]any{"version": "v1"},
		Params: map[string]any{"page_size": "not-an-int"},
	})
	assertValidationError(t, err)

	// deepObject given a non-object.
	_, err = buildErr(t, "apikey_list.yaml", "widget", "list", Inputs{
		Config: map[string]any{"version": "v1"},
		Params: map[string]any{"filter": "scalar"},
	})
	assertValidationError(t, err)

	// Missing required path parameter.
	_, err = buildErr(t, "basicauth_write.yaml", "account", "delete", Inputs{Params: map[string]any{}})
	assertValidationError(t, err)
}

func TestRequestServerVariableEnumEnforced(t *testing.T) {
	_, err := buildErr(t, "apikey_list.yaml", "widget", "list", Inputs{
		Config: map[string]any{"version": "v99"},
	})
	assertValidationError(t, err)
}

func TestRequestImmutablePlanValue(t *testing.T) {
	// The plan is a plain value; mutating a returned slice must not affect a
	// freshly built plan.
	plan := buildFromFixture(t, "apikey_list.yaml", "widget", "list", Inputs{
		Config: map[string]any{"version": "v1"},
		Params: map[string]any{"status": "active"},
	})
	if len(plan.Headers) != 0 {
		t.Fatalf("unexpected headers")
	}
	plan.Headers = append(plan.Headers, Header{Name: "X", Value: "y"})
	plan2 := buildFromFixture(t, "apikey_list.yaml", "widget", "list", Inputs{
		Config: map[string]any{"version": "v1"},
		Params: map[string]any{"status": "active"},
	})
	if len(plan2.Headers) != 0 {
		t.Fatalf("plan build not independent")
	}
}

// buildErr parses+resolves a fixture and returns the BuildRequest error.
func buildErr(t *testing.T, fixture, entity, action string, in Inputs) (*RequestPlan, error) {
	t.Helper()
	def, err := ParseDefinition(loadFixture(t, fixture))
	if err != nil {
		t.Fatalf("parse %s: %v", fixture, err)
	}
	r, err := def.ResolveOperation(entity, action)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return BuildRequest(r, in)
}
