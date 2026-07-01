"""AI tool credential import commands."""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any

from tag.core.config import load_config, save_config, config_path
from tag.core.paths import runtime_codex_home, runtime_home, profile_home, hermes_env, tag_home
from tag.core.utils import nonnegative_int, read_dotenv, _upsert_env_line

try:
    from tag.tui_output import print_error, print_success, print_warning
except Exception:
    def print_error(msg): print(f"error: {msg}", file=sys.stderr)
    def print_success(msg): print(msg)
    def print_warning(msg): print(f"warning: {msg}", file=sys.stderr)

import subprocess
import yaml

from tag.controller import (
    ensure_hermes_ready,
    ensure_runtime_dirs,
    write_yaml,
    import_codex_into_profile,
    import_claude_into_profile,
    import_gemini_into_profile,
    import_continue_into_profile,
    import_mistral_into_profile,
    import_opencode_into_profile,
    import_zed_into_profile,
    import_copilot_into_profile,
    import_aider_into_profile,
    import_aws_into_profile,
    import_cursor_into_profile,
)


def _import_fail(args: argparse.Namespace, message: str, *, payload: dict | None = None, code: int = 1):
    """Fail an import command with a consistent contract.

    When ``--json`` is set, emit a machine-readable JSON object on stdout (so
    scripts get valid JSON on *every* outcome, not just success); otherwise
    fall back to the plain-text ``SystemExit`` behaviour. Always exits nonzero.
    """
    if getattr(args, "json", False):
        out = dict(payload) if payload is not None else {"status": "error"}
        out.setdefault("status", "error")
        out.setdefault("error", message)
        print(json.dumps(out, indent=2))
        raise SystemExit(code)
    raise SystemExit(message)


def cmd_import_codex(args: argparse.Namespace) -> int:
    from tag.controller import (
        ensure_hermes_ready as _ensure_hermes_ready,
        ensure_runtime_dirs as _ensure_runtime_dirs,
        profile_home as _profile_home,
        import_codex_into_profile as _import_codex_into_profile,
    )
    cfg = load_config(config_path(args.config))
    _ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profiles = cfg.get("profiles", {})
    if args.profile not in profiles:
        available = ", ".join(sorted(profiles))
        _import_fail(args, f"Unknown profile '{args.profile}'. Available: {available}")

    _ensure_runtime_dirs(cfg)
    target_home = _profile_home(cfg, args.profile)
    if not target_home.exists():
        _import_fail(
            args,
            f"Profile home does not exist for '{args.profile}'. Run bootstrap first.",
        )

    source_home = (
        Path(args.codex_home).expanduser().resolve()
        if args.codex_home
        else Path(
            os.environ.get("TAG_IMPORT_CODEX_HOME", str(runtime_codex_home(cfg)))
        ).expanduser().resolve()
    )
    result = _import_codex_into_profile(
        cfg,
        profile_name=args.profile,
        source_codex_home=source_home,
    )
    if result["status"] != "imported":
        _import_fail(args, str(result.get("message", "Codex import failed.")), payload=result)

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
        _import_fail(args, f"Unknown profile '{args.profile}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, args.profile)
    if not target_home.exists():
        _import_fail(
            args,
            f"Profile home does not exist for '{args.profile}'. Run `tag bootstrap` first.",
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
        _import_fail(
            args,
            "No Claude credentials found. Set ANTHROPIC_API_KEY or use "
            "`tag import-claude --use-oauth` to import from claude auth login.",
            payload=result,
        )
    if result["status"] == "profile-missing":
        _import_fail(
            args,
            f"Profile '{args.profile}' home does not exist. Run `tag bootstrap` first.",
            payload=result,
        )
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
        _import_fail(args, f"Unknown profile '{args.profile}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, args.profile)
    if not target_home.exists():
        _import_fail(
            args,
            f"Profile home does not exist for '{args.profile}'. Run `tag bootstrap` first.",
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
        _import_fail(
            args,
            "No Gemini credentials found. Set GEMINI_API_KEY (from "
            "https://aistudio.google.com/app/apikey) or use "
            "`tag import-gemini --use-oauth` to import from ~/.gemini/oauth_creds.json.",
            payload=result,
        )
    if result["status"] == "profile-missing":
        _import_fail(
            args,
            f"Profile '{args.profile}' home does not exist. Run `tag bootstrap` first.",
            payload=result,
        )
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
        _import_fail(args, f"Unknown profile '{args.profile}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, args.profile)
    if not target_home.exists():
        _import_fail(
            args,
            f"Profile home does not exist for '{args.profile}'. Run `tag bootstrap` first.",
        )
    source_home = (
        Path(args.continue_home).expanduser().resolve()
        if getattr(args, "continue_home", None)
        else None
    )
    result = import_continue_into_profile(cfg, profile_name=args.profile, source_continue_home=source_home)
    if result["status"] == "skipped-no-auth":
        _import_fail(
            args,
            "No Continue.dev config found with API keys. "
            "Expected ~/.continue/config.yaml or ~/.continue/config.json.",
            payload=result,
        )
    if result["status"] == "profile-missing":
        _import_fail(
            args,
            f"Profile '{args.profile}' home does not exist. Run `tag bootstrap` first.",
            payload=result,
        )
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
        _import_fail(args, f"Unknown profile '{args.profile}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, args.profile)
    if not target_home.exists():
        _import_fail(
            args,
            f"Profile home does not exist for '{args.profile}'. Run `tag bootstrap` first.",
        )
    source_home = (
        Path(args.vibe_home).expanduser().resolve()
        if getattr(args, "vibe_home", None)
        else None
    )
    result = import_mistral_into_profile(cfg, profile_name=args.profile, source_vibe_home=source_home)
    if result["status"] == "skipped-no-auth":
        _import_fail(
            args,
            "No Mistral credentials found. Set MISTRAL_API_KEY or ensure "
            "`mistral-vibe` has written ~/.vibe/.env.",
            payload=result,
        )
    if result["status"] == "profile-missing":
        _import_fail(
            args,
            f"Profile '{args.profile}' home does not exist. Run `tag bootstrap` first.",
            payload=result,
        )
    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    print(f"Imported Mistral credentials into profile '{args.profile}'.")
    return 0


def _cmd_import_generic(
    args: argparse.Namespace,
    *,
    import_fn: Any,
    no_auth_msg: str,
    source_path_attr: str | None,
    display_name: str,
    extra_kwargs: dict[str, Any] | None = None,
    source_kwarg: str | None = None,
) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profiles = cfg.get("profiles", {})
    if args.profile not in profiles:
        available = ", ".join(sorted(profiles))
        _import_fail(args, f"Unknown profile '{args.profile}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, args.profile)
    if not target_home.exists():
        _import_fail(
            args,
            f"Profile home does not exist for '{args.profile}'. Run `tag bootstrap` first.",
        )
    kwargs: dict[str, Any] = {"profile_name": args.profile}
    if source_path_attr and getattr(args, source_path_attr, None):
        raw = getattr(args, source_path_attr)
        # The argparse dest (source_path_attr) often differs from the import
        # function's parameter name (source_kwarg); pass under the fn's name.
        key = source_kwarg or source_path_attr
        try:
            kwargs[key] = Path(raw).expanduser().resolve()
        except (OSError, RuntimeError) as exc:
            _import_fail(args, f"Cannot resolve path '{raw}': {exc}")
    if extra_kwargs:
        kwargs.update(extra_kwargs)
    result = import_fn(cfg, **kwargs)
    if result["status"] == "skipped-no-auth":
        _import_fail(args, no_auth_msg, payload=result)
    if result["status"] == "profile-missing":
        _import_fail(
            args,
            f"Profile '{args.profile}' home does not exist. Run `tag bootstrap` first.",
            payload=result,
        )
    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    providers = result.get("providers_imported")
    if providers:
        print(f"Imported {display_name} credentials into profile '{args.profile}' ({', '.join(providers)}).")
    else:
        mode = result.get("mode", "")
        print(f"Imported {display_name} credentials into profile '{args.profile}' (mode: {mode}).")
    if "tos_warning" in result:
        print(f"WARNING: {result['tos_warning']}")
    return 0


def cmd_import_opencode(args: argparse.Namespace) -> int:
    return _cmd_import_generic(
        args,
        import_fn=import_opencode_into_profile,
        no_auth_msg="No opencode credentials found. Expected ~/.local/share/opencode/auth.json.",
        source_path_attr="opencode_data_dir",
        source_kwarg="source_data_dir",
        display_name="opencode",
    )


def cmd_import_zed(args: argparse.Namespace) -> int:
    return _cmd_import_generic(
        args,
        import_fn=import_zed_into_profile,
        no_auth_msg=(
            "No API keys found in Zed settings. Zed stores keys in the OS keychain by default; "
            "set keys via Zed's Agent Settings panel and ensure they are also exported as standard "
            "env vars (OPENAI_API_KEY, ANTHROPIC_API_KEY, etc.)."
        ),
        source_path_attr="zed_config",
        source_kwarg="source_zed_config",
        display_name="Zed",
    )


def cmd_import_copilot(args: argparse.Namespace) -> int:
    return _cmd_import_generic(
        args,
        import_fn=import_copilot_into_profile,
        no_auth_msg=(
            "No GitHub token found. Run `gh auth login` to authenticate the gh CLI, "
            "or set GITHUB_TOKEN in your environment."
        ),
        source_path_attr="gh_config",
        source_kwarg="source_gh_config",
        display_name="GitHub Copilot",
    )


def cmd_import_aider(args: argparse.Namespace) -> int:
    return _cmd_import_generic(
        args,
        import_fn=import_aider_into_profile,
        no_auth_msg=(
            "No Aider credentials found. Expected ~/.aider.conf.yml, ~/.env, or ~/.aider.env "
            "with at least one of: OPENAI_API_KEY, ANTHROPIC_API_KEY, GEMINI_API_KEY, etc."
        ),
        source_path_attr="aider_home",
        source_kwarg="source_home",
        display_name="Aider",
    )


def cmd_import_aws(args: argparse.Namespace) -> int:
    return _cmd_import_generic(
        args,
        import_fn=import_aws_into_profile,
        no_auth_msg=(
            "No AWS credentials found. Run `aws configure` or set AWS_ACCESS_KEY_ID and "
            "AWS_SECRET_ACCESS_KEY in your environment."
        ),
        source_path_attr="aws_dir",
        source_kwarg="source_aws_dir",
        display_name="AWS Bedrock",
    )


def cmd_import_cursor(args: argparse.Namespace) -> int:
    return _cmd_import_generic(
        args,
        import_fn=import_cursor_into_profile,
        no_auth_msg=(
            "No API keys found in Cursor's local storage. Add API keys via Cursor Settings -> "
            "Models (BYOK) and ensure Cursor has been run at least once."
        ),
        source_path_attr="cursor_dir",
        source_kwarg="source_cursor_dir",
        display_name="Cursor",
    )


# ---------------------------------------------------------------------------
# PRD-001: Supermemory and Honcho credential import
# ---------------------------------------------------------------------------

def _detect_supermemory_credentials(
    source_config_dir: Path | None = None,
) -> dict[str, str]:
    """Read Supermemory API key from known config locations."""
    candidates: list[Path] = []
    if source_config_dir:
        candidates.append(source_config_dir / "config.json")
    candidates += [
        Path.home() / ".config" / "supermemory" / "config.json",
        Path.home() / ".supermemory" / "config.json",
    ]
    for path in candidates:
        if path.exists():
            try:
                data = json.loads(path.read_text())
                key = data.get("api_key") or data.get("token")
                if key:
                    return {"SUPERMEMORY_API_KEY": str(key)}
            except (json.JSONDecodeError, OSError):
                pass
    if key := os.environ.get("SUPERMEMORY_API_KEY", ""):
        return {"SUPERMEMORY_API_KEY": key}
    return {}


def import_supermemory_into_profile(
    cfg: dict[str, Any],
    profile_name: str,
    *,
    api_key: str | None = None,
    source_config_dir: Path | None = None,
    force: bool = False,
) -> dict[str, Any]:
    ph = profile_home(cfg, profile_name)
    if not ph.exists():
        return {"status": "profile-missing"}
    creds = (
        {"SUPERMEMORY_API_KEY": api_key}
        if api_key and api_key.strip()
        else _detect_supermemory_credentials(source_config_dir)
    )
    if not creds:
        return {"status": "skipped-no-auth"}
    env_file = ph / ".env"
    for key, value in creds.items():
        _upsert_env_line(env_file, key, value)
    _upsert_env_line(env_file, "SUPERMEMORY_SESSION_INGEST", "1")
    return {"status": "ok", "profile": profile_name, "providers_imported": ["supermemory"]}


def _detect_honcho_credentials(
    source_config: Path | None = None,
) -> dict[str, str]:
    """Read Honcho credentials from known config locations."""
    candidates: list[Path] = []
    if source_config:
        candidates.append(source_config)
    candidates += [
        Path.home() / ".honcho" / ".env",
        Path.home() / ".config" / "honcho" / "config.yaml",
    ]
    result: dict[str, str] = {}
    for path in candidates:
        if not path.exists():
            continue
        try:
            if path.suffix in (".yaml", ".yml"):
                data = yaml.safe_load(path.read_text()) or {}
                if k := data.get("api_key") or data.get("HONCHO_API_KEY"):
                    result["HONCHO_API_KEY"] = str(k)
                if u := data.get("base_url") or data.get("HONCHO_BASE_URL"):
                    result["HONCHO_BASE_URL"] = str(u)
            else:
                for line in path.read_text().splitlines():
                    line = line.strip()
                    if "=" in line and not line.startswith("#"):
                        k, _, v = line.partition("=")
                        if k in ("HONCHO_API_KEY", "HONCHO_BASE_URL"):
                            result[k] = v.strip()
        except (OSError, yaml.YAMLError):
            pass
    for key in ("HONCHO_API_KEY", "HONCHO_BASE_URL"):
        if key not in result and (val := os.environ.get(key, "")):
            result[key] = val
    return result


def import_honcho_into_profile(
    cfg: dict[str, Any],
    profile_name: str,
    *,
    source_config: Path | None = None,
    base_url: str | None = None,
    force: bool = False,
) -> dict[str, Any]:
    ph = profile_home(cfg, profile_name)
    if not ph.exists():
        return {"status": "profile-missing"}
    creds = _detect_honcho_credentials(source_config)
    if base_url:
        creds["HONCHO_BASE_URL"] = base_url
    # A base URL is configuration, not a credential. Require an actual API key
    # before reporting a successful credential import.
    if "HONCHO_API_KEY" not in creds:
        return {"status": "skipped-no-auth", "profile": profile_name}
    env_file = ph / ".env"
    for key, value in creds.items():
        _upsert_env_line(env_file, key, value)
    return {"status": "ok", "profile": profile_name, "providers_imported": list(creds.keys())}


def cmd_import_supermemory(args: argparse.Namespace) -> int:
    api_key = getattr(args, "api_key", None)
    if api_key is not None and not api_key.strip():
        _import_fail(
            args,
            "Supermemory API key is empty or whitespace-only. Pass a non-empty --api-key.",
        )
    return _cmd_import_generic(
        args,
        import_fn=import_supermemory_into_profile,
        no_auth_msg=(
            "No Supermemory API key found. Pass --api-key or set SUPERMEMORY_API_KEY.\n"
            "Get a key at https://supermemory.ai/"
        ),
        source_path_attr="source_config_dir",
        display_name="Supermemory",
        extra_kwargs={"api_key": getattr(args, "api_key", None) or None},
    )


def cmd_import_honcho(args: argparse.Namespace) -> int:
    return _cmd_import_generic(
        args,
        import_fn=import_honcho_into_profile,
        no_auth_msg=(
            "No Honcho credentials found. Pass --base-url and set HONCHO_API_KEY.\n"
            "See https://honcho.dev/ for self-hosted setup."
        ),
        source_path_attr="source_config",
        display_name="Honcho",
        extra_kwargs={"base_url": getattr(args, "base_url", None) or None},
    )


# ---------------------------------------------------------------------------
# PRD-006: Nous Portal Tool Gateway
# ---------------------------------------------------------------------------

def _detect_nous_portal_credentials(
    source_config: Path | None = None,
) -> dict[str, str]:
    """Read Nous Portal API key from known config locations."""
    candidates: list[Path] = []
    if source_config:
        candidates.append(source_config)
    candidates += [
        Path.home() / ".config" / "nousresearch" / "portal.json",
        Path.home() / ".nousresearch" / "config.json",
        Path.home() / ".nousresearch" / "portal.json",
    ]
    for path in candidates:
        if path.exists():
            try:
                data = json.loads(path.read_text())
                key = data.get("api_key") or data.get("token") or data.get("key")
                if key:
                    return {"NOUS_PORTAL_API_KEY": str(key)}
            except (json.JSONDecodeError, OSError):
                pass
    if key := os.environ.get("NOUS_PORTAL_API_KEY", ""):
        return {"NOUS_PORTAL_API_KEY": key}
    return {}


def import_nous_portal_into_profile(
    cfg: dict[str, Any],
    profile_name: str,
    *,
    api_key: str | None = None,
    source_config: Path | None = None,
    force: bool = False,
) -> dict[str, Any]:
    """Write NOUS_PORTAL_API_KEY to profile .env and enable gateway in config."""
    ph = profile_home(cfg, profile_name)
    if not ph.exists():
        return {"status": "profile-missing", "profile": profile_name}

    creds = {"NOUS_PORTAL_API_KEY": api_key} if api_key else _detect_nous_portal_credentials(source_config)
    if not creds:
        return {"status": "skipped-no-auth", "profile": profile_name}

    env_file = ph / ".env"
    for key, value in creds.items():
        _upsert_env_line(env_file, key, value)

    # Enable use_gateway in profile's Hermes config.yaml
    profile_config_file = ph / "config.yaml"
    if profile_config_file.exists():
        try:
            pcfg = yaml.safe_load(profile_config_file.read_text()) or {}
            pcfg.setdefault("gateway", {})["use_gateway"] = True
            write_yaml(profile_config_file, pcfg, force=True)
        except Exception:
            pass

    return {
        "status": "ok",
        "profile": profile_name,
        "providers_imported": ["nous_portal"],
        "env_file": str(env_file),
    }


def cmd_import_nous_portal(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profiles_cfg = cfg.get("profiles", {})

    if getattr(args, "all_profiles", False):
        profiles_to_update = list(profiles_cfg.keys())
    else:
        p = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        if p not in profiles_cfg:
            available = ", ".join(sorted(profiles_cfg))
            _import_fail(args, f"Unknown profile '{p}'. Available: {available}")
        profiles_to_update = [p]

    api_key_arg = getattr(args, "api_key", None) or None

    # Validate the effective key length at the CLI layer — covering both
    # --api-key AND env/config-detected keys (B120). The library import fn does
    # not validate, so direct callers keep control.
    effective_key = api_key_arg
    if effective_key is None:
        detected = _detect_nous_portal_credentials()
        effective_key = detected.get("NOUS_PORTAL_API_KEY") or None
    if effective_key is not None and len(effective_key) < 20:
        raise SystemExit(
            f"API key too short ({len(effective_key)} chars); "
            "Nous Portal keys are at least 20 characters"
        )

    results = []
    for p in profiles_to_update:
        ensure_runtime_dirs(cfg)
        result = import_nous_portal_into_profile(
            cfg,
            p,
            api_key=api_key_arg,
            force=getattr(args, "force", False),
        )
        results.append(result)

    # A no-credentials / invalid-key outcome must exit nonzero even under --json,
    # so scripts can't mistake "nothing was written" for success.
    any_ok = any(r["status"] == "ok" for r in results)

    if getattr(args, "json", False):
        print(json.dumps(results, indent=2))
        return 0 if any_ok else 1

    for r in results:
        profile_name = r.get("profile", "?")
        if r["status"] == "ok":
            print(f"  ✓ {profile_name}: Nous Portal gateway enabled")
        elif r["status"] == "skipped-no-auth":
            print(f"  – {profile_name}: no credentials found")
        elif r["status"] == "invalid-key":
            print(f"  ✗ {profile_name}: {r.get('error', 'invalid API key')}")
        else:
            print(f"  ✗ {profile_name}: {r['status']}")

    if not any_ok:
        print(
            "Hint: pass --api-key YOUR_KEY or set NOUS_PORTAL_API_KEY env var.\n"
            "Note: Requires an active Nous Portal subscription (https://portal.nousresearch.com/)."
        )
        return 1
    return 0


# ---------------------------------------------------------------------------
# PRD-005: Execution backend credential import helpers
# ---------------------------------------------------------------------------

_VALID_BACKENDS = ("local", "docker", "ssh", "modal", "daytona", "singularity")
_DOCKER_IMAGE_RE = re.compile(r'^[a-zA-Z0-9][a-zA-Z0-9_./:@-]*$')


def import_docker_into_profile(
    cfg: dict[str, Any],
    profile_name: str,
    *,
    image: str | None = None,
    force: bool = False,
) -> dict[str, Any]:
    """Write Docker backend settings to the profile's .env file."""
    if image and not _DOCKER_IMAGE_RE.match(image):
        raise SystemExit(f"Invalid Docker image name: {image!r}")
    env_file = profile_home(cfg, profile_name) / ".env"
    env_file.parent.mkdir(parents=True, exist_ok=True)

    result: dict[str, Any] = {"profile": profile_name, "status": "ok", "keys_written": []}
    default_image = image or "ubuntu:22.04"
    _upsert_env_line(env_file, "DOCKER_DEFAULT_IMAGE", default_image)
    result["keys_written"].append("DOCKER_DEFAULT_IMAGE")

    # Verify Docker is actually available (advisory only — we don't block on this)
    import shutil as _shutil
    if _shutil.which("docker"):
        try:
            proc = subprocess.run(
                ["docker", "info"],
                capture_output=True,
                timeout=5,
            )
            result["docker_available"] = proc.returncode == 0
        except Exception:
            result["docker_available"] = False
    else:
        result["docker_available"] = False
        result["warning"] = "docker binary not found — install Docker before using this backend"

    return result


_SSH_HOST_RE = re.compile(r'^[a-zA-Z0-9.\-_\[\]:]+$')


def import_ssh_into_profile(
    cfg: dict[str, Any],
    profile_name: str,
    *,
    host: str,
    user: str | None = None,
    key_file: str | None = None,
    port: int = 22,
    force: bool = False,
) -> dict[str, Any]:
    """Write SSH backend credentials to the profile's .env file."""
    if not host or not host.strip():
        raise SystemExit("--host is required for SSH backend import")
    if not _SSH_HOST_RE.match(host.strip()):
        raise SystemExit(
            f"Invalid SSH host '{host}': must contain only alphanumerics, dots, hyphens, "
            "underscores, brackets, and colons (no shell metacharacters)"
        )
    if not (1 <= port <= 65535):
        raise SystemExit(f"Invalid SSH port {port}: must be 1–65535")
    env_file = profile_home(cfg, profile_name) / ".env"
    env_file.parent.mkdir(parents=True, exist_ok=True)

    result: dict[str, Any] = {"profile": profile_name, "status": "ok", "keys_written": []}
    _upsert_env_line(env_file, "SSH_HOST", host)
    result["keys_written"].append("SSH_HOST")
    if user:
        _upsert_env_line(env_file, "SSH_USER", user)
        result["keys_written"].append("SSH_USER")
    if key_file:
        _upsert_env_line(env_file, "SSH_KEY_FILE", str(Path(key_file).expanduser()))
        result["keys_written"].append("SSH_KEY_FILE")
        if not Path(key_file).expanduser().exists():
            result["warning"] = f"Key file not found: {key_file}"
    if port != 22:
        _upsert_env_line(env_file, "SSH_PORT", str(port))
        result["keys_written"].append("SSH_PORT")
    return result


def import_modal_into_profile(
    cfg: dict[str, Any],
    profile_name: str,
    *,
    token_id: str,
    token_secret: str,
    force: bool = False,
) -> dict[str, Any]:
    """Write Modal backend credentials to the profile's .env file."""
    if not token_id or not token_id.strip() or not token_secret or not token_secret.strip():
        raise SystemExit("--token-id and --token-secret must not be empty or whitespace-only")
    env_file = profile_home(cfg, profile_name) / ".env"
    env_file.parent.mkdir(parents=True, exist_ok=True)

    _upsert_env_line(env_file, "MODAL_TOKEN_ID", token_id)
    _upsert_env_line(env_file, "MODAL_TOKEN_SECRET", token_secret)
    return {"profile": profile_name, "status": "ok", "keys_written": ["MODAL_TOKEN_ID", "MODAL_TOKEN_SECRET"]}


def import_daytona_into_profile(
    cfg: dict[str, Any],
    profile_name: str,
    *,
    workspace_id: str,
    api_key: str | None = None,
    force: bool = False,
) -> dict[str, Any]:
    """Write Daytona workspace ID to the profile's .env file."""
    if not workspace_id or not workspace_id.strip():
        raise SystemExit("--workspace-id must not be empty or whitespace-only")
    env_file = profile_home(cfg, profile_name) / ".env"
    env_file.parent.mkdir(parents=True, exist_ok=True)

    keys: list[str] = []
    _upsert_env_line(env_file, "DAYTONA_WORKSPACE_ID", workspace_id)
    keys.append("DAYTONA_WORKSPACE_ID")
    if api_key:
        _upsert_env_line(env_file, "DAYTONA_API_KEY", api_key)
        keys.append("DAYTONA_API_KEY")
    return {"profile": profile_name, "status": "ok", "keys_written": keys}


def cmd_import_docker(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profile_name = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
    if profile_name not in cfg.get("profiles", {}):
        available = ", ".join(sorted(cfg.get("profiles", {})))
        _import_fail(args, f"Unknown profile '{profile_name}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    result = import_docker_into_profile(
        cfg,
        profile_name,
        image=getattr(args, "image", None) or None,
        force=getattr(args, "force", False),
    )
    if getattr(args, "json", False):
        print(json.dumps(result, indent=2))
        return 0
    if result["status"] == "ok":
        print(f"✓ {profile_name}: Docker backend configured")
        if not result.get("docker_available"):
            print(f"  ⚠ Warning: {result.get('warning', 'Docker daemon not running')}")
    else:
        print(f"✗ {profile_name}: {result['status']}")
        return 1
    return 0


def cmd_import_ssh(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profile_name = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
    if profile_name not in cfg.get("profiles", {}):
        available = ", ".join(sorted(cfg.get("profiles", {})))
        _import_fail(args, f"Unknown profile '{profile_name}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    port_arg = getattr(args, "port", None)
    result = import_ssh_into_profile(
        cfg,
        profile_name,
        host=args.host,
        user=getattr(args, "user", None) or None,
        key_file=getattr(args, "key_file", None) or None,
        port=port_arg if port_arg is not None else 22,
        force=getattr(args, "force", False),
    )
    if getattr(args, "json", False):
        print(json.dumps(result, indent=2))
        return 0
    if result["status"] == "ok":
        print(f"✓ {profile_name}: SSH backend configured (host: {args.host})")
        if result.get("warning"):
            print(f"  ⚠ Warning: {result['warning']}")
    else:
        print(f"✗ {profile_name}: {result['status']}")
        return 1
    return 0


def cmd_import_modal(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profile_name = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
    if profile_name not in cfg.get("profiles", {}):
        available = ", ".join(sorted(cfg.get("profiles", {})))
        _import_fail(args, f"Unknown profile '{profile_name}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    result = import_modal_into_profile(
        cfg,
        profile_name,
        token_id=args.token_id,
        token_secret=args.token_secret,
        force=getattr(args, "force", False),
    )
    if getattr(args, "json", False):
        print(json.dumps(result, indent=2))
        return 0
    print(f"✓ {profile_name}: Modal backend credentials written")
    return 0


def cmd_import_daytona(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profile_name = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
    if profile_name not in cfg.get("profiles", {}):
        available = ", ".join(sorted(cfg.get("profiles", {})))
        _import_fail(args, f"Unknown profile '{profile_name}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    result = import_daytona_into_profile(
        cfg,
        profile_name,
        workspace_id=args.workspace_id,
        api_key=getattr(args, "api_key", None) or None,
        force=getattr(args, "force", False),
    )
    if getattr(args, "json", False):
        print(json.dumps(result, indent=2))
        return 0
    print(f"✓ {profile_name}: Daytona backend configured (workspace: {args.workspace_id})")
    return 0


def register(sub: argparse._SubParsersAction) -> None:  # type: ignore[type-arg]
    """Register all import-* subcommands onto *sub*."""

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

    import_opencode = sub.add_parser(
        "import-opencode",
        help="Import API keys from opencode (~/.local/share/opencode/auth.json) into a TAG-managed profile",
    )
    import_opencode.add_argument("--profile", required=True)
    import_opencode.add_argument(
        "--opencode-data-dir",
        help="Path to opencode data dir (default: ~/.local/share/opencode)",
    )
    import_opencode.add_argument("--json", action="store_true")
    import_opencode.set_defaults(func=cmd_import_opencode)

    import_zed = sub.add_parser(
        "import-zed",
        help="Import API keys from Zed editor settings.json into a TAG-managed profile",
    )
    import_zed.add_argument("--profile", required=True)
    import_zed.add_argument(
        "--zed-config",
        help="Path to Zed settings.json (default: ~/.config/zed/settings.json)",
    )
    import_zed.add_argument("--json", action="store_true")
    import_zed.set_defaults(func=cmd_import_zed)

    import_copilot = sub.add_parser(
        "import-copilot",
        help="Import GitHub OAuth token from gh CLI into a TAG-managed profile",
    )
    import_copilot.add_argument("--profile", required=True)
    import_copilot.add_argument(
        "--gh-config",
        help="Path to gh CLI hosts.yml (default: ~/.config/gh/hosts.yml)",
    )
    import_copilot.add_argument("--json", action="store_true")
    import_copilot.set_defaults(func=cmd_import_copilot)

    import_aider = sub.add_parser(
        "import-aider",
        help="Import API keys from Aider config (~/.aider.conf.yml or ~/.env) into a TAG-managed profile",
    )
    import_aider.add_argument("--profile", required=True)
    import_aider.add_argument(
        "--aider-home",
        help="Base directory for Aider config files (default: ~)",
    )
    import_aider.add_argument("--json", action="store_true")
    import_aider.set_defaults(func=cmd_import_aider)

    import_aws = sub.add_parser(
        "import-aws",
        help="Import AWS credentials (~/.aws/credentials) for Amazon Bedrock / Q Developer into a TAG-managed profile",
    )
    import_aws.add_argument("--profile", required=True)
    import_aws.add_argument(
        "--aws-dir",
        help="Path to AWS config directory (default: ~/.aws)",
    )
    import_aws.add_argument("--json", action="store_true")
    import_aws.set_defaults(func=cmd_import_aws)

    import_cursor = sub.add_parser(
        "import-cursor",
        help="Import BYOK API keys from Cursor IDE's local SQLite store into a TAG-managed profile",
    )
    import_cursor.add_argument("--profile", required=True)
    import_cursor.add_argument(
        "--cursor-dir",
        help="Path to Cursor globalStorage directory containing state.vscdb",
    )
    import_cursor.add_argument("--json", action="store_true")
    import_cursor.set_defaults(func=cmd_import_cursor)

    # ---- PRD-001: import-supermemory, import-honcho ----
    import_sm = sub.add_parser("import-supermemory", help="Import Supermemory API key into a TAG profile")
    import_sm.add_argument("--profile", required=True)
    import_sm.add_argument("--api-key", metavar="KEY", dest="api_key")
    import_sm.add_argument("--source-config-dir", metavar="PATH", dest="source_config_dir")
    import_sm.add_argument("--json", action="store_true")
    import_sm.set_defaults(func=cmd_import_supermemory)

    import_honcho = sub.add_parser("import-honcho", help="Import Honcho credentials into a TAG profile")
    import_honcho.add_argument("--profile", required=True)
    import_honcho.add_argument("--base-url", metavar="URL", dest="base_url")
    import_honcho.add_argument("--source-config", metavar="PATH", dest="source_config")
    import_honcho.add_argument("--json", action="store_true")
    import_honcho.set_defaults(func=cmd_import_honcho)

    # ---- PRD-006: import-nous-portal ----
    import_nous = sub.add_parser("import-nous-portal", help="Import Nous Portal API key (enables Tool Gateway)")
    import_nous.add_argument("--profile", metavar="NAME")
    import_nous.add_argument("--api-key", metavar="KEY", dest="api_key")
    import_nous.add_argument("--all-profiles", action="store_true", dest="all_profiles")
    import_nous.add_argument("--force", action="store_true")
    import_nous.add_argument("--json", action="store_true")
    import_nous.set_defaults(func=cmd_import_nous_portal)

    # ---- PRD-005: execution backend imports ----
    import_docker = sub.add_parser("import-docker", help="Configure Docker execution backend for a profile")
    import_docker.add_argument("--profile", metavar="NAME")
    import_docker.add_argument("--image", metavar="IMAGE", help="Docker image (default: ubuntu:22.04)")
    import_docker.add_argument("--force", action="store_true")
    import_docker.add_argument("--json", action="store_true")
    import_docker.set_defaults(func=cmd_import_docker)

    import_ssh_p = sub.add_parser("import-ssh", help="Configure SSH remote execution backend for a profile")
    import_ssh_p.add_argument("--profile", metavar="NAME")
    import_ssh_p.add_argument("--host", required=True, metavar="HOST")
    import_ssh_p.add_argument("--user", metavar="USER")
    import_ssh_p.add_argument("--key-file", metavar="PATH", dest="key_file")
    import_ssh_p.add_argument("--port", type=int, default=22)
    import_ssh_p.add_argument("--force", action="store_true")
    import_ssh_p.add_argument("--json", action="store_true")
    import_ssh_p.set_defaults(func=cmd_import_ssh)

    import_modal_p = sub.add_parser("import-modal", help="Configure Modal cloud execution backend for a profile")
    import_modal_p.add_argument("--profile", metavar="NAME")
    import_modal_p.add_argument("--token-id", required=True, metavar="ID", dest="token_id")
    import_modal_p.add_argument("--token-secret", required=True, metavar="SECRET", dest="token_secret")
    import_modal_p.add_argument("--force", action="store_true")
    import_modal_p.add_argument("--json", action="store_true")
    import_modal_p.set_defaults(func=cmd_import_modal)

    import_daytona_p = sub.add_parser("import-daytona", help="Configure Daytona workspace backend for a profile")
    import_daytona_p.add_argument("--profile", metavar="NAME")
    import_daytona_p.add_argument("--workspace-id", required=True, metavar="ID", dest="workspace_id")
    import_daytona_p.add_argument("--api-key", metavar="KEY", dest="api_key")
    import_daytona_p.add_argument("--force", action="store_true")
    import_daytona_p.add_argument("--json", action="store_true")
    import_daytona_p.set_defaults(func=cmd_import_daytona)
