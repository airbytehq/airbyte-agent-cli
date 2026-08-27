package localexec

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// specForYAML compiles the response spec for an operation.
func specForYAML(t *testing.T, yaml, entity, action string) (*responseSpec, *ResolvedOperation) {
	t.Helper()
	def, err := ParseDefinition(yaml)
	if err != nil {
		t.Fatalf("ParseDefinition: %v", err)
	}
	op, err := def.ResolveOperation(entity, action)
	if err != nil {
		t.Fatalf("ResolveOperation: %v", err)
	}
	spec, err := parseResponseSpec(op)
	if err != nil {
		t.Fatalf("parseResponseSpec: %v", err)
	}
	return spec, op
}

func jsonResp(body string) *httpResponse {
	return &httpResponse{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(body),
	}
}

func TestShapeResponse_RecordSelector(t *testing.T) {
	spec, op := specForYAML(t, respSelectorYAML, "widget", "list")
	resp := jsonResp(`{"data":[{"id":1},{"id":2}],"meta":{"next":"c2","total":2}}`)
	res, err := shapeResponse(spec, resp, op, shapeOptions{})
	if err != nil {
		t.Fatalf("shapeResponse: %v", err)
	}
	if res.RecordCount != 2 {
		t.Fatalf("record count = %d", res.RecordCount)
	}
	if res.Metadata["next"] != "c2" {
		t.Fatalf("meta next = %v", res.Metadata["next"])
	}
	if res.Metadata["total"] != float64(2) {
		t.Fatalf("meta total = %v", res.Metadata["total"])
	}
}

func TestShapeResponse_NoSelectorArrayBody(t *testing.T) {
	spec, op := specForYAML(t, respNoSelectorYAML, "thing", "list")
	res, err := shapeResponse(spec, jsonResp(`[{"a":1},{"a":2},{"a":3}]`), op, shapeOptions{})
	if err != nil {
		t.Fatalf("shapeResponse: %v", err)
	}
	if res.RecordCount != 3 {
		t.Fatalf("record count = %d", res.RecordCount)
	}
}

func TestShapeResponse_NoSelectorObjectBody(t *testing.T) {
	spec, op := specForYAML(t, respNoSelectorYAML, "thing", "list")
	res, err := shapeResponse(spec, jsonResp(`{"a":1}`), op, shapeOptions{})
	if err != nil {
		t.Fatalf("shapeResponse: %v", err)
	}
	if res.RecordCount != 1 {
		t.Fatalf("record count = %d", res.RecordCount)
	}
}

func TestShapeResponse_SelectFields(t *testing.T) {
	spec, op := specForYAML(t, respNoSelectorYAML, "thing", "list")
	resp := jsonResp(`[{"id":1,"name":"a","nested":{"keep":true,"drop":false}}]`)
	res, err := shapeResponse(spec, resp, op, shapeOptions{selectFields: []string{"id", "nested.keep"}})
	if err != nil {
		t.Fatalf("shapeResponse: %v", err)
	}
	want := map[string]any{"id": float64(1), "nested": map[string]any{"keep": true}}
	if !reflect.DeepEqual(res.Records[0], want) {
		t.Fatalf("select got %#v want %#v", res.Records[0], want)
	}
}

func TestShapeResponse_ExcludeFields(t *testing.T) {
	spec, op := specForYAML(t, respNoSelectorYAML, "thing", "list")
	resp := jsonResp(`[{"id":1,"secret":"x","nested":{"keep":true,"drop":false}}]`)
	res, err := shapeResponse(spec, resp, op, shapeOptions{excludeFields: []string{"secret", "nested.drop"}})
	if err != nil {
		t.Fatalf("shapeResponse: %v", err)
	}
	want := map[string]any{"id": float64(1), "nested": map[string]any{"keep": true}}
	if !reflect.DeepEqual(res.Records[0], want) {
		t.Fatalf("exclude got %#v want %#v", res.Records[0], want)
	}
}

func TestShapeResponse_Filter(t *testing.T) {
	spec, op := specForYAML(t, respFilterYAML, "thing", "list")
	resp := jsonResp(`{"data":[{"id":1,"status":"active"},{"id":2,"status":"archived"},{"id":3,"status":"active"}]}`)
	res, err := shapeResponse(spec, resp, op, shapeOptions{})
	if err != nil {
		t.Fatalf("shapeResponse: %v", err)
	}
	if res.RecordCount != 2 {
		t.Fatalf("filter kept %d records", res.RecordCount)
	}
}

func TestShapeResponse_Transform(t *testing.T) {
	spec, op := specForYAML(t, respTransformYAML, "thing", "list")
	resp := jsonResp(`{"data":[{"id":1,"attrs":{"name":"alpha"}}]}`)
	res, err := shapeResponse(spec, resp, op, shapeOptions{})
	if err != nil {
		t.Fatalf("shapeResponse: %v", err)
	}
	want := map[string]any{"identifier": float64(1), "label": "alpha"}
	if !reflect.DeepEqual(res.Records[0], want) {
		t.Fatalf("transform got %#v want %#v", res.Records[0], want)
	}
}

func TestShapeResponse_ErrorCheck(t *testing.T) {
	spec, op := specForYAML(t, respErrorCheckYAML, "thing", "list")
	resp := jsonResp(`{"errors":[{"message":"boom"}]}`)
	_, err := shapeResponse(spec, resp, op, shapeOptions{})
	le, ok := AsError(err)
	if !ok || le.Type() != TypeConnectorExecution {
		t.Fatalf("expected connector_execution_error, got %v", err)
	}
	if strings.Contains(le.Message, "boom") {
		t.Fatalf("error-check leaked body content: %q", le.Message)
	}
}

func TestShapeResponse_ErrorCheckPasses(t *testing.T) {
	spec, op := specForYAML(t, respErrorCheckYAML, "thing", "list")
	resp := jsonResp(`{"data":[{"id":1}]}`)
	if _, err := shapeResponse(spec, resp, op, shapeOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestShapeResponse_MalformedJSON(t *testing.T) {
	spec, op := specForYAML(t, respNoSelectorYAML, "thing", "list")
	resp := &httpResponse{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{not json`)}
	_, err := shapeResponse(spec, resp, op, shapeOptions{})
	if le, ok := AsError(err); !ok || le.Type() != TypeConnectorExecution {
		t.Fatalf("expected connector_execution_error, got %v", err)
	}
}

func TestShapeResponse_EmptyBody(t *testing.T) {
	spec, op := specForYAML(t, respNoSelectorYAML, "thing", "list")
	resp := &httpResponse{StatusCode: 204, Header: http.Header{}, Body: nil}
	res, err := shapeResponse(spec, resp, op, shapeOptions{})
	if err != nil {
		t.Fatalf("shapeResponse: %v", err)
	}
	if res.RecordCount != 0 || res.Records == nil {
		t.Fatalf("empty body should yield empty non-nil records, got %#v", res.Records)
	}
}

func TestShapeResponse_TextBody(t *testing.T) {
	spec, op := specForYAML(t, respTextYAML, "thing", "list")
	resp := &httpResponse{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/plain"}}, Body: []byte("hello world")}
	res, err := shapeResponse(spec, resp, op, shapeOptions{})
	if err != nil {
		t.Fatalf("shapeResponse: %v", err)
	}
	if res.RecordCount != 1 || res.Records[0] != "hello world" {
		t.Fatalf("text body records = %#v", res.Records)
	}
}

func TestShapeResponse_Truncation(t *testing.T) {
	spec, op := specForYAML(t, respNoSelectorYAML, "thing", "list")
	long := strings.Repeat("x", 100)
	resp := jsonResp(`[{"blob":"` + long + `"}]`)
	res, err := shapeResponse(spec, resp, op, shapeOptions{maxFieldStringLen: 10})
	if err != nil {
		t.Fatalf("shapeResponse: %v", err)
	}
	if !res.Truncated {
		t.Fatal("expected Truncated=true")
	}
	rec := res.Records[0].(map[string]any)
	if !strings.HasSuffix(rec["blob"].(string), truncationMarker) {
		t.Fatalf("blob not truncated: %v", rec["blob"])
	}
}

func TestShapeResponse_SkipTruncation(t *testing.T) {
	spec, op := specForYAML(t, respNoSelectorYAML, "thing", "list")
	long := strings.Repeat("x", 100)
	resp := jsonResp(`[{"blob":"` + long + `"}]`)
	res, err := shapeResponse(spec, resp, op, shapeOptions{maxFieldStringLen: 10, skipTruncation: true})
	if err != nil {
		t.Fatalf("shapeResponse: %v", err)
	}
	if res.Truncated {
		t.Fatal("skip_truncation must not truncate")
	}
	rec := res.Records[0].(map[string]any)
	if rec["blob"].(string) != long {
		t.Fatal("blob should be intact")
	}
}

func TestShapeResponse_MaxRecordsLimit(t *testing.T) {
	spec, op := specForYAML(t, respNoSelectorYAML, "thing", "list")
	resp := jsonResp(`[{"a":1},{"a":2},{"a":3},{"a":4},{"a":5}]`)
	res, err := shapeResponse(spec, resp, op, shapeOptions{maxRecords: 2})
	if err != nil {
		t.Fatalf("shapeResponse: %v", err)
	}
	if res.RecordCount != 2 || !res.Truncated {
		t.Fatalf("expected 2 records + truncated, got count=%d truncated=%v", res.RecordCount, res.Truncated)
	}
}

func TestParseResponseSpec_MalformedJSONPathRejected(t *testing.T) {
	_, err := specForYAMLErr(respBadSelectorYAML, "thing", "list")
	if le, ok := AsError(err); !ok || le.Type() != TypeUnsupported {
		t.Fatalf("expected unsupported JSONPath error, got %v", err)
	}
}

func specForYAMLErr(yaml, entity, action string) (*responseSpec, error) {
	def, err := ParseDefinition(yaml)
	if err != nil {
		return nil, err
	}
	op, err := def.ResolveOperation(entity, action)
	if err != nil {
		return nil, err
	}
	return parseResponseSpec(op)
}

// --- test definitions -------------------------------------------------------

const respSelectorYAML = `
openapi: 3.0.0
servers: [{url: "https://api.example.test"}]
paths:
  /widgets:
    get:
      x-airbyte-entity: widget
      x-airbyte-action: list
      x-airbyte-record-selector: "$.data[*]"
      x-airbyte-record-meta:
        next: "$.meta.next"
        total: "$.meta.total"
      responses:
        "200":
          content:
            application/json:
              schema: {type: object}
`

const respNoSelectorYAML = `
openapi: 3.0.0
servers: [{url: "https://api.example.test"}]
paths:
  /things:
    get:
      x-airbyte-entity: thing
      x-airbyte-action: list
      responses:
        "200":
          content:
            application/json:
              schema: {type: object}
`

const respFilterYAML = `
openapi: 3.0.0
servers: [{url: "https://api.example.test"}]
paths:
  /things:
    get:
      x-airbyte-entity: thing
      x-airbyte-action: list
      x-airbyte-record-selector: "$.data[*]"
      x-airbyte-record-filter:
        path: "$.status"
        equals: active
      responses:
        "200":
          content:
            application/json:
              schema: {type: object}
`

const respTransformYAML = `
openapi: 3.0.0
servers: [{url: "https://api.example.test"}]
paths:
  /things:
    get:
      x-airbyte-entity: thing
      x-airbyte-action: list
      x-airbyte-record-selector: "$.data[*]"
      x-airbyte-record-transform:
        identifier: "$.id"
        label: "$.attrs.name"
      responses:
        "200":
          content:
            application/json:
              schema: {type: object}
`

const respErrorCheckYAML = `
openapi: 3.0.0
servers: [{url: "https://api.example.test"}]
paths:
  /things:
    get:
      x-airbyte-entity: thing
      x-airbyte-action: list
      x-airbyte-record-selector: "$.data[*]"
      x-airbyte-error-check:
        error_path: "$.errors"
      responses:
        "200":
          content:
            application/json:
              schema: {type: object}
`

const respTextYAML = `
openapi: 3.0.0
servers: [{url: "https://api.example.test"}]
paths:
  /things:
    get:
      x-airbyte-entity: thing
      x-airbyte-action: list
      responses:
        "200":
          content:
            text/plain:
              schema: {type: string}
`

const respBadSelectorYAML = `
openapi: 3.0.0
servers: [{url: "https://api.example.test"}]
paths:
  /things:
    get:
      x-airbyte-entity: thing
      x-airbyte-action: list
      x-airbyte-record-selector: "$..recursive"
      responses:
        "200":
          content:
            application/json:
              schema: {type: object}
`
