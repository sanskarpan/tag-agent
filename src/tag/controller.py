#!/usr/bin/env python3
"""TAG control-plane CLI — thin dispatcher."""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import sqlite3
import subprocess
import sys
import tarfile
import urllib.error
import urllib.request
from pathlib import Path, PurePosixPath
from typing import Any

import yaml

try:
    from tag import __version__
except Exception:  # pragma: no cover — fallback for direct file loading in tests
    __version__ = "0.1.0"


try:
    from tag.tui_output import (
        chat_spinner,
        get_console,
        make_benchmark_progress,
        make_submit_progress,
        print_doctor_report,
        print_error,
        print_success,
        print_warning,
        send_desktop_notification,
    )
    _TUI_OUTPUT_AVAILABLE = True
except Exception:  # pragma: no cover — tui_output not importable in all test environments
    _TUI_OUTPUT_AVAILABLE = False

    def get_console():  # type: ignore[misc]
        return None

    def print_error(msg: str) -> None:  # type: ignore[misc]
        print(f"error: {msg}", file=sys.stderr)

    def print_success(msg: str) -> None:  # type: ignore[misc]
        print(msg)

    def print_warning(msg: str) -> None:  # type: ignore[misc]
        print(f"warning: {msg}", file=sys.stderr)

    def print_doctor_report(groups: dict) -> None:  # type: ignore[misc]
        for group, checks in groups.items():
            print(f"\n{group.upper()}")
            for c in checks:
                st = c.get("status", "pass")
                icon = {"pass": "✓", "warn": "⚠", "fail": "✗"}.get(st, "?")
                print(f"  {icon} {c.get('name', '?'):<28} {c.get('message', '')}")

    def send_desktop_notification(title: str, message: str) -> None:  # type: ignore[misc]
        pass

    def chat_spinner(*a, **kw):  # type: ignore[misc]
        import contextlib
        return contextlib.nullcontext()

    def make_benchmark_progress():  # type: ignore[misc]
        return None

    def make_submit_progress():  # type: ignore[misc]
        return None



# ---------------------------------------------------------------------------
# Backward-compat re-exports — test_controller.py loads this file directly
# via importlib and accesses everything as TAG.<name>.
# ---------------------------------------------------------------------------

from tag.core.config import load_config, save_config, config_path, benchmark_suite_path  # noqa: E402
from tag.core.paths import (  # noqa: E402
    package_root, resource_path, bundled_hermes_archive,
    tag_home, managed_root, hermes_root, hermes_bin,
    resolve_home_relative, ensure_default_file,
    is_hermes_checkout, hermes_checkout_kind, discover_local_hermes_checkout,
    python_runtime_supported, config_root,
    runtime_home, runtime_codex_home, runtime_db_path,
    hermes_repo_url, hermes_ref, hermes_env, profile_home, profile_exec_env,
    ensure_runtime_dirs, tag_cli_label, tag_cli_bin,
    can_launch_interactive_tui,
    DEFAULT_TAG_HOME, DEFAULT_HERMES_CHECKOUT, MIN_PYTHON, MAX_PYTHON_EXCLUSIVE,
    APP_NAME, CLI_LABEL,
)
from tag.core.db import (  # noqa: E402
    open_db, journal_save, journal_list, journal_forget, journal_clear,
    journal_to_prompt_prefix, queue_insert_job, queue_update_pid,
    queue_update_status, queue_get_job, queue_list_jobs, queue_clear_completed,
    launch_queue_worker,
)
from tag.core.paths import is_tty  # noqa: E402 — is_tty lives in paths, not utils
from tag.core.utils import (  # noqa: E402
    utc_now, nonnegative_int, positive_int, slugify, normalize_chat_output,
    rewrite_cli_hints, strip_json_fences,
    merged_env_example, configured_skins, install_profile_skins, _deep_merge,
    write_yaml, write_text, read_dotenv, _sanitize_env_value, _upsert_env_line,
    _fix_box_title_alignment, infrastructure_failure_reason,
)
from tag.core.profile import (  # noqa: E402
    render_profiles, bootstrap_profiles, resolve_route, parse_model_ref,
    format_model_ref, collect_assignments, load_model_inventory,
    load_openrouter_catalog, ensure_profile_exists, apply_route_model_overrides,
    run_chat_step, load_benchmark_suite, case_passed, show_kanban_task,
    create_temp_profile, insert_run, update_run_status, insert_step,
)
from tag.core.run import run_hermes, run_profile_hermes, run_profile_python  # noqa: E402

# ---------------------------------------------------------------------------
# run_chat_step wrapper — overrides the core.profile version so that tests
# can monkeypatch TAG.run_profile_hermes and have run_chat_step honour it.
# (The original monolithic controller.py had both in the same module.)
# ---------------------------------------------------------------------------

import datetime as _dt  # noqa: E402


def run_chat_step(  # type: ignore[misc]
    cfg: dict[str, Any],
    *,
    profile_name: str,
    prompt: str,
) -> dict[str, Any]:
    """Controller-level run_chat_step that uses the module-level run_profile_hermes.

    This wrapper allows tests to monkeypatch TAG.run_profile_hermes and have
    TAG.run_chat_step honour the patch (since both live in the same globals()).
    """
    started = _dt.datetime.now(_dt.timezone.utc)
    # 'run_profile_hermes' is looked up in globals() at call time, so
    # monkeypatch.setattr(TAG, "run_profile_hermes", ...) takes effect.
    proc = run_profile_hermes(cfg, profile_name, "chat", "-q", prompt, "-Q", check=False)
    finished = _dt.datetime.now(_dt.timezone.utc)
    output = proc.stdout.strip() if hasattr(proc, "stdout") else ""
    if hasattr(proc, "stderr") and (proc.stderr or "").strip():
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
        "model_ref": f"{model_cfg.get('provider','')}/{model_cfg.get('default','')}".strip("/"),
        **({"failure_reason": failure_reason} if failure_reason else {}),
    }


# ---------------------------------------------------------------------------
# Local helper functions — kept here because cmd/* modules do deferred
# "from tag.controller import <these>" to avoid circular imports.
# ---------------------------------------------------------------------------


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


def setup_python_bin(cfg: dict[str, Any]) -> Path:
    return hermes_root(cfg) / ".venv" / "bin" / "python"


def hermes_patch_path() -> Path:
    return resource_path("patches", "hermes-ui.patch")


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
        # TAG-branded aliases (hermes_* kept for internal compat)
        "tag_runtime_exists": root.exists(),
        "tag_runtime_kind": hermes_checkout_kind(root),
        "tag_python_exists": python_bin.exists(),
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


def safe_extract_tar_gz(archive: Path, target: Path) -> None:
    target_real = target.resolve()
    try:
        with tarfile.open(archive, "r:gz") as tf:
            members = tf.getmembers()
            for member in members:
                member_name = member.name
                pure = PurePosixPath(member_name)
                if pure.is_absolute() or ".." in pure.parts:
                    raise SystemExit(f"TAG runtime archive contains an unsafe entry: {member_name}")
                if member.issym() or member.islnk():
                    raise SystemExit(f"TAG runtime archive contains an unsupported link entry: {member_name}")
                if not (member.isdir() or member.isfile()):
                    raise SystemExit(f"TAG runtime archive contains an unsupported entry type: {member_name}")
                dest = (target / member_name).resolve()
                if target_real != dest and target_real not in dest.parents:
                    raise SystemExit(f"TAG runtime archive contains a path traversal entry: {member_name}")
            for member in members:
                tf.extract(member, target)
    except (tarfile.TarError, OSError) as exc:
        raise SystemExit(f"TAG runtime archive could not be read: {archive}") from exc


def extract_bundled_hermes(root: Path) -> dict[str, Any]:
    archive = bundled_hermes_archive()
    if not archive.exists():
        raise SystemExit("TAG runtime bundle is not available in this build.")
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


# ---------------------------------------------------------------------------
# Codex credential import
# ---------------------------------------------------------------------------


def import_codex_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_codex_home: Path,
) -> dict[str, Any]:
    import textwrap
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


# ---------------------------------------------------------------------------
# Claude Code credential import
# ---------------------------------------------------------------------------


def _detect_claude_code_credentials(
    source_home: Path | None = None,
) -> dict[str, Any]:
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


# ---------------------------------------------------------------------------
# Gemini CLI credential import
# ---------------------------------------------------------------------------


def _detect_gemini_credentials(
    source_home: Path | None = None,
) -> dict[str, Any]:
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


# ---------------------------------------------------------------------------
# Continue.dev credential import
# ---------------------------------------------------------------------------

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
    keys = _detect_continue_credentials()
    if not keys:
        return [{"profile": p, "status": "skipped-no-auth"} for p in cfg.get("profiles", {})]
    return [import_continue_into_profile(cfg, profile_name=p) for p in cfg.get("profiles", {})]


# ---------------------------------------------------------------------------
# Mistral Vibe credential import
# ---------------------------------------------------------------------------


def _detect_mistral_credentials(
    source_home: Path | None = None,
) -> dict[str, Any]:
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
    creds = _detect_mistral_credentials()
    if not creds["api_key"]:
        return [{"profile": p, "status": "skipped-no-auth"} for p in cfg.get("profiles", {})]
    return [import_mistral_into_profile(cfg, profile_name=p) for p in cfg.get("profiles", {})]


# ---------------------------------------------------------------------------
# opencode credential import
# ---------------------------------------------------------------------------

_OPENCODE_PROVIDER_ENV_MAP: dict[str, str] = {
    "anthropic": "ANTHROPIC_API_KEY",
    "openai": "OPENAI_API_KEY",
    "google": "GEMINI_API_KEY",
    "google-vertex-ai": "GEMINI_API_KEY",
    "groq": "GROQ_API_KEY",
    "deepseek": "DEEPSEEK_API_KEY",
    "openrouter": "OPENROUTER_API_KEY",
    "xai": "XAI_API_KEY",
    "mistral": "MISTRAL_API_KEY",
    "fireworks": "FIREWORKS_API_KEY",
    "together": "TOGETHER_API_KEY",
    "perplexity": "PERPLEXITY_API_KEY",
    "cohere": "COHERE_API_KEY",
    "nvidia": "NVIDIA_API_KEY",
    "github": "GITHUB_TOKEN",
}


def _detect_opencode_credentials(
    source_data_dir: Path | None = None,
) -> dict[str, str]:
    data_dir = source_data_dir or (Path.home() / ".local" / "share" / "opencode")
    auth_file = data_dir / "auth.json"
    found: dict[str, str] = {}
    if not auth_file.exists():
        return found
    try:
        data = json.loads(auth_file.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError):
        return found
    for provider_id, cred in data.items():
        if not isinstance(cred, dict) or cred.get("type") != "api":
            continue
        key = (cred.get("key") or "").strip()
        env_var = _OPENCODE_PROVIDER_ENV_MAP.get(provider_id.lower())
        if key and env_var and env_var not in found:
            found[env_var] = key
    return found


def import_opencode_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_data_dir: Path | None = None,
) -> dict[str, Any]:
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}
    keys = _detect_opencode_credentials(source_data_dir)
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


def auto_import_opencode_profiles(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    keys = _detect_opencode_credentials()
    if not keys:
        return [{"profile": p, "status": "skipped-no-auth"} for p in cfg.get("profiles", {})]
    return [import_opencode_into_profile(cfg, profile_name=p) for p in cfg.get("profiles", {})]


# ---------------------------------------------------------------------------
# Zed editor credential import
# ---------------------------------------------------------------------------


def _strip_jsonc(text: str) -> str:
    """Remove ``//`` and ``/* */`` comments and trailing commas from JSONC text.

    Real Zed ``settings.json`` files are JSONC (JSON with comments/trailing
    commas). Comment/string handling is character-by-character so that ``//`` or
    ``/*`` sequences inside string literals are preserved verbatim.
    """
    out: list[str] = []
    i = 0
    n = len(text)
    in_string = False
    while i < n:
        ch = text[i]
        if in_string:
            out.append(ch)
            if ch == "\\" and i + 1 < n:  # keep escaped char intact
                out.append(text[i + 1])
                i += 2
                continue
            if ch == '"':
                in_string = False
            i += 1
            continue
        if ch == '"':
            in_string = True
            out.append(ch)
            i += 1
            continue
        if ch == "/" and i + 1 < n and text[i + 1] == "/":
            i += 2
            while i < n and text[i] not in "\r\n":
                i += 1
            continue
        if ch == "/" and i + 1 < n and text[i + 1] == "*":
            i += 2
            while i + 1 < n and not (text[i] == "*" and text[i + 1] == "/"):
                i += 1
            i += 2
            continue
        out.append(ch)
        i += 1
    stripped = "".join(out)
    # Drop trailing commas before a closing } or ] (allowed in JSONC, not JSON).
    stripped = re.sub(r",(\s*[}\]])", r"\1", stripped)
    return stripped


def _load_jsonc(text: str) -> dict[str, Any]:
    """Parse JSONC text into a dict, tolerating comments and trailing commas."""
    try:
        data = json.loads(text)
    except json.JSONDecodeError:
        data = json.loads(_strip_jsonc(text))
    return data if isinstance(data, dict) else {}


def _detect_zed_credentials(
    source_zed_config: Path | None = None,
) -> dict[str, str]:
    zed_settings = source_zed_config or (Path.home() / ".config" / "zed" / "settings.json")
    found: dict[str, str] = {}

    zed_provider_env_map = {
        "openai": "OPENAI_API_KEY",
        "anthropic": "ANTHROPIC_API_KEY",
        "google": "GEMINI_API_KEY",
        "mistral": "MISTRAL_API_KEY",
        "deepseek": "DEEPSEEK_API_KEY",
        "xai": "XAI_API_KEY",
        "groq": "GROQ_API_KEY",
        "ollama": None,
    }

    if zed_settings.exists():
        try:
            data = _load_jsonc(zed_settings.read_text(encoding="utf-8"))
            lm = data.get("language_models") or {}
            for provider, cfg_block in lm.items():
                if not isinstance(cfg_block, dict):
                    continue
                key = (cfg_block.get("api_key") or "").strip()
                env_var = zed_provider_env_map.get(provider.lower())
                if key and env_var and env_var not in found:
                    found[env_var] = key
        except (json.JSONDecodeError, OSError):
            pass

    return found


def import_zed_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_zed_config: Path | None = None,
) -> dict[str, Any]:
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}
    keys = _detect_zed_credentials(source_zed_config)
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


def auto_import_zed_profiles(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    keys = _detect_zed_credentials()
    if not keys:
        return [{"profile": p, "status": "skipped-no-auth"} for p in cfg.get("profiles", {})]
    return [import_zed_into_profile(cfg, profile_name=p) for p in cfg.get("profiles", {})]


# ---------------------------------------------------------------------------
# GitHub Copilot credential import
# ---------------------------------------------------------------------------


def _detect_copilot_credentials(
    source_gh_config: Path | None = None,
) -> dict[str, Any]:
    result: dict[str, Any] = {"github_token": None, "source": None}

    gh_token = os.environ.get("GITHUB_TOKEN", "").strip() or os.environ.get("GH_TOKEN", "").strip()
    if gh_token:
        result["github_token"] = gh_token
        return result

    hosts_file = source_gh_config or (Path.home() / ".config" / "gh" / "hosts.yml")
    if hosts_file.exists():
        try:
            data = yaml.safe_load(hosts_file.read_text(encoding="utf-8")) or {}
            token = (
                (data.get("github.com") or {}).get("oauth_token", "")
                or (data.get("github.com") or {}).get("token", "")
            ).strip()
            if token:
                result["github_token"] = token
                result["source"] = str(hosts_file)
        except Exception:
            pass

    return result


def import_copilot_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_gh_config: Path | None = None,
) -> dict[str, Any]:
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}
    creds = _detect_copilot_credentials(source_gh_config)
    if not creds["github_token"]:
        return {"profile": profile_name, "status": "skipped-no-auth"}
    _upsert_env_line(target_home / ".env", "GITHUB_TOKEN", creds["github_token"])
    return {
        "profile": profile_name,
        "status": "imported",
        "mode": "oauth_token",
        "provider": "github-copilot",
        "source": creds.get("source"),
    }


def auto_import_copilot_profiles(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    creds = _detect_copilot_credentials()
    if not creds["github_token"]:
        return [{"profile": p, "status": "skipped-no-auth"} for p in cfg.get("profiles", {})]
    return [import_copilot_into_profile(cfg, profile_name=p) for p in cfg.get("profiles", {})]


# ---------------------------------------------------------------------------
# Aider credential import
# ---------------------------------------------------------------------------

_AIDER_YAML_KEY_MAP: dict[str, str] = {
    "openai-api-key": "OPENAI_API_KEY",
    "anthropic-api-key": "ANTHROPIC_API_KEY",
    "gemini-api-key": "GEMINI_API_KEY",
    "deepseek-api-key": "DEEPSEEK_API_KEY",
    "openrouter-api-key": "OPENROUTER_API_KEY",
    "mistral-api-key": "MISTRAL_API_KEY",
    "groq-api-key": "GROQ_API_KEY",
    "xai-api-key": "XAI_API_KEY",
    "cohere-api-key": "COHERE_API_KEY",
    "perplexity-api-key": "PERPLEXITY_API_KEY",
}

_AIDER_API_KEY_PREFIX_MAP: dict[str, str] = {
    "anthropic": "ANTHROPIC_API_KEY",
    "gemini": "GEMINI_API_KEY",
    "openrouter": "OPENROUTER_API_KEY",
    "mistral": "MISTRAL_API_KEY",
    "groq": "GROQ_API_KEY",
    "deepseek": "DEEPSEEK_API_KEY",
    "xai": "XAI_API_KEY",
    "cohere": "COHERE_API_KEY",
    "perplexity": "PERPLEXITY_API_KEY",
    "together": "TOGETHER_API_KEY",
    "fireworks": "FIREWORKS_API_KEY",
}


def _detect_aider_credentials(
    source_home: Path | None = None,
) -> dict[str, str]:
    base = source_home or Path.home()
    found: dict[str, str] = {}

    aider_yaml = base / ".aider.conf.yml"
    if aider_yaml.exists():
        try:
            data = yaml.safe_load(aider_yaml.read_text(encoding="utf-8")) or {}
            for yaml_key, env_var in _AIDER_YAML_KEY_MAP.items():
                val = (str(data.get(yaml_key) or "")).strip()
                if val and env_var not in found:
                    found[env_var] = val
            api_key_list = data.get("api-key") or []
            if isinstance(api_key_list, list):
                for entry in api_key_list:
                    entry = (str(entry) or "").strip()
                    if "=" in entry:
                        prefix, _, val = entry.partition("=")
                        env_var = _AIDER_API_KEY_PREFIX_MAP.get(prefix.strip().lower())
                        if val.strip() and env_var and env_var not in found:
                            found[env_var] = val.strip()
        except Exception:
            pass

    for dotenv_path in (base / ".env", base / ".aider.env"):
        if dotenv_path.exists():
            for env_var in (
                "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY",
                "OPENROUTER_API_KEY", "MISTRAL_API_KEY", "GROQ_API_KEY",
                "DEEPSEEK_API_KEY", "XAI_API_KEY", "PERPLEXITY_API_KEY",
                "COHERE_API_KEY", "TOGETHER_API_KEY", "FIREWORKS_API_KEY",
            ):
                val = read_dotenv(dotenv_path).get(env_var, "").strip()
                if val and env_var not in found:
                    found[env_var] = val

    return found


def import_aider_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_home: Path | None = None,
) -> dict[str, Any]:
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}
    keys = _detect_aider_credentials(source_home)
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


def auto_import_aider_profiles(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    keys = _detect_aider_credentials()
    if not keys:
        return [{"profile": p, "status": "skipped-no-auth"} for p in cfg.get("profiles", {})]
    return [import_aider_into_profile(cfg, profile_name=p) for p in cfg.get("profiles", {})]


# ---------------------------------------------------------------------------
# AWS / Amazon Bedrock credential import
# ---------------------------------------------------------------------------


def _detect_aws_credentials(
    source_aws_dir: Path | None = None,
) -> dict[str, Any]:
    result: dict[str, Any] = {
        "access_key_id": None,
        "secret_access_key": None,
        "session_token": None,
        "region": None,
        "source": None,
    }

    result["access_key_id"] = os.environ.get("AWS_ACCESS_KEY_ID", "").strip() or None
    result["secret_access_key"] = os.environ.get("AWS_SECRET_ACCESS_KEY", "").strip() or None
    result["session_token"] = os.environ.get("AWS_SESSION_TOKEN", "").strip() or None
    result["region"] = os.environ.get("AWS_DEFAULT_REGION", "").strip() or None

    aws_dir = source_aws_dir or (Path.home() / ".aws")
    credentials_file = aws_dir / "credentials"
    config_file = aws_dir / "config"

    def _read_ini_section(path: Path, section: str) -> dict[str, str]:
        try:
            import configparser
            cp = configparser.ConfigParser()
            cp.read(str(path))
            if cp.has_section(section):
                return dict(cp[section])
        except Exception:
            pass
        return {}

    if credentials_file.exists() and not result["access_key_id"]:
        creds = _read_ini_section(credentials_file, "default")
        result["access_key_id"] = creds.get("aws_access_key_id", "").strip() or None
        result["secret_access_key"] = creds.get("aws_secret_access_key", "").strip() or None
        result["session_token"] = creds.get("aws_session_token", "").strip() or None
        if result["access_key_id"]:
            result["source"] = str(credentials_file)

    if config_file.exists() and not result["region"]:
        cfg_data = _read_ini_section(config_file, "default")
        result["region"] = cfg_data.get("region", "").strip() or None

    return result


def import_aws_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_aws_dir: Path | None = None,
    aws_profile: str = "default",
) -> dict[str, Any]:
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}

    creds = _detect_aws_credentials(source_aws_dir)
    if not creds["access_key_id"] or not creds["secret_access_key"]:
        return {"profile": profile_name, "status": "skipped-no-auth"}

    env_file = target_home / ".env"
    _upsert_env_line(env_file, "AWS_ACCESS_KEY_ID", creds["access_key_id"])
    _upsert_env_line(env_file, "AWS_SECRET_ACCESS_KEY", creds["secret_access_key"])
    if creds["session_token"]:
        _upsert_env_line(env_file, "AWS_SESSION_TOKEN", creds["session_token"])
    if creds["region"]:
        _upsert_env_line(env_file, "AWS_DEFAULT_REGION", creds["region"])

    return {
        "profile": profile_name,
        "status": "imported",
        "mode": "access_key",
        "provider": "aws-bedrock",
        "source": creds.get("source"),
    }


def auto_import_aws_profiles(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    creds = _detect_aws_credentials()
    if not creds["access_key_id"] or not creds["secret_access_key"]:
        return [{"profile": p, "status": "skipped-no-auth"} for p in cfg.get("profiles", {})]
    return [import_aws_into_profile(cfg, profile_name=p) for p in cfg.get("profiles", {})]


# ---------------------------------------------------------------------------
# Cursor IDE credential import
# ---------------------------------------------------------------------------


def _detect_cursor_credentials(
    source_cursor_dir: Path | None = None,
) -> dict[str, str]:
    found: dict[str, str] = {}

    if source_cursor_dir:
        db_candidates = [source_cursor_dir / "state.vscdb"]
    else:
        db_candidates = [
            Path.home() / "Library" / "Application Support" / "Cursor" / "User" / "globalStorage" / "state.vscdb",
            Path.home() / ".config" / "Cursor" / "User" / "globalStorage" / "state.vscdb",
        ]

    db_path = next((p for p in db_candidates if p.exists()), None)
    if not db_path:
        return found

    known_key_map = {
        "openai.apiKey": "OPENAI_API_KEY",
        "cursor.openaiApiKey": "OPENAI_API_KEY",
        "anthropic.apiKey": "ANTHROPIC_API_KEY",
        "cursor.anthropicApiKey": "ANTHROPIC_API_KEY",
        "gemini.apiKey": "GEMINI_API_KEY",
        "cursor.googleApiKey": "GEMINI_API_KEY",
    }

    api_key_value_patterns: list[tuple[str, str]] = [
        ("sk-ant-", "ANTHROPIC_API_KEY"),
        ("sk-or-", "OPENROUTER_API_KEY"),
        ("AIza", "GEMINI_API_KEY"),
    ]

    try:
        conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
        try:
            rows = conn.execute("SELECT key, value FROM ItemTable").fetchall()
        finally:
            conn.close()
    except Exception:
        return found

    for db_key, db_value in rows:
        if not db_value or not isinstance(db_value, str):
            continue
        value = db_value.strip()
        env_var = known_key_map.get(db_key)
        if env_var and value and env_var not in found:
            found[env_var] = value
            continue
        for prefix, env_var in api_key_value_patterns:
            if value.startswith(prefix) and env_var not in found:
                found[env_var] = value
                break

    return found


def import_cursor_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_cursor_dir: Path | None = None,
) -> dict[str, Any]:
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}
    keys = _detect_cursor_credentials(source_cursor_dir)
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


def auto_import_cursor_profiles(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    keys = _detect_cursor_credentials()
    if not keys:
        return [{"profile": p, "status": "skipped-no-auth"} for p in cfg.get("profiles", {})]
    return [import_cursor_into_profile(cfg, profile_name=p) for p in cfg.get("profiles", {})]


# ---------------------------------------------------------------------------
# ensure_hermes_ready + normalize_hermes_passthrough_args
# (cmd/session.py and cmd/system.py do deferred "from tag.controller import"
# for these so they must live here)
# ---------------------------------------------------------------------------


def normalize_hermes_passthrough_args(args: list[str]) -> list[str]:
    normalized = list(args)
    if normalized[:1] == ["--"]:
        normalized = normalized[1:]
    if len(normalized) >= 2 and normalized[1] == "--":
        normalized = [normalized[0], *normalized[2:]]
    if not normalized:
        return ["--help"]
    return normalized


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


# ---------------------------------------------------------------------------
# Doctor helper functions — used by cmd_doctor in system.py (via deferred
# "from tag.controller import _doctor_*") and accessed directly by tests.
# ---------------------------------------------------------------------------


def _doctor_system_checks(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    """Return pass/warn/fail checks for the host system."""
    prereqs = doctor_prerequisites(cfg)
    checks: list[dict[str, Any]] = []

    py_ver = sys.version_info[:2]
    py_ok = python_runtime_supported(py_ver)
    checks.append({
        "name": "python_version",
        "status": "pass" if py_ok else "fail",
        "message": f"{sys.version.split()[0]} ({'supported' if py_ok else 'unsupported — need 3.11–3.13'})",
    })

    git_info = prereqs.get("git", {})
    checks.append({
        "name": "git",
        "status": "pass" if git_info.get("found") else "warn",
        "message": git_info.get("version", "not found"),
    })

    npm_info = prereqs.get("npm", {})
    checks.append({
        "name": "npm",
        "status": "pass" if npm_info.get("found") else "warn",
        "message": npm_info.get("version", "not found"),
    })

    return checks


def _doctor_hermes_checks(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    """Return pass/warn/fail checks for the managed runtime."""
    prereqs = doctor_prerequisites(cfg)
    checks: list[dict[str, Any]] = []

    bin_exists = hermes_bin(cfg).exists()
    checks.append({
        "name": "runtime_binary",
        "status": "pass" if bin_exists else "fail",
        "message": str(hermes_bin(cfg)) if bin_exists else "not provisioned — run `tag setup`",
    })

    ps = prereqs.get("patch_status", "")
    patch_ok = ps in ("applied", "prepatched")
    checks.append({
        "name": "patch_applied",
        "status": "pass" if patch_ok else ("warn" if ps == "diverged" else "fail"),
        "message": ps or "unknown",
    })

    tui_ok = prereqs.get("tui_dist_exists", False)
    checks.append({
        "name": "tui_dist",
        "status": "pass" if tui_ok else "warn",
        "message": "built" if tui_ok else "not built — run `tag setup`",
    })

    return checks


def _doctor_profile_checks(cfg: dict[str, Any], profile_name: str) -> list[dict[str, Any]]:
    """Return pass/warn/fail checks for a single TAG profile."""
    checks: list[dict[str, Any]] = []
    home = profile_home(cfg, profile_name)

    home_ok = home.exists() and (home / "config.yaml").exists()
    checks.append({
        "name": "home",
        "status": "pass" if home_ok else "fail",
        "message": str(home) if home_ok else f"missing — run `tag bootstrap` ({home})",
    })

    if home_ok:
        env_file = home / ".env"
        env_vals = read_dotenv(env_file) if env_file.exists() else {}
        api_key_vars = [
            k for k in env_vals
            if (k.endswith("_API_KEY") or k.endswith("_TOKEN"))
            and str(env_vals[k]).strip()
        ]
        checks.append({
            "name": "OPENROUTER_API_KEY" if not api_key_vars else api_key_vars[0],
            "status": "pass" if api_key_vars else "warn",
            "message": "found" if api_key_vars else "no API key configured in profile .env",
        })

    # Check execution backend requirements
    profile_cfg = cfg.get("profiles", {}).get(profile_name, {})
    exec_cfg = profile_cfg.get("config", {}).get("execution", {})
    backend = exec_cfg.get("backend", "local")

    if backend == "docker":
        docker_found = bool(shutil.which("docker"))
        checks.append({
            "name": "docker_daemon",
            "status": "pass" if docker_found else "warn",
            "message": "docker found" if docker_found else "docker not found — required for docker backend",
        })
    elif backend == "ssh":
        ssh_host = exec_cfg.get("ssh", {}).get("host", "")
        checks.append({
            "name": "ssh_host",
            "status": "pass" if ssh_host else "warn",
            "message": ssh_host if ssh_host else "no SSH host configured",
        })
    elif backend == "modal":
        modal_key = os.environ.get("MODAL_TOKEN_ID", "")
        checks.append({
            "name": "modal_credentials",
            "status": "pass" if modal_key else "warn",
            "message": "MODAL_TOKEN_ID found" if modal_key else "MODAL_TOKEN_ID not set",
        })

    return checks


# ---------------------------------------------------------------------------
# Cost estimation helper — not in any cmd/ file; accessed directly by tests.
# ---------------------------------------------------------------------------

# Pricing table in USD per 1M tokens: {model_key: (input_per_1m, output_per_1m)}
_MODEL_PRICING: dict[str, tuple[float, float]] = {
    "openai/gpt-4o":              (5.00, 15.00),
    "openai/gpt-4o-mini":         (0.15,  0.60),
    "openai/gpt-4-turbo":        (10.00, 30.00),
    "openai/gpt-4":              (30.00, 60.00),
    "openai/gpt-3.5-turbo":       (0.50,  1.50),
    "anthropic/claude-opus-4-8":  (5.00, 25.00),
    "anthropic/claude-sonnet-4-6":(3.00, 15.00),
    "anthropic/claude-haiku-4-5": (1.00,  5.00),
    "google/gemini-1.5-pro":      (3.50, 10.50),
    "google/gemini-1.5-flash":    (0.35,  1.05),
    "meta-llama/llama-3.1-70b-instruct": (0.52, 0.75),
    "deepseek/deepseek-chat":     (0.14,  0.28),
}
_DEFAULT_PRICING = (1.00, 3.00)


def _estimate_cost(input_tokens: int, output_tokens: int, model_ref: str) -> float:
    """Estimate USD cost from token counts and model ref string."""
    if input_tokens == 0 and output_tokens == 0:
        return 0.0
    # Normalise: strip openrouter/ prefix that some refs carry
    key = model_ref.removeprefix("openrouter/")
    input_price, output_price = _MODEL_PRICING.get(key, _DEFAULT_PRICING)
    return (input_tokens * input_price + output_tokens * output_price) / 1_000_000.0


# ---------------------------------------------------------------------------
# Desktop app helpers — not in any cmd/ file; accessed directly by tests.
# ---------------------------------------------------------------------------


def desktop_build_root(cfg: dict[str, Any]) -> "Path":
    """Return the path where the TAG desktop Electron app is built."""
    return tag_home() / "desktop"


def desktop_app_path(cfg: dict[str, Any]) -> "Path | None":
    """Return the path to the built desktop app binary, or None if not built."""
    import platform
    build_dir = desktop_build_root(cfg) / "build"
    system = platform.system()
    if system == "Darwin":
        candidate = build_dir / "Hermes.app" / "Contents" / "MacOS" / "Hermes"
    elif system == "Linux":
        candidate = build_dir / "linux-unpacked" / "hermes"
    elif system == "Windows":
        candidate = build_dir / "win-unpacked" / "Hermes.exe"
    else:
        return None
    return candidate if candidate.exists() else None


# ---------------------------------------------------------------------------
# Lazy re-exports of cmd_* functions.
# cmd/* modules import from tag.controller at their module level, so we
# cannot import them here at controller module level without a circular dep.
# Instead, __getattr__ resolves them on first access.
# ---------------------------------------------------------------------------

_CMD_ATTR_MAP: dict[str, tuple[str, str]] = {
    # attr_name -> (module, attr_in_module)  [fast-path for commonly used attrs]
    "cmd_setup":              ("tag.cmd.system",  "cmd_setup"),
    "cmd_hermes_passthrough": ("tag.cmd.system",  "cmd_hermes_passthrough"),
    "cmd_tui":                ("tag.cmd.system",  "cmd_tui"),
    "cmd_doctor":             ("tag.cmd.system",  "cmd_doctor"),
    "cmd_bootstrap":          ("tag.cmd.system",  "cmd_bootstrap"),
    "cmd_render":             ("tag.cmd.system",  "cmd_render"),
    "cmd_env":                ("tag.cmd.system",  "cmd_env"),
    "cmd_update":             ("tag.cmd.system",  "cmd_update"),
    "cmd_chat":               ("tag.cmd.session", "cmd_chat"),
    "cmd_dashboard":          ("tag.cmd.session", "cmd_dashboard"),
    "cmd_default":            ("tag.cmd.session", "cmd_default"),
    "cmd_submit":             ("tag.cmd.routing", "cmd_submit"),
    "cmd_benchmark":          ("tag.cmd.routing", "cmd_benchmark"),
    "cmd_runs":               ("tag.cmd.routing", "cmd_runs"),
    "cmd_route":              ("tag.cmd.routing", "cmd_route"),
    "cmd_import_codex":       ("tag.cmd.import_", "cmd_import_codex"),
    "cmd_import_claude":      ("tag.cmd.import_", "cmd_import_claude"),
    "cmd_import_gemini":      ("tag.cmd.import_", "cmd_import_gemini"),
}


def __getattr__(name: str) -> object:
    """Lazily load cmd_* attributes, searching all cmd/ modules as fallback."""
    # Fast path: static map for well-known attrs
    if name in _CMD_ATTR_MAP:
        mod_name, attr = _CMD_ATTR_MAP[name]
        import importlib as _il
        mod = _il.import_module(mod_name)
        val = getattr(mod, attr)
        globals()[name] = val
        return val

    # Slow path: search all cmd/ modules for any callable attr.
    # This handles the long tail of functions that tests access via TAG.*
    # but that live in cmd/ submodules not covered by the static map above.
    if not name.startswith("__"):
        try:
            import importlib as _il
            from tag.cmd import get_command_modules
            for _mod in get_command_modules():
                try:
                    _val = getattr(_mod, name)
                    if callable(_val) or isinstance(_val, (dict, list, str, int, float, bool, type(None))):
                        globals()[name] = _val
                        return _val
                except AttributeError:
                    continue
        except Exception:
            pass

    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")


# ---------------------------------------------------------------------------
# Parser + main entry point
# ---------------------------------------------------------------------------


def build_parser() -> argparse.ArgumentParser:
    from tag.cmd import COMMAND_MODULES
    p = argparse.ArgumentParser(
        prog="tag",
        description="TAG — The Agent Gateway CLI",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--config", metavar="PATH", help="Path to tag.yaml")
    p.add_argument("--version", action="version", version=f"%(prog)s {__version__}")
    sub = p.add_subparsers(dest="command", metavar="COMMAND")
    for mod in COMMAND_MODULES:
        try:
            mod.register(sub)
        except ValueError:
            # Ignore duplicate subparser registrations (e.g., 'route' in both
            # cmd.system and cmd.routing, 'mem2' in memory and prd_clusters).
            pass
    return p


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if not hasattr(args, "func"):
        parser.print_help()
        return 1
    try:
        return int(args.func(args) or 0)
    except SystemExit as exc:
        # Honor CPython's SystemExit convention: an int payload is the exit
        # status; anything else (typically a string message) is printed to
        # stderr and maps to exit 1. The old `int(exc.code)` crashed with a
        # ValueError on every `raise SystemExit("message")`.
        code = exc.code
        if code is None:
            return 0
        if isinstance(code, int):
            return code
        print(code, file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("Aborted.", file=sys.stderr)
        return 130
    except BrokenPipeError:
        return 0
    except Exception as exc:  # noqa: BLE001 — top-level CLI safety net
        # Never surface a raw traceback to end users. Set TAG_DEBUG=1 to
        # re-raise for development.
        if os.environ.get("TAG_DEBUG"):
            raise
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())

# ---------------------------------------------------------------------------
# Self-register: ensure sys.modules["tag.controller"] points to the module
# object that contains our fully-initialized globals. When tests load this
# file via importlib under a name other than "tag.controller" (e.g.
# "tag_controller"), cmd/* modules that later do
#   "from tag.controller import cmd_setup"
# would get a stale/different object. By replacing the sys.modules entry
# here — after all definitions are ready — monkeypatching on the test-loaded
# module propagates correctly to cmd modules that do deferred imports.
#
# We locate our own module by scanning sys.modules for the object whose
# __dict__ IS our globals(). If not found, we build a module from globals().
# ---------------------------------------------------------------------------
import ctypes as _ctypes
import types as _types

# ---------------------------------------------------------------------------
# Locate the actual module object for this executing code. During normal
# import, sys.modules["tag.controller"] IS this module. During test loading
# via importlib.util.module_from_spec + exec_module, the module was created
# with a non-"tag.controller" name (e.g. "tag_controller") and is NOT in
# sys.modules. We use ctypes to retrieve the frame's f_locals which in
# module-level code IS the module's __dict__, then walk the gc referrers to
# find the ModuleType object that owns it.
# ---------------------------------------------------------------------------
try:
    import gc as _gc
    _my_dict = globals()
    _self_module = None
    for _ref in _gc.get_referrers(_my_dict):
        if isinstance(_ref, _types.ModuleType) and vars(_ref) is _my_dict:
            _self_module = _ref
            break
    if _self_module is not None:
        _existing = sys.modules.get("tag.controller")
        if _existing is None or _existing is _self_module:
            # Normal import: sys.modules already points here (Python sets it
            # before exec_module to break circular imports), or the slot is
            # empty. Either way, make sure it points to us.
            sys.modules["tag.controller"] = _self_module
        else:
            # A DIFFERENT controller module was loaded first (e.g. by another
            # test file using importlib). The first-loaded module stays as the
            # canonical "tag.controller" so that monkeypatches on it propagate
            # through cmd/* modules' lazy imports. We patch THIS module's
            # __getattr__ to proxy attribute lookups through the canonical one,
            # so tests that access attributes via this module still work.
            _canonical = _existing
            _old_ga = _self_module.__dict__.pop("__getattr__", None)

            def __getattr__(name: str, _c=_canonical, _orig=_old_ga) -> object:  # type: ignore[misc]
                try:
                    return getattr(_c, name)
                except AttributeError:
                    pass
                if _orig is not None:
                    return _orig(name)
                raise AttributeError(f"module {__name__!r} has no attribute {name!r}")

            _self_module.__getattr__ = __getattr__  # type: ignore[attr-defined]
    del _gc, _my_dict, _self_module
except Exception:
    pass

del _ctypes, _types
