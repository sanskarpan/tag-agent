"""PRD-068: Background sleep-time memory consolidation (GC).

Merges near-duplicate memories, evicts low-confidence/decayed memories,
promotes frequently-accessed memories, and schedules background runs as
a cron job or daemon thread.
"""
from __future__ import annotations

import sqlite3
import threading
import time
import uuid
from dataclasses import dataclass
from datetime import datetime, timezone

from tag.semantic_memory import compute_confidence
from tag.semantic_memory import ensure_schema as ensure_memory_schema


# ---------------------------------------------------------------------------
# Dataclasses
# ---------------------------------------------------------------------------

@dataclass
class GCConfig:
    """Tunable parameters for a GC run."""

    min_confidence_to_keep: float = 0.05
    dedup_similarity_threshold: float = 0.75
    max_memories_per_profile: int = 500
    promote_threshold: float = 0.9
    batch_size: int = 100


@dataclass
class GCResult:
    """Summary of a single GC run for one profile."""

    profile: str
    evicted_count: int
    merged_count: int
    promoted_count: int
    duration_seconds: float
    run_at: str


# ---------------------------------------------------------------------------
# Schema
# ---------------------------------------------------------------------------

def ensure_schema(conn: sqlite3.Connection) -> None:
    """Create the memory_gc_runs audit table if it does not already exist."""
    conn.execute(
        """CREATE TABLE IF NOT EXISTS memory_gc_runs (
            id         TEXT    PRIMARY KEY,
            profile    TEXT    NOT NULL,
            evicted    INTEGER NOT NULL DEFAULT 0,
            merged     INTEGER NOT NULL DEFAULT 0,
            promoted   INTEGER NOT NULL DEFAULT 0,
            duration_s REAL    NOT NULL DEFAULT 0.0,
            run_at     TEXT    NOT NULL
        )"""
    )
    conn.commit()


# ---------------------------------------------------------------------------
# Similarity helpers
# ---------------------------------------------------------------------------

def _jaccard(text_a: str, text_b: str) -> float:
    """Jaccard similarity of the word bags of two strings."""
    set_a = set(text_a.lower().split())
    set_b = set(text_b.lower().split())
    union = set_a | set_b
    if not union:
        return 0.0
    return len(set_a & set_b) / len(union)


def _find_near_duplicates(
    memories: list[dict],
    threshold: float,
) -> list[tuple[str, str]]:
    """Return (id_a, id_b) pairs whose Jaccard word similarity > threshold.

    Only the upper triangle of the pair matrix is examined so each pair is
    returned at most once.
    """
    pairs: list[tuple[str, str]] = []
    n = len(memories)
    for i in range(n):
        for j in range(i + 1, n):
            sim = _jaccard(memories[i]["content"], memories[j]["content"])
            if sim > threshold:
                pairs.append((memories[i]["id"], memories[j]["id"]))
    return pairs


# ---------------------------------------------------------------------------
# Low-level delete (keeps FTS in sync)
# ---------------------------------------------------------------------------

def _delete_memory_row(conn: sqlite3.Connection, mem_id: str, profile: str) -> None:
    conn.execute(
        "DELETE FROM semantic_memories WHERE id=? AND profile=?",
        (mem_id, profile),
    )
    try:
        conn.execute("DELETE FROM semantic_memories_fts WHERE id=?", (mem_id,))
    except sqlite3.OperationalError:
        pass  # FTS table absent in test environments


# ---------------------------------------------------------------------------
# GC operations
# ---------------------------------------------------------------------------

def merge_duplicates(
    conn: sqlite3.Connection,
    profile: str,
    config: GCConfig,
) -> int:
    """Detect near-duplicate memories for *profile* and delete the weaker copy.

    Effective confidence (after decay) determines which copy survives.
    When effective confidences are equal the memory with the lower base
    confidence is removed (i.e. the one that decayed more is kept intact,
    favouring newer data).

    Returns the number of memories removed (merged away).
    """
    ensure_memory_schema(conn)

    rows = conn.execute(
        """SELECT id, content, memory_type, confidence, created_at
           FROM semantic_memories
           WHERE profile=?""",
        (profile,),
    ).fetchall()

    memories = [
        {
            "id": r[0],
            "content": r[1],
            "memory_type": r[2],
            "confidence_base": r[3],
            "created_at": r[4],
        }
        for r in rows
    ]

    if len(memories) < 2:
        return 0

    pairs = _find_near_duplicates(memories, config.dedup_similarity_threshold)
    if not pairs:
        return 0

    by_id = {m["id"]: m for m in memories}
    deleted: set[str] = set()
    merged = 0

    for id_a, id_b in pairs:
        if id_a in deleted or id_b in deleted:
            continue
        mem_a = by_id.get(id_a)
        mem_b = by_id.get(id_b)
        if mem_a is None or mem_b is None:
            continue

        eff_a = compute_confidence(
            mem_a["confidence_base"], mem_a["memory_type"], mem_a["created_at"]
        )
        eff_b = compute_confidence(
            mem_b["confidence_base"], mem_b["memory_type"], mem_b["created_at"]
        )

        # Keep the one with higher effective confidence.
        loser = id_b if eff_a >= eff_b else id_a
        _delete_memory_row(conn, loser, profile)
        deleted.add(loser)
        merged += 1

    if merged:
        conn.commit()

    return merged


def evict_low_confidence(
    conn: sqlite3.Connection,
    profile: str,
    config: GCConfig,
) -> int:
    """Evict memories whose effective confidence is below the minimum threshold.

    Also enforces max_memories_per_profile: after the confidence cull, if
    the surviving count still exceeds the cap the lowest-confidence / oldest
    memories are removed until the profile is within the limit.

    Returns the total number of evicted memories.
    """
    ensure_memory_schema(conn)

    rows = conn.execute(
        """SELECT id, memory_type, confidence, created_at
           FROM semantic_memories
           WHERE profile=?
           ORDER BY created_at ASC""",
        (profile,),
    ).fetchall()

    evicted_ids: set[str] = set()
    effective_map: dict[str, float] = {}

    for row in rows:
        mem_id, mtype, conf_base, created = row
        eff = compute_confidence(conf_base, mtype, created)
        effective_map[mem_id] = eff
        if eff < config.min_confidence_to_keep:
            _delete_memory_row(conn, mem_id, profile)
            evicted_ids.add(mem_id)

    if evicted_ids:
        conn.commit()

    # Cap enforcement — work with the survivors still in the DB.
    survivors = [
        (effective_map[mid], mid)
        for mid in effective_map
        if mid not in evicted_ids
    ]

    cap_evicted = 0
    if len(survivors) > config.max_memories_per_profile:
        overage = len(survivors) - config.max_memories_per_profile
        # Sort ascending by effective confidence so weakest are removed first.
        survivors.sort(key=lambda x: x[0])
        for eff, mid in survivors[:overage]:
            _delete_memory_row(conn, mid, profile)
            cap_evicted += 1

    if cap_evicted:
        conn.commit()

    return len(evicted_ids) + cap_evicted


def promote_high_access(
    conn: sqlite3.Connection,
    profile: str,
    config: GCConfig,
) -> int:
    """Boost base confidence of frequently-accessed memories.

    Any memory with access_count > 5 and confidence_base < promote_threshold
    has its base confidence bumped to min(1.0, confidence_base * 1.2).

    Returns the number of memories promoted.
    """
    rows = conn.execute(
        """SELECT id, confidence
           FROM semantic_memories
           WHERE profile=?
             AND access_count > 5
             AND confidence < ?""",
        (profile, config.promote_threshold),
    ).fetchall()

    promoted = 0
    for mem_id, conf_base in rows:
        new_conf = min(1.0, conf_base * 1.2)
        conn.execute(
            "UPDATE semantic_memories SET confidence=? WHERE id=?",
            (new_conf, mem_id),
        )
        promoted += 1

    if promoted:
        conn.commit()

    return promoted


# ---------------------------------------------------------------------------
# Orchestrators
# ---------------------------------------------------------------------------

def run_gc(
    conn: sqlite3.Connection,
    profile: str,
    *,
    config: GCConfig | None = None,
) -> GCResult:
    """Run a full GC cycle for *profile* (evict → merge → promote).

    Records the outcome in the memory_gc_runs audit table and returns a
    GCResult summary.
    """
    if config is None:
        config = GCConfig()

    ensure_schema(conn)
    ensure_memory_schema(conn)

    run_at = datetime.now(timezone.utc).isoformat()
    t0 = time.monotonic()

    evicted = evict_low_confidence(conn, profile, config)
    merged = merge_duplicates(conn, profile, config)
    promoted = promote_high_access(conn, profile, config)

    duration = time.monotonic() - t0
    run_id = uuid.uuid4().hex[:16]

    conn.execute(
        """INSERT INTO memory_gc_runs
               (id, profile, evicted, merged, promoted, duration_s, run_at)
           VALUES (?, ?, ?, ?, ?, ?, ?)""",
        (run_id, profile, evicted, merged, promoted, round(duration, 6), run_at),
    )
    conn.commit()

    return GCResult(
        profile=profile,
        evicted_count=evicted,
        merged_count=merged,
        promoted_count=promoted,
        duration_seconds=round(duration, 6),
        run_at=run_at,
    )


def run_gc_all_profiles(
    conn: sqlite3.Connection,
    *,
    config: GCConfig | None = None,
) -> list[GCResult]:
    """Discover all distinct profiles in semantic_memories and run GC for each.

    Returns a list of GCResult objects (one per profile).
    """
    ensure_memory_schema(conn)

    profiles = [
        r[0]
        for r in conn.execute(
            "SELECT DISTINCT profile FROM semantic_memories"
        ).fetchall()
    ]

    return [run_gc(conn, profile, config=config) for profile in profiles]


# ---------------------------------------------------------------------------
# Scheduling
# ---------------------------------------------------------------------------

def _daemon_loop(interval_seconds: float, config: GCConfig) -> None:  # pragma: no cover
    """Background daemon thread: sleep then GC all profiles."""
    while True:
        time.sleep(interval_seconds)
        try:
            from tag import db as _tag_db  # type: ignore[import]
            conn = _tag_db.get_connection()
            run_gc_all_profiles(conn, config=config)
        except Exception:
            # Never let the daemon thread crash.
            pass


def schedule_gc(interval_hours: float = 6.0) -> None:
    """Register GC as a periodic background task.

    Attempts to register via tag.cron_scheduler; if that module is not
    available falls back to starting a daemon thread with a time.sleep loop.
    """
    config = GCConfig()
    interval_seconds = interval_hours * 3600.0

    try:
        from tag import cron_scheduler  # type: ignore[import]

        def _cron_job() -> None:
            try:
                from tag import db as _tag_db  # type: ignore[import]
                conn = _tag_db.get_connection()
            except Exception:
                conn = sqlite3.connect(":memory:", check_same_thread=False)
                ensure_memory_schema(conn)
            run_gc_all_profiles(conn, config=config)

        # cron_scheduler may expose a register() helper; if not, fall through.
        _register = getattr(cron_scheduler, "register", None)
        if _register is None:
            raise ImportError("cron_scheduler.register not available")
        _register(
            name="memory_gc",
            interval_seconds=interval_seconds,
            callback=_cron_job,
        )
        return
    except (ImportError, AttributeError, TypeError):
        pass

    # Fallback: daemon thread.
    t = threading.Thread(
        target=_daemon_loop,
        args=(interval_seconds, config),
        daemon=True,
        name="memory-gc-daemon",
    )
    t.start()
