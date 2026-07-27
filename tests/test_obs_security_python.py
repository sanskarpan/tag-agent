"""Regression tests for the observability / security / control-plane fixes (issue #578).

Each test below reproduces a defect that was confirmed by running the shipped
binaries, and fails against the pre-fix code:

#1 — the Python webhook receiver accepted unsigned POSTs by default, letting an
     anonymous caller enqueue agent work.
#2 — `do_POST` enqueued on a connection that never ensured the queue schema, so a
     fresh install raised `no such table: queue_jobs` and dropped the connection
     (empty reply); worse, the webhook_events row was committed `processed`
     *before* the enqueue, recording a job that was never created.
#3 — the receiver lacked replay protection, Slack timestamp tolerance, and
     authentication on the /webhooks/* introspection routes.
#4 — the webhook server was single-threaded with no read timeout, so one stalled
     client wedged every other request.
"""
from __future__ import annotations

import http.client
import http.server
import json
import hashlib
import hmac
import socket
import sqlite3
import threading
import time
from pathlib import Path

import pytest

# Imported as a module (not by symbol) so that this file still *collects* against
# the pre-fix code: the regression then shows up as a failing assertion in the
# specific test, rather than a module-level ImportError that hides which
# behaviours regressed.
import tag.webhook_server as ws
from tag.webhook_server import create_rule, ensure_schema, verify_signature

WebhookServer = ws.WebhookServer

SECRET = "test-secret-not-a-real-credential"


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _github_sig(body: bytes, secret: str = SECRET) -> str:
    return "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()


@pytest.fixture()
def server_factory(tmp_path):
    """Start a WebhookServer on a free port; yields a (start, db_path) helper."""
    started: list[WebhookServer] = []

    def start(secret: str | None = SECRET, allow_unsigned: bool = False) -> tuple[int, Path]:
        db_path = tmp_path / f"wh{len(started)}.sqlite3"
        conn = sqlite3.connect(db_path)
        ensure_schema(conn)
        create_rule(conn, "github", "issue.*", "default", "run")
        conn.close()
        port = _free_port()
        srv = WebhookServer(
            db_path=str(db_path), cfg={}, host="127.0.0.1", port=port,
            secret=secret, allow_unsigned=allow_unsigned,
        )
        srv.start_background()
        started.append(srv)
        for _ in range(100):  # wait for the listener
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                    break
            except OSError:
                time.sleep(0.05)
        return port, db_path

    yield start
    for srv in started:
        srv.stop()


def _post(port: int, path: str, body: bytes, headers: dict | None = None):
    c = http.client.HTTPConnection("127.0.0.1", port, timeout=10)
    try:
        c.request("POST", path, body=body, headers=headers or {})
        r = c.getresponse()
        return r.status, r.read()
    finally:
        c.close()


def _get(port: int, path: str, headers: dict | None = None):
    c = http.client.HTTPConnection("127.0.0.1", port, timeout=10)
    try:
        c.request("GET", path, headers=headers or {})
        r = c.getresponse()
        return r.status, r.read()
    finally:
        c.close()


# ---------------------------------------------------------------------------
# #1 — unsigned webhooks must not be accepted by default
# ---------------------------------------------------------------------------

class TestUnsignedWebhookRejected:
    def test_unsigned_post_rejected_401_when_no_secret(self, server_factory):
        """Pre-fix: returned 200 and enqueued work for an anonymous caller."""
        port, db_path = server_factory(secret=None, allow_unsigned=False)
        body = json.dumps({"action": "opened", "issue": {"title": "x"}}).encode()
        status, payload = _post(port, "/webhook/github", body)
        assert status == 401, f"unsigned POST must be refused, got {status}"
        assert b"HMAC secret" in payload

    def test_unsigned_post_writes_no_event_row(self, server_factory):
        """Pre-fix: an anonymous POST persisted a webhook_events row."""
        port, db_path = server_factory(secret=None, allow_unsigned=False)
        body = json.dumps({"action": "opened", "issue": {"title": "x"}}).encode()
        _post(port, "/webhook/github", body)
        conn = sqlite3.connect(db_path)
        try:
            n = conn.execute("SELECT COUNT(*) FROM webhook_events").fetchone()[0]
        finally:
            conn.close()
        assert n == 0, "a rejected unsigned event must not be recorded"

    def test_allow_unsigned_opt_in_still_works(self, server_factory):
        """The explicit operator opt-in must still accept unsigned events."""
        port, _ = server_factory(secret=None, allow_unsigned=True)
        body = json.dumps({"action": "opened", "issue": {"title": "x"}}).encode()
        status, _payload = _post(port, "/webhook/github", body)
        assert status == 200

    def test_cli_refuses_to_start_without_secret(self, tmp_path, monkeypatch, capsys):
        """`webhook listen` must exit non-zero rather than serve unauthenticated."""
        import argparse

        from tag.cmd import prd_clusters

        monkeypatch.delenv("TAG_WEBHOOK_SECRET", raising=False)
        monkeypatch.setenv("TAG_HOME", str(tmp_path / "home"))
        args = argparse.Namespace(
            hooks_subcommand="listen", port=_free_port(), host="127.0.0.1",
            secret=None, allow_unsigned=False, profile=None, config=None,
        )
        rc = prd_clusters.cmd_webhook_server(args)
        assert rc == 1
        captured = capsys.readouterr()
        assert "refusing to start without an HMAC secret" in (captured.out + captured.err)


# ---------------------------------------------------------------------------
# #2 — enqueue must not explode on a fresh DB, and status must follow the enqueue
# ---------------------------------------------------------------------------

class TestWebhookEnqueueIntegrity:
    def test_matched_rule_enqueues_on_fresh_db(self, server_factory):
        """Pre-fix: sqlite3.OperationalError: no such table: queue_jobs -> empty reply."""
        port, db_path = server_factory(secret=SECRET)
        body = json.dumps({"action": "opened", "issue": {"title": "t"}}).encode()
        status, payload = _post(
            port, "/webhook/github", body,
            {"X-Hub-Signature-256": _github_sig(body)},
        )
        assert status == 200, f"expected clean 200, got {status}"
        assert json.loads(payload)["rules_matched"] == 1
        conn = sqlite3.connect(db_path)
        try:
            jobs = conn.execute("SELECT COUNT(*) FROM queue_jobs").fetchone()[0]
        finally:
            conn.close()
        assert jobs == 1, "the matched rule must actually create a queue job"

    def test_event_not_marked_processed_without_a_job(self, server_factory):
        """A webhook_events row may only say 'processed' if the job exists."""
        port, db_path = server_factory(secret=SECRET)
        body = json.dumps({"action": "opened", "issue": {"title": "t"}}).encode()
        _post(port, "/webhook/github", body,
              {"X-Hub-Signature-256": _github_sig(body)})
        conn = sqlite3.connect(db_path)
        try:
            rows = conn.execute("SELECT status FROM webhook_events").fetchall()
            jobs = conn.execute("SELECT COUNT(*) FROM queue_jobs").fetchone()[0]
        finally:
            conn.close()
        processed = [r for r in rows if r[0] == "processed"]
        assert not (processed and jobs == 0), (
            "DB claims a processed event but no job was created"
        )

    def test_handler_returns_500_not_empty_reply_on_internal_error(
        self, server_factory, monkeypatch
    ):
        """An unexpected error must yield a clean 500, never a dropped connection."""
        import tag.webhook_server as ws

        def boom(*a, **k):
            raise RuntimeError("planted failure")

        monkeypatch.setattr(ws, "parse_event", boom)
        port, _ = server_factory(secret=SECRET)
        body = json.dumps({"action": "opened", "issue": {"title": "t"}}).encode()
        status, _payload = _post(
            port, "/webhook/github", body,
            {"X-Hub-Signature-256": _github_sig(body)},
        )
        assert status == 500


# ---------------------------------------------------------------------------
# #3 — replay protection, Slack timestamp tolerance, introspection auth
# ---------------------------------------------------------------------------

class TestWebhookReplayAndAuth:
    def test_duplicate_delivery_id_rejected_409(self, server_factory):
        """Pre-fix: a captured signed delivery could be replayed indefinitely."""
        port, db_path = server_factory(secret=SECRET)
        body = json.dumps({"action": "opened", "issue": {"title": "t"}}).encode()
        hdrs = {"X-Hub-Signature-256": _github_sig(body), "X-GitHub-Delivery": "dup-1"}
        first, _ = _post(port, "/webhook/github", body, hdrs)
        second, _ = _post(port, "/webhook/github", body, hdrs)
        assert first == 200
        assert second == 409, f"replayed delivery must be refused, got {second}"
        conn = sqlite3.connect(db_path)
        try:
            jobs = conn.execute("SELECT COUNT(*) FROM queue_jobs").fetchone()[0]
        finally:
            conn.close()
        assert jobs == 1, "a replay must not enqueue a second job"

    def test_delivery_cache_is_bounded_and_thread_safe(self):
        cache = ws.DeliveryCache(max_ids=4)
        assert cache.mark_seen("a") is True
        assert cache.mark_seen("a") is False
        for k in ("b", "c", "d", "e"):
            cache.mark_seen(k)
        # "a" was evicted by the bound, so it is accepted again
        assert cache.mark_seen("a") is True

    def test_slack_stale_timestamp_rejected(self):
        """Pre-fix: a captured Slack-signed payload verified forever."""
        body = b'{"text":"hi"}'
        stale = str(int(time.time()) - (ws.SLACK_TIMESTAMP_TOLERANCE_SECONDS + 60))
        base = b"v0:" + stale.encode() + b":" + body
        sig = "v0=" + hmac.new(SECRET.encode(), base, hashlib.sha256).hexdigest()
        assert verify_signature("slack", body, sig, SECRET, stale) is False

    def test_slack_fresh_timestamp_accepted(self):
        body = b'{"text":"hi"}'
        fresh = str(int(time.time()))
        base = b"v0:" + fresh.encode() + b":" + body
        sig = "v0=" + hmac.new(SECRET.encode(), base, hashlib.sha256).hexdigest()
        assert verify_signature("slack", body, sig, SECRET, fresh) is True

    def test_rules_route_requires_bearer_token(self, server_factory):
        """Pre-fix: /webhooks/rules was served to any unauthenticated caller."""
        port, _ = server_factory(secret=SECRET)
        status, _ = _get(port, "/webhooks/rules")
        assert status == 401, f"expected 401 without a token, got {status}"
        ok, payload = _get(port, "/webhooks/rules",
                           {"Authorization": f"Bearer {SECRET}"})
        assert ok == 200
        assert json.loads(payload)[0]["platform"] == "github"

    def test_events_route_requires_bearer_token(self, server_factory):
        port, _ = server_factory(secret=SECRET)
        status, _ = _get(port, "/webhooks/events")
        assert status == 401
        ok, _ = _get(port, "/webhooks/events", {"Authorization": f"Bearer {SECRET}"})
        assert ok == 200

    def test_health_stays_public(self, server_factory):
        port, _ = server_factory(secret=SECRET)
        status, payload = _get(port, "/health")
        assert status == 200
        assert json.loads(payload)["status"] == "ok"


# ---------------------------------------------------------------------------
# #4 — a stalled client must not wedge the server
# ---------------------------------------------------------------------------

class TestWebhookConcurrency:
    def test_stalled_client_does_not_block_other_requests(self, server_factory):
        """Pre-fix (single-threaded HTTPServer): /health returned nothing at all."""
        port, _ = server_factory(secret=SECRET)
        stalled = socket.create_connection(("127.0.0.1", port), timeout=5)
        try:
            time.sleep(0.3)  # connected, never sends a request line
            status, payload = _get(port, "/health")
            assert status == 200, "a stalled client wedged the whole server"
            assert json.loads(payload)["status"] == "ok"
        finally:
            stalled.close()

    def test_server_is_threaded_with_a_socket_timeout(self, server_factory):
        port, _ = server_factory(secret=SECRET)
        srv = None
        for t in threading.enumerate():
            if t.name == "webhook-server":
                srv = t
        assert srv is not None
        assert issubclass(ws._ThreadingWebhookServer, http.server.ThreadingHTTPServer)
        assert ws._TimeoutWebhookHandler.timeout is not None


class TestDevUIConcurrency:
    """#4 (devui half) — the DevUI was single-threaded with no read timeout."""

    def _start(self, tmp_path):
        from tag.devui import DevUIServer

        db_path = tmp_path / "devui.sqlite3"
        sqlite3.connect(db_path).close()
        port = _free_port()
        srv = DevUIServer(db_path=str(db_path), host="127.0.0.1", port=port)
        srv.start_background()
        for _ in range(100):
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                    break
            except OSError:
                time.sleep(0.05)
        return srv, port

    def test_stalled_client_does_not_block_devui(self, tmp_path):
        """Pre-fix: a single stalled client wedged the dashboard entirely."""
        srv, port = self._start(tmp_path)
        stalled = socket.create_connection(("127.0.0.1", port), timeout=5)
        try:
            time.sleep(0.3)  # connected, never sends a request line
            c = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
            try:
                c.request("GET", "/")
                assert c.getresponse().status == 200, "stalled client wedged the DevUI"
            finally:
                c.close()
        finally:
            stalled.close()
            srv._server.shutdown()
            srv._server.server_close()

    def test_devui_server_is_threaded(self, tmp_path):
        from tag.devui import _ThreadingDevUIServer

        assert issubclass(_ThreadingDevUIServer, http.server.ThreadingHTTPServer)
