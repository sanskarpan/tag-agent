"""PRD-027: Eval Framework (tag eval).

Lightweight YAML-driven eval runner. Each eval suite is a YAML file with test
cases; each case has an input prompt, expected keywords/patterns, and optional
quality thresholds. Results are stored in SQLite for regression tracking.

Suite YAML format:
    name: My Suite
    profile: coder
    cases:
      - id: test_001
        input: "Write hello world in Python"
        expect_contains:
          - "print"
          - "Hello"
        expect_not_contains:
          - "error"
        min_length: 10
        max_length: 500
"""
from __future__ import annotations

import re
import sqlite3
import uuid
from pathlib import Path
from typing import Any

import yaml


def ensure_schema(conn: sqlite3.Connection) -> None:
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS eval_runs (
          id           TEXT PRIMARY KEY,
          suite_path   TEXT NOT NULL,
          profile      TEXT NOT NULL,
          suite_name   TEXT NOT NULL DEFAULT '',
          status       TEXT NOT NULL DEFAULT 'running',
          pass_count   INTEGER NOT NULL DEFAULT 0,
          fail_count   INTEGER NOT NULL DEFAULT 0,
          total_count  INTEGER NOT NULL DEFAULT 0,
          created_at   TEXT NOT NULL,
          completed_at TEXT
        );
        CREATE INDEX IF NOT EXISTS idx_er_status ON eval_runs(status, created_at);

        CREATE TABLE IF NOT EXISTS eval_cases (
          id           TEXT PRIMARY KEY,
          eval_run_id  TEXT NOT NULL,
          case_id      TEXT NOT NULL,
          input        TEXT NOT NULL,
          output       TEXT NOT NULL DEFAULT '',
          passed       INTEGER NOT NULL DEFAULT 0,
          score        REAL NOT NULL DEFAULT 0.0,
          failure_reason TEXT,
          created_at   TEXT NOT NULL,
          FOREIGN KEY(eval_run_id) REFERENCES eval_runs(id)
        );
        CREATE INDEX IF NOT EXISTS idx_ec_run ON eval_cases(eval_run_id, passed);
    """)
    conn.commit()


def load_suite(suite_path: Path) -> dict[str, Any]:
    """Load and validate an eval suite YAML file."""
    if not suite_path.exists():
        raise FileNotFoundError(f"Suite not found: {suite_path}")
    with suite_path.open() as fh:
        data = yaml.safe_load(fh)
    if not isinstance(data, dict):
        raise ValueError(f"Suite must be a YAML mapping, got: {type(data)}")
    if "cases" not in data:
        raise ValueError("Suite must have a 'cases' list")
    cases = data["cases"]
    if not isinstance(cases, list) or len(cases) == 0:
        raise ValueError("Suite must have at least one case")
    for i, case in enumerate(cases):
        if not isinstance(case, dict):
            raise ValueError(f"Case {i} must be a mapping, got: {type(case).__name__}")
        label = case.get("id", i)
        for key in ("expect_contains", "expect_not_contains", "expect_regex"):
            val = case.get(key)
            if val is None:
                continue
            if not isinstance(val, list) or not all(isinstance(x, str) for x in val):
                raise ValueError(
                    f"Case {label!r} field {key!r} must be a list of strings"
                )
        for key in ("min_length", "max_length"):
            val = case.get(key)
            if val is None:
                continue
            # bool is a subclass of int; reject it explicitly.
            if isinstance(val, bool) or not isinstance(val, int):
                raise ValueError(
                    f"Case {label!r} field {key!r} must be an integer"
                )
    return data


def score_case(case: dict[str, Any], output: str) -> tuple[bool, float, str | None]:
    """Score a single eval case against the model output.

    Returns (passed, score_0_to_1, failure_reason).
    """
    reasons = []
    checks = 0
    passed_checks = 0

    # expect_contains
    for keyword in case.get("expect_contains", []):
        checks += 1
        if keyword.lower() in output.lower():
            passed_checks += 1
        else:
            reasons.append(f"missing '{keyword}'")

    # expect_not_contains
    for keyword in case.get("expect_not_contains", []):
        checks += 1
        if keyword.lower() not in output.lower():
            passed_checks += 1
        else:
            reasons.append(f"should not contain '{keyword}'")

    # expect_regex
    for pattern in case.get("expect_regex", []):
        checks += 1
        if re.search(pattern, output):
            passed_checks += 1
        else:
            reasons.append(f"regex not matched: {pattern!r}")

    # min_length
    min_len = case.get("min_length")
    if min_len is not None:
        checks += 1
        if len(output) >= min_len:
            passed_checks += 1
        else:
            reasons.append(f"output too short ({len(output)} < {min_len})")

    # max_length
    max_len = case.get("max_length")
    if max_len is not None:
        checks += 1
        if len(output) <= max_len:
            passed_checks += 1
        else:
            reasons.append(f"output too long ({len(output)} > {max_len})")

    if checks == 0:
        # No checks defined — always passes
        return True, 1.0, None

    score = passed_checks / checks
    passed = len(reasons) == 0
    reason = "; ".join(reasons) if reasons else None
    return passed, score, reason


def create_eval_run(
    conn: sqlite3.Connection,
    suite_path: str,
    profile: str,
    suite_name: str = "",
) -> str:
    """Create a new eval run record. Returns the run id."""
    ensure_schema(conn)
    import datetime
    run_id = uuid.uuid4().hex[:16]
    now = datetime.datetime.now(datetime.timezone.utc).isoformat()
    conn.execute(
        """INSERT INTO eval_runs(id, suite_path, profile, suite_name, status,
           pass_count, fail_count, total_count, created_at)
           VALUES(?,?,?,?,'running',0,0,0,?)""",
        (run_id, suite_path, profile, suite_name, now),
    )
    conn.commit()
    return run_id


def record_case_result(
    conn: sqlite3.Connection,
    eval_run_id: str,
    case_id: str,
    input_text: str,
    output: str,
    *,
    passed: bool,
    score: float,
    failure_reason: str | None = None,
) -> None:
    """Record a single case result."""
    import datetime
    case_pk = uuid.uuid4().hex[:16]
    now = datetime.datetime.now(datetime.timezone.utc).isoformat()
    conn.execute(
        """INSERT INTO eval_cases(id, eval_run_id, case_id, input, output, passed, score,
           failure_reason, created_at)
           VALUES(?,?,?,?,?,?,?,?,?)""",
        (case_pk, eval_run_id, case_id, input_text, output,
         1 if passed else 0, score, failure_reason, now),
    )
    conn.commit()


def finalize_eval_run(conn: sqlite3.Connection, eval_run_id: str) -> dict:
    """Aggregate results and mark run as completed."""
    import datetime
    agg = conn.execute(
        "SELECT COUNT(*), SUM(passed), SUM(1-passed) FROM eval_cases WHERE eval_run_id=?",
        (eval_run_id,),
    ).fetchone()
    total, passes, fails = agg
    now = datetime.datetime.now(datetime.timezone.utc).isoformat()
    conn.execute(
        """UPDATE eval_runs SET status='completed', pass_count=?, fail_count=?,
           total_count=?, completed_at=? WHERE id=?""",
        (passes or 0, fails or 0, total or 0, now, eval_run_id),
    )
    conn.commit()
    return {
        "eval_run_id": eval_run_id,
        "total": total or 0,
        "passed": passes or 0,
        "failed": fails or 0,
    }


def list_eval_runs(conn: sqlite3.Connection, *, limit: int = 20) -> list[dict]:
    """List recent eval runs."""
    ensure_schema(conn)
    rows = conn.execute(
        """SELECT id, suite_path, profile, suite_name, status,
               pass_count, fail_count, total_count, created_at
           FROM eval_runs ORDER BY created_at DESC LIMIT ?""",
        (limit,),
    ).fetchall()
    return [
        {
            "id": r[0], "suite_path": r[1], "profile": r[2],
            "suite_name": r[3], "status": r[4],
            "pass_count": r[5], "fail_count": r[6], "total_count": r[7],
            "created_at": r[8],
        }
        for r in rows
    ]


def get_eval_run_detail(conn: sqlite3.Connection, run_id: str) -> dict | None:
    """Return full detail for an eval run including all case results."""
    ensure_schema(conn)
    run = conn.execute(
        "SELECT * FROM eval_runs WHERE id=?", (run_id,)
    ).fetchone()
    if not run:
        return None
    cases = conn.execute(
        "SELECT case_id, passed, score, failure_reason, created_at FROM eval_cases WHERE eval_run_id=?",
        (run_id,),
    ).fetchall()
    return {
        "id": run[0],
        "suite_path": run[1],
        "profile": run[2],
        "suite_name": run[3],
        "status": run[4],
        "pass_count": run[5],
        "fail_count": run[6],
        "total_count": run[7],
        "created_at": run[8],
        "completed_at": run[9] if len(run) > 9 else None,
        "cases": [
            {
                "case_id": c[0],
                "passed": bool(c[1]),
                "score": c[2],
                "failure_reason": c[3],
                "created_at": c[4],
            }
            for c in cases
        ],
    }

