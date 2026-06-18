# PRD-072: Cross-Session Vector Store (`tag mem store`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** L (8-13 days)
**Category:** Memory
**Affects:** `semantic_memory.py + controller.py`
**Depends on:** PRD-065 (automatic post-run memory extraction), PRD-067 (hierarchical memory tiers), PRD-066 (hybrid memory search), PRD-043 (vector-based tool retrieval — embedding infrastructure)
**Inspired by:** LanceDB embedded vector store, Chroma persistent store, mem0 cross-session memory, Zep temporal knowledge graph

---

## 1. Overview

TAG's memory system (PRD-065, PRD-067) extracts and stores facts from agent runs, but the current vector storage implementation is session-ephemeral — vectors computed during a run are not persisted across process restarts, requiring re-embedding on every new session. This eliminates the core value proposition of a memory system: learning from past interactions and making that knowledge available in future sessions without re-processing.

Cross-Session Vector Store (`tag mem store`) introduces a persistent, embedded vector database backed by LanceDB (or a numpy-based flat-index fallback) that stores memory vectors durably in `~/.tag/memory/`. Vectors are written once at extraction time, indexed for fast approximate nearest-neighbor (ANN) search, and available immediately in subsequent sessions without re-embedding. The store supports CRUD operations, metadata filtering, index compaction, and migration utilities for upgrading the index format across TAG versions.

The design is inspired by LanceDB's embedded deployment model (persistent Arrow/Lance files, no server process), Chroma's persistent client (SQLite + HNSW index files in a directory), and mem0's cross-session storage (PostgreSQL + pgvector for persistent semantic memory). TAG's implementation prioritizes zero-server operation: all data lives in `~/.tag/memory/` as files on disk, compatible with the existing local-first design.

---

## 2. Problem Statement

### 2.1 Memory vectors lost on process exit

PRD-065 extracts facts from completed runs and stores them in `memory_items` (SQLite text column) with vector embeddings computed in-process. When the TAG process exits, in-process HNSW indices are lost. On the next session, all vectors must be re-embedded from scratch — a costly API call for large memory stores — or the in-process index is rebuilt from the raw embedding blobs in SQLite (slow for > 1000 items).

### 2.2 No ANN index for large memory stores

The current implementation stores embeddings as BLOB columns in SQLite and uses linear scan for similarity search. This is acceptable for < 1000 items but becomes prohibitively slow at 10,000+ items (PRD-066 identified < 500ms requirement). A dedicated vector store with an HNSW index provides sub-50ms ANN search at 100,000+ items.

### 2.3 No cross-session memory growth or forgetting

Without persistent vector storage, memories extracted in session A are not searchable in session B without explicit re-ingestion. A truly persistent memory system requires a durable vector index that grows incrementally with each session and supports controlled deletion (forgetting) for privacy or relevance management.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Persist memory vectors durably in `~/.tag/memory/` using LanceDB (primary) or a numpy flat-index fallback (no external deps). |
| G2 | Provide `tag mem store init`, `tag mem store add`, `tag mem store remove`, `tag mem store compact`, `tag mem store stats` subcommands. |
| G3 | Support incremental writes: add new memories without re-indexing existing ones (append-only index updates). |
| G4 | Support ANN search via HNSW index with < 50ms P95 latency for a 100,000-item store. |
| G5 | Support metadata filtering: filter by `profile`, `tier`, `created_at` range before ANN search. |
| G6 | Provide `tag mem store compact` to rebuild the index and reclaim space after bulk deletions. |
| G7 | `tag mem store migrate` upgrades index format across TAG versions (e.g., embedding dimension change). |
| G8 | Support a `--backend lancedb|numpy` flag; default to LanceDB if installed, numpy otherwise. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Remote or distributed vector stores (Pinecone, Weaviate, Qdrant). Local-only in this PRD. |
| NG2 | Real-time replication or synchronization across machines. |
| NG3 | GPU-accelerated ANN search. |
| NG4 | Exact nearest-neighbor search (only approximate). |
| NG5 | Built-in encryption of vector store files. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| ANN search latency (100k items) | < 50ms P95 for top-10 query with LanceDB backend | Benchmark test |
| ANN search latency (numpy fallback, 10k items) | < 500ms P95 | Benchmark test |
| Write throughput | 1000 vectors inserted in < 5s | Benchmark test |
| Cross-session persistence | Vectors written in session 1 are searchable in session 2 without re-embedding | Integration test |
| Index size overhead | LanceDB index for 100k × 1536-dim vectors < 1.2GB on disk | Disk measurement |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Developer | Have memories from past sessions automatically available in new sessions | I don't have to re-explain context every time |
| US2 | Developer | Run `tag mem store stats` to see how large my memory store is | I can manage disk usage |
| US3 | Developer | Run `tag mem store compact` to reclaim space after deleting old memories | I keep the store size manageable |
| US4 | Developer | Use `tag mem store remove --before 30d` to forget memories older than 30 days | I maintain memory relevance and privacy |
| US5 | Developer | Have the store work without installing LanceDB (numpy fallback) | I can use memory features on any Python install |

---

## 6. CLI Surface

```
tag mem store <subcommand> [options]

Subcommands:
  init       Initialize the vector store in ~/.tag/memory/
  add        Add a memory text (and embed it) to the store
  remove     Remove memories by ID, profile, or age filter
  compact    Rebuild and compact the index
  stats      Show store statistics (item count, index size, disk usage)
  migrate    Migrate index format to current version
  export     Export all vectors + metadata as JSONL
  import     Import vectors + metadata from JSONL

tag mem store init [--backend lancedb|numpy] [--dim 1536]

tag mem store add \
  --text "The API uses JWT bearer tokens" \
  --profile default \
  --tier recall \
  [--entity "auth-service"] \
  [--tags "auth,security"]

tag mem store remove \
  [--id MEMORY_ID] \
  [--profile PROFILE] \
  [--before DURATION]  # e.g. 30d, 7d

tag mem store compact [--dry-run]

tag mem store stats [--profile PROFILE]

tag mem store migrate [--from-version VERSION]

Options:
  --backend lancedb|numpy   Vector store backend (default: auto-detect)
  --dim INT                 Embedding dimension (default: 1536)
  --store-path PATH         Override default store path (~/.tag/memory/)
  --profile PROFILE         Scope operations to a specific profile
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `tag mem store init` creates `~/.tag/memory/` directory and initializes the chosen backend's index files; idempotent if already initialized. |
| FR-02 | On memory extraction (PRD-065), the extractor calls `VectorStore.add_batch()` to persist vectors durably before the TAG process exits. |
| FR-03 | `VectorStore.search(query_vec, k, filter)` returns top-k approximate nearest neighbors with metadata from the chosen backend in a unified `VectorMatch` list. |
| FR-04 | LanceDB backend: store vectors in a Lance table with columns `(id TEXT, vector FLOAT[1536], text TEXT, profile TEXT, tier TEXT, entity_tags TEXT, created_at TEXT)`; create a vector index (IVF-PQ or HNSW) after first 1000 items. |
| FR-05 | Numpy backend: store vectors as `(N, 1536)` float32 numpy array in `.npy` file, metadata in companion `.jsonl` file; rebuild cosine similarity via `numpy.dot`. |
| FR-06 | `tag mem store remove --before 30d` soft-deletes rows (marks `deleted_at`) then removes them on next `compact`. |
| FR-07 | `tag mem store compact` rebuilds the index from non-deleted rows, reclaims disk space, updates the version manifest. |
| FR-08 | `tag mem store stats` queries the index for item count, last write timestamp, index size on disk, memory tier distribution. |
| FR-09 | `tag mem store migrate` detects index version from a `MANIFEST.json` file, applies any registered migration functions to upgrade to the current version. |
| FR-10 | Auto-init: if `~/.tag/memory/` does not exist when any `tag mem` command runs, automatically initialize with the best available backend. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Store files must survive concurrent access from multiple TAG processes; use file-level locking (fcntl/portalocker) for write operations. |
| NFR-02 | Numpy backend must not require any packages beyond numpy (available in all Python installs). |
| NFR-03 | LanceDB backend is an optional extra: `pip install tag[memory]` or `pip install lancedb`; gracefully fall back to numpy if not installed. |
| NFR-04 | Store schema version tracked in `~/.tag/memory/MANIFEST.json`; migration required before use after version upgrade. |
| NFR-05 | Batch write of 10,000 vectors completes in < 30s on LanceDB backend. |

---

## 9. Technical Design

### 9.1 Target files

| File | Change |
|------|--------|
| `src/tag/semantic_memory.py` | Add `VectorStore`, `LanceDBBackend`, `NumpyBackend`, `VectorMatch` dataclass |
| `src/tag/controller.py` | Add `cmd_mem_store` entrypoint; register `mem store` subparser |

### 9.2 File layout on disk

```
~/.tag/memory/
├── MANIFEST.json          # {"version": 2, "backend": "lancedb", "dim": 1536, "created_at": "..."}
├── lancedb/               # LanceDB backend files (Lance format)
│   └── memories.lance/
├── numpy/                 # Numpy fallback files
│   ├── vectors.npy        # shape (N, 1536) float32
│   └── metadata.jsonl     # one JSON per line: {id, text, profile, tier, ...}
```

### 9.3 Python core

```python
from __future__ import annotations
import dataclasses
import json
import os
from pathlib import Path
from typing import List, Optional, Protocol
import numpy as np

@dataclasses.dataclass
class VectorMatch:
    id: str
    text: str
    profile: Optional[str]
    tier: Optional[str]
    score: float
    created_at: str

class VectorBackend(Protocol):
    def add_batch(self, items: List[dict]) -> None: ...
    def search(self, query_vec: np.ndarray, k: int, profile: Optional[str]) -> List[VectorMatch]: ...
    def remove(self, ids: List[str]) -> None: ...
    def count(self) -> int: ...
    def compact(self) -> None: ...

class NumpyBackend:
    def __init__(self, store_path: Path, dim: int = 1536) -> None:
        self.store_path = store_path
        self.dim = dim
        self._vecs_path = store_path / "numpy" / "vectors.npy"
        self._meta_path = store_path / "numpy" / "metadata.jsonl"
        (store_path / "numpy").mkdir(parents=True, exist_ok=True)

    def _load(self):
        if not self._vecs_path.exists():
            return np.empty((0, self.dim), dtype=np.float32), []
        vecs = np.load(str(self._vecs_path))
        meta = [json.loads(line) for line in self._meta_path.read_text().splitlines() if line.strip()]
        return vecs, meta

    def add_batch(self, items: List[dict]) -> None:
        vecs, meta = self._load()
        new_vecs = np.array([it["vector"] for it in items], dtype=np.float32)
        new_meta = [{k: v for k, v in it.items() if k != "vector"} for it in items]
        combined = np.vstack([vecs, new_vecs]) if len(vecs) else new_vecs
        np.save(str(self._vecs_path), combined)
        with self._meta_path.open("a") as f:
            for m in new_meta:
                f.write(json.dumps(m) + "\n")

    def search(self, query_vec: np.ndarray, k: int = 10,
               profile: Optional[str] = None) -> List[VectorMatch]:
        vecs, meta = self._load()
        if len(vecs) == 0:
            return []
        mask = np.ones(len(vecs), dtype=bool)
        if profile:
            mask = np.array([m.get("profile") == profile for m in meta])
        if not mask.any():
            return []
        filtered_vecs = vecs[mask]
        filtered_meta = [m for m, ok in zip(meta, mask) if ok]
        norms = np.linalg.norm(filtered_vecs, axis=1)
        norms[norms == 0] = 1.0
        qn = query_vec / (np.linalg.norm(query_vec) or 1.0)
        scores = (filtered_vecs / norms[:, None]) @ qn
        top_k = min(k, len(scores))
        top_idx = np.argsort(-scores)[:top_k]
        return [VectorMatch(
            id=filtered_meta[i].get("id", ""),
            text=filtered_meta[i].get("text", ""),
            profile=filtered_meta[i].get("profile"),
            tier=filtered_meta[i].get("tier"),
            score=float(scores[i]),
            created_at=filtered_meta[i].get("created_at", ""),
        ) for i in top_idx]

    def remove(self, ids: List[str]) -> None:
        vecs, meta = self._load()
        id_set = set(ids)
        keep = [i for i, m in enumerate(meta) if m.get("id") not in id_set]
        if len(keep) == len(meta):
            return
        np.save(str(self._vecs_path), vecs[keep])
        with self._meta_path.open("w") as f:
            for i in keep:
                f.write(json.dumps(meta[i]) + "\n")

    def count(self) -> int:
        _, meta = self._load()
        return len(meta)

    def compact(self) -> None:
        pass  # numpy backend is always compacted

class VectorStore:
    def __init__(self, store_path: Optional[str] = None, backend: str = "auto") -> None:
        self.store_path = Path(store_path or os.path.expanduser("~/.tag/memory/"))
        self.store_path.mkdir(parents=True, exist_ok=True)
        manifest_path = self.store_path / "MANIFEST.json"
        if not manifest_path.exists():
            manifest = {"version": 1, "backend": "numpy", "dim": 1536}
            manifest_path.write_text(json.dumps(manifest, indent=2))
        manifest = json.loads(manifest_path.read_text())
        chosen = backend if backend != "auto" else manifest.get("backend", "numpy")
        if chosen == "lancedb":
            try:
                from tag.semantic_memory_lancedb import LanceDBBackend
                self._backend: VectorBackend = LanceDBBackend(self.store_path)
            except ImportError:
                self._backend = NumpyBackend(self.store_path, manifest.get("dim", 1536))
        else:
            self._backend = NumpyBackend(self.store_path, manifest.get("dim", 1536))

    def add_batch(self, items: List[dict]) -> None:
        self._backend.add_batch(items)

    def search(self, query_vec: np.ndarray, k: int = 10,
               profile: Optional[str] = None) -> List[VectorMatch]:
        return self._backend.search(query_vec, k, profile)

    def remove(self, ids: Optional[List[str]] = None, profile: Optional[str] = None,
               before_days: Optional[int] = None) -> int:
        if ids:
            self._backend.remove(ids)
            return len(ids)
        return 0

    def compact(self) -> None:
        self._backend.compact()

    def stats(self) -> dict:
        count = self._backend.count()
        disk_mb = sum(f.stat().st_size for f in self.store_path.rglob("*") if f.is_file()) / 1e6
        return {"item_count": count, "disk_mb": round(disk_mb, 2)}
```

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Memory store readable by other users | `~/.tag/memory/` created with mode 0700; files written with mode 0600 |
| PII in persisted memory vectors | Memory extraction (PRD-065) applies secret scanning before writing; users can `remove --before 30d` to forget |
| Index file corruption on crash | Numpy backend uses atomic write (write to temp file, rename); LanceDB uses WAL internally |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | `NumpyBackend.add_batch`, `search`, `remove` correctness; `VectorStore` auto-backend selection |
| Integration | Write 1000 vectors in session 1, restart process, assert searchable in session 2 |
| Performance | 100k vector ANN benchmark for LanceDB; 10k for numpy |
| Migration | Add v1 manifest, run `migrate`, assert v2 manifest and index rebuilt |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | After `tag mem store init`, `~/.tag/memory/MANIFEST.json` exists with correct backend field |
| AC-02 | Vectors written in one TAG process are searchable by a subsequent TAG process without re-embedding |
| AC-03 | `tag mem store stats` shows accurate item count and disk usage |
| AC-04 | `tag mem store remove --before 30d` removes memories older than 30 days |
| AC-05 | `tag mem store compact` completes without errors and reduces disk usage after bulk removal |
| AC-06 | Numpy backend requires only numpy; no import errors on minimal Python install |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| PRD-065 memory extraction | Source of vectors to persist |
| PRD-043 embedding infrastructure | Embedding function for `mem store add` |
| lancedb (optional) | ANN index backend for large stores |
| numpy | Required for numpy backend and vector operations |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should the vector store be shared across profiles or per-profile? Current design is shared with profile metadata filtering. |
| OQ-02 | What is the maximum supported store size before recommending a paid backend? |
| OQ-03 | Should cross-session vectors include provenance (which run they came from) for auditability? |

---

## 15. Complexity & Timeline

**Complexity:** Large (L)
**Estimated effort:** 8–13 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | `NumpyBackend` implementation, unit tests, file layout | 2 |
| 2 | `VectorStore` abstraction, MANIFEST, auto-backend selection | 1 |
| 3 | LanceDB backend (optional extra), integration tests | 3 |
| 4 | CLI (`init`, `add`, `remove`, `compact`, `stats`, `migrate`) | 2 |
| 5 | Migration framework, performance benchmarks | 2 |
| 6 | Integration with PRD-065 memory extraction pipeline | 1 |
| 7 | Documentation, final integration tests | 2 |

