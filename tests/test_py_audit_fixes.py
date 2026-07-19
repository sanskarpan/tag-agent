"""Regression tests for the Python-side audit fixes (issues #567, #568).

#567 — `mem search` must recall partial-word (substring) and CJK queries that
       the FTS5 tokenizer misses.
#568 — an invalid/missing `--config` must exit non-zero, not silently exit 0.

These tests FAIL before the fixes and pass after.
"""
from __future__ import annotations

import importlib.util
import sqlite3
from pathlib import Path

import pytest


ROOT = Path(__file__).resolve().parents[1]


def _load_module(rel_path: str, name: str):
    path = ROOT / rel_path
    spec = importlib.util.spec_from_file_location(name, path)
    assert spec and spec.loader
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


SM = _load_module("src/tag/semantic_memory.py", "tag_semantic_memory")
TAG = _load_module("src/tag/controller.py", "tag_controller_audit")


# ---------------------------------------------------------------------------
# #567 — partial-word + CJK recall in mem search
# ---------------------------------------------------------------------------

@pytest.fixture()
def mem_conn():
    conn = sqlite3.connect(":memory:")
    SM.add_memory(conn, "default", "日本語 émoji café")
    yield conn
    conn.close()


def _contents(results):
    return [r["content"] for r in results]


def test_mem_search_ascii_substring_recall(mem_conn):
    # "moji" is an ASCII substring of "émoji" — the FTS tokenizer misses it.
    results = SM.search_memories(mem_conn, "default", "moji")
    assert "日本語 émoji café" in _contents(results)


def test_mem_search_cjk_substring_recall(mem_conn):
    # CJK substrings of "日本語" that the tokenizer does not split on.
    for query in ("日本語", "本語", "日本"):
        results = SM.search_memories(mem_conn, "default", query)
        assert "日本語 émoji café" in _contents(results), f"missed {query!r}"


def test_mem_search_normal_fts_query_still_works(mem_conn):
    # A whole-token query must keep matching (no regression to FTS behaviour).
    results = SM.search_memories(mem_conn, "default", "café")
    assert "日本語 émoji café" in _contents(results)


def test_mem_search_non_substring_query_returns_empty(mem_conn):
    # A query that is genuinely absent must still return nothing.
    assert SM.search_memories(mem_conn, "default", "メモ") == []


def test_mem_search_like_wildcards_are_literal(mem_conn):
    # '%' / '_' in the query must be matched literally, not as SQL wildcards.
    assert SM.search_memories(mem_conn, "default", "%café%zzz") == []


def test_mem_search_hybrid_substring_recall(mem_conn):
    for query in ("moji", "本語"):
        results = SM.search_memories_hybrid(mem_conn, "default", query)
        assert "日本語 émoji café" in _contents(results), f"hybrid missed {query!r}"


# ---------------------------------------------------------------------------
# #568 — invalid / missing --config must exit non-zero
# ---------------------------------------------------------------------------

def test_invalid_yaml_config_exits_nonzero(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    bad = tmp_path / "bad.yaml"
    bad.write_text("foo: [unclosed\n", encoding="utf-8")
    code = TAG.main(["--config", str(bad), "mem", "list"])
    assert code != 0


def test_missing_config_exits_nonzero(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    missing = tmp_path / "does-not-exist.yaml"
    code = TAG.main(["--config", str(missing), "mem", "list"])
    assert code != 0
