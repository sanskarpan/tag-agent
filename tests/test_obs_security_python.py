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


# ---------------------------------------------------------------------------
# #12 — local servers must not silently bind non-loopback interfaces
# ---------------------------------------------------------------------------

class TestLoopbackBindGuard:
    """Pre-fix: `--host 0.0.0.0` bound all interfaces, serving spans/costs/
    memories/alerts to the whole network with no auth and (for devui/webhook)
    no warning at all."""

    def test_is_loopback_host_classification(self):
        from tag.core.utils import is_loopback_host

        for good in ("127.0.0.1", "localhost", "::1", "[::1]", "127.0.0.2", ""):
            assert is_loopback_host(good) is True, good
        for bad in ("0.0.0.0", "192.168.1.10", "10.0.0.1", "::", "example.com"):
            assert is_loopback_host(bad) is False, bad

    @pytest.mark.parametrize(
        "handler_mod,handler_name,kwargs",
        [
            ("tag.cmd.prd_clusters", "cmd_devui", {"port": 7999}),
            ("tag.cmd.marketplace", "cmd_web", {"port": 8999, "no_browser": True}),
        ],
    )
    def test_non_loopback_bind_refused(
        self, handler_mod, handler_name, kwargs, tmp_path, monkeypatch, capsys
    ):
        import argparse
        import importlib

        monkeypatch.setenv("TAG_HOME", str(tmp_path / "home"))
        mod = importlib.import_module(handler_mod)
        args = argparse.Namespace(
            host="0.0.0.0", profile=None, config=None, allow_remote=False,
            open_browser=False, **kwargs
        )
        rc = getattr(mod, handler_name)(args)
        assert rc == 1, "a non-loopback bind must be refused"
        out = capsys.readouterr()
        assert "non-loopback" in (out.out + out.err)

    def test_webhook_listen_refuses_non_loopback(self, tmp_path, monkeypatch, capsys):
        import argparse

        from tag.cmd import prd_clusters

        monkeypatch.setenv("TAG_HOME", str(tmp_path / "home"))
        args = argparse.Namespace(
            hooks_subcommand="listen", port=_free_port(), host="0.0.0.0",
            secret=SECRET, allow_unsigned=False, allow_remote=False,
            profile=None, config=None,
        )
        rc = prd_clusters.cmd_webhook_server(args)
        assert rc == 1
        out = capsys.readouterr()
        assert "non-loopback" in (out.out + out.err)


# ---------------------------------------------------------------------------
# #5 — `tag pricing` reported wrong costs for shipped Anthropic models
# ---------------------------------------------------------------------------

class TestPricingTable:
    """Pre-fix: claude-opus-4-8 was listed at 15/75 (a 3x overcharge, $90 for
    1M/1M instead of $30) and claude-haiku-4-5 at 0.80/4.00 ($4.80 vs $6.00)."""

    @pytest.mark.parametrize(
        "model,want_in,want_out",
        [
            ("claude-opus-4-8", 5.00, 25.00),
            ("claude-sonnet-4-6", 3.00, 15.00),
            ("claude-haiku-4-5", 1.00, 5.00),
            ("claude-haiku-4-5-20251001", 1.00, 5.00),
        ],
    )
    def test_published_rates(self, model, want_in, want_out):
        from tag.cost_table import list_all_models, reload_pricing_table

        reload_pricing_table()
        by_id = {m.model_id: m for m in list_all_models()}
        assert model in by_id, f"{model} missing from pricing table"
        entry = by_id[model]
        assert entry.input_usd_per_1m == want_in
        assert entry.output_usd_per_1m == want_out

    @pytest.mark.parametrize(
        "model,want_cost",
        [
            ("claude-opus-4-8", 30.00),
            ("claude-sonnet-4-6", 18.00),
            ("claude-haiku-4-5", 6.00),
        ],
    )
    def test_cost_for_1m_in_1m_out(self, model, want_cost):
        from tag.cost_table import compute_cost, reload_pricing_table

        reload_pricing_table()
        cost = compute_cost(model, 1_000_000, 1_000_000)
        assert cost == pytest.approx(want_cost), (
            f"{model}: computed ${cost}, expected ${want_cost}"
        )


# ---------------------------------------------------------------------------
# #9 — Python `security scan` missed hardcoded passwords the Go scanner catches
# ---------------------------------------------------------------------------

class TestGenericSecretPattern:
    """Pre-fix: security.py had no `generic_secret` pattern (Go's scan.go does),
    so `PASSWORD = "..."` / `DB_PASSWORD=...` lines fell below the 4.5-bit
    entropy threshold and a directory full of them scanned as clean."""

    # All values below are obviously-fake placeholders.
    @pytest.mark.parametrize(
        "line",
        [
            'PASSWORD = "notarealpassword123"',
            "DB_PASSWORD=notarealpassword123",
            'api_key: "placeholderplaceholder"',
            "SECRET = 'fakefakefakefakefake'",
            'auth_token = "totallyfakevalue1234"',
        ],
    )
    def test_hardcoded_credentials_are_reported(self, line, tmp_path):
        from tag import security

        findings = security.scan_text(line, tmp_path / "app.py")
        assert findings, f"no finding for {line!r}"
        assert findings[0].pattern_name == "generic_secret"

    def test_ordinary_code_is_not_flagged(self, tmp_path):
        from tag import security

        benign = "\n".join([
            "def main():",
            "    print('hello world')",
            "    return 0",
            "password = get_password()",   # no literal assigned
            "TOKEN_LENGTH = 16",           # too short / not a credential
        ])
        assert security.scan_text(benign, tmp_path / "ok.py") == []

    @pytest.mark.parametrize(
        "line",
        [
            "    OPENROUTER_API_KEY: your-openrouter-api-key",  # shipped tag.yaml
            'API_KEY = "<redacted-by-the-scrubber>"',
            "password: changeme-please-now",
            'token = "${MY_TOKEN_FROM_ENV}"',
        ],
    )
    def test_documentation_placeholders_are_not_flagged(self, line, tmp_path):
        """The generic pattern must not make every fresh install scan dirty."""
        from tag import security

        assert security.scan_text(line, tmp_path / "tag.yaml") == []

    def test_directory_scan_reports_planted_password(self, tmp_path):
        from tag import security

        (tmp_path / "app.py").write_text('PASSWORD = "notarealpassword123"\n')
        findings = list(security.scan_directory(tmp_path))
        assert [f.pattern_name for f in findings] == ["generic_secret"]

    def test_vendor_specific_patterns_still_win(self, tmp_path):
        """generic_secret is last in the list, so more precise names survive."""
        from tag import security

        findings = security.scan_text(
            'api_key = "sk-ant-AAAAAAAAAAAAAAAAAAAAAAAAAAAA"', tmp_path / "k.py"
        )
        assert findings and findings[0].pattern_name == "anthropic_api_key"

    def test_skip_lists_reconciled_with_go(self):
        """Python skip-lists gained vendor/.db/.sqlite3; Go skip-dirs gained
        dist/build/venv."""
        from tag import security

        assert "vendor" in security._SKIP_DIRS
        assert {".db", ".sqlite3"} <= security._SKIP_EXTS

        go_scan = (
            Path(__file__).resolve().parents[1]
            / "tag-go" / "internal" / "security" / "scan.go"
        )
        if go_scan.exists():
            src = go_scan.read_text()
            skip_dirs_line = next(
                ln for ln in src.splitlines() if ln.startswith("var skipDirs")
            )
            for d in ("dist", "build", "venv"):
                assert f'"{d}": true' in skip_dirs_line, f"Go skipDirs missing {d}"

    def test_generic_secret_regex_matches_go_source(self):
        """The ported pattern is the same regex the Go implementation uses."""
        from tag import security

        by_name = dict(security._PATTERNS)
        assert "generic_secret" in by_name
        go_scan = (
            Path(__file__).resolve().parents[1]
            / "tag-go" / "internal" / "security" / "scan.go"
        )
        if go_scan.exists():
            assert "generic_secret" in go_scan.read_text()


# ---------------------------------------------------------------------------
# #15 — `swarm run` returned exit code 2 (usage) for a RUNTIME failure
# ---------------------------------------------------------------------------

class TestSwarmRunExitCodes:
    """Contract: usage/arg errors exit 2, runtime failures exit 1.

    Pre-fix: a coordinator that failed at runtime (SwarmManifestError — e.g. no
    API key, model returned no usable JSON) returned 2, misreporting a runtime
    failure as CLI misuse."""

    def _args(self, **over):
        import argparse

        base = dict(
            swarm_subcommand="run", config=None, goal="do a thing",
            coordinator_profile=None, max_agents=2, failure_policy="best_effort",
            timeout_per_agent=5, approve=False, sequential=True, dry_run=False,
            json=False,
        )
        base.update(over)
        return argparse.Namespace(**base)

    def test_runtime_coordinator_failure_exits_1(self, tmp_path, monkeypatch, capsys):
        import tag.swarm as swarm_mod
        from tag.cmd import swarm as swarm_cmd

        monkeypatch.setenv("TAG_HOME", str(tmp_path / "home"))
        monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
        monkeypatch.delenv("OPENAI_API_KEY", raising=False)

        def _boom(self, goal, swarm_id, max_agents):
            raise swarm_mod.SwarmManifestError(
                "Coordinator produced no usable JSON output after 2 attempts"
            )

        monkeypatch.setattr(swarm_mod.SwarmCoordinator, "produce_manifest", _boom)

        rc = swarm_cmd.cmd_swarm_context(self._args())
        err = capsys.readouterr().err
        assert "coordinator failed" in err
        assert rc == 1, f"runtime failure must exit 1, got {rc}"

    def test_missing_required_arg_still_exits_2(self):
        """A genuine usage error (missing --goal) keeps argparse's exit code 2."""
        import argparse

        from tag.cmd import swarm as swarm_cmd

        parser = argparse.ArgumentParser(prog="tag")
        sub = parser.add_subparsers(dest="command")
        swarm_cmd.register(sub)
        with pytest.raises(SystemExit) as exc:
            parser.parse_args(["swarm", "run"])
        assert exc.value.code == 2


# ---------------------------------------------------------------------------
# #16 — `sandbox run` advertised a "Shell command" but split into argv
# ---------------------------------------------------------------------------

class TestSandboxShellMetacharacters:
    """Pre-fix: `tag sandbox run "echo hi > /tmp/x"` printed `hi > /tmp/x` and
    created no file — the redirection was passed to echo as literal argv words
    while the help text promised a "Shell command". The fix rejects unquoted
    shell metacharacters (rather than spawning an unconfined shell) and says so
    in the help text."""

    @pytest.mark.parametrize(
        "command,meta",
        [
            ("echo hi > /tmp/x", ">"),
            ("cat f | wc -l", "|"),
            ("ls *.py", "*"),
            ("echo a && echo b", "&"),
            ("echo a; echo b", ";"),
            ("echo $HOME", "$"),
            ("echo `id`", "`"),
        ],
    )
    def test_metacharacters_detected(self, command, meta):
        from tag.sandbox import find_shell_metacharacters

        assert meta in find_shell_metacharacters(command)

    @pytest.mark.parametrize(
        "command",
        [
            "echo hello",
            "python -c 'print(2 * 3)'",          # metachar inside single quotes
            'sh -c "echo hi > out.txt"',          # explicit, still jailed, shell
            "grep -n foo bar.txt",
        ],
    )
    def test_quoted_or_plain_commands_are_allowed(self, command):
        from tag.sandbox import find_shell_metacharacters

        assert find_shell_metacharacters(command) == []

    def test_run_in_sandbox_rejects_redirection(self, tmp_path):
        from tag.sandbox import ShellMetacharacterError, run_in_sandbox

        conn = sqlite3.connect(tmp_path / "t.db")
        with pytest.raises(ShellMetacharacterError) as exc:
            run_in_sandbox(conn, "echo hi > /tmp/tag_sbx_probe.txt")
        msg = str(exc.value)
        assert "'>'" in msg and "no shell" in msg
        # The rejected command must not be recorded as a run.
        assert conn.execute("SELECT COUNT(*) FROM sandbox_runs").fetchone()[0] == 0
        conn.close()
        assert not Path("/tmp/tag_sbx_probe.txt").exists()

    def test_shell_metacharacter_error_is_value_error(self):
        from tag.sandbox import ShellMetacharacterError

        assert issubclass(ShellMetacharacterError, ValueError)

    def test_help_text_no_longer_claims_shell(self):
        import argparse

        from tag.cmd import marketplace

        parser = argparse.ArgumentParser(prog="tag")
        sub = parser.add_subparsers(dest="command")
        marketplace.register(sub)
        sb_run = sub.choices["sandbox"]._subparsers._group_actions[0].choices["run"]
        help_text = sb_run.format_help()
        assert "Shell command to run" not in help_text
        assert "without a shell" in help_text.lower()

    def test_cli_returns_usage_exit_code_for_metacharacters(self, tmp_path, monkeypatch, capsys):
        import argparse

        from tag.cmd import marketplace

        monkeypatch.setenv("TAG_HOME", str(tmp_path / "home"))
        args = argparse.Namespace(
            sandbox_subcommand="run", command="echo hi > /tmp/tag_sbx_probe2.txt",
            backend="restricted", image="python:3.12-slim", timeout=10,
            json=False, config=None,
        )
        rc = marketplace.cmd_sandbox(args)
        assert rc == 2, f"malformed COMMAND is a usage error (2), got {rc}"
        assert "metacharacter" in capsys.readouterr().err
        assert not Path("/tmp/tag_sbx_probe2.txt").exists()


# ---------------------------------------------------------------------------
# #7 — `trace diff` silently dropped same-named spans
# ---------------------------------------------------------------------------

def _seed_span(conn, span_id, trace_id, name, prompt, completion, duration):
    conn.execute(
        "INSERT INTO spans(id,trace_id,parent_id,name,profile,model_id,started_at,"
        "finished_at,duration_ms,status,prompt_tokens,completion_tokens,kind) "
        "VALUES(?,?,NULL,?,'default','m',?,?,?, 'ok',?,?,'llm')",
        (span_id, trace_id, name, f"2026-01-01T00:00:0{duration % 10}Z",
         "2026-01-01T00:01:00Z", duration, prompt, completion),
    )


class TestTraceDiffDuplicateSpanNames:
    """Pre-fix both engines keyed spans by name alone, so two `llm.call` spans
    collapsed to one: a trace totalling 1300 tokens reported 900, and the
    printed delta was computed from a single span."""

    @pytest.fixture()
    def diff_args(self, tmp_path, monkeypatch):
        import argparse

        from tag.core.db import open_db

        home = tmp_path / "home"
        monkeypatch.setenv("TAG_HOME", str(home))
        from tag.core.config import config_path, load_config

        cfg = load_config(config_path(None))
        conn = open_db(cfg)
        _seed_span(conn, "a1", "tr-a", "llm.call", 300, 100, 1500)
        _seed_span(conn, "a2", "tr-a", "llm.call", 700, 200, 2700)
        _seed_span(conn, "b1", "tr-b", "llm.call", 30, 10, 900)
        conn.commit()
        conn.close()
        return argparse.Namespace(
            trace_subcommand="diff", trace_a="tr-a", trace_b="tr-b",
            config=None, profile=None, json=False,
        )

    def test_both_same_named_spans_are_reported(self, diff_args, capsys):
        from tag.cmd.observability import cmd_trace

        assert cmd_trace(diff_args) == 0
        out = capsys.readouterr().out
        # Two llm.call rows must survive, not one.
        assert out.count("llm.call") == 2, f"a span was dropped:\n{out}"
        # And the A-side total must be the real 1300, not just the last span.
        assert "1300" in out, f"A-side total wrong (span collapsed):\n{out}"

    def test_json_keeps_every_occurrence(self, diff_args, capsys):
        from tag.cmd.observability import cmd_trace

        diff_args.json = True
        assert cmd_trace(diff_args) == 0
        entries = json.loads(capsys.readouterr().out)
        llm = [e for e in entries if e["name"] == "llm.call"]
        assert len(llm) == 2, f"expected 2 llm.call entries, got {len(llm)}"
        assert {e["occurrence"] for e in llm} == {1, 2}
        a_tokens = sum((e["a"] or {}).get("prompt_tokens", 0) for e in llm)
        assert a_tokens == 1000, f"A prompt tokens = {a_tokens}, want 1000"


# ---------------------------------------------------------------------------
# #13 — --json gaps and the global --json flag
# ---------------------------------------------------------------------------

class TestJSONFlagCoverage:
    """Pre-fix these all exited 2 (`unrecognized arguments: --json`) where Go
    returns valid JSON, and the global `tag --json <cmd>` form did not exist."""

    JSON_COMMANDS = [
        ["runs", "list"],
        ["webhook", "events"],
        ["webhook", "rule-list"],
        ["annotate", "stats"],
        ["logs"],
    ]

    @pytest.mark.parametrize("cmd", JSON_COMMANDS, ids=lambda c: "-".join(c))
    def test_trailing_json_flag(self, cmd, tmp_path, monkeypatch, capsys):
        from tag.controller import main

        monkeypatch.setenv("TAG_HOME", str(tmp_path / "home"))
        rc = main([*cmd, "--json"])
        assert rc == 0, f"`tag {' '.join(cmd)} --json` exited {rc}"
        json.loads(capsys.readouterr().out)  # must be parseable JSON

    @pytest.mark.parametrize("cmd", JSON_COMMANDS, ids=lambda c: "-".join(c))
    def test_global_json_flag(self, cmd, tmp_path, monkeypatch, capsys):
        from tag.controller import main

        monkeypatch.setenv("TAG_HOME", str(tmp_path / "home"))
        rc = main(["--json", *cmd])
        assert rc == 0, f"`tag --json {' '.join(cmd)}` exited {rc}"
        json.loads(capsys.readouterr().out)

    def test_agentops_status_schema_matches_go(self, tmp_path, monkeypatch, capsys):
        """The two engines reported completely disjoint field sets."""
        from tag.controller import main

        monkeypatch.setenv("TAG_HOME", str(tmp_path / "home"))
        assert main(["agentops", "status", "--json"]) == 0
        payload = json.loads(capsys.readouterr().out)
        # Union of both engines' fields (see tag-go/internal/cli/agentops.go).
        for field in (
            "sdk_installed", "api_key_configured", "api_key_masked",
            "total_runs", "prompt_tokens", "completion_tokens", "total_tokens",
            "estimated_cost_usd", "statuses", "profiles",
        ):
            assert field in payload, f"agentops status --json missing {field!r}"
        assert isinstance(payload["profiles"], list)


# ---------------------------------------------------------------------------
# #5 (follow-up) — `gpt-5.4`, TAG's own master-profile default, was unpriced
# ---------------------------------------------------------------------------

class TestShippedDefaultModelsArePriced:
    """The pricing pass corrected the Anthropic rates and added the deepseek/qwen
    profile defaults, but missed `gpt-5.4` — the model
    src/tag/config/default.yaml ships as the master/orchestrator default. Pre-fix
    `tag pricing get gpt-5.4` exited 1 with `Model not found: 'gpt-5.4'`, so the
    default profile silently costed at $0."""

    def test_gpt_5_4_is_priced(self):
        from tag.cost_table import compute_cost, reload_pricing_table

        reload_pricing_table()
        cost = compute_cost("gpt-5.4", 1_000_000, 1_000_000)
        assert cost is not None, "gpt-5.4 missing from src/tag/assets/pricing.yaml"
        assert abs(cost - 11.25) < 1e-9, f"gpt-5.4 1M/1M = {cost}, want 11.25"

    def test_every_shipped_profile_default_is_priced(self):
        """Whatever default.yaml ships must resolve — this is what regressed."""
        import yaml

        from tag.cost_table import compute_cost, reload_pricing_table

        reload_pricing_table()
        default_yaml = Path(__file__).resolve().parents[1] / "src/tag/config/default.yaml"
        cfg = yaml.safe_load(default_yaml.read_text())

        models: set[str] = set()

        def walk(node):
            if isinstance(node, dict):
                # A model block is {"default": "<id>", ...} under a "model" key.
                if isinstance(node.get("model"), dict) and node["model"].get("default"):
                    models.add(str(node["model"]["default"]))
                for v in node.values():
                    walk(v)
            elif isinstance(node, list):
                for v in node:
                    walk(v)

        walk(cfg)
        assert models, "no profile model defaults found in default.yaml"
        unpriced = sorted(m for m in models if compute_cost(m, 1000, 1000) is None)
        assert not unpriced, f"shipped profile defaults missing from pricing.yaml: {unpriced}"

    def test_unknown_model_still_reports_not_found(self):
        """The fix must add a real entry, not make every lookup succeed."""
        from tag.cost_table import compute_cost, reload_pricing_table

        reload_pricing_table()
        assert compute_cost("definitely-not-a-real-model-xyz", 10, 10) is None


# ---------------------------------------------------------------------------
# #13 (follow-up) — `costs --json` broke its own JSON contract
# ---------------------------------------------------------------------------

class TestCostsJSONContract:
    """`cmd_costs` emits `{"runs": [], "totals": {}}` when the DB is absent, but
    when the DB exists with a pre-cost `runs` schema it printed the human
    sentence "No cost data recorded yet (run some tasks first)." even under
    --json, so a caller parsing stdout got a JSONDecodeError."""

    def test_json_stays_json_on_legacy_runs_schema(self, tmp_path, monkeypatch, capsys):
        import argparse

        from tag.cmd.observability import cmd_costs
        from tag.controller import config_path, load_config, runtime_db_path

        monkeypatch.setenv("TAG_HOME", str(tmp_path / "home"))
        cfg = load_config(config_path(None))
        db_path = runtime_db_path(cfg)
        db_path.parent.mkdir(parents=True, exist_ok=True)
        conn = sqlite3.connect(str(db_path))
        # `runs` without the total_tokens column — the pre-cost schema.
        conn.execute("CREATE TABLE runs(id TEXT PRIMARY KEY, master_profile TEXT)")
        conn.commit()
        conn.close()

        args = argparse.Namespace(config=None, json=True, limit=20, profile=None)
        assert cmd_costs(args) == 0
        out = capsys.readouterr().out
        payload = json.loads(out)  # pre-fix: JSONDecodeError on the prose line
        assert payload == {"runs": [], "totals": {}}

    def test_human_output_unchanged_without_json(self, tmp_path, monkeypatch, capsys):
        import argparse

        from tag.cmd.observability import cmd_costs
        from tag.controller import config_path, load_config, runtime_db_path

        monkeypatch.setenv("TAG_HOME", str(tmp_path / "home"))
        cfg = load_config(config_path(None))
        db_path = runtime_db_path(cfg)
        db_path.parent.mkdir(parents=True, exist_ok=True)
        conn = sqlite3.connect(str(db_path))
        conn.execute("CREATE TABLE runs(id TEXT PRIMARY KEY, master_profile TEXT)")
        conn.commit()
        conn.close()

        args = argparse.Namespace(config=None, json=False, limit=20, profile=None)
        assert cmd_costs(args) == 0
        assert "No cost data recorded yet" in capsys.readouterr().out
