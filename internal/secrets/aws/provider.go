// Package aws implements the secrets.Provider interface backed by AWS Secrets
// Manager. It builds an aws.Config using a deterministic precedence (explicit
// profile > dedicated env credentials > default SDK chain), calls
// GetSecretValue, enforces strict SecretString handling, and classifies
// failures into the redaction-safe typed errors defined in package secrets.
package aws

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/smithy-go"

	appconfig "github.com/airbytehq/airbyte-agent-cli/internal/config"
	"github.com/airbytehq/airbyte-agent-cli/internal/secrets"
)

// providerName is used in redaction-safe error messages. It names the provider
// but never a coordinate or secret ID.
const providerName = "aws secrets manager"

// secretsAPI is the narrow slice of the Secrets Manager client used by the
// provider. Keeping it minimal makes the provider testable with a fake.
type secretsAPI interface {
	GetSecretValue(ctx context.Context, params *secretsmanager.GetSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

// configLoader loads an aws.Config from a set of load options. It mirrors
// config.LoadDefaultConfig and is injectable for tests.
type configLoader func(ctx context.Context, optFns ...func(*config.LoadOptions) error) (awssdk.Config, error)

// clientFactory builds a Secrets Manager API client from a resolved
// aws.Config. Injectable for tests.
type clientFactory func(cfg awssdk.Config) secretsAPI

// Provider resolves secret coordinates against AWS Secrets Manager.
type Provider struct {
	exec appconfig.ExecutionConfig

	// loadConfig and newClient are injection seams for testing. In production
	// they default to the real AWS SDK constructors.
	loadConfig configLoader
	newClient  clientFactory
}

// Option customises a Provider (used by tests to inject fakes).
type Option func(*Provider)

// WithConfigLoader overrides the aws.Config loader.
func WithConfigLoader(l configLoader) Option {
	return func(p *Provider) { p.loadConfig = l }
}

// WithClientFactory overrides the Secrets Manager client factory.
func WithClientFactory(f clientFactory) Option {
	return func(p *Provider) { p.newClient = f }
}

// New constructs an AWS Secrets Manager provider from resolved execution
// configuration.
func New(exec appconfig.ExecutionConfig, opts ...Option) *Provider {
	p := &Provider{
		exec:       exec,
		loadConfig: config.LoadDefaultConfig,
		newClient: func(cfg awssdk.Config) secretsAPI {
			return secretsmanager.NewFromConfig(cfg)
		},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Resolve fetches a single secret by its coordinate suffix (the AWS Secrets
// Manager secret ID). It returns the scalar SecretString value, or a typed
// *secrets.Error. The secret ID is never included in any returned error.
func (p *Provider) Resolve(ctx context.Context, coordinate string) (string, error) {
	if strings.TrimSpace(coordinate) == "" {
		return "", &secrets.Error{Type: secrets.ErrHydration, Message: "secret coordinate is empty"}
	}

	cfg, err := p.awsConfig(ctx)
	if err != nil {
		return "", err
	}

	client := p.newClient(cfg)

	out, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: awssdk.String(coordinate),
	})
	if err != nil {
		return "", p.classify(err)
	}

	return validateSecret(out)
}

// awsConfig builds the aws.Config applying the precedence documented in the
// plan. Load failures are classified as authentication errors (they generally
// mean credentials/SSO could not be resolved).
func (p *Provider) awsConfig(ctx context.Context) (awssdk.Config, error) {
	var loadOpts []func(*config.LoadOptions) error

	// Region precedence: explicit flag/dedicated-env region is applied here;
	// otherwise the SDK's own region chain is left intact.
	if p.exec.AWSRegion != "" {
		loadOpts = append(loadOpts, config.WithRegion(p.exec.AWSRegion))
	}

	switch {
	case p.exec.AWSProfile != "":
		// An explicitly selected profile is authoritative. Do NOT layer in
		// unrelated static env credentials.
		loadOpts = append(loadOpts, config.WithSharedConfigProfile(p.exec.AWSProfile))
	case p.exec.HasDedicatedCredentials():
		// Dedicated AWS_SECRET_MANAGER_* static credentials.
		loadOpts = append(loadOpts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(
				p.exec.AWSAccessKeyID,
				p.exec.AWSSecretAccessKey,
				p.exec.AWSSessionToken,
			),
		))
	default:
		// Preserve the normal AWS SDK v2 credential chain untouched.
	}

	cfg, err := p.loadConfig(ctx, loadOpts...)
	if err != nil {
		return awssdk.Config{}, p.classify(err)
	}
	return cfg, nil
}

// validateSecret enforces strict SecretString handling: SecretBinary is
// rejected, an absent value is rejected, and a SecretString that decodes to a
// JSON object or array is rejected. Scalar strings are returned exactly.
func validateSecret(out *secretsmanager.GetSecretValueOutput) (string, error) {
	if out == nil {
		return "", &secrets.Error{Type: secrets.ErrHydration, Message: "empty response from " + providerName}
	}
	if out.SecretBinary != nil {
		return "", &secrets.Error{Type: secrets.ErrHydration, Message: "binary secrets are not supported"}
	}
	if out.SecretString == nil {
		return "", &secrets.Error{Type: secrets.ErrHydration, Message: "secret has no string value"}
	}
	value := *out.SecretString
	if isNonScalarJSON(value) {
		return "", &secrets.Error{Type: secrets.ErrHydration, Message: "structured (JSON object/array) secrets are not supported"}
	}
	return value, nil
}

// isNonScalarJSON reports whether s parses as a JSON object or array. A string
// that is not valid JSON, or that is a JSON scalar (string/number/bool/null),
// is treated as a scalar secret value and accepted.
func isNonScalarJSON(s string) bool {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return false
	}
	switch trimmed[0] {
	case '{', '[':
	default:
		return false
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		// Not valid JSON: treat as an opaque scalar string.
		return false
	}
	switch v.(type) {
	case map[string]any, []any:
		return true
	default:
		return false
	}
}

// classify converts an AWS SDK error into a typed, redaction-safe
// *secrets.Error. The AWS error is never formatted wholesale into the message;
// only its category informs classification. The wrapped cause is retained for
// errors.Is/As inspection but callers must not print it.
func (p *Provider) classify(err error) error {
	// SSO cached-token expiry / invalidity.
	var invalidToken *ssocreds.InvalidTokenError
	if errors.As(err, &invalidToken) {
		return p.ssoError(err)
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return p.classifyAPIError(apiErr, err)
	}

	// Non-API errors typically originate from the credential/SSO resolution
	// chain. Detect SSO-shaped failures for the login hint; otherwise report a
	// generic authentication failure (no credentials resolved).
	if looksLikeSSO(err) {
		return p.ssoError(err)
	}
	return &secrets.Error{
		Type:    secrets.ErrAuthentication,
		Message: "could not resolve AWS credentials for " + providerName,
		Hint:    p.loginHint(),
		Err:     err,
	}
}

// classifyAPIError maps a Secrets Manager / STS API error code onto a typed
// error. Only the stable error code drives the decision.
func (p *Provider) classifyAPIError(apiErr smithy.APIError, cause error) error {
	code := apiErr.ErrorCode()

	// Not found: never echo the secret ID.
	var notFound *smtypes.ResourceNotFoundException
	if errors.As(cause, &notFound) || code == "ResourceNotFoundException" {
		return &secrets.Error{Type: secrets.ErrNotFound, Message: "secret not found in " + providerName, Err: cause}
	}

	switch {
	case strings.Contains(code, "AccessDenied"),
		strings.Contains(code, "KMSAccessDenied"),
		strings.Contains(code, "AccessDeniedException"),
		code == "UnauthorizedException",
		code == "AuthorizationError":
		return &secrets.Error{Type: secrets.ErrAccess, Message: "access denied by " + providerName, Err: cause}
	case code == "ExpiredTokenException",
		code == "ExpiredToken",
		code == "InvalidClientTokenId",
		code == "UnrecognizedClientException":
		return &secrets.Error{Type: secrets.ErrAuthentication, Message: "AWS credentials are expired or invalid", Hint: p.loginHint(), Err: cause}
	default:
		return &secrets.Error{Type: secrets.ErrHydration, Message: "request to " + providerName + " failed", Err: cause}
	}
}

// ssoError builds an authentication error for an SSO cache miss/expiry,
// including the explicit remediation command only when a profile was
// explicitly selected. It never launches or retries login.
func (p *Provider) ssoError(cause error) error {
	return &secrets.Error{
		Type:    secrets.ErrAuthentication,
		Message: "AWS SSO session is expired or missing",
		Hint:    p.loginHint(),
		Err:     cause,
	}
}

// loginHint returns the exact `aws sso login --profile <profile>` remediation
// when an explicit profile is selected, else empty. The profile name is
// shell-quoted so it is safe to paste.
func (p *Provider) loginHint() string {
	if p.exec.AWSProfile == "" {
		return ""
	}
	return "run: aws sso login --profile " + shellQuote(p.exec.AWSProfile)
}

// looksLikeSSO reports whether a non-API error appears to originate from the
// SSO token provider. This is a best-effort textual heuristic used only to
// select the authentication category and login hint; no secret material is
// examined.
func looksLikeSSO(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "sso")
}

// shellQuote wraps s in single quotes, escaping embedded single quotes, so the
// value is safe to paste into a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var _ secrets.Provider = (*Provider)(nil)
