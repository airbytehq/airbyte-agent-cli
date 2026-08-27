// Package secrets defines a provider-neutral interface for resolving secret
// coordinates into secret values, plus a hydration walker that replaces secret
// coordinates embedded in arbitrary request payloads.
//
// This package deliberately knows nothing about AWS or any other concrete
// secret backend. Concrete providers (e.g. internal/secrets/aws) implement the
// Provider interface. Error categories are defined here so that both the
// hydrator and every provider report failures with a stable, redaction-safe
// contract that later phases can map to CLI exit codes.
package secrets

import (
	"context"
	"errors"
	"fmt"
)

// CoordinatePrefix is the exact prefix that marks a string value as a secret
// coordinate. Only strings beginning with this prefix are resolved; the
// remainder (the suffix) is the provider-specific secret identifier.
const CoordinatePrefix = "secret_coordinate::"

// Provider resolves a single secret coordinate into its scalar string value.
//
// Implementations MUST:
//   - Treat coordinate as opaque provider-specific identifier material and
//     never leak it (or the resolved value) into returned error messages.
//   - Return one of the typed *Error categories defined in this package so
//     callers can map failures deterministically.
//   - Honour context cancellation.
type Provider interface {
	Resolve(ctx context.Context, coordinate string) (string, error)
}

// ErrorType is a stable, machine-readable classification of a secrets failure.
// The string values are part of the CLI's error contract and are consumed by
// later phases to select an exit code; do not rename them.
type ErrorType string

const (
	// ErrValidation covers invalid execution mode, an incomplete dedicated AWS
	// credential env pair, or a missing required region. Exit code 4.
	ErrValidation ErrorType = "validation_error"

	// ErrAuthentication covers a missing/expired cached SSO token or absent AWS
	// credentials. May carry a profile login hint. Exit code 2.
	ErrAuthentication ErrorType = "secret_manager_authentication_error"

	// ErrAccess covers AWS access-denied / KMS-denied responses. Exit code 2.
	ErrAccess ErrorType = "secret_manager_access_error"

	// ErrNotFound covers a GetSecretValue "not found" response. The secret ID
	// is never echoed. Exit code 3.
	ErrNotFound ErrorType = "secret_not_found"

	// ErrHydration covers SecretBinary, non-scalar JSON SecretString payloads,
	// invalid/empty coordinates, and generic provider service failures. Exit
	// code 1.
	ErrHydration ErrorType = "secret_hydration_error"
)

// ExitCode returns the CLI exit code associated with an ErrorType. Phase 4
// wiring maps these onto process exit codes.
func (t ErrorType) ExitCode() int {
	switch t {
	case ErrValidation:
		return 4
	case ErrAuthentication:
		return 2
	case ErrAccess:
		return 2
	case ErrNotFound:
		return 3
	case ErrHydration:
		return 1
	default:
		return 1
	}
}

// Error is the typed error returned by providers and the hydrator. It carries a
// stable Type (and therefore a stable exit code) plus a redaction-safe Message.
// The Message MUST NOT contain secret coordinates, secret IDs, credential
// values, or hydrated payloads. It MAY name the provider and the selected
// profile.
type Error struct {
	Type ErrorType
	// Message is a human-readable, already-redacted description.
	Message string
	// Hint is an optional, redaction-safe remediation string (e.g. an
	// `aws sso login --profile <profile>` command). May be empty.
	Hint string
	// Err is an optional wrapped cause. Callers MUST NOT format the wrapped
	// AWS error wholesale into user output; it is retained only for
	// errors.Is/errors.As inspection.
	Err error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Hint != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Type, e.Message, e.Hint)
	}
	return fmt.Sprintf("%s: %s", e.Type, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// ExitCode returns the exit code for this error's Type.
func (e *Error) ExitCode() int {
	if e == nil {
		return 0
	}
	return e.Type.ExitCode()
}

// newError constructs a typed *Error.
func newError(t ErrorType, message string) *Error {
	return &Error{Type: t, Message: message}
}

// AsError extracts a *Error from an error chain, if present.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
