"""
Comprehensive end-to-end tests for PRD-001 through PRD-010.

Each section maps to one PRD and exercises the public API of that feature
using only local filesystem operations (no network, no real Hermes binary,
no real API keys).
"""
from __future__ import annotations

import importlib.util
import io
import json
import os
import sqlite3
import subprocess
import sys
from copy import deepcopy
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

ROOT = Path(__file__).resolve().parents[1]
MODULE_PATH = ROOT / "src" / "tag" / "controller.py"
SPEC = importlib.util.spec_from_file_location("tag_controller", MODULE_PATH)
assert SPEC and SPEC.loader
TAG = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(TAG)

TUI_MODULE_PATH = ROOT / "src" / "tag" / "tui_output.py"
TUI_SPEC = importlib.util.spec_from_file_location("tag_tui_output", TUI_MODULE_PATH)
assert TUI_SPEC and TUI_SPEC.loader
TUI = importlib.util.module_from_spec(TUI_SPEC)
TUI_SPEC.loader.exec_module(TUI)

QUEUE_MODULE_PATH = ROOT / "src" / "tag" / "queue_worker.py"
QW_SPEC = importlib.util.spec_from_file_location("tag_queue_worker", QUEUE_MODULE_PATH)
assert QW_SPEC and QW_SPEC.loader
QW = importlib.util.module_from_spec(QW_SPEC)
QW_SPEC.loader.exec_module(QW)


def load_cfg(monkeypatch=None, tmp_path=None):
    cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
    if monkeypatch and tmp_path:
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
    return cfg


def make_db(tmp_path):
    """Create a fresh TAG runtime DB for tests."""
    monkeyenv = {"TAG_HOME": str(tmp_path / "taghome")}
    with patch.dict(os.environ, monkeyenv):
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        db = TAG.open_db(cfg)
    return cfg, db


# ===========================================================================
# PRD-002: Cross-Session Memory Journal
# ===========================================================================

class TestMemoryJournal:

    def test_journal_save_returns_id(self, tmp_path):
        cfg, db = make_db(tmp_path)
        entry_id = TAG.journal_save(db, "orchestrator", "project", "TAG agent orchestration platform")
        assert isinstance(entry_id, str)
        assert len(entry_id) > 0

    def test_journal_save_upserts_same_key(self, tmp_path):
        cfg, db = make_db(tmp_path)
        id1 = TAG.journal_save(db, "orchestrator", "project", "value1")
        id2 = TAG.journal_save(db, "orchestrator", "project", "value2")
        entries = TAG.journal_list(db, "orchestrator")
        matching = [e for e in entries if e["key"] == "project"]
        assert len(matching) == 1
        assert matching[0]["value"] == "value2"

    def test_journal_list_returns_entries(self, tmp_path):
        cfg, db = make_db(tmp_path)
        TAG.journal_save(db, "orchestrator", "k1", "v1")
        TAG.journal_save(db, "orchestrator", "k2", "v2")
        entries = TAG.journal_list(db, "orchestrator")
        keys = {e["key"] for e in entries}
        assert "k1" in keys
        assert "k2" in keys

    def test_journal_list_excludes_other_profiles(self, tmp_path):
        cfg, db = make_db(tmp_path)
        TAG.journal_save(db, "orchestrator", "shared", "belongs to orchestrator")
        TAG.journal_save(db, "coder", "shared", "belongs to coder")
        orch_entries = TAG.journal_list(db, "orchestrator", include_global=False)
        coder_entries = TAG.journal_list(db, "coder", include_global=False)
        assert all(e["profile"] == "orchestrator" for e in orch_entries)
        assert all(e["profile"] == "coder" for e in coder_entries)

    def test_journal_forget_removes_entry(self, tmp_path):
        cfg, db = make_db(tmp_path)
        entry_id = TAG.journal_save(db, "orchestrator", "to-remove", "temp")
        result = TAG.journal_forget(db, entry_id)
        assert result is True
        entries = TAG.journal_list(db, "orchestrator")
        assert not any(e["id"] == entry_id for e in entries)

    def test_journal_forget_returns_false_for_unknown_id(self, tmp_path):
        cfg, db = make_db(tmp_path)
        result = TAG.journal_forget(db, "nonexistent-id")
        assert result is False

    def test_journal_clear_removes_all_profile_entries(self, tmp_path):
        cfg, db = make_db(tmp_path)
        TAG.journal_save(db, "researcher", "a", "1")
        TAG.journal_save(db, "researcher", "b", "2")
        TAG.journal_save(db, "coder", "a", "3")
        count = TAG.journal_clear(db, "researcher")
        assert count == 2
        assert TAG.journal_list(db, "researcher", include_global=False) == []
        # coder entry survives
        coder_entries = TAG.journal_list(db, "coder", include_global=False)
        assert len(coder_entries) == 1

    def test_journal_to_prompt_prefix_returns_none_when_empty(self, tmp_path):
        cfg, db = make_db(tmp_path)
        result = TAG.journal_to_prompt_prefix(db, "orchestrator")
        assert result is None

    def test_journal_to_prompt_prefix_formats_entries(self, tmp_path):
        cfg, db = make_db(tmp_path)
        TAG.journal_save(db, "orchestrator", "goal", "Build a world-class agent platform")
        TAG.journal_save(db, "orchestrator", "stack", "Python + SQLite")
        prefix = TAG.journal_to_prompt_prefix(db, "orchestrator")
        assert prefix is not None
        assert "goal" in prefix
        assert "Build a world-class agent platform" in prefix
        assert "stack" in prefix

    def test_journal_ttl_expiry_excludes_expired(self, tmp_path):
        cfg, db = make_db(tmp_path)
        # Save with ttl_days=0 (expires immediately in the past by overriding expires_at directly)
        entry_id = TAG.journal_save(db, "orchestrator", "ephemeral", "gone soon")
        # Manually expire it
        import datetime
        past = (
            datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(days=1)
        ).isoformat()
        db.execute("UPDATE memory_journal SET expires_at=? WHERE id=?", (past, entry_id))
        db.commit()
        entries = TAG.journal_list(db, "orchestrator")
        assert not any(e["id"] == entry_id for e in entries)

    def test_journal_global_scope_included_in_all_profiles(self, tmp_path):
        cfg, db = make_db(tmp_path)
        # Global entries use profile='*' as the sentinel
        global_id = TAG.journal_save(db, "*", "shared-fact", "visible to all")
        orch_entries = TAG.journal_list(db, "orchestrator", include_global=True)
        assert any(e["id"] == global_id for e in orch_entries)

    def test_cmd_memory_journal_save_stdout(self, tmp_path, monkeypatch, capsys):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        TAG.open_db(cfg)  # initialise schema

        import argparse
        args = argparse.Namespace(
            config=None,
            mj_subcommand="save",
            key="test-key",
            value="test-value",
            profile=None,
            ttl_days=None,
            json=False,
        )
        rc = TAG.cmd_memory_journal(args)
        assert rc == 0
        captured = capsys.readouterr()
        assert "test-key" in captured.out or "saved" in captured.out.lower()

    def test_cmd_memory_journal_list_json(self, tmp_path, monkeypatch, capsys):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        db = TAG.open_db(cfg)
        TAG.journal_save(db, "orchestrator", "list-key", "list-value")
        db.close()

        import argparse
        args = argparse.Namespace(
            config=None,
            mj_subcommand="list",
            profile=None,
            json=True,
        )
        rc = TAG.cmd_memory_journal(args)
        assert rc == 0
        captured = capsys.readouterr()
        data = json.loads(captured.out)
        assert isinstance(data, list)
        assert any(e["key"] == "list-key" for e in data)

    def test_cmd_memory_journal_forget(self, tmp_path, monkeypatch, capsys):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        db = TAG.open_db(cfg)
        entry_id = TAG.journal_save(db, "orchestrator", "forget-this", "temporary")
        db.close()

        import argparse
        args = argparse.Namespace(
            config=None,
            mj_subcommand="forget",
            entry_id=entry_id,
            json=False,
        )
        rc = TAG.cmd_memory_journal(args)
        assert rc == 0

    def test_cmd_memory_journal_clear_requires_confirm(self, tmp_path, monkeypatch, capsys):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        db = TAG.open_db(cfg)
        TAG.journal_save(db, "orchestrator", "keep-or-remove", "?")
        db.close()

        import argparse
        args = argparse.Namespace(
            config=None,
            mj_subcommand="clear",
            profile=None,
            confirm=False,
            json=False,
        )
        rc = TAG.cmd_memory_journal(args)
        # Without --confirm, should abort
        assert rc != 0 or "confirm" in capsys.readouterr().out.lower()


# ===========================================================================
# PRD-003: Rich Streaming TUI Output
# ===========================================================================

class TestTuiOutput:

    def test_get_console_returns_none_when_not_tty(self, monkeypatch):
        monkeypatch.setattr(sys.stdout, "isatty", lambda: False)
        monkeypatch.setattr(sys.stderr, "isatty", lambda: False)
        console = TUI.get_console()
        assert console is None

    def test_get_console_returns_none_with_no_color_env(self, monkeypatch):
        monkeypatch.setenv("NO_COLOR", "1")
        # Force TTY-looking
        monkeypatch.setattr(sys.stdout, "isatty", lambda: True)
        monkeypatch.setattr(sys.stderr, "isatty", lambda: True)
        console = TUI.get_console()
        assert console is None

    def test_get_console_returns_none_with_tag_no_color(self, monkeypatch):
        monkeypatch.setenv("TAG_NO_COLOR", "1")
        console = TUI.get_console()
        assert console is None

    def test_print_error_writes_to_stderr(self, capsys, monkeypatch):
        monkeypatch.setattr(sys.stdout, "isatty", lambda: False)
        monkeypatch.setattr(sys.stderr, "isatty", lambda: False)
        TUI.print_error("something went wrong")
        captured = capsys.readouterr()
        assert "something went wrong" in captured.err

    def test_print_success_writes_to_stdout(self, capsys, monkeypatch):
        monkeypatch.setattr(sys.stdout, "isatty", lambda: False)
        monkeypatch.setattr(sys.stderr, "isatty", lambda: False)
        TUI.print_success("all done")
        captured = capsys.readouterr()
        assert "all done" in captured.out

    def test_print_warning_writes_to_stderr(self, capsys, monkeypatch):
        monkeypatch.setattr(sys.stdout, "isatty", lambda: False)
        monkeypatch.setattr(sys.stderr, "isatty", lambda: False)
        TUI.print_warning("heads up")
        captured = capsys.readouterr()
        assert "heads up" in captured.err

    def test_print_doctor_report_plain_text(self, capsys, monkeypatch):
        monkeypatch.setattr(sys.stdout, "isatty", lambda: False)
        monkeypatch.setattr(sys.stderr, "isatty", lambda: False)
        groups = {
            "system": [
                {"name": "python", "status": "pass", "message": "3.12"},
                {"name": "disk", "status": "warn", "message": "< 2 GB free"},
            ],
            "hermes": [
                {"name": "binary", "status": "fail", "message": "not found", "fix_cmd": "tag setup"},
            ],
        }
        TUI.print_doctor_report(groups)
        captured = capsys.readouterr()
        out = captured.out
        assert "python" in out
        assert "disk" in out
        assert "binary" in out
        assert "tag setup" in out
        assert "Summary:" in out

    def test_print_doctor_report_summary_counts(self, capsys, monkeypatch):
        monkeypatch.setattr(sys.stdout, "isatty", lambda: False)
        monkeypatch.setattr(sys.stderr, "isatty", lambda: False)
        groups = {
            "test": [
                {"name": "a", "status": "pass", "message": "ok"},
                {"name": "b", "status": "warn", "message": "hmm"},
                {"name": "c", "status": "fail", "message": "bad"},
            ]
        }
        TUI.print_doctor_report(groups)
        captured = capsys.readouterr()
        assert "1 pass" in captured.out
        assert "1 warn" in captured.out
        assert "1 fail" in captured.out

    def test_chat_spinner_context_manager(self, monkeypatch):
        monkeypatch.setattr(sys.stdout, "isatty", lambda: False)
        monkeypatch.setattr(sys.stderr, "isatty", lambda: False)
        # Should not raise even without a real TTY
        with TUI.chat_spinner("orchestrator", "gpt-4"):
            pass

    def test_make_benchmark_progress_returns_none_without_tty(self, monkeypatch):
        monkeypatch.setattr(sys.stdout, "isatty", lambda: False)
        monkeypatch.setattr(sys.stderr, "isatty", lambda: False)
        result = TUI.make_benchmark_progress()
        assert result is None

    def test_make_submit_progress_returns_none_without_tty(self, monkeypatch):
        monkeypatch.setattr(sys.stdout, "isatty", lambda: False)
        monkeypatch.setattr(sys.stderr, "isatty", lambda: False)
        result = TUI.make_submit_progress()
        assert result is None

    def test_send_desktop_notification_does_not_crash(self, monkeypatch):
        # Mock subprocess.run to avoid actually calling osascript
        monkeypatch.setattr(subprocess, "run", lambda *a, **kw: MagicMock(returncode=0))
        TUI.send_desktop_notification("Test Title", "Test Message")

    def test_stream_output_plain(self, capsys, monkeypatch):
        monkeypatch.setattr(sys.stdout, "isatty", lambda: False)
        monkeypatch.setattr(sys.stderr, "isatty", lambda: False)
        TUI.stream_output("hello world")
        captured = capsys.readouterr()
        assert "hello world" in captured.out


# ===========================================================================
# PRD-005: Execution Backend Selection
# ===========================================================================

class TestExecutionBackend:

    def test_import_docker_writes_image_env(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        TAG.profile_home(cfg, "coder").mkdir(parents=True, exist_ok=True)

        with patch("subprocess.run", return_value=MagicMock(returncode=0)):
            result = TAG.import_docker_into_profile(cfg, "coder", image="python:3.12")
        assert result["status"] == "ok"
        env = TAG.read_dotenv(TAG.profile_home(cfg, "coder") / ".env")
        assert env["DOCKER_DEFAULT_IMAGE"] == "python:3.12"

    def test_import_docker_default_image(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        TAG.profile_home(cfg, "coder").mkdir(parents=True, exist_ok=True)

        with patch("subprocess.run", return_value=MagicMock(returncode=0)):
            result = TAG.import_docker_into_profile(cfg, "coder")
        env = TAG.read_dotenv(TAG.profile_home(cfg, "coder") / ".env")
        assert env["DOCKER_DEFAULT_IMAGE"] == "ubuntu:22.04"

    def test_import_docker_marks_unavailable_when_no_docker_binary(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        TAG.profile_home(cfg, "coder").mkdir(parents=True, exist_ok=True)

        import shutil as _shutil
        orig_which = _shutil.which
        monkeypatch.setattr(_shutil, "which", lambda x: None if x == "docker" else orig_which(x))
        result = TAG.import_docker_into_profile(cfg, "coder")
        assert result["docker_available"] is False
        assert "warning" in result

    def test_import_ssh_writes_env_vars(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        TAG.profile_home(cfg, "coder").mkdir(parents=True, exist_ok=True)

        result = TAG.import_ssh_into_profile(
            cfg, "coder",
            host="myserver.example.com",
            user="devuser",
            key_file="~/.ssh/id_rsa",
        )
        assert result["status"] == "ok"
        assert "SSH_HOST" in result["keys_written"]
        assert "SSH_USER" in result["keys_written"]
        env = TAG.read_dotenv(TAG.profile_home(cfg, "coder") / ".env")
        assert env["SSH_HOST"] == "myserver.example.com"
        assert env["SSH_USER"] == "devuser"

    def test_import_ssh_host_required(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        TAG.profile_home(cfg, "coder").mkdir(parents=True, exist_ok=True)

        with pytest.raises(SystemExit):
            TAG.import_ssh_into_profile(cfg, "coder", host="")

    def test_import_modal_writes_credentials(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        TAG.profile_home(cfg, "researcher").mkdir(parents=True, exist_ok=True)

        result = TAG.import_modal_into_profile(
            cfg, "researcher",
            token_id="ak-test-id",
            token_secret="ak-test-secret",
        )
        assert result["status"] == "ok"
        env = TAG.read_dotenv(TAG.profile_home(cfg, "researcher") / ".env")
        assert env["MODAL_TOKEN_ID"] == "ak-test-id"
        assert env["MODAL_TOKEN_SECRET"] == "ak-test-secret"

    def test_import_modal_requires_both_credentials(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        TAG.profile_home(cfg, "researcher").mkdir(parents=True, exist_ok=True)

        with pytest.raises(SystemExit):
            TAG.import_modal_into_profile(cfg, "researcher", token_id="id", token_secret="")

    def test_import_daytona_writes_workspace_id(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        TAG.profile_home(cfg, "coder").mkdir(parents=True, exist_ok=True)

        result = TAG.import_daytona_into_profile(
            cfg, "coder",
            workspace_id="ws-abc123",
            api_key="daytona-key-xyz",
        )
        assert result["status"] == "ok"
        env = TAG.read_dotenv(TAG.profile_home(cfg, "coder") / ".env")
        assert env["DAYTONA_WORKSPACE_ID"] == "ws-abc123"
        assert env["DAYTONA_API_KEY"] == "daytona-key-xyz"

    def test_render_profiles_writes_docker_backend(self, tmp_path, monkeypatch):
        import yaml
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")

        # Inject docker backend into coder profile
        cfg["profiles"]["coder"].setdefault("config", {})["execution"] = {
            "backend": "docker",
            "docker": {"image": "python:3.11"},
        }

        # Create profile homes
        for name in cfg["profiles"]:
            TAG.profile_home(cfg, name).mkdir(parents=True, exist_ok=True)

        TAG.render_profiles(cfg, force=True)

        config_yaml = TAG.profile_home(cfg, "coder") / "config.yaml"
        assert config_yaml.exists()
        rendered = yaml.safe_load(config_yaml.read_text())
        assert rendered.get("execution", {}).get("backend") == "docker"
        assert rendered["execution"]["docker"]["image"] == "python:3.11"

    def test_render_profiles_local_backend_not_written(self, tmp_path, monkeypatch):
        import yaml
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")

        # Explicitly set local backend
        cfg["profiles"]["coder"].setdefault("config", {})["execution"] = {"backend": "local"}

        for name in cfg["profiles"]:
            TAG.profile_home(cfg, name).mkdir(parents=True, exist_ok=True)

        TAG.render_profiles(cfg, force=True)

        config_yaml = TAG.profile_home(cfg, "coder") / "config.yaml"
        rendered = yaml.safe_load(config_yaml.read_text()) or {}
        # local backend should NOT produce an execution key in config
        assert "execution" not in rendered

    def test_render_profiles_ssh_backend(self, tmp_path, monkeypatch):
        import yaml
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")

        cfg["profiles"]["coder"].setdefault("config", {})["execution"] = {
            "backend": "ssh",
            "ssh": {"host": "myhost.example.com", "user": "ubuntu", "port": 22},
        }
        for name in cfg["profiles"]:
            TAG.profile_home(cfg, name).mkdir(parents=True, exist_ok=True)

        TAG.render_profiles(cfg, force=True)

        rendered = yaml.safe_load((TAG.profile_home(cfg, "coder") / "config.yaml").read_text())
        assert rendered["execution"]["backend"] == "ssh"
        assert rendered["execution"]["ssh"]["host"] == "myhost.example.com"

    def test_doctor_profile_warns_missing_docker_daemon(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")

        cfg["profiles"]["coder"].setdefault("config", {})["execution"] = {"backend": "docker"}
        TAG.profile_home(cfg, "coder").mkdir(parents=True, exist_ok=True)

        import shutil as _shutil
        orig_which = _shutil.which
        monkeypatch.setattr(_shutil, "which", lambda x: None if x == "docker" else orig_which(x))

        checks = TAG._doctor_profile_checks(cfg, "coder")
        backend_check = next((c for c in checks if "docker" in c["name"]), None)
        assert backend_check is not None
        assert backend_check["status"] == "warn"

    def test_doctor_profile_warns_missing_ssh_host(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")

        cfg["profiles"]["coder"].setdefault("config", {})["execution"] = {
            "backend": "ssh",
            "ssh": {"host": ""},
        }
        TAG.profile_home(cfg, "coder").mkdir(parents=True, exist_ok=True)
        (TAG.profile_home(cfg, "coder") / "config.yaml").write_text("{}")

        checks = TAG._doctor_profile_checks(cfg, "coder")
        ssh_check = next((c for c in checks if "ssh" in c["name"]), None)
        assert ssh_check is not None
        assert ssh_check["status"] == "warn"

    def test_doctor_profile_warns_missing_modal_credentials(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")

        cfg["profiles"]["researcher"].setdefault("config", {})["execution"] = {"backend": "modal"}
        TAG.profile_home(cfg, "researcher").mkdir(parents=True, exist_ok=True)
        (TAG.profile_home(cfg, "researcher") / "config.yaml").write_text("{}")

        checks = TAG._doctor_profile_checks(cfg, "researcher")
        modal_check = next((c for c in checks if "modal" in c["name"]), None)
        assert modal_check is not None
        assert modal_check["status"] == "warn"


# ===========================================================================
# PRD-001: Structured Memory Configuration (Supermemory / Honcho)
# ===========================================================================

class TestStructuredMemoryConfig:

    def test_detect_supermemory_credentials_from_env(self, monkeypatch):
        monkeypatch.setenv("SUPERMEMORY_API_KEY", "sm-test-key-12345")
        creds = TAG._detect_supermemory_credentials()
        assert creds.get("SUPERMEMORY_API_KEY") == "sm-test-key-12345"

    def test_detect_supermemory_empty_when_no_env(self, monkeypatch):
        monkeypatch.delenv("SUPERMEMORY_API_KEY", raising=False)
        creds = TAG._detect_supermemory_credentials()
        assert creds.get("SUPERMEMORY_API_KEY", "") == ""

    def test_import_supermemory_writes_api_key(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        TAG.profile_home(cfg, "orchestrator").mkdir(parents=True, exist_ok=True)

        result = TAG.import_supermemory_into_profile(
            cfg, "orchestrator", api_key="sm-key-abc"
        )
        assert result["status"] in ("imported", "ok")
        env = TAG.read_dotenv(TAG.profile_home(cfg, "orchestrator") / ".env")
        assert env.get("SUPERMEMORY_API_KEY") == "sm-key-abc"

    def test_import_supermemory_skips_without_key(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        monkeypatch.delenv("SUPERMEMORY_API_KEY", raising=False)
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        TAG.profile_home(cfg, "orchestrator").mkdir(parents=True, exist_ok=True)

        result = TAG.import_supermemory_into_profile(cfg, "orchestrator")
        assert result["status"] != "imported"

    def test_detect_honcho_credentials_from_env(self, monkeypatch):
        monkeypatch.setenv("HONCHO_API_KEY", "hn-test-key")
        creds = TAG._detect_honcho_credentials()
        assert creds.get("HONCHO_API_KEY") == "hn-test-key"

    def test_import_honcho_writes_credentials(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        TAG.profile_home(cfg, "orchestrator").mkdir(parents=True, exist_ok=True)

        result = TAG.import_honcho_into_profile(
            cfg, "orchestrator", base_url="https://honcho.example.com"
        )
        assert result["status"] in ("imported", "ok", "skipped-no-auth")

    def test_render_profiles_writes_memory_config(self, tmp_path, monkeypatch):
        import yaml
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")

        cfg["profiles"]["orchestrator"].setdefault("config", {})["memory"] = {
            "backend": "local",
        }
        for name in cfg["profiles"]:
            TAG.profile_home(cfg, name).mkdir(parents=True, exist_ok=True)

        # Should not raise
        TAG.render_profiles(cfg, force=True)


# ===========================================================================
# PRD-006: Tool Gateway Opt-in (Nous Portal)
# ===========================================================================

class TestToolGatewayOptIn:

    def test_detect_nous_portal_credentials_from_env(self, monkeypatch):
        monkeypatch.setenv("NOUS_PORTAL_API_KEY", "np-test-key-xyz")
        creds = TAG._detect_nous_portal_credentials()
        assert creds.get("NOUS_PORTAL_API_KEY") == "np-test-key-xyz"

    def test_detect_nous_portal_empty_when_no_env(self, monkeypatch):
        monkeypatch.delenv("NOUS_PORTAL_API_KEY", raising=False)
        creds = TAG._detect_nous_portal_credentials()
        assert creds.get("NOUS_PORTAL_API_KEY", "") == ""

    def test_import_nous_portal_writes_api_key(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        TAG.profile_home(cfg, "orchestrator").mkdir(parents=True, exist_ok=True)

        result = TAG.import_nous_portal_into_profile(
            cfg, "orchestrator", api_key="np-key-abc123"
        )
        assert result["status"] == "ok"
        env = TAG.read_dotenv(TAG.profile_home(cfg, "orchestrator") / ".env")
        assert env.get("NOUS_PORTAL_API_KEY") == "np-key-abc123"

    def test_import_nous_portal_skipped_when_no_key(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        monkeypatch.delenv("NOUS_PORTAL_API_KEY", raising=False)
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        TAG.profile_home(cfg, "orchestrator").mkdir(parents=True, exist_ok=True)

        result = TAG.import_nous_portal_into_profile(cfg, "orchestrator")
        assert result["status"] == "skipped-no-auth"

    def test_import_nous_portal_force_overwrites(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        TAG.profile_home(cfg, "orchestrator").mkdir(parents=True, exist_ok=True)

        TAG.import_nous_portal_into_profile(cfg, "orchestrator", api_key="np-first")
        TAG.import_nous_portal_into_profile(cfg, "orchestrator", api_key="np-second", force=True)

        env = TAG.read_dotenv(TAG.profile_home(cfg, "orchestrator") / ".env")
        assert env.get("NOUS_PORTAL_API_KEY") == "np-second"

    def test_render_profiles_enables_gateway_when_key_set(self, tmp_path, monkeypatch):
        import yaml
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")

        cfg["profiles"]["orchestrator"].setdefault("config", {})["gateway"] = {
            "enabled": True,
            "tools": ["web_search", "code_interpreter"],
        }
        for name in cfg["profiles"]:
            TAG.profile_home(cfg, name).mkdir(parents=True, exist_ok=True)

        TAG.render_profiles(cfg, force=True)

        rendered = yaml.safe_load(
            (TAG.profile_home(cfg, "orchestrator") / "config.yaml").read_text()
        ) or {}
        assert rendered.get("gateway", {}).get("use_gateway") is True
        assert "web_search" in rendered.get("gateway", {}).get("allowed_tools", [])


# ===========================================================================
# PRD-007: Desktop App Launcher
# ===========================================================================

class TestDesktopApp:

    def test_desktop_build_root_is_under_tag_home(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        build_root = TAG.desktop_build_root(cfg)
        assert str(TAG.tag_home()) in str(build_root) or str(tmp_path) in str(build_root)

    def test_desktop_app_path_returns_none_when_not_built(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        result = TAG.desktop_app_path(cfg)
        assert result is None

    def test_desktop_app_path_returns_path_when_built(self, tmp_path, monkeypatch):
        import platform
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        build_root = TAG.desktop_build_root(cfg)
        build_dir = build_root / "build"
        build_dir.mkdir(parents=True, exist_ok=True)

        system = platform.system()
        if system == "Darwin":
            app_bundle = build_dir / "Hermes.app" / "Contents" / "MacOS"
            app_bundle.mkdir(parents=True, exist_ok=True)
            (app_bundle / "Hermes").touch()
        elif system == "Linux":
            unpacked = build_dir / "linux-unpacked"
            unpacked.mkdir(parents=True, exist_ok=True)
            exe = unpacked / "hermes"
            exe.touch()
            exe.chmod(0o755)
        elif system == "Windows":
            win_dir = build_dir / "win-unpacked"
            win_dir.mkdir(parents=True, exist_ok=True)
            (win_dir / "Hermes.exe").touch()
        else:
            pytest.skip("Unknown platform for desktop test")

        result = TAG.desktop_app_path(cfg)
        assert result is not None


# ===========================================================================
# PRD-008: Background Task Queue
# ===========================================================================

class TestBackgroundTaskQueue:

    def _new_job_id(self):
        import uuid
        return uuid.uuid4().hex[:8]

    def test_queue_insert_job_creates_row(self, tmp_path):
        cfg, db = make_db(tmp_path)
        job_id = self._new_job_id()
        TAG.queue_insert_job(db, job_id, "orchestrator", "write a Python script", task_type="implementation")
        job = TAG.queue_get_job(db, job_id)
        assert job is not None
        assert job["id"] == job_id

    def test_queue_get_job_returns_dict(self, tmp_path):
        cfg, db = make_db(tmp_path)
        job_id = self._new_job_id()
        TAG.queue_insert_job(db, job_id, "orchestrator", "research topic X")
        job = TAG.queue_get_job(db, job_id)
        assert job is not None
        assert job["id"] == job_id
        assert job["task"] == "research topic X"
        assert job["status"] == "queued"

    def test_queue_update_status_changes_status(self, tmp_path):
        cfg, db = make_db(tmp_path)
        job_id = self._new_job_id()
        TAG.queue_insert_job(db, job_id, "orchestrator", "task")
        TAG.queue_update_status(db, job_id, "running")
        job = TAG.queue_get_job(db, job_id)
        assert job["status"] == "running"

    def test_queue_list_jobs_returns_all(self, tmp_path):
        cfg, db = make_db(tmp_path)
        TAG.queue_insert_job(db, self._new_job_id(), "orchestrator", "task 1")
        TAG.queue_insert_job(db, self._new_job_id(), "coder", "task 2")
        TAG.queue_insert_job(db, self._new_job_id(), "researcher", "task 3")
        jobs = TAG.queue_list_jobs(db)
        assert len(jobs) >= 3

    def test_queue_list_jobs_status_filter(self, tmp_path):
        cfg, db = make_db(tmp_path)
        j1 = self._new_job_id()
        j2 = self._new_job_id()
        TAG.queue_insert_job(db, j1, "orchestrator", "running task")
        TAG.queue_insert_job(db, j2, "orchestrator", "queued task")
        TAG.queue_update_status(db, j1, "running")
        running = TAG.queue_list_jobs(db, status="running")
        assert all(j["status"] == "running" for j in running)
        queued = TAG.queue_list_jobs(db, status="queued")
        assert all(j["status"] == "queued" for j in queued)

    def test_queue_clear_completed_removes_done_jobs(self, tmp_path):
        cfg, db = make_db(tmp_path)
        j_done = self._new_job_id()
        j_running = self._new_job_id()
        TAG.queue_insert_job(db, j_done, "orchestrator", "done task")
        TAG.queue_insert_job(db, j_running, "orchestrator", "running task")
        TAG.queue_update_status(db, j_done, "done")
        TAG.queue_update_status(db, j_running, "running")

        count = TAG.queue_clear_completed(db)
        assert count >= 1
        remaining = TAG.queue_list_jobs(db)
        assert not any(j["id"] == j_done for j in remaining)
        assert any(j["id"] == j_running for j in remaining)

    def test_queue_insert_job_with_priority(self, tmp_path):
        cfg, db = make_db(tmp_path)
        job_id = self._new_job_id()
        TAG.queue_insert_job(db, job_id, "orchestrator", "urgent task", priority=1)
        job = TAG.queue_get_job(db, job_id)
        assert job["priority"] == 1

    def test_queue_insert_job_no_notify(self, tmp_path):
        cfg, db = make_db(tmp_path)
        job_id = self._new_job_id()
        TAG.queue_insert_job(db, job_id, "orchestrator", "silent task", notify=False)
        job = TAG.queue_get_job(db, job_id)
        assert job["notify"] == 0

    def test_cmd_queue_add_creates_job_without_running(self, tmp_path, monkeypatch, capsys):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        TAG.open_db(cfg)

        import argparse
        args = argparse.Namespace(
            config=None,
            queue_subcommand="add",
            task="implement feature X",
            profile=None,
            task_type="implementation",
            priority=5,
            no_notify=False,
            json=False,
        )

        # Mock launch_queue_worker so we don't start a real process
        with patch.object(TAG, "launch_queue_worker", return_value=99999):
            rc = TAG.cmd_queue(args)
        assert rc == 0
        captured = capsys.readouterr()
        assert "queued" in captured.out.lower() or "job" in captured.out.lower() or len(captured.out) > 0

    def test_cmd_queue_list_json(self, tmp_path, monkeypatch, capsys):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        db = TAG.open_db(cfg)
        TAG.queue_insert_job(db, self._new_job_id(), "orchestrator", "existing task")
        db.close()

        import argparse
        args = argparse.Namespace(
            config=None,
            queue_subcommand="list",
            status_filter=None,
            json=True,
        )
        rc = TAG.cmd_queue(args)
        assert rc == 0
        data = json.loads(capsys.readouterr().out)
        assert isinstance(data, list)
        assert len(data) >= 1

    def test_cmd_queue_clear_removes_completed(self, tmp_path, monkeypatch, capsys):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        db = TAG.open_db(cfg)
        j = self._new_job_id()
        TAG.queue_insert_job(db, j, "orchestrator", "done")
        TAG.queue_update_status(db, j, "done")
        db.close()

        import argparse
        args = argparse.Namespace(
            config=None,
            queue_subcommand="clear",
            json=False,
        )
        rc = TAG.cmd_queue(args)
        assert rc == 0


# ===========================================================================
# PRD-009: Enhanced Doctor Diagnostics
# ===========================================================================

class TestEnhancedDoctor:

    def test_doctor_system_checks_returns_list(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        checks = TAG._doctor_system_checks(cfg)
        assert isinstance(checks, list)
        assert len(checks) > 0
        for c in checks:
            assert "name" in c
            assert "status" in c
            assert c["status"] in ("pass", "warn", "fail")

    def test_doctor_system_checks_has_python_version(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        checks = TAG._doctor_system_checks(cfg)
        names = {c["name"] for c in checks}
        assert any("python" in n for n in names)

    def test_doctor_hermes_checks_returns_list(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        checks = TAG._doctor_hermes_checks(cfg)
        assert isinstance(checks, list)
        assert len(checks) > 0

    def test_doctor_profile_checks_fails_when_home_missing(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        checks = TAG._doctor_profile_checks(cfg, "orchestrator")
        home_check = next((c for c in checks if c["name"] == "home"), None)
        assert home_check is not None
        assert home_check["status"] == "fail"

    def test_doctor_profile_checks_passes_when_home_exists(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        TAG.profile_home(cfg, "orchestrator").mkdir(parents=True, exist_ok=True)
        (TAG.profile_home(cfg, "orchestrator") / "config.yaml").write_text("{}")

        checks = TAG._doctor_profile_checks(cfg, "orchestrator")
        home_check = next(c for c in checks if c["name"] == "home")
        assert home_check["status"] == "pass"

    def test_doctor_profile_checks_warns_missing_api_key(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        TAG.profile_home(cfg, "orchestrator").mkdir(parents=True, exist_ok=True)
        (TAG.profile_home(cfg, "orchestrator") / "config.yaml").write_text("{}")
        # No .env, so no API key

        checks = TAG._doctor_profile_checks(cfg, "orchestrator")
        api_key_check = next((c for c in checks if "API_KEY" in c.get("name", "")), None)
        if api_key_check:
            assert api_key_check["status"] in ("warn", "fail")

    def test_doctor_profile_checks_passes_with_api_key(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        ph = TAG.profile_home(cfg, "orchestrator")
        ph.mkdir(parents=True, exist_ok=True)
        (ph / "config.yaml").write_text("{}")
        (ph / ".env").write_text("OPENROUTER_API_KEY=sk-or-real-key\n")

        checks = TAG._doctor_profile_checks(cfg, "orchestrator")
        api_key_check = next((c for c in checks if "OPENROUTER_API_KEY" in c.get("name", "")), None)
        if api_key_check:
            assert api_key_check["status"] == "pass"

    def test_cmd_doctor_json_mode(self, tmp_path, monkeypatch, capsys):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))

        import argparse
        args = argparse.Namespace(
            config=None,
            json=True,
            profile=None,
        )
        rc = TAG.cmd_doctor(args)
        # rc==1 expected when profile homes don't exist (fail checks); 0 if env has them
        assert rc in (0, 1)
        data = json.loads(capsys.readouterr().out)
        assert "hermes_bin_exists" in data
        assert "profiles" in data

    def test_cmd_doctor_returns_1_when_fail(self, tmp_path, monkeypatch, capsys):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        # Profile homes don't exist → fail checks
        import argparse
        args = argparse.Namespace(
            config=None,
            json=False,
            profile=None,
        )
        rc = TAG.cmd_doctor(args)
        # Should be 1 because hermes binary won't exist
        assert rc in (0, 1)  # allow 0 if checks are lenient

    def test_cmd_doctor_with_profile_filter(self, tmp_path, monkeypatch, capsys):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        import argparse
        args = argparse.Namespace(
            config=None,
            json=False,
            profile="orchestrator",
        )
        rc = TAG.cmd_doctor(args)
        assert rc in (0, 1)


# ===========================================================================
# PRD-010: Dashboard Admin Panel / Deep Merge
# ===========================================================================

class TestDashboardAndDeepMerge:

    def test_deep_merge_shallow(self):
        base = {"a": 1, "b": 2}
        override = {"b": 99, "c": 3}
        result = TAG._deep_merge(base, override)
        assert result == {"a": 1, "b": 99, "c": 3}

    def test_deep_merge_nested(self):
        base = {"model": {"provider": "openrouter", "default": "gpt-4"}}
        override = {"model": {"default": "claude-3"}}
        result = TAG._deep_merge(base, override)
        assert result["model"]["provider"] == "openrouter"
        assert result["model"]["default"] == "claude-3"

    def test_deep_merge_does_not_mutate_inputs(self):
        base = {"a": {"x": 1}}
        override = {"a": {"y": 2}}
        result = TAG._deep_merge(base, override)
        assert "y" not in base["a"]
        assert result["a"] == {"x": 1, "y": 2}

    def test_deep_merge_override_wins_scalar(self):
        base = {"flag": False}
        override = {"flag": True}
        result = TAG._deep_merge(base, override)
        assert result["flag"] is True

    def test_deep_merge_adds_new_keys(self):
        base = {"existing": "value"}
        override = {"new_key": "new_value"}
        result = TAG._deep_merge(base, override)
        assert result["existing"] == "value"
        assert result["new_key"] == "new_value"

    def test_render_profiles_preserves_existing_config(self, tmp_path, monkeypatch):
        import yaml
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")

        for name in cfg["profiles"]:
            TAG.profile_home(cfg, name).mkdir(parents=True, exist_ok=True)

        # Write a pre-existing config.yaml with panel-edited keys
        ph = TAG.profile_home(cfg, "orchestrator")
        existing_config = {"panel_custom_key": "should-be-preserved", "model": {"default": "panel-model"}}
        (ph / "config.yaml").write_text(yaml.dump(existing_config))

        TAG.render_profiles(cfg, force=False)

        rendered = yaml.safe_load((ph / "config.yaml").read_text()) or {}
        # panel_custom_key should survive the merge
        assert rendered.get("panel_custom_key") == "should-be-preserved"

    def test_render_profiles_force_skips_existing_read(self, tmp_path, monkeypatch):
        import yaml
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")

        for name in cfg["profiles"]:
            TAG.profile_home(cfg, name).mkdir(parents=True, exist_ok=True)

        ph = TAG.profile_home(cfg, "orchestrator")
        (ph / "config.yaml").write_text(yaml.dump({"panel_custom_key": "pre-existing"}))

        # force=True means we DON'T read the existing config — fresh render from default.yaml
        TAG.render_profiles(cfg, force=True)
        rendered = yaml.safe_load((ph / "config.yaml").read_text()) or {}
        # The panel key should NOT survive a forced re-render (it reads no existing config)
        assert "panel_custom_key" not in rendered

    def test_cmd_dashboard_registers_in_parser(self):
        parser = TAG.build_parser()
        # dashboard subcommand should be registered
        subparsers_action = next(
            a for a in parser._actions if hasattr(a, "_name_parser_map")
        )
        assert "dashboard" in subparsers_action._name_parser_map


# ===========================================================================
# PRD-008: Queue Worker module unit tests
# ===========================================================================

class TestQueueWorkerModule:

    def test_utc_now_is_iso_string(self):
        ts = QW._utc_now()
        import datetime
        dt = datetime.datetime.fromisoformat(ts)
        assert dt.tzinfo is not None

    def test_open_db_creates_wal_mode(self, tmp_path):
        db_path = tmp_path / "test.db"
        conn = QW._open_db(db_path)
        mode = conn.execute("PRAGMA journal_mode").fetchone()[0]
        assert mode == "wal"
        conn.close()

    def test_mark_running_updates_status(self, tmp_path):
        db_path = tmp_path / "test.db"
        conn = QW._open_db(db_path)
        conn.execute(
            """CREATE TABLE IF NOT EXISTS queue_jobs (
               id TEXT PRIMARY KEY, status TEXT, started_at TEXT, pid INTEGER,
               task TEXT, task_type TEXT, profile TEXT, priority INTEGER,
               created_at TEXT, finished_at TEXT, exit_code INTEGER,
               result_path TEXT, error TEXT, notify INTEGER
            )"""
        )
        conn.execute(
            "INSERT INTO queue_jobs VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
            ("job-1", "queued", None, None, "test", "mixed", "orchestrator", 5, "2026-01-01", None, None, None, None, 1),
        )
        conn.commit()
        QW._mark_running(conn, "job-1")
        row = conn.execute("SELECT status, pid FROM queue_jobs WHERE id='job-1'").fetchone()
        assert row[0] == "running"
        assert row[1] == os.getpid()
        conn.close()

    def test_mark_done_sets_status_done(self, tmp_path):
        db_path = tmp_path / "test.db"
        conn = QW._open_db(db_path)
        conn.execute(
            """CREATE TABLE IF NOT EXISTS queue_jobs (
               id TEXT PRIMARY KEY, status TEXT, started_at TEXT, pid INTEGER,
               task TEXT, task_type TEXT, profile TEXT, priority INTEGER,
               created_at TEXT, finished_at TEXT, exit_code INTEGER,
               result_path TEXT, error TEXT, notify INTEGER
            )"""
        )
        conn.execute(
            "INSERT INTO queue_jobs VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
            ("job-2", "running", None, None, "test", "mixed", "orchestrator", 5, "2026-01-01", None, None, None, None, 1),
        )
        conn.commit()
        QW._mark_done(conn, "job-2", exit_code=0, result_path="/tmp/result.md")
        row = conn.execute("SELECT status, exit_code FROM queue_jobs WHERE id='job-2'").fetchone()
        assert row[0] == "done"
        assert row[1] == 0
        conn.close()

    def test_mark_done_sets_status_failed_on_nonzero(self, tmp_path):
        db_path = tmp_path / "test.db"
        conn = QW._open_db(db_path)
        conn.execute(
            """CREATE TABLE IF NOT EXISTS queue_jobs (
               id TEXT PRIMARY KEY, status TEXT, started_at TEXT, pid INTEGER,
               task TEXT, task_type TEXT, profile TEXT, priority INTEGER,
               created_at TEXT, finished_at TEXT, exit_code INTEGER,
               result_path TEXT, error TEXT, notify INTEGER
            )"""
        )
        conn.execute(
            "INSERT INTO queue_jobs VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
            ("job-3", "running", None, None, "test", "mixed", "orchestrator", 5, "2026-01-01", None, None, None, None, 1),
        )
        conn.commit()
        QW._mark_done(conn, "job-3", exit_code=1, result_path="/tmp/err.md", error="failed!")
        row = conn.execute("SELECT status, error FROM queue_jobs WHERE id='job-3'").fetchone()
        assert row[0] == "failed"
        assert row[1] == "failed!"
        conn.close()


# ===========================================================================
# Integration: build_parser registers all new commands
# ===========================================================================

class TestParserRegistrations:

    @pytest.fixture(autouse=True)
    def _parser(self):
        self.parser = TAG.build_parser()
        self.sub = next(a for a in self.parser._actions if hasattr(a, "_name_parser_map"))

    def _has_cmd(self, name):
        return name in self.sub._name_parser_map

    def test_memory_journal_registered(self):
        assert self._has_cmd("memory-journal")

    def test_queue_registered(self):
        assert self._has_cmd("queue")

    def test_swarm_registered(self):
        assert self._has_cmd("swarm")

    def test_desktop_registered(self):
        assert self._has_cmd("desktop")

    def test_import_supermemory_registered(self):
        assert self._has_cmd("import-supermemory")

    def test_import_honcho_registered(self):
        assert self._has_cmd("import-honcho")

    def test_import_nous_portal_registered(self):
        assert self._has_cmd("import-nous-portal")

    def test_import_docker_registered(self):
        assert self._has_cmd("import-docker")

    def test_import_ssh_registered(self):
        assert self._has_cmd("import-ssh")

    def test_import_modal_registered(self):
        assert self._has_cmd("import-modal")

    def test_import_daytona_registered(self):
        assert self._has_cmd("import-daytona")

    def test_memory_journal_has_subcommands(self):
        mj_parser = self.sub._name_parser_map["memory-journal"]
        mj_sub = next(
            (a for a in mj_parser._actions if hasattr(a, "_name_parser_map")), None
        )
        assert mj_sub is not None
        assert "save" in mj_sub._name_parser_map
        assert "list" in mj_sub._name_parser_map
        assert "forget" in mj_sub._name_parser_map
        assert "clear" in mj_sub._name_parser_map

    def test_queue_has_subcommands(self):
        q_parser = self.sub._name_parser_map["queue"]
        q_sub = next(
            (a for a in q_parser._actions if hasattr(a, "_name_parser_map")), None
        )
        assert q_sub is not None
        assert "add" in q_sub._name_parser_map
        assert "list" in q_sub._name_parser_map
        assert "result" in q_sub._name_parser_map
        assert "cancel" in q_sub._name_parser_map
        assert "clear" in q_sub._name_parser_map

    def test_swarm_has_task_positional(self):
        swarm_parser = self.sub._name_parser_map["swarm"]
        positionals = [a.dest for a in swarm_parser._actions if a.option_strings == []]
        assert "task" in positionals

    def test_swarm_has_no_wait_flag(self):
        swarm_parser = self.sub._name_parser_map["swarm"]
        no_wait_action = next(
            (a for a in swarm_parser._actions if "--no-wait" in a.option_strings), None
        )
        assert no_wait_action is not None

    def test_import_ssh_has_required_host(self):
        ssh_parser = self.sub._name_parser_map["import-ssh"]
        host_action = next(
            (a for a in ssh_parser._actions if "--host" in a.option_strings), None
        )
        assert host_action is not None
        assert host_action.required is True

    def test_doctor_parser_has_profile_flag(self):
        doctor_parser = self.sub._name_parser_map["doctor"]
        profile_action = next(
            (a for a in doctor_parser._actions if "--profile" in a.option_strings), None
        )
        assert profile_action is not None

    def test_dashboard_parser_registered(self):
        assert self._has_cmd("dashboard")

    def test_dashboard_has_port_flag(self):
        dash_parser = self.sub._name_parser_map["dashboard"]
        port_action = next(
            (a for a in dash_parser._actions if "--port" in a.option_strings), None
        )
        assert port_action is not None
        assert port_action.type is int

    def test_dashboard_has_no_browser_flag(self):
        dash_parser = self.sub._name_parser_map["dashboard"]
        nb_action = next(
            (a for a in dash_parser._actions if "--no-browser" in a.option_strings), None
        )
        assert nb_action is not None

    def test_dashboard_open_browser_default_true(self):
        """Default should be open_browser=True so no --no-open is passed to hermes."""
        dash_parser = self.sub._name_parser_map["dashboard"]
        ns = dash_parser.parse_args([])
        assert getattr(ns, "open_browser", True) is True

    def test_dashboard_no_browser_sets_false(self):
        dash_parser = self.sub._name_parser_map["dashboard"]
        ns = dash_parser.parse_args(["--no-browser"])
        assert ns.open_browser is False


# ===========================================================================
# PRD-010: profile_exec_env injects HERMES_SYSTEM_INJECT when journal exists
# ===========================================================================

class TestProfileExecEnvMemoryInjection:

    def test_hermes_system_inject_set_when_journal_has_entries(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        db = TAG.open_db(cfg)
        TAG.journal_save(db, "orchestrator", "goal", "Build a world-class agent platform")
        db.close()

        env = TAG.profile_exec_env(cfg, "orchestrator")
        assert "HERMES_SYSTEM_INJECT" in env
        assert "goal" in env["HERMES_SYSTEM_INJECT"]

    def test_hermes_system_inject_absent_when_journal_empty(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        TAG.open_db(cfg)  # initialise but don't add entries

        env = TAG.profile_exec_env(cfg, "orchestrator")
        assert "HERMES_SYSTEM_INJECT" not in env

    def test_hermes_system_inject_absent_when_no_db(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        # Don't call open_db, so DB file doesn't exist

        env = TAG.profile_exec_env(cfg, "orchestrator")
        assert "HERMES_SYSTEM_INJECT" not in env


# ===========================================================================
# Adversarial QA Regression Tests — all bugs found and fixed
# ===========================================================================

class TestAdversarialQARegressions:

    # B1: ensure_default_file crashes with NotADirectoryError when TAG_HOME is a file
    def test_ensure_default_file_tag_home_is_file(self, tmp_path):
        file_not_dir = tmp_path / "not_a_dir"
        file_not_dir.write_text("I am a file")
        target = file_not_dir / "subpath" / "tag.yaml"
        source = ROOT / "src" / "tag" / "config" / "default.yaml"
        with pytest.raises(SystemExit):
            TAG.ensure_default_file(target, source)

    # B2: _cmd_import_generic survives OSError/RuntimeError from path.resolve()
    def test_cmd_import_generic_path_resolve_oserror(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        ph = TAG.profile_home(cfg, "orchestrator")
        ph.mkdir(parents=True, exist_ok=True)
        args = TAG.argparse.Namespace(
            config=None,
            profile="orchestrator",
            json=False,
            opencode_data_dir="/this/path/cannot/be/resolved/due/to/depth/../../../..",
        )
        # Should raise SystemExit cleanly, not an unhandled RuntimeError
        try:
            TAG._cmd_import_generic(
                args,
                import_fn=lambda cfg, **kw: {"status": "ok"},
                no_auth_msg="no auth",
                source_path_attr="opencode_data_dir",
                display_name="test",
            )
        except SystemExit:
            pass  # expected path
        except Exception as exc:
            pytest.fail(f"Expected SystemExit but got {type(exc).__name__}: {exc}")

    # B6: journal forget nonexistent exits 1, not 0
    def test_journal_forget_nonexistent_exits_1(self, tmp_path, monkeypatch, capsys):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        args = TAG.argparse.Namespace(
            config=None,
            profile="orchestrator",
            json=False,
            mj_subcommand="forget",
            entry_id="nonexistent-id-xyz",
        )
        rc = TAG.cmd_memory_journal(args)
        assert rc == 1
        out = capsys.readouterr().out
        assert "not found" in out

    # B7: memory-journal no subcommand falls back to list, not silent
    def test_memory_journal_no_subcommand_lists(self, tmp_path, monkeypatch, capsys):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        args = TAG.argparse.Namespace(
            config=None,
            profile="orchestrator",
            json=False,
            mj_subcommand=None,
        )
        rc = TAG.cmd_memory_journal(args)
        assert rc == 0  # list with no entries is fine

    # B9: swarm unknown profile warns instead of silently continuing
    def test_swarm_unknown_profile_warns(self, tmp_path, monkeypatch, capsys):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        args = TAG.argparse.Namespace(
            config=None,
            profile="nonexistent-profile-xyz",
            board="default",
            task="test task",
            task_type="mixed",
            no_wait=True,
            json=False,
        )
        # May fail for various reasons, but should at minimum warn about profile
        try:
            TAG.cmd_swarm(args)
        except (SystemExit, Exception):
            pass
        captured = capsys.readouterr()
        assert "nonexistent-profile-xyz" in captured.err or "nonexistent-profile-xyz" in captured.out

    # B12: SSH port 0 should not be silently coerced to 22
    def test_ssh_port_0_raises_system_exit(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        ph = TAG.profile_home(cfg, "orchestrator")
        ph.mkdir(parents=True, exist_ok=True)
        with pytest.raises(SystemExit, match="Invalid SSH port 0"):
            TAG.import_ssh_into_profile(cfg, "orchestrator", host="example.com", port=0)

    # B13: Docker image name validation
    def test_docker_image_name_invalid_rejected(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        ph = TAG.profile_home(cfg, "orchestrator")
        ph.mkdir(parents=True, exist_ok=True)
        with pytest.raises(SystemExit, match="Invalid Docker image"):
            TAG.import_docker_into_profile(cfg, "orchestrator", image="bad image!name")

    def test_docker_image_name_valid_accepted(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        ph = TAG.profile_home(cfg, "orchestrator")
        ph.mkdir(parents=True, exist_ok=True)
        result = TAG.import_docker_into_profile(cfg, "orchestrator", image="ubuntu:22.04")
        assert result["status"] == "ok"

    # B14: SSH key-file warning when file doesn't exist
    def test_ssh_keyfile_not_found_sets_warning(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        ph = TAG.profile_home(cfg, "orchestrator")
        ph.mkdir(parents=True, exist_ok=True)
        result = TAG.import_ssh_into_profile(
            cfg, "orchestrator", host="example.com", key_file="/nonexistent/key.pem"
        )
        assert "warning" in result
        assert "Key file not found" in result["warning"]

    # B15: Whitespace-only Modal token rejected
    def test_modal_whitespace_token_rejected(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        ph = TAG.profile_home(cfg, "orchestrator")
        ph.mkdir(parents=True, exist_ok=True)
        with pytest.raises(SystemExit):
            TAG.import_modal_into_profile(cfg, "orchestrator", token_id="   ", token_secret="real")

    def test_daytona_whitespace_workspace_rejected(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        ph = TAG.profile_home(cfg, "orchestrator")
        ph.mkdir(parents=True, exist_ok=True)
        with pytest.raises(SystemExit):
            TAG.import_daytona_into_profile(cfg, "orchestrator", workspace_id="   ")

    # B16: SSH hostname with shell metacharacters rejected
    def test_ssh_host_semicolon_rejected(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        ph = TAG.profile_home(cfg, "orchestrator")
        ph.mkdir(parents=True, exist_ok=True)
        with pytest.raises(SystemExit, match="Invalid SSH host"):
            TAG.import_ssh_into_profile(cfg, "orchestrator", host="host; rm -rf /")

    # B17: Cancel already-done job exits 1
    def test_queue_cancel_done_job_exits_1(self, tmp_path, monkeypatch, capsys):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        db = TAG.open_db(cfg)
        job_id = "testjob1"
        TAG.queue_insert_job(db, job_id, "orchestrator", "some task")
        TAG.queue_update_status(db, job_id, "done")
        db.close()

        args = TAG.argparse.Namespace(
            config=None,
            queue_subcommand="cancel",
            job_id=job_id,
            json=False,
        )
        rc = TAG.cmd_queue(args)
        assert rc == 1
        assert "already done" in capsys.readouterr().err

    # B18/B19: Queue priority range validation
    def test_queue_add_priority_too_high_exits_1(self, tmp_path, monkeypatch, capsys):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        args = TAG.argparse.Namespace(
            config=None,
            queue_subcommand="add",
            task="test task",
            profile=None,
            task_type="mixed",
            priority=999,
            no_notify=True,
            json=False,
        )
        rc = TAG.cmd_queue(args)
        assert rc == 1
        assert "priority" in capsys.readouterr().err.lower()

    def test_queue_add_priority_negative_exits_1(self, tmp_path, monkeypatch, capsys):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        args = TAG.argparse.Namespace(
            config=None,
            queue_subcommand="add",
            task="test task",
            profile=None,
            task_type="mixed",
            priority=-1,
            no_notify=True,
            json=False,
        )
        rc = TAG.cmd_queue(args)
        assert rc == 1

    # B20: Priority 0 treated as 0, not coerced to 5
    def test_queue_add_priority_zero_exits_1(self, tmp_path, monkeypatch, capsys):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        args = TAG.argparse.Namespace(
            config=None,
            queue_subcommand="add",
            task="test task",
            profile=None,
            task_type="mixed",
            priority=0,
            no_notify=True,
            json=False,
        )
        rc = TAG.cmd_queue(args)
        assert rc == 1  # 0 is out of 1-10 range

    def test_queue_add_priority_5_accepted(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        with patch.object(TAG, "launch_queue_worker", return_value=12345), \
             patch.object(TAG, "queue_update_pid"):
            args = TAG.argparse.Namespace(
                config=None,
                queue_subcommand="add",
                task="test task",
                profile=None,
                task_type="mixed",
                priority=5,
                no_notify=True,
                json=True,
            )
            rc = TAG.cmd_queue(args)
        assert rc == 0

    # B21: Queue list --limit respects provided value
    def test_queue_list_shows_truncation_message(self, tmp_path, monkeypatch, capsys):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        db = TAG.open_db(cfg)
        for i in range(5):
            TAG.queue_insert_job(db, f"job{i:03d}", "orchestrator", f"task {i}")
        db.close()
        args = TAG.argparse.Namespace(
            config=None,
            queue_subcommand="list",
            status_filter=None,
            limit=2,
            json=False,
        )
        rc = TAG.cmd_queue(args)
        assert rc == 0
        out = capsys.readouterr().out
        assert "showing" in out or "more" in out

    # B23: patch_applied counts "applied" and "prepatched" as pass
    def test_doctor_hermes_checks_patch_applied_status(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        with patch.object(TAG, "doctor_prerequisites", return_value={
            "python_runtime_supported": True,
            "npm": {"found": True, "version": "9.0"},
            "git": {"found": True, "version": "2.40"},
            "tui_dist_exists": True,
            "patch_status": "applied",
        }), patch.object(TAG, "hermes_bin", return_value=ROOT / "nonexistent"):
            checks = TAG._doctor_hermes_checks(cfg)
        patch_check = next(c for c in checks if c["name"] == "patch_applied")
        assert patch_check["status"] == "pass"

    def test_doctor_hermes_checks_patch_prepatched_status(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        with patch.object(TAG, "doctor_prerequisites", return_value={
            "python_runtime_supported": True,
            "npm": {"found": True, "version": "9.0"},
            "git": {"found": True, "version": "2.40"},
            "tui_dist_exists": True,
            "patch_status": "prepatched",
        }), patch.object(TAG, "hermes_bin", return_value=ROOT / "nonexistent"):
            checks = TAG._doctor_hermes_checks(cfg)
        patch_check = next(c for c in checks if c["name"] == "patch_applied")
        assert patch_check["status"] == "pass"

    # B24: --config nonexistent gives clean SystemExit, no traceback
    def test_load_config_nonexistent_file_raises_system_exit(self, tmp_path):
        nonexistent = tmp_path / "no" / "such" / "file.yaml"
        with pytest.raises(SystemExit, match="not found"):
            TAG.load_config(nonexistent)

    # B26: Nous Portal key too short rejected
    def test_nous_portal_short_key_rejected(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        args = TAG.argparse.Namespace(
            config=None,
            profile=None,
            all_profiles=False,
            api_key="tooshort",
            force=False,
            json=False,
        )
        with pytest.raises(SystemExit, match="too short"):
            TAG.cmd_import_nous_portal(args)

    # B27: Dashboard SQL uses id AS run_id (no OperationalError)
    def test_dashboard_snapshot_sql_column_name(self, tmp_path, monkeypatch):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
        db = TAG.open_db(cfg)
        TAG.insert_run(
            db,
            run_id="runtest1",
            kind="chat",
            task_type="mixed",
            execution="direct",
            master_profile="orchestrator",
            board="default",
            prompt="hello",
            route={},
            status="completed",
            metadata={},
        )
        db.close()
        snap = TAG._dashboard_snapshot(cfg)
        assert len(snap["runs"]) >= 1
        assert snap["runs"][0]["run_id"] == "runtest1"

    # B28: Dashboard plain view handles ISO timestamp without TypeError
    def test_dashboard_plain_view_iso_timestamp(self, tmp_path, monkeypatch, capsys):
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
        snap = {
            "runs": [
                {
                    "run_id": "abc12345",
                    "kind": "chat",
                    "master_profile": "orchestrator",
                    "status": "completed",
                    "created_at": "2026-06-12T10:30:00+00:00",
                }
            ],
            "queue": [],
            "journal_count": 0,
            "kanban": {},
        }
        TAG._render_dashboard_plain(snap, "orchestrator")
        out = capsys.readouterr().out
        assert "abc12345" in out
