"""Runtime contracts uncovered by the pinned checker, not checker-only casts."""
import argparse
import sqlite3
from pathlib import Path
from types import SimpleNamespace

import pytest


def test_eval_judge_uses_real_cost_signature(monkeypatch):
    from tag import eval_judge
    from tag.core import paths
    monkeypatch.setattr(paths, "hermes_bin", lambda cfg: Path("/synthetic/hermes"))
    monkeypatch.setattr(eval_judge.subprocess, "run", lambda *a, **kw:
                        SimpleNamespace(stdout='{"score":0.9,"rationale":"good"}'))
    calls = []
    def cost(*, model_id, input_tokens, output_tokens):
        calls.append((model_id, input_tokens, output_tokens))
        return .0123
    monkeypatch.setattr(eval_judge, "compute_cost", cost)
    monkeypatch.setattr(eval_judge, "_HAS_COST", True)
    result = eval_judge.invoke_judge("question", "answer", "correctness", "model", {})
    assert result.cost_usd == .0123
    assert calls[0][0] == "model"
    assert calls[0][1] > 0 and calls[0][2] > 0


@pytest.mark.parametrize("all_profiles", [False, True])
def test_gc_preview_matches_execution_without_mutation(tmp_path, monkeypatch, capsys, all_profiles):
    from tag.cmd import prd_clusters
    from tag.semantic_memory import add_memory
    path = tmp_path / "memory.db"
    conn = sqlite3.connect(path)
    add_memory(conn, "coder", "ephemeral", confidence=.01)
    add_memory(conn, "researcher", "other", confidence=.01)
    conn.close()
    before = path.read_bytes()
    monkeypatch.setattr(prd_clusters, "_load_cfg_and_profile", lambda _: ({}, "coder"))
    monkeypatch.setattr(prd_clusters, "_db_for_profile", lambda *a: path)
    args = argparse.Namespace(mem_subcommand="gc", profile="coder", dry_run=True,
                              all_profiles=all_profiles)
    assert prd_clusters.cmd_mem_ext(args) == 0
    preview = capsys.readouterr().out
    assert "preview only" in preview and "evicted=1" in preview
    assert path.read_bytes() == before
    args.dry_run = False
    assert prd_clusters.cmd_mem_ext(args) == 0
    execution = capsys.readouterr().out
    assert preview.splitlines()[1:] == execution.splitlines()
    conn = sqlite3.connect(path)
    assert conn.execute("SELECT COUNT(*) FROM semantic_memories").fetchone()[0] == (0 if all_profiles else 1)
    conn.close()


def test_memory_extract_handles_count_result(tmp_path, monkeypatch, capsys):
    from tag.cmd import prd_clusters
    from tag import memory_extractor
    path = tmp_path / "memory.db"
    conn = sqlite3.connect(path)
    conn.execute("CREATE TABLE runs(id TEXT, output TEXT)")
    conn.execute("INSERT INTO runs VALUES('test','output')")
    conn.commit()
    conn.close()
    monkeypatch.setattr(prd_clusters, "_load_cfg_and_profile", lambda _: ({}, "coder"))
    monkeypatch.setattr(prd_clusters, "_db_for_profile", lambda *a: path)
    monkeypatch.setattr(memory_extractor, "auto_extract_post_run", lambda *a: 2)
    assert prd_clusters.cmd_mem_ext(argparse.Namespace(mem_subcommand="extract", run_id="test")) == 0
    assert "Extracted 2 memories" in capsys.readouterr().out


def test_fact_list_requires_timestamp_and_calls_actual_signature(tmp_path, monkeypatch, capsys):
    from tag.cmd import prd_clusters
    from tag import semantic_memory
    path = tmp_path / "memory.db"
    monkeypatch.setattr(prd_clusters, "_load_cfg_and_profile", lambda _: ({}, "coder"))
    monkeypatch.setattr(prd_clusters, "_db_for_profile", lambda *a: path)
    args = argparse.Namespace(mem_subcommand="fact", action="list-at", at=None)
    assert prd_clusters.cmd_mem_ext(args) == 1
    capsys.readouterr()
    calls = []
    original = semantic_memory.list_facts_at
    def capture(conn, profile, at_time):
        calls.append(at_time)
        return original(conn, profile, at_time)
    monkeypatch.setattr(semantic_memory, "list_facts_at", capture)
    args.at = "2026-09-08T00:00:00Z"
    assert prd_clusters.cmd_mem_ext(args) == 0
    assert calls == [args.at]
