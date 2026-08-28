package localexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

func assertUnsupported(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if e.Type() != TypeUnsupported {
		t.Fatalf("expected %s, got %s (%v)", TypeUnsupported, e.Type(), err)
	}
	if e.ExitCode() != 4 {
		t.Fatalf("expected exit 4, got %d", e.ExitCode())
	}
}

func TestDefinitionResolveREST(t *testing.T) {
	def, err := ParseDefinition(loadFixture(t, "apikey_list.yaml"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r, err := def.ResolveOperation("widget", "list")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.Method != "GET" || r.Path != "/widgets" {
		t.Fatalf("method/path = %s %s", r.Method, r.Path)
	}
	if r.Operation.OperationID != "listWidgets" {
		t.Fatalf("operationId = %s", r.Operation.OperationID)
	}
}

func TestDefinitionResolveGraphQL(t *testing.T) {
	def, err := ParseDefinition(loadFixture(t, "graphql_search.yaml"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r, err := def.ResolveOperation("issue", "api_search")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.Method != "POST" || r.Operation.BodyType != "graphql" {
		t.Fatalf("unexpected resolution: %s %s", r.Method, r.Operation.BodyType)
	}
}

func TestDefinitionResolvePathOverride(t *testing.T) {
	src := `
openapi: 3.0.0
servers:
  - url: https://api.example.test
paths:
  /placeholder:
    get:
      x-airbyte-entity: thing
      x-airbyte-action: list
      x-airbyte-path-override: /v2/things
      responses:
        "200":
          description: OK
          content:
            application/json:
              schema:
                type: object
`
	def, err := ParseDefinition(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r, err := def.ResolveOperation("thing", "list")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.Path != "/v2/things" {
		t.Fatalf("path override not applied: %s", r.Path)
	}
}

func TestDefinitionAmbiguousOperation(t *testing.T) {
	src := `
openapi: 3.0.0
servers:
  - url: https://api.example.test
paths:
  /a:
    get:
      x-airbyte-entity: dup
      x-airbyte-action: list
      responses:
        "200": {description: OK}
  /b:
    get:
      x-airbyte-entity: dup
      x-airbyte-action: list
      responses:
        "200": {description: OK}
`
	def, err := ParseDefinition(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = def.ResolveOperation("dup", "list")
	assertValidationError(t, err)
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguity message, got %v", err)
	}
}

func TestDefinitionMalformedYAML(t *testing.T) {
	_, err := ParseDefinition("openapi: 3.0.0\npaths: [this is: not valid")
	assertValidationError(t, err)
}

func TestDefinitionNoPaths(t *testing.T) {
	_, err := ParseDefinition("openapi: 3.0.0\nservers:\n  - url: https://x.test\n")
	assertValidationError(t, err)
}

// Real connector definitions (e.g. Stripe) place a boolean `required: true` on
// individual property schemas, unlike the object-level `required: [names]`
// list. The parser must tolerate the boolean form (as an empty required list)
// rather than failing the whole definition.
func TestDefinitionPropertyLevelBooleanRequired(t *testing.T) {
	src := "openapi: 3.0.0\n" +
		"paths:\n" +
		"  /widgets:\n" +
		"    get:\n" +
		"      x-airbyte-entity: widget\n" +
		"      x-airbyte-action: list\n" +
		"      parameters:\n" +
		"        - name: filter\n" +
		"          in: query\n" +
		"          schema:\n" +
		"            type: object\n" +
		"            properties:\n" +
		"              enabled:\n" +
		"                type: boolean\n" +
		"                required: true\n"
	def, err := ParseDefinition(src)
	if err != nil {
		t.Fatalf("boolean property-level required should parse: %v", err)
	}
	schema := def.Paths["/widgets"].Get.Parameters[0].Schema
	if len(schema.Properties["enabled"].Required) != 0 {
		t.Errorf("boolean required should decode to empty list, got %v", schema.Properties["enabled"].Required)
	}
}

func TestDefinitionUnknownAction(t *testing.T) {
	def, err := ParseDefinition(loadFixture(t, "apikey_list.yaml"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Recognized-but-unresolvable action -> validation error.
	_, err = def.ResolveOperation("widget", "get")
	assertValidationError(t, err)
	// Wholly unrecognized action string -> validation error.
	_, err = def.ResolveOperation("widget", "frobnicate")
	assertValidationError(t, err)
}

func TestDefinitionKnownUnsupportedActions(t *testing.T) {
	def, err := ParseDefinition(loadFixture(t, "apikey_list.yaml"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, action := range []string{"describe", "download", "context_store_get"} {
		t.Run(action, func(t *testing.T) {
			_, err := def.ResolveOperation("widget", action)
			assertUnsupported(t, err)
		})
	}
}

func TestDefinitionBinaryResponseUnsupported(t *testing.T) {
	src := `
openapi: 3.0.0
servers:
  - url: https://api.example.test
paths:
  /export:
    get:
      x-airbyte-entity: export
      x-airbyte-action: get
      responses:
        "200":
          description: OK
          content:
            application/octet-stream:
              schema:
                type: string
                format: binary
`
	def, err := ParseDefinition(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = def.ResolveOperation("export", "get")
	assertUnsupported(t, err)
}

func TestDefinitionRefreshableOAuthUnsupported(t *testing.T) {
	refreshURL := `
openapi: 3.0.0
servers:
  - url: https://api.example.test
components:
  securitySchemes:
    oauth:
      type: oauth2
      flows:
        authorizationCode:
          authorizationUrl: https://auth.example.test/authorize
          tokenUrl: https://auth.example.test/token
          refreshUrl: https://auth.example.test/refresh
paths:
  /x:
    get:
      x-airbyte-entity: x
      x-airbyte-action: list
      responses:
        "200": {description: OK}
`
	if _, err := ParseDefinition(refreshURL); err == nil {
		t.Fatal("expected unsupported error for refreshUrl")
	} else {
		assertUnsupported(t, err)
	}

	refreshFlag := `
openapi: 3.0.0
servers:
  - url: https://api.example.test
components:
  securitySchemes:
    oauth:
      type: oauth2
      x-airbyte-oauth-refresh: true
      flows:
        clientCredentials:
          tokenUrl: https://auth.example.test/token
paths:
  /x:
    get:
      x-airbyte-entity: x
      x-airbyte-action: list
      responses:
        "200": {description: OK}
`
	_, err := ParseDefinition(refreshFlag)
	assertUnsupported(t, err)
}

func TestDefinitionStaticOAuthAllowed(t *testing.T) {
	src := `
openapi: 3.0.0
servers:
  - url: https://api.example.test
components:
  securitySchemes:
    oauth:
      type: oauth2
      flows:
        clientCredentials:
          tokenUrl: https://auth.example.test/token
paths:
  /x:
    get:
      x-airbyte-entity: x
      x-airbyte-action: list
      responses:
        "200":
          description: OK
          content:
            application/json:
              schema:
                type: object
`
	def, err := ParseDefinition(src)
	if err != nil {
		t.Fatalf("static oauth should parse: %v", err)
	}
	if _, err := def.ResolveOperation("x", "list"); err != nil {
		t.Fatalf("static oauth should resolve: %v", err)
	}
}

func TestDefinitionUnknownExtensionUnsupported(t *testing.T) {
	src := `
openapi: 3.0.0
servers:
  - url: https://api.example.test
paths:
  /x:
    get:
      x-airbyte-entity: x
      x-airbyte-action: list
      x-airbyte-made-up-thing: true
      responses:
        "200": {description: OK}
`
	_, err := ParseDefinition(src)
	assertUnsupported(t, err)
}

func TestDefinitionSizeLimit(t *testing.T) {
	big := "openapi: 3.0.0\n# " + strings.Repeat("x", MaxYAMLBytes)
	_, err := ParseDefinition(big)
	assertValidationError(t, err)
}

func TestDefinitionKnownExtensionsAllowed(t *testing.T) {
	// A definition exercising the recognized extension families must parse.
	src := `
openapi: 3.0.0
servers:
  - url: https://api.example.test
components:
  securitySchemes:
    apiKey:
      type: apiKey
      in: header
      name: X-API-Key
      x-airbyte-auth:
        config_key: credentials.api_key
paths:
  /x:
    get:
      x-airbyte-entity: x
      x-airbyte-action: list
      x-airbyte-config-injection: {foo: bar}
      x-airbyte-record-selector: "$.data[*]"
      x-airbyte-record-filter: {}
      x-airbyte-record-transform: {}
      x-airbyte-error-check: {}
      x-airbyte-retry: {max_attempts: 3}
      responses:
        "200":
          description: OK
          content:
            application/json:
              schema:
                type: object
`
	if _, err := ParseDefinition(src); err != nil {
		t.Fatalf("recognized extensions should parse: %v", err)
	}
}
