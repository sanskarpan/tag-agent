#!/usr/bin/env python3
"""TAG control-plane CLI built on top of Hermes."""

from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import re
import shutil
import sqlite3
import subprocess
import sys
import tarfile
import textwrap
import time
import uuid
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path
from pathlib import PurePosixPath
from typing import Any, TextIO

import yaml

try:
    from tag import __version__
except Exception:  # pragma: no cover - fallback for direct file loading in tests
    __version__ = "0.1.0"

APP_NAME = "TAG"
CLI_LABEL = "tag"
DEFAULT_TAG_HOME = Path("~/.tag").expanduser()
DEFAULT_HERMES_CHECKOUT = "managed/hermes-agent-upstream"
MIN_PYTHON = (3, 11)
MAX_PYTHON_EXCLUSIVE = (3, 14)


def package_root() -> Path:
    return Path(__file__).resolve().parent


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
    except PermissionError as exc:
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


def config_path(arg_value: str | None) -> Path:
    if arg_value:
        return Path(arg_value).expanduser().resolve()
    return ensure_default_file(config_root() / "tag.yaml", resource_path("config", "default.yaml"))


def benchmark_suite_path(arg_value: str | None) -> Path:
    if arg_value:
        return Path(arg_value).expanduser().resolve()
    return ensure_default_file(
        config_root() / "benchmark-suite.yaml",
        resource_path("config", "benchmark-suite.yaml"),
    )


def load_config(path: Path) -> dict[str, Any]:
    with path.open("r", encoding="utf-8-sig") as fh:
        data = yaml.safe_load(fh) or {}
    if not isinstance(data, dict):
        raise SystemExit(f"Config at {path} must be a YAML object.")
    return data


def save_config(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as fh:
        yaml.safe_dump(payload, fh, sort_keys=False)


def utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat()


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


def profile_exec_env(cfg: dict[str, Any], profile_name: str) -> dict[str, str]:
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
    for key, value in read_dotenv(profile_home(cfg, profile_name) / ".env").items():
        env[key] = value
    return env


def ensure_runtime_dirs(cfg: dict[str, Any]) -> None:
    env = hermes_env(cfg)
    Path(env["HOME"]).mkdir(parents=True, exist_ok=True)
    Path(env["HERMES_HOME"]).mkdir(parents=True, exist_ok=True)
    Path(env["CODEX_HOME"]).mkdir(parents=True, exist_ok=True)
    runtime_db_path(cfg).parent.mkdir(parents=True, exist_ok=True)


def open_db(cfg: dict[str, Any]) -> sqlite3.Connection:
    ensure_runtime_dirs(cfg)
    conn = sqlite3.connect(runtime_db_path(cfg), timeout=5)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA busy_timeout = 5000")
    conn.execute("PRAGMA foreign_keys = ON")
    last_error: sqlite3.OperationalError | None = None
    for _ in range(20):
        try:
            conn.execute("PRAGMA journal_mode = WAL")
            conn.execute("PRAGMA synchronous = NORMAL")
            last_error = None
            break
        except sqlite3.OperationalError as exc:
            if "locked" not in str(exc).lower():
                raise
            last_error = exc
            time.sleep(0.1)
    if last_error is not None:
        raise last_error
    conn.executescript(
        """
        CREATE TABLE IF NOT EXISTS runs (
          id TEXT PRIMARY KEY,
          created_at TEXT NOT NULL,
          kind TEXT NOT NULL,
          task_type TEXT NOT NULL,
          execution TEXT NOT NULL,
          master_profile TEXT NOT NULL,
          board TEXT NOT NULL,
          prompt TEXT NOT NULL,
          route_json TEXT NOT NULL,
          status TEXT NOT NULL,
          metadata_json TEXT NOT NULL
        );

        CREATE TABLE IF NOT EXISTS steps (
          id INTEGER PRIMARY KEY AUTOINCREMENT,
          run_id TEXT NOT NULL,
          role TEXT NOT NULL,
          profile TEXT NOT NULL,
          model_ref TEXT NOT NULL,
          prompt TEXT NOT NULL,
          output TEXT NOT NULL,
          status TEXT NOT NULL,
          started_at TEXT NOT NULL,
          finished_at TEXT NOT NULL,
          duration_ms INTEGER NOT NULL,
          extra_json TEXT NOT NULL,
          FOREIGN KEY(run_id) REFERENCES runs(id)
        );
        """
    )
    return conn


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
) -> subprocess.CompletedProcess[str]:
    ensure_runtime_dirs(cfg)
    return subprocess.run(
        [str(hermes_bin(cfg)), *args],
        env=profile_exec_env(cfg, profile_name),
        text=True,
        capture_output=True,
        check=check,
    )


def profile_home(cfg: dict[str, Any], profile_name: str) -> Path:
    return Path(hermes_env(cfg)["HERMES_HOME"]) / "profiles" / profile_name


def read_dotenv(path: Path) -> dict[str, str]:
    if not path.exists():
        return {}
    values: dict[str, str] = {}
    for raw_line in path.read_text(encoding="utf-8").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        values[key.strip()] = value.strip()
    return values


def _upsert_env_line(env_file: Path, key: str, value: str) -> None:
    """Write or replace KEY=VALUE in an .env file without disturbing other lines."""
    env_file.parent.mkdir(parents=True, exist_ok=True)
    lines = env_file.read_text(encoding="utf-8").splitlines() if env_file.exists() else []
    prefix = f"{key}="
    new_line = f"{key}={value}"
    replaced = False
    out = []
    for line in lines:
        stripped = line.strip()
        if stripped.startswith(prefix) or stripped.lstrip("# ").startswith(prefix):
            out.append(new_line)
            replaced = True
        else:
            out.append(line)
    if not replaced:
        out.append(new_line)
    env_file.write_text("\n".join(out) + "\n", encoding="utf-8")


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


def write_yaml(path: Path, payload: dict[str, Any], force: bool) -> None:
    if path.exists() and not force:
        return
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as fh:
        yaml.safe_dump(payload, fh, sort_keys=False)


def write_text(path: Path, content: str, force: bool) -> None:
    if path.exists() and not force:
        return
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")


def slugify(text: str) -> str:
    slug = re.sub(r"[^a-z0-9]+", "-", text.lower()).strip("-")
    return slug[:48] or "run"


def normalize_chat_output(output: str) -> str:
    cleaned: list[str] = []
    for line in output.splitlines():
        if line.strip().startswith("session_id:"):
            continue
        if "tirith security scanner enabled but not available" in line.lower():
            continue
        cleaned.append(line)
    return "\n".join(cleaned).strip()


def rewrite_cli_hints(text: str) -> str:
    if not text:
        return text
    label = tag_cli_label()

    def replace_inner(inner: str) -> str:
        return re.sub(r"\bhermes\b", label, inner, flags=re.IGNORECASE)

    rewritten = re.sub(
        r"`([^`\n]*\bhermes\b[^`\n]*)`",
        lambda match: f"`{replace_inner(match.group(1))}`",
        text,
        flags=re.IGNORECASE,
    )
    rewritten = re.sub(
        r"'([^'\n]*\bhermes\b[^'\n]*)'",
        lambda match: f"'{replace_inner(match.group(1))}'",
        rewritten,
        flags=re.IGNORECASE,
    )
    rewritten = re.sub(
        r"\bhermes (?=(auth|config|model|setup|update|gateway|sessions|doctor|tools|portal|status|plugins|skills|mcp|logs|memory|completion|prompt-size|chat|--resume|-c)\b)",
        f"{label} ",
        rewritten,
        flags=re.IGNORECASE,
    )
    rewritten = re.sub(r"\bHermes/tag\b", "TAG", rewritten, flags=re.IGNORECASE)
    rewritten = re.sub(r"\btag/tag\b", "tag", rewritten, flags=re.IGNORECASE)
    rewritten = re.sub(r"\bHermes Agent\b", "TAG", rewritten, flags=re.IGNORECASE)
    rewritten = re.sub(r"\bhermes-agent\b", "tag-agent", rewritten, flags=re.IGNORECASE)
    rewritten = re.sub(r"\bthis Hermes profile\b", "this TAG profile", rewritten, flags=re.IGNORECASE)
    rewritten = re.sub(r"\bActive Hermes profile\b", "Active TAG profile", rewritten, flags=re.IGNORECASE)
    rewritten = re.sub(r"\bHermes profile\b", "TAG profile", rewritten, flags=re.IGNORECASE)
    rewritten = rewritten.replace("~/.hermes/.env", "the active TAG profile env file")
    return rewritten


def infrastructure_failure_reason(output: str) -> str | None:
    normalized = normalize_chat_output(output)
    lowered = normalized.lower()
    known_failures = (
        "error: codex authentication failed",
        "login looks expired or invalid",
        "api call failed after",
        "no api keys or providers found",
        "it looks like the managed runtime isn't configured yet",
        "it looks like hermes isn't configured yet",
    )
    for marker in known_failures:
        if marker in lowered:
            return marker
    return None


def strip_json_fences(text: str) -> str:
    stripped = text.strip()
    if stripped.startswith("```") and stripped.endswith("```"):
        lines = stripped.splitlines()
        if len(lines) >= 3:
            return "\n".join(lines[1:-1]).strip()
    return stripped


def merged_env_example(cfg: dict[str, Any], profile_name: str) -> str:
    env_examples = cfg.get("env_examples", {})
    if env_examples is None:
        env_examples = {}
    if not isinstance(env_examples, dict):
        raise SystemExit("Config field 'env_examples' must be a YAML object.")
    shared = env_examples.get("shared", {})
    profiles = env_examples.get("profiles", {})
    if shared is None:
        shared = {}
    if profiles is None:
        profiles = {}
    if not isinstance(shared, dict):
        raise SystemExit("Config field 'env_examples.shared' must be a YAML object.")
    if not isinstance(profiles, dict):
        raise SystemExit("Config field 'env_examples.profiles' must be a YAML object.")
    per_profile = profiles.get(profile_name, {})
    if per_profile is None:
        per_profile = {}
    if not isinstance(per_profile, dict):
        raise SystemExit(
            f"Config field 'env_examples.profiles.{profile_name}' must be a YAML object."
        )
    lines: list[str] = []
    for key, value in {**shared, **per_profile}.items():
        lines.append(f"{key}={value}")
    if not lines:
        lines.append("# Add provider credentials here.")
    lines.append("")
    return "\n".join(lines)


def configured_skins(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    skins = cfg.get("skins", {})
    if not isinstance(skins, dict):
        return []
    resolved: list[dict[str, Any]] = []
    for name, data in skins.items():
        source: str | None
        if isinstance(data, dict):
            source = str(data.get("source", "") or "").strip()
        else:
            source = str(data or "").strip()
        if not source:
            continue
        source_path = Path(source)
        if not source_path.is_absolute():
            candidate = resource_path(source)
            source_path = candidate.resolve() if candidate.exists() else resolve_home_relative(source)
        resolved.append({"name": str(name).strip(), "source": source_path})
    return resolved


def nonnegative_int(value: str) -> int:
    parsed = int(value)
    if parsed < 0:
        raise argparse.ArgumentTypeError("value must be >= 0")
    return parsed


def positive_int(value: str) -> int:
    parsed = int(value)
    if parsed <= 0:
        raise argparse.ArgumentTypeError("value must be > 0")
    return parsed


def install_profile_skins(cfg: dict[str, Any], profile_name: str, force: bool) -> list[str]:
    installed: list[str] = []
    skins = configured_skins(cfg)
    if not skins:
        return installed
    skin_dir = profile_home(cfg, profile_name) / "skins"
    skin_dir.mkdir(parents=True, exist_ok=True)
    for skin in skins:
        name = str(skin["name"])
        source = Path(skin["source"])
        if not source.exists():
            raise SystemExit(f"Skin asset not found: {source}")
        destination = skin_dir / f"{name}.yaml"
        if destination.exists() and not force:
            installed.append(str(destination))
            continue
        shutil.copy2(source, destination)
        installed.append(str(destination))
    return installed


def render_profiles(cfg: dict[str, Any], force: bool) -> list[dict[str, str]]:
    results: list[dict[str, str]] = []
    profiles = cfg.get("profiles", {})
    for name, profile in profiles.items():
        home = profile_home(cfg, name)
        config_file = home / "config.yaml"
        env_example = home / ".env.example"
        profile_cfg = profile.get("config", {})
        write_yaml(config_file, profile_cfg, force=force)
        write_text(env_example, merged_env_example(cfg, name), force=force)
        installed_skins = install_profile_skins(cfg, name, force=force)
        results.append(
            {
                "profile": name,
                "config": str(config_file),
                "env_example": str(env_example),
                "skins": ", ".join(installed_skins),
            }
        )
    return results


def bootstrap_profiles(cfg: dict[str, Any]) -> list[dict[str, str]]:
    created: list[dict[str, str]] = []
    profiles = cfg.get("profiles", {})
    for name, profile in profiles.items():
        home = profile_home(cfg, name)
        if home.exists():
            created.append({"profile": name, "status": "existing"})
            continue
        cmd = ["profile", "create", name, "--no-alias"]
        description = str(profile.get("description", "")).strip()
        if description:
            cmd.extend(["--description", description])
        try:
            run_hermes(cfg, *cmd)
        except subprocess.CalledProcessError as exc:
            message = exc.stderr.strip() or exc.stdout.strip() or str(exc)
            raise SystemExit(f"Failed to create TAG-managed profile '{name}': {message}") from exc
        created.append({"profile": name, "status": "created"})
    return created


def resolve_route(cfg: dict[str, Any], task_type: str, master_override: str | None, worker_override: list[str]) -> dict[str, Any]:
    defaults = cfg.get("defaults", {})
    routing = cfg.get("routing", {}).get("task_types", {})
    route = dict(routing.get(task_type, {}))
    if not route:
        available = ", ".join(sorted(routing))
        raise SystemExit(f"Unknown task type '{task_type}'. Available: {available}")

    master = master_override or defaults.get("master_profile")
    workers = worker_override or route.get("workers", [])
    verifier = route.get("verifier")

    profiles = cfg.get("profiles", {})
    snapshot = {
        "master_profile": master,
        "board": defaults.get("board", "default"),
        "execution": route.get("execution", "kanban"),
        "workers": [],
        "verifier": None,
    }

    if master not in profiles:
        raise SystemExit(f"Master profile '{master}' is not defined in config.")

    for worker in workers:
        if worker not in profiles:
            raise SystemExit(f"Worker profile '{worker}' is not defined in config.")
        pdata = profiles[worker]
        snapshot["workers"].append(
            {
                "name": worker,
                "description": pdata.get("description", ""),
                "tags": pdata.get("tags", []),
                "model": pdata.get("config", {}).get("model", {}),
            }
        )

    if verifier:
        if verifier not in profiles:
            raise SystemExit(f"Verifier profile '{verifier}' is not defined in config.")
        vdata = profiles[verifier]
        snapshot["verifier"] = {
            "name": verifier,
            "description": vdata.get("description", ""),
            "tags": vdata.get("tags", []),
            "model": vdata.get("config", {}).get("model", {}),
        }

    master_data = profiles[master]
    snapshot["master"] = {
        "name": master,
        "description": master_data.get("description", ""),
        "tags": master_data.get("tags", []),
        "model": master_data.get("config", {}).get("model", {}),
        "delegation": master_data.get("config", {}).get("delegation", {}),
    }
    return snapshot


def parse_model_ref(value: str) -> tuple[str, str]:
    if any(ord(c) < 32 or ord(c) == 127 for c in value):
        raise SystemExit(
            f"Invalid model reference '{value}'. Provider and model must not contain control characters."
        )
    ref = value.strip()
    if "/" not in ref:
        raise SystemExit(
            f"Invalid model reference '{value}'. Use provider/model-id format."
        )
    provider, model = ref.split("/", 1)
    provider = provider.strip()
    model = model.strip()
    if not provider or not model:
        raise SystemExit(
            f"Invalid model reference '{value}'. Use provider/model-id format."
        )
    return provider, model


def format_model_ref(model_cfg: dict[str, Any]) -> str:
    provider = str(model_cfg.get("provider", "") or "").strip()
    model = str(model_cfg.get("default", model_cfg.get("name", "")) or "").strip()
    if provider and model:
        return f"{provider}/{model}"
    if model:
        return model
    return "-"


def collect_assignments(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for name, profile in (cfg.get("profiles") or {}).items():
        profile_cfg = profile.get("config", {})
        primary = profile_cfg.get("model", {}) if isinstance(profile_cfg, dict) else {}
        delegation = (
            profile_cfg.get("delegation", {}) if isinstance(profile_cfg, dict) else {}
        )
        row = {
            "profile": name,
            "description": profile.get("description", ""),
            "primary_model": format_model_ref(primary if isinstance(primary, dict) else {}),
            "delegation_model": "-",
            "openai_runtime": "",
        }
        if isinstance(delegation, dict) and delegation.get("provider") and delegation.get("model"):
            row["delegation_model"] = f"{delegation['provider']}/{delegation['model']}"
        if isinstance(primary, dict) and primary.get("openai_runtime"):
            row["openai_runtime"] = str(primary["openai_runtime"])
        rows.append(row)
    return rows


def load_model_inventory(cfg: dict[str, Any], profile_name: str) -> dict[str, Any]:
    inline = textwrap.dedent(
        """
        import json
        from hermes_cli.inventory import build_models_payload, load_picker_context

        payload = build_models_payload(
            load_picker_context(),
            include_unconfigured=True,
            picker_hints=True,
            canonical_order=True,
            pricing=True,
            capabilities=True,
            max_models=50,
        )
        print(json.dumps(payload))
        """
    ).strip()
    proc = run_profile_python(cfg, profile_name, inline)
    stdout = proc.stdout.strip()
    if not stdout:
        raise SystemExit(f"Failed to load model inventory for profile '{profile_name}'.")
    return json.loads(stdout)


def load_openrouter_catalog(cfg: dict[str, Any], profile_name: str) -> list[dict[str, Any]]:
    env = profile_exec_env(cfg, profile_name)
    api_key = env.get("OPENROUTER_API_KEY", "").strip()
    if not api_key:
        raise SystemExit(
            f"OPENROUTER_API_KEY is not set for profile '{profile_name}'."
        )
    req = urllib.request.Request(
        "https://openrouter.ai/api/v1/models",
        headers={"Authorization": f"Bearer {api_key}"},
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            payload = json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        raise SystemExit(f"OpenRouter models request failed with HTTP {exc.code}.") from exc
    except urllib.error.URLError as exc:
        reason = exc.reason if exc.reason else "unknown network error"
        raise SystemExit(f"OpenRouter models request failed: {reason}") from exc
    except TimeoutError as exc:
        raise SystemExit("OpenRouter models request timed out.") from exc
    except json.JSONDecodeError as exc:
        raise SystemExit("OpenRouter models response was not valid JSON.") from exc
    rows = payload.get("data", [])
    if not isinstance(rows, list):
        raise SystemExit("Unexpected OpenRouter models payload.")
    return rows


def ensure_profile_exists(cfg: dict[str, Any], profile_name: str) -> None:
    profiles = cfg.get("profiles", {})
    if profile_name not in profiles:
        available = ", ".join(sorted(profiles))
        raise SystemExit(f"Unknown profile '{profile_name}'. Available: {available}")


def apply_route_model_overrides(
    route: dict[str, Any],
    *,
    master_model: str | None,
    verifier_model: str | None,
    worker_models: list[str],
) -> dict[str, Any]:
    if master_model:
        provider, model = parse_model_ref(master_model)
        route["master"]["model"] = {"provider": provider, "default": model}
    if verifier_model and route.get("verifier"):
        provider, model = parse_model_ref(verifier_model)
        route["verifier"]["model"] = {"provider": provider, "default": model}
    overrides: dict[str, tuple[str, str]] = {}
    for item in worker_models:
        if "=" not in item:
            raise SystemExit(
                f"Invalid worker override '{item}'. Use profile=provider/model-id."
            )
        worker_name, ref = item.split("=", 1)
        overrides[worker_name.strip()] = parse_model_ref(ref)
    for worker in route["workers"]:
        if worker["name"] not in overrides:
            continue
        provider, model = overrides[worker["name"]]
        worker["model"] = {"provider": provider, "default": model}
    return route


def run_chat_step(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    prompt: str,
) -> dict[str, Any]:
    started = dt.datetime.now(dt.timezone.utc)
    proc = run_profile_hermes(
        cfg,
        profile_name,
        "chat",
        "-q",
        prompt,
        "-Q",
        check=False,
    )
    finished = dt.datetime.now(dt.timezone.utc)
    output = proc.stdout.strip()
    if proc.stderr.strip():
        output = f"{output}\n{proc.stderr.strip()}".strip()
    profiles = cfg.get("profiles", {})
    model_cfg = profiles.get(profile_name, {}).get("config", {}).get("model", {})
    failure_reason = infrastructure_failure_reason(output)
    return {
        "profile": profile_name,
        "status": "ok" if proc.returncode == 0 and not failure_reason else "error",
        "prompt": prompt,
        "output": output,
        "started_at": started.isoformat(),
        "finished_at": finished.isoformat(),
        "duration_ms": int((finished - started).total_seconds() * 1000),
        "returncode": proc.returncode,
        "model_ref": format_model_ref(model_cfg if isinstance(model_cfg, dict) else {}),
        "failure_reason": failure_reason or "",
    }


def load_benchmark_suite(path: Path) -> list[dict[str, Any]]:
    payload = load_config(path)
    cases = payload.get("cases", [])
    if not isinstance(cases, list):
        raise SystemExit(f"Benchmark suite at {path} must contain a 'cases' list.")
    return cases


def case_passed(case: dict[str, Any], output: str) -> tuple[bool, str]:
    normalized = normalize_chat_output(output)
    expected_exact = case.get("expected_exact")
    if isinstance(expected_exact, str):
        ok = normalized == expected_exact
        return ok, f"expected exact tail '{expected_exact}'"
    expected_regex = case.get("expected_regex")
    if isinstance(expected_regex, str):
        ok = re.search(expected_regex, normalized, re.MULTILINE) is not None
        return ok, f"expected regex '{expected_regex}'"
    expected_json = case.get("expected_json")
    if isinstance(expected_json, dict):
        try:
            parsed = json.loads(strip_json_fences(normalized))
        except Exception:
            return False, "expected valid JSON in final line"
        for key, value in expected_json.items():
            if parsed.get(key) != value:
                return False, f"expected JSON field {key}={value!r}"
        return True, "matched expected JSON fields"
    return False, "benchmark case missing expectation"


def create_temp_profile(
    cfg: dict[str, Any],
    *,
    base_profile: str,
    model_ref: str,
) -> str:
    ensure_profile_exists(cfg, base_profile)
    provider, model = parse_model_ref(model_ref)
    profile_name = f"bench-{base_profile}-{slugify(provider)}-{slugify(model)}"
    home = profile_home(cfg, profile_name)
    if not home.exists():
        run_hermes(cfg, "profile", "create", profile_name, "--no-alias")
    base_cfg = cfg.get("profiles", {}).get(base_profile, {})
    profile_cfg = json.loads(json.dumps(base_cfg))
    profile_cfg.setdefault("config", {}).setdefault("model", {})
    profile_cfg["config"]["model"]["provider"] = provider
    profile_cfg["config"]["model"]["default"] = model
    write_yaml(home / "config.yaml", profile_cfg.get("config", {}), force=True)
    base_env = profile_home(cfg, base_profile) / ".env"
    if base_env.exists():
        write_text(home / ".env", base_env.read_text(encoding="utf-8"), force=True)
    for name in ("auth.json", "auth.lock", "models_dev_cache.json", "provider_models_cache.json"):
        src = profile_home(cfg, base_profile) / name
        dst = home / name
        if src.exists() and not dst.exists():
            shutil.copy2(src, dst)
    return profile_name


def show_kanban_task(cfg: dict[str, Any], *, profile_name: str, board: str, task_id: str) -> dict[str, Any]:
    proc = run_profile_hermes(
        cfg,
        profile_name,
        "kanban",
        "--board",
        board,
        "show",
        task_id,
        "--json",
        check=False,
    )
    if proc.returncode != 0 or not proc.stdout.strip():
        raise SystemExit(proc.stderr.strip() or proc.stdout.strip() or f"Failed to show task {task_id}.")
    return json.loads(proc.stdout)


def insert_run(
    conn: sqlite3.Connection,
    *,
    run_id: str,
    kind: str,
    task_type: str,
    execution: str,
    master_profile: str,
    board: str,
    prompt: str,
    route: dict[str, Any],
    status: str,
    metadata: dict[str, Any],
) -> None:
    conn.execute(
        """
        INSERT INTO runs (
          id, created_at, kind, task_type, execution, master_profile, board,
          prompt, route_json, status, metadata_json
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        """,
        (
            run_id,
            utc_now(),
            kind,
            task_type,
            execution,
            master_profile,
            board,
            prompt,
            json.dumps(route),
            status,
            json.dumps(metadata),
        ),
    )
    conn.commit()


def update_run_status(
    conn: sqlite3.Connection,
    *,
    run_id: str,
    status: str,
    metadata: dict[str, Any] | None = None,
) -> None:
    if metadata is None:
        conn.execute("UPDATE runs SET status = ? WHERE id = ?", (status, run_id))
    else:
        conn.execute(
            "UPDATE runs SET status = ?, metadata_json = ? WHERE id = ?",
            (status, json.dumps(metadata), run_id),
        )
    conn.commit()


def insert_step(
    conn: sqlite3.Connection,
    *,
    run_id: str,
    role: str,
    profile: str,
    model_ref: str,
    prompt: str,
    output: str,
    status: str,
    started_at: str,
    finished_at: str,
    duration_ms: int,
    extra: dict[str, Any],
) -> None:
    conn.execute(
        """
        INSERT INTO steps (
          run_id, role, profile, model_ref, prompt, output, status,
          started_at, finished_at, duration_ms, extra_json
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        """,
        (
            run_id,
            role,
            profile,
            model_ref,
            prompt,
            output,
            status,
            started_at,
            finished_at,
            duration_ms,
            json.dumps(extra),
        ),
    )
    conn.commit()


def ensure_parent(path: Path) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)


def run_external(
    cmd: list[str],
    *,
    cwd: Path | None = None,
    env: dict[str, str] | None = None,
    check: bool = True,
    capture_output: bool = True,
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        cmd,
        cwd=str(cwd) if cwd else None,
        env=env,
        text=True,
        capture_output=capture_output,
        check=check,
    )


def tool_path(name: str) -> str:
    return shutil.which(name) or ""


def tool_version(cmd: list[str]) -> str:
    proc = run_external(cmd, check=False)
    if proc.returncode != 0:
        return (proc.stderr or proc.stdout).strip()
    return (proc.stdout or proc.stderr).strip()


def patch_status(cfg: dict[str, Any]) -> str:
    root = hermes_root(cfg)
    patch = hermes_patch_path()
    if not root.exists():
        return "checkout-missing"
    reverse = run_external(
        ["git", "apply", "--reverse", "--check", str(patch)],
        cwd=root,
        check=False,
    )
    if reverse.returncode == 0:
        return "prepatched" if hermes_checkout_kind(root) == "bundled" else "applied"
    forward = run_external(
        ["git", "apply", "--check", str(patch)],
        cwd=root,
        check=False,
    )
    if forward.returncode == 0:
        return "not-applied"
    return "diverged"


def workspace_node_module_manifest(root: Path, package: str) -> Path:
    scoped_parts = package.split("/")
    candidates = (
        root / "node_modules" / Path(*scoped_parts) / "package.json",
        root / "ui-tui" / "node_modules" / Path(*scoped_parts) / "package.json",
    )
    for candidate in candidates:
        if candidate.exists():
            return candidate
    return candidates[0]


def doctor_prerequisites(cfg: dict[str, Any]) -> dict[str, Any]:
    git = tool_path("git")
    npm = tool_path("npm")
    python = sys.executable
    root = hermes_root(cfg)
    tui_dist = root / "ui-tui" / "dist" / "entry.js"
    tui_react = workspace_node_module_manifest(root, "react")
    tui_vitest = workspace_node_module_manifest(root, "vitest")
    python_bin = setup_python_bin(cfg)

    report: dict[str, Any] = {
        "git": {"found": bool(git), "path": git, "version": tool_version([git, "--version"]) if git else ""},
        "npm": {"found": bool(npm), "path": npm, "version": tool_version([npm, "--version"]) if npm else ""},
        "python": {"path": python, "version": tool_version([python, "--version"])},
        "python_runtime_supported": python_runtime_supported(sys.version_info[:2]),
        "hermes_checkout_exists": root.exists(),
        "hermes_checkout_kind": hermes_checkout_kind(root),
        "hermes_python_exists": python_bin.exists(),
        "bundled_hermes_available": bundled_hermes_archive().exists(),
        "patch_status": patch_status(cfg),
        "tui_dist_exists": tui_dist.exists(),
        "tui_react_installed": tui_react.exists(),
        "tui_vitest_installed": tui_vitest.exists(),
    }

    return report


def ensure_setup_prereqs(cfg: dict[str, Any], *, need_npm: bool, need_git: bool) -> None:
    prereqs = doctor_prerequisites(cfg)
    missing: list[str] = []

    if need_git and not prereqs["git"]["found"]:
        missing.append("git")
    if need_npm and not prereqs["npm"]["found"]:
        missing.append("npm")
    if not prereqs["python_runtime_supported"]:
        version = prereqs["python"]["version"]
        raise SystemExit(
            "TAG currently requires Python >=3.11 and <3.14 because the managed runtime does. "
            f"Current runtime: {version}."
        )

    if missing:
        names = ", ".join(missing)
        raise SystemExit(f"Missing required tools for TAG setup: {names}. Run `tag doctor` for details.")

def setup_python_bin(cfg: dict[str, Any]) -> Path:
    return hermes_root(cfg) / ".venv" / "bin" / "python"


def hermes_patch_path() -> Path:
    return resource_path("patches", "hermes-ui.patch")


def safe_extract_tar_gz(archive: Path, target: Path) -> None:
    target_real = target.resolve()
    try:
        with tarfile.open(archive, "r:gz") as tf:
            members = tf.getmembers()
            for member in members:
                member_name = member.name
                pure = PurePosixPath(member_name)
                if pure.is_absolute() or ".." in pure.parts:
                    raise SystemExit(f"Bundled Hermes archive contains an unsafe entry: {member_name}")
                if member.issym() or member.islnk():
                    raise SystemExit(f"Bundled Hermes archive contains an unsupported link entry: {member_name}")
                if not (member.isdir() or member.isfile()):
                    raise SystemExit(f"Bundled Hermes archive contains an unsupported entry type: {member_name}")
                dest = (target / member_name).resolve()
                if target_real != dest and target_real not in dest.parents:
                    raise SystemExit(f"Bundled Hermes archive contains a path traversal entry: {member_name}")
            for member in members:
                tf.extract(member, target)
    except (tarfile.TarError, OSError) as exc:
        raise SystemExit(f"Bundled Hermes archive could not be read: {archive}") from exc


def extract_bundled_hermes(root: Path) -> dict[str, Any]:
    archive = bundled_hermes_archive()
    if not archive.exists():
        raise SystemExit("Bundled Hermes snapshot is not available in this TAG build.")
    ensure_parent(root)
    if root.exists():
        shutil.rmtree(root)
    root.mkdir(parents=True, exist_ok=True)
    safe_extract_tar_gz(archive, root)
    return {"checkout": str(root), "status": "bundled", "archive": str(archive)}


def clone_or_update_hermes(cfg: dict[str, Any], *, refresh: bool) -> dict[str, Any]:
    override = os.environ.get("TAG_HERMES_ROOT", "").strip()
    root = (
        Path(override).expanduser().resolve()
        if override
        else resolve_home_relative(str(cfg.get("upstream", {}).get("checkout_dir", DEFAULT_HERMES_CHECKOUT)))
    )
    repo = hermes_repo_url(cfg)
    ref = hermes_ref(cfg)
    archive = bundled_hermes_archive()

    if root.exists():
        if refresh and not (root / ".git").exists() and archive.exists():
            return extract_bundled_hermes(root)
        if refresh:
            run_external(["git", "fetch", "--all", "--tags"], cwd=root)
            run_external(["git", "checkout", ref], cwd=root)
            run_external(["git", "pull", "--ff-only"], cwd=root, check=False)
            return {"checkout": str(root), "status": "updated", "ref": ref}
        return {"checkout": str(root), "status": "existing", "ref": ref}

    if archive.exists():
        return extract_bundled_hermes(root)

    ensure_parent(root)
    run_external(["git", "clone", "--depth", "1", "--branch", ref, repo, str(root)])
    return {"checkout": str(root), "status": "cloned", "ref": ref}


def ensure_venv(cfg: dict[str, Any]) -> dict[str, Any]:
    python_bin = setup_python_bin(cfg)
    if python_bin.exists():
        return {"venv": str(python_bin.parent.parent), "status": "existing"}
    run_external([sys.executable, "-m", "venv", str(python_bin.parent.parent)])
    return {"venv": str(python_bin.parent.parent), "status": "created"}


def install_hermes_python(cfg: dict[str, Any]) -> dict[str, Any]:
    python_bin = setup_python_bin(cfg)
    run_external([str(python_bin), "-m", "ensurepip", "--upgrade"], cwd=hermes_root(cfg), check=False)
    run_external([str(python_bin), "-m", "pip", "install", "--upgrade", "pip"], cwd=hermes_root(cfg))
    run_external(
        [str(python_bin), "-m", "pip", "install", "-e", ".[cli,web,mcp]"],
        cwd=hermes_root(cfg),
    )
    return {"python": str(python_bin), "status": "installed"}


def apply_hermes_patch(cfg: dict[str, Any]) -> dict[str, Any]:
    patch = hermes_patch_path()
    root = hermes_root(cfg)
    reverse = run_external(
        ["git", "apply", "--reverse", "--check", str(patch)],
        cwd=root,
        check=False,
    )
    if reverse.returncode == 0:
        status = "prepatched" if hermes_checkout_kind(root) == "bundled" else "already-applied"
        return {"patch": str(patch), "status": status}
    forward = run_external(
        ["git", "apply", "--check", str(patch)],
        cwd=root,
        check=False,
    )
    if forward.returncode != 0:
        message = forward.stderr.strip() or forward.stdout.strip() or "TAG patch check failed."
        raise SystemExit(message)
    run_external(["git", "apply", str(patch)], cwd=root)
    return {"patch": str(patch), "status": "applied"}


def install_tui_dependencies(cfg: dict[str, Any]) -> dict[str, Any]:
    root = hermes_root(cfg)
    run_external(
        [
            "npm",
            "install",
            "--workspace",
            "ui-tui",
            "--silent",
            "--no-fund",
            "--no-audit",
            "--progress=false",
        ],
        cwd=root,
    )
    run_external(["npm", "run", "build", "--workspace", "ui-tui"], cwd=root)
    return {"ui_tui": str(root / "ui-tui"), "status": "built"}


def import_codex_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_codex_home: Path,
) -> dict[str, Any]:
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}

    env = hermes_env(cfg)
    env["HERMES_HOME"] = str(target_home)
    env["CODEX_HOME"] = str(source_codex_home.expanduser().resolve())

    inline = textwrap.dedent(
        """
        import json
        from hermes_cli.auth import _import_codex_cli_tokens, _save_codex_tokens

        tokens = _import_codex_cli_tokens()
        if not tokens:
            raise SystemExit("No importable Codex CLI tokens found.")
        _save_codex_tokens(tokens)
        print(json.dumps({"imported": True}))
        """
    ).strip()

    proc = subprocess.run(
        [str(hermes_root(cfg) / ".venv" / "bin" / "python"), "-c", inline],
        env=env,
        text=True,
        capture_output=True,
    )
    if proc.returncode != 0:
        message = proc.stderr.strip() or proc.stdout.strip() or "Codex import failed."
        return {"profile": profile_name, "status": "failed", "message": message}
    return {"profile": profile_name, "status": "imported", "codex_home": str(env["CODEX_HOME"])}


def auto_import_codex_profiles(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    source_home = Path(
        os.environ.get("TAG_IMPORT_CODEX_HOME", str(Path.home() / ".codex"))
    ).expanduser().resolve()
    if not (source_home / "auth.json").exists():
        return [
            {"profile": "orchestrator", "status": "skipped-no-auth"},
            {"profile": "codex-runtime-master", "status": "skipped-no-auth"},
        ]
    results = []
    for profile_name in ("orchestrator", "codex-runtime-master"):
        results.append(
            import_codex_into_profile(
                cfg,
                profile_name=profile_name,
                source_codex_home=source_home,
            )
        )
    return results


# ---------- Claude Code credential import ----------


def _detect_claude_code_credentials(
    source_home: Path | None = None,
) -> dict[str, Any]:
    """Detect available Claude Code credentials on the local machine.

    Checks, in order:
      1. ANTHROPIC_API_KEY env var (safest — no ToS risk)
      2. ~/.claude/.credentials.json  (OAuth, Linux/Windows/SSH)
      3. ~/.claude.json               (OAuth, macOS app-state fallback)

    Returns a dict with keys: api_key, oauth_token, oauth_expires_at, source.
    """
    result: dict[str, Any] = {
        "api_key": None,
        "oauth_token": None,
        "oauth_expires_at": None,
        "source": None,
    }

    api_key = os.environ.get("ANTHROPIC_API_KEY", "").strip()
    if api_key:
        result["api_key"] = api_key

    claude_home = source_home or (Path.home() / ".claude")

    creds_file = claude_home / ".credentials.json"
    if creds_file.exists():
        try:
            data = json.loads(creds_file.read_text(encoding="utf-8"))
            oauth = data.get("claudeAiOauth") or {}
            token = (oauth.get("accessToken") or "").strip()
            if token:
                result["oauth_token"] = token
                result["oauth_expires_at"] = oauth.get("expiresAt")
                result["source"] = str(creds_file)
        except (json.JSONDecodeError, OSError):
            pass

    if not result["oauth_token"]:
        dot_claude_json = Path.home() / ".claude.json"
        if dot_claude_json.exists():
            try:
                data = json.loads(dot_claude_json.read_text(encoding="utf-8"))
                oauth = data.get("claudeAiOauth") or data.get("oauthAccount") or {}
                token = (oauth.get("accessToken") or "").strip()
                if token:
                    result["oauth_token"] = token
                    result["oauth_expires_at"] = oauth.get("expiresAt")
                    result["source"] = str(dot_claude_json)
            except (json.JSONDecodeError, OSError):
                pass

    return result


def import_claude_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_claude_home: Path | None = None,
    use_oauth: bool = False,
) -> dict[str, Any]:
    """Import Claude Code / Anthropic credentials into a TAG-managed Hermes profile.

    API key (ANTHROPIC_API_KEY) is always preferred — zero ToS risk.
    OAuth token (CLAUDE_CODE_OAUTH_TOKEN) is only written when use_oauth=True;
    Anthropic prohibits use of claude auth login tokens in third-party tools
    as of early 2026 and actively suspends violating accounts.
    """
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}

    creds = _detect_claude_code_credentials(source_claude_home)

    if creds["api_key"]:
        _upsert_env_line(target_home / ".env", "ANTHROPIC_API_KEY", creds["api_key"])
        return {
            "profile": profile_name,
            "status": "imported",
            "mode": "api_key",
            "provider": "anthropic",
        }

    if use_oauth and creds["oauth_token"]:
        _upsert_env_line(target_home / ".env", "CLAUDE_CODE_OAUTH_TOKEN", creds["oauth_token"])
        return {
            "profile": profile_name,
            "status": "imported",
            "mode": "oauth",
            "provider": "anthropic",
            "source": creds["source"],
            "tos_warning": (
                "Anthropic prohibits use of claude auth login OAuth tokens in "
                "third-party tools. Set ANTHROPIC_API_KEY for ToS-compliant access."
            ),
        }

    return {"profile": profile_name, "status": "skipped-no-auth"}


def auto_import_claude_profiles(
    cfg: dict[str, Any],
    *,
    use_oauth: bool = False,
) -> list[dict[str, Any]]:
    """Auto-import Claude/Anthropic credentials into all non-Codex profiles during setup."""
    creds = _detect_claude_code_credentials()
    if not creds["api_key"] and not (use_oauth and creds["oauth_token"]):
        return [
            {"profile": p, "status": "skipped-no-auth"}
            for p in cfg.get("profiles", {})
            if p != "codex-runtime-master"
        ]
    return [
        import_claude_into_profile(cfg, profile_name=p, use_oauth=use_oauth)
        for p in cfg.get("profiles", {})
        if p != "codex-runtime-master"
    ]


# ---------- Gemini CLI credential import ----------


def _detect_gemini_credentials(
    source_home: Path | None = None,
) -> dict[str, Any]:
    """Detect available Gemini CLI credentials on the local machine.

    Checks, in order:
      1. GEMINI_API_KEY env var (safest — no ToS risk)
      2. ~/.gemini/.env          (GEMINI_API_KEY stored by gemini CLI)
      3. ~/.gemini/oauth_creds.json  (OAuth, use_oauth=True only)

    Returns a dict with keys:
      api_key, oauth_token, refresh_token, oauth_expiry_ms, source.
    """
    result: dict[str, Any] = {
        "api_key": None,
        "oauth_token": None,
        "refresh_token": None,
        "oauth_expiry_ms": None,
        "source": None,
    }

    api_key = os.environ.get("GEMINI_API_KEY", "").strip()
    if api_key:
        result["api_key"] = api_key

    gemini_home = source_home or (Path.home() / ".gemini")

    if not result["api_key"]:
        gemini_dotenv = gemini_home / ".env"
        key = read_dotenv(gemini_dotenv).get("GEMINI_API_KEY", "").strip()
        if key:
            result["api_key"] = key

    oauth_file = gemini_home / "oauth_creds.json"
    if oauth_file.exists():
        try:
            data = json.loads(oauth_file.read_text(encoding="utf-8"))
            token = (data.get("access_token") or "").strip()
            refresh = (data.get("refresh_token") or "").strip()
            if token or refresh:
                result["oauth_token"] = token or None
                result["refresh_token"] = refresh or None
                result["oauth_expiry_ms"] = data.get("expiry_date")
                result["source"] = str(oauth_file)
        except (json.JSONDecodeError, OSError):
            pass

    return result


def import_gemini_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_gemini_home: Path | None = None,
    use_oauth: bool = False,
) -> dict[str, Any]:
    """Import Gemini CLI / Google API credentials into a TAG-managed Hermes profile.

    API key (GEMINI_API_KEY) is always preferred — zero ToS risk.
    OAuth tokens from ~/.gemini/oauth_creds.json are written to Hermes's
    google_oauth.json store only when use_oauth=True. Google explicitly bans
    piggybacking on Gemini CLI OAuth (enforced with account suspensions since
    March 2026). Use GEMINI_API_KEY from https://aistudio.google.com/app/apikey
    for ToS-compliant access.
    """
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}

    creds = _detect_gemini_credentials(source_gemini_home)

    if creds["api_key"]:
        _upsert_env_line(target_home / ".env", "GEMINI_API_KEY", creds["api_key"])
        return {
            "profile": profile_name,
            "status": "imported",
            "mode": "api_key",
            "provider": "gemini",
        }

    if use_oauth and (creds["oauth_token"] or creds["refresh_token"]):
        google_oauth_dir = target_home / "auth"
        google_oauth_dir.mkdir(parents=True, exist_ok=True)
        google_oauth_file = google_oauth_dir / "google_oauth.json"
        existing: dict[str, Any] = {}
        if google_oauth_file.exists():
            try:
                existing = json.loads(google_oauth_file.read_text(encoding="utf-8"))
            except (json.JSONDecodeError, OSError):
                existing = {}
        existing.update({
            "access_token": creds["oauth_token"] or "",
            "refresh_token": creds["refresh_token"] or "",
            "expiry_date": creds["oauth_expiry_ms"],
            "source": "gemini-cli-import",
        })
        google_oauth_file.write_text(json.dumps(existing, indent=2), encoding="utf-8")
        return {
            "profile": profile_name,
            "status": "imported",
            "mode": "oauth",
            "provider": "google-gemini-cli",
            "source": creds["source"],
            "tos_warning": (
                "Google explicitly prohibits piggybacking on Gemini CLI OAuth tokens "
                "in third-party tools and began enforcing bans in March 2026. "
                "Use GEMINI_API_KEY from https://aistudio.google.com/app/apikey "
                "for ToS-compliant access."
            ),
        }

    return {"profile": profile_name, "status": "skipped-no-auth"}


def auto_import_gemini_profiles(
    cfg: dict[str, Any],
    *,
    use_oauth: bool = False,
) -> list[dict[str, Any]]:
    """Auto-import Gemini credentials into all profiles during setup."""
    creds = _detect_gemini_credentials()
    if not creds["api_key"] and not (use_oauth and (creds["oauth_token"] or creds["refresh_token"])):
        return [
            {"profile": p, "status": "skipped-no-auth"}
            for p in cfg.get("profiles", {})
        ]
    return [
        import_gemini_into_profile(cfg, profile_name=p, use_oauth=use_oauth)
        for p in cfg.get("profiles", {})
    ]


# ---------- Continue.dev credential import ----------

# Maps Continue provider slugs to the Hermes env var name
_CONTINUE_PROVIDER_ENV_MAP: dict[str, str] = {
    "openai": "OPENAI_API_KEY",
    "anthropic": "ANTHROPIC_API_KEY",
    "google": "GEMINI_API_KEY",
    "gemini": "GEMINI_API_KEY",
    "mistral": "MISTRAL_API_KEY",
    "deepseek": "DEEPSEEK_API_KEY",
    "xai": "XAI_API_KEY",
    "openrouter": "OPENROUTER_API_KEY",
    "huggingface": "HF_TOKEN",
    "nvidia": "NVIDIA_API_KEY",
    "groq": "GROQ_API_KEY",
    "together": "TOGETHER_API_KEY",
    "cohere": "COHERE_API_KEY",
    "fireworks": "FIREWORKS_API_KEY",
    "perplexity": "PERPLEXITY_API_KEY",
}


def _detect_continue_credentials(
    source_home: Path | None = None,
) -> dict[str, str]:
    """Detect API keys stored in a Continue.dev config file.

    Reads ~/.continue/config.yaml (new format) or ~/.continue/config.json
    (deprecated format) and extracts provider→apiKey mappings.

    Returns a dict mapping Hermes env var names to key values.
    Keys stored as 'localEnv:VAR_NAME' references are resolved from the
    current environment; entries without resolvable values are skipped.
    """
    continue_home = source_home or (Path.home() / ".continue")
    found: dict[str, str] = {}

    def _resolve_key(raw: str) -> str | None:
        raw = (raw or "").strip()
        if raw.startswith("localEnv:"):
            return os.environ.get(raw[len("localEnv:"):], "").strip() or None
        return raw or None

    def _extract_from_models(models: list[Any]) -> None:
        for model in models:
            if not isinstance(model, dict):
                continue
            provider = (model.get("provider") or "").strip().lower()
            api_key = _resolve_key(model.get("apiKey") or model.get("api_key") or "")
            env_var = _CONTINUE_PROVIDER_ENV_MAP.get(provider)
            if env_var and api_key and env_var not in found:
                found[env_var] = api_key

    yaml_cfg = continue_home / "config.yaml"
    json_cfg = continue_home / "config.json"

    if yaml_cfg.exists():
        try:
            data = yaml.safe_load(yaml_cfg.read_text(encoding="utf-8")) or {}
            _extract_from_models(data.get("models") or [])
        except Exception:
            pass

    if json_cfg.exists():
        try:
            data = json.loads(json_cfg.read_text(encoding="utf-8"))
            _extract_from_models(data.get("models") or [])
        except (json.JSONDecodeError, OSError):
            pass

    return found


def import_continue_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_continue_home: Path | None = None,
) -> dict[str, Any]:
    """Import API keys from a Continue.dev config into a TAG-managed Hermes profile.

    Reads ~/.continue/config.yaml (or config.json) and writes each discovered
    provider API key to the profile's .env file. Only env-var style keys are
    written — no OAuth tokens are involved, so there is no ToS risk.
    """
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}

    keys = _detect_continue_credentials(source_continue_home)
    if not keys:
        return {"profile": profile_name, "status": "skipped-no-auth"}

    env_file = target_home / ".env"
    for env_var, value in keys.items():
        _upsert_env_line(env_file, env_var, value)

    return {
        "profile": profile_name,
        "status": "imported",
        "mode": "api_keys",
        "providers_imported": list(keys.keys()),
    }


def auto_import_continue_profiles(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    """Auto-import Continue.dev API keys into all profiles during setup."""
    keys = _detect_continue_credentials()
    if not keys:
        return [
            {"profile": p, "status": "skipped-no-auth"}
            for p in cfg.get("profiles", {})
        ]
    return [
        import_continue_into_profile(cfg, profile_name=p)
        for p in cfg.get("profiles", {})
    ]


# ---------- Mistral Vibe credential import ----------


def _detect_mistral_credentials(
    source_home: Path | None = None,
) -> dict[str, Any]:
    """Detect Mistral API key from Mistral Vibe CLI config.

    Checks, in order:
      1. MISTRAL_API_KEY env var
      2. ~/.vibe/.env (written by `mistral-vibe` CLI on first auth)
    """
    result: dict[str, Any] = {"api_key": None, "source": None}

    api_key = os.environ.get("MISTRAL_API_KEY", "").strip()
    if api_key:
        result["api_key"] = api_key
        return result

    vibe_home = source_home or (Path.home() / ".vibe")
    vibe_dotenv = vibe_home / ".env"
    if vibe_dotenv.exists():
        key = read_dotenv(vibe_dotenv).get("MISTRAL_API_KEY", "").strip()
        if key:
            result["api_key"] = key
            result["source"] = str(vibe_dotenv)

    return result


def import_mistral_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_vibe_home: Path | None = None,
) -> dict[str, Any]:
    """Import Mistral API key from the Mistral Vibe CLI into a TAG-managed profile."""
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}

    creds = _detect_mistral_credentials(source_vibe_home)
    if not creds["api_key"]:
        return {"profile": profile_name, "status": "skipped-no-auth"}

    _upsert_env_line(target_home / ".env", "MISTRAL_API_KEY", creds["api_key"])
    return {
        "profile": profile_name,
        "status": "imported",
        "mode": "api_key",
        "provider": "mistral",
        "source": creds.get("source"),
    }


def auto_import_mistral_profiles(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    """Auto-import Mistral API key into all profiles during setup."""
    creds = _detect_mistral_credentials()
    if not creds["api_key"]:
        return [
            {"profile": p, "status": "skipped-no-auth"}
            for p in cfg.get("profiles", {})
        ]
    return [
        import_mistral_into_profile(cfg, profile_name=p)
        for p in cfg.get("profiles", {})
    ]


def ensure_hermes_ready(
    cfg: dict[str, Any],
    *,
    config_arg: str | None,
    need_tui: bool,
) -> None:
    if hermes_bin(cfg).exists():
        return
    setup_args = argparse.Namespace(
        config=config_arg,
        refresh=False,
        skip_python_install=False,
        skip_tui_build=not need_tui,
        json=False,
    )
    cmd_setup(setup_args)


def normalize_hermes_passthrough_args(args: list[str]) -> list[str]:
    normalized = list(args)
    if normalized[:1] == ["--"]:
        normalized = normalized[1:]
    if len(normalized) >= 2 and normalized[1] == "--":
        normalized = [normalized[0], *normalized[2:]]
    if not normalized:
        return ["--help"]
    return normalized


def cmd_setup(args: argparse.Namespace) -> int:
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
    raw_args = list(args.hermes_args)
    normalized_args = normalize_hermes_passthrough_args(raw_args)
    if raw_args and normalized_args in (["--help"], ["-h"]):
        passthrough = argparse.Namespace(
            config=args.config,
            profile=args.profile,
            hermes_args=["--help"],
            hermes_version=False,
        )
        return cmd_hermes_passthrough(passthrough)
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
    return cmd_hermes_passthrough(passthrough)


def cmd_hermes_command(args: argparse.Namespace, command_name: str) -> int:
    forwarded = [command_name, *args.hermes_args]
    passthrough = argparse.Namespace(
        config=args.config,
        profile=args.profile,
        hermes_args=forwarded,
        hermes_version=False,
    )
    return cmd_hermes_passthrough(passthrough)


def cmd_chat(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "chat")


def cmd_gateway(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "gateway")


def cmd_kanban(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "kanban")


def cmd_model(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "model")


def cmd_profile(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "profile")


def cmd_status(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "status")


def cmd_config(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "config")


def cmd_sessions(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "sessions")


def cmd_skills(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "skills")


def cmd_plugins(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "plugins")


def cmd_tools(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "tools")


def cmd_mcp(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "mcp")


def cmd_logs(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "logs")


def cmd_dashboard(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "dashboard")


def cmd_memory(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "memory")


def cmd_completion(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "completion")


def cmd_prompt_size(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "prompt-size")


def cmd_update(args: argparse.Namespace) -> int:
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
        cmd_setup(setup_args)
    else:
        bootstrap_profiles(cfg)
        render_profiles(cfg, force=False)
    tui_args = argparse.Namespace(config=args.config, profile="orchestrator", hermes_args=[])
    return cmd_tui(tui_args)


def cmd_doctor(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    env = hermes_env(cfg)
    report = {
        "app_name": APP_NAME,
        "package_root": str(package_root()),
        "tag_home": str(tag_home()),
        "managed_root": str(managed_root()),
        "hermes_root": str(hermes_root(cfg)),
        "hermes_bin_exists": hermes_bin(cfg).exists(),
        "home": env["HOME"],
        "hermes_home": env["HERMES_HOME"],
        "codex_home": env["CODEX_HOME"],
        "config": str(config_path(args.config)),
        "benchmark_suite": str(benchmark_suite_path(None)),
        "prerequisites": doctor_prerequisites(cfg),
    }
    if hermes_bin(cfg).exists():
        try:
            version = run_hermes(cfg, "--version")
            report["hermes_version"] = version.stdout.strip()
        except subprocess.CalledProcessError as exc:
            report["hermes_version_error"] = exc.stderr.strip()
    else:
        report["hermes_version"] = "not provisioned yet"

    if args.json:
        print(json.dumps(report, indent=2))
        return 0

    for key, value in report.items():
        if key == "prerequisites":
            print("prerequisites:")
            for pkey, pdata in value.items():
                print(f"  {pkey}: {pdata}")
            continue
        print(f"{key}: {value}")
    return 0


def cmd_bootstrap(args: argparse.Namespace) -> int:
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
        print(f"  {item['profile']}: {item['config']}")
    return 0


def cmd_render(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    rendered = render_profiles(cfg, force=args.force)
    if args.json:
        print(json.dumps(rendered, indent=2))
        return 0
    for item in rendered:
        print(f"{item['profile']}: {item['config']}")
    return 0


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
    print(f"master: {route['master']['name']} -> {route['master']['model']}")
    for worker in route["workers"]:
        print(f"worker: {worker['name']} -> {worker['model']}")
    if route["verifier"]:
        print(f"verifier: {route['verifier']['name']} -> {route['verifier']['model']}")
    return 0


def cmd_env(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    env = hermes_env(cfg)
    for key in ("HOME", "HERMES_HOME", "CODEX_HOME", "PATH"):
        print(f"{key}={env[key]}")
    return 0


def cmd_import_codex(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profiles = cfg.get("profiles", {})
    if args.profile not in profiles:
        available = ", ".join(sorted(profiles))
        raise SystemExit(f"Unknown profile '{args.profile}'. Available: {available}")

    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, args.profile)
    if not target_home.exists():
        raise SystemExit(
            f"Profile home does not exist for '{args.profile}'. Run bootstrap first."
        )

    source_home = (
        Path(args.codex_home).expanduser().resolve()
        if args.codex_home
        else Path(
            os.environ.get("TAG_IMPORT_CODEX_HOME", str(runtime_codex_home(cfg)))
        ).expanduser().resolve()
    )
    result = import_codex_into_profile(
        cfg,
        profile_name=args.profile,
        source_codex_home=source_home,
    )
    if result["status"] != "imported":
        raise SystemExit(str(result.get("message", "Codex import failed.")))

    if args.json:
        print(
            json.dumps(
                {
                    "profile": args.profile,
                    "codex_home": str(source_home),
                    "hermes_home": str(target_home),
                    "status": "imported",
                },
                indent=2,
            )
        )
        return 0

    print(f"Imported Codex credentials into profile '{args.profile}'.")
    return 0


def cmd_import_claude(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profiles = cfg.get("profiles", {})
    if args.profile not in profiles:
        available = ", ".join(sorted(profiles))
        raise SystemExit(f"Unknown profile '{args.profile}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, args.profile)
    if not target_home.exists():
        raise SystemExit(
            f"Profile home does not exist for '{args.profile}'. Run `tag bootstrap` first."
        )
    source_home = (
        Path(args.claude_home).expanduser().resolve()
        if getattr(args, "claude_home", None)
        else None
    )
    result = import_claude_into_profile(
        cfg,
        profile_name=args.profile,
        source_claude_home=source_home,
        use_oauth=getattr(args, "use_oauth", False),
    )
    if result["status"] == "skipped-no-auth":
        raise SystemExit(
            "No Claude credentials found. Set ANTHROPIC_API_KEY or use "
            "`tag import-claude --use-oauth` to import from claude auth login."
        )
    if result["status"] == "profile-missing":
        raise SystemExit(f"Profile '{args.profile}' home does not exist. Run `tag bootstrap` first.")
    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    mode = result.get("mode", "unknown")
    print(f"Imported Claude credentials into profile '{args.profile}' (mode: {mode}).")
    if "tos_warning" in result:
        print(f"WARNING: {result['tos_warning']}")
    return 0


def cmd_import_gemini(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profiles = cfg.get("profiles", {})
    if args.profile not in profiles:
        available = ", ".join(sorted(profiles))
        raise SystemExit(f"Unknown profile '{args.profile}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, args.profile)
    if not target_home.exists():
        raise SystemExit(
            f"Profile home does not exist for '{args.profile}'. Run `tag bootstrap` first."
        )
    source_home = (
        Path(args.gemini_home).expanduser().resolve()
        if getattr(args, "gemini_home", None)
        else None
    )
    result = import_gemini_into_profile(
        cfg,
        profile_name=args.profile,
        source_gemini_home=source_home,
        use_oauth=getattr(args, "use_oauth", False),
    )
    if result["status"] == "skipped-no-auth":
        raise SystemExit(
            "No Gemini credentials found. Set GEMINI_API_KEY (from "
            "https://aistudio.google.com/app/apikey) or use "
            "`tag import-gemini --use-oauth` to import from ~/.gemini/oauth_creds.json."
        )
    if result["status"] == "profile-missing":
        raise SystemExit(f"Profile '{args.profile}' home does not exist. Run `tag bootstrap` first.")
    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    mode = result.get("mode", "unknown")
    print(f"Imported Gemini credentials into profile '{args.profile}' (mode: {mode}).")
    if "tos_warning" in result:
        print(f"WARNING: {result['tos_warning']}")
    return 0


def cmd_import_continue(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profiles = cfg.get("profiles", {})
    if args.profile not in profiles:
        available = ", ".join(sorted(profiles))
        raise SystemExit(f"Unknown profile '{args.profile}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, args.profile)
    if not target_home.exists():
        raise SystemExit(
            f"Profile home does not exist for '{args.profile}'. Run `tag bootstrap` first."
        )
    source_home = (
        Path(args.continue_home).expanduser().resolve()
        if getattr(args, "continue_home", None)
        else None
    )
    result = import_continue_into_profile(cfg, profile_name=args.profile, source_continue_home=source_home)
    if result["status"] == "skipped-no-auth":
        raise SystemExit(
            "No Continue.dev config found with API keys. "
            "Expected ~/.continue/config.yaml or ~/.continue/config.json."
        )
    if result["status"] == "profile-missing":
        raise SystemExit(f"Profile '{args.profile}' home does not exist. Run `tag bootstrap` first.")
    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    providers = ", ".join(result.get("providers_imported") or [])
    print(f"Imported Continue.dev credentials into profile '{args.profile}' ({providers}).")
    return 0


def cmd_import_mistral(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profiles = cfg.get("profiles", {})
    if args.profile not in profiles:
        available = ", ".join(sorted(profiles))
        raise SystemExit(f"Unknown profile '{args.profile}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, args.profile)
    if not target_home.exists():
        raise SystemExit(
            f"Profile home does not exist for '{args.profile}'. Run `tag bootstrap` first."
        )
    source_home = (
        Path(args.vibe_home).expanduser().resolve()
        if getattr(args, "vibe_home", None)
        else None
    )
    result = import_mistral_into_profile(cfg, profile_name=args.profile, source_vibe_home=source_home)
    if result["status"] == "skipped-no-auth":
        raise SystemExit(
            "No Mistral credentials found. Set MISTRAL_API_KEY or ensure "
            "`mistral-vibe` has written ~/.vibe/.env."
        )
    if result["status"] == "profile-missing":
        raise SystemExit(f"Profile '{args.profile}' home does not exist. Run `tag bootstrap` first.")
    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    print(f"Imported Mistral credentials into profile '{args.profile}'.")
    return 0


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


def cmd_models(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_profile_exists(cfg, args.profile)
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
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


def cmd_set_model(args: argparse.Namespace) -> int:
    path = config_path(args.config)
    cfg = load_config(path)
    ensure_profile_exists(cfg, args.profile)
    provider, model = parse_model_ref(args.ref)
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

    save_config(path, cfg)
    render_profiles(cfg, force=True)

    result = {
        "profile": args.profile,
        "target": args.target,
        "ref": f"{provider}/{model}",
        "config": str(path),
    }
    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    print(f"{args.profile} {args.target} model -> {provider}/{model}")
    return 0


def cmd_submit(args: argparse.Namespace) -> int:
    cfg_path = config_path(args.config)
    cfg = load_config(cfg_path)
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
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


def cmd_benchmark(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
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

    model_refs = args.model_ref or [
        collect_assignments(cfg)
        and next(
            row["primary_model"]
            for row in collect_assignments(cfg)
            if row["profile"] == args.profile
        )
    ]
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
    update_run_status(conn, run_id=run_id, status=result["status"], metadata={"suite": str(benchmark_suite_path(args.suite))})
    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    print(f"run_id: {run_id}")
    print(f"status: {result['status']}")
    for model in result["models"]:
        failed = sum(1 for case in model["cases"] if case["status"] != "ok")
        print(f"{model['model_ref']}: {len(model['cases']) - failed}/{len(model['cases'])} passed")
    return 0


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


def cmd_openrouter_models(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_profile_exists(cfg, args.profile)
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
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


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="TAG orchestration CLI")
    parser.add_argument("--config", help="Path to lab config YAML")
    parser.add_argument("--version", action="version", version=f"%(prog)s {__version__}")
    sub = parser.add_subparsers(dest="command")

    setup = sub.add_parser("setup", help="Provision the managed runtime, apply TAG patches, build the TUI, and bootstrap profiles")
    setup.add_argument("--refresh", action="store_true", help="Fetch and update an existing managed runtime checkout")
    setup.add_argument("--skip-python-install", action="store_true")
    setup.add_argument("--skip-tui-build", action="store_true")
    setup.add_argument("--json", action="store_true")
    setup.set_defaults(func=cmd_setup)

    doctor = sub.add_parser("doctor", help="Validate local TAG paths and the managed runtime")
    doctor.add_argument("--json", action="store_true")
    doctor.set_defaults(func=cmd_doctor)

    bootstrap = sub.add_parser("bootstrap", help="Create profiles and render config")
    bootstrap.add_argument("--force", action="store_true", help="Overwrite rendered files")
    bootstrap.add_argument("--json", action="store_true")
    bootstrap.set_defaults(func=cmd_bootstrap)

    render = sub.add_parser("render", help="Render per-profile config only")
    render.add_argument("--force", action="store_true", help="Overwrite rendered files")
    render.add_argument("--json", action="store_true")
    render.set_defaults(func=cmd_render)

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

    env_cmd = sub.add_parser("env", help="Print the isolated environment values")
    env_cmd.set_defaults(func=cmd_env)

    assignments = sub.add_parser(
        "assignments", help="Show the current default model assignment per profile"
    )
    assignments.add_argument("--json", action="store_true")
    assignments.set_defaults(func=cmd_assignments)

    models = sub.add_parser(
        "models", help="List curated provider/model options for a profile"
    )
    models.add_argument("--profile", required=True)
    models.add_argument("--provider", help="Filter to one provider slug")
    models.add_argument("--limit", type=nonnegative_int, default=10)
    models.add_argument("--json", action="store_true")
    models.set_defaults(func=cmd_models)

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

    benchmark = sub.add_parser(
        "benchmark", help="Run a prompt-contract benchmark across one or more models"
    )
    benchmark.add_argument("--profile", required=True)
    benchmark.add_argument("--suite", help="Path to benchmark suite YAML")
    benchmark.add_argument("--model-ref", action="append", default=[])
    benchmark.add_argument("--case", action="append", default=[])
    benchmark.add_argument("--json", action="store_true")
    benchmark.set_defaults(func=cmd_benchmark)

    runs = sub.add_parser("runs", help="Show recent submit and benchmark runs")
    runs.add_argument("--limit", type=positive_int, default=20)
    runs.add_argument("--json", action="store_true")
    runs.set_defaults(func=cmd_runs)

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

    import_codex = sub.add_parser(
        "import-codex",
        help="Import existing Codex CLI credentials into a TAG-managed profile",
    )
    import_codex.add_argument("--profile", required=True)
    import_codex.add_argument("--codex-home", help="Path to the source CODEX_HOME")
    import_codex.add_argument("--json", action="store_true")
    import_codex.set_defaults(func=cmd_import_codex)

    import_claude = sub.add_parser(
        "import-claude",
        help="Import Claude Code / Anthropic API credentials into a TAG-managed profile",
    )
    import_claude.add_argument("--profile", required=True)
    import_claude.add_argument(
        "--claude-home",
        help="Path to source ~/.claude directory (default: ~/.claude)",
    )
    import_claude.add_argument(
        "--use-oauth",
        action="store_true",
        help=(
            "Import the OAuth session token from `claude auth login`. "
            "Anthropic prohibits this in third-party tools; ANTHROPIC_API_KEY is preferred."
        ),
    )
    import_claude.add_argument("--json", action="store_true")
    import_claude.set_defaults(func=cmd_import_claude)

    import_gemini = sub.add_parser(
        "import-gemini",
        help="Import Gemini CLI / Google API credentials into a TAG-managed profile",
    )
    import_gemini.add_argument("--profile", required=True)
    import_gemini.add_argument(
        "--gemini-home",
        help="Path to source ~/.gemini directory (default: ~/.gemini)",
    )
    import_gemini.add_argument(
        "--use-oauth",
        action="store_true",
        help=(
            "Import OAuth tokens from ~/.gemini/oauth_creds.json. "
            "Google prohibits this in third-party tools; GEMINI_API_KEY is preferred."
        ),
    )
    import_gemini.add_argument("--json", action="store_true")
    import_gemini.set_defaults(func=cmd_import_gemini)

    import_continue = sub.add_parser(
        "import-continue",
        help="Import API keys from a Continue.dev config into a TAG-managed profile",
    )
    import_continue.add_argument("--profile", required=True)
    import_continue.add_argument(
        "--continue-home",
        help="Path to source ~/.continue directory (default: ~/.continue)",
    )
    import_continue.add_argument("--json", action="store_true")
    import_continue.set_defaults(func=cmd_import_continue)

    import_mistral = sub.add_parser(
        "import-mistral",
        help="Import Mistral API key from the Mistral Vibe CLI into a TAG-managed profile",
    )
    import_mistral.add_argument("--profile", required=True)
    import_mistral.add_argument(
        "--vibe-home",
        help="Path to source ~/.vibe directory (default: ~/.vibe)",
    )
    import_mistral.add_argument("--json", action="store_true")
    import_mistral.set_defaults(func=cmd_import_mistral)

    hermes_cmd = sub.add_parser("hermes", help="Pass raw arguments through to the managed runtime binary")
    hermes_cmd.add_argument("--profile", help="Run the managed runtime inside one TAG profile home")
    hermes_cmd.add_argument("--version", dest="hermes_version", action="store_true", help="Show the managed runtime version")
    hermes_cmd.add_argument("hermes_args", nargs=argparse.REMAINDER)
    hermes_cmd.set_defaults(func=cmd_hermes_passthrough)

    chat = sub.add_parser("chat", help="Run chat inside a TAG profile")
    chat.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    chat.add_argument("hermes_args", nargs=argparse.REMAINDER)
    chat.set_defaults(func=cmd_chat)

    gateway = sub.add_parser("gateway", help="Run gateway commands inside a TAG profile")
    gateway.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    gateway.add_argument("hermes_args", nargs=argparse.REMAINDER)
    gateway.set_defaults(func=cmd_gateway)

    kanban = sub.add_parser("kanban", help="Run Kanban commands inside a TAG profile")
    kanban.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    kanban.add_argument("hermes_args", nargs=argparse.REMAINDER)
    kanban.set_defaults(func=cmd_kanban)

    model = sub.add_parser("model", help="Run model commands inside a TAG profile")
    model.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    model.add_argument("hermes_args", nargs=argparse.REMAINDER)
    model.set_defaults(func=cmd_model)

    profile = sub.add_parser("profile", help="Run profile commands in the managed TAG environment")
    profile.add_argument("--profile", help="Optional active profile home override")
    profile.add_argument("hermes_args", nargs=argparse.REMAINDER)
    profile.set_defaults(func=cmd_profile)

    status = sub.add_parser("status", help="Run status inside a TAG profile")
    status.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    status.add_argument("hermes_args", nargs=argparse.REMAINDER)
    status.set_defaults(func=cmd_status)

    config_cmd = sub.add_parser("config", help="Run config inside a TAG profile")
    config_cmd.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    config_cmd.add_argument("hermes_args", nargs=argparse.REMAINDER)
    config_cmd.set_defaults(func=cmd_config)

    sessions = sub.add_parser("sessions", help="Run sessions inside a TAG profile")
    sessions.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    sessions.add_argument("hermes_args", nargs=argparse.REMAINDER)
    sessions.set_defaults(func=cmd_sessions)

    skills = sub.add_parser("skills", help="Run skills inside a TAG profile")
    skills.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    skills.add_argument("hermes_args", nargs=argparse.REMAINDER)
    skills.set_defaults(func=cmd_skills)

    plugins = sub.add_parser("plugins", help="Run plugins inside a TAG profile")
    plugins.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    plugins.add_argument("hermes_args", nargs=argparse.REMAINDER)
    plugins.set_defaults(func=cmd_plugins)

    tools_cmd = sub.add_parser("tools", help="Run tools inside a TAG profile")
    tools_cmd.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    tools_cmd.add_argument("hermes_args", nargs=argparse.REMAINDER)
    tools_cmd.set_defaults(func=cmd_tools)

    mcp = sub.add_parser("mcp", help="Run MCP commands inside a TAG profile")
    mcp.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    mcp.add_argument("hermes_args", nargs=argparse.REMAINDER)
    mcp.set_defaults(func=cmd_mcp)

    logs = sub.add_parser("logs", help="Run logs inside a TAG profile")
    logs.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    logs.add_argument("hermes_args", nargs=argparse.REMAINDER)
    logs.set_defaults(func=cmd_logs)

    dashboard = sub.add_parser("dashboard", help="Run dashboard inside a TAG profile")
    dashboard.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    dashboard.add_argument("hermes_args", nargs=argparse.REMAINDER)
    dashboard.set_defaults(func=cmd_dashboard)

    memory = sub.add_parser("memory", help="Run memory inside a TAG profile")
    memory.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    memory.add_argument("hermes_args", nargs=argparse.REMAINDER)
    memory.set_defaults(func=cmd_memory)

    completion = sub.add_parser("completion", help="Run completion inside a TAG profile")
    completion.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    completion.add_argument("hermes_args", nargs=argparse.REMAINDER)
    completion.set_defaults(func=cmd_completion)

    prompt_size = sub.add_parser("prompt-size", help="Run prompt-size inside a TAG profile")
    prompt_size.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    prompt_size.add_argument("hermes_args", nargs=argparse.REMAINDER)
    prompt_size.set_defaults(func=cmd_prompt_size)

    update = sub.add_parser("update", help="Run update inside a TAG profile")
    update.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    update.add_argument("--json", action="store_true", help="When TAG manages the update locally, emit JSON")
    update.add_argument("hermes_args", nargs=argparse.REMAINDER)
    update.set_defaults(func=cmd_update)

    tui = sub.add_parser("tui", help="Launch the managed TUI through TAG")
    tui.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    tui.add_argument("hermes_args", nargs=argparse.REMAINDER)
    tui.set_defaults(func=cmd_tui)

    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()
    if not getattr(args, "command", None):
        return int(cmd_default(args))
    return int(args.func(args))


if __name__ == "__main__":
    sys.exit(main())
