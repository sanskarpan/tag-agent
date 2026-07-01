"""PRD-040: Notification Hooks (tag hooks notify).

First-class structured notification delivery channels layered on top of the
existing hook system. Four channels: slack, email, desktop, webhook.

Credentials are stored only as env-var NAMES in config; actual values are
read from the profile's .env file at delivery time. Message content is never
written to the delivery log table.
"""
from __future__ import annotations

import ipaddress
import json
import os
import smtplib
import socket
import sqlite3
import subprocess
import sys
import urllib.error
import urllib.request
import uuid
from email.mime.text import MIMEText
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

VALID_CHANNELS = {"slack", "email", "desktop", "webhook"}
VALID_EVENTS = {
    "run.completed", "run.failed", "run.started",
    "budget.warning", "budget.exceeded",
    "queue.done", "queue.failed",
    "loop.completed", "loop.failed",
}

# Template variables that may be substituted
_TEMPLATE_VARS = {
    "run_id", "profile", "duration", "tokens_used", "cost_usd",
    "status", "error_message", "task", "event",
}


def _render_template(template: str, ctx: dict[str, Any]) -> str:
    """Simple {{var}} substitution from an allow-list."""
    result = template
    for key in _TEMPLATE_VARS:
        result = result.replace("{{" + key + "}}", str(ctx.get(key, "")))
    return result


# ---------------------------------------------------------------------------
# Schema
# ---------------------------------------------------------------------------

def ensure_schema(conn: sqlite3.Connection) -> None:
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS notification_hooks (
          id           TEXT PRIMARY KEY,
          profile      TEXT,
          event        TEXT NOT NULL,
          channel      TEXT NOT NULL,
          config_json  TEXT NOT NULL DEFAULT '{}',
          template     TEXT NOT NULL DEFAULT '',
          enabled      INTEGER NOT NULL DEFAULT 1,
          created_at   TEXT NOT NULL
        );
        CREATE INDEX IF NOT EXISTS idx_nh_event ON notification_hooks(event, enabled);

        CREATE TABLE IF NOT EXISTS notification_log (
          id           TEXT PRIMARY KEY,
          hook_id      TEXT NOT NULL,
          event        TEXT NOT NULL,
          channel      TEXT NOT NULL,
          outcome      TEXT NOT NULL,
          http_status  INTEGER,
          attempt      INTEGER NOT NULL DEFAULT 1,
          created_at   TEXT NOT NULL,
          FOREIGN KEY(hook_id) REFERENCES notification_hooks(id)
        );
    """)
    conn.commit()


def _utc_now() -> str:
    import datetime
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


# ---------------------------------------------------------------------------
# Hook CRUD
# ---------------------------------------------------------------------------

def add_hook(
    conn: sqlite3.Connection,
    event: str,
    channel: str,
    config: dict,
    *,
    profile: str | None = None,
    template: str = "",
) -> str:
    ensure_schema(conn)
    if event not in VALID_EVENTS:
        raise ValueError(f"event must be one of {sorted(VALID_EVENTS)}, got {event!r}")
    if channel not in VALID_CHANNELS:
        raise ValueError(f"channel must be one of {sorted(VALID_CHANNELS)}, got {channel!r}")
    hook_id = uuid.uuid4().hex[:12]
    conn.execute(
        "INSERT INTO notification_hooks(id, profile, event, channel, config_json, template, enabled, created_at) "
        "VALUES(?,?,?,?,?,?,1,?)",
        (hook_id, profile, event, channel, json.dumps(config), template, _utc_now()),
    )
    conn.commit()
    return hook_id


def list_hooks(conn: sqlite3.Connection, *, profile: str | None = None) -> list[dict]:
    ensure_schema(conn)
    if profile:
        rows = conn.execute(
            "SELECT id, profile, event, channel, config_json, template, enabled FROM notification_hooks "
            "WHERE (profile=? OR profile IS NULL) AND enabled=1 ORDER BY created_at",
            (profile,),
        ).fetchall()
    else:
        rows = conn.execute(
            "SELECT id, profile, event, channel, config_json, template, enabled FROM notification_hooks "
            "ORDER BY created_at"
        ).fetchall()
    return [
        {
            "id": r[0], "profile": r[1], "event": r[2], "channel": r[3],
            "config": json.loads(r[4] or "{}"), "template": r[5], "enabled": bool(r[6]),
        }
        for r in rows
    ]


def remove_hook(conn: sqlite3.Connection, hook_id: str) -> bool:
    ensure_schema(conn)
    cur = conn.execute("DELETE FROM notification_hooks WHERE id=?", (hook_id,))
    conn.commit()
    return cur.rowcount > 0


def set_hook_enabled(conn: sqlite3.Connection, hook_id: str, enabled: bool) -> bool:
    ensure_schema(conn)
    cur = conn.execute(
        "UPDATE notification_hooks SET enabled=? WHERE id=?", (int(enabled), hook_id)
    )
    conn.commit()
    return cur.rowcount > 0


# ---------------------------------------------------------------------------
# Delivery
# ---------------------------------------------------------------------------

def _validate_outbound_url(url: str) -> str | None:
    """Return an error string if *url* is unsafe to POST to, else None (SSRF guard).

    Blocks non-http(s) schemes and requests that resolve to loopback,
    link-local (incl. cloud metadata 169.254.169.254), private, reserved,
    multicast or unspecified addresses.
    """
    try:
        parsed = urlparse(url)
    except Exception as exc:
        return f"invalid URL: {exc}"
    if parsed.scheme not in ("http", "https"):
        return f"refusing scheme {parsed.scheme!r}: only http/https are allowed"
    host = parsed.hostname
    if not host:
        return "URL has no host"

    def _blocked(addr: str) -> bool:
        try:
            ip = ipaddress.ip_address(addr.split("%", 1)[0])
        except ValueError:
            return False
        return (ip.is_loopback or ip.is_link_local or ip.is_private
                or ip.is_reserved or ip.is_multicast or ip.is_unspecified)

    # A literal IP (e.g. http://127.0.0.1/, http://169.254.169.254/) needs no DNS.
    try:
        ipaddress.ip_address(host)
        if _blocked(host):
            return f"refusing to connect to non-public address {host} (SSRF protection)"
        return None
    except ValueError:
        pass

    # Hostname: resolve and block if any resolved address is non-public. If
    # resolution fails we don't block on that alone — urlopen would fail anyway,
    # and we avoid coupling validation to network availability.
    try:
        infos = socket.getaddrinfo(host, parsed.port or None)
    except OSError:
        return None
    for info in infos:
        if _blocked(info[4][0]):
            return f"refusing to connect to non-public address {info[4][0]} (SSRF protection)"
    return None


def _ip_is_blocked(addr: str) -> bool:
    """True if *addr* is a non-public (SSRF-sensitive) IP literal."""
    try:
        ip = ipaddress.ip_address(addr.split("%", 1)[0])
    except ValueError:
        return False
    return (ip.is_loopback or ip.is_link_local or ip.is_private
            or ip.is_reserved or ip.is_multicast or ip.is_unspecified)


def _safe_urlopen(url, *, timeout: int = 10):
    """SSRF-hardened urlopen: pins the resolved IP at connect time and refuses
    redirects to non-public addresses.

    ``_validate_outbound_url`` only checks the first hop; the default opener then
    follows 3xx redirects and re-resolves DNS independently, so a public
    ``302 -> http://169.254.169.254/`` (or a DNS-rebinding record) reaches
    internal/metadata addresses. Here every redirect is re-validated and every
    connection (including each redirect hop) re-resolves and refuses non-public
    IPs at connect time.
    """
    import http.client

    def _connect_pinned(conn):
        infos = socket.getaddrinfo(conn.host, conn.port, 0, socket.SOCK_STREAM)
        for info in infos:
            if _ip_is_blocked(info[4][0]):
                raise OSError(
                    f"refusing to connect to non-public address {info[4][0]} (SSRF protection)"
                )
        last_err = None
        for family, socktype, proto, _canon, sockaddr in infos:
            sock = None
            try:
                sock = socket.socket(family, socktype, proto)
                if conn.timeout is not socket._GLOBAL_DEFAULT_TIMEOUT:
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
            err = _validate_outbound_url(newurl)
            if err:
                raise urllib.error.HTTPError(newurl, code, f"blocked redirect: {err}", headers, fp)
            return super().redirect_request(req, fp, code, msg, headers, newurl)

    opener = urllib.request.build_opener(
        _PinnedHTTPHandler, _PinnedHTTPSHandler, _GuardRedirect
    )
    return opener.open(url, timeout=timeout)


def _deliver_slack(webhook_url: str, message: str) -> tuple[bool, int | None, str]:
    """POST a Slack message. Returns (ok, http_status, error_msg)."""
    unsafe = _validate_outbound_url(webhook_url)
    if unsafe:
        return False, None, unsafe
    payload = json.dumps({"text": message}).encode()
    req = urllib.request.Request(
        webhook_url, data=payload,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with _safe_urlopen(req, timeout=10) as resp:
            return True, resp.status, ""
    except urllib.error.HTTPError as exc:
        return False, exc.code, str(exc)
    except Exception as exc:
        return False, None, str(exc)


def _deliver_email(
    smtp_host: str,
    smtp_port: int,
    username: str,
    password: str,
    from_addr: str,
    to_addr: str,
    subject: str,
    body: str,
    *,
    use_tls: bool = True,
) -> tuple[bool, None, str]:
    try:
        msg = MIMEText(body, "plain")
        msg["Subject"] = subject
        msg["From"] = from_addr
        msg["To"] = to_addr
        if use_tls and smtp_port == 465:
            with smtplib.SMTP_SSL(smtp_host, smtp_port, timeout=10) as s:
                s.login(username, password)
                s.send_message(msg)
        else:
            with smtplib.SMTP(smtp_host, smtp_port, timeout=10) as s:
                if use_tls:
                    s.starttls()
                if username:
                    s.login(username, password)
                s.send_message(msg)
        return True, None, ""
    except Exception as exc:
        return False, None, str(exc)


def _deliver_desktop(message: str, title: str = "TAG Notification") -> tuple[bool, None, str]:
    try:
        if sys.platform == "darwin":
            # Pass message/title as osascript argv, never interpolated into the
            # script — prevents AppleScript/shell injection via notification text.
            script = (
                "on run {msg, ttl}\n"
                "display notification msg with title ttl\n"
                "end run"
            )
            subprocess.run(["osascript", "-e", script, message, title], check=True,
                           timeout=5, capture_output=True)
        elif sys.platform.startswith("linux"):
            subprocess.run(["notify-send", title, message], check=True, timeout=5,
                           capture_output=True)
        else:
            return False, None, f"Desktop notifications not supported on {sys.platform}"
        return True, None, ""
    except FileNotFoundError as exc:
        return False, None, f"Notification command not found: {exc}"
    except Exception as exc:
        return False, None, str(exc)


def _deliver_webhook(url: str, payload: dict, headers: dict | None = None) -> tuple[bool, int | None, str]:
    unsafe = _validate_outbound_url(url)
    if unsafe:
        return False, None, unsafe
    body = json.dumps(payload).encode()
    h = {"Content-Type": "application/json"}
    if headers:
        h.update(headers)
    req = urllib.request.Request(url, data=body, headers=h, method="POST")
    try:
        with _safe_urlopen(req, timeout=10) as resp:
            return True, resp.status, ""
    except urllib.error.HTTPError as exc:
        return False, exc.code, str(exc)
    except Exception as exc:
        return False, None, str(exc)


def _deliver_with_meta(
    hook: dict,
    event: str,
    ctx: dict[str, Any],
    *,
    max_retries: int = 3,
) -> tuple[bool, str, int | None, int]:
    """Deliver a notification, returning (success, error, http_status, attempts).

    Message content is never logged.
    """
    channel = hook["channel"]
    config = hook.get("config", {})
    template = hook.get("template", "") or "TAG event {{event}} — profile {{profile}} status {{status}}"
    message = _render_template(template, {"event": event, **ctx})

    ok, http_status, err = False, None, "unknown channel"
    attempt = 1

    for attempt in range(1, max_retries + 1):
        if channel == "slack":
            webhook_url = config.get("webhook_url") or os.environ.get(
                config.get("webhook_url_env", "SLACK_WEBHOOK_URL"), ""
            )
            if not webhook_url:
                return False, "No Slack webhook URL configured", None, attempt
            ok, http_status, err = _deliver_slack(webhook_url, message)

        elif channel == "email":
            smtp_host = config.get("smtp_host", "smtp.gmail.com")
            smtp_port = int(config.get("smtp_port", 587))
            username = config.get("username") or os.environ.get(config.get("username_env", ""), "")
            password = config.get("password") or os.environ.get(config.get("password_env", ""), "")
            from_addr = config.get("from", username)
            to_addr = config.get("to", username)
            subject = config.get("subject", f"TAG: {event}")
            ok, http_status, err = _deliver_email(
                smtp_host, smtp_port, username, password, from_addr, to_addr, subject, message
            )

        elif channel == "desktop":
            title = config.get("title", "TAG")
            ok, http_status, err = _deliver_desktop(message, title)

        elif channel == "webhook":
            url = config.get("url", "")
            if not url:
                return False, "No webhook URL configured", None, attempt
            extra_headers = config.get("headers", {})
            payload = {"event": event, **{k: v for k, v in ctx.items() if k != "error_message"}}
            ok, http_status, err = _deliver_webhook(url, payload, extra_headers)

        if ok:
            break

        if attempt < max_retries:
            import time
            time.sleep(2 ** (attempt - 1))  # 1s, 2s, 4s

    return ok, err, http_status, attempt


def deliver(
    hook: dict,
    event: str,
    ctx: dict[str, Any],
    *,
    max_retries: int = 3,
) -> tuple[bool, str]:
    """Deliver a notification for *hook* with context *ctx*.

    Returns (success, error_message). Message content is never logged.
    """
    ok, err, _http_status, _attempt = _deliver_with_meta(
        hook, event, ctx, max_retries=max_retries
    )
    return ok, err


def fire_event_notifications(
    conn: sqlite3.Connection,
    event: str,
    ctx: dict[str, Any],
) -> None:
    """Fire all enabled hooks for *event* and log outcomes."""
    ensure_schema(conn)
    profile = ctx.get("profile")
    hooks = list_hooks(conn, profile=profile)
    matching = [h for h in hooks if h["event"] == event and h["enabled"]]

    for hook in matching:
        ok, err, http_status, attempt = _deliver_with_meta(hook, event, ctx)
        now = _utc_now()
        conn.execute(
            "INSERT INTO notification_log(id, hook_id, event, channel, outcome, http_status, attempt, created_at) "
            "VALUES(?,?,?,?,?,?,?,?)",
            (uuid.uuid4().hex[:12], hook["id"], event, hook["channel"],
             "ok" if ok else "failed", http_status, attempt, now),
        )
    conn.commit()

