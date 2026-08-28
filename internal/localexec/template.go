package localexec

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Bounds on template expansion. These prevent a pathological definition from
// forcing unbounded work while substituting placeholders.
const (
	maxTemplatePlaceholders = 32
	maxConfigPathSegments   = 32
)

// lookupConfigPath resolves a dotted path (e.g. "credentials.api_key") against a
// decoded config object. It performs a plain, bounded object traversal — it does
// NOT evaluate code or JSONPath. Numeric-looking segments still index maps by
// string key, never arrays; config lookups are object-only by contract.
//
// It returns the found value and true, or (nil, false) when any segment is
// missing. A path is considered "present but null" when the terminal value is
// nil; that returns (nil, true) so callers can distinguish nullable variables
// from missing ones.
func lookupConfigPath(config map[string]any, path string) (any, bool) {
	if path == "" {
		return nil, false
	}
	segments := strings.Split(path, ".")
	if len(segments) > maxConfigPathSegments {
		return nil, false
	}
	var current any = config
	for _, seg := range segments {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := m[seg]
		if !ok {
			return nil, false
		}
		current = v
	}
	return current, true
}

// scalarString renders a scalar config/param value as the string used for URL
// and query substitution. It rejects composite values (maps/arrays) because
// they cannot be placed into a single scalar slot. A nil value renders as the
// empty string only when explicitly allowed by the caller (nullable variables).
func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		if t {
			return "true", true
		}
		return "false", true
	case float64:
		return trimFloat(t), true
	case int:
		return fmt.Sprintf("%d", t), true
	case int64:
		return fmt.Sprintf("%d", t), true
	case nil:
		return "", false
	default:
		return "", false
	}
}

// trimFloat renders a float64 without a trailing ".0" for integral values so
// that JSON-decoded numbers like 1.0 serialize as "1".
func trimFloat(f float64) string {
	s := fmt.Sprintf("%g", f)
	return s
}

// resolveServerURL expands a server URL template's {variable} placeholders using
// (in precedence order) params, then config, then the variable's declared
// default. Substituted values are percent-encoded for the path context so a
// value can never inject extra path or authority structure.
//
// NOTE(parity): the exact Sonar precedence for server-variable resolution is not
// reachable from this environment. We assume params override config, which
// overrides the OpenAPI-declared default, and that a variable with no resolvable
// value and no default is a validation error.
func resolveServerURL(server Server, config, params map[string]any) (string, error) {
	tmpl := server.URL
	if tmpl == "" {
		return "", validationError("server is missing a url")
	}

	var b strings.Builder
	count := 0
	i := 0
	for i < len(tmpl) {
		c := tmpl[i]
		if c != '{' {
			b.WriteByte(c)
			i++
			continue
		}
		// Find the closing brace.
		end := strings.IndexByte(tmpl[i:], '}')
		if end < 0 {
			return "", validationError("server url has an unterminated '{' placeholder")
		}
		name := tmpl[i+1 : i+end]
		if name == "" {
			return "", validationError("server url has an empty '{}' placeholder")
		}
		count++
		if count > maxTemplatePlaceholders {
			return "", validationError(fmt.Sprintf("server url exceeds maximum of %d placeholders", maxTemplatePlaceholders))
		}

		val, err := resolveServerVariable(server, name, config, params)
		if err != nil {
			return "", err
		}
		// Percent-encode for the path segment context. url.PathEscape leaves
		// '/' unescaped, so escape it explicitly to prevent structure injection.
		safe := strings.ReplaceAll(url.PathEscape(val), "/", "%2F")
		b.WriteString(safe)
		i += end + 1
	}
	return b.String(), nil
}

// resolveServerVariable resolves a single server variable by name.
func resolveServerVariable(server Server, name string, config, params map[string]any) (string, error) {
	variable, declared := server.Variables[name]

	// A scoped variable redirects the config lookup to a nested path.
	lookupKey := name
	if declared && variable.Scope != "" {
		lookupKey = variable.Scope
	}

	if params != nil {
		if v, ok := params[name]; ok {
			return serverVariableString(name, v, variable)
		}
	}
	if v, ok := lookupConfigPath(config, lookupKey); ok {
		return serverVariableString(name, v, variable)
	}
	if declared && variable.Default != "" {
		return serverVariableString(name, variable.Default, variable)
	}
	if declared && variable.Nullable {
		// Nullable variable with no value resolves to empty.
		return "", nil
	}
	return "", validationError(fmt.Sprintf("server url variable %q has no configured value", name))
}

// serverVariableString coerces a resolved value to a string and enforces any
// declared enum. Enum values are compared as strings.
func serverVariableString(name string, v any, variable ServerVariable) (string, error) {
	if v == nil {
		if variable.Nullable {
			return "", nil
		}
		return "", validationError(fmt.Sprintf("server url variable %q is null", name))
	}
	s, ok := scalarString(v)
	if !ok {
		return "", validationError(fmt.Sprintf("server url variable %q must be a scalar value", name))
	}
	if len(variable.Enum) > 0 && !containsString(variable.Enum, s) {
		return "", validationError(fmt.Sprintf("server url variable %q is not one of the allowed values", name))
	}
	return s, nil
}

// deepObjectPair is one serialized key/value produced by deepObject style.
type deepObjectPair struct {
	Key   string
	Value string
}

// serializeDeepObject expands an object value into deepObject query pairs of the
// form name[key]=value, as used by OpenAPI style: deepObject. Only one level of
// nesting is supported; nested objects/arrays are rejected. Keys are emitted in
// sorted order for deterministic output.
//
// NOTE(parity): OpenAPI deepObject is only formally defined for a single level
// of object nesting; we reject deeper structures rather than guessing an
// encoding.
func serializeDeepObject(name string, obj map[string]any) ([]deepObjectPair, error) {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	pairs := make([]deepObjectPair, 0, len(keys))
	for _, k := range keys {
		s, ok := scalarString(obj[k])
		if !ok {
			return nil, validationError(fmt.Sprintf("deepObject parameter %q must contain only scalar values", name))
		}
		pairs = append(pairs, deepObjectPair{Key: fmt.Sprintf("%s[%s]", name, k), Value: s})
	}
	return pairs, nil
}

func containsString(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}
