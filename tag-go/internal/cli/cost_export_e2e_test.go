package cli_test

import (
	"encoding/json"
	"math"
	"os"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// TestRunCostIsConsistentWithCosts: the cost `runs show` reports must equal what
// `costs --run-id` computes — the run INSERT used to omit estimated_cost_usd, so
// `runs show` printed 0 while `costs` had the real figure (#751). Asserting
// CONSISTENCY (not a magnitude) keeps this robust to #742 (echo attribution):
// if echo billing is later zeroed, both surfaces move to 0 together.
func TestRunCostIsConsistentWithCosts(t *testing.T) {
	h := newHome(t)
	if _, code := run(t, h, "run", "cost consistency check"); code != 0 {
		t.Fatalf("run failed: %d", code)
	}
	listOut, _ := run(t, h, "--json", "runs", "list")
	var list []map[string]any
	if err := json.Unmarshal([]byte(listOut), &list); err != nil || len(list) == 0 {
		t.Fatalf("no runs: %v / %s", err, listOut)
	}
	id, _ := list[0]["id"].(string)

	showOut, _ := run(t, h, "--json", "runs", "show", id)
	var show map[string]any
	if err := json.Unmarshal([]byte(showOut), &show); err != nil {
		t.Fatalf("runs show json: %v / %s", err, showOut)
	}
	runCost, _ := show["estimated_cost_usd"].(float64)

	costOut, _ := run(t, h, "--json", "costs", "--run-id", id)
	var cost map[string]any
	if err := json.Unmarshal([]byte(costOut), &cost); err != nil {
		t.Fatalf("costs json: %v / %s", err, costOut)
	}
	var total float64
	if rows, ok := cost["rows"].([]any); ok {
		for _, r := range rows {
			if rm, ok := r.(map[string]any); ok {
				if c, ok := rm["cost_usd"].(float64); ok {
					total += c
				}
			}
		}
	}
	if math.Abs(runCost-total) > 1e-9 {
		t.Errorf("runs show cost %v must equal costs --run-id total %v", runCost, total)
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
