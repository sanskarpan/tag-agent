"""PRD-018: Context Window Management for TAG CLI."""

from __future__ import annotations

import json
import subprocess
from pathlib import Path
from typing import Any

DEFAULT_MAX_TOKENS: int = 128_000


def get_context_size(hermes_bin_path: Path, profile_home: Path) -> dict[str, Any]:
    """Return current context-window usage by running ``hermes prompt-size --json``.

    Returns a dict with keys:
      - ``used_tokens`` (int)
      - ``max_tokens`` (int)
      - ``pct`` (float, 0–100)

    On any failure the dict contains zeros for all three fields.
    """
    try:
        result = subprocess.run(
            [str(hermes_bin_path), "prompt-size", "--json"],
            capture_output=True,
            text=True,
            timeout=30,
            cwd=str(profile_home),
        )
        if result.returncode != 0:
            return {"used_tokens": 0, "max_tokens": 0, "pct": 0.0}

        data: dict[str, Any] = json.loads(result.stdout)
        used = int(data.get("used_tokens", data.get("used", 0)))
        max_t = int(data.get("max_tokens", data.get("max", DEFAULT_MAX_TOKENS)))
        pct = (used / max_t * 100.0) if max_t > 0 else 0.0
        return {"used_tokens": used, "max_tokens": max_t, "pct": round(pct, 2)}
    except Exception:
        return {"used_tokens": 0, "max_tokens": 0, "pct": 0.0}


def format_context_bar(used: int, max_t: int) -> str:
    """Return a Rich-compatible string showing context usage with threshold-based colour.

    Colour rules:
      - green  when < 50 %
      - yellow when 50 % – 80 %
      - red    when > 80 %
    """
    pct = (used / max_t * 100.0) if max_t > 0 else 0.0
    pct_int = round(pct)

    if pct < 50:
        colour = "green"
    elif pct <= 80:
        colour = "yellow"
    else:
        colour = "red"

    label = f"{used:,} / {max_t:,} ({pct_int}%)"
    return f"[{colour}]{label}[/{colour}]"


def summarize_context(
    hermes_bin_path: Path,
    profile_home: Path,
    keep_last: int = 10,
) -> bool:
    """Trim the session history via ``hermes sessions trim --keep-last N``.

    Returns True on success, False on any error.
    """
    try:
        result = subprocess.run(
            [str(hermes_bin_path), "sessions", "trim", "--keep-last", str(keep_last)],
            capture_output=True,
            text=True,
            timeout=30,
            cwd=str(profile_home),
        )
        return result.returncode == 0
    except Exception:
        return False


def reset_context(hermes_bin_path: Path, profile_home: Path) -> bool:
    """Clear all session history via ``hermes sessions clear``.

    Returns True on success, False on any error.
    """
    try:
        result = subprocess.run(
            [str(hermes_bin_path), "sessions", "clear"],
            capture_output=True,
            text=True,
            timeout=30,
            cwd=str(profile_home),
        )
        return result.returncode == 0
    except Exception:
        return False


def export_context(hermes_bin_path: Path, profile_home: Path) -> str:
    """Export session history as Markdown via ``hermes sessions export --format markdown``.

    Returns the markdown string, or an empty string on failure.
    """
    try:
        result = subprocess.run(
            [str(hermes_bin_path), "sessions", "export", "--format", "markdown"],
            capture_output=True,
            text=True,
            timeout=30,
            cwd=str(profile_home),
        )
        if result.returncode == 0:
            return result.stdout
        return ""
    except Exception:
        return ""
