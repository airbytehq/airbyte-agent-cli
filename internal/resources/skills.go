package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/airbytehq/airbyte-agent-cli/internal/client"
	"github.com/airbytehq/airbyte-agent-cli/internal/registry"
)

const (
	connectorSourceSkillPrefix = "connector-source:"
	defaultSkillsLimit         = 20
	maxWorkspaceLookupPages    = 100
)

type skillsResource struct{}

func (sr *skillsResource) Name() string        { return "skills" }
func (sr *skillsResource) Description() string { return "Discover and read connector and agent skill docs" }
func (sr *skillsResource) Operations() []registry.Operation {
	return []registry.Operation{
		{
			Name:        "list",
			Description: "List available skill docs",
			Schema: registry.OperationSchema{
				Description: "List connector and static skill docs for a workspace",
				Params: map[string]registry.ParamSchema{
					"workspace": {Type: "string", Required: false, Description: "Workspace name (defaults to 'default' when omitted)"},
					"limit":     {Type: "integer", Required: false, Description: "Max results to return", Default: defaultSkillsLimit},
					"cursor":    {Type: "string", Required: false, Description: "Opaque cursor from a previous response"},
				},
			},
			Run: skillsList,
		},
		{
			Name:        "search",
			Description: "Search skill docs",
			Schema: registry.OperationSchema{
				Description: "Search connector and static skill docs for a workspace",
				Params: map[string]registry.ParamSchema{
					"workspace": {Type: "string", Required: false, Description: "Workspace name (defaults to 'default' when omitted)"},
					"query":     {Type: "string", Required: true, Description: "Search query"},
					"limit":     {Type: "integer", Required: false, Description: "Max results to return", Default: defaultSkillsLimit},
					"cursor":    {Type: "string", Required: false, Description: "Opaque cursor from a previous response"},
				},
			},
			Run: skillsSearch,
		},
		{
			Name:        "docs",
			Description: "Read skill usage docs",
			Schema: registry.OperationSchema{
				Description: "Read rendered skill docs by skill ID. Pass format=json to return the backend docs envelope unchanged.",
				Params: map[string]registry.ParamSchema{
					"id":        {Type: "string", Required: true, Description: "Skill ID, usually docs_skill_id from connectors inspect"},
					"section":   {Type: "string", Required: false, Description: "Exact section ID from the docs outline"},
					"workspace": {Type: "string", Required: false, Description: "Workspace name. Omit for connector-source:* docs unless explicit scoping is needed."},
					"format":    {Type: "string", Required: false, Description: "Output format: markdown or json", Default: "markdown"},
				},
			},
			Run: skillsDocs,
		},
	}
}

func skillsList(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
	qp := skillsPageParams(params)
	workspaceID, err := resolveWorkspaceIDForSkills(ctx, c, params)
	if err != nil {
		return nil, err
	}
	qp["workspace_id"] = workspaceID
	raw, err := c.Get(ctx, "/api/v1/skills", qp)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func skillsSearch(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
	query, _ := params["query"].(string)
	qp := skillsPageParams(params)
	qp["query"] = query
	workspaceID, err := resolveWorkspaceIDForSkills(ctx, c, params)
	if err != nil {
		return nil, err
	}
	qp["workspace_id"] = workspaceID
	raw, err := c.Get(ctx, "/api/v1/skills/search", qp)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func skillsDocs(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
	id, _ := params["id"].(string)
	format, _ := params["format"].(string)
	if format == "" {
		format = "markdown"
	}
	if format != "markdown" && format != "json" {
		return nil, client.NewValidationError(
			fmt.Sprintf("unsupported skills docs format %q", format),
			`pass "format": "markdown" or "format": "json"`,
		)
	}

	qp := map[string]string{"id": id}
	if section, ok := params["section"].(string); ok && section != "" {
		qp["section"] = section
	}
	if shouldResolveWorkspaceForSkillDocs(id, params) {
		workspaceID, err := resolveWorkspaceIDForSkills(ctx, c, params)
		if err != nil {
			return nil, err
		}
		qp["workspace_id"] = workspaceID
	}

	raw, err := c.Get(ctx, "/api/v1/skills/docs", qp)
	if err != nil {
		return nil, err
	}
	if format == "json" {
		return raw, nil
	}

	var docs map[string]any
	if err := json.Unmarshal(raw, &docs); err != nil {
		return nil, fmt.Errorf("parsing skill docs: %w", err)
	}
	return map[string]any{
		"data": map[string]any{
			"format":     "markdown",
			"markdown":   renderSkillDocs(docs),
			"metadata":   skillDocsMetadataSubset(docs),
			"section_id": docs["section_id"],
		},
	}, nil
}

func skillsPageParams(params map[string]any) map[string]string {
	qp := map[string]string{"limit": strconv.Itoa(defaultSkillsLimit)}
	if limit, ok := intParam(params["limit"]); ok && limit > 0 {
		qp["limit"] = strconv.Itoa(limit)
	}
	if cursor, ok := params["cursor"].(string); ok && cursor != "" {
		qp["cursor"] = cursor
	}
	return qp
}

func shouldResolveWorkspaceForSkillDocs(id string, params map[string]any) bool {
	if workspace, supplied := params["workspace"].(string); supplied && workspace != "" {
		return true
	}
	return !strings.HasPrefix(id, connectorSourceSkillPrefix)
}

func resolveWorkspaceIDForSkills(ctx context.Context, c *client.Client, params map[string]any) (string, error) {
	workspaceName := applyDefaultWorkspace(c, params)
	workspaces, err := fetchAllWorkspaces(ctx, c)
	if err != nil {
		return "", err
	}

	var matches []workspaceLookupItem
	for _, workspace := range workspaces {
		if workspace.ID == "" || !workspace.IsActive() {
			continue
		}
		if strings.EqualFold(workspace.Name, workspaceName) {
			matches = append(matches, workspace)
		}
	}

	switch len(matches) {
	case 0:
		return "", client.NewNotFoundError(
			fmt.Sprintf("workspace %q not found", workspaceName),
			"run 'airbyte-agent workspaces list' to see available workspaces",
		)
	case 1:
		return matches[0].ID, nil
	default:
		return "", client.NewValidationError(
			fmt.Sprintf("ambiguous: %d active workspaces match %q case-insensitively", len(matches), workspaceName),
			"set a different default workspace with 'airbyte-agent workspaces use' or rename duplicate workspaces before using skills docs",
		)
	}
}

func fetchAllWorkspaces(ctx context.Context, c *client.Client) ([]workspaceLookupItem, error) {
	raw, err := c.Get(ctx, "/api/v1/workspaces", nil)
	if err != nil {
		return nil, err
	}

	var out []workspaceLookupItem
	seenNext := map[string]bool{}
	for pageCount := 0; ; pageCount++ {
		if pageCount >= maxWorkspaceLookupPages {
			return nil, client.NewValidationError(
				fmt.Sprintf("workspace pagination exceeded %d pages", maxWorkspaceLookupPages),
				"retry later or run 'airbyte-agent workspaces list' to inspect workspace pagination",
			)
		}

		var page workspaceListPage
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("parsing workspaces: %w", err)
		}

		for _, item := range page.Data {
			var workspace workspaceLookupItem
			if err := json.Unmarshal(item, &workspace); err != nil {
				return nil, fmt.Errorf("parsing workspace entry: %w", err)
			}
			out = append(out, workspace)
		}

		if page.Next == nil || *page.Next == "" {
			break
		}
		if seenNext[*page.Next] {
			return nil, client.NewValidationError(
				"workspace pagination returned a repeated next URL",
				"retry later or run 'airbyte-agent workspaces list' to inspect workspace pagination",
			)
		}
		seenNext[*page.Next] = true
		raw, err = c.GetURL(ctx, *page.Next)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func intParam(value any) (int, bool) {
	switch n := value.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	default:
		return 0, false
	}
}

func skillDocsMetadataSubset(docs map[string]any) map[string]any {
	metadata, ok := docs["metadata"].(map[string]any)
	if !ok {
		return map[string]any{}
	}

	out := map[string]any{}
	for _, key := range []string{"id", "kind", "title", "summary", "warnings"} {
		if value, ok := metadata[key]; ok {
			out[key] = value
		}
	}
	return out
}
