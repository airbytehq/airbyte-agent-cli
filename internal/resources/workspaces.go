package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/airbytehq/airbyte-agent-cli/internal/auth"
	"github.com/airbytehq/airbyte-agent-cli/internal/client"
	"github.com/airbytehq/airbyte-agent-cli/internal/registry"
)

type workspacesResource struct{}

func (w *workspacesResource) Name() string        { return "workspaces" }
func (w *workspacesResource) Description() string { return "Manage workspaces" }
func (w *workspacesResource) Operations() []registry.Operation {
	return []registry.Operation{
		{
			Name:        "list",
			Description: "List workspaces",
			Schema: registry.OperationSchema{
				Description: "List all workspaces with cursor pagination",
				Params: map[string]registry.ParamSchema{
					"name_contains": {Type: "string", Required: false, Description: "Filter by name substring"},
					"status":        {Type: "string", Required: false, Description: "Filter by status"},
					"limit":         {Type: "integer", Required: false, Description: "Max total results to return"},
				},
			},
			SpecRef: registry.SpecRef{Path: "/api/v1/workspaces", Method: "GET"},
			Run:     listWorkspaces,
		},
		{
			Name:        "use",
			Description: "Set the default workspace stored in ~/.airbyte-agent/settings.json",
			Schema: registry.OperationSchema{
				Description: "Update settings.json so subsequent commands that don't pass `workspace` use this name. The workspace must exist (the command verifies via the API before writing).",
				Params: map[string]registry.ParamSchema{
					"name": {Type: "string", Required: true, Description: "Workspace name to use as the default"},
				},
			},
			Run: useWorkspace,
		},
	}
}

// useWorkspace updates Settings.Workspace in ~/.airbyte-agent/settings.json.
// It verifies the workspace exists via the API first, so users can't save a
// typo'd or nonexistent name. The canonical case from the API response is
// what gets persisted.
func useWorkspace(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
	name, _ := params["name"].(string)
	if name == "" {
		return nil, client.NewValidationError(
			"workspace name is required",
			`pass --json '{"name": "<workspace>"}'`,
		)
	}

	canonical, err := lookupWorkspaceName(ctx, c, name)
	if err != nil {
		return nil, err
	}

	settings, err := auth.ReadSettingsFile()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, client.NewNotFoundError(
				"settings file does not exist",
				"run 'airbyte-agent login' first to create ~/.airbyte-agent/settings.json",
			)
		}
		return nil, fmt.Errorf("reading settings: %w", err)
	}

	settings.Workspace = canonical
	if err := auth.WriteSettingsFile(settings); err != nil {
		return nil, fmt.Errorf("writing settings: %w", err)
	}

	return map[string]any{
		"status":    "saved",
		"workspace": canonical,
		"message":   fmt.Sprintf("default workspace set to %q in ~/.airbyte-agent/settings.json", canonical),
	}, nil
}

// lookupWorkspaceName confirms a workspace with the given name exists and
// returns its canonical-cased name. Match is case-insensitive (so the user
// can type "Default" and we'll accept "default").
func lookupWorkspaceName(ctx context.Context, c *client.Client, name string) (string, error) {
	workspace, err := lookupWorkspace(ctx, c, name)
	if err != nil {
		return "", err
	}
	return workspace.Name, nil
}

func lookupWorkspace(ctx context.Context, c *client.Client, name string) (workspaceLookupItem, error) {
	raw, err := c.Get(ctx, "/api/v1/workspaces", map[string]string{
		"name_contains": name,
	})
	if err != nil {
		return workspaceLookupItem{}, err
	}

	var page workspaceListPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return workspaceLookupItem{}, fmt.Errorf("parsing workspaces: %w", err)
	}

	var matches []workspaceLookupItem
	for _, item := range page.Data {
		var ws workspaceLookupItem
		if err := json.Unmarshal(item, &ws); err != nil {
			continue
		}
		if ws.ID != "" && ws.IsActive() && strings.EqualFold(ws.Name, name) {
			matches = append(matches, ws)
		}
	}

	switch len(matches) {
	case 0:
		return workspaceLookupItem{}, client.NewNotFoundError(
			fmt.Sprintf("active workspace %q not found", name),
			"run 'airbyte-agent workspaces list' to see available workspaces",
		)
	case 1:
		return matches[0], nil
	default:
		return workspaceLookupItem{}, client.NewValidationError(
			fmt.Sprintf("ambiguous: %d active workspaces match %q case-insensitively", len(matches), name),
			"use a workspace name with a unique case-insensitive match",
		)
	}
}

func listWorkspaces(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
	qp := make(map[string]string)
	if v, ok := params["name_contains"].(string); ok && v != "" {
		qp["name_contains"] = v
	}
	if v, ok := params["status"].(string); ok && v != "" {
		qp["status"] = v
	}

	limit, _ := intParam(params["limit"])
	if limit > 0 {
		qp["limit"] = fmt.Sprintf("%d", limit)
	}

	var allData []json.RawMessage

	raw, err := c.Get(ctx, "/api/v1/workspaces", qp)
	if err != nil {
		return nil, err
	}

	for {
		var page workspaceListPage
		if err := json.Unmarshal(raw, &page); err != nil {
			return raw, nil
		}

		allData = append(allData, page.Data...)

		if limit > 0 && len(allData) >= limit {
			allData = allData[:limit]
			break
		}

		if page.Next == nil || *page.Next == "" {
			break
		}

		raw, err = c.GetURL(ctx, *page.Next)
		if err != nil {
			return nil, err
		}
	}

	return map[string]any{"data": allData}, nil
}
