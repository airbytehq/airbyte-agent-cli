package resources

// connectors_execute.go implements the `connectors execute` orchestration. A
// single execute body is built from the operation params (identical for both
// runtimes) and then the runtime execution mode selects one of two branches:
//
//   - hosted (default): the body is POSTed to the Airbyte-hosted
//     /execute endpoint and the raw response is returned unchanged. This is
//     byte-for-byte the pre-Phase-4 behaviour.
//
//   - local (opt-in via the global --execution-mode / AIRBYTE_EXECUTION_MODE):
//     the SAME body is POSTed to /execute/prepare (an authorization/audit
//     boundary that carries normal Airbyte auth), the returned bundle is
//     decoded and executed locally against the connector's origin, and a
//     bundle-free standard envelope is returned.
//
// The local branch NEVER falls back to the hosted /execute endpoint: any local
// failure is surfaced as a typed error. Connector-origin HTTP happens only
// inside the localexec transport, never via the Airbyte API client.

import (
	"context"
	"errors"

	"github.com/airbytehq/airbyte-agent-cli/internal/client"
	"github.com/airbytehq/airbyte-agent-cli/internal/config"
	"github.com/airbytehq/airbyte-agent-cli/internal/localexec"
	"github.com/airbytehq/airbyte-agent-cli/internal/secrets"
	"github.com/airbytehq/airbyte-agent-cli/internal/secrets/aws"
)

// newSecretsProvider builds the secrets provider used to hydrate a local
// execution bundle. It is a package-level var (mirroring statusWriter /
// confirmDestructive) so tests can inject a fake provider without touching AWS.
var newSecretsProvider = func(ec config.ExecutionConfig) secrets.Provider {
	return aws.New(ec)
}

// newLocalExecutor builds the local executor bound to a secrets provider. Tests
// override it to inject a fake round-tripper / clock so local execution hits an
// httptest server deterministically and never opens a real socket.
var newLocalExecutor = func(p secrets.Provider) *localexec.Executor {
	return localexec.NewExecutor(p)
}

func connectorsExecute(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
	id, _ := params["id"].(string)
	body := buildExecuteBody(params)

	execConfig, err := c.ExecutionConfig()
	if err != nil {
		// Invalid runtime configuration (e.g. a bad --execution-mode) must
		// surface as a validation error, NOT silently run hosted.
		return nil, translateExecutionError(err)
	}

	if execConfig.Mode != config.ModeLocal {
		// Hosted (default): identical to the pre-Phase-4 behaviour — POST the
		// body to /execute and return the raw response unchanged.
		raw, err := c.Post(ctx, connectorPath(id)+"/execute", body)
		if err != nil {
			return nil, err
		}
		return raw, nil
	}

	return executeLocal(ctx, c, id, body, execConfig)
}

// buildExecuteBody constructs the execute request body from operation params.
// This is the single source of truth for the wire body and is byte-for-byte
// identical to the original hosted implementation (entity, action, and the
// optional params / select_fields / exclude_fields / skip_truncation / intent
// keys — intent omitted when absent). Both runtimes send exactly this body.
func buildExecuteBody(params map[string]any) map[string]any {
	entity, _ := params["entity"].(string)
	action, _ := params["action"].(string)

	body := map[string]any{
		"entity": entity,
		"action": action,
	}
	if p, ok := params["params"]; ok {
		body["params"] = p
	}
	if sf, ok := params["select_fields"]; ok {
		body["select_fields"] = sf
	}
	if ef, ok := params["exclude_fields"]; ok {
		body["exclude_fields"] = ef
	}
	if st, ok := params["skip_truncation"]; ok {
		body["skip_truncation"] = st
	}
	if i, ok := params["intent"]; ok {
		body["intent"] = i
	}
	return body
}

// executeLocal runs the local-execution pipeline in the mandated order:
// prepare -> decode -> require bundle -> hydrate+validate+request (inside
// Execute) -> shape. It never calls the hosted /execute endpoint and never
// falls back to hosted on failure.
func executeLocal(ctx context.Context, c *client.Client, id string, body map[string]any, execConfig config.ExecutionConfig) (any, error) {
	// (1) Prepare. This is an Airbyte API call and carries normal auth — it is
	// the authorization/audit boundary. A prepare failure is a hosted-style API
	// error and is returned unchanged (never translated, never retried locally).
	rawEnv, err := c.Post(ctx, connectorPath(id)+"/execute/prepare", body)
	if err != nil {
		return nil, err
	}

	// (2) Decode + validate the envelope. DecodeEnvelope also requires the
	// bundle to be present and structurally valid.
	env, err := localexec.DecodeEnvelope(rawEnv)
	if err != nil {
		return nil, translateExecutionError(err)
	}
	// Defense-in-depth: DecodeEnvelope already rejects a missing bundle, but
	// guard explicitly so local execution can never proceed without one.
	if env.Bundle == nil {
		return nil, client.NewLocalValidationError(
			"prepare response did not include an execution bundle required for local execution",
			"re-run without --execution-mode local to execute on the Airbyte-hosted API",
		)
	}

	// (3) Create the provider and executor, then execute. Static validation
	// happens INSIDE Execute before any secret is resolved, so an unsupported
	// bundle fails without hydrating a single secret.
	provider := newSecretsProvider(execConfig)
	executor := newLocalExecutor(provider)
	result, err := executor.Execute(ctx, env.Bundle)
	if err != nil {
		return nil, translateExecutionError(err)
	}

	// (4) Build the returned envelope from the prepare envelope + local result.
	return buildLocalEnvelope(env, result), nil
}

// buildLocalEnvelope assembles the standard, bundle-free response envelope
// returned to the caller in local mode. It preserves status, connector_metadata,
// warning (when present) and execution_metadata.connector_instance_id from the
// prepare envelope, replaces execution_metadata.execution_time_ms with the local
// end-to-end execution time, inserts a result built from the local execution
// result, and omits the bundle entirely.
func buildLocalEnvelope(env *localexec.Envelope, result *localexec.Result) map[string]any {
	out := map[string]any{
		"status": env.Status,
		"result": buildLocalResult(result),
	}
	if len(env.ConnectorMetadata) > 0 {
		// json.RawMessage marshals through unchanged.
		out["connector_metadata"] = env.ConnectorMetadata
	}

	execMeta := map[string]any{}
	if env.ExecutionMetadata != nil {
		execMeta["connector_instance_id"] = env.ExecutionMetadata.ConnectorInstanceID
	}
	// Replace the prepare-time timing with the local end-to-end time.
	execMeta["execution_time_ms"] = result.ExecutionTimeMs
	out["execution_metadata"] = execMeta

	if env.Warning != "" {
		out["warning"] = env.Warning
	}
	return out
}

// buildLocalResult shapes a localexec.Result into the `result` field of the
// standard envelope. The hosted `result` field is an opaque connector payload;
// for local execution we surface the extracted records plus (when present) the
// response metadata, along with the record_count / truncated bookkeeping. The
// end-to-end timing is deliberately NOT duplicated here — it lives in
// execution_metadata.execution_time_ms.
func buildLocalResult(result *localexec.Result) map[string]any {
	records := result.Records
	if records == nil {
		records = []any{}
	}
	out := map[string]any{
		"records":      records,
		"record_count": result.RecordCount,
		"truncated":    result.Truncated,
	}
	if len(result.Metadata) > 0 {
		out["metadata"] = result.Metadata
	}
	return out
}

// translateExecutionError maps a locally-originated error onto the stable
// {type, message, status_code, retryable, hint} contract with the correct exit
// code, without leaking any secret coordinate, secret ID, credential, or
// request/response body into the surfaced message. Already-structured
// *client.APIError values (e.g. from the prepare call or the execution-config
// resolver) pass through unchanged.
func translateExecutionError(err error) error {
	if err == nil {
		return nil
	}

	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}

	var cfgErr *config.ValidationError
	if errors.As(err, &cfgErr) {
		return client.NewLocalValidationError(cfgErr.Error(), "")
	}

	if lerr, ok := localexec.AsError(err); ok {
		switch lerr.Type() {
		case localexec.TypeUnsupported:
			return client.NewLocalExecutionUnsupportedError(lerr.Error(), "")
		case localexec.TypeValidation:
			return client.NewLocalValidationError(lerr.Error(), "")
		default:
			// e.g. connector_execution_error from the transport stage.
			return client.NewConnectorExecutionError(lerr.Error(), "")
		}
	}

	if serr, ok := secrets.AsError(err); ok {
		// Message and Hint are documented redaction-safe; preserve the Hint
		// (e.g. the `aws sso login --profile ...` remediation).
		switch serr.Type {
		case secrets.ErrValidation:
			return client.NewLocalValidationError(serr.Message, serr.Hint)
		case secrets.ErrAuthentication:
			return client.NewSecretManagerAuthenticationError(serr.Message, serr.Hint)
		case secrets.ErrAccess:
			return client.NewSecretManagerAccessError(serr.Message, serr.Hint)
		case secrets.ErrNotFound:
			return client.NewSecretNotFoundError(serr.Message, serr.Hint)
		default:
			return client.NewSecretHydrationError(serr.Message, serr.Hint)
		}
	}

	// Unknown error: surface as a connector execution error without leaking the
	// wrapped cause into user output.
	return client.NewConnectorExecutionError("local connector execution failed", "")
}
