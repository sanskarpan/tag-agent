# PRD-021: Cron / Scheduled Agents (`tag cron`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (2 weeks)
**Affects:** `controller.py` (new `cmd_cron`), new `src/tag/scheduler.py`, `queue_worker.py` (cron-sourced job handling), `tag.sqlite3` schema (new `cron_jobs` table), `pyproject.toml` (add `apscheduler>=3.10`)

---

## 1. Overview

TAG has no scheduling primitive. Every agent invocation today requires a human at a keyboard. This PRD defines `tag cron`, a persistent scheduling subsystem that fires agent runs on a cron expression schedule — daily standup summarisations, nightly benchmark sweeps, scheduled PR reviews — without leaving a terminal open. It runs as an optional background daemon (PID-file managed, auto-start on CLI invocation), bridges to the existing `queue_worker` subprocess model for actual execution, and stores all job definitions and execution history in the same SQLite database TAG already uses, via APScheduler's `SQLAlchemyJobStore`.

The design deliberately reuses as much existing infrastructure as possible:

- `queue_insert_job` / `launch_queue_worker` (already in `controller.py`) are the execution primitives — the scheduler merely enqueues jobs on the correct tick.
- The `runtime_db_path(cfg)` SQLite file carries both the new `cron_jobs` metadata table (TAG-managed) and the APScheduler job store table (APScheduler-managed).
- Desktop notification on job failure reuses `tui_output.send_desktop_notification`.

---

## 2. Goals

1. **G1 — Declarative recurring runs:** `tag cron add '0 9 * * 1-5' --profile standup --goal "Summarise overnight GitHub activity"` schedules a daily weekday agent run with a single command and persists it across reboots.
2. **G2 — Cross-restart persistence:** Scheduled jobs survive `tag cron daemon --restart`, OS reboots, and `pip install --upgrade tag-agent` upgrades because APScheduler's `SQLAlchemyJobStore` stores next-fire-time in the same SQLite database used by the rest of TAG.
3. **G3 — Full lifecycle management:** Users can list, show, pause, resume, remove, and immediately trigger any named cron job without editing files.
4. **G4 — Zero new services required:** The daemon is an optional background process managed via a PID file; it is NOT a systemd unit, launchd plist, or cron entry. Users with no daemon can still run jobs manually via `tag cron run <name>`.
5. **G5 — Failure visibility:** Jobs that exit non-zero emit a desktop notification (when `--notify-on-failure` is set) and write structured failure records to `cron_run_log` so `tag cron logs <name>` shows history.
6. **G6 — Timezone correctness:** Every cron expression is evaluated in a named IANA timezone (default: local system timezone, overridable per-job) so `0 9 * * 1-5` means 9 AM in the user's wall-clock timezone, not UTC.
7. **G7 — Concurrency safety:** If a prior run of a job is still executing when the next tick fires, the new fire is skipped (misfire policy: `max_instances=1`) and logged rather than spawning a second concurrent worker for the same job.
8. **G8 — Audit trail:** Every scheduled execution is recorded in `cron_run_log` with its queue job ID, start time, finish time, exit code, and result path so users can reconstruct history even after queue jobs are purged.

---

## 3. Non-Goals

3.1 **Distributed scheduling** — TAG is a single-user, single-machine tool. Multi-node job distribution (Celery, Dramatiq, RQ) is out of scope.

3.2 **Systemd / launchd integration** — Writing unit files or plists is a separate installation concern. This PRD covers only the in-process daemon. A companion `tag cron install-service` command is deferred to a follow-on PRD.

3.3 **Cron expression authoring UI** — A TUI wizard for building cron expressions (e.g. "every Monday at 9 AM") is out of scope for v1; users write standard 5-field POSIX cron syntax or use `croniter` shorthand strings (`@daily`, `@weekly`, `@hourly`).

3.4 **Inter-job dependencies / DAGs** — Job chaining (run B only if A succeeds) is not supported. Use `tag queue` for sequential hand-offs if needed.

3.5 **Remote trigger API** — An HTTP endpoint to fire a job externally (e.g. from a webhook) is deferred; `tag cron run <name>` covers the local one-shot use case.

---

## 4. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|------------|----------|
| U1 | Developer | run `tag cron add '0 9 * * 1-5' --profile standup --goal "Summarise overnight GitHub notifications and open PRs" --name morning-standup` | the standup agent fires automatically every weekday morning and I read the digest instead of triaging myself |
| U2 | ML Engineer | run `tag cron add '0 2 * * *' --profile bench --goal "Run full benchmark suite and append results to bench-log.md" --name nightly-bench --notify-on-failure` | benchmarks run while I sleep and I get a desktop alert only if something regresses |
| U3 | Tech Lead | run `tag cron add '0 17 * * 5' --profile reviewer --goal "Review all PRs opened this week and post summary comment" --name weekly-pr-review` | the weekly PR review digest is waiting in Slack on Friday afternoon without manual effort |
| U4 | Developer | run `tag cron add '@2026-07-01T09:00:00' --profile deployer --goal "Deploy v2.0 to production" --name v2-deploy` | a one-shot future-dated agent run fires once at the exact date-time and then removes itself |
| U5 | Developer | run `tag cron list` and see a table with name, schedule, profile, last_run, next_run, status | I have a single command that shows everything scheduled without grepping config files |
| U6 | Developer | run `tag cron pause nightly-bench` before a vacation and `tag cron resume nightly-bench` on return | I suspend scheduled runs temporarily without deleting the job definition |
| U7 | Developer | run `tag cron remove morning-standup` | I permanently delete a schedule I no longer need |
| U8 | Developer | run `tag cron run nightly-bench` at 3 PM to validate a new benchmark | I trigger an immediate one-shot execution with the exact same settings as the scheduled run |
| U9 | Developer | run `tag cron logs nightly-bench --last 10` | I inspect the last 10 execution outcomes — timestamps, exit codes, result paths — to diagnose a flaky job |
| U10 | Developer | run `tag cron daemon --start` once after a reboot | the scheduler daemon starts in the background, writes its PID to `~/.tag/cron.pid`, and survives terminal close |

---

## 5. Proposed CLI Surface

All subcommands are grouped under `tag cron`. The `tag cron daemon` subcommand is the only one that manages a long-running process; all other subcommands are one-shot and complete immediately.

### 5.1 `tag cron add`

```
tag cron add '<cron-expr>' \
  --profile <name> \
  --goal <text> \
  --name <label> \
  [--task-type <mixed|code|chat>] \
  [--timezone <IANA-tz>] \
  [--env KEY=VAL ...] \
  [--notify-on-failure] \
  [--max-instances <N>] \
  [--misfire-grace <seconds>]
```

- `<cron-expr>`: Standard 5-field cron (`minute hour dom month dow`) OR APScheduler shorthand (`@hourly`, `@daily`, `@weekly`). One-shot ISO-8601 datetime strings (`@2026-07-01T09:00:00`) are supported as date triggers.
- `--name`: Unique human-readable identifier. Used as the APScheduler `job_id`. Must match `[a-z0-9_-]{1,64}`. Defaults to `<profile>-<cron-hash>` if omitted.
- `--profile`: Required. Must name an existing profile in the TAG config. Validated at add-time.
- `--goal`: Required. The free-text prompt sent to the agent. Stored verbatim.
- `--timezone`: IANA timezone string (e.g. `America/New_York`). Defaults to `tzlocal()`.
- `--env KEY=VAL`: Extra environment variables injected into the `queue_worker` subprocess. May be repeated. Stored as JSON in `cron_jobs.env_json`. Values are shell-expanded but NOT further interpolated after storage to prevent injection.
- `--notify-on-failure`: If set, a desktop notification fires when a run exits non-zero.
- `--max-instances`: Maximum concurrent executions of this job (default: 1). Values >1 allow parallel runs if APScheduler fires while a prior run is still queued but not yet consuming a worker slot.
- `--misfire-grace`: Seconds within which a missed trigger (daemon was stopped) is still fired on daemon restart (default: 3600). Set to 0 to never fire missed runs.

### 5.2 `tag cron list`

```
tag cron list [--profile <name>] [--all]
```

Prints a Rich table with columns: `NAME`, `SCHEDULE`, `TIMEZONE`, `PROFILE`, `STATUS` (`active`/`paused`), `LAST RUN`, `NEXT RUN`, `RUN COUNT`, `FAIL COUNT`.

`--all` includes one-shot jobs that have already fired and self-removed.

### 5.3 `tag cron show <name>`

```
tag cron show <name>
```

Prints full job metadata: all columns from `cron_jobs`, APScheduler's `next_run_time`, the last 5 log entries from `cron_run_log`, and the `--env` variables with values masked to `***` for keys matching `*KEY*`, `*SECRET*`, `*TOKEN*`, `*PASSWORD*`.

### 5.4 `tag cron remove <name>`

```
tag cron remove <name> [--force]
```

Removes the job from APScheduler and deletes the `cron_jobs` row. `cron_run_log` rows are retained for audit. Requires `--force` if a run is currently active (status `running` in `queue_jobs`).

### 5.5 `tag cron pause <name>` / `tag cron resume <name>`

```
tag cron pause <name>
tag cron resume <name>
```

`pause` calls `scheduler.pause_job(job_id)` — APScheduler stops firing new runs; any active run completes normally. Sets `cron_jobs.enabled = 0` for persistence across daemon restarts.

`resume` calls `scheduler.resume_job(job_id)` and sets `cron_jobs.enabled = 1`. Next fire time is recalculated from now.

### 5.6 `tag cron run <name>`

```
tag cron run <name> [--async]
```

Immediately enqueues the job as a `queue_jobs` row (bypassing the schedule) and launches `queue_worker` via `launch_queue_worker`. Without `--async`, the CLI polls `queue_jobs.status` every 2 seconds and streams progress until the job reaches `done` or `failed`. With `--async`, the CLI prints the job ID and exits immediately.

The run is logged to `cron_run_log` with `trigger_source = 'manual'` so it is distinguishable from scheduled runs in `tag cron logs`.

### 5.7 `tag cron daemon`

```
tag cron daemon [--start | --stop | --status | --restart]
  [--config <path>]
  [--log-level <debug|info|warning|error>]
```

Manages the long-running scheduler daemon process. Exactly one flag must be provided.

- `--start`: Forks `scheduler.py` as a detached process. Writes PID to `TAG_HOME/cron.pid`. Exits 0 if already running.
- `--stop`: Sends `SIGTERM` to the PID in `cron.pid`. Waits up to 10 seconds for graceful shutdown; sends `SIGKILL` on timeout.
- `--status`: Reports whether the daemon is running, prints PID, uptime, job count, and next scheduled fire time.
- `--restart`: Equivalent to `--stop` followed by `--start`.

The daemon process writes its own log to `TAG_HOME/logs/cron-daemon.log` (rotating at 10 MB, keeping 3 backups).

### 5.8 `tag cron logs <name>`

```
tag cron logs <name> [--last <N>] [--json]
```

Queries `cron_run_log` for the N most recent executions of `<name>` (default: 20). Prints a table with: `RUN_AT`, `TRIGGER`, `JOB_ID`, `STATUS`, `EXIT_CODE`, `DURATION`, `RESULT_PATH`.

`--json` emits newline-delimited JSON for scripting.

---

## 6. Functional Requirements

**FR-01 APScheduler BackgroundScheduler integration**
`scheduler.py` MUST instantiate `apscheduler.schedulers.background.BackgroundScheduler` with a single `SQLAlchemyJobStore` pointing at the TAG SQLite URL (`sqlite:////<TAG_HOME>/runtime/tag.sqlite3`). The `BackgroundScheduler` runs in a daemon thread inside the scheduler process, so the process stays alive via a `signal.pause()` loop.

**FR-02 SQLAlchemyJobStore persistence**
The APScheduler job store MUST use the `sqlalchemy` backend with `tablename='apscheduler_jobs'` in the same SQLite database file as all other TAG tables. This ensures that `next_run_time` is persisted to disk; on daemon restart APScheduler reads the existing rows and resumes scheduling without requiring the user to re-add jobs.

**FR-03 Cron expression validation at add-time**
`cmd_cron_add` MUST validate the cron expression before storing it. Validation uses `croniter.croniter.is_valid(expr)` for 5-field expressions, and recognises the shorthand aliases `@hourly`, `@daily`, `@weekly`, `@monthly`, `@yearly`/`@annually` via a lookup table. ISO-8601 one-shot strings (`@YYYY-MM-DDTHH:MM:SS`) are parsed as `apscheduler.triggers.date.DateTrigger`. An invalid expression MUST print a clear error and exit non-zero without writing any database row.

**FR-04 TAG metadata table `cron_jobs`**
A TAG-managed table `cron_jobs` (separate from APScheduler's `apscheduler_jobs`) MUST be created in `open_db` alongside `queue_jobs`. It stores human-readable metadata not present in APScheduler's internal schema (profile, goal text, env vars, failure-notification flag). APScheduler's `job_id` is the foreign key linking the two stores. See Section 8.3 for the full schema.

**FR-05 Daemon lifecycle via PID file**
The scheduler daemon MUST write its PID as a plain integer to `TAG_HOME/cron.pid` on start and delete the file on clean shutdown. On `--start`, the CLI MUST first check whether the PID file exists AND the process is alive (using `psutil.pid_exists(pid)` — already a core dependency). If the PID file exists but the process is dead, it is treated as a stale file and overwritten (crash recovery). If the process is alive, `--start` exits 0 with a message "Daemon already running (PID <N>)".

**FR-06 Job-to-queue bridge (trigger function)**
When APScheduler fires a cron job it calls `_cron_trigger(job_name, cfg_path)` — a plain Python callable registered as the APScheduler job function. This function MUST:
  1. Open the TAG SQLite database.
  2. Read `cron_jobs` row for `job_name` to get `profile`, `goal`, `task_type`, `env_json`.
  3. Call `queue_insert_job(db, job_id, profile, goal, task_type=task_type)`.
  4. Call `launch_queue_worker(cfg, job_id)` with any extra env vars merged into the subprocess environment.
  5. Insert a row into `cron_run_log` with `trigger_source = 'scheduled'`, `queue_job_id = job_id`, `fired_at = utc_now()`.
  6. Update `cron_jobs.last_run_at`, `cron_jobs.run_count += 1`.

**FR-07 Failure notification**
After a `queue_worker` subprocess completes (detected via `psutil.wait_procs` or by polling `queue_jobs.status`), if `exit_code != 0` AND `cron_jobs.notify_on_failure = 1`, `_cron_trigger` MUST call `send_desktop_notification("TAG Cron", f"{job_name} failed: exit {exit_code}")`. Notification is sent from the daemon process, not the queue_worker, to decouple them.

**FR-08 Missed-run handling policy**
APScheduler's `misfire_grace_time` for each job MUST be set from `--misfire-grace` (default: 3600 seconds). If the daemon was stopped and restarts more than `misfire_grace_time` seconds after a scheduled fire time, that fire is silently skipped and logged to `cron_run_log` with `trigger_source = 'missed_skipped'`. If within the grace period, the fire MUST be executed immediately with `trigger_source = 'missed_fired'`.

**FR-09 Timezone support**
Every `CronTrigger` MUST be constructed with the `timezone` parameter populated. If the user did not supply `--timezone`, the daemon calls `tzlocal.get_localzone_name()` at add-time (NOT at fire-time) and stores the IANA string in `cron_jobs.timezone`. This ensures the stored definition is portable and unambiguous. On Windows and environments without a system timezone database, the `tzdata` package already in core dependencies supplies the IANA data.

**FR-10 Concurrent execution limit**
APScheduler's `max_instances` parameter for each job MUST be set from `--max-instances` (default: 1). With `max_instances=1`, if a prior scheduled run is still in the `queue_jobs` table with `status IN ('queued', 'running')` when the next tick fires, APScheduler's coalescing MUST log a misfire and skip the fire. `cron_run_log` records this as `trigger_source = 'skipped_concurrent'`.

**FR-11 Auto-start check on CLI invocation**
If `TAG_AUTO_CRON_DAEMON=1` is set in the environment (or `cron.auto_start: true` in `tag.yaml`), every `tag cron add/remove/pause/resume/run` invocation MUST check whether the daemon is alive and start it if not. This removes the need for users to manually call `tag cron daemon --start` after a reboot.

**FR-12 One-shot (date trigger) self-cleanup**
When a `DateTrigger` job fires its single execution, APScheduler automatically removes it from `apscheduler_jobs`. `_cron_trigger` MUST detect this (by checking `scheduler.get_job(job_id) is None` after firing) and also set `cron_jobs.enabled = 0` and `cron_jobs.finished_at = utc_now()` for the record.

**FR-13 `tag cron list` reads from `cron_jobs` only**
`tag cron list` MUST be usable without the daemon running. It reads `cron_jobs` directly from SQLite and derives `next_run_at` from `cron_jobs.next_run_at` (updated by the daemon on each fire and on add). If the daemon is not running, `next_run_at` is shown as `(daemon stopped)`.

**FR-14 Log rotation for daemon log**
The daemon MUST configure Python `logging.handlers.RotatingFileHandler` for `TAG_HOME/logs/cron-daemon.log` with `maxBytes=10_485_760` (10 MB) and `backupCount=3`. The log level is configurable via `--log-level` (default: `info`).

**FR-15 `tag cron logs` queries `cron_run_log`**
`cron_run_log` MUST retain all execution records indefinitely (no auto-purge). A separate `tag cron logs --purge-before <ISO-date>` flag (out of scope for v1 but the schema supports it) allows manual cleanup. Retention policy is user-controlled.

**FR-16 Config-file cron definitions (optional v1.1)**
A `cron:` key in `tag.yaml` may define jobs as YAML, loaded on daemon start. Any job present in config but absent from `cron_jobs` is auto-added; any job absent from config but present in `cron_jobs` with `source='config'` is auto-removed. CLI-added jobs have `source='cli'` and are never auto-removed by config loading. This is noted here as a design constraint so the schema accommodates it; it is not required for v1.

---

## 7. Non-Functional Requirements

**NFR-01 Daemon memory footprint**
The scheduler daemon process MUST consume fewer than 60 MB of RSS memory in steady state with 20 or fewer cron jobs registered. APScheduler's `BackgroundScheduler` is a single Python thread; the main daemon thread sleeps in `signal.pause()`. No polling loops that pin the CPU.

**NFR-02 Schedule drift tolerance**
APScheduler's `CronTrigger` uses the system clock to compute next-fire-time. The daemon MUST NOT introduce drift beyond 5 seconds for jobs with a minute-granularity schedule. This is guaranteed by APScheduler's internal scheduler thread wakeup mechanism (it sleeps until the next fire time, not in a fixed poll interval).

**NFR-03 Restart recovery latency**
After `tag cron daemon --restart`, the daemon MUST be accepting new fire times and have loaded all persisted jobs within 3 seconds on a cold SSD. Verified by `tests/test_scheduler.py::test_restart_recovery`.

**NFR-04 SQLite WAL compatibility**
All daemon writes to `tag.sqlite3` MUST use `PRAGMA journal_mode = WAL` and `PRAGMA busy_timeout = 5000` (consistent with `open_db` in `controller.py`) so concurrent CLI reads (`tag cron list`) do not block daemon writes.

**NFR-05 Signal handling**
The daemon MUST handle `SIGTERM` by calling `scheduler.shutdown(wait=True)` (allowing in-flight trigger functions to complete), deleting `cron.pid`, flushing logs, and exiting 0. `SIGHUP` MUST trigger a config reload (re-read `tag.yaml` and sync any `source='config'` jobs) without restarting the process.

---

## 8. Technical Design

### 8.1 New file: `src/tag/scheduler.py`

```
src/tag/scheduler.py
```

Responsibilities:
- APScheduler `BackgroundScheduler` wrapper: construction, job add/remove/pause/resume, daemon entry point.
- PID file management: `write_pid_file(path)`, `read_pid_file(path) -> int | None`, `delete_pid_file(path)`.
- Daemon entry point: `def daemon_main(config_path: str, log_level: str) -> None` — called via `python -m tag.scheduler --config <path> --log-level <level>`.
- Trigger function: `def _cron_trigger(job_name: str, config_path: str) -> None` — the APScheduler-callable that bridges to `queue_insert_job` + `launch_queue_worker`.
- Failure poller: A second background thread (or APScheduler `IntervalTrigger` job at 30-second intervals) that checks `queue_jobs` for recently completed cron-sourced jobs and fires failure notifications.
- Public API consumed by `controller.py`:
  - `scheduler_add_job(db_url, name, cron_expr, profile, goal, tz, max_instances, misfire_grace, notify_on_failure, env_json, task_type) -> None`
  - `scheduler_remove_job(db_url, name) -> None`
  - `scheduler_pause_job(db_url, name) -> None`
  - `scheduler_resume_job(db_url, name) -> None`
  - `scheduler_get_next_run(db_url, name) -> datetime | None`
  - `daemon_is_running(pid_path: Path) -> bool`
  - `daemon_start(config_path: str, pid_path: Path, log_level: str) -> int` — forks via `subprocess.Popen([sys.executable, '-m', 'tag.scheduler', ...], start_new_session=True)` and writes PID.
  - `daemon_stop(pid_path: Path) -> None`
  - `daemon_status(pid_path: Path) -> dict` — returns `{running, pid, uptime_s, job_count, next_fire_at}`.

**APScheduler initialisation (canonical snippet):**
```python
from apscheduler.schedulers.background import BackgroundScheduler
from apscheduler.jobstores.sqlalchemy import SQLAlchemyJobStore
from apscheduler.executors.pool import ThreadPoolExecutor as APThreadPoolExecutor

jobstores = {
    'default': SQLAlchemyJobStore(url=f'sqlite:///{db_path}', tablename='apscheduler_jobs')
}
executors = {
    'default': APThreadPoolExecutor(max_workers=4)
}
job_defaults = {
    'coalesce': True,       # collapse multiple missed fires into one
    'max_instances': 1,
}
scheduler = BackgroundScheduler(
    jobstores=jobstores,
    executors=executors,
    job_defaults=job_defaults,
    timezone=local_tz,
)
scheduler.start()
```

**Adding a cron job (canonical snippet):**
```python
from apscheduler.triggers.cron import CronTrigger

trigger = CronTrigger.from_crontab(cron_expr, timezone=timezone_str)
scheduler.add_job(
    func=_cron_trigger,
    trigger=trigger,
    id=job_name,
    name=f'TAG cron: {job_name}',
    kwargs={'job_name': job_name, 'config_path': config_path},
    misfire_grace_time=misfire_grace,
    max_instances=max_instances,
    replace_existing=True,
)
```

**PID file handling (canonical snippet):**
```python
import os, signal
from pathlib import Path

PID_FILE = Path(tag_home()) / 'cron.pid'

def write_pid_file(path: Path) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(str(os.getpid()), encoding='utf-8')

def read_pid_file(path: Path) -> int | None:
    try:
        return int(path.read_text(encoding='utf-8').strip())
    except (FileNotFoundError, ValueError):
        return None

def daemon_is_running(path: Path) -> bool:
    pid = read_pid_file(path)
    if pid is None:
        return False
    try:
        import psutil
        return psutil.pid_exists(pid) and psutil.Process(pid).status() != psutil.STATUS_ZOMBIE
    except Exception:
        return False
```

**Daemon main loop (canonical snippet):**
```python
import signal, logging

def daemon_main(config_path: str, log_level: str) -> None:
    _setup_logging(log_level)
    write_pid_file(PID_FILE)
    scheduler = _build_scheduler(config_path)
    scheduler.start()
    _sync_jobs_from_db(scheduler, config_path)

    def _on_sigterm(signum, frame):
        logging.info('SIGTERM received — shutting down')
        scheduler.shutdown(wait=True)
        PID_FILE.unlink(missing_ok=True)
        raise SystemExit(0)

    def _on_sighup(signum, frame):
        logging.info('SIGHUP received — reloading config')
        _sync_jobs_from_db(scheduler, config_path)

    signal.signal(signal.SIGTERM, _on_sigterm)
    signal.signal(signal.SIGHUP, _on_sighup)
    signal.pause()   # sleep until signal — zero CPU, no polling
```

### 8.2 Changed file: `controller.py`

Add `cmd_cron(args: argparse.Namespace) -> int` alongside `cmd_queue`. The function dispatches to `args.cron_subcommand`:

```python
def cmd_cron(args: argparse.Namespace) -> int:
    from tag import scheduler as _sched
    cfg = load_config(config_path(args.config))
    db_path = runtime_db_path(cfg)
    pid_path = tag_home() / 'cron.pid'

    sub = args.cron_subcommand
    if sub == 'add':       return _cron_add(args, cfg, db_path, pid_path)
    if sub == 'list':      return _cron_list(args, cfg, db_path)
    if sub == 'show':      return _cron_show(args, cfg, db_path)
    if sub == 'remove':    return _cron_remove(args, cfg, db_path, pid_path)
    if sub == 'pause':     return _cron_pause(args, cfg, db_path, pid_path)
    if sub == 'resume':    return _cron_resume(args, cfg, db_path, pid_path)
    if sub == 'run':       return _cron_run_now(args, cfg, db_path)
    if sub == 'daemon':    return _cron_daemon(args, cfg, pid_path)
    if sub == 'logs':      return _cron_logs(args, cfg, db_path)
    print_error(f'Unknown cron subcommand: {sub}')
    return 1
```

The parser addition in `build_parser()`:

```python
cron_p = sub.add_parser('cron', help='Manage scheduled agent runs')
cron_sub = cron_p.add_subparsers(dest='cron_subcommand', required=True)
# ... add_parser for each subcommand ...
cron_p.set_defaults(func=cmd_cron)
```

`open_db` gains the `cron_jobs` and `cron_run_log` table creation (see Section 8.3).

### 8.3 Changed file: `queue_worker.py`

No structural changes are required. The cron bridge (`_cron_trigger` in `scheduler.py`) calls the existing `queue_insert_job` + `launch_queue_worker` functions. The `queue_worker` subprocess is entirely unaware it was triggered by cron.

One addition: after `_mark_done`, if `job.get('cron_job_name')` is populated (a new optional column added to `queue_jobs`), `queue_worker` MUST update `cron_run_log.finished_at`, `cron_run_log.exit_code`, and `cron_run_log.result_path` for the corresponding log row. This is the cleanest way to record completion without requiring the daemon to poll `queue_jobs`.

Alternatively (preferred to avoid coupling): the daemon's failure poller thread (an `IntervalTrigger` job at 30-second intervals) queries `queue_jobs WHERE cron_job_name IS NOT NULL AND status IN ('done','failed') AND cron_logged = 0`, updates `cron_run_log`, marks `cron_logged = 1`, and fires failure notifications. This keeps `queue_worker.py` free of scheduler knowledge.

### 8.4 Changed file: `pyproject.toml`

Add to `dependencies`:
```toml
"apscheduler==3.10.4",
"SQLAlchemy==2.0.41",   # already may be present transitively; pin explicitly
```

Note: APScheduler 3.x uses `SQLAlchemy` for the job store. APScheduler 4.x (async-first, still in prerelease as of 2026-06) uses a different API. This PRD targets the stable APScheduler 3.10.x line to avoid prerelease risk. The `cron` optional extra in `pyproject.toml` currently has an empty dependencies list (kept for back-compat); it should remain empty since `apscheduler` is now a core dependency.

### 8.5 Schema: `cron_jobs` and `cron_run_log` tables

Added to `open_db` in `controller.py`:

```sql
CREATE TABLE IF NOT EXISTS cron_jobs (
  name              TEXT PRIMARY KEY,          -- human label, == APScheduler job_id
  profile           TEXT NOT NULL,             -- TAG profile name
  goal              TEXT NOT NULL,             -- prompt text sent to agent
  cron_expr         TEXT NOT NULL,             -- raw expression as entered ('0 9 * * 1-5', '@daily', '@2026-07-01T09:00:00')
  trigger_type      TEXT NOT NULL DEFAULT 'cron', -- 'cron' | 'date' | 'interval'
  timezone          TEXT NOT NULL,             -- IANA timezone string
  task_type         TEXT NOT NULL DEFAULT 'mixed',
  env_json          TEXT NOT NULL DEFAULT '{}', -- JSON object of extra env vars
  enabled           INTEGER NOT NULL DEFAULT 1, -- 0=paused
  notify_on_failure INTEGER NOT NULL DEFAULT 0,
  max_instances     INTEGER NOT NULL DEFAULT 1,
  misfire_grace     INTEGER NOT NULL DEFAULT 3600, -- seconds
  source            TEXT NOT NULL DEFAULT 'cli',   -- 'cli' | 'config'
  last_run_at       TEXT,                      -- ISO-8601 UTC
  next_run_at       TEXT,                      -- ISO-8601 UTC (updated by daemon)
  run_count         INTEGER NOT NULL DEFAULT 0,
  fail_count        INTEGER NOT NULL DEFAULT 0,
  created_at        TEXT NOT NULL,
  finished_at       TEXT                       -- non-null for one-shot jobs after firing
);

CREATE TABLE IF NOT EXISTS cron_run_log (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  job_name        TEXT NOT NULL,               -- FK → cron_jobs.name (not enforced to allow log retention after job deletion)
  queue_job_id    TEXT,                        -- FK → queue_jobs.id
  trigger_source  TEXT NOT NULL,               -- 'scheduled' | 'manual' | 'missed_fired' | 'missed_skipped' | 'skipped_concurrent'
  fired_at        TEXT NOT NULL,               -- ISO-8601 UTC, when APScheduler fired
  started_at      TEXT,                        -- ISO-8601 UTC, when queue_worker began
  finished_at     TEXT,                        -- ISO-8601 UTC, when queue_worker completed
  exit_code       INTEGER,
  result_path     TEXT,
  error           TEXT,
  cron_logged     INTEGER NOT NULL DEFAULT 0   -- 1 after daemon has synced completion back
);
CREATE INDEX IF NOT EXISTS idx_crl_job ON cron_run_log(job_name, fired_at DESC);
CREATE INDEX IF NOT EXISTS idx_crl_queue ON cron_run_log(queue_job_id);
```

Also add `cron_job_name TEXT` column to `queue_jobs` (guarded by `ALTER TABLE IF NOT EXISTS` in `open_db`):

```sql
ALTER TABLE queue_jobs ADD COLUMN cron_job_name TEXT;
```

SQLite allows `ALTER TABLE ... ADD COLUMN` without data loss. The column is NULL for manually-submitted queue jobs and populated by `_cron_trigger` for scheduler-sourced jobs.

### 8.6 Daemon Architecture Diagram

```
┌─────────────────────────────────────────────────────────────┐
│  tag cron daemon --start                                    │
│   → forks: python -m tag.scheduler --config ...             │
│                        │                                    │
│          ┌─────────────▼──────────────┐                    │
│          │  scheduler.py (daemon)     │                    │
│          │  ┌──────────────────────┐  │ TAG_HOME/cron.pid  │
│          │  │ BackgroundScheduler  │  │◄── PID written     │
│          │  │   (APScheduler 3.10) │  │                    │
│          │  │  SQLAlchemyJobStore  │──┼──► tag.sqlite3     │
│          │  │  (apscheduler_jobs)  │  │    (WAL mode)      │
│          │  └────────┬─────────────┘  │                    │
│          │           │ on fire        │                    │
│          │  _cron_trigger(job_name)   │                    │
│          │           │               │                    │
│          │  1. queue_insert_job()     │                    │
│          │  2. launch_queue_worker()  │──► tag.queue_worker │
│          │  3. insert cron_run_log    │       (subprocess) │
│          │           │               │                    │
│          │  [30s IntervalTrigger]     │                    │
│          │  failure_poller()          │                    │
│          │  → check queue_jobs done  │                    │
│          │  → update cron_run_log     │                    │
│          │  → desktop notification   │                    │
│          └───────────────────────────┘                    │
└─────────────────────────────────────────────────────────────┘
```

---

## 9. Security Considerations

**SEC-01 Profile validation before scheduling**
`cmd_cron_add` MUST call the existing `load_profiles(cfg)` function and verify the named `--profile` exists before writing any database row or registering any APScheduler job. An attempt to schedule against a non-existent profile exits non-zero with a clear error. This prevents orphaned jobs that would silently fail at fire time.

**SEC-02 Cron expression injection via goal text**
The `--goal` text is stored verbatim in `cron_jobs.goal` and passed to `queue_insert_job` as the `task` string. It is never interpolated by the shell; it is passed as a positional Python string to `subprocess.run([..., '--prompt', goal, ...])` in `queue_worker.py`'s `_run_job`. Shell metacharacters in goal text therefore have no injection effect. This invariant MUST be preserved — `_run_job` MUST NOT pass `shell=True`.

**SEC-03 Env var storage**
`--env KEY=VAL` pairs are stored as a JSON object in `cron_jobs.env_json`. Values are stored as-is. Values for keys matching `(?i)(key|secret|token|password|credential|auth)` MUST be displayed as `***` in `tag cron show` output and in daemon logs. The plaintext values remain in `tag.sqlite3` which has the same trust boundary as the rest of the TAG runtime database; users should apply OS-level filesystem permissions (`chmod 600`) to `~/.tag/runtime/tag.sqlite3`.

**SEC-04 Privilege escalation prevention**
The daemon process runs as the same user who invoked `tag cron daemon --start`. There is no `setuid`, no capability request, and no systemd `AmbientCapabilities`. `tag cron` MUST refuse to start if run as root (`os.getuid() == 0`) unless `TAG_ALLOW_ROOT_CRON=1` is set, and MUST print a warning in that case.

**SEC-05 PID file symlink attack**
Before writing the PID file, the daemon MUST check that `TAG_HOME/cron.pid` is not a symlink (`os.path.islink(pid_path)`) and that `TAG_HOME` is owned by the current user (`os.stat(tag_home()).st_uid == os.getuid()`). If either check fails, the daemon MUST abort with an error.

**SEC-06 Notification webhook security**
Desktop notifications (via `osascript` or `notify-send`) contain only `job_name` and exit code — never the full `--goal` text or any credential values. This prevents leaking sensitive goal content to notification centre logs or snooping processes.

**SEC-07 Config-file cron injection**
When `source='config'` jobs are loaded from `tag.yaml`, the cron expression and goal text MUST be validated through the same `croniter.is_valid()` / argument-sanitisation path as CLI-added jobs. A compromised `tag.yaml` MUST NOT be able to cause arbitrary code execution via APScheduler's job store; since APScheduler stores the `_cron_trigger` function reference (not a pickled callable with arbitrary arguments), the only attack surface is `job_name` → database lookup, which is a parameterised SQL query.

**SEC-08 SQLite WAL and file permissions**
The daemon MUST ensure `TAG_HOME/runtime/` has mode `0o700` and `tag.sqlite3` has mode `0o600` on creation (`open_db` already sets this via `runtime_db_path(cfg).parent.mkdir(mode=0o700, ...)`). APScheduler's SQLAlchemy connection does not override file permissions; the file is created by TAG before APScheduler first touches it.

**SEC-09 Log file permissions**
`TAG_HOME/logs/cron-daemon.log` MUST be created with mode `0o600`. The `RotatingFileHandler` MUST be constructed with `delay=False` so TAG creates the file (and sets permissions) before the first log write, rather than leaving creation to the logging library which does not set restrictive permissions.

---

## 10. Testing Strategy

### 10.1 Time-mocking approach

APScheduler's `BackgroundScheduler` uses `datetime.datetime.now(tz)` internally. Tests MUST freeze time using `unittest.mock.patch('apscheduler.util.datetime', ...)` or, preferably, the `freezegun` library (`freeze_time`), which patches `datetime` globally. All scheduler tests MUST run with `freezegun` to avoid flakiness from wall-clock timing.

Example pattern:
```python
from freezegun import freeze_time
from apscheduler.schedulers.background import BackgroundScheduler

@freeze_time('2026-01-05 08:59:55')  # 5 seconds before 09:00 Monday
def test_daily_standup_fires(tmp_db_url, monkeypatch):
    triggered = []
    monkeypatch.setattr('tag.scheduler._cron_trigger', lambda **kw: triggered.append(kw))
    sched = _build_test_scheduler(tmp_db_url)
    sched.add_job(_cron_trigger, CronTrigger.from_crontab('0 9 * * 1-5'), id='standup', kwargs={...})
    sched.start()
    # Advance time past 09:00
    with freeze_time('2026-01-05 09:00:01'):
        time.sleep(0.1)  # allow scheduler thread to wake
    assert len(triggered) == 1
    sched.shutdown()
```

### 10.2 APScheduler test patterns

- Use `in-memory` SQLAlchemy URL (`sqlite:///:memory:`) for unit tests of `scheduler_add_job`, `scheduler_remove_job`, `scheduler_pause_job`.
- Use a `tmp_path` SQLite file for integration tests of PID file management and daemon restart.
- Test `coalesce=True` behaviour: register a job, freeze time to 3 missed ticks, start scheduler, assert trigger called exactly once (not three times).

### 10.3 Daemon lifecycle tests

```
tests/test_scheduler.py:
  test_daemon_start_writes_pid()
  test_daemon_start_idempotent()        # second --start is a no-op
  test_daemon_stop_removes_pid()
  test_stale_pid_file_recovery()        # pid file exists, process dead → overwrite
  test_restart_recovery()               # add job, stop daemon, start daemon, assert job persists
  test_sigterm_graceful_shutdown()
  test_sighup_reloads_config()
```

### 10.4 Bridge tests

```
tests/test_cron_bridge.py:
  test_cron_trigger_inserts_queue_job()
  test_cron_trigger_launches_worker()
  test_cron_trigger_logs_run()
  test_failure_poller_updates_run_log()
  test_failure_poller_sends_notification()
  test_concurrent_skip_logged()
```

### 10.5 CLI tests

```
tests/test_cron_cli.py:
  test_cron_add_valid()
  test_cron_add_invalid_expr_exits_nonzero()
  test_cron_add_unknown_profile_exits_nonzero()
  test_cron_list_empty()
  test_cron_list_with_jobs()
  test_cron_show()
  test_cron_pause_resume()
  test_cron_remove()
  test_cron_run_now_sync()
  test_cron_run_now_async()
  test_cron_logs()
  test_cron_logs_json()
```

---

## 11. Acceptance Criteria

**AC-01** `tag cron add '0 9 * * 1-5' --profile standup --goal "Test" --name standup-test` exits 0, writes a row to `cron_jobs`, and registers a job in `apscheduler_jobs`.

**AC-02** `tag cron add 'not a valid cron' --profile x --goal y --name z` exits non-zero with a message containing "invalid cron expression" and writes no database rows.

**AC-03** `tag cron add '0 9 * * 1-5' --profile does-not-exist --goal y --name z` exits non-zero with a message containing "profile not found" and writes no database rows.

**AC-04** After `tag cron daemon --start`, `~/.tag/cron.pid` exists and contains a valid integer PID of a live process.

**AC-05** After `tag cron daemon --stop`, `~/.tag/cron.pid` is deleted (or absent) and the formerly-running PID is no longer alive.

**AC-06** After stopping the daemon and restarting it, `tag cron list` shows the same jobs as before the stop (persistence via `apscheduler_jobs` table).

**AC-07** `tag cron pause standup-test` sets `cron_jobs.enabled = 0`; `tag cron resume standup-test` sets it back to 1; APScheduler `get_job('standup-test').next_run_time` is `None` while paused.

**AC-08** `tag cron run standup-test` inserts a `queue_jobs` row with `cron_job_name = 'standup-test'` and `trigger_source = 'manual'` in `cron_run_log`.

**AC-09** With `max_instances=1` and a prior run in `queue_jobs` status `running`, a second scheduled fire is recorded in `cron_run_log` with `trigger_source = 'skipped_concurrent'` and does NOT insert a second `queue_jobs` row.

**AC-10** `tag cron logs standup-test --last 5` prints at most 5 rows ordered by `fired_at DESC` from `cron_run_log`.

**AC-11** `tag cron logs standup-test --json` emits valid newline-delimited JSON, one object per line.

**AC-12** A job with `--notify-on-failure` that exits non-zero causes `send_desktop_notification` to be called exactly once with title `"TAG Cron"` (verified via mock in `test_failure_poller_sends_notification`).

**AC-13** A one-shot job added with `tag cron add '@2026-07-01T09:00:00' --profile x --goal y --name one-shot` is absent from `apscheduler_jobs` after firing and has `cron_jobs.enabled = 0` and a non-null `finished_at`.

**AC-14** The daemon process RSS memory at steady state with 10 registered jobs is below 60 MB (measured via `psutil.Process().memory_info().rss`).

**AC-15** `tag cron remove standup-test` with no active runs exits 0, removes the `apscheduler_jobs` row, and sets `cron_jobs` deleted (or removes the row); `cron_run_log` rows for `standup-test` are retained.

---

## 12. Dependencies

| Package | Version | Justification |
|---------|---------|---------------|
| `apscheduler` | `==3.10.4` | Stable release of APScheduler 3.x. `BackgroundScheduler` + `CronTrigger` + `SQLAlchemyJobStore`. APScheduler 4.x is async-first and still prerelease as of 2026-06. |
| `SQLAlchemy` | `==2.0.41` | Required by `SQLAlchemyJobStore`. SQLAlchemy 2.x is the minimum for the 3.10 job store. Should be pinned explicitly per TAG's supply-chain policy. Verify no transitive conflict with `fastapi`/`uvicorn` which may already pull SQLAlchemy. |
| `tzlocal` | (already pulled transitively) | `tzlocal.get_localzone_name()` for default timezone detection. If not present as a direct dep, add `tzlocal==5.2`. |
| `croniter` | `==6.0.0` | Already a core dependency. Used for `croniter.is_valid()` validation at add-time. |
| `psutil` | `==7.2.2` | Already a core dependency. Used for PID liveness checks in `daemon_is_running`. |
| `freezegun` | `>=1.5` | Test-only dependency. Add to `[project.optional-dependencies] dev`. |

---

## 13. Open Questions

**OQ-01 Daemon vs. OS-level scheduler**
Should `tag cron` prefer registering jobs in the OS scheduler (cron on macOS/Linux via `crontab -e`, Task Scheduler on Windows) rather than maintaining its own daemon? OS-level scheduling avoids the need for a persistent daemon process. The counterarguments: OS cron requires user-level `crontab` access (not always available in CI or containers), does not support pause/resume without editing crontab, and loses the rich metadata / run-log we need for `tag cron logs`. Decision: TAG daemon is the v1 choice; OS cron integration is deferred.

**OQ-02 Timezone handling for DST transitions**
When a cron job is scheduled for `0 2 * * *` (2 AM daily) in `America/New_York`, what happens on the DST "spring forward" night when 2:00 AM does not exist? APScheduler's `CronTrigger` skips the non-existent time and fires on the next valid tick. Should TAG display a warning to the user when adding a job whose expression includes a potentially DST-ambiguous time? Decision: defer to v1.1; document the APScheduler behaviour in `tag cron add --help`.

**OQ-03 Missed runs policy for long-stopped daemon**
If the daemon is stopped for 7 days and then restarted, the default `misfire_grace_time=3600` means all 7 days of missed fires are skipped. Is this the right default? Some users may want `--misfire-grace 0` (never fire missed) while others want `--misfire-grace -1` (always fire all missed, which APScheduler does not natively support). Decision: keep the default at 3600 seconds (1 hour); document explicitly in `tag cron add --help` and in the PRD. Do not support `-1` (fire all missed) in v1 as it can overwhelm the queue after a long outage.

**OQ-04 Interaction with `tag queue` concurrency limit**
`tag queue` supports a `max_concurrent` setting in `tag.yaml`. If the queue already has N running jobs and a cron job fires, it is enqueued but may not start immediately. Should `tag cron` have its own per-job timeout (separate from `queue_worker`'s 1-hour timeout) that cancels the job if it waits in the queue longer than X minutes? Decision: not in v1; the 1-hour `queue_worker` timeout covers this implicitly. Add a `--queue-timeout` flag as a v1.1 item.

**OQ-05 APScheduler 4.x migration path**
APScheduler 4.x has a different API (async-first, `AsyncScheduler`, different job store interface). If APScheduler 4.x stabilises during TAG's development cycle, should this PRD target 4.x instead? Decision: target 3.10.x for v1 (stable, well-documented, synchronous). File a follow-on issue to evaluate 4.x migration when it reaches GA.

---

## 14. Complexity and Timeline

**Complexity:** M (Medium)

**Rationale:** The core scheduling logic is provided by APScheduler (no custom timer/thread logic needed). The bridge to `queue_worker` reuses existing `queue_insert_job` + `launch_queue_worker` functions. The main new engineering effort is:
- `scheduler.py` (~350 lines): APScheduler wrapper, PID management, daemon entry point, failure poller.
- `controller.py` additions (~250 lines): `cmd_cron` dispatcher and 8 helper functions for each subcommand.
- Schema additions (~30 lines of SQL).
- Test suite (~400 lines across 3 test files).

**Estimated Sprint:** 1 sprint (2 weeks), single engineer.

| Week | Milestone |
|------|-----------|
| Week 1, Days 1–2 | Schema migration (`cron_jobs`, `cron_run_log`, `ALTER TABLE queue_jobs`); `open_db` update; `pyproject.toml` dep addition |
| Week 1, Days 3–4 | `scheduler.py`: APScheduler init, `_cron_trigger`, PID management, daemon entry point |
| Week 1, Day 5 | `controller.py`: `cmd_cron` parser and `add`/`list`/`show` subcommands |
| Week 2, Days 1–2 | `controller.py`: `remove`/`pause`/`resume`/`run`/`daemon`/`logs` subcommands |
| Week 2, Days 3–4 | Failure poller, notification integration, log rotation |
| Week 2, Day 5 | Full test suite; manual QA on macOS with a real standup job; documentation update (`README`, `--help` text) |

**Risk:** APScheduler's `SQLAlchemyJobStore` writes to the same WAL-mode SQLite file as all other TAG components. Under high concurrency (many queue jobs + daemon polling simultaneously), `PRAGMA busy_timeout = 5000` should be sufficient, but stress-testing is recommended before release.
