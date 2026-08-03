"""Regression: the fresh-install path raised NameError.

``ensure_hermes_ready`` called ``cmd_setup(setup_args)`` as a bare global.
``cmd_setup`` reaches ``tag.controller`` only through its ``_CMD_ATTR_MAP``
lazy re-export, and a module-level ``__getattr__`` is consulted for attribute
access on the *module* — never for a bare name lookup inside a function defined
in that same module. So the call raised
``NameError: name 'cmd_setup' is not defined``.

It only fired when the hermes binary was absent (the function returns early
otherwise), so every already-bootstrapped machine and CI run skipped it. A user
installing tag-agent and running ``tag submit`` hit it on their first command.
"""
from __future__ import annotations

import sys
from pathlib import Path
from unittest.mock import patch

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from tag import controller  # noqa: E402


def test_ensure_hermes_ready_invokes_setup_when_binary_missing(tmp_path):
    cfg = {"paths": {"root": str(tmp_path)}}
    missing = tmp_path / "definitely-not-here" / "hermes"

    with patch.object(controller, "hermes_bin", return_value=missing), \
         patch("tag.cmd.system.cmd_setup", return_value=0) as setup:
        # The bug: this raised NameError instead of calling setup.
        controller.ensure_hermes_ready(cfg, config_arg=None, need_tui=False)

    assert setup.called, "setup must run when the hermes binary is absent"
    ns = setup.call_args[0][0]
    assert ns.skip_tui_build is True, "need_tui=False should skip the TUI build"


def test_ensure_hermes_ready_is_a_noop_when_binary_exists(tmp_path):
    present = tmp_path / "hermes"
    present.write_text("#!/bin/sh\n")

    with patch.object(controller, "hermes_bin", return_value=present), \
         patch("tag.cmd.system.cmd_setup", return_value=0) as setup:
        controller.ensure_hermes_ready({}, config_arg=None, need_tui=False)

    assert not setup.called, "an existing binary must not trigger setup"


def test_need_tui_is_threaded_through(tmp_path):
    missing = tmp_path / "nope" / "hermes"
    with patch.object(controller, "hermes_bin", return_value=missing), \
         patch("tag.cmd.system.cmd_setup", return_value=0) as setup:
        controller.ensure_hermes_ready({}, config_arg="cfg.yaml", need_tui=True)
    ns = setup.call_args[0][0]
    assert ns.skip_tui_build is False
    assert ns.config == "cfg.yaml"
