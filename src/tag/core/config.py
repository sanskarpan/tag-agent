"""Configuration loading and saving utilities."""
from __future__ import annotations

from pathlib import Path
from typing import Any

import yaml

from tag.core.paths import config_root, package_root, ensure_default_file


def config_path(arg_value: str | None) -> Path:
    if arg_value:
        return Path(arg_value).expanduser().resolve()
    return ensure_default_file(config_root() / "tag.yaml", package_root() / "config" / "default.yaml")


def benchmark_suite_path(arg_value: str | None) -> Path:
    if arg_value:
        return Path(arg_value).expanduser().resolve()
    return ensure_default_file(
        config_root() / "benchmark-suite.yaml",
        package_root() / "config" / "benchmark-suite.yaml",
    )


def load_config(path: Path) -> dict[str, Any]:
    try:
        with path.open("r", encoding="utf-8-sig") as fh:
            data = yaml.safe_load(fh) or {}
    except FileNotFoundError:
        raise SystemExit(f"Config file not found: {path}")
    if not isinstance(data, dict):
        raise SystemExit(f"Config at {path} must be a YAML object.")
    return data


def save_config(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as fh:
        yaml.safe_dump(payload, fh, sort_keys=False)
