"""Multi-agent swarm orchestration commands."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import subprocess
import sys
import time
import uuid
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path
from typing import Any

from tag.core.config import load_config, config_path
from tag.core.paths import runtime_db_path, hermes_root
from tag.core.db import open_db
from tag.core.run import run_profile_hermes
from tag.core.utils import nonnegative_int, utc_now

try:
    from tag.tui_output import print_error, print_success, print_warning
except Exception:
    def print_error(msg):
        print(f"error: {msg}", file=sys.stderr)

    def print_success(msg):
        print(msg)

    def print_warning(msg):
        print(f"warning: {msg}", file=sys.stderr)


# ---------------------------------------------------------------------------
# Helpers (inlined from controller.py to keep cmd modules self-contained)
# ---------------------------------------------------------------------------

def _hermes_bin(cfg: dict[str, Any] | None = None) -> Path:
    from tag.controller import hermes_bin  # noqa: PLC0415
    return hermes_bin(cfg)


def _profile_exec_env(cfg: dict[str, Any], profile_name: str) -> dict[str, str]:
    from tag.controller import profile_exec_env  # noqa: PLC0415
    return profile_exec_env(cfg, profile_name)


def _insert_run(db, *, run_id, kind, task_type, execution, master_profile, board, prompt, route, status, metadata):
    from tag.controller import insert_run  # noqa: PLC0415
    return insert_run(
        db,
        run_id=run_id, kind=kind, task_type=task_type, execution=execution,
        master_profile=master_profile, board=board, prompt=prompt,
        route=route, status=status, metadata=metadata,
    )


def _update_run_status(db, *, run_id, status, metadata=None):
    from tag.controller import update_run_status  # noqa: PLC0415
    return update_run_status(db, run_id=run_id, status=status, metadata=metadata)


def _resolve_route(cfg, task_type, profile, workers):
    from tag.controller import resolve_route  # noqa: PLC0415
    return resolve_route(cfg, task_type, profile, workers)


def _try_start_gateway(cfg: dict[str, Any], profile_name: str) -> None:
    """Best-effort: start hermes gateway so it can dispatch tasks we created.

    Management-plane operations (create/monitor tasks) don't need this.
    Execution-plane (AI agents running tasks) does. Fire-and-forget.
    """
    try:
        hbin = _hermes_bin(cfg)
        if not hbin.exists():
            return
        env = _profile_exec_env(cfg, profile_name)
        result = subprocess.run(
            [str(hbin), "gateway", "status"],
            env={**os.environ, **env},
            capture_output=True,
            timeout=5,
        )
        if result.returncode == 0:
            return
        subprocess.Popen(
            [str(hbin), "gateway", "start"],
            env={**os.environ, **env},
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        time.sleep(1)
    except Exception:
        pass


def _now_utc() -> str:
    import datetime
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"


# ---------------------------------------------------------------------------
# PRD-004: kanban-based swarm (legacy)
# ---------------------------------------------------------------------------

def cmd_swarm(args: argparse.Namespace) -> int:
    """Create a kanban swarm using TAG's native kanban layer (PRD-004).

    Management plane (task creation + monitoring) is pure SQLite — no hermes
    binary and no AI API key needed. Execution (agents running tasks) still
    goes through the hermes gateway, which needs a profile API key. That's
    expected: you need AI credentials to run AI.
    """
    import tag.kanban as _kanban  # noqa: PLC0415

    cfg = load_config(config_path(args.config))

    task_type = getattr(args, "task_type", "mixed") or "mixed"
    board = getattr(args, "board", None) or cfg["defaults"].get("board", "default")
    profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
    task_text = args.task

    if profile not in cfg.get("profiles", {}):
        print(f"warning: unknown profile '{profile}' — not found in config", file=sys.stderr)

    try:
        route = _resolve_route(cfg, task_type, profile, [])
    except SystemExit:
        route = {}

    workers_cfg = route.get("workers", [])
    verifier_cfg = route.get("verifier") or {}
    verifier_name = (
        verifier_cfg.get("name") if isinstance(verifier_cfg, dict) else str(verifier_cfg)
    ) or profile
    synthesizer_name = profile

    # Validate inputs early — before inserting any DB records
    task_text = task_text.replace("\x00", "").strip()
    if not task_text:
        print("error: task/goal text must not be empty.", file=sys.stderr)
        return 1

    try:
        kanban_path = _kanban.profile_kanban_db_path(cfg, profile, board)
    except ValueError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1

    worker_specs: list[tuple[str, str]] = [
        (
            (w.get("name") if isinstance(w, dict) else str(w)),
            task_text[:80],
        )
        for w in workers_cfg
    ]
    if not worker_specs:
        defaults = [p for p in ["researcher", "coder"] if p in cfg.get("profiles", {})]
        worker_specs = [(w, task_text[:80]) for w in (defaults or [profile])]

    db = open_db(cfg)
    run_id = str(uuid.uuid4())[:8]
    _insert_run(
        db,
        run_id=run_id,
        kind="swarm",
        task_type=task_type,
        execution="kanban",
        master_profile=profile,
        board=board,
        prompt=task_text,
        route=route,
        status="running",
        metadata={},
    )

    print(f"Swarm run: {run_id}")
    print(f"Profile: {profile}  Board: {board}  Task: {task_text[:60]}")

    try:
        kconn = _kanban.open_db(kanban_path)
    except Exception as exc:
        print(f"kanban db error: {exc}", file=sys.stderr)
        _update_run_status(db, run_id=run_id, status="failed")
        db.close()
        return 1

    idem_key = hashlib.sha256(f"{board}:{task_text}".encode()).hexdigest()[:16]
    try:
        topology = _kanban.create_swarm(
            kconn,
            goal=task_text,
            workers=worker_specs,
            verifier_assignee=verifier_name,
            synthesizer_assignee=synthesizer_name,
            idempotency_key=idem_key,
        )
    except Exception as exc:
        print(f"swarm creation failed: {exc}", file=sys.stderr)
        _update_run_status(db, run_id=run_id, status="failed")
        kconn.close()
        db.close()
        return 1

    # Best-effort: nudge gateway to start picking up the tasks
    _try_start_gateway(cfg, profile)

    swarm_out = {
        "run_id": run_id,
        "status": "running",
        "swarm": topology,
        "kanban_db": str(kanban_path),
    }

    if getattr(args, "no_wait", False):
        _update_run_status(db, run_id=run_id, status="running")
        kconn.close()
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(swarm_out))
        else:
            print(f"Swarm created: root={topology['root_id']}  "
                  f"workers={topology['worker_ids']}")
            print(f"Kanban DB: {kanban_path}")
        return 0

    # Monitor via direct SQLite reads — no hermes binary, no API key
    poll_interval = cfg.get("swarm", {}).get("poll_interval_seconds", 5)
    max_wait = cfg.get("swarm", {}).get("max_wait_seconds", 3600)
    deadline = time.time() + max_wait
    all_task_ids = (
        topology["worker_ids"]
        + [topology["verifier_id"], topology["synthesizer_id"]]
    )

    try:
        while time.time() < deadline:
            if _kanban.tasks_are_terminal(kconn, all_task_ids):
                break
            snap = _kanban.swarm_status_summary(kconn, topology)
            print(f"\r  {snap['done']}/{snap['total']} tasks done", end="", flush=True)
            time.sleep(poll_interval)
    except KeyboardInterrupt:
        print()
        _update_run_status(db, run_id=run_id, status="interrupted")
        kconn.close()
        db.close()
        sys.exit(130)

    print()
    final = _kanban.swarm_status_summary(kconn, topology)
    kconn.close()

    status = "completed" if final["complete"] else "timeout"
    _update_run_status(db, run_id=run_id, status=status)
    db.close()

    if getattr(args, "json", False):
        print(json.dumps({**swarm_out, "status": status, "final": final}))
    else:
        print(f"Swarm {status}: {run_id}  ({final['done']}/{final['total']} tasks done)")
    return 0 if status == "completed" else 1


# ---------------------------------------------------------------------------
# PRD-023: Context-centric multi-agent swarm (swarm run/list/status/abort/results)
# ---------------------------------------------------------------------------

def cmd_swarm_context(args: argparse.Namespace) -> int:
    """PRD-023: Context-centric swarm orchestration — dispatch subcommands."""
    sub = getattr(args, "swarm_subcommand", None)
    if sub == "run":
        return _cmd_swarm_run(args)
    if sub == "list":
        return _cmd_swarm_list(args)
    if sub == "status":
        return _cmd_swarm_status(args)
    if sub == "abort":
        return _cmd_swarm_abort(args)
    if sub == "results":
        return _cmd_swarm_results(args)
    print("usage: tag swarm run|list|status|abort|results [options]")
    return 0


def _cmd_swarm_run(args: argparse.Namespace) -> int:
    from tag.swarm import (  # noqa: PLC0415
        SwarmCoordinator, SwarmRunner, SwarmManifestError,
        ContextBus, create_swarm_run, insert_swarm_tasks, SWARM_MAX_AGENTS,
    )
    cfg = load_config(config_path(args.config))
    coordinator_profile = getattr(args, "coordinator_profile", None) or cfg["defaults"].get("master_profile", "orchestrator")
    goal = getattr(args, "goal", "")
    max_agents = min(int(getattr(args, "max_agents", 4) or 4), SWARM_MAX_AGENTS)
    failure_policy = getattr(args, "failure_policy", "best_effort") or "best_effort"
    timeout = int(getattr(args, "timeout_per_agent", 300) or 300)
    dry_run = getattr(args, "dry_run", False)
    as_json = getattr(args, "json", False)
    approve = getattr(args, "approve", False)
    parallel = not getattr(args, "sequential", False)

    if max_agents > SWARM_MAX_AGENTS:
        print(f"error: --max-agents must be ≤ {SWARM_MAX_AGENTS}", file=sys.stderr)
        return 1
    if not goal:
        print("error: --goal is required", file=sys.stderr)
        return 1
    if failure_policy not in ("abort_on_any", "best_effort", "require_majority"):
        print("error: invalid --failure-policy value", file=sys.stderr)
        return 1

    db = open_db(cfg)
    swarm_id = uuid.uuid4().hex[:12]

    # Step 1: Run coordinator to produce task manifest
    coordinator = SwarmCoordinator(cfg, coordinator_profile)
    try:
        manifest = coordinator.produce_manifest(goal, swarm_id, max_agents)
    except SwarmManifestError as exc:
        print(f"error: coordinator failed: {exc}", file=sys.stderr)
        return 2

    manifest["coordinator_profile"] = coordinator_profile

    if dry_run:
        print(f"Swarm ID: {swarm_id}")
        print(f"Goal:     {goal}")
        print(f"\n{'Task ID':<20} {'Profile':<18} {'Context Type':<14} Context Selector")
        print("-" * 80)
        for t in manifest["tasks"]:
            cs = t.get("context_slice", {})
            sel = cs.get("selector", "")
            if isinstance(sel, list):
                sel = ", ".join(str(s) for s in sel[:3])
            print(f"{t['task_id']:<20} {t['profile']:<18} {cs.get('type', ''):<14} {str(sel)[:40]}")
        if as_json:
            print(json.dumps(manifest, indent=2))
        return 0

    # Step 2: Persist run + tasks
    create_swarm_run(db, swarm_id, goal, coordinator_profile, failure_policy, max_agents)
    insert_swarm_tasks(db, swarm_id, manifest["tasks"])

    print(f"Swarm {swarm_id} — {len(manifest['tasks'])} tasks — policy: {failure_policy}")

    # Step 3: Execute
    bus = ContextBus(db, swarm_id)
    runner = SwarmRunner(
        cfg=cfg, manifest=manifest, bus=bus, conn=db,
        swarm_id=swarm_id, max_agents=max_agents,
        timeout_per_agent=timeout, failure_policy=failure_policy,
        parallel=parallel, approve=approve,
    )
    result = runner.run()
    db.close()

    if as_json:
        print(json.dumps(result, indent=2, default=str))
        return 0

    status = result.get("status", "unknown")
    final = result.get("final_output", "")
    print(f"\nStatus: {status}")
    if final:
        print(f"\n{final}")
    exit_codes = {"completed": 0, "partial": 5, "failed": 3, "aborted": 4}
    return exit_codes.get(status, 1)


def _cmd_swarm_list(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    db = open_db(cfg)
    status_filter = getattr(args, "status", None)
    where = "WHERE status=?" if status_filter else ""
    params = (status_filter,) if status_filter else ()
    rows = db.execute(
        f"""SELECT swarm_id, goal, status, task_count, started_at, completed_at, total_cost_usd
            FROM swarm_runs {where} ORDER BY created_at DESC LIMIT 50""",
        params,
    ).fetchall()
    db.close()
    if getattr(args, "json", False):
        keys = ["swarm_id", "goal", "status", "task_count", "started_at", "completed_at", "total_cost_usd"]
        print(json.dumps([dict(zip(keys, r)) for r in rows], indent=2))
        return 0
    if not rows:
        print("No swarm runs found.")
        return 0
    print(f"{'Swarm ID':<14} {'Status':<12} {'Tasks':>5} {'Cost':>8}  Goal")
    print("-" * 80)
    for r in rows:
        sid, goal, status, tasks, started, completed, cost = r
        elapsed = ""
        if started and completed:
            try:
                import datetime
                s = datetime.datetime.fromisoformat(started.rstrip("Z"))
                e = datetime.datetime.fromisoformat(completed.rstrip("Z"))
                secs = int((e - s).total_seconds())
                elapsed = f"{secs}s"
            except Exception:
                pass
        print(f"{sid:<14} {(status or ''):<12} {(tasks or 0):>5} ${(cost or 0):>7.4f}  {(goal or '')[:45]}")
    return 0


def _cmd_swarm_status(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    swarm_id = args.swarm_id
    db = open_db(cfg)
    run = db.execute(
        "SELECT swarm_id, goal, status, task_count, started_at, total_cost_usd FROM swarm_runs WHERE swarm_id=?",
        (swarm_id,),
    ).fetchone()
    if not run:
        print(f"error: swarm {swarm_id!r} not found", file=sys.stderr)
        db.close()
        return 1
    tasks = db.execute(
        "SELECT task_id, profile, status, started_at, completed_at, cost_usd, error_message FROM swarm_tasks WHERE swarm_id=? ORDER BY id",
        (swarm_id,),
    ).fetchall()
    db.close()
    if getattr(args, "json", False):
        print(json.dumps({"run": dict(zip(["swarm_id", "goal", "status", "task_count", "started_at", "total_cost_usd"], run)),
                          "tasks": [dict(zip(["task_id", "profile", "status", "started_at", "completed_at", "cost_usd", "error_message"], t)) for t in tasks]}, indent=2))
        return 0
    print(f"Swarm:  {run[0]}  ({run[2]})  tasks={run[3]}  cost=${run[5] or 0:.4f}")
    print(f"Goal:   {run[1]}")
    print(f"\n{'Task ID':<22} {'Profile':<18} {'Status':<14} Cost")
    print("-" * 70)
    for t in tasks:
        err = f"  [{t[6][:40]}]" if t[6] else ""
        print(f"{t[0]:<22} {(t[1] or ''):<18} {(t[2] or ''):<14} ${(t[5] or 0):.4f}{err}")
    return 0


def _cmd_swarm_abort(args: argparse.Namespace) -> int:
    import signal as _signal
    cfg = load_config(config_path(args.config))
    swarm_id = args.swarm_id
    db = open_db(cfg)
    pids = db.execute(
        "SELECT pid FROM swarm_tasks WHERE swarm_id=? AND status='running' AND pid IS NOT NULL",
        (swarm_id,),
    ).fetchall()
    killed = 0
    for (pid,) in pids:
        try:
            import os as _os
            pgid = _os.getpgid(pid)
            _os.killpg(pgid, _signal.SIGTERM)
            killed += 1
        except Exception:
            pass
    db.execute(
        "UPDATE swarm_tasks SET status='failed', error_message='aborted by user' WHERE swarm_id=? AND status='running'",
        (swarm_id,),
    )
    db.execute(
        "UPDATE swarm_runs SET status='aborted', completed_at=? WHERE swarm_id=?",
        (_now_utc(), swarm_id),
    )
    db.commit()
    db.close()
    print(f"Swarm {swarm_id} aborted — {killed} process(es) signalled.")
    return 0


def _cmd_swarm_results(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    swarm_id = args.swarm_id
    db = open_db(cfg)
    run = db.execute(
        "SELECT swarm_id, goal, status, final_output, total_cost_usd FROM swarm_runs WHERE swarm_id=?",
        (swarm_id,),
    ).fetchone()
    if not run:
        print(f"error: swarm {swarm_id!r} not found", file=sys.stderr)
        db.close()
        return 1
    tasks = db.execute(
        "SELECT task_id, profile, status, cost_usd, tokens_prompt, tokens_completion, output, error_message FROM swarm_tasks WHERE swarm_id=? ORDER BY id",
        (swarm_id,),
    ).fetchall()
    include_ctx = getattr(args, "include_context", False)
    ctx_rows: list = []
    if include_ctx:
        from tag.swarm import ContextBus  # noqa: PLC0415
        bus = ContextBus(db, swarm_id)
        ctx_rows = bus.full_audit()
    db.close()
    fmt = getattr(args, "format", "table")
    if fmt == "json":
        out = {
            "swarm_id": run[0], "goal": run[1], "status": run[2],
            "final_output": run[3], "total_cost_usd": run[4],
            "tasks": [dict(zip(["task_id", "profile", "status", "cost_usd", "tokens_prompt", "tokens_completion", "output", "error_message"], t)) for t in tasks],
        }
        if include_ctx:
            out["context_bus"] = ctx_rows
        print(json.dumps(out, indent=2))
        return 0
    print(f"Swarm:  {run[0]}  ({run[2]})  total_cost=${run[4] or 0:.4f}")
    print(f"Goal:   {run[1]}\n")
    print(f"{'Task ID':<22} {'Status':<14} {'Tokens':>8} {'Cost':>8}")
    print("-" * 60)
    for t in tasks:
        print(f"{t[0]:<22} {(t[2] or ''):<14} {(t[4] or 0) + (t[5] or 0):>8} ${(t[3] or 0):>7.4f}")
    if run[3]:
        print(f"\n── Final Output ──\n{run[3]}")
    if include_ctx and ctx_rows:
        print(f"\n── Context Bus ({len(ctx_rows)} entries) ──")
        for e in ctx_rows:
            print(f"  [{e['written_by']}] {e['key']} = {json.dumps(e['value'])[:80]}")
    return 0


# ---------------------------------------------------------------------------
# register(sub) — called by the CLI harness
# ---------------------------------------------------------------------------

def register(sub: argparse._SubParsersAction) -> None:  # type: ignore[name-defined]
    """Register the swarm command and its subcommands onto *sub*."""
    # ---- PRD-004: swarm ----
    swarm = sub.add_parser("swarm", help="Multi-agent swarm orchestration")
    swarm_sub = swarm.add_subparsers(dest="swarm_subcommand")

    # Legacy kanban-based swarm (PRD-004) — keep as default positional
    swarm.add_argument("task", nargs="?", help="Task description (kanban swarm — legacy)")
    swarm.add_argument("--profile", help="Orchestrator profile (default: orchestrator)")
    swarm.add_argument("--type", dest="task_type", default="mixed",
                       choices=("research", "implementation", "review", "mixed"))
    swarm.add_argument("--board", help="Kanban board name (default: from config)")
    swarm.add_argument("--no-wait", action="store_true", dest="no_wait")
    swarm.add_argument("--json", action="store_true")
    swarm.set_defaults(func=cmd_swarm)

    # PRD-023: context-centric swarm subcommands
    sw_run = swarm_sub.add_parser("run", help="Launch a context-centric multi-agent swarm")
    sw_run.add_argument("--goal", required=True, help="Natural-language goal for the swarm")
    sw_run.add_argument("--coordinator-profile", dest="coordinator_profile",
                        help="Profile to use as coordinator")
    sw_run.add_argument("--max-agents", dest="max_agents", type=int, default=4,
                        help="Max concurrent sub-agents (cap: 10)")
    sw_run.add_argument("--failure-policy", dest="failure_policy", default="best_effort",
                        choices=("abort_on_any", "best_effort", "require_majority"))
    sw_run.add_argument("--timeout-per-agent", dest="timeout_per_agent", type=int, default=300)
    sw_run.add_argument("--approve", action="store_true",
                        help="Pause for approval before each subtask")
    sw_run.add_argument("--sequential", action="store_true",
                        help="Dispatch agents sequentially (default: parallel)")
    sw_run.add_argument("--dry-run", dest="dry_run", action="store_true",
                        help="Show manifest without running agents")
    sw_run.add_argument("--json", action="store_true")
    sw_run.set_defaults(func=cmd_swarm_context)

    sw_list = swarm_sub.add_parser("list", help="List swarm runs")
    sw_list.add_argument("--status",
                         choices=("running", "completed", "aborted", "failed", "partial"))
    sw_list.add_argument("--json", action="store_true")
    sw_list.set_defaults(func=cmd_swarm_context)

    sw_status = swarm_sub.add_parser("status", help="Show per-agent status for a swarm")
    sw_status.add_argument("swarm_id", metavar="SWARM_ID")
    sw_status.add_argument("--watch", action="store_true")
    sw_status.add_argument("--json", action="store_true")
    sw_status.set_defaults(func=cmd_swarm_context)

    sw_abort = swarm_sub.add_parser("abort", help="Abort a running swarm")
    sw_abort.add_argument("swarm_id", metavar="SWARM_ID")
    sw_abort.set_defaults(func=cmd_swarm_context)

    sw_results = swarm_sub.add_parser("results", help="Show results and final output for a swarm")
    sw_results.add_argument("swarm_id", metavar="SWARM_ID")
    sw_results.add_argument("--format", choices=("table", "json"), default="table")
    sw_results.add_argument("--include-context", dest="include_context", action="store_true")
    sw_results.set_defaults(func=cmd_swarm_context)

    for sw_p in [swarm, sw_run, sw_list, sw_status, sw_abort, sw_results]:
        if "config" not in {a.dest for a in sw_p._actions}:
            sw_p.add_argument("--config", help=argparse.SUPPRESS)
