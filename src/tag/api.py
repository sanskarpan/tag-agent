"""PRD-036: Web Dashboard backend (tag serve --web).

Lightweight HTTP API server using stdlib http.server + SSE streaming.
Reads from existing SQLite schema (runs, spans, queue_jobs).
Binds to 127.0.0.1 by default; pass --host 0.0.0.0 for LAN access.

Endpoints:
  GET /           → HTML dashboard
  GET /api/runs   → JSON list of recent runs
  GET /api/spans/<run_id> → JSON span waterfall for one run
  GET /api/queue  → JSON queue jobs
  GET /api/costs  → JSON cost summary per profile
  GET /api/stream → SSE live feed (runs+queue delta every 2s)
"""
from __future__ import annotations

import json
import sqlite3
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, HTTPServer, ThreadingHTTPServer
from pathlib import Path
from typing import Any


# ---------------------------------------------------------------------------
# Data access helpers
# ---------------------------------------------------------------------------

def _fetch_runs(conn: sqlite3.Connection, limit: int = 50) -> list[dict]:
    try:
        rows = conn.execute(
            """SELECT id, master_profile, status, created_at, estimated_cost_usd,
                   prompt_tokens, completion_tokens, model_id
               FROM runs ORDER BY created_at DESC LIMIT ?""",
            (limit,),
        ).fetchall()
    except sqlite3.OperationalError:
        return []
    return [
        {
            "id": r[0], "profile": r[1], "status": r[2], "created_at": r[3],
            "cost_usd": r[4], "prompt_tokens": r[5], "completion_tokens": r[6],
            "model": r[7],
        }
        for r in rows
    ]


def _fetch_spans(conn: sqlite3.Connection, run_id: str) -> list[dict]:
    rows = conn.execute(
        """SELECT id, name, started_at, finished_at, duration_ms, status,
               prompt_tokens, completion_tokens, model_id, attributes
           FROM spans WHERE trace_id=? ORDER BY started_at""",
        (run_id,),
    ).fetchall()
    return [
        {
            "id": r[0], "name": r[1], "started_at": r[2], "finished_at": r[3],
            "duration_ms": r[4], "status": r[5],
            "prompt_tokens": r[6], "completion_tokens": r[7], "model": r[8],
        }
        for r in rows
    ]


def _fetch_queue(conn: sqlite3.Connection, limit: int = 50) -> list[dict]:
    try:
        rows = conn.execute(
            """SELECT id, profile, task, status, created_at, started_at, finished_at,
                   exit_code
               FROM queue_jobs ORDER BY created_at DESC LIMIT ?""",
            (limit,),
        ).fetchall()
        return [
            {
                "id": r[0], "profile": r[1], "task": (r[2] or "")[:80],
                "status": r[3], "created_at": r[4],
                "started_at": r[5], "finished_at": r[6], "exit_code": r[7],
            }
            for r in rows
        ]
    except sqlite3.OperationalError:
        return []


def _fetch_cost_summary(conn: sqlite3.Connection) -> list[dict]:
    try:
        rows = conn.execute(
            """SELECT master_profile, COUNT(*) runs, SUM(estimated_cost_usd) total_cost,
                   SUM(prompt_tokens + completion_tokens) total_tokens, model_id
               FROM runs GROUP BY master_profile, model_id ORDER BY total_cost DESC"""
        ).fetchall()
    except sqlite3.OperationalError:
        return []
    return [
        {
            "profile": r[0], "runs": r[1],
            "total_cost_usd": round(r[2] or 0.0, 6),
            "total_tokens": r[3], "model": r[4],
        }
        for r in rows
    ]


# ---------------------------------------------------------------------------
# SSE helpers
# ---------------------------------------------------------------------------

def _sse_event(data: Any, event: str = "message") -> bytes:
    payload = json.dumps(data)
    return f"event: {event}\ndata: {payload}\n\n".encode("utf-8")


# ---------------------------------------------------------------------------
# Dashboard HTML (minimal, self-contained)
# ---------------------------------------------------------------------------

_DASHBOARD_HTML = """\
<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>TAG Web Dashboard</title>
<style>
body{font-family:monospace;background:#111;color:#eee;margin:0;padding:16px}
h1{color:#7ec8e3;margin-bottom:4px}
nav{margin:8px 0;display:flex;gap:12px}
nav a{color:#7ec8e3;text-decoration:none;cursor:pointer}
nav a:hover{text-decoration:underline}
.panel{display:none}
.panel.active{display:block}
table{border-collapse:collapse;width:100%;margin-top:8px}
th{background:#222;color:#7ec8e3;padding:6px 10px;text-align:left}
td{padding:4px 10px;border-bottom:1px solid #333;font-size:0.9em}
.ok{color:#5fbb5f}.fail{color:#e05252}.run{color:#e0c000}.pend{color:#999}
#status{float:right;color:#888;font-size:0.8em}
</style></head>
<body>
<h1>TAG Web Dashboard <span id=status></span></h1>
<nav>
  <a onclick="show('runs')">Runs</a>
  <a onclick="show('queue')">Queue</a>
  <a onclick="show('costs')">Costs</a>
</nav>
<div id=runs class="panel active">
  <h2>Recent Runs</h2>
  <table id=runs-table>
    <tr><th>ID</th><th>Profile</th><th>Model</th><th>Status</th><th>Tokens</th><th>Cost</th><th>When</th></tr>
  </table>
</div>
<div id=queue class="panel">
  <h2>Queue Jobs</h2>
  <table id=queue-table>
    <tr><th>ID</th><th>Profile</th><th>Task</th><th>Status</th><th>When</th></tr>
  </table>
</div>
<div id=costs class="panel">
  <h2>Cost Summary</h2>
  <table id=costs-table>
    <tr><th>Profile</th><th>Model</th><th>Runs</th><th>Tokens</th><th>Total Cost</th></tr>
  </table>
</div>
<script>
const es=new EventSource('/api/stream');
es.addEventListener('update',e=>{
  const d=JSON.parse(e.data);
  document.getElementById('status').textContent='Updated '+new Date().toLocaleTimeString();
  renderRuns(d.runs||[]);
  renderQueue(d.queue||[]);
  renderCosts(d.costs||[]);
});
function cls(s){return s==='completed'?'ok':s==='failed'?'fail':s==='running'?'run':'pend';}
function renderRuns(rs){
  const t=document.getElementById('runs-table');
  t.innerHTML='<tr><th>ID</th><th>Profile</th><th>Model</th><th>Status</th><th>Tokens</th><th>Cost</th><th>When</th></tr>';
  rs.slice(0,20).forEach(r=>{
    const when=(r.created_at||'').substring(11,16);
    const tok=((r.prompt_tokens||0)+(r.completion_tokens||0)).toLocaleString();
    const cost=r.cost_usd?'$'+r.cost_usd.toFixed(4):'—';
    t.innerHTML+=`<tr><td>${r.id.substring(0,12)}</td><td>${r.profile||''}</td><td>${r.model||''}</td><td class="${cls(r.status)}">${r.status}</td><td>${tok}</td><td>${cost}</td><td>${when}</td></tr>`;
  });
}
function renderQueue(qs){
  const t=document.getElementById('queue-table');
  t.innerHTML='<tr><th>ID</th><th>Profile</th><th>Task</th><th>Status</th><th>When</th></tr>';
  qs.slice(0,15).forEach(q=>{
    const when=(q.created_at||'').substring(11,16);
    t.innerHTML+=`<tr><td>${(q.id||'').substring(0,12)}</td><td>${q.profile||''}</td><td>${(q.task||'').substring(0,50)}</td><td class="${cls(q.status)}">${q.status}</td><td>${when}</td></tr>`;
  });
}
function renderCosts(cs){
  const t=document.getElementById('costs-table');
  t.innerHTML='<tr><th>Profile</th><th>Model</th><th>Runs</th><th>Tokens</th><th>Total Cost</th></tr>';
  cs.forEach(c=>{
    const tok=(c.total_tokens||0).toLocaleString();
    const cost='$'+(c.total_cost_usd||0).toFixed(4);
    t.innerHTML+=`<tr><td>${c.profile||''}</td><td>${c.model||''}</td><td>${c.runs}</td><td>${tok}</td><td>${cost}</td></tr>`;
  });
}
function show(panel){
  document.querySelectorAll('.panel').forEach(p=>p.classList.remove('active'));
  document.getElementById(panel).classList.add('active');
}
</script></body></html>
"""


# ---------------------------------------------------------------------------
# HTTP handler
# ---------------------------------------------------------------------------

class _Handler(BaseHTTPRequestHandler):
    db_path: Path  # set on class by DashboardServer
    log_message = lambda *a: None  # suppress access log

    def _get_conn(self) -> sqlite3.Connection:
        conn = sqlite3.connect(str(self.db_path), timeout=5)
        conn.row_factory = sqlite3.Row
        return conn

    def do_GET(self) -> None:
        path = self.path.split("?")[0]

        if path == "/":
            self._send_html(_DASHBOARD_HTML)
        elif path == "/api/runs":
            conn = self._get_conn()
            self._send_json(_fetch_runs(conn))
            conn.close()
        elif path.startswith("/api/spans/"):
            run_id = path.split("/api/spans/", 1)[-1]
            conn = self._get_conn()
            self._send_json(_fetch_spans(conn, run_id))
            conn.close()
        elif path == "/api/queue":
            conn = self._get_conn()
            self._send_json(_fetch_queue(conn))
            conn.close()
        elif path == "/api/costs":
            conn = self._get_conn()
            self._send_json(_fetch_cost_summary(conn))
            conn.close()
        elif path == "/api/stream":
            self._sse_loop()
        else:
            self.send_response(404)
            self.end_headers()

    def _send_html(self, html: str) -> None:
        body = html.encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _send_json(self, data: Any) -> None:
        body = json.dumps(data).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        # No wildcard CORS: these endpoints expose local run/cost/trace data.
        # A `*` ACAO lets any visited web page fetch() this localhost server and
        # read it cross-origin, defeating the 127.0.0.1 bind.
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _sse_loop(self) -> None:
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        # No wildcard CORS on the live data stream (see _send_json).
        self.end_headers()
        try:
            while True:
                conn = self._get_conn()
                payload = {
                    "runs": _fetch_runs(conn, limit=20),
                    "queue": _fetch_queue(conn, limit=15),
                    "costs": _fetch_cost_summary(conn),
                }
                conn.close()
                self.wfile.write(_sse_event(payload, "update"))
                self.wfile.flush()
                time.sleep(2)
        except (BrokenPipeError, ConnectionResetError):
            pass


class DashboardServer:
    """Wrapper around HTTPServer for the TAG web dashboard."""

    def __init__(self, db_path: Path, host: str = "127.0.0.1", port: int = 8787):
        self.db_path = db_path
        self.host = host
        self.port = port
        self._server: ThreadingHTTPServer | None = None

    def start(self, *, open_browser: bool = True) -> None:
        # Inject db_path into handler class
        handler = type("Handler", (_Handler,), {"db_path": self.db_path})
        # ThreadingHTTPServer (not the single-threaded HTTPServer): the /api/stream
        # SSE handler blocks its thread indefinitely, so on a plain HTTPServer the
        # first EventSource client would wedge every other request.
        self._server = ThreadingHTTPServer((self.host, self.port), handler)
        url = f"http://{self.host}:{self.port}"
        print(f"TAG Dashboard running at {url}")
        if open_browser:
            import webbrowser
            webbrowser.open(url)
        try:
            self._server.serve_forever()
        except KeyboardInterrupt:
            pass

    def stop(self) -> None:
        if self._server:
            self._server.shutdown()
            # Also release the listening socket/fd; shutdown() alone only stops
            # the serve loop, leaking the port on repeated start/stop.
            self._server.server_close()

