package cli_test

import (
	"os"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// TestRunRecordsCost: a run with token usage must persist a non-zero cost, so
// `runs show` matches `costs --run-id` instead of printing 0 (#751). RED against
// pre-fix code, whose INSERT omitted estimated_cost_usd.
func TestRunRecordsCost(t *testing.T) {
	h := newHome(t)
	if _, code := run(t, h, "run", "hello world cost test"); code != 0 {
		t.Fatalf("run failed: %d", code)
	}
	out, _ := run(t, h, "--json", "runs", "list")
	// The run row must carry a positive estimated cost; check runs show text.
	show, _ := run(t, h, "runs", "list")
	_ = out
	_ = show
	// Find the run id from the json list.
	var rows []map[string]any
	if err := yaml.Unmarshal([]byte(out), &rows); err != nil || len(rows) == 0 {
		t.Fatalf("no runs listed: %v / %s", err, out)
	}
	id, _ := rows[0]["id"].(string)
	detail, code := run(t, h, "runs", "show", id)
	if code != 0 {
		t.Fatalf("runs show failed: %d", code)
	}
	// The echo provider produces token usage, so cost must not be a bare 0.
	for _, ln := range strings.Split(detail, "\n") {
		if strings.HasPrefix(ln, "Cost (usd):") {
			if strings.Contains(ln, " 0\n") || strings.TrimSpace(strings.TrimPrefix(ln, "Cost (usd):")) == "0" {
				t.Errorf("run with token usage must record a non-zero cost, got: %q", ln)
			}
		}
	}
}

// TestTemplateExportIncludesConfig: export must capture the profile's config
// (model/display) from tag.yaml, not emit an empty config: {} (#753). RED
// against pre-fix code, which read only the (absent) rendered config.yaml.
func TestTemplateExportIncludesConfig(t *testing.T) {
	h := newHome(t)
	tf := t.TempDir() + "/t.yaml"
	if _, code := run(t, h, "template", "export", "--profile", "coder", "--output", tf); code != 0 {
		t.Fatalf("export failed: %d", code)
	}
	b, err := os.ReadFile(tf)
	if err != nil {
		t.Fatal(err)
	}
	var tmpl map[string]any
	if err := yaml.Unmarshal(b, &tmpl); err != nil {
		t.Fatal(err)
	}
	cfg, _ := tmpl["config"].(map[string]any)
	if len(cfg) == 0 {
		t.Errorf("export must include the profile config, got empty: %s", b)
	}
	if _, ok := cfg["model"]; !ok {
		t.Errorf("exported config must include the model, got: %v", cfg)
	}
}
