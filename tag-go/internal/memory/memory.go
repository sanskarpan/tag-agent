// Package memory implements semantic memory with confidence decay + FTS5/BM25
// hybrid search (Go port of semantic_memory.py). Pure arithmetic, no numpy peer needed.
package memory

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Mem is one semantic memory row.
type Mem struct {
	ID          string  `json:"id"`
	Profile     string  `json:"profile"`
	Content     string  `json:"content"`
	MemoryType  string  `json:"memory_type"`
	Confidence  float64 `json:"confidence"`
	CreatedAt   string  `json:"created_at"`
	AccessedAt  string  `json:"accessed_at"`
	AccessCount int     `json:"access_count"`
	Source      string  `json:"source"`
}

// halfLives maps memory_type to its confidence half-life in days (port of
// semantic_memory.HALF_LIVES). "convention" never decays. Unknown types default
// to 60 days (matching Python's HALF_LIVES.get(memory_type, 60.0)).
var halfLives = map[string]float64{
	"convention": -1, // sentinel: never decays
	"decision":   180.0,
	"gotcha":     90.0,
	"fact":       90.0,
	"other":      60.0,
}

const defaultHalfLifeDays = 60.0

func nowISO() string { return time.Now().UTC().Format(time.RFC3339) }

// Add inserts a memory, validating confidence in (0,1].
func Add(db *sql.DB, profile, content, memType string, confidence float64) (string, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return "", fmt.Errorf("memory content required")
	}
	if confidence <= 0 || confidence > 1 {
		return "", fmt.Errorf("confidence must be in (0, 1], got %g", confidence)
	}
	if memType == "" {
		memType = "fact"
	}
	if _, ok := halfLives[memType]; !ok {
		return "", fmt.Errorf("memory_type must be one of convention/decision/gotcha/fact/other, got %q", memType)
	}
	id := uuid.NewString()[:16]
	now := nowISO()
	_, err := db.Exec(`INSERT INTO semantic_memories(id,profile,content,memory_type,confidence,created_at,accessed_at,access_count,source,tier)
		VALUES(?,?,?,?,?,?,?,0,'manual','archival')`, id, profile, content, memType, confidence, now, now)
	if err != nil {
		return "", err
	}
	_, _ = db.Exec(`INSERT INTO semantic_memories_fts(id,profile,content,memory_type) VALUES(?,?,?,?)`, id, profile, content, memType)
	return id, nil
}

// effectiveConfidence applies type-specific exponential decay from created_at.
// "convention" memories never decay (returns base unchanged).
func effectiveConfidence(base float64, memType, createdAt string) float64 {
	hl, ok := halfLives[memType]
	if !ok {
		hl = defaultHalfLifeDays
	}
	if hl < 0 { // convention: never decays
		return base
	}
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return base
	}
	ageDays := time.Since(t).Hours() / 24.0
	factor := math.Pow(0.5, ageDays/hl)
	return base * factor
}

// List returns memories for a profile (newest first), decay applied.
func List(db *sql.DB, profile, memType string, limit int) ([]Mem, error) {
	if limit < 0 {
		return nil, fmt.Errorf("limit must be >= 0")
	}
	q := `SELECT id,profile,content,memory_type,confidence,created_at,accessed_at,access_count,source FROM semantic_memories WHERE profile=?`
	args := []any{profile}
	if memType != "" {
		q += ` AND memory_type=?`
		args = append(args, memType)
	}
	q += ` ORDER BY created_at DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	return scan(db, q, args...)
}

// Search runs FTS5 MATCH ranked by BM25 (SQLite built-in), decay applied.
func Search(db *sql.DB, profile, query string, limit int, memType string) ([]Mem, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("query required")
	}
	if limit <= 0 {
		limit = 10
	}
	// FTS join; bm25() lower is better
	q := `SELECT m.id,m.profile,m.content,m.memory_type,m.confidence,m.created_at,m.accessed_at,m.access_count,m.source
		FROM semantic_memories_fts f JOIN semantic_memories m ON m.id=f.id
		WHERE f.profile=? AND semantic_memories_fts MATCH ?`
	args := []any{profile, ftsEscape(query)}
	if memType != "" {
		q += ` AND m.memory_type=?`
		args = append(args, memType)
	}
	q += ` ORDER BY bm25(semantic_memories_fts, 1.5, 0.75) LIMIT ?`
	args = append(args, limit)
	res, err := scan(db, q, args...)
	if err != nil {
		// FTS may fail on odd queries; degrade to LIKE
		like := `SELECT id,profile,content,memory_type,confidence,created_at,accessed_at,access_count,source
			FROM semantic_memories WHERE profile=? AND content LIKE ? ORDER BY created_at DESC LIMIT ?`
		res, err = scan(db, like, profile, "%"+query+"%", limit)
		if err != nil {
			return nil, err
		}
	}
	// Bump access bookkeeping on each hit (mirrors the Python semantic_memory,
	// and is what PromoteHighAccess in GC depends on).
	if len(res) > 0 {
		now := nowISO()
		for _, m := range res {
			db.Exec(`UPDATE semantic_memories SET access_count=access_count+1, accessed_at=? WHERE id=?`, now, m.ID)
		}
	}
	return res, nil
}

// ftsEscape quotes each term so punctuation can't break the MATCH grammar, and
// joins with a space — FTS5 treats space-separated terms as an implicit AND
// (matching the Python query semantics; the previous OR silently broadened results).
func ftsEscape(q string) string {
	parts := strings.Fields(q)
	for i, p := range parts {
		parts[i] = `"` + strings.ReplaceAll(p, `"`, "") + `"`
	}
	return strings.Join(parts, " ")
}

// Forget deletes a memory by id.
func Forget(db *sql.DB, profile, id string) (bool, error) {
	r, err := db.Exec(`DELETE FROM semantic_memories WHERE id=? AND profile=?`, id, profile)
	if err != nil {
		return false, err
	}
	_, _ = db.Exec(`DELETE FROM semantic_memories_fts WHERE id=?`, id)
	n, _ := r.RowsAffected()
	return n > 0, nil
}

// Stats returns per-type counts with both the average BASE confidence
// (avg_confidence_base — the stored value, matching Python) and the average
// effective (age-decayed) confidence (avg_confidence).
func Stats(db *sql.DB, profile string) (map[string]map[string]any, error) {
	mems, err := List(db, profile, "", 0)
	if err != nil {
		return nil, err
	}
	// base confidence per type, straight from the stored column (no decay)
	baseSum := map[string]float64{}
	rows, err := db.Query(`SELECT memory_type, confidence FROM semantic_memories WHERE profile=?`, profile)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var mt string
		var c float64
		if err := rows.Scan(&mt, &c); err != nil {
			rows.Close()
			return nil, err
		}
		baseSum[mt] += c
	}
	rows.Close()

	out := map[string]map[string]any{}
	for _, m := range mems {
		s := out[m.MemoryType]
		if s == nil {
			s = map[string]any{"count": 0, "sum_conf": 0.0}
			out[m.MemoryType] = s
		}
		s["count"] = s["count"].(int) + 1
		// m.Confidence is already decayed by List; don't re-apply (avoid double decay).
		s["sum_conf"] = s["sum_conf"].(float64) + m.Confidence
	}
	for mt, s := range out {
		n := s["count"].(int)
		s["avg_confidence"] = s["sum_conf"].(float64) / float64(n)
		s["avg_confidence_base"] = baseSum[mt] / float64(n)
		delete(s, "sum_conf")
	}
	return out, nil
}

func scan(db *sql.DB, q string, args ...any) ([]Mem, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mem
	for rows.Next() {
		var m Mem
		if err := rows.Scan(&m.ID, &m.Profile, &m.Content, &m.MemoryType, &m.Confidence, &m.CreatedAt, &m.AccessedAt, &m.AccessCount, &m.Source); err != nil {
			return nil, err
		}
		m.Confidence = effectiveConfidence(m.Confidence, m.MemoryType, m.CreatedAt)
		out = append(out, m)
	}
	return out, rows.Err()
}

// Tier classifies a memory into core/recall/archival from its EFFECTIVE
// (age-decayed) confidence and age. Port of semantic_memory.get_memory_tier:
// core if eff>=0.8; recall if eff>=0.4 and age<=90d; else archival.
func Tier(effConfidence float64, createdAt string) string {
	if effConfidence >= 0.8 {
		return "core"
	}
	ageDays := 0.0
	if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
		ageDays = time.Since(t).Hours() / 24.0
	}
	if effConfidence >= 0.4 && ageDays <= 90 {
		return "recall"
	}
	return "archival"
}

// MemoryTiers lists the tier names in classification order.
var MemoryTiers = []string{"core", "recall", "archival"}
