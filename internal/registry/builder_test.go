package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/airbytehq/airbyte-agent-cli/internal/client"
	"github.com/spf13/cobra"
)

// stubClient returns a non-nil Client for tests whose operations never make HTTP calls.
func stubClient() *client.Client {
	return client.New("", "", "test", nil)
}

type stubFlags struct {
	output string
	fields []string
}

func (s *stubFlags) GetOutput() string {
	return s.output
}

func (s *stubFlags) GetFields() []string {
	return s.fields
}

func newTestRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "airbyte",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringP("output", "o", "", "")
	return root
}

func captureExit(t *testing.T) (exitCode *int, restore func()) {
	t.Helper()
	old := osExit
	code := new(int)
	*code = -1
	osExit = func(c int) {
		*code = c
		panic("osExit called")
	}
	return code, func() { osExit = old }
}

func TestBuildCreatesCommands(t *testing.T) {
	t.Cleanup(func() { Reset() })

	Register(newMockResource("connectors", "Manage connectors",
		newMockOperation("list"),
		newMockOperation("get"),
	))

	root := newTestRoot()
	flags := &stubFlags{}
	Build(root, nil, flags)

	resCmd, _, err := root.Find([]string{"connectors"})
	if err != nil {
		t.Fatalf("expected 'connectors' command: %v", err)
	}
	if resCmd.Use != "connectors" {
		t.Errorf("expected Use='connectors', got %q", resCmd.Use)
	}

	listCmd, _, err := root.Find([]string{"connectors", "list"})
	if err != nil {
		t.Fatalf("expected 'connectors list' command: %v", err)
	}
	if listCmd.Use != "list" {
		t.Errorf("expected Use='list', got %q", listCmd.Use)
	}

	getCmd, _, err := root.Find([]string{"connectors", "get"})
	if err != nil {
		t.Fatalf("expected 'connectors get' command: %v", err)
	}
	if getCmd.Use != "get" {
		t.Errorf("expected Use='get', got %q", getCmd.Use)
	}
}

func TestBuildOperationFlags(t *testing.T) {
	t.Cleanup(func() { Reset() })

	Register(newMockResource("test", "Test resource", newMockOperation("execute")))

	root := newTestRoot()
	flags := &stubFlags{}
	Build(root, nil, flags)

	cmd, _, _ := root.Find([]string{"test", "execute"})
	jsonFlag := cmd.Flags().Lookup("json")
	if jsonFlag == nil {
		t.Fatal("expected --json flag on operation command")
	}

	idFlag := cmd.Flags().Lookup("id")
	if idFlag == nil {
		t.Fatal("expected --id flag on operation command")
	}
}

func TestBuildSchemaOutput(t *testing.T) {
	op := Operation{
		Name:        "list",
		Description: "List items",
		Schema: OperationSchema{
			Description: "List all items",
			Params: map[string]ParamSchema{
				"workspace_id": {Type: "string", Required: true, Description: "Workspace ID"},
				"limit":        {Type: "integer", Required: false, Description: "Max results", Default: 20},
			},
		},
	}

	result := BuildSchemaOutput(op)

	if result.Description != "List all items" {
		t.Errorf("expected description 'List all items', got %q", result.Description)
	}
	if _, ok := result.Params["workspace_id"]; !ok {
		t.Error("expected 'workspace_id' param in schema")
	}
	if !result.Params["workspace_id"].Required {
		t.Error("expected 'workspace_id' to be required")
	}
}

func TestRunReturnsJSON(t *testing.T) {
	t.Cleanup(func() { Reset() })

	Register(newMockResource("items", "Items",
		Operation{
			Name:        "list",
			Description: "List items",
			Schema: OperationSchema{
				Params: map[string]ParamSchema{},
			},
			Run: func(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
				return []map[string]string{{"id": "1", "name": "alpha"}}, nil
			},
		},
	))

	tmpFile := filepath.Join(t.TempDir(), "out.json")
	root := newTestRoot()
	flags := &stubFlags{output: tmpFile}
	Build(root, stubClient(), flags)

	root.SetArgs([]string{"items", "list", "--json", "{}"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}

	var result []map[string]string
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if len(result) != 1 || result[0]["id"] != "1" {
		t.Errorf("unexpected output: %s", string(data))
	}
}

func TestMissingRequiredParamReturnsValidationError(t *testing.T) {
	t.Cleanup(func() { Reset() })

	Register(newMockResource("things", "Things",
		Operation{
			Name:        "create",
			Description: "Create thing",
			Schema: OperationSchema{
				Params: map[string]ParamSchema{
					"entity": {Type: "string", Required: true, Description: "Entity name"},
					"type":   {Type: "string", Required: true, Description: "Entity type"},
				},
			},
			Run: func(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
				return nil, nil
			},
		},
	))

	exitCode, restore := captureExit(t)
	defer restore()

	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	root := newTestRoot()
	flags := &stubFlags{}
	Build(root, nil, flags)

	root.SetArgs([]string{"things", "create", "--json", "{}"})

	func() {
		defer func() { _ = recover() }()
		_ = root.Execute()
	}()

	_ = w.Close()
	os.Stderr = oldStderr

	if *exitCode != client.ExitValidation {
		t.Fatalf("expected exit code %d, got %d", client.ExitValidation, *exitCode)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var result map[string]any
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("parsing error output: %v (output: %s)", err, buf.String())
	}

	if result["type"] != "validation_error" {
		t.Errorf("expected type 'validation_error', got %v", result["type"])
	}

	fields, ok := result["fields"].(map[string]any)
	if !ok {
		t.Fatalf("expected 'fields' map, got %T: %v", result["fields"], result["fields"])
	}
	if fields["entity"] != "required" {
		t.Errorf("expected field 'entity'='required', got %v", fields["entity"])
	}
	if fields["type"] != "required" {
		t.Errorf("expected field 'type'='required', got %v", fields["type"])
	}
}

func TestFileInput(t *testing.T) {
	t.Cleanup(func() { Reset() })

	var captured map[string]any
	Register(newMockResource("things", "Things",
		Operation{
			Name:        "create",
			Description: "Create thing",
			Schema: OperationSchema{
				Params: map[string]ParamSchema{
					"name": {Type: "string", Required: true, Description: "Name"},
				},
			},
			Run: func(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
				captured = params
				return map[string]string{"status": "created"}, nil
			},
		},
	))

	tmpDir := t.TempDir()
	inputFile := filepath.Join(tmpDir, "input.json")
	if err := os.WriteFile(inputFile, []byte(`{"name":"test-thing"}`), 0o644); err != nil {
		t.Fatalf("writing input file: %v", err)
	}

	outFile := filepath.Join(tmpDir, "out.json")
	root := newTestRoot()
	flags := &stubFlags{output: outFile}
	Build(root, stubClient(), flags)

	root.SetArgs([]string{"things", "create", "--json", "@" + inputFile})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if captured["name"] != "test-thing" {
		t.Errorf("expected name='test-thing', got %v", captured["name"])
	}
}

func TestIDFlag(t *testing.T) {
	t.Cleanup(func() { Reset() })

	var captured map[string]any
	Register(newMockResource("things", "Things",
		Operation{
			Name:        "get",
			Description: "Get thing",
			Schema: OperationSchema{
				Params: map[string]ParamSchema{
					"id": {Type: "string", Required: true, Description: "ID"},
				},
			},
			Run: func(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
				captured = params
				return map[string]string{"id": params["id"].(string)}, nil
			},
		},
	))

	outFile := filepath.Join(t.TempDir(), "out.json")
	root := newTestRoot()
	flags := &stubFlags{output: outFile}
	Build(root, stubClient(), flags)

	root.SetArgs([]string{"things", "get", "--id", "abc-123"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if captured["id"] != "abc-123" {
		t.Errorf("expected id='abc-123', got %v", captured["id"])
	}
}

func TestInteractiveHookOverridesRun(t *testing.T) {
	t.Cleanup(func() { Reset() })

	runCalled := false
	interactiveCalled := false

	Register(newMockResource("auth", "Auth",
		Operation{
			Name:        "login",
			Description: "Login",
			Schema: OperationSchema{
				Params: map[string]ParamSchema{},
			},
			Run: func(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
				runCalled = true
				return nil, nil
			},
			Hooks: OperationHooks{
				Interactive: func(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
					interactiveCalled = true
					return map[string]string{"status": "authenticated"}, nil
				},
			},
		},
	))

	outFile := filepath.Join(t.TempDir(), "out.json")
	root := newTestRoot()
	flags := &stubFlags{output: outFile}
	Build(root, stubClient(), flags)

	root.SetArgs([]string{"auth", "login"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if runCalled {
		t.Error("Run should not be called when Interactive hook is set")
	}
	if !interactiveCalled {
		t.Error("Interactive hook should be called")
	}
}

func TestPreRunHookModifiesParams(t *testing.T) {
	t.Cleanup(func() { Reset() })

	var capturedParams map[string]any

	Register(newMockResource("resources", "Resources",
		Operation{
			Name:        "create",
			Description: "Create",
			Schema: OperationSchema{
				Params: map[string]ParamSchema{
					"name": {Type: "string", Required: true, Description: "Name"},
				},
			},
			Run: func(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
				capturedParams = params
				return map[string]string{"status": "ok"}, nil
			},
			Hooks: OperationHooks{
				PreRun: func(ctx context.Context, c *client.Client, params map[string]any) (map[string]any, error) {
					params["injected"] = "by-prerun"
					return params, nil
				},
			},
		},
	))

	outFile := filepath.Join(t.TempDir(), "out.json")
	root := newTestRoot()
	flags := &stubFlags{output: outFile}
	Build(root, stubClient(), flags)

	root.SetArgs([]string{"resources", "create", "--json", `{"name":"test"}`})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedParams["injected"] != "by-prerun" {
		t.Errorf("expected PreRun to inject 'injected' param, got %v", capturedParams)
	}
	if capturedParams["name"] != "test" {
		t.Errorf("expected name='test', got %v", capturedParams["name"])
	}
}

func TestPreRunHookError(t *testing.T) {
	t.Cleanup(func() { Reset() })

	runCalled := false
	Register(newMockResource("resources", "Resources",
		Operation{
			Name:        "create",
			Description: "Create",
			Schema: OperationSchema{
				Params: map[string]ParamSchema{},
			},
			Run: func(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
				runCalled = true
				return nil, nil
			},
			Hooks: OperationHooks{
				PreRun: func(ctx context.Context, c *client.Client, params map[string]any) (map[string]any, error) {
					return nil, &client.APIError{
						Type:       "server_error",
						Message:    "prerun failed",
						StatusCode: 500,
						Retryable:  false,
					}
				},
			},
		},
	))

	exitCode, restore := captureExit(t)
	defer restore()

	root := newTestRoot()
	flags := &stubFlags{}
	Build(root, stubClient(), flags)

	root.SetArgs([]string{"resources", "create"})

	func() {
		defer func() { _ = recover() }()
		_ = root.Execute()
	}()

	if runCalled {
		t.Error("Run should not be called when PreRun hook fails")
	}
	if *exitCode != client.ExitGeneral {
		t.Errorf("expected exit code %d, got %d", client.ExitGeneral, *exitCode)
	}
}

func TestNilClientReturnsAuthError(t *testing.T) {
	t.Cleanup(func() { Reset() })

	Register(newMockResource("items", "Items",
		Operation{
			Name:        "list",
			Description: "List items",
			Schema: OperationSchema{
				Params: map[string]ParamSchema{},
			},
			Run: func(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
				t.Fatal("Run should not be called with nil client")
				return nil, nil
			},
		},
	))

	exitCode, restore := captureExit(t)
	defer restore()

	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	root := newTestRoot()
	flags := &stubFlags{}
	Build(root, nil, flags)

	root.SetArgs([]string{"items", "list", "--json", "{}"})

	func() {
		defer func() { _ = recover() }()
		_ = root.Execute()
	}()

	_ = w.Close()
	os.Stderr = oldStderr

	if *exitCode != client.ExitAuth {
		t.Fatalf("expected exit code %d, got %d", client.ExitAuth, *exitCode)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var result map[string]any
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("parsing error output: %v (output: %s)", err, buf.String())
	}

	if result["type"] != "auth_error" {
		t.Errorf("expected type 'auth_error', got %v", result["type"])
	}

	hint, _ := result["hint"].(string)
	for _, want := range []string{"airbyte-agent login", "AIRBYTE_CLIENT_ID", "AIRBYTE_CLIENT_SECRET", "AIRBYTE_ORGANIZATION_ID", "~/.airbyte-agent/settings.json"} {
		if !strings.Contains(hint, want) {
			t.Errorf("expected hint to mention %q; got %q", want, hint)
		}
	}
}

// registerMixedTypeOp registers a mock operation with one parameter of each
// supported scalar type plus an array — used by several flag-mode tests.
func registerMixedTypeOp(captured *map[string]any) {
	Register(newMockResource("things", "Things",
		Operation{
			Name:        "do",
			Description: "Do thing",
			Schema: OperationSchema{
				Params: map[string]ParamSchema{
					"workspace":     {Type: "string", Required: true, Description: "Workspace name"},
					"name":          {Type: "string", Required: false, Description: "Thing name"},
					"limit":         {Type: "integer", Required: false, Description: "Limit"},
					"dry_run":       {Type: "bool", Required: false, Description: "Dry run"},
					"select_fields": {Type: "array", Required: false, Description: "Fields"},
					"params":        {Type: "object", Required: false, Description: "Free-form payload"},
				},
			},
			Run: func(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
				*captured = params
				return map[string]string{"ok": "yes"}, nil
			},
		},
	))
}

func TestParamFlagsString(t *testing.T) {
	t.Cleanup(func() { Reset() })

	var captured map[string]any
	registerMixedTypeOp(&captured)

	outFile := filepath.Join(t.TempDir(), "out.json")
	root := newTestRoot()
	Build(root, stubClient(), &stubFlags{output: outFile})

	root.SetArgs([]string{"things", "do", "--workspace", "prod", "--name", "thing-1"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if captured["workspace"] != "prod" {
		t.Errorf("expected workspace='prod', got %v", captured["workspace"])
	}
	if captured["name"] != "thing-1" {
		t.Errorf("expected name='thing-1', got %v", captured["name"])
	}
}

func TestParamFlagsTypeBindings(t *testing.T) {
	t.Cleanup(func() { Reset() })

	var captured map[string]any
	registerMixedTypeOp(&captured)

	outFile := filepath.Join(t.TempDir(), "out.json")
	root := newTestRoot()
	Build(root, stubClient(), &stubFlags{output: outFile})

	root.SetArgs([]string{
		"things", "do",
		"--workspace", "prod",
		"--limit", "42",
		"--dry-run",
		"--select-fields", "id,name,email",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if captured["limit"] != 42 {
		t.Errorf("expected limit=42 (int), got %v (%T)", captured["limit"], captured["limit"])
	}
	if captured["dry_run"] != true {
		t.Errorf("expected dry_run=true, got %v", captured["dry_run"])
	}
	arr, ok := captured["select_fields"].([]any)
	if !ok {
		t.Fatalf("expected select_fields []any, got %T", captured["select_fields"])
	}
	if len(arr) != 3 || arr[0] != "id" || arr[2] != "email" {
		t.Errorf("unexpected select_fields contents: %v", arr)
	}
}

func TestParamFlagsKebabCase(t *testing.T) {
	t.Cleanup(func() { Reset() })

	var captured map[string]any
	registerMixedTypeOp(&captured)

	root := newTestRoot()
	Build(root, stubClient(), &stubFlags{output: filepath.Join(t.TempDir(), "out.json")})

	cmd, _, _ := root.Find([]string{"things", "do"})
	if cmd.Flags().Lookup("dry-run") == nil {
		t.Error("expected --dry-run flag (kebab-case for dry_run)")
	}
	if cmd.Flags().Lookup("select-fields") == nil {
		t.Error("expected --select-fields flag (kebab-case for select_fields)")
	}
	if cmd.Flags().Lookup("dry_run") != nil {
		t.Error("did not expect --dry_run (snake_case form)")
	}
}

func TestObjectParamRegistersAsJSONStringFlag(t *testing.T) {
	t.Cleanup(func() { Reset() })

	var captured map[string]any
	registerMixedTypeOp(&captured)

	root := newTestRoot()
	Build(root, stubClient(), &stubFlags{})

	cmd, _, _ := root.Find([]string{"things", "do"})
	flag := cmd.Flags().Lookup("params")
	if flag == nil {
		t.Fatal("expected --params flag for object-typed param")
	}
	if flag.Value.Type() != "string" {
		t.Errorf("expected --params to be a string flag (JSON input), got %s", flag.Value.Type())
	}
	if !strings.Contains(flag.Usage, "JSON object") {
		t.Errorf("expected --params usage to mention 'JSON object', got %q", flag.Usage)
	}
}

func TestObjectParamUnmarshalsJSON(t *testing.T) {
	t.Cleanup(func() { Reset() })

	var captured map[string]any
	registerMixedTypeOp(&captured)

	outFile := filepath.Join(t.TempDir(), "out.json")
	root := newTestRoot()
	Build(root, stubClient(), &stubFlags{output: outFile})

	root.SetArgs([]string{
		"things", "do",
		"--workspace", "ws",
		"--params", `{"limit": 50, "filters": ["a", "b"]}`,
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, ok := captured["params"].(map[string]any)
	if !ok {
		t.Fatalf("expected params to be a map, got %T (%v)", captured["params"], captured["params"])
	}
	if got["limit"] != float64(50) { // JSON numbers decode as float64
		t.Errorf("expected limit=50, got %v", got["limit"])
	}
	filters, ok := got["filters"].([]any)
	if !ok || len(filters) != 2 || filters[0] != "a" {
		t.Errorf("expected filters=[a,b], got %v", got["filters"])
	}
}

func TestObjectParamRejectsInvalidJSON(t *testing.T) {
	t.Cleanup(func() { Reset() })

	var captured map[string]any
	registerMixedTypeOp(&captured)

	exitCode, restore := captureExit(t)
	defer restore()

	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	root := newTestRoot()
	Build(root, stubClient(), &stubFlags{})

	root.SetArgs([]string{
		"things", "do",
		"--workspace", "ws",
		"--params", `not-json`,
	})
	func() {
		defer func() { _ = recover() }()
		_ = root.Execute()
	}()

	_ = w.Close()
	os.Stderr = oldStderr

	if *exitCode != client.ExitValidation {
		t.Fatalf("expected exit %d, got %d", client.ExitValidation, *exitCode)
	}
	if captured != nil {
		t.Fatal("Run should not be called when --params JSON is invalid")
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	var errPayload map[string]any
	if err := json.Unmarshal(buf.Bytes(), &errPayload); err != nil {
		t.Fatalf("parsing stderr: %v", err)
	}
	if errPayload["type"] != "validation_error" {
		t.Errorf("expected type=validation_error, got %v", errPayload["type"])
	}
	msg, _ := errPayload["message"].(string)
	if !strings.Contains(msg, "--params") {
		t.Errorf("expected message to mention --params, got %q", msg)
	}
}

func TestJSONAndParamFlagsConflict(t *testing.T) {
	t.Cleanup(func() { Reset() })

	var captured map[string]any
	registerMixedTypeOp(&captured)

	exitCode, restore := captureExit(t)
	defer restore()

	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	root := newTestRoot()
	Build(root, stubClient(), &stubFlags{})

	root.SetArgs([]string{
		"things", "do",
		"--json", `{"workspace": "prod"}`,
		"--name", "thing-1",
	})

	func() {
		defer func() { _ = recover() }()
		_ = root.Execute()
	}()

	_ = w.Close()
	os.Stderr = oldStderr

	if *exitCode != client.ExitValidation {
		t.Fatalf("expected exit code %d, got %d", client.ExitValidation, *exitCode)
	}
	if captured != nil {
		t.Fatal("Run should not have been called when --json conflicts with flags")
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var result map[string]any
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("parsing error output: %v (raw: %s)", err, buf.String())
	}
	if result["type"] != "validation_error" {
		t.Errorf("expected type='validation_error', got %v", result["type"])
	}
	msg, _ := result["message"].(string)
	if !strings.Contains(msg, "--name") || !strings.Contains(msg, "--json") {
		t.Errorf("expected message to mention --json and --name; got %q", msg)
	}
}

func TestFieldsFilterAppliedToOutput(t *testing.T) {
	t.Cleanup(func() { Reset() })

	Register(newMockResource("items", "Items",
		Operation{
			Name:        "list",
			Description: "List items",
			Schema: OperationSchema{
				Params: map[string]ParamSchema{},
			},
			Run: func(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
				return map[string]any{
					"data": []any{
						map[string]any{"id": "1", "name": "alpha", "secret": "drop"},
						map[string]any{"id": "2", "name": "beta", "secret": "drop"},
					},
					"next": "cursor-1",
				}, nil
			},
		},
	))

	outFile := filepath.Join(t.TempDir(), "out.json")
	root := newTestRoot()
	flags := &stubFlags{output: outFile, fields: []string{"data.id", "next"}}
	Build(root, stubClient(), flags)

	root.SetArgs([]string{"items", "list"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parsing output: %v (raw: %s)", err, string(data))
	}

	rows, ok := got["data"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("expected 2 rows under 'data', got %v", got["data"])
	}
	for i, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			t.Fatalf("row %d not a map: %T", i, row)
		}
		if _, hasSecret := m["secret"]; hasSecret {
			t.Errorf("row %d still contains 'secret' after filter: %v", i, m)
		}
		if _, hasName := m["name"]; hasName {
			t.Errorf("row %d still contains 'name' after filter: %v", i, m)
		}
		if m["id"] == nil {
			t.Errorf("row %d missing 'id' after filter", i)
		}
	}
	if got["next"] != "cursor-1" {
		t.Errorf("expected 'next' kept, got %v", got["next"])
	}
}

func TestFieldsFilterAppliedToNestedExecuteEnvelope(t *testing.T) {
	t.Cleanup(func() { Reset() })

	Register(newMockResource("connectors", "Connectors",
		Operation{
			Name:        "execute",
			Description: "Execute action",
			Schema:      OperationSchema{Params: map[string]ParamSchema{}},
			Run: func(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
				return map[string]any{
					"status": "success",
					"result": map[string]any{
						"data": []any{map[string]any{"total_users": 4, "ignored": true}},
						"meta": map[string]any{"has_more": false},
					},
					"warning": map[string]any{},
				}, nil
			},
		},
	))

	outFile := filepath.Join(t.TempDir(), "out.json")
	root := newTestRoot()
	flags := &stubFlags{
		output: outFile,
		fields: []string{"result.data.total_users", "result.meta", "warning"},
	}
	Build(root, stubClient(), flags)

	root.SetArgs([]string{"connectors", "execute"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if _, ok := got["status"]; ok {
		t.Fatalf("status should have been filtered out: %v", got)
	}
	result, ok := got["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result object, got %T", got["result"])
	}
	rows, ok := result["data"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("expected one result row, got %v", result["data"])
	}
	row, ok := rows[0].(map[string]any)
	if !ok || row["total_users"] != float64(4) {
		t.Fatalf("unexpected projected row: %v", rows[0])
	}
	if _, ok := row["ignored"]; ok {
		t.Fatalf("ignored field should have been filtered out: %v", row)
	}
	if _, ok := result["meta"]; !ok {
		t.Fatalf("expected result.meta to be retained: %v", result)
	}
	if _, ok := got["warning"]; !ok {
		t.Fatalf("expected warning to be retained: %v", got)
	}
}

func TestFieldsFilterRejectsCompletelyUnmatchedPaths(t *testing.T) {
	t.Cleanup(func() { Reset() })

	Register(newMockResource("connectors", "Connectors",
		Operation{
			Name:        "execute",
			Description: "Execute action",
			Schema:      OperationSchema{Params: map[string]ParamSchema{}},
			Run: func(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
				return map[string]any{
					"status": "success",
					"result": map[string]any{
						"data": []any{map[string]any{"id": "1"}},
					},
				}, nil
			},
		},
	))

	exitCode, restore := captureExit(t)
	defer restore()

	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	defer func() { os.Stderr = oldStderr }()

	root := newTestRoot()
	Build(root, stubClient(), &stubFlags{fields: []string{"data.id", "meta"}})
	root.SetArgs([]string{"connectors", "execute"})
	func() {
		defer func() { _ = recover() }()
		_ = root.Execute()
	}()

	_ = w.Close()
	if *exitCode != client.ExitValidation {
		t.Fatalf("expected exit %d, got %d", client.ExitValidation, *exitCode)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	var errPayload map[string]any
	if err := json.Unmarshal(buf.Bytes(), &errPayload); err != nil {
		t.Fatalf("parsing stderr: %v (raw: %s)", err, buf.String())
	}
	if errPayload["type"] != "validation_error" {
		t.Errorf("expected type=validation_error, got %v", errPayload["type"])
	}
	message, _ := errPayload["message"].(string)
	if !strings.Contains(message, "data.id") || !strings.Contains(message, "meta") {
		t.Errorf("expected message to list unmatched paths, got %q", message)
	}
	hint, _ := errPayload["hint"].(string)
	if !strings.Contains(hint, "remove --fields") {
		t.Errorf("expected hint to explain recovery, got %q", hint)
	}
}

func TestFieldsFilterDoesNotApplyToErrors(t *testing.T) {
	t.Cleanup(func() { Reset() })

	Register(newMockResource("items", "Items",
		Operation{
			Name:        "fail",
			Description: "Always fail",
			Schema:      OperationSchema{Params: map[string]ParamSchema{}},
			Run: func(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
				return nil, &client.APIError{
					Type:       "not_found",
					Message:    "missing",
					StatusCode: 404,
				}
			},
		},
	))

	exitCode, restore := captureExit(t)
	defer restore()

	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	root := newTestRoot()
	// fields filter that would, if applied, drop the error fields.
	flags := &stubFlags{fields: []string{"nonexistent"}}
	Build(root, stubClient(), flags)

	root.SetArgs([]string{"items", "fail"})
	func() {
		defer func() { _ = recover() }()
		_ = root.Execute()
	}()

	_ = w.Close()
	os.Stderr = oldStderr

	if *exitCode != client.ExitNotFound {
		t.Fatalf("expected exit %d, got %d", client.ExitNotFound, *exitCode)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var errPayload map[string]any
	if err := json.Unmarshal(buf.Bytes(), &errPayload); err != nil {
		t.Fatalf("parsing stderr: %v (raw: %s)", err, buf.String())
	}
	if errPayload["type"] != "not_found" {
		t.Errorf("error payload was filtered or malformed: %v", errPayload)
	}
	if errPayload["message"] != "missing" {
		t.Errorf("error payload missing message field: %v", errPayload)
	}
}

func TestParamFlagsMissingRequired(t *testing.T) {
	t.Cleanup(func() { Reset() })

	var captured map[string]any
	registerMixedTypeOp(&captured)

	exitCode, restore := captureExit(t)
	defer restore()

	oldStderr := os.Stderr
	_, w, _ := os.Pipe()
	os.Stderr = w

	root := newTestRoot()
	Build(root, stubClient(), &stubFlags{})

	// `workspace` is required but not provided via any mode.
	root.SetArgs([]string{"things", "do", "--name", "thing-1"})

	func() {
		defer func() { _ = recover() }()
		_ = root.Execute()
	}()

	_ = w.Close()
	os.Stderr = oldStderr

	if *exitCode != client.ExitValidation {
		t.Fatalf("expected exit code %d, got %d", client.ExitValidation, *exitCode)
	}
}

func TestJSONUnknownParamReturnsValidationError(t *testing.T) {
	t.Cleanup(func() { Reset() })

	var captured map[string]any
	registerMixedTypeOp(&captured)

	exitCode, restore := captureExit(t)
	defer restore()

	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	root := newTestRoot()
	Build(root, stubClient(), &stubFlags{})
	root.SetArgs([]string{"things", "do", "--json", `{"workspace":"prod","extra":"bad"}`})

	func() {
		defer func() { _ = recover() }()
		_ = root.Execute()
	}()

	_ = w.Close()
	os.Stderr = oldStderr

	if *exitCode != client.ExitValidation {
		t.Fatalf("expected exit code %d, got %d", client.ExitValidation, *exitCode)
	}
	if captured != nil {
		t.Fatal("Run should not have been called with unknown params")
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	var result map[string]any
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("parsing error output: %v (raw: %s)", err, buf.String())
	}

	fields, ok := result["fields"].(map[string]any)
	if !ok {
		t.Fatalf("expected fields map, got %T", result["fields"])
	}
	if fields["extra"] != "unknown parameter" {
		t.Errorf("expected unknown parameter error, got %v", fields["extra"])
	}
}

func TestJSONWrongTypeReturnsValidationError(t *testing.T) {
	t.Cleanup(func() { Reset() })

	var captured map[string]any
	registerMixedTypeOp(&captured)

	exitCode, restore := captureExit(t)
	defer restore()

	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	root := newTestRoot()
	Build(root, stubClient(), &stubFlags{})
	root.SetArgs([]string{"things", "do", "--json", `{"workspace":"prod","limit":"42"}`})

	func() {
		defer func() { _ = recover() }()
		_ = root.Execute()
	}()

	_ = w.Close()
	os.Stderr = oldStderr

	if *exitCode != client.ExitValidation {
		t.Fatalf("expected exit code %d, got %d", client.ExitValidation, *exitCode)
	}
	if captured != nil {
		t.Fatal("Run should not have been called with wrong param types")
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	var result map[string]any
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("parsing error output: %v (raw: %s)", err, buf.String())
	}

	fields, ok := result["fields"].(map[string]any)
	if !ok {
		t.Fatalf("expected fields map, got %T", result["fields"])
	}
	if fields["limit"] != "expected integer" {
		t.Errorf("expected integer type error, got %v", fields["limit"])
	}
}

func TestInteractiveHookRequiresAuthByDefault(t *testing.T) {
	t.Cleanup(func() { Reset() })

	interactiveCalled := false
	Register(newMockResource("connectors", "Connectors",
		Operation{
			Name: "create",
			Schema: OperationSchema{
				Params: map[string]ParamSchema{},
			},
			Hooks: OperationHooks{
				Interactive: func(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
					interactiveCalled = true
					return map[string]string{"status": "ok"}, nil
				},
			},
		},
	))

	exitCode, restore := captureExit(t)
	defer restore()

	root := newTestRoot()
	Build(root, nil, &stubFlags{})
	root.SetArgs([]string{"connectors", "create"})

	func() {
		defer func() { _ = recover() }()
		_ = root.Execute()
	}()

	if *exitCode != client.ExitAuth {
		t.Fatalf("expected exit code %d, got %d", client.ExitAuth, *exitCode)
	}
	if interactiveCalled {
		t.Fatal("Interactive should not be called without credentials by default")
	}
}

func TestInteractiveHookCanAllowUnauthenticated(t *testing.T) {
	t.Cleanup(func() { Reset() })

	interactiveCalled := false
	Register(newMockResource("auth", "Auth",
		Operation{
			Name: "login",
			Schema: OperationSchema{
				Params: map[string]ParamSchema{},
			},
			Hooks: OperationHooks{
				Interactive: func(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
					interactiveCalled = true
					return map[string]string{"status": "ok"}, nil
				},
				AllowUnauthenticated: true,
			},
		},
	))

	outFile := filepath.Join(t.TempDir(), "out.json")
	root := newTestRoot()
	Build(root, nil, &stubFlags{output: outFile})
	root.SetArgs([]string{"auth", "login"})

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !interactiveCalled {
		t.Fatal("Interactive should be called when unauthenticated access is allowed")
	}
}

func TestParamFlagCollisionReturnsValidationError(t *testing.T) {
	t.Cleanup(func() { Reset() })

	Register(newMockResource("things", "Things",
		Operation{
			Name: "do",
			Schema: OperationSchema{
				Params: map[string]ParamSchema{
					"json": {Type: "string", Required: false, Description: "Reserved"},
				},
			},
			Run: func(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
				t.Fatal("Run should not be called when schema flags conflict")
				return nil, nil
			},
		},
	))

	exitCode, restore := captureExit(t)
	defer restore()

	root := newTestRoot()
	Build(root, stubClient(), &stubFlags{})
	root.SetArgs([]string{"things", "do"})

	func() {
		defer func() { _ = recover() }()
		_ = root.Execute()
	}()

	if *exitCode != client.ExitValidation {
		t.Fatalf("expected exit code %d, got %d", client.ExitValidation, *exitCode)
	}
}

func TestParamFlagDuplicateNameReturnsValidationError(t *testing.T) {
	t.Cleanup(func() { Reset() })

	Register(newMockResource("things", "Things",
		Operation{
			Name: "do",
			Schema: OperationSchema{
				Params: map[string]ParamSchema{
					"foo_bar": {Type: "string", Required: false, Description: "One"},
					"foo-bar": {Type: "string", Required: false, Description: "Two"},
				},
			},
			Run: func(ctx context.Context, c *client.Client, params map[string]any) (any, error) {
				t.Fatal("Run should not be called when generated flags conflict")
				return nil, nil
			},
		},
	))

	exitCode, restore := captureExit(t)
	defer restore()

	root := newTestRoot()
	Build(root, stubClient(), &stubFlags{})
	root.SetArgs([]string{"things", "do"})

	func() {
		defer func() { _ = recover() }()
		_ = root.Execute()
	}()

	if *exitCode != client.ExitValidation {
		t.Fatalf("expected exit code %d, got %d", client.ExitValidation, *exitCode)
	}
}
