// Package localexec compiles an Airbyte connector execution bundle into an
// in-memory HTTP request plan without performing any network I/O.
//
// The package is deliberately structured as a set of distinct, ordered stages:
//
//	DecodeBundle   -> validate the wire envelope / execute_bundle shape+bounds
//	ParseDefinition-> parse the connector YAML into a minimal typed model
//	ResolveOperation-> map (entity, action) to exactly one operation and reject
//	                   statically-detectable unsupported capabilities
//	BuildRequest   -> compile the operation + params + resolved config into an
//	                   immutable request plan
//
// Crucially, every validation and unsupported-capability check runs BEFORE any
// caller could hydrate secrets or open a socket. Later phases wire a secret
// provider and HTTP transport around this package; this package itself performs
// no hydration and no I/O.
//
// Errors are reported with a stable, redaction-safe contract that mirrors the
// shape used by internal/secrets and internal/config: every error exposes
// Type() string, ExitCode() int, and Error() string. Messages never contain
// source_config values, config values, secret coordinates, or secret IDs.
package localexec

import "errors"

// Stable error type strings. These are part of the CLI's error contract and are
// consumed by later phases to select an exit code; do not rename them.
const (
	// TypeUnsupported marks a bundle/definition that names a capability this
	// local executor does not implement. It is returned as early as possible,
	// before any hydration or network call. Exit code 4.
	TypeUnsupported = "local_execution_unsupported"

	// TypeValidation marks a malformed bundle, envelope, or definition. Exit
	// code 4.
	TypeValidation = "validation_error"
)

// Error is the typed error returned by this package. It carries a stable Type
// (and therefore a stable exit code) plus a redaction-safe Message. The Message
// MUST NOT contain config values, source_config values, secret coordinates, or
// secret IDs. It MAY name a field, an entity/action, or a capability.
type Error struct {
	ErrType string
	Message string
	// Err is an optional wrapped cause, retained only for errors.Is/errors.As
	// inspection. Callers MUST NOT format it wholesale into user output.
	Err error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// Type returns the stable error category string.
func (e *Error) Type() string {
	if e == nil {
		return ""
	}
	return e.ErrType
}

// ExitCode returns the process exit code for this error. Both categories map to
// exit code 4 (validation), aligning with config.ValidationError.
func (e *Error) ExitCode() int {
	if e == nil {
		return 0
	}
	switch e.ErrType {
	case TypeUnsupported, TypeValidation:
		return 4
	default:
		return 1
	}
}

func (e *Error) Unwrap() error { return e.Err }

// unsupportedError constructs a *Error of type local_execution_unsupported.
func unsupportedError(message string) *Error {
	return &Error{ErrType: TypeUnsupported, Message: message}
}

// validationError constructs a *Error of type validation_error.
func validationError(message string) *Error {
	return &Error{ErrType: TypeValidation, Message: message}
}

// AsError extracts a *Error from an error chain, if present.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
