package memory

import (
	"testing"

	"github.com/tag-agent/tag/internal/store"
)

func TestJaccard(t *testing.T) {
	if j := jaccard("the quick brown fox", "the quick brown fox"); j != 1.0 {
		t.Errorf("identical should be 1.0, got %g", j)
	}
	if j := jaccard("a b c d", "a b c e"); j <= 0.5 || j >= 0.7 {
		t.Errorf("3/5 overlap should be 0.6, got %g", j)
	}
	if j := jaccard("", ""); j != 0.0 {
		t.Errorf("empty should be 0.0, got %g", j)
	}
}

func TestGCMergesNearDuplicates(t *testing.T) {
	db, err := store.OpenPath(t.TempDir() + "/gc.sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// two near-identical, one distinct
	Add(db.DB, "p", "deploy pipeline uses github actions and docker", "fact", 0.9)
	Add(db.DB, "p", "deploy pipeline uses github actions and docker containers", "fact", 0.6)
	Add(db.DB, "p", "completely unrelated trivia about cats", "fact", 0.9)

	r, err := RunGC(db.DB, "p", DefaultGCConfig())
	if err != nil {
		t.Fatal(err)
	}
	if r.MergedCount != 1 {
		t.Errorf("expected 1 merge, got %d", r.MergedCount)
	}
	// the higher-confidence duplicate survives
	mems, _ := List(db.DB, "p", "", 0)
	if len(mems) != 2 {
		t.Fatalf("expected 2 memories after merge, got %d", len(mems))
	}
	// audit row recorded
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM memory_gc_runs WHERE profile='p'`).Scan(&n)
	if n != 1 {
		t.Errorf("expected 1 gc audit row, got %d", n)
	}
}

func TestGCEvictsBelowFloor(t *testing.T) {
	db, err := store.OpenPath(t.TempDir() + "/gc2.sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// insert an already-decayed memory below the 0.05 floor by hand (old created_at)
	db.Exec(`INSERT INTO semantic_memories(id,profile,content,memory_type,confidence,created_at,accessed_at,access_count,source,tier)
		VALUES('old','p','ancient low note','other',0.02,'2000-01-01T00:00:00Z','2000-01-01T00:00:00Z',0,'manual','archival')`)
	r, err := RunGC(db.DB, "p", DefaultGCConfig())
	if err != nil {
		t.Fatal(err)
	}
	if r.EvictedCount != 1 {
		t.Errorf("expected 1 eviction (below 0.05 floor), got %d", r.EvictedCount)
	}
}

func TestConventionNeverDecays(t *testing.T) {
	// convention memories keep full confidence regardless of age
	got := effectiveConfidence(0.9, "convention", "2000-01-01T00:00:00Z")
	if got != 0.9 {
		t.Errorf("convention should not decay, got %g", got)
	}
	// fact decays over 25 years (90d half-life) to ~0
	decayed := effectiveConfidence(0.9, "fact", "2000-01-01T00:00:00Z")
	if decayed >= 0.9 || decayed < 0 {
		t.Errorf("fact should decay substantially, got %g", decayed)
	}
}

func TestEpisodeLifecycle(t *testing.T) {
	db, err := store.OpenPath(t.TempDir() + "/ep.sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := StartEpisode(db.DB, "p", "session one")
	if err != nil {
		t.Fatal(err)
	}
	// link a memory to the episode and verify it comes back
	mid, _ := Add(db.DB, "p", "a fact learned in the session", "fact", 0.9)
	if _, err := TagMemoryWithEpisode(db.DB, mid, id); err != nil {
		t.Fatal(err)
	}
	mems, err := EpisodeMemories(db.DB, id)
	if err != nil || len(mems) != 1 {
		t.Fatalf("expected 1 linked memory, got %d err=%v", len(mems), err)
	}
	eps, _ := ListEpisodes(db.DB, "p", 20)
	if len(eps) != 1 || eps[0].Status != "open" || eps[0].MemoryCount != 1 {
		t.Fatalf("episode list wrong: %+v", eps)
	}
	ended, err := EndEpisode(db.DB, id, "wrapped up")
	if err != nil || !ended {
		t.Fatalf("end episode: ended=%v err=%v", ended, err)
	}
	eps, _ = ListEpisodes(db.DB, "p", 20)
	if eps[0].Status != "closed" || eps[0].Summary != "wrapped up" {
		t.Errorf("episode should be closed with summary: %+v", eps[0])
	}
	if ok, _ := EndEpisode(db.DB, "missing", ""); ok {
		t.Error("ending a missing episode should return false")
	}
}

func TestFactVersioning(t *testing.T) {
	db, err := store.OpenPath(t.TempDir() + "/fact.sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, _ := Add(db.DB, "p", "capital is Alpha", "fact", 1.0)
	newID, err := UpdateFact(db.DB, id, "capital is Beta", "p", "correction")
	if err != nil {
		t.Fatal(err)
	}
	if newID == id {
		t.Error("update should produce a new id")
	}
	// only the new version is live
	live, _ := List(db.DB, "p", "", 0)
	if len(live) != 1 || live[0].Content != "capital is Beta" {
		t.Errorf("live should be the new version, got %+v", live)
	}
	// history links old->new
	hist, err := FactHistory(db.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) == 0 || hist[0].Content != "capital is Alpha" || hist[0].SuccessorID != newID {
		t.Errorf("history should record the superseded version: %+v", hist)
	}
	// empty content rejected
	if _, err := UpdateFact(db.DB, newID, "  ", "p", ""); err == nil {
		t.Error("empty content should error")
	}
	// missing memory rejected
	if _, err := UpdateFact(db.DB, "missing", "x", "p", ""); err == nil {
		t.Error("updating a missing fact should error")
	}
	// bad timestamp rejected
	if _, err := FactAt(db.DB, "p", "not-a-date"); err == nil {
		t.Error("bad timestamp should error")
	}
}

func TestSearchIsAndNotOr(t *testing.T) {
	db, _ := store.OpenPath(t.TempDir() + "/s.sqlite3")
	defer db.Close()
	Add(db.DB, "p", "alpha only note", "fact", 0.9)
	Add(db.DB, "p", "beta only note", "fact", 0.9)
	Add(db.DB, "p", "alpha and beta together", "fact", 0.9)
	// AND semantics: only the doc containing BOTH terms should match
	res, err := Search(db.DB, "p", "alpha beta", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || !contains(res[0].Content, "together") {
		t.Errorf("multi-term search should AND (expect 1 doc), got %d: %+v", len(res), res)
	}
}

func TestSearchBumpsAccessCount(t *testing.T) {
	db, _ := store.OpenPath(t.TempDir() + "/s2.sqlite3")
	defer db.Close()
	id, _ := Add(db.DB, "p", "searchable term", "fact", 0.9)
	Search(db.DB, "p", "searchable", 10, "")
	Search(db.DB, "p", "searchable", 10, "")
	var count int
	db.QueryRow(`SELECT access_count FROM semantic_memories WHERE id=?`, id).Scan(&count)
	if count != 2 {
		t.Errorf("access_count should be 2 after two searches, got %d", count)
	}
}

func TestAddValidatesMemoryType(t *testing.T) {
	db, _ := store.OpenPath(t.TempDir() + "/s3.sqlite3")
	defer db.Close()
	if _, err := Add(db.DB, "p", "x", "banana", 0.9); err == nil {
		t.Error("an invalid memory_type should be rejected")
	}
	if _, err := Add(db.DB, "p", "x", "decision", 0.9); err != nil {
		t.Errorf("a valid memory_type should be accepted: %v", err)
	}
}

func TestFactAtSeesArchivedHistory(t *testing.T) {
	db, _ := store.OpenPath(t.TempDir() + "/s4.sqlite3")
	defer db.Close()
	db.Exec(`INSERT INTO memory_fact_history(history_id,original_id,successor_id,profile,content,memory_type,confidence,source,valid_at,invalid_at,reason,archived_at) VALUES('h1','m1','m2','p','capital is Alpha','fact',1.0,'manual','2020-01-01T00:00:00Z','2022-01-01T00:00:00Z','','2022-01-01T00:00:00Z')`)
	db.Exec(`INSERT INTO semantic_memories(id,profile,content,memory_type,confidence,created_at,accessed_at,access_count,source,tier,valid_at) VALUES('m2','p','capital is Beta','fact',1.0,'2022-01-01T00:00:00Z','2022-01-01T00:00:00Z',0,'manual','archival','2022-01-01T00:00:00Z')`)
	// Querying in 2021 must surface the ARCHIVED Alpha, not the live Beta.
	facts, err := FactAt(db.DB, "p", "2021-06-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	foundAlpha, foundBeta := false, false
	for _, f := range facts {
		if contains(f.Content, "Alpha") {
			foundAlpha = true
		}
		if contains(f.Content, "Beta") {
			foundBeta = true
		}
	}
	if !foundAlpha {
		t.Errorf("FactAt(2021) must surface the archived Alpha version: %+v", facts)
	}
	if foundBeta {
		t.Errorf("FactAt(2021) must NOT surface Beta (valid only from 2022): %+v", facts)
	}
	if _, err := FactAt(db.DB, "p", "2026-01-01"); err != nil {
		t.Errorf("date-only timestamp should be accepted: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool { return indexOf(s, sub) >= 0 })()
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
