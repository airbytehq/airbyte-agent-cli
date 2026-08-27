package localexec

import (
	"fmt"
	"strconv"
	"strings"
)

// Bounds on a JSONPath expression. A path may not be longer than
// maxJSONPathBytes and may not contain more than maxJSONPathSegments segments.
// These bound untrusted expressions embedded in a connector definition.
const (
	maxJSONPathBytes    = 1024
	maxJSONPathSegments = 64
)

// jsonPathSegment is one compiled step of a restricted JSONPath.
type jsonPathSegment struct {
	// kind is one of: "key", "index", "wildcard".
	kind string
	// key is set when kind == "key".
	key string
	// index is set when kind == "index".
	index int
}

// JSONPath is a compiled, restricted JSONPath expression. The supported grammar
// is intentionally tiny:
//
//	root            $
//	dotted key      $.a.b
//	bracket key     $['a']["b"]
//	numeric index   $.a[0]
//	wildcard        $.a[*]  or  $.a.*
//
// Everything else is rejected at compile time with a
// local_execution_unsupported error: recursive descent (..), filters (?(...)),
// script expressions, array slices (a:b), unions (a,b), and any other syntax.
type JSONPath struct {
	raw      string
	segments []jsonPathSegment
}

// String returns the original expression.
func (p *JSONPath) String() string { return p.raw }

// CompileJSONPath parses expr into a restricted JSONPath or returns a typed
// unsupported error naming the rejected construct (never echoing values).
func CompileJSONPath(expr string) (*JSONPath, error) {
	if len(expr) > maxJSONPathBytes {
		return nil, unsupportedError(fmt.Sprintf("JSONPath exceeds maximum length of %d bytes", maxJSONPathBytes))
	}
	if expr == "" || expr[0] != '$' {
		return nil, unsupportedError("JSONPath must start with the root token '$'")
	}
	// Reject recursive descent early; it is easy to misread as two dots.
	if strings.Contains(expr, "..") {
		return nil, unsupportedError("JSONPath recursive descent '..' is not supported")
	}

	p := &JSONPath{raw: expr}
	i := 1 // consumed '$'
	n := len(expr)
	for i < n {
		if len(p.segments) >= maxJSONPathSegments {
			return nil, unsupportedError(fmt.Sprintf("JSONPath exceeds maximum of %d segments", maxJSONPathSegments))
		}
		switch expr[i] {
		case '.':
			i++
			if i >= n {
				return nil, unsupportedError("JSONPath ends with a trailing '.'")
			}
			if expr[i] == '*' {
				p.segments = append(p.segments, jsonPathSegment{kind: "wildcard"})
				i++
				continue
			}
			// Dotted object key: read until the next '.' or '['.
			start := i
			for i < n && expr[i] != '.' && expr[i] != '[' {
				if !isKeyChar(expr[i]) {
					return nil, unsupportedError(fmt.Sprintf("JSONPath contains an unsupported character %q in a dotted key", string(expr[i])))
				}
				i++
			}
			if i == start {
				return nil, unsupportedError("JSONPath contains an empty dotted key")
			}
			p.segments = append(p.segments, jsonPathSegment{kind: "key", key: expr[start:i]})
		case '[':
			seg, next, err := parseBracket(expr, i)
			if err != nil {
				return nil, err
			}
			p.segments = append(p.segments, seg)
			i = next
		default:
			return nil, unsupportedError(fmt.Sprintf("JSONPath contains unexpected character %q", string(expr[i])))
		}
	}
	return p, nil
}

// parseBracket parses a single bracket expression starting at expr[i]=='[' and
// returns the compiled segment plus the index just past the closing ']'.
func parseBracket(expr string, i int) (jsonPathSegment, int, error) {
	n := len(expr)
	// expr[i] == '['
	j := i + 1
	if j >= n {
		return jsonPathSegment{}, 0, unsupportedError("JSONPath has an unterminated '['")
	}
	switch {
	case expr[j] == '*':
		if j+1 >= n || expr[j+1] != ']' {
			return jsonPathSegment{}, 0, unsupportedError("JSONPath wildcard must be written as '[*]'")
		}
		return jsonPathSegment{kind: "wildcard"}, j + 2, nil
	case expr[j] == '\'' || expr[j] == '"':
		quote := expr[j]
		j++
		var sb strings.Builder
		for j < n && expr[j] != quote {
			if expr[j] == '\\' { // support escaped quote and backslash
				if j+1 >= n {
					return jsonPathSegment{}, 0, unsupportedError("JSONPath has a trailing escape in a quoted key")
				}
				j++
				sb.WriteByte(expr[j])
				j++
				continue
			}
			sb.WriteByte(expr[j])
			j++
		}
		if j >= n {
			return jsonPathSegment{}, 0, unsupportedError("JSONPath has an unterminated quoted key")
		}
		j++ // consume closing quote
		if j >= n || expr[j] != ']' {
			return jsonPathSegment{}, 0, unsupportedError("JSONPath quoted key must be closed with ']'")
		}
		return jsonPathSegment{kind: "key", key: sb.String()}, j + 1, nil
	default:
		// Must be a numeric index. Read until ']', then reject anything that is
		// not a plain non-negative integer (this catches slices a:b and unions
		// a,b and expressions).
		start := j
		for j < n && expr[j] != ']' {
			j++
		}
		if j >= n {
			return jsonPathSegment{}, 0, unsupportedError("JSONPath has an unterminated '['")
		}
		inner := expr[start:j]
		if strings.ContainsAny(inner, ":") {
			return jsonPathSegment{}, 0, unsupportedError("JSONPath array slices '[a:b]' are not supported")
		}
		if strings.ContainsAny(inner, ",") {
			return jsonPathSegment{}, 0, unsupportedError("JSONPath unions '[a,b]' are not supported")
		}
		if strings.HasPrefix(strings.TrimSpace(inner), "?") {
			return jsonPathSegment{}, 0, unsupportedError("JSONPath filter expressions '[?(...)]' are not supported")
		}
		idx, err := strconv.Atoi(inner)
		if err != nil || idx < 0 {
			return jsonPathSegment{}, 0, unsupportedError("JSONPath supports only non-negative numeric indexes inside brackets")
		}
		return jsonPathSegment{kind: "index", index: idx}, j + 1, nil
	}
}

// isKeyChar reports whether c is allowed in an unquoted dotted key. Keys with
// spaces or punctuation must use the bracket-quoted form.
func isKeyChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z':
		return true
	case c >= 'A' && c <= 'Z':
		return true
	case c >= '0' && c <= '9':
		return true
	case c == '_' || c == '-':
		return true
	default:
		return false
	}
}

// Eval evaluates the path against a decoded-JSON value (map[string]any, []any,
// or scalar) and returns every matched value. A path that does not exist yields
// no matches (an empty slice) rather than an error. A wildcard over a map or
// array produces one match per element.
func (p *JSONPath) Eval(root any) []any {
	current := []any{root}
	for _, seg := range p.segments {
		var next []any
		for _, v := range current {
			switch seg.kind {
			case "key":
				if m, ok := v.(map[string]any); ok {
					if child, ok := m[seg.key]; ok {
						next = append(next, child)
					}
				}
			case "index":
				if arr, ok := v.([]any); ok {
					if seg.index < len(arr) {
						next = append(next, arr[seg.index])
					}
				}
			case "wildcard":
				switch t := v.(type) {
				case []any:
					next = append(next, t...)
				case map[string]any:
					// Deterministic order is not guaranteed for maps; callers
					// that need order should index explicitly.
					for _, e := range t {
						next = append(next, e)
					}
				}
			}
		}
		current = next
		if len(current) == 0 {
			return nil
		}
	}
	return current
}

// EvalOne returns the single matched value and true, or false when the path
// matched zero or more than one value. It is a convenience for callers that
// require exactly one result.
func (p *JSONPath) EvalOne(root any) (any, bool) {
	matches := p.Eval(root)
	if len(matches) != 1 {
		return nil, false
	}
	return matches[0], true
}
