package memory

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// UpdateFact invalidates the old version of a fact (snapshotting it into
// memory_fact_history) and inserts a new version. Returns the new memory id.
// Port of semantic_memory.update_fact (PRD-069 temporal versioning).
func UpdateFact(db *sql.DB, memID, newContent, profile, reason string) (string, error) {
	newContent = strings.TrimSpace(newContent)
	if newContent == "" {
		return "", fmt.Errorf("memory content must not be empty")
	}
	var oldID, oldProfile, oldContent, oldType, oldSrc, oldCreated, oldValidAt string
	var oldConf float64
	var oldAccessed sql.NullString
	var oldCount sql.NullInt64
	err := db.QueryRow(`SELECT id, profile, content, memory_type, confidence, created_at,
		accessed_at, access_count, source, COALESCE(valid_at, created_at)
		FROM semantic_memories WHERE id=? AND profile=?`, memID, profile).
		Scan(&oldID, &oldProfile, &oldContent, &oldType, &oldConf, &oldCreated, &oldAccessed, &oldCount, &oldSrc, &oldValidAt)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("memory %q not found for profile %q", memID, profile)
	}
	if err != nil {
		return "", err
	}
	now := nowISO()
	historyID := uuid.NewString()[:16]
	newID := uuid.NewString()[:16]
	if _, err := db.Exec(`INSERT INTO memory_fact_history
		(history_id,original_id,successor_id,profile,content,memory_type,confidence,source,valid_at,invalid_at,reason,archived_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		historyID, oldID, newID, oldProfile, oldContent, oldType, oldConf, oldSrc, oldValidAt, now, reason, now); err != nil {
		return "", err
	}
	db.Exec(`DELETE FROM semantic_memories WHERE id=?`, oldID)
	db.Exec(`DELETE FROM semantic_memories_fts WHERE id=?`, oldID)
	if _, err := db.Exec(`INSERT INTO semantic_memories
		(id,profile,content,memory_type,confidence,created_at,accessed_at,access_count,source,valid_at,tier)
		VALUES(?,?,?,?,?,?,?,0,?,?,'archival')`,
		newID, oldProfile, newContent, oldType, oldConf, now, now, oldSrc, now); err != nil {
		return "", err
	}
	db.Exec(`INSERT INTO semantic_memories_fts(id,profile,content,memory_type) VALUES(?,?,?,?)`, newID, oldProfile, newContent, oldType)
	return newID, nil
}

// FactVersion is one entry in a fact's history.
type FactVersion struct {
	HistoryID   string  `json:"history_id"`
	OriginalID  string  `json:"original_id"`
	SuccessorID string  `json:"successor_id"`
	Profile     string  `json:"profile"`
	Content     string  `json:"content"`
	MemoryType  string  `json:"memory_type"`
	Confidence  float64 `json:"confidence"`
	Source      string  `json:"source"`
	ValidAt     string  `json:"valid_at"`
	InvalidAt   string  `json:"invalid_at"`
	Reason      string  `json:"reason"`
	Current     bool    `json:"_current,omitempty"`
}

// FactHistory returns all archived versions of a fact plus the live row.
func FactHistory(db *sql.DB, memID string) ([]FactVersion, error) {
	rows, err := db.Query(`SELECT history_id, original_id, COALESCE(successor_id,''), profile, content,
		memory_type, confidence, source, valid_at, invalid_at, reason
		FROM memory_fact_history WHERE original_id=? OR successor_id=? ORDER BY valid_at ASC`, memID, memID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FactVersion
	for rows.Next() {
		var v FactVersion
		if err := rows.Scan(&v.HistoryID, &v.OriginalID, &v.SuccessorID, &v.Profile, &v.Content,
			&v.MemoryType, &v.Confidence, &v.Source, &v.ValidAt, &v.InvalidAt, &v.Reason); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	// append the live version if it matches
	var v FactVersion
	var invalid sql.NullString
	err = db.QueryRow(`SELECT id, profile, content, memory_type, confidence, source,
		COALESCE(valid_at, created_at), invalid_at FROM semantic_memories WHERE id=?`, memID).
		Scan(&v.OriginalID, &v.Profile, &v.Content, &v.MemoryType, &v.Confidence, &v.Source, &v.ValidAt, &invalid)
	if err == nil {
		v.InvalidAt = invalid.String
		v.Current = true
		out = append(out, v)
	}
	return out, nil
}

// FactAt returns memories valid at a given ISO timestamp — BOTH live rows and
// archived versions from memory_fact_history (the point-in-time query must see
// versions that have since been superseded, matching Python's list_facts_at).
// Accepts a full RFC3339 timestamp or a date-only value like "2026-01-01".
func FactAt(db *sql.DB, profile, atTime string) ([]Mem, error) {
	if _, err := parseISOFlexible(atTime); err != nil {
		return nil, fmt.Errorf("at_time is not a valid ISO-8601 timestamp: %q", atTime)
	}
	var out []Mem
	// live rows valid at atTime
	rows, err := db.Query(`SELECT id, profile, content, memory_type, confidence, created_at,
		accessed_at, access_count, source FROM semantic_memories
		WHERE profile=? AND COALESCE(valid_at, created_at) <= ? AND (invalid_at IS NULL OR invalid_at > ?)`,
		profile, atTime, atTime)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m Mem
		if err := rows.Scan(&m.ID, &m.Profile, &m.Content, &m.MemoryType, &m.Confidence, &m.CreatedAt, &m.AccessedAt, &m.AccessCount, &m.Source); err != nil {
			rows.Close()
			return nil, err
		}
		m.Confidence = effectiveConfidence(m.Confidence, m.MemoryType, m.CreatedAt)
		out = append(out, m)
	}
	rows.Close()
	// archived versions valid at atTime (valid_at <= atTime < invalid_at)
	hrows, err := db.Query(`SELECT original_id, profile, content, memory_type, confidence, valid_at
		FROM memory_fact_history WHERE profile=? AND valid_at <= ? AND invalid_at > ?`,
		profile, atTime, atTime)
	if err != nil {
		return nil, err
	}
	defer hrows.Close()
	for hrows.Next() {
		var m Mem
		if err := hrows.Scan(&m.ID, &m.Profile, &m.Content, &m.MemoryType, &m.Confidence, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.Source = "history"
		out = append(out, m)
	}
	return out, hrows.Err()
}

// parseISOFlexible accepts a full RFC3339 timestamp or a bare date (YYYY-MM-DD).
func parseISOFlexible(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", s)
}
