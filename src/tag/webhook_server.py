"""PRD-056: Inbound webhook trigger server.

Receives GitHub/Linear/Slack webhooks, verifies HMAC signatures,
matches trigger rules, and enqueues TAG tasks.
"""
from __future__ import annotations

import hashlib
import hmac
import http.server
import json
import sqlite3
import threading
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


class WebhookPlatform:
    GITHUB = "github"
    LINEAR = "linear"
    JIRA = "jira"
    SLACK = "slack"
    GENERIC = "generic"


@dataclass
class TriggerRule:
    id: str
    platform: str
    event: str
    profile: str
    action: str
    filter_labels: list[str]
    created_at: str
    enabled: bool = True


@dataclass
class WebhookEvent:
    id: str
    platform: str
    event_type: str
    payload: dict
    received_at: str
    signature_valid: bool
    matched_rules: list[str] = field(default_factory=list)
    status: str = "pending"


def _utc_now() -> str:
    return datetime.now(timezone.utc).isoformat()


def ensure_schema(conn: sqlite3.Connection) -> None:
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS trigger_rules (
            id              TEXT PRIMARY KEY,
            platform        TEXT NOT NULL,
            event           TEXT NOT NULL,
            profile         TEXT NOT NULL,
            action          TEXT NOT NULL,
            filter_labels   TEXT NOT NULL DEFAULT '[]',
            created_at      TEXT NOT NULL,
            enabled         INTEGER NOT NULL DEFAULT 1
        );
        CREATE INDEX IF NOT EXISTS idx_tr_platform ON trigger_rules(platform, event);

        CREATE TABLE IF NOT EXISTS webhook_events (
            id               TEXT PRIMARY KEY,
            platform         TEXT NOT NULL,
            event_type       TEXT NOT NULL,
            payload_json     TEXT NOT NULL DEFAULT '{}',
            received_at      TEXT NOT NULL,
            signature_valid  INTEGER NOT NULL DEFAULT 0,
            matched_rules    TEXT NOT NULL DEFAULT '[]',
            status           TEXT NOT NULL DEFAULT 'pending'
        );
        CREATE INDEX IF NOT EXISTS idx_we_platform ON webhook_events(platform, received_at);
    """)
    conn.commit()


def verify_signature(
    platform: str,
    payload_bytes: bytes,
    signature_header: str,
    secret: str,
    timestamp: str = "",
) -> bool:
    if not secret:
        # Without a configured secret we cannot cryptographically verify the
        # payload, so we must NOT report the signature as valid. Callers decide
        # whether to enforce (reject) or merely record signature_valid=False.
        return False
    secret_bytes = secret.encode("utf-8") if isinstance(secret, str) else secret

    if platform == WebhookPlatform.GITHUB:
        # X-Hub-Signature-256: sha256=<hex>
        expected_prefix = "sha256="
        if not signature_header.startswith(expected_prefix):
            return False
        sig_hex = signature_header[len(expected_prefix):]
        computed = hmac.new(secret_bytes, payload_bytes, hashlib.sha256).hexdigest()
        return hmac.compare_digest(computed, sig_hex)

    if platform == WebhookPlatform.SLACK:
        # X-Slack-Signature: v0=<hex> where the HMAC is computed over the
        # signature base string "v0:{X-Slack-Request-Timestamp}:{raw body}".
        # See https://api.slack.com/authentication/verifying-requests-from-slack
        if not signature_header.startswith("v0="):
            return False
        if not timestamp:
            # Slack always sends X-Slack-Request-Timestamp; without it we cannot
            # reconstruct the signed base string, so verification must fail.
            return False
        sig_hex = signature_header[3:]
        base_string = b"v0:" + timestamp.encode("utf-8") + b":" + payload_bytes
        computed = hmac.new(secret_bytes, base_string, hashlib.sha256).hexdigest()
        return hmac.compare_digest(computed, sig_hex)

    # LINEAR and GENERIC: standard HMAC-SHA256 hex
    computed = hmac.new(secret_bytes, payload_bytes, hashlib.sha256).hexdigest()
    # Strip any prefix like "sha256="
    clean_sig = signature_header.split("=")[-1] if "=" in signature_header else signature_header
    return hmac.compare_digest(computed, clean_sig)


def parse_event(platform: str, payload: dict) -> dict:
    if platform == WebhookPlatform.GITHUB:
        action = payload.get("action", "")
        pr = payload.get("pull_request", {})
        issue = payload.get("issue", {})
        obj = pr or issue
        event_type = "pull_request" if pr else "issue" if issue else "push"
        return {
            "type": f"{event_type}.{action}" if action else event_type,
            "title": obj.get("title", ""),
            "body": obj.get("body", ""),
            "url": obj.get("html_url", ""),
            "labels": [lbl.get("name","") for lbl in (obj.get("labels") or [])],
            "assignee": (obj.get("assignee") or {}).get("login"),
            "repo": (payload.get("repository") or {}).get("full_name"),
            "number": obj.get("number"),
        }
    if platform == WebhookPlatform.LINEAR:
        data = payload.get("data", {})
        return {
            "type": f"{payload.get('type','issue')}.{payload.get('action','created')}",
            "title": data.get("title", ""),
            "body": data.get("description", ""),
            "url": data.get("url", ""),
            "labels": [lbl.get("name","") for lbl in (data.get("labels") or [])],
            "assignee": (data.get("assignee") or {}).get("name"),
            "repo": None,
            "number": None,
        }
    # Generic / Slack fallback
    return {
        "type": payload.get("type", "generic"),
        "title": str(payload.get("title", "")),
        "body": str(payload.get("body", payload.get("text", ""))),
        "url": payload.get("url", ""),
        "labels": [],
        "assignee": None,
        "repo": None,
        "number": None,
    }


def create_rule(
    conn: sqlite3.Connection,
    platform: str,
    event: str,
    profile: str,
    action: str,
    *,
    filter_labels: list[str] | None = None,
) -> TriggerRule:
    ensure_schema(conn)
    rule_id = uuid.uuid4().hex[:12]
    now = _utc_now()
    labels_json = json.dumps(filter_labels or [])
    conn.execute(
        """INSERT INTO trigger_rules(id,platform,event,profile,action,filter_labels,created_at,enabled)
           VALUES(?,?,?,?,?,?,?,1)""",
        (rule_id, platform, event, profile, action, labels_json, now),
    )
    conn.commit()
    return TriggerRule(
        id=rule_id, platform=platform, event=event, profile=profile,
        action=action, filter_labels=filter_labels or [], created_at=now,
    )


def list_rules(
    conn: sqlite3.Connection, *, platform: str | None = None
) -> list[TriggerRule]:
    ensure_schema(conn)
    where = "WHERE platform=?" if platform else ""
    params = [platform] if platform else []
    rows = conn.execute(
        f"SELECT * FROM trigger_rules {where} ORDER BY created_at", params
    ).fetchall()
    result = []
    for r in rows:
        result.append(TriggerRule(
            id=r[0], platform=r[1], event=r[2], profile=r[3],
            action=r[4], filter_labels=json.loads(r[5] or "[]"),
            created_at=r[6], enabled=bool(r[7]),
        ))
    return result


def match_rules(
    conn: sqlite3.Connection,
    platform: str,
    event_type: str,
    payload: dict,
) -> list[TriggerRule]:
    ensure_schema(conn)
    rows = conn.execute(
        "SELECT * FROM trigger_rules WHERE platform=? AND enabled=1", (platform,)
    ).fetchall()
    normalized = parse_event(platform, payload)
    payload_labels = set(normalized.get("labels", []))

    matched: list[TriggerRule] = []
    for r in rows:
        rule = TriggerRule(
            id=r[0], platform=r[1], event=r[2], profile=r[3],
            action=r[4], filter_labels=json.loads(r[5] or "[]"),
            created_at=r[6], enabled=bool(r[7]),
        )
        # Match event pattern (supports wildcards: "pull_request.*")
        if not _event_matches(rule.event, event_type):
            continue
        # Label filter
        if rule.filter_labels and not payload_labels.intersection(rule.filter_labels):
            continue
        matched.append(rule)
    return matched


def _build_task_text(platform: str, event_type: str, event_info: dict) -> str:
    """Build the queue-job prompt text for a webhook-triggered task."""
    header = f"Webhook {platform} {event_type}".strip()
    parts = [header]
    title = (event_info.get("title") or "").strip()
    body = (event_info.get("body") or "").strip()
    url = (event_info.get("url") or "").strip()
    if title:
        parts.append(f"Title: {title}")
    if body:
        parts.append(body)
    if url:
        parts.append(f"URL: {url}")
    return "\n\n".join(parts)


def _event_matches(pattern: str, event_type: str) -> bool:
    import fnmatch
    # Match exactly, or via an explicit shell-style wildcard (e.g. "pull_request.*").
    # An empty pattern matches nothing, and the previous unanchored startswith()
    # fallback (which fired "issue" for "issues.opened") is intentionally gone.
    if not pattern:
        return False
    return fnmatch.fnmatch(event_type, pattern)


def list_events(
    conn: sqlite3.Connection, *, limit: int = 50, status: str | None = None
) -> list[WebhookEvent]:
    ensure_schema(conn)
    where = "WHERE status=?" if status else ""
    params = [status] if status else []
    rows = conn.execute(
        f"SELECT * FROM webhook_events {where} ORDER BY received_at DESC LIMIT ?",
        params + [limit],
    ).fetchall()
    return [
        WebhookEvent(
            id=r[0], platform=r[1], event_type=r[2],
            payload=json.loads(r[3] or "{}"),
            received_at=r[4], signature_valid=bool(r[5]),
            matched_rules=json.loads(r[6] or "[]"),
            status=r[7],
        )
        for r in rows
    ]


class _WebhookHandler(http.server.BaseHTTPRequestHandler):
    # Cap inbound webhook bodies to avoid unbounded reads/memory (10 MiB).
    _MAX_BODY_BYTES = 10 * 1024 * 1024

    def log_message(self, *args: Any) -> None:
        pass

    def _send_json(self, code: int, data: Any) -> None:
        body = json.dumps(data).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:
        db_path = self.server._db_path
        conn = sqlite3.connect(str(db_path))
        try:
            if self.path == "/health":
                self._send_json(200, {"status": "ok"})
            elif self.path.startswith("/webhooks/events"):
                events = list_events(conn, limit=20)
                self._send_json(200, [
                    {"id": e.id, "platform": e.platform, "event_type": e.event_type,
                     "status": e.status, "received_at": e.received_at}
                    for e in events
                ])
            elif self.path.startswith("/webhooks/rules"):
                rules = list_rules(conn)
                self._send_json(200, [
                    {"id": r.id, "platform": r.platform, "event": r.event,
                     "action": r.action, "profile": r.profile}
                    for r in rules
                ])
            else:
                self._send_json(404, {"error": "not found"})
        finally:
            conn.close()

    def do_POST(self) -> None:
        path_parts = self.path.strip("/").split("/")
        if len(path_parts) < 2 or path_parts[0] != "webhook":
            self._send_json(404, {"error": "unknown path"})
            return

        platform = path_parts[1].lower()

        # Validate Content-Length before reading the body: a non-numeric header
        # must not raise an uncaught ValueError (which drops the response), and a
        # negative/oversized value must not trigger read-until-EOF or unbounded
        # memory use. Reject malformed lengths with 400 and cap the body size.
        try:
            length = int(self.headers.get("Content-Length", 0))
        except (TypeError, ValueError):
            self._send_json(400, {"error": "invalid Content-Length"})
            return
        if length < 0 or length > self._MAX_BODY_BYTES:
            self._send_json(400, {"error": "invalid Content-Length"})
            return
        body_bytes = self.rfile.read(length)

        # Signature verification
        secret = self.server._secret or ""
        sig_header = (
            self.headers.get("X-Hub-Signature-256", "")
            or self.headers.get("X-Linear-Signature", "")
            or self.headers.get("X-Slack-Signature", "")
        )
        slack_ts = self.headers.get("X-Slack-Request-Timestamp", "")
        valid = verify_signature(platform, body_bytes, sig_header, secret, slack_ts)
        # Enforce the signature: when a secret is configured, reject a request
        # that fails HMAC verification BEFORE parsing/matching/enqueuing.
        # Without this gate a forged request could enqueue arbitrary agent work.
        if secret and not valid:
            self._send_json(401, {"error": "invalid signature"})
            return

        try:
            payload = json.loads(body_bytes.decode("utf-8"))
        except Exception:
            self._send_json(400, {"error": "invalid JSON"})
            return

        event_info = parse_event(platform, payload)
        event_type = event_info.get("type", "unknown")

        db_path = self.server._db_path
        conn = sqlite3.connect(str(db_path))
        try:
            rules = match_rules(conn, platform, event_type, payload)
            rule_ids = [r.id for r in rules]

            event_id = uuid.uuid4().hex[:12]
            now = _utc_now()
            ensure_schema(conn)
            conn.execute(
                """INSERT INTO webhook_events(id,platform,event_type,payload_json,
                   received_at,signature_valid,matched_rules,status)
                   VALUES(?,?,?,?,?,?,?,?)""",
                (event_id, platform, event_type, json.dumps(payload),
                 now, int(valid), json.dumps(rule_ids), "processed"),
            )
            conn.commit()

            # Enqueue a queue job for each matched rule and launch a detached
            # worker. queue_worker has no enqueue(); the real job-creation path
            # is core.db.queue_insert_job(...) + launch_queue_worker(cfg, id),
            # the same one the `tag queue add` CLI uses. Both write/read the same
            # runtime DB, so a worker launched here finds the row we insert.
            from tag.core.db import (
                queue_insert_job,
                queue_update_pid,
                launch_queue_worker,
            )

            cfg = getattr(self.server, "_cfg", None)
            for rule in rules:
                job_id = uuid.uuid4().hex[:8]
                task_text = _build_task_text(platform, event_type, event_info)
                queue_insert_job(
                    conn,
                    job_id,
                    rule.profile,
                    task_text,
                    task_type=(rule.action or "mixed"),
                )
                # Job row is now persisted (this is the dispatch of record).
                # Launching the worker subprocess is best-effort: if it fails the
                # queued row can still be picked up later, so don't fail the POST.
                if cfg is not None:
                    try:
                        pid = launch_queue_worker(cfg, job_id)
                        queue_update_pid(conn, job_id, pid)
                    except Exception:
                        pass

            self._send_json(200, {
                "event_id": event_id,
                "rules_matched": len(rules),
                "signature_valid": valid,
            })
        finally:
            conn.close()


class WebhookServer:
    def __init__(
        self,
        db_path: str | Path,
        cfg: dict,
        host: str = "127.0.0.1",
        port: int = 8080,
        secret: str | None = None,
    ) -> None:
        import os
        self._db_path = Path(db_path)
        self._cfg = cfg
        self._host = host
        self._port = port
        # Allow the signing secret to be supplied via the environment so
        # verification is reachable even when the CLI does not pass secret=.
        self._secret = secret or os.environ.get("TAG_WEBHOOK_SECRET") or None
        self._server: http.server.HTTPServer | None = None

    def start(self) -> None:
        self._server = http.server.HTTPServer((self._host, self._port), _WebhookHandler)
        self._server._db_path = self._db_path
        self._server._secret = self._secret
        # Expose cfg to the request handler so matched rules can launch the
        # detached queue worker via launch_queue_worker(cfg, job_id).
        self._server._cfg = self._cfg
        url = f"http://{self._host}:{self._port}"
        print(f"TAG webhook server listening on {url}")
        self._server.serve_forever()

    def start_background(self) -> None:
        t = threading.Thread(target=self.start, daemon=True, name="webhook-server")
        t.start()
        print(f"TAG webhook server starting on http://{self._host}:{self._port}")

    def stop(self) -> None:
        if self._server:
            self._server.shutdown()
            # Also release the listening socket/fd; shutdown() alone only stops
            # the serve loop, leaking the port on repeated start/stop.
            self._server.server_close()
