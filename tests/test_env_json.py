"""Regression test for #757 (Python `env --json`), 2026-08-21 QA sweep."""

from __future__ import annotations

import argparse
import json

from tag.cmd.system import cmd_env


def test_env_json_emits_valid_json(monkeypatch, tmp_path, capsys):
    # #757: `env --json` must emit a JSON object (Go parity), not argparse-error
    # or silently ignore the flag.
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "home"))
    assert cmd_env(argparse.Namespace(json=True, config=None)) == 0
    obj = json.loads(capsys.readouterr().out)
    assert "HOME" in obj and "PATH" in obj

    # Without --json it stays KEY=value text.
    assert cmd_env(argparse.Namespace(json=False, config=None)) == 0
    assert "=" in capsys.readouterr().out
