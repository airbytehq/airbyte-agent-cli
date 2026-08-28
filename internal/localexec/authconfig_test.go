package localexec

import (
	"encoding/base64"
	"testing"
)

// These tests cover the x-airbyte-auth-config auth model that every real Airbyte
// connector definition uses (the older top-level x-airbyte-auth extension is not
// emitted by any connector). Each fixture mirrors the real shape: an
// auth_mapping of connector auth params to bare ${field} references, plus a
// replication_auth_key_mapping of source-config paths to those auth fields.

func authConfigBearerYAML() string {
	return "openapi: 3.0.0\n" +
		"servers:\n" +
		"  - url: https://api.example.test/v1\n" +
		"paths:\n" +
		"  /customers:\n" +
		"    get:\n" +
		"      x-airbyte-entity: customer\n" +
		"      x-airbyte-action: list\n" +
		"      security:\n" +
		"        - bearerAuth: []\n" +
		"components:\n" +
		"  securitySchemes:\n" +
		"    bearerAuth:\n" +
		"      type: http\n" +
		"      scheme: bearer\n" +
		"      x-airbyte-auth-config:\n" +
		"        auth_mapping:\n" +
		"          token: \"${api_key}\"\n" +
		"        replication_auth_key_mapping:\n" +
		"          client_secret: \"api_key\"\n"
}

// TestAuthConfig_Bearer_ReplicationMapping is the Stripe shape: the hydrated
// source_config.client_secret is named api_key by the replication mapping, and
// auth_mapping routes it to the bearer token.
func TestAuthConfig_Bearer_ReplicationMapping(t *testing.T) {
	spec, _ := resolveAuthForYAML(t, authConfigBearerYAML(), "customer", "list")
	src := map[string]any{"client_secret": "sk_live_hydrated"}
	authed, err := applyAuth(spec, basePlan(), src, nil)
	if err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	if got, _ := headerValue(authed, "Authorization"); got != "Bearer sk_live_hydrated" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer sk_live_hydrated")
	}
}

func TestAuthConfig_Basic_ReplicationMapping(t *testing.T) {
	yaml := "openapi: 3.0.0\n" +
		"servers:\n" +
		"  - url: https://api.example.test/v1\n" +
		"paths:\n" +
		"  /accounts:\n" +
		"    get:\n" +
		"      x-airbyte-entity: account\n" +
		"      x-airbyte-action: list\n" +
		"      security:\n" +
		"        - basicAuth: []\n" +
		"components:\n" +
		"  securitySchemes:\n" +
		"    basicAuth:\n" +
		"      type: http\n" +
		"      scheme: basic\n" +
		"      x-airbyte-auth-config:\n" +
		"        auth_mapping:\n" +
		"          username: \"${username}\"\n" +
		"          password: \"${password}\"\n" +
		"        replication_auth_key_mapping:\n" +
		"          user_field: \"username\"\n" +
		"          pass_field: \"password\"\n"
	spec, _ := resolveAuthForYAML(t, yaml, "account", "list")
	src := map[string]any{"user_field": "alice", "pass_field": "p@ss"}
	authed, err := applyAuth(spec, basePlan(), src, nil)
	if err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:p@ss"))
	if got, _ := headerValue(authed, "Authorization"); got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

func TestAuthConfig_APIKeyHeader_ReplicationMapping(t *testing.T) {
	yaml := "openapi: 3.0.0\n" +
		"servers:\n" +
		"  - url: https://api.example.test/v1\n" +
		"paths:\n" +
		"  /widgets:\n" +
		"    get:\n" +
		"      x-airbyte-entity: widget\n" +
		"      x-airbyte-action: list\n" +
		"      security:\n" +
		"        - apiKeyAuth: []\n" +
		"components:\n" +
		"  securitySchemes:\n" +
		"    apiKeyAuth:\n" +
		"      type: apiKey\n" +
		"      in: header\n" +
		"      name: X-API-Key\n" +
		"      x-airbyte-auth-config:\n" +
		"        auth_mapping:\n" +
		"          api_key: \"${api_key}\"\n" +
		"        replication_auth_key_mapping:\n" +
		"          my_secret: \"api_key\"\n"
	spec, _ := resolveAuthForYAML(t, yaml, "widget", "list")
	src := map[string]any{"my_secret": "abc123"}
	authed, err := applyAuth(spec, basePlan(), src, nil)
	if err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	if got, _ := headerValue(authed, "X-API-Key"); got != "abc123" {
		t.Fatalf("X-API-Key = %q, want abc123", got)
	}
}

// Direct-only: no replication_auth_key_mapping, so the auth field name resolves
// identically against the source config (Pass 2 identity mapping).
func TestAuthConfig_APIKeyHeader_DirectOnlyIdentity(t *testing.T) {
	yaml := "openapi: 3.0.0\n" +
		"servers:\n" +
		"  - url: https://api.example.test/v1\n" +
		"paths:\n" +
		"  /widgets:\n" +
		"    get:\n" +
		"      x-airbyte-entity: widget\n" +
		"      x-airbyte-action: list\n" +
		"      security:\n" +
		"        - apiKeyAuth: []\n" +
		"components:\n" +
		"  securitySchemes:\n" +
		"    apiKeyAuth:\n" +
		"      type: apiKey\n" +
		"      in: header\n" +
		"      name: X-API-Key\n" +
		"      x-airbyte-auth-config:\n" +
		"        required:\n" +
		"          - api_key\n" +
		"        properties:\n" +
		"          api_key:\n" +
		"            type: string\n" +
		"        auth_mapping:\n" +
		"          api_key: \"${api_key}\"\n"
	spec, _ := resolveAuthForYAML(t, yaml, "widget", "list")
	src := map[string]any{"api_key": "direct123"}
	authed, err := applyAuth(spec, basePlan(), src, nil)
	if err != nil {
		t.Fatalf("applyAuth: %v", err)
	}
	if got, _ := headerValue(authed, "X-API-Key"); got != "direct123" {
		t.Fatalf("X-API-Key = %q, want direct123", got)
	}
}

// A missing credential is a redacted soft miss, not a silent success.
func TestAuthConfig_Bearer_MissingCredential(t *testing.T) {
	spec, _ := resolveAuthForYAML(t, authConfigBearerYAML(), "customer", "list")
	_, err := applyAuth(spec, basePlan(), map[string]any{}, nil)
	if err == nil {
		t.Fatal("expected error when the credential is absent")
	}
}
