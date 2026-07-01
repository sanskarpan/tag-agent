"""Rich-based terminal output helpers for TAG CLI commands (PRD-003).

Gracefully degrades to plain text when Rich is not available or stdout is not a TTY.
All public functions are safe to call unconditionally.
"""
from __future__ import annotations

import os
import subprocess
import sys
from contextlib import contextmanager
from typing import TYPE_CHECKING, Any, Iterator

if TYPE_CHECKING:
    from rich.console import Console
    from rich.progress import Progress

try:
    from rich.console import Console as _Console
    from rich.live import Live
    from rich.progress import (
        BarColumn,
        Progress,
        SpinnerColumn,
        TaskID,
        TextColumn,
        TimeElapsedColumn,
    )
    from rich.spinner import Spinner

    _RICH_AVAILABLE = True
except ImportError:  # pragma: no cover — Rich may not be installed in tests
    _RICH_AVAILABLE = False


def _is_tty() -> bool:
    return sys.stdout.isatty() and sys.stderr.isatty()


def _no_color() -> bool:
    return (
        os.environ.get("NO_COLOR", "") != ""
        or os.environ.get("TAG_NO_COLOR", "") != ""
    )


def get_console() -> "Console | None":
    """Return a Rich Console writing to stderr, or None if Rich unavailable / not a TTY."""
    if not _RICH_AVAILABLE or not _is_tty() or _no_color():
        return None
    return _Console(stderr=True, highlight=False)


# ---------------------------------------------------------------------------
# Spinner / status
# ---------------------------------------------------------------------------

@contextmanager
def chat_spinner(profile: str, model: str) -> Iterator[None]:
    """Show a spinner while waiting for a model response.  Falls back to nothing."""
    console = get_console()
    if console is None:
        yield
        return
    label = f"[bold cyan]{profile}[/]"
    if model:
        label += f" · [dim]{model}[/]"
    label += " · thinking…"
    with console.status(label, spinner="dots"):
        yield


# ---------------------------------------------------------------------------
# Output streaming
# ---------------------------------------------------------------------------

def stream_output(text: str, *, end: str = "") -> None:
    """Print model output, bypassing Rich markup so raw text is preserved."""
    console = get_console()
    if console is None:
        print(text, end=end, flush=True)
        return
    console.out(text, end=end)


def print_status_bar(
    profile: str,
    model: str,
    in_tokens: int,
    out_tokens: int,
    elapsed: float,
) -> None:
    """Print a one-line status summary after a chat turn completes."""
    console = get_console()
    if console is None:
        parts = [f"▸ {profile}"]
        if model:
            parts.append(model)
        if in_tokens or out_tokens:
            parts.append(f"{in_tokens:,}↑ {out_tokens:,}↓")
        parts.append(f"{elapsed:.1f}s")
        print("  " + " · ".join(parts), file=sys.stderr)
        return
    parts: list[str] = [f"[bold cyan]{profile}[/bold cyan]"]
    if model:
        parts.append(f"[dim]{model}[/dim]")
    if in_tokens or out_tokens:
        parts.append(f"[dim]{in_tokens:,}↑ {out_tokens:,}↓[/dim]")
    parts.append(f"[dim]{elapsed:.1f}s[/dim]")
    console.print("[dim]▸[/dim] " + " [dim]·[/dim] ".join(parts))


# ---------------------------------------------------------------------------
# Progress bars
# ---------------------------------------------------------------------------

def make_benchmark_progress() -> "Progress | None":
    """Create a Rich Progress bar for benchmark runs. Returns None if unavailable."""
    if not _RICH_AVAILABLE or not _is_tty() or _no_color():
        return None
    return Progress(
        SpinnerColumn(),
        TextColumn("[bold]{task.description}"),
        BarColumn(),
        TextColumn("[progress.percentage]{task.percentage:>3.0f}%"),
        TextColumn("({task.completed}/{task.total})"),
        TimeElapsedColumn(),
        transient=False,
    )


def make_submit_progress() -> "Progress | None":
    """Create a Rich Progress bar for submit / swarm steps. Returns None if unavailable."""
    if not _RICH_AVAILABLE or not _is_tty() or _no_color():
        return None
    return Progress(
        SpinnerColumn(),
        TextColumn("[bold cyan]{task.description}"),
        TimeElapsedColumn(),
        transient=False,
    )


# ---------------------------------------------------------------------------
# Messaging helpers
# ---------------------------------------------------------------------------

def print_error(msg: str) -> None:
    """Print an error message, highlighted red on TTY."""
    console = get_console()
    if console is None:
        print(f"error: {msg}", file=sys.stderr)
        return
    console.print(f"[bold red]error:[/bold red] {msg}")


def print_success(msg: str) -> None:
    """Print a success message with a green tick on TTY."""
    console = get_console()
    if console is None:
        print(msg)
        return
    console.print(f"[bold green]✓[/bold green] {msg}")


def print_warning(msg: str) -> None:
    """Print a warning message, highlighted yellow on TTY."""
    console = get_console()
    if console is None:
        print(f"warning: {msg}", file=sys.stderr)
        return
    console.print(f"[bold yellow]⚠[/bold yellow] {msg}", stderr=True)


# ---------------------------------------------------------------------------
# Doctor / health report
# ---------------------------------------------------------------------------

def print_doctor_report(groups: dict[str, list[dict[str, Any]]]) -> None:
    """Render a pass/warn/fail health report.

    groups: {group_name: [{name, status, message, fix_cmd?}]}
    """
    console = get_console()
    _STATUS_ICON = {"pass": "✓", "warn": "⚠", "fail": "✗"}
    _STATUS_COLOR = {"pass": "green", "warn": "yellow", "fail": "red"}

    totals = {"pass": 0, "warn": 0, "fail": 0}

    for group, checks in groups.items():
        if console:
            console.print(f"\n[bold]{group.upper()}[/bold]")
        else:
            print(f"\n{group.upper()}")

        for check in checks:
            st = check.get("status", "pass")
            icon = _STATUS_ICON.get(st, "?")
            msg = check.get("message", "")
            name = check.get("name", "?")
            fix = check.get("fix_cmd")

            totals[st] = totals.get(st, 0) + 1

            if console:
                color = _STATUS_COLOR.get(st, "white")
                line = f"  [{color}]{icon}[/{color}] [dim]{name:<28}[/dim] {msg}"
                if st != "pass" and fix:
                    line += f"\n      [dim]→ run: [italic]{fix}[/italic][/dim]"
                console.print(line)
            else:
                line = f"  {icon} {name:<28} {msg}"
                if st != "pass" and fix:
                    line += f"\n    → run: {fix}"
                print(line)

    # Summary line
    summary = (
        f"{totals.get('pass',0)} pass, "
        f"{totals.get('warn',0)} warn, "
        f"{totals.get('fail',0)} fail"
    )
    if console:
        color = "red" if totals.get("fail", 0) else ("yellow" if totals.get("warn", 0) else "green")
        console.print(f"\n[dim]Summary: [{color}]{summary}[/{color}][/dim]")
    else:
        print(f"\nSummary: {summary}")


# ---------------------------------------------------------------------------
# Desktop notification (PRD-008 / PRD-016)
# ---------------------------------------------------------------------------

def send_desktop_notification(title: str, message: str) -> None:
    """Send a native desktop notification. Silently no-ops if not supported."""
    import platform

    system = platform.system()
    try:
        if system == "Darwin":
            # Pass message/title as osascript arguments (argv), never
            # interpolated into the script text — otherwise a double-quote in
            # the message escapes the string and `do shell script` runs
            # arbitrary shell (local code execution).
            script = (
                "on run {msg, ttl}\n"
                "display notification msg with title ttl\n"
                "end run"
            )
            subprocess.run(
                ["osascript", "-e", script, message, title],
                check=False,
                capture_output=True,
                timeout=5,
            )
        elif system == "Linux":
            subprocess.run(
                ["notify-send", title, message],
                check=False,
                capture_output=True,
                timeout=5,
            )
        # Windows: silently skipped for now
    except (OSError, subprocess.TimeoutExpired):
        pass

