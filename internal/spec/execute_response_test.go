package spec_test

// This test lives in an external test package so it can import both the spec
// package (to inspect the generated route schema) and the resources package (to
// inspect the live `connectors execute` operation param schema) without any
// import cycle. It guards the Phase 4 opt-in local-execution contract:
//
//   - the extracted execute-route response schema exposes the optional `bundle`
//     and `warning` fields and no longer requires `result`; and
//   - the `connectors execute` operation param schema does NOT expose `bundle`
//     (or any other secret-bearing field) as a CLI input.

import (
	"encoding/json"
	"testing"

	"github.com/airbytehq/airbyte-agent-cli/internal/registry"
	"github.com/airbytehq/airbyte-agent-cli/internal/resources"
	"github.com/airbytehq/airbyte-agent-cli/internal/spec"
)

const executeRouteKey = "POST /api/v1/integrations/connectors/{id}/execute"

func TestExecuteResponseExposesLocalExecutionContract(t *testing.T) {
	route, ok := spec.Lookup(executeRouteKey)
	if !ok {
		t.Fatalf("execute route %q not found in extracted schemas", executeRouteKey)
	}
	if len(route.Response) == 0 {
		t.Fatalf("execute route %q has no response schema", executeRouteKey)
	}

	var resp struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(route.Response, &resp); err != nil {
		t.Fatalf("parsing execute response schema: %v", err)
	}

	for _, field := range []string{"bundle", "warning"} {
		if _, present := resp.Properties[field]; !present {
			t.Errorf("execute response schema missing optional %q property", field)
		}
	}

	for _, req := range resp.Required {
		if req == "result" {
			t.Errorf("execute response schema still requires %q; it must be optional for prepare responses", "result")
		}
	}
	// bundle/warning must never be required.
	for _, req := range resp.Required {
		if req == "bundle" || req == "warning" {
			t.Errorf("execute response schema must not require %q", req)
		}
	}
}

func TestExecuteOperationParamsDoNotExposeBundle(t *testing.T) {
	registry.Reset()
	resources.RegisterAll()

	var execOp *registry.Operation
	for _, res := range registry.All() {
		if res.Name() != "connectors" {
			continue
		}
		for _, op := range res.Operations() {
			if op.Name == "execute" {
				o := op
				execOp = &o
			}
		}
	}
	if execOp == nil {
		t.Fatal("connectors execute operation not found")
	}

	// bundle must never become a CLI input, nor should the global runtime
	// controls leak into the operation param schema.
	forbidden := []string{"bundle", "execution_mode", "aws_profile", "aws_region"}
	for _, name := range forbidden {
		if _, present := execOp.Schema.Params[name]; present {
			t.Errorf("connectors execute param schema must not expose %q as an input", name)
		}
	}
}
