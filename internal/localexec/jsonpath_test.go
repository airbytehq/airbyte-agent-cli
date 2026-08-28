package localexec

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("bad test JSON: %v", err)
	}
	return v
}

func TestJSONPathCompileRejects(t *testing.T) {
	cases := []struct {
		name string
		expr string
	}{
		{"empty", ""},
		{"no root", "a.b"},
		{"recursive descent", "$..a"},
		{"filter", "$.a[?(@.x==1)]"},
		{"slice", "$.a[0:2]"},
		{"union", "$.a[0,1]"},
		{"trailing dot", "$.a."},
		{"empty dotted key", "$..a"},
		{"unterminated bracket", "$.a["},
		{"bad wildcard", "$.a[*"},
		{"negative index", "$.a[-1]"},
		{"non numeric index", "$.a[x]"},
		{"unsupported char in key", "$.a b"},
		{"unterminated quote", "$['a]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CompileJSONPath(tc.expr)
			if err == nil {
				t.Fatalf("expected error for %q", tc.expr)
			}
			e, ok := AsError(err)
			if !ok {
				t.Fatalf("expected *Error, got %T", err)
			}
			if e.Type() != TypeUnsupported {
				t.Fatalf("expected %s, got %s", TypeUnsupported, e.Type())
			}
			if e.ExitCode() != 4 {
				t.Fatalf("expected exit 4, got %d", e.ExitCode())
			}
		})
	}
}

func TestJSONPathLengthBound(t *testing.T) {
	long := "$" + strings.Repeat(".a", maxJSONPathSegments+5)
	if _, err := CompileJSONPath(long); err == nil {
		t.Fatal("expected segment bound error")
	}
	if _, err := CompileJSONPath("$." + strings.Repeat("a", maxJSONPathBytes+1)); err == nil {
		t.Fatal("expected byte bound error")
	}
}

func TestJSONPathEval(t *testing.T) {
	doc := mustJSON(t, `{
		"data": [
			{"id": 1, "name": "a"},
			{"id": 2, "name": "b"}
		],
		"meta": {"next": "cursor", "weird key": "ok"},
		"empty": []
	}`)

	cases := []struct {
		name string
		expr string
		want []any
	}{
		{"root", "$", []any{doc}},
		{"dotted", "$.meta.next", []any{"cursor"}},
		{"bracket single quote", "$['meta']['next']", []any{"cursor"}},
		{"bracket double quote", `$["meta"]["next"]`, []any{"cursor"}},
		{"quoted key with space", "$['meta']['weird key']", []any{"ok"}},
		{"index", "$.data[0].name", []any{"a"}},
		{"index second", "$.data[1].id", []any{float64(2)}},
		{"wildcard bracket", "$.data[*].id", []any{float64(1), float64(2)}},
		{"wildcard dot", "$.data.*.name", []any{"a", "b"}},
		{"missing key", "$.data[0].missing", nil},
		{"missing path", "$.nope.deep", nil},
		{"index out of range", "$.data[9]", nil},
		{"index into non array", "$.meta[0]", nil},
		{"wildcard empty", "$.empty[*]", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := CompileJSONPath(tc.expr)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			got := p.Eval(doc)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Eval(%q) = %#v, want %#v", tc.expr, got, tc.want)
			}
		})
	}
}

func TestJSONPathEvalOne(t *testing.T) {
	doc := mustJSON(t, `{"a": {"b": 1}, "list": [1, 2]}`)
	p, _ := CompileJSONPath("$.a.b")
	if v, ok := p.EvalOne(doc); !ok || v != float64(1) {
		t.Fatalf("EvalOne = %v, %v", v, ok)
	}
	multi, _ := CompileJSONPath("$.list[*]")
	if _, ok := multi.EvalOne(doc); ok {
		t.Fatal("EvalOne should be false for multiple matches")
	}
	missing, _ := CompileJSONPath("$.nope")
	if _, ok := missing.EvalOne(doc); ok {
		t.Fatal("EvalOne should be false for zero matches")
	}
}

func TestJSONPathEscaping(t *testing.T) {
	doc := mustJSON(t, `{"a'b": {"c\"d": 42}}`)
	p, err := CompileJSONPath(`$['a\'b']["c\"d"]`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got := p.Eval(doc)
	if len(got) != 1 || got[0] != float64(42) {
		t.Fatalf("got %#v", got)
	}
}
