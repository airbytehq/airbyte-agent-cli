package localexec

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Content types for the supported body encodings.
const (
	contentTypeJSON      = "application/json"
	contentTypeForm      = "application/x-www-form-urlencoded"
	contentTypeMultipart = "multipart/form-data"
)

// multipartBoundary is a fixed boundary so that compiled multipart bodies are
// byte-for-byte deterministic (required for golden request tests). It is chosen
// to be improbable in field values; a body whose values contain the boundary is
// rejected rather than silently corrupted.
const multipartBoundary = "airbyteagentlocalexecboundary"

// Cookie is a single request cookie in the compiled plan.
type Cookie struct {
	Name  string
	Value string
}

// RequestPlan is the immutable result of compiling an operation into a concrete
// HTTP request. It is a plain value struct with no mutating methods. Phase 3
// merges auth into a copy of this plan and hands it to the transport.
//
// Headers/Query/Cookies are stored as ordered slices so the plan serializes
// deterministically; callers that want maps can index them.
type RequestPlan struct {
	Method      string
	URL         string
	Headers     []Header
	Cookies     []Cookie
	ContentType string
	Body        []byte
}

// Header is a single request header in the compiled plan.
type Header struct {
	Name  string
	Value string
}

// Inputs carries the already-resolved values compiled into a request. Config
// holds resolved config values (server variables, injected config) and Params
// holds the bundle's operation params. This phase never hydrates: callers pass
// values that are already concrete (Phase 3 hydrates source_config first, then
// calls BuildRequest).
type Inputs struct {
	Params map[string]any
	Config map[string]any
}

// BuildRequest compiles a resolved operation plus inputs into an immutable
// request plan. It applies path/query/header/cookie parameter placement,
// deepObject query serialization, operation defaults, required/type/enum
// validation, and body encoding (JSON, form, multipart, GraphQL). It performs
// no hydration and no network I/O.
func BuildRequest(r *ResolvedOperation, in Inputs) (*RequestPlan, error) {
	if r == nil || r.Operation == nil {
		return nil, validationError("no resolved operation to compile")
	}
	params := in.Params
	if params == nil {
		params = map[string]any{}
	}
	config := in.Config
	if config == nil {
		config = map[string]any{}
	}

	base, err := resolveServerURL(r.Server, config, params)
	if err != nil {
		return nil, err
	}
	base = strings.TrimRight(base, "/")

	// Classify declared parameters by location.
	pathParams := map[string]*Parameter{}
	var queryParams, headerParams, cookieParams []*Parameter
	for _, p := range r.Operation.Parameters {
		if p == nil {
			continue
		}
		switch p.In {
		case "path":
			pathParams[p.Name] = p
		case "query":
			queryParams = append(queryParams, p)
		case "header":
			headerParams = append(headerParams, p)
		case "cookie":
			cookieParams = append(cookieParams, p)
		default:
			return nil, validationError(fmt.Sprintf("parameter %q has unsupported location %q", p.Name, p.In))
		}
	}

	pathStr, err := buildPath(r.Path, pathParams, params)
	if err != nil {
		return nil, err
	}

	query, err := buildQuery(queryParams, params)
	if err != nil {
		return nil, err
	}

	fullURL := base + pathStr
	if query != "" {
		fullURL += "?" + query
	}

	headers, err := buildHeaders(headerParams, params)
	if err != nil {
		return nil, err
	}
	cookies, err := buildCookies(cookieParams, params)
	if err != nil {
		return nil, err
	}

	contentType, body, err := buildBody(r, params)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		headers = append(headers, Header{Name: "Content-Type", Value: contentType})
	}
	sort.Slice(headers, func(i, j int) bool {
		if headers[i].Name == headers[j].Name {
			return headers[i].Value < headers[j].Value
		}
		return headers[i].Name < headers[j].Name
	})

	return &RequestPlan{
		Method:      r.Method,
		URL:         fullURL,
		Headers:     headers,
		Cookies:     cookies,
		ContentType: contentType,
		Body:        body,
	}, nil
}

// resolveParamValue returns the effective value for a parameter, applying its
// schema default and enforcing required-ness and type/enum constraints.
func resolveParamValue(p *Parameter, params map[string]any) (any, bool, error) {
	v, ok := params[p.Name]
	if !ok && p.Schema != nil && p.Schema.Default != nil {
		v, ok = p.Schema.Default, true
	}
	if !ok {
		if p.Required {
			return nil, false, validationError(fmt.Sprintf("required %s parameter %q is missing", p.In, p.Name))
		}
		return nil, false, nil
	}
	if err := validateAgainstSchema(p.Name, v, p.Schema); err != nil {
		return nil, false, err
	}
	return v, true, nil
}

// buildPath substitutes path parameters into the path template. Every {name}
// placeholder must correspond to a declared path parameter with a value, and
// values are percent-encoded so they cannot inject extra path structure.
func buildPath(pathTmpl string, pathParams map[string]*Parameter, params map[string]any) (string, error) {
	var b strings.Builder
	i := 0
	for i < len(pathTmpl) {
		c := pathTmpl[i]
		if c != '{' {
			b.WriteByte(c)
			i++
			continue
		}
		end := strings.IndexByte(pathTmpl[i:], '}')
		if end < 0 {
			return "", validationError("path has an unterminated '{' placeholder")
		}
		name := pathTmpl[i+1 : i+end]
		p, ok := pathParams[name]
		if !ok {
			return "", validationError(fmt.Sprintf("path placeholder %q has no declared path parameter", name))
		}
		v, present, err := resolveParamValue(p, params)
		if err != nil {
			return "", err
		}
		if !present {
			return "", validationError(fmt.Sprintf("path parameter %q is missing", name))
		}
		s, ok := scalarString(v)
		if !ok {
			return "", validationError(fmt.Sprintf("path parameter %q must be a scalar value", name))
		}
		b.WriteString(escapePathValue(s))
		i += end + 1
	}
	return b.String(), nil
}

// escapePathValue percent-encodes a value for a single path segment, escaping
// '/' explicitly so the value cannot introduce new segments.
func escapePathValue(s string) string {
	return strings.ReplaceAll(url.PathEscape(s), "/", "%2F")
}

// queryPair is an encoded query key with an unencoded value pending encoding.
type queryPair struct {
	encKey string
	value  string
}

// buildQuery serializes query parameters (including deepObject style and array
// explosion) into a sorted, percent-encoded query string.
func buildQuery(queryParams []*Parameter, params map[string]any) (string, error) {
	var pairs []queryPair
	for _, p := range queryParams {
		v, present, err := resolveParamValue(p, params)
		if err != nil {
			return "", err
		}
		if !present {
			continue
		}
		if p.Style == "deepObject" {
			obj, ok := v.(map[string]any)
			if !ok {
				return "", validationError(fmt.Sprintf("deepObject parameter %q must be an object", p.Name))
			}
			dp, err := serializeDeepObject(p.Name, obj)
			if err != nil {
				return "", err
			}
			for _, d := range dp {
				pairs = append(pairs, queryPair{encKey: encodeDeepObjectKey(d.Key), value: d.Value})
			}
			continue
		}
		if arr, ok := v.([]any); ok {
			for _, e := range arr {
				s, ok := scalarString(e)
				if !ok {
					return "", validationError(fmt.Sprintf("array query parameter %q must contain only scalar values", p.Name))
				}
				pairs = append(pairs, queryPair{encKey: url.QueryEscape(p.Name), value: s})
			}
			continue
		}
		s, ok := scalarString(v)
		if !ok {
			return "", validationError(fmt.Sprintf("query parameter %q must be a scalar value", p.Name))
		}
		pairs = append(pairs, queryPair{encKey: url.QueryEscape(p.Name), value: s})
	}

	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].encKey == pairs[j].encKey {
			return pairs[i].value < pairs[j].value
		}
		return pairs[i].encKey < pairs[j].encKey
	})
	var parts []string
	for _, p := range pairs {
		parts = append(parts, p.encKey+"="+url.QueryEscape(p.value))
	}
	return strings.Join(parts, "&"), nil
}

// encodeDeepObjectKey encodes a deepObject key of the form name[subkey],
// escaping the name and subkey but preserving the literal brackets that are the
// deepObject wire convention.
func encodeDeepObjectKey(key string) string {
	open := strings.IndexByte(key, '[')
	if open < 0 || !strings.HasSuffix(key, "]") {
		return url.QueryEscape(key)
	}
	name := key[:open]
	sub := key[open+1 : len(key)-1]
	return url.QueryEscape(name) + "[" + url.QueryEscape(sub) + "]"
}

// buildHeaders resolves header parameters into ordered headers.
func buildHeaders(headerParams []*Parameter, params map[string]any) ([]Header, error) {
	var headers []Header
	for _, p := range headerParams {
		v, present, err := resolveParamValue(p, params)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		s, ok := scalarString(v)
		if !ok {
			return nil, validationError(fmt.Sprintf("header parameter %q must be a scalar value", p.Name))
		}
		if strings.ContainsAny(s, "\r\n") {
			return nil, validationError(fmt.Sprintf("header parameter %q contains illegal newline characters", p.Name))
		}
		headers = append(headers, Header{Name: p.Name, Value: s})
	}
	return headers, nil
}

// buildCookies resolves cookie parameters into ordered cookies.
func buildCookies(cookieParams []*Parameter, params map[string]any) ([]Cookie, error) {
	var cookies []Cookie
	for _, p := range cookieParams {
		v, present, err := resolveParamValue(p, params)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		s, ok := scalarString(v)
		if !ok {
			return nil, validationError(fmt.Sprintf("cookie parameter %q must be a scalar value", p.Name))
		}
		if strings.ContainsAny(s, "\r\n;") {
			return nil, validationError(fmt.Sprintf("cookie parameter %q contains illegal characters", p.Name))
		}
		cookies = append(cookies, Cookie{Name: p.Name, Value: s})
	}
	sort.Slice(cookies, func(i, j int) bool { return cookies[i].Name < cookies[j].Name })
	return cookies, nil
}

// buildBody encodes the request body according to the resolved body type. It
// returns the content type and encoded bytes; an operation with no body returns
// ("", nil, nil).
func buildBody(r *ResolvedOperation, params map[string]any) (string, []byte, error) {
	bodyType := r.Operation.BodyType
	if bodyType == "" {
		bodyType = inferBodyType(r.Operation.RequestBody)
	}
	switch bodyType {
	case "":
		return "", nil, nil
	case "graphql":
		return buildGraphQLBody(r.Operation, params)
	case "json":
		return buildJSONBody(r.Operation.RequestBody, params)
	case "form":
		return buildFormBody(r.Operation.RequestBody, params)
	case "multipart":
		return buildMultipartBody(r.Operation.RequestBody, params)
	default:
		return "", nil, unsupportedError(fmt.Sprintf("body type %q is not supported by local execution", bodyType))
	}
}

// inferBodyType derives a body type from a request body's declared content
// media types when no explicit x-airbyte-body-type is set.
func inferBodyType(rb *RequestBody) string {
	if rb == nil {
		return ""
	}
	for mt := range rb.Content {
		switch normalizeMediaType(mt) {
		case contentTypeJSON:
			return "json"
		case contentTypeForm:
			return "form"
		case contentTypeMultipart:
			return "multipart"
		}
	}
	return ""
}

func normalizeMediaType(mt string) string {
	mt = strings.ToLower(strings.TrimSpace(mt))
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	return mt
}

// bodyObject builds the body key/value object from the request body's schema
// properties, applying defaults and enforcing required-ness and type/enum.
func bodyObject(rb *RequestBody, mediaType string, params map[string]any) (map[string]any, error) {
	if rb == nil {
		return map[string]any{}, nil
	}
	mt := rb.Content[mediaType]
	if mt == nil {
		// Fall back to any declared content whose normalized type matches.
		for k, v := range rb.Content {
			if normalizeMediaType(k) == mediaType {
				mt = v
				break
			}
		}
	}
	if mt == nil || mt.Schema == nil {
		return map[string]any{}, nil
	}
	schema := mt.Schema
	required := map[string]bool{}
	for _, name := range schema.Required {
		required[name] = true
	}
	obj := map[string]any{}
	names := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		prop := schema.Properties[name]
		v, ok := params[name]
		if !ok && prop != nil && prop.Default != nil {
			v, ok = prop.Default, true
		}
		if !ok {
			if required[name] {
				return nil, validationError(fmt.Sprintf("required body property %q is missing", name))
			}
			continue
		}
		if err := validateAgainstSchema(name, v, prop); err != nil {
			return nil, err
		}
		obj[name] = v
	}
	return obj, nil
}

func buildJSONBody(rb *RequestBody, params map[string]any) (string, []byte, error) {
	obj, err := bodyObject(rb, contentTypeJSON, params)
	if err != nil {
		return "", nil, err
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return "", nil, validationError("failed to encode JSON body")
	}
	return contentTypeJSON, b, nil
}

func buildGraphQLBody(op *Operation, params map[string]any) (string, []byte, error) {
	if op.GraphQLQuery == "" {
		return "", nil, validationError("graphql operation is missing x-airbyte-graphql-query")
	}
	// Variables are the operation params. A dedicated "variables" param, when
	// present, is used verbatim; otherwise the whole params map is the variable
	// set (minus nothing, since GraphQL has no other placement).
	var variables any
	if v, ok := params["variables"]; ok {
		variables = v
	} else {
		variables = params
	}
	payload := map[string]any{
		"query":     op.GraphQLQuery,
		"variables": variables,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", nil, validationError("failed to encode GraphQL body")
	}
	return contentTypeJSON, b, nil
}

func buildFormBody(rb *RequestBody, params map[string]any) (string, []byte, error) {
	obj, err := bodyObject(rb, contentTypeForm, params)
	if err != nil {
		return "", nil, err
	}
	values := url.Values{}
	names := make([]string, 0, len(obj))
	for name := range obj {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if arr, ok := obj[name].([]any); ok {
			for _, e := range arr {
				s, ok := scalarString(e)
				if !ok {
					return "", nil, validationError(fmt.Sprintf("form field %q must contain only scalar values", name))
				}
				values.Add(name, s)
			}
			continue
		}
		s, ok := scalarString(obj[name])
		if !ok {
			return "", nil, validationError(fmt.Sprintf("form field %q must be a scalar value", name))
		}
		values.Add(name, s)
	}
	return contentTypeForm, []byte(values.Encode()), nil
}

func buildMultipartBody(rb *RequestBody, params map[string]any) (string, []byte, error) {
	obj, err := bodyObject(rb, contentTypeMultipart, params)
	if err != nil {
		return "", nil, err
	}
	names := make([]string, 0, len(obj))
	for name := range obj {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		s, ok := scalarString(obj[name])
		if !ok {
			return "", nil, validationError(fmt.Sprintf("multipart field %q must be a scalar value", name))
		}
		if strings.Contains(s, multipartBoundary) {
			return "", nil, validationError(fmt.Sprintf("multipart field %q collides with the encoder boundary", name))
		}
		if strings.ContainsAny(name, "\"\r\n") {
			return "", nil, validationError(fmt.Sprintf("multipart field name %q contains illegal characters", name))
		}
		b.WriteString("--" + multipartBoundary + "\r\n")
		b.WriteString(fmt.Sprintf("Content-Disposition: form-data; name=%q\r\n\r\n", name))
		b.WriteString(s)
		b.WriteString("\r\n")
	}
	b.WriteString("--" + multipartBoundary + "--\r\n")
	ct := contentTypeMultipart + "; boundary=" + multipartBoundary
	return ct, []byte(b.String()), nil
}

// validateAgainstSchema enforces a schema's declared type and enum against a
// resolved value. It validates the top level only; nested object/array element
// validation is intentionally shallow for this phase.
func validateAgainstSchema(name string, v any, schema *Schema) error {
	if schema == nil {
		return nil
	}
	if v == nil {
		if schema.Nullable {
			return nil
		}
		return validationError(fmt.Sprintf("parameter %q must not be null", name))
	}
	if schema.Type != "" {
		if err := validateType(name, v, schema.Type); err != nil {
			return err
		}
	}
	if len(schema.Enum) > 0 && !enumContains(schema.Enum, v) {
		return validationError(fmt.Sprintf("parameter %q is not one of the allowed values", name))
	}
	return nil
}

func validateType(name string, v any, typ string) error {
	switch typ {
	case "string":
		if _, ok := v.(string); !ok {
			return validationError(fmt.Sprintf("parameter %q must be a string", name))
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return validationError(fmt.Sprintf("parameter %q must be a boolean", name))
		}
	case "integer":
		if !isIntegerValue(v) {
			return validationError(fmt.Sprintf("parameter %q must be an integer", name))
		}
	case "number":
		if !isNumberValue(v) {
			return validationError(fmt.Sprintf("parameter %q must be a number", name))
		}
	case "array":
		if _, ok := v.([]any); !ok {
			return validationError(fmt.Sprintf("parameter %q must be an array", name))
		}
	case "object":
		if _, ok := v.(map[string]any); !ok {
			return validationError(fmt.Sprintf("parameter %q must be an object", name))
		}
	}
	return nil
}

func isIntegerValue(v any) bool {
	switch t := v.(type) {
	case int, int64:
		return true
	case float64:
		return t == float64(int64(t))
	default:
		return false
	}
}

func isNumberValue(v any) bool {
	switch v.(type) {
	case int, int64, float64:
		return true
	default:
		return false
	}
}

func enumContains(enum []any, v any) bool {
	for _, e := range enum {
		if enumEqual(e, v) {
			return true
		}
	}
	return false
}

// enumEqual compares an enum entry (as decoded from YAML) against a value (as
// decoded from JSON), tolerating int/float numeric skew.
func enumEqual(a, b any) bool {
	if a == b {
		return true
	}
	af, aok := numericValue(a)
	bf, bok := numericValue(b)
	if aok && bok {
		return af == bf
	}
	return false
}

func numericValue(v any) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case float64:
		return t, true
	default:
		return 0, false
	}
}
