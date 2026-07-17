package graph

import (
	"testing"

	"github.com/tag-agent/tag/internal/store"
)

func TestExtractEntities(t *testing.T) {
	ents := ExtractEntities("Alice Johnson works at Acme Corp using Python and Docker")
	byName := map[string]string{}
	for _, e := range ents {
		byName[e.Name] = e.Type
	}
	if byName["Python"] != "technology" {
		t.Errorf("Python should be technology, got %q", byName["Python"])
	}
	if byName["Docker"] != "technology" {
		t.Errorf("Docker should be technology, got %q", byName["Docker"])
	}
	if byName["Alice Johnson"] != "person" {
		t.Errorf("Alice Johnson should be person, got %q", byName["Alice Johnson"])
	}
	if byName["Acme Corp"] != "organization" {
		t.Errorf("Acme Corp should be organization, got %q", byName["Acme Corp"])
	}
}

func TestExtractDedup(t *testing.T) {
	// "python" (keyword) and "Python" (cap phrase) must dedup to one entity.
	ents := ExtractEntities("python Python PYTHON")
	count := 0
	for _, e := range ents {
		if e.Name == "Python" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected Python once, got %d", count)
	}
}

func TestUnionFindCommunities(t *testing.T) {
	// {a,b,c} connected; {d} isolated -> 2 components
	nodes := []string{"a", "b", "c", "d"}
	edges := [][2]string{{"a", "b"}, {"b", "c"}}
	m := unionFind(nodes, edges)
	if m["a"] != m["c"] {
		t.Error("a and c should share a root")
	}
	if m["a"] == m["d"] {
		t.Error("d should be its own component")
	}
}

func TestBuildAndQueryRoundTrip(t *testing.T) {
	db, err := store.OpenPath(t.TempDir() + "/g.sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Lowercase content so the three keywords match but no multi-word capitalized
	// phrase is captured (the cap-phrase regex would otherwise fold "Python Docker
	// Redis" into a 4th "other" entity). Keyword match is substring-based (faithful
	// to Python), so avoid tokens embedding a keyword (e.g. "again" contains "ai").
	if _, _, err := ExtractAndStore(db, "m1", "python plus docker plus redis", "p1"); err != nil {
		t.Fatal(err)
	}
	// re-adding python bumps mention_count, not entity count
	if _, _, err := ExtractAndStore(db, "m2", "python", "p1"); err != nil {
		t.Fatal(err)
	}
	ents, err := Query(db, "p1", "python", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].MentionCount != 2 {
		t.Errorf("Python mention_count should be 2, got %+v", ents)
	}
	nEnt, nRel, _, err := Summary(db, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if nEnt != 3 {
		t.Errorf("expected 3 distinct entities, got %d", nEnt)
	}
	if nRel != 3 { // C(3,2) co-occurrence from m1
		t.Errorf("expected 3 relations, got %d", nRel)
	}
	// Reset clears state
	if err := Reset(db, "p1"); err != nil {
		t.Fatal(err)
	}
	if n, _, _, _ := Summary(db, "p1"); n != 0 {
		t.Errorf("after reset expected 0 entities, got %d", n)
	}
}
