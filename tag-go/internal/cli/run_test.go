package cli

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/tag-agent/tag/internal/config"
	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/store"
)

// rfSeed inserts a route_fallbacks edge.
func rfSeed(t *testing.T, db *store.DB, profile, primary, fallback, cond string, priority int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO route_fallbacks(id,profile,primary_model,fallback_model,condition,priority,enabled,created_at)
		VALUES(?,?,?,?,?,?,1,?)`,
		primary+"->"+fallback, profile, primary, fallback, cond, priority, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("seed route_fallback %s->%s: %v", primary, fallback, err)
	}
}

// TestBuildFallbackProviderMixedPrefixMultiHop covers #564: a depth-2 fallback
// chain whose edges use DIFFERENT prefix forms for the same logical model must
// still reach the deepest step. Before the fix, walk() matched child edges only
// by the parent's exact stored fallback_model string, so a chain that stored
// gpt-mid prefixed at one hop and bare at the next dead-linked.
func TestBuildFallbackProviderMixedPrefixMultiHop(t *testing.T) {
	db, err := store.OpenPath(filepath.Join(t.TempDir(), "fb.sqlite3"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	const profile = "coder"
	// Primary is a bare model with a configured provider slug "openai".
	// Hop 1: gpt-primary -> openai/gpt-mid   (PREFIXED fallback form)
	// Hop 2: gpt-mid     -> gpt-deep         (edge keyed by the BARE form)
	// The mismatch (openai/gpt-mid stored at hop 1, gpt-mid keyed at hop 2) is
	// exactly what dead-linked before the fix.
	rfSeed(t, db, profile, "gpt-primary", "openai/gpt-mid", "always", 1)
	rfSeed(t, db, profile, "gpt-mid", "gpt-deep", "always", 1)

	app := &App{
		DB: db,
		Cfg: &config.Config{Data: map[string]any{
			"defaults": map[string]any{"master_profile": profile},
			"profiles": map[string]any{
				profile: map[string]any{
					"config": map[string]any{
						"model": map[string]any{"default": "gpt-primary", "provider": "openai"},
					},
				},
			},
		}},
	}

	fp, err := buildFallbackProvider(app, llm.Registry["openai"], "openai", "gpt-primary", profile)
	if err != nil {
		t.Fatalf("buildFallbackProvider: %v", err)
	}
	if fp == nil {
		t.Fatal("expected a fallback provider, got nil")
	}
	// Collect the (bare) models across all steps.
	got := map[string]bool{}
	for _, s := range fp.Steps {
		got[s.Model] = true
	}
	if !got["gpt-primary"] {
		t.Errorf("primary step missing: %+v", fp.Steps)
	}
	if !got["gpt-mid"] {
		t.Errorf("hop-1 step (gpt-mid) missing: %+v", fp.Steps)
	}
	if !got["gpt-deep"] {
		t.Errorf("hop-2 step (gpt-deep) unreachable — mixed-prefix multi-hop dead-linked (#564): %+v", fp.Steps)
	}
	if len(fp.Steps) != 3 {
		t.Errorf("want 3 steps (primary, mid, deep), got %d: %+v", len(fp.Steps), fp.Steps)
	}
}
