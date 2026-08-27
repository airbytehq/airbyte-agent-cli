package config

import (
	"errors"
	"strings"
	"testing"
)

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("AIRBYTE_API_HOST", "")

	cfg := Load()
	if cfg.APIHost != "https://api.airbyte.ai" {
		t.Errorf("APIHost = %q, want %q", cfg.APIHost, "https://api.airbyte.ai")
	}
}

func TestLoad_EnvOverrides(t *testing.T) {
	t.Setenv("AIRBYTE_API_HOST", "https://custom.example.com")

	cfg := Load()
	if cfg.APIHost != "https://custom.example.com" {
		t.Errorf("APIHost = %q, want %q", cfg.APIHost, "https://custom.example.com")
	}
}

// clearExecEnv resets every env var that participates in execution/provider
// resolution so each case starts from a clean slate.
func clearExecEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		envExecutionMode,
		envCompatSwitch,
		envSMAccessKeyID,
		envSMSecretAccessKey,
		envSMSessionToken,
		envSMRegion,
	} {
		t.Setenv(k, "")
	}
}

func TestResolveExecutionConfig_ModePrecedence(t *testing.T) {
	tests := []struct {
		name     string
		modeFlag string
		envMode  string
		compat   string
		want     ExecutionMode
	}{
		{name: "default hosted", want: ModeHosted},
		{name: "flag wins", modeFlag: "local", envMode: "hosted", compat: "true", want: ModeLocal},
		{name: "flag hosted over env local", modeFlag: "hosted", envMode: "local", want: ModeHosted},
		{name: "env when no flag", envMode: "local", want: ModeLocal},
		{name: "compat only when flag and env absent", compat: "true", want: ModeLocal},
		{name: "compat ignored when env set", envMode: "hosted", compat: "true", want: ModeHosted},
		{name: "compat falsey ignored", compat: "false", want: ModeHosted},
		{name: "flag case-insensitive", modeFlag: "LOCAL", want: ModeLocal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearExecEnv(t)
			if tt.envMode != "" {
				t.Setenv(envExecutionMode, tt.envMode)
			}
			if tt.compat != "" {
				t.Setenv(envCompatSwitch, tt.compat)
			}
			cfg, err := ResolveExecutionConfig(tt.modeFlag, "", "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Mode != tt.want {
				t.Errorf("Mode = %q, want %q", cfg.Mode, tt.want)
			}
		})
	}
}

func TestResolveExecutionConfig_TruthyCompatValues(t *testing.T) {
	truthy := []string{"1", "t", "true", "TRUE", "y", "yes", "on", "  true  "}
	for _, v := range truthy {
		t.Run("truthy_"+v, func(t *testing.T) {
			clearExecEnv(t)
			t.Setenv(envCompatSwitch, v)
			cfg, err := ResolveExecutionConfig("", "", "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Mode != ModeLocal {
				t.Errorf("Mode = %q, want local for compat=%q", cfg.Mode, v)
			}
		})
	}
	falsey := []string{"", "0", "false", "no", "off", "banana"}
	for _, v := range falsey {
		t.Run("falsey_"+v, func(t *testing.T) {
			clearExecEnv(t)
			t.Setenv(envCompatSwitch, v)
			cfg, err := ResolveExecutionConfig("", "", "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Mode != ModeHosted {
				t.Errorf("Mode = %q, want hosted for compat=%q", cfg.Mode, v)
			}
		})
	}
}

func TestResolveExecutionConfig_InvalidMode(t *testing.T) {
	clearExecEnv(t)
	_, err := ResolveExecutionConfig("banana", "", "")
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %T, want *ValidationError", err)
	}
	if ve.Type() != "validation_error" {
		t.Errorf("Type = %q, want validation_error", ve.Type())
	}
	if ve.ExitCode() != 4 {
		t.Errorf("ExitCode = %d, want 4", ve.ExitCode())
	}
	// Must not leak the invalid value beyond a bare echo; ensure it does not
	// mention any secret material.
	if strings.Contains(strings.ToLower(err.Error()), "secret") {
		t.Errorf("error message unexpectedly mentions secrets: %q", err.Error())
	}
}

func TestResolveExecutionConfig_InvalidEnvMode(t *testing.T) {
	clearExecEnv(t)
	t.Setenv(envExecutionMode, "sideways")
	_, err := ResolveExecutionConfig("", "", "")
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %T (%v), want *ValidationError", err, err)
	}
}

func TestResolveExecutionConfig_DedicatedCredentials(t *testing.T) {
	tests := []struct {
		name       string
		accessKey  string
		secretKey  string
		session    string
		wantErr    bool
		wantHasCap bool
		wantToken  string
	}{
		{name: "complete pair", accessKey: "AKIA", secretKey: "shh", wantHasCap: true},
		{name: "complete pair with session", accessKey: "AKIA", secretKey: "shh", session: "tok", wantHasCap: true, wantToken: "tok"},
		{name: "none set", wantHasCap: false},
		{name: "access without secret", accessKey: "AKIA", wantErr: true},
		{name: "secret without access", secretKey: "shh", wantErr: true},
		{name: "session without pair", session: "tok", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearExecEnv(t)
			if tt.accessKey != "" {
				t.Setenv(envSMAccessKeyID, tt.accessKey)
			}
			if tt.secretKey != "" {
				t.Setenv(envSMSecretAccessKey, tt.secretKey)
			}
			if tt.session != "" {
				t.Setenv(envSMSessionToken, tt.session)
			}
			cfg, err := ResolveExecutionConfig("", "", "")
			if tt.wantErr {
				var ve *ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("error = %v, want *ValidationError", err)
				}
				if ve.ExitCode() != 4 {
					t.Errorf("ExitCode = %d, want 4", ve.ExitCode())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.HasDedicatedCredentials() != tt.wantHasCap {
				t.Errorf("HasDedicatedCredentials = %v, want %v", cfg.HasDedicatedCredentials(), tt.wantHasCap)
			}
			if cfg.AWSSessionToken != tt.wantToken {
				t.Errorf("AWSSessionToken = %q, want %q", cfg.AWSSessionToken, tt.wantToken)
			}
		})
	}
}

func TestResolveExecutionConfig_ProfileSuppressesDedicatedCreds(t *testing.T) {
	clearExecEnv(t)
	// An explicit profile is authoritative: dedicated static env creds must be
	// ignored (and their validation not applied).
	t.Setenv(envSMAccessKeyID, "AKIA")
	// Note: only access key set (would be an error path without a profile).
	cfg, err := ResolveExecutionConfig("", "myprofile", "")
	if err != nil {
		t.Fatalf("unexpected error with explicit profile: %v", err)
	}
	if cfg.AWSProfile != "myprofile" {
		t.Errorf("AWSProfile = %q, want myprofile", cfg.AWSProfile)
	}
	if cfg.HasDedicatedCredentials() {
		t.Error("dedicated credentials should be ignored when a profile is set")
	}
	if cfg.AWSAccessKeyID != "" {
		t.Errorf("AWSAccessKeyID = %q, want empty", cfg.AWSAccessKeyID)
	}
}

func TestResolveExecutionConfig_RegionPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		regionFlag string
		envRegion  string
		want       string
	}{
		{name: "flag wins", regionFlag: "us-west-2", envRegion: "eu-central-1", want: "us-west-2"},
		{name: "env when no flag", envRegion: "eu-central-1", want: "eu-central-1"},
		{name: "empty defers to sdk", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearExecEnv(t)
			if tt.envRegion != "" {
				t.Setenv(envSMRegion, tt.envRegion)
			}
			cfg, err := ResolveExecutionConfig("", "", tt.regionFlag)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.AWSRegion != tt.want {
				t.Errorf("AWSRegion = %q, want %q", cfg.AWSRegion, tt.want)
			}
		})
	}
}

func TestValidationError_NoSecretLeak(t *testing.T) {
	// The dedicated-credential validation error must name the env var but must
	// NOT include any credential value.
	clearExecEnv(t)
	t.Setenv(envSMAccessKeyID, "AKIA-super-secret-value")
	_, err := ResolveExecutionConfig("", "", "")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "AKIA-super-secret-value") {
		t.Errorf("error message leaked credential value: %q", err.Error())
	}
}
