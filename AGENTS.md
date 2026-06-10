# AGENTS.md

## Project Overview

`airbyte-agent` is a Go CLI for interacting with the Airbyte API. It uses a registry-based architecture where resources and operations are defined as Go structs and dynamically converted into Cobra commands at startup.

> [!IMPORTANT]
> **Registry Architecture**: Commands are defined as `Resource` + `Operation` structs in `internal/resources/`, NOT as raw Cobra commands in `cmd/`. When adding a new command, implement the `Resource` interface and register it in `register.go`. Do NOT add `cobra.Command` definitions directly.

> [!IMPORTANT]
> **Skills**: A single agent skill lives at `skills/airbyte-agent/` (top-level `skills/` directory). The umbrella `SKILL.md` carries cross-command rules + a routing table; per-command playbooks live under `skills/airbyte-agent/references/<command>.md` and are loaded on demand per the [Agent Skills spec](https://agentskills.io/specification). Skills are not embedded in the binary — they are distributed separately for agent harnesses to consume.

> [!NOTE]
> **Minimal Dependencies**: The CLI has 3 external dependencies (Cobra + pflag + segmentio/analytics-go). Everything else is stdlib. analytics-go is the deliberate exception for telemetry — see `internal/telemetry/`. Do not add additional dependencies without strong justification.

## Build & Test

```bash
cd cli && go build ./...     # Build
cd cli && go test ./...      # Run tests
cd cli && go vet ./...       # Lint
```

> [!IMPORTANT]
> **Test Coverage**: When adding new resources or operations, add corresponding tests in `internal/resources/<name>_test.go`. Use the existing `newTestTokenServer()` and `newTestClient()` helpers for HTTP mocking. Registry tests use `newMockResource()` / `newMockOperation()`.

## Architecture

The CLI uses a **resource-registry** pattern:

1. `main.go` loads config, resolves credentials, creates an authenticated HTTP client
2. `resources.RegisterAll()` registers all resource definitions in the global registry
3. `registry.Build()` converts registered resources into a Cobra command tree
4. Cobra parses argv and dispatches to the matching operation's `Run` function

### Package Layout

| Package | Purpose |
| --- | --- |
| `main.go` | Entry point: config -> auth -> client -> registry -> execute |
| `cmd/` | Root Cobra command, persistent flags, version and login commands |
| `internal/registry/` | Resource/Operation types, dynamic Cobra command builder |
| `internal/resources/` | All resource implementations |
| `internal/spec/` | OpenAPI request/response schemas (extracted at build time) |
| `cmd/extract-schemas/` | Generator: reads `api/*.json` and emits `internal/spec/extracted_gen.go` |
| `api/` | Checked-in OpenAPI specs (source of truth for the schema feature) |
| `skills/` | Single agent skill at `skills/airbyte-agent/`. Umbrella `SKILL.md` + per-command playbooks under `references/<command>.md` |
| `internal/client/` | HTTP client with retry logic, structured error types |
| `internal/auth/` | Credential resolution (env -> file), OAuth token caching |
| `internal/config/` | Environment variable configuration loader |
| `internal/output/` | JSON output formatter |
| `internal/telemetry/` | Segment-backed anonymous usage events. One `CLI Command Executed` event per tracked invocation. Hardcoded write key in `config.go`; tracker no-ops when key is empty, mode is disabled, or org_id is unresolved. |
| `internal/versioncheck/` | Once-per-day GitHub Releases poll + stderr nudge when the installed CLI is behind. Cache lives at `~/.airbyte-agent/version-check.json` (24h TTL). No-ops on non-TTY stderr, dev builds, prereleases, or when the user opts out. |

### Registry (`internal/registry/`)

| File | Purpose |
| --- | --- |
| `types.go` | `Resource` interface, `Operation` struct, `OperationSchema`, `ParamSchema`, `OperationHooks` |
| `registry.go` | Thread-safe global registry: `Register()`, `All()`, `Get()`, `Reset()` |
| `builder.go` | Converts registered resources into Cobra commands with per-parameter flags, `--json`, file input (`@filename`), parameter validation, and hook execution |

### Resources (`internal/resources/`)

| File | Purpose |
| --- | --- |
| `register.go` | `RegisterAll()` -- registers all resources in the global registry |
| `organizations.go` | `organizations list\|use` -- list and persist a default organization |
| `workspaces.go` | `workspaces list` -- list/filter workspaces with automatic cursor pagination |
| `connectors.go` | `connectors list\|list-available\|describe\|inspect\|execute\|update\|delete` -- connector management with name->ID resolution hooks |
| `skills.go` | `skills list\|search\|docs` -- discover and render connector/static skill docs |
| `connectors_create.go` | `connectors create` -- interactive browser-based credential flow (OAuth session + polling) |

### Client (`internal/client/`)

| File | Purpose |
| --- | --- |
| `client.go` | HTTP client: `Get()`, `Post()`, `Patch()`, `Delete()` with auth headers, retry logic (3x exponential backoff on 429/502/503/504), 30s timeout |
| `errors.go` | `APIError` struct with `Type`, `Message`, `StatusCode`, `Retryable`, `ExitCode()` mapping |

### Auth (`internal/auth/`)

| File | Purpose |
| --- | --- |
| `credentials.go` | `ResolveSettings()` -- returns `Settings{Credentials, OrganizationID}`. Env vars first (all three required), then `~/.airbyte-agent/settings.json` |
| `credentials_file.go` | Read/write `~/.airbyte-agent/settings.json` (`{settings: {credentials: {...}, organization_id: "..."}}` shape) with atomic writes and 0600 permission enforcement |
| `token.go` | `TokenManager` -- OAuth token acquisition and caching with auto-refresh |
| `browserlogin/` | Browser-based `login` flow: PKCE primitives (`pkce.go`, `state.go`), loopback server (`server.go`), Keycloak token exchange (`flow.go`), sonar bootstrap caller + multi-org picker (`bootstrap.go`, `picker.go`). Consumed only by `cmd/login.go`. Keycloak access/refresh tokens are transient -- discarded after the bootstrap call. |

## Command Surface

| Resource | Operation | Description | Key Params |
| --- | --- | --- | --- |
| `organizations` | `list` | List organizations | -- |
| `organizations` | `use` | Set the default organization in `~/.airbyte-agent/settings.json` | `id` (required) |
| `workspaces` | `list` | List/filter workspaces | `name_contains`, `status`, `limit` |
| `connectors` | `list` | List workspace connectors | `workspace` (required) |
| `connectors` | `list-available` | List connector templates | -- |
| `connectors` | `describe` | Get connector details + schema | `name`+`workspace` or `--id` |
| `connectors` | `inspect` | Get connector metadata, readiness, and `docs_skill_id` | `name`+`workspace` or `--id` |
| `connectors` | `execute` | Execute a connector action | `name`+`workspace` or `--id`, `entity`, `action`, `params` |
| `connectors` | `create` | Interactive credential flow | `workspace`, `name` (template) or `id` (template ID) |
| `connectors` | `update` | Open the browser to edit a connector's credentials | `name`+`workspace` or `--id` |
| `connectors` | `delete` | Delete a connector | `name`+`workspace` or `--id` |
| `skills` | `list` | List connector/static skill docs | `workspace`, `limit`, `cursor` |
| `skills` | `search` | Search connector/static skill docs | `workspace`, `query`, `limit`, `cursor` |
| `skills` | `docs` | Read rendered skill docs or raw docs JSON | `id`, `section`, `workspace`, `format` |

### Common Flags

| Flag | Description | Default |
| --- | --- | --- |
| `--output, -o` | Write output to file instead of stdout | -- |
| `--verbose, -v` | Enable debug logging | `false` |
| `--json` | Operation flag for inline JSON parameters; mutually exclusive with per-parameter flags | -- |
| `--<param>` | Per-parameter operation flags generated from each scalar/array schema parameter, e.g. `--id`, `--workspace`, `--select-fields` | -- |
| `--fields` | Client-side response filter (comma-separated dotted paths, e.g. `data.id,data.name`). Applied in `writeResult` after `Run`; bypasses error payloads. | -- |

## Credential Security

> [!IMPORTANT]
> **NEVER accept credentials (API keys, tokens, passwords, secrets) directly as parameters or in chat.** Two browser-based flows handle every credential entry path:
> - **CLI account credentials** (`client_id` / `client_secret` / `organization_id`) — handled by `airbyte-agent login`, which opens the browser to Keycloak, completes PKCE, and bootstraps the trio from the airbyte.ai bootstrap endpoints.
> - **Connector credentials** (API keys, OAuth tokens for individual connectors) — handled by `airbyte-agent connectors create`, which opens a secure browser-based UI via an OAuth session.
>
> If a user offers credentials in conversation, decline and start the appropriate browser flow.

> [!NOTE]
> **Credential file permissions**: `WriteCredentialsFile` writes with `0600` permissions by default, but the CLI does not enforce permissions on read.

### `login` flow (PKCE + sonar bootstrap)

1. Generate a PKCE verifier + challenge and a CSRF state (`internal/auth/browserlogin/pkce.go`, `state.go`).
2. Bind a one-shot loopback server on `127.0.0.1:<ephemeral>/callback` (`server.go`).
3. Open the browser to `https://cloud.airbyte.com/auth/realms/_airbyte-cloud-users/protocol/openid-connect/auth?...` (`flow.go`).
4. Exchange the authorization code for a Keycloak access token at the token endpoint.
5. Call three sonar endpoints with the access token: `/internal/account/enrollment-status`, `/internal/account/organizations` (only when needed), `/internal/account/applications` (`bootstrap.go`). The applications POST is idempotent and returns the trio.
6. Write the trio to `~/.airbyte-agent/settings.json`. Keycloak tokens are discarded.
7. Use `--manual` for the legacy prompt flow (headless / no-browser environments). Use `--org-id <uuid>` to skip the multi-organization picker.

> [!NOTE]
> **Keycloak prerequisite**: the `sonar-webapp` Keycloak client in realm `_airbyte-cloud-users` must list `http://127.0.0.1:*/callback` in its `redirectUris` allowlist for the loopback hand-off to work.

### `connectors create` flow

1. Resolve template ID (by name or ID)
2. Create a widget token for the web app
3. Create an OAuth session for the source definition
4. Open browser to `<webapp>/widget-bridge?widget_token=...&session=...&selectedTemplateId=...`
5. Poll session status with exponential backoff (2s, 4s, 8s, 16s)
6. On completion, create the connector with returned credentials

## Input Validation

> [!IMPORTANT]
> **This CLI is frequently invoked by AI agents.** Always assume inputs can be adversarial. Connector and workspace names are user-supplied -- always use the resolution hooks (`resolveConnectorID`, `resolveWorkspaceID`) to convert names to IDs server-side rather than embedding raw strings in API URL paths.

- **Name resolution**: The `PreRun` hook `resolveConnectorID` converts `name` + `workspace` into a validated `id` before the operation runs. This prevents path injection.
- **Parameter validation**: The registry builder validates all parameters against the operation schema before execution. Required params are enforced, types are checked.
- **File input**: The `@filename` syntax reads parameters from a local file. The file path is resolved relative to CWD.

## Error Handling

All errors are returned as JSON on stderr:
```json
{"type": "<error_type>", "message": "...", "status_code": 400, "retryable": false}
```

> [!NOTE]
> **Unknown commands**: Invoking an unknown command or subcommand (e.g., `airbyte-agent nothing`, `airbyte-agent connectors notarealthing`) returns an `unknown_command` error on stderr. The payload includes `available_commands` (scoped to the parent), `did_you_mean` (Levenshtein-close suggestions, omitted when empty), and a `hint` pointing at `--help`. Exit code is `4` (validation). No `status_code` / `retryable` fields — the error is local, not from the API.

### Exit Codes

| Code | Meaning | HTTP Status |
| --- | --- | --- |
| `0` | Success | 2xx |
| `1` | General error | 500, others |
| `2` | Authentication error | 401, 403 |
| `3` | Not found | 404 |
| `4` | Validation error | 400, 422 |

### Retry Behavior

The HTTP client automatically retries transient failures:
- **Retryable**: 429 (rate limit), 502, 503, 504 (server errors)
- **Not retryable**: 400, 401, 403, 404, 422
- **Strategy**: 3 retries with exponential backoff (1s, 2s, 4s)
- **Timeout**: 30 seconds per request

## Environment Variables

### Authentication

| Variable | Description | Default |
| --- | --- | --- |
| `AIRBYTE_CLIENT_ID` | OAuth client ID | (required) |
| `AIRBYTE_CLIENT_SECRET` | OAuth client secret | (required) |
| `AIRBYTE_ORGANIZATION_ID` | Organization ID | (required) |
| `AIRBYTE_WORKSPACE` | Default workspace name | `default` |

All three are also stored in the settings file (`~/.airbyte-agent/settings.json`). Env-var resolution requires all three to be set; otherwise the CLI falls through to the file. `airbyte-agent login` writes the file; the env-var precedence at runtime is unchanged by the login flow.

### Configuration

| Variable | Description | Default |
| --- | --- | --- |
| `AIRBYTE_API_HOST` | API base URL | `https://api.airbyte.ai` |
| `AIRBYTE_WEBAPP_URL` | Web app URL for `connectors create` credential flow | `https://app.airbyte.ai` |
| `AIRBYTE_KEYCLOAK_URL` | Keycloak realm base URL for the `login` browser flow | `https://cloud.airbyte.com/auth/realms/_airbyte-cloud-users` |
| `AIRBYTE_CREDENTIAL_TIMEOUT` | `connectors create` credential flow timeout in seconds | `180` |
| `AIRBYTE_ALLOW_DESTRUCTIVE` | When truthy (`1`/`true`/`yes`/`on`), skips the interactive confirmation prompt on destructive commands like `connectors delete`. Mirrors the `allow_destructive` settings.json key. | `false` |
| `AIRBYTE_TELEMETRY_MODE` | Set to `disabled` to turn off telemetry emission. Any other value (or unset) falls through to the `telemetry_enabled` key in settings.json. | (settings file) |
| `AIRBYTE_INTERNAL_USER` | When truthy, tags emitted events with `is_internal_user: true` so internal events can be filtered out of customer analytics. Overrides the `is_internal_user` key in settings.json when non-empty. | (settings file) |
| `AIRBYTE_EXECUTION_CONTEXT` | Self-reported invocation context (`mcp`, `agent`, `direct`). Emitted as the `execution_context` property on every telemetry event. | `direct` |
| `AIRBYTE_VERSION_CHECK` | Set to `disabled` to suppress the once-per-day "new version available" nudge. Any other value (or unset) falls through to the `version_check_enabled` key in settings.json. | (settings file) |

Settings file at `~/.airbyte-agent/settings.json` (JSON format, 0600 permissions):

```json
{
  "settings": {
    "credentials": {
      "client_id": "your-client-id",
      "client_secret": "your-client-secret"
    },
    "organization_id": "your-org-id",
    "workspace": "default",
    "allow_destructive": false,
    "telemetry_enabled": true,
    "is_internal_user": false,
    "version_check_enabled": true
  }
}
```

`workspace` is optional. When absent or empty, commands that take a `workspace` parameter without receiving one fall back to the literal `"default"`. Resources read the configured value via `client.Client.DefaultWorkspace()`, which `main.go` populates from `Settings.Workspace`.

`allow_destructive` is optional (default `false`). When `true`, destructive operations (currently `connectors delete`) skip the interactive `"Type 'yes' to confirm:"` prompt. Intended as a one-time permission grant for agent harnesses that can't answer a TTY prompt. The non-interactive default refuses with a clear `validation_error` rather than hanging on stdin. Resources read this via `client.Client.AllowDestructive()`.

`telemetry_enabled` defaults to `true` when the key is absent (matching the documented default). `login` always writes this key, so the absent case only occurs for settings files predating the field. `AIRBYTE_TELEMETRY_MODE=disabled` overrides the file value at runtime.

`is_internal_user` defaults to `false`. Edit the file directly (or set `AIRBYTE_INTERNAL_USER=true`) to mark an invocation as Airbyte-internal so its events can be filtered out of customer analytics.

`version_check_enabled` defaults to `true` when the key is absent. When `true`, the CLI hits `https://api.github.com/repos/airbytehq/airbyte-agent-cli/releases/latest` once every 24h and prints a one-line nudge to stderr when the installed binary is behind. The check is skipped on non-TTY stderr (so scripted use doesn't see the nudge), for dev builds and prereleases, and when `AIRBYTE_VERSION_CHECK=disabled`. Cache lives at `~/.airbyte-agent/version-check.json`.

## Adding New Resources

When adding a new resource or operation:

1. Create `internal/resources/<name>.go` implementing the `Resource` interface (`Name()`, `Description()`, `Operations()`)
2. Register it in `internal/resources/register.go` via `registry.Register()`
3. Add tests in `internal/resources/<name>_test.go` using `newTestTokenServer()` and `newTestClient()` helpers
4. If the resource uses name-based lookup, add a `PreRun` hook for server-side ID resolution
5. Update the **Command Surface** table in this file
6. If the resource adds a new leaf command, add a corresponding playbook at `skills/airbyte-agent/references/<command>.md` and link it in the **Command index** table of `skills/airbyte-agent/SKILL.md`
7. Set `SpecRef: registry.SpecRef{Path: "...", Method: "..."}` on each operation that maps to an OpenAPI route, then run `go generate ./...` (or `make generate`) so `internal/spec/extracted_gen.go` picks up the new route. CI fails if this file is stale.

### Adding New Skill References

Per-command playbooks live as plain markdown under `skills/airbyte-agent/references/`. To add one:

1. Create the file: `skills/airbyte-agent/references/<resource>-<operation>.md` (no YAML frontmatter — references are opened on demand by the umbrella skill).
2. Lead with an H1 (`# <resource> <operation>`) and follow with task-oriented body content (when to use, usage examples, error recovery, "do NOT" guidance).
3. Add a row to the **Command index** table in `skills/airbyte-agent/SKILL.md` pointing at the new file.
4. Promote any cross-command rules into the **Universal rules** or **Connector rules** sections of `SKILL.md` rather than duplicating them per reference.
5. No Go changes required — skills are not embedded in the binary.

## Skills Reference

The single agent skill is at `skills/airbyte-agent/`. Browse `skills/airbyte-agent/SKILL.md` for the routing table and `skills/airbyte-agent/references/` for per-command playbooks.
