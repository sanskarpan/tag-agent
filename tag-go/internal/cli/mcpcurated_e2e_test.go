package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PRD-076: `mcp-registry add-curated` installs a curated BUNDLE of MCP servers
// in one verb, following the `plugin install` precedent: native record + enable
// (no npm/pip), and an honesty guard that refuses — naming the exact missing
// variable — when a selected server declares a requires_env secret that is not
// set. These tests fail before the verb exists (`unknown command`, exit 2).

// readProfileMCP returns the mcp_servers keys recorded in a profile's runtime
// config.yaml. The file lives under TAG_HOME at a path the CLI owns
// (runtime/home/.hermes/profiles/<p>/config.yaml), so the test finds it by
// walking rather than hard-coding a layout it does not control.
func readProfileMCP(t *testing.T, home, profile string) []string {
	t.Helper()
	var path string
	_ = filepath.Walk(home, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint:nilerr // a partially built home is not a test failure
		}
		if filepath.Base(p) == "config.yaml" && filepath.Base(filepath.Dir(p)) == profile {
			path = p
		}
		return nil
	})
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	// The server names are the first-level keys under `mcp_servers:`. A tiny
	// indentation-aware scanner keeps this test free of a YAML dependency.
	var out []string
	indent := -1
	in := false
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(ln, "mcp_servers:") {
			in = true
			continue
		}
		if !in || strings.TrimSpace(ln) == "" {
			continue
		}
		lead := len(ln) - len(strings.TrimLeft(ln, " "))
		if lead == 0 {
			in = false
			continue
		}
		if indent == -1 {
			indent = lead
		}
		if lead == indent && strings.HasSuffix(strings.TrimSpace(ln), ":") {
			out = append(out, strings.TrimSuffix(strings.TrimSpace(ln), ":"))
		}
	}
	return out
}

// TestE2EMCPAddCuratedRefusesMissingEnv is the honesty guard: the curated
// catalog's `web` category contains mcp-brave-search, which declares
// requires_env: [BRAVE_API_KEY]. With that variable absent the bundle must be
// refused by name, and NOTHING may be recorded.
func TestE2EMCPAddCuratedRefusesMissingEnv(t *testing.T) {
	h := newHome(t)
	t.Setenv("BRAVE_API_KEY", "")
	os.Unsetenv("BRAVE_API_KEY")

	out, code := run(t, h, "mcp-registry", "add-curated", "--category", "web", "--profile", "researcher")
	if code == 0 {
		t.Fatalf("add-curated with a missing requires_env must fail, got exit 0: %s", out)
	}
	if !strings.Contains(out, "BRAVE_API_KEY") {
		t.Errorf("refusal must name the exact missing variable BRAVE_API_KEY, got: %s", out)
	}
	if !strings.Contains(out, "mcp-brave-search") {
		t.Errorf("refusal must name the server that needs it, got: %s", out)
	}
	// No partial install: not one server from the bundle may be recorded.
	if got := readProfileMCP(t, h, "researcher"); len(got) != 0 {
		t.Errorf("refused bundle must record nothing, got mcp_servers=%v", got)
	}
	if lo, _ := run(t, h, "mcp-registry", "list-curated", "--profile", "researcher", "--json"); strings.Contains(lo, `"installed": true`) {
		t.Errorf("refused bundle must record nothing in the install ledger: %s", lo)
	}
}

// TestE2EMCPAddCuratedRecordsExactlyTheBundle installs a category whose servers
// declare no secrets and asserts the recorded set is EXACTLY that category —
// no more, no less.
func TestE2EMCPAddCuratedRecordsExactlyTheBundle(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "mcp-registry", "add-curated", "--category", "database",
		"--profile", "coder", "--skip-missing-env", "--json")
	if code != 0 {
		t.Fatalf("add-curated --category database: exit %d: %s", code, out)
	}
	var payload struct {
		Profile   string   `json:"profile"`
		Installed []string `json:"installed"`
		Skipped   []struct {
			Name    string   `json:"name"`
			Missing []string `json:"missing_env"`
		} `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("add-curated --json is not JSON: %v: %s", err, out)
	}
	// The embedded catalog's `database` category is {mcp-postgres (DATABASE_URL),
	// mcp-sqlite (no secrets)}. With --skip-missing-env exactly mcp-sqlite lands.
	if len(payload.Installed) != 1 || payload.Installed[0] != "mcp-sqlite" {
		t.Fatalf("installed = %v, want exactly [mcp-sqlite]", payload.Installed)
	}
	if len(payload.Skipped) != 1 || payload.Skipped[0].Name != "mcp-postgres" ||
		len(payload.Skipped[0].Missing) != 1 || payload.Skipped[0].Missing[0] != "DATABASE_URL" {
		t.Fatalf("skipped = %+v, want mcp-postgres missing DATABASE_URL", payload.Skipped)
	}
	got := readProfileMCP(t, h, "coder")
	if len(got) != 1 || got[0] != "mcp-sqlite" {
		t.Fatalf("profile mcp_servers = %v, want exactly [mcp-sqlite]", got)
	}
}

// TestE2EMCPAddCuratedDryRunWritesNothing pins --dry-run: exit 0, a plan, and
// no state change at all.
func TestE2EMCPAddCuratedDryRunWritesNothing(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "mcp-registry", "add-curated", "--category", "reasoning",
		"--profile", "coder", "--dry-run")
	if code != 0 {
		t.Fatalf("--dry-run should exit 0, got %d: %s", code, out)
	}
	if !strings.Contains(out, "mcp-sequentialthinking") {
		t.Errorf("--dry-run must print the plan, got: %s", out)
	}
	if got := readProfileMCP(t, h, "coder"); len(got) != 0 {
		t.Errorf("--dry-run must write nothing, got %v", got)
	}
}

// TestE2EMCPAddCuratedUnknownCategoryIsUsageError pins the exit-code contract:
// a bad flag value is a usage error (2), not a runtime error (1).
func TestE2EMCPAddCuratedUnknownCategoryIsUsageError(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "mcp-registry", "add-curated", "--category", "nope", "--profile", "coder")
	if code != 2 {
		t.Fatalf("unknown --category should exit 2, got %d: %s", code, out)
	}
	// The message must name the categories the embedded catalog actually has,
	// so the user can fix the flag without reading the YAML.
	if !strings.Contains(out, "database") || !strings.Contains(out, "web") {
		t.Errorf("unknown --category error must list the valid categories, got: %s", out)
	}
}

// TestE2EMCPListCuratedJSONEmptyIsArray pins the --json contract: an empty
// selection is [] rather than null.
func TestE2EMCPListCuratedJSONEmptyIsArray(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "mcp-registry", "list-curated", "--category", "no-such-category", "--json")
	if code != 0 {
		t.Fatalf("list-curated --json: exit %d: %s", code, out)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("empty list-curated --json = %q, want []", strings.TrimSpace(out))
	}
}
