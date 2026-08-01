package memory

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// topicEmbedder is a deterministic offline embedder: it projects text onto four
// topic axes by marker word. No network, no key, no cost.
type topicEmbedder struct{ model string }

func (t topicEmbedder) Model() string {
	if t.model != "" {
		return t.model
	}
	return "topic-mock"
}

func (t topicEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i, in := range inputs {
		out[i] = topicVector(in)
	}
	return out, nil
}

// axes: 0 = auth/identity, 1 = database, 2 = deployment, 3 = cooking
func topicVector(s string) []float32 {
	l := strings.ToLower(s)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(l, w) {
				return true
			}
		}
		return false
	}
	v := []float32{0.01, 0.01, 0.01, 0.01}
	if has("auth", "login", "sign-in", "identity", "credential", "session token") {
		v[0] += 1
	}
	if has("database", "postgres", "sql", "schema", "migration") {
		v[1] += 1
	}
	if has("deploy", "release", "kubernetes", "rollout") {
		v[2] += 1
	}
	if has("recipe", "cooking", "kitchen", "food") {
		v[3] += 1
	}
	return v
}

// embedAllMemories stores a vector for every memory in the profile.
func embedAllMemories(t *testing.T, db *sql.DB, e Embedder, profile string) {
	t.Helper()
	n, err := RebuildEmbeddings(context.Background(), db, e, profile, true)
	if err != nil {
		t.Fatalf("rebuild embeddings: %v", err)
	}
	if n == 0 {
		t.Fatal("rebuild embedded nothing")
	}
}

// TestHybridBeatsPureFTSOnSemanticMiss is the core PRD-066 assertion: a query
// whose words appear NOWHERE in the store (so FTS5 and the LIKE union both
// return nothing) is still answered by the hybrid path via the dense leg.
func TestHybridBeatsPureFTSOnSemanticMiss(t *testing.T) {
	db := memTestDB(t)
	e := topicEmbedder{}
	Add(db, "p", "the login sequence issues a session token valid for one hour", "fact", 0.9)
	Add(db, "p", "postgres schema migrations run with make migrate", "convention", 0.9)
	Add(db, "p", "a good recipe for sourdough needs a mature starter", "other", 0.9)
	embedAllMemories(t, db, e, "p")

	const query = "authentication flow"

	// Pure FTS: no token overlap ("authentication" is not "login"), no substring
	// match — zero recall. This is exactly the gap PRD-066 exists to close.
	fts, err := Search(db, "p", query, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(fts) != 0 {
		t.Fatalf("FTS baseline should miss this query entirely, got %d hits: %+v", len(fts), fts)
	}

	hits, mode, err := SearchHybrid(context.Background(), db, e, "p", query, DefaultHybridOptions())
	if err != nil {
		t.Fatal(err)
	}
	if mode != ModeHybrid {
		t.Fatalf("mode = %q, want %q", mode, ModeHybrid)
	}
	if len(hits) == 0 {
		t.Fatal("hybrid must recover the semantically-related memory FTS missed")
	}
	if !strings.Contains(hits[0].Content, "login sequence") {
		t.Fatalf("top hybrid hit should be the auth memory, got %q", hits[0].Content)
	}
	if hits[0].DenseScore <= 0 {
		t.Errorf("dense score must be reported for transparency: %+v", hits[0])
	}
}

// TestHybridKeepsExactKeywordWin: hybrid must not LOSE the exact-match strength
// of BM25 — an exact phrase still ranks first.
func TestHybridKeepsExactKeywordWin(t *testing.T) {
	db := memTestDB(t)
	e := topicEmbedder{}
	Add(db, "p", "the login sequence issues a session token valid for one hour", "fact", 0.9)
	Add(db, "p", "DATABASE_URL is the postgres connection string env var", "fact", 0.9)
	embedAllMemories(t, db, e, "p")

	hits, mode, err := SearchHybrid(context.Background(), db, e, "p", "DATABASE_URL", DefaultHybridOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || !strings.Contains(hits[0].Content, "DATABASE_URL") {
		t.Fatalf("exact keyword match must stay rank-1 under hybrid (mode=%s): %+v", mode, hits)
	}
}

// TestHybridDoesNotRegressCJKRecall is the guard for issue #574: the sparse leg
// of hybrid must be the FTS+LIKE union, so a CJK / mid-word query that FTS5's
// tokenizer cannot see is still recalled — with and without a dense backend.
func TestHybridDoesNotRegressCJKRecall(t *testing.T) {
	db := memTestDB(t)
	Add(db, "p", "数据库使用 PostgreSQL", "fact", 0.9)
	Add(db, "p", "the deployment pipeline is green", "fact", 0.9)

	cases := []struct{ query, want string }{
		{"数据库", "数据库使用 PostgreSQL"},
		{"据库使", "数据库使用 PostgreSQL"},
		{"deploy", "the deployment pipeline is green"}, // partial word
	}
	for _, backend := range []struct {
		name string
		e    Embedder
	}{
		{"no-embedder", nil},
		{"with-embedder", topicEmbedder{}},
	} {
		if backend.e != nil {
			embedAllMemories(t, db, backend.e, "p")
		}
		for _, c := range cases {
			hits, _, err := SearchHybrid(context.Background(), db, backend.e, "p", c.query, DefaultHybridOptions())
			if err != nil {
				t.Fatalf("%s %q: %v", backend.name, c.query, err)
			}
			found := false
			for _, h := range hits {
				if h.Content == c.want {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: hybrid lost recall for %q — want %q, got %+v", backend.name, c.query, c.want, hits)
			}
		}
	}
}

// TestHybridDegradesHonestlyWithoutEmbedder: no key must mean fts, reported.
func TestHybridDegradesHonestlyWithoutEmbedder(t *testing.T) {
	db := memTestDB(t)
	Add(db, "p", "the login sequence issues a session token", "fact", 0.9)

	hits, mode, err := SearchHybrid(context.Background(), db, nil, "p", "login", DefaultHybridOptions())
	if err != nil {
		t.Fatal(err)
	}
	if mode != ModeFTS {
		t.Fatalf("keyless hybrid must report %q, got %q", ModeFTS, mode)
	}
	if len(hits) != 1 {
		t.Fatalf("sparse leg must still answer: %+v", hits)
	}
	// Dense-only request with no backend must degrade too, never claim "dense".
	_, mode, err = SearchHybrid(context.Background(), db, nil, "p", "login",
		HybridOptions{Mode: ModeDense, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if mode != ModeFTS {
		t.Fatalf("keyless dense must degrade to %q, got %q", ModeFTS, mode)
	}
}

// TestHybridDegradesWhenNoVectorsStored: an embedder configured but nothing
// embedded yet must not yield an empty "dense" answer.
func TestHybridDegradesWhenNoVectorsStored(t *testing.T) {
	db := memTestDB(t)
	Add(db, "p", "the login sequence issues a session token", "fact", 0.9)
	hits, mode, err := SearchHybrid(context.Background(), db, topicEmbedder{}, "p", "login", DefaultHybridOptions())
	if err != nil {
		t.Fatal(err)
	}
	if mode != ModeFTS || len(hits) != 1 {
		t.Fatalf("un-embedded store must degrade to fts with results: mode=%q hits=%+v", mode, hits)
	}
}

// TestAlphaEndpointsMatchPureModes covers PRD-066 AC-03/AC-04.
func TestAlphaEndpointsMatchPureModes(t *testing.T) {
	db := memTestDB(t)
	e := topicEmbedder{}
	Add(db, "p", "the login sequence issues a session token", "fact", 0.9)
	Add(db, "p", "postgres schema migrations run with make migrate", "fact", 0.9)
	Add(db, "p", "a recipe for sourdough", "fact", 0.9)
	embedAllMemories(t, db, e, "p")

	ids := func(hs []HybridHit) []string {
		out := make([]string, len(hs))
		for i, h := range hs {
			out[i] = h.ID
		}
		return out
	}
	eq := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	pureFTS, mFTS, err := SearchHybrid(context.Background(), db, e, "p", "postgres", HybridOptions{Mode: ModeFTS, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	a0, m0, err := SearchHybrid(context.Background(), db, e, "p", "postgres", HybridOptions{Mode: ModeHybrid, Alpha: 0, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if m0 != mFTS || !eq(ids(a0), ids(pureFTS)) {
		t.Errorf("alpha=0 must equal pure fts: %v (%s) vs %v (%s)", ids(a0), m0, ids(pureFTS), mFTS)
	}

	pureDense, mDense, err := SearchHybrid(context.Background(), db, e, "p", "postgres", HybridOptions{Mode: ModeDense, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	a1, m1, err := SearchHybrid(context.Background(), db, e, "p", "postgres", HybridOptions{Mode: ModeHybrid, Alpha: 1, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if m1 != mDense || !eq(ids(a1), ids(pureDense)) {
		t.Errorf("alpha=1 must equal pure dense: %v (%s) vs %v (%s)", ids(a1), m1, ids(pureDense), mDense)
	}
}

func TestHybridValidatesInput(t *testing.T) {
	db := memTestDB(t)
	if _, _, err := SearchHybrid(context.Background(), db, nil, "p", "  ", DefaultHybridOptions()); err == nil {
		t.Error("empty query must be rejected")
	}
	if _, _, err := SearchHybrid(context.Background(), db, nil, "p", "x", HybridOptions{Mode: "bogus"}); err == nil {
		t.Error("bad mode must be rejected")
	}
	if _, _, err := SearchHybrid(context.Background(), db, nil, "p", "x", HybridOptions{Mode: ModeHybrid, Alpha: 1.5}); err == nil {
		t.Error("alpha out of range must be rejected")
	}
}

func TestValidateHybridModeAliases(t *testing.T) {
	for in, want := range map[string]string{
		"": ModeHybrid, "hybrid": ModeHybrid,
		"fts": ModeFTS, "bm25": ModeFTS, "sparse": ModeFTS, "keyword": ModeFTS,
		"dense": ModeDense, "vector": ModeDense, "DENSE": ModeDense,
	} {
		got, err := ValidateHybridMode(in)
		if err != nil || got != want {
			t.Errorf("ValidateHybridMode(%q) = (%q,%v) want %q", in, got, err, want)
		}
	}
}

// TestHybridEmptyResultIsNonNil: the --json contract needs [] not null.
func TestHybridEmptyResultIsNonNil(t *testing.T) {
	db := memTestDB(t)
	hits, _, err := SearchHybrid(context.Background(), db, nil, "p", "nothing-here", DefaultHybridOptions())
	if err != nil {
		t.Fatal(err)
	}
	if hits == nil {
		t.Fatal("empty hybrid result must be a non-nil slice")
	}
}
