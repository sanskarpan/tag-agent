package cli_test

// End-to-end coverage for the memory/retrieval PRDs (043, 065, 066, 068),
// driven through the real binary in an isolated TAG_HOME.
//
// Every LLM/embedding endpoint here is a local httptest server on 127.0.0.1
// returning deterministic canned data. No test in this file may reach a real
// provider: runEnvIO explicitly blanks every credential env var it knows about,
// so a developer machine with OPENAI_API_KEY exported cannot accidentally spend
// money running the suite.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// lockedBuf is a mutex-guarded io.Writer for capturing a live subprocess's
// output while the test polls it.
type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// runEnvIO runs the binary with an isolated TAG_HOME plus extra env, returning
// stdout, stderr and the exit code separately (stdout stays parseable JSON).
func runEnvIO(t *testing.T, home string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(tagBin, args...)
	cmd.Env = append(os.Environ(),
		"TAG_HOME="+home,
		// Hard offline guarantee for this file.
		"OPENAI_API_KEY=", "ANTHROPIC_API_KEY=", "TAG_EMBED_API_KEY=", "TAG_EMBED_BASE_URL=",
		"TAG_LOCAL_BASE_URL=", "TAG_MEMORY_AUTO_EXTRACT=",
	)
	cmd.Env = append(cmd.Env, env...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running %v: %v", args, err)
	}
	return out.String(), errb.String(), code
}

// mockEmbedHTTP serves an OpenAI-compatible /embeddings endpoint whose vectors
// are a deterministic projection onto four topic axes.
func mockEmbedHTTP(t *testing.T) *httptest.Server {
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
		data := make([]item, 0, len(req.Input))
		for i, in := range req.Input {
			data = append(data, item{Index: i, Embedding: topicVec(in), Object: "embedding"})
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "model": req.Model})
	}))
	// CloseClientConnections first: a subprocess that exited without draining a
	// keep-alive connection otherwise makes srv.Close() block for 5s.
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	return srv
}

// topicVec: axis 0 = web/internet, 1 = database, 2 = auth/identity, 3 = chat.
func topicVec(s string) []float32 {
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
	if has("web", "internet", "online", "browser", "http", "hypertext", "scraping", "fetch", "puppeteer", "search") {
		v[0] += 1
	}
	if has("database", "sql", "postgres", "sqlite") {
		v[1] += 1
	}
	if has("auth", "login", "sign-in", "identity", "credential", "session token") {
		v[2] += 1
	}
	if has("slack", "message", "chat") {
		v[3] += 1
	}
	return v
}

// mockChatHTTP serves an OpenAI-compatible /chat/completions SSE stream that
// always replies with the same canned text.
func mockChatHTTP(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.Error(w, "not found", 404)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		chunk, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{"delta": map[string]any{"content": reply}}},
		})
		fmt.Fprintf(w, "data: %s\n\n", chunk)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	return srv
}

// ---- PRD-043: vector-based tool retrieval -----------------------------------

// TestE2EToolIndexVectorRetrieval: with an embeddings backend configured, tool
// retrieval is semantic — a query with zero literal overlap finds the right
// server — and the mode is reported. Without one it stays keyword and says so.
func TestE2EToolIndexVectorRetrieval(t *testing.T) {
	h := newHome(t)
	emb := mockEmbedHTTP(t)
	env := []string{"TAG_EMBED_BASE_URL=" + emb.URL, "TAG_EMBED_API_KEY=mock-key", "TAG_EMBED_MODEL=mock-topic"}

	// Baseline (no backend): keyword, and the semantic query finds nothing.
	out, _, code := runEnvIO(t, h, nil, "tool-index", "index")
	if code != 0 || !strings.Contains(out, "backend: keyword") {
		t.Fatalf("keyless index: %q code=%d", out, code)
	}
	out, _, code = runEnvIO(t, h, nil, "--json", "tool-index", "search", "retrieve remote hypertext documents")
	if code != 0 {
		t.Fatalf("keyless search exit %d: %q", code, out)
	}
	var kw struct {
		Mode    string           `json:"mode"`
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &kw); err != nil {
		t.Fatalf("search json: %v: %q", err, out)
	}
	if kw.Mode != "keyword" {
		t.Errorf("keyless search must report keyword, got %q", kw.Mode)
	}
	if len(kw.Results) != 0 {
		t.Errorf("keyword baseline should miss this query, got %+v", kw.Results)
	}

	// Now with the mock embeddings backend.
	out, _, code = runEnvIO(t, h, env, "tool-index", "index")
	if code != 0 || !strings.Contains(out, "backend: vector") {
		t.Fatalf("vector index: %q code=%d", out, code)
	}
	out, _, code = runEnvIO(t, h, env, "--json", "tool-index", "search", "retrieve remote hypertext documents", "--top-k", "3")
	if code != 0 {
		t.Fatalf("vector search exit %d: %q", code, out)
	}
	var vec struct {
		Mode    string `json:"mode"`
		Results []struct {
			Name  string  `json:"name"`
			Score float64 `json:"score"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &vec); err != nil {
		t.Fatalf("search json: %v: %q", err, out)
	}
	if vec.Mode != "vector" {
		t.Fatalf("configured backend must retrieve by vector, got %q", vec.Mode)
	}
	if len(vec.Results) == 0 {
		t.Fatal("vector retrieval returned nothing")
	}
	web := map[string]bool{"mcp-brave-search": true, "mcp-fetch": true, "mcp-puppeteer": true}
	if !web[vec.Results[0].Name] {
		t.Errorf("top vector hit should be a web tool, got %q (%+v)", vec.Results[0].Name, vec.Results)
	}

	// status reports the vector backend and the model.
	out, _, code = runEnvIO(t, h, env, "--json", "tool-index", "status")
	if code != 0 || !strings.Contains(out, `"backend": "vector"`) || !strings.Contains(out, "mock-topic") {
		t.Errorf("status: %q code=%d", out, code)
	}
	// ...and honestly downgrades when the key goes away.
	out, _, code = runEnvIO(t, h, nil, "--json", "tool-index", "status")
	if code != 0 || !strings.Contains(out, `"backend": "keyword"`) {
		t.Errorf("status without a backend must report keyword: %q code=%d", out, code)
	}
}

// TestE2EToolIndexNoEmbedFlag: --no-embed keeps the index keyword-only even when
// a backend is configured (no surprise embedding spend).
func TestE2EToolIndexNoEmbedFlag(t *testing.T) {
	h := newHome(t)
	emb := mockEmbedHTTP(t)
	env := []string{"TAG_EMBED_BASE_URL=" + emb.URL, "TAG_EMBED_API_KEY=mock-key"}
	out, _, code := runEnvIO(t, h, env, "tool-index", "index", "--no-embed")
	if code != 0 || !strings.Contains(out, "backend: keyword") {
		t.Fatalf("--no-embed: %q code=%d", out, code)
	}
	out, _, code = runEnvIO(t, h, env, "--json", "tool-index", "search", "postgres")
	if code != 0 || !strings.Contains(out, `"mode": "keyword"`) {
		t.Errorf("un-embedded index must degrade to keyword: %q code=%d", out, code)
	}
}

// ---- PRD-066: hybrid memory search ------------------------------------------

// TestE2EMemSearchHybrid: hybrid retrieval recovers a memory that FTS/BM25
// cannot match at all, and reports the mode it used.
func TestE2EMemSearchHybrid(t *testing.T) {
	h := newHome(t)
	emb := mockEmbedHTTP(t)
	env := []string{"TAG_EMBED_BASE_URL=" + emb.URL, "TAG_EMBED_API_KEY=mock-key", "TAG_EMBED_MODEL=mock-topic"}

	for _, c := range []string{
		"the login sequence issues a session token valid for one hour",
		"postgres is reachable at the DATABASE_URL env var",
	} {
		if _, errs, code := runEnvIO(t, h, nil, "mem", "add", c); code != 0 {
			t.Fatalf("mem add %q: %d %s", c, code, errs)
		}
	}

	// FTS baseline: "authentication flow" shares no token or substring.
	out, _, code := runEnvIO(t, h, nil, "--json", "mem", "search", "authentication flow", "--mode", "fts")
	if code != 0 {
		t.Fatalf("fts search exit %d: %q", code, out)
	}
	var ftsHits []map[string]any
	if err := json.Unmarshal([]byte(out), &ftsHits); err != nil {
		t.Fatalf("fts json: %v: %q", err, out)
	}
	if len(ftsHits) != 0 {
		t.Fatalf("FTS baseline should miss the query; PRD-066 has nothing to fix otherwise: %+v", ftsHits)
	}

	// Embed the store, then hybrid must find it.
	if out, errs, code := runEnvIO(t, h, env, "mem2", "store", "rebuild"); code != 0 {
		t.Fatalf("store rebuild: %d %q %q", code, out, errs)
	}
	out, _, code = runEnvIO(t, h, env, "--json", "mem", "search", "authentication flow", "--mode", "hybrid", "--verbose")
	if code != 0 {
		t.Fatalf("hybrid search exit %d: %q", code, out)
	}
	var hy struct {
		Mode    string `json:"mode"`
		Results []struct {
			Content     string  `json:"content"`
			DenseScore  float64 `json:"dense_score"`
			SparseScore float64 `json:"sparse_score"`
			HybridScore float64 `json:"hybrid_score"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &hy); err != nil {
		t.Fatalf("hybrid json: %v: %q", err, out)
	}
	if hy.Mode != "hybrid" {
		t.Fatalf("mode = %q, want hybrid", hy.Mode)
	}
	if len(hy.Results) == 0 || !strings.Contains(hy.Results[0].Content, "login sequence") {
		t.Fatalf("hybrid must recover the memory FTS missed: %+v", hy.Results)
	}
	if hy.Results[0].DenseScore <= 0 || hy.Results[0].HybridScore <= 0 {
		t.Errorf("scores must be reported: %+v", hy.Results[0])
	}
}

// TestE2EMemSearchHybridCJKNotRegressed guards issue #574 through the CLI: the
// unconditional LIKE union still governs the sparse leg of hybrid.
func TestE2EMemSearchHybridCJKNotRegressed(t *testing.T) {
	h := newHome(t)
	emb := mockEmbedHTTP(t)
	env := []string{"TAG_EMBED_BASE_URL=" + emb.URL, "TAG_EMBED_API_KEY=mock-key", "TAG_EMBED_MODEL=mock-topic"}
	if _, _, code := runEnvIO(t, h, nil, "mem", "add", "数据库使用 PostgreSQL"); code != 0 {
		t.Fatal("mem add CJK failed")
	}
	if _, _, code := runEnvIO(t, h, nil, "mem", "add", "the deployment pipeline is green"); code != 0 {
		t.Fatal("mem add failed")
	}
	if _, _, code := runEnvIO(t, h, env, "mem2", "store", "rebuild"); code != 0 {
		t.Fatal("store rebuild failed")
	}
	for _, tc := range []struct{ query, want string }{
		{"数据库", "数据库使用 PostgreSQL"},
		{"据库使", "数据库使用 PostgreSQL"},
		{"deploy", "deployment pipeline"},
	} {
		for _, e := range [][]string{nil, env} {
			out, _, code := runEnvIO(t, h, e, "mem", "search", tc.query)
			if code != 0 || !strings.Contains(out, tc.want) {
				t.Errorf("hybrid lost recall for %q (embedder=%v): exit=%d out=%q", tc.query, e != nil, code, out)
			}
		}
	}
}

// TestE2EMemSearchModeValidation: bad flag values are usage errors (exit 2) and
// emit a JSON error object under --json.
func TestE2EMemSearchModeValidation(t *testing.T) {
	h := newHome(t)
	out, _, code := runEnvIO(t, h, nil, "--json", "mem", "search", "x", "--mode", "bogus")
	if code != 2 {
		t.Errorf("bad --mode must exit 2, got %d (%q)", code, out)
	}
	if !strings.Contains(out, `"error"`) {
		t.Errorf("bad --mode under --json must emit an error object: %q", out)
	}
	_, _, code = runEnvIO(t, h, nil, "mem", "search", "x", "--alpha", "2")
	if code != 2 {
		t.Errorf("out-of-range --alpha must exit 2, got %d", code)
	}
}

// TestE2EMemSearchDegradesHonestly: an explicit --mode dense with no backend
// says so on stderr and still answers from FTS, exit 0.
func TestE2EMemSearchDegradesHonestly(t *testing.T) {
	h := newHome(t)
	runEnvIO(t, h, nil, "mem", "add", "the login sequence issues a session token")
	stdout, stderr, code := runEnvIO(t, h, nil, "--json", "mem", "search", "login", "--mode", "dense")
	if code != 0 {
		t.Fatalf("dense with no backend must still succeed, exit %d", code)
	}
	if !strings.Contains(stderr, "unavailable") {
		t.Errorf("degradation must be announced on stderr, got %q", stderr)
	}
	var hits []map[string]any
	if err := json.Unmarshal([]byte(stdout), &hits); err != nil {
		t.Fatalf("stdout must stay parseable JSON: %v: %q", err, stdout)
	}
	if len(hits) != 1 {
		t.Errorf("degraded dense must still return the FTS hit: %+v", hits)
	}
}

// ---- PRD-065: automatic post-run memory extraction ---------------------------

// TestE2EAutoExtractIsOptIn: a plain `tag run` triggers nothing; --auto-memorize
// triggers extraction; --no-auto-memorize suppresses it even when config says on.
func TestE2EAutoExtractIsOptIn(t *testing.T) {
	h := newHome(t)

	countExtractions := func() int {
		out, _, code := runEnvIO(t, h, nil, "--json", "mem2", "extractions", "--all-profiles")
		if code != 0 {
			t.Fatalf("extractions exit %d: %q", code, out)
		}
		var rows []map[string]any
		if err := json.Unmarshal([]byte(out), &rows); err != nil {
			t.Fatalf("extractions json: %v: %q", err, out)
		}
		return len(rows)
	}

	if _, _, code := runEnvIO(t, h, nil, "run", "set up the project"); code != 0 {
		t.Fatal("plain run failed")
	}
	if n := countExtractions(); n != 0 {
		t.Fatalf("default `tag run` must not trigger extraction, got %d rows", n)
	}

	_, stderr, code := runEnvIO(t, h, nil, "run", "set up the project", "--auto-memorize")
	if code != 0 {
		t.Fatalf("--auto-memorize must not change the run's exit code, got %d", code)
	}
	if !strings.Contains(stderr, "memory: extracted") {
		t.Errorf("--auto-memorize must report the extraction on stderr: %q", stderr)
	}
	if n := countExtractions(); n != 1 {
		t.Fatalf("--auto-memorize should have recorded 1 extraction, got %d", n)
	}

	// Config on, flag off → suppressed.
	if out, _, code := runEnvIO(t, h, nil, "mem2", "config", "set", "auto_extract", "true"); code != 0 {
		t.Fatalf("config set: %d %q", code, out)
	}
	if _, _, code := runEnvIO(t, h, nil, "run", "another task", "--no-auto-memorize"); code != 0 {
		t.Fatal("run failed")
	}
	if n := countExtractions(); n != 1 {
		t.Fatalf("--no-auto-memorize must suppress extraction, got %d rows", n)
	}
	// Config on, no flag → triggered.
	if _, _, code := runEnvIO(t, h, nil, "run", "a third task"); code != 0 {
		t.Fatal("run failed")
	}
	if n := countExtractions(); n != 2 {
		t.Fatalf("memory.auto_extract=true must trigger extraction, got %d rows", n)
	}
}

// TestE2EAutoExtractOfflineHonest: with the offline echo provider, extraction
// fabricates nothing and says why.
func TestE2EAutoExtractOfflineHonest(t *testing.T) {
	h := newHome(t)
	if _, _, code := runEnvIO(t, h, nil, "run", "the database adapter is asyncpg", "--provider", "echo", "--auto-memorize"); code != 0 {
		t.Fatal("run failed")
	}
	out, _, code := runEnvIO(t, h, nil, "--json", "mem", "list")
	if code != 0 {
		t.Fatalf("mem list exit %d", code)
	}
	var mems []map[string]any
	if err := json.Unmarshal([]byte(out), &mems); err != nil {
		t.Fatalf("mem list json: %v: %q", err, out)
	}
	if len(mems) != 0 {
		t.Fatalf("echo provider must fabricate no memories, got %+v", mems)
	}
}

// TestE2EAutoExtractWritesFactsWithMockModel proves the trigger is real, not a
// no-op: pointed at a mock OpenAI-compatible server that returns a fact array,
// the post-run hook writes those memories.
func TestE2EAutoExtractWritesFactsWithMockModel(t *testing.T) {
	h := newHome(t)
	chat := mockChatHTTP(t, `[{"text":"Project uses ruff, not pylint","memory_type":"convention","confidence":0.95}]`)
	env := []string{"TAG_LOCAL_BASE_URL=" + chat.URL}

	if out, _, code := runEnvIO(t, h, env, "mem2", "config", "set", "extractor_provider", "local"); code != 0 {
		t.Fatalf("config set: %d %q", code, out)
	}
	_, stderr, code := runEnvIO(t, h, env, "run", "lint the project", "--provider", "echo", "--auto-memorize")
	if code != 0 {
		t.Fatalf("run exit %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, "memory: extracted 1") {
		t.Fatalf("expected 1 extracted memory, stderr=%q", stderr)
	}
	out, _, _ := runEnvIO(t, h, nil, "--json", "mem", "list")
	if !strings.Contains(out, "ruff") || !strings.Contains(out, "auto_extract") {
		t.Errorf("extracted memory must be stored and tagged auto_extract: %q", out)
	}
}

// TestE2EAutoExtractNeverFailsTheRun: even when the extractor backend is dead,
// the parent run still exits 0 and prints its own result.
func TestE2EAutoExtractNeverFailsTheRun(t *testing.T) {
	h := newHome(t)
	// Port 1 on loopback refuses connections instantly.
	env := []string{"TAG_LOCAL_BASE_URL=http://127.0.0.1:1/v1"}
	if out, _, code := runEnvIO(t, h, env, "mem2", "config", "set", "extractor_provider", "local"); code != 0 {
		t.Fatalf("config set: %d %q", code, out)
	}
	stdout, stderr, code := runEnvIO(t, h, env, "run", "do the thing", "--provider", "echo", "--auto-memorize")
	if code != 0 {
		t.Fatalf("a failing extractor must not fail the run; exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "do the thing") {
		t.Errorf("run output must be intact: %q", stdout)
	}
	if !strings.Contains(stderr, "extraction failed") {
		t.Errorf("the failure must be reported as a warning: %q", stderr)
	}
}

// TestE2EMem2ExtractManual: the manual command reports honestly offline and
// errors (exit 1) on an unknown run.
func TestE2EMem2ExtractManual(t *testing.T) {
	h := newHome(t)
	stdout, _, code := runEnvIO(t, h, nil, "run", "--json", "record a run")
	if code != 0 {
		t.Fatal("run failed")
	}
	var rr struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(stdout), &rr); err != nil || rr.RunID == "" {
		t.Fatalf("run --json: %v %q", err, stdout)
	}
	out, _, code := runEnvIO(t, h, nil, "mem2", "extract", rr.RunID)
	if code != 0 {
		t.Fatalf("manual extract exit %d: %q", code, out)
	}
	if !strings.Contains(out, "Extracted 0 memories") || !strings.Contains(out, "note:") {
		t.Errorf("offline extract must report 0 with an explanation: %q", out)
	}
	if _, _, code := runEnvIO(t, h, nil, "mem2", "extract", "does-not-exist"); code != 1 {
		t.Errorf("unknown run id must exit 1, got %d", code)
	}
	// --dry-run against a mock model classifies but writes nothing.
	chat := mockChatHTTP(t, `[{"text":"Tests live under tests/","memory_type":"fact","confidence":0.9}]`)
	env := []string{"TAG_LOCAL_BASE_URL=" + chat.URL}
	out, _, code = runEnvIO(t, h, env, "mem2", "extract", rr.RunID, "--provider", "local", "--dry-run")
	if code != 0 || !strings.Contains(out, "DRY RUN") || !strings.Contains(out, "would ADD") {
		t.Fatalf("dry run: exit=%d %q", code, out)
	}
	list, _, _ := runEnvIO(t, h, nil, "--json", "mem", "list")
	if strings.Contains(list, "tests/") {
		t.Errorf("dry run must write nothing: %q", list)
	}
}

// ---- PRD-068: background sleep-time consolidation ----------------------------

// TestE2EMem2GCDaemonExitsOnSIGTERM starts the real daemon, lets it complete a
// cycle, then sends SIGTERM and requires a prompt clean exit (code 0).
func TestE2EMem2GCDaemonExitsOnSIGTERM(t *testing.T) {
	h := newHome(t)
	runEnvIO(t, h, nil, "mem", "add", "durable fact worth consolidating")

	cmd := exec.Command(tagBin, "mem2", "gc", "--daemon", "--interval", "200ms")
	cmd.Env = append(os.Environ(), "TAG_HOME="+h, "OPENAI_API_KEY=", "ANTHROPIC_API_KEY=")
	// exec writes from its own goroutine while the test polls, so the sink has
	// to be synchronized (a bare strings.Builder is a data race under -race).
	out := &lockedBuf{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	// Wait for at least one cycle to be reported, so we know it is really looping
	// and not just parked at startup.
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "cycle 1") {
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("daemon never reported a cycle: %q", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon must exit 0 on SIGTERM, got %v (%q)", err, out.String())
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("daemon ignored SIGTERM for 5s: %q", out.String())
	}
	if !strings.Contains(out.String(), "consolidation daemon stopping") {
		t.Errorf("daemon must announce its shutdown: %q", out.String())
	}
}

// TestE2EMem2GCDaemonUsageErrors: bad flag combinations are exit 2, not a
// silently-wrong daemon.
func TestE2EMem2GCDaemonUsageErrors(t *testing.T) {
	h := newHome(t)
	if _, _, code := runEnvIO(t, h, nil, "mem2", "gc", "--daemon", "--dry-run"); code != 2 {
		t.Errorf("--daemon --dry-run must exit 2, got %d", code)
	}
	if _, _, code := runEnvIO(t, h, nil, "mem2", "gc", "--daemon", "--interval", "0s"); code != 2 {
		t.Errorf("--interval 0 must exit 2, got %d", code)
	}
	out, _, code := runEnvIO(t, h, nil, "--json", "mem2", "gc", "--daemon", "--dry-run")
	if code != 2 || !strings.Contains(out, `"error"`) {
		t.Errorf("usage error under --json must emit an error object: exit=%d %q", code, out)
	}
}

// TestE2EMem2GCOnDemandUnchanged: adding --daemon must not alter the one-shot
// behavior the PRD already shipped.
func TestE2EMem2GCOnDemandUnchanged(t *testing.T) {
	h := newHome(t)
	runEnvIO(t, h, nil, "mem", "add", "a fact")
	out, _, code := runEnvIO(t, h, nil, "mem2", "gc")
	if code != 0 || !strings.Contains(out, "GC done") {
		t.Errorf("one-shot gc: exit=%d %q", code, out)
	}
}
