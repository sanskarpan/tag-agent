"""PRD-022: Cron / Scheduled Agents.

Provides cron-style scheduling for TAG agent runs. Jobs are stored in SQLite;
a lightweight daemon process polls the table and launches queue_worker jobs
when a schedule fires.

Schedule format: standard 5-field cron ``MIN HOUR DOM MON DOW``
e.g. ``"0 9 * * 1-5"`` fires at 09:00 on weekdays.
"""
from __future__ import annotations

import re
import time
from datetime import datetime, timezone


# ---------------------------------------------------------------------------
# Minimal cron expression parser (no external deps)
# ---------------------------------------------------------------------------

def _field_matches(field: str, value: int, lo: int, hi: int) -> bool:
    """Return True if *value* matches a single cron field token."""
    if field == "*":
        return True
    # Step: */N or lo-hi/N
    if "/" in field:
        base, step_str = field.rsplit("/", 1)
        step = int(step_str)
        if base == "*":
            return (value - lo) % step == 0
        if "-" in base:
            start, end = (int(x) for x in base.split("-", 1))
            return start <= value <= end and (value - start) % step == 0
        return value == int(base)
    # Range: lo-hi
    if "-" in field:
        start, end = (int(x) for x in field.split("-", 1))
        return start <= value <= end
    # List: a,b,c
    if "," in field:
        return value in {int(x) for x in field.split(",")}
    # Literal
    return value == int(field)


# Schedulable cron aliases → their 5-field equivalents. ``@reboot`` is
# deliberately absent: a polling daemon has no "boot" event to fire it on, so it
# is rejected at validation time rather than accepted and left permanently dead.
_ALIAS_EXPANSION = {
    "@yearly":   "0 0 1 1 *",
    "@annually": "0 0 1 1 *",
    "@monthly":  "0 0 1 * *",
    "@weekly":   "0 0 * * 0",
    "@daily":    "0 0 * * *",
    "@midnight": "0 0 * * *",
    "@hourly":   "0 * * * *",
}

_CRON_ALIASES = frozenset(_ALIAS_EXPANSION)


def _dow_matches(field: str, cron_dow: int) -> bool:
    """Match a cron day-of-week field, treating both 0 and 7 as Sunday.

    *cron_dow* is already in cron space (0=Sun .. 6=Sat).
    """
    if _field_matches(field, cron_dow, 0, 7):
        return True
    # Sunday is expressible as either 0 or 7 in cron; try the alternate form.
    if cron_dow == 0 and _field_matches(field, 7, 0, 7):
        return True
    return False


def cron_matches(expr: str, dt: datetime) -> bool:
    """Return True if *dt* matches the 5-field cron expression *expr*."""
    expr_expanded = _ALIAS_EXPANSION.get(expr.strip().lower(), expr)
    parts = expr_expanded.strip().split()
    if len(parts) != 5:
        raise ValueError(f"Invalid cron expression (need 5 fields): {expr!r}")
    minute, hour, dom, month, dow = parts

    # Python's weekday() is 0=Mon..6=Sun; cron's dow is 0/7=Sun,1=Mon..6=Sat.
    cron_dow = (dt.weekday() + 1) % 7

    if not (
        _field_matches(minute, dt.minute, 0, 59)
        and _field_matches(hour, dt.hour, 0, 23)
        and _field_matches(month, dt.month, 1, 12)
    ):
        return False

    dom_match = _field_matches(dom, dt.day, 1, 31)
    dow_match = _dow_matches(dow, cron_dow)

    # POSIX: when BOTH day-of-month and day-of-week are restricted (neither is
    # ``*``), a day matches if EITHER field matches (OR). Otherwise the two are
    # combined with the rest via AND.
    if dom != "*" and dow != "*":
        return dom_match or dow_match
    return dom_match and dow_match


def validate_cron_expression(expr: str) -> None:
    """Raise ValueError if *expr* is not a valid 5-field cron expression or known alias."""
    stripped = expr.strip()
    if stripped.lower() in _CRON_ALIASES:
        return
    parts = stripped.split()
    if len(parts) != 5:
        raise ValueError(f"Cron expression must have exactly 5 fields, got: {expr!r}")
    field_names = ["minute", "hour", "day-of-month", "month", "day-of-week"]
    ranges = [(0, 59), (0, 23), (1, 31), (1, 12), (0, 7)]
    for field, name, (lo, hi) in zip(parts, field_names, ranges):
        _validate_cron_field(field, name, lo, hi, expr)


def _validate_cron_field(field: str, name: str, lo: int, hi: int, expr: str) -> None:
    """Validate a single cron field, rejecting negatives, zero steps, and
    reversed ranges — the old regex treated ``-`` purely as a range separator,
    so ``-1`` silently parsed as ``1`` and ``*/0``/``50-10`` slipped through."""
    if field == "*":
        return

    def _check_value(v: int) -> None:
        if not (lo <= v <= hi):
            raise ValueError(f"Cron {name} value {v} out of range [{lo}-{hi}] in {expr!r}")

    for item in field.split(","):
        if item == "":
            raise ValueError(f"Empty element in cron {name} field of {expr!r}")
        # Optional step: base/step
        if "/" in item:
            item, _, step_part = item.partition("/")
            if not re.fullmatch(r"[0-9]+", step_part) or int(step_part) == 0:
                raise ValueError(f"Invalid cron step {step_part!r} in {name} of {expr!r}")
        if item == "*":
            continue
        if "-" in item:
            bounds = item.split("-")
            if len(bounds) != 2 or not all(re.fullmatch(r"[0-9]+", b) for b in bounds):
                raise ValueError(f"Invalid cron range {item!r} in {name} of {expr!r}")
            start, end = int(bounds[0]), int(bounds[1])
            if start > end:
                raise ValueError(f"Reversed cron range {item!r} in {name} of {expr!r}")
            _check_value(start)
            _check_value(end)
        else:
            if not re.fullmatch(r"[0-9]+", item):
                raise ValueError(f"Invalid cron value {item!r} in {name} of {expr!r}")
            _check_value(int(item))


# ---------------------------------------------------------------------------
# Cron daemon
# ---------------------------------------------------------------------------

def run_daemon(db_path: str, config_path: str) -> None:
    """Poll cron_jobs every 30 seconds and fire due jobs via queue_worker."""
    import sqlite3
    import subprocess
    import sys
    import uuid

    def _open(path: str) -> sqlite3.Connection:
        conn = sqlite3.connect(path, timeout=10)
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA journal_mode = WAL")
        conn.execute("PRAGMA busy_timeout = 5000")
        return conn

    def _utc_now() -> str:
        return datetime.now(timezone.utc).isoformat()

    conn = _open(db_path)
    try:
        conn.executescript("""
            CREATE TABLE IF NOT EXISTS cron_jobs (
              id          TEXT PRIMARY KEY,
              name        TEXT NOT NULL,
              schedule    TEXT NOT NULL,
              profile     TEXT NOT NULL,
              task        TEXT NOT NULL,
              enabled     INTEGER NOT NULL DEFAULT 1,
              last_run_at TEXT,
              run_count   INTEGER NOT NULL DEFAULT 0,
              created_at  TEXT NOT NULL,
              updated_at  TEXT NOT NULL
            );
        """)
        conn.commit()
    except Exception:
        pass

    while True:
        now = datetime.now(timezone.utc)
        try:
            jobs = conn.execute(
                "SELECT * FROM cron_jobs WHERE enabled=1"
            ).fetchall()
        except Exception:
            time.sleep(30)
            continue

        for job in jobs:
            try:
                if not cron_matches(job["schedule"], now):
                    continue
                last = job["last_run_at"]
                if last:
                    last_dt = datetime.fromisoformat(last)
                    # Prevent firing twice in the same minute
                    if (now - last_dt).total_seconds() < 55:
                        continue
            except Exception:
                continue

            # Queue a job
            job_id = uuid.uuid4().hex[:8]
            try:
                conn.execute(
                    """INSERT INTO queue_jobs(id, profile, task, task_type, status, priority,
                       created_at, notify) VALUES(?,?,?,?,?,?,?,?)""",
                    (job_id, job["profile"], job["task"], "mixed", "queued", 5,
                     _utc_now(), 1),
                )
                conn.execute(
                    "UPDATE cron_jobs SET last_run_at=?, run_count=run_count+1, updated_at=? WHERE id=?",
                    (_utc_now(), _utc_now(), job["id"]),
                )
                conn.commit()

                # Launch worker
                subprocess.Popen(
                    [sys.executable, "-m", "tag.queue_worker",
                     "--job-id", job_id,
                     "--config", config_path,
                     "--db", db_path],
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL,
                    start_new_session=True,
                )
            except Exception:
                pass

        time.sleep(30)

