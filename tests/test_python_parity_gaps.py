"""Python-side parity gaps found by the FEATURES.md audit — each mirrors a Go
fix landed earlier and now applied to the Python edition (#763). Function-local
imports keep the suite order-independent.
"""
from __future__ import annotations

import json
import os
from types import SimpleNamespace
from unittest.mock import patch

import pytest


def _home(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "home"))


def _alert(**kw):
    base = dict(config=None, alert_subcommand="create", name="r", metric=None, condition=None,
                threshold=None, metric_pos=None, condition_pos=None, threshold_pos=None,
                severity="warning", profile=None, json=False)
    base.update(kw)
    return SimpleNamespace(**base)


def test_annotate_stats_human_vs_json(tmp_path, monkeypatch, capsys):
    from tag.cmd.prd_clusters import cmd_annotate
    _home(tmp_path, monkeypatch)
    # Human default is NOT raw JSON.
    assert cmd_annotate(SimpleNamespace(config=None, annotate_subcommand="stats", json=False)) == 0
    assert not capsys.readouterr().out.strip().startswith("{")
    # --json emits JSON.
    assert cmd_annotate(SimpleNamespace(config=None, annotate_subcommand="stats", json=True)) == 0
    assert capsys.readouterr().out.strip().startswith("{")


def test_alert_list_empty_state(tmp_path, monkeypatch, capsys):
    from tag.cmd.prd_clusters import cmd_alert
    _home(tmp_path, monkeypatch)
    assert cmd_alert(SimpleNamespace(config=None, alert_subcommand="list", json=False, profile=None)) == 0
    assert "No alert rules." in capsys.readouterr().out


def test_prompt_list_empty_state(tmp_path, monkeypatch, capsys):
    from tag.cmd.prd_clusters import cmd_prompt_hub
    _home(tmp_path, monkeypatch)
    assert cmd_prompt_hub(SimpleNamespace(config=None, prompt_subcommand="list", json=False)) == 0
    assert "No prompts saved." in capsys.readouterr().out


def test_alert_create_positional_form(tmp_path, monkeypatch, capsys):
    from tag.cmd.prd_clusters import cmd_alert
    _home(tmp_path, monkeypatch)
    # Go-style positional form works.
    assert cmd_alert(_alert(name="p", metric_pos="eval_score", condition_pos="gt", threshold_pos="10")) == 0
    assert "Created rule 'p'" in capsys.readouterr().out


def test_alert_create_bad_metric_is_enumerated(tmp_path, monkeypatch, capsys):
    from tag.cmd.prd_clusters import cmd_alert
    _home(tmp_path, monkeypatch)
    rc = cmd_alert(_alert(name="p", metric_pos="cost_usd", condition_pos="gt", threshold_pos="10"))
    assert rc == 2
    err = capsys.readouterr().err
    assert "eval_score" in err and "cache_hit_rate" in err  # enumerated list


def test_alert_create_missing_fields_usage_error(tmp_path, monkeypatch):
    from tag.cmd.prd_clusters import cmd_alert
    _home(tmp_path, monkeypatch)
    assert cmd_alert(_alert(name="p")) == 2  # no metric/condition/threshold


def test_webhook_rule_add_accepts_generic():
    from tag.controller import build_parser
    ns = build_parser().parse_args(["webhook", "rule-add", "--platform", "generic",
                                    "--event", "e", "--profile", "orchestrator"])
    assert ns.platform == "generic"
    # historical platforms still accepted
    for p in ("github", "slack", "linear"):
        ns = build_parser().parse_args(["webhook", "rule-add", "--platform", p,
                                        "--event", "e", "--profile", "orchestrator"])
        assert ns.platform == p
