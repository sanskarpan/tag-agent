"""Regression: the generated workflows invoked commands that do not exist.

`tag agentic-ci install-action` wrote workflows into users' repositories that
ran `tag ci review`, `tag ci test-gen`, `tag ci fix-vuln` and
`tag eval --profile …`. None of those are real:

    $ tag ci review --help
    tag ci: error: argument ci_subcommand: invalid choice: 'review'
            (choose from diagnose, commit-lint, status)

Every workflow the scaffold produced was dead on arrival. The existing tests
checked that the YAML contained workflow keys, never that the command it runs
can be invoked — so this shipped.

This test extracts the actual `run:` command from every generated workflow and
parses it with the real argparse parser.
"""
from __future__ import annotations

import contextlib
import io
import re
import shlex
import sys
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from tag.controller import build_parser  # noqa: E402
from tag.ci import _GH_ACTION_TEMPLATES, scaffold_github_action as ci_scaffold  # noqa: E402
from tag.eval_ci import scaffold_github_action as eval_scaffold  # noqa: E402


def _tag_commands(workflow_yaml: str) -> list[list[str]]:
    """Every `tag ...` invocation in the workflow's run: steps, as argv."""
    doc = yaml.safe_load(workflow_yaml)
    out: list[list[str]] = []
    for job in doc.get("jobs", {}).values():
        for step in job.get("steps", []):
            run = step.get("run")
            if not run:
                continue
            # Join line continuations, then take lines that invoke tag.
            joined = re.sub(r"\\\s*\n\s*", " ", run)
            for line in joined.splitlines():
                line = line.strip()
                if not line.startswith("tag "):
                    continue
                # Substitute shell/GitHub expansions before parsing. "1" is
                # used because it is simultaneously a valid str, int and float —
                # a word placeholder fails argparse's type= on numeric flags and
                # would report a fake defect.
                line = re.sub(r"\$\{\{[^}]*\}\}", "1", line)
                line = re.sub(r"\$[A-Za-z_][A-Za-z0-9_]*", "1", line)
                out.append(shlex.split(line)[1:])  # drop the leading "tag"
    return out


def _all_workflows():
    for wf in sorted(_GH_ACTION_TEMPLATES):
        yield f"ci:{wf}", ci_scaffold(wf)
    yield "eval_ci:eval", eval_scaffold("eval")


CASES = [(name, argv) for name, wf in _all_workflows() for argv in _tag_commands(wf)]
assert CASES, "no tag commands found in any generated workflow — the extractor is broken"


@pytest.mark.parametrize("name,argv", CASES, ids=[f"{n}:{' '.join(a[:2])}" for n, a in CASES])
def test_generated_command_parses(name, argv):
    parser = build_parser()
    buf = io.StringIO()
    try:
        with contextlib.redirect_stderr(buf), contextlib.redirect_stdout(buf):
            parser.parse_args(argv)
    except SystemExit as exc:
        pytest.fail(
            f"{name}: the generated workflow runs `tag {' '.join(argv)}`, which the CLI "
            f"rejects (exit {exc.code}):\n{buf.getvalue().strip()}"
        )


def test_every_workflow_actually_invokes_tag():
    for name, wf in _all_workflows():
        assert _tag_commands(wf), f"{name}: generated a workflow that never runs tag"
