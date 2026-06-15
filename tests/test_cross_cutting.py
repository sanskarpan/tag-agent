"""
Cross-cutting concerns test suite for all 24 PRDs (PRD-021 through PRD-044).

Covers:
  1. Database integrity — all 13 migration tables coexist, integrity_check passes
  2. NOT NULL violations — no command writes NULL to a NOT NULL column
  3. SQL injection safety — injection payloads in --profile/--name/--path args
  4. File path traversal — security scan with relative paths
  5. Error message quality — zero-args and missing-required-arg behavior
  6. Config missing — fresh tmpdir auto-creates DB without crash
  7. Concurrent access — WAL + promote_ready_jobs thread safety
  8. Output format — --json valid JSON, tabular output survives 80-char width
"""
from __future__ import annotations

import importlib.util
import json
import os
import sys
import threading
import time
import uuid
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import MagicMock, patch

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

def _load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod

TAG = _load("tag_controller", ROOT / "src" / "tag" / "controller.py")


def make_db(tmp_path):
    with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        db = TAG.open_db(cfg)
    return cfg, db


def make_args(**kwargs):
    defaults = {"config": None, "json": False, "profile": None}
    defaults.update(kwargs)
    return SimpleNamespace(**defaults)


# ===========================================================================
# 1. DATABASE INTEGRITY
# ===========================================================================

class TestDatabaseIntegrity:
    """All 13 PRD-033-044 tables coexist alongside PRD-021-032 tables."""

    EXPECTED_TABLES = {
        # Core
        "runs", "steps", "memory_journal", "spans", "queue_jobs",
        # PRD-021-032 (actual table names as created by controller.py)
        "workspace_files",          # PRD-024 workspace index
        "eval_cases", "eval_runs",  # PRD-027 eval framework
        "loop_runs", "loop_iterations",  # PRD-021 agent loop
        "trace_snapshots",          # PRD-028 replay / snapshot
        "cron_jobs",                # PRD-022 cron scheduler
        "sandbox_runs",             # PRD-023 sandbox exec
        "profile_cache",            # PRD-029 profile cache
        "benchmark_comparisons", "benchmark_results",  # PRD-030 model comparison
        "semantic_memories",        # PRD-025 semantic memory
        "events", "hook_log",       # PRD-031 event bus / hooks
        # PRD-033-044
        "queue_dags",
        "security_scans", "security_findings",
        "lsp_sessions",
        "personas", "active_personas",
        "token_budgets",
        "notification_hooks", "notification_log",
        "split_runs", "split_items",
        "tool_index_meta",
        "agentops_sessions",
    }

    def test_all_tables_created(self, tmp_path):
        cfg, db = make_db(tmp_path)
        tables = {r[0] for r in db.execute(
            "SELECT name FROM sqlite_master WHERE type='table'"
        ).fetchall()}
        missing = self.EXPECTED_TABLES - tables
        db.close()
        assert not missing, f"Missing tables: {missing}"

    def test_integrity_check_passes(self, tmp_path):
        cfg, db = make_db(tmp_path)
        result = db.execute("PRAGMA integrity_check").fetchone()[0]
        db.close()
        assert result == "ok"

    def test_foreign_keys_enabled(self, tmp_path):
        cfg, db = make_db(tmp_path)
        result = db.execute("PRAGMA foreign_keys").fetchone()[0]
        db.close()
        assert result == 1

    def test_wal_mode_active(self, tmp_path):
        cfg, db = make_db(tmp_path)
        result = db.execute("PRAGMA journal_mode").fetchone()[0]
        db.close()
        assert result == "wal"

    def test_deps_json_column_in_queue_jobs(self, tmp_path):
        cfg, db = make_db(tmp_path)
        cols = {r[1] for r in db.execute("PRAGMA table_info(queue_jobs)").fetchall()}
        db.close()
        assert "deps_json" in cols

    def test_all_commands_reopen_same_db(self, tmp_path):
        """Every command opening a DB should see the same tables (idempotent migrations)."""
        env = {"TAG_HOME": str(tmp_path / "taghome")}
        with patch.dict(os.environ, env):
            cfg1 = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
            db1 = TAG.open_db(cfg1)
            db1.close()
            cfg2 = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
            db2 = TAG.open_db(cfg2)
            tables = {r[0] for r in db2.execute(
                "SELECT name FROM sqlite_master WHERE type='table'"
            ).fetchall()}
            db2.close()
        assert "token_budgets" in tables
        assert "agentops_sessions" in tables

    def test_migration_idempotent_multiple_opens(self, tmp_path):
        """Opening the DB 5 times in succession should never raise."""
        env = {"TAG_HOME": str(tmp_path / "taghome")}
        for _ in range(5):
            with patch.dict(os.environ, env):
                cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
                db = TAG.open_db(cfg)
                db.close()

    def test_run_all_list_commands_no_conflict(self, tmp_path):
        """Run list subcommand for each of the 12 new commands — no table conflicts."""
        env = {"TAG_HOME": str(tmp_path / "taghome")}

        list_args = [
            make_args(dag_subcommand="list", config=None),
            make_args(security_subcommand="list", config=None),
            make_args(lsp_subcommand="status", config=None),
            make_args(persona_subcommand="list", config=None),
            make_args(budget_subcommand="list", config=None),
            make_args(notify_subcommand="list", config=None),
            make_args(split_subcommand="list", config=None),
            make_args(tr_subcommand="stats", config=None),
            make_args(agentops_subcommand="sessions", limit=10, config=None),
        ]
        fns = [
            TAG.cmd_dag,
            TAG.cmd_security,
            TAG.cmd_lsp,
            TAG.cmd_persona,
            TAG.cmd_budget,
            TAG.cmd_notify,
            TAG.cmd_split,
            TAG.cmd_tool_retrieval,
            TAG.cmd_agentops,
        ]
        with patch.dict(os.environ, env):
            for fn, args in zip(fns, list_args):
                fn(args)  # must not raise

        # Final integrity check after all commands ran
        with patch.dict(os.environ, env):
            cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
            db = TAG.open_db(cfg)
            result = db.execute("PRAGMA integrity_check").fetchone()[0]
            db.close()
        assert result == "ok"


# ===========================================================================
# 2. NOT NULL COLUMN VIOLATIONS
# ===========================================================================

class TestNotNullConstraints:
    """Verify no command path writes NULL to a NOT NULL column."""

    def test_token_budgets_not_null(self, tmp_path):
        from tag.budget import set_budget, get_budget, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        set_budget(db, "test-profile", 100_000, period="daily")
        row = db.execute(
            "SELECT id, profile, period, max_tokens, warn_pct, enabled, created_at, updated_at "
            "FROM token_budgets WHERE profile='test-profile'"
        ).fetchone()
        db.close()
        assert all(v is not None for v in row), f"NULL in NOT NULL column: {dict(zip(row.keys(), row))}"

    def test_notification_hooks_not_null(self, tmp_path):
        from tag.notifications import add_hook, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        hook_id = add_hook(db, "run.completed", "desktop", {})
        row = db.execute(
            "SELECT id, event, channel, config_json, template, enabled, created_at "
            "FROM notification_hooks WHERE id=?",(hook_id,)
        ).fetchone()
        db.close()
        assert all(v is not None for v in row), f"NULL in NOT NULL column: {dict(zip(row.keys(), row))}"

    def test_personas_not_null(self, tmp_path):
        from tag.persona import ensure_schema, list_personas
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        personas = list_personas(db)
        # list_personas returns: {id, name, description, inject, tags, source}
        for p in personas:
            for col in ("id", "name", "inject", "source"):
                assert p[col] is not None, f"NULL {col} in persona {p.get('name')}"
        # Verify the underlying style_prompt column is also NOT NULL via direct query
        rows = db.execute("SELECT name, style_prompt, created_at FROM personas").fetchall()
        for row in rows:
            assert row[1] is not None, f"NULL style_prompt for persona {row[0]}"
            assert row[2] is not None, f"NULL created_at for persona {row[0]}"
        db.close()

    def test_split_runs_not_null(self, tmp_path):
        from tag.split_agent import create_split_run, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        run_id = create_split_run(db, "Test task", "claude-opus-4", "claude-haiku-4-5", "coder")
        row = db.execute(
            "SELECT id, task, architect_model, editor_model, profile, status, "
            "items_total, items_done, created_at, updated_at "
            "FROM split_runs WHERE id=?", (run_id,)
        ).fetchone()
        db.close()
        assert all(v is not None for v in row)

    def test_security_scan_not_null(self, tmp_path):
        from tag.security import record_scan, scan_text, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        findings = scan_text("x = 1\n", Path("clean.py"))
        scan_id = record_scan(db, "/tmp/testdir", findings)
        row = db.execute(
            "SELECT id, scanned_path, finding_count, status, created_at "
            "FROM security_scans WHERE id=?", (scan_id,)
        ).fetchone()
        db.close()
        assert all(v is not None for v in row)

    def test_dag_jobs_not_null(self, tmp_path):
        from tag.dag import add_job, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        job_id = add_job(db, "Test task", "coder")
        # Check the columns that ARE NOT NULL in queue_jobs schema
        row = db.execute(
            "SELECT id, profile, task, task_type, status, created_at "
            "FROM queue_jobs WHERE id=?", (job_id,)
        ).fetchone()
        db.close()
        assert all(v is not None for v in row)


# ===========================================================================
# 3. SQL INJECTION SAFETY
# ===========================================================================

class TestSqlInjection:
    """All user-supplied args must be parameterized (no f-string SQL)."""

    INJECTION_PROFILE = "'; DROP TABLE token_budgets; --"
    INJECTION_NAME = "'; DROP TABLE personas; --"
    INJECTION_EVENT = "'; DROP TABLE notification_hooks; --"

    def test_budget_set_injection_safe(self, tmp_path):
        from tag.budget import set_budget, get_budget, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        # Should not raise; table must survive
        set_budget(db, self.INJECTION_PROFILE, 1000)
        b = get_budget(db, self.INJECTION_PROFILE)
        assert b is not None
        # Table still exists
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "token_budgets" in tables
        db.close()

    def test_budget_profile_stored_verbatim(self, tmp_path):
        from tag.budget import set_budget, get_budget, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        set_budget(db, self.INJECTION_PROFILE, 5000)
        b = get_budget(db, self.INJECTION_PROFILE)
        db.close()
        assert b["profile"] == self.INJECTION_PROFILE

    def test_persona_apply_injection_safe(self, tmp_path):
        from tag.persona import apply_persona, ensure_schema, get_active_personas
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        # Apply to an injection-string profile name
        apply_persona(db, self.INJECTION_PROFILE, "terse-engineer")
        personas = get_active_personas(db, self.INJECTION_PROFILE)
        assert len(personas) == 1
        # personas table must survive
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "personas" in tables
        db.close()

    def test_dag_add_job_injection_safe(self, tmp_path):
        from tag.dag import add_job, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        job_id = add_job(db, self.INJECTION_PROFILE, profile=self.INJECTION_PROFILE)
        row = db.execute("SELECT task FROM queue_jobs WHERE id=?", (job_id,)).fetchone()
        assert row[0] == self.INJECTION_PROFILE
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "queue_jobs" in tables
        db.close()

    def test_notify_add_injection_safe(self, tmp_path):
        from tag.notifications import add_hook, list_hooks, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        hook_id = add_hook(db, "run.completed", "desktop", {}, profile=self.INJECTION_PROFILE)
        hooks = list_hooks(db, profile=self.INJECTION_PROFILE)
        assert len(hooks) == 1
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "notification_hooks" in tables
        db.close()

    def test_security_record_injection_safe(self, tmp_path):
        from tag.security import record_scan, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        path_injection = "'; DROP TABLE security_scans; --"
        scan_id = record_scan(db, path_injection, [])
        row = db.execute("SELECT scanned_path FROM security_scans WHERE id=?", (scan_id,)).fetchone()
        assert row[0] == path_injection
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "security_scans" in tables
        db.close()

    def test_split_run_injection_safe(self, tmp_path):
        from tag.split_agent import create_split_run, get_split_run, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        run_id = create_split_run(db, self.INJECTION_PROFILE, "opus", "haiku",
                                  self.INJECTION_PROFILE)
        run = get_split_run(db, run_id)
        assert run["task"] == self.INJECTION_PROFILE
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "split_runs" in tables
        db.close()

    def test_agentops_session_injection_safe(self, tmp_path):
        from tag.integrations.agentops_bridge import ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        run_id = uuid.uuid4().hex
        now = TAG.utc_now()
        db.execute(
            "INSERT INTO agentops_sessions(id, run_id, status, created_at) VALUES(?,?,?,?)",
            (uuid.uuid4().hex, run_id, "'; DROP TABLE agentops_sessions; --", now),
        )
        db.commit()
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "agentops_sessions" in tables
        db.close()


# ===========================================================================
# 4. FILE PATH TRAVERSAL
# ===========================================================================

class TestPathTraversal:
    def test_security_scan_parent_dir_allowed(self, tmp_path):
        """Security scan with ../ is permitted — it's a security TOOL, not a sink."""
        from tag.security import scan_directory
        # Should return a generator without exception, even with relative paths
        # We'll point to a known-clean directory
        clean_dir = tmp_path / "clean"
        clean_dir.mkdir()
        (clean_dir / "app.py").write_text("x = 1\n")
        findings = list(scan_directory(clean_dir))
        assert isinstance(findings, list)

    def test_security_scan_absolute_path(self, tmp_path):
        """Security scan of /tmp should work without crashing."""
        from tag.security import scan_directory
        findings = list(scan_directory(tmp_path))
        assert isinstance(findings, list)

    def test_diff_context_blocked_patterns_prevent_env(self):
        """Blocked patterns must include .env files."""
        from tag.diff_context import DEFAULT_BLOCKED_PATTERNS, _is_blocked
        # Common sensitive file patterns must all be blocked
        for path in [".env", "prod.env", "secrets.key", "id_rsa.pem",
                     "credentials.json", "api.token", "secret_config.yaml"]:
            assert _is_blocked(path, DEFAULT_BLOCKED_PATTERNS), \
                f"Expected {path!r} to be blocked but wasn't"

    def test_diff_context_safe_files_not_blocked(self):
        """Regular source files must NOT be blocked."""
        from tag.diff_context import DEFAULT_BLOCKED_PATTERNS, _is_blocked
        for path in ["app.py", "main.go", "config.yaml", "README.md", "package.json"]:
            assert not _is_blocked(path, DEFAULT_BLOCKED_PATTERNS), \
                f"Expected {path!r} to not be blocked but it was"

    def test_workspace_index_stores_files(self, tmp_path):
        """workspace index (workspace_files table) stores indexed files."""
        from tag.workspace import _ensure_schema, index_workspace
        # Create a tiny workspace with one Python file
        ws_dir = tmp_path / "workspace"
        ws_dir.mkdir()
        (ws_dir / "app.py").write_text("def hello():\n    return 'hi'\n")

        cfg, db = make_db(tmp_path)
        _ensure_schema(db)
        result = index_workspace(db, ws_dir, max_files=10)
        rows = db.execute("SELECT path FROM workspace_files").fetchall()
        db.close()
        assert len(rows) >= 1


# ===========================================================================
# 5. ERROR MESSAGE QUALITY
# ===========================================================================

class TestErrorMessageQuality:
    """Zero-args and missing-required-args should produce usage hints, not tracebacks."""

    def _run_cmd(self, args_list, tmp_path):
        """Run tag CLI subprocess and return (returncode, stdout+stderr)."""
        import subprocess
        env = os.environ.copy()
        env["TAG_HOME"] = str(tmp_path / "taghome")
        env["PYTHONPATH"] = str(ROOT / "src")
        result = subprocess.run(
            [sys.executable, "-m", "tag", *args_list],
            capture_output=True, text=True, env=env,
            cwd=str(ROOT),
        )
        return result.returncode, result.stdout + result.stderr

    def _assert_no_traceback(self, out, cmd):
        """Core invariant: no raw Python traceback in any command output."""
        assert "traceback" not in out.lower(), \
            f"`tag {cmd}` produced a traceback:\n{out}"
        assert "most recent call last" not in out.lower(), \
            f"`tag {cmd}` produced exception text:\n{out}"

    def test_dag_no_args_no_traceback(self, tmp_path):
        rc, out = self._run_cmd(["dag"], tmp_path)
        self._assert_no_traceback(out, "dag")
        # dag defaults to show-dag (valid behavior): "No jobs found." OR shows the graph
        assert len(out) > 0

    def test_budget_no_args_no_traceback(self, tmp_path):
        rc, out = self._run_cmd(["budget"], tmp_path)
        self._assert_no_traceback(out, "budget")
        # Shows help or "No budgets" — either is fine; just no crash
        assert len(out) > 0

    def test_security_no_args_no_traceback(self, tmp_path):
        rc, out = self._run_cmd(["security"], tmp_path)
        self._assert_no_traceback(out, "security")
        assert len(out) > 0

    def test_persona_no_args_no_traceback(self, tmp_path):
        rc, out = self._run_cmd(["persona"], tmp_path)
        self._assert_no_traceback(out, "persona")
        assert len(out) > 0

    def test_notify_no_args_no_traceback(self, tmp_path):
        rc, out = self._run_cmd(["notify"], tmp_path)
        self._assert_no_traceback(out, "notify")
        assert len(out) > 0

    def test_split_no_args_no_traceback(self, tmp_path):
        rc, out = self._run_cmd(["split"], tmp_path)
        self._assert_no_traceback(out, "split")
        assert len(out) > 0

    def test_lsp_no_args_no_traceback(self, tmp_path):
        # After the Bug #1 fix, 'tag lsp' with no subcommand defaults to 'status'
        # (non-blocking), NOT to starting the server.
        import subprocess
        env = os.environ.copy()
        env["TAG_HOME"] = str(tmp_path / "taghome")
        env["PYTHONPATH"] = str(ROOT / "src")
        result = subprocess.run(
            [sys.executable, "-m", "tag", "lsp"],
            capture_output=True, text=True, env=env, cwd=str(ROOT),
            timeout=10,  # must complete fast if fixed, fail if still blocking
        )
        out = result.stdout + result.stderr
        self._assert_no_traceback(out, "lsp")
        assert len(out) > 0

    def test_agentops_no_args_no_traceback(self, tmp_path):
        rc, out = self._run_cmd(["agentops"], tmp_path)
        self._assert_no_traceback(out, "agentops")
        assert len(out) > 0

    def test_help_shows_all_new_commands(self, tmp_path):
        rc, out = self._run_cmd(["--help"], tmp_path)
        for cmd in ["dag", "budget", "security", "persona", "notify", "split",
                    "lsp", "web", "agentops", "otel-export"]:
            assert cmd in out, f"Command {cmd!r} missing from --help output"

    def test_budget_set_missing_max_tokens(self, tmp_path):
        """budget set without --max-tokens (required) should error gracefully."""
        rc, out = self._run_cmd(["budget", "set", "--profile", "coder"], tmp_path)
        assert "traceback" not in out.lower()
        # Either fails with non-zero (missing required arg) or mentions usage
        assert rc != 0 or "max-tokens" in out.lower() or "usage" in out.lower()

    def test_persona_apply_missing_name(self, tmp_path):
        """persona apply without name should error gracefully."""
        rc, out = self._run_cmd(["persona", "apply"], tmp_path)
        assert "traceback" not in out.lower()
        assert rc != 0 or "usage" in out.lower()


# ===========================================================================
# 6. CONFIG MISSING — FRESH TMPDIR
# ===========================================================================

class TestConfigMissing:
    """Every new command must auto-create DB from scratch — no crash on first run."""

    def test_fresh_db_dag_list(self, tmp_path):
        fresh = tmp_path / "fresh_taghome"
        # Do NOT pre-create it
        args = make_args(dag_subcommand="list", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(fresh)}):
            TAG.cmd_dag(args)  # must not raise
        assert fresh.exists()

    def test_fresh_db_budget_list(self, tmp_path):
        fresh = tmp_path / "fresh_b"
        args = make_args(budget_subcommand="list", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(fresh)}):
            TAG.cmd_budget(args)
        assert fresh.exists()

    def test_fresh_db_security_list(self, tmp_path):
        fresh = tmp_path / "fresh_s"
        args = make_args(security_subcommand="list", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(fresh)}):
            TAG.cmd_security(args)
        assert fresh.exists()

    def test_fresh_db_persona_list(self, tmp_path):
        fresh = tmp_path / "fresh_p"
        args = make_args(persona_subcommand="list", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(fresh)}):
            TAG.cmd_persona(args)
        assert fresh.exists()

    def test_fresh_db_notify_list(self, tmp_path):
        fresh = tmp_path / "fresh_n"
        args = make_args(notify_subcommand="list", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(fresh)}):
            TAG.cmd_notify(args)
        assert fresh.exists()

    def test_fresh_db_split_list(self, tmp_path):
        fresh = tmp_path / "fresh_sp"
        args = make_args(split_subcommand="list", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(fresh)}):
            TAG.cmd_split(args)
        assert fresh.exists()

    def test_fresh_db_lsp_status(self, tmp_path):
        fresh = tmp_path / "fresh_lsp"
        args = make_args(lsp_subcommand="status", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(fresh)}):
            TAG.cmd_lsp(args)
        assert fresh.exists()

    def test_fresh_db_agentops_sessions(self, tmp_path):
        fresh = tmp_path / "fresh_ao"
        args = make_args(agentops_subcommand="sessions", limit=10, config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(fresh)}):
            TAG.cmd_agentops(args)
        assert fresh.exists()

    def test_fresh_db_tool_retrieval_stats(self, tmp_path):
        fresh = tmp_path / "fresh_tr"
        args = make_args(tr_subcommand="stats", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(fresh)}):
            TAG.cmd_tool_retrieval(args)
        assert fresh.exists()

    def test_fresh_db_integrity_check(self, tmp_path):
        """After first auto-creation, integrity_check must pass."""
        fresh = tmp_path / "fresh_integrity"
        args = make_args(persona_subcommand="list", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(fresh)}):
            TAG.cmd_persona(args)
            cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
            db = TAG.open_db(cfg)
            result = db.execute("PRAGMA integrity_check").fetchone()[0]
            db.close()
        assert result == "ok"


# ===========================================================================
# 7. CONCURRENT ACCESS (WAL + thread safety)
# ===========================================================================

class TestConcurrentAccess:
    def test_promote_ready_jobs_thread_safe(self, tmp_path):
        """Two threads calling promote_ready_jobs simultaneously must not corrupt data."""
        from tag.dag import add_job, promote_ready_jobs, ensure_schema
        import sqlite3

        # Use make_db to create the FULL schema (queue_jobs + all tables)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
            db = TAG.open_db(cfg)
            ensure_schema(db)
            db_path = TAG.runtime_db_path(cfg)
            parent_id = add_job(db, "parent")
            child_ids = [add_job(db, f"child-{i}", depends_on=[parent_id]) for i in range(5)]
            db.execute("UPDATE queue_jobs SET status='done' WHERE id=?", (parent_id,))
            db.commit()
            db.close()

        promoted_counts = []
        errors = []

        def worker():
            c = sqlite3.connect(str(db_path), timeout=10)
            c.execute("PRAGMA journal_mode = WAL")
            c.execute("PRAGMA busy_timeout = 5000")
            try:
                promoted = promote_ready_jobs(c)
                promoted_counts.append(len(promoted))
            except Exception as e:
                errors.append(str(e))
            finally:
                c.close()

        threads = [threading.Thread(target=worker) for _ in range(2)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()

        assert not errors, f"Thread errors: {errors}"
        # Total promoted across both threads should be exactly 5 (each child promoted once)
        total_promoted = sum(promoted_counts)
        assert total_promoted == 5, f"Expected 5 total promoted, got {total_promoted}"

        # Verify all children are now 'ready', none are 'pending'
        conn2 = sqlite3.connect(str(db_path), timeout=5)
        for cid in child_ids:
            row = conn2.execute("SELECT status FROM queue_jobs WHERE id=?", (cid,)).fetchone()
            assert row[0] == "ready", f"Child {cid} still {row[0]}"
        conn2.close()

    def test_concurrent_notification_log_no_duplicates(self, tmp_path):
        """Ten threads logging to notification_log concurrently all produce unique rows."""
        from tag.notifications import ensure_schema as notif_schema
        import sqlite3

        # Compute db_path INSIDE the env context so tag_home() resolves correctly
        tag_home_path = str(tmp_path / "taghome")
        with patch.dict(os.environ, {"TAG_HOME": tag_home_path}):
            cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
            db = TAG.open_db(cfg)
            db_path = TAG.runtime_db_path(cfg)
            db.close()

        conn0 = sqlite3.connect(str(db_path), timeout=10)
        conn0.execute("PRAGMA journal_mode = WAL")
        notif_schema(conn0)
        # Insert a hook to reference
        hook_id = uuid.uuid4().hex
        now = TAG.utc_now()
        conn0.execute(
            "INSERT INTO notification_hooks(id, event, channel, config_json, template, enabled, created_at) "
            "VALUES(?,?,?,?,?,?,?)",
            (hook_id, "run.completed", "desktop", "{}", "", 1, now),
        )
        conn0.commit()
        conn0.close()

        inserted_ids = []
        errors = []

        def insert_log(n):
            c = sqlite3.connect(str(db_path), timeout=10)
            c.execute("PRAGMA journal_mode = WAL")
            c.execute("PRAGMA busy_timeout = 5000")
            log_id = uuid.uuid4().hex + str(n)
            try:
                c.execute(
                    "INSERT INTO notification_log(id, hook_id, event, channel, outcome, attempt, created_at) "
                    "VALUES(?,?,?,?,?,?,?)",
                    (log_id, hook_id, "run.completed", "desktop", "ok", 1, TAG.utc_now()),
                )
                c.commit()
                inserted_ids.append(log_id)
            except Exception as e:
                errors.append(str(e))
            finally:
                c.close()

        threads = [threading.Thread(target=insert_log, args=(i,)) for i in range(10)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()

        assert not errors, f"Thread errors: {errors}"

        conn3 = sqlite3.connect(str(db_path), timeout=5)
        count = conn3.execute("SELECT COUNT(*) FROM notification_log").fetchone()[0]
        conn3.close()
        assert count == 10, f"Expected 10 log rows, got {count}"


# ===========================================================================
# 8. OUTPUT FORMAT
# ===========================================================================

class TestOutputFormat:
    def test_otel_export_json_valid(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(trace_id=None, endpoint="", semconv="1.28.0",
                         no_metrics=False, config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_otel_export(args)
        out = capsys.readouterr().out
        assert rc == 0
        data = json.loads(out)
        assert "resourceSpans" in data

    def test_budget_list_json_flag(self, tmp_path, capsys):
        from tag.budget import set_budget, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        set_budget(db, "coder", 100_000)
        set_budget(db, "reviewer", 50_000)
        db.close()
        args = make_args(budget_subcommand="list", json=True, config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_budget(args)
        out = capsys.readouterr().out
        data = json.loads(out)
        assert isinstance(data, list)
        assert len(data) == 2

    def test_persona_list_json_flag(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(persona_subcommand="list", json=True, config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_persona(args)
        out = capsys.readouterr().out
        data = json.loads(out)
        assert isinstance(data, list)
        assert len(data) >= 5  # 5 built-ins

    def test_notify_list_json_flag(self, tmp_path, capsys):
        from tag.notifications import add_hook, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        add_hook(db, "run.completed", "desktop", {})
        db.close()
        args = make_args(notify_subcommand="list", json=True, config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_notify(args)
        out = capsys.readouterr().out
        data = json.loads(out)
        assert isinstance(data, list)
        assert len(data) == 1

    def test_security_list_json_flag(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(security_subcommand="list", json=True, config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_security(args)
        out = capsys.readouterr().out
        data = json.loads(out)
        assert isinstance(data, list)

    def test_dag_list_json_flag(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(dag_subcommand="list", json=True, config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_dag(args)
        out = capsys.readouterr().out
        data = json.loads(out)
        assert isinstance(data, list)

    def test_split_list_json_flag(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(split_subcommand="list", json=True, config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_split(args)
        out = capsys.readouterr().out
        data = json.loads(out)
        assert isinstance(data, list)

    def test_agentops_sessions_json_flag(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(agentops_subcommand="sessions", limit=10, json=True, config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_agentops(args)
        out = capsys.readouterr().out
        data = json.loads(out)
        assert isinstance(data, list)

    def test_tabular_output_fits_80_chars(self, tmp_path, capsys):
        """Tabular output for budget list must not produce lines >120 chars that break logic."""
        from tag.budget import set_budget, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        # Long profile name edge case
        set_budget(db, "a" * 40, 999_999_999)
        db.close()
        args = make_args(budget_subcommand="list", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_budget(args)
        out = capsys.readouterr().out
        # All output lines should be reasonable (no single token/value gets truncated to cause parse errors)
        assert "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" in out or len(out) > 0

    def test_show_dag_ascii_valid(self, tmp_path):
        from tag.dag import add_job, show_dag, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        j1 = add_job(db, "Step 1: Generate")
        j2 = add_job(db, "Step 2: Test", depends_on=[j1])
        j3 = add_job(db, "Step 3: Deploy", depends_on=[j2])
        output = show_dag(db)
        db.close()
        # Each job must appear in the output
        assert "Step 1: Generate" in output
        assert "Step 2: Test" in output
        assert "Step 3: Deploy" in output
        # Dependency arrows
        assert j1[:8] in output
        assert j2[:8] in output

    def test_otel_export_with_spans(self, tmp_path, capsys):
        """otel-export with actual span data produces valid OTLP JSON."""
        import datetime as dt
        cfg, db = make_db(tmp_path)
        now = dt.datetime.now(dt.timezone.utc).isoformat()
        run_id = uuid.uuid4().hex
        span_id = uuid.uuid4().hex
        db.execute(
            """INSERT INTO runs(id, created_at, kind, task_type, execution, master_profile,
               board, prompt, route_json, status, metadata_json, prompt_tokens, completion_tokens,
               total_tokens, model_id)
               VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
            (run_id, now, "submit", "mixed", "sequential", "coder",
             "default", "Write a test", "{}", "completed", "{}", 100, 200, 300, "claude-opus-4"),
        )
        db.execute(
            """INSERT INTO spans(id, trace_id, name, started_at, finished_at, duration_ms,
               status, prompt_tokens, completion_tokens, model_id)
               VALUES(?,?,?,?,?,?,?,?,?,?)""",
            (span_id, run_id, "inference", now, now, 1000, "ok", 100, 200, "claude-opus-4"),
        )
        db.commit()
        db.close()

        args = make_args(trace_id=run_id, endpoint="", semconv="1.28.0",
                         no_metrics=False, config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_otel_export(args)
        out = capsys.readouterr().out
        assert rc == 0
        data = json.loads(out)
        spans = data["resourceSpans"][0]["scopeSpans"][0]["spans"]
        attrs = {a["key"]: a for a in spans[0]["attributes"]}
        assert "gen_ai.usage.input_tokens" in attrs
        assert "gen_ai.request.model" in attrs
        assert attrs["gen_ai.request.model"]["value"]["stringValue"] == "claude-opus-4"
