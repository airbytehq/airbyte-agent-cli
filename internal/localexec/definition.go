package localexec

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// allowedInlineExtensions is the set of x-airbyte-* extension keys that this
// phase recognizes but does not model with an explicit struct field. They are
// carried through untouched for later phases (auth mapping, config
// normalization/injection, record extraction/filtering/transformation, response
// error checks, retry config). Any x-airbyte-* key that is neither an explicit
// struct field nor in this set is a statically-detectable unsupported feature.
var allowedInlineExtensions = map[string]bool{
	"x-airbyte-auth":                 true,
	"x-airbyte-config-normalization": true,
	"x-airbyte-config-injection":     true,
	"x-airbyte-scope":                true,
	"x-airbyte-nullable":             true,
	"x-airbyte-record-selector":      true,
	"x-airbyte-record-meta":          true,
	"x-airbyte-record-filter":        true,
	"x-airbyte-record-transform":     true,
	"x-airbyte-error-check":          true,
	"x-airbyte-retry":                true,
}

// supportedActions is the closed set of actions this local executor compiles.
var supportedActions = map[string]bool{
	"list":       true,
	"get":        true,
	"create":     true,
	"update":     true,
	"delete":     true,
	"api_search": true,
	"authorize":  true,
}

// knownUnsupportedActions are actions that are recognized but deliberately not
// implemented locally. They are rejected as local_execution_unsupported before
// any hydration or network call.
var knownUnsupportedActions = map[string]bool{
	"describe": true,
	"download": true,
}

// Definition is the minimal, extension-aware model of a connector's OpenAPI
// document required to compile a request plan. Only the subset exercised by the
// parity fixtures is modeled; anything else is either ignored (standard OpenAPI
// keys we do not need) or rejected (unknown x-airbyte-* extensions).
type Definition struct {
	OpenAPI    string                `yaml:"openapi"`
	Servers    []Server              `yaml:"servers"`
	Paths      map[string]*PathItem  `yaml:"paths"`
	Components Components            `yaml:"components"`
	Security   []map[string][]string `yaml:"security"`
	Extensions map[string]yaml.Node  `yaml:",inline"`
}

// Server models an OpenAPI server entry with variable declarations.
type Server struct {
	URL        string                    `yaml:"url"`
	Variables  map[string]ServerVariable `yaml:"variables"`
	Extensions map[string]yaml.Node      `yaml:",inline"`
}

// ServerVariable models an OpenAPI server variable plus two x-airbyte
// extensions: scope (redirect the config lookup to a nested path) and nullable.
type ServerVariable struct {
	Default    string               `yaml:"default"`
	Enum       []string             `yaml:"enum"`
	Scope      string               `yaml:"x-airbyte-scope"`
	Nullable   bool                 `yaml:"x-airbyte-nullable"`
	Extensions map[string]yaml.Node `yaml:",inline"`
}

// PathItem holds the operations declared under a single path.
type PathItem struct {
	Get        *Operation           `yaml:"get"`
	Post       *Operation           `yaml:"post"`
	Put        *Operation           `yaml:"put"`
	Patch      *Operation           `yaml:"patch"`
	Delete     *Operation           `yaml:"delete"`
	Extensions map[string]yaml.Node `yaml:",inline"`
}

// Operation models a single OpenAPI operation and its x-airbyte extensions.
type Operation struct {
	OperationID  string                `yaml:"operationId"`
	Parameters   []*Parameter          `yaml:"parameters"`
	RequestBody  *RequestBody          `yaml:"requestBody"`
	Responses    map[string]*Response  `yaml:"responses"`
	Security     []map[string][]string `yaml:"security"`
	Entity       string                `yaml:"x-airbyte-entity"`
	Action       string                `yaml:"x-airbyte-action"`
	BodyType     string                `yaml:"x-airbyte-body-type"`
	PathOverride string                `yaml:"x-airbyte-path-override"`
	GraphQLQuery string                `yaml:"x-airbyte-graphql-query"`
	Extensions   map[string]yaml.Node  `yaml:",inline"`
}

// Parameter models an OpenAPI parameter (path/query/header/cookie).
type Parameter struct {
	Name       string               `yaml:"name"`
	In         string               `yaml:"in"`
	Required   bool                 `yaml:"required"`
	Style      string               `yaml:"style"`
	Schema     *Schema              `yaml:"schema"`
	Extensions map[string]yaml.Node `yaml:",inline"`
}

// RequestBody models an OpenAPI request body.
type RequestBody struct {
	Required   bool                  `yaml:"required"`
	Content    map[string]*MediaType `yaml:"content"`
	Extensions map[string]yaml.Node  `yaml:",inline"`
}

// MediaType models one entry of a content map.
type MediaType struct {
	Schema *Schema `yaml:"schema"`
}

// Response models the subset of an OpenAPI response we inspect (its content, to
// detect binary/download responses).
type Response struct {
	Content    map[string]*MediaType `yaml:"content"`
	Extensions map[string]yaml.Node  `yaml:",inline"`
}

// Schema models the subset of JSON Schema used by parameters and bodies.
type Schema struct {
	Type       string               `yaml:"type"`
	Format     string               `yaml:"format"`
	Enum       []any                `yaml:"enum"`
	Default    any                  `yaml:"default"`
	Properties map[string]*Schema   `yaml:"properties"`
	Required   requiredNames        `yaml:"required"`
	Items      *Schema              `yaml:"items"`
	Nullable   bool                 `yaml:"nullable"`
	Extensions map[string]yaml.Node `yaml:",inline"`
}

// requiredNames models an OpenAPI `required` value. At the object-schema level it
// is a list of required property names. Some real connector definitions (e.g.
// Stripe) also emit a boolean `required: true` on individual property schemas —
// technically-off OpenAPI, but common — which must not fail the whole parse.
// Any non-sequence value decodes to an empty list.
type requiredNames []string

func (r *requiredNames) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.SequenceNode {
		*r = nil
		return nil
	}
	var names []string
	if err := node.Decode(&names); err != nil {
		return err
	}
	*r = names
	return nil
}

// Components models the components object; only security schemes are inspected.
type Components struct {
	SecuritySchemes map[string]*SecurityScheme `yaml:"securitySchemes"`
	Extensions      map[string]yaml.Node       `yaml:",inline"`
}

// SecurityScheme models an OpenAPI security scheme plus the x-airbyte-oauth
// refresh flag used to reject refreshable OAuth statically.
type SecurityScheme struct {
	Type         string               `yaml:"type"`
	In           string               `yaml:"in"`
	Name         string               `yaml:"name"`
	Scheme       string               `yaml:"scheme"`
	BearerFormat string               `yaml:"bearerFormat"`
	Flows        *OAuthFlows          `yaml:"flows"`
	OAuthRefresh bool                 `yaml:"x-airbyte-oauth-refresh"`
	AuthConfig   *AuthConfig          `yaml:"x-airbyte-auth-config"`
	Extensions   map[string]yaml.Node `yaml:",inline"`
}

// AuthConfig models the x-airbyte-auth-config extension attached to a security
// scheme. It is the auth contract every real Airbyte connector definition uses
// (the older top-level x-airbyte-auth extension is not emitted by any connector).
//
// AuthMapping maps a connector auth parameter (e.g. "token", "username",
// "api_key") to a bare `${field}` reference. ReplicationAuthKeyMapping maps a
// source-config path to the auth field it supplies (source_path -> auth_key).
// Required and Properties drive direct-only identity mapping when no replication
// mapping is present. Values are resolved from the hydrated source config in
// auth.go; nothing here is a secret.
type AuthConfig struct {
	Required                  requiredNames      `yaml:"required"`
	Properties                map[string]*Schema `yaml:"properties"`
	AuthMapping               map[string]string  `yaml:"auth_mapping"`
	ReplicationAuthKeyMapping map[string]string  `yaml:"replication_auth_key_mapping"`
}

// OAuthFlows models the OAuth2 flows object.
type OAuthFlows struct {
	Implicit          *OAuthFlow `yaml:"implicit"`
	Password          *OAuthFlow `yaml:"password"`
	ClientCredentials *OAuthFlow `yaml:"clientCredentials"`
	AuthorizationCode *OAuthFlow `yaml:"authorizationCode"`
}

// OAuthFlow models a single OAuth2 flow; a non-empty refreshUrl marks the flow
// as refreshable.
type OAuthFlow struct {
	AuthorizationURL string `yaml:"authorizationUrl"`
	TokenURL         string `yaml:"tokenUrl"`
	RefreshURL       string `yaml:"refreshUrl"`
}

// ResolvedOperation is the immutable result of resolving an (entity, action)
// against a definition: the selected server, effective path, HTTP method, and
// operation. It is the value BuildRequest compiles against and the value later
// phases hydrate around.
type ResolvedOperation struct {
	Definition *Definition
	Server     Server
	Method     string
	Path       string
	Operation  *Operation
}

// ParseDefinition parses a connector definition YAML string into the minimal
// typed model, rejecting malformed YAML, unknown x-airbyte-* extensions, and
// refreshable OAuth. It performs no network I/O and no hydration.
func ParseDefinition(src string) (*Definition, error) {
	if len(src) > MaxYAMLBytes {
		return nil, validationError(fmt.Sprintf("definition_yaml exceeds maximum size of %d bytes", MaxYAMLBytes))
	}

	var def Definition
	// NOTE(parity): yaml.v3 provides alias-expansion limits but no explicit
	// nesting-depth guard; the byte bound above is the primary defense against
	// pathological documents for this phase.
	if err := yaml.Unmarshal([]byte(src), &def); err != nil {
		return nil, &Error{ErrType: TypeValidation, Message: "definition_yaml is not valid YAML", Err: err}
	}
	if len(def.Paths) == 0 {
		return nil, validationError("definition_yaml declares no paths")
	}
	if err := def.rejectUnknownExtensions(); err != nil {
		return nil, err
	}
	if err := def.rejectRefreshableOAuth(); err != nil {
		return nil, err
	}
	return &def, nil
}

// rejectUnknownExtensions walks every inline extension map and rejects any
// x-airbyte-* key that is not modeled explicitly or allow-listed.
func (d *Definition) rejectUnknownExtensions() error {
	checks := []map[string]yaml.Node{d.Extensions, d.Components.Extensions}
	for _, s := range d.Servers {
		checks = append(checks, s.Extensions)
		for _, v := range s.Variables {
			checks = append(checks, v.Extensions)
		}
	}
	for _, scheme := range d.Components.SecuritySchemes {
		if scheme != nil {
			checks = append(checks, scheme.Extensions)
		}
	}
	for _, item := range d.Paths {
		if item == nil {
			continue
		}
		checks = append(checks, item.Extensions)
		for _, op := range item.operations() {
			checks = append(checks, op.Extensions)
			for _, p := range op.Parameters {
				if p != nil {
					checks = append(checks, p.Extensions)
				}
			}
			if op.RequestBody != nil {
				checks = append(checks, op.RequestBody.Extensions)
			}
		}
	}
	for _, exts := range checks {
		if err := rejectUnknownInline(exts); err != nil {
			return err
		}
	}
	return nil
}

// rejectUnknownInline returns an unsupported error naming the first x-airbyte-*
// key that is not allow-listed. Non-airbyte inline keys (standard OpenAPI fields
// we do not model) are ignored.
func rejectUnknownInline(exts map[string]yaml.Node) error {
	if len(exts) == 0 {
		return nil
	}
	keys := make([]string, 0, len(exts))
	for k := range exts {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic error selection
	for _, k := range keys {
		if strings.HasPrefix(k, "x-airbyte-") && !allowedInlineExtensions[k] {
			return unsupportedError(fmt.Sprintf("unknown extension %q is not supported by local execution", k))
		}
	}
	return nil
}

// rejectRefreshableOAuth rejects any security scheme that requires token
// refresh. Static OAuth2 access tokens (no refresh URL, no refresh flag) are
// allowed; Phase 3 applies them.
//
// NOTE(parity): the precise Sonar signal for "refreshable" is not reachable
// here. We treat a non-empty refreshUrl on any OAuth2 flow, or an explicit
// x-airbyte-oauth-refresh: true, as refreshable and reject it.
func (d *Definition) rejectRefreshableOAuth() error {
	for _, scheme := range d.Components.SecuritySchemes {
		if scheme == nil {
			continue
		}
		if scheme.OAuthRefresh {
			return unsupportedError("refreshable OAuth is not supported by local execution")
		}
		if scheme.Flows != nil {
			for _, f := range []*OAuthFlow{
				scheme.Flows.Implicit, scheme.Flows.Password,
				scheme.Flows.ClientCredentials, scheme.Flows.AuthorizationCode,
			} {
				if f != nil && f.RefreshURL != "" {
					return unsupportedError("refreshable OAuth is not supported by local execution")
				}
			}
		}
	}
	return nil
}

// operations returns the non-nil operations declared on a path item.
func (p *PathItem) operations() map[string]*Operation {
	out := map[string]*Operation{}
	if p.Get != nil {
		out["GET"] = p.Get
	}
	if p.Post != nil {
		out["POST"] = p.Post
	}
	if p.Put != nil {
		out["PUT"] = p.Put
	}
	if p.Patch != nil {
		out["PATCH"] = p.Patch
	}
	if p.Delete != nil {
		out["DELETE"] = p.Delete
	}
	return out
}

// ResolveOperation maps (entity, action) to exactly one operation and rejects
// statically-detectable unsupported capabilities (unsupported/known-unsupported
// actions and binary/download responses). It is pure: it never calls a secret
// provider or performs I/O, so callers can prove validation precedes hydration.
func (d *Definition) ResolveOperation(entity, action string) (*ResolvedOperation, error) {
	if entity == "" {
		return nil, validationError("entity is required")
	}
	if action == "" {
		return nil, validationError("action is required")
	}
	if knownUnsupportedActions[action] || strings.HasPrefix(action, "context_store") {
		return nil, unsupportedError(fmt.Sprintf("action %q is not supported by local execution", action))
	}
	if !supportedActions[action] {
		return nil, validationError(fmt.Sprintf("action %q is not a recognized connector action", action))
	}

	if len(d.Servers) == 0 {
		return nil, validationError("definition_yaml declares no servers")
	}

	// Find every operation whose entity/action agree with the request. Iterate
	// paths in sorted order so an ambiguity error is deterministic.
	type match struct {
		method string
		path   string
		op     *Operation
	}
	var matches []match
	paths := make([]string, 0, len(d.Paths))
	for p := range d.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		item := d.Paths[p]
		if item == nil {
			continue
		}
		methods := item.operations()
		mkeys := make([]string, 0, len(methods))
		for m := range methods {
			mkeys = append(mkeys, m)
		}
		sort.Strings(mkeys)
		for _, m := range mkeys {
			op := methods[m]
			if op.Entity == entity && op.Action == action {
				matches = append(matches, match{method: m, path: p, op: op})
			}
		}
	}

	if len(matches) == 0 {
		return nil, validationError(fmt.Sprintf("no operation found for entity %q action %q", entity, action))
	}
	if len(matches) > 1 {
		return nil, validationError(fmt.Sprintf("entity %q action %q is ambiguous: %d operations match", entity, action, len(matches)))
	}

	sel := matches[0]
	effectivePath := sel.path
	if sel.op.PathOverride != "" {
		effectivePath = sel.op.PathOverride
	}

	resolved := &ResolvedOperation{
		Definition: d,
		Server:     d.Servers[0],
		Method:     sel.method,
		Path:       effectivePath,
		Operation:  sel.op,
	}

	if err := resolved.rejectBinaryResponse(); err != nil {
		return nil, err
	}
	return resolved, nil
}

// rejectBinaryResponse rejects operations whose success response is a binary /
// non-JSON download. CLI output is JSON-only, so binary responses cannot be
// represented and are unsupported.
func (r *ResolvedOperation) rejectBinaryResponse() error {
	for status, resp := range r.Operation.Responses {
		if resp == nil {
			continue
		}
		if !strings.HasPrefix(status, "2") && status != "default" {
			continue
		}
		for mediaType := range resp.Content {
			if isBinaryMediaType(mediaType) {
				return unsupportedError("binary response bodies are not supported by local execution (JSON output only)")
			}
		}
	}
	return nil
}

// isBinaryMediaType reports whether a response media type denotes a binary
// download rather than a JSON/text payload.
func isBinaryMediaType(mediaType string) bool {
	mt := strings.ToLower(strings.TrimSpace(mediaType))
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	switch {
	case mt == "application/octet-stream":
		return true
	case mt == "application/pdf", mt == "application/zip", mt == "application/gzip":
		return true
	case strings.HasPrefix(mt, "image/"):
		return true
	case strings.HasPrefix(mt, "audio/"):
		return true
	case strings.HasPrefix(mt, "video/"):
		return true
	default:
		return false
	}
}
