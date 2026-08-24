"""#755/#761: Python set-model/models accept the Go-style positional form."""

from __future__ import annotations

import argparse

import yaml

from tag.cmd.routing import cmd_set_model
from tag.core.config import config_path


def _home(monkeypatch, tmp_path):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "h"))
    p = config_path(None)
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(yaml.safe_dump({
        "profiles": {"coder": {"config": {"model": {"provider": "openrouter", "default": "x"}}}},
        "defaults": {"master_profile": "coder"},
    }))
    return p


def _args(**kw):
    ns = argparse.Namespace(profile=None, ref=None, profile_pos=None, ref_pos=None,
                            target="primary", openai_runtime=None, json=False, config=None)
    for k, v in kw.items():
        setattr(ns, k, v)
    return ns


def test_set_model_accepts_positional(monkeypatch, tmp_path):
    p = _home(monkeypatch, tmp_path)
    # Go-style positional form.
    assert cmd_set_model(_args(profile_pos="coder", ref_pos="openrouter/pos")) == 0
    assert yaml.safe_load(p.read_text())["profiles"]["coder"]["config"]["model"]["default"] == "pos"


def test_set_model_accepts_flags(monkeypatch, tmp_path):
    p = _home(monkeypatch, tmp_path)
    assert cmd_set_model(_args(profile="coder", ref="openrouter/flg")) == 0
    assert yaml.safe_load(p.read_text())["profiles"]["coder"]["config"]["model"]["default"] == "flg"


def test_set_model_missing_both_is_usage_error(monkeypatch, tmp_path):
    _home(monkeypatch, tmp_path)
    assert cmd_set_model(_args()) == 2
