package localexec

// executor.go orchestrates the full local-execution pipeline for a single
// prepared bundle:
//
//	static validate (definition + resolve operation + auth spec + response spec)
//	  -> hydrate (source_config + config_values via secrets.Provider)
//	  -> map auth/config (replication_auth_key_mapping)
//	  -> build request (BuildRequest)
//	  -> execute (isolated hardened transport)
//	  -> shape response
//	  -> return standard Result
//
// The ordering is a hard contract: ALL static validation completes before the
// secret provider is ever called. A malformed/unsupported bundle therefore
// fails without resolving a single secret (proven by the zero-call provider
// test).
//
// Every sensitive intermediate — the hydrated config, the auth-bearing request
// plan, the request body, and the response body — is a function-local value in
// Execute. Nothing sensitive is stored on the Executor or in any package global,
// so it becomes unreachable (and GC-eligible) once Execute returns.

import (
	"context"
	"net/http"
	"time"

	"github.com/airbytehq/airbyte-agent-cli/internal/secrets"
)

// Executor runs local connector execution. It holds only injectable seams
// (provider, clock, transport configuration, response limits) — never any
// credential, hydrated config, or per-request state.
type Executor struct {
	provider secrets.Provider
	clock    func() time.Time
	tcfg     transportConfig
	limits   shapeOptions
}

// Option configures an Executor.
type Option func(*Executor)

// NewExecutor constructs an Executor bound to a secrets.Provider. Additional
// seams (clock, transport, HTTP allowance, limits) are supplied via options so
// tests are deterministic and production defaults stay hardened.
//
// Phase 4 wiring: construct with the AWS-backed secrets provider and no other
// options for production, e.g.
//
//	exec := localexec.NewExecutor(awsProvider)
//	result, err := exec.Execute(ctx, envelope.Bundle)
func NewExecutor(provider secrets.Provider, opts ...Option) *Executor {
	e := &Executor{
		provider: provider,
		clock:    time.Now,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// WithClock injects a deterministic clock (controls execution_time_ms).
func WithClock(clk func() time.Time) Option {
	return func(e *Executor) {
		if clk != nil {
			e.clock = clk
		}
	}
}

// WithRoundTripper injects a custom http.RoundTripper (e.g. an httptest server's
// client transport) instead of the hardened default. Production never sets this.
func WithRoundTripper(rt http.RoundTripper) Option {
	return func(e *Executor) { e.tcfg.roundTripper = rt }
}

// WithInsecureHTTP relaxes the HTTPS-only destination rule. TEST-ONLY: it lets
// tests target a plain-HTTP httptest.Server without weakening production
// defaults, which never call this.
func WithInsecureHTTP() Option {
	return func(e *Executor) { e.tcfg.allowHTTP = true }
}

// WithTimeout sets the per-request timeout.
func WithTimeout(d time.Duration) Option {
	return func(e *Executor) { e.tcfg.timeout = d }
}

// WithMaxRetries sets the retry budget applied to idempotent methods only.
func WithMaxRetries(n int) Option {
	return func(e *Executor) { e.tcfg.maxRetries = n }
}

// WithMaxBodyBytes bounds the response body size.
func WithMaxBodyBytes(n int64) Option {
	return func(e *Executor) { e.tcfg.maxBodyBytes = n }
}

// WithRetryBackoff injects the retry backoff schedule (tests use zero).
func WithRetryBackoff(fn func(attempt int) time.Duration) Option {
	return func(e *Executor) { e.tcfg.backoff = fn }
}

// WithResponseLimits bounds the number of returned records and the maximum
// per-field string length before truncation.
func WithResponseLimits(maxRecords, maxFieldStringLen int) Option {
	return func(e *Executor) {
		e.limits.maxRecords = maxRecords
		e.limits.maxFieldStringLen = maxFieldStringLen
	}
}

// staticPlan is the immutable, hydration-independent result of validating a
// bundle. All of it is computed before the provider is consulted.
type staticPlan struct {
	op    *ResolvedOperation
	auth  *authSpec
	resp  *responseSpec
	retry int
}

// validate performs every static check: definition parse, operation resolution,
// auth spec compilation, response spec compilation (including all JSONPaths),
// and retry-config parsing. It performs NO hydration and NO I/O.
func validateBundle(b *ExecuteBundle) (*staticPlan, error) {
	def, err := ParseDefinition(b.DefinitionYAML)
	if err != nil {
		return nil, err
	}
	op, err := def.ResolveOperation(b.Entity, b.Action)
	if err != nil {
		return nil, err
	}
	auth, err := resolveAuthSpec(op)
	if err != nil {
		return nil, err
	}
	resp, err := parseResponseSpec(op)
	if err != nil {
		return nil, err
	}
	retry, err := parseRetry(op)
	if err != nil {
		return nil, err
	}
	return &staticPlan{op: op, auth: auth, resp: resp, retry: retry}, nil
}

// parseRetry reads the x-airbyte-retry extension's max_retries, if present. The
// value is clamped by the transport to a hard maximum and only ever applied to
// idempotent methods.
func parseRetry(op *ResolvedOperation) (int, error) {
	var cfg struct {
		MaxRetries int `yaml:"max_retries"`
	}
	if _, err := decodeExt(op.Operation.Extensions, "x-airbyte-retry", &cfg); err != nil {
		return 0, err
	}
	if cfg.MaxRetries < 0 {
		return 0, nil
	}
	return cfg.MaxRetries, nil
}

// Execute runs the full pipeline for one bundle and returns the logical Result.
//
// Contract: static validation (validateBundle) runs first; only after it
// succeeds is the secrets provider invoked to hydrate the source config and
// config values. This guarantees that an unsupported/malformed bundle never
// triggers a secret resolution.
func (e *Executor) Execute(ctx context.Context, b *ExecuteBundle) (*Result, error) {
	if b == nil {
		return nil, validationError("no execute bundle provided")
	}

	// (1)(2) Static validation — BEFORE any provider call.
	plan, err := validateBundle(b)
	if err != nil {
		return nil, err
	}

	start := e.clock()

	// (3) Hydrate secrets. Both maps may contain secret_coordinate:: values.
	hydratedSource, err := hydrateMap(ctx, e.provider, b.SourceConfig)
	if err != nil {
		return nil, err
	}
	hydratedConfigValues, err := hydrateMap(ctx, e.provider, b.ConfigValues)
	if err != nil {
		return nil, err
	}

	// (4) Map config: merge hydrated config values over the hydrated source
	// config for server-variable / config resolution during request building.
	buildConfig := deepMerge(hydratedSource, hydratedConfigValues)

	requestPlan, err := BuildRequest(plan.op, Inputs{Params: b.Params, Config: buildConfig})
	if err != nil {
		return nil, err
	}

	// (4b) Map auth: rebuild credentials from the hydrated source config via the
	// replication_auth_key_mapping and merge them into a COPY of the plan. The
	// authed plan is the ONLY place credentials live from here on.
	authed, err := applyAuth(plan.auth, requestPlan, hydratedSource, b.ReplicationAuthKeyMapping)
	if err != nil {
		return nil, err
	}

	// (5) Execute against the isolated hardened transport. The definition's
	// requested retry budget governs; an explicit executor override caps it.
	tcfg := e.tcfg
	retries := plan.retry
	if e.tcfg.maxRetries > 0 && e.tcfg.maxRetries < retries {
		retries = e.tcfg.maxRetries
	}
	tcfg.maxRetries = retries
	transport := newTransport(tcfg)
	resp, err := transport.do(ctx, authed)
	if err != nil {
		return nil, err
	}

	// (6) Shape the response into the logical result.
	opts := shapeOptions{
		selectFields:      b.SelectFields,
		excludeFields:     b.ExcludeFields,
		skipTruncation:    b.SkipTruncation,
		maxRecords:        e.limits.maxRecords,
		maxFieldStringLen: e.limits.maxFieldStringLen,
	}
	result, err := shapeResponse(plan.resp, resp, plan.op, opts)
	if err != nil {
		return nil, err
	}
	result.ExecutionTimeMs = int64(e.clock().Sub(start) / time.Millisecond)
	return result, nil
}

// hydrateMap hydrates a decoded-JSON object with the provider and returns a
// concrete map[string]any (empty, never nil, when the input is empty).
func hydrateMap(ctx context.Context, p secrets.Provider, in map[string]any) (map[string]any, error) {
	if len(in) == 0 {
		return map[string]any{}, nil
	}
	out, err := secrets.Hydrate(ctx, p, in)
	if err != nil {
		return nil, err
	}
	m, ok := out.(map[string]any)
	if !ok {
		return map[string]any{}, nil
	}
	return m, nil
}

// deepMerge returns a new map with over's keys layered onto base's keys.
// Nested maps are merged recursively; scalars/arrays in over replace base.
func deepMerge(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		if bm, ok := out[k].(map[string]any); ok {
			if om, ok := v.(map[string]any); ok {
				out[k] = deepMerge(bm, om)
				continue
			}
		}
		out[k] = v
	}
	return out
}
