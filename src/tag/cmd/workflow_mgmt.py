"""Workflow management and navigation commands.

Covers: hooks (lifecycle), compare, context, shell, route-fallback,
mcp-registry, template.
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import re
import sqlite3
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path
from typing import Any

import yaml

try:
    from tag.tui_output import print_error, print_success, print_warning
except Exception:
    def print_error(msg: str) -> None:  # type: ignore[misc]
        print(f"error: {msg}", file=sys.stderr)

    def print_success(msg: str) -> None:  # type: ignore[misc]
        print(msg)

    def print_warning(msg: str) -> None:  # type: ignore[misc]
        print(f"warning: {msg}", file=sys.stderr)

from tag.core.config import load_config, save_config, config_path
from tag.core.paths import (
    tag_home,
    hermes_root,
    hermes_bin as _hermes_bin,
    runtime_home as _runtime_home,
    profile_home as _profile_home,
    runtime_db_path as _runtime_db_path,
    profile_exec_env as _profile_exec_env,
    ensure_runtime_dirs as _ensure_runtime_dirs,
)
from tag.core.db import open_db
from tag.core.utils import nonnegative_int, write_yaml as _write_yaml, positive_int as _positive_int


# ---------------------------------------------------------------------------
# Helpers (local to this module)
# ---------------------------------------------------------------------------

def _utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat()


def _safe_profile_path(base: Path, profile: str) -> Path:
    resolved = (base / profile).resolve()
    base_resolved = base.resolve()
    try:
        resolved.relative_to(base_resolved)
    except ValueError:
        raise SystemExit(f"Invalid profile name (path traversal detected): {profile!r}")
    return resolved


def _default_master_profile(cfg: dict[str, Any]) -> str:
    """The configured default master profile.

    The real key is ``cfg['defaults']['master_profile']``; ``cfg.get('master_profile')``
    is always ``None`` so it silently falls back to 'orchestrator', ignoring a
    user-set default.
    """
    return cfg.get("defaults", {}).get("master_profile", "orchestrator")


def _runtime_profile_dir(cfg: dict[str, Any], profile: str) -> Path:
    """The profile directory the runtime actually reads
    (runtime_home/.hermes/profiles/<profile>), with a path-traversal guard."""
    return _safe_profile_path(_runtime_home(cfg) / ".hermes" / "profiles", profile)


def _validate_fetch_url(url: str) -> None:
    """Restrict outbound fetches to public http/https hosts (SSRF / file:// guard).

    Rejects non-http(s) schemes (notably ``file://``) and refuses to connect to
    loopback, link-local (incl. cloud metadata 169.254.169.254), private,
    reserved, multicast or unspecified addresses.
    """
    import ipaddress
    import socket
    from urllib.parse import urlparse

    parsed = urlparse(url)
    if parsed.scheme not in ("http", "https"):
        raise ValueError(
            f"unsupported URL scheme {parsed.scheme or '(none)'!r}: only http/https are allowed"
        )
    host = parsed.hostname
    if not host:
        raise ValueError("URL has no host")

    candidates: list[Any] = []
    try:
        candidates = [ipaddress.ip_address(host)]
    except ValueError:
        try:
            for info in socket.getaddrinfo(host, None):
                candidates.append(ipaddress.ip_address(info[4][0]))
        except (socket.gaierror, ValueError, OSError):
            candidates = []

    for ip in candidates:
        if (ip.is_loopback or ip.is_link_local or ip.is_private
                or ip.is_reserved or ip.is_multicast or ip.is_unspecified):
            raise ValueError(f"refusing to fetch from non-public address {ip} (host {host!r})")


def _ip_is_blocked(addr: str) -> bool:
    """True if *addr* is a non-public (SSRF-sensitive) IP literal."""
    import ipaddress
    try:
        ip = ipaddress.ip_address(addr.split("%", 1)[0])
    except ValueError:
        return False
    return (ip.is_loopback or ip.is_link_local or ip.is_private
            or ip.is_reserved or ip.is_multicast or ip.is_unspecified)


def _safe_urlopen(url, *, timeout: int = 15):
    """SSRF-hardened urlopen: pins the resolved IP at connect time and refuses
    redirects to non-public addresses.

    Closes two holes in the plain ``urllib.request.urlopen``:

    * **Redirect following** — the default opener follows 3xx transparently, so a
      public ``302 -> http://127.0.0.1/`` reaches internal services. Every
      redirect target is re-validated with :func:`_validate_fetch_url` and each
      hop opens a fresh (also pinned) connection.
    * **DNS rebinding (TOCTOU)** — validation and connection normally resolve DNS
      independently. The pinned connection re-resolves at connect time and
      refuses non-public addresses, so a low-TTL record cannot flip to
      loopback/metadata between validate and fetch.
    """
    import http.client
    import socket as _socket

    def _connect_pinned(conn):
        infos = _socket.getaddrinfo(conn.host, conn.port, 0, _socket.SOCK_STREAM)
        for info in infos:
            if _ip_is_blocked(info[4][0]):
                raise OSError(
                    f"refusing to connect to non-public address {info[4][0]} (SSRF protection)"
                )
        last_err = None
        for family, socktype, proto, _canon, sockaddr in infos:
            sock = None
            try:
                sock = _socket.socket(family, socktype, proto)
                if conn.timeout is not _socket._GLOBAL_DEFAULT_TIMEOUT:
                    sock.settimeout(conn.timeout)
                if getattr(conn, "source_address", None):
                    sock.bind(conn.source_address)
                sock.connect(sockaddr)
                return sock
            except OSError as exc:
                last_err = exc
                if sock is not None:
                    sock.close()
        raise last_err if last_err is not None else OSError("connection failed")

    class _PinnedHTTPConnection(http.client.HTTPConnection):
        def connect(self):
            self.sock = _connect_pinned(self)
            if self._tunnel_host:
                self._tunnel()

    class _PinnedHTTPSConnection(http.client.HTTPSConnection):
        def connect(self):
            self.sock = _connect_pinned(self)
            if self._tunnel_host:
                self._tunnel()
                server_hostname = self._tunnel_host
            else:
                server_hostname = self.host
            self.sock = self._context.wrap_socket(self.sock, server_hostname=server_hostname)

    class _PinnedHTTPHandler(urllib.request.HTTPHandler):
        def http_open(self, req):
            return self.do_open(_PinnedHTTPConnection, req)

    class _PinnedHTTPSHandler(urllib.request.HTTPSHandler):
        def https_open(self, req):
            return self.do_open(_PinnedHTTPSConnection, req)

    class _GuardRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, req, fp, code, msg, headers, newurl):
            try:
                _validate_fetch_url(newurl)
            except ValueError as exc:
                raise urllib.error.HTTPError(
                    newurl, code, f"blocked redirect: {exc}", headers, fp
                )
            return super().redirect_request(req, fp, code, msg, headers, newurl)

    opener = urllib.request.build_opener(
        _PinnedHTTPHandler, _PinnedHTTPSHandler, _GuardRedirect
    )
    return opener.open(url, timeout=timeout)


# ---------------------------------------------------------------------------
# PRD-014: MCP Server Registry
# ---------------------------------------------------------------------------

def _load_mcp_registry() -> dict[str, Any]:
    p = Path(__file__).parent.parent / "config" / "mcp-registry.yaml"
    if not p.exists():
        return {}
    with p.open() as fh:
        return yaml.safe_load(fh) or {}


def cmd_mcp_registry(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(getattr(args, "config", None)))
    reg = _load_mcp_registry()
    servers: dict[str, Any] = reg.get("servers", {})
    sub = getattr(args, "mcp_reg_subcommand", None)

    if sub == "list" or sub is None:
        category_filter = getattr(args, "category", None)
        rows = []
        for name, info in servers.items():
            if category_filter and info.get("category") != category_filter:
                continue
            rows.append({
                "name": name,
                "description": info.get("description", ""),
                "category": info.get("category", ""),
                "requires_env": info.get("requires_env", []),
            })
        if getattr(args, "json", False):
            print(json.dumps(rows, indent=2))
        else:
            print(f"{'Name':<30} {'Category':<14} {'Description'}")
            print("-" * 80)
            for r in rows:
                env_note = f" [needs: {', '.join(r['requires_env'])}]" if r["requires_env"] else ""
                print(f"  {r['name']:<28} {r['category']:<14} {r['description']}{env_note}")
        return 0

    if sub == "install":
        name = args.server_name
        info = servers.get(name)
        if not info:
            print_error(f"Unknown MCP server: {name}")
            return 1
        install = info.get("install", {})
        pkg = install.get("package", name)
        itype = install.get("type", "npm")
        if itype == "npm":
            result = subprocess.run(["npm", "install", "-g", pkg], capture_output=True, text=True)
        elif itype == "pip":
            result = subprocess.run([sys.executable, "-m", "pip", "install", pkg], capture_output=True, text=True)
        else:
            print_error(f"Unknown install type: {itype}")
            return 1
        if result.returncode != 0:
            print_error(f"Install failed: {result.stderr.strip()}")
            return result.returncode
        print_success(f"Installed MCP server '{name}' ({pkg})")
        return 0

    if sub == "enable":
        name = args.server_name
        info = servers.get(name)
        if not info:
            print_error(f"Unknown MCP server: {name}")
            return 1
        profile = getattr(args, "profile", None) or _default_master_profile(cfg)
        cfg_block = info.get("config", {})
        # Write to the profile config the runtime actually loads
        # (runtime_home/.hermes/profiles/<profile>/config.yaml), using hermes'
        # top-level `mcp_servers` mapping keyed by server name.
        profile_dir = _runtime_profile_dir(cfg, profile)
        profile_cfg_path = profile_dir / "config.yaml"
        if profile_cfg_path.exists():
            with profile_cfg_path.open() as fh:
                pcfg = yaml.safe_load(fh) or {}
        else:
            pcfg = {}
        mcp_servers = pcfg.get("mcp_servers")
        if not isinstance(mcp_servers, dict):
            mcp_servers = {}
        pcfg["mcp_servers"] = mcp_servers
        if name not in mcp_servers:
            mcp_servers[name] = cfg_block
            profile_cfg_path.parent.mkdir(parents=True, exist_ok=True)
            _write_yaml(profile_cfg_path, pcfg, force=True)
            print_success(f"Enabled MCP server '{name}' for profile '{profile}'")
        else:
            print(f"MCP server '{name}' is already enabled for profile '{profile}'")
        return 0

    if sub == "disable":
        name = args.server_name
        profile = getattr(args, "profile", None) or _default_master_profile(cfg)
        profile_dir = _runtime_profile_dir(cfg, profile)
        profile_cfg_path = profile_dir / "config.yaml"
        if profile_cfg_path.exists():
            with profile_cfg_path.open() as fh:
                pcfg = yaml.safe_load(fh) or {}
            mcp_servers = pcfg.get("mcp_servers")
            if isinstance(mcp_servers, dict):
                mcp_servers.pop(name, None)
                pcfg["mcp_servers"] = mcp_servers
            elif isinstance(mcp_servers, list):
                pcfg["mcp_servers"] = [e for e in mcp_servers if e.get("name") != name]
            _write_yaml(profile_cfg_path, pcfg, force=True)
            print_success(f"Disabled MCP server '{name}' for profile '{profile}'")
        else:
            print(f"No profile config found for '{profile}'")
        return 0

    print_error(f"Unknown subcommand: {sub}")
    return 1


# ---------------------------------------------------------------------------
# PRD-015: Profile Templates
# ---------------------------------------------------------------------------

_REDACT_PATTERNS = re.compile(
    r"(api[_-]?key|secret|token|password|credential|auth|url)",
    re.IGNORECASE,
)


def _redact_env(key: str, val: str) -> str:
    if _REDACT_PATTERNS.search(key):
        return f"<{key.upper()}>"
    return val


def cmd_template(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(getattr(args, "config", None)))
    sub = getattr(args, "template_subcommand", None)

    if sub == "export" or sub is None:
        profile = getattr(args, "profile", None) or _default_master_profile(cfg)
        # Read the profile's real config/env from where the runtime stores them
        # (runtime_home/.hermes/profiles/<profile>), not the phantom tag_home dir.
        profile_dir = _profile_home(cfg, profile)
        env_file = profile_dir / ".env"
        cfg_file = profile_dir / "config.yaml"

        template: dict[str, Any] = {
            "name": profile,
            "version": "1",
            "description": f"TAG profile template for '{profile}'",
            "env": {},
            "config": {},
        }

        if env_file.exists():
            for line in env_file.read_text().splitlines():
                line = line.strip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                k, _, v = line.partition("=")
                template["env"][k.strip()] = _redact_env(k.strip(), v.strip())

        if cfg_file.exists():
            with cfg_file.open() as fh:
                template["config"] = yaml.safe_load(fh) or {}

        out_path = getattr(args, "output", None)
        yaml_text = yaml.dump(template, default_flow_style=False, sort_keys=False)
        if out_path:
            Path(out_path).write_text(yaml_text)
            print_success(f"Template exported to {out_path}")
        else:
            print(yaml_text)
        return 0

    if sub == "import":
        tmpl_path = args.template_file
        with open(tmpl_path) as fh:
            tmpl = yaml.safe_load(fh)
        if not isinstance(tmpl, dict):
            print_error(f"Template file '{tmpl_path}' does not contain a valid YAML mapping")
            return 1
        import re as _re
        profile = str(getattr(args, "profile", None) or tmpl.get("name") or "imported").strip()
        # A profile name maps to a directory under TAG_HOME/profiles. Reject
        # anything that isn't a plain slug so a crafted `name:` in an untrusted
        # template cannot escape the profiles dir (path traversal / absolute
        # path) or inject separators/newlines.
        if not _re.match(r"^[A-Za-z0-9][A-Za-z0-9._-]*$", profile):
            print_error(
                f"Invalid profile name: {profile!r} "
                "(use letters, digits, dot, dash, underscore; no path separators)."
            )
            return 1
        # Create the profile where the runtime reads profiles from
        # (runtime_home/.hermes/profiles/<profile>), so the imported profile is
        # actually visible to the agent.
        profile_dir = _profile_home(cfg, profile)
        if profile_dir.exists():
            print_error(f"Profile '{profile}' already exists; choose a different --profile name.")
            return 1
        profile_dir.mkdir(parents=True, exist_ok=True)

        env_data = tmpl.get("env", {})
        if env_data:
            env_file = profile_dir / ".env"
            lines = []
            for k, v in env_data.items():
                if str(v).startswith("<") and str(v).endswith(">"):
                    lines.append(f"# {k}=<fill in>")
                else:
                    lines.append(f"{k}={v}")
            # Create the .env with 0600 *before* writing secrets, so an imported
            # ANTHROPIC_API_KEY etc. is never briefly world/group readable (0644).
            import os as _os
            fd = _os.open(str(env_file), _os.O_WRONLY | _os.O_CREAT | _os.O_TRUNC, 0o600)
            try:
                _os.write(fd, ("\n".join(lines) + "\n").encode())
            finally:
                _os.close(fd)
            _os.chmod(env_file, 0o600)

        cfg_data = tmpl.get("config", {})
        if cfg_data:
            _write_yaml(profile_dir / "config.yaml", cfg_data, force=True)

        print_success(f"Template imported as profile '{profile}'")
        return 0

    if sub == "fetch":
        url = args.url
        try:
            _validate_fetch_url(url)
        except ValueError as exc:
            print_error(f"Refused to fetch template: {exc}")
            return 1
        try:
            with _safe_urlopen(url, timeout=15) as resp:  # noqa: S310
                tmpl_text = resp.read().decode()
        except urllib.error.URLError as exc:
            print_error(f"Failed to fetch template: {exc}")
            return 1
        print(tmpl_text)
        return 0

    print_error(f"Unknown subcommand: {sub}")
    return 1


# ---------------------------------------------------------------------------
# PRD-016: Webhook Event Hooks
# ---------------------------------------------------------------------------

def _interpolate(template: str, payload: dict[str, Any], *, shell_safe: bool = False) -> str:
    import shlex
    for k, v in payload.items():
        # Payload values are data (e.g. event title/body) and may be
        # attacker-influenced. When the result is fed to a shell, quote each
        # substituted value so it can't break out of its argument / inject
        # commands (`; rm -rf ~`, `$(...)`, backticks, ...).
        rendered = shlex.quote(str(v)) if shell_safe else str(v)
        template = template.replace(f"{{{{{k}}}}}", rendered)
    return template


def _execute_hook(hook: dict[str, Any], payload: dict[str, Any]) -> bool:
    hook_type = hook.get("type", "shell")
    if hook_type == "shell":
        cmd_str = _interpolate(hook.get("command", ""), payload, shell_safe=True)
        try:
            result = subprocess.run(cmd_str, shell=True, capture_output=True, text=True, timeout=30)
            return result.returncode == 0
        except subprocess.TimeoutExpired:
            return False
    if hook_type == "webhook":
        url = hook.get("url", "")
        # Share the SSRF guard used by template fetch / notifications so a
        # config-driven webhook hook cannot reach loopback/link-local/metadata.
        try:
            _validate_fetch_url(url)
        except ValueError:
            return False
        data = json.dumps(payload).encode()
        req = urllib.request.Request(
            url, data=data, method="POST",
            headers={"Content-Type": "application/json"},
        )
        try:
            with _safe_urlopen(req, timeout=10):
                return True
        except urllib.error.URLError:
            return False
    return False


def _fire_hooks(cfg: dict[str, Any], event_type: str, payload: dict[str, Any], db_path: Path | None = None) -> int:
    hooks: list[dict[str, Any]] = cfg.get("hooks", {}).get(event_type, [])
    if not hooks:
        return 0
    fired = 0
    for hook in hooks:
        exc_msg: str | None = None
        try:
            ok = _execute_hook(hook, payload)
        except Exception as exc:
            ok = False
            exc_msg = str(exc)
        if ok:
            fired += 1
        if db_path:
            try:
                conn = sqlite3.connect(str(db_path))
                conn.execute(
                    "INSERT INTO hook_log (id, hook_name, event_id, status, response, fired_at) "
                    "VALUES (?,?,?,?,?,datetime('now'))",
                    (uuid.uuid4().hex[:12], hook.get("name", ""), event_type,
                     "ok" if ok else "error", exc_msg),
                )
                conn.commit()
                conn.close()
            except Exception:
                pass
    return fired


def cmd_hooks(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(getattr(args, "config", None)))
    sub = getattr(args, "hooks_subcommand", None)

    if sub == "list" or sub is None:
        hooks_cfg: dict[str, Any] = cfg.get("hooks", {})
        if getattr(args, "json", False):
            print(json.dumps(hooks_cfg, indent=2))
            return 0
        if not hooks_cfg:
            print("No hooks configured.")
            return 0
        for event_type, hook_list in hooks_cfg.items():
            print(f"\n  {event_type}:")
            for h in hook_list:
                print(f"    - {h.get('name', '(unnamed)')}: {h.get('type', 'shell')}")
        return 0

    if sub == "log":
        db_path = _runtime_db_path(cfg)
        if not db_path.exists():
            print("No hook log found.")
            return 0
        conn = sqlite3.connect(str(db_path))
        try:
            rows = conn.execute(
                "SELECT id, hook_name, event_id, status, response, fired_at "
                "FROM hook_log ORDER BY fired_at DESC LIMIT ?",
                (getattr(args, "limit", 50),),
            ).fetchall()
        finally:
            conn.close()
        if getattr(args, "json", False):
            print(json.dumps([
                {"id": r[0], "hook_name": r[1], "event_type": r[2],
                 "status": r[3], "response": r[4], "fired_at": r[5]}
                for r in rows
            ], indent=2))
            return 0
        print(f"{'ID':<14} {'Event':<20} {'Hook':<25} {'Status':<8} {'Time'}")
        print("-" * 90)
        for r in rows:
            print(f"  {r[0]:<12} {r[1]:<20} {r[2]:<25} {r[3]:<8} {r[5]}")
        return 0

    if sub == "test":
        event_type = args.event_type
        payload = {"event_type": event_type, "test": "true", "timestamp": str(dt.datetime.now(dt.timezone.utc))}
        db_path = _runtime_db_path(cfg)
        # Ensure the hook_log schema exists so fired test hooks are recorded.
        try:
            open_db(cfg).close()
        except Exception:
            pass
        fired = _fire_hooks(cfg, event_type, payload, db_path=db_path)
        if fired == 0:
            print_warning(f"No hooks matched event '{event_type}'")
        print_success(f"Fired {fired} hook(s) for event '{event_type}'")
        return 0

    print_error(f"Unknown subcommand: {sub}")
    return 1


# ---------------------------------------------------------------------------
# PRD-017: Multi-Model Comparison
# ---------------------------------------------------------------------------

def cmd_compare(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(getattr(args, "config", None)))
    db_path = _runtime_db_path(cfg)
    sub = getattr(args, "compare_subcommand", None)

    if sub == "list" or sub is None:
        if not db_path.exists():
            print("No benchmark database found.")
            return 0
        conn = sqlite3.connect(str(db_path))
        try:
            rows = conn.execute(
                "SELECT id, suite_path, created_at, status, models FROM benchmark_comparisons "
                "ORDER BY created_at DESC LIMIT ?",
                (getattr(args, "limit", 20),),
            ).fetchall()
        finally:
            conn.close()
        if getattr(args, "json", False):
            print(json.dumps([
                {"id": r[0], "suite_path": r[1], "created_at": r[2], "status": r[3], "models": r[4]}
                for r in rows
            ], indent=2))
            return 0
        print(f"{'ID':<14} {'Suite':<40} {'Status':<12} {'Created'}")
        print("-" * 90)
        for r in rows:
            print(f"  {r[0]:<12} {r[1]:<40} {r[3]:<12} {r[2]}")
        return 0

    if sub == "show":
        comparison_id = args.comparison_id
        if not db_path.exists():
            if getattr(args, "json", False):
                print(json.dumps({"error": "no benchmark database found", "id": comparison_id}))
            else:
                print_error("No benchmark database found.")
            return 1
        conn = sqlite3.connect(str(db_path))
        try:
            meta = conn.execute(
                "SELECT id, suite_path, created_at, status, models FROM benchmark_comparisons WHERE id = ?",
                (comparison_id,),
            ).fetchone()
            if not meta:
                if getattr(args, "json", False):
                    print(json.dumps({"error": f"comparison {comparison_id!r} not found", "id": comparison_id}))
                else:
                    print_error(f"Comparison '{comparison_id}' not found")
                return 1
            results = conn.execute(
                "SELECT model_id, case_id, quality_score, latency_ms, prompt_tokens, completion_tokens, output "
                "FROM benchmark_results WHERE comparison_id = ? ORDER BY case_id, quality_score DESC",
                (comparison_id,),
            ).fetchall()
        finally:
            conn.close()
        if getattr(args, "json", False):
            print(json.dumps({
                "id": meta[0], "suite_path": meta[1], "created_at": meta[2],
                "status": meta[3], "models": meta[4],
                "results": [
                    {"model_id": r[0], "case_id": r[1], "quality_score": r[2],
                     "latency_ms": r[3], "prompt_tokens": r[4], "completion_tokens": r[5]}
                    for r in results
                ],
            }, indent=2))
            return 0
        print(f"Comparison: {meta[1]} (id={meta[0]})")
        print(f"Status:     {meta[3]}  |  Created: {meta[2]}")
        print(f"Models:     {meta[4]}")
        print(f"\n{'Model':<40} {'Case':<25} {'Score':>6} {'Latency':>10}")
        print("-" * 90)
        for r in results:
            print(f"  {r[0]:<38} {r[1]:<25} {r[2] or '-':>6} {(str(r[3]) + 'ms') if r[3] else 'n/a':>10}")
        return 0

    if sub == "run":
        profile = getattr(args, "profile", None) or _default_master_profile(cfg)
        model_refs = getattr(args, "model_ref", [])
        suite_path = getattr(args, "suite", None)
        if not model_refs:
            print_error("Provide at least one --model-ref")
            return 1
        if not suite_path:
            print_error("Provide --suite <path>")
            return 1
        with open(suite_path) as fh:
            suite = yaml.safe_load(fh) or {}
        cases = suite.get("cases", [])
        if not cases:
            print_error("Suite has no cases")
            return 1

        comparison_id = uuid.uuid4().hex[:12]
        comparison_name = suite.get("name", Path(suite_path).stem)
        conn = open_db(cfg)
        conn.execute(
            "INSERT INTO benchmark_comparisons (id, suite_path, created_at, status, models) "
            "VALUES (?,?,datetime('now'),?,?)",
            (comparison_id, str(suite_path), "running", json.dumps(model_refs)),
        )
        conn.commit()

        for case in cases:
            case_name = case.get("name", "unnamed")
            prompt_text = case.get("prompt", "")
            for model_ref in model_refs:
                print(f"  Running case '{case_name}' with model '{model_ref}'...")
                env = _profile_exec_env(cfg, profile)
                env["HERMES_MODEL"] = model_ref
                start = time.monotonic()
                try:
                    result = subprocess.run(
                        [str(_hermes_bin(cfg)), "chat", "-q", prompt_text, "-Q"],
                        env=env, capture_output=True, text=True, timeout=120,
                    )
                    latency = int((time.monotonic() - start) * 1000)
                    output = result.stdout.strip()
                    score = None
                except subprocess.TimeoutExpired:
                    latency = 120000
                    output = "(timeout)"
                    score = 0
                result_id = uuid.uuid4().hex[:12]
                conn.execute(
                    "INSERT INTO benchmark_results (id, comparison_id, model_id, case_id, quality_score, "
                    "latency_ms, prompt_tokens, completion_tokens, output) VALUES (?,?,?,?,?,?,?,?,?)",
                    (result_id, comparison_id, model_ref, case_name, score, latency, 0, 0, output),
                )
                conn.commit()

        conn.close()
        print_success(f"Comparison '{comparison_name}' saved (id={comparison_id})")
        return 0

    print_error(f"Unknown subcommand: {sub}")
    return 1


# ---------------------------------------------------------------------------
# PRD-018: Context Window Management
# ---------------------------------------------------------------------------

def cmd_context(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(getattr(args, "config", None)))
    profile = getattr(args, "profile", None) or _default_master_profile(cfg)
    sub = getattr(args, "context_subcommand", None)

    if sub == "show" or sub is None:
        # hermes `sessions list` only supports --source/--limit (no --json).
        result = subprocess.run(
            [str(_hermes_bin(cfg)), "sessions", "list"],
            env=_profile_exec_env(cfg, profile),
            capture_output=True, text=True,
        )
        if result.returncode != 0:
            if getattr(args, "json", False):
                print(json.dumps({"error": result.stderr.strip() or "sessions list failed"}))
            else:
                print_error(f"Failed to list sessions: {result.stderr.strip()}")
            return 1
        try:
            sessions = json.loads(result.stdout)
        except json.JSONDecodeError:
            sessions = None
        if getattr(args, "json", False):
            if sessions is not None:
                print(json.dumps(sessions, indent=2))
            else:
                print(json.dumps({"raw": result.stdout.strip()}))
            return 0
        if sessions is None:
            # hermes printed a human-readable table — pass it through.
            if result.stdout.strip():
                print(result.stdout, end="")
            else:
                print(f"No active sessions for profile '{profile}'")
            return 0
        if not sessions:
            print(f"No active sessions for profile '{profile}'")
            return 0
        print(f"Sessions for profile '{profile}':")
        for sess in sessions[:20]:
            sid = sess.get("id", sess.get("session_id", "?"))
            tokens = sess.get("token_count", sess.get("tokens", "?"))
            model = sess.get("model_id", sess.get("model", "?"))
            print(f"  {sid:<40} tokens={tokens:<10} model={model}")
        return 0

    if sub == "compress":
        session_id = getattr(args, "session_id", None)
        if not session_id:
            print_error("Provide --session-id")
            return 1
        # hermes exposes context compression as `sessions optimize` (there is no
        # `sessions compress` subcommand).
        cmd_args = [str(_hermes_bin(cfg)), "sessions", "optimize", session_id]
        result = subprocess.run(
            cmd_args, env=_profile_exec_env(cfg, profile),
            capture_output=True, text=True,
        )
        if result.returncode != 0:
            print_error(f"Context compression failed: {result.stderr.strip()}")
            return 1
        print_success(f"Context compressed for session '{session_id}'")
        return 0

    if sub == "trim":
        session_id = getattr(args, "session_id", None)
        if not session_id:
            print_error("Provide --session-id")
            return 1
        # hermes has no `sessions trim`/`--keep-last`; `sessions optimize` is the
        # supported context-reduction subcommand.
        result = subprocess.run(
            [str(_hermes_bin(cfg)), "sessions", "optimize", session_id],
            env=_profile_exec_env(cfg, profile),
            capture_output=True, text=True,
        )
        if result.returncode != 0:
            print_error(f"Context trim failed: {result.stderr.strip()}")
            return 1
        print_success(f"Trimmed (optimized) session '{session_id}'")
        return 0

    print_error(f"Unknown subcommand: {sub}")
    return 1


# ---------------------------------------------------------------------------
# PRD-019: Natural Language Shell
# ---------------------------------------------------------------------------

def cmd_shell(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(getattr(args, "config", None)))
    profile = getattr(args, "profile", None) or _default_master_profile(cfg)
    try:
        from tag.shell_mode import run_shell
        return run_shell(cfg, profile)
    except ImportError as exc:
        print_error(f"Shell mode not available: {exc}")
        return 1


# ---------------------------------------------------------------------------
# PRD-031: Route Fallback Chains
# ---------------------------------------------------------------------------

def cmd_route_fallback(args: argparse.Namespace) -> int:
    """Manage model fallback chains for automatic provider switching."""
    cfg = load_config(config_path(getattr(args, "config", None)))
    _ensure_runtime_dirs(cfg)
    db = open_db(cfg)
    sub = getattr(args, "fallback_subcommand", "list")
    profile = getattr(args, "profile", None) or cfg.get("defaults", {}).get("master_profile", "orchestrator")

    if sub == "add":
        primary = (getattr(args, "primary", "") or "").strip()
        fallback = (getattr(args, "fallback", "") or "").strip()
        if not primary or not fallback:
            db.close()
            print_error("--primary and --fallback model IDs required")
            return 1
        if primary == fallback:
            db.close()
            print_error("--primary and --fallback must be different models")
            return 1
        condition = getattr(args, "condition", "context_overflow") or "context_overflow"
        valid_conditions = {"context_overflow", "error", "timeout", "cost_limit", "any"}
        if condition not in valid_conditions:
            db.close()
            print_error(f"--condition must be one of: {', '.join(sorted(valid_conditions))}")
            return 1
        # Preserve an explicit --priority 0 (don't let `or` clobber it) and bound it.
        priority = getattr(args, "priority", 1)
        if priority is None:
            priority = 1
        if priority < 0:
            db.close()
            print_error("--priority must be >= 0")
            return 1

        # Reject exact duplicates (profile, primary, fallback, condition).
        existing = db.execute(
            "SELECT id FROM route_fallbacks WHERE profile=? AND primary_model=? "
            "AND fallback_model=? AND condition=?",
            (profile, primary, fallback, condition),
        ).fetchone()
        if existing:
            db.close()
            print_error(
                f"Fallback already exists for {primary} -> {fallback} "
                f"(condition={condition}) in profile '{profile}'"
            )
            return 1

        # Reject a fallback that would close a cycle (e.g. A->B then B->A):
        # walk existing edges from the new fallback and see if it reaches primary.
        edges: dict[str, set[str]] = {}
        for r in db.execute(
            "SELECT primary_model, fallback_model FROM route_fallbacks WHERE profile=?",
            (profile,),
        ).fetchall():
            edges.setdefault(r[0], set()).add(r[1])
        stack = [fallback]
        seen: set[str] = set()
        creates_cycle = False
        while stack:
            node = stack.pop()
            if node == primary:
                creates_cycle = True
                break
            if node in seen:
                continue
            seen.add(node)
            stack.extend(edges.get(node, ()))
        if creates_cycle:
            db.close()
            print_error(
                f"Refusing to add fallback: {primary} -> {fallback} would create a cycle"
            )
            return 1

        fb_id = uuid.uuid4().hex[:8]
        db.execute(
            """INSERT INTO route_fallbacks(id, profile, primary_model, fallback_model,
               condition, priority, enabled, created_at)
               VALUES(?,?,?,?,?,?,1,?)""",
            (fb_id, profile, primary, fallback, condition, priority, _utc_now()),
        )
        db.commit()
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"id": fb_id, "profile": profile, "primary": primary, "fallback": fallback}))
        else:
            print(f"Fallback added: {fb_id}")
            print(f"  {primary} -> {fallback}  on: {condition}  priority: {priority}")
        return 0

    if sub == "list":
        rows = db.execute(
            """SELECT id, primary_model, fallback_model, condition, priority, enabled
               FROM route_fallbacks WHERE profile=? ORDER BY priority, created_at""",
            (profile,),
        ).fetchall()
        db.close()
        if getattr(args, "json", False):
            print(json.dumps([{
                "id": r[0], "primary": r[1], "fallback": r[2],
                "condition": r[3], "priority": r[4], "enabled": bool(r[5]),
            } for r in rows], indent=2))
            return 0
        if not rows:
            print(f"No fallback chains for profile '{profile}'.")
            return 0
        print(f"  {'ID':<10} {'PRIMARY':<36} {'FALLBACK':<36} {'CONDITION':<16} {'PRI':>4} {'EN':>4}")
        print("  " + "-" * 110)
        for r in rows:
            en = "Y" if r[5] else "N"
            print(f"  {r[0]:<10} {r[1]:<36} {r[2]:<36} {r[3]:<16} {r[4]:>4} {en:>4}")
        return 0

    if sub == "remove":
        fb_id = getattr(args, "fb_id", None)
        if not fb_id:
            db.close()
            print_error("FALLBACK_ID required")
            return 1
        cur = db.execute("DELETE FROM route_fallbacks WHERE id=? AND profile=?", (fb_id, profile))
        db.commit()
        db.close()
        if cur.rowcount == 0:
            print_error(f"Fallback '{fb_id}' not found for profile '{profile}'")
            return 1
        print(f"removed: {fb_id}")
        return 0

    if sub == "resolve":
        primary = (getattr(args, "primary", "") or "").strip()
        condition = getattr(args, "condition", "context_overflow") or "context_overflow"
        if not primary:
            db.close()
            print_error("--primary required")
            return 1
        row = db.execute(
            """SELECT fallback_model FROM route_fallbacks
               WHERE profile=? AND primary_model=? AND condition=? AND enabled=1
               ORDER BY priority LIMIT 1""",
            (profile, primary, condition),
        ).fetchone()
        db.close()
        if not row:
            # A valid query that simply has no fallback configured is not an
            # error (consistent with `list` on an empty profile) — exit 0.
            if getattr(args, "json", False):
                print(json.dumps({"primary": primary, "fallback": None, "condition": condition}))
            else:
                print(f"No fallback configured for {primary!r} on condition={condition!r}")
            return 0
        if getattr(args, "json", False):
            print(json.dumps({"primary": primary, "fallback": row[0], "condition": condition}))
        else:
            print(f"Fallback: {primary} -> {row[0]}  (condition: {condition})")
        return 0

    db.close()
    return 0


# ---------------------------------------------------------------------------
# Subparser registration
# ---------------------------------------------------------------------------

def register(sub: argparse._SubParsersAction) -> None:  # type: ignore[type-arg]
    """Register workflow management subcommands onto *sub*."""

    # ---- PRD-014: mcp-registry ----
    mcp_reg = sub.add_parser("mcp-registry", help="Browse and install curated MCP servers")
    mcp_reg_sub = mcp_reg.add_subparsers(dest="mcp_reg_subcommand")
    mcp_list = mcp_reg_sub.add_parser("list", help="List available MCP servers")
    mcp_list.add_argument("--category", help="Filter by category")
    mcp_list.add_argument("--json", action="store_true")
    mcp_install = mcp_reg_sub.add_parser("install", help="Install an MCP server globally")
    mcp_install.add_argument("server_name", metavar="NAME")
    mcp_enable = mcp_reg_sub.add_parser("enable", help="Enable an MCP server for a profile")
    mcp_enable.add_argument("server_name", metavar="NAME")
    mcp_enable.add_argument("--profile")
    mcp_disable = mcp_reg_sub.add_parser("disable", help="Disable an MCP server for a profile")
    mcp_disable.add_argument("server_name", metavar="NAME")
    mcp_disable.add_argument("--profile")
    for mp in [mcp_reg, mcp_list, mcp_install, mcp_enable, mcp_disable]:
        mp.set_defaults(func=cmd_mcp_registry)

    # ---- PRD-015: template ----
    tmpl = sub.add_parser("template", help="Export/import/fetch profile config templates")
    tmpl_sub = tmpl.add_subparsers(dest="template_subcommand")
    tmpl_export = tmpl_sub.add_parser("export", help="Export a profile as a YAML template")
    tmpl_export.add_argument("--profile")
    tmpl_export.add_argument("--output", "-o", metavar="FILE", help="Write to file instead of stdout")
    tmpl_import = tmpl_sub.add_parser("import", help="Import a YAML template as a new profile")
    tmpl_import.add_argument("template_file", metavar="FILE")
    tmpl_import.add_argument("--profile", help="Override profile name from template")
    tmpl_fetch = tmpl_sub.add_parser("fetch", help="Fetch a template from a URL")
    tmpl_fetch.add_argument("url", metavar="URL")
    for tp in [tmpl, tmpl_export, tmpl_import, tmpl_fetch]:
        tp.set_defaults(func=cmd_template)

    # ---- PRD-016: hooks ----
    hooks_cmd = sub.add_parser("hooks", help="Manage and test TAG lifecycle event hooks")
    hooks_sub = hooks_cmd.add_subparsers(dest="hooks_subcommand")
    hooks_list = hooks_sub.add_parser("list", help="List configured hooks")
    hooks_list.add_argument("--json", action="store_true")
    hooks_log = hooks_sub.add_parser("log", help="Show recent hook execution log")
    hooks_log.add_argument("--limit", type=_positive_int, default=50)
    hooks_log.add_argument("--json", action="store_true")
    hooks_test = hooks_sub.add_parser("test", help="Test-fire hooks for an event type")
    hooks_test.add_argument("event_type", metavar="EVENT")
    for hp in [hooks_cmd, hooks_list, hooks_log, hooks_test]:
        hp.set_defaults(func=cmd_hooks)

    # ---- PRD-017: compare ----
    compare = sub.add_parser("compare", help="Multi-model benchmark comparisons")
    compare_sub = compare.add_subparsers(dest="compare_subcommand")
    compare_list = compare_sub.add_parser("list", help="List saved comparisons")
    compare_list.add_argument("--limit", type=_positive_int, default=20)
    compare_list.add_argument("--json", action="store_true")
    compare_show = compare_sub.add_parser("show", help="Show comparison results")
    compare_show.add_argument("comparison_id", metavar="ID")
    compare_show.add_argument("--json", action="store_true")
    compare_run = compare_sub.add_parser("run", help="Run a new multi-model comparison")
    compare_run.add_argument("--profile")
    compare_run.add_argument("--suite", required=True, help="Path to benchmark suite YAML")
    compare_run.add_argument("--model-ref", action="append", default=[], metavar="REF",
                             help="Model reference (provider/model-id); repeat for multiple")
    compare_run.add_argument("--json", action="store_true")
    for cp in [compare, compare_list, compare_show, compare_run]:
        cp.set_defaults(func=cmd_compare)

    # ---- PRD-018: context ----
    context_cmd = sub.add_parser("context", help="Manage agent context window size")
    context_sub = context_cmd.add_subparsers(dest="context_subcommand")
    ctx_show = context_sub.add_parser("show", help="List active sessions and their token counts")
    ctx_show.add_argument("--profile")
    ctx_show.add_argument("--json", action="store_true")
    ctx_compress = context_sub.add_parser("compress", help="Summarize and compress a session context")
    ctx_compress.add_argument("--profile")
    ctx_compress.add_argument("--session-id", required=True, dest="session_id")
    ctx_trim = context_sub.add_parser("trim", help="Trim a session to the last N turns")
    ctx_trim.add_argument("--profile")
    ctx_trim.add_argument("--session-id", required=True, dest="session_id")
    ctx_trim.add_argument("--keep-last", type=_positive_int, default=10, dest="keep_last")
    for ctx_p in [context_cmd, ctx_show, ctx_compress, ctx_trim]:
        ctx_p.set_defaults(func=cmd_context)

    # ---- PRD-019: shell ----
    shell_cmd = sub.add_parser("shell", help="Open interactive natural-language TAG shell")
    shell_cmd.add_argument("--profile", help="Profile to use (default: orchestrator)")
    shell_cmd.set_defaults(func=cmd_shell)

    # ---- PRD-031: route-fallback ----
    route_fallback_cmd = sub.add_parser("route-fallback", help="Manage model fallback chains")
    rf_sub = route_fallback_cmd.add_subparsers(dest="fallback_subcommand")
    rf_add = rf_sub.add_parser("add", help="Add a fallback chain")
    rf_add.add_argument("--primary", required=True, help="Primary model ID")
    rf_add.add_argument("--fallback", required=True, help="Fallback model ID")
    rf_add.add_argument("--condition", default="context_overflow",
                        choices=["context_overflow", "error", "timeout", "cost_limit", "any"])
    rf_add.add_argument("--priority", type=int, default=1)
    rf_add.add_argument("--profile")
    rf_add.add_argument("--json", action="store_true")
    rf_list = rf_sub.add_parser("list", help="List fallback chains")
    rf_list.add_argument("--profile")
    rf_list.add_argument("--json", action="store_true")
    rf_remove = rf_sub.add_parser("remove", help="Remove a fallback chain")
    rf_remove.add_argument("fb_id", metavar="FALLBACK_ID")
    rf_remove.add_argument("--profile")
    rf_resolve = rf_sub.add_parser("resolve", help="Show which fallback would be used")
    rf_resolve.add_argument("--primary", required=True)
    rf_resolve.add_argument("--condition", default="context_overflow")
    rf_resolve.add_argument("--profile")
    rf_resolve.add_argument("--json", action="store_true")
    for rfp in [route_fallback_cmd, rf_add, rf_list, rf_remove, rf_resolve]:
        rfp.set_defaults(func=cmd_route_fallback)
