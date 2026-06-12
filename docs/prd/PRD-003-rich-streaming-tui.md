# PRD-003: Rich Streaming TUI Output

**Status:** Proposed  
**Priority:** P0 (Highest Visible Impact)  
**Estimated Effort:** M (2 weeks)  
**Affects:** `controller.py` (`cmd_chat`, `cmd_submit`, `run_chat_step`), new `tag/tui.py` module

---

## 1. Overview

TAG's terminal output is currently raw text: no spinners, no progress indicators, no color, no streaming. Users running `tag chat` or `tag submit` see either nothing (while the model thinks) or a wall of text when output arrives. Hermes itself ships `rich==14.3.3` as a core dependency, meaning the library is already available in the Hermes venv. This PRD defines a Rich-based output layer for TAG that delivers a Claude Code-style terminal experience: live streaming, a status bar, spinner animation, and color-coded output — with zero new pip dependencies.

---

## 2. Problem Statement

- `tag chat` shows nothing while the model generates, then dumps output — users don't know if it's working.
- `tag submit` runs tasks in the background with no progress feedback.
- `tag benchmark` runs N tasks with no per-task status indicator.
- There is no way to see current profile, model, or token usage from the terminal.
- Every competing tool (Claude Code, Cursor, Aider, Continue) has rich terminal output. TAG looks unfinished by comparison.

---

## 3. Goals

1. `tag chat` streams output token-by-token with a spinner while waiting.
2. A persistent status bar shows: profile name, model, token count (in/out), elapsed time.
3. `tag submit` shows a Rich progress bar with per-step completion.
4. `tag benchmark` shows a per-case progress panel.
5. Error output is color-coded red; model output uses a subtle color scheme.
6. All Rich output gracefully degrades to plain text when `stdout` is not a TTY (pipes, CI).

---

## 4. Non-Goals

- Replacing the Hermes TUI (`tag tui`) — Rich output is for non-TUI commands only.
- Interactive input in the Rich panel — Rich is output-only here.
- Animated graphics or ASCII art.

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | see a spinner while `tag chat` waits for the model | I know the agent is working |
| U2 | Developer | see streaming output token-by-token | I can read the response as it arrives |
| U3 | Developer | see "researcher / deepseek-v4-flash / 1,234 tokens" in the status bar | I know what model is running and its cost |
| U4 | Developer | run `tag benchmark` and see a progress bar | I know how many cases remain |
| U5 | CI | pipe `tag chat` output to a file | I get clean text, no ANSI codes |

---

## 6. Technical Design

### 6.1 New module: `src/tag/tui_output.py`

```python
"""Rich-based output helpers for TAG CLI commands."""
from __future__ import annotations
import sys
from contextlib import contextmanager
from typing import Iterator

try:
    from rich.console import Console
    from rich.live import Live
    from rich.spinner import Spinner
    from rich.progress import Progress, SpinnerColumn, TextColumn, BarColumn, TimeElapsedColumn
    from rich.panel import Panel
    from rich.text import Text
    RICH_AVAILABLE = True
except ImportError:
    RICH_AVAILABLE = False


def _is_tty() -> bool:
    return sys.stdout.isatty() and sys.stderr.isatty()


def get_console() -> "Console | None":
    if not RICH_AVAILABLE or not _is_tty():
        return None
    return Console(stderr=True)


@contextmanager
def chat_spinner(profile: str, model: str) -> Iterator[None]:
    """Context manager: show spinner while waiting for model response."""
    console = get_console()
    if console is None:
        yield
        return
    with console.status(
        f"[bold cyan]{profile}[/] · [dim]{model}[/] · thinking…",
        spinner="dots",
    ):
        yield


def stream_output(text: str, *, profile: str = "") -> None:
    """Print model output with optional profile prefix. Degrades to plain print."""
    console = get_console()
    if console is None:
        print(text, end="", flush=True)
        return
    console.print(text, end="", highlight=False, markup=False)


def print_status_bar(profile: str, model: str, in_tokens: int, out_tokens: int, elapsed: float) -> None:
    """Print a one-line status summary after a chat turn completes."""
    console = get_console()
    if console is None:
        return
    elapsed_str = f"{elapsed:.1f}s"
    console.print(
        f"[dim]▸ {profile} · {model} · {in_tokens:,}↑ {out_tokens:,}↓ · {elapsed_str}[/dim]",
        highlight=False,
    )


def make_benchmark_progress() -> "Progress | None":
    """Create a Rich Progress bar for benchmark runs."""
    if not RICH_AVAILABLE or not _is_tty():
        return None
    return Progress(
        SpinnerColumn(),
        TextColumn("[bold]{task.description}"),
        BarColumn(),
        TextColumn("[progress.percentage]{task.percentage:>3.0f}%"),
        TextColumn("({task.completed}/{task.total})"),
        TimeElapsedColumn(),
    )


def make_submit_progress() -> "Progress | None":
    """Create a Rich Progress bar for submit steps."""
    if not RICH_AVAILABLE or not _is_tty():
        return None
    return Progress(
        SpinnerColumn(),
        TextColumn("[bold cyan]{task.description}"),
        TimeElapsedColumn(),
    )


def print_error(msg: str) -> None:
    console = get_console()
    if console is None:
        print(f"error: {msg}", file=sys.stderr)
        return
    console.print(f"[bold red]error:[/] {msg}", highlight=False)


def print_success(msg: str) -> None:
    console = get_console()
    if console is None:
        print(msg)
        return
    console.print(f"[bold green]✓[/] {msg}", highlight=False)
```

### 6.2 `cmd_chat` changes

Current implementation:
```python
def cmd_chat(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "chat")
```

New implementation:
```python
def cmd_chat(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    profile = getattr(args, "profile", cfg["defaults"]["master_profile"])
    model = _resolve_display_model(cfg, profile)
    
    from tag.tui_output import chat_spinner, stream_output, print_status_bar
    import time
    
    start = time.monotonic()
    with chat_spinner(profile, model):
        result = cmd_hermes_command(args, "chat")
    elapsed = time.monotonic() - start
    # Token counts would come from Hermes output parsing; skip if unavailable
    print_status_bar(profile, model, in_tokens=0, out_tokens=0, elapsed=elapsed)
    return result
```

For streaming: intercept Hermes' stdout line-by-line and feed through `stream_output()`. This requires changing `cmd_hermes_command` to use `subprocess.Popen` with `stdout=PIPE` when `--stream` is set (or by default for chat).

### 6.3 `cmd_benchmark` changes

Wrap the existing `ThreadPoolExecutor` loop with a Rich `Progress` context:

```python
progress = make_benchmark_progress()
if progress:
    task_id = progress.add_task("benchmark", total=len(cases))
    ctx = progress
else:
    ctx = contextlib.nullcontext()

with ctx:
    for future in as_completed(futures):
        result = future.result()
        if progress:
            progress.advance(task_id)
            progress.update(task_id, description=f"case {result['case_id']}: {'✓' if result['passed'] else '✗'}")
```

### 6.4 `cmd_submit` changes

Show a spinner per step using `make_submit_progress()`. Each worker step (orchestrator → researcher → coder → reviewer) gets its own progress row.

### 6.5 TTY detection and fallback

All Rich calls go through `get_console()` which returns `None` when not a TTY. Every caller falls back to plain `print()`. This ensures piped output is clean.

Add `--no-color` / `TAG_NO_COLOR=1` env var to force plain output even on TTY (standard convention).

---

## 7. Implementation Plan

| Step | Task |
|------|------|
| 1 | Create `src/tag/tui_output.py` with all helper functions |
| 2 | Import `tui_output` in `controller.py` with try/except for RICH_AVAILABLE fallback |
| 3 | Update `cmd_chat` to use `chat_spinner` and `print_status_bar` |
| 4 | Update `cmd_benchmark` to use `make_benchmark_progress` |
| 5 | Update `cmd_submit` to use `make_submit_progress` |
| 6 | Add `--no-color` flag to parser + `TAG_NO_COLOR` env var check |
| 7 | Add tests: `test_chat_spinner_degrades_on_non_tty`, `test_benchmark_progress_counts_correctly` |
| 8 | Manual test: verify output on TTY vs pipe |

---

## 8. Success Metrics

- `tag chat --profile researcher "hello"` shows a spinner while waiting (manual verification).
- `tag chat ... | cat` produces clean text with no ANSI escape codes.
- `tag benchmark` shows a progress bar advancing per case.
- Zero `ImportError` from `rich` import — it's already in Hermes' venv which is on `PATH`.

---

## 9. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Rich not importable from TAG's process (Hermes venv vs TAG's venv differ) | Bundle `rich` as TAG's own dependency in `pyproject.toml`; it's small (~1MB) |
| Spinner output corrupts piped output | TTY check in `get_console()` catches this |
| Windows terminal compatibility | Rich handles Windows console natively; no extra work needed |
| Streaming requires Popen changes to `cmd_hermes_command` | Add `--stream` flag first; non-streaming chat still shows spinner |
