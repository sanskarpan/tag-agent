"""Queue and DAG workflow commands."""
from __future__ import annotations

import argparse
import json
import os
import sys
import time
import uuid
from pathlib import Path
from typing import Any

from tag.core.config import load_config, config_path
from tag.core.db import open_db, queue_insert_job, queue_list_jobs, queue_get_job, queue_clear_completed, launch_queue_worker
from tag.core.utils import nonnegative_int, utc_now

try:
    from tag.tui_output import print_error, print_success, print_warning
except Exception:
    def print_error(msg): print(f"error: {msg}", file=sys.stderr)
    def print_success(msg): print(msg)
    def print_warning(msg): print(f"warning: {msg}", file=sys.stderr)


# ---------------------------------------------------------------------------
# Internal helpers (mirror of controller-level helpers for db updates)
# ---------------------------------------------------------------------------

def _queue_update_pid(db: Any, job_id: str, pid: int) -> None:
    db.execute("UPDATE queue_jobs SET pid=? WHERE id=?", (pid, job_id))
    db.commit()


def _queue_update_status(db: Any, job_id: str, status: str) -> None:
    # timezone-aware ISO-8601 (+00:00), matching created_at written elsewhere
    # via core.utils.utc_now — not the deprecated, naive utcnow() (B145).
    now = utc_now()
    db.execute(
        "UPDATE queue_jobs SET status=?, finished_at=? WHERE id=?",
        (status, now, job_id),
    )
    db.commit()


def _dispatch_ready_jobs(cfg: dict[str, Any], db: Any, job_ids: list[str]) -> tuple[list[tuple[str, int]], list[str]]:
    """Launch a worker for every 'ready' job; return (launched, pending).

    launched: list of (job_id, pid). pending: job_ids still waiting on deps.
    """
    launched: list[tuple[str, int]] = []
    pending: list[str] = []
    for jid in job_ids:
        row = db.execute("SELECT status FROM queue_jobs WHERE id=?", (jid,)).fetchone()
        status = row[0] if row else None
        if status == "ready":
            try:
                pid = launch_queue_worker(cfg, jid)
                _queue_update_pid(db, jid, pid)
                launched.append((jid, pid))
            except Exception as exc:
                print_warning(f"failed to launch worker for {jid}: {exc}")
        elif status == "pending":
            pending.append(jid)
    return launched, pending


def _ensure_runtime_dirs(cfg: dict[str, Any]) -> None:
    """Create required runtime directories if they don't exist."""
    from pathlib import Path as _Path
    for key in ("data_dir", "log_dir", "run_dir"):
        d = cfg.get(key)
        if d:
            _Path(d).mkdir(parents=True, exist_ok=True)


# ---------------------------------------------------------------------------
# PRD-008: queue
# ---------------------------------------------------------------------------

def cmd_queue(args: argparse.Namespace) -> int:
    """Background task queue — submit, list, result, cancel."""
    cfg = load_config(config_path(args.config))
    _ensure_runtime_dirs(cfg)
    db = open_db(cfg)
    sub = getattr(args, "queue_subcommand", "list")

    if sub == "add":
        task_text = (args.task or "").replace("\x00", "").strip()
        if not task_text:
            db.close()
            print("error: task text must not be empty.", file=sys.stderr)
            return 1
        job_id = uuid.uuid4().hex[:8]
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        task_type = getattr(args, "task_type", "mixed") or "mixed"
        priority_arg = getattr(args, "priority", None)
        priority = priority_arg if priority_arg is not None else 5
        if not (1 <= priority <= 10):
            db.close()
            print(f"error: --priority must be between 1 and 10, got {priority}.", file=sys.stderr)
            return 1
        notify = not getattr(args, "no_notify", False)
        queue_insert_job(db, job_id, profile, task_text, task_type=task_type, priority=priority, notify=notify)
        pid = launch_queue_worker(cfg, job_id)
        _queue_update_pid(db, job_id, pid)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"job_id": job_id, "pid": pid, "status": "queued"}))
        else:
            print(f"queued: {job_id}  (worker pid {pid})")
        return 0

    if sub == "list":
        status_filter = getattr(args, "status_filter", None)
        # Honor an explicit 0 (show none) and reject negatives, instead of
        # `or 50` clobbering 0 / a negative LIMIT returning everything (B047/B087).
        limit = getattr(args, "limit", 50)
        if limit is None:
            limit = 50
        if limit < 0:
            db.close()
            msg = f"--limit must be >= 0, got {limit}."
            if getattr(args, "json", False):
                print(json.dumps({"error": msg}))
            else:
                print(f"error: {msg}", file=sys.stderr)
            return 1
        jobs = queue_list_jobs(db, status=status_filter, limit=limit + 1)
        total_indicator = len(jobs) > limit
        if total_indicator:
            jobs = jobs[:limit]
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(jobs, indent=2))
            return 0
        if not jobs:
            print("No jobs in queue.")
            return 0
        print(f"  {'ID':<10} {'STATUS':<12} {'PROFILE':<16} {'TASK'}")
        print("  " + "─" * 70)
        for j in jobs:
            task_short = (j.get("task") or "")[:40]
            print(f"  {j['id']:<10} {j['status']:<12} {j.get('profile','?'):<16} {task_short}")
        if total_indicator:
            print(f"  (showing {limit} of more — use --limit N to see more)")
        return 0

    if sub == "result":
        job = queue_get_job(db, args.job_id)
        db.close()
        as_json = getattr(args, "json", False)
        if not job:
            if as_json:
                print(json.dumps({"error": f"job {args.job_id} not found", "job_id": args.job_id}))
            else:
                print(f"Job '{args.job_id}' not found.", file=sys.stderr)
            return 1
        result_path = job.get("result_path")
        content = None
        if result_path and Path(result_path).exists():
            content = Path(result_path).read_text()
        if as_json:
            print(json.dumps({
                "job_id": args.job_id,
                "status": job.get("status"),
                "result_path": result_path,
                "result": content,
            }))
            return 0
        if content is not None:
            print(content)
        else:
            print(f"No result yet (status: {job['status']})")
        return 0

    if sub == "cancel":
        job = queue_get_job(db, args.job_id)
        if not job:
            db.close()
            print(f"Job '{args.job_id}' not found.", file=sys.stderr)
            return 1
        if job["status"] in ("done", "failed", "cancelled"):
            db.close()
            print(f"Job '{args.job_id}' is already {job['status']}.", file=sys.stderr)
            return 1
        pid = job.get("pid")
        if pid:
            import signal as _signal
            try:
                os.kill(pid, _signal.SIGTERM)
            except ProcessLookupError:
                pass
        _queue_update_status(db, args.job_id, "cancelled")
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"job_id": args.job_id, "status": "cancelled"}))
        else:
            print(f"cancelled: {args.job_id}")
        return 0

    if sub == "clear":
        count = queue_clear_completed(db)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"cleared": count}))
        else:
            print(f"cleared {count} completed/failed jobs")
        return 0

    db.close()
    return 0


# ---------------------------------------------------------------------------
# PRD-033: dag
# ---------------------------------------------------------------------------

def cmd_dag(args: argparse.Namespace) -> int:
    """PRD-033: tag queue dag show/save/run/list."""
    from tag.dag import (
        ensure_schema as dag_ensure, show_dag, save_dag, run_dag,
        list_dags, DagSpec,
    )
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    dag_ensure(db)
    sub = getattr(args, "dag_subcommand", None)

    if sub == "show" or sub is None:
        job_ids = getattr(args, "job_ids", None) or []
        from tag.dag import list_jobs_raw
        if getattr(args, "json", False):
            rows = list_jobs_raw(db, job_ids if job_ids else None)
            db.close()
            print(json.dumps(rows, indent=2))
            return 0
        print(show_dag(db, job_ids if job_ids else None))
        db.close()
        return 0

    if sub == "save":
        name = getattr(args, "name", "")
        steps_json = getattr(args, "steps", "[]")
        try:
            steps = json.loads(steps_json)
        except json.JSONDecodeError as exc:
            print_error(f"Invalid steps JSON: {exc}")
            db.close()
            return 1
        spec = DagSpec(name=name, steps=steps)
        try:
            dag_id = save_dag(db, spec)
        except ValueError as exc:
            db.close()
            print_error(str(exc))
            return 1
        db.close()
        print(f"DAG saved: {name} ({dag_id})")
        return 0

    if sub == "run":
        name = getattr(args, "name", "")
        board = getattr(args, "board", "default")
        try:
            job_ids = run_dag(db, name, board=board)
        except ValueError as exc:
            print_error(str(exc))
            db.close()
            return 1
        # Actually dispatch the jobs that have no unmet dependencies so they run,
        # instead of leaving them 'ready' forever with a false 'submitted' message
        # (B045). Dependent jobs stay 'pending' until promoted.
        launched, pending = _dispatch_ready_jobs(cfg, db, job_ids)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({
                "dag": name,
                "submitted": job_ids,
                "dispatched": [j for j, _ in launched],
                "pending": pending,
            }))
            return 0
        print(f"DAG '{name}' submitted: {len(job_ids)} jobs "
              f"({len(launched)} dispatched, {len(pending)} pending on dependencies)")
        for jid, pid in launched:
            print(f"  {jid}  (worker pid {pid})")
        for jid in pending:
            print(f"  {jid}  (pending — run `tag queue-dep promote` as dependencies finish)")
        return 0

    if sub == "list":
        dags = list_dags(db)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(dags, indent=2))
            return 0
        if not dags:
            print("No saved DAGs.")
            return 0
        for d in dags:
            print(f"{d['id'][:8]}  {d['name']:<30}  {d['step_count']} steps  {d['created_at'][:19]}")
        return 0

    db.close()
    print_error(f"Unknown dag subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# PRD-033: queue-dep (queue_extended)
# ---------------------------------------------------------------------------

def cmd_queue_extended(args: argparse.Namespace) -> int:
    """PRD-033: extended queue subcommands (depends-on support)."""
    from tag.dag import ensure_schema as dag_ensure, add_job, promote_ready_jobs
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    dag_ensure(db)

    sub = getattr(args, "queue_ext_subcommand", None)

    if sub == "add":
        task = getattr(args, "task", "")
        profile = getattr(args, "profile", None)
        depends_on = getattr(args, "depends_on", []) or []
        if not task.strip():
            print_error("Task must not be empty.")
            db.close()
            return 1
        try:
            job_id = add_job(db, task, profile=profile, depends_on=depends_on)
        except ValueError as exc:
            print_error(str(exc))
            db.close()
            return 1
        # Dispatch immediately if the job has no unmet dependencies, so it runs
        # rather than sitting 'ready' forever (B045).
        launched, pending = _dispatch_ready_jobs(cfg, db, [job_id])
        db.close()
        pid = launched[0][1] if launched else None
        job_status = "dispatched" if launched else "pending"
        if getattr(args, "json", False):
            print(json.dumps({"job_id": job_id, "status": job_status,
                              "pid": pid, "depends_on": depends_on}))
        elif pid is not None:
            print(f"Queue job added: {job_id}  (worker pid {pid})")
        else:
            print(f"Queue job added: {job_id}  (pending on dependencies — "
                  f"run `tag queue-dep promote` as they finish)")
        return 0

    if sub == "promote":
        promoted = promote_ready_jobs(db)
        # Dispatch the jobs we just promoted so the DAG actually progresses (B045).
        launched, _ = _dispatch_ready_jobs(cfg, db, promoted)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"promoted": promoted, "dispatched": [j for j, _ in launched]}))
        elif promoted:
            print(f"Promoted {len(promoted)} jobs to ready: {', '.join(promoted)}")
            for jid, pid in launched:
                print(f"  dispatched {jid}  (worker pid {pid})")
        else:
            print("No jobs promoted.")
        return 0

    if sub == "list":
        from tag.dag import list_jobs_raw
        jobs = list_jobs_raw(db)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(jobs, indent=2))
        else:
            if not jobs:
                print("No DAG jobs.")
            else:
                print(f"  {'ID':<14} {'STATUS':<12} {'TASK':<40}")
                print("  " + "─" * 70)
                for j in jobs:
                    print(f"  {j['id']:<14} {j['status']:<12} {str(j.get('task',''))[:40]}")
        return 0

    db.close()
    return 0


# ---------------------------------------------------------------------------
# Parser registration
# ---------------------------------------------------------------------------

def register(sub: argparse._SubParsersAction) -> None:  # type: ignore[type-arg]
    """Register queue, queue-dep, and dag subcommands."""

    # ---- PRD-008: queue ----
    queue = sub.add_parser("queue", help="Background task queue")
    queue_sub = queue.add_subparsers(dest="queue_subcommand")

    q_add = queue_sub.add_parser("add", help="Queue a task to run in the background")
    q_add.add_argument("task", help="Task description")
    q_add.add_argument("--profile", help="Profile to use (default: orchestrator)")
    q_add.add_argument("--type", dest="task_type", default="mixed")
    q_add.add_argument("--priority", type=int, default=5)
    q_add.add_argument("--no-notify", action="store_true")
    q_add.add_argument("--json", action="store_true")

    q_list = queue_sub.add_parser("list", help="List queued/running/done jobs")
    # queue_jobs is shared with the DAG layer, so accept both vocabularies
    # (queued/running/done/failed/cancelled and pending/ready/timed_out/skipped).
    q_list.add_argument("--status", dest="status_filter",
                        choices=("queued", "pending", "ready", "running", "done",
                                 "failed", "cancelled", "timed_out", "skipped"))
    q_list.add_argument("--limit", type=int, default=50, metavar="N", help="Max jobs to show (default: 50)")
    q_list.add_argument("--json", action="store_true")

    q_result = queue_sub.add_parser("result", help="Show output of a completed job")
    q_result.add_argument("job_id")
    q_result.add_argument("--json", action="store_true")

    q_cancel = queue_sub.add_parser("cancel", help="Cancel a running job")
    q_cancel.add_argument("job_id")
    q_cancel.add_argument("--json", action="store_true")

    q_clear = queue_sub.add_parser("clear", help="Remove completed/failed jobs from list")
    q_clear.add_argument("--json", action="store_true")

    for qp in [queue, q_add, q_list, q_result, q_cancel, q_clear]:
        qp.set_defaults(func=cmd_queue)

    # ---- PRD-033: dag ----
    dag_cmd = sub.add_parser("dag", help="DAG workflow engine for queue jobs")
    dag_sub = dag_cmd.add_subparsers(dest="dag_subcommand")

    dag_show = dag_sub.add_parser("show", help="Show job dependency graph")
    dag_show.add_argument("job_ids", nargs="*", metavar="JOB_ID", help="Job IDs to show (default: all)")
    dag_show.add_argument("--json", action="store_true")

    dag_save = dag_sub.add_parser("save", help="Save a named DAG spec")
    dag_save.add_argument("name", metavar="NAME")
    dag_save.add_argument("--steps", default="[]", help="JSON array of step objects")

    dag_run = dag_sub.add_parser("run", help="Submit a named DAG")
    dag_run.add_argument("name", metavar="NAME")
    dag_run.add_argument("--board", default="default")
    dag_run.add_argument("--json", action="store_true")

    dag_list = dag_sub.add_parser("list", help="List saved DAGs")
    dag_list.add_argument("--json", action="store_true")

    for dp in [dag_cmd, dag_show, dag_save, dag_run, dag_list]:
        dp.set_defaults(func=cmd_dag)

    # ---- PRD-033: queue-dep ----
    qext_cmd = sub.add_parser("queue-dep", help="Add queue job with dependencies")
    qext_sub = qext_cmd.add_subparsers(dest="queue_ext_subcommand")

    qadd = qext_sub.add_parser("add", help="Add a queue job with --depends-on")
    qadd.add_argument("task", metavar="TASK")
    qadd.add_argument("--depends-on", dest="depends_on", action="append", metavar="JOB_ID",
                      help="Prerequisite job ID (can be repeated)")
    qadd.add_argument("--profile")
    qadd.add_argument("--json", action="store_true")

    qpromote = qext_sub.add_parser("promote", help="Promote ready pending jobs")
    qpromote.add_argument("--json", action="store_true")

    qlist = qext_sub.add_parser("list", help="List DAG jobs and their status")
    qlist.add_argument("--json", action="store_true")

    for qp in [qext_cmd, qadd, qpromote, qlist]:
        qp.set_defaults(func=cmd_queue_extended)
