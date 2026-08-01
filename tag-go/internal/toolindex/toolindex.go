// Package toolindex implements retrieval over the MCP tool registry (PRD-043).
//
// Two retrieval backends share one index:
//
//   - vector — cosine similarity over embeddings of "{name}: {description}",
//     produced by the SAME internal/memory embedding machinery that backs
//     `mem2 store search` (OpenAI-compatible endpoint, float32 BLOB storage).
//   - keyword — the original term-overlap scan, kept as the honest fallback for
//     the keyless/offline case.
//
// Which backend actually ran is always reported back to the caller (Mode*), so
// no caller can mistake a keyword result for a semantic one. Nothing here ever
// contacts an embeddings endpoint unless the user configured one
// (TAG_EMBED_BASE_URL / TAG_EMBED_API_KEY / OPENAI_API_KEY).
package toolindex

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tag-agent/tag/internal/memory"
)

// Retrieval modes reported by Build/Search.
const (
	ModeVector  = "vector"
	ModeKeyword = "keyword"
)

// Tool is one indexed MCP tool plus its retrieval score.
type Tool struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Server      string  `json:"server"`
	Score       float64 `json:"score"`
}

// Status summarizes the persisted index.
type Status struct {
	Built       bool   `json:"built"`
	ToolCount   int    `json:"tool_count"`
	BuiltAt     string `json:"built_at,omitempty"`
	Backend     string `json:"backend"`
	Vectors     int    `json:"vectors"`
	EmbedModel  string `json:"embed_model,omitempty"`
	VectorReady bool   `json:"vector_ready"`
}

// EnsureSchema adds the vector columns to tool_index idempotently.
//
// It deliberately does NOT edit migrate/schema.sql: that file is shared with
// every other subsystem, and SQLite has no ADD COLUMN IF NOT EXISTS, so the
// columns are probed and added here instead. Safe to call on every command.
func EnsureSchema(db *sql.DB) error {
	cols, err := tableColumns(db, "tool_index")
	if err != nil {
		return err
	}
	if !cols["embedding"] {
		if _, err := db.Exec(`ALTER TABLE tool_index ADD COLUMN embedding BLOB`); err != nil {
			return err
		}
	}
	if !cols["embed_model"] {
		if _, err := db.Exec(`ALTER TABLE tool_index ADD COLUMN embed_model TEXT`); err != nil {
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
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		cols[n] = true
	}
	return cols, rows.Err()
}

// Document is the text that gets embedded for a tool. Exposed so the CLI and
// tests agree on exactly what the vector represents.
func Document(name, description string) string {
	d := strings.TrimSpace(description)
	if d == "" {
		d = "(no description)"
	}
	return name + ": " + d
}

// BuildResult reports what an index build actually did.
type BuildResult struct {
	Tools    int    `json:"tools"`
	Embedded int    `json:"embedded"`
	Mode     string `json:"mode"`
	Model    string `json:"embed_model,omitempty"`
	Note     string `json:"note,omitempty"`
}

// Build replaces the tool index from the registry. When e is non-nil the tool
// documents are embedded too, enabling vector retrieval; when it is nil (or
// embedding fails) the index is still written and retrieval stays keyword-based.
// An embedding failure NEVER fails the build — it downgrades the mode and says so.
func Build(ctx context.Context, db *sql.DB, e memory.Embedder, tools []Tool) (BuildResult, error) {
	res := BuildResult{Mode: ModeKeyword}
	if err := EnsureSchema(db); err != nil {
		return res, err
	}

	var vecs [][]float32
	if e != nil && len(tools) > 0 {
		docs := make([]string, len(tools))
		for i, t := range tools {
			docs[i] = Document(t.Name, t.Description)
		}
		v, err := memory.EmbedAll(ctx, e, docs)
		if err != nil {
			res.Note = fmt.Sprintf("embedding backend unavailable (%v) — index built keyword-only", err)
		} else if len(v) != len(tools) {
			res.Note = fmt.Sprintf("embeddings backend returned %d vectors for %d tools — index built keyword-only", len(v), len(tools))
		} else {
			vecs = v
		}
	} else if e == nil {
		res.Note = "no embedding backend configured (set TAG_EMBED_BASE_URL or OPENAI_API_KEY) — keyword retrieval only"
	}

	tx, err := db.Begin()
	if err != nil {
		return res, err
	}
	if _, err := tx.Exec(`DELETE FROM tool_index`); err != nil {
		_ = tx.Rollback()
		return res, err
	}
	model := ""
	if vecs != nil {
		model = e.Model()
	}
	for i, t := range tools {
		var blob []byte
		var mdl any
		if vecs != nil {
			blob = memory.EncodeVector(vecs[i])
			mdl = model
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO tool_index(name,description,server,embedding,embed_model) VALUES(?,?,?,?,?)`,
			t.Name, t.Description, t.Server, blob, mdl); err != nil {
			_ = tx.Rollback()
			return res, err
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(`INSERT OR REPLACE INTO tool_index_meta(id,tool_count,built_at) VALUES('singleton',?,?)`, len(tools), now); err != nil {
		_ = tx.Rollback()
		return res, err
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	res.Tools = len(tools)
	if vecs != nil {
		res.Embedded = len(tools)
		res.Mode = ModeVector
		res.Model = model
	}
	return res, nil
}

// Search returns the top-k tools for query and the retrieval mode that produced
// them. Vector ranking is used only when an embedder is configured AND the index
// carries vectors for that embedder's model AND the query embeds successfully;
// every other path degrades to keyword and reports ModeKeyword.
func Search(ctx context.Context, db *sql.DB, e memory.Embedder, query string, topK int) ([]Tool, string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, "", fmt.Errorf("query must not be empty")
	}
	if err := EnsureSchema(db); err != nil {
		return nil, "", err
	}
	if e != nil {
		hits, ok, err := vectorSearch(ctx, db, e, query, topK)
		if err != nil {
			return nil, "", err
		}
		if ok {
			return hits, ModeVector, nil
		}
	}
	hits, err := keywordSearch(db, query, topK)
	return hits, ModeKeyword, err
}

// vectorSearch cosine-ranks the stored vectors. The bool reports whether vector
// ranking was actually possible; false means "fall back to keyword", never an
// empty success.
func vectorSearch(ctx context.Context, db *sql.DB, e memory.Embedder, query string, topK int) ([]Tool, bool, error) {
	qv, err := e.Embed(ctx, []string{query})
	if err != nil || len(qv) != 1 {
		return nil, false, nil // offline / bad key / quota: degrade, don't fail
	}
	rows, err := db.Query(`SELECT name, description, server, embedding FROM tool_index
		WHERE embedding IS NOT NULL AND embed_model = ?`, e.Model())
	if err != nil {
		return nil, false, err
	}
	scored := []Tool{}
	for rows.Next() {
		var t Tool
		var blob []byte
		if err := rows.Scan(&t.Name, &t.Description, &t.Server, &blob); err != nil {
			rows.Close()
			return nil, false, err
		}
		vec, derr := memory.DecodeVector(blob)
		if derr != nil {
			continue // skip a corrupt row rather than fail the whole search
		}
		t.Score = memory.Cosine(qv[0], vec)
		scored = append(scored, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(scored) == 0 {
		// Index never embedded (or embedded under a different model) — the honest
		// answer is the keyword backend, not an empty vector result.
		return nil, false, nil
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	if topK > 0 && len(scored) > topK {
		scored = scored[:topK]
	}
	return scored, true, nil
}

// keywordSearch is the original term-overlap scan: score = number of distinct
// query terms present in "name description".
func keywordSearch(db *sql.DB, query string, topK int) ([]Tool, error) {
	rows, err := db.Query(`SELECT name, description, server FROM tool_index`)
	if err != nil {
		return nil, err
	}
	var all []Tool
	for rows.Next() {
		var t Tool
		if err := rows.Scan(&t.Name, &t.Description, &t.Server); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	terms := strings.Fields(strings.ToLower(query))
	// Non-nil so an empty result marshals to `[]`, not `null` (#559).
	scored := []Tool{}
	for _, t := range all {
		text := strings.ToLower(t.Name + " " + t.Description)
		s := 0
		for _, w := range terms {
			if strings.Contains(text, w) {
				s++
			}
		}
		if s > 0 {
			t.Score = float64(s)
			scored = append(scored, t)
		}
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	if topK > 0 && len(scored) > topK {
		scored = scored[:topK]
	}
	return scored, nil
}

// Load reports the current index status, including whether vector retrieval is
// actually available (vectors present for the configured model).
func Load(db *sql.DB, e memory.Embedder) (Status, error) {
	st := Status{Backend: ModeKeyword}
	if err := EnsureSchema(db); err != nil {
		return st, err
	}
	err := db.QueryRow(`SELECT tool_count, built_at FROM tool_index_meta WHERE id='singleton'`).Scan(&st.ToolCount, &st.BuiltAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return st, nil
		}
		return st, nil // meta table missing/unreadable: report "not built" rather than erroring
	}
	st.Built = true
	var model sql.NullString
	if err := db.QueryRow(`SELECT COUNT(*), MAX(embed_model) FROM tool_index WHERE embedding IS NOT NULL`).Scan(&st.Vectors, &model); err != nil {
		return st, err
	}
	if model.Valid {
		st.EmbedModel = model.String
	}
	if st.Vectors > 0 && e != nil && st.EmbedModel == e.Model() {
		st.Backend = ModeVector
		st.VectorReady = true
	}
	return st, nil
}
