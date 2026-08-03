"""Regression: the eval scorer graded the wrong text, and unchecked cases passed.

Four defects in one subsystem, all of which made a suite report a score it had
not earned:

1. The runner scored ``result.stdout`` of ``tag submit`` — the submission
   acknowledgement ("run_id: … status: queued"), not the model's answer.
2. ``expected_output`` was accepted in suites and never checked, and
   ``eval-dataset export`` emits cases whose ONLY field is ``expected_output`` —
   so every dataset-derived suite had zero checks and passed unconditionally.
3. A case with no assertions returned a bare ``(True, 1.0)``, indistinguishable
   from a case that was actually verified.
4. ``--dry-run`` wrote ``passed=1, score=1.0`` rows and a ``completed`` run.
"""
from __future__ import annotations

import json
import subprocess
import sys
import types
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from tag.eval_framework import NO_CHECKS_REASON, score_case  # noqa: E402
from tag.cmd.marketplace import _extract_model_output  # noqa: E402


# --- expected_output is a real check -----------------------------------------

def test_expected_output_is_scored():
    ok, score, reason = score_case({"expected_output": "hello world"}, "goodbye")
    assert not ok and score == 0.0 and "expected_output" in reason


def test_expected_output_matches_on_normalised_text():
    ok, _, _ = score_case({"expected_output": "Hello   World\n"}, "hello world")
    assert ok, "whitespace/case differences must not fail an otherwise-correct answer"


def test_dataset_derived_case_can_now_fail():
    # `eval-dataset export` emits exactly this shape.
    case = {"id": "c1", "expected_output": "42"}
    ok, _, _ = score_case(case, "definitely not the answer")
    assert not ok, "a dataset-derived suite must be able to fail"


# --- a case with no assertions is labelled -----------------------------------

def test_zero_check_case_is_labelled():
    ok, score, reason = score_case({"id": "c1"}, "whatever")
    assert ok and score == 1.0, "it still cannot fail — inventing a failure is its own lie"
    assert reason == NO_CHECKS_REASON, "but it must say it verified nothing"


def test_checked_case_has_no_marker():
    _, _, reason = score_case({"expect_contains": ["a"]}, "a")
    assert reason is None


# --- the model's output, not the receipt -------------------------------------

def _result(returncode=0, stdout="", stderr=""):
    return types.SimpleNamespace(returncode=returncode, stdout=stdout, stderr=stderr)


def test_extracts_model_output_from_steps():
    payload = {"run_id": "r1", "status": "queued",
               "steps": [{"role": "worker", "output": "THE ANSWER"},
                         {"role": "worker", "output": "MORE"}]}
    out, err = _extract_model_output(_result(stdout=json.dumps(payload)))
    assert err is None
    assert "THE ANSWER" in out and "MORE" in out
    assert "run_id" not in out, "the receipt must not be scored"


@pytest.mark.parametrize("name,res", [
    ("submit failed", _result(returncode=1, stderr="error: boom")),
    ("not json", _result(stdout="run_id: r1\nstatus: queued\n")),
    ("no steps", _result(stdout=json.dumps({"run_id": "r1", "status": "queued"}))),
    ("steps with no output", _result(stdout=json.dumps({"steps": [{"role": "worker"}]}))),
])
def test_refuses_to_score_when_output_is_unavailable(name, res):
    out, err = _extract_model_output(res)
    assert err, f"{name}: must report why, not return text to score"
    assert out == "", f"{name}: must not hand back partial text"


def test_the_old_receipt_text_would_now_be_refused():
    # This is verbatim what the scorer used to receive and grade.
    receipt = "run_id: run-mixed-4a54aa3687\nstatus: queued\nresearcher: ok\ncoder: ok\n"
    out, err = _extract_model_output(_result(stdout=receipt))
    assert err and out == ""


# --- an interrupted run must not be stranded ---------------------------------

def test_interrupted_run_is_marked_cancelled(tmp_path, monkeypatch):
    """An interrupted eval left status='running' forever.

    `eval list` then accumulated phantom in-flight runs that nothing would ever
    finish, and a caller could not tell a run still going from one that died.
    """
    import sqlite3
    import tag.cmd.marketplace as mk

    suite = tmp_path / "s.yaml"
    suite.write_text("name: s\ncases:\n  - id: c1\n    input: hi\n    expect_contains: [hi]\n")

    monkeypatch.setenv("TAG_HOME", str(tmp_path / "home"))

    # Interrupt during the first case, exactly as Ctrl-C would.
    # cmd_eval imports subprocess locally, so patch the module it resolves to.
    import subprocess as _sp

    def boom(*a, **kw):
        raise KeyboardInterrupt

    monkeypatch.setattr(_sp, "run", boom)

    args = mk.argparse.Namespace(
        eval_subcommand="run", suite=str(suite), profile="orchestrator",
        dry_run=False, config=None, json=False,
    )
    rc = mk.cmd_eval(args)
    assert rc == 130, f"an interrupted run should report 130, got {rc}"

    # Find the run row and confirm it is not stranded.
    home = tmp_path / "home"
    dbs = list(home.rglob("*.sqlite3")) + list(home.rglob("*.db"))
    assert dbs, "no database was created"
    for p in dbs:
        conn = sqlite3.connect(p)
        try:
            rows = conn.execute("SELECT status FROM eval_runs").fetchall()
        except sqlite3.OperationalError:
            continue
        finally:
            conn.close()
        if rows:
            assert all(r[0] != "running" for r in rows), \
                f"an interrupted run was left in 'running': {rows}"
            return
