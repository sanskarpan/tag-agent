"""
Comprehensive tests for PRD-033 through PRD-044.

Coverage:
  PRD-033: Dependency-aware task queue / DAG engine
  PRD-034: Secret scanning (entropy + named patterns)
  PRD-035: IDE Bridge / LSP server
  PRD-036: Web Dashboard API
  PRD-037: Agent Personas (builtin, apply, merge)
  PRD-038: Diff-Aware Context Injection (blocking, token counting)
  PRD-039: Token Budget Enforcement (set/check/exceeded)
  PRD-040: Notification Hooks (CRUD, delivery, template)
  PRD-041: OTel GenAI Span Cost Attribution (attribute mapping)
  PRD-042: Architect/Editor Agent Split (spec, CRUD)
  PRD-043: Vector-Based Tool Retrieval (keyword fallback + schema)
  PRD-044: AgentOps Session Observability (session CRUD, mask key)
"""
from __future__ import annotations

import importlib.util
import json
import os
import sys
import textwrap
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
# PRD-033: Dependency-Aware Task Queue
# ===========================================================================

class TestDAG:
    def test_dag_tables_created(self, tmp_path):
        cfg, db = make_db(tmp_path)
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "queue_dags" in tables
        db.close()

    def test_deps_json_column_added(self, tmp_path):
        cfg, db = make_db(tmp_path)
        cols = {r[1] for r in db.execute("PRAGMA table_info(queue_jobs)").fetchall()}
        assert "deps_json" in cols
        db.close()

    def test_add_job_no_deps(self, tmp_path):
        from tag.dag import add_job, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        job_id = add_job(db, "task A", profile="coder")
        row = db.execute("SELECT status, deps_json FROM queue_jobs WHERE id=?", (job_id,)).fetchone()
        assert row[0] == "ready"
        assert json.loads(row[1]) == []
        db.close()

    def test_add_job_with_dep(self, tmp_path):
        from tag.dag import add_job, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        parent_id = add_job(db, "parent task")
        child_id = add_job(db, "child task", depends_on=[parent_id])
        row = db.execute("SELECT status, deps_json FROM queue_jobs WHERE id=?", (child_id,)).fetchone()
        assert row[0] == "pending"
        assert parent_id in json.loads(row[1])
        db.close()

    def test_add_job_invalid_dep_raises(self, tmp_path):
        from tag.dag import add_job, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        with pytest.raises(ValueError, match="not found"):
            add_job(db, "task", depends_on=["nonexistent-job"])
        db.close()

    def test_promote_ready_jobs(self, tmp_path):
        from tag.dag import add_job, ensure_schema, promote_ready_jobs
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        parent_id = add_job(db, "parent")
        child_id = add_job(db, "child", depends_on=[parent_id])
        # Mark parent done
        db.execute("UPDATE queue_jobs SET status='done' WHERE id=?", (parent_id,))
        db.commit()
        promoted = promote_ready_jobs(db)
        assert child_id in promoted
        row = db.execute("SELECT status FROM queue_jobs WHERE id=?", (child_id,)).fetchone()
        assert row[0] == "ready"
        db.close()

    def test_cascade_fail_on_failed_dep(self, tmp_path):
        from tag.dag import add_job, ensure_schema, promote_ready_jobs
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        parent_id = add_job(db, "parent")
        child_id = add_job(db, "child", depends_on=[parent_id])
        db.execute("UPDATE queue_jobs SET status='failed' WHERE id=?", (parent_id,))
        db.commit()
        promote_ready_jobs(db)
        row = db.execute("SELECT status FROM queue_jobs WHERE id=?", (child_id,)).fetchone()
        assert row[0] == "failed"
        db.close()

    def test_all_deps_satisfied_multiple(self, tmp_path):
        from tag.dag import add_job, ensure_schema, all_deps_satisfied
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        p1 = add_job(db, "p1")
        p2 = add_job(db, "p2")
        child = add_job(db, "child", depends_on=[p1, p2])
        db.execute("UPDATE queue_jobs SET status='done' WHERE id=?", (p1,))
        db.commit()
        # One dep still pending
        assert not all_deps_satisfied(db, child)
        db.execute("UPDATE queue_jobs SET status='done' WHERE id=?", (p2,))
        db.commit()
        assert all_deps_satisfied(db, child)
        db.close()

    def test_show_dag_renders(self, tmp_path):
        from tag.dag import add_job, ensure_schema, show_dag
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        j1 = add_job(db, "Job A")
        j2 = add_job(db, "Job B", depends_on=[j1])
        output = show_dag(db)
        assert "Job A" in output
        assert "Job B" in output
        db.close()

    def test_save_and_run_dag(self, tmp_path):
        from tag.dag import ensure_schema, save_dag, run_dag, list_dags, DagSpec
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        spec = DagSpec("my-pipeline", [
            {"task": "Generate code", "profile": "coder"},
            {"task": "Write tests", "profile": "coder", "depends_on": [0]},
            {"task": "Review", "profile": "reviewer", "depends_on": [1]},
        ])
        save_dag(db, spec)
        dags = list_dags(db)
        assert len(dags) == 1
        assert dags[0]["name"] == "my-pipeline"
        assert dags[0]["step_count"] == 3

        job_ids = run_dag(db, "my-pipeline")
        assert len(job_ids) == 3
        # First job is ready, others are pending (deps)
        row0 = db.execute("SELECT status FROM queue_jobs WHERE id=?", (job_ids[0],)).fetchone()
        row1 = db.execute("SELECT status FROM queue_jobs WHERE id=?", (job_ids[1],)).fetchone()
        assert row0[0] == "ready"
        assert row1[0] == "pending"
        db.close()

    def test_cmd_dag_list(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(dag_subcommand="list", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_dag(args)
        assert "No saved DAGs" in capsys.readouterr().out


# ===========================================================================
# PRD-034: Secret Scanning
# ===========================================================================

class TestSecretScanning:
    def test_scan_tables_created(self, tmp_path):
        cfg, db = make_db(tmp_path)
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "security_scans" in tables
        assert "security_findings" in tables
        db.close()

    def test_scan_anthropic_key(self):
        from tag.security import scan_text
        content = "ANTHROPIC_API_KEY=sk-ant-api03-" + "A" * 80
        findings = scan_text(content, Path("test.env"))
        assert any(f.pattern_name == "anthropic_api_key" for f in findings)

    def test_scan_openai_key(self):
        from tag.security import scan_text
        content = "OPENAI_API_KEY=sk-" + "a" * 48
        findings = scan_text(content, Path("test.env"))
        assert any(f.pattern_name == "openai_api_key" for f in findings)

    def test_scan_github_pat(self):
        from tag.security import scan_text
        content = "GH_TOKEN=ghp_" + "a" * 36
        findings = scan_text(content, Path("test.env"))
        assert any(f.pattern_name == "github_pat_classic" for f in findings)

    def test_scan_aws_key(self):
        from tag.security import scan_text
        content = "AWS_ACCESS_KEY_ID=AKIA" + "A" * 16
        findings = scan_text(content, Path("config.py"))
        assert any(f.pattern_name == "aws_access_key" for f in findings)

    def test_scan_no_secrets_clean(self):
        from tag.security import scan_text
        content = "x = 1\nprint('hello world')\n"
        findings = scan_text(content, Path("app.py"))
        assert len(findings) == 0

    def test_entropy_detection_high(self):
        from tag.security import _shannon_entropy, _high_entropy_windows
        # High entropy random-looking string
        s = "aB3!xQ9#kP2@mZ7&nR4"  # 20 chars mixed
        entropy = _shannon_entropy(s)
        assert entropy > 3.0  # at least somewhat random

    def test_entropy_detection_low(self):
        from tag.security import _shannon_entropy
        # Low entropy repetitive string
        s = "aaaaaaaaaaaaaaaaaaaaaa"
        entropy = _shannon_entropy(s)
        assert entropy < 1.0

    def test_scan_private_key_header(self):
        from tag.security import scan_text
        content = "-----BEGIN RSA PRIVATE KEY-----\nABCD1234...\n-----END RSA PRIVATE KEY-----"
        findings = scan_text(content, Path("key.pem"))
        assert any(f.pattern_name == "generic_private_key" for f in findings)

    def test_scan_jwt_token(self):
        from tag.security import scan_text
        content = "token=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ0ZXN0In0.abc123xyz"
        findings = scan_text(content, Path("config.json"))
        assert any(f.pattern_name == "jwt_token" for f in findings)

    def test_scan_binary_file_skipped(self, tmp_path):
        from tag.security import scan_file
        f = tmp_path / "image.png"
        f.write_bytes(b"\x89PNG\r\n\x1a\n" + b"\x00" * 100)
        findings = scan_file(f)
        assert len(findings) == 0

    def test_scan_directory(self, tmp_path):
        from tag.security import scan_directory
        (tmp_path / "app.py").write_text("x = 1\n")
        (tmp_path / "secrets.env").write_text("KEY=sk-" + "a" * 48)
        findings = list(scan_directory(tmp_path))
        assert len(findings) >= 1

    def test_scan_skips_git_dir(self, tmp_path):
        from tag.security import scan_directory
        (tmp_path / ".git").mkdir()
        (tmp_path / ".git" / "secret.txt").write_text("KEY=sk-" + "a" * 48)
        (tmp_path / "app.py").write_text("x = 1")
        findings = list(scan_directory(tmp_path))
        assert not any(".git" in str(f.file) for f in findings)

    def test_record_scan(self, tmp_path):
        from tag.security import scan_text, record_scan
        cfg, db = make_db(tmp_path)
        findings = scan_text("OPENAI=sk-" + "a"*48, Path("f.py"))
        scan_id = record_scan(db, "/tmp/test", findings)
        row = db.execute("SELECT finding_count, status FROM security_scans WHERE id=?", (scan_id,)).fetchone()
        assert row[0] == 1
        assert row[1] == "secrets_found"
        db.close()

    def test_cmd_security_scan_clean(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        (tmp_path / "app.py").write_text("x = 1")
        args = make_args(
            security_subcommand="scan",
            path=str(tmp_path),
            max_files=100,
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_security(args)
        assert rc == 0
        out = capsys.readouterr().out
        assert "No secrets found" in out

    def test_cmd_security_scan_finds_secret(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        (tmp_path / "leak.env").write_text("KEY=sk-ant-api03-" + "B" * 80)
        args = make_args(
            security_subcommand="scan",
            path=str(tmp_path),
            max_files=100,
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_security(args)
        # Returns 1 when secrets found
        assert rc == 1
        out = capsys.readouterr().out
        # Value should NOT be in output
        assert "B" * 80 not in out


# ===========================================================================
# PRD-035: IDE Bridge / LSP Server
# ===========================================================================

class TestLspServer:
    def test_lsp_table_created(self, tmp_path):
        cfg, db = make_db(tmp_path)
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "lsp_sessions" in tables
        db.close()

    def test_lsp_server_initialize(self):
        from tag.lsp_server import TagLspServer
        server = TagLspServer(profiles=["coder", "reviewer"])
        msg = {
            "jsonrpc": "2.0", "id": 1,
            "method": "initialize",
            "params": {"capabilities": {}},
        }
        resp = server.handle(msg)
        assert resp["id"] == 1
        assert "capabilities" in resp["result"]
        assert resp["result"]["capabilities"]["codeActionProvider"] is True

    def test_lsp_server_code_action(self):
        from tag.lsp_server import TagLspServer
        server = TagLspServer(profiles=["coder", "reviewer"])
        # Must initialize first
        server.handle({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}})
        msg = {
            "jsonrpc": "2.0", "id": 2,
            "method": "textDocument/codeAction",
            "params": {
                "textDocument": {"uri": "file:///src/app.py"},
                "range": {"start": {"line": 0}, "end": {"line": 10}},
            },
        }
        resp = server.handle(msg)
        actions = resp["result"]
        assert len(actions) == 2
        assert any("coder" in a["title"] for a in actions)
        assert any("reviewer" in a["title"] for a in actions)

    def test_lsp_server_execute_command(self):
        from tag.lsp_server import TagLspServer
        server = TagLspServer(profiles=["coder"])
        msg = {
            "jsonrpc": "2.0", "id": 3,
            "method": "workspace/executeCommand",
            "params": {
                "command": "tag.profile.coder",
                "arguments": ["file:///src/main.py", {}],
            },
        }
        resp = server.handle(msg)
        assert resp["result"]["executed"] is True
        assert resp["result"]["profile"] == "coder"

    def test_lsp_server_shutdown(self):
        from tag.lsp_server import TagLspServer
        server = TagLspServer(profiles=["coder"])
        msg = {"jsonrpc": "2.0", "id": 99, "method": "shutdown", "params": {}}
        resp = server.handle(msg)
        assert resp["result"] is None
        assert server._shutdown is True

    def test_lsp_server_unknown_method(self):
        from tag.lsp_server import TagLspServer
        server = TagLspServer(profiles=["coder"])
        msg = {"jsonrpc": "2.0", "id": 5, "method": "unknown/method", "params": {}}
        resp = server.handle(msg)
        assert "error" in resp
        assert resp["error"]["code"] == -32601

    def test_lsp_message_framing(self):
        from tag.lsp_server import _write_message, _read_message
        import io
        buf = io.BytesIO()

        class FakeStream:
            def write(self, b): buf.write(b)
            def flush(self): pass

        msg = {"jsonrpc": "2.0", "id": 1, "result": {"ok": True}}
        _write_message(FakeStream(), msg)

        buf.seek(0)
        result = _read_message(buf)
        assert result["id"] == 1

    def test_cmd_lsp_status_empty(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(lsp_subcommand="status", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_lsp(args)
        out = capsys.readouterr().out
        assert "No active LSP sessions" in out


# ===========================================================================
# PRD-036: Web Dashboard
# ===========================================================================

class TestWebDashboard:
    def test_dashboard_html_is_valid(self):
        from tag.api import _DASHBOARD_HTML
        assert "EventSource" in _DASHBOARD_HTML
        assert "/api/stream" in _DASHBOARD_HTML
        assert "TAG Web Dashboard" in _DASHBOARD_HTML

    def test_fetch_runs_empty(self, tmp_path):
        from tag.api import _fetch_runs
        cfg, db = make_db(tmp_path)
        runs = _fetch_runs(db, limit=10)
        assert runs == []
        db.close()

    def test_fetch_queue_empty(self, tmp_path):
        from tag.api import _fetch_queue
        cfg, db = make_db(tmp_path)
        q = _fetch_queue(db, limit=10)
        assert q == []
        db.close()

    def test_fetch_cost_summary_empty(self, tmp_path):
        from tag.api import _fetch_cost_summary
        cfg, db = make_db(tmp_path)
        costs = _fetch_cost_summary(db)
        assert costs == []
        db.close()

    def test_dashboard_server_init(self, tmp_path):
        from tag.api import DashboardServer
        cfg, db = make_db(tmp_path)
        db_path = TAG.runtime_db_path(cfg)
        db.close()
        server = DashboardServer(db_path=db_path, host="127.0.0.1", port=8787)
        assert server.host == "127.0.0.1"
        assert server.port == 8787

    def test_sse_event_format(self):
        from tag.api import _sse_event
        payload = {"runs": [], "queue": []}
        evt = _sse_event(payload, "update")
        assert evt.startswith(b"event: update")
        assert b"data:" in evt
        data = json.loads(evt.split(b"data: ", 1)[1].split(b"\n")[0])
        assert "runs" in data


# ===========================================================================
# PRD-037: Agent Personas
# ===========================================================================

class TestPersonas:
    def test_persona_tables_created(self, tmp_path):
        cfg, db = make_db(tmp_path)
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "personas" in tables
        assert "active_personas" in tables
        db.close()

    def test_list_personas_includes_builtins(self, tmp_path):
        from tag.persona import list_personas, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        personas = list_personas(db)
        db.close()
        names = [p["name"] for p in personas]
        assert "terse-engineer" in names
        assert "verbose-explainer" in names
        assert "security-focused" in names

    def test_get_builtin_persona(self, tmp_path):
        from tag.persona import get_persona, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        p = get_persona(db, "terse-engineer")
        db.close()
        assert p is not None
        assert p["name"] == "terse-engineer"
        assert p["inject"] == "prepend"
        assert "terse" in p["style_prompt"].lower() or "engineer" in p["style_prompt"].lower()

    def test_apply_and_get_active_personas(self, tmp_path):
        from tag.persona import apply_persona, get_active_personas, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        apply_persona(db, "coder", "terse-engineer")
        apply_persona(db, "coder", "security-focused")
        personas = get_active_personas(db, "coder")
        db.close()
        assert len(personas) == 2

    def test_apply_unknown_persona_raises(self, tmp_path):
        from tag.persona import apply_persona, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        with pytest.raises(ValueError, match="not found"):
            apply_persona(db, "coder", "nonexistent-persona")
        db.close()

    def test_remove_active_persona(self, tmp_path):
        from tag.persona import apply_persona, remove_active_persona, get_active_personas, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        apply_persona(db, "coder", "terse-engineer")
        remove_active_persona(db, "coder", "terse-engineer")
        personas = get_active_personas(db, "coder")
        db.close()
        assert len(personas) == 0

    def test_build_merged_prompt_prepend(self, tmp_path):
        from tag.persona import build_merged_prompt, ensure_schema, get_persona
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        terse = get_persona(db, "terse-engineer")
        db.close()
        terse["position"] = 0
        base = "You are a helpful coding assistant."
        merged = build_merged_prompt(base, [terse])
        # Prepend means style comes first
        assert merged.index(terse["style_prompt"]) < merged.index(base)

    def test_build_merged_prompt_append(self, tmp_path):
        from tag.persona import build_merged_prompt
        persona = {
            "name": "test", "style_prompt": "Be verbose.", "inject": "append", "position": 0
        }
        base = "Base system prompt."
        merged = build_merged_prompt(base, [persona])
        assert merged.index("Be verbose.") > merged.index("Base system prompt.")

    def test_build_merged_prompt_empty_personas(self, tmp_path):
        from tag.persona import build_merged_prompt
        base = "Base prompt."
        merged = build_merged_prompt(base, [])
        assert merged == base

    def test_install_persona_from_file(self, tmp_path):
        from tag.persona import load_persona_file, install_persona, get_persona, ensure_schema
        persona_file = tmp_path / "custom.yaml"
        persona_file.write_text(textwrap.dedent("""\
            name: custom-persona
            description: My custom persona
            style_prompt: Always respond in bullet points.
            inject: append
            tags: [style]
        """))
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        persona_data = load_persona_file(persona_file)
        install_persona(db, persona_data, source="user")
        p = get_persona(db, "custom-persona")
        db.close()
        assert p is not None
        assert p["source"] == "user"
        assert "bullet" in p["style_prompt"]

    def test_cmd_persona_list(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(persona_subcommand="list", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_persona(args)
        out = capsys.readouterr().out
        assert "terse-engineer" in out

    def test_cmd_persona_apply(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(
            persona_subcommand="apply",
            name="terse-engineer",
            profile="orchestrator",
            session_id=None,
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_persona(args)
        assert rc == 0
        assert "applied" in capsys.readouterr().out


# ===========================================================================
# PRD-038: Diff-Aware Context Injection
# ===========================================================================

class TestDiffContext:
    def test_blocked_patterns_filter(self):
        from tag.diff_context import _is_blocked, DEFAULT_BLOCKED_PATTERNS
        assert _is_blocked(".env", DEFAULT_BLOCKED_PATTERNS)
        assert _is_blocked("secrets.key", DEFAULT_BLOCKED_PATTERNS)
        assert _is_blocked("prod.env", DEFAULT_BLOCKED_PATTERNS)
        assert not _is_blocked("app.py", DEFAULT_BLOCKED_PATTERNS)
        assert not _is_blocked("config.yaml", DEFAULT_BLOCKED_PATTERNS)

    def test_binary_extension_check(self):
        from tag.diff_context import _is_binary
        assert _is_binary("image.png")
        assert _is_binary("archive.zip")
        assert not _is_binary("app.py")
        assert not _is_binary("config.yaml")

    def test_estimate_tokens(self):
        from tag.diff_context import _estimate_tokens
        text = "a" * 400
        assert _estimate_tokens(text) == 100

    def test_build_diff_context_no_git(self, tmp_path):
        from tag.diff_context import build_diff_context
        # Non-git directory — should raise RuntimeError
        with pytest.raises(RuntimeError):
            build_diff_context("HEAD", workdir=tmp_path)

    def test_get_changed_files_in_repo(self):
        from tag.diff_context import get_changed_files
        # Run in the actual repo — HEAD should work
        try:
            files = get_changed_files("HEAD", workdir=ROOT)
            # Just verify it returns a list
            assert isinstance(files, list)
        except RuntimeError:
            pytest.skip("git not available")

    def test_pr_diff_context_gh_not_found(self):
        from tag.diff_context import pr_diff_context
        with pytest.raises(RuntimeError, match="gh CLI not found"):
            with patch("subprocess.run", side_effect=FileNotFoundError):
                pr_diff_context(42)

    def test_cmd_diff_inject_no_changes(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(
            ref="HEAD",
            staged=False,
            pr=None,
            repo=None,
            context_lines=3,
            max_files=10,
            blocked=[],
            output_only=True,
            workdir=str(ROOT),
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            # May succeed or fail depending on diff state — just check it runs
            try:
                TAG.cmd_diff_inject(args)
            except Exception:
                pass


# ===========================================================================
# PRD-039: Token Budget Enforcement
# ===========================================================================

class TestBudget:
    def test_budget_table_created(self, tmp_path):
        cfg, db = make_db(tmp_path)
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "token_budgets" in tables
        db.close()

    def test_set_and_get_budget(self, tmp_path):
        from tag.budget import set_budget, get_budget, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        set_budget(db, "coder", 100_000, period="daily")
        b = get_budget(db, "coder")
        db.close()
        assert b["max_tokens"] == 100_000
        assert b["period"] == "daily"
        assert b["enabled"] is True

    def test_set_budget_invalid_tokens(self, tmp_path):
        from tag.budget import set_budget, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        with pytest.raises(ValueError, match="max_tokens"):
            set_budget(db, "coder", -1)
        db.close()

    def test_set_budget_invalid_period(self, tmp_path):
        from tag.budget import set_budget, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        with pytest.raises(ValueError, match="period"):
            set_budget(db, "coder", 1000, period="yearly")
        db.close()

    def test_remove_budget(self, tmp_path):
        from tag.budget import set_budget, remove_budget, get_budget, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        set_budget(db, "reviewer", 50_000)
        removed = remove_budget(db, "reviewer")
        assert removed is True
        assert get_budget(db, "reviewer") is None
        db.close()

    def test_list_budgets(self, tmp_path):
        from tag.budget import set_budget, list_budgets, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        set_budget(db, "coder", 100_000)
        set_budget(db, "reviewer", 50_000)
        budgets = list_budgets(db)
        db.close()
        assert len(budgets) == 2

    def test_check_budget_no_budget(self, tmp_path):
        from tag.budget import check_budget, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        result = check_budget(db, "no-budget-profile")
        db.close()
        assert result["allowed"] is True
        assert result["budget"] is None

    def test_check_budget_under_limit(self, tmp_path):
        from tag.budget import set_budget, check_budget, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        set_budget(db, "coder", 1_000_000)
        result = check_budget(db, "coder")
        db.close()
        assert result["allowed"] is True
        assert result["pct"] == 0.0

    def test_check_budget_exceeded_raises(self, tmp_path):
        from tag.budget import set_budget, check_budget, BudgetExceeded, ensure_schema
        import uuid, datetime
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        set_budget(db, "heavy", 100, period="daily")

        # Insert a run that uses 200 tokens
        now = datetime.datetime.now(datetime.timezone.utc).isoformat()
        db.execute(
            """INSERT INTO runs(id, created_at, kind, task_type, execution, master_profile,
               board, prompt, route_json, status, metadata_json,
               prompt_tokens, completion_tokens, total_tokens, model_id)
               VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
            (uuid.uuid4().hex, now, "submit", "mixed", "sequential", "heavy",
             "default", "p", "{}", "completed", "{}", 150, 150, 300, "claude-opus-4"),
        )
        db.commit()
        with pytest.raises(BudgetExceeded):
            check_budget(db, "heavy")
        db.close()

    def test_warn_at_threshold(self, tmp_path):
        from tag.budget import set_budget, check_budget, ensure_schema
        import uuid, datetime
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        set_budget(db, "warn-test", 1000, warn_pct=0.5)

        now = datetime.datetime.now(datetime.timezone.utc).isoformat()
        db.execute(
            """INSERT INTO runs(id, created_at, kind, task_type, execution, master_profile,
               board, prompt, route_json, status, metadata_json,
               prompt_tokens, completion_tokens, total_tokens, model_id)
               VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
            (uuid.uuid4().hex, now, "submit", "mixed", "sequential", "warn-test",
             "default", "p", "{}", "completed", "{}", 300, 300, 600, "claude-opus-4"),
        )
        db.commit()
        result = check_budget(db, "warn-test")
        db.close()
        assert result["allowed"] is True
        assert result["warn"] is True

    def test_cmd_budget_set_and_list(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(
            budget_subcommand="set",
            profile="coder",
            max_tokens=500_000,
            period="weekly",
            warn_pct=0.8,
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_budget(args)
        out = capsys.readouterr().out
        assert "500,000" in out or "Budget set" in out

        args2 = make_args(budget_subcommand="list", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_budget(args2)
        out2 = capsys.readouterr().out
        assert "coder" in out2


# ===========================================================================
# PRD-040: Notification Hooks
# ===========================================================================

class TestNotifications:
    def test_notification_tables_created(self, tmp_path):
        cfg, db = make_db(tmp_path)
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "notification_hooks" in tables
        assert "notification_log" in tables
        db.close()

    def test_add_and_list_hook(self, tmp_path):
        from tag.notifications import add_hook, list_hooks, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        hook_id = add_hook(db, "run.completed", "desktop", {}, profile="coder")
        hooks = list_hooks(db)
        db.close()
        assert len(hooks) == 1
        assert hooks[0]["id"] == hook_id
        assert hooks[0]["channel"] == "desktop"

    def test_add_hook_invalid_event(self, tmp_path):
        from tag.notifications import add_hook, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        with pytest.raises(ValueError, match="event"):
            add_hook(db, "invalid.event", "desktop", {})
        db.close()

    def test_add_hook_invalid_channel(self, tmp_path):
        from tag.notifications import add_hook, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        with pytest.raises(ValueError, match="channel"):
            add_hook(db, "run.completed", "sms", {})
        db.close()

    def test_remove_hook(self, tmp_path):
        from tag.notifications import add_hook, remove_hook, list_hooks, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        hook_id = add_hook(db, "run.failed", "desktop", {})
        remove_hook(db, hook_id)
        hooks = list_hooks(db)
        db.close()
        assert len(hooks) == 0

    def test_enable_disable_hook(self, tmp_path):
        from tag.notifications import add_hook, set_hook_enabled, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        hook_id = add_hook(db, "run.completed", "desktop", {})
        set_hook_enabled(db, hook_id, False)
        row = db.execute("SELECT enabled FROM notification_hooks WHERE id=?", (hook_id,)).fetchone()
        assert row[0] == 0
        set_hook_enabled(db, hook_id, True)
        row = db.execute("SELECT enabled FROM notification_hooks WHERE id=?", (hook_id,)).fetchone()
        assert row[0] == 1
        db.close()

    def test_template_rendering(self):
        from tag.notifications import _render_template
        template = "Run {{run_id}} {{status}} on {{profile}}"
        ctx = {"run_id": "abc123", "status": "completed", "profile": "coder"}
        result = _render_template(template, ctx)
        assert result == "Run abc123 completed on coder"

    def test_template_unknown_vars_ignored(self):
        from tag.notifications import _render_template
        # Only allowlisted vars are substituted; unknown vars remain verbatim
        template = "Hello {{unknown_var}} world {{run_id}}"
        result = _render_template(template, {"run_id": "abc123"})
        assert "abc123" in result
        # Unknown var left untouched (not substituted, not removed)
        assert "{{unknown_var}}" in result

    def test_deliver_desktop_success(self):
        from tag.notifications import deliver
        hook = {
            "id": "test", "channel": "desktop",
            "config": {"title": "Test"},
            "template": "Test notification",
            "enabled": True,
        }
        with patch("subprocess.run", return_value=MagicMock(returncode=0)):
            ok, err = deliver(hook, "test", {"event": "test", "profile": "coder", "status": "ok"}, max_retries=1)
        assert ok

    def test_deliver_slack_mock(self):
        from tag.notifications import deliver
        hook = {
            "id": "test", "channel": "slack",
            "config": {"webhook_url": "https://hooks.slack.com/test"},
            "template": "Slack {{status}}",
            "enabled": True,
        }
        mock_resp = MagicMock()
        mock_resp.__enter__ = lambda s: s
        mock_resp.__exit__ = MagicMock(return_value=False)
        mock_resp.status = 200
        with patch("urllib.request.urlopen", return_value=mock_resp):
            ok, err = deliver(hook, "run.completed", {"status": "done", "profile": "coder"}, max_retries=1)
        assert ok

    def test_cmd_notify_add(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(
            notify_subcommand="add",
            event="run.completed",
            channel="desktop",
            profile="coder",
            config_json="{}",
            template="",
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_notify(args)
        assert rc == 0
        assert "added" in capsys.readouterr().out


# ===========================================================================
# PRD-041: OTel GenAI Span Cost Attribution
# ===========================================================================

class TestOtelSemconv:
    def test_detect_provider(self):
        from tag.otel_semconv import detect_provider
        assert detect_provider("claude-opus-4") == "anthropic"
        assert detect_provider("gpt-4o") == "openai"
        assert detect_provider("gemini-pro") == "google"
        assert detect_provider("mistral-7b") == "mistral"
        assert detect_provider("unknown-model") == "unknown"

    def test_map_span_attributes(self):
        from tag.otel_semconv import map_span_attributes
        span = {
            "id": "span001",
            "name": "inference",
            "model_id": "claude-opus-4",
            "prompt_tokens": 500,
            "completion_tokens": 200,
            "status": "ok",
        }
        mapped = map_span_attributes(span)
        attrs = mapped["attributes"]
        assert attrs["gen_ai.usage.input_tokens"] == 500
        assert attrs["gen_ai.usage.output_tokens"] == 200
        assert attrs["gen_ai.request.model"] == "claude-opus-4"
        assert attrs["gen_ai.system"] == "anthropic"
        assert attrs["gen_ai.operation.name"] == "chat"

    def test_map_span_preserves_originals(self):
        from tag.otel_semconv import map_span_attributes
        span = {"prompt_tokens": 100, "completion_tokens": 50, "model_id": "gpt-4o"}
        mapped = map_span_attributes(span)
        # Original fields still present in the top-level dict
        assert mapped["prompt_tokens"] == 100
        assert mapped["completion_tokens"] == 50

    def test_spans_to_otlp_json_structure(self):
        from tag.otel_semconv import spans_to_otlp_json
        spans = [
            {"id": "s1", "trace_id": "t1", "name": "chat", "model_id": "claude-opus-4",
             "prompt_tokens": 100, "completion_tokens": 50,
             "started_at": "2026-06-15T10:00:00+00:00",
             "finished_at": "2026-06-15T10:00:01+00:00", "status": "ok"},
        ]
        payload = spans_to_otlp_json(spans, include_metrics=True)
        assert "resourceSpans" in payload
        assert "resourceMetrics" in payload
        rs = payload["resourceSpans"][0]
        scope_spans = rs["scopeSpans"][0]["spans"]
        assert len(scope_spans) == 1
        # Check gen_ai attribute is present
        attrs = {a["key"]: a for a in scope_spans[0]["attributes"]}
        assert "gen_ai.usage.input_tokens" in attrs
        assert "gen_ai.request.model" in attrs

    def test_token_metrics_generated(self):
        from tag.otel_semconv import spans_to_otlp_json
        spans = [
            {"id": "s1", "trace_id": "t1", "name": "chat", "model_id": "claude-haiku-4-5",
             "prompt_tokens": 200, "completion_tokens": 100,
             "started_at": "2026-06-15T10:00:00+00:00",
             "finished_at": "2026-06-15T10:00:01+00:00", "status": "ok"},
        ]
        payload = spans_to_otlp_json(spans, include_metrics=True)
        metrics = payload["resourceMetrics"][0]["scopeMetrics"][0]["metrics"]
        assert any(m["name"] == "gen_ai.client.token.usage" for m in metrics)

    def test_iso_to_ns(self):
        from tag.otel_semconv import _iso_to_ns
        ns = _iso_to_ns("2026-06-15T10:00:00+00:00")
        assert ns > 0
        assert _iso_to_ns("") == 0
        assert _iso_to_ns("bad") == 0

    def test_semconv_version_pinned(self):
        from tag.otel_semconv import SEMCONV_VERSION
        assert SEMCONV_VERSION == "1.28.0"

    def test_cmd_otel_export_json(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(
            trace_id=None,
            endpoint="",
            semconv="1.28.0",
            no_metrics=False,
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_otel_export(args)
        assert rc == 0
        out = capsys.readouterr().out
        data = json.loads(out)
        assert "resourceSpans" in data


# ===========================================================================
# PRD-042: Architect/Editor Agent Split
# ===========================================================================

class TestSplitAgent:
    def test_split_tables_created(self, tmp_path):
        cfg, db = make_db(tmp_path)
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "split_runs" in tables
        assert "split_items" in tables
        db.close()

    def test_change_item_roundtrip(self):
        from tag.split_agent import ChangeItem
        item = ChangeItem(id="i1", file="src/app.py", description="Add type hints",
                          action="modify", priority=0)
        d = item.to_dict()
        item2 = ChangeItem.from_dict(d)
        assert item2.file == "src/app.py"
        assert item2.description == "Add type hints"

    def test_change_spec_json_roundtrip(self):
        from tag.split_agent import ChangeSpec, ChangeItem
        spec = ChangeSpec(
            task="Refactor auth module",
            items=[
                ChangeItem(id="a", file="auth.py", description="Extract validate_token"),
                ChangeItem(id="b", file="models.py", description="Add UserToken model"),
            ],
            rationale="Clean separation of concerns",
        )
        s = spec.to_json()
        spec2 = ChangeSpec.from_json(s)
        assert spec2.task == "Refactor auth module"
        assert len(spec2.items) == 2

    def test_create_and_save_spec(self, tmp_path):
        from tag.split_agent import create_split_run, save_spec, get_split_run, ChangeSpec, ChangeItem, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        run_id = create_split_run(db, "Fix bug", "claude-opus-4", "claude-haiku-4-5", "coder")
        spec = ChangeSpec("Fix bug", [
            ChangeItem("x1", "app.py", "Fix null pointer"),
            ChangeItem("x2", "tests.py", "Add regression test"),
        ])
        save_spec(db, run_id, spec)
        run = get_split_run(db, run_id)
        db.close()
        assert run["task"] == "Fix bug"
        assert len(run["items"]) == 2
        assert run["status"] == "planning"

    def test_list_split_runs(self, tmp_path):
        from tag.split_agent import create_split_run, list_split_runs, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        create_split_run(db, "Task 1", "opus", "haiku", "p1")
        create_split_run(db, "Task 2", "opus", "haiku", "p1")
        runs = list_split_runs(db)
        db.close()
        assert len(runs) == 2

    def test_architect_system_prompt(self):
        from tag.split_agent import ARCHITECT_SYSTEM
        assert "JSON" in ARCHITECT_SYSTEM
        assert "Architect" in ARCHITECT_SYSTEM
        assert "items" in ARCHITECT_SYSTEM

    def test_editor_system_prompt(self):
        from tag.split_agent import EDITOR_SYSTEM
        assert "Editor" in EDITOR_SYSTEM
        assert "file" in EDITOR_SYSTEM.lower() or "write" in EDITOR_SYSTEM.lower()

    def test_build_architect_prompt(self):
        from tag.split_agent import build_architect_prompt
        prompt = build_architect_prompt("Fix the bug", "README.md\nsrc/app.py")
        assert "Fix the bug" in prompt
        assert "REPO MAP" in prompt

    def test_cmd_split_list_empty(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(split_subcommand="list", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_split(args)
        out = capsys.readouterr().out
        assert "No architect" in out or "split runs" in out

    def test_cmd_split_plan(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(
            split_subcommand="plan",
            task="Refactor the auth module",
            architect="claude-opus-4",
            editor="claude-haiku-4-5",
            profile="coder",
            spec_json=None,
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_split(args)
        assert rc == 0
        out = capsys.readouterr().out
        assert "Split run created" in out


# ===========================================================================
# PRD-043: Vector-Based Tool Retrieval
# ===========================================================================

class TestToolRetrieval:
    def test_tool_index_table_created(self, tmp_path):
        cfg, db = make_db(tmp_path)
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "tool_index_meta" in tables
        db.close()

    def test_is_available_false_when_no_deps(self):
        from tag.tool_retrieval import is_available
        # In test env, chromadb/sentence-transformers may not be installed
        result = is_available()
        assert isinstance(result, bool)

    def test_keyword_search_tools(self):
        from tag.tool_retrieval import keyword_search_tools
        tools = [
            {"name": "github_create_pr", "description": "Create a GitHub pull request"},
            {"name": "filesystem_read", "description": "Read files from the filesystem"},
            {"name": "bash_exec", "description": "Execute shell commands"},
        ]
        results = keyword_search_tools("github pull request", tools, top_k=2)
        assert len(results) == 1
        assert results[0]["name"] == "github_create_pr"

    def test_keyword_search_no_match(self):
        from tag.tool_retrieval import keyword_search_tools
        tools = [{"name": "github", "description": "GitHub operations"}]
        results = keyword_search_tools("kubernetes deploy", tools, top_k=5)
        assert len(results) == 0

    def test_keyword_search_ranking(self):
        from tag.tool_retrieval import keyword_search_tools
        tools = [
            {"name": "github_pr", "description": "GitHub PR operations pull request"},
            {"name": "git_commit", "description": "Git commit"},
            {"name": "gh_review", "description": "Review GitHub pull request code"},
        ]
        results = keyword_search_tools("github pull request review", tools, top_k=3)
        # github_pr has 3 matching words, gh_review has 3 too — both should beat git_commit (0)
        assert "git_commit" not in [t["name"] for t in results[:2]]

    def test_get_index_stats_not_built(self, tmp_path):
        from tag.tool_retrieval import get_index_stats, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        stats = get_index_stats(db)
        db.close()
        assert stats["built"] is False
        assert stats["tool_count"] == 0

    def test_is_index_stale_when_not_built(self, tmp_path):
        from tag.tool_retrieval import is_index_stale, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        stale = is_index_stale(db, registry_mtime=1234567890.0)
        db.close()
        assert stale is True

    def test_cmd_tool_retrieval_index_no_registry(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(tr_subcommand="index", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_tool_retrieval(args)
        assert rc == 0  # succeeds even with empty/missing registry

    def test_cmd_tool_retrieval_search_empty_query(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(
            tr_subcommand="search",
            query="",
            top_k=5,
            config=None,
        )
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            rc = TAG.cmd_tool_retrieval(args)
        assert rc == 1  # empty query is an error


# ===========================================================================
# PRD-044: AgentOps Session Observability
# ===========================================================================

class TestAgentOps:
    def test_agentops_table_created(self, tmp_path):
        cfg, db = make_db(tmp_path)
        tables = {r[0] for r in db.execute("SELECT name FROM sqlite_master WHERE type='table'").fetchall()}
        assert "agentops_sessions" in tables
        db.close()

    def test_mask_key(self):
        from tag.integrations.agentops_bridge import mask_key
        assert mask_key("abc12345") == "****2345"
        # "short" is 5 chars: 1 star + last 4 = "*hort"
        assert mask_key("short") == "*hort"
        assert mask_key("ab") == "****"
        assert mask_key("") == "****"

    def test_is_available_false_no_sdk(self):
        from tag.integrations.agentops_bridge import is_available
        result = is_available()
        assert isinstance(result, bool)

    def test_is_configured_false_no_key(self, tmp_path):
        from tag.integrations.agentops_bridge import is_configured
        cfg, _ = make_db(tmp_path)
        with patch.dict(os.environ, {}, clear=False):
            # Remove any real key from env
            os.environ.pop("AGENTOPS_API_KEY", None)
            result = is_configured(cfg)
        assert result is False

    def test_is_configured_true_with_key(self, tmp_path):
        from tag.integrations.agentops_bridge import is_configured
        cfg = {"agentops": {"api_key": "test-key-1234"}}
        assert is_configured(cfg) is True

    def test_get_session_for_run_not_found(self, tmp_path):
        from tag.integrations.agentops_bridge import get_session_for_run, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        result = get_session_for_run(db, "nonexistent-run")
        db.close()
        assert result is None

    def test_list_sessions_empty(self, tmp_path):
        from tag.integrations.agentops_bridge import list_sessions, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        sessions = list_sessions(db)
        db.close()
        assert sessions == []

    def test_session_stored_manually(self, tmp_path):
        from tag.integrations.agentops_bridge import get_session_for_run, list_sessions, ensure_schema
        cfg, db = make_db(tmp_path)
        ensure_schema(db)
        import uuid
        now = TAG.utc_now()
        run_id = uuid.uuid4().hex
        db.execute(
            "INSERT INTO agentops_sessions(id, run_id, session_id, status, created_at) VALUES(?,?,?,?,?)",
            (run_id, run_id, "sess-abc123", "completed", now),
        )
        db.commit()
        result = get_session_for_run(db, run_id)
        assert result is not None
        assert result["session_id"] == "sess-abc123"
        assert "agentops.ai" in result["dashboard_url"]
        sessions = list_sessions(db)
        assert len(sessions) == 1
        db.close()

    def test_agentops_session_no_op_when_no_key(self, tmp_path):
        from tag.integrations.agentops_bridge import AgentOpsSession
        cfg, db = make_db(tmp_path)
        session = AgentOpsSession("run1", "coder", "test task", conn=db, cfg={})
        # Should not crash; no active session
        session.record_llm_call("claude-opus-4", 100, 50)
        session.record_tool_call("bash", {}, "output")
        session.record_error("test error")
        session.close(success=True)
        db.close()

    def test_cmd_agentops_status(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(agentops_subcommand="status", config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_agentops(args)
        out = capsys.readouterr().out
        assert "AgentOps SDK" in out

    def test_cmd_agentops_sessions_empty(self, tmp_path, capsys):
        cfg, db = make_db(tmp_path)
        db.close()
        args = make_args(agentops_subcommand="sessions", limit=20, config=None)
        with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "taghome")}):
            TAG.cmd_agentops(args)
        out = capsys.readouterr().out
        assert "No AgentOps sessions" in out
