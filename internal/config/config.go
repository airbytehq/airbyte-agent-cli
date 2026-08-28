package config

import (
	"fmt"
	"os"
	"strings"
)

const defaultAPIHost = "https://api.airbyte.ai"

type Config struct {
	APIHost string
}

func Load() *Config {
	host := os.Getenv("AIRBYTE_API_HOST")
	if host == "" {
		host = defaultAPIHost
	}

	return &Config{
		APIHost: host,
	}
}

// ExecutionMode selects how connector operations are executed. It is a runtime
// control (not a per-operation parameter) so it composes with output flags such
// as --json.
type ExecutionMode string

const (
	// ModeHosted routes connector execution through the Airbyte-hosted API
	// (the default, and the only behaviour exercised before Phase 4).
	ModeHosted ExecutionMode = "hosted"
	// ModeLocal executes connectors locally, resolving secret coordinates via
	// a secrets.Provider.
	ModeLocal ExecutionMode = "local"
)

// Environment variable names that participate in execution/provider resolution.
const (
	envExecutionMode = "AIRBYTE_EXECUTION_MODE"
	// envCompatSwitch is the AWS SDK compatibility switch: when truthy it opts
	// into local mode, but only when neither --execution-mode nor
	// AIRBYTE_EXECUTION_MODE is provided.
	envCompatSwitch = "SECRETS_CONFIGURED_FROM_ENVIRONMENT"

	// Dedicated AWS Secrets Manager credential env vars. These are honoured
	// only when no --aws-profile is set, and the access-key pair is
	// all-or-nothing.
	envSMAccessKeyID     = "AWS_SECRET_MANAGER_ACCESS_KEY_ID"
	envSMSecretAccessKey = "AWS_SECRET_MANAGER_SECRET_ACCESS_KEY"
	envSMSessionToken    = "AWS_SECRET_MANAGER_SESSION_TOKEN"
	envSMRegion          = "AWS_SECRET_MANAGER_REGION"
)

// ExecutionConfig is the resolved runtime configuration for connector
// execution and (in local mode) secret hydration. It is a plain value with no
// behaviour; the AWS provider consumes it to build an aws.Config.
//
// Credential fields are populated only from the dedicated AWS_SECRET_MANAGER_*
// env vars and are never persisted or logged.
type ExecutionConfig struct {
	Mode ExecutionMode

	// AWSProfile is the explicitly selected shared-config profile, or "" to
	// leave the AWS SDK default chain intact. When set it is authoritative.
	AWSProfile string
	// AWSRegion is the resolved region override, or "" to defer to the AWS SDK
	// region chain.
	AWSRegion string

	// Dedicated static credentials from AWS_SECRET_MANAGER_* env vars. Only
	// populated when AWSProfile is empty.
	AWSAccessKeyID     string
	AWSSecretAccessKey string
	AWSSessionToken    string
}

// HasDedicatedCredentials reports whether a complete dedicated static-credential
// pair was supplied.
func (c ExecutionConfig) HasDedicatedCredentials() bool {
	return c.AWSAccessKeyID != "" && c.AWSSecretAccessKey != ""
}

// ResolveExecutionConfig applies the documented precedence and validation to
// produce an ExecutionConfig. The arguments are the raw root-flag values
// (empty string means "flag not set"); env vars and defaults fill the rest.
//
// Precedence:
//   - Mode: flag > AIRBYTE_EXECUTION_MODE > compatibility switch (only when
//     both are absent) > hosted default.
//   - Profile: flag > (unmodified AWS SDK chain, i.e. left empty here).
//   - Region: flag > AWS_SECRET_MANAGER_REGION > (AWS SDK region chain, left
//     empty here).
//
// Returns a *ValidationError (type "validation_error", exit code 4) on an
// invalid mode or an incomplete dedicated-credential set.
func ResolveExecutionConfig(modeFlag, profileFlag, regionFlag string) (ExecutionConfig, error) {
	cfg := ExecutionConfig{}

	mode, err := resolveMode(modeFlag)
	if err != nil {
		return ExecutionConfig{}, err
	}
	cfg.Mode = mode

	// Profile: flag is authoritative; otherwise leave empty so the AWS SDK
	// chain (which reads AWS_PROFILE itself) is untouched.
	cfg.AWSProfile = strings.TrimSpace(profileFlag)

	// Region: flag > dedicated env > (defer to SDK).
	region := strings.TrimSpace(regionFlag)
	if region == "" {
		region = strings.TrimSpace(os.Getenv(envSMRegion))
	}
	cfg.AWSRegion = region

	// Dedicated static credentials are honoured only without an explicit
	// profile. An explicitly selected profile must not silently pick up
	// unrelated static env credentials.
	if cfg.AWSProfile == "" {
		if err := applyDedicatedCredentials(&cfg); err != nil {
			return ExecutionConfig{}, err
		}
	}

	return cfg, nil
}

// resolveMode implements the execution-mode precedence and validation.
func resolveMode(modeFlag string) (ExecutionMode, error) {
	flagVal := strings.TrimSpace(modeFlag)
	envVal := strings.TrimSpace(os.Getenv(envExecutionMode))

	switch {
	case flagVal != "":
		return validateMode(flagVal)
	case envVal != "":
		return validateMode(envVal)
	case isTruthy(os.Getenv(envCompatSwitch)):
		// Compatibility switch opts into local mode only when neither the flag
		// nor the env mode is present.
		return ModeLocal, nil
	default:
		return ModeHosted, nil
	}
}

// validateMode normalizes and validates a mode string.
func validateMode(v string) (ExecutionMode, error) {
	switch ExecutionMode(strings.ToLower(v)) {
	case ModeHosted:
		return ModeHosted, nil
	case ModeLocal:
		return ModeLocal, nil
	default:
		return "", &ValidationError{Message: fmt.Sprintf("invalid execution mode %q (valid: hosted, local)", v)}
	}
}

// applyDedicatedCredentials reads the AWS_SECRET_MANAGER_* credential env vars,
// validating the all-or-nothing access-key pair and that a session token is
// not supplied on its own.
func applyDedicatedCredentials(cfg *ExecutionConfig) error {
	accessKey := strings.TrimSpace(os.Getenv(envSMAccessKeyID))
	secretKey := strings.TrimSpace(os.Getenv(envSMSecretAccessKey))
	sessionToken := strings.TrimSpace(os.Getenv(envSMSessionToken))

	switch {
	case accessKey != "" && secretKey != "":
		cfg.AWSAccessKeyID = accessKey
		cfg.AWSSecretAccessKey = secretKey
		cfg.AWSSessionToken = sessionToken
		return nil
	case accessKey == "" && secretKey == "":
		if sessionToken != "" {
			return &ValidationError{Message: "AWS_SECRET_MANAGER_SESSION_TOKEN is set without AWS_SECRET_MANAGER_ACCESS_KEY_ID and AWS_SECRET_MANAGER_SECRET_ACCESS_KEY"}
		}
		return nil
	default:
		return &ValidationError{Message: "AWS_SECRET_MANAGER_ACCESS_KEY_ID and AWS_SECRET_MANAGER_SECRET_ACCESS_KEY must both be set or both be unset"}
	}
}

// isTruthy reports whether an env value opts a boolean-style switch on. It
// accepts common truthy spellings case-insensitively.
func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	default:
		return false
	}
}

// ValidationError reports invalid runtime configuration. It carries a stable
// type string and exit code aligned with the secrets error contract so Phase 4
// can map it uniformly.
type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

// Type returns the stable error category ("validation_error").
func (e *ValidationError) Type() string { return "validation_error" }

// ExitCode returns the process exit code for a validation error.
func (e *ValidationError) ExitCode() int { return 4 }
