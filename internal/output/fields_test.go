package output

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestFilter_EmptyPathsPassthrough(t *testing.T) {
	in := map[string]any{"a": 1, "b": 2}
	got := Filter(in, nil)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("expected passthrough, got %v", got)
	}
}

func TestFilter_TopLevelKeys(t *testing.T) {
	in := map[string]any{
		"id":          "x",
		"name":        "thing",
		"description": "ignored",
	}
	got := Filter(in, []string{"id", "name"})
	want := map[string]any{"id": "x", "name": "thing"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilter_NestedPath(t *testing.T) {
	in := map[string]any{
		"data": map[string]any{
			"id":     "x",
			"name":   "thing",
			"secret": "leaked",
		},
		"meta": "kept",
	}
	got := Filter(in, []string{"data.id", "meta"})
	want := map[string]any{
		"data": map[string]any{"id": "x"},
		"meta": "kept",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilter_ArrayBroadcast(t *testing.T) {
	in := map[string]any{
		"data": []any{
			map[string]any{"id": "1", "name": "alpha", "secret": "s1"},
			map[string]any{"id": "2", "name": "beta", "secret": "s2"},
		},
		"next": "cursor",
	}
	got := Filter(in, []string{"data.id", "data.name"})
	want := map[string]any{
		"data": []any{
			map[string]any{"id": "1", "name": "alpha"},
			map[string]any{"id": "2", "name": "beta"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilter_BareKeyKeepsSubtree(t *testing.T) {
	in := map[string]any{
		"data": []any{
			map[string]any{"id": "1", "name": "alpha"},
		},
	}
	got := Filter(in, []string{"data"})
	want := map[string]any{
		"data": []any{
			map[string]any{"id": "1", "name": "alpha"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilter_MissingPathSkippedSilently(t *testing.T) {
	in := map[string]any{"id": "x"}
	got, matched := FilterWithMatch(in, []string{"id", "missing", "deeply.nested.missing"})
	want := map[string]any{"id": "x"}
	if !matched {
		t.Fatal("expected existing id path to count as a match")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilterWithMatch_NestedExecuteEnvelope(t *testing.T) {
	in := map[string]any{
		"status": "success",
		"result": map[string]any{
			"data": []any{map[string]any{"total_users": 4, "ignored": true}},
			"meta": map[string]any{"has_more": false},
		},
		"warning": map[string]any{},
	}

	got, matched := FilterWithMatch(in, []string{"result.data.total_users", "result.meta", "warning"})
	want := map[string]any{
		"result": map[string]any{
			"data": []any{map[string]any{"total_users": 4}},
			"meta": map[string]any{"has_more": false},
		},
		"warning": map[string]any{},
	}
	if !matched {
		t.Fatal("expected projection to match execute envelope")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilterWithMatch_CompletelyUnmatched(t *testing.T) {
	in := map[string]any{
		"result": map[string]any{
			"data": []any{map[string]any{"id": "1"}},
		},
	}

	got, matched := FilterWithMatch(in, []string{"data.id", "meta"})
	want := map[string]any{}
	if matched {
		t.Fatal("expected projection to report no matches")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilterWithMatch_EmptyArrayCountsAsMatch(t *testing.T) {
	in := map[string]any{"data": []any{}}

	got, matched := FilterWithMatch(in, []string{"data.id"})
	want := map[string]any{"data": []any{}}
	if !matched {
		t.Fatal("expected empty array projection to count as a match")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilter_RawMessage(t *testing.T) {
	raw := json.RawMessage(`{"id": "x", "name": "thing", "secret": "leaked"}`)
	got := Filter(raw, []string{"id", "name"})
	want := map[string]any{"id": "x", "name": "thing"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilter_RawMessageWithArray(t *testing.T) {
	raw := json.RawMessage(`{"data": [{"id": 1, "extra": "drop"}, {"id": 2, "extra": "drop"}]}`)
	got := Filter(raw, []string{"data.id"})
	want := map[string]any{
		"data": []any{
			map[string]any{"id": float64(1)},
			map[string]any{"id": float64(2)},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilter_PrimitiveReturned(t *testing.T) {
	got := Filter("hello", []string{"x"})
	if got != "hello" {
		t.Errorf("expected 'hello' returned unchanged, got %v", got)
	}
}

func TestFilter_DoesNotMutateInput(t *testing.T) {
	in := map[string]any{
		"id":     "x",
		"secret": "s",
	}
	_ = Filter(in, []string{"id"})
	if _, ok := in["secret"]; !ok {
		t.Error("Filter should not mutate the input map")
	}
}

func TestFilter_SmartFallbackThroughDataWrapper(t *testing.T) {
	// Common Airbyte response shape: {"data": [...]}. When the user passes
	// row-level field names, broadcast through the single array wrapper.
	in := map[string]any{
		"data": []any{
			map[string]any{"id": "1", "organization_name": "Acme", "secret": "drop"},
			map[string]any{"id": "2", "organization_name": "Globex", "secret": "drop"},
		},
	}
	got := Filter(in, []string{"id", "organization_name"})
	want := map[string]any{
		"data": []any{
			map[string]any{"id": "1", "organization_name": "Acme"},
			map[string]any{"id": "2", "organization_name": "Globex"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilter_FallbackDropsNonArraySiblings(t *testing.T) {
	// When the fallback fires, the wrapper structure is preserved but
	// non-requested top-level fields (e.g. cursors) are not auto-included.
	// Users who want them must pass them explicitly: --fields next,id.
	in := map[string]any{
		"data": []any{map[string]any{"id": "1"}},
		"next": "cursor-1",
	}
	got := Filter(in, []string{"id"})
	want := map[string]any{
		"data": []any{map[string]any{"id": "1"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilter_FallbackSkippedWhenAnyPathMatchesTopLevel(t *testing.T) {
	// "next" matches a top-level key, so the user is presumed to be passing
	// top-level paths. "id" is silently dropped (does not exist top-level)
	// rather than triggering the fallback.
	in := map[string]any{
		"data": []any{map[string]any{"id": "1"}},
		"next": "cursor-1",
	}
	got := Filter(in, []string{"next", "id"})
	want := map[string]any{"next": "cursor-1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilter_FallbackSkippedWhenMultipleArrays(t *testing.T) {
	// Ambiguous: two arrays at the top level. Don't guess.
	in := map[string]any{
		"data":   []any{map[string]any{"id": "1"}},
		"errors": []any{map[string]any{"code": "x"}},
	}
	got := Filter(in, []string{"id"})
	want := map[string]any{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilter_FallbackSkippedWhenNoArray(t *testing.T) {
	// Top-level value is a single object, not a list response. No array to
	// broadcast through.
	in := map[string]any{
		"id":   "x",
		"meta": map[string]any{"v": 1},
	}
	got := Filter(in, []string{"missing"})
	want := map[string]any{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilter_BroadcastsThroughRawMessageArray(t *testing.T) {
	// Mirror what `workspaces list` returns: a map whose `data` value is
	// []json.RawMessage (paginated and accumulated without decoding each row).
	// `--fields id,name` should auto-broadcast through the wrapper despite
	// the array elements being late-bound JSON.
	in := map[string]any{
		"data": []json.RawMessage{
			json.RawMessage(`{"id": "ws-1", "name": "Production", "extra": "drop"}`),
			json.RawMessage(`{"id": "ws-2", "name": "Staging", "extra": "drop"}`),
		},
	}
	got := Filter(in, []string{"id", "name"})
	want := map[string]any{
		"data": []any{
			map[string]any{"id": "ws-1", "name": "Production"},
			map[string]any{"id": "ws-2", "name": "Staging"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilter_RawMessageArrayWithExplicitDottedPaths(t *testing.T) {
	// Same shape, but the user passed long-form paths. Should also work.
	in := map[string]any{
		"data": []json.RawMessage{
			json.RawMessage(`{"id": "ws-1", "name": "Production"}`),
		},
		"next": "cursor-1",
	}
	got := Filter(in, []string{"data.id", "next"})
	want := map[string]any{
		"data": []any{map[string]any{"id": "ws-1"}},
		"next": "cursor-1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilter_BareWinsOverNested(t *testing.T) {
	// If both "data" and "data.id" are requested, the bare path wins —
	// the caller asked for everything under `data`.
	in := map[string]any{
		"data": map[string]any{"id": "x", "name": "thing"},
	}
	got := Filter(in, []string{"data", "data.id"})
	want := map[string]any{
		"data": map[string]any{"id": "x", "name": "thing"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
