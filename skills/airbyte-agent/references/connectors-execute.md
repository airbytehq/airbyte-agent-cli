# connectors execute

Run an action against an entity on a connector — the workhorse command for actually moving data. This reference embeds the SDK-level knowledge of how the underlying API behaves (filter operators, pagination, response shape, field-selection rules).

> [!IMPORTANT]
> **Skill docs are the source of truth for what a specific connector supports.** Run `connectors inspect`, then pass the returned `docs_skill_id` to `skills docs` to discover the **entities**, **actions**, and **params** this connector supports. Do NOT guess these values. The action table below is a baseline only. **Read [`connectors-inspect.md`](connectors-inspect.md) and [`skills-docs.md`](skills-docs.md) before running `execute` against an unfamiliar connector.**

## Usage

```bash
airbyte-agent connectors execute --json '{
  "workspace": "default",
  "name": "hubspot",
  "entity": "contacts",
  "action": "context_store_search",
  "select_fields": ["id", "email", "firstName"],
  "params": {"limit": 20, "query": {"filter": {"fuzzy": {"firstName": "Teo"}}}},
  "intent": "look up contact details for Teo to draft an intro email"
}'
```

`name` (or `id`), `entity`, and `action` are required. `workspace` defaults to `default` when omitted. Pass complex payloads via `--json @path/to/file.json` to keep the shell command short.

`intent` (optional, max 512 chars) records *why* you're making this call — the goal, not the action (e.g. `"answer a refund dispute"`, not `"list orders"`). It's stored alongside the execution audit record; include it whenever you have meaningful context about the user's goal.

## Available actions (baseline)

Most connectors expose these actions, but the authoritative list for a given connector comes from `skills docs` using the `docs_skill_id` returned by `connectors inspect` (see next section). Entities are always per-connector — never assume them.

| Action | Purpose | Supports filtering? |
|---|---|---|
| `context_store_search` | **Default for reads.** Filter, sort, paginate over the indexed entity store. | yes (rich) |
| `list` | Live read from the source. Use when search index may lag or returns empty. | limited |
| `get` | Fetch a single entity by ID. | n/a |
| `api_search` | Provider-native search (e.g. Slack search syntax). Returns `{data, meta: {has_more}}`. | provider-specific |
| `create` | Write a new entity. | n/a |
| `update` | Modify an existing entity. | n/a |

## Discovering entities, actions, and params

Before composing `entity` / `action` / `params`, inspect the connector and read its skill docs. The inspect response returns `docs_skill_id`; the docs outline lists exact section IDs to request before executing. See [`connectors-inspect.md`](connectors-inspect.md) and [`skills-docs.md`](skills-docs.md) for the full playbooks.

```bash
airbyte-agent connectors inspect --json '{"workspace": "default", "name": "hubspot"}'
airbyte-agent skills docs --json '{"id": "<docs_skill_id from inspect>"}' --fields data.markdown
airbyte-agent skills docs --json '{"id": "<docs_skill_id from inspect>", "section": "<exact-section-id>"}' --fields data.markdown
```

What you'll find in the response:

- **`docs_skill_id`** from `connectors inspect` — pass this exact value to `skills docs`; do not construct it yourself.
- **Outline section IDs** from `skills docs` — pass an exact `section` value for the entity/action you plan to use.
- **Entity/action/params docs** — use these to choose `entity`, `action`, `params`, and `select_fields` precisely; never `select_fields: ["everything"]`.

Workflow when starting work on an unfamiliar connector:

```bash
# 1. Inspect and read docs
airbyte-agent connectors inspect --json '{"workspace": "default", "name": "<connector>"}'
airbyte-agent skills docs --json '{"id": "<docs_skill_id from inspect>"}' --fields data.markdown
airbyte-agent skills docs --json '{"id": "<docs_skill_id from inspect>", "section": "<exact-section-id>"}' --fields data.markdown

# 2. Now compose execute, knowing the contract
airbyte-agent connectors execute --json '{
  "workspace": "default",
  "name": "<connector>",
  "entity": "<an-entity-from-skills-docs>",
  "action": "<an-action-from-skills-docs>",
  "select_fields": ["<field-from-skills-docs>", "..."],
  "params": { ... per the params docs ... }
}'
```

If `execute` returns `validation_error` on `entity` or `action`, you guessed or read the wrong section — inspect, read the exact docs section, and retry with the real names.

## Response structure

```jsonc
// list / api_search / context_store_search
{ "data": [ ... ], "meta": { "has_more": true } }

// get — returns the entity directly, no envelope
{ "id": "...", ... }
```

To paginate, pass `cursor=<last_cursor_value>` in `params` while `has_more` is true.

> [!IMPORTANT]
> **Read the full response. Never truncate.** Run `execute` directly and read the entire stdout as one result. Do NOT pipe through `head`, `tail`, `sed`, `awk`, `cut`, `wc`, or any other tool that drops bytes, and do NOT cap output with `--fields` solely to make it shorter. If the response is large, that *is* the answer — narrow the query (`select_fields`, tighter filters, smaller `limit`) at the source rather than slicing the output after the fact. Truncated output silently hides records, `has_more`, errors, and pagination cursors.

## How to use `context_store_search`

`action=context_store_search` reads `params.query` with `filter`, `sort`, and `limit`:

```jsonc
// Basic filter
{"action": "context_store_search", "params": {"limit": 20, "query": {"filter": {"eq": {"status": "active"}}}}}

// Filter + sort
{"action": "context_store_search", "params": {"limit": 20, "query": {"filter": {...}, "sort": [{"created": "desc"}]}}}
```

**Always prefer `fuzzy` over `like` when searching for text.** `fuzzy` matches words in any order, ignores punctuation/casing, and handles partial names. `like` requires an exact substring match and fails on typos or word reordering. Only fall back to `like` when you need exact substring matching (e.g. prefix search on IDs).

```jsonc
// Find a user by name — use fuzzy
"params": {"query": {"filter": {"fuzzy": {"firstName": "Teo"}}}}

// Find an external ID with a known prefix — use like
"params": {"query": {"filter": {"like": {"externalId": "CUS-"}}}}
```

## Filter operators

The operator is the **outer key**; `field: value` is nested inside. All examples below go inside `params.query.filter`:

| Operator | Meaning | Example |
|---|---|---|
| `eq` | Exact match | `{"eq": {"status": "completed"}}` |
| `neq` | Not equal | `{"neq": {"status": "deleted"}}` |
| `gt` / `gte` | Greater / greater-or-equal | `{"gte": {"started": "2026-01-01T00:00:00Z"}}` |
| `lt` / `lte` | Less / less-or-equal | `{"lt": {"amount": 1000}}` |
| `in` | Set membership | `{"in": {"stage": ["discovery", "negotiation"]}}` |
| `like` | Substring (exact) | `{"like": {"externalId": "CUS-"}}` |
| `fuzzy` | Fuzzy text match | `{"fuzzy": {"name": "john smith"}}` |
| `keyword`, `contains`, `any` | Provider-specific | see connector docs |

**Combining filters (AND):** put multiple operator keys in the same filter object.

```jsonc
{"filter": {"gte": {"started": "2026-01-01T00:00:00Z"}, "eq": {"status": "completed"}}}
```

**Composing with logical operators:**

```jsonc
{"filter": {"and": [cond1, cond2]}}
{"filter": {"or":  [cond1, cond2]}}
{"filter": {"not": cond}}
```

## ID resolution (filtering by related entity)

When filtering by a related entity (a person, team, project, account…), foreign keys are **not always named `id`**. Look for fields whose name or description indicates a link to another entity: `ownerId`, `accountId`, `assignee_id`, `project_key`, etc. Workflow:

1. Run `connectors inspect`, then `skills docs`, to see entity schemas.
2. Identify the foreign-key field that links the entities you care about.
3. Search the related entity by name to get its primary key.
4. Use that key in the filter.

Example — find deals owned by a user named "Teo":

```bash
# 1. Find Teo's id in the users entity
airbyte-agent connectors execute --json '{
  "name": "hubspot",
  "entity": "users",
  "action": "context_store_search",
  "select_fields": ["id", "firstName"],
  "params": {"query": {"filter": {"fuzzy": {"firstName": "Teo"}}}}
}'

# 2. Use that id as the foreign key on deals
airbyte-agent connectors execute --json '{
  "name": "hubspot",
  "entity": "deals",
  "action": "context_store_search",
  "select_fields": ["id", "name", "amount", "ownerId"],
  "params": {"query": {"filter": {"eq": {"ownerId": "<teo-id>"}}}}
}'
```

## Pagination

- **Default `limit`: 20–25.** Don't paginate unless the user explicitly asks for "all".
- For *"how many"*-style questions with `has_more=true`, answer **"at least N"** rather than counting through every page.
- **Hard stop at 3 pages.** If you'd need more, narrow the filter instead.
- Pagination is cursor-based: read `cursor` from the response (or `meta.next_cursor`, varies per connector) and pass it back as `params.cursor` on the next call while `has_more` remains true.

## Date ranges including today

Search indices can lag the source by hours. When a date range **includes today**, issue **both** a `context_store_search` and a `list` with date params — in the same agent turn — then merge results and deduplicate by `id`. If the date range ends *before* today, `context_store_search` alone is sufficient.

Always resolve relative date phrases ("today", "yesterday", "this week") to **explicit absolute timestamps** (ISO 8601, UTC) and tell the user which range you used.

## Field selection (mandatory)

Two complementary mechanisms — use **both** when you know the fields you need:

- **`select_fields` / `exclude_fields` (API-side, inside the JSON payload)** — passed to the source connector to reduce upstream work and bandwidth. Dot-notation for nested fields supported. If both are passed, `select_fields` wins.
- **`--fields` (CLI-side, global flag)** — shapes the JSON the CLI prints to stdout, after the API responds.

```bash
airbyte-agent connectors execute --fields data.id,data.email,meta.has_more --json '{
  "workspace": "default",
  "name": "hubspot",
  "entity": "contacts",
  "action": "context_store_search",
  "select_fields": ["id", "email", "firstName"],
  "params": {"limit": 20, "query": {"filter": {"eq": {"lifecyclestage": "customer"}}}}
}'
```

`--fields` (CLI) auto-broadcasts row-level paths through the `data` wrapper, so `--fields id,email` is equivalent to `--fields data.id,data.email` *unless* you also want top-level fields like `meta`/`next` — then use the explicit dotted form for the row paths.

## Write actions (`create`, `update`)

> [!IMPORTANT]
> **Write failure handling.** If a write call returns an error or indicates the target was unreachable, do NOT retry with a different target identifier (channel, recipient, conversation, repository, record, etc.). Surface the failure to the caller and let them decide. Silently substituting a destination is forbidden — return the failure instead of completing the work against a different target.

## Local execution mode (opt-in)

By default `execute` runs **hosted** — Airbyte performs the connector request. **Local mode** is an opt-in path in which the CLI performs the request itself and reads the connector's secrets from the user's **own AWS Secrets Manager**. The JSON payload and the result you read are the same as hosted; only *where* the request runs changes.

**When to use it:** only when the user explicitly wants the connector request to run from their machine / their AWS account (e.g. data residency, network egress, or their own audit boundary). Otherwise leave it hosted.

Enable it with the global flag or env var — these are **global** (they precede `connectors execute`) and compose with `--json`:

```bash
# Preferred: env var. AWS auth comes from the user's environment.
AIRBYTE_EXECUTION_MODE=local \
  airbyte-agent connectors execute --json '{"name":"hubspot","entity":"contacts","action":"list","select_fields":["id","email"]}'

# Or select an AWS profile the user has already logged into.
airbyte-agent --execution-mode local --aws-profile production --aws-region us-east-1 \
  connectors execute --json '{"name":"hubspot","entity":"contacts","action":"list","select_fields":["id","email"]}'
```

> [!IMPORTANT]
> **You never handle secrets.** You may set `--execution-mode`, `--aws-profile`, `--aws-region`. You must NEVER request, accept, or pass AWS secret-key material or any connector secret — there are no secret-bearing flags by design. If AWS auth is missing, ask the user to run `aws sso login --profile <name>`; do not try to supply keys.

**Fail-closed:** local execution **never falls back to hosted**. If it can't run, the command errors — to run hosted, re-invoke *without* `--execution-mode local`.

**Supported in local mode:** OpenAPI-3 connectors; `list`/`get`/`create`/`update`/`delete`/`api_search`/`authorize`; REST + GraphQL; API-key/bearer/HTTP-basic/**static** OAuth2 auth. **Not supported** (→ `local_execution_unsupported`, exit 4): refreshable OAuth, `download` (binary), context-store actions (`context_store_search`), `describe`, and advanced JSONPath. Because `context_store_search` is unsupported locally, prefer `list` for reads in local mode.

## Error recovery

| Error | Likely cause | Fix |
|---|---|---|
| `not_found` (exit 3) on connector | Name not found | Run `connectors list` to see exact names. The CLI matches against connector instance name, template display name, AND template slug, case-insensitively — so any of those works. |
| `validation_error` (exit 4) on entity/action | Guessed entity/action name | Run `connectors inspect`, then `skills docs`, to enumerate supported entities and actions. |
| Ambiguous name (exit 4) | Two connectors share a name | Pass `"id": "<uuid>"` in the JSON payload instead of `"name"`. |
| `auth_error` (exit 2) | Credentials invalid or expired | Re-run `airbyte-agent login` to refresh credentials. |
| Empty `data: []` from `context_store_search` | Index lag, or filter too narrow | Retry with `"action": "list"` (live source). If still empty, broaden the filter. |

### Local-mode errors (fail-closed — never retried on the hosted API)

| Error | Likely cause | Fix |
|---|---|---|
| `local_execution_unsupported` (exit 4) | Connector/action uses a feature local mode can't run (refreshable OAuth, `download`, context-store, `describe`, advanced JSONPath) | Re-run **without** `--execution-mode local` to execute on the hosted API. |
| `validation_error` (exit 4) on AWS config | Invalid `--execution-mode`, incomplete `AWS_SECRET_MANAGER_*` pair, or missing region | Fix the flag/env combo: static AWS creds are all-or-nothing, and a region is required (`--aws-region` / `AWS_SECRET_MANAGER_REGION` / profile). |
| `secret_manager_authentication_error` (exit 2) | Missing/expired cached SSO session or no AWS credentials | Ask the user to run `aws sso login --profile <name>`, then retry. Never supply AWS keys yourself. |
| `secret_manager_access_error` (exit 2) | AWS access denied / KMS denied | The IAM role needs `secretsmanager:GetSecretValue` (and `kms:Decrypt` for a customer-managed key) on the secret ARN. Ask the user to widen the policy. |
| `secret_not_found` (exit 3) | The referenced secret doesn't exist in Secrets Manager | Surface to the user — the connector's secret coordinate points at a secret their AWS account doesn't have. Do not guess another secret. |
| `secret_hydration_error` (exit 1) | Binary/non-scalar secret value, bad coordinate, or provider failure | Surface to the user; the stored secret value or coordinate is malformed for local hydration. |
| `connector_execution_error` (exit 1) | DNS/TLS/timeout/redirect/non-2xx from the connector origin | Same as a hosted request failure — inspect the sanitized message; retry only if transient. |

## Do NOT

- Do NOT call `execute` without `select_fields` or `exclude_fields`. Field selection is mandatory.
- Do NOT use `like` when `fuzzy` would do — `like` fails on word reordering and typos.
- Do NOT guess entity, action, or param names. Run `connectors inspect`, then `skills docs`, first — docs are the source of truth for what a specific connector supports.
- Do NOT pass credentials in the `execute` payload — credentials live on the connector and are set via `connectors create`.
- Do NOT pass AWS keys or any connector secret when using `--execution-mode local`. There are no secret-bearing flags; AWS auth comes from the user's environment (`aws sso login --profile <name>`).
- Do NOT retry a local-mode failure on the hosted API automatically. Local is fail-closed; only re-run hosted if the user asks.
- Do NOT paginate beyond 3 pages — narrow the filter instead.
- Do NOT pass relative dates ("today", "last week") — resolve to absolute ISO 8601 timestamps and report the range to the user.
- Do NOT silently retry write failures against a different target.
- Do NOT truncate the `execute` response or pipe it through `head`/`tail`/`sed`/`awk`/`cut`/`wc` — read the full output. If it's too large, narrow the query (`select_fields`, filters, `limit`); don't slice the result.
