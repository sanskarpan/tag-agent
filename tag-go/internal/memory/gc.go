package memory

import (
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// GCConfig holds tunable garbage-collection parameters (port of memory_gc.GCConfig).
type GCConfig struct {
	MinConfidenceToKeep   float64
	DedupSimilarityThresh float64
	MaxMemoriesPerProfile int
	PromoteThreshold      float64
}

// DefaultGCConfig returns the Python defaults.
func DefaultGCConfig() GCConfig {
	return GCConfig{
		MinConfidenceToKeep:   0.05,
		DedupSimilarityThresh: 0.75,
		MaxMemoriesPerProfile: 500,
		PromoteThreshold:      0.9,
	}
}

// GCResult summarizes one GC run for a profile.
type GCResult struct {
	Profile         string  `json:"profile"`
	EvictedCount    int     `json:"evicted_count"`
	MergedCount     int     `json:"merged_count"`
	PromotedCount   int     `json:"promoted_count"`
	DurationSeconds float64 `json:"duration_seconds"`
	RunAt           string  `json:"run_at"`
}

func jaccard(a, b string) float64 {
	setA := map[string]bool{}
	for _, w := range strings.Fields(strings.ToLower(a)) {
		setA[w] = true
	}
	setB := map[string]bool{}
	for _, w := range strings.Fields(strings.ToLower(b)) {
		setB[w] = true
	}
	union := map[string]bool{}
	for w := range setA {
		union[w] = true
	}
	for w := range setB {
		union[w] = true
	}
	if len(union) == 0 {
		return 0
	}
	inter := 0
	for w := range setA {
		if setB[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(union))
}

func deleteMemoryRow(db *sql.DB, id, profile string) {
	db.Exec(`DELETE FROM semantic_memories WHERE id=? AND profile=?`, id, profile)
	db.Exec(`DELETE FROM semantic_memories_fts WHERE id=?`, id)
}

type gcMem struct {
	id, content, mtype, created string
	confBase                    float64
}

// EvictLowConfidence removes memories below the confidence floor, then enforces
// the per-profile cap by removing the weakest survivors. Returns count evicted.
func EvictLowConfidence(db *sql.DB, profile string, cfg GCConfig) (int, error) {
	rows, err := db.Query(`SELECT id, memory_type, confidence, created_at FROM semantic_memories WHERE profile=? ORDER BY created_at ASC`, profile)
	if err != nil {
		return 0, err
	}
	var mems []gcMem
	for rows.Next() {
		var m gcMem
		if err := rows.Scan(&m.id, &m.mtype, &m.confBase, &m.created); err != nil {
			rows.Close()
			return 0, err
		}
		mems = append(mems, m)
	}
	rows.Close()

	evicted := map[string]bool{}
	effMap := map[string]float64{}
	for _, m := range mems {
		eff := effectiveConfidence(m.confBase, m.mtype, m.created)
		effMap[m.id] = eff
		if eff < cfg.MinConfidenceToKeep {
			deleteMemoryRow(db, m.id, profile)
			evicted[m.id] = true
		}
	}
	// Cap enforcement over survivors.
	type surv struct {
		eff float64
		id  string
	}
	var survivors []surv
	for id, eff := range effMap {
		if !evicted[id] {
			survivors = append(survivors, surv{eff, id})
		}
	}
	capEvicted := 0
	if len(survivors) > cfg.MaxMemoriesPerProfile {
		overage := len(survivors) - cfg.MaxMemoriesPerProfile
		sort.Slice(survivors, func(i, j int) bool { return survivors[i].eff < survivors[j].eff })
		for _, s := range survivors[:overage] {
			deleteMemoryRow(db, s.id, profile)
			capEvicted++
		}
	}
	return len(evicted) + capEvicted, nil
}

// MergeDuplicates removes the weaker copy of each near-duplicate pair (Jaccard >
// threshold), keeping the higher effective-confidence memory. Returns count removed.
func MergeDuplicates(db *sql.DB, profile string, cfg GCConfig) (int, error) {
	rows, err := db.Query(`SELECT id, content, memory_type, confidence, created_at FROM semantic_memories WHERE profile=?`, profile)
	if err != nil {
		return 0, err
	}
	var mems []gcMem
	for rows.Next() {
		var m gcMem
		if err := rows.Scan(&m.id, &m.content, &m.mtype, &m.confBase, &m.created); err != nil {
			rows.Close()
			return 0, err
		}
		mems = append(mems, m)
	}
	rows.Close()
	if len(mems) < 2 {
		return 0, nil
	}
	byID := map[string]gcMem{}
	for _, m := range mems {
		byID[m.id] = m
	}
	deleted := map[string]bool{}
	merged := 0
	for i := 0; i < len(mems); i++ {
		for j := i + 1; j < len(mems); j++ {
			a, b := mems[i], mems[j]
			if deleted[a.id] || deleted[b.id] {
				continue
			}
			if jaccard(a.content, b.content) <= cfg.DedupSimilarityThresh {
				continue
			}
			effA := effectiveConfidence(a.confBase, a.mtype, a.created)
			effB := effectiveConfidence(b.confBase, b.mtype, b.created)
			loser := b.id
			if effA < effB {
				loser = a.id
			}
			deleteMemoryRow(db, loser, profile)
			deleted[loser] = true
			merged++
		}
	}
	return merged, nil
}

// PromoteHighAccess boosts base confidence of frequently-accessed memories
// (access_count > 5 and confidence < promote_threshold → min(1, conf*1.2)).
func PromoteHighAccess(db *sql.DB, profile string, cfg GCConfig) (int, error) {
	rows, err := db.Query(`SELECT id, confidence FROM semantic_memories WHERE profile=? AND access_count > 5 AND confidence < ?`, profile, cfg.PromoteThreshold)
	if err != nil {
		return 0, err
	}
	type pr struct {
		id   string
		conf float64
	}
	var toPromote []pr
	for rows.Next() {
		var p pr
		if err := rows.Scan(&p.id, &p.conf); err != nil {
			rows.Close()
			return 0, err
		}
		toPromote = append(toPromote, p)
	}
	rows.Close()
	for _, p := range toPromote {
		nc := p.conf * 1.2
		if nc > 1.0 {
			nc = 1.0
		}
		db.Exec(`UPDATE semantic_memories SET confidence=? WHERE id=?`, nc, p.id)
	}
	return len(toPromote), nil
}

// RunGC runs a full GC cycle (evict → merge → promote) for a profile, records an
// audit row, and returns the summary.
func RunGC(db *sql.DB, profile string, cfg GCConfig) (GCResult, error) {
	runAt := nowISO()
	t0 := time.Now()
	evicted, err := EvictLowConfidence(db, profile, cfg)
	if err != nil {
		return GCResult{}, err
	}
	merged, err := MergeDuplicates(db, profile, cfg)
	if err != nil {
		return GCResult{}, err
	}
	promoted, err := PromoteHighAccess(db, profile, cfg)
	if err != nil {
		return GCResult{}, err
	}
	dur := time.Since(t0).Seconds()
	id := uuid.NewString()[:16]
	if _, err := db.Exec(`INSERT INTO memory_gc_runs(id,profile,evicted,merged,promoted,duration_s,run_at) VALUES(?,?,?,?,?,?,?)`,
		id, profile, evicted, merged, promoted, dur, runAt); err != nil {
		return GCResult{}, err
	}
	return GCResult{Profile: profile, EvictedCount: evicted, MergedCount: merged, PromotedCount: promoted, DurationSeconds: dur, RunAt: runAt}, nil
}

// RunGCAllProfiles runs GC for every distinct profile in semantic_memories.
func RunGCAllProfiles(db *sql.DB, cfg GCConfig) ([]GCResult, error) {
	rows, err := db.Query(`SELECT DISTINCT profile FROM semantic_memories`)
	if err != nil {
		return nil, err
	}
	var profiles []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		profiles = append(profiles, p)
	}
	rows.Close()
	var results []GCResult
	for _, p := range profiles {
		r, err := RunGC(db, p, cfg)
		if err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, nil
}
