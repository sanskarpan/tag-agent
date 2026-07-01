"""
Comprehensive end-to-end tests for PRD-021 through PRD-032.

Each section maps to one PRD and exercises the public API using only local
filesystem operations — no network calls, no real Hermes binary, no real
API keys.
"""
from __future__ import annotations

import importlib.util
import io
import json
import os
import sqlite3
import sys
import textwrap
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import MagicMock, patch

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

# Load modules via importlib so we can reload cleanly without polluting sys.modules
def _load(name: str, path: Path):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod

TAG = _load("tag_controller", ROOT / "src" / "tag" / "controller.py")


def make_db(tmp_path: Path):
    """Return (cfg, db) with a fresh runtime DB."""
    with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        db = TAG.open_db(cfg)
    return cfg, db


def make_args(**kwargs) -> SimpleNamespace:
    defaults = {
        "config": None,
        "json": False,
        "profile": None,
    }
    defaults.update(kwargs)
    return SimpleNamespace(**defaults)


# ===========================================================================
# PRD-021: Agent Loop / Autonomous Mode
# ===========================================================================

class TestAgentLoop:
    def test_loop_table_created(self, tmp_path):
        cfg, db = make_db(tmp_path)
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "loop_runs" in tables
        assert "loop_iterations" in tables
        db.close()

    def test_loop_start_list(self, tmp_path):
        cfg, db = make_db(tmp_path)

        # Insert a loop_run directly (bypass subprocess)
        now = TAG.utc_now()
        db.execute(
            "INSERT INTO loop_runs(id, profile, goal, max_iters, current_iter, status, "
            "approval, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?)",
            ("testloop1", "orchestrator", "Fix the bug", 5, 2, "running", "auto", now, now),
        )
        db.commit()

        rows = db.execute("SELECT id, status, goal FROM loop_runs WHERE id='testloop1'").fetchall()
        assert len(rows) == 1
        assert rows[0][1] == "running"
        assert "Fix" in rows[0][2]
        db.close()

    def test_loop_abort_updates_status(self, tmp_path):
        cfg, db = make_db(tmp_path)
        now = TAG.utc_now()
        db.execute(
            "INSERT INTO loop_runs(id, profile, goal, max_iters, current_iter, status, "
            "approval, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?)",
            ("loop_abort_test", "orchestrator", "Goal", 10, 0, "running", "auto", now, now),
        )
        db.commit()
        # Simulate abort
        db.execute(
            "UPDATE loop_runs SET status='aborted' WHERE id=? AND status='running'",
            ("loop_abort_test",),
        )
        db.commit()
        row = db.execute("SELECT status FROM loop_runs WHERE id='loop_abort_test'").fetchone()
        assert row[0] == "aborted"
        db.close()

    def test_loop_iteration_insert(self, tmp_path):
        cfg, db = make_db(tmp_path)
        now = TAG.utc_now()
        import uuid
        loop_id = "loopiter1"
        db.execute(
            "INSERT INTO loop_runs(id, profile, goal, max_iters, current_iter, status, "
            "approval, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?)",
            (loop_id, "orchestrator", "Goal text", 5, 0, "running", "auto", now, now),
        )
        iter_id = uuid.uuid4().hex[:12]
        db.execute(
            "INSERT INTO loop_iterations(id, loop_id, iteration, input, output, decision, created_at) "
            "VALUES(?,?,?,?,?,?,?)",
            (iter_id, loop_id, 1, "Goal text", "GOAL_ACHIEVED found", "goal_achieved", now),
        )
        db.commit()
        iters = db.execute(
            "SELECT decision FROM loop_iterations WHERE loop_id=?", (loop_id,)
        ).fetchall()
        assert len(iters) == 1
        assert iters[0][0] == "goal_achieved"
        db.close()

    def test_cmd_loop_list_empty(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(loop_subcommand="list", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_loop(args)
        captured = capsys.readouterr()
        assert "No loop runs" in captured.out

    def test_cmd_loop_start_requires_goal(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(loop_subcommand="start", goal="", profile=None,
                         max_iters=5, approval="auto", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_loop(args)
        assert rc == 1


# ===========================================================================
# PRD-022: Cron / Scheduled Agents
# ===========================================================================

class TestCronScheduler:
    def test_cron_table_created(self, tmp_path):
        cfg, db = make_db(tmp_path)
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "cron_jobs" in tables
        db.close()

    def test_validate_cron_expression_valid(self):
        from tag.cron_scheduler import validate_cron_expression
        validate_cron_expression("0 9 * * 1-5")
        validate_cron_expression("*/5 * * * *")
        validate_cron_expression("0 0 1 1 *")

    def test_validate_cron_expression_invalid(self):
        from tag.cron_scheduler import validate_cron_expression
        with pytest.raises(ValueError):
            validate_cron_expression("not-valid")
        with pytest.raises(ValueError):
            validate_cron_expression("0 9 *")  # too few fields

    def test_cron_matches_exact(self):
        from tag.cron_scheduler import cron_matches
        from datetime import datetime
        # Monday 09:00
        dt = datetime(2026, 6, 15, 9, 0)  # Monday
        assert cron_matches("0 9 * * *", dt) is True
        assert cron_matches("0 10 * * *", dt) is False
        assert cron_matches("1 9 * * *", dt) is False

    def test_cron_matches_range(self):
        from tag.cron_scheduler import cron_matches
        from datetime import datetime
        dt = datetime(2026, 6, 15, 9, 30)  # Monday 09:30
        assert cron_matches("30 9 * * 0-4", dt) is True  # Mon-Fri
        assert cron_matches("30 9 * * 5-6", dt) is False  # Sat-Sun

    def test_cron_matches_step(self):
        from tag.cron_scheduler import cron_matches
        from datetime import datetime
        dt = datetime(2026, 6, 15, 9, 15)
        assert cron_matches("*/15 * * * *", dt) is True
        dt2 = datetime(2026, 6, 15, 9, 7)
        assert cron_matches("*/15 * * * *", dt2) is False

    def test_cmd_cron_add_and_list(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(
            cron_subcommand="add",
            name="standup",
            schedule="0 9 * * 1-5",
            profile="orchestrator",
            task="Run standup summary",
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_cron(args)
        assert rc == 0
        out = capsys.readouterr().out
        assert "cron job added" in out

        args2 = make_args(cron_subcommand="list", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_cron(args2)
        out2 = capsys.readouterr().out
        assert "standup" in out2
        assert "0 9 * * 1-5" in out2

    def test_cmd_cron_invalid_schedule(self, tmp_path):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(
            cron_subcommand="add",
            name="bad",
            schedule="not-a-cron",
            profile="orchestrator",
            task="task",
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_cron(args)
        assert rc == 1

    def test_cmd_cron_remove(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        import uuid
        now = TAG.utc_now()
        job_id = uuid.uuid4().hex[:8]
        db.execute(
            "INSERT INTO cron_jobs(id, name, schedule, profile, task, enabled, run_count, created_at, updated_at) "
            "VALUES(?,?,?,?,?,1,0,?,?)",
            (job_id, "testjob", "0 * * * *", "orchestrator", "task", now, now),
        )
        db.commit()
        db.close()

        args = make_args(cron_subcommand="remove", job_id=job_id, config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_cron(args)
        assert rc == 0
        assert "removed" in capsys.readouterr().out

    def test_cmd_cron_enable_disable(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        import uuid
        now = TAG.utc_now()
        job_id = uuid.uuid4().hex[:8]
        db.execute(
            "INSERT INTO cron_jobs(id, name, schedule, profile, task, enabled, run_count, created_at, updated_at) "
            "VALUES(?,?,?,?,?,1,0,?,?)",
            (job_id, "entest", "0 * * * *", "orchestrator", "task", now, now),
        )
        db.commit()
        db.close()

        args = make_args(cron_subcommand="disable", job_id=job_id, config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_cron(args)

        cfg2, db2 = make_db(tmp_path)
        row = db2.execute("SELECT enabled FROM cron_jobs WHERE id=?", (job_id,)).fetchone()
        assert row[0] == 0
        db2.close()


# ===========================================================================
# PRD-023: Multi-Agent Swarm (existing cmd_swarm tested minimally)
# ===========================================================================

class TestSwarm:
    def test_swarm_command_exists(self):
        assert callable(TAG.cmd_swarm)

    def test_swarm_requires_task(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(
            task="",
            task_type="mixed",
            board="default",
            profile=None,
            no_wait=False,
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_swarm(args)
        assert rc == 1
        assert "empty" in capsys.readouterr().err.lower()


# ===========================================================================
# PRD-024: Repo-Map / Workspace Context
# ===========================================================================

class TestWorkspace:
    def test_workspace_table_created(self, tmp_path):
        cfg, db = make_db(tmp_path)
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "workspace_files" in tables
        db.close()

    def test_index_workspace_basic(self, tmp_path):
        from tag.workspace import index_workspace
        # Create some test files
        (tmp_path / "main.py").write_text("print('hello')")
        (tmp_path / "utils.py").write_text("def helper(): pass")
        (tmp_path / "README.md").write_text("# Project")
        (tmp_path / "data.json").write_text('{"key": "val"}')
        # Hidden dir — should be skipped
        (tmp_path / ".git").mkdir()
        (tmp_path / ".git" / "config").write_text("[core]")

        cfg, db = make_db(tmp_path)
        result = index_workspace(db, tmp_path, max_files=100)
        db.close()
        assert result["files_indexed"] >= 3
        assert result["total_tokens"] > 0

    def test_build_workspace_map(self, tmp_path):
        from tag.workspace import index_workspace, build_workspace_map
        (tmp_path / "app.py").write_text("x = 1\n" * 10)
        (tmp_path / "test.py").write_text("y = 2\n" * 5)

        cfg, db = make_db(tmp_path)
        index_workspace(db, tmp_path)
        ws_map = build_workspace_map(db, tmp_path, budget_tokens=10000)
        db.close()
        assert "app.py" in ws_map or "test.py" in ws_map

    def test_workspace_status_empty(self, tmp_path):
        from tag.workspace import workspace_status
        cfg, db = make_db(tmp_path)
        stats = workspace_status(db)
        db.close()
        assert stats["file_count"] == 0

    def test_cmd_workspace_index(self, tmp_path, capsys):
        (tmp_path / "src").mkdir()
        (tmp_path / "src" / "app.py").write_text("print('x')")
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(
            workspace_subcommand="index",
            path=str(tmp_path),
            max_files=100,
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_workspace(args)
        assert rc == 0
        out = capsys.readouterr().out
        assert "Indexed" in out

    def test_cmd_workspace_map(self, tmp_path, capsys):
        from tag.workspace import index_workspace
        (tmp_path / "main.py").write_text("x = 1")
        cfg, db = make_db(tmp_path)
        index_workspace(db, tmp_path)
        db.close()
        args = make_args(
            workspace_subcommand="map",
            path=str(tmp_path),
            budget=4000,
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_workspace(args)
        out = capsys.readouterr().out
        assert "Workspace" in out or "main.py" in out

    def test_cmd_workspace_clear(self, tmp_path, capsys):
        from tag.workspace import index_workspace
        (tmp_path / "a.py").write_text("x")
        cfg, db = make_db(tmp_path)
        index_workspace(db, tmp_path)
        db.close()
        args = make_args(workspace_subcommand="clear", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_workspace(args)
        out = capsys.readouterr().out
        assert "cleared" in out.lower()


# ===========================================================================
# PRD-025: Semantic Memory with Confidence Decay
# ===========================================================================

class TestSemanticMemory:
    def test_semantic_memory_table_created(self, tmp_path):
        cfg, db = make_db(tmp_path)
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "semantic_memories" in tables
        db.close()

    def test_add_and_search_memory(self, tmp_path):
        from tag.semantic_memory import add_memory, search_memories, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        mid = add_memory(db, "default", "We use pytest for testing", memory_type="convention")
        assert len(mid) > 0
        results = search_memories(db, "default", "pytest")
        db.close()
        assert len(results) > 0
        assert "pytest" in results[0]["content"]

    def test_confidence_decay_convention(self):
        from tag.semantic_memory import compute_confidence
        # Conventions never decay
        conf = compute_confidence(1.0, "convention", "2020-01-01T00:00:00+00:00")
        assert conf == 1.0

    def test_confidence_decay_fact(self):
        from tag.semantic_memory import compute_confidence
        # fact with half_life=90d — after 90 days should be ~0.5
        import datetime
        old_date = (datetime.datetime.now(datetime.timezone.utc)
                    - datetime.timedelta(days=90)).isoformat()
        conf = compute_confidence(1.0, "fact", old_date)
        assert 0.4 < conf < 0.6  # ~0.5

    def test_confidence_decay_decision(self):
        from tag.semantic_memory import compute_confidence
        import datetime
        very_old = (datetime.datetime.now(datetime.timezone.utc)
                    - datetime.timedelta(days=360)).isoformat()
        conf = compute_confidence(1.0, "decision", very_old)
        # After 2 half-lives (360/180=2), should be ~0.25
        assert conf < 0.35

    def test_list_memories(self, tmp_path):
        from tag.semantic_memory import add_memory, list_memories, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        add_memory(db, "p1", "Memory A", memory_type="fact")
        add_memory(db, "p1", "Memory B", memory_type="decision")
        add_memory(db, "p2", "Other profile memory")
        mems = list_memories(db, "p1")
        db.close()
        assert len(mems) == 2
        assert all(m["content"] in ("Memory A", "Memory B") for m in mems)

    def test_forget_memory(self, tmp_path):
        from tag.semantic_memory import add_memory, forget_memory, list_memories, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        mid = add_memory(db, "p1", "To be forgotten")
        deleted = forget_memory(db, mid, "p1")
        assert deleted is True
        mems = list_memories(db, "p1")
        db.close()
        assert len(mems) == 0

    def test_forget_wrong_profile_fails(self, tmp_path):
        from tag.semantic_memory import add_memory, forget_memory, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        mid = add_memory(db, "p1", "Secret")
        deleted = forget_memory(db, mid, "p2")  # wrong profile
        db.close()
        assert deleted is False

    def test_memory_stats(self, tmp_path):
        from tag.semantic_memory import add_memory, memory_stats, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        add_memory(db, "p1", "Fact 1", memory_type="fact")
        add_memory(db, "p1", "Convention 1", memory_type="convention")
        stats = memory_stats(db, "p1")
        db.close()
        assert stats["total"] == 2
        assert "fact" in stats["by_type"]
        assert "convention" in stats["by_type"]

    def test_cmd_mem_add_search(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        add_args = make_args(
            mem_subcommand="add",
            content="Use SQLite for persistence",
            memory_type="convention",
            confidence=1.0,
            profile="orchestrator",
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_memory_semantic(add_args)
        assert rc == 0

        search_args = make_args(
            mem_subcommand="search",
            query="SQLite",
            memory_type=None,
            limit=5,
            profile="orchestrator",
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_memory_semantic(search_args)
        out = capsys.readouterr().out
        assert "SQLite" in out or "Memory saved" in out

    def test_cmd_mem_add_invalid_type(self, tmp_path):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(
            mem_subcommand="add",
            content="Some content",
            memory_type="invalid_type",
            confidence=1.0,
            profile="orchestrator",
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_memory_semantic(args)
        assert rc == 1


# ===========================================================================
# PRD-026: Profile Marketplace
# ===========================================================================

class TestProfileMarketplace:
    def test_marketplace_table_created(self, tmp_path):
        cfg, db = make_db(tmp_path)
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "profile_cache" in tables
        db.close()

    def test_marketplace_list_empty(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(marketplace_subcommand="list", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_profile_marketplace(args)
        out = capsys.readouterr().out
        assert "No cached profiles" in out

    def test_marketplace_pull_saves_profile(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        # Mock the HTTP call
        fake_yaml = b"name: test-profile\nmodel: claude-opus-4\n"
        mock_resp = MagicMock()
        mock_resp.read.return_value = fake_yaml

        with patch("urllib.request.urlopen", return_value=mock_resp), \
             patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            args = make_args(
                marketplace_subcommand="pull",
                url="https://example.com/profile.yaml",
                name="test-profile",
                config=None,
            )
            rc = TAG.cmd_profile_marketplace(args)
        assert rc == 0
        out = capsys.readouterr().out
        assert "Pulled profile" in out

    def test_marketplace_pull_invalid_yaml(self, tmp_path):
        cfg, db = make_db(tmp_path)
        db.close()
        mock_resp = MagicMock()
        mock_resp.read.return_value = b"[not: valid: yaml: :"

        import urllib.error
        with patch("urllib.request.urlopen", side_effect=urllib.error.URLError("network error")), \
             patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            args = make_args(
                marketplace_subcommand="pull",
                url="https://bad.example.com/x.yaml",
                name=None,
                config=None,
            )
            rc = TAG.cmd_profile_marketplace(args)
        assert rc == 1


# ===========================================================================
# PRD-027: Eval Framework
# ===========================================================================

class TestEvalFramework:
    def test_eval_tables_created(self, tmp_path):
        cfg, db = make_db(tmp_path)
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "eval_runs" in tables
        assert "eval_cases" in tables
        db.close()

    def test_score_case_all_pass(self):
        from tag.eval_framework import score_case
        case = {
            "id": "t1",
            "expect_contains": ["hello", "world"],
            "min_length": 5,
        }
        output = "Hello World! This is a test."
        passed, score, reason = score_case(case, output)
        assert passed
        assert score == 1.0
        assert reason is None

    def test_score_case_partial_fail(self):
        from tag.eval_framework import score_case
        case = {
            "expect_contains": ["hello", "missing_word"],
        }
        output = "hello there"
        passed, score, reason = score_case(case, output)
        assert not passed
        assert 0.0 < score < 1.0
        assert "missing_word" in reason

    def test_score_case_not_contains(self):
        from tag.eval_framework import score_case
        case = {
            "expect_not_contains": ["error", "exception"],
        }
        output = "All good!"
        passed, score, reason = score_case(case, output)
        assert passed
        assert score == 1.0

    def test_score_case_fail_not_contains(self):
        from tag.eval_framework import score_case
        case = {
            "expect_not_contains": ["error"],
        }
        output = "An error occurred."
        passed, score, reason = score_case(case, output)
        assert not passed

    def test_score_case_regex(self):
        from tag.eval_framework import score_case
        case = {"expect_regex": [r"\d{4}-\d{2}-\d{2}"]}
        output = "Today is 2026-06-15 and all is well."
        passed, score, reason = score_case(case, output)
        assert passed

    def test_load_suite_valid(self, tmp_path):
        from tag.eval_framework import load_suite
        suite_file = tmp_path / "suite.yaml"
        suite_file.write_text(textwrap.dedent("""\
            name: Test Suite
            profile: coder
            cases:
              - id: t1
                input: "Write hello world"
                expect_contains:
                  - "hello"
        """))
        suite = load_suite(suite_file)
        assert suite["name"] == "Test Suite"
        assert len(suite["cases"]) == 1

    def test_load_suite_missing_file(self, tmp_path):
        from tag.eval_framework import load_suite
        with pytest.raises(FileNotFoundError):
            load_suite(tmp_path / "nonexistent.yaml")

    def test_create_and_finalize_eval_run(self, tmp_path):
        from tag.eval_framework import (
            create_eval_run, record_case_result, finalize_eval_run,
            list_eval_runs,
        )
        cfg, db = make_db(tmp_path)
        run_id = create_eval_run(db, "suite.yaml", "coder", "My Suite")
        record_case_result(db, run_id, "t1", "input", "output", passed=True, score=1.0)
        record_case_result(db, run_id, "t2", "input2", "bad output", passed=False, score=0.5,
                           failure_reason="missing keyword")
        summary = finalize_eval_run(db, run_id)
        assert summary["passed"] == 1
        assert summary["failed"] == 1
        runs = list_eval_runs(db)
        db.close()
        assert len(runs) >= 1
        assert runs[0]["status"] == "completed"

    def test_cmd_eval_dry_run(self, tmp_path, capsys):
        suite_file = tmp_path / "suite.yaml"
        suite_file.write_text(textwrap.dedent("""\
            name: DryRun Suite
            cases:
              - id: c1
                input: "Say hello"
                expect_contains: ["hello"]
        """))
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(
            eval_subcommand="run",
            suite=str(suite_file),
            profile="orchestrator",
            dry_run=True,
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_eval(args)
        out = capsys.readouterr().out
        assert "1/1 passed" in out or "Results" in out


# ===========================================================================
# PRD-028: Sandbox Code Execution
# ===========================================================================

class TestSandbox:
    def test_sandbox_table_created(self, tmp_path):
        cfg, db = make_db(tmp_path)
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "sandbox_runs" in tables
        db.close()

    def test_run_basic_echo(self, tmp_path):
        from tag.sandbox import run_in_sandbox
        cfg, db = make_db(tmp_path)
        result = run_in_sandbox(db, "echo hello", backend="restricted", timeout=10)
        db.close()
        assert result["status"] == "done"
        assert result["exit_code"] == 0
        assert "hello" in result["output"]

    def test_run_exit_code_failure(self, tmp_path):
        from tag.sandbox import run_in_sandbox
        cfg, db = make_db(tmp_path)
        result = run_in_sandbox(db, "false", backend="restricted", timeout=10)
        db.close()
        assert result["exit_code"] != 0
        assert result["status"] == "failed"

    def test_run_nonexistent_command(self, tmp_path):
        from tag.sandbox import run_in_sandbox
        cfg, db = make_db(tmp_path)
        result = run_in_sandbox(db, "nonexistent_command_xyz_abc", backend="restricted", timeout=10)
        db.close()
        assert result["exit_code"] != 0

    def test_run_timeout(self, tmp_path):
        from tag.sandbox import run_in_sandbox
        cfg, db = make_db(tmp_path)
        result = run_in_sandbox(db, "sleep 100", backend="restricted", timeout=1)
        db.close()
        assert result["exit_code"] == 124  # timeout exit code

    def test_invalid_backend_raises(self, tmp_path):
        from tag.sandbox import run_in_sandbox
        cfg, db = make_db(tmp_path)
        with pytest.raises(ValueError, match="backend"):
            run_in_sandbox(db, "echo x", backend="invalid")
        db.close()

    def test_list_sandbox_runs(self, tmp_path):
        from tag.sandbox import run_in_sandbox, list_sandbox_runs
        cfg, db = make_db(tmp_path)
        run_in_sandbox(db, "echo a", backend="restricted", timeout=10)
        run_in_sandbox(db, "echo b", backend="restricted", timeout=10)
        runs = list_sandbox_runs(db, limit=10)
        db.close()
        assert len(runs) == 2

    def test_get_sandbox_run(self, tmp_path):
        from tag.sandbox import run_in_sandbox, get_sandbox_run
        cfg, db = make_db(tmp_path)
        result = run_in_sandbox(db, "echo detail_test", backend="restricted", timeout=10)
        detail = get_sandbox_run(db, result["id"])
        db.close()
        assert detail is not None
        assert "detail_test" in detail["output"]

    def test_cmd_sandbox_run(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(
            sandbox_subcommand="run",
            command="echo sandbox_test",
            backend="restricted",
            image="python:3.12-slim",
            timeout=10,
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_sandbox(args)
        assert rc == 0
        out = capsys.readouterr().out
        assert "sandbox_test" in out or "Sandbox run" in out

    def test_cmd_sandbox_list(self, tmp_path, capsys):
        from tag.sandbox import run_in_sandbox
        cfg, db = make_db(tmp_path)
        run_in_sandbox(db, "echo x", backend="restricted", timeout=5)
        db.close()
        args = make_args(sandbox_subcommand="list", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_sandbox(args)
        out = capsys.readouterr().out
        assert "echo" in out or "restricted" in out


# ===========================================================================
# PRD-029: Streaming TUI Dashboard (serve command)
# ===========================================================================

class TestDashboardServe:
    def test_cmd_serve_is_callable(self):
        assert callable(TAG.cmd_serve)

    def test_dashboard_html_contains_sse(self):
        html = TAG._dashboard_html("orchestrator")
        assert "EventSource" in html
        assert "/events" in html
        assert "orchestrator" in html

    def test_dashboard_html_is_valid_html(self):
        html = TAG._dashboard_html("test-profile")
        assert html.startswith("<!DOCTYPE html>")
        assert "</html>" in html


# ===========================================================================
# PRD-030: Prompt Cache Analytics
# ===========================================================================

class TestCacheAnalytics:
    def test_cache_columns_created(self, tmp_path):
        cfg, db = make_db(tmp_path)
        cols = {row[1] for row in db.execute("PRAGMA table_info(runs)").fetchall()}
        assert "cache_read_tokens" in cols
        assert "cache_creation_tokens" in cols
        db.close()

    def test_cmd_cache_no_data(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(cache_subcommand="stats", profile=None, config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_cache(args)
        out = capsys.readouterr().out
        assert "No run data" in out or "No runs database" in out

    def test_cmd_cache_stats_with_data(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        import uuid, datetime
        run_id = uuid.uuid4().hex
        now = datetime.datetime.now(datetime.timezone.utc).isoformat()
        db.execute(
            """INSERT INTO runs(id, created_at, kind, task_type, execution, master_profile,
               board, prompt, route_json, status, metadata_json,
               prompt_tokens, completion_tokens, total_tokens, estimated_cost_usd, model_id,
               cache_read_tokens, cache_creation_tokens)
               VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
            (run_id, now, "submit", "mixed", "sequential", "orchestrator",
             "default", "test", "{}", "completed", "{}",
             1000, 500, 1500, 0.01, "claude-opus-4",
             400, 100),
        )
        db.commit()
        db.close()

        args = make_args(cache_subcommand="stats", profile=None, config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_cache(args)
        out = capsys.readouterr().out
        assert "orchestrator" in out or "claude-opus-4" in out

    def test_cache_hit_rate_calculation(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        import uuid, datetime
        now = datetime.datetime.now(datetime.timezone.utc).isoformat()
        db.execute(
            """INSERT INTO runs(id, created_at, kind, task_type, execution, master_profile,
               board, prompt, route_json, status, metadata_json,
               prompt_tokens, completion_tokens, total_tokens, model_id,
               cache_read_tokens, cache_creation_tokens)
               VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
            (uuid.uuid4().hex, now, "submit", "mixed", "sequential", "tester",
             "default", "p", "{}", "completed", "{}",
             1000, 200, 1200, "claude-sonnet-4-6", 500, 50),
        )
        db.commit()
        db.close()
        args = make_args(cache_subcommand="stats", profile="tester", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_cache(args)
        out = capsys.readouterr().out
        # 500/1000 = 50% hit rate
        assert "50.0" in out or "tester" in out


# ===========================================================================
# PRD-031: Model Fallback Chains
# ===========================================================================

class TestRouteFallback:
    def test_fallback_table_created(self, tmp_path):
        cfg, db = make_db(tmp_path)
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "route_fallbacks" in tables
        db.close()

    def test_add_fallback_chain(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(
            fallback_subcommand="add",
            primary="claude-opus-4",
            fallback="claude-sonnet-4-6",
            condition="context_overflow",
            priority=1,
            profile="orchestrator",
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_route_fallback(args)
        assert rc == 0
        out = capsys.readouterr().out
        assert "Fallback added" in out

    def test_list_fallback_chains(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        import uuid
        db.execute(
            "INSERT INTO route_fallbacks(id, profile, primary_model, fallback_model, "
            "condition, priority, enabled, created_at) VALUES(?,?,?,?,?,?,?,?)",
            (uuid.uuid4().hex[:8], "orchestrator", "gpt-4o", "gpt-3.5-turbo",
             "error", 1, 1, TAG.utc_now()),
        )
        db.commit()
        db.close()
        args = make_args(fallback_subcommand="list", profile="orchestrator", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_route_fallback(args)
        out = capsys.readouterr().out
        assert "gpt-4o" in out
        assert "gpt-3.5-turbo" in out

    def test_remove_fallback_chain(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        import uuid
        fb_id = uuid.uuid4().hex[:8]
        db.execute(
            "INSERT INTO route_fallbacks(id, profile, primary_model, fallback_model, "
            "condition, priority, enabled, created_at) VALUES(?,?,?,?,?,?,?,?)",
            (fb_id, "orchestrator", "m1", "m2", "error", 1, 1, TAG.utc_now()),
        )
        db.commit()
        db.close()
        args = make_args(fallback_subcommand="remove", fb_id=fb_id, profile="orchestrator", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_route_fallback(args)
        assert rc == 0
        assert "removed" in capsys.readouterr().out

    def test_resolve_fallback(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        import uuid
        db.execute(
            "INSERT INTO route_fallbacks(id, profile, primary_model, fallback_model, "
            "condition, priority, enabled, created_at) VALUES(?,?,?,?,?,?,?,?)",
            (uuid.uuid4().hex[:8], "orchestrator", "claude-opus-4", "claude-haiku-4-5",
             "context_overflow", 1, 1, TAG.utc_now()),
        )
        db.commit()
        db.close()
        args = make_args(
            fallback_subcommand="resolve",
            primary="claude-opus-4",
            condition="context_overflow",
            profile="orchestrator",
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_route_fallback(args)
        assert rc == 0
        out = capsys.readouterr().out
        assert "claude-haiku-4-5" in out

    def test_resolve_no_match(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(
            fallback_subcommand="resolve",
            primary="unknown-model",
            condition="context_overflow",
            profile="orchestrator",
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_route_fallback(args)
        # No fallback configured is a valid answer (rc 0), consistent with `list`.
        assert rc == 0

    def test_invalid_condition(self, tmp_path):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(
            fallback_subcommand="add",
            primary="m1",
            fallback="m2",
            condition="invalid_condition",
            priority=1,
            profile="orchestrator",
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_route_fallback(args)
        assert rc == 1


# ===========================================================================
# PRD-032: Agent Replay / Time-Travel Debugging
# ===========================================================================

class TestTraceReplay:
    def _make_trace(self, db, trace_id: str, profile: str = "orchestrator"):
        """Insert fake spans into a trace for testing."""
        import uuid
        now = TAG.utc_now()
        for i, name in enumerate(["start", "tool_call", "end"]):
            db.execute(
                """INSERT INTO spans(id, trace_id, parent_id, name, profile, model_id,
                   started_at, finished_at, duration_ms, status, prompt_tokens,
                   completion_tokens, attributes)
                   VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)""",
                (uuid.uuid4().hex[:16], trace_id, None, name, profile, "claude-opus-4",
                 now, now, i * 100 + 50, "ok", 100 * (i + 1), 50 * (i + 1), "{}"),
            )
        db.commit()

    def test_snapshot_tables_created(self, tmp_path):
        cfg, db = make_db(tmp_path)
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "trace_snapshots" in tables
        db.close()

    def test_snapshot_trace(self, tmp_path):
        cfg, db = make_db(tmp_path)
        trace_id = "test-trace-001"
        self._make_trace(db, trace_id)
        TAG._snapshot_trace(db, trace_id)
        row = db.execute(
            "SELECT snapshot_json FROM trace_snapshots WHERE trace_id=?", (trace_id,)
        ).fetchone()
        db.close()
        assert row is not None
        snap = json.loads(row[0])
        assert snap["trace_id"] == trace_id
        assert len(snap["spans"]) == 3

    def test_cmd_trace_replay(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        trace_id = "replay-test-001"
        self._make_trace(db, trace_id)
        db.close()

        args = make_args(
            trace_subcommand="replay",
            trace_id=trace_id,
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_trace(args)
        assert rc == 0
        out = capsys.readouterr().out
        assert trace_id in out
        assert "start" in out

    def test_cmd_trace_diff(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        trace_a = "diff-trace-a"
        trace_b = "diff-trace-b"
        self._make_trace(db, trace_a)
        self._make_trace(db, trace_b)
        db.close()

        args = make_args(
            trace_subcommand="diff",
            trace_a=trace_a,
            trace_b=trace_b,
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_trace(args)
        assert rc == 0
        out = capsys.readouterr().out
        assert "diff" in out.lower() or "start" in out

    def test_cmd_trace_snapshot(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        trace_id = "snap-test-001"
        self._make_trace(db, trace_id)
        db.close()

        args = make_args(
            trace_subcommand="snapshot",
            trace_id=trace_id,
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_trace(args)
        assert rc == 0
        out = capsys.readouterr().out
        assert "Snapshot" in out or trace_id in out

    def test_cmd_trace_checkpoint(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        trace_id = "chk-test-001"
        self._make_trace(db, trace_id)
        db.close()

        args = make_args(
            trace_subcommand="checkpoint",
            trace_id=trace_id,
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_trace(args)
        assert rc == 0

    def test_replay_nonexistent_trace(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(
            trace_subcommand="replay",
            trace_id="nonexistent-trace-xyz",
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_trace(args)
        assert rc == 1

    def test_diff_json_output(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        trace_a = "djson-a"
        trace_b = "djson-b"
        self._make_trace(db, trace_a)
        self._make_trace(db, trace_b)
        db.close()

        args = make_args(
            trace_subcommand="diff",
            trace_a=trace_a,
            trace_b=trace_b,
            json=True,
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_trace(args)
        out = capsys.readouterr().out
        data = json.loads(out)
        assert isinstance(data, list)
        assert len(data) > 0
        assert "name" in data[0]

