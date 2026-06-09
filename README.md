# airbyte-agent-cli

A Go CLI for the Airbyte Agent API, designed to be driven by both humans and AI agents.

The CLI exposes Airbyte's resources (organizations, workspaces, connectors, etc.) as a uniform `airbyte-agent <resource> <operation>` interface. Every command supports JSON input/output, schema introspection via `airbyte-agent schema`, and structured JSON errors with stable exit codes — making it safe to script and easy for agents to discover at runtime.

See [`AGENTS.md`](AGENTS.md) for the full architecture reference and [`CONTEXT.md`](CONTEXT.md) for the agent-facing usage guide.

## Install

### Quick install (macOS, Linux, WSL)

The fastest way to install the CLI **and** the agent skills.

```bash
curl -fsSL https://airbyte.ai/install.sh | bash
```

Environment overrides:

| Variable | Description | Default |
| --- | --- | --- |
| `AIRBYTE_AGENT_VERSION` | Install a specific tag (e.g. `v0.2.0`) | latest release |
| `AIRBYTE_AGENT_INSTALL_DIR` | CLI target directory | `/usr/local/bin` if writable, else `~/.local/bin` |
| `AIRBYTE_AGENT_SKILLS_DIR` | Skills target directory | `~/.claude/skills` |
| `AIRBYTE_AGENT_SKIP_SKILLS` | Set to `1` to install only the CLI | unset |

### Homebrew (macOS, Linux)

```bash
brew install airbytehq/tap/airbyte-agent-cli
```

### Manual binary download

Grab the archive for your platform from the [latest release](https://github.com/airbytehq/airbyte-agent-cli/releases/latest), extract it, and put `airbyte-agent` somewhere on your `$PATH`. Builds are published for `linux`/`darwin`/`windows` × `amd64`/`arm64` (Windows arm64 excepted).

### Build from source

```bash
git clone https://github.com/airbytehq/airbyte-agent-cli.git
cd airbyte-agent-cli
make build         # builds ./airbyte-agent
# or
make install       # installs to $GOBIN
```

Or directly without the Makefile:

```bash
go build -o airbyte-agent .
```

## Skills

The repo ships a single agent skill, `skills/airbyte-agent/`, that bundles per-command playbooks under `skills/airbyte-agent/references/<command>.md`. The top-level `SKILL.md` carries cross-command rules (`--json` payload format, `--fields` filtering, auth recovery, `connectors inspect` + `skills docs` before `execute`) and a routing table the agent uses to open the matching reference for the task at hand. This follows the [Agent Skills spec](https://agentskills.io/specification) for progressive disclosure: only the small `SKILL.md` is loaded on activation, and per-command references are opened on demand.

The skill works alongside the CLI — it tells the agent *how* to invoke each command, while the `airbyte-agent` binary does the actual work. Install the CLI first, then install the skill into your agent.

### Install via `npx skills`

The [`skills`](https://github.com/vercel-labs/skills) installer discovers the `skills/` directory in this repo and wires `SKILL.md` (with its `references/` subdirectory) into your agent (Claude Code, etc.):

```bash
# Install the skill
npx skills add airbytehq/airbyte-agent-cli

# Preview without installing
npx skills add airbytehq/airbyte-agent-cli --list

# Install globally instead of per-project
npx skills add airbytehq/airbyte-agent-cli -g
```

Target a specific agent with `--agent claude-code` (or another supported agent). See the [`skills` CLI docs](https://github.com/vercel-labs/skills) for the full flag set.

### Manual install

Copy or symlink `skills/airbyte-agent/` into your agent's skill directory directly (e.g. `~/.claude/skills/airbyte-agent/` for Claude Code). The `references/` subdirectory must be preserved alongside `SKILL.md` — the umbrella skill points at it on demand.

## Configure

Settings can be supplied via environment variables or a settings file at `~/.airbyte-agent/settings.json`. Three pieces of information are always required: client ID, client secret, and organization ID.

### Sign in with `airbyte-agent login`

By default, `airbyte-agent login` opens your browser to complete the Keycloak login at airbyte.ai, then bootstraps `client_id`, `client_secret`, and `organization_id` from the airbyte.ai bootstrap endpoints and writes them to `~/.airbyte-agent/settings.json` with `0600` permissions. The Keycloak access token used during the handshake is transient — it is discarded as soon as the credentials are written.

```bash
# Default: browser flow
airbyte-agent login

# Headless / no-browser environment? Fall back to the legacy prompt flow
# that asks you for client_id, client_secret, and organization_id directly.
airbyte-agent login --manual

# Belong to more than one organization? Skip the multi-org picker.
airbyte-agent login --org-id <organization-uuid>

# Inspect the saved settings (the secret is obfuscated)
airbyte-agent login show
```

The browser flow uses PKCE (no client secret in the CLI) and starts a one-shot loopback server on `127.0.0.1:<ephemeral>/callback` to receive the OAuth redirect. Re-running `airbyte-agent login` only rewrites the credential trio — your `workspace`, `allow_destructive`, `telemetry_enabled`, and `is_internal_user` values are preserved.

If your organization is not yet enrolled, the flow exits with a `validation_error` and a hint to complete enrollment at `https://app.airbyte.ai` before retrying.

### Resolution order

1. **Environment variables** — used only if **all three** of `AIRBYTE_CLIENT_ID`, `AIRBYTE_CLIENT_SECRET`, and `AIRBYTE_ORGANIZATION_ID` are set. If any is missing, the CLI falls through to the file.
2. **Settings file** at `~/.airbyte-agent/settings.json`. All three fields must be populated.
3. If neither is configured, the CLI exits with an authentication error.

Env vars take precedence over the file when all three are present, so they're useful for one-off overrides (e.g. `AIRBYTE_ORGANIZATION_ID=... airbyte-agent ...`).

### Environment variables

| Variable | Description | Default |
| --- | --- | --- |
| `AIRBYTE_CLIENT_ID` | OAuth client ID | (required) |
| `AIRBYTE_CLIENT_SECRET` | OAuth client secret | (required) |
| `AIRBYTE_ORGANIZATION_ID` | Organization ID | (required) |
| `AIRBYTE_WORKSPACE` | Default workspace name (used when commands don't pass `workspace`) | `default` |
| `AIRBYTE_API_HOST` | API base URL | `https://api.airbyte.ai` |
| `AIRBYTE_WEBAPP_URL` | Web app URL for the `connectors create` credential widget | `https://app.airbyte.ai` |
| `AIRBYTE_KEYCLOAK_URL` | Keycloak realm base URL used by the `login` browser flow | `https://cloud.airbyte.com/auth/realms/_airbyte-cloud-users` |
| `AIRBYTE_CREDENTIAL_TIMEOUT` | `connectors create` credential flow timeout (seconds) | `180` |
| `AIRBYTE_ALLOW_DESTRUCTIVE` | When truthy (`1`/`true`/`yes`/`on`), skip the interactive confirmation prompt on destructive commands like `connectors delete`. Mirrors `allow_destructive` in the settings file. | `false` |
| `AIRBYTE_TELEMETRY_MODE` | Set to `disabled` to turn off telemetry. Any other value (or unset) falls through to `telemetry_enabled` in the settings file. | (settings file) |
| `AIRBYTE_VERSION_CHECK` | Set to `disabled` to suppress the once-per-day "new version available" nudge. Any other value (or unset) falls through to `version_check_enabled` in the settings file. | (settings file) |

### Settings file

`~/.airbyte-agent/settings.json`:

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
    "version_check_enabled": true
  }
}
```

| Key | Description | Default |
| --- | --- | --- |
| `credentials.client_id` | OAuth client ID | (required) |
| `credentials.client_secret` | OAuth client secret | (required) |
| `organization_id` | Organization ID | (required) |
| `workspace` | Default workspace name; commands that take a `workspace` parameter fall back to this when not provided | `default` |
| `allow_destructive` | When `true`, destructive commands (e.g. `connectors delete`) skip the interactive confirmation prompt. Intended as a one-time permission grant for agent harnesses without TTY input. | `false` |
| `telemetry_enabled` | When `false`, anonymous usage telemetry is disabled. Can also be turned off at runtime with `AIRBYTE_TELEMETRY_MODE=disabled`. | `true` |
| `version_check_enabled` | When `true`, the CLI polls GitHub Releases once every 24h and prints a one-line nudge to stderr if a newer release is available. The cache lives at `~/.airbyte-agent/version-check.json`. The check is automatically skipped on non-TTY stderr, for `dev`/prerelease builds, and when `AIRBYTE_VERSION_CHECK=disabled`. | `true` |

## Usage

```bash
airbyte-agent <resource> <operation> [flags]
```

Parameters can be supplied two ways: as a single JSON document via `--json`, or as individual flags (`--workspace foo --name bar`). The two modes are **mutually exclusive** — passing both is an error. Output is JSON.

### Two ways to pass parameters

**1. Individual flags (recommended for humans)** — scalar and array parameters in the operation's schema are exposed as `--<param>` flags, with snake_case keys converted to kebab-case (e.g. `select_fields` → `--select-fields`):

```bash
airbyte-agent connectors inspect --workspace default --name hubspot
airbyte-agent skills docs --id '<docs_skill_id from inspect>'
airbyte-agent connectors execute --workspace default --name hubspot \
  --entity contacts --action read \
  --select-fields id,email,name
```

Run `airbyte-agent <resource> <operation> --help` to see the available flags for any command.

**2. JSON (recommended for agents and complex payloads)** — pass the whole parameter set as a JSON object:

```bash
airbyte-agent connectors execute --json '{
  "workspace": "default",
  "name": "hubspot",
  "entity": "contacts",
  "action": "read",
  "select_fields": ["id", "email", "name"]
}'
```

Use `@filename` to load JSON from a file: `--json @params.json`. `--json` is the only way to pass nested objects (e.g. the `params` field on `connectors execute`).

### Common flags

| Flag | Description | Default |
| --- | --- | --- |
| `--json` | Operation flag for inline JSON parameters (or `@filename` to load from a file). Cannot be combined with per-parameter flags. | -- |
| `--output, -o` | Write output to a file instead of stdout | -- |
| `--verbose, -v` | Enable debug logging | `false` |
| `--fields` | Filter the response to only the listed fields. Comma-separated, dotted paths (e.g. `data.id,data.name`). Applied client-side, after the API responds. Errors are not filtered. | -- |

### Filtering output with `--fields`

`--fields` shapes the response payload after it returns from the API. Paths use dotted notation; when a path crosses an array, the remaining segments are applied to every element ("array broadcast"):

```bash
# Both of these work — list responses are wrapped in {"data": [...]} and the
# CLI auto-broadcasts when no path matches a top-level key.
airbyte-agent organizations list --fields id,organization_name
airbyte-agent organizations list --fields data.id,data.organization_name

# Mixed paths require explicit prefixes — the auto-broadcast only fires
# when *no* path matches a top-level key:
airbyte-agent connectors list --fields data.id,data.name,next
```

**Path resolution rules:**

1. **Strict match first.** Paths are matched against top-level keys of the response.
2. **Smart wrapper fallback.** When *no* paths match top-level keys AND the response has *exactly one* top-level array (e.g. `{"data": [...]}`), each path is implicitly prefixed with that wrapper's key and re-applied. Lets you write `--fields id,name` instead of `--fields data.id,data.name` for list-style responses.
3. **Mixed cases stay strict.** If even one path matches top-level, no rewrite happens — pass explicit dotted paths if you also want row-level fields.
4. **Missing paths are dropped silently.** Errors are never filtered.

This is **client-side**: the full payload still travels from the API to the CLI. To reduce upstream work, `connectors execute` separately accepts `select_fields` / `exclude_fields` which are sent to the source connector. The two are complementary — combine them when you want both bandwidth savings and a clean output shape.

### Discovering commands

```bash
airbyte-agent --help                              # list resources
airbyte-agent connectors --help                   # list operations
airbyte-agent schema connectors execute           # CLI params + OpenAPI request/response schema
```

`airbyte-agent schema <resource> <operation>` returns the CLI-level parameters under `params`, plus the underlying OpenAPI route (path, method, parameters, request body, response) under `api`. The OpenAPI schemas are extracted at build time from `api/*.json` and cover only the routes the CLI actually uses, so the dump stays focused.

### Examples

```bash
# Find a workspace
airbyte-agent workspaces list --json '{}'

# Inspect connector metadata and read usage docs
airbyte-agent connectors inspect --json '{"workspace": "default", "name": "hubspot"}'
airbyte-agent skills docs --json '{"id": "<docs_skill_id from inspect>"}' --fields data.markdown
airbyte-agent skills docs --json '{"id": "<docs_skill_id from inspect>", "section": "<exact-section-id>"}' --fields data.markdown

# Read data, limiting fields to keep the response small
airbyte-agent connectors execute --json '{
  "workspace": "default",
  "name": "hubspot",
  "entity": "contacts",
  "action": "read",
  "select_fields": ["id", "email", "name"]
}'

# Create a new connector (opens a browser for secure credential entry)
airbyte-agent connectors create --json '{
  "workspace": "default",
  "name": "hubspot"
}'

# Load a complex payload from a file
airbyte-agent connectors execute --json @params.json
```

`connectors describe` remains available for legacy clients that consume the old merged schema output, but new workflows should use `connectors inspect` and `skills docs`.

## Develop

```bash
go build ./...
go test ./...
go vet ./...
```

To add a new resource: implement the `Resource` interface in `internal/resources/<name>.go`, register it in `register.go`, and add tests using the existing `newTestTokenServer()` / `newTestClient()` helpers. See [`AGENTS.md`](AGENTS.md) for the full guide.

## Releases

Releases are cut by pushing a `v*` tag. The `release` workflow builds binaries for `linux`/`darwin`/`windows` × `amd64`/`arm64` (Windows arm64 excepted) via [goreleaser](https://goreleaser.com), uploads them as a draft GitHub release, and commits an updated `Formula/airbyte-agent-cli.rb` to [airbytehq/homebrew-tap](https://github.com/airbytehq/homebrew-tap).

### Versioning

Tags follow [semver](https://semver.org). While the API is unstable, stay on `v0.x.y`:

- **`v0.x.0`** — new features or behavior changes (added a command, changed default behavior)
- **`v0.x.y`** — bug fixes only
- **`v1.0.0`** — first stable release; downgrade contract begins

Pre-releases use a suffix (`v0.2.0-rc1`, `v0.2.0-beta`); goreleaser publishes the GitHub release but **skips** the Homebrew formula update for these.

### Cutting a release

1. **Make sure `main` is green.** All tests pass, `go generate ./...` produces no diff (CI enforces this).
2. **Tag from `main`:**

   ```bash
   git checkout main
   git pull
   git tag v0.1.0
   git push origin v0.1.0
   ```

3. **Watch the release workflow.** It runs in two halves:
   - Builds + uploads a **draft** GitHub release (binaries, archives, `checksums.txt`).
   - Commits `Formula/airbyte-agent-cli.rb` to `airbytehq/homebrew-tap@main`.
4. **Review the draft release.** Open it in the GitHub UI, check the auto-generated changelog, and either edit the notes or accept them.
5. **Click Publish.** This is intentional manual gating — easy to catch a misconfigured formula or stale changelog before users see it.
6. **Smoke-test the install** on a fresh machine:

   ```bash
   brew install airbytehq/tap/airbyte-agent-cli
   airbyte-agent version
   ```

### Dry-run before tagging

Validate the release pipeline locally without pushing anything:

```bash
goreleaser check                    # validates .goreleaser.yaml
goreleaser release --snapshot --clean
# inspect dist/ — binaries, archives, and dist/homebrew/Formula/airbyte-agent-cli.rb
```

The snapshot run produces real artifacts in `dist/` so you can confirm the formula renders correctly and the version-stamped ldflags resolve as expected. Nothing is uploaded.

### If a release goes wrong

- **Bad binaries, formula not yet committed.** Delete the draft release in the GitHub UI, delete the tag locally and remotely (`git tag -d v0.1.0 && git push origin :refs/tags/v0.1.0`), fix, retag.
- **Formula already committed but binaries broken.** Delete the formula commit on `homebrew-tap` (open a one-line revert PR), then retag a new patch version. Don't re-use the same tag.
- **Need to yank a published release.** Delete the formula commit on `homebrew-tap`. Existing `brew install`s won't rollback automatically; users with the broken version will keep it until they `brew upgrade airbyte-agent-cli`. Consider a `0.x.y+1` patch release rather than a yank when possible — fewer surprises for users.

## License

[Elastic License 2.0](LICENSE).
