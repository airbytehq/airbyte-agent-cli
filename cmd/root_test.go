package cmd

import "testing"

// resetRootFlags restores every persistent root-flag package global to its
// zero value. Cobra flag state is package-global, so cases must reset it to
// stay independent.
func resetRootFlags() {
	output = ""
	verbose = false
	fields = nil
	executionMode = ""
	awsProfile = ""
	awsRegion = ""
}

func TestRootFlags_Registered(t *testing.T) {
	for _, name := range []string{"execution-mode", "aws-profile", "aws-region"} {
		if f := rootCmd.PersistentFlags().Lookup(name); f == nil {
			t.Errorf("persistent flag %q not registered", name)
		}
	}
}

func TestRootFlags_NoSecretBearingFlags(t *testing.T) {
	// Guard against ever adding credential-bearing input surface.
	forbidden := []string{
		"aws-access-key-id",
		"aws-secret-access-key",
		"aws-session-token",
		"secret-value",
	}
	for _, name := range forbidden {
		if f := rootCmd.PersistentFlags().Lookup(name); f != nil {
			t.Errorf("forbidden secret-bearing flag %q is registered", name)
		}
	}
}

func TestRootFlags_Defaults(t *testing.T) {
	resetRootFlags()
	if got := GetExecutionMode(); got != "" {
		t.Errorf("GetExecutionMode default = %q, want empty", got)
	}
	if got := GetAWSProfile(); got != "" {
		t.Errorf("GetAWSProfile default = %q, want empty", got)
	}
	if got := GetAWSRegion(); got != "" {
		t.Errorf("GetAWSRegion default = %q, want empty", got)
	}
}

func TestRootFlags_Accessors(t *testing.T) {
	resetRootFlags()
	defer resetRootFlags()

	executionMode = "local"
	awsProfile = "prod"
	awsRegion = "us-east-1"

	if got := GetExecutionMode(); got != "local" {
		t.Errorf("GetExecutionMode = %q, want local", got)
	}
	if got := GetAWSProfile(); got != "prod" {
		t.Errorf("GetAWSProfile = %q, want prod", got)
	}
	if got := GetAWSRegion(); got != "us-east-1" {
		t.Errorf("GetAWSRegion = %q, want us-east-1", got)
	}
}

func TestRootFlags_ParseSetsGlobals(t *testing.T) {
	resetRootFlags()
	defer resetRootFlags()

	if err := rootCmd.PersistentFlags().Parse([]string{
		"--execution-mode", "local",
		"--aws-profile", "myprofile",
		"--aws-region", "eu-west-1",
	}); err != nil {
		t.Fatalf("parse error: %v", err)
	}

	if GetExecutionMode() != "local" {
		t.Errorf("GetExecutionMode = %q, want local", GetExecutionMode())
	}
	if GetAWSProfile() != "myprofile" {
		t.Errorf("GetAWSProfile = %q, want myprofile", GetAWSProfile())
	}
	if GetAWSRegion() != "eu-west-1" {
		t.Errorf("GetAWSRegion = %q, want eu-west-1", GetAWSRegion())
	}
}
