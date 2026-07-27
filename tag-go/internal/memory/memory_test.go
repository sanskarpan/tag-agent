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

// TestSearchRecallCJKAndPartialWord is the regression guard for the Go
// equivalent of Python issue #567: Search only fell back to LIKE when the FTS
// query ERRORED, so a zero-row FTS result (CJK, which FTS5's default tokenizer
// cannot tokenize, and partial words, which it only matches whole) was accepted
// as "no matches" and recall was silently lost.
func TestSearchRecallCJKAndPartialWord(t *testing.T) {
	db := testDB(t)
	if _, err := Add(db.DB, "p", "数据库使用 PostgreSQL", "fact", 1.0); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(db.DB, "p", "Kubernetes deployment strategy", "fact", 1.0); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ query, want string }{
		{"数据库", "数据库使用 PostgreSQL"},                      // CJK substring
		{"据库使", "数据库使用 PostgreSQL"},                      // CJK mid-string substring
		{"deploy", "Kubernetes deployment strategy"},     // partial word
		{"DEPLOY", "Kubernetes deployment strategy"},     // ASCII case-insensitive
		{"Kubernetes", "Kubernetes deployment strategy"}, // whole word (FTS path)
	}
	for _, c := range cases {
		res, err := Search(db.DB, "p", c.query, 10, "")
		if err != nil {
			t.Fatalf("search %q: %v", c.query, err)
		}
		if len(res) != 1 || res[0].Content != c.want {
			t.Errorf("search %q = %d hits %+v, want exactly %q", c.query, len(res), res, c.want)
		}
	}
}

// TestSearchEscapesLikeWildcards guards the escaping half of the Python parity:
// the LIKE supplement must treat %, _ and \ in the user's query as LITERAL
// characters, not wildcards, so `mem search %` cannot match everything.
func TestSearchEscapesLikeWildcards(t *testing.T) {
	db := testDB(t)
	Add(db.DB, "p", "alpha", "fact", 1.0)
	Add(db.DB, "p", "beta", "fact", 1.0)
	Add(db.DB, "p", "literal 50% off", "fact", 1.0)
	Add(db.DB, "p", "snake_case name", "fact", 1.0)

	if res, _ := Search(db.DB, "p", "%", 10, ""); len(res) != 1 || res[0].Content != "literal 50% off" {
		t.Errorf("search %%: got %d %+v, want only the literal-%% row", len(res), res)
	}
	if res, _ := Search(db.DB, "p", "_", 10, ""); len(res) != 1 || res[0].Content != "snake_case name" {
		t.Errorf("search _: got %d %+v, want only the snake_case row", len(res), res)
	}
	if res, _ := Search(db.DB, "p", "e_a", 10, ""); len(res) != 0 {
		t.Errorf("search e_a should not wildcard-match 'beta': %+v", res)
	}
}

// TestSearchTypeFilterAppliesToLikeHits ensures --type still narrows results
// once the LIKE supplement is in play (Python passes memory_type through).
func TestSearchTypeFilterAppliesToLikeHits(t *testing.T) {
	db := testDB(t)
	Add(db.DB, "p", "deployment via helm", "fact", 1.0)
	Add(db.DB, "p", "deployment must be blue/green", "convention", 1.0)
	res, err := Search(db.DB, "p", "deploy", 10, "convention")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].MemoryType != "convention" {
		t.Errorf("typed search = %+v, want the single convention row", res)
	}
}
