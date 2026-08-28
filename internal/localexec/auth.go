package localexec

// auth.go rebuilds authentication material from the hydrated source
// configuration and merges it into a COPY of a compiled request plan. It never
// mutates the plan produced by BuildRequest and never retains credentials in
// any package-level or long-lived state: the resolved value flows straight into
// the returned plan copy that the transport consumes.
//
// Auth values are located through two indirections:
//
//  1. Each security scheme carries an x-airbyte-auth extension naming a logical
//     config key (a dotted path such as "credentials.api_key").
//  2. The bundle's replication_auth_key_mapping optionally rewrites that logical
//     key to the concrete dotted path in the hydrated source_config. Missing
//     mapping entries fall back to an identity mapping (the logical key is the
//     source path).
//
// Every error is redaction-safe: it may name a scheme or a definition-authored
// config key, but never the resolved secret value.

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Supported security scheme categories.
const (
	schemeAPIKey = "apiKey"
	schemeHTTP   = "http"
	schemeOAuth2 = "oauth2"
)

// authExt models the x-airbyte-auth extension attached to a security scheme.
// It names the source-config keys that supply the credential material for a
// scheme; the concrete values are resolved later from the hydrated source
// config (optionally remapped through replication_auth_key_mapping).
type authExt struct {
	ConfigKey   string `yaml:"config_key"`
	UsernameKey string `yaml:"username_key"`
	PasswordKey string `yaml:"password_key"`
	// HeaderName optionally overrides the OpenAPI scheme name for the header,
	// query, or cookie an apiKey credential is placed in.
	HeaderName string `yaml:"header"`
	// ValuePrefix is prepended to an apiKey value (e.g. "Token ").
	ValuePrefix string `yaml:"value_prefix"`
}

// authScheme is a single compiled security scheme within an alternative.
type authScheme struct {
	name       string
	typ        string // apiKey, http, oauth2
	in         string // header/query/cookie (apiKey only)
	httpScheme string // basic/bearer (http only)
	keyName    string // header/query/cookie name (apiKey only)
	ext        authExt
	// mapping overrides the bundle-level replication_auth_key_mapping for schemes
	// compiled from x-airbyte-auth-config (whose per-scheme mapping is authored on
	// the scheme itself, keyed auth_key -> source_path). Nil for legacy
	// x-airbyte-auth schemes, which use the bundle-level mapping.
	mapping map[string]any
}

// authSpec is the compiled, hydration-independent authentication requirement of
// a resolved operation. Each alternative is a set of schemes that must all be
// satisfiable; the first fully satisfiable alternative wins.
type authSpec struct {
	alternatives [][]authScheme
}

// resolveAuthSpec compiles the effective security requirement of a resolved
// operation into an authSpec. It is pure: it performs no hydration and no I/O,
// so it runs during static validation before any secret provider is consulted.
// It rejects undefined schemes, unsupported scheme categories, and (defense in
// depth) refreshable OAuth.
func resolveAuthSpec(op *ResolvedOperation) (*authSpec, error) {
	if op == nil || op.Operation == nil {
		return nil, validationError("no resolved operation for authentication")
	}
	requirements := op.Operation.Security
	if requirements == nil {
		requirements = op.Definition.Security
	}
	spec := &authSpec{}
	for _, req := range requirements {
		if len(req) == 0 {
			// An empty requirement object means "no auth"; represent it as an
			// always-satisfiable alternative with no schemes.
			spec.alternatives = append(spec.alternatives, nil)
			continue
		}
		names := make([]string, 0, len(req))
		for name := range req {
			names = append(names, name)
		}
		sort.Strings(names) // deterministic scheme ordering
		var schemes []authScheme
		for _, name := range names {
			def := op.Definition.Components.SecuritySchemes[name]
			if def == nil {
				return nil, validationError(fmt.Sprintf("security scheme %q is not defined in components", name))
			}
			if def.OAuthRefresh {
				return nil, unsupportedError("refreshable OAuth is not supported by local execution")
			}
			if def.Flows != nil {
				for _, f := range []*OAuthFlow{def.Flows.Implicit, def.Flows.Password, def.Flows.ClientCredentials, def.Flows.AuthorizationCode} {
					if f != nil && f.RefreshURL != "" {
						return nil, unsupportedError("refreshable OAuth is not supported by local execution")
					}
				}
			}
			s, err := compileScheme(name, def)
			if err != nil {
				return nil, err
			}
			schemes = append(schemes, s)
		}
		spec.alternatives = append(spec.alternatives, schemes)
	}
	return spec, nil
}

// compileScheme validates and compiles a single security scheme definition.
func compileScheme(name string, def *SecurityScheme) (authScheme, error) {
	s := authScheme{name: name, typ: def.Type}
	if def.AuthConfig != nil {
		// Real connectors carry x-airbyte-auth-config; derive the credential
		// config key(s) and per-scheme mapping from it.
		deriveAuthConfigScheme(&s, def)
	} else if _, err := decodeExt(def.Extensions, "x-airbyte-auth", &s.ext); err != nil {
		return authScheme{}, err
	}
	switch def.Type {
	case schemeAPIKey:
		switch def.In {
		case "header", "query", "cookie":
		default:
			return authScheme{}, unsupportedError(fmt.Sprintf("apiKey scheme %q has unsupported location %q", name, def.In))
		}
		s.in = def.In
		s.keyName = s.ext.HeaderName
		if s.keyName == "" {
			s.keyName = def.Name
		}
		if s.keyName == "" {
			return authScheme{}, validationError(fmt.Sprintf("apiKey scheme %q is missing a name", name))
		}
	case schemeHTTP:
		switch def.Scheme {
		case "basic", "bearer":
			s.httpScheme = def.Scheme
		default:
			return authScheme{}, unsupportedError(fmt.Sprintf("http auth scheme %q (%q) is not supported by local execution", name, def.Scheme))
		}
	case schemeOAuth2:
		// Only static (non-refreshing) access tokens reach here; refreshable
		// flows are rejected above.
	default:
		return authScheme{}, unsupportedError(fmt.Sprintf("security scheme type %q is not supported by local execution", def.Type))
	}
	return s, nil
}

// deriveAuthConfigScheme populates the credential config key(s) and per-scheme
// mapping on s from an x-airbyte-auth-config, so the shared applyScheme placement
// logic resolves the credential from the hydrated source config. The credential
// field is the bare ${field} referenced by the scheme's canonical auth_mapping
// entry (token / username+password / api_key / access_token); with no auth_mapping
// entry it falls back to identity on that canonical name (direct-only schemes).
func deriveAuthConfigScheme(s *authScheme, def *SecurityScheme) {
	ac := def.AuthConfig
	switch def.Type {
	case schemeAPIKey:
		s.ext.ConfigKey = authConfigCredKey(ac, "api_key")
	case schemeHTTP:
		if def.Scheme == "basic" {
			s.ext.UsernameKey = authConfigCredKey(ac, "username")
			s.ext.PasswordKey = authConfigCredKey(ac, "password")
		} else {
			s.ext.ConfigKey = authConfigCredKey(ac, "token")
		}
	case schemeOAuth2:
		s.ext.ConfigKey = authConfigCredKey(ac, "access_token")
	}
	s.mapping = invertAuthMapping(ac.ReplicationAuthKeyMapping)
}

// authConfigCredKey returns the source-config field that supplies a canonical
// auth parameter. When auth_mapping names the parameter it must be a bare
// ${field} reference (matching the backend, which supports only bare references);
// a non-bare template yields "" (a soft miss). With no auth_mapping entry the
// canonical name is used directly (direct-only identity mapping).
func authConfigCredKey(ac *AuthConfig, canonical string) string {
	if tmpl, ok := ac.AuthMapping[canonical]; ok {
		if v, ok := bareVar(tmpl); ok {
			return v
		}
		return ""
	}
	return canonical
}

// bareVar returns the field name inside a bare ${field} reference. Constants and
// concatenations (e.g. "Bearer ${x}") are not bare references and return false.
func bareVar(t string) (string, bool) {
	if len(t) > 3 && strings.HasPrefix(t, "${") && strings.HasSuffix(t, "}") {
		inner := t[2 : len(t)-1]
		if inner != "" && !strings.ContainsAny(inner, "${}") {
			return inner, true
		}
	}
	return "", false
}

// invertAuthMapping inverts an x-airbyte-auth-config replication_auth_key_mapping
// (source_path -> auth_key) into the auth_key -> source_path form authValue's
// mapping indirection expects. Returns nil for an empty mapping so identity
// resolution applies.
func invertAuthMapping(repl map[string]string) map[string]any {
	if len(repl) == 0 {
		return nil
	}
	inv := make(map[string]any, len(repl))
	for sourcePath, authKey := range repl {
		if authKey != "" {
			inv[authKey] = sourcePath
		}
	}
	return inv
}

// applyAuth returns a COPY of plan with the first fully satisfiable security
// alternative applied. The original plan is never mutated. When no alternative
// is satisfiable it returns a redacted connector-agnostic error naming only the
// scheme(s), never any resolved value.
func applyAuth(spec *authSpec, plan *RequestPlan, sourceConfig, mapping map[string]any) (*RequestPlan, error) {
	if spec == nil || len(spec.alternatives) == 0 {
		return copyPlan(plan), nil
	}
	var firstErr error
	for _, schemes := range spec.alternatives {
		cp := copyPlan(plan)
		ok, err := applyAlternative(schemes, cp, sourceConfig, mapping)
		if err != nil {
			// A hard error (non-scalar value) aborts immediately; it is not a
			// "try the next alternative" condition.
			return nil, err
		}
		if ok {
			return cp, nil
		}
		if firstErr == nil && len(schemes) > 0 {
			firstErr = unsupportedMissingAuth(schemes)
		}
	}
	if firstErr == nil {
		firstErr = validationError("no satisfiable authentication scheme for this operation")
	}
	return nil, firstErr
}

// unsupportedMissingAuth builds a redacted error naming the schemes of an
// alternative whose credential material was absent from the source config.
func unsupportedMissingAuth(schemes []authScheme) error {
	names := make([]string, 0, len(schemes))
	for _, s := range schemes {
		names = append(names, s.name)
	}
	sort.Strings(names)
	return validationError(fmt.Sprintf("authentication material for scheme(s) %v is missing from the source configuration", names))
}

// applyAlternative attempts to apply every scheme in one alternative to cp. It
// returns (true, nil) when all schemes resolved and were applied, (false, nil)
// when a required credential was missing (caller tries the next alternative),
// or a hard error for a structurally invalid value.
func applyAlternative(schemes []authScheme, cp *RequestPlan, sourceConfig, mapping map[string]any) (bool, error) {
	for _, s := range schemes {
		applied, err := applyScheme(s, cp, sourceConfig, mapping)
		if err != nil {
			return false, err
		}
		if !applied {
			return false, nil
		}
	}
	return true, nil
}

// applyScheme resolves and applies a single scheme's credential to cp.
func applyScheme(s authScheme, cp *RequestPlan, sourceConfig, mapping map[string]any) (bool, error) {
	// x-airbyte-auth-config schemes carry their own auth_key -> source_path
	// mapping; legacy x-airbyte-auth schemes use the bundle-level mapping.
	if s.mapping != nil {
		mapping = s.mapping
	}
	switch s.typ {
	case schemeAPIKey:
		val, ok, err := authValue(s.name, s.ext.ConfigKey, sourceConfig, mapping)
		if err != nil || !ok {
			return false, err
		}
		return true, placeAPIKey(s, cp, s.ext.ValuePrefix+val)
	case schemeHTTP:
		if s.httpScheme == "bearer" {
			val, ok, err := authValue(s.name, s.ext.ConfigKey, sourceConfig, mapping)
			if err != nil || !ok {
				return false, err
			}
			cp.Headers = append(cp.Headers, Header{Name: "Authorization", Value: "Bearer " + val})
			return true, nil
		}
		// basic
		user, uok, err := authValue(s.name, s.ext.UsernameKey, sourceConfig, mapping)
		if err != nil {
			return false, err
		}
		pass, pok, err := authValue(s.name, s.ext.PasswordKey, sourceConfig, mapping)
		if err != nil {
			return false, err
		}
		if !uok || !pok {
			return false, nil
		}
		token := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		cp.Headers = append(cp.Headers, Header{Name: "Authorization", Value: "Basic " + token})
		return true, nil
	case schemeOAuth2:
		val, ok, err := authValue(s.name, s.ext.ConfigKey, sourceConfig, mapping)
		if err != nil || !ok {
			return false, err
		}
		cp.Headers = append(cp.Headers, Header{Name: "Authorization", Value: "Bearer " + val})
		return true, nil
	default:
		return false, unsupportedError(fmt.Sprintf("security scheme type %q is not supported by local execution", s.typ))
	}
}

// placeAPIKey inserts an apiKey credential into the plan copy at its declared
// location (header, query, or cookie).
func placeAPIKey(s authScheme, cp *RequestPlan, value string) error {
	switch s.in {
	case "header":
		cp.Headers = append(cp.Headers, Header{Name: s.keyName, Value: value})
	case "cookie":
		cp.Cookies = append(cp.Cookies, Cookie{Name: s.keyName, Value: value})
	case "query":
		next, err := withQueryParam(cp.URL, s.keyName, value)
		if err != nil {
			return err
		}
		cp.URL = next
	default:
		return unsupportedError(fmt.Sprintf("apiKey scheme %q has unsupported location %q", s.name, s.in))
	}
	return nil
}

// authValue resolves the credential value for a logical config key against the
// hydrated source config, applying the replication_auth_key_mapping. It returns
// (value, true, nil) on success, ("", false, nil) when the field is absent or
// empty (a soft miss the caller can try another alternative for), or an error
// when the located value is not a usable scalar. The returned value is never
// echoed in any error.
func authValue(scheme, logicalKey string, sourceConfig, mapping map[string]any) (string, bool, error) {
	if logicalKey == "" {
		return "", false, nil
	}
	path := mapAuthKey(mapping, logicalKey)
	v, ok := lookupConfigPath(sourceConfig, path)
	if !ok || v == nil {
		return "", false, nil
	}
	s, ok := scalarString(v)
	if !ok {
		return "", false, validationError(fmt.Sprintf("authentication value for scheme %q is not a scalar", scheme))
	}
	if s == "" {
		return "", false, nil
	}
	return s, true, nil
}

// mapAuthKey translates a logical auth key to a source-config path using the
// replication_auth_key_mapping. It supports:
//   - a direct/flat entry keyed by the literal (possibly dotted) logical key,
//   - a nested entry reached by splitting the logical key on '.',
//   - identity (no entry) — the logical key is itself the source path.
func mapAuthKey(mapping map[string]any, logicalKey string) string {
	if len(mapping) == 0 {
		return logicalKey
	}
	if v, ok := mapping[logicalKey]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	if p, ok := nestedStringLookup(mapping, logicalKey); ok {
		return p
	}
	return logicalKey
}

// nestedStringLookup walks mapping by the dotted segments of key and returns the
// terminal string leaf, if present.
func nestedStringLookup(mapping map[string]any, key string) (string, bool) {
	v, ok := lookupConfigPath(mapping, key)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// withQueryParam returns rawURL with key=value set in its query string. The
// query is re-encoded (and therefore key-sorted) deterministically.
func withQueryParam(rawURL, key, value string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", validationError("request URL is not parseable for query auth injection")
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// copyPlan returns a shallow copy of a request plan with independent Headers and
// Cookies slices so auth merging never mutates the caller's plan. The immutable
// Body byte slice is shared (it is never written to).
func copyPlan(p *RequestPlan) *RequestPlan {
	cp := *p
	cp.Headers = append([]Header(nil), p.Headers...)
	cp.Cookies = append([]Cookie(nil), p.Cookies...)
	return &cp
}

// decodeExt decodes an x-airbyte-* inline extension node into out. It returns
// (false, nil) when the key is absent. A malformed node is a validation error
// naming only the key.
func decodeExt(exts map[string]yaml.Node, key string, out any) (bool, error) {
	node, ok := exts[key]
	if !ok {
		return false, nil
	}
	if err := node.Decode(out); err != nil {
		return false, validationError(fmt.Sprintf("extension %q is malformed", key))
	}
	return true, nil
}
