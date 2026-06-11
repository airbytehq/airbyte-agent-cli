package resources

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/airbytehq/airbyte-agent-cli/internal/auth"
	"github.com/airbytehq/airbyte-agent-cli/internal/client"
)

func TestSkillsListResolvesWorkspaceAndPassesThrough(t *testing.T) {
	var skillsQuery string
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/workspaces":
			if r.URL.Query().Get("name_contains") != "default" {
				t.Errorf("name_contains = %q, want default", r.URL.Query().Get("name_contains"))
			}
			_, _ = w.Write([]byte(`{"data": [{"id": "ws-1", "name": "Default", "status": "active"}]}`))
		case "/api/v1/skills":
			skillsQuery = r.URL.RawQuery
			if r.URL.Query().Get("workspace_id") != "ws-1" {
				t.Errorf("workspace_id = %q, want ws-1", r.URL.Query().Get("workspace_id"))
			}
			if r.URL.Query().Get("limit") != "5" {
				t.Errorf("limit = %q, want 5", r.URL.Query().Get("limit"))
			}
			_, _ = w.Write([]byte(`{"data": [{"id": "agent:mcp", "kind": "static"}], "next_cursor": null}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer apiServer.Close()

	c, cleanup := newTestClient(t, apiServer)
	defer cleanup()

	result, err := skillsList(context.Background(), c, map[string]any{"workspace": "default", "limit": float64(5)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(skillsQuery, "workspace_id=ws-1") {
		t.Errorf("skills query missing workspace_id: %s", skillsQuery)
	}
	raw, ok := result.(json.RawMessage)
	if !ok {
		t.Fatalf("expected json.RawMessage, got %T", result)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parsing result: %v", err)
	}
	if parsed["data"] == nil {
		t.Fatalf("expected pass-through data, got %v", parsed)
	}
}

func TestSkillsSearchSendsQueryCursorAndDefaultLimit(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/workspaces":
			if r.URL.Query().Get("name_contains") != "default" {
				t.Errorf("name_contains = %q, want default", r.URL.Query().Get("name_contains"))
			}
			_, _ = w.Write([]byte(`{"data": [{"id": "ws-1", "name": "default"}]}`))
		case "/api/v1/skills/search":
			if r.URL.Query().Get("query") != "post slack" {
				t.Errorf("query = %q", r.URL.Query().Get("query"))
			}
			if r.URL.Query().Get("cursor") != "cur-1" {
				t.Errorf("cursor = %q", r.URL.Query().Get("cursor"))
			}
			if r.URL.Query().Get("limit") != "20" {
				t.Errorf("default limit = %q", r.URL.Query().Get("limit"))
			}
			_, _ = w.Write([]byte(`{"data": []}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer apiServer.Close()

	c, cleanup := newTestClient(t, apiServer)
	defer cleanup()

	if _, err := skillsSearch(context.Background(), c, map[string]any{"workspace": "default", "query": "post slack", "cursor": "cur-1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSkillsListRejectsNonPositiveLimit(t *testing.T) {
	_, err := skillsList(context.Background(), nil, map[string]any{"workspace": "default", "limit": float64(0)})
	if err == nil {
		t.Fatal("expected validation error")
	}
	apiErr, ok := err.(*client.APIError)
	if !ok {
		t.Fatalf("expected *client.APIError, got %T", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", apiErr.StatusCode)
	}
}

func TestSkillsDocsConnectorSourceOmitsWorkspaceByDefault(t *testing.T) {
	var workspaceRequests int
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/workspaces":
			workspaceRequests++
			_, _ = w.Write([]byte(`{"data": [{"id": "ws-1", "name": "default"}]}`))
		case "/api/v1/skills/docs":
			if _, ok := r.URL.Query()["workspace_id"]; ok {
				t.Errorf("connector-source docs should omit workspace_id by default, got %s", r.URL.RawQuery)
			}
			_, _ = w.Write(skillDocsFixtureJSON())
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer apiServer.Close()

	c, cleanup := newTestClient(t, apiServer)
	defer cleanup()

	result, err := skillsDocs(context.Background(), c, map[string]any{"id": "connector-source:conn-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if workspaceRequests != 0 {
		t.Fatalf("expected no workspace resolution, got %d requests", workspaceRequests)
	}

	data := skillsDocsData(t, result)
	if data["format"] != "markdown" {
		t.Errorf("format = %v", data["format"])
	}
	if !strings.Contains(data["markdown"].(string), "Skill ID: connector-source:conn-1") {
		t.Errorf("markdown missing skill ID:\n%s", data["markdown"])
	}
}

func TestSkillsDocsConnectorSourceWithEmptyWorkspaceOmitsWorkspace(t *testing.T) {
	var workspaceRequests int
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/workspaces":
			workspaceRequests++
			_, _ = w.Write([]byte(`{"data": [{"id": "ws-1", "name": "default"}]}`))
		case "/api/v1/skills/docs":
			if _, ok := r.URL.Query()["workspace_id"]; ok {
				t.Errorf("empty workspace should not scope connector-source docs, got %s", r.URL.RawQuery)
			}
			_, _ = w.Write(skillDocsFixtureJSON())
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer apiServer.Close()

	c, cleanup := newTestClient(t, apiServer)
	defer cleanup()

	if _, err := skillsDocs(context.Background(), c, map[string]any{"id": "connector-source:conn-1", "workspace": ""}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if workspaceRequests != 0 {
		t.Fatalf("expected no workspace resolution, got %d requests", workspaceRequests)
	}
}

func TestSkillsDocsConnectorSourceWithWorkspaceSendsWorkspaceID(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/workspaces":
			if r.URL.Query().Get("name_contains") != "default" {
				t.Errorf("name_contains = %q, want default", r.URL.Query().Get("name_contains"))
			}
			_, _ = w.Write([]byte(`{"data": [{"id": "ws-1", "name": "default"}]}`))
		case "/api/v1/skills/docs":
			if r.URL.Query().Get("workspace_id") != "ws-1" {
				t.Errorf("workspace_id = %q, want ws-1", r.URL.Query().Get("workspace_id"))
			}
			_, _ = w.Write(skillDocsFixtureJSON())
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer apiServer.Close()

	c, cleanup := newTestClient(t, apiServer)
	defer cleanup()

	if _, err := skillsDocs(context.Background(), c, map[string]any{"id": "connector-source:conn-1", "workspace": "default"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSkillsDocsStaticSkillResolvesDefaultWorkspace(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/workspaces":
			if r.URL.Query().Get("name_contains") != "team-ws" {
				t.Errorf("name_contains = %q, want team-ws", r.URL.Query().Get("name_contains"))
			}
			_, _ = w.Write([]byte(`{"data": [{"id": "ws-2", "name": "team-ws"}]}`))
		case "/api/v1/skills/docs":
			if r.URL.Query().Get("workspace_id") != "ws-2" {
				t.Errorf("workspace_id = %q, want ws-2", r.URL.Query().Get("workspace_id"))
			}
			_, _ = w.Write(skillDocsFixtureJSON())
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer apiServer.Close()

	tokenServer := newTestTokenServer(t)
	defer tokenServer.Close()
	creds := &auth.Credentials{ClientID: "id", ClientSecret: "secret"}
	tm := auth.NewTokenManager(tokenServer.URL, "", creds)
	c := client.New(apiServer.URL, "org-123", "test", tm, client.WithDefaultWorkspace("team-ws"))

	var stderr strings.Builder
	prev := statusWriter
	statusWriter = &stderr
	defer func() { statusWriter = prev }()

	if _, err := skillsDocs(context.Background(), c, map[string]any{"id": "agent:mcp"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stderr.String(), "team-ws") {
		t.Errorf("expected default workspace notice, got %q", stderr.String())
	}
}

func TestSkillsDocsFormatJSONPassesBackendThrough(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/v1/skills/docs" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(skillDocsFixtureJSON())
	}))
	defer apiServer.Close()

	c, cleanup := newTestClient(t, apiServer)
	defer cleanup()

	result, err := skillsDocs(context.Background(), c, map[string]any{"id": "connector-source:conn-1", "format": "json"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	raw, ok := result.(json.RawMessage)
	if !ok {
		t.Fatalf("expected raw backend JSON, got %T", result)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parsing raw result: %v", err)
	}
	if parsed["metadata"] == nil {
		t.Fatalf("expected backend metadata, got %v", parsed)
	}
}

func TestSkillsDocsRejectsInvalidFormat(t *testing.T) {
	_, err := skillsDocs(context.Background(), nil, map[string]any{"id": "connector-source:conn-1", "format": "xml"})
	if err == nil {
		t.Fatal("expected validation error")
	}
	apiErr, ok := err.(*client.APIError)
	if !ok {
		t.Fatalf("expected *client.APIError, got %T", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", apiErr.StatusCode)
	}
}

func TestResolveWorkspaceIDForSkillsUsesServerFilterAndRejectsDuplicates(t *testing.T) {
	requests := 0
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/v1/workspaces" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("name_contains") != "default" {
			t.Errorf("name_contains = %q, want default", r.URL.Query().Get("name_contains"))
		}
		_, _ = w.Write([]byte(`{"data": [
			{"id": "ws-1", "name": "default", "status": "active"},
			{"id": "ws-2", "name": "DEFAULT", "status": "active"}
		]}`))
	}))
	defer apiServer.Close()

	c, cleanup := newTestClient(t, apiServer)
	defer cleanup()

	_, err := resolveWorkspaceIDForSkills(context.Background(), c, map[string]any{"workspace": "default"})
	if err == nil {
		t.Fatal("expected duplicate workspace validation error")
	}
	apiErr, ok := err.(*client.APIError)
	if !ok {
		t.Fatalf("expected *client.APIError, got %T", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", apiErr.StatusCode)
	}
	if requests != 1 {
		t.Errorf("requests = %d, want 1", requests)
	}
}

func TestLookupWorkspaceSkipsMalformedWorkspaceEntry(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/v1/workspaces" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"data": ["not-an-object", {"id": "ws-1", "name": "default", "status": "active"}]}`))
	}))
	defer apiServer.Close()

	c, cleanup := newTestClient(t, apiServer)
	defer cleanup()

	workspace, err := lookupWorkspace(context.Background(), c, "default")
	if err != nil {
		t.Fatalf("lookupWorkspace returned error: %v", err)
	}
	if workspace.ID != "ws-1" {
		t.Fatalf("workspace ID = %q, want ws-1", workspace.ID)
	}
}

func TestRenderSkillDocsMatchesMCPFixture(t *testing.T) {
	var docs map[string]any
	if err := json.Unmarshal(skillDocsFixtureJSON(), &docs); err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}

	got := renderSkillDocs(docs)
	want := "# HubSpot\n" +
		"Skill ID: connector-source:conn-1\n" +
		"Kind: connector_source\n" +
		"Summary: Use HubSpot CRM data.\n\n" +
		"Warnings:\n" +
		"- Index building\n\n" +
		"## Overview\n\n" +
		"Read before executing.\n\n" +
		"| Field | Meaning |\n" +
		"| --- | --- |\n" +
		"| company \\| name | Company name |\n\n" +
		"{\"extra\":true,\"type\":\"mystery\"}\n\n" +
		"## Outline\n" +
		"- `actions.contacts.search` Search contacts - Search indexed contacts"
	if got != want {
		t.Fatalf("rendered markdown mismatch\ngot:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestRenderSkillDocsSection(t *testing.T) {
	raw := []byte(`{
		"metadata": {"id": "agent:mcp", "kind": "static", "title": "MCP"},
		"section_id": "setup",
		"outline": [{"id": "setup", "title": "Setup"}],
		"content": [{"type": "code", "language": "bash", "code": "airbyte-agent skills list"}]
	}`)
	var docs map[string]any
	if err := json.Unmarshal(raw, &docs); err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}

	got := renderSkillDocs(docs)
	if !strings.Contains(got, "## Outline") {
		t.Fatalf("section docs should include outline:\n%s", got)
	}
	if !strings.Contains(got, "## Section `setup`") {
		t.Fatalf("section docs should include section header:\n%s", got)
	}
	if !strings.Contains(got, "```bash\nairbyte-agent skills list\n```") {
		t.Fatalf("section docs should render code block:\n%s", got)
	}
}

func TestRenderSkillDocsEscapesCodeFenceWithBackticks(t *testing.T) {
	raw := []byte("{\n" +
		"\"metadata\": {\"id\": \"agent:mcp\", \"kind\": \"static\", \"title\": \"MCP\"},\n" +
		"\"content\": [{\"type\": \"code\", \"language\": \"markdown\", \"code\": \"before\\n```\\ninside\\n```\"}]\n" +
		"}")
	var docs map[string]any
	if err := json.Unmarshal(raw, &docs); err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}

	got := renderSkillDocs(docs)
	if !strings.Contains(got, "````markdown\nbefore\n```\ninside\n```\n````") {
		t.Fatalf("expected four-backtick code fence, got:\n%s", got)
	}
}

func TestRenderSkillDocsTablePreservesExtraCells(t *testing.T) {
	raw := []byte(`{
		"metadata": {"id": "agent:mcp", "kind": "static", "title": "MCP"},
		"content": [{"type": "table", "headers": ["A"], "rows": [["one", "two", "three"]]}]
	}`)
	var docs map[string]any
	if err := json.Unmarshal(raw, &docs); err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}

	got := renderSkillDocs(docs)
	if !strings.Contains(got, "| A | Extra 1 | Extra 2 |") || !strings.Contains(got, "| one | two | three |") {
		t.Fatalf("expected extra table cells to render, got:\n%s", got)
	}
}

func TestRenderSkillDocsTitleFallsBackToMetadataID(t *testing.T) {
	raw := []byte(`{"metadata": {"id": "agent:mcp", "kind": "static"}, "content": []}`)
	var docs map[string]any
	if err := json.Unmarshal(raw, &docs); err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}

	got := renderSkillDocs(docs)
	if !strings.HasPrefix(got, "# agent:mcp\n") {
		t.Fatalf("expected title to fall back to metadata.id, got:\n%s", got)
	}
}

func skillsDocsData(t *testing.T, result any) map[string]any {
	t.Helper()
	outer, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", result)
	}
	data, ok := outer["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected data map, got %v", outer["data"])
	}
	return data
}

func skillDocsFixtureJSON() []byte {
	return []byte(`{
		"metadata": {
			"id": "connector-source:conn-1",
			"kind": "connector_source",
			"title": "HubSpot",
			"summary": "Use HubSpot CRM data.",
			"warnings": ["Index building"]
		},
		"outline": [{"id": "actions.contacts.search", "title": "Search contacts", "summary": "Search indexed contacts"}],
		"content": [
			{"type": "heading", "level": 2, "text": "Overview"},
			{"type": "paragraph", "text": "Read before executing."},
			{"type": "table", "headers": ["Field", "Meaning"], "rows": [["company | name", "Company name"]]},
			{"type": "mystery", "extra": true}
		]
	}`)
}
