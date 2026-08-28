package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// fakeProvider records the coordinates it was asked to resolve and returns a
// canned value or error per coordinate.
type fakeProvider struct {
	mu     sync.Mutex
	calls  []string
	values map[string]string
	errs   map[string]error
	// fixed, if non-nil, is returned for every coordinate.
	fixed *string
}

func (f *fakeProvider) Resolve(ctx context.Context, coordinate string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, coordinate)
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if f.errs != nil {
		if err, ok := f.errs[coordinate]; ok {
			return "", err
		}
	}
	if f.fixed != nil {
		return *f.fixed, nil
	}
	if f.values != nil {
		if v, ok := f.values[coordinate]; ok {
			return v, nil
		}
	}
	return "resolved:" + coordinate, nil
}

func coord(suffix string) string { return CoordinatePrefix + suffix }

func TestHydrate_NestedMapsAndArrays(t *testing.T) {
	p := &fakeProvider{values: map[string]string{
		"db/pw":   "secretpw",
		"api/key": "secretkey",
	}}
	input := map[string]any{
		"name": "plain",
		"nested": map[string]any{
			"password": coord("db/pw"),
			"list": []any{
				"literal",
				coord("api/key"),
				map[string]any{"deep": coord("db/pw")},
			},
		},
	}
	got, err := Hydrate(context.Background(), p, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]any{
		"name": "plain",
		"nested": map[string]any{
			"password": "secretpw",
			"list": []any{
				"literal",
				"secretkey",
				map[string]any{"deep": "secretpw"},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestHydrate_DeduplicatesRepeatedCoordinate(t *testing.T) {
	p := &fakeProvider{values: map[string]string{"same": "v"}}
	input := []any{coord("same"), coord("same"), coord("same")}
	if _, err := Hydrate(context.Background(), p, input); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.calls) != 1 {
		t.Errorf("provider called %d times, want 1 (dedup)", len(p.calls))
	}
}

func TestHydrate_NoCacheAcrossInvocations(t *testing.T) {
	p := &fakeProvider{values: map[string]string{"x": "v"}}
	input := coord("x")
	_, _ = Hydrate(context.Background(), p, input)
	_, _ = Hydrate(context.Background(), p, input)
	if len(p.calls) != 2 {
		t.Errorf("provider called %d times across 2 invocations, want 2 (no cross-call cache)", len(p.calls))
	}
}

func TestHydrate_PlainStringsUnchanged(t *testing.T) {
	p := &fakeProvider{}
	for _, s := range []string{
		"just a string",
		"secret_coordinate",             // missing trailing ::
		"prefix secret_coordinate::mid", // prefix not at start
		"SECRET_COORDINATE::x",          // wrong case
	} {
		got, err := Hydrate(context.Background(), p, s)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", s, err)
		}
		if got != s {
			t.Errorf("got %q, want unchanged %q", got, s)
		}
	}
	if len(p.calls) != 0 {
		t.Errorf("provider should not be called for non-coordinates, got %d calls", len(p.calls))
	}
}

func TestHydrate_EmptySuffixRejected(t *testing.T) {
	p := &fakeProvider{}
	_, err := Hydrate(context.Background(), p, coord(""))
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("error = %T, want *Error", err)
	}
	if e.Type != ErrHydration {
		t.Errorf("Type = %q, want %q", e.Type, ErrHydration)
	}
	if len(p.calls) != 0 {
		t.Error("provider should not be called for empty suffix")
	}
}

func TestHydrate_PreservesScalarTypes(t *testing.T) {
	p := &fakeProvider{}
	input := map[string]any{
		"b":    true,
		"n":    json.Number("42"),
		"f":    3.14,
		"null": nil,
		"i":    int64(7),
	}
	got, err := Hydrate(context.Background(), p, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, input) {
		t.Errorf("scalars not preserved: got %#v, want %#v", got, input)
	}
}

func TestHydrate_NoMutationOfInput(t *testing.T) {
	p := &fakeProvider{values: map[string]string{"k": "v"}}
	inner := map[string]any{"pw": coord("k")}
	list := []any{coord("k")}
	input := map[string]any{"inner": inner, "list": list}

	if _, err := Hydrate(context.Background(), p, input); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inner["pw"] != coord("k") {
		t.Errorf("input map mutated: %v", inner["pw"])
	}
	if list[0] != coord("k") {
		t.Errorf("input slice mutated: %v", list[0])
	}
}

func TestHydrate_ProviderFailurePropagates(t *testing.T) {
	sentinel := newError(ErrAccess, "denied by provider")
	p := &fakeProvider{errs: map[string]error{"k": sentinel}}
	_, err := Hydrate(context.Background(), p, coord("k"))
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("error = %T, want *Error", err)
	}
	if e.Type != ErrAccess {
		t.Errorf("Type = %q, want %q (typed error passed through)", e.Type, ErrAccess)
	}
}

func TestHydrate_UntypedProviderErrorWrapped(t *testing.T) {
	p := &fakeProvider{errs: map[string]error{"k": errors.New("boom raw")}}
	_, err := Hydrate(context.Background(), p, coord("k"))
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("error = %T, want *Error", err)
	}
	if e.Type != ErrHydration {
		t.Errorf("Type = %q, want %q", e.Type, ErrHydration)
	}
}

func TestHydrate_Cancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &fakeProvider{}
	_, err := Hydrate(ctx, p, coord("k"))
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error does not wrap context.Canceled: %v", err)
	}
}

func TestHydrate_NoCoordinateOrValueLeakInErrors(t *testing.T) {
	secretSuffix := "super/sensitive/coordinate"
	secretValue := "the-actual-secret-value"
	p := &fakeProvider{errs: map[string]error{
		secretSuffix: errors.New(secretValue),
	}}
	_, err := Hydrate(context.Background(), p, coord(secretSuffix))
	if err == nil {
		t.Fatal("expected error")
	}
	// The typed *Error's own message must not carry the coordinate. (The
	// wrapped cause may, but callers must not print it wholesale.)
	e, _ := AsError(err)
	if strings.Contains(e.Message, secretSuffix) {
		t.Errorf("error message leaked coordinate: %q", e.Message)
	}
}
