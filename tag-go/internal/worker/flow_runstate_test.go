package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestRunStateSurvivesMissingResultColumn: RunState reads the inline `result`
// column, which the worker adds lazily on first completion. On a DB where a
// flow job was queued but no worker has finished one, that column is absent —
// and the query used to fail with "no such column: result", crashing every
// `dag state` on a submitted-but-not-run DAG (#735). RunState must ensure the
// column, like it ensures flow_json. RED against pre-fix code.
func TestRunStateSurvivesMissingResultColumn(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "q.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// queue_jobs deliberately WITHOUT a `result` column, with a queued flow job.
	if _, err := db.Exec(`CREATE TABLE queue_jobs (
		id TEXT PRIMARY KEY, status TEXT NOT NULL DEFAULT 'queued', flow_json TEXT)`); err != nil {
		t.Fatal(err)
	}
	fj, _ := json.Marshal(Flow{RunID: "r1", Index: 0, Output: "out"})
	if _, err := db.Exec(`INSERT INTO queue_jobs(id,status,flow_json) VALUES('j1','done',?)`, string(fj)); err != nil {
		t.Fatal(err)
	}

	st, err := RunState(context.Background(), db, "r1")
	if err != nil {
		t.Fatalf("RunState must not crash on a DB with no result column, got: %v", err)
	}
	if _, ok := st["out"]; !ok {
		t.Errorf("expected the flow job's output key in the state, got %v", st)
	}
}
