"""Process execution utilities for TAG CLI."""
from __future__ import annotations

import subprocess
import sys
from pathlib import Path
from typing import Any

from tag.core.paths import hermes_bin, hermes_env, hermes_root, profile_exec_env, ensure_runtime_dirs


def run_hermes(cfg: dict[str, Any], *args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
    ensure_runtime_dirs(cfg)
    return subprocess.run(
        [str(hermes_bin(cfg)), *args],
        env=hermes_env(cfg),
        text=True,
        capture_output=True,
        check=check,
    )


def run_profile_hermes(
    cfg: dict[str, Any],
    profile_name: str,
    *args: str,
    check: bool = True,
    timeout: int | None = None,
) -> subprocess.CompletedProcess[str]:
    ensure_runtime_dirs(cfg)
    return subprocess.run(
        [str(hermes_bin(cfg)), *args],
        env=profile_exec_env(cfg, profile_name),
        text=True,
        capture_output=True,
        check=check,
        timeout=timeout,
    )


def run_profile_python(
    cfg: dict[str, Any],
    profile_name: str,
    inline: str,
    *,
    check: bool = True,
) -> subprocess.CompletedProcess[str]:
    ensure_runtime_dirs(cfg)
    return subprocess.run(
        [str(hermes_root(cfg) / ".venv" / "bin" / "python"), "-c", inline],
        env=profile_exec_env(cfg, profile_name),
        text=True,
        capture_output=True,
        check=check,
    )
