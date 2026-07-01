"""PRD-021: Agent Loop / Autonomous Mode.

Runs a TAG profile in a goal-directed loop with an iteration cap, human-approval
gates, and a per-loop journal stored in SQLite. Launched as a detached subprocess
(like queue_worker) via ``tag loop start``.
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
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS loop_runs (
          id           TEXT PRIMARY KEY,
          profile      TEXT NOT NULL,
          goal         TEXT NOT NULL,
          max_iters    INTEGER NOT NULL DEFAULT 10,
          current_iter INTEGER NOT NULL DEFAULT 0,
          status       TEXT NOT NULL DEFAULT 'running',
          approval     TEXT NOT NULL DEFAULT 'auto',
          created_at   TEXT NOT NULL,
          updated_at   TEXT NOT NULL,
          completed_at TEXT
        );
        CREATE INDEX IF NOT EXISTS idx_lr_status ON loop_runs(status, created_at);

        CREATE TABLE IF NOT EXISTS loop_iterations (
          id         TEXT PRIMARY KEY,
          loop_id    TEXT NOT NULL,
          iteration  INTEGER NOT NULL,
          input      TEXT NOT NULL,
          output     TEXT NOT NULL DEFAULT '',
          decision   TEXT NOT NULL DEFAULT 'continue',
          created_at TEXT NOT NULL,
          FOREIGN KEY(loop_id) REFERENCES loop_runs(id)
        );
        CREATE INDEX IF NOT EXISTS idx_li_loop ON loop_iterations(loop_id, iteration);
    """)
    conn.commit()
    return conn


def _mark_iteration(
    conn: sqlite3.Connection,
    loop_id: str,
    iteration: int,
    *,
    input_text: str,
    output: str = "",
    decision: str = "continue",
) -> str:
    import uuid
    iter_id = uuid.uuid4().hex[:12]
    now = _utc_now()
    conn.execute(
        "INSERT INTO loop_iterations(id, loop_id, iteration, input, output, decision, created_at) "
        "VALUES(?,?,?,?,?,?,?)",
        (iter_id, loop_id, iteration, input_text, output, decision, now),
    )
    conn.execute(
        "UPDATE loop_runs SET current_iter=?, updated_at=? WHERE id=?",
        (iteration, now, loop_id),
    )
    conn.commit()
    return iter_id


def _current_status(conn: sqlite3.Connection, loop_id: str) -> str | None:
    row = conn.execute("SELECT status FROM loop_runs WHERE id=?", (loop_id,)).fetchone()
    return row[0] if row else None


def _is_aborted(conn: sqlite3.Connection, loop_id: str) -> bool:
    """True if the loop was aborted externally (e.g. `tag loop abort`)."""
    return _current_status(conn, loop_id) == "aborted"


def _update_loop_status(conn: sqlite3.Connection, loop_id: str, status: str) -> None:
    now = _utc_now()
    completed = now if status in ("completed", "failed", "aborted", "max_iters") else None
    conn.execute(
        "UPDATE loop_runs SET status=?, updated_at=?, completed_at=? WHERE id=?",
        (status, now, completed, loop_id),
    )
    conn.commit()


def _run_iteration(
    loop_id: str,
    iteration: int,
    goal: str,
    profile: str,
    config_path: str,
    previous_output: str,
) -> tuple[str, int]:
    """Run one agent iteration. Returns (output, exit_code)."""
    if previous_output:
        prompt = (
            f"Goal: {goal}\n\n"
            f"Previous iteration output:\n{previous_output[:2000]}\n\n"
            f"This is iteration {iteration}. Continue working toward the goal, "
            f"or output GOAL_ACHIEVED if the goal has been met."
        )
    else:
        prompt = (
            f"Goal: {goal}\n\n"
            f"This is iteration {iteration} of an autonomous agent loop. "
            f"Work toward achieving the goal. Output GOAL_ACHIEVED when done."
        )

    tag_bin = sys.executable
    cmd = [
        tag_bin, "-m", "tag",
        "--config", config_path,
        "submit",
        "--task-type", "mixed",
        "--prompt", prompt,
        "--master-profile", profile,
        "--source", "loop",
    ]
    try:
        proc = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            timeout=600,
        )
        return proc.stdout + (proc.stderr or ""), proc.returncode
    except subprocess.TimeoutExpired:
        return "Iteration timed out (600s)", 1
    except Exception as exc:
        return f"Iteration error: {exc}", 1


def _is_goal_achieved(output: str) -> bool:
    return "GOAL_ACHIEVED" in output


def _request_approval(
    loop_id: str,
    iteration: int,
    output: str,
    approval_file: Path,
    conn: sqlite3.Connection | None = None,
    timeout_seconds: int = 300,
) -> bool:
    """Write a pending approval request; poll for user decision.

    A user approves/denies via `tag loop approve|deny <loop_id>`, which writes
    the decision into *approval_file*. Also honors an external abort
    (status='aborted') so the loop does not hang the full timeout (B020/B021).
    Returns True to continue, False to stop.
    """
    approval_file.write_text(json.dumps({
        "loop_id": loop_id,
        "iteration": iteration,
        "output_preview": output[:500],
        "decision": "pending",
    }))
    deadline = time.time() + timeout_seconds
    while time.time() < deadline:
        if conn is not None and _is_aborted(conn, loop_id):
            return False
        try:
            data = json.loads(approval_file.read_text())
            decision = data.get("decision")
            if decision in ("continue", "approve", "approved"):
                return True
            if decision in ("abort", "deny", "denied", "reject"):
                return False
        except Exception:
            pass
        time.sleep(2)
    return False  # Timed out — abort


def main() -> int:
    parser = argparse.ArgumentParser(description="TAG loop worker")
    parser.add_argument("--loop-id", required=True)
    parser.add_argument("--config", required=True)
    parser.add_argument("--db", required=True)
    args = parser.parse_args()

    db_path = Path(args.db)
    conn = _open_db(db_path)

    row = conn.execute("SELECT * FROM loop_runs WHERE id=?", (args.loop_id,)).fetchone()
    if not row:
        print(f"loop_worker: loop {args.loop_id} not found", file=sys.stderr)
        return 1

    loop = dict(row)
    goal = loop["goal"]
    profile = loop["profile"]
    max_iters = loop["max_iters"]
    approval = loop["approval"]

    try:
        signal.signal(signal.SIGHUP, signal.SIG_IGN)
    except (AttributeError, OSError):
        pass

    approval_dir = db_path.parent / "loop-approvals"
    approval_dir.mkdir(parents=True, exist_ok=True)
    approval_file = approval_dir / f"{args.loop_id}.json"

    previous_output = ""
    for iteration in range(1, max_iters + 1):
        # Honor an external abort (`tag loop abort` sets status='aborted').
        # Check before starting work and do NOT clobber the status back to
        # 'running' — that was silently overwriting the abort (B020).
        if _is_aborted(conn, args.loop_id):
            conn.close()
            return 0

        output, exit_code = _run_iteration(
            args.loop_id, iteration, goal, profile,
            args.config, previous_output,
        )

        # The abort may have arrived while the (long-running) iteration ran.
        # Record the iteration but stop without overwriting the 'aborted' status.
        if _is_aborted(conn, args.loop_id):
            _mark_iteration(
                conn, args.loop_id, iteration,
                input_text=goal if iteration == 1 else previous_output[:500],
                output=output,
                decision="aborted",
            )
            conn.close()
            return 0

        decision = "goal_achieved" if _is_goal_achieved(output) else "continue"
        if exit_code != 0 and not output.strip():
            decision = "error"

        _mark_iteration(
            conn, args.loop_id, iteration,
            input_text=goal if iteration == 1 else previous_output[:500],
            output=output,
            decision=decision,
        )

        if decision == "goal_achieved":
            _update_loop_status(conn, args.loop_id, "completed")
            conn.close()
            return 0

        if decision == "error":
            _update_loop_status(conn, args.loop_id, "failed")
            conn.close()
            return 1

        if approval == "human" and iteration < max_iters:
            approved = _request_approval(args.loop_id, iteration, output, approval_file, conn)
            if not approved:
                _update_loop_status(conn, args.loop_id, "aborted")
                conn.close()
                return 0

        previous_output = output

    _update_loop_status(conn, args.loop_id, "max_iters")
    conn.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())

