"""Background queue worker for TAG — runs as a detached subprocess (PRD-008).

Invoked as:
    python -m tag.queue_worker --job-id JOB_ID --config CONFIG_PATH --db DB_PATH

The worker updates the queue_jobs row in SQLite and writes output to
~/.tag/runtime/queue-results/<job_id>.md when the task completes.
"""
from __future__ import annotations

import argparse
import json
import os
import signal
import sqlite3
import subprocess
import sys
import time
from pathlib import Path


def _utc_now() -> str:
    import datetime
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def _open_db(db_path: Path) -> sqlite3.Connection:
    conn = sqlite3.connect(str(db_path), timeout=10)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA busy_timeout = 5000")
    conn.execute("PRAGMA journal_mode = WAL")
    return conn


def _mark_running(conn: sqlite3.Connection, job_id: str) -> None:
    conn.execute(
        "UPDATE queue_jobs SET status='running', started_at=?, pid=? WHERE id=?",
        (_utc_now(), os.getpid(), job_id),
    )
    conn.commit()


def _mark_done(
    conn: sqlite3.Connection,
    job_id: str,
    *,
    exit_code: int,
    result_path: str,
    error: str | None = None,
) -> None:
    status = "done" if exit_code == 0 else "failed"
    conn.execute(
        """UPDATE queue_jobs
           SET status=?, finished_at=?, exit_code=?, result_path=?, error=?
           WHERE id=?""",
        (status, _utc_now(), exit_code, result_path, error, job_id),
    )
    conn.commit()


def _get_job(conn: sqlite3.Connection, job_id: str) -> dict | None:
    row = conn.execute("SELECT * FROM queue_jobs WHERE id=?", (job_id,)).fetchone()
    return dict(row) if row else None


def _run_job(job: dict, config_path: str, results_dir: Path) -> tuple[int, Path, str]:
    """Execute the job task. Returns (exit_code, result_path, error_text)."""
    result_path = results_dir / f"{job['id']}.md"
    results_dir.mkdir(parents=True, exist_ok=True)

    tag_bin = sys.executable
    cmd = [
        tag_bin,
        "-m",
        "tag",
        "--config",
        config_path,
        "submit",
        "--task-type",
        job.get("task_type", "mixed"),
        "--prompt",
        job["task"],
        "--source",
        "queue",
    ]
    if job.get("profile"):
        cmd += ["--master-profile", job["profile"]]

    error_text = ""
    try:
        proc = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            timeout=3600,  # 1-hour hard timeout
        )
        output = proc.stdout + ("\n\n---\n" + proc.stderr if proc.stderr else "")
        result_path.write_text(f"# Queue Job: {job['id']}\n\n{output}")
        return proc.returncode, result_path, ""
    except subprocess.TimeoutExpired:
        error_text = "job timed out after 3600 seconds"
        result_path.write_text(f"# Queue Job: {job['id']}\n\n**Timed out.**\n")
        return 1, result_path, error_text
    except Exception as exc:
        error_text = str(exc)
        result_path.write_text(f"# Queue Job: {job['id']}\n\n**Error:** {error_text}\n")
        return 1, result_path, error_text


def _send_notification(title: str, message: str) -> None:
    try:
        from tag.tui_output import send_desktop_notification
        send_desktop_notification(title, message)
    except Exception:
        pass


def main() -> int:
    parser = argparse.ArgumentParser(description="TAG queue worker")
    parser.add_argument("--job-id", required=True)
    parser.add_argument("--config", required=True)
    parser.add_argument("--db", required=True)
    args = parser.parse_args()

    db_path = Path(args.db)
    results_dir = db_path.parent / "queue-results"

    conn = _open_db(db_path)
    try:
        try:
            job = _get_job(conn, args.job_id)
        except sqlite3.OperationalError as exc:
            # e.g. "no such table: queue_jobs" on a fresh/foreign DB — fail
            # cleanly instead of crashing with an uncaught traceback (B096).
            print(f"queue_worker: cannot read queue_jobs ({exc})", file=sys.stderr)
            return 1
        if not job:
            print(f"queue_worker: job {args.job_id} not found", file=sys.stderr)
            return 1

        # Ignore SIGHUP so the process stays alive after terminal close
        try:
            signal.signal(signal.SIGHUP, signal.SIG_IGN)
        except (AttributeError, OSError):
            pass

        _mark_running(conn, args.job_id)

        exit_code, result_path, error = _run_job(job, args.config, results_dir)

        _mark_done(
            conn,
            args.job_id,
            exit_code=exit_code,
            result_path=str(result_path),
            error=error or None,
        )

        # Desktop notification
        if job.get("notify", 1):
            status_word = "completed" if exit_code == 0 else "failed"
            task_short = (job["task"] or "")[:60]
            _send_notification("TAG Queue", f"{job.get('profile','agent')} {status_word}: {task_short}")

        return exit_code
    finally:
        conn.close()


if __name__ == "__main__":
    sys.exit(main())

