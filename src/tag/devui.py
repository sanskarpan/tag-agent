"""PRD-054: Local browser-based agent execution visualizer (TAG DevUI).

Serves a minimal HTML dashboard showing real-time agent traces, eval results,
and memory stats from the TAG SQLite database using only stdlib.
"""
from __future__ import annotations

import json
import sqlite3
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, urlparse


# ---------------------------------------------------------------------------
# DB helper
# ---------------------------------------------------------------------------

def _query_db(db_path: str | Path, sql: str, params: tuple = ()) -> list[dict]:
    """Execute *sql* against *db_path* and return rows as a list of dicts.

    Returns an empty list on any error (table may not exist yet).
    """
    try:
        conn = sqlite3.connect(str(db_path))
        conn.row_factory = sqlite3.Row
        try:
            cur = conn.execute(sql, params)
            return [dict(row) for row in cur.fetchall()]
        finally:
            conn.close()
    except Exception:
        return []


def _safe_limit(raw: str, default: int = 50, maximum: int = 1000) -> int:
    """Parse a `limit` query param defensively.

    A non-integer value (e.g. ``?limit=abc``) or a negative value (which SQLite
    treats as unbounded) falls back to *default*; oversized values are clamped
    to *maximum*. Never raises, so the endpoint can't crash on hostile input.
    """
    try:
        value = int(raw)
    except (TypeError, ValueError):
        return default
    if value < 0:
        return default
    return min(value, maximum)


# ---------------------------------------------------------------------------
# Inline HTML dashboard
# ---------------------------------------------------------------------------

_DASHBOARD_HTML = """\
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1.0"/>
<title>TAG DevUI</title>
<style>
  :root {
    --bg: #0f1117;
    --sidebar-bg: #161b22;
    --card-bg: #1c2128;
    --border: #30363d;
    --text: #e6edf3;
    --muted: #7d8590;
    --accent: #58a6ff;
    --green: #3fb950;
    --red: #f85149;
    --yellow: #d29922;
  }
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { background: var(--bg); color: var(--text); font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', monospace; display: flex; flex-direction: column; height: 100vh; }

  /* Header */
  header {
    background: var(--sidebar-bg);
    border-bottom: 1px solid var(--border);
    padding: 12px 20px;
    display: flex;
    align-items: center;
    justify-content: space-between;
    flex-shrink: 0;
  }
  header h1 { font-size: 18px; color: var(--accent); letter-spacing: 0.03em; }
  header .meta { font-size: 12px; color: var(--muted); }
  #refresh-status { font-size: 11px; color: var(--muted); }

  /* Body layout */
  .layout { display: flex; flex: 1; overflow: hidden; }

  /* Sidebar nav */
  nav {
    width: 160px;
    background: var(--sidebar-bg);
    border-right: 1px solid var(--border);
    padding: 16px 0;
    flex-shrink: 0;
  }
  nav a {
    display: block;
    padding: 10px 20px;
    color: var(--muted);
    text-decoration: none;
    font-size: 14px;
    cursor: pointer;
    border-left: 3px solid transparent;
    transition: color 0.15s, border-color 0.15s;
  }
  nav a:hover { color: var(--text); }
  nav a.active { color: var(--accent); border-left-color: var(--accent); background: rgba(88,166,255,0.06); }

  /* Stats bar */
  #stats-bar {
    display: flex;
    gap: 16px;
    padding: 10px 20px;
    background: var(--card-bg);
    border-bottom: 1px solid var(--border);
    font-size: 13px;
    flex-shrink: 0;
  }
  .stat { display: flex; flex-direction: column; align-items: center; gap: 2px; }
  .stat .label { font-size: 10px; color: var(--muted); text-transform: uppercase; letter-spacing: 0.06em; }
  .stat .value { font-weight: 600; color: var(--accent); }

  /* Main content */
  main { flex: 1; overflow-y: auto; padding: 20px; }

  /* Section panels */
  .panel { display: none; }
  .panel.active { display: block; }

  h2 { font-size: 15px; margin-bottom: 14px; color: var(--text); }

  /* Tables */
  table { width: 100%; border-collapse: collapse; font-size: 13px; }
  th { text-align: left; padding: 8px 10px; background: var(--card-bg); color: var(--muted); font-weight: 500; border-bottom: 1px solid var(--border); position: sticky; top: 0; }
  td { padding: 7px 10px; border-bottom: 1px solid var(--border); vertical-align: top; word-break: break-all; max-width: 300px; }
  tr:hover td { background: rgba(255,255,255,0.02); }

  /* Status badges */
  .badge {
    display: inline-block;
    padding: 2px 7px;
    border-radius: 10px;
    font-size: 11px;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
  .badge-ok, .badge-pass, .badge-running { background: rgba(63,185,80,0.15); color: var(--green); }
  .badge-error, .badge-fail { background: rgba(248,81,73,0.15); color: var(--red); }
  .badge-timeout, .badge-warn { background: rgba(210,153,34,0.15); color: var(--yellow); }

  /* Empty state */
  .empty { color: var(--muted); font-size: 13px; padding: 30px 0; text-align: center; }

  /* JSON fallback */
  pre.json { background: var(--card-bg); border: 1px solid var(--border); border-radius: 6px; padding: 12px; font-size: 12px; overflow-x: auto; color: var(--text); white-space: pre-wrap; }

  /* Cost column color */
  .cost { color: var(--yellow); font-family: monospace; }
  .trace-id { font-family: monospace; font-size: 11px; color: var(--muted); }

  /* Scrollbar */
  ::-webkit-scrollbar { width: 6px; height: 6px; }
  ::-webkit-scrollbar-track { background: transparent; }
  ::-webkit-scrollbar-thumb { background: var(--border); border-radius: 3px; }
</style>
</head>
<body>
<header>
  <h1>TAG DevUI</h1>
  <span class="meta" id="db-path"></span>
  <span id="refresh-status">Loading…</span>
</header>
<div class="layout">
  <nav>
    <a class="active" data-panel="traces" onclick="switchPanel('traces')">Traces</a>
    <a data-panel="evals" onclick="switchPanel('evals')">Evals</a>
    <a data-panel="memories" onclick="switchPanel('memories')">Memories</a>
    <a data-panel="alerts" onclick="switchPanel('alerts')">Alerts</a>
  </nav>
  <div style="flex:1;display:flex;flex-direction:column;overflow:hidden;">
    <div id="stats-bar">
      <div class="stat"><span class="label">Spans</span><span class="value" id="s-spans">—</span></div>
      <div class="stat"><span class="label">Runs</span><span class="value" id="s-runs">—</span></div>
      <div class="stat"><span class="label">Cost (USD)</span><span class="value" id="s-cost">—</span></div>
      <div class="stat"><span class="label">Memories</span><span class="value" id="s-mem">—</span></div>
    </div>
    <main>
      <!-- Traces -->
      <div id="panel-traces" class="panel active">
        <h2>Recent Spans</h2>
        <div id="traces-content"><span class="empty">Loading…</span></div>
      </div>

      <!-- Evals -->
      <div id="panel-evals" class="panel">
        <h2>Eval Runs</h2>
        <div id="evals-content"><span class="empty">Loading…</span></div>
        <h2 style="margin-top:24px;">Judge Runs</h2>
        <div id="judge-content"><span class="empty">Loading…</span></div>
      </div>

      <!-- Memories -->
      <div id="panel-memories" class="panel">
        <h2>Memories</h2>
        <div id="memories-content"><span class="empty">Loading…</span></div>
      </div>

      <!-- Alerts -->
      <div id="panel-alerts" class="panel">
        <h2>Alert Firings</h2>
        <div id="alerts-content"><span class="empty">Loading…</span></div>
      </div>
    </main>
  </div>
</div>
<script>
'use strict';

let currentPanel = 'traces';

function switchPanel(name) {
  currentPanel = name;
  document.querySelectorAll('.panel').forEach(p => p.classList.remove('active'));
  document.querySelectorAll('nav a').forEach(a => a.classList.remove('active'));
  document.getElementById('panel-' + name).classList.add('active');
  document.querySelector('[data-panel="' + name + '"]').classList.add('active');
  loadPanel(name);
}

async function apiFetch(url) {
  try {
    const r = await fetch(url);
    if (!r.ok) return [];
    return await r.json();
  } catch (e) {
    return [];
  }
}

function badge(val) {
  if (!val) return '';
  const cls = val.toLowerCase();
  return '<span class="badge badge-' + cls + '">' + val + '</span>';
}

function esc(s) {
  if (s === null || s === undefined) return '';
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

function renderTable(rows, cols) {
  if (!rows || rows.length === 0) return '<div class="empty">No data yet.</div>';
  let html = '<table><thead><tr>' + cols.map(c => '<th>' + esc(c.label) + '</th>').join('') + '</tr></thead><tbody>';
  for (const row of rows) {
    html += '<tr>';
    for (const c of cols) {
      let val = row[c.key];
      if (c.render) { html += '<td>' + c.render(val, row) + '</td>'; }
      else { html += '<td>' + esc(val === null || val === undefined ? '' : val) + '</td>'; }
    }
    html += '</tr>';
  }
  html += '</tbody></table>';
  return html;
}

async function loadStats() {
  const s = await apiFetch('/api/stats');
  if (s && typeof s === 'object') {
    document.getElementById('s-spans').textContent = s.total_spans ?? '—';
    document.getElementById('s-runs').textContent = s.total_runs ?? '—';
    document.getElementById('s-cost').textContent = s.total_cost_usd != null ? '$' + Number(s.total_cost_usd).toFixed(4) : '—';
    document.getElementById('s-mem').textContent = s.total_memories ?? '—';
    if (s.db) document.getElementById('db-path').textContent = s.db;
  }
}

async function loadTraces() {
  const rows = await apiFetch('/api/spans?limit=100');
  const cols = [
    { key: 'started_at', label: 'Time', render: v => '<span style="white-space:nowrap;font-size:11px;">' + esc((v||'').replace('T',' ').substring(0,19)) + '</span>' },
    { key: 'name', label: 'Name' },
    { key: 'profile', label: 'Profile' },
    { key: 'model_id', label: 'Model' },
    { key: 'status', label: 'Status', render: v => badge(v) },
    { key: 'duration_ms', label: 'ms', render: v => v != null ? Number(v).toFixed(0) : '' },
    { key: 'prompt_tokens', label: 'Prompt T' },
    { key: 'completion_tokens', label: 'Comp T' },
    { key: 'cost_usd', label: 'Cost', render: v => v != null ? '<span class="cost">$' + Number(v).toFixed(5) + '</span>' : '' },
    { key: 'trace_id', label: 'Trace ID', render: v => '<span class="trace-id">' + esc((v||'').substring(0,12)) + '</span>' },
  ];
  document.getElementById('traces-content').innerHTML = renderTable(rows, cols);
}

async function loadEvals() {
  const rows = await apiFetch('/api/eval_runs?limit=50');
  const cols = [
    { key: 'created_at', label: 'Time', render: v => '<span style="white-space:nowrap;font-size:11px;">' + esc((v||'').replace('T',' ').substring(0,19)) + '</span>' },
    { key: 'suite_name', label: 'Suite' },
    { key: 'profile', label: 'Profile' },
    { key: 'status', label: 'Status', render: v => badge(v) },
    { key: 'pass_count', label: 'Pass', render: v => '<span style="color:var(--green)">' + esc(v) + '</span>' },
    { key: 'fail_count', label: 'Fail', render: v => v > 0 ? '<span style="color:var(--red)">' + esc(v) + '</span>' : esc(v) },
    { key: 'total_count', label: 'Total' },
  ];
  document.getElementById('evals-content').innerHTML = renderTable(rows, cols);

  const jrows = await apiFetch('/api/judge_runs?limit=50');
  const jcols = [
    { key: 'created_at', label: 'Time', render: v => '<span style="white-space:nowrap;font-size:11px;">' + esc((v||'').replace('T',' ').substring(0,19)) + '</span>' },
    { key: 'eval_run_id', label: 'Eval Run', render: v => '<span class="trace-id">' + esc((v||'').substring(0,12)) + '</span>' },
    { key: 'judge_model', label: 'Judge Model' },
    { key: 'status', label: 'Status', render: v => badge(v) },
    { key: 'pass_count', label: 'Pass', render: v => '<span style="color:var(--green)">' + esc(v) + '</span>' },
    { key: 'fail_count', label: 'Fail', render: v => v > 0 ? '<span style="color:var(--red)">' + esc(v) + '</span>' : esc(v) },
    { key: 'total_count', label: 'Total' },
  ];
  if (!jrows || jrows.length === 0) {
    document.getElementById('judge-content').innerHTML = '<div class="empty">No judge runs yet.</div>';
  } else {
    document.getElementById('judge-content').innerHTML = renderTable(jrows, jcols);
  }
}

async function loadMemories() {
  const rows = await apiFetch('/api/memories?limit=100');
  const cols = [
    { key: 'created_at', label: 'Time', render: v => '<span style="white-space:nowrap;font-size:11px;">' + esc((v||'').replace('T',' ').substring(0,19)) + '</span>' },
    { key: 'profile', label: 'Profile' },
    { key: 'memory_type', label: 'Type' },
    { key: 'content', label: 'Content', render: v => '<span style="max-width:400px;display:block;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;" title="' + esc(v) + '">' + esc(v) + '</span>' },
    { key: 'confidence_base', label: 'Confidence', render: v => v != null ? Number(v).toFixed(2) : '' },
  ];
  document.getElementById('memories-content').innerHTML = renderTable(rows, cols);
}

async function loadAlerts() {
  const rows = await apiFetch('/api/alerts?limit=50');
  const cols = [
    { key: 'fired_at', label: 'Time', render: v => '<span style="white-space:nowrap;font-size:11px;">' + esc((v||'').replace('T',' ').substring(0,19)) + '</span>' },
    { key: 'alert_name', label: 'Alert' },
    { key: 'profile', label: 'Profile' },
    { key: 'severity', label: 'Severity', render: v => badge(v) },
    { key: 'message', label: 'Message' },
    { key: 'resolved_at', label: 'Resolved' },
  ];
  if (!rows || rows.length === 0) {
    document.getElementById('alerts-content').innerHTML = '<div class="empty">No alert firings yet.</div>';
  } else {
    document.getElementById('alerts-content').innerHTML = renderTable(rows, cols);
  }
}

async function loadPanel(name) {
  if (name === 'traces') await loadTraces();
  else if (name === 'evals') await loadEvals();
  else if (name === 'memories') await loadMemories();
  else if (name === 'alerts') await loadAlerts();
}

async function refresh() {
  const ts = new Date().toLocaleTimeString();
  document.getElementById('refresh-status').textContent = 'Refreshing…';
  await Promise.all([loadStats(), loadPanel(currentPanel)]);
  document.getElementById('refresh-status').textContent = 'Updated ' + ts;
}

// Initial load + periodic refresh
refresh();
setInterval(refresh, 10000);
</script>
</body>
</html>
"""


# ---------------------------------------------------------------------------
# HTTP request handler
# ---------------------------------------------------------------------------

class _Handler(BaseHTTPRequestHandler):
    """Minimal HTTP request handler for the TAG DevUI."""

    # Injected by DevUIServer before creating the HTTPServer
    db_path: str = ""

    def log_message(self, format: str, *args: Any) -> None:  # noqa: A002
        # Suppress default access log noise
        pass

    def _send_json(self, data: Any, status: int = 200) -> None:
        body = json.dumps(data, default=str).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        # No wildcard CORS: these endpoints expose local spans/costs/memories.
        # A `*` ACAO would let any visited web page read them cross-origin,
        # defeating the 127.0.0.1 bind.
        self.end_headers()
        self.wfile.write(body)

    def _send_html(self, body: str) -> None:
        encoded = body.encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def do_GET(self) -> None:  # noqa: N802
        parsed = urlparse(self.path)
        qs = parse_qs(parsed.query)
        path = parsed.path

        def qp(key: str, default: str = "") -> str:
            return qs.get(key, [default])[0]

        db = self.db_path

        if path == "/" or path == "":
            # The template carries no __DB_PATH__ token (dead substitution) and
            # the absolute host DB path must not be embedded in the page.
            self._send_html(_DASHBOARD_HTML)

        elif path == "/health":
            self._send_json({"status": "ok"})

        elif path == "/api/stats":
            spans_rows = _query_db(db, "SELECT COUNT(*) AS n, SUM(cost_usd) AS c FROM spans")
            runs_rows = _query_db(db, "SELECT COUNT(*) AS n FROM eval_runs")
            mem_rows = _query_db(db, "SELECT COUNT(*) AS n FROM semantic_memories")
            total_spans = spans_rows[0]["n"] if spans_rows else 0
            total_cost = spans_rows[0]["c"] if spans_rows else 0
            total_runs = runs_rows[0]["n"] if runs_rows else 0
            total_mem = mem_rows[0]["n"] if mem_rows else 0
            self._send_json({
                "total_spans": total_spans or 0,
                "total_runs": total_runs or 0,
                "total_cost_usd": round(float(total_cost or 0), 6),
                "total_memories": total_mem or 0,
            })

        elif path == "/api/spans":
            limit = _safe_limit(qp("limit", "50"), 50)
            trace_id = qp("trace_id")
            if trace_id:
                rows = _query_db(
                    db,
                    "SELECT * FROM spans WHERE trace_id = ? ORDER BY started_at DESC LIMIT ?",
                    (trace_id, limit),
                )
            else:
                rows = _query_db(
                    db,
                    "SELECT * FROM spans ORDER BY started_at DESC LIMIT ?",
                    (limit,),
                )
            self._send_json(rows)

        elif path == "/api/eval_runs":
            limit = _safe_limit(qp("limit", "20"), 20)
            rows = _query_db(
                db,
                "SELECT * FROM eval_runs ORDER BY created_at DESC LIMIT ?",
                (limit,),
            )
            self._send_json(rows)

        elif path == "/api/judge_runs":
            limit = _safe_limit(qp("limit", "20"), 20)
            rows = _query_db(
                db,
                "SELECT * FROM judge_runs ORDER BY created_at DESC LIMIT ?",
                (limit,),
            )
            self._send_json(rows)

        elif path == "/api/memories":
            limit = _safe_limit(qp("limit", "50"), 50)
            profile = qp("profile")
            if profile:
                rows = _query_db(
                    db,
                    "SELECT * FROM semantic_memories WHERE profile = ? ORDER BY created_at DESC LIMIT ?",
                    (profile, limit),
                )
            else:
                rows = _query_db(
                    db,
                    "SELECT * FROM semantic_memories ORDER BY created_at DESC LIMIT ?",
                    (limit,),
                )
            self._send_json(rows)

        elif path == "/api/alerts":
            limit = _safe_limit(qp("limit", "20"), 20)
            rows = _query_db(
                db,
                "SELECT * FROM alert_firings ORDER BY fired_at DESC LIMIT ?",
                (limit,),
            )
            self._send_json(rows)

        else:
            self._send_json({"error": "not found"}, 404)


# ---------------------------------------------------------------------------
# DevUIServer
# ---------------------------------------------------------------------------

class DevUIServer:
    """Minimal HTTP server exposing the TAG DevUI dashboard."""

    def __init__(
        self,
        db_path: str | Path,
        host: str = "127.0.0.1",
        port: int = 7881,
    ) -> None:
        self.db_path = str(db_path)
        self.host = host
        self.port = port
        self._server: HTTPServer | None = None
        self._thread: threading.Thread | None = None

    def _build_server(self) -> HTTPServer:
        db_path = self.db_path

        class BoundHandler(_Handler):
            pass

        BoundHandler.db_path = db_path
        server = HTTPServer((self.host, self.port), BoundHandler)
        return server

    @property
    def url(self) -> str:
        return f"http://{self.host}:{self.port}"

    def start(self) -> None:
        """Start the server in the current thread (blocking)."""
        self._server = self._build_server()
        print(f"TAG DevUI running at {self.url}  (db: {self.db_path})", flush=True)
        try:
            self._server.serve_forever()
        except KeyboardInterrupt:
            pass
        finally:
            self._server.server_close()

    def start_background(self) -> None:
        """Start the server in a daemon thread (non-blocking)."""
        self._server = self._build_server()
        t = threading.Thread(target=self._server.serve_forever, daemon=True)
        t.start()
        self._thread = t
        print(f"TAG DevUI running at {self.url}  (db: {self.db_path})", flush=True)

    def wait(self) -> None:
        """Block until the background serve thread exits (or Ctrl-C)."""
        if self._thread is not None:
            self._thread.join()

    def stop(self) -> None:
        """Shut down the server if running."""
        if self._server is not None:
            self._server.shutdown()
            self._server.server_close()
            self._server = None


# ---------------------------------------------------------------------------
# Controller entry point
# ---------------------------------------------------------------------------

def cmd_devui(args: Any) -> int:
    """Entry point called from controller.py for `tag devui` subcommand.

    Expected attributes on *args*:
        args.port       int  (default 7881)
        args.host       str  (default "127.0.0.1")
        args.background bool
        args.db         str | Path  (TAG SQLite database path)
    """
    db_path: str | Path = getattr(args, "db", "tag.db")
    host: str = getattr(args, "host", "127.0.0.1")
    port: int = int(getattr(args, "port", 7881))
    background: bool = bool(getattr(args, "background", False))

    server = DevUIServer(db_path=db_path, host=host, port=port)

    if background:
        # The serve loop runs in a daemon thread, which the interpreter kills on
        # exit. Returning immediately would let the CLI tear down the process and
        # silently stop serving (a false-success no-op), so block on the thread
        # to keep the server alive until interrupted.
        server.start_background()
        try:
            server.wait()
        except KeyboardInterrupt:
            server.stop()
        return 0
    else:
        server.start()
        return 0
