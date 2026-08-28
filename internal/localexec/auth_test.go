package localexec

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
)

// resolveAuthForYAML parses a definition, resolves the (entity, action), and
// returns the compiled auth spec plus the resolved operation.
func resolveAuthForYAML(t *testing.T, yaml, entity, action string) (*authSpec, *ResolvedOperation) {
	t.Helper()
	def, err := ParseDefinition(yaml)
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	op, err := def.ResolveOperation(entity, action)
	if err != nil {
		t.Fatalf("ResolveOperation: %v", err)
	}
	spec, err := resolveAuthSpec(op)
	if err != nil {
		t.Fatalf("resolveAuthSpec: %v", err)
	}
	return spec, op
}

// basePlan is a minimal plan the auth layer merges into.
func basePlan() *RequestPlan {
	return &RequestPlan{Method: "GET", URL: "https://api.example.test/v1/widgets"}
}

func headerValue(p *RequestPlan, name string) (string, bool) {
	for _, h := range p.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value, true
		}
	}
	return "", false
}

func cookieValue(p *RequestPlan, name string) (string, bool) {
	for _, c := range p.Cookies {
		if c.Name == name {
			return c.Value, true
		}
	}
	return "", false
}

func TestApplyAuth_APIKeyHeader_Identity(t *testing.T) {
	spec, _ := resolveAuthForYAML(t, apikeyListYAML, "widget", "list")
	// Identity mapping: no replication_auth_key_mapping; config_key path resolves
	// directly against the hydrated source config.
	src := map[string]any{"credentials": map[string]any{"api_key": "secret-value"}}
	authed, err := applyAuth(spec, basePlan(), src, nil)
	if err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	got, ok := headerValue(authed, "X-API-Key")
	if !ok || got != "secret-value" {
		t.Fatalf("X-API-Key = %q, %v", got, ok)
	}
}

func TestApplyAuth_APIKeyHeader_NestedMapping(t *testing.T) {
	spec, _ := resolveAuthForYAML(t, apikeyListYAML, "widget", "list")
	// The logical key credentials.api_key is remapped (nested) to a different
	// source-config location.
	src := map[string]any{"auth": map[string]any{"token": "mapped-value"}}
	mapping := map[string]any{"credentials": map[string]any{"api_key": "auth.token"}}
	authed, err := applyAuth(spec, basePlan(), src, mapping)
	if err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	if got, _ := headerValue(authed, "X-API-Key"); got != "mapped-value" {
		t.Fatalf("X-API-Key = %q", got)
	}
}

func TestApplyAuth_APIKeyHeader_DirectFlatMapping(t *testing.T) {
	spec, _ := resolveAuthForYAML(t, apikeyListYAML, "widget", "list")
	src := map[string]any{"top_secret": "flat-value"}
	// A flat/direct mapping keyed by the literal dotted logical key.
	mapping := map[string]any{"credentials.api_key": "top_secret"}
	authed, err := applyAuth(spec, basePlan(), src, mapping)
	if err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	if got, _ := headerValue(authed, "X-API-Key"); got != "flat-value" {
		t.Fatalf("X-API-Key = %q", got)
	}
}

func TestApplyAuth_Bearer(t *testing.T) {
	spec, _ := resolveAuthForYAML(t, graphqlSearchYAML, "issue", "api_search")
	src := map[string]any{"credentials": map[string]any{"access_token": "jwt-token"}}
	authed, err := applyAuth(spec, basePlan(), src, nil)
	if err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	if got, _ := headerValue(authed, "Authorization"); got != "Bearer jwt-token" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestApplyAuth_Basic(t *testing.T) {
	spec, _ := resolveAuthForYAML(t, basicauthWriteYAML, "account", "create")
	src := map[string]any{"credentials": map[string]any{"username": "alice", "password": "p@ss"}}
	authed, err := applyAuth(spec, basePlan(), src, nil)
	if err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	got, ok := headerValue(authed, "Authorization")
	if !ok {
		t.Fatal("missing Authorization header")
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:p@ss"))
	if got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

func TestApplyAuth_APIKeyQuery(t *testing.T) {
	spec, _ := resolveAuthForYAML(t, apikeyQueryYAML, "thing", "list")
	src := map[string]any{"credentials": map[string]any{"api_key": "qk"}}
	plan := &RequestPlan{Method: "GET", URL: "https://api.example.test/v1/things?page=1"}
	authed, err := applyAuth(spec, plan, src, nil)
	if err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	u, _ := url.Parse(authed.URL)
	if got := u.Query().Get("api_key"); got != "qk" {
		t.Fatalf("query api_key = %q (url=%s)", got, authed.URL)
	}
	if got := u.Query().Get("page"); got != "1" {
		t.Fatalf("existing query param page dropped: %q", got)
	}
}

func TestApplyAuth_APIKeyCookie(t *testing.T) {
	spec, _ := resolveAuthForYAML(t, apikeyCookieYAML, "thing", "list")
	src := map[string]any{"credentials": map[string]any{"api_key": "ck"}}
	authed, err := applyAuth(spec, basePlan(), src, nil)
	if err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	if got, _ := cookieValue(authed, "session"); got != "ck" {
		t.Fatalf("cookie session = %q", got)
	}
}

func TestApplyAuth_MultipleAlternatives_PicksFirstSatisfiable(t *testing.T) {
	spec, _ := resolveAuthForYAML(t, multiAltYAML, "thing", "list")
	// Only the bearer alternative is satisfiable (no apiKey value present).
	src := map[string]any{"credentials": map[string]any{"access_token": "tok"}}
	authed, err := applyAuth(spec, basePlan(), src, nil)
	if err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	if _, ok := headerValue(authed, "X-API-Key"); ok {
		t.Fatal("unexpected apiKey header applied")
	}
	if got, _ := headerValue(authed, "Authorization"); got != "Bearer tok" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestApplyAuth_MissingField_RedactedError(t *testing.T) {
	spec, _ := resolveAuthForYAML(t, apikeyListYAML, "widget", "list")
	// No credential present anywhere.
	_, err := applyAuth(spec, basePlan(), map[string]any{}, nil)
	if err == nil {
		t.Fatal("expected error for missing auth field")
	}
	le, ok := AsError(err)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	// The error may name the scheme but must never leak a resolved value.
	if !strings.Contains(le.Message, "apiKey") {
		t.Fatalf("error should name the scheme: %q", le.Message)
	}
}

func TestApplyAuth_DoesNotMutateOriginalPlan(t *testing.T) {
	spec, _ := resolveAuthForYAML(t, apikeyListYAML, "widget", "list")
	src := map[string]any{"credentials": map[string]any{"api_key": "v"}}
	plan := basePlan()
	if _, err := applyAuth(spec, plan, src, nil); err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	if len(plan.Headers) != 0 {
		t.Fatalf("original plan was mutated: %+v", plan.Headers)
	}
}

func TestResolveAuthSpec_RejectsRefreshableOAuth(t *testing.T) {
	// Defense in depth: even if a refreshable scheme reached resolveAuthSpec, it
	// must be rejected as unsupported. (ParseDefinition also rejects it earlier.)
	def := &Definition{
		Security: []map[string][]string{{"oauth": nil}},
		Components: Components{SecuritySchemes: map[string]*SecurityScheme{
			"oauth": {Type: "oauth2", OAuthRefresh: true},
		}},
	}
	op := &ResolvedOperation{Definition: def, Operation: &Operation{}}
	_, err := resolveAuthSpec(op)
	le, ok := AsError(err)
	if !ok || le.Type() != TypeUnsupported {
		t.Fatalf("expected unsupported error, got %v", err)
	}
}

func TestResolveAuthSpec_UndefinedScheme(t *testing.T) {
	def := &Definition{
		Security:   []map[string][]string{{"ghost": nil}},
		Components: Components{SecuritySchemes: map[string]*SecurityScheme{}},
	}
	op := &ResolvedOperation{Definition: def, Operation: &Operation{}}
	_, err := resolveAuthSpec(op)
	le, ok := AsError(err)
	if !ok || le.Type() != TypeValidation {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestApplyAuth_StaticOAuth2AccessToken(t *testing.T) {
	spec, _ := resolveAuthForYAML(t, oauth2StaticYAML, "thing", "list")
	src := map[string]any{"credentials": map[string]any{"access_token": "static-tok"}}
	authed, err := applyAuth(spec, basePlan(), src, nil)
	if err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	if got, _ := headerValue(authed, "Authorization"); got != "Bearer static-tok" {
		t.Fatalf("Authorization = %q", got)
	}
}

// --- test definitions -------------------------------------------------------

const apikeyListYAML = `
openapi: 3.0.0
servers:
  - url: https://api.example.test/v1
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
      responses:
        "200":
          content:
            application/json:
              schema:
                type: object
`

const graphqlSearchYAML = `
openapi: 3.0.0
servers:
  - url: https://graph.example.test
components:
  securitySchemes:
    bearer:
      type: http
      scheme: bearer
      x-airbyte-auth:
        config_key: credentials.access_token
security:
  - bearer: []
paths:
  /graphql:
    post:
      x-airbyte-entity: issue
      x-airbyte-action: api_search
      x-airbyte-body-type: graphql
      x-airbyte-graphql-query: "query { me { id } }"
      responses:
        "200":
          content:
            application/json:
              schema:
                type: object
`

const basicauthWriteYAML = `
openapi: 3.0.0
servers:
  - url: https://api.example.test
components:
  securitySchemes:
    basic:
      type: http
      scheme: basic
      x-airbyte-auth:
        username_key: credentials.username
        password_key: credentials.password
security:
  - basic: []
paths:
  /accounts:
    post:
      x-airbyte-entity: account
      x-airbyte-action: create
      responses:
        "201":
          content:
            application/json:
              schema:
                type: object
`

const apikeyQueryYAML = `
openapi: 3.0.0
servers:
  - url: https://api.example.test/v1
components:
  securitySchemes:
    apiKey:
      type: apiKey
      in: query
      name: api_key
      x-airbyte-auth:
        config_key: credentials.api_key
security:
  - apiKey: []
paths:
  /things:
    get:
      x-airbyte-entity: thing
      x-airbyte-action: list
      responses:
        "200":
          content:
            application/json:
              schema:
                type: object
`

const apikeyCookieYAML = `
openapi: 3.0.0
servers:
  - url: https://api.example.test/v1
components:
  securitySchemes:
    apiKey:
      type: apiKey
      in: cookie
      name: session
      x-airbyte-auth:
        config_key: credentials.api_key
security:
  - apiKey: []
paths:
  /things:
    get:
      x-airbyte-entity: thing
      x-airbyte-action: list
      responses:
        "200":
          content:
            application/json:
              schema:
                type: object
`

const multiAltYAML = `
openapi: 3.0.0
servers:
  - url: https://api.example.test/v1
components:
  securitySchemes:
    apiKey:
      type: apiKey
      in: header
      name: X-API-Key
      x-airbyte-auth:
        config_key: credentials.api_key
    bearer:
      type: http
      scheme: bearer
      x-airbyte-auth:
        config_key: credentials.access_token
security:
  - apiKey: []
  - bearer: []
paths:
  /things:
    get:
      x-airbyte-entity: thing
      x-airbyte-action: list
      responses:
        "200":
          content:
            application/json:
              schema:
                type: object
`

const oauth2StaticYAML = `
openapi: 3.0.0
servers:
  - url: https://api.example.test/v1
components:
  securitySchemes:
    oauth:
      type: oauth2
      x-airbyte-auth:
        config_key: credentials.access_token
security:
  - oauth: []
paths:
  /things:
    get:
      x-airbyte-entity: thing
      x-airbyte-action: list
      responses:
        "200":
          content:
            application/json:
              schema:
                type: object
`
