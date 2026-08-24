"""#763 polish backlog — Python-side parity/UX fixes that converge on the Go
harness. Function-local imports keep the suite order-independent.
"""
from __future__ import annotations

import json
import os
from types import SimpleNamespace
from unittest.mock import patch

import pytest


def _home(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "home"))


def _args(**kw):
    base = dict(config=None, json=False)
    base.update(kw)
    return SimpleNamespace(**base)


def test_runs_empty_state_prints_message(tmp_path, monkeypatch, capsys):
    from tag.cmd.routing import cmd_runs
    _home(tmp_path, monkeypatch)
    rc = cmd_runs(_args(limit=50))
    assert rc == 0
    assert capsys.readouterr().out.strip() == "No runs found."


def test_doc_check_engine_is_empty_string_not_null(tmp_path, monkeypatch, capsys):
    from tag.cmd.prd_clusters import cmd_doc
    _home(tmp_path, monkeypatch)
    # Force "no engine" so the field is exercised.
    with patch("tag.docs.engine_path", return_value=None):
        rc = cmd_doc(SimpleNamespace(config=None, doc_subcommand="check", file=None, json=True))
    assert rc == 0
    out = json.loads(capsys.readouterr().out)
    assert out["available"] is False
    assert out["engine"] == ""  # Go emits "", never null


def test_assignments_json_sorted_by_profile(tmp_path, monkeypatch, capsys):
    from tag.cmd.routing import cmd_assignments
    _home(tmp_path, monkeypatch)
    # bootstrap the default profiles via a config load path
    from tag.core.config import config_path, load_config
    load_config(config_path(None))
    rc = cmd_assignments(_args(json=True))
    assert rc == 0
    profiles = [r["profile"] for r in json.loads(capsys.readouterr().out)]
    assert profiles == sorted(profiles)


def test_doc_check_nonexistent_file_is_not_supported(tmp_path, monkeypatch, capsys):
    from tag.cmd.prd_clusters import cmd_doc
    _home(tmp_path, monkeypatch)
    missing = str(tmp_path / "nope.pdf")
    rc = cmd_doc(SimpleNamespace(config=None, doc_subcommand="check", file=missing, json=True))
    assert rc == 0
    out = json.loads(capsys.readouterr().out)
    # A file that does not exist must never be reported as supported (#763).
    assert out["supported"] is False
    assert out.get("file_error") == "file not found"


def test_graph_show_json_has_counts(tmp_path, monkeypatch, capsys):
    from tag.cmd.prd_clusters import cmd_entity_graph
    _home(tmp_path, monkeypatch)
    rc = cmd_entity_graph(SimpleNamespace(config=None, graph_subcommand="show",
                                          profile=None, json=True))
    assert rc == 0
    out = json.loads(capsys.readouterr().out)
    assert "counts" in out
    assert set(out["counts"].keys()) == {"communities", "entities", "relations"}
    assert "entities" in out and "relations" in out
