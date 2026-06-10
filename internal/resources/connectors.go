package resources

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/airbytehq/airbyte-agent-cli/internal/client"
	"github.com/airbytehq/airbyte-agent-cli/internal/registry"
)

type connectorsResource struct{}

func (cr *connectorsResource) Name() string        { return "connectors" }
func (cr *connectorsResource) Description() string { return "Create, manage, and execute connectors" }
func (cr *connectorsResource) Operations() []registry.Operation {
	return []registry.Operation{
		connectorsCreateOperation(),
		connectorsUpdateOperation(),
		{
			Name:        "list",
			Description: "List connectors in a workspace",
			Schema: registry.OperationSchema{
				Description: "List all connectors for a workspace. If 'workspace' is omitted, falls back to 'default'.",
				Params: map[string]registry.ParamSchema{
					"workspace": {Type: "string", Required: false, Description: "Workspace name (defaults to 'default' when omitted)"},
				},
			},
			SpecRef: registry.SpecRef{Path: "/api/v1/integrations/connectors", Method: "GET"},
			Run:     connectorsList,
		},
		{
			Name:        "list-available",
			Description: "List available connector templates",
			Schema: registry.OperationSchema{
				Description: "List all available source connector templates",
				Params:      map[string]registry.ParamSchema{},
			},
			SpecRef: registry.SpecRef{Path: "/api/v1/integrations/templates/sources", Method: "GET"},
			Run:     connectorsListAvailable,
		},
		{
			Name:        "describe",
			Description: "Describe a connector's schema (legacy — prefer 'connectors inspect' + 'skills docs')",
			Schema: registry.OperationSchema{
				Description: "Get connector details and schema description. Legacy compatibility command; prefer 'connectors inspect' plus 'skills docs' for new workflows.",
				Params: map[string]registry.ParamSchema{
					"name":      {Type: "string", Required: false, Description: "Connector name (requires workspace)"},
					"workspace": {Type: "string", Required: false, Description: "Workspace name (defaults to 'default' when used with name)"},
					"id":        {Type: "string", Required: false, Description: "Connector ID (alternative to name)"},
				},
			},
			SpecRef: registry.SpecRef{Path: "/api/v1/integrations/connectors/{id}", Method: "GET"},
			Run:     connectorsDescribe,
			Hooks: registry.OperationHooks{
				PreRun: resolveConnectorID,
			},
		},
		{
			Name:        "inspect",
			Description: "Inspect connector metadata and docs readiness",
			Schema: registry.OperationSchema{
				Description: "Get connector metadata, readiness warnings, and docs_skill_id for usage documentation",
				Params: map[string]registry.ParamSchema{
					"name":      {Type: "string", Required: false, Description: "Connector name (requires workspace)"},
					"workspace": {Type: "string", Required: false, Description: "Workspace name (defaults to 'default' when used with name)"},
					"id":        {Type: "string", Required: false, Description: "Connector ID (alternative to name)"},
				},
			},
			Run: connectorsInspect,
			Hooks: registry.OperationHooks{
				PreRun: resolveConnectorID,
			},
			SpecRef: registry.SpecRef{Path: "/api/v1/integrations/connectors/{id}/inspect", Method: "GET"},
		},
		{
			Name:        "execute",
			Description: "Execute a connector action",
			Schema: registry.OperationSchema{
				Description: "Execute an action on a connector",
				Params: map[string]registry.ParamSchema{
					"name":            {Type: "string", Required: false, Description: "Connector name (requires workspace)"},
					"workspace":       {Type: "string", Required: false, Description: "Workspace name (required when using name)"},
					"id":              {Type: "string", Required: false, Description: "Connector ID (alternative to name)"},
					"entity":          {Type: "string", Required: true, Description: "Entity name"},
					"action":          {Type: "string", Required: true, Description: "Action name"},
					"params":          {Type: "object", Required: false, Description: "Action parameters"},
					"select_fields":   {Type: "array", Required: false, Description: "Fields to include in response"},
					"exclude_fields":  {Type: "array", Required: false, Description: "Fields to exclude from response"},
					"skip_truncation": {Type: "boolean", Required: false, Description: "Disable automatic truncation of long text fields in list/search responses"},
					"intent":          {Type: "string", Required: false, Description: "Optional short reason for this call, recorded with the execution audit (max 512 chars)"},
				},
			},
			SpecRef: registry.SpecRef{Path: "/api/v1/integrations/connectors/{id}/execute", Method: "POST"},
			Run:     connectorsExecute,
			Hooks: registry.OperationHooks{
				PreRun: resolveConnectorID,
			},
		},
		{
			Name:        "delete",
			Description: "Delete a connector",
			Schema: registry.OperationSchema{
				Description: "Delete a connector by name or ID",
				Params: map[string]registry.ParamSchema{
					"name":      {Type: "string", Required: false, Description: "Connector name (requires workspace)"},
					"workspace": {Type: "string", Required: false, Description: "Workspace name (defaults to 'default' when used with name)"},
					"id":        {Type: "string", Required: false, Description: "Connector ID (alternative to name)"},
				},
			},
			SpecRef: registry.SpecRef{Path: "/api/v1/integrations/connectors/{id}", Method: "DELETE"},
			Run:     connectorsDelete,
			Hooks: registry.OperationHooks{
				PreRun: resolveConnectorID,
			},
		},
	}
}

func resolveConnectorID(ctx context.Context, c *client.Client, params map[string]any) (map[string]any, error) {
	id, hasID := params["id"].(string)
	name, hasName := params["name"].(string)
	hasID = hasID && id != ""
	hasName = hasName && name != ""

	if hasID && hasName {
		return nil, client.NewValidationError(
			"provide either 'id' or 'name', not both",
			"provide only one of 'id' or 'name'",
		)
	}
	if !hasID && !hasName {
		return nil, client.NewValidationError(
			"either 'name' + 'workspace' or 'id' is required",
			"run 'airbyte-agent connectors list --json '{\"workspace\": \"...\"}'' to find connector names, or use --id with a connector ID",
		)
	}

	if hasID {
		return params, nil
	}

	workspaceName := applyDefaultWorkspace(c, params)

	raw, err := c.Get(ctx, "/api/v1/integrations/connectors", map[string]string{
		"workspace_name": workspaceName,
	})
	if err != nil {
		return nil, err
	}

	var resp connectorLookupResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parsing connectors list: %w", err)
	}

	// Accept matches against the connector instance name, the template's
	// display name, OR the template's slug — users may type any of these.
	// Deduplicate so a single connector matched by multiple fields counts
	// once.
	seen := map[string]bool{}
	var matches []string
	for _, conn := range resp.Data {
		candidates := []string{
			conn.Name,
			conn.SummarizedSourceTemplate.Name,
			conn.SummarizedSourceTemplate.ConnectorName,
		}
		for _, candidate := range candidates {
			if candidate != "" && strings.EqualFold(candidate, name) {
				if !seen[conn.ID] {
					matches = append(matches, conn.ID)
					seen[conn.ID] = true
				}
				break
			}
		}
	}

	switch len(matches) {
	case 0:
		return nil, client.NewNotFoundError(
			fmt.Sprintf("connector %q not found in workspace %q", name, workspaceName),
			fmt.Sprintf("run 'airbyte-agent connectors list --json '{\"workspace\": \"%s\"}'' to see available connectors", workspaceName),
		)
	case 1:
		params["id"] = matches[0]
		return params, nil
	default:
		return nil, client.NewValidationError(
			fmt.Sprintf("ambiguous: %d connectors named %q in workspace %q", len(matches), name, workspaceName),
			"use 'id' instead to target a specific connector",
		)
	}
}

// fallbackWorkspaceName is the last-resort default applied when neither
// the caller nor the user's settings.json supply a workspace. It matches
// the API's own default-workspace convention for new accounts.
const fallbackWorkspaceName = "default"

// statusWriter is the stream where the connectors resource prints user-facing
// status messages (e.g. the workspace fallback notice). Tests override it to
// capture output without touching os.Stderr.
var statusWriter io.Writer = os.Stderr

// applyDefaultWorkspace resolves params["workspace"], falling back to the
// user's configured default (from ~/.airbyte-agent/settings.json, exposed on the
// client) and ultimately to the literal "default" if neither is set. When
// the fallback engages, a JSON notice is printed to stderr so users can see
// which workspace was actually used.
func applyDefaultWorkspace(c *client.Client, params map[string]any) string {
	name, _ := params["workspace"].(string)
	if name != "" {
		return name
	}
	resolved := configuredDefaultWorkspace(c)
	notice, _ := json.Marshal(map[string]string{
		"message":   fmt.Sprintf("no workspace provided; falling back to %q", resolved),
		"workspace": resolved,
	})
	fmt.Fprintln(statusWriter, string(notice))
	params["workspace"] = resolved
	return resolved
}

func configuredDefaultWorkspace(c *client.Client) string {
	if c != nil {
		if name := c.DefaultWorkspace(); name != "" {
			return name
		}
	}
	return fallbackWorkspaceName
}

// credentialsPageSize is the per-page limit passed to the org-scoped
// credentials list endpoint. 200 is large enough that most accounts get
// everything in a single request while keeping each response payload
// manageable.
const credentialsPageSize = 200

// credentialsMaxPages caps how many pages fetchAllCredentials will request
// before giving up. At credentialsPageSize=200 this is a 10k-credential
// ceiling — far above any plausible org size, so hitting it indicates a
// pagination bug (e.g. the server stops decreasing the remaining count)
// rather than a legitimate large account.
const credentialsMaxPages = 50

// fetchAllCredentials returns every credential in the caller's organization,
// keyed by credential ID. It paginates GET
// /api/v1/organizations/{organization_id}/credentials at limit=200 until a
// short page (or empty page) is returned. Any non-2xx response from the API
// or JSON decode failure is returned unwrapped — callers decide how to
// surface partial fetch errors. The pagination loop is capped at
// credentialsMaxPages defensively.
//
// This helper is pure data fetching: it does not mutate caller state and
// does not feed into connectorsList's response handling.
func fetchAllCredentials(ctx context.Context, c *client.Client) (map[string]credentialsListItem, error) {
	orgID := c.OrganizationID()
	path := "/api/v1/organizations/" + url.PathEscape(orgID) + "/credentials"

	out := map[string]credentialsListItem{}
	offset := 0
	for page := 0; page < credentialsMaxPages; page++ {
		raw, err := c.Get(ctx, path, map[string]string{
			"limit":  strconv.Itoa(credentialsPageSize),
			"offset": strconv.Itoa(offset),
		})
		if err != nil {
			return nil, err
		}

		var resp credentialsListResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("parsing credentials list: %w", err)
		}

		for _, item := range resp.Data {
			out[item.ID] = item
		}

		// Natural stop: a short page (including empty) means we've drained
		// the server's data. The `total` field is informational only — the
		// page length is the authoritative signal that no more data exists.
		if len(resp.Data) < credentialsPageSize {
			return out, nil
		}

		offset += credentialsPageSize
	}

	return nil, fmt.Errorf("fetchAllCredentials: pagination exceeded %d pages — likely a server pagination bug", credentialsMaxPages)
}

// connectorsList fans out the connectors-list and credentials-list calls in
// parallel, then merges `context_store_status` + `context_store_entity_count`
// into each connector item by `id`. The credentials call is best-effort: on
// error, a JSON notice is written to statusWriter and the merge produces
// `null` / `0` on every item instead of failing the whole request. This
// keeps the primary listing usable even when the org credentials endpoint is
// degraded.
//
// The parallel fan-out mirrors connectorsDescribe: each call runs in its
// own goroutine, both report through a buffered channel, and a WaitGroup
// joins before we read either result.
func connectorsList(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
	workspaceName := applyDefaultWorkspace(c, params)

	type connectorsResult struct {
		data json.RawMessage
		err  error
	}
	type credentialsResult struct {
		data map[string]credentialsListItem
		err  error
	}

	var wg sync.WaitGroup
	connectorsCh := make(chan connectorsResult, 1)
	credentialsCh := make(chan credentialsResult, 1)

	wg.Add(2)
	go func() {
		defer wg.Done()
		raw, err := c.Get(ctx, "/api/v1/integrations/connectors", map[string]string{
			"workspace_name": workspaceName,
		})
		connectorsCh <- connectorsResult{raw, err}
	}()
	go func() {
		defer wg.Done()
		creds, err := fetchAllCredentials(ctx, c)
		credentialsCh <- credentialsResult{creds, err}
	}()
	wg.Wait()

	connRes := <-connectorsCh
	credRes := <-credentialsCh

	if connRes.err != nil {
		return nil, connRes.err
	}

	// Soft-fail on credentials error: emit a stderr notice and use an empty
	// map so the merge loop runs and stamps null/0 on every item.
	creds := credRes.data
	if credRes.err != nil {
		notice, _ := json.Marshal(map[string]any{
			"message":              fmt.Sprintf("context store status unavailable: %v", credRes.err),
			"context_store_status": nil,
		})
		fmt.Fprintln(statusWriter, string(notice))
		creds = map[string]credentialsListItem{}
	}

	// Defensive decode: if the envelope is missing `data` or is not the
	// expected shape, return the raw response untouched plus a stderr
	// notice. This mirrors the organizations.go envelope handling and
	// keeps the command resilient to unexpected upstream payloads.
	var connectors map[string]any
	if err := json.Unmarshal(connRes.data, &connectors); err != nil {
		notice, _ := json.Marshal(map[string]any{
			"message":              fmt.Sprintf("context store status unavailable: connectors response not a JSON object: %v", err),
			"context_store_status": nil,
		})
		fmt.Fprintln(statusWriter, string(notice))
		return connRes.data, nil
	}

	data, ok := connectors["data"].([]any)
	if !ok {
		notice, _ := json.Marshal(map[string]any{
			"message":              "context store status unavailable: connectors response missing data array",
			"context_store_status": nil,
		})
		fmt.Fprintln(statusWriter, string(notice))
		return connectors, nil
	}

	for _, item := range data {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := entry["id"].(string)
		cred, hasCred := creds[id]
		if hasCred {
			// ContextStoreStatus is a *string — nil serializes as JSON null,
			// which is exactly what we want when the upstream reports null.
			entry["context_store_status"] = cred.ContextStoreStatus
			entry["context_store_entity_count"] = cred.ContextStoreEntityCount
		} else {
			entry["context_store_status"] = nil
			entry["context_store_entity_count"] = 0
		}
	}

	return connectors, nil
}

func connectorsListAvailable(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
	raw, err := c.Get(ctx, "/api/v1/integrations/templates/sources/global", nil)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func connectorsDescribe(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
	id, _ := params["id"].(string)
	path := connectorPath(id)
	execPath := path + "/execute"

	type result struct {
		data json.RawMessage
		err  error
	}

	var wg sync.WaitGroup
	getCh := make(chan result, 1)
	describeCh := make(chan result, 1)

	wg.Add(2)
	go func() {
		defer wg.Done()
		raw, err := c.Get(ctx, path, nil)
		getCh <- result{raw, err}
	}()
	go func() {
		defer wg.Done()
		raw, err := c.Post(ctx, execPath, map[string]string{
			"entity": "",
			"action": "describe",
		})
		describeCh <- result{raw, err}
	}()
	wg.Wait()

	getResult := <-getCh
	describeResult := <-describeCh

	if getResult.err != nil {
		return nil, getResult.err
	}

	var connector map[string]any
	if err := json.Unmarshal(getResult.data, &connector); err != nil {
		return nil, fmt.Errorf("parsing connector: %w", err)
	}

	if describeResult.err != nil {
		return nil, describeResult.err
	}

	var schema any
	if err := json.Unmarshal(describeResult.data, &schema); err != nil {
		return nil, fmt.Errorf("parsing connector schema: %w", err)
	}
	connector["schema"] = schema

	return connector, nil
}

func connectorsInspect(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
	id, _ := params["id"].(string)
	raw, err := c.Get(ctx, connectorPath(id)+"/inspect", nil)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func connectorsExecute(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
	id, _ := params["id"].(string)
	entity, _ := params["entity"].(string)
	action, _ := params["action"].(string)

	body := map[string]any{
		"entity": entity,
		"action": action,
	}
	if p, ok := params["params"]; ok {
		body["params"] = p
	}
	if sf, ok := params["select_fields"]; ok {
		body["select_fields"] = sf
	}
	if ef, ok := params["exclude_fields"]; ok {
		body["exclude_fields"] = ef
	}
	if st, ok := params["skip_truncation"]; ok {
		body["skip_truncation"] = st
	}
	if i, ok := params["intent"]; ok {
		body["intent"] = i
	}

	execPath := connectorPath(id) + "/execute"
	raw, err := c.Post(ctx, execPath, body)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func connectorsDelete(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
	id, _ := params["id"].(string)

	if !c.AllowDestructive() {
		if err := confirmDestructive(deletePromptFor(params)); err != nil {
			return nil, err
		}
	}

	path := connectorPath(id)
	raw, err := c.Delete(ctx, path)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// deletePromptFor crafts the user-facing description of the connector
// being deleted. Includes name + workspace when available (more readable
// than a bare UUID) and always includes the ID as the authoritative
// identifier.
func deletePromptFor(params map[string]any) string {
	id, _ := params["id"].(string)
	name, _ := params["name"].(string)
	workspace, _ := params["workspace"].(string)

	switch {
	case name != "" && workspace != "":
		return fmt.Sprintf("Delete connector %q (id %s) from workspace %q?", name, id, workspace)
	case name != "":
		return fmt.Sprintf("Delete connector %q (id %s)?", name, id)
	default:
		return fmt.Sprintf("Delete connector with id %s?", id)
	}
}

// confirmReader is the source of confirmation input. Tests swap this out
// to inject canned responses.
var confirmReader io.Reader = os.Stdin

// confirmWriter is where the confirmation prompt is printed. Defaults to
// stderr so stdout stays clean for JSON output.
var confirmWriter io.Writer = os.Stderr

// isTerminal reports whether stdin is connected to a TTY. Tests override
// it to simulate piped input.
var isTerminal = func() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// confirmDestructive is the package-level confirmation hook. It is a
// variable (not a function) so tests can replace the prompt+read logic
// wholesale without monkey-patching multiple primitives. The default
// implementation reads a line from confirmReader after writing prompt to
// confirmWriter, and only accepts an exact "yes" (case-insensitive,
// trimmed) — anything else is treated as a cancel.
var confirmDestructive = func(prompt string) error {
	if !isTerminal() {
		return client.NewValidationError(
			"destructive action requires confirmation but no TTY is available",
			"set \"allow_destructive\": true in ~/.airbyte-agent/settings.json (or AIRBYTE_ALLOW_DESTRUCTIVE=true) to allow non-interactive destructive operations",
		)
	}

	fmt.Fprintf(confirmWriter, "%s Type 'yes' to confirm: ", prompt)
	line, err := bufio.NewReader(confirmReader).ReadString('\n')
	if err != nil && err != io.EOF {
		return fmt.Errorf("reading confirmation: %w", err)
	}
	if strings.ToLower(strings.TrimSpace(line)) != "yes" {
		return client.NewValidationError(
			"destructive action cancelled by user",
			"re-run the command and type 'yes' to confirm, or set \"allow_destructive\": true in settings.json",
		)
	}
	return nil
}

func connectorPath(id string) string {
	return "/api/v1/integrations/connectors/" + url.PathEscape(id)
}
