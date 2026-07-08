package memory

import (
	"path/filepath"
	"testing"

	"github.com/tag-agent/tag/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.OpenPath(filepath.Join(t.TempDir(), "test.sqlite3"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestAddValidatesConfidence(t *testing.T) {
	db := testDB(t)
	if _, err := Add(db.DB, "p", "hello", "fact", 0); err == nil {
		t.Error("confidence 0 should be rejected")
	}
	if _, err := Add(db.DB, "p", "hello", "fact", 1.5); err == nil {
		t.Error("confidence 1.5 should be rejected")
	}
	if _, err := Add(db.DB, "p", "", "fact", 1.0); err == nil {
		t.Error("empty content should be rejected")
	}
	if _, err := Add(db.DB, "p", "valid", "fact", 0.9); err != nil {
		t.Errorf("valid add failed: %v", err)
	}
}

func TestAddListSearchRoundTrip(t *testing.T) {
	db := testDB(t)
	Add(db.DB, "p", "the sky is blue today", "fact", 1.0)
	Add(db.DB, "p", "always use tabs not spaces", "convention", 0.9)
	list, err := List(db.DB, "p", "", 0)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: got %d err %v", len(list), err)
	}
	res, err := Search(db.DB, "p", "sky", 10, "")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) != 1 || res[0].Content != "the sky is blue today" {
		t.Errorf("search 'sky' = %+v", res)
	}
}

func TestForget(t *testing.T) {
	db := testDB(t)
	id, _ := Add(db.DB, "p", "temp", "fact", 1.0)
	ok, _ := Forget(db.DB, "p", id)
	if !ok {
		t.Error("forget should return true")
	}
	ok, _ = Forget(db.DB, "p", id)
	if ok {
		t.Error("double-forget should return false")
	}
}

func TestConfidenceDecayApplied(t *testing.T) {
	db := testDB(t)
	Add(db.DB, "p", "recent", "fact", 1.0)
	list, _ := List(db.DB, "p", "", 0)
	if len(list) != 1 {
		t.Fatal("expected 1")
	}
	// fresh memory: decay factor ~1, confidence close to base
	if list[0].Confidence > 1.0 || list[0].Confidence < 0.99 {
		t.Errorf("fresh confidence should be ~1.0, got %g", list[0].Confidence)
	}
}
