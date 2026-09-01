package output

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// Filter returns a new value containing only the requested paths. Paths use
// dotted notation (e.g. "data.id"); when a path segment encounters an array,
// the remaining segments are applied to each element ("array broadcast").
//
// If paths is empty, the original value is returned unchanged. Individual
// missing paths are skipped when at least one requested path matches.
//
// Filter intentionally ignores the match result for callers that only need the
// projected value. Use FilterWithMatch when a complete miss must be detected.
func Filter(value any, paths []string) any {
	filtered, _, err := FilterWithMatch(value, paths)
	if err != nil {
		return value
	}
	return filtered
}

// FilterWithMatch filters value and reports whether at least one requested
// path matched. Empty arrays count as matches because they may have no rows
// against which to resolve a valid row-level path. Invalid JSON-shaped values
// return an error instead of being reported as either a match or a miss.
func FilterWithMatch(value any, paths []string) (any, bool, error) {
	if len(paths) == 0 {
		return value, true, nil
	}

	return filter(value, paths)
}

// filter walks the generic value tree, retaining only the nodes named by the
// provided paths.
func filter(value any, paths []string) (any, bool, error) {
	groups, hasTerminal := groupPaths(paths)
	if hasTerminal {
		return value, true, nil
	}

	switch v := value.(type) {
	case json.RawMessage:
		var decoded any
		if err := json.Unmarshal(v, &decoded); err != nil {
			return nil, false, fmt.Errorf("decoding JSON value: %w", err)
		}
		return filter(decoded, paths)
	case []byte:
		var decoded any
		if err := json.Unmarshal(v, &decoded); err != nil {
			return nil, false, fmt.Errorf("decoding JSON value: %w", err)
		}
		return filter(decoded, paths)
	case map[string]any:
		// Smart wrapper fallback: list-style endpoints commonly return
		// {"data": [...], ...}. If none of the requested paths match a
		// top-level key but there is exactly one array-valued sibling,
		// rewrite paths to broadcast through that wrapper. Mixed cases
		// (some paths match, some don't) keep strict semantics.
		anyMatch := false
		for key := range groups {
			if _, ok := v[key]; ok {
				anyMatch = true
				break
			}
		}
		if !anyMatch {
			arrayKeys := []string{}
			for key, val := range v {
				if isJSONArray(val) {
					arrayKeys = append(arrayKeys, key)
				}
			}
			if len(arrayKeys) == 1 {
				wrapper := arrayKeys[0]
				rewritten := make([]string, len(paths))
				for i, p := range paths {
					rewritten[i] = wrapper + "." + p
				}
				return filter(v, rewritten)
			}
		}

		out := make(map[string]any, len(groups))
		matched := false
		for key, remaining := range groups {
			child, ok := v[key]
			if !ok {
				continue
			}
			filtered, childMatched, err := filter(child, remaining)
			if err != nil {
				return nil, false, err
			}
			if !childMatched {
				continue
			}
			out[key] = filtered
			matched = true
		}
		return out, matched, nil
	case []any:
		// Array broadcast: apply the same paths to every element.
		out := make([]any, len(v))
		if len(v) == 0 {
			return out, true, nil
		}
		matched := false
		for i, item := range v {
			filtered, itemMatched, err := filter(item, paths)
			if err != nil {
				return nil, false, err
			}
			if itemMatched || isJSONContainer(filtered) {
				out[i] = filtered
			}
			matched = matched || itemMatched
		}
		return out, matched, nil
	case []json.RawMessage:
		// Array of unparsed JSON values — operations that page through API
		// responses without fully decoding each row hand back this shape
		// (e.g. workspaces list). Decode each element on the fly so the
		// filter logic doesn't need to know about late-bound JSON.
		out := make([]any, len(v))
		if len(v) == 0 {
			return out, true, nil
		}
		matched := false
		for i, item := range v {
			var decoded any
			if err := json.Unmarshal(item, &decoded); err != nil {
				return nil, false, fmt.Errorf("decoding JSON array element %d: %w", i, err)
			}
			filtered, itemMatched, err := filter(decoded, paths)
			if err != nil {
				return nil, false, err
			}
			if itemMatched || isJSONContainer(filtered) {
				out[i] = filtered
			}
			matched = matched || itemMatched
		}
		return out, matched, nil
	default:
		normalized, ok, err := normalizeJSONContainer(v)
		if err != nil {
			return nil, false, err
		}
		if ok {
			return filter(normalized, paths)
		}
		// Primitive — nothing below it can match. The caller decides whether
		// to retain the value based on whether a sibling matched.
		return v, false, nil
	}
}

// isJSONArray reports whether v is one of the array shapes the filter knows
// how to broadcast through.
func isJSONArray(v any) bool {
	switch v.(type) {
	case json.RawMessage, []byte:
		return false
	case []any, []json.RawMessage:
		return true
	}

	rv := reflect.ValueOf(v)
	return rv.IsValid() && (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array)
}

func isJSONContainer(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

// normalizeJSONContainer converts typed maps, slices, arrays, structs, and
// pointers into the generic JSON shapes used by the filter. Operation results
// are not required to use map[string]any (for example, connector creation
// returns map[string]string), but --fields must behave consistently for every
// JSON-serializable result.
func normalizeJSONContainer(value any) (any, bool, error) {
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return nil, false, nil
	}
	switch rv.Kind() {
	case reflect.Map, reflect.Slice, reflect.Array, reflect.Struct, reflect.Pointer:
	default:
		return nil, false, nil
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, false, fmt.Errorf("encoding JSON value: %w", err)
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return nil, false, fmt.Errorf("decoding normalized JSON value: %w", err)
	}
	return decoded, true, nil
}

// groupPaths splits each path on its first "." and returns a map of head
// segment to remaining paths. Bare paths (no dot) produce an empty remaining
// path, which signals "keep this subtree as-is" via hasTerminal.
func groupPaths(paths []string) (map[string][]string, bool) {
	groups := map[string][]string{}
	hasTerminal := false
	for _, p := range paths {
		if p == "" {
			hasTerminal = true
			continue
		}
		head, tail, hasTail := strings.Cut(p, ".")
		if !hasTail {
			groups[head] = append(groups[head], "")
			continue
		}
		groups[head] = append(groups[head], tail)
	}
	return groups, hasTerminal
}
