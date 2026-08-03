"""Regression: a SARIF the tool cannot understand must not read as "clean".

``parse_sarif`` used ``data.get("runs", [])``, so a top-level array, an object
with no ``runs`` key, or ``{"hello": 1}`` all produced zero findings — which the
CLI printed as "No vulnerabilities found" and exited 0. On a security gate,
"I could not understand this file" and "this file is clean" must never produce
the same answer.

The Go harness already raises ErrMalformedSARIF and exits 2 here; this pins the
Python side to the same contract.
"""
from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from tag.ci import parse_sarif  # noqa: E402

MALFORMED = {
    "top-level array": "[]",
    "top-level scalar": '"hello"',
    "no runs key": '{"hello": 1}',
    "runs is a string": '{"runs": "x"}',
    "runs entry is a scalar": '{"runs": [1]}',
    "results is a string": '{"runs": [{"results": "x"}]}',
}


@pytest.mark.parametrize("name,body", sorted(MALFORMED.items()))
def test_malformed_sarif_raises(tmp_path, name, body):
    p = tmp_path / "in.sarif"
    p.write_text(body)
    with pytest.raises(ValueError) as exc:
        parse_sarif(p)
    assert "alformed" in str(exc.value), f"{name}: message should say malformed"


def test_valid_empty_sarif_is_genuinely_clean(tmp_path):
    p = tmp_path / "in.sarif"
    p.write_text(json.dumps({"version": "2.1.0", "runs": []}))
    assert parse_sarif(p) == []


def test_valid_sarif_with_a_finding_still_parses(tmp_path):
    p = tmp_path / "in.sarif"
    p.write_text(json.dumps({
        "version": "2.1.0",
        "runs": [{
            "tool": {"driver": {"rules": [{"id": "R1", "defaultConfiguration": {"level": "error"}}]}},
            "results": [{
                "ruleId": "R1",
                "message": {"text": "boom"},
                "locations": [{"physicalLocation": {
                    "artifactLocation": {"uri": "a.py"}, "region": {"startLine": 3}}}],
            }],
        }],
    }))
    got = parse_sarif(p)
    assert len(got) == 1
    assert got[0]["rule_id"] == "R1" and got[0]["start_line"] == 3


def _run(tmp_path, body):
    p = tmp_path / "in.sarif"
    p.write_text(body)
    r = subprocess.run(
        [sys.executable, "-m", "tag", "agentic-ci", "fix-vuln", str(p)],
        capture_output=True, text=True,
        env={"PATH": "/usr/bin:/bin", "PYTHONPATH": str(ROOT / "src"),
             "TAG_HOME": str(tmp_path / "home"), "HOME": str(tmp_path)},
    )
    return r.returncode, r.stdout + r.stderr


@pytest.mark.parametrize("name,body", sorted(MALFORMED.items()))
def test_cli_exits_2_on_malformed(tmp_path, name, body):
    code, out = _run(tmp_path, body)
    assert code == 2, f"{name}: exit {code}, want 2 (usage)\n{out}"
    assert "No vulnerabilities found" not in out, f"{name}: reported clean on an unparsed file"


def test_cli_exits_0_on_genuinely_clean(tmp_path):
    code, out = _run(tmp_path, json.dumps({"version": "2.1.0", "runs": []}))
    assert code == 0, out
    assert "No vulnerabilities found" in out
