"""Parity of `mem` --json fields with the Go harness (2026-08-24 QA sweep)."""

from __future__ import annotations

import sqlite3

from tag.semantic_memory import (
    add_memory,
    ensure_schema,
    list_memories,
    search_memories_hybrid,
)


def _db():
    conn = sqlite3.connect(":memory:")
    ensure_schema(conn)
    return conn


def test_hybrid_search_exposes_score_fields():
    # #758: mem search must carry dense_score/sparse_score/hybrid_score (Go parity).
    conn = _db()
    add_memory(conn, "default", "the sky is blue", memory_type="fact")
    results = search_memories_hybrid(conn, "default", "sky", limit=5)
    assert results, "expected a hit"
    r = results[0]
    for key in ("dense_score", "sparse_score", "hybrid_score"):
        assert key in r, f"missing {key}"
    assert isinstance(r["hybrid_score"], float)


def test_list_includes_profile():
    # #759: mem list items must include the profile (Go parity).
    conn = _db()
    add_memory(conn, "coder", "some content here", memory_type="fact")
    items = list_memories(conn, "coder")
    assert items and items[0]["profile"] == "coder"
