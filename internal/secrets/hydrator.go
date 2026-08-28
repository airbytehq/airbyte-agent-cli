package secrets

import (
	"context"
	"strings"
)

// Hydrate walks an arbitrary decoded-JSON value (produced by
// encoding/json into interface{} shapes: map[string]any, []any, and scalars)
// and returns a NEW value in which every exact secret-coordinate string has
// been replaced by the resolved secret value.
//
// Behaviour contract:
//   - Only strings that begin exactly with CoordinatePrefix are resolved. The
//     remainder of the string is the coordinate suffix passed to the provider.
//   - Maps and slices are recursed into; a fresh map/slice is always returned so
//     the input is never mutated (immutable hydration).
//   - Scalar types other than coordinate strings are returned unchanged with
//     their original Go type preserved.
//   - Each distinct coordinate is resolved at most once per Hydrate call
//     (invocation-local deduplication). No secret value is cached across calls.
//   - A coordinate whose suffix is empty is rejected as a hydration error.
//   - Context cancellation is honoured before each provider call.
//
// On any provider or validation failure Hydrate returns a typed *Error and a
// nil result. Error messages never contain coordinates or secret values.
func Hydrate(ctx context.Context, p Provider, value any) (any, error) {
	h := &hydration{
		provider: p,
		resolved: make(map[string]string),
	}
	return h.walk(ctx, value)
}

type hydration struct {
	provider Provider
	// resolved memoizes suffix -> secret value for this invocation only.
	resolved map[string]string
}

func (h *hydration) walk(ctx context.Context, value any) (any, error) {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, elem := range v {
			hydrated, err := h.walk(ctx, elem)
			if err != nil {
				return nil, err
			}
			out[k] = hydrated
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, elem := range v {
			hydrated, err := h.walk(ctx, elem)
			if err != nil {
				return nil, err
			}
			out[i] = hydrated
		}
		return out, nil
	case string:
		return h.resolveString(ctx, v)
	default:
		// Preserve scalar types (bool, float64, json.Number, nil, etc.)
		// exactly as-is.
		return value, nil
	}
}

// resolveString replaces a coordinate string with its secret value, or returns
// the string unchanged when it is not a coordinate.
func (h *hydration) resolveString(ctx context.Context, s string) (any, error) {
	if !strings.HasPrefix(s, CoordinatePrefix) {
		return s, nil
	}
	suffix := strings.TrimPrefix(s, CoordinatePrefix)
	if suffix == "" {
		// Do not echo the (empty) coordinate; the category is enough.
		return nil, newError(ErrHydration, "secret coordinate is empty")
	}

	if cached, ok := h.resolved[suffix]; ok {
		return cached, nil
	}

	if err := ctx.Err(); err != nil {
		// Surface cancellation as a hydration error while wrapping the cause
		// for errors.Is inspection. No coordinate is included.
		return nil, &Error{Type: ErrHydration, Message: "secret resolution canceled", Err: err}
	}

	resolved, err := h.provider.Resolve(ctx, suffix)
	if err != nil {
		// Provider errors are already typed and redaction-safe. Pass a typed
		// error through unchanged; wrap anything else as a hydration error
		// without leaking the coordinate.
		if _, ok := AsError(err); ok {
			return nil, err
		}
		return nil, &Error{Type: ErrHydration, Message: "secret provider failed", Err: err}
	}

	h.resolved[suffix] = resolved
	return resolved, nil
}
