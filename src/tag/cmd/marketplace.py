"""Marketplace, eval framework, sandbox, and web dashboard commands."""
from __future__ import annotations

import argparse
import json
import os
import sys
import time
import threading
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any

from tag.core.config import load_config, config_path
from tag.core.paths import runtime_db_path, hermes_root, tag_home, runtime_home, profile_home, ensure_runtime_dirs
from tag.core.db import open_db
from tag.core.utils import nonnegative_int, utc_now

try:
    from tag.tui_output import print_error, print_success, print_warning
except Exception:
    def print_error(msg): print(f"error: {msg}", file=sys.stderr)
    def print_success(msg): print(msg)
    def print_warning(msg): print(f"warning: {msg}", file=sys.stderr)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _profile_sha256(path: Path) -> str:
    import hashlib
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _validate_profile_name(name: str) -> str:
    """Reject path separators / traversal / absolute paths in a profile name.

    A profile name maps to a directory/file under a profiles root, so anything
    other than a plain slug could be used to write outside that root
    (e.g. ``../PWNED`` or ``/tmp/ABSPWN``).
    """
    import re
    if not re.match(r"^[A-Za-z0-9][A-Za-z0-9._-]*$", name or ""):
        raise ValueError(
            f"Invalid profile name: {name!r} "
            "(use letters, digits, dot, dash, underscore; no path separators)."
        )
    return name


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
        # Hostname — resolve best-effort. If resolution fails the connection
        # will fail anyway, so don't block on that.
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

    Two SSRF holes the plain ``urllib.request.urlopen`` leaves open are closed:

    * **Redirect following** — the default global opener transparently follows
      3xx redirects, so a public ``302 -> http://127.0.0.1/`` reaches internal
      services. Here every redirect target is re-validated with
      :func:`_validate_fetch_url`, and each hop opens a fresh (also pinned)
      connection.
    * **DNS rebinding (TOCTOU)** — validation and connection normally resolve
      DNS independently. The pinned connection re-resolves at connect time and
      refuses to connect to any non-public address, so a low-TTL record cannot
      flip to loopback/metadata between validate and fetch.
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


def _dashboard_snapshot(cfg: dict[str, Any]) -> dict[str, Any]:
    """Read current TAG state for dashboard display — pure SQLite, no hermes."""
    from tag.core.db import queue_list_jobs
    snap: dict[str, Any] = {"runs": [], "queue": [], "journal_count": 0, "kanban": {}}
    try:
        db = open_db(cfg)
        rows = db.execute(
            "SELECT id AS run_id, kind, task_type, master_profile, status, "
            "created_at FROM runs ORDER BY created_at DESC LIMIT 20"
        ).fetchall()
        snap["runs"] = [dict(r) for r in rows]
        snap["queue"] = queue_list_jobs(db, status=None)
        snap["journal_count"] = db.execute(
            "SELECT COUNT(*) FROM memory_journal"
        ).fetchone()[0]
        db.close()
    except Exception:
        pass

    kanban_by_profile: dict[str, Any] = {}
    try:
        from tag import kanban as _kanban
        for pname in cfg.get("profiles", {}):
            try:
                kpath = _kanban.profile_kanban_db_path(cfg, pname)
                if not kpath.exists():
                    continue
                kconn = _kanban.open_db(kpath)
                tasks = _kanban.list_tasks(kconn)
                kconn.close()
                by_status: dict[str, int] = {}
                for t in tasks:
                    by_status[t["status"]] = by_status.get(t["status"], 0) + 1
                kanban_by_profile[pname] = {"total": len(tasks), "by_status": by_status}
            except Exception:
                pass
    except Exception:
        pass
    snap["kanban"] = kanban_by_profile
    return snap


def _dashboard_html(profile: str) -> str:
    """Minimal HTML page that connects to the SSE stream and renders a live table."""
    return f"""<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>TAG Dashboard — {profile}</title>
<style>
body{{font-family:monospace;background:#111;color:#eee;padding:16px}}
h1{{color:#7ec8e3}}table{{border-collapse:collapse;width:100%;margin:8px 0}}
th{{background:#222;color:#7ec8e3;padding:6px 10px;text-align:left}}
td{{padding:4px 10px;border-bottom:1px solid #333}}
.ok{{color:#5fbb5f}}.fail{{color:#e05252}}.run{{color:#e0c000}}
#ts{{float:right;color:#888;font-size:0.8em}}
</style></head>
<body>
<h1>TAG Dashboard <span id=ts></span></h1>
<h2>Recent Runs</h2><table id=runs><tr><th>ID</th><th>Profile</th><th>Status</th><th>When</th></tr></table>
<h2>Queue</h2><table id=queue><tr><th>Job</th><th>Status</th><th>Task</th></tr></table>
<script>
const es=new EventSource('/events');
es.onmessage=e=>{{
  const d=JSON.parse(e.data);
  document.getElementById('ts').textContent=new Date().toLocaleTimeString();
  const runs=document.getElementById('runs');
  runs.innerHTML='<tr><th>ID</th><th>Profile</th><th>Status</th><th>When</th></tr>';
  (d.runs||[]).slice(0,10).forEach(r=>{{
    const cls=r.status==='completed'?'ok':r.status==='failed'?'fail':'run';
    const when=(r.created_at||'').substring(11,16);
    runs.innerHTML+=`<tr><td>${{r.run_id}}</td><td>${{r.master_profile}}</td><td class="${{cls}}">${{r.status}}</td><td>${{when}}</td></tr>`;
  }});
  const q=document.getElementById('queue');
  q.innerHTML='<tr><th>Job</th><th>Status</th><th>Task</th></tr>';
  (d.queue||[]).slice(0,8).forEach(j=>{{
    const cls=j.status==='done'?'ok':j.status==='failed'?'fail':'run';
    q.innerHTML+=`<tr><td>${{j.id}}</td><td class="${{cls}}">${{j.status}}</td><td>${{(j.task||'').substring(0,60)}}</td></tr>`;
  }});
}};
</script></body></html>"""


# ---------------------------------------------------------------------------
# PRD-026: Profile Marketplace
# ---------------------------------------------------------------------------

def cmd_profile_marketplace(args: argparse.Namespace) -> int:
    """PRD-026: Pull/push profiles from/to GitHub Gist or URL."""
    import hashlib
    import uuid
    import yaml

    cfg = load_config(config_path(getattr(args, "config", None)))
    ensure_runtime_dirs(cfg)
    db = open_db(cfg)
    sub = getattr(args, "marketplace_subcommand", None)

    if sub == "pull":
        url = getattr(args, "url", "")
        if not url:
            db.close()
            print_error("URL required (e.g. https://raw.githubusercontent.com/user/repo/main/profile.yaml)")
            return 1
        name = getattr(args, "name", None) or Path(url).stem
        try:
            name = _validate_profile_name(name)
        except ValueError as exc:
            db.close()
            print_error(str(exc))
            return 1

        try:
            _validate_fetch_url(url)
        except ValueError as exc:
            db.close()
            print_error(f"Refused to fetch profile: {exc}")
            return 1

        try:
            response = _safe_urlopen(url, timeout=15)  # noqa: S310
            content = response.read()
        except urllib.error.URLError as exc:
            db.close()
            print_error(f"Failed to fetch profile: {exc}")
            return 1

        # Basic YAML validation
        try:
            profile_data = yaml.safe_load(content)
            if not isinstance(profile_data, dict):
                raise ValueError("Profile must be a YAML mapping")
        except Exception as exc:
            db.close()
            print_error(f"Invalid profile YAML: {exc}")
            return 1

        sha = hashlib.sha256(content).hexdigest()
        # Store where the runtime actually reads profiles from
        # (runtime_home/.hermes/profiles/<name>/config.yaml), consistent with
        # `template import` and `mcp-registry enable`.
        profile_dir = profile_home(cfg, name)
        profile_dir.mkdir(parents=True, exist_ok=True)
        local_path = profile_dir / "config.yaml"
        local_path.write_bytes(content)

        now = utc_now()
        db.execute(
            """INSERT INTO profile_cache(id, name, source_url, sha256, local_path, downloaded_at)
               VALUES(?,?,?,?,?,?)
               ON CONFLICT(name) DO UPDATE SET
                 source_url=excluded.source_url, sha256=excluded.sha256,
                 local_path=excluded.local_path, downloaded_at=excluded.downloaded_at""",
            (uuid.uuid4().hex[:12], name, url, sha, str(local_path), now),
        )
        db.commit()
        db.close()

        if getattr(args, "json", False):
            print(json.dumps({"name": name, "sha256": sha, "local_path": str(local_path)}))
        else:
            print(f"Pulled profile: {name}")
            print(f"  SHA256: {sha[:16]}...")
            print(f"  Saved to: {local_path}")
        return 0

    if sub == "push":
        profile_name = getattr(args, "profile_name", None)
        if not profile_name:
            db.close()
            print_error("profile name required")
            return 1
        # Validate the name is a plain slug before joining it into a path, so a
        # traversal name (e.g. ../../../tmp/secret) cannot disclose the path +
        # SHA256 of an arbitrary config.yaml. Symmetric with `pull` (:_validate_profile_name).
        try:
            profile_name = _validate_profile_name(profile_name)
        except ValueError as exc:
            db.close()
            print_error(str(exc))
            return 1
        # Find the profile file — either a pulled profile (flat profiles/<name>.yaml)
        # or a bootstrapped profile (.hermes/profiles/<name>/config.yaml).
        profiles_dir = runtime_home(cfg) / "profiles"
        candidates = list(profiles_dir.glob(f"{profile_name}.yaml"))
        if not candidates:
            bootstrapped = runtime_home(cfg) / ".hermes" / "profiles" / profile_name / "config.yaml"
            if bootstrapped.exists():
                candidates = [bootstrapped]
        if not candidates:
            db.close()
            print_error(
                f"Profile not found: {profile_name!r} (looked in {profiles_dir}/ and "
                f"{runtime_home(cfg)}/.hermes/profiles/{profile_name}/)"
            )
            return 1
        pfile = candidates[0]
        sha = _profile_sha256(pfile)
        db.close()
        # For now, print info — actual GitHub Gist push requires auth token
        print(f"Profile: {profile_name}")
        print(f"  File: {pfile}")
        print(f"  SHA256: {sha}")
        print("  To push: gh gist create --public --filename profile.yaml " + str(pfile))
        return 0

    if sub == "list":
        rows = db.execute(
            "SELECT name, source_url, sha256, downloaded_at FROM profile_cache ORDER BY name"
        ).fetchall()
        db.close()
        if getattr(args, "json", False):
            print(json.dumps([{"name": r[0], "source_url": r[1], "sha256": r[2][:12], "downloaded_at": r[3]} for r in rows], indent=2))
            return 0
        if not rows:
            print("No cached profiles. Use `tag marketplace pull <url>` to add one.")
            return 0
        for r in rows:
            print(f"  {r[0]:<24} {r[3][:10]}  {r[1][:60]}")
        return 0

    db.close()
    if sub is None:
        print_error("marketplace: a subcommand is required (pull, push, list)")
    else:
        print_error(f"Unknown marketplace subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# PRD-027: Eval Framework
# ---------------------------------------------------------------------------

def cmd_eval(args: argparse.Namespace) -> int:
    """PRD-027: Run eval suites against TAG profiles."""
    import subprocess

    cfg = load_config(config_path(getattr(args, "config", None)))
    ensure_runtime_dirs(cfg)
    db = open_db(cfg)
    sub = getattr(args, "eval_subcommand", "list")

    try:
        from tag.eval_framework import (
            load_suite, score_case, create_eval_run,
            record_case_result, finalize_eval_run,
            list_eval_runs, get_eval_run_detail,
        )
    except ImportError as exc:
        db.close()
        print_error(f"tag.eval_framework not available: {exc}")
        return 1

    if sub == "run":
        suite_path_str = getattr(args, "suite", None)
        if not suite_path_str:
            db.close()
            print_error("--suite SUITE_PATH required")
            return 1
        suite_path = Path(suite_path_str)
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        dry_run = getattr(args, "dry_run", False)

        try:
            suite = load_suite(suite_path)
        except (FileNotFoundError, ValueError) as exc:
            db.close()
            print_error(str(exc))
            return 1

        suite_name = suite.get("name", suite_path.stem)
        run_id = create_eval_run(db, str(suite_path), profile, suite_name)
        cases = suite.get("cases", [])

        if not dry_run:
            print(f"Eval run: {run_id}  suite: {suite_name}  profile: {profile}")
            print(f"Running {len(cases)} cases...")

        passed = 0
        failed = 0
        for case in cases:
            case_id = case.get("id", f"case_{cases.index(case)+1}")
            input_text = case.get("input", "")

            if dry_run:
                output = "(dry-run — no agent invocation)"
                ok, score, reason = True, 1.0, None
            else:
                # Run the case via hermes
                result = subprocess.run(
                    [sys.executable, "-m", "tag", "--config",
                     str(config_path(getattr(args, "config", None)) or ""),
                     "submit", "--task-type", "mixed", "--prompt", input_text,
                     "--master-profile", profile, "--source", "eval"],
                    capture_output=True, text=True, timeout=300,
                )
                output = result.stdout
                ok, score, reason = score_case(case, output)

            record_case_result(
                db, run_id, case_id, input_text, output,
                passed=ok, score=score, failure_reason=reason,
            )
            if ok:
                passed += 1
            else:
                failed += 1
            status_char = "✓" if ok else "✗"
            if not dry_run:
                print(f"  [{status_char}] {case_id}  score={score:.2f}" +
                      (f"  {reason}" if reason else ""))

        summary = finalize_eval_run(db, run_id)
        db.close()

        if getattr(args, "json", False):
            print(json.dumps(summary, indent=2))
        else:
            print(f"\nResults: {passed}/{len(cases)} passed")
        return 0 if failed == 0 else 1

    if sub == "list":
        runs = list_eval_runs(db)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(runs, indent=2))
            return 0
        if not runs:
            print("No eval runs yet.")
            return 0
        print(f"  {'ID':<18} {'SUITE':<24} {'PROFILE':<14} {'STATUS':<10} {'PASS':<6} {'FAIL':<6}")
        print("  " + "─" * 80)
        for r in runs:
            print(f"  {r['id']:<18} {r['suite_name'][:24]:<24} {r['profile']:<14} "
                  f"{r['status']:<10} {r['pass_count']:<6} {r['fail_count']:<6}")
        return 0

    if sub == "show":
        run_id = getattr(args, "run_id", None)
        if not run_id:
            db.close()
            print_error("RUN_ID required")
            return 1
        detail = get_eval_run_detail(db, run_id)
        db.close()
        if not detail:
            print_error(f"Eval run '{run_id}' not found")
            return 1
        if getattr(args, "json", False):
            print(json.dumps(detail, indent=2))
            return 0
        print(f"Eval run: {detail['id']}")
        print(f"  Suite: {detail['suite_name']}  Profile: {detail['profile']}")
        print(f"  Status: {detail['status']}  {detail['pass_count']}/{detail['total_count']} passed")
        for c in detail.get("cases", []):
            icon = "✓" if c["passed"] else "✗"
            reason = f"  — {c['failure_reason']}" if c.get("failure_reason") else ""
            print(f"  [{icon}] {c['case_id']}  score={c['score']:.2f}{reason}")
        return 0

    db.close()
    return 0


# ---------------------------------------------------------------------------
# PRD-028: Sandbox Code Execution
# ---------------------------------------------------------------------------

def cmd_sandbox(args: argparse.Namespace) -> int:
    """PRD-028: Isolated code execution via restricted subprocess or Docker."""
    cfg = load_config(config_path(getattr(args, "config", None)))
    ensure_runtime_dirs(cfg)
    db = open_db(cfg)
    sub = getattr(args, "sandbox_subcommand", "list")

    try:
        from tag.sandbox import run_in_sandbox, list_sandbox_runs, get_sandbox_run
    except ImportError as exc:
        db.close()
        print_error(f"tag.sandbox not available: {exc}")
        return 1

    if sub == "run":
        command = getattr(args, "command", "")
        if not command:
            db.close()
            print_error("COMMAND required")
            return 1
        backend = getattr(args, "backend", "restricted") or "restricted"
        image = getattr(args, "image", "python:3.12-slim") or "python:3.12-slim"
        timeout = args.timeout if getattr(args, "timeout", None) is not None else 60

        try:
            result = run_in_sandbox(db, command, backend=backend, image=image, timeout=timeout)
        except ValueError as exc:
            db.close()
            print_error(str(exc))
            return 1
        db.close()

        if getattr(args, "json", False):
            out = {k: v for k, v in result.items() if k != "output"}
            out["output_preview"] = (result.get("output") or "")[:200]
            print(json.dumps(out, indent=2))
        else:
            print(f"Sandbox run: {result['id']}  exit={result['exit_code']}")
            if result.get("output"):
                print("--- output ---")
                print(result["output"][:2000])
        return 0 if result.get("exit_code") == 0 else 1

    if sub == "list":
        runs = list_sandbox_runs(db, limit=20)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(runs, indent=2))
            return 0
        if not runs:
            print("No sandbox runs.")
            return 0
        print(f"  {'ID':<14} {'BACKEND':<12} {'STATUS':<10} {'EXIT':<5} {'COMMAND'}")
        print("  " + "─" * 70)
        for r in runs:
            ec = str(r["exit_code"]) if r["exit_code"] is not None else "?"
            print(f"  {r['id']:<14} {r['backend']:<12} {r['status']:<10} {ec:<5} {r['command']}")
        return 0

    if sub == "result":
        run_id = getattr(args, "run_id", None)
        if not run_id:
            db.close()
            print_error("RUN_ID required")
            return 1
        run = get_sandbox_run(db, run_id)
        db.close()
        if not run:
            print_error(f"Sandbox run '{run_id}' not found")
            return 1
        if getattr(args, "json", False):
            print(json.dumps(run, indent=2))
        else:
            print(f"Sandbox run: {run['id']}  backend: {run['backend']}  exit: {run['exit_code']}")
            print(run.get("output") or "(no output)")
        return 0

    db.close()
    return 0


# ---------------------------------------------------------------------------
# PRD-029: Streaming TUI Dashboard (tag serve)
# ---------------------------------------------------------------------------

def cmd_serve(args: argparse.Namespace) -> int:
    """PRD-029: Start a local HTTP server serving the TAG dashboard as SSE stream."""
    import http.server

    cfg = load_config(config_path(getattr(args, "config", None)))
    ensure_runtime_dirs(cfg)
    port = getattr(args, "port", 7880) or 7880
    profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]

    try:
        class _Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, fmt, *a):
                pass  # Silence default access log

            def do_GET(self):
                if self.path == "/events":
                    self._serve_sse()
                elif self.path == "/" or self.path == "/index.html":
                    self._serve_html()
                else:
                    self.send_error(404)

            def _serve_html(self):
                html = _dashboard_html(profile)
                self.send_response(200)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.send_header("Content-Length", str(len(html.encode())))
                self.end_headers()
                self.wfile.write(html.encode())

            def _serve_sse(self):
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Cache-Control", "no-cache")
                # No wildcard CORS on this local data stream (it carries run/queue
                # task text, journal + kanban counts). A wildcard ACAO would let any
                # web page EventSource this 127.0.0.1 endpoint cross-origin,
                # defeating the loopback bind. Consistent with api.py / devui.py.
                self.end_headers()
                try:
                    while True:
                        snap = _dashboard_snapshot(cfg)
                        data = json.dumps(snap)
                        msg = f"data: {data}\n\n"
                        self.wfile.write(msg.encode())
                        self.wfile.flush()
                        time.sleep(3)
                except (BrokenPipeError, ConnectionResetError):
                    pass

        server = http.server.ThreadingHTTPServer(("127.0.0.1", port), _Handler)
        url = f"http://127.0.0.1:{port}"
        print(f"TAG dashboard server: {url}  (Ctrl+C to stop)")

        # Try to open browser
        try:
            import webbrowser
            threading.Timer(0.5, lambda: webbrowser.open(url)).start()
        except Exception:
            pass

        server.serve_forever()
    except KeyboardInterrupt:
        print("\nServer stopped.")
    return 0


# ---------------------------------------------------------------------------
# PRD-035: IDE Bridge / LSP
# ---------------------------------------------------------------------------

def cmd_lsp(args: argparse.Namespace) -> int:
    """PRD-035: tag lsp [--port PORT] [--stdio] [status]."""
    from tag.lsp_server import TagLspServer, get_lsp_status, ensure_schema as lsp_ensure
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    lsp_ensure(db)
    sub = getattr(args, "lsp_subcommand", None)

    if sub == "status" or sub is None:
        sessions = get_lsp_status(db)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(sessions, indent=2))
            return 0
        if not sessions:
            print("No active LSP sessions.")
            return 0
        for s in sessions:
            tp = s["transport"]
            port_suffix = f":{s['port']}" if s.get("port") else ""
            print(f"{s['id'][:8]}  {tp}{port_suffix}  pid={s['pid']}  {s['created_at'][:19]}")
        return 0

    if sub == "start":
        # Collect profile names
        profiles_dir = tag_home() / "profiles"
        profiles: list[str] = []
        if profiles_dir.exists():
            profiles = [p.name for p in profiles_dir.iterdir() if p.is_dir()]
        if not profiles:
            profiles = ["orchestrator", "coder", "reviewer"]

        server_port = getattr(args, "port", 7878)
        use_stdio = getattr(args, "stdio", False)
        server = TagLspServer(profiles=profiles, conn=db)

        if use_stdio or server_port == 0:
            print("TAG LSP server starting on stdio ...", file=sys.stderr)
            server.run_stdio()
        else:
            server.run_tcp(host="127.0.0.1", port=server_port)

    db.close()
    return 0


# ---------------------------------------------------------------------------
# PRD-036: Web Dashboard
# ---------------------------------------------------------------------------

def cmd_web(args: argparse.Namespace) -> int:
    """PRD-036: tag web [--port 8787] [--host 127.0.0.1] [--no-browser]."""
    from tag.api import DashboardServer
    cfg = load_config(config_path(getattr(args, "config", None)))
    db_path = runtime_db_path(cfg)
    if not db_path.exists():
        # Ensure DB exists with base schema
        db = open_db(cfg)
        db.close()

    host = getattr(args, "host", "127.0.0.1") or "127.0.0.1"
    port = getattr(args, "port", 8787) or 8787
    no_browser = getattr(args, "no_browser", False)

    if host != "127.0.0.1":
        print(f"⚠ WARNING: Binding to {host} — dashboard will be accessible on your network.", file=sys.stderr)

    server = DashboardServer(db_path=db_path, host=host, port=port)
    server.start(open_browser=not no_browser)
    return 0


# ---------------------------------------------------------------------------
# Parser registration
# ---------------------------------------------------------------------------

def register(sub: argparse._SubParsersAction) -> None:  # type: ignore[type-arg]
    """Register marketplace, eval, sandbox, serve, lsp, and web subcommands."""

    # ---- PRD-026: marketplace ----
    mkt_cmd = sub.add_parser("marketplace", help="Profile marketplace: pull/push profiles")
    mkt_sub = mkt_cmd.add_subparsers(dest="marketplace_subcommand")

    mkt_pull = mkt_sub.add_parser("pull", help="Download a profile from a URL")
    mkt_pull.add_argument("url", metavar="URL")
    mkt_pull.add_argument("--name", help="Local name for the profile (default: filename)")
    mkt_pull.add_argument("--json", action="store_true")

    mkt_push = mkt_sub.add_parser("push", help="Show how to push a profile to GitHub Gist")
    mkt_push.add_argument("profile_name", metavar="PROFILE_NAME")

    mkt_list = mkt_sub.add_parser("list", help="List cached profiles")
    mkt_list.add_argument("--json", action="store_true")

    for mp in [mkt_cmd, mkt_pull, mkt_push, mkt_list]:
        mp.set_defaults(func=cmd_profile_marketplace)

    # ---- PRD-027: eval ----
    eval_cmd = sub.add_parser("eval", help="Run eval suites against TAG profiles")
    eval_sub = eval_cmd.add_subparsers(dest="eval_subcommand")

    eval_run = eval_sub.add_parser("run", help="Run an eval suite")
    eval_run.add_argument("--suite", required=True, metavar="SUITE_PATH", help="Path to YAML eval suite")
    eval_run.add_argument("--profile", help="Profile to evaluate")
    eval_run.add_argument("--dry-run", action="store_true", dest="dry_run",
                          help="Validate suite without running agent")
    eval_run.add_argument("--json", action="store_true")

    eval_list = eval_sub.add_parser("list", help="List eval runs")
    eval_list.add_argument("--json", action="store_true")

    eval_show = eval_sub.add_parser("show", help="Show eval run detail")
    eval_show.add_argument("run_id", metavar="RUN_ID")
    eval_show.add_argument("--json", action="store_true")

    for ep in [eval_cmd, eval_run, eval_list, eval_show]:
        ep.set_defaults(func=cmd_eval)

    # ---- PRD-028: sandbox ----
    sb_cmd = sub.add_parser("sandbox", help="Isolated code execution (restricted subprocess or Docker)")
    sb_sub = sb_cmd.add_subparsers(dest="sandbox_subcommand")

    sb_run = sb_sub.add_parser("run", help="Run a command in the sandbox")
    sb_run.add_argument("command", metavar="COMMAND", help="Shell command to run")
    sb_run.add_argument("--backend", choices=["restricted", "docker"], default="restricted")
    sb_run.add_argument("--image", default="python:3.12-slim", help="Docker image (for --backend docker)")
    sb_run.add_argument("--timeout", type=int, default=60, metavar="SECONDS")
    sb_run.add_argument("--json", action="store_true")

    sb_list = sb_sub.add_parser("list", help="List recent sandbox runs")
    sb_list.add_argument("--json", action="store_true")

    sb_result = sb_sub.add_parser("result", help="Show sandbox run output")
    sb_result.add_argument("run_id", metavar="RUN_ID")
    sb_result.add_argument("--json", action="store_true")

    for sp in [sb_cmd, sb_run, sb_list, sb_result]:
        sp.set_defaults(func=cmd_sandbox)

    # ---- PRD-029: serve ----
    serve_cmd = sub.add_parser("serve", help="Start local HTTP dashboard server with SSE streaming")
    serve_cmd.add_argument("--port", type=int, default=7880, help="Port to listen on (default: 7880)")
    serve_cmd.add_argument("--profile", help="Default profile for dashboard view")
    serve_cmd.set_defaults(func=cmd_serve)

    # ---- PRD-035: IDE Bridge / LSP ----
    lsp_cmd = sub.add_parser("lsp", help="TAG IDE Bridge / LSP server")
    lsp_sub = lsp_cmd.add_subparsers(dest="lsp_subcommand")

    lsp_start = lsp_sub.add_parser("start", help="Start LSP server")
    lsp_start.add_argument("--port", type=int, default=7878, help="TCP port (0=stdio)")
    lsp_start.add_argument("--stdio", action="store_true", help="Use stdio transport")

    lsp_status = lsp_sub.add_parser("status", help="Show running LSP sessions")
    lsp_status.add_argument("--json", action="store_true")

    for lp in [lsp_cmd, lsp_start, lsp_status]:
        lp.set_defaults(func=cmd_lsp)

    # ---- PRD-036: Web Dashboard ----
    web_cmd = sub.add_parser("web", help="Local web dashboard (FastAPI+React)")
    web_cmd.add_argument("--port", type=int, default=8787)
    web_cmd.add_argument("--host", default="127.0.0.1")
    web_cmd.add_argument("--no-browser", action="store_true")
    web_cmd.set_defaults(func=cmd_web)
