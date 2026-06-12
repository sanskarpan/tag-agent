# PRD-004: Kanban Swarm Topology Helpers

**Status:** Proposed  
**Priority:** P1  
**Estimated Effort:** M (2 weeks)  
**Affects:** `controller.py` (`cmd_submit`, new `cmd_swarm`), `default.yaml` routing config

---

## 1. Overview

Hermes v0.15.0 shipped a full multi-agent Kanban platform (104 PRs) including `hermes kanban swarm` — a topology helper that creates orchestrator-delegated task trees and launches concurrent worker agents per Kanban card. TAG's `tag kanban` is a bare pass-through and `tag submit` manually orchestrates profiles in sequence. This PRD defines `tag swarm`, a higher-level command that wraps the Hermes swarm system with TAG's profile topology, visual progress (PRD-003), and run history (existing SQLite `runs` table).

---

## 2. Problem Statement

- `tag submit "<task>"` runs profiles sequentially (orchestrator → researcher → coder → reviewer) without true parallel execution.
- `hermes kanban swarm` is powerful but undiscoverable via TAG — users must know the raw Hermes command.
- There is no way to see swarm progress across all profiles simultaneously in TAG.
- The existing routing config defines worker pools but they are not used for true parallel fan-out.
- Users running large research or implementation tasks want concurrent agents but must manually manage multiple terminal windows.

---

## 3. Goals

1. `tag swarm "<task>"` launches a full Hermes kanban swarm using TAG's configured profile topology.
2. Users can specify task type to select the right worker pool from `routing.task_types`.
3. Real-time progress display shows each profile's current step using Rich (from PRD-003).
4. Swarm run is recorded in `runs` table with `type=swarm`.
5. `tag swarm status <run_id>` shows swarm progress for a previously launched run.
6. `tag swarm cancel <run_id>` sends cancel signals to all worker processes.

---

## 4. Non-Goals

- Building a custom task scheduler — all scheduling is delegated to Hermes kanban.
- Cross-machine distributed swarms — single-machine only in this release.
- Custom swarm topologies beyond what default.yaml routing defines.

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag swarm "implement feature X"` | orchestrator decomposes and fans out to coder+reviewer concurrently |
| U2 | Researcher | run `tag swarm "research topic Y" --type research` | researcher + reviewer run in parallel |
| U3 | Developer | see live per-profile status in the terminal | I know what each agent is doing |
| U4 | Developer | run `tag swarm status abc123` | check swarm progress from another terminal |
| U5 | Developer | run `tag runs --type swarm` | see history of all swarm runs |

---

## 6. Technical Design

### 6.1 Swarm launch flow

```
tag swarm "<task>" [--type TASK_TYPE] [--profile MASTER_PROFILE] [--board BOARD]
```

1. Load config, resolve route via `resolve_route(cfg, task_type, None, [])`.
2. Create a `runs` record with `type='swarm'`.
3. Start the orchestrator profile's Hermes gateway: `hermes gateway start` (in background).
4. Call `hermes kanban create --board <board> --title "<task>"` via the orchestrator profile env.
5. Call `hermes kanban swarm --board <board>` via orchestrator — this triggers Hermes' auto-decomposition.
6. While running: poll `hermes kanban list --board <board> --json` every 5 seconds, render progress.
7. On completion: collect output from all kanban cards, update `runs` status.

### 6.2 New functions

```python
def cmd_swarm(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    task_type = getattr(args, "task_type", "mixed")
    route = resolve_route(cfg, task_type, None, [])
    
    run_id = str(uuid.uuid4())[:8]
    db = open_db(cfg)
    insert_run(db, run_id=run_id, task=args.task, type="swarm", ...)
    
    master = route.get("master", cfg["defaults"]["master_profile"])
    board = getattr(args, "board", cfg["defaults"]["board"])
    
    # Start gateway for orchestrator if not running
    _ensure_gateway(cfg, master)
    
    # Create and launch kanban swarm
    _launch_kanban_swarm(cfg, master, board, args.task, run_id)
    
    # Stream progress until done
    return _monitor_swarm(cfg, master, board, run_id, db)


def _ensure_gateway(cfg: dict, profile_name: str) -> None:
    """Start Hermes gateway for profile if not already running."""
    env = profile_exec_env(cfg, profile_name)
    result = run_profile_hermes(cfg, profile_name, "gateway", "status", "--json", check=False)
    if result.returncode != 0:
        subprocess.Popen(
            [str(hermes_bin(cfg)), "gateway", "start"],
            env={**os.environ, **env},
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        time.sleep(2)  # Allow gateway to start


def _launch_kanban_swarm(cfg: dict, profile: str, board: str, task: str, run_id: str) -> None:
    run_profile_hermes(cfg, profile, "kanban", "create", "--board", board, "--title", task)
    run_profile_hermes(cfg, profile, "kanban", "swarm", "--board", board)


def _monitor_swarm(cfg: dict, profile: str, board: str, run_id: str, db: sqlite3.Connection) -> int:
    """Poll kanban board and render progress until all cards complete."""
    from tag.tui_output import make_submit_progress
    progress = make_submit_progress()
    tasks_map: dict[str, Any] = {}
    
    while True:
        cards_raw = run_profile_hermes(cfg, profile, "kanban", "list", "--board", board, "--json", check=False)
        if cards_raw.returncode != 0:
            break
        cards = json.loads(cards_raw.stdout)
        
        all_done = all(c["status"] in {"done", "cancelled"} for c in cards)
        
        if progress:
            for card in cards:
                if card["id"] not in tasks_map:
                    tasks_map[card["id"]] = progress.add_task(
                        f"{card['assignee']}: {card['title'][:40]}",
                        total=100
                    )
                pct = 100 if card["status"] == "done" else 50 if card["status"] == "in_progress" else 0
                progress.update(tasks_map[card["id"]], completed=pct)
        
        if all_done:
            break
        time.sleep(5)
    
    update_run_status(db, run_id, "completed")
    return 0
```

### 6.3 `tag swarm status` subcommand

```
tag swarm status <run_id>     — poll current board state for that run
tag swarm cancel <run_id>     — send hermes kanban cancel for all in-progress cards
tag swarm list                — list all swarm runs from runs table (type=swarm)
```

### 6.4 default.yaml swarm config (optional override)

```yaml
swarm:
  max_concurrent_workers: 4
  poll_interval_seconds: 5
  auto_start_gateway: true
  gateway_start_wait_seconds: 2
```

---

## 7. Implementation Plan

| Step | Task |
|------|------|
| 1 | Add `type` column to `runs` table (with migration: `ALTER TABLE runs ADD COLUMN type TEXT DEFAULT 'submit'`) |
| 2 | Implement `_ensure_gateway`, `_launch_kanban_swarm`, `_monitor_swarm` |
| 3 | Implement `cmd_swarm` with subcommand routing (`launch`/`status`/`cancel`/`list`) |
| 4 | Register `swarm` parser with subparsers |
| 5 | Add Rich progress rendering (depends on PRD-003 `make_submit_progress`) |
| 6 | Add tests: `test_cmd_swarm_creates_run_record`, `test_monitor_swarm_exits_on_all_done` |
| 7 | Update README with swarm quickstart |

---

## 8. Success Metrics

- `tag swarm "write a hello world program"` creates a Kanban card and launches swarm without error.
- `tag runs --type swarm` shows the completed swarm run.
- Progress display updates every 5 seconds while swarm is running.
- `tag swarm cancel <id>` successfully cancels all in-progress cards.

---

## 9. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Concurrent multi-profile gateway execution not confirmed in Hermes | Start with single-profile orchestrator only; fan-out is delegated to Hermes' own kanban dispatcher |
| Gateway startup race condition | Configurable `gateway_start_wait_seconds`; retry with backoff |
| Hermes kanban JSON output format changes | Parse defensively with `.get()`, log unknown keys as warnings |
| Long-running swarms with no progress updates | Timeout after `swarm.poll_timeout_minutes` (default 60), mark run as `timed_out` |
