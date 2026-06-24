"""Path resolution utilities for TAG CLI."""
from __future__ import annotations

import os
import shutil
import sys
from pathlib import Path
from typing import Any, TextIO

APP_NAME = "TAG"
CLI_LABEL = "tag"
DEFAULT_TAG_HOME = Path("~/.tag").expanduser()
DEFAULT_HERMES_CHECKOUT = "managed/hermes-agent-upstream"
MIN_PYTHON = (3, 11)
MAX_PYTHON_EXCLUSIVE = (3, 14)


def package_root() -> Path:
    return Path(__file__).resolve().parent.parent  # core/ → src/tag/


def resource_path(*parts: str) -> Path:
    return package_root().joinpath(*parts)


def bundled_hermes_archive() -> Path:
    return resource_path("vendor", "hermes-agent-upstream.tar.gz")


def python_runtime_supported(version_info: tuple[int, int]) -> bool:
    return MIN_PYTHON <= version_info < MAX_PYTHON_EXCLUSIVE


def hermes_checkout_kind(root: Path) -> str:
    if not root.exists():
        return "missing"
    if (root / ".git").exists():
        return "git"
    return "bundled"


def is_hermes_checkout(root: Path) -> bool:
    return root.exists() and (root / "pyproject.toml").exists() and (root / "ui-tui" / "package.json").exists()


def discover_local_hermes_checkout() -> Path | None:
    candidates: list[Path] = []
    cwd = Path.cwd().resolve()
    candidates.extend([cwd / "hermes-agent-upstream", cwd.parent / "hermes-agent-upstream"])
    package_candidates = [
        package_root().parents[2] / "hermes-agent-upstream",
        package_root().parents[3] / "hermes-agent-upstream" if len(package_root().parents) > 3 else None,
    ]
    candidates.extend(candidate for candidate in package_candidates if candidate is not None)
    hermes_exec = shutil.which("hermes")
    if hermes_exec:
        exec_path = Path(hermes_exec).resolve()
        if len(exec_path.parents) >= 3:
            candidates.append(exec_path.parents[2])
    seen: set[Path] = set()
    for candidate in candidates:
        resolved = candidate.resolve()
        if resolved in seen:
            continue
        seen.add(resolved)
        if is_hermes_checkout(resolved):
            return resolved
    return None


def tag_home() -> Path:
    return Path(os.environ.get("TAG_HOME", str(DEFAULT_TAG_HOME))).expanduser().resolve()


def tag_cli_label() -> str:
    return os.environ.get("TAG_CLI_LABEL", CLI_LABEL).strip() or CLI_LABEL


def tag_cli_bin() -> str:
    override = os.environ.get("TAG_BIN", "").strip()
    if override:
        return override
    argv0 = Path(sys.argv[0]).expanduser()
    if argv0.exists():
        return str(argv0.resolve())
    found = shutil.which(tag_cli_label())
    if found:
        return found
    return tag_cli_label()


def resolve_home_relative(value: str, *, base: Path | None = None) -> Path:
    raw = Path(value).expanduser()
    if raw.is_absolute():
        return raw.resolve()
    return ((base or tag_home()) / raw).resolve()


def ensure_default_file(target: Path, source: Path) -> Path:
    if target.exists():
        return target
    try:
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source, target)
    except (PermissionError, NotADirectoryError) as exc:
        raise SystemExit(f"Cannot initialize TAG file {target}: {exc.strerror or exc}") from exc
    return target


def is_tty(stream: TextIO | None) -> bool:
    try:
        return bool(stream and stream.isatty())
    except Exception:
        return False


def can_launch_interactive_tui(
    stdin: TextIO | None = None,
    stdout: TextIO | None = None,
    stderr: TextIO | None = None,
) -> bool:
    return is_tty(stdin or sys.stdin) and (is_tty(stdout or sys.stdout) or is_tty(stderr or sys.stderr))


def config_root() -> Path:
    return tag_home() / "config"


def managed_root() -> Path:
    return tag_home() / "managed"


def hermes_root(cfg: dict[str, Any] | None = None) -> Path:
    override = os.environ.get("TAG_HERMES_ROOT", "").strip()
    if override:
        return Path(override).expanduser().resolve()
    configured = resolve_home_relative(
        str(cfg.get("upstream", {}).get("checkout_dir", DEFAULT_HERMES_CHECKOUT))
        if cfg is not None
        else DEFAULT_HERMES_CHECKOUT
    )
    if configured.exists():
        return configured
    discovered = discover_local_hermes_checkout()
    if discovered is not None:
        return discovered
    return configured


def hermes_bin(cfg: dict[str, Any] | None = None) -> Path:
    return hermes_root(cfg) / ".venv" / "bin" / "hermes"


def runtime_home(cfg: dict[str, Any]) -> Path:
    value = cfg.get("runtime", {}).get("home_dir", "runtime/home")
    return resolve_home_relative(str(value))


def runtime_codex_home(cfg: dict[str, Any]) -> Path:
    override = os.environ.get("TAG_CODEX_HOME")
    if override:
        return Path(override).expanduser().resolve()
    value = cfg.get("runtime", {}).get("codex_home", "runtime/home/.codex")
    return resolve_home_relative(str(value))


def runtime_db_path(cfg: dict[str, Any]) -> Path:
    value = cfg.get("runtime", {}).get("db_path", "runtime/tag.sqlite3")
    return resolve_home_relative(str(value))


def hermes_repo_url(cfg: dict[str, Any]) -> str:
    return str(
        os.environ.get(
            "TAG_HERMES_REPO",
            cfg.get("upstream", {}).get("repo", "https://github.com/NousResearch/Hermes-Agent.git"),
        )
    )


def hermes_ref(cfg: dict[str, Any]) -> str:
    return str(os.environ.get("TAG_HERMES_REF", cfg.get("upstream", {}).get("ref", "main")))


def hermes_env(cfg: dict[str, Any]) -> dict[str, str]:
    home_dir = runtime_home(cfg)
    hhome = Path(os.environ.get("TAG_HERMES_HOME", home_dir / ".hermes"))
    codex_home = runtime_codex_home(cfg)
    tui_dir = hermes_root(cfg) / "ui-tui"
    env = os.environ.copy()
    env["HOME"] = str(home_dir)
    env["HERMES_HOME"] = str(hhome)
    env["CODEX_HOME"] = str(codex_home)
    env["HERMES_BIN"] = tag_cli_bin()
    env["HERMES_BIN_LABEL"] = tag_cli_label()
    env["HERMES_ENV_LABEL"] = "the active TAG profile env file"
    env["HERMES_TUI_DIR"] = str(tui_dir)
    env["PATH"] = f"{hermes_root(cfg) / '.venv' / 'bin'}:{env.get('PATH', '')}"
    return env


def profile_home(cfg: dict[str, Any], profile_name: str) -> Path:
    return runtime_home(cfg) / ".hermes" / "profiles" / profile_name


def profile_exec_env(cfg: dict[str, Any], profile_name: str) -> dict[str, str]:
    import sqlite3 as _sq3
    env = hermes_env(cfg)
    real_home = os.environ.get("HOME", "")
    passthrough_profiles = {
        item.strip()
        for item in os.environ.get(
            "TAG_PASSTHROUGH_HOME_PROFILES", "codex-runtime-master"
        ).split(",")
        if item.strip()
    }
    if profile_name in passthrough_profiles:
        env["HOME"] = os.environ.get(
            "TAG_REAL_HOME", real_home or str(runtime_home(cfg))
        )
        env["CODEX_HOME"] = os.environ.get(
            "TAG_CODEX_HOME", str(Path(real_home).expanduser() / ".codex") if real_home else str(runtime_codex_home(cfg))
        )
    env["HERMES_HOME"] = str(profile_home(cfg, profile_name))
    # Load profile .env file
    ph = profile_home(cfg, profile_name)
    _env_file = ph / ".env"
    if _env_file.exists():
        from tag.core.utils import read_dotenv
        for key, value in read_dotenv(_env_file).items():
            env[key] = value
    # PRD-002: inject memory journal as system prompt prefix when DB exists
    db_path = runtime_db_path(cfg)
    if db_path.exists():
        try:
            _db = _sq3.connect(str(db_path), timeout=2)
            _db.row_factory = _sq3.Row
            from tag.core.db import journal_to_prompt_prefix
            prefix = journal_to_prompt_prefix(_db, profile_name)
            _db.close()
            if prefix:
                env["HERMES_SYSTEM_INJECT"] = prefix
        except Exception:
            pass
    return env


def ensure_runtime_dirs(cfg: dict[str, Any]) -> None:
    env = hermes_env(cfg)
    Path(env["HOME"]).mkdir(parents=True, exist_ok=True)
    Path(env["HERMES_HOME"]).mkdir(parents=True, exist_ok=True)
    Path(env["CODEX_HOME"]).mkdir(parents=True, exist_ok=True)
    runtime_db_path(cfg).parent.mkdir(parents=True, exist_ok=True)
