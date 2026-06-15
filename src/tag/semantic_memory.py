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
