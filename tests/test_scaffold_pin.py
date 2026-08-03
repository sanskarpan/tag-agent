"""Regression: the generated CI workflow must pin the tag-agent install.

The scaffolds used to emit a bare ``pip install tag-agent``. That handed every
user's CI whatever PyPI served at run time, so a release that changes an exit
code — 0.10.0 changed several — silently changed the meaning of their pipeline,
with no review on their side.

Mirrors TestScaffoldPinsTheInstall in tag-go/internal/ciauto/scaffold_test.go.
"""
from __future__ import annotations

import re
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from tag import __version__  # noqa: E402
from tag.ci import scaffold_github_action as ci_scaffold  # noqa: E402
from tag.eval_ci import scaffold_github_action as eval_scaffold  # noqa: E402


def _scaffolds():
    from tag.ci import _GH_ACTION_TEMPLATES

    for wf in sorted(_GH_ACTION_TEMPLATES):
        yield f"ci:{wf}", ci_scaffold(wf)
    yield "eval_ci:eval", eval_scaffold("eval")


@pytest.mark.parametrize("name,out", list(_scaffolds()))
def test_install_is_pinned(name, out):
    assert "pip install tag-agent\n" not in out, f"{name}: unpinned install"
    assert f"tag-agent~={__version__}" in out, f"{name}: not pinned to {__version__}"


@pytest.mark.parametrize("name,out", list(_scaffolds()))
def test_no_unrendered_placeholder(name, out):
    # The ci.py templates go through str.format; a missing field would emit the
    # literal brace text into a user's workflow file.
    leftovers = re.findall(r"\{[a-z_]+\}", out)
    assert not leftovers, f"{name}: unrendered placeholders {leftovers}"


@pytest.mark.parametrize("name,out", list(_scaffolds()))
def test_generated_yaml_parses(name, out):
    # pyyaml is a hard dependency; importorskip on a required dep turns a
    # real failure into a silent skip.
    import yaml

    doc = yaml.safe_load(out)
    assert isinstance(doc, dict) and "jobs" in doc, f"{name}: not a valid workflow"
