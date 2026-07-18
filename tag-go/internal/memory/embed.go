package memory

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// DefaultEmbedModel is the OpenAI embeddings model used when none is configured.
// text-embedding-3-small is 1536-dim, cheap, and widely mocked in tests.
const DefaultEmbedModel = "text-embedding-3-small"

// Embedder turns text into a dense vector. Decoupled from any concrete backend
// so tests can inject a deterministic mock (mirrors llm.Provider).
type Embedder interface {
	// Embed returns one vector per input string, in order.
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
	// Model reports the model identifier persisted alongside stored vectors.
	Model() string
}

// OpenAIEmbedder calls an OpenAI-compatible embeddings endpoint
// (POST {base}/embeddings). It mirrors llm.streamOpenAICompatible: plain
// net/http, base+key resolved from env with explicit overrides, mockable via a
// custom BaseURL pointing at an httptest server.
type OpenAIEmbedder struct {
	APIKey     string
	BaseURL    string // e.g. https://api.openai.com/v1
	EmbedModel string
	HTTPClient *http.Client
}

// EmbedderFromEnv builds an OpenAIEmbedder from the environment, honoring the
// local/mock overrides TAG_EMBED_BASE_URL and TAG_EMBED_API_KEY before falling
// back to the standard OpenAI base and OPENAI_API_KEY. Returns (nil, false) when
// no usable configuration exists (no override base and no OpenAI key) so callers
// can degrade to FTS.
//
// The concrete *OpenAIEmbedder return keeps Model() reachable for CLI display;
// callers passing it into the Embedder-typed functions below must first check
// ok and pass a genuine nil interface when !ok (boxing a nil pointer would
// defeat the e==nil guards). See EmbedderFromEnvIface for a nil-safe variant.
func EmbedderFromEnv() (*OpenAIEmbedder, bool) {
	base := strings.TrimSpace(os.Getenv("TAG_EMBED_BASE_URL"))
	key := strings.TrimSpace(os.Getenv("TAG_EMBED_API_KEY"))
	if key == "" {
		key = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	}
	model := strings.TrimSpace(os.Getenv("TAG_EMBED_MODEL"))
	if model == "" {
		model = DefaultEmbedModel
	}
	// A configured backend needs either a mock/local base URL OR an API key.
	// With neither, there is no way to embed — signal FTS fallback.
	if base == "" && key == "" {
		return nil, false
	}
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	return &OpenAIEmbedder{APIKey: key, BaseURL: base, EmbedModel: model}, true
}

// EmbedderFromEnvIface is the nil-safe form of EmbedderFromEnv: it returns a
// genuine nil Embedder interface (not a boxed nil pointer) when no backend is
// configured, so callers can pass the result straight into StoreEmbedding /
// RebuildEmbeddings / SearchByVector and have the e==nil guards fire correctly.
func EmbedderFromEnvIface() Embedder {
	if e, ok := EmbedderFromEnv(); ok {
		return e
	}
	return nil
}

// Model returns the configured embedding model id.
func (e *OpenAIEmbedder) Model() string {
	if e.EmbedModel != "" {
		return e.EmbedModel
	}
	return DefaultEmbedModel
}

// Embed POSTs the inputs to {base}/embeddings and returns the vectors in order.
func (e *OpenAIEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	base := e.BaseURL
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	body := map[string]any{"model": e.Model(), "input": inputs}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(base, "/") + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	if e.APIKey != "" {
		req.Header.Set("authorization", "Bearer "+e.APIKey)
	}
	client := e.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("embeddings API %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("embeddings decode: %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("embeddings API error: %s", parsed.Error.Message)
	}
	if len(parsed.Data) != len(inputs) {
		return nil, fmt.Errorf("embeddings API returned %d vectors for %d inputs", len(parsed.Data), len(inputs))
	}
	// The API guarantees index order but sort defensively so mock servers that
	// shuffle still line up with their inputs.
	sort.Slice(parsed.Data, func(i, j int) bool { return parsed.Data[i].Index < parsed.Data[j].Index })
	out := make([][]float32, len(parsed.Data))
	for i, d := range parsed.Data {
		if len(d.Embedding) == 0 {
			return nil, fmt.Errorf("embeddings API returned empty vector at index %d", i)
		}
		out[i] = d.Embedding
	}
	return out, nil
}

// embedBatchSize caps how many inputs are sent per embeddings request. OpenAI's
// embeddings API rejects input arrays larger than ~2048 items and enforces a
// per-request token cap; batching also keeps each response well under the 16 MiB
// read limit in Embed.
const embedBatchSize = 256

// embedAll embeds inputs in fixed-size batches, preserving input order, so large
// profiles don't exceed the embeddings API's array/token/response limits.
func embedAll(ctx context.Context, e Embedder, inputs []string) ([][]float32, error) {
	out := make([][]float32, 0, len(inputs))
	for start := 0; start < len(inputs); start += embedBatchSize {
		end := start + embedBatchSize
		if end > len(inputs) {
			end = len(inputs)
		}
		vecs, err := e.Embed(ctx, inputs[start:end])
		if err != nil {
			return nil, err
		}
		if len(vecs) != end-start {
			return nil, fmt.Errorf("embeddings API returned %d vectors for %d inputs", len(vecs), end-start)
		}
		out = append(out, vecs...)
	}
	return out, nil
}

// ---- vector (de)serialization ----------------------------------------------

// encodeVector packs a float32 vector into a little-endian BLOB for the
// semantic_memories.embedding column. Layout: contiguous 4-byte IEEE-754 floats.
func encodeVector(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// decodeVector reverses encodeVector. A blob whose length is not a multiple of 4
// is rejected rather than silently truncated.
func decodeVector(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("embedding blob length %d is not a multiple of 4", len(b))
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out, nil
}

// cosine returns the cosine similarity of a and b in [-1,1]. Returns 0 for
// mismatched lengths or a zero-norm operand (port of _cosine_sim).
func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// ---- schema self-ensure -----------------------------------------------------

// ensureVectorSchema guarantees the embedding/embed_model columns exist. The
// managed schema already ships them, but a DB created by an older build (or a
// test that runs raw SQL) might not — so add them idempotently without touching
// migrate/schema.sql. SQLite has no ADD COLUMN IF NOT EXISTS, so probe first.
func ensureVectorSchema(db *sql.DB) error {
	cols, err := tableColumns(db, "semantic_memories")
	if err != nil {
		return err
	}
	if !cols["embedding"] {
		if _, err := db.Exec(`ALTER TABLE semantic_memories ADD COLUMN embedding BLOB`); err != nil {
			return err
		}
	}
	if !cols["embed_model"] {
		if _, err := db.Exec(`ALTER TABLE semantic_memories ADD COLUMN embed_model TEXT`); err != nil {
			return err
		}
	}
	return nil
}

func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// ---- store / rebuild / search ----------------------------------------------

// VectorHit is one semantic-search result carrying its cosine similarity.
type VectorHit struct {
	Mem
	Similarity float64 `json:"similarity"`
}

// StoreEmbedding embeds a single memory's content and persists the vector as a
// BLOB, recording the model used. Returns the vector length. Errors clearly if
// the memory is missing.
func StoreEmbedding(ctx context.Context, db *sql.DB, e Embedder, profile, id string) (int, error) {
	if e == nil {
		return 0, fmt.Errorf("no embedding backend configured (set OPENAI_API_KEY or TAG_EMBED_BASE_URL)")
	}
	if err := ensureVectorSchema(db); err != nil {
		return 0, err
	}
	var content string
	if err := db.QueryRow(`SELECT content FROM semantic_memories WHERE id=? AND profile=?`, id, profile).Scan(&content); err != nil {
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("memory not found: %q (profile %q)", id, profile)
		}
		return 0, err
	}
	vecs, err := e.Embed(ctx, []string{content})
	if err != nil {
		return 0, err
	}
	if len(vecs) != 1 {
		return 0, fmt.Errorf("expected 1 vector, got %d", len(vecs))
	}
	if _, err := db.Exec(`UPDATE semantic_memories SET embedding=?, embed_model=? WHERE id=?`,
		encodeVector(vecs[0]), e.Model(), id); err != nil {
		return 0, err
	}
	return len(vecs[0]), nil
}

// RebuildEmbeddings embeds every memory in the profile that lacks a vector for
// the active model (or, if force is true, all of them), sending inputs in
// fixed-size batches. Returns the count embedded. Errors clearly when no backend
// is configured.
func RebuildEmbeddings(ctx context.Context, db *sql.DB, e Embedder, profile string, force bool) (int, error) {
	if e == nil {
		return 0, fmt.Errorf("no embedding backend configured (set OPENAI_API_KEY or TAG_EMBED_BASE_URL) — no embeddings written")
	}
	if err := ensureVectorSchema(db); err != nil {
		return 0, err
	}
	var rows *sql.Rows
	var err error
	if force {
		rows, err = db.Query(`SELECT id, content FROM semantic_memories WHERE profile=?`, profile)
	} else {
		// Re-embed rows with no vector, or a vector from a different model.
		rows, err = db.Query(`SELECT id, content FROM semantic_memories
			WHERE profile=? AND (embedding IS NULL OR embed_model IS NULL OR embed_model <> ?)`, profile, e.Model())
	}
	if err != nil {
		return 0, err
	}
	var ids, contents []string
	for rows.Next() {
		var id, content string
		if err := rows.Scan(&id, &content); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		contents = append(contents, content)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	vecs, err := embedAll(ctx, e, contents)
	if err != nil {
		return 0, err
	}
	if len(vecs) != len(ids) {
		return 0, fmt.Errorf("embeddings API returned %d vectors for %d memories", len(vecs), len(ids))
	}
	model := e.Model()
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE semantic_memories SET embedding=?, embed_model=? WHERE id=?`,
			encodeVector(vecs[i]), model, id); err != nil {
			_ = tx.Rollback()
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// SearchByVector embeds the query, cosine-ranks stored vectors, and returns the
// top-limit hits. Only vectors stored under the active embed model are compared,
// so a model switch never ranks against dimension-mismatched vectors. It
// transparently falls back to FTS Search when: no embedder is configured, the
// query cannot be embedded, or no memories carry vectors for the active model
// yet — so offline/keyless use still works. The bool reports whether vector
// ranking was actually used (true) or FTS fallback (false).
func SearchByVector(ctx context.Context, db *sql.DB, e Embedder, profile, query string, limit int) ([]VectorHit, bool, error) {
	if limit <= 0 {
		limit = 10
	}
	if err := ensureVectorSchema(db); err != nil {
		return nil, false, err
	}
	fallback := func() ([]VectorHit, bool, error) {
		var mems []Mem
		var err error
		if strings.TrimSpace(query) == "" {
			mems, err = List(db, profile, "", limit)
		} else {
			mems, err = Search(db, profile, query, limit, "")
		}
		if err != nil {
			return nil, false, err
		}
		hits := make([]VectorHit, 0, len(mems))
		for _, m := range mems {
			hits = append(hits, VectorHit{Mem: m})
		}
		return hits, false, nil
	}

	if e == nil || strings.TrimSpace(query) == "" {
		return fallback()
	}
	qv, err := e.Embed(ctx, []string{query})
	if err != nil || len(qv) != 1 {
		// Embedding failed (offline, quota, bad key) — degrade rather than error.
		return fallback()
	}
	queryVec := qv[0]

	rows, err := db.Query(`SELECT id,profile,content,memory_type,confidence,created_at,accessed_at,access_count,source,embedding
		FROM semantic_memories WHERE profile=? AND embedding IS NOT NULL AND embed_model=?`, profile, e.Model())
	if err != nil {
		return nil, false, err
	}
	var scored []VectorHit
	for rows.Next() {
		var m Mem
		var blob []byte
		if err := rows.Scan(&m.ID, &m.Profile, &m.Content, &m.MemoryType, &m.Confidence,
			&m.CreatedAt, &m.AccessedAt, &m.AccessCount, &m.Source, &blob); err != nil {
			rows.Close()
			return nil, false, err
		}
		docVec, derr := decodeVector(blob)
		if derr != nil {
			continue // skip corrupt vectors rather than fail the whole search
		}
		m.Confidence = effectiveConfidence(m.Confidence, m.MemoryType, m.CreatedAt)
		scored = append(scored, VectorHit{Mem: m, Similarity: cosine(queryVec, docVec)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(scored) == 0 {
		// No stored vectors — fall back to FTS so results aren't empty.
		return fallback()
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].Similarity > scored[j].Similarity })
	if len(scored) > limit {
		scored = scored[:limit]
	}
	// Bump access bookkeeping (mirrors Search / Python search_by_vector).
	if len(scored) > 0 {
		now := nowISO()
		for _, h := range scored {
			_, _ = db.Exec(`UPDATE semantic_memories SET access_count=access_count+1, accessed_at=? WHERE id=?`, now, h.ID)
		}
	}
	return scored, true, nil
}
