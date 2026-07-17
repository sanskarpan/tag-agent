package cli

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/tag-agent/tag/internal/store"
)

func aopSeedRun(t *testing.T, db *store.DB, id, profile, status string, pt, ct int64, cost float64) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(`INSERT INTO runs(
		id, created_at, kind, task_type, execution, master_profile, board, prompt,
		route_json, status, model_id, prompt_tokens, completion_tokens, estimated_cost_usd)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, now, "chat", "mixed", "single", profile, "main", "hi",
		"{}", status, "openai/gpt-4o", pt, ct, cost)
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
}

func TestAopSummarize(t *testing.T) {
	db, err := store.OpenPath(filepath.Join(t.TempDir(), "x.sqlite3"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	aopSeedRun(t, db, "r1", "coder", "done", 100, 50, 0.001)
	aopSeedRun(t, db, "r2", "coder", "failed", 200, 0, 0.002)
	aopSeedRun(t, db, "r3", "reviewer", "done", 10, 5, 0.0005)

	sum, err := aopSummarize(db)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if sum.TotalRuns != 3 {
		t.Errorf("TotalRuns=%d want 3", sum.TotalRuns)
	}
	if sum.TotalTokens != 365 {
		t.Errorf("TotalTokens=%d want 365", sum.TotalTokens)
	}
	if sum.PromptTokens != 310 || sum.CompletionTokens != 55 {
		t.Errorf("tokens prompt=%d completion=%d want 310/55", sum.PromptTokens, sum.CompletionTokens)
	}
	if sum.Statuses["done"] != 2 || sum.Statuses["failed"] != 1 {
		t.Errorf("statuses=%v want done=2 failed=1", sum.Statuses)
	}
	if len(sum.Profiles) != 2 {
		t.Fatalf("profiles=%d want 2", len(sum.Profiles))
	}
	// sorted: coder first
	coder := sum.Profiles[0]
	if coder.Profile != "coder" || coder.Runs != 2 || coder.TotalTokens != 350 {
		t.Errorf("coder rollup wrong: %+v", coder)
	}
	if coder.Statuses["done"] != 1 || coder.Statuses["failed"] != 1 {
		t.Errorf("coder statuses=%v", coder.Statuses)
	}
}
