"""Task routing, model assignment, and submission commands."""
from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import textwrap
import time
import uuid
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path
from typing import Any

import yaml

from tag.core.config import load_config, save_config, config_path, benchmark_suite_path
from tag.core.paths import hermes_root, hermes_bin, runtime_db_path, tag_home
from tag.core.db import open_db
from tag.core.run import run_hermes, run_profile_hermes, run_profile_python
from tag.core.profile import (
    resolve_route,
    apply_route_model_overrides,
    parse_model_ref,
    format_model_ref,
    collect_assignments,
    load_model_inventory,
    load_openrouter_catalog,
    ensure_profile_exists,
    run_chat_step,
    load_benchmark_suite,
    case_passed,
    show_kanban_task,
    create_temp_profile,
    render_profiles,
    insert_run,
    update_run_status,
    insert_step,
    slugify,
)
from tag.core.utils import nonnegative_int, positive_int, strip_json_fences, utc_now

try:
    from tag.tui_output import print_error, print_success, print_warning, make_benchmark_progress, make_submit_progress
    _TUI_AVAILABLE = True
except Exception:
    _TUI_AVAILABLE = False

    def print_error(msg: str) -> None:
        print(f"error: {msg}", file=sys.stderr)

    def print_success(msg: str) -> None:
        print(msg)

    def print_warning(msg: str) -> None:
        print(f"warning: {msg}", file=sys.stderr)

    def make_benchmark_progress():
        return None

    def make_submit_progress():
        return None


# ---------------------------------------------------------------------------
# Lazy import of ensure_hermes_ready from controller to avoid circular deps
# ---------------------------------------------------------------------------

def _ensure_hermes_ready(cfg: dict[str, Any], *, config_arg: str | None, need_tui: bool) -> None:
    from tag.controller import ensure_hermes_ready  # type: ignore[import]
    ensure_hermes_ready(cfg, config_arg=config_arg, need_tui=need_tui)


# ---------------------------------------------------------------------------
# cmd_route
# ---------------------------------------------------------------------------

def cmd_route(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    route = resolve_route(cfg, args.task_type, args.master_profile, args.worker_profile)
    route = apply_route_model_overrides(
        route,
        master_model=args.master_model,
        verifier_model=args.verifier_model,
        worker_models=args.worker_model_override,
    )
    if args.json:
        print(json.dumps(route, indent=2))
        return 0
    print(f"task_type: {args.task_type}")
    print(f"board: {route['board']}")
    print(f"execution: {route['execution']}")
    print(f"master: {route['master']['name']} -> {format_model_ref(route['master']['model'])}")
    for worker in route["workers"]:
        print(f"worker: {worker['name']} -> {format_model_ref(worker['model'])}")
    if route["verifier"]:
        print(f"verifier: {route['verifier']['name']} -> {format_model_ref(route['verifier']['model'])}")
    return 0


# ---------------------------------------------------------------------------
# cmd_assignments
# ---------------------------------------------------------------------------

def cmd_assignments(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    rows = collect_assignments(cfg)
    if args.json:
        print(json.dumps(rows, indent=2))
        return 0
    for row in rows:
        runtime = f" [{row['openai_runtime']}]" if row["openai_runtime"] else ""
        print(f"{row['profile']}: {row['primary_model']}{runtime}")
        if row["delegation_model"] != "-":
            print(f"  delegation: {row['delegation_model']}")
    return 0


# ---------------------------------------------------------------------------
# cmd_models
# ---------------------------------------------------------------------------

def cmd_models(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_profile_exists(cfg, args.profile)
    _ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    payload = load_model_inventory(cfg, args.profile)
    providers = payload.get("providers", [])
    if args.provider:
        providers = [item for item in providers if item.get("slug") == args.provider]
    result = {
        "profile": args.profile,
        "current_provider": payload.get("provider", ""),
        "current_model": payload.get("model", ""),
        "providers": providers,
    }
    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    print(f"profile: {args.profile}")
    current = (
        f"{result['current_provider']}/{result['current_model']}"
        if result["current_provider"] and result["current_model"]
        else "-"
    )
    print(f"current: {current}")
    for provider in providers:
        header = provider.get("slug", "")
        if provider.get("authenticated") is False:
            header = f"{header} (not configured)"
        print(header)
        for model in provider.get("models", [])[: args.limit]:
            print(f"  - {model}")
    return 0


# ---------------------------------------------------------------------------
# cmd_set_model
# ---------------------------------------------------------------------------

def cmd_set_model(args: argparse.Namespace) -> int:
    from tag.core.config import update_config
    path = config_path(args.config)
    cfg = load_config(path)
    ensure_profile_exists(cfg, args.profile)
    provider, model = parse_model_ref(args.ref)

    def _mutate(cfg: dict) -> None:
        profile_cfg = cfg.setdefault("profiles", {}).setdefault(args.profile, {}).setdefault("config", {})
        if args.target == "primary":
            model_cfg = profile_cfg.setdefault("model", {})
            model_cfg["provider"] = provider
            model_cfg["default"] = model
            if args.openai_runtime:
                model_cfg["openai_runtime"] = args.openai_runtime
        else:
            delegation_cfg = profile_cfg.setdefault("delegation", {})
            delegation_cfg["provider"] = provider
            delegation_cfg["model"] = model
            if args.openai_runtime:
                delegation_cfg["openai_runtime"] = args.openai_runtime

    # Hold the lock across the whole read-modify-write so two set-model calls on
    # different profiles don't clobber each other (lost-update race, B005).
    cfg = update_config(path, _mutate)
    render_profiles(cfg, force=True)

    result = {
        "profile": args.profile,
        "target": args.target,
        "ref": f"{provider}/{model}",
        "config": str(path),
    }
    if args.openai_runtime:
        result["openai_runtime"] = args.openai_runtime
    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    print(f"{args.profile} {args.target} model -> {provider}/{model}")
    return 0


# ---------------------------------------------------------------------------
# cmd_submit
# ---------------------------------------------------------------------------

def cmd_submit(args: argparse.Namespace) -> int:
    cfg_path = config_path(args.config)
    cfg = load_config(cfg_path)
    _ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    prompt = args.prompt.strip()
    if not prompt:
        raise SystemExit("Prompt cannot be empty.")

    route = resolve_route(cfg, args.task_type, args.master_profile, args.worker_profile)
    route = apply_route_model_overrides(
        route,
        master_model=args.master_model,
        verifier_model=args.verifier_model,
        worker_models=args.worker_model_override,
    )
    execution = (
        args.execution
        if args.execution != "auto"
        else str(route.get("execution", "kanban"))
    )
    run_id = f"run-{slugify(args.task_type)}-{uuid.uuid4().hex[:10]}"
    conn = open_db(cfg)
    metadata = {
        "title": args.title or "",
        "source": args.source,
        "config": str(cfg_path),
    }
    insert_run(
        conn,
        run_id=run_id,
        kind="submit",
        task_type=args.task_type,
        execution=execution,
        master_profile=route["master"]["name"],
        board=route["board"],
        prompt=prompt,
        route=route,
        status="running",
        metadata=metadata,
    )

    result: dict[str, Any] = {
        "run_id": run_id,
        "execution": execution,
        "route": route,
        "steps": [],
    }

    if execution == "direct":
        futures = {}
        with ThreadPoolExecutor(max_workers=max(1, len(route["workers"]))) as pool:
            for worker in route["workers"]:
                worker_prompt = prompt
                futures[
                    pool.submit(
                        run_chat_step,
                        cfg,
                        profile_name=worker["name"],
                        prompt=worker_prompt,
                    )
                ] = worker
            for future in as_completed(futures):
                worker = futures[future]
                step = future.result()
                step["role"] = "worker"
                step["profile"] = worker["name"]
                step["model_ref"] = format_model_ref(worker["model"])
                result["steps"].append(step)
                insert_step(
                    conn,
                    run_id=run_id,
                    role="worker",
                    profile=worker["name"],
                    model_ref=step["model_ref"],
                    prompt=step["prompt"],
                    output=step["output"],
                    status=step["status"],
                    started_at=step["started_at"],
                    finished_at=step["finished_at"],
                    duration_ms=step["duration_ms"],
                    extra={"returncode": step["returncode"]},
                )

        if args.verify and route.get("verifier"):
            verifier_prompt = textwrap.dedent(
                f"""
                Task:
                {prompt}

                Worker outputs:
                {json.dumps([{k: v for k, v in step.items() if k in ('profile', 'status', 'output')} for step in result['steps']], indent=2)}

                Return compact JSON with keys status, verdict, notes.
                """
            ).strip()
            verify_step = run_chat_step(
                cfg,
                profile_name=route["verifier"]["name"],
                prompt=verifier_prompt,
            )
            verify_step["role"] = "verifier"
            verify_step["profile"] = route["verifier"]["name"]
            verify_step["model_ref"] = format_model_ref(route["verifier"]["model"])
            result["verifier"] = verify_step
            insert_step(
                conn,
                run_id=run_id,
                role="verifier",
                profile=verify_step["profile"],
                model_ref=verify_step["model_ref"],
                prompt=verify_step["prompt"],
                output=verify_step["output"],
                status=verify_step["status"],
                started_at=verify_step["started_at"],
                finished_at=verify_step["finished_at"],
                duration_ms=verify_step["duration_ms"],
                extra={"returncode": verify_step["returncode"]},
            )

        failures = [step for step in result["steps"] if step["status"] != "ok"]
        final_status = "ok" if not failures else "error"
        result["status"] = final_status
        update_run_status(conn, run_id=run_id, status=final_status, metadata=metadata)
    elif execution == "kanban":
        board = route["board"]
        title = args.title or f"{args.task_type}: {prompt[:80]}"
        create_cmd = [
            "kanban",
            "--board",
            board,
            "create",
            title,
            "--assignee",
        ]
        for worker in route["workers"]:
            worker_prompt = prompt
            proc = run_profile_hermes(
                cfg,
                route["master"]["name"],
                *create_cmd,
                worker["name"],
                "--body",
                worker_prompt,
                "--json",
                check=False,
            )
            output = (proc.stdout.strip() or proc.stderr.strip()).strip()
            step = {
                "role": "worker",
                "profile": worker["name"],
                "model_ref": format_model_ref(worker["model"]),
                "prompt": worker_prompt,
                "output": output,
                "status": "ok" if proc.returncode == 0 else "error",
                "task_id": "",
            }
            try:
                task_payload = json.loads(output) if output else {}
                step["task_id"] = str(task_payload.get("id", "") or "")
            except Exception:
                step["task_id"] = ""
            result["steps"].append(step)
            now = utc_now()
            insert_step(
                conn,
                run_id=run_id,
                role="worker",
                profile=worker["name"],
                model_ref=step["model_ref"],
                prompt=worker_prompt,
                output=output,
                status=step["status"],
                started_at=now,
                finished_at=now,
                duration_ms=0,
                extra={"kanban": True, "task_id": step["task_id"]},
            )
        final_status = "queued"
        if args.wait_seconds > 0:
            deadline = time.time() + args.wait_seconds
            pending = {step["task_id"]: step for step in result["steps"] if step.get("task_id")}
            while pending and time.time() < deadline:
                for task_id, step in list(pending.items()):
                    snapshot = show_kanban_task(
                        cfg,
                        profile_name=route["master"]["name"],
                        board=board,
                        task_id=task_id,
                    )
                    task = snapshot.get("task", {})
                    task_status = str(task.get("status", "") or "")
                    if task_status in {"done", "blocked", "archived"}:
                        step["task_status"] = task_status
                        step["latest_summary"] = snapshot.get("latest_summary")
                        pending.pop(task_id, None)
                if pending:
                    time.sleep(3)
            if pending:
                final_status = "queued"
            else:
                final_status = "ok"
        result["status"] = final_status
        update_run_status(conn, run_id=run_id, status=final_status, metadata=metadata)
    else:
        raise SystemExit(f"Unsupported execution mode '{execution}'.")

    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    print(f"run_id: {run_id}")
    print(f"status: {result['status']}")
    for step in result["steps"]:
        print(f"{step['profile']}: {step['status']}")
    return 0


# ---------------------------------------------------------------------------
# cmd_benchmark
# ---------------------------------------------------------------------------

def cmd_benchmark(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_profile_exists(cfg, args.profile)
    _ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    suite_path = benchmark_suite_path(args.suite)
    try:
        suite = load_benchmark_suite(suite_path)
    except FileNotFoundError as exc:
        raise SystemExit(f"Benchmark suite not found: {suite_path}") from exc
    if args.case:
        selected = set(args.case)
        suite = [case for case in suite if case.get("id") in selected]
    if not suite:
        raise SystemExit("No benchmark cases selected.")

    if args.model_ref:
        model_refs = args.model_ref
    else:
        primary = next(
            (row["primary_model"] for row in collect_assignments(cfg)
             if row["profile"] == args.profile),
            None,
        )
        if not primary:
            raise SystemExit(
                f"No primary model assigned for profile '{args.profile}'. "
                "Pass --model-ref or set one with `tag set-model`."
            )
        model_refs = [primary]
    run_id = f"bench-{slugify(args.profile)}-{uuid.uuid4().hex[:10]}"
    conn = open_db(cfg)
    insert_run(
        conn,
        run_id=run_id,
        kind="benchmark",
        task_type="benchmark",
        execution="direct",
        master_profile=args.profile,
        board="-",
        prompt=f"benchmark suite: {benchmark_suite_path(args.suite)}",
        route={"profile": args.profile, "models": model_refs},
        status="running",
        metadata={"suite": str(benchmark_suite_path(args.suite))},
    )
    result = {"run_id": run_id, "profile": args.profile, "models": []}
    overall_ok = True

    for model_ref in model_refs:
        temp_profile = create_temp_profile(cfg, base_profile=args.profile, model_ref=model_ref)
        model_entry = {"model_ref": model_ref, "profile": temp_profile, "cases": []}
        for case in suite:
            step = run_chat_step(cfg, profile_name=temp_profile, prompt=str(case.get("prompt", "")))
            ok, reason = case_passed(case, step["output"])
            case_result = {
                "id": case.get("id"),
                "status": "ok" if ok and step["status"] == "ok" else "error",
                "reason": reason,
                "output": step["output"],
            }
            model_entry["cases"].append(case_result)
            overall_ok = overall_ok and case_result["status"] == "ok"
            insert_step(
                conn,
                run_id=run_id,
                role="benchmark",
                profile=temp_profile,
                model_ref=model_ref,
                prompt=step["prompt"],
                output=step["output"],
                status=case_result["status"],
                started_at=step["started_at"],
                finished_at=step["finished_at"],
                duration_ms=step["duration_ms"],
                extra={"case_id": case.get("id"), "reason": reason},
            )
        result["models"].append(model_entry)

    result["status"] = "ok" if overall_ok else "error"
    update_run_status(
        conn,
        run_id=run_id,
        status=result["status"],
        metadata={"suite": str(benchmark_suite_path(args.suite))},
    )
    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    print(f"run_id: {run_id}")
    print(f"status: {result['status']}")
    for model in result["models"]:
        failed = sum(1 for case in model["cases"] if case["status"] != "ok")
        print(f"{model['model_ref']}: {len(model['cases']) - failed}/{len(model['cases'])} passed")
    return 0


# ---------------------------------------------------------------------------
# cmd_runs
# ---------------------------------------------------------------------------

def cmd_runs(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    conn = open_db(cfg)
    rows = conn.execute(
        "SELECT id, created_at, kind, task_type, execution, master_profile, status FROM runs ORDER BY created_at DESC LIMIT ?",
        (args.limit,),
    ).fetchall()
    payload = [dict(row) for row in rows]
    if args.json:
        print(json.dumps(payload, indent=2))
        return 0
    for row in payload:
        print(
            f"{row['id']} | {row['kind']} | {row['task_type']} | {row['execution']} | {row['master_profile']} | {row['status']}"
        )
    return 0


# ---------------------------------------------------------------------------
# cmd_openrouter_models
# ---------------------------------------------------------------------------

def cmd_openrouter_models(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_profile_exists(cfg, args.profile)
    _ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    rows = load_openrouter_catalog(cfg, args.profile)

    if args.search:
        needle = args.search.lower()
        rows = [
            row for row in rows
            if needle in str(row.get("id", "")).lower()
            or needle in str(row.get("name", "")).lower()
            or needle in str(row.get("description", "")).lower()
        ]

    def prompt_cost(row: dict[str, Any]) -> float:
        try:
            return float(row.get("pricing", {}).get("prompt", "0") or 0)
        except Exception:
            return 0.0

    def completion_cost(row: dict[str, Any]) -> float:
        try:
            return float(row.get("pricing", {}).get("completion", "0") or 0)
        except Exception:
            return 0.0

    if args.sort == "prompt":
        rows = sorted(rows, key=prompt_cost)
    elif args.sort == "completion":
        rows = sorted(rows, key=completion_cost)
    elif args.sort == "context":
        rows = sorted(rows, key=lambda row: int(row.get("context_length", 0) or 0), reverse=True)
    else:
        rows = sorted(rows, key=lambda row: str(row.get("id", "")))

    if args.limit == 0:
        rows = []
    elif args.limit > 0:
        rows = rows[: args.limit]

    if args.ids_only:
        for row in rows:
            print(f"openrouter/{row.get('id', '')}")
        return 0

    if args.json:
        print(json.dumps(rows, indent=2))
        return 0

    for row in rows:
        pricing = row.get("pricing", {}) or {}
        print(f"{row.get('id', '')}")
        print(
            f"  prompt={pricing.get('prompt', '?')} completion={pricing.get('completion', '?')} context={row.get('context_length', '?')}"
        )
    return 0


# ---------------------------------------------------------------------------
# PRD-011: Plugin System
# ---------------------------------------------------------------------------

def _safe_profile_path(base: Path, profile: str) -> Path:
    """Return ``base / profile`` only when the resolved path stays within *base*.

    Raises ``SystemExit`` if *profile* contains path-traversal components such
    as ``../`` that would escape the base directory.
    """
    resolved = (base / profile).resolve()
    base_resolved = base.resolve()
    try:
        resolved.relative_to(base_resolved)
    except ValueError:
        raise SystemExit(f"Invalid profile name (path traversal detected): {profile!r}")
    return base / profile


def _plugin_registry_path() -> Path:
    # This module lives at tag/cmd/routing.py; config is at tag/config/
    return Path(__file__).parent.parent / "config" / "plugin-registry.yaml"


def _load_plugin_registry() -> dict[str, Any]:
    p = _plugin_registry_path()
    if not p.exists():
        return {}
    with p.open() as fh:
        return yaml.safe_load(fh) or {}


def _hermes_venv_pip(cfg: dict[str, Any], profile: str, *pip_args: str) -> subprocess.CompletedProcess:
    venv_pip = tag_home() / "venvs" / profile / "bin" / "pip"
    if not venv_pip.exists():
        venv_pip = hermes_bin(cfg).parent / "pip"
    return subprocess.run([str(venv_pip), *pip_args], capture_output=True, text=True)


def cmd_plugin(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(getattr(args, "config", None)))
    registry = _load_plugin_registry()
    plugins_map: dict[str, Any] = registry.get("plugins", registry).get("registry", {})
    sub = getattr(args, "plugin_subcommand", None)

    if sub == "list" or sub is None:
        if not plugins_map:
            print("No plugins in registry.")
            return 0
        rows = []
        for name, info in plugins_map.items():
            rows.append({"name": name, "description": info.get("description", ""), "pypi": info.get("pypi", "")})
        if getattr(args, "json", False):
            print(json.dumps(rows, indent=2))
        else:
            for r in rows:
                print(f"  {r['name']:<35} {r['description']}")
        return 0

    if sub == "install":
        name = args.plugin_name
        info = plugins_map.get(name)
        if not info:
            print_error(f"Unknown plugin: {name}")
            return 1
        pypi = info.get("pypi", name)
        profile = getattr(args, "profile", None) or cfg.get("master_profile", "orchestrator")
        result = _hermes_venv_pip(cfg, profile, "install", pypi)
        if result.returncode != 0:
            print_error(f"pip install failed: {result.stderr.strip()}")
            return result.returncode
        print_success(f"Installed {name} ({pypi}) into profile '{profile}'")
        return 0

    if sub == "enable":
        name = args.plugin_name
        profile = getattr(args, "profile", None) or cfg.get("master_profile", "orchestrator")
        profile_dir = _safe_profile_path(tag_home() / "profiles", profile)
        env_file = profile_dir / ".env"
        # Normalise the plugin name to a valid env var identifier (replace any
        # non-alphanumeric characters with underscores, not just hyphens).
        env_key_suffix = re.sub(r"[^A-Z0-9]", "_", name.upper())
        line = f"TAG_PLUGIN_{env_key_suffix}_ENABLED=true\n"
        if env_file.exists():
            existing = env_file.read_text()
            if f"TAG_PLUGIN_{env_key_suffix}" in existing:
                env_file.write_text(re.sub(
                    rf"TAG_PLUGIN_{re.escape(env_key_suffix)}_ENABLED=.*\n",
                    line, existing,
                ))
            else:
                with env_file.open("a") as fh:
                    fh.write(line)
        else:
            env_file.parent.mkdir(parents=True, exist_ok=True)
            env_file.write_text(line)
        print_success(f"Enabled plugin '{name}' for profile '{profile}'")
        return 0

    if sub == "disable":
        name = args.plugin_name
        profile = getattr(args, "profile", None) or cfg.get("master_profile", "orchestrator")
        profile_dir = _safe_profile_path(tag_home() / "profiles", profile)
        env_file = profile_dir / ".env"
        if env_file.exists():
            env_key_suffix = re.sub(r"[^A-Z0-9]", "_", name.upper())
            key = f"TAG_PLUGIN_{env_key_suffix}_ENABLED"
            lines = [line for line in env_file.read_text().splitlines(keepends=True)
                     if not line.startswith(key)]
            env_file.write_text("".join(lines))
        print_success(f"Disabled plugin '{name}' for profile '{profile}'")
        return 0

    print_error(f"Unknown subcommand: {sub}")
    return 1


# ---------------------------------------------------------------------------
# Parser registration
# ---------------------------------------------------------------------------

def register(sub: argparse._SubParsersAction) -> None:  # type: ignore[type-arg]
    """Register all routing-related sub-commands onto *sub*."""

    # route
    route = sub.add_parser("route", help="Resolve task routing from lab policy")
    route.add_argument("--task-type", required=True)
    route.add_argument("--master-profile")
    route.add_argument("--worker-profile", action="append", default=[])
    route.add_argument("--master-model", help="Override master as provider/model-id")
    route.add_argument("--verifier-model", help="Override verifier as provider/model-id")
    route.add_argument(
        "--worker-model-override",
        action="append",
        default=[],
        help="Override worker as profile=provider/model-id",
    )
    route.add_argument("--json", action="store_true")
    route.set_defaults(func=cmd_route)

    # assignments
    assignments = sub.add_parser(
        "assignments", help="Show the current default model assignment per profile"
    )
    assignments.add_argument("--json", action="store_true")
    assignments.set_defaults(func=cmd_assignments)

    # models
    models = sub.add_parser(
        "models", help="List curated provider/model options for a profile"
    )
    models.add_argument("--profile", required=True)
    models.add_argument("--provider", help="Filter to one provider slug")
    models.add_argument("--limit", type=nonnegative_int, default=10)
    models.add_argument("--json", action="store_true")
    models.set_defaults(func=cmd_models)

    # set-model
    set_model = sub.add_parser(
        "set-model", help="Persist a profile's primary or delegation model"
    )
    set_model.add_argument("--profile", required=True)
    set_model.add_argument("--ref", required=True, help="provider/model-id")
    set_model.add_argument(
        "--target",
        choices=("primary", "delegation"),
        default="primary",
    )
    set_model.add_argument(
        "--openai-runtime",
        help="Optional runtime override when setting a primary OpenAI/Codex model",
    )
    set_model.add_argument("--json", action="store_true")
    set_model.set_defaults(func=cmd_set_model)

    # submit
    submit = sub.add_parser(
        "submit", help="Resolve a route and execute it directly or through Kanban"
    )
    submit.add_argument("--task-type", required=True)
    submit.add_argument("--prompt", required=True)
    submit.add_argument("--title")
    submit.add_argument("--source", default="manual")
    submit.add_argument(
        "--execution",
        choices=("auto", "direct", "kanban"),
        default="auto",
    )
    submit.add_argument("--master-profile")
    submit.add_argument("--worker-profile", action="append", default=[])
    submit.add_argument("--master-model")
    submit.add_argument("--verifier-model")
    submit.add_argument("--worker-model-override", action="append", default=[])
    submit.add_argument("--verify", action="store_true")
    submit.add_argument(
        "--wait-seconds",
        type=nonnegative_int,
        default=0,
        help="For Kanban submits, poll spawned tasks until completion or timeout",
    )
    submit.add_argument("--json", action="store_true")
    submit.set_defaults(func=cmd_submit)

    # benchmark
    benchmark = sub.add_parser(
        "benchmark", help="Run a prompt-contract benchmark across one or more models"
    )
    benchmark.add_argument("--profile", required=True)
    benchmark.add_argument("--suite", help="Path to benchmark suite YAML")
    benchmark.add_argument("--model-ref", action="append", default=[])
    benchmark.add_argument("--case", action="append", default=[])
    benchmark.add_argument("--json", action="store_true")
    benchmark.set_defaults(func=cmd_benchmark)

    # runs
    runs = sub.add_parser("runs", help="Show recent submit and benchmark runs")
    runs.add_argument("--limit", type=positive_int, default=20)
    runs.add_argument("--json", action="store_true")
    runs.set_defaults(func=cmd_runs)

    # openrouter-models
    openrouter_models = sub.add_parser(
        "openrouter-models",
        help="Query the full OpenRouter model catalog for a profile's API key",
    )
    openrouter_models.add_argument("--profile", required=True)
    openrouter_models.add_argument("--search")
    openrouter_models.add_argument(
        "--sort",
        choices=("id", "prompt", "completion", "context"),
        default="id",
    )
    openrouter_models.add_argument("--limit", type=nonnegative_int, default=20)
    openrouter_models.add_argument("--ids-only", action="store_true")
    openrouter_models.add_argument("--json", action="store_true")
    openrouter_models.set_defaults(func=cmd_openrouter_models)

    # plugin
    plugin = sub.add_parser("plugin", help="Manage TAG plugins")
    plugin_sub = plugin.add_subparsers(dest="plugin_subcommand")
    plugin_list = plugin_sub.add_parser("list", help="List available plugins")
    plugin_list.add_argument("--json", action="store_true")
    plugin_install = plugin_sub.add_parser("install", help="Install a plugin into a profile venv")
    plugin_install.add_argument("plugin_name", metavar="NAME")
    plugin_install.add_argument("--profile")
    plugin_install.add_argument("--json", action="store_true")
    plugin_enable = plugin_sub.add_parser("enable", help="Enable a plugin for a profile")
    plugin_enable.add_argument("plugin_name", metavar="NAME")
    plugin_enable.add_argument("--profile")
    plugin_disable = plugin_sub.add_parser("disable", help="Disable a plugin for a profile")
    plugin_disable.add_argument("plugin_name", metavar="NAME")
    plugin_disable.add_argument("--profile")
    for pp in [plugin, plugin_list, plugin_install, plugin_enable, plugin_disable]:
        pp.set_defaults(func=cmd_plugin)
