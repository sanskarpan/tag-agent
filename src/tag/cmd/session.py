"""Session management and basic UI commands."""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
from pathlib import Path
from typing import Any

from tag.core.config import load_config, save_config, config_path
from tag.core.paths import (
    hermes_root, hermes_bin, runtime_home, hermes_env, profile_home,
    profile_exec_env, tag_home, tag_cli_label,
    can_launch_interactive_tui,
)
from tag.core.db import open_db
from tag.core.run import run_hermes, run_profile_hermes, run_profile_python
from tag.core.utils import nonnegative_int, positive_int, utc_now, rewrite_cli_hints

try:
    from tag.tui_output import print_error, print_success, print_warning, chat_spinner, send_desktop_notification
    _TUI_AVAILABLE = True
except Exception:
    _TUI_AVAILABLE = False
    def print_error(msg): print(f"error: {msg}", file=sys.stderr)
    def print_success(msg): print(msg)
    def print_warning(msg): print(f"warning: {msg}", file=sys.stderr)
    def chat_spinner(*a, **kw):
        import contextlib; return contextlib.nullcontext()
    def send_desktop_notification(*a, **kw): pass


# ---------------------------------------------------------------------------
# Internal helpers
# ---------------------------------------------------------------------------

def _normalize_hermes_passthrough_args(args: list[str]) -> list[str]:
    normalized = list(args)
    if normalized[:1] == ["--"]:
        normalized = normalized[1:]
    if len(normalized) >= 2 and normalized[1] == "--":
        normalized = [normalized[0], *normalized[2:]]
    if not normalized:
        return ["--help"]
    return normalized


def _ensure_hermes_ready(
    cfg: dict[str, Any],
    *,
    config_arg: str | None,
    need_tui: bool,
) -> None:
    if hermes_bin(cfg).exists():
        return
    # Lazy import to avoid circular dependency
    from tag.controller import cmd_setup  # type: ignore[import]
    setup_args = argparse.Namespace(
        config=config_arg,
        refresh=False,
        skip_python_install=False,
        skip_tui_build=not need_tui,
        json=False,
    )
    cmd_setup(setup_args)


def _cmd_hermes_passthrough(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    _ensure_hermes_ready(
        cfg,
        config_arg=args.config,
        need_tui="--tui" in args.hermes_args,
    )
    env = profile_exec_env(cfg, args.profile) if args.profile else hermes_env(cfg)
    raw_args = list(args.hermes_args)
    hermes_args = _normalize_hermes_passthrough_args(raw_args)
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


def _cmd_hermes_command(args: argparse.Namespace, command_name: str) -> int:
    forwarded = [command_name, *args.hermes_args]
    passthrough = argparse.Namespace(
        config=args.config,
        profile=args.profile,
        hermes_args=forwarded,
        hermes_version=False,
    )
    return _cmd_hermes_passthrough(passthrough)


# ---------------------------------------------------------------------------
# Dashboard helpers
# ---------------------------------------------------------------------------

def _queue_list_jobs(
    db: Any,
    *,
    status: str | None = None,
    profile: str | None = None,
    limit: int = 50,
) -> list[dict[str, Any]]:
    query = "SELECT * FROM queue_jobs WHERE 1=1"
    params: list[Any] = []
    if status:
        query += " AND status=?"
        params.append(status)
    if profile:
        query += " AND profile=?"
        params.append(profile)
    query += " ORDER BY created_at DESC LIMIT ?"
    params.append(limit)
    return [dict(r) for r in db.execute(query, params).fetchall()]


def _dashboard_snapshot(cfg: dict[str, Any]) -> dict[str, Any]:
    """Read current TAG state for dashboard display — pure SQLite, no hermes."""
    snap: dict[str, Any] = {"runs": [], "queue": [], "journal_count": 0, "kanban": {}}
    try:
        db = open_db(cfg)
        rows = db.execute(
            "SELECT id AS run_id, kind, task_type, master_profile, status, "
            "created_at FROM runs ORDER BY created_at DESC LIMIT 20"
        ).fetchall()
        snap["runs"] = [dict(r) for r in rows]
        snap["queue"] = _queue_list_jobs(db, status=None)
        snap["journal_count"] = db.execute(
            "SELECT COUNT(*) FROM memory_journal"
        ).fetchone()[0]
        db.close()
    except Exception:
        pass

    import tag.kanban as _kanban  # type: ignore[import]
    kanban_by_profile: dict[str, Any] = {}
    for pname in cfg.get("profiles", {}):
        try:
            kpath = _kanban.profile_kanban_db_path(cfg, pname)
            if not kpath.exists():
                continue
            kconn = _kanban.open_db(kpath)
            tasks = _kanban.list_tasks(kconn)
            kconn.close()
            by_status: dict[str, int] = {}
            for t in tasks:
                by_status[t["status"]] = by_status.get(t["status"], 0) + 1
            kanban_by_profile[pname] = {"total": len(tasks), "by_status": by_status}
        except Exception:
            pass
    snap["kanban"] = kanban_by_profile
    return snap


def _render_dashboard_plain(snap: dict[str, Any], profile: str) -> None:
    """Print a static dashboard snapshot (fallback when Rich unavailable)."""
    import datetime
    print(f"\n=== TAG Dashboard  (profile: {profile}) ===")

    runs = snap.get("runs", [])
    print(f"\nRuns ({len(runs)} recent):")
    if runs:
        print(f"  {'ID':<10} {'KIND':<10} {'PROFILE':<16} {'STATUS':<12} WHEN")
        for r in runs[:10]:
            try:
                ts = datetime.datetime.fromisoformat(r.get("created_at") or "").strftime("%H:%M")
            except Exception:
                ts = "?"
            print(f"  {r['run_id']:<10} {r['kind']:<10} {r['master_profile']:<16} "
                  f"{r['status']:<12} {ts}")
    else:
        print("  (none)")

    queue = snap.get("queue", [])
    print(f"\nQueue ({len(queue)} jobs):")
    if queue:
        for j in queue[:8]:
            print(f"  [{j['id']}] {j['status']:<10} {j.get('task','')[:40]}")
    else:
        print("  (empty)")

    print(f"\nMemory journal entries: {snap.get('journal_count', 0)}")

    kanban = snap.get("kanban", {})
    if kanban:
        print("\nKanban boards:")
        for pname, info in kanban.items():
            by_s = info.get("by_status", {})
            parts = ", ".join(f"{s}:{n}" for s, n in by_s.items())
            print(f"  {pname}: {info['total']} tasks  [{parts}]")


# ---------------------------------------------------------------------------
# Desktop helpers
# ---------------------------------------------------------------------------

def _desktop_app_path(cfg: dict[str, Any]) -> Path | None:
    """Return the built Electron app binary path, or None if not built."""
    import platform
    from tag.controller import desktop_build_root  # type: ignore[import]
    build_root = desktop_build_root(cfg)
    system = platform.system()

    if system == "Darwin":
        apps = list((build_root / "build").glob("*.app/Contents/MacOS/*")) if (build_root / "build").exists() else []
        return apps[0] if apps else None
    if system == "Linux":
        build_dir = build_root / "build"
        if build_dir.exists():
            appimages = list(build_dir.glob("*.AppImage"))
            unpacked = list((build_dir / "linux-unpacked").glob("*")) if (build_dir / "linux-unpacked").exists() else []
            candidates = appimages + [p for p in unpacked if p.is_file() and os.access(p, os.X_OK)]
            return candidates[0] if candidates else None
    if system == "Windows":
        build_dir = build_root / "build" / "win-unpacked"
        if build_dir.exists():
            exes = list(build_dir.glob("*.exe"))
            return exes[0] if exes else None
    return None


# ---------------------------------------------------------------------------
# Command handlers
# ---------------------------------------------------------------------------

def cmd_chat(args: argparse.Namespace) -> int:
    return _cmd_hermes_command(args, "chat")


def cmd_gateway(args: argparse.Namespace) -> int:
    return _cmd_hermes_command(args, "gateway")


def cmd_kanban(args: argparse.Namespace) -> int:
    return _cmd_hermes_command(args, "kanban")


def cmd_model(args: argparse.Namespace) -> int:
    return _cmd_hermes_command(args, "model")


def cmd_profile(args: argparse.Namespace) -> int:
    return _cmd_hermes_command(args, "profile")


def cmd_status(args: argparse.Namespace) -> int:
    return _cmd_hermes_command(args, "status")


def cmd_config(args: argparse.Namespace) -> int:
    return _cmd_hermes_command(args, "config")


def cmd_sessions(args: argparse.Namespace) -> int:
    return _cmd_hermes_command(args, "sessions")


def cmd_skills(args: argparse.Namespace) -> int:
    return _cmd_hermes_command(args, "skills")


def cmd_plugins(args: argparse.Namespace) -> int:
    return _cmd_hermes_command(args, "plugins")


def cmd_tools(args: argparse.Namespace) -> int:
    return _cmd_hermes_command(args, "tools")


def cmd_mcp(args: argparse.Namespace) -> int:
    return _cmd_hermes_command(args, "mcp")


def cmd_logs(args: argparse.Namespace) -> int:
    return _cmd_hermes_command(args, "logs")


def cmd_dashboard(args: argparse.Namespace) -> int:
    """TAG-native live dashboard — reads directly from TAG's SQLite state (PRD-010).

    No hermes binary dependency. Shows runs, queue, journal, and kanban
    board status for all profiles. Refreshes every few seconds.
    Use --no-browser to suppress the browser open (legacy flag, kept for
    CLI compat; dashboard is terminal-only).
    """
    cfg = load_config(config_path(args.config))
    profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
    if profile not in cfg.get("profiles", {}):
        print(f"warning: unknown profile '{profile}'", file=sys.stderr)
    refresh_secs = getattr(args, "port", None) or 3  # --port reused as refresh interval
    # Note: --port is repurposed here as refresh_seconds for the live view.
    # A value >=10 is assumed to be a port (legacy hermes mode); <=9 is refresh rate.
    if isinstance(refresh_secs, int) and refresh_secs >= 10:
        refresh_secs = 3

    try:
        from rich.console import Console
        from rich.live import Live
        from rich.table import Table
        from rich.panel import Panel
        from rich import box
        import datetime

        console = Console()

        def make_layout() -> Panel:
            snap = _dashboard_snapshot(cfg)

            run_table = Table(box=box.SIMPLE, show_header=True, header_style="bold cyan",
                              expand=True, min_width=60)
            run_table.add_column("ID", width=10)
            run_table.add_column("Kind", width=10)
            run_table.add_column("Profile", width=16)
            run_table.add_column("Status", width=12)
            run_table.add_column("When", width=8)
            for r in snap.get("runs", [])[:8]:
                try:
                    ts = datetime.datetime.fromisoformat(r.get("created_at") or "").strftime("%H:%M")
                except Exception:
                    ts = "?"
                s = r["status"]
                style = "green" if s == "completed" else "red" if s == "failed" else "yellow"
                run_table.add_row(r["run_id"], r["kind"], r["master_profile"],
                                  f"[{style}]{s}[/{style}]", ts)

            q_table = Table(box=box.SIMPLE, show_header=True, header_style="bold cyan",
                            expand=True, min_width=60)
            q_table.add_column("Job", width=10)
            q_table.add_column("Status", width=12)
            q_table.add_column("Task", width=40)
            for j in snap.get("queue", [])[:6]:
                s = j["status"]
                style = "green" if s == "done" else "red" if s in ("failed", "cancelled") else "yellow"
                q_table.add_row(j["id"], f"[{style}]{s}[/{style}]", (j.get("task") or "")[:40])

            kb_table = Table(box=box.SIMPLE, show_header=True, header_style="bold cyan",
                             expand=True, min_width=40)
            kb_table.add_column("Profile", width=16)
            kb_table.add_column("Total", width=6)
            kb_table.add_column("Ready/Running", width=14)
            kb_table.add_column("Done", width=6)
            for pname, info in snap.get("kanban", {}).items():
                by_s = info.get("by_status", {})
                active = by_s.get("ready", 0) + by_s.get("running", 0)
                done = by_s.get("done", 0)
                kb_table.add_row(pname, str(info["total"]), str(active), str(done))

            from rich.columns import Columns
            from rich.text import Text
            header = Text(
                f"TAG Dashboard  ·  profile: {profile}  ·  "
                f"journal entries: {snap.get('journal_count', 0)}  ·  "
                f"Press Ctrl+C to exit",
                style="bold",
            )
            from rich.layout import Layout
            layout = Layout()
            layout.split_column(
                Layout(header, size=1),
                Layout(Panel(run_table, title="[bold]Runs[/bold]"), name="runs"),
                Layout(
                    Columns([
                        Panel(q_table, title="[bold]Queue[/bold]"),
                        Panel(kb_table, title="[bold]Kanban[/bold]"),
                    ]),
                    name="bottom",
                    size=12,
                ),
            )
            return Panel(layout, title="[bold blue]TAG[/bold blue]", border_style="blue")

        with Live(make_layout(), console=console, refresh_per_second=0.5,
                  screen=True) as live:
            while True:
                time.sleep(refresh_secs)
                live.update(make_layout())

    except KeyboardInterrupt:
        pass
    except ImportError:
        # Rich not available — static snapshot
        snap = _dashboard_snapshot(cfg)
        _render_dashboard_plain(snap, profile)
    return 0


def cmd_desktop(args: argparse.Namespace) -> int:
    """PRD-007: Build and launch the Electron desktop app."""
    cfg = load_config(config_path(args.config))
    _ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    sub = getattr(args, "desktop_subcommand", "open")

    if sub == "build":
        print("Building Electron desktop app (this may take 2-3 minutes)...")
        from tag.controller import build_desktop_app  # type: ignore[import]
        result = build_desktop_app(cfg, force=getattr(args, "force", False))
        if getattr(args, "json", False):
            print(json.dumps(result, indent=2))
            return 0
        if result["status"] == "built":
            print(f"Built: {result['app_path']}")
            return 0
        print(
            f"Build failed ({result['status']}): {result.get('message', result.get('stderr', ''))}",
            file=sys.stderr,
        )
        return 1

    # sub == "open"
    app = _desktop_app_path(cfg)
    if not app:
        print("Desktop app not built. Run: tag desktop build", file=sys.stderr)
        return 1

    profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
    env = {**os.environ, **profile_exec_env(cfg, profile), "TAG_DESKTOP_PROFILE": profile}
    subprocess.Popen([str(app)], env=env)
    print(f"Launched desktop (profile: {profile})")
    return 0


def cmd_default(args: argparse.Namespace) -> int:
    if not can_launch_interactive_tui():
        print(
            "TAG detected a non-interactive shell, so it will not auto-launch the TUI.\n"
            "Run `tag doctor` to inspect the install, `tag setup` to bootstrap the managed runtime, "
            "or `tag submit ...` / `tag hermes ...` for non-interactive usage.",
            file=sys.stderr,
        )
        return 2
    cfg = load_config(config_path(args.config))
    if not hermes_bin(cfg).exists():
        setup_args = argparse.Namespace(
            config=args.config,
            refresh=False,
            skip_python_install=False,
            skip_tui_build=False,
            json=False,
        )
        from tag.controller import cmd_setup  # type: ignore[import]
        cmd_setup(setup_args)
    else:
        from tag.controller import bootstrap_profiles, render_profiles  # type: ignore[import]
        bootstrap_profiles(cfg)
        render_profiles(cfg, force=False)
    from tag.controller import cmd_tui  # type: ignore[import]
    tui_args = argparse.Namespace(config=args.config, profile="orchestrator", hermes_args=[])
    return cmd_tui(tui_args)


# ---------------------------------------------------------------------------
# Parser registration
# ---------------------------------------------------------------------------

def register(sub: argparse._SubParsersAction) -> None:  # type: ignore[type-arg]
    """Register all session/UI subcommands onto the given subparser action."""

    # chat
    chat = sub.add_parser("chat", help="Run chat inside a TAG profile")
    chat.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    chat.add_argument("hermes_args", nargs=argparse.REMAINDER, metavar="...")
    chat.set_defaults(func=cmd_chat)

    # gateway
    gateway = sub.add_parser("gateway", help="Run gateway commands inside a TAG profile")
    gateway.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    gateway.add_argument("hermes_args", nargs=argparse.REMAINDER, metavar="...")
    gateway.set_defaults(func=cmd_gateway)

    # kanban
    kanban = sub.add_parser("kanban", help="Run Kanban commands inside a TAG profile")
    kanban.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    kanban.add_argument("hermes_args", nargs=argparse.REMAINDER, metavar="...")
    kanban.set_defaults(func=cmd_kanban)

    # model
    model = sub.add_parser("model", help="Run model commands inside a TAG profile")
    model.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    model.add_argument("hermes_args", nargs=argparse.REMAINDER, metavar="...")
    model.set_defaults(func=cmd_model)

    # profile
    profile = sub.add_parser("profile", help="Run profile commands in the managed TAG environment")
    profile.add_argument("--profile", help="Optional active profile home override")
    profile.add_argument("hermes_args", nargs=argparse.REMAINDER, metavar="...")
    profile.set_defaults(func=cmd_profile)

    # status
    status = sub.add_parser("status", help="Run status inside a TAG profile")
    status.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    status.add_argument("hermes_args", nargs=argparse.REMAINDER, metavar="...")
    status.set_defaults(func=cmd_status)

    # config
    config_cmd = sub.add_parser("config", help="Run config inside a TAG profile")
    config_cmd.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    config_cmd.add_argument("hermes_args", nargs=argparse.REMAINDER, metavar="...")
    config_cmd.set_defaults(func=cmd_config)

    # sessions
    sessions = sub.add_parser("sessions", help="Run sessions inside a TAG profile")
    sessions.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    sessions.add_argument("hermes_args", nargs=argparse.REMAINDER, metavar="...")
    sessions.set_defaults(func=cmd_sessions)

    # skills
    skills = sub.add_parser("skills", help="Run skills inside a TAG profile")
    skills.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    skills.add_argument("hermes_args", nargs=argparse.REMAINDER, metavar="...")
    skills.set_defaults(func=cmd_skills)

    # plugins
    plugins = sub.add_parser("plugins", help="Run plugins inside a TAG profile")
    plugins.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    plugins.add_argument("hermes_args", nargs=argparse.REMAINDER, metavar="...")
    plugins.set_defaults(func=cmd_plugins)

    # tools
    tools_cmd = sub.add_parser("tools", help="Run tools inside a TAG profile")
    tools_cmd.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    tools_cmd.add_argument("hermes_args", nargs=argparse.REMAINDER, metavar="...")
    tools_cmd.set_defaults(func=cmd_tools)

    # mcp
    mcp = sub.add_parser("mcp", help="Run MCP commands inside a TAG profile")
    mcp.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    mcp.add_argument("hermes_args", nargs=argparse.REMAINDER, metavar="...")
    mcp.set_defaults(func=cmd_mcp)

    # logs
    logs = sub.add_parser("logs", help="Run logs inside a TAG profile")
    logs.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    logs.add_argument("hermes_args", nargs=argparse.REMAINDER, metavar="...")
    logs.set_defaults(func=cmd_logs)

    # dashboard
    dashboard = sub.add_parser("dashboard", help="Run dashboard inside a TAG profile")
    dashboard.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    dashboard.add_argument("--port", type=int, metavar="N", help="Dashboard port (default: 3333)")
    dashboard.add_argument("--no-browser", action="store_false", dest="open_browser",
                           help="Print URL only; don't open browser tab")
    dashboard.add_argument("hermes_args", nargs=argparse.REMAINDER, metavar="...")
    dashboard.set_defaults(func=cmd_dashboard)

    # desktop
    desktop = sub.add_parser("desktop", help="Build and launch Electron desktop app")
    desktop_sub = desktop.add_subparsers(dest="desktop_subcommand")
    desktop_open = desktop_sub.add_parser("open", help="Launch the desktop app")
    desktop_open.add_argument("--profile", help="Profile to launch with")
    desktop_build = desktop_sub.add_parser("build", help="Build the desktop app (one-time, ~2-3 min)")
    desktop_build.add_argument("--force", action="store_true")
    desktop_build.add_argument("--json", action="store_true")
    for dp in [desktop, desktop_open, desktop_build]:
        dp.set_defaults(func=cmd_desktop)
