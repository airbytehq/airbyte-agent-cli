package client

import (
	"encoding/json"
	"fmt"
)

const (
	ExitSuccess    = 0
	ExitGeneral    = 1
	ExitAuth       = 2
	ExitNotFound   = 3
	ExitValidation = 4
)

type APIError struct {
	Type       string          `json:"type"`
	Message    string          `json:"message"`
	StatusCode int             `json:"status_code"`
	Retryable  bool            `json:"retryable"`
	Detail     json.RawMessage `json:"detail,omitempty"`
	Hint       string          `json:"hint,omitempty"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s (status %d): %s", e.Type, e.StatusCode, e.Message)
}

func (e *APIError) ExitCode() int {
	switch {
	case e.StatusCode == 401 || e.StatusCode == 403:
		return ExitAuth
	case e.StatusCode == 404:
		return ExitNotFound
	case e.StatusCode == 400 || e.StatusCode == 422:
		return ExitValidation
	default:
		return ExitGeneral
	}
}

func NewValidationError(message, hint string) *APIError {
	return &APIError{
		Type:       "validation_error",
		Message:    message,
		StatusCode: 400,
		Hint:       hint,
	}
}

func NewNotFoundError(message, hint string) *APIError {
	return &APIError{
		Type:       "not_found",
		Message:    message,
		StatusCode: 404,
		Hint:       hint,
	}
}

// Stable error type strings for locally-originated failures (execution-config
// resolution, local connector execution, secret-manager access, and connector
// transport). These mirror the type contract used by internal/config,
// internal/secrets, and internal/localexec so the CLI surfaces a single,
// consistent {type, message, status_code, retryable, hint} shape regardless of
// where the failure originated. The StatusCode carried by each constructor is
// chosen solely so APIError.ExitCode() yields the documented process exit code
// — these errors never traverse the network.
const (
	TypeValidation                  = "validation_error"
	TypeLocalExecutionUnsupported   = "local_execution_unsupported"
	TypeSecretManagerAuthentication = "secret_manager_authentication_error"
	TypeSecretManagerAccess         = "secret_manager_access_error"
	TypeSecretNotFound              = "secret_not_found"
	TypeSecretHydration             = "secret_hydration_error"
	TypeConnectorExecutionError     = "connector_execution_error"
)

// newLocalError builds a non-retryable *APIError carrying a stable local type,
// a redaction-safe message, an optional hint, and a StatusCode chosen so
// ExitCode() maps to the intended process exit code. The message/hint MUST
// already be redaction-safe: no secret coordinates, secret IDs, credentials,
// auth headers, or request/response bodies.
func newLocalError(errType, message, hint string, statusCode int) *APIError {
	return &APIError{
		Type:       errType,
		Message:    message,
		StatusCode: statusCode,
		Retryable:  false,
		Hint:       hint,
	}
}

// NewLocalValidationError reports invalid runtime configuration or a malformed
// local-execution bundle. Exit code 4.
func NewLocalValidationError(message, hint string) *APIError {
	return newLocalError(TypeValidation, message, hint, 400)
}

// NewLocalExecutionUnsupportedError reports a bundle/definition that names a
// capability the local executor does not implement. Exit code 4.
func NewLocalExecutionUnsupportedError(message, hint string) *APIError {
	return newLocalError(TypeLocalExecutionUnsupported, message, hint, 400)
}

// NewSecretManagerAuthenticationError reports a missing/expired secret-manager
// credential (e.g. an expired SSO session). Exit code 2.
func NewSecretManagerAuthenticationError(message, hint string) *APIError {
	return newLocalError(TypeSecretManagerAuthentication, message, hint, 401)
}

// NewSecretManagerAccessError reports an access-denied response from the secret
// manager. Exit code 2.
func NewSecretManagerAccessError(message, hint string) *APIError {
	return newLocalError(TypeSecretManagerAccess, message, hint, 403)
}

// NewSecretNotFoundError reports a secret that the secret manager could not
// find. The secret ID is never echoed. Exit code 3.
func NewSecretNotFoundError(message, hint string) *APIError {
	return newLocalError(TypeSecretNotFound, message, hint, 404)
}

// NewSecretHydrationError reports a generic secret hydration failure (e.g. a
// non-scalar payload or a provider service error). Exit code 1.
func NewSecretHydrationError(message, hint string) *APIError {
	return newLocalError(TypeSecretHydration, message, hint, 500)
}

// NewConnectorExecutionError reports a connector-origin transport or response
// failure during local execution. Exit code 1.
func NewConnectorExecutionError(message, hint string) *APIError {
	return newLocalError(TypeConnectorExecutionError, message, hint, 500)
}

func newAPIError(statusCode int, message string, body []byte) *APIError {
	typ := errorType(statusCode)
	e := &APIError{
		Type:       typ,
		Message:    message,
		StatusCode: statusCode,
		Retryable:  isRetryable(statusCode),
	}
	if len(body) > 0 && json.Valid(body) {
		e.Detail = json.RawMessage(body)
	}
	return e
}

func errorType(statusCode int) string {
	switch {
	case statusCode == 401:
		return "unauthorized"
	case statusCode == 403:
		return "forbidden"
	case statusCode == 404:
		return "not_found"
	case statusCode == 400 || statusCode == 422:
		return "validation_error"
	case statusCode == 429:
		return "rate_limited"
	case statusCode >= 500:
		return "server_error"
	default:
		return "api_error"
	}
}

func isRetryable(statusCode int) bool {
	switch statusCode {
	case 429, 502, 503, 504:
		return true
	default:
		return false
	}
}
