package localexec

import (
	"fmt"
	"strings"
	"testing"
)

// minimalYAML is a tiny but valid definition used by bundle tests that only
// exercise envelope/bundle structure (not definition parsing).
const minimalYAML = "openapi: 3.0.0\npaths:\n  /x:\n    get:\n      x-airbyte-entity: x\n      x-airbyte-action: list\n"

func validEnvelopeJSON() string {
	return `{
		"status": "prepared",
		"connector_metadata": {"name": "synthetic"},
		"execution_metadata": {"connector_instance_id": "ci-123", "execution_time_ms": 12.5},
		"warning": "heads up",
		"bundle": {
			"connector_definition_id": "def-123",
			"definition_yaml": ` + fmt.Sprintf("%q", minimalYAML) + `,
			"entity": "widget",
			"action": "list",
			"params": {"a": 1},
			"select_fields": ["id", "name"],
			"exclude_fields": [],
			"skip_truncation": true,
			"replication_auth_key_mapping": {"k": "v"},
			"config_values": {"version": "v1"},
			"source_config": {"credentials": {"api_key": "secret_coordinate::synthetic/key"}}
		}
	}`
}

func TestBundleDecodeValidAndPreservesEnvelope(t *testing.T) {
	env, err := DecodeEnvelope([]byte(validEnvelopeJSON()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Status != "prepared" {
		t.Errorf("status = %q", env.Status)
	}
	if env.Warning != "heads up" {
		t.Errorf("warning = %q", env.Warning)
	}
	if env.ExecutionMetadata == nil || env.ExecutionMetadata.ConnectorInstanceID != "ci-123" {
		t.Errorf("execution_metadata not preserved: %+v", env.ExecutionMetadata)
	}
	if env.ExecutionMetadata.ExecutionTimeMs != 12.5 {
		t.Errorf("execution_time_ms = %v", env.ExecutionMetadata.ExecutionTimeMs)
	}
	if string(env.ConnectorMetadata) == "" {
		t.Error("connector_metadata not preserved")
	}
	b := env.Bundle
	if b == nil {
		t.Fatal("bundle nil")
	}
	if b.ConnectorDefinitionID != "def-123" || b.Entity != "widget" || b.Action != "list" {
		t.Errorf("bundle scalars wrong: %+v", b)
	}
	if !b.SkipTruncation {
		t.Error("skip_truncation not preserved")
	}
	if len(b.SelectFields) != 2 || b.SelectFields[0] != "id" {
		t.Errorf("select_fields = %v", b.SelectFields)
	}
	if b.SourceConfig == nil {
		t.Error("source_config not preserved")
	}
}

func TestBundleDecodeUnknownFieldRejected(t *testing.T) {
	raw := `{"status": "ok", "bundle": {"connector_definition_id": "x", "definition_yaml": "y", "entity": "e", "action": "list"}, "surprise": true}`
	_, err := DecodeEnvelope([]byte(raw))
	assertValidationError(t, err)
}

func TestBundleDecodeUnknownBundleFieldRejected(t *testing.T) {
	raw := `{"bundle": {"connector_definition_id": "x", "definition_yaml": "y", "entity": "e", "action": "list", "unexpected": 1}}`
	_, err := DecodeEnvelope([]byte(raw))
	assertValidationError(t, err)
}

func TestBundleDecodeMissingFields(t *testing.T) {
	base := map[string]string{
		"connector_definition_id": `"def"`,
		"definition_yaml":         fmt.Sprintf("%q", minimalYAML),
		"entity":                  `"widget"`,
		"action":                  `"list"`,
	}
	for omit := range base {
		t.Run("missing_"+omit, func(t *testing.T) {
			var fields []string
			for k, v := range base {
				if k == omit {
					continue
				}
				fields = append(fields, fmt.Sprintf("%q: %s", k, v))
			}
			raw := `{"bundle": {` + strings.Join(fields, ",") + `}}`
			_, err := DecodeEnvelope([]byte(raw))
			assertValidationError(t, err)
		})
	}
}

func TestBundleDecodeAbsentBundleRejected(t *testing.T) {
	_, err := DecodeEnvelope([]byte(`{"status": "prepared"}`))
	assertValidationError(t, err)
	_, err = DecodeEnvelope([]byte(`{"status": "prepared", "bundle": null}`))
	assertValidationError(t, err)
}

func TestBundleDecodeSizeLimits(t *testing.T) {
	// Bundle byte cap.
	huge := make([]byte, MaxBundleBytes+1)
	for i := range huge {
		huge[i] = ' '
	}
	if _, err := DecodeEnvelope(huge); err == nil {
		t.Fatal("expected bundle size error")
	}

	// YAML byte cap.
	bigYAML := strings.Repeat("a", MaxYAMLBytes+1)
	raw := fmt.Sprintf(`{"bundle": {"connector_definition_id": "x", "definition_yaml": %q, "entity": "e", "action": "list"}}`, bigYAML)
	_, err := DecodeEnvelope([]byte(raw))
	assertValidationError(t, err)
}

func TestBundleDecodeNullOptionals(t *testing.T) {
	raw := fmt.Sprintf(`{"bundle": {
		"connector_definition_id": "x",
		"definition_yaml": %q,
		"entity": "e",
		"action": "list",
		"params": null,
		"select_fields": null,
		"exclude_fields": null,
		"replication_auth_key_mapping": null,
		"config_values": null,
		"source_config": null
	}}`, minimalYAML)
	env, err := DecodeEnvelope([]byte(raw))
	if err != nil {
		t.Fatalf("null optionals should decode: %v", err)
	}
	if env.Bundle.Params != nil || env.Bundle.SourceConfig != nil {
		t.Error("null optionals should stay nil")
	}
}

func TestBundleDecodeDepthLimit(t *testing.T) {
	var b strings.Builder
	for i := 0; i < MaxJSONDepth+2; i++ {
		b.WriteString(`{"a":`)
	}
	b.WriteString(`1`)
	for i := 0; i < MaxJSONDepth+2; i++ {
		b.WriteString(`}`)
	}
	raw := fmt.Sprintf(`{"bundle": {"connector_definition_id": "x", "definition_yaml": %q, "entity": "e", "action": "list", "source_config": %s}}`, minimalYAML, b.String())
	_, err := DecodeEnvelope([]byte(raw))
	assertValidationError(t, err)
}

func TestBundleDecodeInvalidJSON(t *testing.T) {
	_, err := DecodeEnvelope([]byte(`{not json`))
	assertValidationError(t, err)
	_, err = DecodeEnvelope([]byte(`{"status":"ok"} trailing`))
	assertValidationError(t, err)
}

func assertValidationError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if e.Type() != TypeValidation {
		t.Fatalf("expected %s, got %s (%v)", TypeValidation, e.Type(), err)
	}
	if e.ExitCode() != 4 {
		t.Fatalf("expected exit 4, got %d", e.ExitCode())
	}
}
