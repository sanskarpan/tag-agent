"""System setup and management commands."""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
from pathlib import Path
from typing import Any

from tag.core.config import load_config, config_path, benchmark_suite_path
from tag.core.paths import (
    tag_home, hermes_root, hermes_bin, runtime_home, runtime_db_path,
    hermes_env, ensure_runtime_dirs, tag_cli_label, tag_cli_bin,
    package_root, resource_path, bundled_hermes_archive,
    hermes_repo_url, hermes_ref, python_runtime_supported,
    hermes_checkout_kind, discover_local_hermes_checkout,
    resolve_home_relative, DEFAULT_TAG_HOME,
)
from tag.core.db import open_db
from tag.core.run import run_hermes
from tag.core.profile import render_profiles, bootstrap_profiles, _config_profiles
from tag.core.utils import nonnegative_int, utc_now, write_yaml, write_text

try:
    from tag.tui_output import print_error, print_success, print_warning
except Exception:
    def print_error(msg): print(f"error: {msg}", file=sys.stderr)
    def print_success(msg): print(msg)
    def print_warning(msg): print(f"warning: {msg}", file=sys.stderr)


def cmd_setup(args: argparse.Namespace) -> int:
    from tag.controller import (
        load_config, config_path, benchmark_suite_path,
        ensure_setup_prereqs, ensure_runtime_dirs, doctor_prerequisites,
        clone_or_update_hermes, ensure_venv, install_hermes_python,
        apply_hermes_patch, install_tui_dependencies, bundled_hermes_archive,
        hermes_bin, bootstrap_profiles, render_profiles,
        auto_import_codex_profiles, auto_import_claude_profiles,
        auto_import_gemini_profiles, auto_import_continue_profiles,
        auto_import_mistral_profiles,
    )
    cfg = load_config(config_path(args.config))
    benchmark_path = benchmark_suite_path(None)
    needs_git = bool(args.refresh or not bundled_hermes_archive().exists())
    ensure_setup_prereqs(cfg, need_npm=not args.skip_tui_build, need_git=needs_git)
    ensure_runtime_dirs(cfg)
    steps = {
        "config": {"config": str(config_path(args.config)), "benchmark_suite": str(benchmark_path)},
        "prerequisites": doctor_prerequisites(cfg),
        "clone": clone_or_update_hermes(cfg, refresh=args.refresh),
        "venv": ensure_venv(cfg),
    }
    if not args.skip_python_install:
        steps["python_install"] = install_hermes_python(cfg)
    steps["patch"] = apply_hermes_patch(cfg)
    if not args.skip_tui_build:
        steps["tui"] = install_tui_dependencies(cfg)
    if not hermes_bin(cfg).exists():
        raise SystemExit(
            "The managed runtime Python is not installed; cannot bootstrap profiles. "
            "Re-run `tag setup` without `--skip-python-install`."
        )
    steps["bootstrap"] = {
        "profiles": bootstrap_profiles(cfg),
        "rendered": render_profiles(cfg, force=False),
    }
    steps["codex_import"] = auto_import_codex_profiles(cfg)
    steps["claude_import"] = auto_import_claude_profiles(cfg)
    steps["gemini_import"] = auto_import_gemini_profiles(cfg)
    steps["continue_import"] = auto_import_continue_profiles(cfg)
    steps["mistral_import"] = auto_import_mistral_profiles(cfg)

    if args.json:
        print(json.dumps(steps, indent=2))
        return 0

    for name, payload in steps.items():
        print(f"{name}: {payload}")
    return 0


def cmd_hermes_passthrough(args: argparse.Namespace) -> int:
    from tag.controller import (
        load_config, config_path, ensure_hermes_ready, profile_exec_env,
        hermes_env, normalize_hermes_passthrough_args, rewrite_cli_hints,
        hermes_bin,
    )
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(
        cfg,
        config_arg=args.config,
        need_tui="--tui" in args.hermes_args,
    )
    env = profile_exec_env(cfg, args.profile) if args.profile else hermes_env(cfg)
    raw_args = list(args.hermes_args)
    hermes_args = normalize_hermes_passthrough_args(raw_args)
    wants_help = any(arg in {"--help", "-h"} for arg in hermes_args)
    if getattr(args, "hermes_version", False):
        if not raw_args:
            hermes_args = ["--version"]
        else:
            hermes_args = ["--version", *hermes_args]
            wants_help = True
    interactive_passthrough = (
        "--tui" in hermes_args
        or (
            hermes_args[:1] in (["gateway"], ["dashboard"])
            and not wants_help
        )
        or (
            hermes_args[:1] == ["chat"]
            and "-q" not in hermes_args
            and "--query" not in hermes_args
            and not wants_help
        )
    )
    capture_output = not interactive_passthrough
    proc = subprocess.run(
        [str(hermes_bin(cfg)), *hermes_args],
        env=env,
        text=True,
        check=False,
        capture_output=capture_output,
    )
    if capture_output:
        stdout = getattr(proc, "stdout", "")
        stderr = getattr(proc, "stderr", "")
        if stdout:
            print(rewrite_cli_hints(stdout), end="")
        if stderr:
            print(rewrite_cli_hints(stderr), end="", file=sys.stderr)
    return int(proc.returncode)


def cmd_tui(args: argparse.Namespace) -> int:
    from tag.controller import (
        normalize_hermes_passthrough_args, can_launch_interactive_tui,
        cmd_hermes_passthrough as _cmd_hermes_passthrough,
    )
    raw_args = list(args.hermes_args)
    normalized_args = normalize_hermes_passthrough_args(raw_args)
    if raw_args and normalized_args in (["--help"], ["-h"]):
        passthrough = argparse.Namespace(
            config=args.config,
            profile=args.profile,
            hermes_args=["--help"],
            hermes_version=False,
        )
        return _cmd_hermes_passthrough(passthrough)
    if not can_launch_interactive_tui() and os.environ.get("TAG_FORCE_TUI", "").strip() not in {"1", "true", "yes"}:
        print(
            "TAG TUI requires an interactive terminal. Use `tag doctor`, `tag setup`, "
            "`tag submit ...`, or rerun in a real TTY. Set TAG_FORCE_TUI=1 to bypass this guard.",
            file=sys.stderr,
        )
        return 2
    forwarded = ["--tui", *args.hermes_args]
    passthrough = argparse.Namespace(
        config=args.config,
        profile=args.profile,
        hermes_args=forwarded,
        hermes_version=False,
    )
    return _cmd_hermes_passthrough(passthrough)


def cmd_hermes_command(args: argparse.Namespace, command_name: str) -> int:
    forwarded = [command_name, *args.hermes_args]
    passthrough = argparse.Namespace(
        config=args.config,
        profile=args.profile,
        hermes_args=forwarded,
        hermes_version=False,
    )
    return cmd_hermes_passthrough(passthrough)


def cmd_completion(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "completion")


def cmd_prompt_size(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "prompt-size")


def cmd_update(args: argparse.Namespace) -> int:
    from tag.controller import load_config, config_path, hermes_root
    cfg = load_config(config_path(args.config))
    root = hermes_root(cfg)
    if root.exists() and (root / ".git").exists():
        return cmd_hermes_command(args, "update")
    setup_args = argparse.Namespace(
        config=args.config,
        refresh=True,
        skip_python_install=False,
        skip_tui_build=False,
        json=getattr(args, "json", False),
    )
    return cmd_setup(setup_args)


def cmd_doctor(args: argparse.Namespace) -> int:
    """PRD-009: Comprehensive health check with pass/warn/fail per component."""
    from tag.controller import (
        load_config, config_path, hermes_env, APP_NAME, package_root,
        managed_root, hermes_root, hermes_bin, rewrite_cli_hints,
        run_hermes, doctor_prerequisites, _doctor_profile_checks,
        _doctor_system_checks, _doctor_hermes_checks, print_doctor_report,
    )
    cfg = load_config(config_path(args.config))
    target_profile = getattr(args, "profile", None)

    if getattr(args, "json", False):
        # Legacy JSON mode: include full report + new per-profile checks
        env = hermes_env(cfg)
        report: dict[str, Any] = {
            "app_name": APP_NAME,
            "package_root": str(package_root()),
            "tag_home": str(tag_home()),
            "managed_root": str(managed_root()),
            "tag_runtime_root": rewrite_cli_hints(str(hermes_root(cfg))),
            "tag_bin_exists": hermes_bin(cfg).exists(),
            "home": env["HOME"],
            "tag_runtime_home": env["HERMES_HOME"],
            "codex_home": env["CODEX_HOME"],
            "config": str(config_path(args.config)),
            "benchmark_suite": str(benchmark_suite_path(None)),
            "prerequisites": doctor_prerequisites(cfg),
        }
        if hermes_bin(cfg).exists():
            try:
                report["tag_version"] = rewrite_cli_hints(run_hermes(cfg, "--version").stdout.strip())
            except subprocess.CalledProcessError as exc:
                report["tag_version_error"] = exc.stderr.strip()
        else:
            report["tag_version"] = "not provisioned yet"

        # Include the same system/runtime checks as text mode so runtime/patch
        # failures land in the payload AND affect the exit code (parity with text).
        system_checks = _doctor_system_checks(cfg)
        hermes_checks = _doctor_hermes_checks(cfg)
        report["system"] = system_checks
        report["tag runtime"] = hermes_checks

        profiles_report: dict[str, Any] = {}
        defined_profiles = _config_profiles(cfg)
        profiles_to_check = (
            [target_profile] if target_profile is not None
            else list(defined_profiles.keys())
        )
        for p in profiles_to_check:
            if p not in defined_profiles:
                profiles_report[p] = [{
                    "name": "profile",
                    "status": "fail",
                    "message": f"unknown profile '{p}' — not defined in config",
                }]
            else:
                profiles_report[p] = _doctor_profile_checks(cfg, p)
        report["profiles"] = profiles_report
        print(json.dumps(report, indent=2))
        has_fail = any(
            c.get("status") == "fail"
            for checks_list in (system_checks, hermes_checks, *profiles_report.values())
            for c in checks_list
        )
        return 1 if has_fail else 0

    # Rich / plain-text grouped report
    groups: dict[str, list[dict[str, Any]]] = {}
    groups["system"] = _doctor_system_checks(cfg)
    groups["tag runtime"] = _doctor_hermes_checks(cfg)

    defined_profiles = _config_profiles(cfg)
    profiles_to_check = (
        [target_profile] if target_profile is not None
        else list(defined_profiles.keys())
    )
    for p in profiles_to_check:
        if p not in defined_profiles:
            groups[f"profile: {p}"] = [{
                "name": "profile",
                "status": "fail",
                "message": f"unknown profile '{p}' — not defined in config",
            }]
        else:
            groups[f"profile: {p}"] = _doctor_profile_checks(cfg, p)

    print_doctor_report(groups)

    all_statuses = [c["status"] for checks in groups.values() for c in checks]
    if any(s == "fail" for s in all_statuses):
        return 1
    return 0


def cmd_bootstrap(args: argparse.Namespace) -> int:
    from tag.controller import (
        load_config, config_path, ensure_hermes_ready,
        bootstrap_profiles, render_profiles, rewrite_cli_hints,
    )
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    created = bootstrap_profiles(cfg)
    rendered = render_profiles(cfg, force=args.force)
    result = {"profiles": created, "rendered": rendered}
    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    print("Profiles:")
    for item in created:
        print(f"  {item['profile']}: {item['status']}")
    print("Rendered:")
    for item in rendered:
        print(f"  {item['profile']}: {rewrite_cli_hints(item['config'])}")
    return 0


def cmd_render(args: argparse.Namespace) -> int:
    from tag.controller import (
        load_config, config_path, render_profiles, rewrite_cli_hints,
    )
    cfg = load_config(config_path(args.config))
    rendered = render_profiles(cfg, force=args.force)
    if args.json:
        print(json.dumps(rendered, indent=2))
        return 0
    if not rendered:
        print_warning("no profiles defined in config")
        return 0
    for item in rendered:
        print(f"{item['profile']}: {rewrite_cli_hints(item['config'])}")
    return 0


def cmd_route(args: argparse.Namespace) -> int:
    from tag.controller import (
        load_config, config_path, resolve_route, apply_route_model_overrides,
    )
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
    print(f"master: {route['master']['name']} -> {route['master']['model']}")
    for worker in route["workers"]:
        print(f"worker: {worker['name']} -> {worker['model']}")
    if route["verifier"]:
        print(f"verifier: {route['verifier']['name']} -> {route['verifier']['model']}")
    return 0


def cmd_env(args: argparse.Namespace) -> int:
    from tag.controller import load_config, config_path, hermes_env
    cfg = load_config(config_path(args.config))
    env = hermes_env(cfg)
    for key in ("HOME", "HERMES_HOME", "CODEX_HOME", "PATH"):
        print(f"{key}={env[key]}")
    return 0


def register(sub: argparse._SubParsersAction) -> None:  # type: ignore[type-arg]
    """Register all system/setup commands in the given subparsers action."""

    setup = sub.add_parser("setup", help="Provision the managed runtime, apply TAG patches, build the TUI, and bootstrap profiles")
    setup.add_argument("--refresh", action="store_true", help="Fetch and update an existing managed runtime checkout")
    setup.add_argument("--skip-python-install", action="store_true")
    setup.add_argument("--skip-tui-build", action="store_true")
    setup.add_argument("--json", action="store_true")
    setup.set_defaults(func=cmd_setup)

    doctor = sub.add_parser("doctor", help="Validate local TAG paths and the managed runtime")
    doctor.add_argument("--json", action="store_true")
    doctor.add_argument("--profile", metavar="NAME", help="Check only this profile")
    doctor.set_defaults(func=cmd_doctor)

    bootstrap = sub.add_parser("bootstrap", help="Create profiles and render config")
    bootstrap.add_argument("--force", action="store_true", help="Overwrite rendered files")
    bootstrap.add_argument("--json", action="store_true")
    bootstrap.set_defaults(func=cmd_bootstrap)

    render = sub.add_parser("render", help="Render per-profile config only")
    render.add_argument("--force", action="store_true", help="Overwrite rendered files")
    render.add_argument("--json", action="store_true")
    render.set_defaults(func=cmd_render)

    env_cmd = sub.add_parser("env", help="Print the isolated environment values")
    env_cmd.set_defaults(func=cmd_env)

    hermes_cmd = sub.add_parser("runtime", help="Pass raw arguments through to the managed runtime binary")
    hermes_cmd.add_argument("--profile", help="Run the managed runtime inside one TAG profile home")
    hermes_cmd.add_argument("--version", dest="hermes_version", action="store_true", help="Show the managed runtime version")
    hermes_cmd.add_argument("hermes_args", nargs=argparse.REMAINDER, metavar="...")
    hermes_cmd.set_defaults(func=cmd_hermes_passthrough)

    tui = sub.add_parser("tui", help="Launch the managed TUI through TAG")
    tui.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    tui.add_argument("hermes_args", nargs=argparse.REMAINDER, metavar="...")
    tui.set_defaults(func=cmd_tui)

    completion = sub.add_parser("completion", help="Run completion inside a TAG profile")
    completion.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    completion.add_argument("hermes_args", nargs=argparse.REMAINDER, metavar="...")
    completion.set_defaults(func=cmd_completion)

    prompt_size = sub.add_parser("prompt-size", help="Run prompt-size inside a TAG profile")
    prompt_size.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    prompt_size.add_argument("hermes_args", nargs=argparse.REMAINDER, metavar="...")
    prompt_size.set_defaults(func=cmd_prompt_size)

    update = sub.add_parser("update", help="Run update inside a TAG profile")
    update.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    update.add_argument("--json", action="store_true", help="When TAG manages the update locally, emit JSON")
    update.add_argument("hermes_args", nargs=argparse.REMAINDER, metavar="...")
    update.set_defaults(func=cmd_update)
