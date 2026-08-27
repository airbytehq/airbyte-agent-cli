package localexec

import (
	"reflect"
	"strings"
	"testing"
)

func TestTemplateLookupConfigPath(t *testing.T) {
	config := map[string]any{
		"credentials": map[string]any{
			"api_key": "synthetic-key",
			"nested":  map[string]any{"deep": "value"},
		},
		"nullable": nil,
	}
	cases := []struct {
		path    string
		want    any
		present bool
	}{
		{"credentials.api_key", "synthetic-key", true},
		{"credentials.nested.deep", "value", true},
		{"nullable", nil, true},
		{"credentials.missing", nil, false},
		{"missing.path", nil, false},
		{"", nil, false},
	}
	for _, tc := range cases {
		got, ok := lookupConfigPath(config, tc.path)
		if ok != tc.present || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("lookupConfigPath(%q) = %v,%v want %v,%v", tc.path, got, ok, tc.want, tc.present)
		}
	}
}

func TestTemplateResolveServerURLVariables(t *testing.T) {
	server := Server{
		URL: "https://api.example.test/{version}",
		Variables: map[string]ServerVariable{
			"version": {Default: "v1", Enum: []string{"v1", "v2"}},
		},
	}

	// Default is used when nothing overrides.
	got, err := resolveServerURL(server, nil, nil)
	if err != nil || got != "https://api.example.test/v1" {
		t.Fatalf("default: got %q err %v", got, err)
	}

	// Config overrides default.
	got, err = resolveServerURL(server, map[string]any{"version": "v2"}, nil)
	if err != nil || got != "https://api.example.test/v2" {
		t.Fatalf("config override: got %q err %v", got, err)
	}

	// Params override config.
	got, err = resolveServerURL(server, map[string]any{"version": "v2"}, map[string]any{"version": "v1"})
	if err != nil || got != "https://api.example.test/v1" {
		t.Fatalf("param override: got %q err %v", got, err)
	}

	// Enum violation rejected.
	if _, err := resolveServerURL(server, map[string]any{"version": "v9"}, nil); err == nil {
		t.Fatal("expected enum violation")
	}
}

func TestTemplateResolveServerURLScoped(t *testing.T) {
	server := Server{
		URL: "https://{subdomain}.example.test/{region}",
		Variables: map[string]ServerVariable{
			"subdomain": {Default: "app", Scope: "credentials.account.subdomain"},
			"region":    {Default: "us", Scope: "settings.region"},
		},
	}
	config := map[string]any{
		"credentials": map[string]any{
			"account": map[string]any{"subdomain": "acme"},
		},
		"settings": map[string]any{"region": "eu"},
	}
	got, err := resolveServerURL(server, config, nil)
	if err != nil || got != "https://acme.example.test/eu" {
		t.Fatalf("scoped: got %q err %v", got, err)
	}
}

func TestTemplateResolveServerURLNullableAndMissing(t *testing.T) {
	// A nullable variable with no value resolves to empty.
	nullable := Server{
		URL:       "https://api.example.test/{maybe}suffix",
		Variables: map[string]ServerVariable{"maybe": {Nullable: true}},
	}
	got, err := resolveServerURL(nullable, nil, nil)
	if err != nil || got != "https://api.example.test/suffix" {
		t.Fatalf("nullable: got %q err %v", got, err)
	}

	// A non-nullable variable with no value and no default is an error.
	missing := Server{
		URL:       "https://api.example.test/{required}",
		Variables: map[string]ServerVariable{"required": {}},
	}
	if _, err := resolveServerURL(missing, nil, nil); err == nil {
		t.Fatal("expected missing-config error")
	}
}

func TestTemplateResolveServerURLPathInjectionPrevented(t *testing.T) {
	server := Server{
		URL:       "https://api.example.test/{tenant}/data",
		Variables: map[string]ServerVariable{"tenant": {Default: "t"}},
	}
	got, err := resolveServerURL(server, map[string]any{"tenant": "a/../../admin"}, nil)
	if err != nil {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(got, "/../") {
		t.Fatalf("path traversal not neutralized: %q", got)
	}
	if got != "https://api.example.test/a%2F..%2F..%2Fadmin/data" {
		t.Fatalf("unexpected encoding: %q", got)
	}
}

func TestTemplateResolveServerURLPlaceholderLimit(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("https://api.example.test")
	vars := map[string]ServerVariable{}
	for i := 0; i < maxTemplatePlaceholders+1; i++ {
		name := "v" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		sb.WriteString("/{" + name + "}")
		vars[name] = ServerVariable{Default: "x"}
	}
	server := Server{URL: sb.String(), Variables: vars}
	if _, err := resolveServerURL(server, nil, nil); err == nil {
		t.Fatal("expected placeholder limit error")
	}
}

func TestTemplateSerializeDeepObject(t *testing.T) {
	pairs, err := serializeDeepObject("filter", map[string]any{
		"status": "active",
		"score":  float64(3),
	})
	if err != nil {
		t.Fatalf("err %v", err)
	}
	want := []deepObjectPair{
		{Key: "filter[score]", Value: "3"},
		{Key: "filter[status]", Value: "active"},
	}
	if !reflect.DeepEqual(pairs, want) {
		t.Fatalf("got %#v want %#v", pairs, want)
	}

	// Nested objects are rejected.
	if _, err := serializeDeepObject("filter", map[string]any{"nested": map[string]any{"a": 1}}); err == nil {
		t.Fatal("expected nested rejection")
	}
}
