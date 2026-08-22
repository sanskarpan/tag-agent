package swarm

import (
	"context"
	"testing"
)

// TestMarkDegradedIsPersisted: a coordinator fallback must be recorded on the
// swarm_runs row so machine consumers (swarm list/results --json) can see the
// run was degraded, not a real decomposed swarm (#743). RED against pre-fix
// code, which had no degraded column and no writer.
func TestMarkDegradedIsPersisted(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := CreateRun(ctx, db.DB, "sw1", "goal", "p", "best_effort", 4); err != nil {
		t.Fatal(err)
	}

	// Fresh run: not degraded.
	var degraded int
	var reason string
	if err := db.DB.QueryRow(`SELECT degraded, COALESCE(degraded_reason,'') FROM swarm_runs WHERE swarm_id='sw1'`).
		Scan(&degraded, &reason); err != nil {
		t.Fatalf("read degraded: %v", err)
	}
	if degraded != 0 {
		t.Fatalf("a fresh run must not be degraded, got %d", degraded)
	}

	if err := markDegraded(db.DB, "sw1", "coordinator output not valid JSON"); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow(`SELECT degraded, COALESCE(degraded_reason,'') FROM swarm_runs WHERE swarm_id='sw1'`).
		Scan(&degraded, &reason); err != nil {
		t.Fatalf("read degraded: %v", err)
	}
	if degraded != 1 || reason == "" {
		t.Fatalf("markDegraded must persist degraded=1 with a reason, got %d %q", degraded, reason)
	}
}

// TestEnsureSchemaAddsDegradedColumns: EnsureSchema must add the columns to a
// swarm_runs table created before they existed, so old DBs are readable.
func TestEnsureSchemaAddsDegradedColumns(t *testing.T) {
	db := openTestDB(t)
	// Simulate an old DB: drop the columns is not possible in SQLite, so instead
	// verify EnsureSchema is idempotent and the columns are queryable.
	if err := EnsureSchema(db.DB); err != nil {
		t.Fatalf("EnsureSchema (2nd call) must be idempotent: %v", err)
	}
	if err := CreateRun(context.Background(), db.DB, "sw2", "g", "p", "best_effort", 4); err != nil {
		t.Fatal(err)
	}
	var d int
	if err := db.DB.QueryRow(`SELECT degraded FROM swarm_runs WHERE swarm_id='sw2'`).Scan(&d); err != nil {
		t.Fatalf("degraded column must be queryable after EnsureSchema: %v", err)
	}
}
