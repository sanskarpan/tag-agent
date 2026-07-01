"""Configuration loading and saving utilities."""
from __future__ import annotations

import os
import tempfile
from pathlib import Path
from typing import Any

import yaml

try:
    import fcntl
except ImportError:  # pragma: no cover — non-POSIX (Windows)
    fcntl = None  # type: ignore[assignment]

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
    except yaml.YAMLError as exc:
        raise SystemExit(f"Config at {path} is not valid YAML: {exc}")
    if not isinstance(data, dict):
        raise SystemExit(f"Config at {path} must be a YAML object.")
    return data


def save_config(path: Path, payload: dict[str, Any]) -> None:
    """Persist config atomically and serialized across concurrent writers.

    Two `tag set-model` invocations racing on the same file used to interleave
    into torn YAML that bricked every later config read. We now take an
    advisory lock for the duration and swap the file in with an atomic
    `os.replace`, so a reader always sees either the old or the new file whole.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    lock_path = path.with_name(path.name + ".lock")
    lock_fh = open(lock_path, "w")
    try:
        if fcntl is not None:
            fcntl.flock(lock_fh.fileno(), fcntl.LOCK_EX)
        fd, tmp = tempfile.mkstemp(dir=str(path.parent), prefix=path.name + ".", suffix=".tmp")
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as fh:
                yaml.safe_dump(payload, fh, sort_keys=False)
                fh.flush()
                os.fsync(fh.fileno())
            os.replace(tmp, path)
        except BaseException:
            try:
                os.unlink(tmp)
            except OSError:
                pass
            raise
    finally:
        if fcntl is not None:
            try:
                fcntl.flock(lock_fh.fileno(), fcntl.LOCK_UN)
            except OSError:
                pass
        lock_fh.close()
