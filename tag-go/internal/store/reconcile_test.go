package store

import (
	"database/sql"
	"testing"
)

// openOldSchema creates a database whose `runs` table is the shape an older
// release left behind: no duration_ms, no completed_at.
func openOldSchema(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE runs (
		id TEXT PRIMARY KEY, created_at TEXT NOT NULL, kind TEXT NOT NULL, task_type TEXT NOT NULL,
		execution TEXT NOT NULL, master_profile TEXT NOT NULL, board TEXT NOT NULL, prompt TEXT NOT NULL,
		route_json TEXT NOT NULL, status TEXT NOT NULL, metadata_json TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO runs VALUES('r1','now','run','mixed','local','default','b','p','{}','done','{}')`); err != nil {
		t.Fatal(err)
	}
}

// TestOpenMigratesSchemaDrift is the #664 regression: CREATE TABLE IF NOT EXISTS
// skips an existing table wholesale, so a TAG_HOME created by an older schema
// kept a runs table with no duration_ms and every write to it failed with
// "table runs has no column named duration_ms".
func TestOpenMigratesSchemaDrift(t *testing.T) {
	path := t.TempDir() + "/tag.sqlite3"
	openOldSchema(t, path)

	db, err := OpenPath(path)
	if err != nil {
		t.Fatalf("opening a drifted database must succeed: %v", err)
	}
	defer db.Close()

	cols, err := existingColumns(db.DB, "runs")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"duration_ms", "completed_at", "prompt_tokens", "cache_read_tokens"} {
		if !cols[want] {
			t.Errorf("runs.%s was not added by the migration", want)
		}
	}

	// The actual failing operation: record a run using the newer columns.
	if _, err := db.Exec(`INSERT INTO runs(id,created_at,kind,task_type,execution,master_profile,board,prompt,route_json,status,metadata_json,duration_ms,completed_at)
		VALUES('r2','now','run','mixed','local','default','b','p','{}','done','{}',42,'now')`); err != nil {
		t.Fatalf("recording a run after migration: %v", err)
	}

	// Pre-existing data survives.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&n); err != nil || n != 2 {
		t.Errorf("row count = %d (err %v), want 2 — migration must not drop data", n, err)
	}
}

// Reconciliation must be a no-op on a current database, and safe to repeat.
func TestReconcileIsIdempotent(t *testing.T) {
	path := t.TempDir() + "/tag.sqlite3"
	db, err := OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	before, err := existingColumns(db.DB, "runs")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := reconcileColumns(db.DB); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	after, err := existingColumns(db.DB, "runs")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Errorf("column count changed on a current database: %d -> %d", len(before), len(after))
	}
}

// The DDL parser must read plain columns and skip table-level constraints,
// because a wrong ALTER corrupts the store where a missed one merely leaves the
// original error.
func TestParseSchemaTablesSkipsConstraints(t *testing.T) {
	ddl := `CREATE TABLE IF NOT EXISTS t (
		id TEXT PRIMARY KEY,
		amount REAL NOT NULL DEFAULT 0.0,
		note TEXT,
		UNIQUE(id, note),
		FOREIGN KEY(id) REFERENCES other(id)
	);`
	got := parseSchemaTables(ddl)
	if len(got) != 1 {
		t.Fatalf("expected one table, got %d", len(got))
	}
	var names []string
	for _, c := range got[0].columns {
		names = append(names, c.name)
	}
	if len(names) != 3 || names[0] != "id" || names[1] != "amount" || names[2] != "note" {
		t.Errorf("columns = %v, want [id amount note]", names)
	}
}

// Every table in the embedded schema must be parseable, or reconciliation
// silently covers only part of the store.
func TestEmbeddedSchemaIsFullyParsed(t *testing.T) {
	tables := parseSchemaTables(schemaSQL)
	if len(tables) < 10 {
		t.Fatalf("only %d tables parsed from the embedded schema — the parser is missing shapes", len(tables))
	}
	seen := map[string]bool{}
	for _, tb := range tables {
		seen[tb.name] = true
	}
	for _, want := range []string{"runs", "steps"} {
		if !seen[want] {
			t.Errorf("table %q not parsed from the embedded schema", want)
		}
	}
}
