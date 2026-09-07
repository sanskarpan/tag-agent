"""Regression contracts for the terminal dashboard (issues #813 and #814)."""
import argparse
from pathlib import Path

import pytest

from tag.cmd import session
from tag.core.config import load_config
from tag.core.db import open_db, queue_insert_job
from tag.core.profile import insert_run


@pytest.fixture
def config(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag"))
    return load_config(Path(__file__).parents[1] / "src/tag/config/default.yaml")


def test_snapshot_filters_before_limit_and_filters_every_panel(config, monkeypatch):
    db = open_db(config)
    for i in range(65):
        profile = "coder" if i == 0 else "researcher"
        insert_run(db, run_id=f"run-{i}", kind="chat", task_type="mixed",
                   execution="direct", master_profile=profile, board="default",
                   prompt="test", route={}, status="completed", metadata={})
        db.execute("UPDATE runs SET created_at=? WHERE id=?", (f"2026-09-{i:02}", f"run-{i}"))
        queue_insert_job(db, job_id=f"job-{i}", profile=profile, task="test")
        db.execute("UPDATE queue_jobs SET created_at=? WHERE id=?", (f"2026-09-{i:02}", f"job-{i}"))
        db.execute("INSERT INTO memory_journal(profile,key,value,created_at) VALUES(?,?,?,?)",
                   (profile, str(i), "test", "2026-09-08"))
    db.commit()
    db.close()
    import tag.kanban as kanban
    requested = []
    def board_path(cfg, profile):
        requested.append(profile)
        return Path("/nonexistent-test-kanban")
    monkeypatch.setattr(kanban, "profile_kanban_db_path", board_path)
    snap = session._dashboard_snapshot(config, "coder")
    assert [r["run_id"] for r in snap["runs"]] == ["run-0"]
    assert [r["id"] for r in snap["queue"]] == ["job-0"]
    assert snap["journal_count"] == 1
    assert requested == ["coder"]
    assert session._dashboard_snapshot(config, "absent")["runs"] == []


@pytest.mark.parametrize("overrides", [{"port": -1}, {"port": 3333},
                                     {"open_browser": False}, {"refresh_seconds": 0},
                                     {"profile": "absent"}])
def test_invalid_contract_rejected_before_live_view(config, monkeypatch, capsys, overrides):
    monkeypatch.setattr(session, "load_config", lambda _: config)
    args = argparse.Namespace(config=None, profile="coder", port=None,
                              open_browser=True, refresh_seconds=3)
    vars(args).update(overrides)
    assert session.cmd_dashboard(args) == 2
    assert "error:" in capsys.readouterr().err


def test_refresh_parser_and_honest_help():
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers()
    session.register(sub)
    assert parser.parse_args(["dashboard", "--refresh-seconds", "1"]).refresh_seconds == 1
    for value in ("0", "-1", "nan"):
        with pytest.raises(SystemExit):
            parser.parse_args(["dashboard", "--refresh-seconds", value])
    help_text = sub.choices["dashboard"].format_help()
    assert "Terminal-only" in help_text
    assert "Print URL" not in help_text


@pytest.mark.skipif(__import__("sys").platform == "win32", reason="POSIX PTY")
def test_live_dashboard_pty_profile_scope(config, tmp_path, monkeypatch):
    import fcntl
    import os
    import pty
    import select
    import struct
    import subprocess
    import sys
    import termios
    import time
    db = open_db(config)
    for profile in ("coder", "researcher"):
        insert_run(db, run_id=f"qa-{profile}", kind="chat", task_type="mixed",
                   execution="direct", master_profile=profile, board="default",
                   prompt="synthetic", route={}, status="completed", metadata={})
    db.close()
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 35, 120, 0, 0))
    env = dict(os.environ, TERM="xterm-256color", HOME=str(tmp_path),
               PYTHONPATH=str(Path(__file__).parents[1] / "src"))
    child = subprocess.Popen([sys.executable, "-m", "tag", "dashboard", "--profile", "coder",
                              "--refresh-seconds", "1"], stdin=slave, stdout=slave,
                             stderr=slave, env=env)
    os.close(slave)
    output = b""
    try:
        deadline = time.monotonic() + 8
        while time.monotonic() < deadline and b"qa-coder" not in output:
            if select.select([master], [], [], .2)[0]:
                output += os.read(master, 65536)
        assert b"qa-coder" in output, output.decode(errors="replace")
        assert b"qa-researcher" not in output
        child.send_signal(__import__("signal").SIGINT)
        deadline = time.monotonic() + 5
        while child.poll() is None and time.monotonic() < deadline:
            if select.select([master], [], [], .1)[0]:
                try:
                    output += os.read(master, 65536)
                except OSError:
                    break
        assert child.wait(timeout=5) == 0
    finally:
        if child.poll() is None:
            child.kill()
            child.wait()
        os.close(master)
