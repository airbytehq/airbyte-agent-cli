# Airbyte Agents CLI Context

This document tells AI agents how to use the `airbyte-agent` CLI. For development/architecture details, see `AGENTS.md`.

## Rules of Engagement

> [!IMPORTANT]
> **Schema Discovery**: If you don't know the exact JSON payload structure for a command, run `airbyte-agent schema <resource> <operation>` first. This returns the parameter schema without executing the operation.

> [!IMPORTANT]
> **Always filter responses to the fields you need.** Whenever you know which fields will satisfy the user's request, pass `--fields` to trim the output. This applies to **every command** — list, inspect, docs, execute, etc. Unfiltered responses waste context window and bandwidth on data you will discard anyway. The only time to skip the filter is when you genuinely need the full payload (e.g. one-shot debugging, or you don't yet know which fields exist — in which case run `airbyte-agent schema <resource> <operation>` or do a small probe call first).
>
> For row-level reads via `connectors execute`, also pass `select_fields` (API-side) to reduce upstream work. `select_fields` and `--fields` are complementary: the first stops the source connector from emitting columns you don't need; the second trims what the CLI prints to stdout.

> [!IMPORTANT]
> **Discover before executing**: Always run `connectors inspect`, then `skills docs` with the returned `docs_skill_id`, before the first `execute` on any connector. Entity and action names vary by connector type and are not guessable.

## Core Syntax

```bash
airbyte-agent <resource> <operation> [flags]
```

All parameters are passed via `--json '<JSON>'` or `--id '<ID>'`. Output goes to stdout as JSON.

```bash
airbyte-agent --help                              # List all resources
airbyte-agent <resource> --help                   # List operations for a resource
airbyte-agent schema <resource> <operation>       # Show parameter schema (CLI + OpenAPI)
```

### Key Flags

| Flag | Description | Default |
| --- | --- | --- |
| `--json` | Inline JSON parameters | -- |
| `--id` | Convenience flag for resource ID | -- |
| `--output, -o` | Write output to file instead of stdout | -- |
| `--verbose, -v` | Enable debug logging | `false` |
| `--fields` | Filter response to listed fields (comma-separated dotted paths, e.g. `data.id,data.name`). Client-side; not applied to errors. | -- |

## Usage Patterns

### 1. First-Time Setup

```bash
# Default: opens your browser to complete Keycloak login, then bootstraps
# client_id / client_secret / organization_id and writes them to
# ~/.airbyte-agent/settings.json.
airbyte-agent login

# Headless environment with no browser? Use --manual to fall back to
# the legacy prompt-driven flow.
airbyte-agent login --manual

# Belong to more than one organization? Skip the multi-org picker by
# passing the UUID directly.
airbyte-agent login --org-id <organization-uuid>

# Find your workspace
airbyte-agent workspaces list
```

The browser flow uses PKCE (no client secret required) and runs a one-shot loopback server on `127.0.0.1` to receive the OAuth callback. Keycloak access tokens are transient — they are used once to call the airbyte.ai bootstrap endpoints and then discarded. Only the bootstrapped `client_id`, `client_secret`, and `organization_id` are persisted.

If your organization is not yet enrolled, the flow will exit with a `validation_error` and a hint to complete enrollment at `https://app.airbyte.ai` in the browser before retrying.

### 2. Listing and Discovering Connectors

```bash
# List connectors in a workspace
airbyte-agent connectors list --json '{"workspace": "my-workspace"}'

# List available connector templates (for creating new connectors)
airbyte-agent connectors list-available

# Inspect a connector and get its docs skill ID
airbyte-agent connectors inspect --json '{"workspace": "my-workspace", "name": "my-source"}'

# Or by ID
airbyte-agent connectors inspect --id 'f24fb2b0-c054-48f1-9e0f-cfb62e12f878'

# Read the docs outline and exact section before executing
airbyte-agent skills docs --json '{"id": "<docs_skill_id from inspect>"}' --fields data.markdown
airbyte-agent skills docs --json '{"id": "<docs_skill_id from inspect>", "section": "<exact-section-id>"}' --fields data.markdown
```

### 3. Executing Connector Actions

Always `connectors inspect` and `skills docs` first to discover available entities and actions. `connectors describe` remains available only for legacy clients that consume the old merged schema output.

```bash
# Read data from a connector
airbyte-agent connectors execute --json '{
  "workspace": "my-workspace",
  "name": "my-source",
  "entity": "users",
  "action": "read"
}'

# With parameters
airbyte-agent connectors execute --json '{
  "workspace": "my-workspace",
  "name": "my-source",
  "entity": "deals",
  "action": "search",
  "params": {"query": "status:open"}
}'

# Limit response fields to protect context window
airbyte-agent connectors execute --json '{
  "workspace": "my-workspace",
  "name": "my-source",
  "entity": "contacts",
  "action": "read",
  "select_fields": ["id", "email", "name"]
}'

# Exclude heavy fields
airbyte-agent connectors execute --json '{
  "workspace": "my-workspace",
  "name": "my-source",
  "entity": "messages",
  "action": "read",
  "exclude_fields": ["body_html", "attachments"]
}'
```

### 4. Creating a New Connector

```bash
# Browse available templates
airbyte-agent connectors list-available

# Create (opens browser for secure credential entry)
airbyte-agent connectors create --json '{
  "workspace": "my-workspace",
  "name": "hubspot"
}'
```

### 5. Updating Connector Credentials

```bash
# Open the credentials page in your browser to edit an existing connector
airbyte-agent connectors update --json '{"workspace": "my-workspace", "name": "my-source"}'
```

The CLI resolves the connector, prints the action message + a `Type 'yes' to confirm (skips after 10s)` prompt, and opens your browser to `<webapp>/organizations/<org_id>/credentials` only if you type `yes` within the timeout. Any other input — `no`, empty line, EOF, or the timeout firing — skips the browser open. The result on stdout always includes `url`, `connector_id`, `message`, and `browser_opened: bool`, so non-interactive callers (MCP, CI, piped subprocesses) get the URL even when the prompt is skipped. Honors `AIRBYTE_WEBAPP_URL` for non-prod environments.

### 6. Deleting a Connector

```bash
airbyte-agent connectors delete --json '{"workspace": "my-workspace", "name": "old-source"}'
```

Delete is destructive and prompts for an interactive `"Type 'yes' to confirm:"` on a TTY. Without a TTY (e.g. piped agent input), the command refuses with a `validation_error` whose hint tells you to set `"allow_destructive": true` in `~/.airbyte-agent/settings.json` (or `AIRBYTE_ALLOW_DESTRUCTIVE=true`). Once that permission is granted, the prompt is skipped.

### 7. Schema Introspection

Use the top-level `schema` command to see an operation's parameter schema (and underlying OpenAPI request/response) before calling it:

```bash
airbyte-agent schema connectors execute
# Returns:
# {
#   "description": "Execute an action on a connector",
#   "params": {
#     "name": {"type": "string", "required": false, "description": "Connector name (requires workspace)"},
#     "workspace": {"type": "string", "required": false, "description": "Workspace name (required when using name)"},
#     "id": {"type": "string", "required": false, "description": "Connector ID (alternative to name)"},
#     "entity": {"type": "string", "required": true, "description": "Entity name"},
#     "action": {"type": "string", "required": true, "description": "Action name"},
#     ...
#   },
#   "api": { "path": "...", "method": "POST", "request_body": {...}, "response": {...} }
# }
```

### 8. Loading Parameters from a File

For complex JSON payloads, use `@filename`:

```bash
echo '{"workspace": "my-workspace", "name": "my-source", "entity": "users", "action": "read"}' > params.json
airbyte-agent connectors execute --json @params.json
```

## Error Handling

All errors are JSON on stderr with an exit code:

| Exit Code | Meaning |
| --- | --- |
| `0` | Success |
| `1` | General error |
| `2` | Authentication error |
| `3` | Not found |
| `4` | Validation error |

Errors include a `hint` field with actionable guidance:

```json
{
  "type": "not_found",
  "message": "connector \"gong\" not found in workspace \"default\"",
  "status_code": 404,
  "retryable": false,
  "hint": "run 'airbyte-agent connectors list --json '{\"workspace\": \"default\"}'' to see available connectors"
}
```

API errors (400/422) include the full server response in `detail`:

```json
{
  "type": "validation_error",
  "message": "Invalid configuration",
  "status_code": 400,
  "retryable": false,
  "detail": {"errors": [{"field": "host", "message": "is required"}]}
}
```

When you see a validation error for missing parameters, run `airbyte-agent schema <resource> <operation>` to check the schema:

```json
{
  "type": "validation_error",
  "message": "missing required parameters: entity, action",
  "status_code": 400,
  "hint": "run `airbyte-agent schema <resource> <operation>` to see the expected parameter schema"
}
```
