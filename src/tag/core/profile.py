"""Profile management, routing, and eval utilities for TAG CLI."""
from __future__ import annotations

import datetime as dt
import json
import os
import re
import shutil
import sqlite3
import subprocess
import sys
import textwrap
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path
from typing import Any

import yaml

from tag.core.paths import (
    hermes_root,
    runtime_home,
    profile_home,
    hermes_env,
    tag_home,
    resolve_home_relative,
    hermes_bin,
    profile_exec_env,
    ensure_runtime_dirs,
)
from tag.core.utils import (
    install_profile_skins,
    _deep_merge,
    write_yaml,
    write_text,
    configured_skins,
    read_dotenv,
    utc_now,
)
from tag.core.config import load_config


# ---------------------------------------------------------------------------
# Internal helpers
# ---------------------------------------------------------------------------

def _apply_memory_config(
    profile_cfg: dict[str, Any],
    env_file: Path,
    memory_section: dict[str, Any],
) -> None:
    """PRD-001: Write memory backend config keys into profile_cfg dict."""
    provider = memory_section.get("provider", "none")
    if not provider or provider == "none":
        return

    profile_cfg["memory"] = {"provider": provider}

    if provider == "supermemory":
        sm = memory_section.get("supermemory", {})
        if sm.get("session_ingest"):
            profile_cfg["memory"]["session_ingest"] = True
            _upsert_env_line(env_file, "SUPERMEMORY_SESSION_INGEST", "1")

    elif provider == "honcho":
        honcho = memory_section.get("honcho", {})
        base_url = honcho.get("base_url", "http://localhost:8001")
        app_name = honcho.get("app_name", "tag")
        profile_cfg["memory"]["base_url"] = base_url
        profile_cfg["memory"]["app_name"] = app_name

    elif provider == "local":
        pass  # hermes-local-memory plugin picks up {"provider": "local"}


def _sanitize_env_value(value: str) -> str:
    """Strip characters that would break .env line format or enable injection."""
    return value.replace("\r\n", " ").replace("\n", " ").replace("\r", " ").replace("\x00", "")


def _upsert_env_line(env_file: Path, key: str, value: str) -> None:
    """Write or replace KEY=VALUE in an .env file without disturbing other lines."""
    value = _sanitize_env_value(value)
    env_file.parent.mkdir(parents=True, exist_ok=True)
    lines = env_file.read_text(encoding="utf-8").splitlines() if env_file.exists() else []
    prefix = f"{key}="
    new_line = f"{key}={value}"
    replaced = False
    out = []
    for line in lines:
        # Only replace the first *active* KEY= line. Commented/disabled keys stay
        # disabled and later duplicates are left as-is, so we never re-activate a
        # commented key or emit two active KEY= lines.
        if not replaced and line.strip().startswith(prefix):
            out.append(new_line)
            replaced = True
        else:
            out.append(line)
    if not replaced:
        out.append(new_line)
    env_file.write_text("\n".join(out) + "\n", encoding="utf-8")
    # Files created here hold API keys / tokens; keep them owner-only (0600).
    try:
        os.chmod(env_file, 0o600)
    except OSError:
        pass


def _config_profiles(cfg: dict[str, Any]) -> dict[str, Any]:
    """Return the ``profiles`` mapping, tolerating a present-but-null section.

    ``profiles:`` (null) behaves as an empty mapping; a scalar is a clear config
    error naming the key instead of a cryptic 'NoneType has no attribute items'.
    """
    profiles = cfg.get("profiles")
    if profiles is None:
        return {}
    if not isinstance(profiles, dict):
        raise SystemExit("Config field 'profiles' must be a YAML object.")
    return profiles


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


# ---------------------------------------------------------------------------
# Profile rendering and bootstrapping
# ---------------------------------------------------------------------------

def render_profiles(cfg: dict[str, Any], force: bool) -> list[dict[str, str]]:
    results: list[dict[str, str]] = []
    profiles = _config_profiles(cfg)
    for name, profile in profiles.items():
        home = profile_home(cfg, name)
        config_file = home / "config.yaml"
        env_file = home / ".env"
        env_example = home / ".env.example"

        # PRD-010: deep-merge with existing config to preserve panel edits
        existing: dict[str, Any] = {}
        if config_file.exists() and not force:
            try:
                existing = yaml.safe_load(config_file.read_text()) or {}
            except yaml.YAMLError:
                existing = {}

        profile_cfg = dict(profile.get("config", {}))

        # PRD-001: apply memory backend config
        memory_section = profile_cfg.pop("memory", None)
        if memory_section:
            _apply_memory_config(profile_cfg, env_file, memory_section)

        # PRD-001: apply gateway config
        gateway_section = profile_cfg.pop("gateway", None)
        if gateway_section and gateway_section.get("enabled"):
            profile_cfg["gateway"] = {"use_gateway": True}
            if tools := gateway_section.get("tools"):
                profile_cfg["gateway"]["allowed_tools"] = tools

        # PRD-005: apply execution backend config
        exec_section = profile_cfg.pop("execution", None)
        if exec_section:
            backend = exec_section.get("backend", "local")
            if backend != "local":
                exec_out: dict[str, Any] = {"backend": backend}
                if backend == "docker":
                    docker_cfg = exec_section.get("docker", {})
                    exec_out["docker"] = {
                        "image": docker_cfg.get("image", "ubuntu:22.04"),
                        "auto_pull": docker_cfg.get("auto_pull", True),
                    }
                    if volumes := docker_cfg.get("extra_volumes"):
                        exec_out["docker"]["extra_volumes"] = volumes
                elif backend == "ssh":
                    ssh_cfg = exec_section.get("ssh", {})
                    exec_out["ssh"] = {
                        "host": ssh_cfg.get("host", ""),
                        "user": ssh_cfg.get("user", ""),
                        "port": ssh_cfg.get("port", 22),
                        "key_file": str(Path(ssh_cfg.get("key_file", "~/.ssh/id_rsa")).expanduser()),
                        "remote_work_dir": ssh_cfg.get("remote_work_dir", "/tmp/tag-agent"),
                    }
                elif backend == "modal":
                    modal_cfg = exec_section.get("modal", {})
                    exec_out["modal"] = {
                        "app_name": modal_cfg.get("app_name", f"tag-{name}"),
                        "gpu": modal_cfg.get("gpu", ""),
                    }
                elif backend == "daytona":
                    daytona_cfg = exec_section.get("daytona", {})
                    exec_out["daytona"] = {
                        "workspace_id": daytona_cfg.get("workspace_id", ""),
                    }
                profile_cfg["execution"] = exec_out

        merged_cfg = _deep_merge(existing, profile_cfg)
        write_yaml(config_file, merged_cfg, force=True)
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
    profiles = _config_profiles(cfg)
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
            # Concurrent bootstrap (TOCTOU): another racer created the profile
            # between our home.exists() check above and this create. bootstrap
            # is documented-idempotent, so absorb the loser's failure instead of
            # aborting with a fatal SystemExit.
            if "already exists" in message.lower():
                created.append({"profile": name, "status": "existing"})
                continue
            raise SystemExit(f"Failed to create TAG-managed profile '{name}': {message}") from exc
        created.append({"profile": name, "status": "created"})
    return created


# ---------------------------------------------------------------------------
# Routing
# ---------------------------------------------------------------------------

def resolve_route(cfg: dict[str, Any], task_type: str, master_override: str | None, worker_override: list[str]) -> dict[str, Any]:
    defaults = cfg.get("defaults", {})
    routing = cfg.get("routing", {}).get("task_types", {})
    route = dict(routing.get(task_type, {}))
    if not route:
        available = ", ".join(sorted(routing))
        raise SystemExit(f"Unknown task type '{task_type}'. Available: {available}")

    master = master_override or defaults.get("master_profile")
    workers = worker_override or route.get("workers", [])
    # De-duplicate worker profiles (preserving order): a repeated --worker-profile
    # would otherwise spawn the same worker twice and double-insert DB steps.
    seen_workers: set[str] = set()
    workers = [w for w in workers if not (w in seen_workers or seen_workers.add(w))]
    verifier = route.get("verifier")

    profiles = _config_profiles(cfg)
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


# ---------------------------------------------------------------------------
# Model references
# ---------------------------------------------------------------------------

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
    profiles = _config_profiles(cfg)
    if profile_name not in profiles:
        available = ", ".join(sorted(profiles))
        raise SystemExit(f"Unknown profile '{profile_name}'. Available: {available}")


# ---------------------------------------------------------------------------
# Route model overrides
# ---------------------------------------------------------------------------

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
    matched: set[str] = set()
    for worker in route["workers"]:
        if worker["name"] not in overrides:
            continue
        provider, model = overrides[worker["name"]]
        worker["model"] = {"provider": provider, "default": model}
        matched.add(worker["name"])
    # Surface overrides that name a non-worker (typo detection) instead of
    # silently ignoring them.
    unknown = [name for name in overrides if name not in matched]
    if unknown:
        worker_names = ", ".join(w["name"] for w in route["workers"]) or "(none)"
        raise SystemExit(
            f"Worker override names a non-worker profile: {', '.join(sorted(unknown))}. "
            f"Route workers: {worker_names}."
        )
    return route


# ---------------------------------------------------------------------------
# Chat step execution
# ---------------------------------------------------------------------------

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


# ---------------------------------------------------------------------------
# Benchmark / eval utilities
# ---------------------------------------------------------------------------

def load_benchmark_suite(path: Path) -> list[dict[str, Any]]:
    try:
        payload = load_config(path)
    except SystemExit:
        raise FileNotFoundError(path)
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


# ---------------------------------------------------------------------------
# Kanban task helpers
# ---------------------------------------------------------------------------

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


# ---------------------------------------------------------------------------
# SQLite run / step helpers
# ---------------------------------------------------------------------------

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
