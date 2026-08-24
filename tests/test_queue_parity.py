"""#760: `queue add` enqueues only (no auto-forked worker), `queue list --json`
returns the compact 4-field contract, and `queue worker` drains the queue —
all at parity with the Go harness.

Imports are function-local to keep the suite order-independent.
"""
from __future__ import annotations

import json
import os
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import pytest


def _cfg_and_db(tmp_path):
    from tag.core.config import config_path, load_config
    from tag.core.db import open_db
    cfg = load_config(config_path(None))
    return cfg, open_db(cfg)


def _add(**kw):
    base = dict(config=None, queue_subcommand="add", task="do a thing", profile=None,
                task_type="mixed", priority=5, no_notify=False, json=True)
    base.update(kw)
    return SimpleNamespace(**base)


def test_queue_add_is_enqueue_only_no_worker(tmp_path, monkeypatch, capsys):
    from tag.cmd.queue_dag import cmd_queue
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "home"))

    # If add ever forks a worker again, this mock records it — it must NOT be called.
    with patch("tag.cmd.queue_dag.launch_queue_worker") as forked:
        rc = cmd_queue(_add())
    assert rc == 0
    assert not forked.called, "queue add must not fork a worker (#760)"

    out = json.loads(capsys.readouterr().out)
    assert out["status"] == "queued"
    assert "pid" not in out  # Go's add JSON has no pid

    # The row is genuinely left 'queued' with no pid.
    _, db = _cfg_and_db(tmp_path)
    row = db.execute("SELECT status, pid FROM queue_jobs LIMIT 1").fetchone()
    assert row[0] == "queued"
    assert row[1] is None


def test_queue_list_json_is_the_four_field_contract(tmp_path, monkeypatch, capsys):
    from tag.cmd.queue_dag import cmd_queue
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "home"))
    cmd_queue(_add(task="alpha"))
    capsys.readouterr()  # drain

    rc = cmd_queue(SimpleNamespace(config=None, queue_subcommand="list",
                                   status_filter=None, limit=50, json=True))
    assert rc == 0
    data = json.loads(capsys.readouterr().out)
    assert isinstance(data, list) and len(data) == 1
    assert set(data[0].keys()) == {"id", "priority", "status", "task"}
    assert data[0]["status"] == "queued" and data[0]["task"] == "alpha"


def test_queue_worker_drains_and_marks_done(tmp_path, monkeypatch, capsys):
    from tag.cmd.queue_dag import cmd_queue
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "home"))
    cmd_queue(_add(task="run me"))
    capsys.readouterr()

    def fake_run(job, cfg_path, results_dir):
        p = Path(results_dir) / f"{job['id']}.md"
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text("ok")
        return 0, p, ""

    with patch("tag.queue_worker._run_job", side_effect=fake_run):
        rc = cmd_queue(SimpleNamespace(config=None, queue_subcommand="worker",
                                       max=0, watch=False, json=True))
    assert rc == 0
    summary = json.loads(capsys.readouterr().out)
    assert summary == {"claimed": 1, "done": 1, "failed": 0, "skipped": 0}

    _, db = _cfg_and_db(tmp_path)
    assert db.execute("SELECT status FROM queue_jobs LIMIT 1").fetchone()[0] == "done"


def test_queue_worker_exit_3_on_failure(tmp_path, monkeypatch, capsys):
    from tag.cmd.queue_dag import cmd_queue
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "home"))
    cmd_queue(_add(task="will fail"))
    capsys.readouterr()

    def fake_run(job, cfg_path, results_dir):
        p = Path(results_dir) / f"{job['id']}.md"
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text("boom")
        return 1, p, "boom"

    with patch("tag.queue_worker._run_job", side_effect=fake_run):
        rc = cmd_queue(SimpleNamespace(config=None, queue_subcommand="worker",
                                       max=0, watch=False, json=False))
    assert rc == 3  # ran fine, job failed — not a fabricated success
    assert "1 failed" in capsys.readouterr().out

    _, db = _cfg_and_db(tmp_path)
    assert db.execute("SELECT status FROM queue_jobs LIMIT 1").fetchone()[0] == "failed"


def test_queue_worker_empty_queue_is_clean(tmp_path, monkeypatch, capsys):
    from tag.cmd.queue_dag import cmd_queue
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "home"))
    _cfg_and_db(tmp_path)  # create the schema
    rc = cmd_queue(SimpleNamespace(config=None, queue_subcommand="worker",
                                   max=0, watch=False, json=True))
    assert rc == 0
    assert json.loads(capsys.readouterr().out) == {"claimed": 0, "done": 0, "failed": 0, "skipped": 0}


def test_queue_worker_respects_max(tmp_path, monkeypatch, capsys):
    from tag.cmd.queue_dag import cmd_queue
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "home"))
    for i in range(3):
        cmd_queue(_add(task=f"job {i}"))
    capsys.readouterr()

    def fake_run(job, cfg_path, results_dir):
        p = Path(results_dir) / f"{job['id']}.md"
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text("ok")
        return 0, p, ""

    with patch("tag.queue_worker._run_job", side_effect=fake_run):
        cmd_queue(SimpleNamespace(config=None, queue_subcommand="worker",
                                  max=2, watch=False, json=True))
    summary = json.loads(capsys.readouterr().out)
    assert summary["claimed"] == 2
    _, db = _cfg_and_db(tmp_path)
    remaining = db.execute("SELECT COUNT(*) FROM queue_jobs WHERE status='queued'").fetchone()[0]
    assert remaining == 1
