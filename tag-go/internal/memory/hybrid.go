package memory

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Hybrid retrieval modes (PRD-066).
const (
	// ModeFTS is the sparse-only path: FTS5/BM25 plus the unconditional LIKE
	// substring union that keeps CJK / partial-word recall alive (issue #574).
	ModeFTS = "fts"
	// ModeDense is the vector-only path (cosine over stored embeddings).
	ModeDense = "dense"
	// ModeHybrid fuses the two with reciprocal rank fusion.
	ModeHybrid = "hybrid"
)

// rrfK is the reciprocal-rank-fusion constant from PRD-066 FR-04. 60 is the
// value used by the original RRF paper and by Weaviate/Pinecone hybrid search.
const rrfK = 60.0

// HybridHit is one fused search result. It embeds Mem so every existing
// consumer field (id/content/confidence/...) keeps working, and adds the three
// score components required for transparency (PRD-066 FR-07, G5).
type HybridHit struct {
	Mem
	DenseScore  float64 `json:"dense_score"`
	SparseScore float64 `json:"sparse_score"`
	HybridScore float64 `json:"hybrid_score"`
}

// HybridOptions tunes a hybrid search.
type HybridOptions struct {
	// Mode is one of ModeFTS / ModeDense / ModeHybrid.
	Mode string
	// Alpha is the fusion weight: 0 = pure sparse, 1 = pure dense, 0.5 balanced.
	Alpha float64
	// Limit caps the returned results.
	Limit int
	// MemType optionally filters on memory_type.
	MemType string
}

// DefaultHybridOptions returns the PRD defaults.
func DefaultHybridOptions() HybridOptions {
	return HybridOptions{Mode: ModeHybrid, Alpha: 0.5, Limit: 10}
}

// candidatePoolFactor widens the per-path candidate pool relative to the
// requested limit so fusion has something to fuse: a document ranked #7 by BM25
// and #1 by cosine has to be visible to BOTH lists to be promoted.
const candidatePoolFactor = 5
const minCandidatePool = 50

// ValidateHybridMode normalizes and validates a --mode value. "bm25" is accepted
// as an alias for "fts" (the PRD spells the sparse path "bm25"; the Go store
// spells it "fts" because SQLite's FTS5 bm25() is what ranks it).
func ValidateHybridMode(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", ModeHybrid:
		return ModeHybrid, nil
	case ModeFTS, "bm25", "sparse", "keyword":
		return ModeFTS, nil
	case ModeDense, "vector":
		return ModeDense, nil
	}
	return "", fmt.Errorf("--mode must be one of hybrid/fts/dense (bm25 = fts, vector = dense), got %q", mode)
}

// SearchHybrid runs the requested retrieval mode and reports the mode that was
// ACTUALLY used. A dense or hybrid request degrades to ModeFTS — never to an
// empty result — when no embedding backend is configured, the query cannot be
// embedded, or no memory in the profile carries a vector for the active model.
//
// The sparse path is memory.Search verbatim, so the FTS+LIKE union that fixed
// CJK/partial-word recall (#574 / Python #567) governs sparse candidates here
// exactly as it does for `mem search`.
func SearchHybrid(ctx context.Context, db *sql.DB, e Embedder, profile, query string, opts HybridOptions) ([]HybridHit, string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, "", fmt.Errorf("query required")
	}
	mode, err := ValidateHybridMode(opts.Mode)
	if err != nil {
		return nil, "", err
	}
	if opts.Limit <= 0 {
		opts.Limit = 10
	}
	if opts.Alpha < 0 || opts.Alpha > 1 {
		return nil, "", fmt.Errorf("--alpha must be in [0,1], got %g", opts.Alpha)
	}
	if err := ensureVectorSchema(db); err != nil {
		return nil, "", err
	}

	pool := opts.Limit * candidatePoolFactor
	if pool < minCandidatePool {
		pool = minCandidatePool
	}

	// AC-03/AC-04: alpha at an endpoint collapses hybrid onto a single path, so
	// the results are byte-identical to the corresponding pure mode rather than
	// "mostly the same order with stragglers appended".
	effective := mode
	if mode == ModeHybrid {
		switch {
		case opts.Alpha <= 0:
			effective = ModeFTS
		case opts.Alpha >= 1:
			effective = ModeDense
		}
	}

	var sparse []Mem
	sparseRank := map[string]int{}
	if effective == ModeFTS || effective == ModeHybrid {
		sparse, err = Search(db, profile, query, pool, opts.MemType)
		if err != nil {
			return nil, "", err
		}
		for i, m := range sparse {
			sparseRank[m.ID] = i
		}
	}

	var dense []VectorHit
	denseRank := map[string]int{}
	denseAvailable := false
	if effective == ModeDense || effective == ModeHybrid {
		dense, denseAvailable, err = denseCandidates(ctx, db, e, profile, query, opts.MemType, pool)
		if err != nil {
			return nil, "", err
		}
		for i, h := range dense {
			denseRank[h.ID] = i
		}
	}

	// Honest degradation: a dense/hybrid request with no usable vector path is
	// answered by the sparse path and REPORTED as fts.
	if !denseAvailable && (effective == ModeDense || effective == ModeHybrid) {
		effective = ModeFTS
		if sparse == nil {
			sparse, err = Search(db, profile, query, pool, opts.MemType)
			if err != nil {
				return nil, "", err
			}
			sparseRank = map[string]int{}
			for i, m := range sparse {
				sparseRank[m.ID] = i
			}
		}
		dense = nil
		denseRank = map[string]int{}
	}

	// Union the candidate sets, keeping one Mem per id.
	byID := map[string]Mem{}
	order := []string{}
	addMem := func(m Mem) {
		if _, ok := byID[m.ID]; !ok {
			byID[m.ID] = m
			order = append(order, m.ID)
		}
	}
	for _, m := range sparse {
		addMem(m)
	}
	for _, h := range dense {
		addMem(h.Mem)
	}

	denseSim := map[string]float64{}
	for _, h := range dense {
		denseSim[h.ID] = h.Similarity
	}
	// Sparse "score" is the reciprocal rank: SQLite's bm25() value is not exposed
	// through the Mem rows, and RRF only needs the rank anyway.
	rrf := func(rank int, ok bool) float64 {
		if !ok {
			return 0
		}
		return 1.0 / (rrfK + float64(rank) + 1.0)
	}

	alpha := opts.Alpha
	switch effective {
	case ModeFTS:
		alpha = 0
	case ModeDense:
		alpha = 1
	}

	hits := make([]HybridHit, 0, len(order))
	for _, id := range order {
		m := byID[id]
		sr, sok := sparseRank[id]
		dr, dok := denseRank[id]
		sRRF := rrf(sr, sok)
		dRRF := rrf(dr, dok)
		h := HybridHit{Mem: m, DenseScore: denseSim[id], SparseScore: sRRF}
		switch effective {
		case ModeFTS:
			h.HybridScore = sRRF
		case ModeDense:
			h.HybridScore = dRRF
		default:
			h.HybridScore = (1-alpha)*sRRF + alpha*dRRF
		}
		hits = append(hits, h)
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].HybridScore > hits[j].HybridScore })
	if len(hits) > opts.Limit {
		hits = hits[:opts.Limit]
	}

	// Access bookkeeping: Search already bumped everything it returned, so only
	// bump the dense-only survivors — double-counting would skew the GC's
	// PromoteHighAccess heuristic.
	if len(hits) > 0 {
		now := nowISO()
		for _, h := range hits {
			if _, bumped := sparseRank[h.ID]; bumped {
				continue
			}
			_, _ = db.Exec(`UPDATE semantic_memories SET access_count=access_count+1, accessed_at=? WHERE id=?`, now, h.ID)
		}
	}
	return hits, effective, nil
}

// denseCandidates cosine-ranks the profile's stored vectors for query. The bool
// reports whether the dense path was usable at all (embedder configured, query
// embeddable, at least one vector stored under the active model). It never bumps
// access bookkeeping — SearchHybrid does that once, for the final result set.
func denseCandidates(ctx context.Context, db *sql.DB, e Embedder, profile, query, memType string, limit int) ([]VectorHit, bool, error) {
	if e == nil {
		return nil, false, nil
	}
	qv, err := e.Embed(ctx, []string{query})
	if err != nil || len(qv) != 1 {
		return nil, false, nil // offline / bad key / quota: degrade, don't fail
	}
	q := `SELECT id,profile,content,memory_type,confidence,created_at,accessed_at,access_count,source,embedding
		FROM semantic_memories WHERE profile=? AND embedding IS NOT NULL AND embed_model=?`
	args := []any{profile, e.Model()}
	if memType != "" {
		q += ` AND memory_type=?`
		args = append(args, memType)
	}
	rows, err := db.Query(q, args...)
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
		vec, derr := DecodeVector(blob)
		if derr != nil {
			continue
		}
		m.Confidence = effectiveConfidence(m.Confidence, m.MemoryType, m.CreatedAt)
		scored = append(scored, VectorHit{Mem: m, Similarity: cosine(qv[0], vec)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(scored) == 0 {
		return nil, false, nil
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].Similarity > scored[j].Similarity })
	if limit > 0 && len(scored) > limit {
		scored = scored[:limit]
	}
	return scored, true, nil
}
