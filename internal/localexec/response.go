package localexec

// response.go converts a bounded HTTP response into the logical result payload
// that the executor returns — mirroring the record extraction, filtering,
// transformation, projection, and truncation behavior of Sonar's local
// executor.
//
// All JSONPath extraction reuses the restricted evaluator from jsonpath.go; no
// third-party JSONPath library is introduced. Every JSONPath in the definition
// is compiled during static validation (parseResponseSpec) BEFORE any secret is
// resolved or any socket is opened, so a malformed path fails fast and never
// reaches the provider.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Response shaping defaults. Injectable via executor options.
const (
	defaultMaxRecords        = 10000
	defaultMaxFieldStringLen = 65536
	truncationMarker         = "...[truncated]"
)

// recordFilterSpec keeps records whose Path evaluates to a value that is either
// truthy (no Equals) or equal to Equals.
type recordFilterSpec struct {
	path      *JSONPath
	equals    any
	hasEquals bool
}

// responseSpec is the compiled, hydration-independent response-shaping plan for
// a resolved operation. Nil fields mean the corresponding extension is absent.
type responseSpec struct {
	selector       *JSONPath
	meta           map[string]*JSONPath
	metaOrder      []string
	filter         *recordFilterSpec
	transform      map[string]*JSONPath
	transformOrder []string
	errorPath      *JSONPath
}

// shapeOptions carries per-bundle projection + truncation settings.
type shapeOptions struct {
	selectFields      []string
	excludeFields     []string
	skipTruncation    bool
	maxRecords        int
	maxFieldStringLen int
}

// Result is the logical execution result returned by the executor. Phase 4
// inserts it into the standard connectors-execute envelope (with the bundle
// removed). Records and metadata are the extracted response data; they are the
// user-facing payload, not an error message, so they are not subject to the
// error-path redaction rules.
type Result struct {
	Records         []any          `json:"records"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	RecordCount     int            `json:"record_count"`
	Truncated       bool           `json:"truncated"`
	ExecutionTimeMs int64          `json:"execution_time_ms"`
}

// parseResponseSpec compiles all response-shaping extensions of an operation.
// It runs during static validation; every JSONPath is compiled here so that a
// malformed expression is reported before hydration.
func parseResponseSpec(op *ResolvedOperation) (*responseSpec, error) {
	spec := &responseSpec{}
	exts := op.Operation.Extensions

	// record selector
	var selector string
	if ok, err := decodeExt(exts, "x-airbyte-record-selector", &selector); err != nil {
		return nil, err
	} else if ok && selector != "" {
		p, err := CompileJSONPath(selector)
		if err != nil {
			return nil, err
		}
		spec.selector = p
	}

	// record meta: name -> JSONPath
	var metaRaw map[string]string
	if ok, err := decodeExt(exts, "x-airbyte-record-meta", &metaRaw); err != nil {
		return nil, err
	} else if ok && len(metaRaw) > 0 {
		spec.meta = map[string]*JSONPath{}
		for name, expr := range metaRaw {
			p, err := CompileJSONPath(expr)
			if err != nil {
				return nil, err
			}
			spec.meta[name] = p
			spec.metaOrder = append(spec.metaOrder, name)
		}
		sort.Strings(spec.metaOrder)
	}

	// record filter
	var filterRaw struct {
		Path   string `yaml:"path"`
		Equals any    `yaml:"equals"`
	}
	if ok, err := decodeExt(exts, "x-airbyte-record-filter", &filterRaw); err != nil {
		return nil, err
	} else if ok && filterRaw.Path != "" {
		p, err := CompileJSONPath(filterRaw.Path)
		if err != nil {
			return nil, err
		}
		spec.filter = &recordFilterSpec{path: p, equals: filterRaw.Equals, hasEquals: filterRaw.Equals != nil}
	}

	// record transform: outputKey -> JSONPath relative to each record
	var transformRaw map[string]string
	if ok, err := decodeExt(exts, "x-airbyte-record-transform", &transformRaw); err != nil {
		return nil, err
	} else if ok && len(transformRaw) > 0 {
		spec.transform = map[string]*JSONPath{}
		for name, expr := range transformRaw {
			p, err := CompileJSONPath(expr)
			if err != nil {
				return nil, err
			}
			spec.transform[name] = p
			spec.transformOrder = append(spec.transformOrder, name)
		}
		sort.Strings(spec.transformOrder)
	}

	// error check
	var errCheck struct {
		ErrorPath string `yaml:"error_path"`
	}
	if ok, err := decodeExt(exts, "x-airbyte-error-check", &errCheck); err != nil {
		return nil, err
	} else if ok && errCheck.ErrorPath != "" {
		p, err := CompileJSONPath(errCheck.ErrorPath)
		if err != nil {
			return nil, err
		}
		spec.errorPath = p
	}

	return spec, nil
}

// shapeResponse decodes and shapes a bounded HTTP response into a Result.
func shapeResponse(spec *responseSpec, resp *httpResponse, op *ResolvedOperation, opts shapeOptions) (*Result, error) {
	if opts.maxRecords <= 0 {
		opts.maxRecords = defaultMaxRecords
	}
	if opts.maxFieldStringLen <= 0 {
		opts.maxFieldStringLen = defaultMaxFieldStringLen
	}

	decoded, isJSON, err := decodeBody(resp, op)
	if err != nil {
		return nil, err
	}

	// Error-check: a 2xx body may still carry an API-level error. Only meaningful
	// for structured (JSON) responses.
	if isJSON && spec.errorPath != nil {
		if matches := spec.errorPath.Eval(decoded); hasNonNull(matches) {
			return nil, connectorError(fmt.Sprintf(
				"connector reported an error in its response (HTTP %d)", resp.StatusCode))
		}
	}

	records := extractRecords(spec, decoded)

	if spec.filter != nil {
		records = applyFilter(spec.filter, records)
	}
	if spec.transform != nil {
		records = applyTransform(spec, records)
	}
	records = projectRecords(records, opts.selectFields, opts.excludeFields)

	truncated := false
	if len(records) > opts.maxRecords {
		records = records[:opts.maxRecords]
		truncated = true
	}
	if !opts.skipTruncation {
		for i, rec := range records {
			r, t := truncateStrings(rec, opts.maxFieldStringLen)
			records[i] = r
			truncated = truncated || t
		}
	}

	if records == nil {
		records = []any{}
	}

	result := &Result{
		Records:     records,
		RecordCount: len(records),
		Truncated:   truncated,
	}
	if spec.meta != nil {
		result.Metadata = extractMeta(spec, decoded)
	}
	return result, nil
}

// decodeBody decodes the response body into a Go value and reports whether it
// was parsed as JSON. A JSON-expected body that is malformed is a sanitized
// connector_execution_error. An empty body yields a nil value.
func decodeBody(resp *httpResponse, op *ResolvedOperation) (any, bool, error) {
	jsonExpected := responseIsJSON(resp.Header, op)
	if len(strings.TrimSpace(string(resp.Body))) == 0 {
		return nil, jsonExpected, nil
	}
	if jsonExpected {
		var v any
		if err := json.Unmarshal(resp.Body, &v); err != nil {
			return nil, true, connectorError("connector returned a response that is not valid JSON")
		}
		return v, true, nil
	}
	return string(resp.Body), false, nil
}

// responseIsJSON decides whether to parse the body as JSON, preferring the
// response Content-Type header and falling back to the operation's declared
// success-response media types. Unknown content defaults to JSON.
func responseIsJSON(header map[string][]string, op *ResolvedOperation) bool {
	ct := ""
	if header != nil {
		if vals := header["Content-Type"]; len(vals) > 0 {
			ct = vals[0]
		}
	}
	if ct != "" {
		n := normalizeMediaType(ct)
		switch {
		case strings.Contains(n, "json"):
			return true
		case strings.HasPrefix(n, "text/"):
			return false
		default:
			return true
		}
	}
	// Fall back to the operation's declared success responses.
	for status, resp := range op.Operation.Responses {
		if resp == nil || !(strings.HasPrefix(status, "2") || status == "default") {
			continue
		}
		for mt := range resp.Content {
			if strings.Contains(normalizeMediaType(mt), "json") {
				return true
			}
		}
	}
	return true
}

// extractRecords produces the record list from the decoded body. With a record
// selector the matches are the records; without one an array body becomes the
// record list and any other value becomes a single record.
func extractRecords(spec *responseSpec, decoded any) []any {
	if decoded == nil {
		return nil
	}
	if spec.selector != nil {
		return spec.selector.Eval(decoded)
	}
	if arr, ok := decoded.([]any); ok {
		return arr
	}
	return []any{decoded}
}

// applyFilter keeps records matching the filter spec.
func applyFilter(f *recordFilterSpec, records []any) []any {
	out := make([]any, 0, len(records))
	for _, rec := range records {
		matches := f.path.Eval(rec)
		if len(matches) == 0 {
			continue
		}
		if f.hasEquals {
			kept := false
			for _, m := range matches {
				if enumEqual(m, f.equals) {
					kept = true
					break
				}
			}
			if !kept {
				continue
			}
		} else if !isTruthy(matches[0]) {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// applyTransform rebuilds each record as an object of outputKey -> extracted
// value, in sorted key order.
func applyTransform(spec *responseSpec, records []any) []any {
	out := make([]any, 0, len(records))
	for _, rec := range records {
		obj := map[string]any{}
		for _, name := range spec.transformOrder {
			if v, ok := spec.transform[name].EvalOne(rec); ok {
				obj[name] = v
			}
		}
		out = append(out, obj)
	}
	return out
}

// projectRecords applies select/exclude field projection to each map record.
func projectRecords(records []any, selectFields, excludeFields []string) []any {
	if len(selectFields) == 0 && len(excludeFields) == 0 {
		return records
	}
	out := make([]any, len(records))
	for i, rec := range records {
		m, ok := rec.(map[string]any)
		if !ok {
			out[i] = rec
			continue
		}
		if len(selectFields) > 0 {
			m = selectPaths(m, selectFields)
		}
		if len(excludeFields) > 0 {
			m = excludePaths(m, excludeFields)
		}
		out[i] = m
	}
	return out
}

// selectPaths returns a new object containing only the dotted paths in fields.
func selectPaths(rec map[string]any, fields []string) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		segs := strings.Split(f, ".")
		if v, ok := getPath(rec, segs); ok {
			setPath(out, segs, v)
		}
	}
	return out
}

// excludePaths returns a deep copy of rec with the dotted paths in fields removed.
func excludePaths(rec map[string]any, fields []string) map[string]any {
	out := deepCopyMap(rec)
	for _, f := range fields {
		delPath(out, strings.Split(f, "."))
	}
	return out
}

func getPath(m map[string]any, segs []string) (any, bool) {
	var cur any = m
	for _, s := range segs {
		cm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = cm[s]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func setPath(m map[string]any, segs []string, val any) {
	for i := 0; i < len(segs)-1; i++ {
		s := segs[i]
		child, ok := m[s].(map[string]any)
		if !ok {
			child = map[string]any{}
			m[s] = child
		}
		m = child
	}
	m[segs[len(segs)-1]] = val
}

func delPath(m map[string]any, segs []string) {
	for i := 0; i < len(segs)-1; i++ {
		child, ok := m[segs[i]].(map[string]any)
		if !ok {
			return
		}
		m = child
	}
	delete(m, segs[len(segs)-1])
}

func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyMap(t)
	case []any:
		cp := make([]any, len(t))
		for i, e := range t {
			cp[i] = deepCopyValue(e)
		}
		return cp
	default:
		return v
	}
}

// truncateStrings walks a decoded value and truncates every string longer than
// maxLen, returning the (possibly rebuilt) value and whether anything was cut.
func truncateStrings(v any, maxLen int) (any, bool) {
	switch t := v.(type) {
	case string:
		if len(t) > maxLen {
			return t[:maxLen] + truncationMarker, true
		}
		return t, false
	case map[string]any:
		truncated := false
		out := make(map[string]any, len(t))
		for k, e := range t {
			ne, tr := truncateStrings(e, maxLen)
			out[k] = ne
			truncated = truncated || tr
		}
		return out, truncated
	case []any:
		truncated := false
		out := make([]any, len(t))
		for i, e := range t {
			ne, tr := truncateStrings(e, maxLen)
			out[i] = ne
			truncated = truncated || tr
		}
		return out, truncated
	default:
		return v, false
	}
}

// extractMeta evaluates each metadata JSONPath against the whole decoded body.
func extractMeta(spec *responseSpec, decoded any) map[string]any {
	meta := map[string]any{}
	for _, name := range spec.metaOrder {
		if v, ok := spec.meta[name].EvalOne(decoded); ok {
			meta[name] = v
		}
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}

// hasNonNull reports whether any match is a non-nil value.
func hasNonNull(matches []any) bool {
	for _, m := range matches {
		if m != nil {
			return true
		}
	}
	return false
}

// isTruthy reports whether a value should be treated as "present" by a filter
// with no explicit Equals: non-nil, non-false, and non-empty-string.
func isTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	default:
		return true
	}
}
