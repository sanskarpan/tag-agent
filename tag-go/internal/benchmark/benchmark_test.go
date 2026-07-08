package benchmark

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/tag-agent/tag/internal/llm"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/bench.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestLoadDefaultSuite(t *testing.T) {
	s, err := LoadSuite("")
	if err != nil {
		t.Fatalf("LoadSuite: %v", err)
	}
	if s.Name != "default" || len(s.Cases) == 0 {
		t.Fatalf("unexpected suite: %+v", s)
	}
}

// TestRunEchoScoring verifies pass/fail scoring against the echo provider, which
// streams the prompt back verbatim: a case whose expected text is contained in
// the prompt passes; one whose expected text is absent fails.
func TestRunEchoScoring(t *testing.T) {
	db := openTestDB(t)
	r := &Runner{DB: db, Provider: llm.EchoProvider{}}
	suite := &Suite{Name: "unit", Cases: []Case{
		{ID: "hit", Prompt: "please say bench-ok now", Expected: "bench-ok"},
		{ID: "miss", Prompt: "there is no keyword here", Expected: "bench-ok"},
		{ID: "smoke", Prompt: "anything", Expected: ""},
	}}
	res, err := r.Run(context.Background(), suite)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Total != 3 || res.Passed != 2 || res.Failed != 1 {
		t.Fatalf("scoring wrong: total=%d passed=%d failed=%d", res.Total, res.Passed, res.Failed)
	}
	byID := map[string]bool{}
	for _, c := range res.Cases {
		byID[c.ID] = c.Pass
	}
	if !byID["hit"] || byID["miss"] || !byID["smoke"] {
		t.Fatalf("per-case scoring wrong: %+v", byID)
	}

	// Persisted and retrievable via List + Show (id-prefix).
	list, err := List(db, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != res.ID {
		t.Fatalf("List returned %+v", list)
	}
	got, err := Show(db, res.ID[:4])
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if got.ID != res.ID || len(got.Cases) != 3 {
		t.Fatalf("Show returned %+v", got)
	}
}

func TestShowNotFound(t *testing.T) {
	db := openTestDB(t)
	if _, err := Show(db, "nope"); err == nil {
		t.Fatal("expected not-found error")
	}
}
