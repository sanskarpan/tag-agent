"""PRD-025: Semantic Memory with Confidence Decay.

Stores agent memories in SQLite with FTS5 for full-text search. Confidence
decays exponentially with age using type-specific half-lives:
  - convention: ∞ (never decays)
  - decision: 180 days
  - gotcha: 90 days
  - fact: 90 days
  - other: 60 days

Score = confidence_base * 2^(-age_days / half_life)
"""
from __future__ import annotations

import math
import sqlite3
import uuid
from datetime import datetime, timezone

HALF_LIVES: dict[str, float | None] = {
    "convention": None,   # never decays
    "decision": 180.0,
    "gotcha": 90.0,
    "fact": 90.0,
    "other": 60.0,
}

VALID_TYPES = set(HALF_LIVES.keys())


def _utc_now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _age_days(created_at: str) -> float:
    try:
        ts = datetime.fromisoformat(created_at)
        if ts.tzinfo is None:
            ts = ts.replace(tzinfo=timezone.utc)
        delta = datetime.now(timezone.utc) - ts
        return delta.total_seconds() / 86400
    except Exception:
        return 0.0


def compute_confidence(base: float, memory_type: str, created_at: str) -> float:
    """Return effective confidence accounting for age decay."""
    half_life = HALF_LIVES.get(memory_type, 60.0)
    if half_life is None:
        return base
    age = _age_days(created_at)
    return base * (2.0 ** (-age / half_life))


def ensure_schema(conn: sqlite3.Connection) -> None:
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS semantic_memories (
          id           TEXT PRIMARY KEY,
          profile      TEXT NOT NULL,
          content      TEXT NOT NULL,
          memory_type  TEXT NOT NULL DEFAULT 'fact',
          confidence   REAL NOT NULL DEFAULT 1.0,
          created_at   TEXT NOT NULL,
          accessed_at  TEXT NOT NULL,
          access_count INTEGER NOT NULL DEFAULT 0,
          source       TEXT NOT NULL DEFAULT 'manual'
        );
        CREATE INDEX IF NOT EXISTS idx_sm_profile ON semantic_memories(profile, memory_type);
        CREATE INDEX IF NOT EXISTS idx_sm_conf ON semantic_memories(confidence DESC);

        CREATE VIRTUAL TABLE IF NOT EXISTS semantic_memories_fts
          USING fts5(id, profile, content, memory_type, tokenize='porter unicode61');
    """)
    conn.commit()


def add_memory(
    conn: sqlite3.Connection,
    profile: str,
    content: str,
    *,
    memory_type: str = "fact",
    confidence: float = 1.0,
    source: str = "manual",
) -> str:
    """Insert a new memory. Returns the new memory id."""
    ensure_schema(conn)
    content = content.strip()
    if not content:
        raise ValueError("Memory content must not be empty")
    if memory_type not in VALID_TYPES:
        raise ValueError(f"memory_type must be one of {sorted(VALID_TYPES)}, got {memory_type!r}")
    if not (0.0 < confidence <= 1.0):
        raise ValueError(f"confidence must be in (0, 1], got {confidence}")

    mem_id = uuid.uuid4().hex[:16]
    now = _utc_now()
    conn.execute(
        """INSERT INTO semantic_memories(id, profile, content, memory_type, confidence,
           created_at, accessed_at, access_count, source)
           VALUES(?,?,?,?,?,?,?,0,?)""",
        (mem_id, profile, content, memory_type, confidence, now, now, source),
    )
    # Keep FTS in sync
    conn.execute(
        "INSERT INTO semantic_memories_fts(id, profile, content, memory_type) VALUES(?,?,?,?)",
        (mem_id, profile, content, memory_type),
    )
    conn.commit()
    return mem_id


def search_memories(
    conn: sqlite3.Connection,
    profile: str,
    query: str,
    *,
    limit: int = 10,
    min_confidence: float = 0.0,
    memory_type: str | None = None,
) -> list[dict]:
    """Full-text search over memories, sorted by effective confidence."""
    ensure_schema(conn)
    # FTS query to get candidate IDs
    try:
        fts_rows = conn.execute(
            "SELECT id FROM semantic_memories_fts WHERE content MATCH ? AND profile=? LIMIT 50",
            (query, profile),
        ).fetchall()
        candidate_ids = {r[0] for r in fts_rows}
    except Exception:
        # FTS5 not available or query error — fallback to LIKE
        candidate_ids = None

    if candidate_ids is not None:
        if not candidate_ids:
            return []
        placeholders = ",".join("?" * len(candidate_ids))
        base_where = f"id IN ({placeholders})"
        base_params: list = list(candidate_ids)
    else:
        base_where = "content LIKE ?"
        base_params = [f"%{query}%"]

    type_clause = " AND memory_type=?" if memory_type else ""
    type_params: list = [memory_type] if memory_type else []

    rows = conn.execute(
        f"""SELECT id, profile, content, memory_type, confidence, created_at,
               accessed_at, access_count, source
            FROM semantic_memories
            WHERE profile=? AND {base_where}{type_clause}""",
        [profile] + base_params + type_params,
    ).fetchall()

    results = []
    for r in rows:
        mem_id, prof, content, mtype, conf_base, created, accessed, count, src = r
        effective = compute_confidence(conf_base, mtype, created)
        if effective < min_confidence:
            continue
        results.append({
            "id": mem_id,
            "profile": prof,
            "content": content,
            "memory_type": mtype,
            "confidence_base": conf_base,
            "confidence": round(effective, 4),
            "created_at": created,
            "accessed_at": accessed,
            "access_count": count,
            "source": src,
        })

    results.sort(key=lambda x: -x["confidence"])
    selected = results[:limit]

    # Update access timestamps
    if selected:
        now = _utc_now()
        for mem in selected:
            conn.execute(
                "UPDATE semantic_memories SET accessed_at=?, access_count=access_count+1 WHERE id=?",
                (now, mem["id"]),
            )
        conn.commit()

    return selected


def list_memories(
    conn: sqlite3.Connection,
    profile: str,
    *,
    memory_type: str | None = None,
    limit: int = 50,
) -> list[dict]:
    """List memories sorted by effective confidence (descending)."""
    ensure_schema(conn)
    type_clause = " AND memory_type=?" if memory_type else ""
    type_params: list = [memory_type] if memory_type else []
    rows = conn.execute(
        f"""SELECT id, content, memory_type, confidence, created_at, accessed_at,
               access_count, source
            FROM semantic_memories
            WHERE profile=?{type_clause}
            ORDER BY confidence DESC, created_at DESC
            LIMIT ?""",
        [profile] + type_params + [limit * 3],  # fetch more for re-sort
    ).fetchall()

    results = []
    for r in rows:
        mem_id, content, mtype, conf_base, created, accessed, count, src = r
        effective = compute_confidence(conf_base, mtype, created)
        results.append({
            "id": mem_id,
            "content": content,
            "memory_type": mtype,
            "confidence_base": conf_base,
            "confidence": round(effective, 4),
            "created_at": created,
            "accessed_at": accessed,
            "access_count": count,
            "source": src,
        })

    results.sort(key=lambda x: -x["confidence"])
    return results[:limit]


def forget_memory(conn: sqlite3.Connection, mem_id: str, profile: str) -> bool:
    """Delete a memory by id. Returns True if deleted."""
    ensure_schema(conn)
    cur = conn.execute(
        "DELETE FROM semantic_memories WHERE id=? AND profile=?", (mem_id, profile)
    )
    conn.execute("DELETE FROM semantic_memories_fts WHERE id=?", (mem_id,))
    conn.commit()
    return cur.rowcount > 0


def memory_stats(conn: sqlite3.Connection, profile: str) -> dict:
    """Return aggregate statistics for a profile's memory store."""
    ensure_schema(conn)
    rows = conn.execute(
        """SELECT memory_type, COUNT(*), AVG(confidence)
           FROM semantic_memories WHERE profile=?
           GROUP BY memory_type""",
        (profile,),
    ).fetchall()
    by_type = {r[0]: {"count": r[1], "avg_confidence_base": round(r[2] or 0, 4)} for r in rows}
    total = conn.execute(
        "SELECT COUNT(*) FROM semantic_memories WHERE profile=?", (profile,)
    ).fetchone()[0]
    return {"profile": profile, "total": total, "by_type": by_type}


# ---------------------------------------------------------------------------
# PRD-066: Hybrid memory search (BM25 + FTS5 + optional vector)
# ---------------------------------------------------------------------------

import collections as _collections
import json as _json
import re as _re


def _bm25_score(
    query_terms: list[str],
    doc_terms: list[str],
    corpus_size: int,
    avg_doc_len: float,
    k1: float = 1.5,
    b: float = 0.75,
) -> float:
    """Compute BM25 score for a single document.

    Uses the standard Okapi BM25 formula:
        score = Σ IDF(t) * (tf * (k1+1)) / (tf + k1*(1 - b + b*|D|/avgdl))
    where IDF is approximated with corpus_size because we do not have per-term
    document-frequency counts at call time (caller must supply reasonable
    corpus_size; set to total memory count).
    """
    if not query_terms or not doc_terms:
        return 0.0

    doc_len = len(doc_terms)
    tf_map = _collections.Counter(doc_terms)
    score = 0.0
    # approximate df = 1 (worst case) so IDF = log((N-1+0.5)/0.5) ≈ log(2N)
    # this preserves relative ranking even without true df counts
    for term in set(query_terms):
        tf = tf_map.get(term, 0)
        if tf == 0:
            continue
        idf = math.log((corpus_size - 1 + 0.5) / 0.5 + 1)
        norm_tf = (tf * (k1 + 1)) / (tf + k1 * (1 - b + b * doc_len / max(avg_doc_len, 1.0)))
        score += idf * norm_tf
    return score


def _tokenize(text: str) -> list[str]:
    """Simple whitespace + punctuation tokenizer for BM25."""
    return _re.findall(r"[a-z0-9]+", text.lower())


def search_memories_hybrid(
    conn: sqlite3.Connection,
    profile: str,
    query: str,
    *,
    limit: int = 10,
    min_confidence: float = 0.0,
    memory_type: str | None = None,
    mode: str = "hybrid",
) -> list[dict]:
    """Hybrid search combining FTS5 full-text + confidence scoring + optional BM25 reranking.

    mode:
        'fts'    — FTS5 ranking only (fast, uses SQLite porter-stemmer)
        'bm25'   — BM25 re-rank of FTS5 candidates
        'hybrid' — FTS5 rank + BM25 rank fused via Reciprocal Rank Fusion (RRF, k=60)

    Reciprocal Rank Fusion:
        score = Σ 1/(k + rank_i)  for each ranking list i,  k = 60
    """
    ensure_schema(conn)

    # --- Candidate retrieval via FTS5 (or LIKE fallback) ---
    try:
        fts_rows = conn.execute(
            """SELECT id, rank FROM semantic_memories_fts
               WHERE content MATCH ? AND profile=?
               LIMIT 200""",
            (query, profile),
        ).fetchall()
        # rank in FTS5 is negative (more negative = better match)
        fts_id_rank: dict[str, int] = {r[0]: i for i, r in enumerate(fts_rows)}
    except Exception:
        fts_id_rank = {}

    if not fts_id_rank and mode != "bm25":
        # Fallback: grab all profile memories and do LIKE filter
        fallback_rows = conn.execute(
            "SELECT id FROM semantic_memories WHERE profile=? AND content LIKE ?",
            (profile, f"%{query}%"),
        ).fetchall()
        fts_id_rank = {r[0]: i for i, r in enumerate(fallback_rows)}

    candidate_ids = set(fts_id_rank.keys())
    if not candidate_ids and mode in ("fts", "hybrid"):
        return []

    # --- Fetch full rows for candidates (or all for pure bm25 fallback) ---
    if candidate_ids:
        placeholders = ",".join("?" * len(candidate_ids))
        type_clause = " AND memory_type=?" if memory_type else ""
        type_params: list = [memory_type] if memory_type else []
        rows = conn.execute(
            f"""SELECT id, profile, content, memory_type, confidence, created_at,
                       accessed_at, access_count, source
                FROM semantic_memories
                WHERE profile=? AND id IN ({placeholders}){type_clause}""",
            [profile] + list(candidate_ids) + type_params,
        ).fetchall()
    else:
        # pure BM25 with no FTS index — scan all profile memories
        type_clause = " AND memory_type=?" if memory_type else ""
        type_params = [memory_type] if memory_type else []
        rows = conn.execute(
            f"""SELECT id, profile, content, memory_type, confidence, created_at,
                       accessed_at, access_count, source
                FROM semantic_memories
                WHERE profile=?{type_clause}""",
            [profile] + type_params,
        ).fetchall()

    if not rows:
        return []

    # --- Build memory dicts with effective confidence ---
    memories: list[dict] = []
    for r in rows:
        mem_id, prof, content, mtype, conf_base, created, accessed, count, src = r
        effective = compute_confidence(conf_base, mtype, created)
        if effective < min_confidence:
            continue
        memories.append({
            "id": mem_id,
            "profile": prof,
            "content": content,
            "memory_type": mtype,
            "confidence_base": conf_base,
            "confidence": round(effective, 4),
            "created_at": created,
            "accessed_at": accessed,
            "access_count": count,
            "source": src,
        })

    if not memories:
        return []

    # --- BM25 scoring ---
    query_terms = _tokenize(query)
    corpus_size = len(memories)
    all_doc_terms = [_tokenize(m["content"]) for m in memories]
    avg_doc_len = sum(len(t) for t in all_doc_terms) / max(corpus_size, 1)

    bm25_scores: dict[str, float] = {}
    for mem, doc_terms in zip(memories, all_doc_terms):
        bm25_scores[mem["id"]] = _bm25_score(query_terms, doc_terms, corpus_size, avg_doc_len)

    bm25_ranked: list[str] = sorted(bm25_scores, key=lambda k: -bm25_scores[k])
    bm25_id_rank: dict[str, int] = {mid: i for i, mid in enumerate(bm25_ranked)}

    # --- Rank fusion / mode selection ---
    RRF_K = 60

    def _rrf(mem: dict) -> float:
        mid = mem["id"]
        fts_r = fts_id_rank.get(mid, len(memories))
        bm25_r = bm25_id_rank.get(mid, len(memories))
        if mode == "fts":
            return 1.0 / (RRF_K + fts_r)
        elif mode == "bm25":
            return 1.0 / (RRF_K + bm25_r)
        else:  # hybrid
            return 1.0 / (RRF_K + fts_r) + 1.0 / (RRF_K + bm25_r)

    memories.sort(key=_rrf, reverse=True)
    selected = memories[:limit]

    # Update access timestamps
    if selected:
        now = _utc_now()
        for mem in selected:
            conn.execute(
                "UPDATE semantic_memories SET accessed_at=?, access_count=access_count+1 WHERE id=?",
                (now, mem["id"]),
            )
        conn.commit()

    return selected


# ---------------------------------------------------------------------------
# PRD-067: Hierarchical memory tiers
# ---------------------------------------------------------------------------

MEMORY_TIERS: dict[str, dict] = {
    "core": {"max_age_days": None, "min_confidence": 0.8, "max_count": 50},
    "recall": {"max_age_days": 90, "min_confidence": 0.4, "max_count": 200},
    "archival": {"max_age_days": None, "min_confidence": 0.0, "max_count": None},
}


def ensure_tier_schema(conn: sqlite3.Connection) -> None:
    """Add tier column to semantic_memories if not present."""
    cols = {r[1] for r in conn.execute("PRAGMA table_info(semantic_memories)").fetchall()}
    if "tier" not in cols:
        conn.execute("ALTER TABLE semantic_memories ADD COLUMN tier TEXT NOT NULL DEFAULT 'archival'")
        conn.commit()


def get_memory_tier(memory: dict) -> str:
    """Classify a memory into core/recall/archival based on confidence and age.

    Classification order (highest-priority first):
        core     — effective confidence >= 0.8
        recall   — effective confidence >= 0.4  AND  age <= 90 days
        archival — everything else
    """
    conf = memory.get("confidence", 0.0)
    age = _age_days(memory.get("created_at", _utc_now()))

    core_cfg = MEMORY_TIERS["core"]
    if conf >= core_cfg["min_confidence"]:
        return "core"

    recall_cfg = MEMORY_TIERS["recall"]
    if conf >= recall_cfg["min_confidence"] and age <= (recall_cfg["max_age_days"] or float("inf")):
        return "recall"

    return "archival"


def list_memories_by_tier(
    conn: sqlite3.Connection,
    profile: str,
    tier: str,
    *,
    limit: int = 50,
) -> list[dict]:
    """Return memories in a specific tier, sorted by effective confidence.

    Uses the stored tier column if present (see ensure_tier_schema); otherwise
    falls back to classifying every memory on-the-fly.
    """
    ensure_schema(conn)
    if tier not in MEMORY_TIERS:
        raise ValueError(f"tier must be one of {sorted(MEMORY_TIERS)}, got {tier!r}")

    # Try fast path via stored tier column
    cols = {r[1] for r in conn.execute("PRAGMA table_info(semantic_memories)").fetchall()}
    if "tier" in cols:
        rows = conn.execute(
            """SELECT id, content, memory_type, confidence, created_at,
                      accessed_at, access_count, source
               FROM semantic_memories
               WHERE profile=? AND tier=?
               ORDER BY confidence DESC""",
            (profile, tier),
        ).fetchall()
    else:
        rows = conn.execute(
            """SELECT id, content, memory_type, confidence, created_at,
                      accessed_at, access_count, source
               FROM semantic_memories
               WHERE profile=?
               ORDER BY confidence DESC""",
            (profile,),
        ).fetchall()

    results = []
    for r in rows:
        mem_id, content, mtype, conf_base, created, accessed, count, src = r
        effective = compute_confidence(conf_base, mtype, created)
        mem = {
            "id": mem_id,
            "content": content,
            "memory_type": mtype,
            "confidence_base": conf_base,
            "confidence": round(effective, 4),
            "created_at": created,
            "accessed_at": accessed,
            "access_count": count,
            "source": src,
        }
        # When using fallback (no tier column), filter by computed tier
        if "tier" not in cols and get_memory_tier(mem) != tier:
            continue
        results.append(mem)

    results.sort(key=lambda x: -x["confidence"])
    return results[:limit]


def page_down_tier(
    conn: sqlite3.Connection,
    profile: str,
    from_tier: str = "recall",
    to_tier: str = "archival",
    *,
    batch_size: int = 20,
) -> int:
    """Move memories that no longer meet from_tier criteria down to to_tier.

    Evaluates each memory in from_tier against the tier classification rules and
    writes the new tier value for any that have fallen below the threshold.
    Returns count of memories moved.
    """
    ensure_schema(conn)
    ensure_tier_schema(conn)

    if from_tier not in MEMORY_TIERS or to_tier not in MEMORY_TIERS:
        raise ValueError(f"tier must be one of {sorted(MEMORY_TIERS)}")

    rows = conn.execute(
        """SELECT id, content, memory_type, confidence, created_at,
                  accessed_at, access_count, source
           FROM semantic_memories
           WHERE profile=? AND tier=?
           LIMIT ?""",
        (profile, from_tier, batch_size),
    ).fetchall()

    moved = 0
    for r in rows:
        mem_id, content, mtype, conf_base, created, accessed, count, src = r
        effective = compute_confidence(conf_base, mtype, created)
        mem = {
            "id": mem_id,
            "content": content,
            "memory_type": mtype,
            "confidence_base": conf_base,
            "confidence": round(effective, 4),
            "created_at": created,
            "accessed_at": accessed,
            "access_count": count,
            "source": src,
        }
        computed_tier = get_memory_tier(mem)
        if computed_tier != from_tier:
            # Memory no longer belongs in from_tier — move to to_tier
            conn.execute(
                "UPDATE semantic_memories SET tier=? WHERE id=?",
                (to_tier, mem_id),
            )
            moved += 1

    if moved:
        conn.commit()
    return moved


# ---------------------------------------------------------------------------
# PRD-069: Temporal fact versioning
# ---------------------------------------------------------------------------

def ensure_temporal_schema(conn: sqlite3.Connection) -> None:
    """Add valid_at and invalid_at columns to semantic_memories.
    Add memory_fact_history table to record superseded versions."""
    cols = {r[1] for r in conn.execute("PRAGMA table_info(semantic_memories)").fetchall()}
    if "valid_at" not in cols:
        conn.execute(
            "ALTER TABLE semantic_memories ADD COLUMN valid_at TEXT"
        )
    if "invalid_at" not in cols:
        conn.execute(
            "ALTER TABLE semantic_memories ADD COLUMN invalid_at TEXT"
        )
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS memory_fact_history (
            history_id    TEXT PRIMARY KEY,
            original_id   TEXT NOT NULL,
            successor_id  TEXT,
            profile       TEXT NOT NULL,
            content       TEXT NOT NULL,
            memory_type   TEXT NOT NULL,
            confidence    REAL NOT NULL,
            source        TEXT NOT NULL,
            valid_at      TEXT NOT NULL,
            invalid_at    TEXT NOT NULL,
            reason        TEXT NOT NULL DEFAULT '',
            archived_at   TEXT NOT NULL
        );
        CREATE INDEX IF NOT EXISTS idx_mfh_original ON memory_fact_history(original_id);
        CREATE INDEX IF NOT EXISTS idx_mfh_profile  ON memory_fact_history(profile);
    """)
    conn.commit()


def update_fact(
    conn: sqlite3.Connection,
    mem_id: str,
    new_content: str,
    *,
    profile: str,
    reason: str = "",
) -> str:
    """Update a fact: invalidate old version, create new version.

    The old row is snapshotted into memory_fact_history and then deleted from
    semantic_memories (the live table).  A fresh memory with new_content is
    inserted and its id is returned.
    """
    ensure_schema(conn)
    ensure_temporal_schema(conn)

    row = conn.execute(
        """SELECT id, profile, content, memory_type, confidence, created_at,
                  accessed_at, access_count, source,
                  COALESCE(valid_at, created_at)
           FROM semantic_memories WHERE id=? AND profile=?""",
        (mem_id, profile),
    ).fetchone()
    if row is None:
        raise KeyError(f"Memory {mem_id!r} not found for profile {profile!r}")

    (
        old_id, old_profile, old_content, old_type, old_conf,
        old_created, old_accessed, old_count, old_src, old_valid_at,
    ) = row

    now = _utc_now()

    # Archive old version
    history_id = uuid.uuid4().hex[:16]
    new_id = uuid.uuid4().hex[:16]
    conn.execute(
        """INSERT INTO memory_fact_history
               (history_id, original_id, successor_id, profile, content, memory_type,
                confidence, source, valid_at, invalid_at, reason, archived_at)
           VALUES (?,?,?,?,?,?,?,?,?,?,?,?)""",
        (
            history_id, old_id, new_id, old_profile, old_content, old_type,
            old_conf, old_src, old_valid_at, now, reason, now,
        ),
    )

    # Remove old version from live table and FTS
    conn.execute("DELETE FROM semantic_memories WHERE id=?", (old_id,))
    conn.execute("DELETE FROM semantic_memories_fts WHERE id=?", (old_id,))

    # Insert new version
    conn.execute(
        """INSERT INTO semantic_memories
               (id, profile, content, memory_type, confidence, created_at,
                accessed_at, access_count, source, valid_at)
           VALUES (?,?,?,?,?,?,?,0,?,?)""",
        (new_id, old_profile, new_content.strip(), old_type, old_conf,
         now, now, old_src, now),
    )
    conn.execute(
        "INSERT INTO semantic_memories_fts(id, profile, content, memory_type) VALUES(?,?,?,?)",
        (new_id, old_profile, new_content.strip(), old_type),
    )
    conn.commit()
    return new_id


def get_fact_history(conn: sqlite3.Connection, mem_id: str) -> list[dict]:
    """Return all historical versions of a fact via memory_fact_history.

    Includes both the archived history entries for mem_id (as original_id or
    successor_id) and the current live row, sorted chronologically.
    """
    ensure_temporal_schema(conn)

    rows = conn.execute(
        """SELECT history_id, original_id, successor_id, profile, content,
                  memory_type, confidence, source, valid_at, invalid_at, reason, archived_at
           FROM memory_fact_history
           WHERE original_id=? OR successor_id=?
           ORDER BY valid_at ASC""",
        (mem_id, mem_id),
    ).fetchall()

    history = []
    for r in rows:
        (
            hist_id, orig_id, succ_id, prof, content, mtype, conf,
            src, valid_at, invalid_at, reason, archived_at,
        ) = r
        history.append({
            "history_id": hist_id,
            "original_id": orig_id,
            "successor_id": succ_id,
            "profile": prof,
            "content": content,
            "memory_type": mtype,
            "confidence": conf,
            "source": src,
            "valid_at": valid_at,
            "invalid_at": invalid_at,
            "reason": reason,
            "archived_at": archived_at,
        })

    # Also append live version if it matches mem_id
    live = conn.execute(
        """SELECT id, profile, content, memory_type, confidence, source,
                  COALESCE(valid_at, created_at), invalid_at
           FROM semantic_memories WHERE id=?""",
        (mem_id,),
    ).fetchone()
    if live:
        history.append({
            "history_id": None,
            "original_id": live[0],
            "successor_id": None,
            "profile": live[1],
            "content": live[2],
            "memory_type": live[3],
            "confidence": live[4],
            "source": live[5],
            "valid_at": live[6],
            "invalid_at": live[7],
            "reason": "",
            "archived_at": None,
            "_current": True,
        })

    return history


def list_facts_at(conn: sqlite3.Connection, profile: str, at_time: str) -> list[dict]:
    """Return memories that were valid at a specific ISO timestamp.

    A memory was valid at at_time if:
        valid_at <= at_time  AND  (invalid_at IS NULL OR invalid_at > at_time)

    Also checks memory_fact_history for archived versions valid at that time.
    """
    ensure_temporal_schema(conn)

    # Live memories valid at at_time
    live_rows = conn.execute(
        """SELECT id, profile, content, memory_type, confidence, created_at,
                  accessed_at, access_count, source,
                  COALESCE(valid_at, created_at) AS valid_at, invalid_at
           FROM semantic_memories
           WHERE profile=?
             AND COALESCE(valid_at, created_at) <= ?
             AND (invalid_at IS NULL OR invalid_at > ?)""",
        (profile, at_time, at_time),
    ).fetchall()

    results = []
    for r in live_rows:
        mem_id, prof, content, mtype, conf_base, created, accessed, count, src, valid_at, invalid_at = r
        effective = compute_confidence(conf_base, mtype, created)
        results.append({
            "id": mem_id,
            "profile": prof,
            "content": content,
            "memory_type": mtype,
            "confidence_base": conf_base,
            "confidence": round(effective, 4),
            "created_at": created,
            "source": src,
            "valid_at": valid_at,
            "invalid_at": invalid_at,
            "_source": "live",
        })

    # Archived versions valid at at_time
    hist_rows = conn.execute(
        """SELECT original_id, profile, content, memory_type, confidence,
                  source, valid_at, invalid_at
           FROM memory_fact_history
           WHERE profile=?
             AND valid_at <= ?
             AND invalid_at > ?""",
        (profile, at_time, at_time),
    ).fetchall()

    for r in hist_rows:
        orig_id, prof, content, mtype, conf, src, valid_at, invalid_at = r
        results.append({
            "id": orig_id,
            "profile": prof,
            "content": content,
            "memory_type": mtype,
            "confidence_base": conf,
            "confidence": round(conf, 4),
            "source": src,
            "valid_at": valid_at,
            "invalid_at": invalid_at,
            "_source": "history",
        })

    results.sort(key=lambda x: x.get("valid_at", ""))
    return results


# ---------------------------------------------------------------------------
# PRD-071: Episodic memory session episodes
# ---------------------------------------------------------------------------

def ensure_episode_schema(conn: sqlite3.Connection) -> None:
    """Create memory_episodes table and memory_episode_links junction table."""
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS memory_episodes (
            episode_id   TEXT PRIMARY KEY,
            profile      TEXT NOT NULL,
            description  TEXT NOT NULL DEFAULT '',
            session_id   TEXT,
            started_at   TEXT NOT NULL,
            ended_at     TEXT,
            summary      TEXT NOT NULL DEFAULT '',
            status       TEXT NOT NULL DEFAULT 'open'
        );
        CREATE INDEX IF NOT EXISTS idx_ep_profile ON memory_episodes(profile, started_at DESC);

        CREATE TABLE IF NOT EXISTS memory_episode_links (
            memory_id    TEXT NOT NULL,
            episode_id   TEXT NOT NULL,
            linked_at    TEXT NOT NULL,
            PRIMARY KEY (memory_id, episode_id)
        );
        CREATE INDEX IF NOT EXISTS idx_el_episode ON memory_episode_links(episode_id);
        CREATE INDEX IF NOT EXISTS idx_el_memory  ON memory_episode_links(memory_id);
    """)
    conn.commit()


def start_episode(
    conn: sqlite3.Connection,
    profile: str,
    description: str = "",
    *,
    session_id: str | None = None,
) -> str:
    """Start a new memory episode. Returns episode_id."""
    ensure_episode_schema(conn)
    episode_id = uuid.uuid4().hex[:16]
    now = _utc_now()
    conn.execute(
        """INSERT INTO memory_episodes
               (episode_id, profile, description, session_id, started_at, status)
           VALUES (?,?,?,?,?,'open')""",
        (episode_id, profile, description, session_id, now),
    )
    conn.commit()
    return episode_id


def end_episode(conn: sqlite3.Connection, episode_id: str, *, summary: str = "") -> bool:
    """Mark episode as complete with optional summary. Returns True if found."""
    ensure_episode_schema(conn)
    now = _utc_now()
    cur = conn.execute(
        """UPDATE memory_episodes
           SET status='closed', ended_at=?, summary=?
           WHERE episode_id=?""",
        (now, summary, episode_id),
    )
    conn.commit()
    return cur.rowcount > 0


def tag_memory_with_episode(
    conn: sqlite3.Connection,
    memory_id: str,
    episode_id: str,
) -> bool:
    """Associate a memory with an episode (many-to-many via memory_episode_links).

    Returns True on success, False if the link already exists (idempotent).
    """
    ensure_episode_schema(conn)
    now = _utc_now()
    try:
        conn.execute(
            "INSERT INTO memory_episode_links (memory_id, episode_id, linked_at) VALUES (?,?,?)",
            (memory_id, episode_id, now),
        )
        conn.commit()
        return True
    except sqlite3.IntegrityError:
        # Link already exists — treat as success (idempotent)
        return False


def get_episode_memories(conn: sqlite3.Connection, episode_id: str) -> list[dict]:
    """Return all memories tagged with this episode, sorted by link time."""
    ensure_episode_schema(conn)
    rows = conn.execute(
        """SELECT sm.id, sm.profile, sm.content, sm.memory_type, sm.confidence,
                  sm.created_at, sm.accessed_at, sm.access_count, sm.source,
                  el.linked_at
           FROM memory_episode_links el
           JOIN semantic_memories sm ON sm.id = el.memory_id
           WHERE el.episode_id=?
           ORDER BY el.linked_at ASC""",
        (episode_id,),
    ).fetchall()

    results = []
    for r in rows:
        mem_id, prof, content, mtype, conf_base, created, accessed, count, src, linked_at = r
        effective = compute_confidence(conf_base, mtype, created)
        results.append({
            "id": mem_id,
            "profile": prof,
            "content": content,
            "memory_type": mtype,
            "confidence_base": conf_base,
            "confidence": round(effective, 4),
            "created_at": created,
            "accessed_at": accessed,
            "access_count": count,
            "source": src,
            "linked_at": linked_at,
            "episode_id": episode_id,
        })
    return results


def list_episodes(
    conn: sqlite3.Connection,
    profile: str,
    *,
    limit: int = 20,
) -> list[dict]:
    """List recent episodes with memory count, newest first."""
    ensure_episode_schema(conn)
    rows = conn.execute(
        """SELECT e.episode_id, e.profile, e.description, e.session_id,
                  e.started_at, e.ended_at, e.summary, e.status,
                  COUNT(el.memory_id) AS memory_count
           FROM memory_episodes e
           LEFT JOIN memory_episode_links el ON el.episode_id = e.episode_id
           WHERE e.profile=?
           GROUP BY e.episode_id
           ORDER BY e.started_at DESC
           LIMIT ?""",
        (profile, limit),
    ).fetchall()

    results = []
    for r in rows:
        ep_id, prof, desc, sess_id, started, ended, summary, status, mem_count = r
        results.append({
            "episode_id": ep_id,
            "profile": prof,
            "description": desc,
            "session_id": sess_id,
            "started_at": started,
            "ended_at": ended,
            "summary": summary,
            "status": status,
            "memory_count": mem_count,
        })
    return results


# ---------------------------------------------------------------------------
# PRD-072: Cross-session vector store
# ---------------------------------------------------------------------------

def ensure_vector_schema(conn: sqlite3.Connection) -> None:
    """Add embedding_json column to semantic_memories. Create vector_store_meta table."""
    cols = {r[1] for r in conn.execute("PRAGMA table_info(semantic_memories)").fetchall()}
    if "embedding_json" not in cols:
        conn.execute(
            "ALTER TABLE semantic_memories ADD COLUMN embedding_json TEXT"
        )
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS vector_store_meta (
            key   TEXT PRIMARY KEY,
            value TEXT NOT NULL
        );
    """)
    conn.commit()


def _cosine_sim(a: list[float], b: list[float]) -> float:
    """Compute cosine similarity between two vectors.

    Returns a value in [-1, 1].  Returns 0.0 if either vector has zero norm.
    """
    if not a or not b or len(a) != len(b):
        return 0.0
    dot = sum(x * y for x, y in zip(a, b))
    norm_a = math.sqrt(sum(x * x for x in a))
    norm_b = math.sqrt(sum(y * y for y in b))
    if norm_a == 0.0 or norm_b == 0.0:
        return 0.0
    return dot / (norm_a * norm_b)


def embed_text(text: str) -> list[float] | None:
    """Embed text using sentence-transformers if available, else return None.

    Uses the lightweight 'all-MiniLM-L6-v2' model (384 dims).  The model is
    cached in the process after the first call.  If sentence-transformers is
    not installed this function returns None gracefully so callers can fall
    back to keyword search.
    """
    try:
        from sentence_transformers import SentenceTransformer  # type: ignore

        # Module-level cache to avoid reloading the model on every call
        cache_attr = "_st_model_cache"
        if not hasattr(embed_text, cache_attr):
            setattr(embed_text, cache_attr, SentenceTransformer("all-MiniLM-L6-v2"))
        model = getattr(embed_text, cache_attr)
        vector = model.encode(text, normalize_embeddings=True)
        return vector.tolist()
    except Exception:
        return None


def store_embedding(
    conn: sqlite3.Connection,
    memory_id: str,
    embedding: list[float],
) -> None:
    """Store embedding as JSON in semantic_memories.embedding_json."""
    ensure_vector_schema(conn)
    conn.execute(
        "UPDATE semantic_memories SET embedding_json=? WHERE id=?",
        (_json.dumps(embedding), memory_id),
    )
    conn.commit()


def search_by_vector(
    conn: sqlite3.Connection,
    profile: str,
    query: str,
    *,
    limit: int = 10,
) -> list[dict]:
    """Semantic search using embedding cosine similarity.

    Embeds query with embed_text().  If embeddings are unavailable (library not
    installed or no stored embeddings) falls back transparently to FTS5 search
    via search_memories().

    Results are sorted by cosine similarity descending.
    """
    ensure_schema(conn)
    ensure_vector_schema(conn)

    query_vec = embed_text(query)
    if query_vec is None:
        # Graceful fallback
        return search_memories(conn, profile, query, limit=limit)

    rows = conn.execute(
        """SELECT id, profile, content, memory_type, confidence, created_at,
                  accessed_at, access_count, source, embedding_json
           FROM semantic_memories
           WHERE profile=? AND embedding_json IS NOT NULL""",
        (profile,),
    ).fetchall()

    if not rows:
        # No embeddings stored yet — fall back to FTS
        return search_memories(conn, profile, query, limit=limit)

    scored = []
    for r in rows:
        mem_id, prof, content, mtype, conf_base, created, accessed, count, src, emb_json = r
        try:
            doc_vec = _json.loads(emb_json)
        except Exception:
            continue
        sim = _cosine_sim(query_vec, doc_vec)
        effective = compute_confidence(conf_base, mtype, created)
        scored.append((sim, {
            "id": mem_id,
            "profile": prof,
            "content": content,
            "memory_type": mtype,
            "confidence_base": conf_base,
            "confidence": round(effective, 4),
            "created_at": created,
            "accessed_at": accessed,
            "access_count": count,
            "source": src,
            "similarity": round(sim, 4),
        }))

    scored.sort(key=lambda x: -x[0])
    selected = [item for _, item in scored[:limit]]

    # Update access timestamps
    if selected:
        now = _utc_now()
        for mem in selected:
            conn.execute(
                "UPDATE semantic_memories SET accessed_at=?, access_count=access_count+1 WHERE id=?",
                (now, mem["id"]),
            )
        conn.commit()

    return selected


def rebuild_embeddings(conn: sqlite3.Connection, profile: str) -> int:
    """Re-embed all memories for a profile. Returns count of memories embedded.

    Skips memories for which embed_text() returns None (library not available).
    Existing embeddings are overwritten.
    """
    ensure_schema(conn)
    ensure_vector_schema(conn)

    rows = conn.execute(
        "SELECT id, content FROM semantic_memories WHERE profile=?",
        (profile,),
    ).fetchall()

    count = 0
    for mem_id, content in rows:
        vec = embed_text(content)
        if vec is None:
            break  # library unavailable; no point continuing
        conn.execute(
            "UPDATE semantic_memories SET embedding_json=? WHERE id=?",
            (_json.dumps(vec), mem_id),
        )
        count += 1

    if count:
        conn.commit()
    return count

