package toolindex

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tag-agent/tag/internal/store"
)

// mockEmbedder is a deterministic, fully offline embedder: it maps text onto a
// 4-axis "topic" space by marker words. No network, no key, no cost — the same
// shape as the mock embeddings server in internal/memory/embed_test.go.
type mockEmbedder struct{ model string }

func (m mockEmbedder) Model() string {
	if m.model != "" {
		return m.model
	}
	return "mock-embed"
}

func (m mockEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i, in := range inputs {
		out[i] = vectorFor(in)
	}
	return out, nil
}

// vectorFor: axis 0 = web/internet, 1 = database, 2 = version control, 3 = chat.
func vectorFor(text string) []float32 {
	l := strings.ToLower(text)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(l, w) {
				return true
			}
		}
		return false
	}
	v := []float32{0.01, 0.01, 0.01, 0.01}
	if has("web", "internet", "online", "browser", "http", "scraping", "url") {
		v[0] += 1
	}
	if has("database", "sql", "postgres", "sqlite", "table", "query the data") {
		v[1] += 1
	}
	if has("git", "repository", "commit", "pull request", "version control") {
		v[2] += 1
	}
	if has("slack", "message", "chat", "conversation") {
		v[3] += 1
	}
	return v
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.OpenPath(filepath.Join(t.TempDir(), "t.sqlite3"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db.DB
}

func fixtureTools() []Tool {
	return []Tool{
		{Name: "mcp-brave-search", Description: "Web search via Brave Search API", Server: "mcp-brave-search"},
		{Name: "mcp-postgres", Description: "PostgreSQL read/write access via MCP", Server: "mcp-postgres"},
		{Name: "mcp-github", Description: "GitHub repository operations: clone, PRs, issues, commits", Server: "mcp-github"},
		{Name: "mcp-slack", Description: "Send and read Slack messages via MCP", Server: "mcp-slack"},
		// A decoy whose DESCRIPTION contains the literal query words but whose
		// topic is unrelated — this is what a keyword scan (wrongly) ranks first.
		{Name: "mcp-notes", Description: "Take notes about how to look up things later", Server: "mcp-notes"},
	}
}

// TestVectorRetrievalRanksSemanticallyCloserAboveKeywordOnly is the core PRD-043
// assertion: a query with NO literal term overlap with the right tool must still
// retrieve it, and must rank it above a tool that merely shares query words.
func TestVectorRetrievalRanksSemanticallyCloserAboveKeywordOnly(t *testing.T) {
	db := testDB(t)
	e := mockEmbedder{}
	res, err := Build(context.Background(), db, e, fixtureTools())
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != ModeVector || res.Embedded != len(fixtureTools()) {
		t.Fatalf("build should embed all tools: %+v", res)
	}

	const query = "look up things on the internet"

	// Keyword baseline: the decoy shares "look up things" and wins; the actually
	// relevant web tool has zero term overlap and is absent entirely.
	kw, err := keywordSearch(db, query, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(kw) == 0 || kw[0].Name != "mcp-notes" {
		t.Fatalf("keyword baseline should rank the decoy first, got %+v", kw)
	}
	for _, h := range kw {
		if h.Name == "mcp-brave-search" {
			t.Fatalf("keyword baseline should not find the web tool at all: %+v", kw)
		}
	}

	// Vector retrieval: the web tool is top-1 despite zero term overlap.
	hits, mode, err := Search(context.Background(), db, e, query, 5)
	if err != nil {
		t.Fatal(err)
	}
	if mode != ModeVector {
		t.Fatalf("mode = %q, want %q", mode, ModeVector)
	}
	if len(hits) == 0 || hits[0].Name != "mcp-brave-search" {
		t.Fatalf("vector retrieval should rank the semantically-closest tool first, got %+v", hits)
	}
	for i, h := range hits {
		if h.Name == "mcp-notes" && i == 0 {
			t.Fatalf("keyword-only decoy must not outrank the semantic match: %+v", hits)
		}
	}
}

// TestSearchFallsBackHonestlyWithNoEmbedder: keyless operation must still work
// and must SAY it is keyword-based rather than silently pretending to be vector.
func TestSearchFallsBackHonestlyWithNoEmbedder(t *testing.T) {
	db := testDB(t)
	res, err := Build(context.Background(), db, nil, fixtureTools())
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != ModeKeyword || res.Embedded != 0 {
		t.Fatalf("no embedder must build a keyword-only index: %+v", res)
	}
	if res.Note == "" {
		t.Error("keyless build must explain why retrieval is keyword-only")
	}
	hits, mode, err := Search(context.Background(), db, nil, "web search", 5)
	if err != nil {
		t.Fatal(err)
	}
	if mode != ModeKeyword {
		t.Fatalf("mode = %q, want %q", mode, ModeKeyword)
	}
	if len(hits) == 0 || hits[0].Name != "mcp-brave-search" {
		t.Fatalf("keyword retrieval still has to work: %+v", hits)
	}
}

// TestSearchFallsBackWhenIndexHasNoVectors: an embedder configured AFTER a
// keyword-only build must not return an empty "vector" result — it degrades.
func TestSearchFallsBackWhenIndexHasNoVectors(t *testing.T) {
	db := testDB(t)
	if _, err := Build(context.Background(), db, nil, fixtureTools()); err != nil {
		t.Fatal(err)
	}
	hits, mode, err := Search(context.Background(), db, mockEmbedder{}, "web search", 5)
	if err != nil {
		t.Fatal(err)
	}
	if mode != ModeKeyword {
		t.Fatalf("un-embedded index must degrade to keyword, got %q", mode)
	}
	if len(hits) == 0 {
		t.Fatal("degrading must not swallow the keyword results")
	}
}

// TestSearchFallsBackOnModelSwitch: vectors written under model A must never be
// cosine-compared against a query embedded by model B.
func TestSearchFallsBackOnModelSwitch(t *testing.T) {
	db := testDB(t)
	if _, err := Build(context.Background(), db, mockEmbedder{model: "model-a"}, fixtureTools()); err != nil {
		t.Fatal(err)
	}
	_, mode, err := Search(context.Background(), db, mockEmbedder{model: "model-b"}, "web search", 5)
	if err != nil {
		t.Fatal(err)
	}
	if mode != ModeKeyword {
		t.Fatalf("model switch must degrade to keyword, got %q", mode)
	}
}

func TestSearchRejectsEmptyQuery(t *testing.T) {
	db := testDB(t)
	if _, _, err := Search(context.Background(), db, nil, "   ", 5); err == nil {
		t.Fatal("empty query must be rejected")
	}
}

func TestEnsureSchemaIdempotent(t *testing.T) {
	db := testDB(t)
	for i := 0; i < 3; i++ {
		if err := EnsureSchema(db); err != nil {
			t.Fatalf("EnsureSchema call %d: %v", i, err)
		}
	}
}

func TestStatusReportsBackend(t *testing.T) {
	db := testDB(t)
	st, err := Load(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Built {
		t.Fatal("fresh DB must report not built")
	}
	e := mockEmbedder{}
	if _, err := Build(context.Background(), db, e, fixtureTools()); err != nil {
		t.Fatal(err)
	}
	st, err = Load(db, e)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Built || st.Backend != ModeVector || !st.VectorReady || st.Vectors != len(fixtureTools()) {
		t.Fatalf("status after vector build: %+v", st)
	}
	// Same index, no embedder configured → honest keyword backend.
	st, err = Load(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Backend != ModeKeyword || st.VectorReady {
		t.Fatalf("status with no embedder must report keyword: %+v", st)
	}
}

// TestBuildNeverEmbedsWithoutBackend guards the cost rule: a nil embedder must
// mean zero embedding calls (there is nothing to call), and topK must clamp.
func TestBuildTopKClamps(t *testing.T) {
	db := testDB(t)
	if _, err := Build(context.Background(), db, nil, fixtureTools()); err != nil {
		t.Fatal(err)
	}
	hits, _, err := Search(context.Background(), db, nil, "mcp", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 2 {
		t.Fatalf("top-k not applied: %d hits", len(hits))
	}
}

// TestDocumentPlaceholder: a tool with no description must still be indexed and
// retrievable (FR-12) rather than silently dropped.
func TestDocumentPlaceholder(t *testing.T) {
	if got := Document("mcp-x", "  "); got != "mcp-x: (no description)" {
		t.Fatalf("Document placeholder = %q", got)
	}
	db := testDB(t)
	if _, err := Build(context.Background(), db, nil, []Tool{{Name: "mcp-x", Server: "mcp-x"}}); err != nil {
		t.Fatal(err)
	}
	hits, _, err := Search(context.Background(), db, nil, "mcp-x", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("description-less tool must stay retrievable: %+v", hits)
	}
}
