"""Miscellaneous utility functions for TAG CLI."""
from __future__ import annotations

import datetime as dt
import json
import os
import re
import sys
import textwrap
from pathlib import Path
from typing import Any, TextIO

import yaml


# ---------------------------------------------------------------------------
# Time helpers
# ---------------------------------------------------------------------------

def utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat()


# ---------------------------------------------------------------------------
# .env file helpers
# ---------------------------------------------------------------------------

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


def _sanitize_env_value(value: str) -> str:
    """Strip characters that would break .env line format or enable injection.

    Newlines would create additional KEY=VALUE entries; null bytes corrupt
    the file on some platforms. Strip both. Callers should validate further
    if they expect a specific format (e.g. URL, token).
    """
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
        stripped = line.strip()
        if stripped.startswith(prefix) or stripped.lstrip("# ").startswith(prefix):
            out.append(new_line)
            replaced = True
        else:
            out.append(line)
    if not replaced:
        out.append(new_line)
    env_file.write_text("\n".join(out) + "\n", encoding="utf-8")


# ---------------------------------------------------------------------------
# Profile / subprocess helpers
# ---------------------------------------------------------------------------

def run_profile_python(
    cfg: dict[str, Any],
    profile_name: str,
    inline: str,
    *,
    check: bool = True,
) -> "subprocess.CompletedProcess[str]":
    import subprocess
    from tag.core.paths import ensure_runtime_dirs, hermes_root, profile_exec_env
    ensure_runtime_dirs(cfg)
    return subprocess.run(
        [str(hermes_root(cfg) / ".venv" / "bin" / "python"), "-c", inline],
        env=profile_exec_env(cfg, profile_name),
        text=True,
        capture_output=True,
        check=check,
    )


# ---------------------------------------------------------------------------
# File-writing helpers
# ---------------------------------------------------------------------------

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


# ---------------------------------------------------------------------------
# String helpers
# ---------------------------------------------------------------------------

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
    from tag.core.paths import tag_cli_label
    label = tag_cli_label()

    def replace_inner(inner: str) -> str:
        return re.sub(r"\bhermes\b", label, inner, flags=re.IGNORECASE)

    # BUG-001: hermes auth/portal map to a DIFFERENT command shape (`tag runtime auth`,
    # not `tag auth` — which does not exist). These multi-word special-cases MUST run
    # before the generic code-span / subcommand substitutions below, otherwise the bare
    # `\bhermes\b`->label pass inside backticks rewrites `hermes auth` -> `tag auth` and
    # this special-case can no longer match.
    rewritten = re.sub(r"\bhermes auth\b", f"{label} runtime auth", text, flags=re.IGNORECASE)
    rewritten = re.sub(r"\bhermes portal\b", f"{label} runtime portal", rewritten, flags=re.IGNORECASE)
    # BUG-004: specific Title-Case product strings must run before the generic lowercase
    # `hermes <subcommand>` rule (which lists `status`/`config` and would emit "tag Status").
    rewritten = re.sub(r"\bHermes Configuration\b", "TAG Configuration", rewritten)
    rewritten = re.sub(r"\bHermes Status\b", "TAG Status", rewritten)
    rewritten = re.sub(r"\bHermes Runtime\b", "TAG Runtime", rewritten, flags=re.IGNORECASE)

    rewritten = re.sub(
        r"`([^`\n]*\bhermes\b[^`\n]*)`",
        lambda match: f"`{replace_inner(match.group(1))}`",
        rewritten,
        flags=re.IGNORECASE,
    )
    rewritten = re.sub(
        r"'([^'\n]*\bhermes\b[^'\n]*)'",
        lambda match: f"'{replace_inner(match.group(1))}'",
        rewritten,
        flags=re.IGNORECASE,
    )
    rewritten = re.sub(
        r"\bhermes (?=(config|model|setup|update|gateway|sessions|doctor|tools|status|plugins|skills|mcp|logs|memory|completion|prompt-size|chat|--resume|-c)\b)",
        f"{label} ",
        rewritten,
        flags=re.IGNORECASE,
    )
    rewritten = re.sub(r"\bHermes/tag\b", "TAG", rewritten, flags=re.IGNORECASE)
    rewritten = re.sub(r"\btag/tag\b", "tag", rewritten, flags=re.IGNORECASE)
    # BUG-004/BUG-005: version strings from the runtime binary
    rewritten = re.sub(r"\bHermes Agent\b", "TAG", rewritten, flags=re.IGNORECASE)
    # BUG-005: only rewrite bare "hermes-agent" (not "hermes-agent-upstream" dir names — they're real paths)
    rewritten = re.sub(r"\bhermes-agent(?!-upstream\b)", "tag-agent", rewritten, flags=re.IGNORECASE)
    rewritten = re.sub(r"\bthis Hermes profile\b", "this TAG profile", rewritten, flags=re.IGNORECASE)
    rewritten = re.sub(r"\bActive Hermes profile\b", "Active TAG profile", rewritten, flags=re.IGNORECASE)
    rewritten = re.sub(r"\bHermes profile\b", "TAG profile", rewritten, flags=re.IGNORECASE)
    rewritten = rewritten.replace("~/.hermes/.env", "the active TAG profile env file")
    # BUG-007/008/009: rewrite runtime internal paths to clean display form
    rewritten = re.sub(
        r'(?:/[^/\s]+)+/\.tag/runtime/home/\.hermes/profiles/',
        '~/.tag/runtime/profiles/',
        rewritten,
    )
    # BUG-005: rewrite managed hermes-agent-upstream dir to stable display path
    rewritten = re.sub(
        r'(?:/[^/\s]+)+/\.tag/managed/hermes-agent-upstream',
        '~/.tag/managed/runtime',
        rewritten,
    )
    # BUG-019: tilde-shorten any remaining absolute ~/.tag/runtime/home paths
    rewritten = re.sub(
        r'(?:/[^/\s]+)+/\.tag/runtime/home/',
        '~/.tag/runtime/home/',
        rewritten,
    )
    # BUG-010: bare ~/.hermes path shown in `tag profile`
    rewritten = rewritten.replace("~/.hermes", "~/.tag/profiles")
    # (Hermes Configuration/Status/Runtime title strings are rewritten earlier, before the
    # generic subcommand rule, so casing stays consistent — see BUG-004 block above.)
    # BUG-011: re-centre box titles after brand substitution shortened them
    rewritten = _fix_box_title_alignment(rewritten)
    # Catch any remaining standalone Hermes brand references that aren't filesystem paths
    rewritten = re.sub(r"(?<![/.])\bHermes\b(?![-/.])", "TAG", rewritten)
    return rewritten


_BOX_TITLE_RE = re.compile(r"┌(─+)┐\n│([^\n]*)│\n└(─+)┘", re.MULTILINE)


def _fix_box_title_alignment(text: str) -> str:
    """BUG-011: Re-centre box titles that were shortened by brand substitution.

    Brand substitution (e.g. ``Hermes``->``TAG``) shrinks the title text without
    re-padding the box, so the trailing ``│`` is pulled left and the frame breaks.
    Re-centre any title line whose width no longer matches the border — not just the
    ``⚕``-icon panels (BUG-003: non-icon titles like "TAG Status" were left broken).
    """
    def recentre(m: re.Match) -> str:
        top_dashes, content, bot_dashes = m.group(1), m.group(2), m.group(3)
        inner_width = len(top_dashes)
        # For ⚕ panels, drop any leading padding and re-centre from the icon onward;
        # for plain titles, re-centre the stripped title text.
        if "⚕" in content:
            title_part = content[content.index("⚕"):].strip()
        else:
            title_part = content.strip()
        # Leave well-formed lines untouched; never widen a title that overflows the box.
        if len(content) == inner_width or len(title_part) > inner_width:
            return m.group(0)
        centred = title_part.center(inner_width)
        return f"┌{top_dashes}┐\n│{centred}│\n└{bot_dashes}┘"
    return _BOX_TITLE_RE.sub(recentre, text)


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


# ---------------------------------------------------------------------------
# Config / profile helpers
# ---------------------------------------------------------------------------

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
    from tag.core.paths import resource_path, resolve_home_relative
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
    import argparse
    parsed = int(value)
    if parsed < 0:
        raise argparse.ArgumentTypeError("value must be >= 0")
    return parsed


def positive_int(value: str) -> int:
    import argparse
    parsed = int(value)
    if parsed <= 0:
        raise argparse.ArgumentTypeError("value must be > 0")
    return parsed


def install_profile_skins(cfg: dict[str, Any], profile_name: str, force: bool) -> list[str]:
    import shutil
    from tag.core.paths import profile_home
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


# ---------------------------------------------------------------------------
# Deep merge / memory config
# ---------------------------------------------------------------------------

def _deep_merge(base: dict[str, Any], override: dict[str, Any]) -> dict[str, Any]:
    """Recursively merge override into base. Used by render_profiles (PRD-010)."""
    result = dict(base)
    for k, v in override.items():
        if k in result and isinstance(result[k], dict) and isinstance(v, dict):
            result[k] = _deep_merge(result[k], v)
        else:
            result[k] = v
    return result


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
