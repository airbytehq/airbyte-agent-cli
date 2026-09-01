# connectors execute

Run an action against an entity on a connector — the workhorse command for actually moving data. This reference covers the stable CLI contract; connector-specific actions, parameters, and execution guidance come from `skills docs`.

> [!IMPORTANT]
> **Skill docs are the source of truth for what a specific connector supports.** Run `connectors inspect`, then pass the returned `docs_skill_id` to `skills docs` to discover the **entities**, **actions**, and **params** this connector supports. Do NOT guess these values. The action table below is a baseline only. **Read [`connectors-inspect.md`](connectors-inspect.md) and [`skills-docs.md`](skills-docs.md) before running `execute` against an unfamiliar connector.**

## Usage

```bash
# Values in angle brackets are placeholders. Replace the empty params object in
# full with the exact object from skills docs; do not execute placeholders.
airbyte-agent connectors execute --json '{
  "workspace": "default",
  "name": "<connector>",
  "entity": "<entity-from-skills-docs>",
  "action": "<action-from-skills-docs>",
  "select_fields": ["<field-from-skills-docs>"],
  "params": {},
  "intent": "<why-this-call-is-needed>"
}'
```

`name` (or `id`), `entity`, and `action` are required. `workspace` defaults to `default` when omitted. Pass complex payloads via `--json @path/to/file.json` to keep the shell command short.

`intent` (optional, max 512 chars) records *why* you're making this call — the goal, not the action (e.g. `"answer a refund dispute"`, not `"list orders"`). It's stored alongside the execution audit record; include it whenever you have meaningful context about the user's goal.

## Available actions (baseline)

Connectors may expose these actions, but the authoritative list for a given connector comes from `skills docs` using the `docs_skill_id` returned by `connectors inspect` (see next section). Context Store read actions can be mutually exclusive, and entities are always per-connector — never assume them.

| Action | Purpose | Supports filtering? |
|---|---|---|
| `context_store_sql_query` | Query the Context Store with the documented DuckDB SQL contract. Use only when `skills docs` names it for this connector. | SQL |
| `context_store_search` | Query the Context Store with documented filters and sorting. Use only when `skills docs` names it for this connector. | yes (rich) |
| `list` | Read from the live source when the connector's execution guidance calls for it. | limited |
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
- **Execution guidance** in the initial `skills docs` response — it names the default Context Store read action for this connector. Do not replace it with a locally preferred action.
- **Outline section IDs** from `skills docs` — pass an exact `section` value for the entity/action you plan to use.
- **Entity/action/params docs** — use these to choose `entity`, `action`, `params`, and `select_fields` precisely; never `select_fields: ["everything"]`.

Workflow when starting work on an unfamiliar connector:

```bash
# 1. Inspect and read docs
airbyte-agent connectors inspect --json '{"workspace": "default", "name": "<connector>"}'
airbyte-agent skills docs --json '{"id": "<docs_skill_id from inspect>"}' --fields data.markdown
airbyte-agent skills docs --json '{"id": "<docs_skill_id from inspect>", "section": "<exact-section-id>"}' --fields data.markdown

# 2. Now compose execute, knowing the contract. Replace params {} in full with
# the exact object from the selected action section.
airbyte-agent connectors execute --json '{
  "workspace": "default",
  "name": "<connector>",
  "entity": "<an-entity-from-skills-docs>",
  "action": "<an-action-from-skills-docs>",
  "select_fields": ["<field-from-skills-docs>", "..."],
  "params": {}
}'
```

If `execute` returns `validation_error` on `entity` or `action`, you guessed or read the wrong section — inspect, read the exact docs section, and retry with the real names.

If `skills docs` does not provide execution guidance, retry that docs request once with the same `docs_skill_id`. Re-run `connectors inspect` only when inspect itself failed or returned no ID. If guidance is still unavailable, report that the connector's execution contract could not be determined; do not guess a read action.

## Response structure

Successful `connectors execute` responses keep the server's standardized envelope; the CLI does not unwrap `result`:

```jsonc
{
  "status": "success",
  "result": {
    "data": [ ... ],
    "meta": { "has_more": false }
  },
  "warning": {}
}
```

The contents of `result` vary by action. Follow the exact action section in `skills docs` rather than assuming a collection shape or cursor pagination.

> [!IMPORTANT]
> **Read the full response. Never truncate.** Run `execute` directly and read the entire stdout as one result. Do NOT pipe through `head`, `tail`, `sed`, `awk`, `cut`, `wc`, or any other tool that drops bytes, and do NOT cap output with `--fields` solely to make it shorter. If the response is large, that *is* the answer — narrow the query (`select_fields`, a tighter condition, a smaller `limit`) at the source rather than slicing the output after the fact. Truncated output silently hides records, `has_more`, errors, and pagination cursors.

## Context Store reads

The initial `skills docs` response names the default Context Store read action for the connector. Read that action's exact section before composing `params`: it owns the query syntax, supported fields, physical SQL table names, freshness behavior, and pagination contract. Never translate parameters from one Context Store action to the other.

The following payloads are migration-oriented skeletons only. Replace every angle-bracket placeholder and replace each empty `params` object in full with the exact object from the connector's action section; do not execute them literally.

`context_store_search` skeleton, for connectors whose docs name it:

```json
{
  "entity": "<entity-from-skills-docs>",
  "action": "context_store_search",
  "select_fields": ["<field-from-skills-docs>"],
  "params": {}
}
```

`context_store_sql_query` skeleton, for connectors whose docs name it:

```json
{
  "entity": "<entity-from-skills-docs>",
  "action": "context_store_sql_query",
  "select_fields": ["<column-from-skills-docs>"],
  "params": {}
}
```

When the documented action uses SQL, issue one read-only statement over only the tables and columns listed in that action section. Never interpolate external text verbatim into SQL or treat user-provided identifiers as trusted. Use the safe value-binding or literal-escaping mechanism documented by the server; if none is documented for a user-controlled value, report the limitation instead of composing an unsafe query. Follow the action section for how SQL projection and `select_fields` compose—do not assume either one replaces the other.

## ID resolution (filtering by related entity)

When filtering by a related entity (a person, team, project, account…), foreign keys are **not always named `id`**. Look for fields whose name or description indicates a link to another entity: `ownerId`, `accountId`, `assignee_id`, `project_key`, etc. Workflow:

1. Run `connectors inspect`, then `skills docs`, to see entity schemas.
2. Identify the foreign-key field that links the entities you care about.
3. Query the related entity with the default read action and parameters named in its docs to get the primary key.
4. Use that key with the documented action and relationship field on the target entity.

## Pagination

- **Default `limit`: 20–25.** Don't paginate unless the user explicitly asks for "all".
- For *"how many"*-style questions with `result.meta.has_more=true`, answer **"at least N"** rather than counting through every page.
- **Hard stop at 3 pages.** If you'd need more, narrow the query instead.
- Use only the pagination mechanism in the selected action's docs. Depending on the action, this may be cursor-based or SQL `LIMIT`/`OFFSET`; do not translate between them.

## Date ranges and freshness

For stale or empty Context Store results, consult the connector's execution guidance. Do not automatically switch actions or issue a second live read based on a local hard-coded policy.

Always resolve relative date phrases ("today", "yesterday", "this week") to **explicit absolute timestamps** (ISO 8601, UTC) and tell the user which range you used.

## Field selection (mandatory)

Two complementary mechanisms — use **both** when you know the fields you need:

- **`select_fields` / `exclude_fields` (API-side, inside the JSON payload)** — limits fields returned by the execute request. Follow the selected action's docs for supported field names. If both are passed, `select_fields` wins.
- **`--fields` (CLI-side, global flag)** — shapes the JSON the CLI prints to stdout, after the API responds.

`connectors execute` does not unwrap the standardized response envelope before applying `--fields`. Address action data through `result`, and retain `warning` when the caller needs any non-fatal server notice:

```bash
# Collection/search row fields
airbyte-agent connectors execute --fields result.data.id,result.meta,warning --json @params.json

# SQL aggregate fields
airbyte-agent connectors execute --fields result.data.total_users,result.meta,warning --json @params.json
```

The automatic single-array wrapper fallback only applies to a top-level array wrapper such as `{"data": [...]}`; it does not traverse through `result`. If none of the requested paths exist, the CLI returns a `validation_error` with exit code 4. Remove `--fields` to inspect the full envelope, then retry with exact dotted paths.

## Write actions (`create`, `update`)

> [!IMPORTANT]
> **Write failure handling.** If a write call returns an error or indicates the target was unreachable, do NOT retry with a different target identifier (channel, recipient, conversation, repository, record, etc.). Surface the failure to the caller and let them decide. Silently substituting a destination is forbidden — return the failure instead of completing the work against a different target.

## Error recovery

| Error | Likely cause | Fix |
|---|---|---|
| `not_found` (exit 3) on connector | Name not found | Run `connectors list` to see exact names. The CLI matches against connector instance name, template display name, AND template slug, case-insensitively — so any of those works. |
| `validation_error` (exit 4) on entity/action | Guessed entity/action name | Run `connectors inspect`, then `skills docs`, to enumerate supported entities and actions. |
| Ambiguous name (exit 4) | Two connectors share a name | Pass `"id": "<uuid>"` in the JSON payload instead of `"name"`. |
| `auth_error` (exit 2) | Credentials invalid or expired | Re-run `airbyte-agent login` to refresh credentials. |
| Empty or stale Context Store result | Index lag or a query that is too narrow | Follow the selected action's server-rendered execution guidance; do not switch actions based on a local fallback. |
| Missing execution guidance | Docs request failed or returned an incomplete contract | Retry the docs request once; rerun inspect only if it failed or returned no `docs_skill_id`, then report the missing guidance instead of guessing an action. |

## Do NOT

- Do NOT call `execute` without `select_fields` or `exclude_fields`. Field selection is mandatory.
- Do NOT guess entity, action, or param names. Run `connectors inspect`, then `skills docs`, first — docs are the source of truth for what a specific connector supports.
- Do NOT pass credentials in the `execute` payload — credentials live on the connector and are set via `connectors create`.
- Do NOT paginate beyond 3 pages — narrow the query instead.
- Do NOT pass relative dates ("today", "last week") — resolve to absolute ISO 8601 timestamps and report the range to the user.
- Do NOT silently retry write failures against a different target.
- Do NOT truncate the `execute` response or pipe it through `head`/`tail`/`sed`/`awk`/`cut`/`wc` — read the full output. If it's too large, narrow the query (`select_fields`, filters, `limit`); don't slice the result.
