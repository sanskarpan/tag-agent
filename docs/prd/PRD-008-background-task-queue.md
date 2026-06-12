# PRD-008: Background Task Queue (`tag queue`)

**Status:** Proposed  
**Priority:** P1  
**Estimated Effort:** M (2 weeks)  
**Affects:** `controller.py` (new `cmd_queue`), `tag.sqlite3` schema, new `tag/queue_worker.py`

---

## 1. Overview

The most-demanded AI agent capability in 2025 is async background execution: submit a task, close the terminal, come back to results. Currently, all TAG commands are synchronous — the user must keep the terminal open. This PRD defines `tag queue`, a background task queue system that submits agent tasks as durable jobs, runs them as detached processes, persists results in SQLite, and optionally sends desktop notifications on completion.

---

## 2. Problem Statement

- `tag submit "<long task>"` requires the terminal open for the entire duration (sometimes hours).
- There is no way to queue multiple tasks and have them run one-by-one.
- There is no way to receive a notification when an overnight task completes.
- `tag runs` shows completed runs but there is no mechanism to queue pending ones.
- Competing tools (Devin, OpenHands, SWE-bench runners) all support async job submission.

---

## 3. Goals

1. `tag queue add "<task>"` submits a task that runs in the background.
2. `tag queue list` shows all queued, running, and recently completed tasks.
3. `tag queue result <job_id>` displays the output of a completed job.
4. `tag queue cancel <job_id>` terminates a running job.
5. Desktop notifications on job completion (macOS: `osascript`; Linux: `notify-send`).
6. Queue is persistent — survives terminal close, shell exit, and system sleep (using SQLite + detached subprocess).
7. At most one task runs at a time per profile (configurable: `max_concurrent: N`).

---

## 4. Non-Goals

- Distributed job execution across machines.
- Priority queues or job dependencies in v1.
- Web-based queue UI (covered partially by Hermes admin panel, PRD-009).
- Integration with external job schedulers (cron, systemd, Celery).

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag queue add "implement feature X" --profile coder` at 9pm | it's done when I wake up |
| U2 | Developer | run `tag queue list` | I see which tasks are pending, running, done |
| U3 | Developer | get a macOS notification "Coder finished: feature X" | I know when to review the output |
| U4 | Developer | run `tag queue result abc123` | I read the full agent output |
| U5 | Developer | queue 5 tasks that run sequentially | I batch a day's work |

---

## 6. Technical Design

### 6.1 Schema additions to `tag.sqlite3`

```sql
CREATE TABLE IF NOT EXISTS queue_jobs (
    id          TEXT PRIMARY KEY,
    profile     TEXT NOT NULL,
    task        TEXT NOT NULL,
    task_type   TEXT NOT NULL DEFAULT 'mixed',
    status      TEXT NOT NULL DEFAULT 'queued',  -- queued | running | done | failed | cancelled
    priority    INTEGER NOT NULL DEFAULT 5,
    created_at  TEXT NOT NULL,
    started_at  TEXT,
    finished_at TEXT,
    pid         INTEGER,          -- worker process PID while running
    run_id      TEXT,             -- FK to runs table
    result_path TEXT,             -- path to output file
    exit_code   INTEGER,
    error       TEXT,
    notify      INTEGER NOT NULL DEFAULT 1  -- 0/1 for desktop notification
);
CREATE INDEX IF NOT EXISTS idx_queue_jobs_status ON queue_jobs(status, priority, created_at);
```

### 6.2 New module: `src/tag/queue_worker.py`

This module is invoked as a subprocess:
```
python -m tag.queue_worker --job-id <id> --config <path> --db <path>
```

It runs as a completely detached process:
```python
"""Queue worker — runs as a detached subprocess, updates SQLite job record."""
import sys, os, sqlite3, subprocess, json, time
from pathlib import Path

def main():
    # ... parse --job-id, --config, --db
    # Mark job running in DB
    # Run: same as cmd_submit logic
    # Write output to result_path
    # Mark job done/failed in DB
    # Send desktop notification
    pass

if __name__ == "__main__":
    main()
```

### 6.3 Detached process launch

```python
def _launch_queue_worker(cfg: dict, job_id: str, db_path: Path) -> int:
    """Launch worker as detached process. Returns PID."""
    python = sys.executable
    cmd = [
        python, "-m", "tag.queue_worker",
        "--job-id", job_id,
        "--config", str(config_path(None)),
        "--db", str(db_path),
    ]
    proc = subprocess.Popen(
        cmd,
        start_new_session=True,   # detach from parent process group
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        close_fds=True,
    )
    return proc.pid
```

### 6.4 Desktop notifications

```python
def _send_notification(title: str, message: str) -> None:
    import platform
    system = platform.system()
    if system == "Darwin":
        subprocess.run([
            "osascript", "-e",
            f'display notification "{message}" with title "{title}"'
        ], check=False)
    elif system == "Linux":
        subprocess.run(["notify-send", title, message], check=False)
    # Windows: use winrt or skip
```

### 6.5 `cmd_queue` with subcommands

```python
def cmd_queue(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    db = open_db(cfg)
    sub = args.queue_subcommand
    
    if sub == "add":
        job_id = str(uuid.uuid4())[:8]
        profile = getattr(args, "profile", cfg["defaults"]["master_profile"])
        _queue_insert_job(db, job_id, profile, args.task, 
                          task_type=args.task_type, notify=not args.no_notify)
        pid = _launch_queue_worker(cfg, job_id, runtime_db_path(cfg))
        _queue_update_pid(db, job_id, pid)
        print(f"queued: {job_id} (worker pid {pid})")
    
    elif sub == "list":
        jobs = _queue_list_jobs(db, status=getattr(args, "status", None))
        if args.json:
            print(json.dumps(jobs, indent=2))
        else:
            _print_queue_table(jobs)
    
    elif sub == "result":
        job = _queue_get_job(db, args.job_id)
        if not job:
            print(f"job {args.job_id} not found", file=sys.stderr)
            return 1
        if job["result_path"] and Path(job["result_path"]).exists():
            print(Path(job["result_path"]).read_text())
        else:
            print(f"No result yet (status: {job['status']})")
    
    elif sub == "cancel":
        job = _queue_get_job(db, args.job_id)
        if job and job["pid"]:
            import signal
            try:
                os.kill(job["pid"], signal.SIGTERM)
                _queue_update_status(db, args.job_id, "cancelled")
                print(f"cancelled: {args.job_id}")
            except ProcessLookupError:
                print(f"process {job['pid']} not running; marking cancelled")
                _queue_update_status(db, args.job_id, "cancelled")
    
    elif sub == "clear":
        count = _queue_clear_completed(db)
        print(f"cleared {count} completed/failed jobs")
    
    db.close()
    return 0
```

### 6.6 Parser registration

```python
p_queue = subparsers.add_parser("queue", help="Background task queue")
queue_subs = p_queue.add_subparsers(dest="queue_subcommand", required=True)

p_queue_add = queue_subs.add_parser("add", help="Add task to queue")
p_queue_add.add_argument("task")
p_queue_add.add_argument("--profile", metavar="NAME")
p_queue_add.add_argument("--type", dest="task_type", default="mixed")
p_queue_add.add_argument("--no-notify", action="store_true")
p_queue_add.add_argument("--priority", type=int, default=5)

p_queue_list = queue_subs.add_parser("list", help="List queued/running/done jobs")
p_queue_list.add_argument("--status", choices=["queued","running","done","failed","cancelled"])
p_queue_list.add_argument("--json", action="store_true")

p_queue_result = queue_subs.add_parser("result", help="Show job output")
p_queue_result.add_argument("job_id")

p_queue_cancel = queue_subs.add_parser("cancel", help="Cancel a running job")
p_queue_cancel.add_argument("job_id")

p_queue_clear = queue_subs.add_parser("clear", help="Clear completed jobs from list")

for p in [p_queue, p_queue_add, p_queue_list, p_queue_result, p_queue_cancel, p_queue_clear]:
    p.set_defaults(func=cmd_queue)
```

---

## 7. Implementation Plan

| Step | Task |
|------|------|
| 1 | Add `queue_jobs` table to `open_db()` |
| 2 | Implement `queue_worker.py` as standalone module |
| 3 | Implement `_launch_queue_worker` with `start_new_session=True` |
| 4 | Implement `_send_notification` for macOS/Linux |
| 5 | Implement `cmd_queue` with all subcommands |
| 6 | Register parser |
| 7 | Add tests: `test_queue_add_creates_job`, `test_queue_cancel_sends_sigterm`, `test_queue_list_filters_by_status` |
| 8 | Manual test: submit job, close terminal, verify completion and notification |

---

## 8. Success Metrics

- `tag queue add "hello world" --profile coder` returns a job ID immediately.
- The job completes after the terminal is closed (verified via `tag queue list`).
- Desktop notification appears on macOS when job finishes.
- `tag queue cancel <id>` terminates the worker process.

---

## 9. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Worker process killed by OS on terminal close | `start_new_session=True` + `close_fds=True` ensures full detachment |
| SQLite write contention between main process and worker | WAL mode already set in `open_db()`; worker uses the same `open_db()` |
| Zombie processes if worker crashes before writing PID | Store PID immediately after `Popen()`; `tag queue list` checks `psutil.pid_exists(pid)` |
| Notification spam for long job batches | `--no-notify` flag; group notifications if > 5 jobs complete at once |
| Result files grow unbounded | `tag queue clear --older-than 7d` prunes result files; default retention: 30 days |
