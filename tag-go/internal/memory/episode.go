package memory

import (
	"database/sql"

	"github.com/google/uuid"
)

// Episode is an episodic-memory session (port of semantic_memory episodes).
type Episode struct {
	EpisodeID   string `json:"episode_id"`
	Profile     string `json:"profile"`
	Description string `json:"description"`
	StartedAt   string `json:"started_at"`
	EndedAt     string `json:"ended_at"`
	Summary     string `json:"summary"`
	Status      string `json:"status"`
	MemoryCount int    `json:"memory_count"`
}

// StartEpisode opens a new episode and returns its id.
func StartEpisode(db *sql.DB, profile, description string) (string, error) {
	id := uuid.NewString()[:16]
	_, err := db.Exec(`INSERT INTO memory_episodes(episode_id,profile,description,session_id,started_at,status)
		VALUES(?,?,?,NULL,?,'open')`, id, profile, description, nowISO())
	return id, err
}

// EndEpisode closes an episode with an optional summary. Returns found.
func EndEpisode(db *sql.DB, episodeID, summary string) (bool, error) {
	res, err := db.Exec(`UPDATE memory_episodes SET status='closed', ended_at=?, summary=? WHERE episode_id=?`,
		nowISO(), summary, episodeID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListEpisodes returns recent episodes for a profile (newest first) with counts.
func ListEpisodes(db *sql.DB, profile string, limit int) ([]Episode, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.Query(`SELECT e.episode_id, e.profile, e.description,
		COALESCE(e.started_at,''), COALESCE(e.ended_at,''), COALESCE(e.summary,''), e.status,
		COUNT(el.memory_id)
		FROM memory_episodes e LEFT JOIN memory_episode_links el ON el.episode_id=e.episode_id
		WHERE e.profile=? GROUP BY e.episode_id ORDER BY e.started_at DESC LIMIT ?`, profile, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Episode
	for rows.Next() {
		var e Episode
		if err := rows.Scan(&e.EpisodeID, &e.Profile, &e.Description, &e.StartedAt, &e.EndedAt, &e.Summary, &e.Status, &e.MemoryCount); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// TagMemoryWithEpisode links a memory to an episode (idempotent).
func TagMemoryWithEpisode(db *sql.DB, memoryID, episodeID string) (bool, error) {
	_, err := db.Exec(`INSERT OR IGNORE INTO memory_episode_links(memory_id,episode_id,linked_at) VALUES(?,?,?)`,
		memoryID, episodeID, nowISO())
	return err == nil, err
}

// EpisodeMemories returns memories linked to an episode (decayed confidence).
func EpisodeMemories(db *sql.DB, episodeID string) ([]Mem, error) {
	rows, err := db.Query(`SELECT sm.id, sm.profile, sm.content, sm.memory_type, sm.confidence,
		sm.created_at, sm.accessed_at, sm.access_count, sm.source
		FROM memory_episode_links el JOIN semantic_memories sm ON sm.id=el.memory_id
		WHERE el.episode_id=? ORDER BY el.linked_at ASC`, episodeID)
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
