# PRD-066: Hybrid Memory Search (`tag mem search --mode hybrid`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (5-8 days)
**Category:** Memory
**Affects:** `semantic_memory.py + controller.py`
**Depends on:** PRD-065 (automatic post-run memory extraction), PRD-067 (hierarchical memory tiers), PRD-043 (vector-based tool retrieval — embedding infrastructure)
**Inspired by:** Weaviate hybrid search, Pinecone hybrid index, mem0 hybrid retrieval, BM25+HNSW fusion

---

## 1. Overview

TAG's memory system (PRD-065, PRD-067) stores extracted facts and session episodic memories as vector embeddings in a local store. Pure vector similarity search works well for semantic queries ("what did we decide about authentication?") but fails for keyword-exact queries ("what is the exact API key name?"), entity lookups ("show all memory about user Bob"), and boosting recently-created facts. Conversely, BM25 keyword search misses semantically equivalent phrasings and suffers on short or noisy memory snippets.

Hybrid Memory Search introduces `tag mem search --mode hybrid`, which fuses vector similarity scores (HNSW cosine distance) with BM25 sparse retrieval scores using Reciprocal Rank Fusion (RRF). The result is retrieved in a single pass, ranked by a combined score, and optionally boosted by entity salience and recency. This directly mirrors the architecture of production retrieval systems like Weaviate's hybrid API (alpha parameter for fusion weight), Pinecone's hybrid index (dense + sparse), and mem0's retrieval layer (vector + keyword fallback).

The implementation uses TAG's existing embedding infrastructure (PRD-043) for the dense path and a pure-Python BM25 implementation (no external search server required) for the sparse path. Results are fused via RRF and optionally post-filtered by entity, date range, or memory tier.

---

## 2. Problem Statement

### 2.1 Pure vector search fails for exact-match queries

When an engineer asks "what is the database connection string we stored last week?", pure cosine similarity over embeddings may not surface the exact string if it was paraphrased at extraction time. BM25 would find it immediately by keyword overlap.

### 2.2 BM25 alone misses semantic relationships

Searching for "authentication flow" with BM25 will not find memories tagged "login sequence" or "identity verification" unless those exact words appear. Semantic search bridges this gap.

### 2.3 No entity-boosted retrieval

Memories about a specific entity (a user, a project, a file) should be preferentially surfaced when the query mentions that entity. Current vector-only search has no entity salience boost.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | `tag mem search QUERY --mode hybrid` executes parallel dense (vector) and sparse (BM25) retrieval, fuses results via RRF, and returns a ranked list. |
| G2 | Support `--alpha FLOAT` (0=pure BM25, 1=pure vector, 0.5=balanced) to tune fusion weight. |
| G3 | Support entity boosting: `--entity NAME` surfaces memories mentioning the named entity first. |
| G4 | Support recency weighting: `--recency-weight FLOAT` applies a time-decay factor to older memories. |
| G5 | Return results with both individual scores (dense_score, sparse_score, hybrid_score) for transparency. |
| G6 | `--mode dense` and `--mode bm25` continue to work as before for pure-path retrieval. |
| G7 | Hybrid search must complete in < 500ms for a memory store of 10,000 items on commodity hardware. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | External search server (Elasticsearch, OpenSearch). All computation is local in-process. |
| NG2 | Multi-field BM25 (separate fields for entity, content, tags). Single-field BM25 over the full memory text. |
| NG3 | Neural sparse models (SPLADE, uniCOIL). Plain BM25 only. |
| NG4 | Cross-session query expansion or query rewriting. |
| NG5 | Real-time index updates during search. Index is rebuilt on demand or on a configurable interval. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Recall@10 on hybrid vs pure vector | Hybrid achieves ≥ 5% higher Recall@10 on a 100-query benchmark with mixed keyword/semantic queries | Eval benchmark |
| Search latency (10k memories) | Hybrid search completes in < 500ms P95 | Benchmark test |
| Exact-match recovery | BM25 path recovers exact-match queries that vector search ranks outside top-10 in 90%+ of test cases | Unit eval |
| Entity boost accuracy | `--entity NAME` pushes entity-matched memories to top-3 in 95%+ of test cases | Unit test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Developer | Search memory for "connection string" and get both exact matches and semantic variants | I find what I stored regardless of how it was phrased |
| US2 | Power user | Tune `--alpha 0.2` to bias toward keyword matching | I get precise results for technical queries |
| US3 | Developer | Search with `--entity "auth-service"` to boost entity-relevant memories | I get context-specific results when working on a component |
| US4 | Developer | See the `hybrid_score`, `dense_score`, and `sparse_score` in verbose output | I understand why a memory ranked where it did |

---

## 6. CLI Surface

```
tag mem search QUERY [options]

Options:
  --mode dense|bm25|hybrid        Retrieval mode (default: hybrid)
  --alpha FLOAT                   Fusion weight: 0=pure BM25, 1=pure vector (default: 0.5)
  --limit N                       Max results to return (default: 10)
  --entity NAME                   Boost memories mentioning this entity
  --recency-weight FLOAT          Time-decay factor (0=no decay, 1=strong recency bias; default: 0.1)
  --tier core|recall|archival|all Memory tier filter (default: all)
  --profile PROFILE               Scope to a specific profile
  --since DURATION                Only search memories created after this time
  --verbose                       Show individual dense/sparse/hybrid scores
  --json                          Output as JSON array

Examples:
  tag mem search "authentication flow"
  tag mem search "database password" --mode bm25
  tag mem search "auth" --mode hybrid --alpha 0.3 --entity "auth-service" --verbose
  tag mem search "project goals" --tier core --limit 5
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | Load or build the BM25 index lazily on first `--mode bm25` or `--mode hybrid` search; cache the index in memory for the process lifetime. |
| FR-02 | Dense path: encode query with the existing PRD-043 embedding model; compute cosine similarities against all stored memory vectors. |
| FR-03 | Sparse path: tokenize query and all memory texts; compute BM25 scores (TF-IDF with length normalization, k1=1.5, b=0.75). |
| FR-04 | RRF fusion: `hybrid_score = (1-alpha) * (1/(k+bm25_rank)) + alpha * (1/(k+dense_rank))` where k=60. |
| FR-05 | Entity boost: if `--entity` specified, add `entity_bonus` to hybrid_score for memories whose text contains the entity name (case-insensitive). |
| FR-06 | Recency decay: multiply hybrid_score by `exp(-recency_weight * age_days)` where `age_days` is memory creation age in days. |
| FR-07 | Return results as a list of `MemorySearchResult` dataclass instances with: `id`, `text`, `tier`, `dense_score`, `sparse_score`, `hybrid_score`, `created_at`. |
| FR-08 | `--mode dense` and `--mode bm25` call only the respective single path; no fusion. |
| FR-09 | BM25 index is invalidated and rebuilt when new memories are added (detected by comparing row count against cached index size). |
| FR-10 | `--verbose` renders a table with all score fields; default output shows only text and hybrid_score. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | BM25 index build time < 2s for 10,000 memory items. |
| NFR-02 | Dense vector similarity (cosine over 10k × 1536-dim vectors) < 300ms using numpy vectorized operations. |
| NFR-03 | Total peak memory < 500MB for 10,000 memories with 1536-dim float32 vectors. |
| NFR-04 | No external dependencies beyond numpy and the existing embedding model; BM25 implemented in pure Python. |
| NFR-05 | Index persisted to disk as a pickle file alongside the vector store for fast restart. |

---

## 9. Technical Design

### 9.1 Target files

| File | Change |
|------|--------|
| `src/tag/semantic_memory.py` | Add `HybridMemorySearch` class, `BM25Index` class, `MemorySearchResult` dataclass |
| `src/tag/controller.py` | Update `cmd_mem_search` to accept `--mode`, `--alpha`, `--entity`, `--recency-weight` |

### 9.2 SQLite DDL (no new tables — uses existing memory tables)

```sql
-- Existing tables used:
-- memory_items(id, tier, text, embedding BLOB, entity_tags TEXT, created_at TEXT, profile TEXT)
-- BM25 index is built in-memory from memory_items rows; not stored in SQLite
```

### 9.3 Python core

```python
from __future__ import annotations
import dataclasses
import math
import pickle
import sqlite3
from collections import defaultdict
from pathlib import Path
from typing import List, Optional
import numpy as np

@dataclasses.dataclass
class MemorySearchResult:
    id: str
    text: str
    tier: str
    dense_score: float
    sparse_score: float
    hybrid_score: float
    created_at: str

class BM25Index:
    """Pure-Python BM25 with k1=1.5, b=0.75."""
    def __init__(self, docs: List[str], k1: float = 1.5, b: float = 0.75) -> None:
        self.k1, self.b = k1, b
        self.doc_count = len(docs)
        self.avgdl = sum(len(d.split()) for d in docs) / max(1, len(docs))
        self.idf: dict = {}
        self.tf_per_doc: list = []
        df: dict = defaultdict(int)
        for doc in docs:
            tokens = set(doc.lower().split())
            for t in tokens:
                df[t] += 1
        for term, freq in df.items():
            self.idf[term] = math.log((self.doc_count - freq + 0.5) / (freq + 0.5) + 1)
        for doc in docs:
            tokens = doc.lower().split()
            tf: dict = defaultdict(int)
            for t in tokens:
                tf[t] += 1
            self.tf_per_doc.append((dict(tf), len(tokens)))

    def score(self, query: str) -> np.ndarray:
        qtokens = query.lower().split()
        scores = np.zeros(self.doc_count, dtype=np.float32)
        for qi, (tf_dict, dl) in enumerate(self.tf_per_doc):
            s = 0.0
            for term in qtokens:
                if term in tf_dict:
                    tf = tf_dict[term]
                    idf = self.idf.get(term, 0.0)
                    num = tf * (self.k1 + 1)
                    den = tf + self.k1 * (1 - self.b + self.b * dl / self.avgdl)
                    s += idf * num / den
            scores[qi] = s
        return scores

class HybridMemorySearch:
    RRF_K = 60

    def __init__(self, db_path: str, embed_fn, index_cache_path: Optional[str] = None) -> None:
        self.db_path = db_path
        self.embed_fn = embed_fn
        self.cache_path = index_cache_path
        self._bm25: Optional[BM25Index] = None
        self._row_count = 0
        self._ids: List[str] = []
        self._texts: List[str] = []
        self._vectors: Optional[np.ndarray] = None
        self._created_ats: List[str] = []

    def _load_memories(self, profile: Optional[str], tier: Optional[str]) -> None:
        conn = sqlite3.connect(self.db_path)
        conn.row_factory = sqlite3.Row
        where = []
        params = []
        if profile:
            where.append("profile=?"); params.append(profile)
        if tier and tier != "all":
            where.append("tier=?"); params.append(tier)
        clause = "WHERE " + " AND ".join(where) if where else ""
        rows = conn.execute(
            f"SELECT id, text, tier, embedding, created_at FROM memory_items {clause}", params
        ).fetchall()
        conn.close()
        self._ids = [r["id"] for r in rows]
        self._texts = [r["text"] for r in rows]
        self._created_ats = [r["created_at"] for r in rows]
        vecs = []
        for r in rows:
            if r["embedding"]:
                vecs.append(np.frombuffer(r["embedding"], dtype=np.float32))
            else:
                vecs.append(np.zeros(1536, dtype=np.float32))
        self._vectors = np.stack(vecs) if vecs else np.empty((0, 1536), dtype=np.float32)
        self._row_count = len(rows)
        self._bm25 = BM25Index(self._texts)

    def search(self, query: str, mode: str = "hybrid", alpha: float = 0.5,
               limit: int = 10, entity: Optional[str] = None, recency_weight: float = 0.1,
               profile: Optional[str] = None, tier: Optional[str] = None
               ) -> List[MemorySearchResult]:
        self._load_memories(profile, tier)
        n = self._row_count
        if n == 0:
            return []

        dense_scores = np.zeros(n, dtype=np.float32)
        sparse_scores = np.zeros(n, dtype=np.float32)

        if mode in ("dense", "hybrid") and self._vectors is not None:
            qvec = np.array(self.embed_fn(query), dtype=np.float32)
            norms = np.linalg.norm(self._vectors, axis=1, keepdims=True)
            norms[norms == 0] = 1.0
            vecs_n = self._vectors / norms
            qvec_n = qvec / (np.linalg.norm(qvec) or 1.0)
            dense_scores = vecs_n @ qvec_n  # shape (n,)

        if mode in ("bm25", "hybrid") and self._bm25:
            sparse_scores = self._bm25.score(query)

        if mode == "dense":
            hybrid_scores = dense_scores.copy()
        elif mode == "bm25":
            hybrid_scores = sparse_scores.copy()
        else:
            # RRF fusion
            d_ranks = np.argsort(-dense_scores)
            s_ranks = np.argsort(-sparse_scores)
            d_rrf = np.zeros(n); s_rrf = np.zeros(n)
            for rank, idx in enumerate(d_ranks):
                d_rrf[idx] = 1.0 / (self.RRF_K + rank + 1)
            for rank, idx in enumerate(s_ranks):
                s_rrf[idx] = 1.0 / (self.RRF_K + rank + 1)
            hybrid_scores = (1 - alpha) * s_rrf + alpha * d_rrf

        # Entity boost
        if entity:
            entity_l = entity.lower()
            for i, text in enumerate(self._texts):
                if entity_l in text.lower():
                    hybrid_scores[i] += 0.1

        # Recency decay
        if recency_weight > 0:
            from datetime import datetime, timezone
            now = datetime.now(timezone.utc)
            for i, ca in enumerate(self._created_ats):
                try:
                    age_days = (now - datetime.fromisoformat(ca)).days
                    hybrid_scores[i] *= math.exp(-recency_weight * age_days / 30)
                except Exception:
                    pass

        top_indices = np.argsort(-hybrid_scores)[:limit]
        results = []
        for idx in top_indices:
            results.append(MemorySearchResult(
                id=self._ids[idx],
                text=self._texts[idx],
                tier="",  # fill from rows if needed
                dense_score=float(dense_scores[idx]),
                sparse_score=float(sparse_scores[idx]),
                hybrid_score=float(hybrid_scores[idx]),
                created_at=self._created_ats[idx],
            ))
        return results
```

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Query injection into BM25 tokenization | Tokenization is pure string splitting; no SQL involved |
| Memory content leakage via search results | Results inherit profile scope; cross-profile search requires explicit `--profile all` |
| Large embedding vectors consuming excessive memory | Cap maximum loaded memories at 50,000; warn and paginate beyond |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | `BM25Index.score` correctness on 3-document corpus; RRF fusion formula; entity boost; recency decay |
| Integration | End-to-end: seed 100 memories, run hybrid search, verify recall improvement over pure-vector baseline |
| Performance | 10,000-memory benchmark: assert < 500ms P95 |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | `tag mem search "exact string" --mode bm25` returns the exact memory as rank-1 when it exists |
| AC-02 | `tag mem search "semantic query" --mode hybrid --verbose` shows `dense_score`, `sparse_score`, `hybrid_score` columns |
| AC-03 | `--alpha 0` returns results identical to `--mode bm25` |
| AC-04 | `--alpha 1` returns results identical to `--mode dense` |
| AC-05 | `--entity "auth-service"` promotes entity-matching memories to top of results |
| AC-06 | Search over 10,000 memories completes in < 500ms |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| PRD-043 vector tool retrieval | Embedding function and vector store infrastructure |
| PRD-065 memory extraction | Source of memory items in SQLite |
| numpy | Dense vector operations |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should the BM25 index be persisted to disk between runs to avoid rebuild latency? |
| OQ-02 | Should alpha be per-query or a configurable profile default? |
| OQ-03 | Should entity boosting be based on the entity_tags column (structured) rather than text substring match? |

---

## 15. Complexity & Timeline

**Complexity:** Medium (M)
**Estimated effort:** 5–8 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | `BM25Index` implementation, unit tests with correctness assertions | 1 |
| 2 | Dense retrieval path, RRF fusion, `MemorySearchResult` dataclass | 2 |
| 3 | Entity boost, recency decay, result rendering | 1 |
| 4 | CLI integration (`--mode`, `--alpha`, `--entity`, `--recency-weight`) | 1 |
| 5 | Performance benchmarks, integration tests | 1 |

