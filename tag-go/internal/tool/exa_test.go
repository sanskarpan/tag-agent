package tool

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tag-agent/tag/internal/agent"
)

// mockExa emulates the Exa /search endpoint, recording the key + query it saw.
func mockExa(t *testing.T, results []map[string]any, gotKey, gotQuery *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotKey != nil {
			*gotKey = r.Header.Get("x-api-key")
		}
		body, _ := io.ReadAll(r.Body)
		var in struct {
			Query string `json:"query"`
		}
		json.Unmarshal(body, &in)
		if gotQuery != nil {
			*gotQuery = in.Query
		}
		if !strings.HasSuffix(r.URL.Path, "/search") {
			http.Error(w, "nf", 404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestExaSearchFormatsResults(t *testing.T) {
	var gotKey, gotQuery string
	srv := mockExa(t, []map[string]any{
		{"title": "Go 1.24 released", "url": "https://go.dev/blog", "text": "The Go team announced   version   1.24 with generics improvements."},
		{"title": "Second", "url": "https://example.com"},
	}, &gotKey, &gotQuery)

	tl := exaSearchTool(Options{ExaAPIKey: "exa-key", ExaBaseURL: srv.URL})
	out, err := tl.Exec(context.Background(), map[string]any{"query": "go 1.24"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if gotKey != "exa-key" {
		t.Errorf("x-api-key not sent, got %q", gotKey)
	}
	if gotQuery != "go 1.24" {
		t.Errorf("query not forwarded, got %q", gotQuery)
	}
	if !strings.Contains(out, "1. Go 1.24 released") || !strings.Contains(out, "https://go.dev/blog") {
		t.Errorf("result 1 missing: %s", out)
	}
	if !strings.Contains(out, "generics improvements") {
		t.Errorf("extract text missing/uncollapsed: %s", out)
	}
	if !strings.Contains(out, "2. Second") {
		t.Errorf("result 2 missing: %s", out)
	}
}

func TestExaSearchRequiresKey(t *testing.T) {
	tl := exaSearchTool(Options{}) // no key
	t.Setenv("EXA_API_KEY", "")
	if _, err := tl.Exec(context.Background(), map[string]any{"query": "x"}); err == nil || !strings.Contains(err.Error(), "EXA_API_KEY") {
		t.Fatalf("expected a missing-key error, got %v", err)
	}
}

func TestExaSearchEmptyQuery(t *testing.T) {
	tl := exaSearchTool(Options{ExaAPIKey: "k"})
	if _, err := tl.Exec(context.Background(), map[string]any{"query": "  "}); err == nil {
		t.Error("empty query must error")
	}
}

func TestExaUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", 429)
	}))
	t.Cleanup(srv.Close)
	tl := exaSearchTool(Options{ExaAPIKey: "k", ExaBaseURL: srv.URL})
	if _, err := tl.Exec(context.Background(), map[string]any{"query": "x"}); err == nil || !strings.Contains(err.Error(), "exa API 429") {
		t.Fatalf("expected a labeled 429, got %v", err)
	}
}

// TestToolBudget verifies Register's Disabled gate and the Exa enable flag.
func TestToolBudget(t *testing.T) {
	names := func(opts Options) map[string]bool {
		reg := agent.NewRegistry()
		Register(reg, opts)
		m := map[string]bool{}
		for _, d := range reg.Defs() {
			m[d.Name] = true
		}
		return m
	}

	// default: bash + 3 file tools, no web_search
	def := names(DefaultOptions())
	for _, want := range []string{"bash", "read_file", "write_file", "list_dir"} {
		if !def[want] {
			t.Errorf("default should include %q", want)
		}
	}
	if def["web_search"] {
		t.Error("web_search must be OFF by default")
	}

	// tool budget: disable bash + write_file
	trimmed := names(Options{Disabled: map[string]bool{"bash": true, "write_file": true}})
	if trimmed["bash"] || trimmed["write_file"] {
		t.Error("disabled tools must be omitted")
	}
	if !trimmed["read_file"] {
		t.Error("non-disabled tools must remain")
	}

	// enable Exa (with a key) adds web_search
	withExa := names(Options{EnableExa: true, ExaAPIKey: "k"})
	if !withExa["web_search"] {
		t.Error("EnableExa + key should add web_search")
	}
	// EnableExa without a key does NOT add it
	t.Setenv("EXA_API_KEY", "")
	if names(Options{EnableExa: true})["web_search"] {
		t.Error("web_search must not register without a key")
	}
}
