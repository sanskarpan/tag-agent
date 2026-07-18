package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tag-agent/tag/internal/store"
	_ "modernc.org/sqlite"
)

// ---- pure unit tests: cosine + (de)serialization ----------------------------

func TestCosine(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 0, 0}, []float32{1, 0, 0}, 1},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0},
		{"opposite", []float32{1, 1}, []float32{-1, -1}, -1},
		{"scaled same dir", []float32{2, 0}, []float32{5, 0}, 1},
		{"mismatched len", []float32{1, 0}, []float32{1, 0, 0}, 0},
		{"empty", nil, []float32{1}, 0},
		{"zero norm", []float32{0, 0}, []float32{1, 1}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cosine(c.a, c.b)
			if math.Abs(got-c.want) > 1e-6 {
				t.Fatalf("cosine(%v,%v)=%v want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestCosineRankingOrder(t *testing.T) {
	q := []float32{1, 0, 0}
	near := []float32{0.9, 0.1, 0}
	far := []float32{0, 0, 1}
	if cosine(q, near) <= cosine(q, far) {
		t.Fatalf("near vector should rank above far: near=%v far=%v", cosine(q, near), cosine(q, far))
	}
}

func TestVectorRoundTrip(t *testing.T) {
	orig := []float32{0, 1, -1, 3.14159, -2.71828, 1e-8, 1e8}
	blob := encodeVector(orig)
	if len(blob) != 4*len(orig) {
		t.Fatalf("blob len=%d want %d", len(blob), 4*len(orig))
	}
	got, err := decodeVector(blob)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(orig) {
		t.Fatalf("decoded len=%d want %d", len(got), len(orig))
	}
	for i := range orig {
		if got[i] != orig[i] {
			t.Fatalf("index %d: got %v want %v", i, got[i], orig[i])
		}
	}
}

func TestDecodeVectorRejectsBadLength(t *testing.T) {
	if _, err := decodeVector([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected error for non-multiple-of-4 blob")
	}
	if _, err := decodeVector(nil); err != nil {
		t.Fatalf("empty blob should decode to empty vector, got %v", err)
	}
}

// ---- EmbedderFromEnv resolution --------------------------------------------

func TestEmbedderFromEnv(t *testing.T) {
	t.Setenv("TAG_EMBED_BASE_URL", "")
	t.Setenv("TAG_EMBED_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("TAG_EMBED_MODEL", "")
	if _, ok := EmbedderFromEnv(); ok {
		t.Fatal("no config should yield no embedder (FTS fallback)")
	}

	t.Setenv("TAG_EMBED_BASE_URL", "http://localhost:9999/v1")
	e, ok := EmbedderFromEnv()
	if !ok {
		t.Fatal("base url override should yield an embedder")
	}
	if e.BaseURL != "http://localhost:9999/v1" {
		t.Fatalf("base=%q", e.BaseURL)
	}
	if e.Model() != DefaultEmbedModel {
		t.Fatalf("model=%q want default", e.Model())
	}

	t.Setenv("TAG_EMBED_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	e, ok = EmbedderFromEnv()
	if !ok || e.APIKey != "sk-test" || e.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("openai key path: ok=%v key=%q base=%q", ok, e.APIKey, e.BaseURL)
	}

	// TAG_EMBED_API_KEY takes precedence over OPENAI_API_KEY.
	t.Setenv("TAG_EMBED_API_KEY", "sk-override")
	t.Setenv("TAG_EMBED_MODEL", "custom-model")
	e, _ = EmbedderFromEnv()
	if e.APIKey != "sk-override" || e.Model() != "custom-model" {
		t.Fatalf("override precedence: key=%q model=%q", e.APIKey, e.Model())
	}
}

// TestEmbedderFromEnvIfaceNilSafe guards the boxed-nil-pointer pitfall: with no
// config, the iface form must yield a genuine nil interface so e==nil guards in
// the store/search functions fire instead of panicking on a nil *OpenAIEmbedder.
func TestEmbedderFromEnvIfaceNilSafe(t *testing.T) {
	t.Setenv("TAG_EMBED_BASE_URL", "")
	t.Setenv("TAG_EMBED_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	e := EmbedderFromEnvIface()
	if e != nil {
		t.Fatalf("expected nil interface, got %#v", e)
	}
	db := memTestDB(t)
	Add(db, "default", "postgres database indexing", "fact", 0.9)
	// Must fall back to FTS, not panic.
	hits, vectorUsed, err := SearchByVector(context.Background(), db, e, "default", "database", 5)
	if err != nil {
		t.Fatal(err)
	}
	if vectorUsed || len(hits) == 0 {
		t.Fatalf("expected FTS fallback hits, vectorUsed=%v n=%d", vectorUsed, len(hits))
	}
	// Store/rebuild must error clearly, not panic.
	if _, err := RebuildEmbeddings(context.Background(), db, e, "default", false); err == nil {
		t.Fatal("expected error rebuilding with nil-iface embedder")
	}
}

// ---- mock embeddings server -------------------------------------------------

// mockEmbedServer returns a deterministic 3-dim vector keyed on the presence of
// marker words, so ranking is fully assertable:
//   - contains "database"/"sql"/"postgres" -> axis X  [1,0,0]
//   - contains "python"/"code"             -> axis Y  [0,1,0]
//   - contains "cooking"/"recipe"/"food"   -> axis Z  [0,0,1]
//   - otherwise a small mix so cosine is defined but distinct.
func mockVectorFor(text string) []float32 {
	l := strings.ToLower(text)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(l, w) {
				return true
			}
		}
		return false
	}
	v := []float32{0.01, 0.01, 0.01}
	if has("database", "sql", "postgres", "index") {
		v[0] += 1
	}
	if has("python", "code", "function", "programming") {
		v[1] += 1
	}
	if has("cooking", "recipe", "food", "kitchen") {
		v[2] += 1
	}
	return v
}

func newMockEmbedServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/embeddings") {
			http.Error(w, "not found", 404)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
			Object    string    `json:"object"`
		}
		var data []item
		for i, in := range req.Input {
			data = append(data, item{Index: i, Embedding: mockVectorFor(in), Object: "embedding"})
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "model": req.Model})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// memTestDB opens a real store DB (full migrate/schema.sql) in a temp file, so
// tests exercise the shipping schema — including the embedding/embed_model
// columns — rather than a hand-rolled one.
func memTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.OpenPath(filepath.Join(t.TempDir(), "test.sqlite3"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db.DB
}

func TestEmbedderEmbedViaMock(t *testing.T) {
	srv := newMockEmbedServer(t)
	e := &OpenAIEmbedder{BaseURL: srv.URL + "/v1", EmbedModel: "mock-embed"}
	vecs, err := e.Embed(context.Background(), []string{"a postgres database", "python code"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 {
		t.Fatalf("got %d vectors", len(vecs))
	}
	if vecs[0][0] < 1 || vecs[1][1] < 1 {
		t.Fatalf("unexpected mock vectors: %v %v", vecs[0], vecs[1])
	}
}

// TestSearchByVectorRanking is the core semantic-ranking assertion: three
// memories on orthogonal axes; a query on the DB axis must rank the DB memory
// first.
func TestSearchByVectorRanking(t *testing.T) {
	db := memTestDB(t)
	srv := newMockEmbedServer(t)
	e := &OpenAIEmbedder{BaseURL: srv.URL + "/v1", EmbedModel: "mock-embed"}
	profile := "default"

	dbID, _ := Add(db, profile, "How to add an index to a postgres database table", "fact", 0.9)
	pyID, _ := Add(db, profile, "A python function that parses code", "fact", 0.9)
	cookID, _ := Add(db, profile, "A recipe for cooking pasta in the kitchen", "fact", 0.9)

	// Rebuild embeds all three.
	n, err := RebuildEmbeddings(context.Background(), db, e, profile, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("rebuild embedded %d, want 3", n)
	}

	// Query on the database axis -> DB memory ranks first.
	hits, vectorUsed, err := SearchByVector(context.Background(), db, e, profile, "sql database index tuning", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !vectorUsed {
		t.Fatal("expected vector ranking, got FTS fallback")
	}
	if len(hits) != 3 {
		t.Fatalf("got %d hits, want 3", len(hits))
	}
	if hits[0].ID != dbID {
		t.Fatalf("top hit is %q (%q), want DB memory %q", hits[0].ID, hits[0].Content, dbID)
	}
	// Similarity strictly descending.
	for i := 1; i < len(hits); i++ {
		if hits[i].Similarity > hits[i-1].Similarity {
			t.Fatalf("hits not sorted desc: %v", hits)
		}
	}

	// Query on the python axis -> python memory ranks first.
	hits, _, err = SearchByVector(context.Background(), db, e, profile, "write python programming code", 3)
	if err != nil {
		t.Fatal(err)
	}
	if hits[0].ID != pyID {
		t.Fatalf("python query top hit %q want %q", hits[0].ID, pyID)
	}
	_ = cookID
}

func TestSearchByVectorFallsBackWithoutKey(t *testing.T) {
	db := memTestDB(t)
	profile := "default"
	Add(db, profile, "postgres database indexing tips", "fact", 0.9)
	Add(db, profile, "cooking a recipe", "fact", 0.9)

	// embedder=nil simulates no key configured.
	hits, vectorUsed, err := SearchByVector(context.Background(), db, nil, profile, "database", 5)
	if err != nil {
		t.Fatal(err)
	}
	if vectorUsed {
		t.Fatal("expected FTS fallback with nil embedder")
	}
	if len(hits) == 0 {
		t.Fatal("FTS fallback returned no hits for 'database'")
	}
	if !strings.Contains(strings.ToLower(hits[0].Content), "database") {
		t.Fatalf("FTS top hit unexpected: %q", hits[0].Content)
	}
}

func TestSearchByVectorFallsBackWhenNoVectors(t *testing.T) {
	db := memTestDB(t)
	srv := newMockEmbedServer(t)
	e := &OpenAIEmbedder{BaseURL: srv.URL + "/v1", EmbedModel: "mock-embed"}
	profile := "default"
	Add(db, profile, "postgres database indexing", "fact", 0.9)
	// No rebuild -> no stored vectors -> must fall back to FTS even with a key.
	hits, vectorUsed, err := SearchByVector(context.Background(), db, e, profile, "database", 5)
	if err != nil {
		t.Fatal(err)
	}
	if vectorUsed {
		t.Fatal("expected FTS fallback when no vectors stored")
	}
	if len(hits) == 0 {
		t.Fatal("expected FTS hits")
	}
}

func TestStoreEmbeddingSingleAndErrors(t *testing.T) {
	db := memTestDB(t)
	srv := newMockEmbedServer(t)
	e := &OpenAIEmbedder{BaseURL: srv.URL + "/v1", EmbedModel: "mock-embed"}
	profile := "default"
	id, _ := Add(db, profile, "postgres database", "fact", 0.9)

	n, err := StoreEmbedding(context.Background(), db, e, profile, id)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("dims=%d want 3", n)
	}
	// Vector + model persisted.
	var blob []byte
	var model string
	if err := db.QueryRow(`SELECT embedding, embed_model FROM semantic_memories WHERE id=?`, id).Scan(&blob, &model); err != nil {
		t.Fatal(err)
	}
	if len(blob) != 12 || model != "mock-embed" {
		t.Fatalf("persisted blob len=%d model=%q", len(blob), model)
	}

	// Missing memory errors clearly.
	if _, err := StoreEmbedding(context.Background(), db, e, profile, "nope"); err == nil {
		t.Fatal("expected error for missing memory")
	}
	// Nil embedder errors clearly.
	if _, err := StoreEmbedding(context.Background(), db, nil, profile, id); err == nil {
		t.Fatal("expected error with nil embedder")
	}
}

func TestRebuildErrorsWithoutBackend(t *testing.T) {
	db := memTestDB(t)
	profile := "default"
	Add(db, profile, "x", "fact", 0.9)
	if _, err := RebuildEmbeddings(context.Background(), db, nil, profile, false); err == nil {
		t.Fatal("expected clear error rebuilding without a backend")
	}
}

func TestRebuildOnlyMissing(t *testing.T) {
	db := memTestDB(t)
	srv := newMockEmbedServer(t)
	e := &OpenAIEmbedder{BaseURL: srv.URL + "/v1", EmbedModel: "mock-embed"}
	profile := "default"
	a, _ := Add(db, profile, "postgres database", "fact", 0.9)
	Add(db, profile, "python code", "fact", 0.9)

	// Embed just one via store.
	if _, err := StoreEmbedding(context.Background(), db, e, profile, a); err != nil {
		t.Fatal(err)
	}
	// Rebuild (not force) should only embed the remaining one.
	n, err := RebuildEmbeddings(context.Background(), db, e, profile, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rebuild embedded %d, want 1 (only missing)", n)
	}
	// Force re-embeds all.
	n, err = RebuildEmbeddings(context.Background(), db, e, profile, true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("force rebuild embedded %d, want 2", n)
	}
}

func TestEnsureVectorSchemaIdempotent(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Table WITHOUT the embedding columns.
	if _, err := db.Exec(`CREATE TABLE semantic_memories (
		id TEXT PRIMARY KEY, profile TEXT, content TEXT, memory_type TEXT,
		confidence REAL, created_at TEXT, accessed_at TEXT, access_count INTEGER,
		source TEXT, tier TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := ensureVectorSchema(db); err != nil {
		t.Fatal(err)
	}
	// Second call is a no-op.
	if err := ensureVectorSchema(db); err != nil {
		t.Fatal(err)
	}
	cols, _ := tableColumns(db, "semantic_memories")
	if !cols["embedding"] || !cols["embed_model"] {
		t.Fatalf("columns not ensured: %v", cols)
	}
}
