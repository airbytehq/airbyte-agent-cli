package output

import (
	"encoding/json"
	"strings"
)

// Filter returns a new value containing only the requested paths. Paths use
// dotted notation (e.g. "data.id"); when a path segment encounters an array,
// the remaining segments are applied to each element ("array broadcast").
//
// If paths is empty, the original value is returned unchanged. Individual
// missing paths are skipped when at least one requested path matches.
//
// json.RawMessage inputs are unmarshaled into a generic value before filtering.
func Filter(value any, paths []string) any {
	filtered, _ := FilterWithMatch(value, paths)
	return filtered
}

// FilterWithMatch filters value and reports whether at least one requested
// path matched. Empty arrays count as matches because they may have no rows
// against which to resolve a valid row-level path.
func FilterWithMatch(value any, paths []string) (any, bool) {
	if len(paths) == 0 {
		return value, true
	}

	// Normalize json.RawMessage / []byte into a generic value.
	switch v := value.(type) {
	case json.RawMessage:
		var decoded any
		if err := json.Unmarshal(v, &decoded); err != nil {
			return value, true
		}
		return filter(decoded, paths)
	case []byte:
		var decoded any
		if err := json.Unmarshal(v, &decoded); err != nil {
			return value, true
		}
		return filter(decoded, paths)
	}

	return filter(value, paths)
}

// filter walks the generic value tree, retaining only the nodes named by the
// provided paths.
func filter(value any, paths []string) (any, bool) {
	groups, hasTerminal := groupPaths(paths)
	if hasTerminal {
		return value, true
	}

	switch v := value.(type) {
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
			filtered, childMatched := filter(child, remaining)
			if !childMatched {
				continue
			}
			out[key] = filtered
			matched = true
		}
		return out, matched
	case []any:
		// Array broadcast: apply the same paths to every element.
		out := make([]any, len(v))
		if len(v) == 0 {
			return out, true
		}
		matched := false
		for i, item := range v {
			filtered, itemMatched := filter(item, paths)
			out[i] = filtered
			matched = matched || itemMatched
		}
		return out, matched
	case []json.RawMessage:
		// Array of unparsed JSON values — operations that page through API
		// responses without fully decoding each row hand back this shape
		// (e.g. workspaces list). Decode each element on the fly so the
		// filter logic doesn't need to know about late-bound JSON.
		out := make([]any, len(v))
		if len(v) == 0 {
			return out, true
		}
		matched := false
		for i, item := range v {
			var decoded any
			if err := json.Unmarshal(item, &decoded); err != nil {
				out[i] = item
				continue
			}
			filtered, itemMatched := filter(decoded, paths)
			out[i] = filtered
			matched = matched || itemMatched
		}
		return out, matched
	default:
		// Primitive — nothing to filter.
		return v, false
	}
}

// isJSONArray reports whether v is one of the array shapes the filter knows
// how to broadcast through.
func isJSONArray(v any) bool {
	switch v.(type) {
	case []any, []json.RawMessage:
		return true
	}
	return false
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
