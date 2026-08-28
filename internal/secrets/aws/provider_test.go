package aws

import (
	"context"
	"errors"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/smithy-go"

	appconfig "github.com/airbytehq/airbyte-agent-cli/internal/config"
	"github.com/airbytehq/airbyte-agent-cli/internal/secrets"
)

// fakeSecretsAPI is an injectable Secrets Manager client.
type fakeSecretsAPI struct {
	out      *secretsmanager.GetSecretValueOutput
	err      error
	gotID    string
	callback func(id string)
}

func (f *fakeSecretsAPI) GetSecretValue(ctx context.Context, params *secretsmanager.GetSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	if params.SecretId != nil {
		f.gotID = *params.SecretId
	}
	if f.callback != nil {
		f.callback(f.gotID)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.out, f.err
}

// capturingLoader applies the load options to a LoadOptions struct so tests can
// assert what the provider requested, then returns a fixed config or error.
func capturingLoader(captured *config.LoadOptions, cfg awssdk.Config, err error) configLoader {
	return func(ctx context.Context, optFns ...func(*config.LoadOptions) error) (awssdk.Config, error) {
		for _, fn := range optFns {
			if fn != nil {
				_ = fn(captured)
			}
		}
		return cfg, err
	}
}

func strPtr(s string) *string { return &s }

func newProvider(t *testing.T, exec appconfig.ExecutionConfig, loader configLoader, api secretsAPI) *Provider {
	t.Helper()
	return New(exec,
		WithConfigLoader(loader),
		WithClientFactory(func(awssdk.Config) secretsAPI { return api }),
	)
}

func TestResolve_SharedProfileIsAuthoritative(t *testing.T) {
	var captured config.LoadOptions
	loader := capturingLoader(&captured, awssdk.Config{}, nil)
	api := &fakeSecretsAPI{out: &secretsmanager.GetSecretValueOutput{SecretString: strPtr("val")}}

	exec := appconfig.ExecutionConfig{
		Mode:       appconfig.ModeLocal,
		AWSProfile: "prod",
		// Dedicated creds would be ignored, but they are empty here since the
		// config resolver suppresses them when a profile is set.
	}
	p := newProvider(t, exec, loader, api)

	got, err := p.Resolve(context.Background(), "my-secret-id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "val" {
		t.Errorf("value = %q, want val", got)
	}
	if captured.SharedConfigProfile != "prod" {
		t.Errorf("SharedConfigProfile = %q, want prod", captured.SharedConfigProfile)
	}
	if captured.Credentials != nil {
		t.Error("static credentials must not be set when a profile is authoritative")
	}
	if api.gotID != "my-secret-id" {
		t.Errorf("SecretId = %q, want my-secret-id", api.gotID)
	}
}

func TestResolve_DedicatedStaticCredentials(t *testing.T) {
	var captured config.LoadOptions
	loader := capturingLoader(&captured, awssdk.Config{}, nil)
	api := &fakeSecretsAPI{out: &secretsmanager.GetSecretValueOutput{SecretString: strPtr("v")}}

	exec := appconfig.ExecutionConfig{
		Mode:               appconfig.ModeLocal,
		AWSAccessKeyID:     "AKIA",
		AWSSecretAccessKey: "shh",
		AWSSessionToken:    "tok",
	}
	p := newProvider(t, exec, loader, api)

	if _, err := p.Resolve(context.Background(), "id"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured.Credentials == nil {
		t.Fatal("expected static credentials provider to be set")
	}
	creds, err := captured.Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("retrieving creds: %v", err)
	}
	if creds.AccessKeyID != "AKIA" || creds.SecretAccessKey != "shh" || creds.SessionToken != "tok" {
		t.Errorf("creds = %+v, want AKIA/shh/tok", creds)
	}
	if captured.SharedConfigProfile != "" {
		t.Errorf("SharedConfigProfile = %q, want empty", captured.SharedConfigProfile)
	}
}

func TestResolve_DefaultChainNoOverrides(t *testing.T) {
	var captured config.LoadOptions
	loader := capturingLoader(&captured, awssdk.Config{}, nil)
	api := &fakeSecretsAPI{out: &secretsmanager.GetSecretValueOutput{SecretString: strPtr("v")}}

	p := newProvider(t, appconfig.ExecutionConfig{Mode: appconfig.ModeLocal}, loader, api)
	if _, err := p.Resolve(context.Background(), "id"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured.SharedConfigProfile != "" || captured.Credentials != nil || captured.Region != "" {
		t.Errorf("default chain should be untouched: %+v", captured)
	}
}

func TestResolve_RegionPassedThrough(t *testing.T) {
	var captured config.LoadOptions
	loader := capturingLoader(&captured, awssdk.Config{}, nil)
	api := &fakeSecretsAPI{out: &secretsmanager.GetSecretValueOutput{SecretString: strPtr("v")}}

	exec := appconfig.ExecutionConfig{Mode: appconfig.ModeLocal, AWSRegion: "us-west-2"}
	p := newProvider(t, exec, loader, api)
	if _, err := p.Resolve(context.Background(), "id"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured.Region != "us-west-2" {
		t.Errorf("Region = %q, want us-west-2", captured.Region)
	}
}

func TestResolve_RejectsSecretBinary(t *testing.T) {
	api := &fakeSecretsAPI{out: &secretsmanager.GetSecretValueOutput{SecretBinary: []byte{1, 2, 3}}}
	p := newProvider(t, appconfig.ExecutionConfig{}, capturingLoader(&config.LoadOptions{}, awssdk.Config{}, nil), api)
	_, err := p.Resolve(context.Background(), "id")
	_ = assertType(t, err, secrets.ErrHydration)
}

func TestResolve_RejectsNonScalarJSON(t *testing.T) {
	cases := map[string]string{
		"object": `{"user":"a","pass":"b"}`,
		"array":  `["a","b"]`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			api := &fakeSecretsAPI{out: &secretsmanager.GetSecretValueOutput{SecretString: strPtr(payload)}}
			p := newProvider(t, appconfig.ExecutionConfig{}, capturingLoader(&config.LoadOptions{}, awssdk.Config{}, nil), api)
			_, err := p.Resolve(context.Background(), "id")
			_ = assertType(t, err, secrets.ErrHydration)
		})
	}
}

func TestResolve_AcceptsScalarStrings(t *testing.T) {
	cases := map[string]string{
		"plain":                     "plain-secret",
		"json string":               `"quoted"`,
		"json number":               `42`,
		"json bool":                 `true`,
		"looks-jsonish-but-invalid": `{not json`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			api := &fakeSecretsAPI{out: &secretsmanager.GetSecretValueOutput{SecretString: strPtr(payload)}}
			p := newProvider(t, appconfig.ExecutionConfig{}, capturingLoader(&config.LoadOptions{}, awssdk.Config{}, nil), api)
			got, err := p.Resolve(context.Background(), "id")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != payload {
				t.Errorf("value = %q, want exact %q", got, payload)
			}
		})
	}
}

func TestResolve_NotFound(t *testing.T) {
	api := &fakeSecretsAPI{err: &smtypes.ResourceNotFoundException{Message: strPtr("Secrets Manager can't find the-secret-id")}}
	p := newProvider(t, appconfig.ExecutionConfig{}, capturingLoader(&config.LoadOptions{}, awssdk.Config{}, nil), api)
	_, err := p.Resolve(context.Background(), "the-secret-id")
	e := assertType(t, err, secrets.ErrNotFound)
	if strings.Contains(e.Error(), "the-secret-id") {
		t.Errorf("not-found error leaked secret id: %q", e.Error())
	}
}

// apiErrorFull is a minimal smithy.APIError implementation for tests.
type apiErrorFull struct {
	code string
	msg  string
}

func (e *apiErrorFull) Error() string                 { return e.code + ": " + e.msg }
func (e *apiErrorFull) ErrorCode() string             { return e.code }
func (e *apiErrorFull) ErrorMessage() string          { return e.msg }
func (e *apiErrorFull) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

func TestResolve_AccessDenied(t *testing.T) {
	api := &fakeSecretsAPI{err: &apiErrorFull{code: "AccessDeniedException", msg: "not authorized to perform secretsmanager:GetSecretValue on resource abc"}}
	p := newProvider(t, appconfig.ExecutionConfig{}, capturingLoader(&config.LoadOptions{}, awssdk.Config{}, nil), api)
	_, err := p.Resolve(context.Background(), "abc")
	e := assertType(t, err, secrets.ErrAccess)
	if strings.Contains(e.Error(), "abc") {
		t.Errorf("access error leaked secret id: %q", e.Error())
	}
}

func TestResolve_ExpiredCredentials(t *testing.T) {
	api := &fakeSecretsAPI{err: &apiErrorFull{code: "ExpiredTokenException", msg: "token expired"}}
	p := newProvider(t, appconfig.ExecutionConfig{AWSProfile: "prod"}, capturingLoader(&config.LoadOptions{}, awssdk.Config{}, nil), api)
	_, err := p.Resolve(context.Background(), "id")
	e := assertType(t, err, secrets.ErrAuthentication)
	if !strings.Contains(e.Hint, "aws sso login --profile 'prod'") {
		t.Errorf("expected login hint for explicit profile, got %q", e.Hint)
	}
}

func TestResolve_SSOCacheMissWithExplicitProfile(t *testing.T) {
	// Config load fails with an SSO invalid-token error.
	ssoErr := &ssocreds.InvalidTokenError{Err: errors.New("cached SSO token expired")}
	loader := func(ctx context.Context, optFns ...func(*config.LoadOptions) error) (awssdk.Config, error) {
		return awssdk.Config{}, ssoErr
	}
	p := New(appconfig.ExecutionConfig{AWSProfile: "my prof"},
		WithConfigLoader(loader),
		WithClientFactory(func(awssdk.Config) secretsAPI { return &fakeSecretsAPI{} }),
	)
	_, err := p.Resolve(context.Background(), "id")
	e := assertType(t, err, secrets.ErrAuthentication)
	// Shell-quoted profile with an embedded space.
	if !strings.Contains(e.Hint, "aws sso login --profile 'my prof'") {
		t.Errorf("expected shell-quoted login hint, got %q", e.Hint)
	}
}

func TestResolve_SSOCacheMissWithoutProfileNoHint(t *testing.T) {
	loader := func(ctx context.Context, optFns ...func(*config.LoadOptions) error) (awssdk.Config, error) {
		return awssdk.Config{}, &ssocreds.InvalidTokenError{Err: errors.New("expired")}
	}
	p := New(appconfig.ExecutionConfig{},
		WithConfigLoader(loader),
		WithClientFactory(func(awssdk.Config) secretsAPI { return &fakeSecretsAPI{} }),
	)
	_, err := p.Resolve(context.Background(), "id")
	e := assertType(t, err, secrets.ErrAuthentication)
	if e.Hint != "" {
		t.Errorf("no login hint expected without explicit profile, got %q", e.Hint)
	}
}

func TestResolve_NoCredentialsIsAuthError(t *testing.T) {
	loader := func(ctx context.Context, optFns ...func(*config.LoadOptions) error) (awssdk.Config, error) {
		return awssdk.Config{}, errors.New("failed to refresh cached credentials, no EC2 IMDS role found")
	}
	p := New(appconfig.ExecutionConfig{},
		WithConfigLoader(loader),
		WithClientFactory(func(awssdk.Config) secretsAPI { return &fakeSecretsAPI{} }),
	)
	_, err := p.Resolve(context.Background(), "id")
	_ = assertType(t, err, secrets.ErrAuthentication)
}

func TestResolve_Cancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	api := &fakeSecretsAPI{out: &secretsmanager.GetSecretValueOutput{SecretString: strPtr("v")}}
	p := newProvider(t, appconfig.ExecutionConfig{}, capturingLoader(&config.LoadOptions{}, awssdk.Config{}, nil), api)
	_, err := p.Resolve(ctx, "id")
	if err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestResolve_EmptyCoordinate(t *testing.T) {
	api := &fakeSecretsAPI{}
	p := newProvider(t, appconfig.ExecutionConfig{}, capturingLoader(&config.LoadOptions{}, awssdk.Config{}, nil), api)
	_, err := p.Resolve(context.Background(), "   ")
	_ = assertType(t, err, secrets.ErrHydration)
}

func TestResolve_RedactionNoSecretIDInMessages(t *testing.T) {
	secretID := "prod/db/master-password"
	api := &fakeSecretsAPI{err: &apiErrorFull{code: "SomethingWeird", msg: "boom involving " + secretID}}
	p := newProvider(t, appconfig.ExecutionConfig{}, capturingLoader(&config.LoadOptions{}, awssdk.Config{}, nil), api)
	_, err := p.Resolve(context.Background(), secretID)
	e, ok := secrets.AsError(err)
	if !ok {
		t.Fatalf("error = %T, want *secrets.Error", err)
	}
	if strings.Contains(e.Message, secretID) {
		t.Errorf("error message leaked secret id: %q", e.Message)
	}
}

// assertType asserts err is a *secrets.Error with the given type and returns it.
func assertType(t *testing.T, err error, want secrets.ErrorType) *secrets.Error {
	t.Helper()
	e, ok := secrets.AsError(err)
	if !ok {
		t.Fatalf("error = %v (%T), want *secrets.Error", err, err)
	}
	if e.Type != want {
		t.Fatalf("Type = %q, want %q", e.Type, want)
	}
	return e
}
