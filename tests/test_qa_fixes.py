"""Regression tests for the 2026-08-21 QA sweep fixes (Python side)."""

from __future__ import annotations

import argparse

import yaml

from tag.cmd.routing import cmd_plugin
from tag.cmd.workflow_mgmt import cmd_template
from tag.core.config import config_path, load_config
from tag.core.profile import _config_profiles


def _seed_home(monkeypatch, tmp_path):
    home = tmp_path / "home"
    monkeypatch.setenv("TAG_HOME", str(home))
    p = config_path(None)
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(yaml.safe_dump({
        "profiles": {"coder": {"config": {"model": {"provider": "openrouter", "default": "x"}}}},
        "master_profile": "coder",
    }))
    return home, p


def test_template_import_registers_profile_in_tag_yaml(monkeypatch, tmp_path):
    # #736: import must register profiles.<name> in tag.yaml, not just write
    # runtime files and claim success for a profile nothing can see.
    _seed_home(monkeypatch, tmp_path)
    tmpl = tmp_path / "t.yaml"
    tmpl.write_text(yaml.safe_dump({"config": {"model": {"provider": "openrouter", "default": "y"}}}))

    args = argparse.Namespace(template_subcommand="import", template_file=str(tmpl),
                              file=str(tmpl), profile="newcoder", config=None)
    # cmd_template reads the file positional under different attr names across
    # versions; set the ones it may look for.
    for attr in ("template_file", "path", "file"):
        setattr(args, attr, str(tmpl))
    rc = cmd_template(args)
    assert rc == 0

    cfg = load_config(config_path(None))
    assert "newcoder" in _config_profiles(cfg)


def test_plugin_enable_rejects_unknown_profile(monkeypatch, tmp_path):
    # #756: an unknown profile must be refused (nonzero), not silently create a
    # phantom profile dir and report success.
    _seed_home(monkeypatch, tmp_path)
    args = argparse.Namespace(plugin_subcommand="enable", plugin_name="hermes-web-search",
                              profile="NOPE", config=None)
    assert cmd_plugin(args) != 0
