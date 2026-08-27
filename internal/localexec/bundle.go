package localexec

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Size and depth bounds applied before and during bundle decoding. These bound
// untrusted input so a malicious or malformed prepare response cannot exhaust
// memory or stack before validation runs. The values are intentionally modest;
// real connector definitions are far smaller.
const (
	// MaxBundleBytes bounds the raw envelope JSON accepted by DecodeEnvelope.
	MaxBundleBytes = 1 << 20 // 1 MiB
	// MaxYAMLBytes bounds the definition_yaml string inside a bundle.
	MaxYAMLBytes = 512 << 10 // 512 KiB
	// MaxJSONDepth bounds the nesting depth of decoded params/config/source
	// objects. Exceeding it is a validation error.
	MaxJSONDepth = 64
)

// Envelope is the JSON envelope returned by the prepare endpoint. Field names
// are the wire contract and must match exactly.
type Envelope struct {
	Status            string             `json:"status"`
	ConnectorMetadata json.RawMessage    `json:"connector_metadata"`
	ExecutionMetadata *ExecutionMetadata `json:"execution_metadata"`
	Warning           string             `json:"warning,omitempty"`
	Bundle            *ExecuteBundle     `json:"bundle"`
}

// ExecutionMetadata carries prepare-time execution metadata. It is preserved
// verbatim so Phase 4 can surface it.
type ExecutionMetadata struct {
	ConnectorInstanceID string  `json:"connector_instance_id"`
	ExecutionTimeMs     float64 `json:"execution_time_ms"`
}

// ExecuteBundle is the local-execution bundle carried by the envelope. All
// fields model the current wire contract exactly.
//
// SourceConfig MAY contain secret_coordinate:: values that are UNHYDRATED at
// this stage. This package never hydrates, logs, or serializes it.
type ExecuteBundle struct {
	ConnectorDefinitionID     string         `json:"connector_definition_id"`
	DefinitionYAML            string         `json:"definition_yaml"`
	Entity                    string         `json:"entity"`
	Action                    string         `json:"action"`
	Params                    map[string]any `json:"params"`
	SelectFields              []string       `json:"select_fields"`
	ExcludeFields             []string       `json:"exclude_fields"`
	SkipTruncation            bool           `json:"skip_truncation"`
	ReplicationAuthKeyMapping map[string]any `json:"replication_auth_key_mapping"`
	ConfigValues              map[string]any `json:"config_values"`
	SourceConfig              map[string]any `json:"source_config"`
}

// DecodeEnvelope parses and validates a prepare-endpoint envelope. It bounds the
// raw byte size before parsing, rejects unknown fields, and validates the
// embedded bundle. It performs no hydration and no network I/O.
func DecodeEnvelope(raw []byte) (*Envelope, error) {
	if len(raw) > MaxBundleBytes {
		return nil, validationError(fmt.Sprintf("bundle exceeds maximum size of %d bytes", MaxBundleBytes))
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	var env Envelope
	if err := dec.Decode(&env); err != nil {
		return nil, &Error{ErrType: TypeValidation, Message: "envelope is not valid JSON", Err: err}
	}
	// Reject trailing content after the first JSON value.
	if dec.More() {
		return nil, validationError("envelope contains trailing data after the JSON object")
	}

	if err := env.validate(); err != nil {
		return nil, err
	}
	return &env, nil
}

// validate checks required envelope fields and delegates bundle validation.
func (e *Envelope) validate() error {
	if e.Bundle == nil {
		return validationError("envelope is missing the bundle (local execution requires a prepared bundle)")
	}
	return e.Bundle.validate()
}

// validate checks required bundle fields, entity/action presence, and bounds
// the depth of decoded objects. It does NOT parse the definition YAML (that is
// ParseDefinition's job) but it does bound its size.
func (b *ExecuteBundle) validate() error {
	if b.ConnectorDefinitionID == "" {
		return validationError("bundle is missing connector_definition_id")
	}
	if b.DefinitionYAML == "" {
		return validationError("bundle is missing definition_yaml")
	}
	if len(b.DefinitionYAML) > MaxYAMLBytes {
		return validationError(fmt.Sprintf("definition_yaml exceeds maximum size of %d bytes", MaxYAMLBytes))
	}
	if b.Entity == "" {
		return validationError("bundle is missing entity")
	}
	if b.Action == "" {
		return validationError("bundle is missing action")
	}

	// Bound the nesting depth of every free-form object. Field names are safe
	// to include in errors; their values are never included.
	for _, f := range []struct {
		name string
		val  any
	}{
		{"params", b.Params},
		{"replication_auth_key_mapping", b.ReplicationAuthKeyMapping},
		{"config_values", b.ConfigValues},
		{"source_config", b.SourceConfig},
	} {
		if err := boundDepth(f.val, MaxJSONDepth); err != nil {
			return validationError(fmt.Sprintf("%s nesting exceeds maximum depth of %d", f.name, MaxJSONDepth))
		}
	}
	return nil
}

// boundDepth returns an error if the decoded JSON value nests deeper than max.
// It walks maps and slices only; scalars have depth 0.
func boundDepth(v any, max int) error {
	return walkDepth(v, 0, max)
}

func walkDepth(v any, depth, max int) error {
	if depth > max {
		return fmt.Errorf("max depth exceeded")
	}
	switch t := v.(type) {
	case map[string]any:
		for _, e := range t {
			if err := walkDepth(e, depth+1, max); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range t {
			if err := walkDepth(e, depth+1, max); err != nil {
				return err
			}
		}
	}
	return nil
}
