"""#748: Python distribution ships the guardrail/tripwire engine + CLI surface,
at parity with the Go harness (PRD-123).

Engine tests import tag.core.guardrail (a leaf module) at module scope. The CLI
tests import tag.cmd.guardrail / build_parser inside the test functions so the
suite stays order-independent (see test_arg_parity.py).
"""
from __future__ import annotations

import json
import os
import sqlite3
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import pytest

from tag.core import guardrail as gr


# ---- engine: config validation (fail closed at load) ----------------------

def test_bad_regex_is_a_load_error():
    with pytest.raises(ValueError, match="invalid pattern"):
        gr.parse_layer({"rules": [{"name": "r", "pattern": "(unclosed"}]}, "t")


def test_duplicate_name_is_a_load_error():
    with pytest.raises(ValueError, match="duplicate"):
        gr.parse_layer({"rules": [
            {"name": "dup", "pattern": "a"}, {"name": "dup", "pattern": "b"}]}, "t")


def test_tripwire_needs_positive_threshold():
    with pytest.raises(ValueError, match="positive 'threshold'"):
        gr.parse_layer({"rules": [{"name": "t", "type": "tripwire", "tool": "x"}]}, "t")


def test_require_approval_takes_no_pattern():
    with pytest.raises(ValueError, match="require-approval"):
        gr.parse_layer({"rules": [
            {"name": "ra", "type": "require-approval", "pattern": "x"}]}, "t")


def test_unknown_builtin_rejected():
    with pytest.raises(ValueError, match="unknown builtin"):
        gr.parse_layer({"rules": [{"name": "b", "builtin": "secret"}]}, "t")


def test_unknown_preset_rejected():
    with pytest.raises(ValueError, match="unknown tripwire.preset"):
        gr.parse_layer({"preset": "nope"}, "t")


# ---- engine: detection ----------------------------------------------------

def _proc(rules_block):
    rules = gr.parse_layer(rules_block, "t")
    return gr.Processor(rules=rules)


def test_secrets_builtin_detects_aws_key_and_redacts():
    p = _proc({"rules": [{"name": "s", "builtin": "secrets", "stage": "tool_output", "action": "block"}]})
    v = p.scan(gr.STAGE_TOOL_OUTPUT, "", "tok=AKIAIOSFODNN7EXAMPLE")
    assert v.blocked and v.fired()
    assert v.findings[0].detector == "secrets:aws-access-key-id"
    # The secret must be FULLY masked — not even the first characters may leak
    # into the excerpt or the persisted history (#763 security).
    exc = v.findings[0].excerpt
    assert "AKIAIOSFODNN7EXAMPLE" not in exc
    assert "AKIAIO" not in exc  # no 6-char prefix leak
    assert "redacted" in exc


def test_destructive_builtin_detects_rm_rf_root():
    p = _proc({"rules": [{"name": "d", "builtin": "destructive", "tool": "bash",
                          "stage": "tool_input", "action": "block"}]})
    v = p.scan_args("bash", {"cmd": "rm -rf /"})
    assert v.blocked
    assert v.findings[0].detector == "destructive:rm-rf-root"


def test_clean_content_does_not_fire():
    p = _proc({"rules": [{"name": "s", "builtin": "secrets", "action": "block"}]})
    v = p.scan(gr.STAGE_MODEL_OUTPUT, "", "just some ordinary text")
    assert not v.fired() and not v.blocked


def test_placeholder_secret_is_not_a_finding():
    p = _proc({"rules": [{"name": "s", "builtin": "secrets", "action": "block"}]})
    v = p.scan(gr.STAGE_MODEL_OUTPUT, "", "api_key = YOUR_API_KEY_HERE")
    assert not v.fired()


# ---- engine: precedence & fail-closed -------------------------------------

def test_block_outranks_interrupt_and_warn_in_outcome():
    v = gr.Verdict(stage="s", blocked=True, interrupted=True, warned=True)
    assert v.outcome() == gr.ACTION_BLOCK


def test_scan_args_fails_closed_on_unserialisable_content():
    p = _proc({"rules": [{"name": "s", "builtin": "secrets", "stage": "tool_input", "action": "block"}]})

    class Bad:
        pass
    # default=str lets most things through; use a self-referential structure that
    # json cannot serialise even with default=str.
    circular: dict = {}
    circular["self"] = circular
    v = p.scan_args("bash", circular)
    assert v.blocked and v.undecidable


# ---- engine: tripwire counter ---------------------------------------------

def test_tripwire_fires_exactly_at_threshold():
    conn = sqlite3.connect(":memory:")
    p = gr.Processor(rules=gr.parse_layer(
        {"rules": [{"name": "flood", "type": "tripwire", "tool": "http_*",
                    "threshold": 3, "window": "1h", "action": "block"}]}, "t"),
        conn=conn, session_id="s1")
    outcomes = [p.scan(gr.STAGE_TOOL_INPUT, "http_get", "{}").blocked for _ in range(3)]
    assert outcomes == [False, False, True]


def test_tripwire_fails_closed_without_a_store():
    p = gr.Processor(rules=gr.parse_layer(
        {"rules": [{"name": "t", "type": "tripwire", "threshold": 1, "action": "block"}]}, "t"),
        conn=None)
    v = p.scan(gr.STAGE_TOOL_INPUT, "x", "{}")
    assert v.blocked and v.undecidable


# ---- engine: duration parsing ---------------------------------------------

@pytest.mark.parametrize("text,secs", [("1h", 3600), ("30m", 1800), ("1h30m", 5400), ("45s", 45)])
def test_parse_duration(text, secs):
    assert gr.parse_duration_seconds(text) == secs


def test_parse_duration_rejects_garbage():
    with pytest.raises(ValueError):
        gr.parse_duration_seconds("garbage")


# ---- CLI: parser accepts the command surface ------------------------------

def test_parser_exposes_tripwire_and_guardrail():
    from tag.controller import build_parser

    ns = build_parser().parse_args(["tripwire", "check", "--text", "x", "--stage", "model_output"])
    assert ns.text == "x" and ns.stage == "model_output"
    ns = build_parser().parse_args(["guardrail", "runtime", "add", "--name", "r", "--pattern", "p"])
    assert ns.name == "r" and ns.pattern == "p"
    ns = build_parser().parse_args(["guardrail", "runtime", "list"])
    assert hasattr(ns, "func")


# ---- CLI: handlers end-to-end against a temp config -----------------------

def _write_cfg(tmp_path: Path, block: dict) -> Path:
    import yaml
    cfg = {"lab_name": "t", "defaults": {"master_profile": "orchestrator"},
           "profiles": {"orchestrator": {"config": {}}}, "tripwire": block}
    p = tmp_path / "tag.yaml"
    p.write_text(yaml.safe_dump(cfg, sort_keys=False))
    return p


def test_cli_check_fires_exit_3(tmp_path):
    from tag.cmd import guardrail as cmd
    cfg_path = _write_cfg(tmp_path, {"preset": "standard"})
    with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "home")}):
        args = SimpleNamespace(config=str(cfg_path), stage="tool_output",
                               text="tok=AKIAIOSFODNN7EXAMPLE", file="", stdin=False,
                               session="", profile=None, exit_zero=False, json=False)
        assert cmd.cmd_tripwire_check(args) == 3


def test_cli_check_clean_exit_0(tmp_path):
    from tag.cmd import guardrail as cmd
    cfg_path = _write_cfg(tmp_path, {"preset": "standard"})
    with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "home")}):
        args = SimpleNamespace(config=str(cfg_path), stage="tool_output",
                               text="nothing to see", file="", stdin=False,
                               session="", profile=None, exit_zero=False, json=False)
        assert cmd.cmd_tripwire_check(args) == 0


def test_cli_check_no_rules_is_honest_usage_error(tmp_path, capsys):
    from tag.cmd import guardrail as cmd
    cfg_path = _write_cfg(tmp_path, {})  # empty tripwire block => no rules
    with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "home")}):
        args = SimpleNamespace(config=str(cfg_path), stage="model_output",
                               text="x", file="", stdin=False, session="",
                               profile=None, exit_zero=False, json=False)
        assert cmd.cmd_tripwire_check(args) == 2  # never a fabricated pass


def test_cli_add_then_remove_roundtrip(tmp_path):
    from tag.cmd import guardrail as cmd
    import yaml
    cfg_path = _write_cfg(tmp_path, {})
    with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "home")}):
        add = SimpleNamespace(config=str(cfg_path), profile="", name="r1", tool="bash",
                              type="", pattern="danger", builtin="", stage="tool_input",
                              action="block", threshold=0, window="", message="", json=False)
        assert cmd.cmd_guardrail_add(add) == 0
        data = yaml.safe_load(cfg_path.read_text())
        assert any(r["name"] == "r1" for r in data["tripwire"]["rules"])

        # duplicate is refused
        assert cmd.cmd_guardrail_add(add) == 2

        rem = SimpleNamespace(config=str(cfg_path), profile="", name="r1", json=False)
        assert cmd.cmd_guardrail_remove(rem) == 0
        assert cmd.cmd_guardrail_remove(rem) == 2  # not found now


def test_cli_add_rejects_bad_regex(tmp_path):
    from tag.cmd import guardrail as cmd
    cfg_path = _write_cfg(tmp_path, {})
    with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "home")}):
        add = SimpleNamespace(config=str(cfg_path), profile="", name="bad", tool="",
                              type="", pattern="(unclosed", builtin="", stage="",
                              action="block", threshold=0, window="", message="", json=False)
        assert cmd.cmd_guardrail_add(add) == 2
