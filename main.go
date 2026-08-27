package main

import (
	"os"
	"strings"
	"time"

	"github.com/airbytehq/airbyte-agent-cli/cmd"
	"github.com/airbytehq/airbyte-agent-cli/internal/auth"
	"github.com/airbytehq/airbyte-agent-cli/internal/client"
	"github.com/airbytehq/airbyte-agent-cli/internal/config"
	"github.com/airbytehq/airbyte-agent-cli/internal/registry"
	"github.com/airbytehq/airbyte-agent-cli/internal/resources"
	"github.com/airbytehq/airbyte-agent-cli/internal/telemetry"
	"github.com/airbytehq/airbyte-agent-cli/internal/versioncheck"
)

func main() {
	cfg := config.Load()

	settings, _ := auth.ResolveSettings()

	var c *client.Client
	var t *telemetry.Tracker
	if settings != nil {
		creds := settings.Credentials
		tm := auth.NewTokenManager(cfg.APIHost, settings.OrganizationID, &creds)
		c = client.New(cfg.APIHost, settings.OrganizationID, cmd.Version, tm,
			client.WithDebugFunc(cmd.GetVerbose),
			client.WithDefaultWorkspace(settings.Workspace),
			client.WithAllowDestructive(settings.AllowDestructive),
			client.WithExecutionConfigFunc(resolveExecutionConfig),
		)
		t = telemetry.New(
			telemetry.ResolveMode(settings.TelemetryEnabled),
			settings.OrganizationID,
			cmd.Version,
			settings.IsInternalUser,
		)
	}

	// Version check runs even when settings are missing (e.g. before
	// `login`) so a stale CLI still gets nudged on its very first
	// command. Run is bounded internally by a 1s network timeout on the
	// first-run sync fetch.
	vcEnabled := versionCheckEnabled(settings)
	vcIsTTY := versioncheck.IsTerminal(os.Stderr)
	vcDone := versioncheck.Run(
		cmd.Version,
		vcEnabled,
		vcIsTTY,
		os.Stderr,
	)
	versioncheck.CheckSkill(
		cmd.ExpectedSkillVersion,
		vcEnabled,
		vcIsTTY,
		os.Stderr,
	)

	registry.SetTracker(t)
	resources.RegisterAll()
	registry.Build(cmd.GetRootCmd(), c, cmd.FlagAccessor())

	err := cmd.Execute()
	t.Flush()

	if vcDone != nil {
		select {
		case <-vcDone:
		case <-time.After(500 * time.Millisecond):
		}
	}

	if err != nil {
		os.Exit(1)
	}
}

// resolveExecutionConfig is the deferred resolver wired into the API client.
// It reads the root flag getters lazily (after Cobra has parsed argv) and
// applies env precedence + validation via config.ResolveExecutionConfig.
//
// A resolution error (e.g. an invalid --execution-mode) is propagated
// unchanged, NOT swallowed into a hosted fallback: connector execution maps it
// to a typed validation_error (exit 4) so invalid runtime configuration never
// silently runs hosted.
func resolveExecutionConfig() (config.ExecutionConfig, error) {
	return config.ResolveExecutionConfig(
		cmd.GetExecutionMode(),
		cmd.GetAWSProfile(),
		cmd.GetAWSRegion(),
	)
}

// versionCheckEnabled resolves whether the version-check nudge should
// run on this invocation. When settings exist, the file's value (with
// env override already applied) wins. When settings are missing, fall
// back to the env override directly so CI can suppress the nudge before
// `login` has ever run.
func versionCheckEnabled(s *auth.Settings) bool {
	if s != nil {
		return s.VersionCheckEnabled
	}
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("AIRBYTE_VERSION_CHECK")), "disabled")
}
