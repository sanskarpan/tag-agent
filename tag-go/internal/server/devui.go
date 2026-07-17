// DevUI (PRD-054) — a richer local developer dashboard than `serve`. Ports
// devui.py: an HTML dashboard plus JSON endpoints for spans/traces, eval runs,
// judge runs, semantic memories, alert firings, and aggregate stats. Bound to
// loopback only; no wildcard CORS (these endpoints expose local spans, costs,
// and memories that a `*` ACAO would leak cross-origin). Handlers are pure funcs
// of *store.DB so they are testable with net/http/httptest.
package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/tag-agent/tag/internal/store"
)

// devQueryMaps runs sql and returns rows as a slice of column->value maps,
// mirroring Python's sqlite3.Row dict rows. Returns an empty (non-nil) slice on
// any error so a missing table (e.g. judge_runs) degrades to "no data" instead
// of a 500 — matching devui.py's defensive _query_db.
func devQueryMaps(db *store.DB, query string, args ...any) []map[string]any {
	out := []map[string]any{}
	rows, err := db.Query(query, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return out
	}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if rows.Scan(ptrs...) != nil {
			continue
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			v := vals[i]
			// Render []byte columns (SQLite TEXT) as strings, skip BLOBs sensibly.
			if b, ok := v.([]byte); ok {
				m[c] = string(b)
			} else {
				m[c] = v
			}
		}
		out = append(out, m)
	}
	return out
}

// devSafeLimit parses a `limit` query param defensively: non-integer or negative
// falls back to def; oversized clamps to a hard maximum. Never panics on hostile
// input. Mirrors devui.py's _safe_limit.
func devSafeLimit(raw string, def int) int {
	const maximum = 1000
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return def
	}
	if v > maximum {
		return maximum
	}
	return v
}

func devSendJSON(w http.ResponseWriter, data any, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// DevUIHandler builds the HTTP mux for the DevUI dashboard + JSON API.
func DevUIHandler(db *store.DB, profile string) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			devSendJSON(w, map[string]string{"error": "not found"}, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, devUIHTML)
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		devSendJSON(w, map[string]string{"status": "ok"}, http.StatusOK)
	})

	// /api/snapshot — reuse the shared control-plane snapshot (task requirement).
	mux.HandleFunc("/api/snapshot", func(w http.ResponseWriter, r *http.Request) {
		snap, err := ReadSnapshot(db)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		devSendJSON(w, snap, http.StatusOK)
	})

	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		var totalSpans int
		var totalCost sql.NullFloat64
		db.QueryRow(`SELECT COUNT(*), SUM(cost_usd) FROM spans`).Scan(&totalSpans, &totalCost)
		var totalRuns int
		db.QueryRow(`SELECT COUNT(*) FROM eval_runs`).Scan(&totalRuns)
		var totalMem int
		db.QueryRow(`SELECT COUNT(*) FROM semantic_memories`).Scan(&totalMem)
		devSendJSON(w, map[string]any{
			"total_spans":    totalSpans,
			"total_runs":     totalRuns,
			"total_cost_usd": roundTo(totalCost.Float64, 6),
			"total_memories": totalMem,
		}, http.StatusOK)
	})

	mux.HandleFunc("/api/spans", func(w http.ResponseWriter, r *http.Request) {
		limit := devSafeLimit(r.URL.Query().Get("limit"), 50)
		if trace := r.URL.Query().Get("trace_id"); trace != "" {
			devSendJSON(w, devQueryMaps(db,
				`SELECT * FROM spans WHERE trace_id = ? ORDER BY started_at DESC LIMIT ?`,
				trace, limit), http.StatusOK)
			return
		}
		devSendJSON(w, devQueryMaps(db,
			`SELECT * FROM spans ORDER BY started_at DESC LIMIT ?`, limit), http.StatusOK)
	})

	mux.HandleFunc("/api/eval_runs", func(w http.ResponseWriter, r *http.Request) {
		limit := devSafeLimit(r.URL.Query().Get("limit"), 20)
		devSendJSON(w, devQueryMaps(db,
			`SELECT * FROM eval_runs ORDER BY created_at DESC LIMIT ?`, limit), http.StatusOK)
	})

	mux.HandleFunc("/api/judge_runs", func(w http.ResponseWriter, r *http.Request) {
		limit := devSafeLimit(r.URL.Query().Get("limit"), 20)
		// judge_runs may not exist in every schema; devQueryMaps returns [] then.
		devSendJSON(w, devQueryMaps(db,
			`SELECT * FROM judge_runs ORDER BY created_at DESC LIMIT ?`, limit), http.StatusOK)
	})

	mux.HandleFunc("/api/memories", func(w http.ResponseWriter, r *http.Request) {
		limit := devSafeLimit(r.URL.Query().Get("limit"), 50)
		if profileQ := r.URL.Query().Get("profile"); profileQ != "" {
			devSendJSON(w, devQueryMaps(db,
				`SELECT * FROM semantic_memories WHERE profile = ? ORDER BY created_at DESC LIMIT ?`,
				profileQ, limit), http.StatusOK)
			return
		}
		devSendJSON(w, devQueryMaps(db,
			`SELECT * FROM semantic_memories ORDER BY created_at DESC LIMIT ?`, limit), http.StatusOK)
	})

	mux.HandleFunc("/api/alerts", func(w http.ResponseWriter, r *http.Request) {
		limit := devSafeLimit(r.URL.Query().Get("limit"), 20)
		devSendJSON(w, devQueryMaps(db,
			`SELECT * FROM alert_firings ORDER BY fired_at DESC LIMIT ?`, limit), http.StatusOK)
	})

	return mux
}

// ServeDevUI starts the DevUI HTTP server on 127.0.0.1:port (blocking).
func ServeDevUI(db *store.DB, profile string, port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	srv := &http.Server{Addr: addr, Handler: DevUIHandler(db, profile)}
	fmt.Printf("TAG DevUI running at http://%s  (Ctrl+C to stop)\n", addr)
	return srv.ListenAndServe()
}

func roundTo(v float64, places int) float64 {
	p := 1.0
	for i := 0; i < places; i++ {
		p *= 10
	}
	// simple half-up rounding sufficient for cost display
	if v >= 0 {
		return float64(int64(v*p+0.5)) / p
	}
	return float64(int64(v*p-0.5)) / p
}

const devUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1.0"/>
<title>TAG DevUI</title>
<style>
  :root { --bg:#0f1117; --sidebar-bg:#161b22; --card-bg:#1c2128; --border:#30363d;
    --text:#e6edf3; --muted:#7d8590; --accent:#58a6ff; --green:#3fb950; --red:#f85149; --yellow:#d29922; }
  * { box-sizing:border-box; margin:0; padding:0; }
  body { background:var(--bg); color:var(--text); font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',monospace; display:flex; flex-direction:column; height:100vh; }
  header { background:var(--sidebar-bg); border-bottom:1px solid var(--border); padding:12px 20px; display:flex; align-items:center; justify-content:space-between; flex-shrink:0; }
  header h1 { font-size:18px; color:var(--accent); letter-spacing:0.03em; }
  #refresh-status { font-size:11px; color:var(--muted); }
  .layout { display:flex; flex:1; overflow:hidden; }
  nav { width:160px; background:var(--sidebar-bg); border-right:1px solid var(--border); padding:16px 0; flex-shrink:0; }
  nav a { display:block; padding:10px 20px; color:var(--muted); text-decoration:none; font-size:14px; cursor:pointer; border-left:3px solid transparent; }
  nav a:hover { color:var(--text); }
  nav a.active { color:var(--accent); border-left-color:var(--accent); background:rgba(88,166,255,0.06); }
  #stats-bar { display:flex; gap:16px; padding:10px 20px; background:var(--card-bg); border-bottom:1px solid var(--border); font-size:13px; flex-shrink:0; }
  .stat { display:flex; flex-direction:column; align-items:center; gap:2px; }
  .stat .label { font-size:10px; color:var(--muted); text-transform:uppercase; letter-spacing:0.06em; }
  .stat .value { font-weight:600; color:var(--accent); }
  main { flex:1; overflow-y:auto; padding:20px; }
  .panel { display:none; } .panel.active { display:block; }
  h2 { font-size:15px; margin-bottom:14px; color:var(--text); }
  table { width:100%; border-collapse:collapse; font-size:13px; }
  th { text-align:left; padding:8px 10px; background:var(--card-bg); color:var(--muted); font-weight:500; border-bottom:1px solid var(--border); position:sticky; top:0; }
  td { padding:7px 10px; border-bottom:1px solid var(--border); vertical-align:top; word-break:break-all; max-width:300px; }
  .badge { display:inline-block; padding:2px 7px; border-radius:10px; font-size:11px; font-weight:600; text-transform:uppercase; }
  .badge-ok,.badge-pass,.badge-running { background:rgba(63,185,80,0.15); color:var(--green); }
  .badge-error,.badge-fail { background:rgba(248,81,73,0.15); color:var(--red); }
  .badge-timeout,.badge-warn { background:rgba(210,153,34,0.15); color:var(--yellow); }
  .empty { color:var(--muted); font-size:13px; padding:30px 0; text-align:center; }
  .cost { color:var(--yellow); font-family:monospace; }
  .trace-id { font-family:monospace; font-size:11px; color:var(--muted); }
</style>
</head>
<body>
<header>
  <h1>TAG DevUI</h1>
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
      <div id="panel-traces" class="panel active"><h2>Recent Spans</h2><div id="traces-content"><span class="empty">Loading…</span></div></div>
      <div id="panel-evals" class="panel"><h2>Eval Runs</h2><div id="evals-content"><span class="empty">Loading…</span></div>
        <h2 style="margin-top:24px;">Judge Runs</h2><div id="judge-content"><span class="empty">Loading…</span></div></div>
      <div id="panel-memories" class="panel"><h2>Memories</h2><div id="memories-content"><span class="empty">Loading…</span></div></div>
      <div id="panel-alerts" class="panel"><h2>Alert Firings</h2><div id="alerts-content"><span class="empty">Loading…</span></div></div>
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
  try { const r = await fetch(url); if (!r.ok) return []; return await r.json(); } catch (e) { return []; }
}
function badge(val) { if (!val) return ''; return '<span class="badge badge-' + String(val).toLowerCase() + '">' + esc(val) + '</span>'; }
function esc(s) { if (s === null || s === undefined) return ''; return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;'); }
function renderTable(rows, cols) {
  if (!rows || rows.length === 0) return '<div class="empty">No data yet.</div>';
  let html = '<table><thead><tr>' + cols.map(c => '<th>' + esc(c.label) + '</th>').join('') + '</tr></thead><tbody>';
  for (const row of rows) {
    html += '<tr>';
    for (const c of cols) {
      let val = row[c.key];
      if (c.render) html += '<td>' + c.render(val, row) + '</td>';
      else html += '<td>' + esc(val === null || val === undefined ? '' : val) + '</td>';
    }
    html += '</tr>';
  }
  return html + '</tbody></table>';
}
async function loadStats() {
  const s = await apiFetch('/api/stats');
  if (s && typeof s === 'object') {
    document.getElementById('s-spans').textContent = s.total_spans ?? '—';
    document.getElementById('s-runs').textContent = s.total_runs ?? '—';
    document.getElementById('s-cost').textContent = s.total_cost_usd != null ? '$' + Number(s.total_cost_usd).toFixed(4) : '—';
    document.getElementById('s-mem').textContent = s.total_memories ?? '—';
  }
}
async function loadTraces() {
  const rows = await apiFetch('/api/spans?limit=100');
  const cols = [
    { key:'started_at', label:'Time', render:v => esc((v||'').replace('T',' ').substring(0,19)) },
    { key:'name', label:'Name' }, { key:'profile', label:'Profile' }, { key:'model_id', label:'Model' },
    { key:'status', label:'Status', render:v => badge(v) },
    { key:'duration_ms', label:'ms', render:v => v != null ? Number(v).toFixed(0) : '' },
    { key:'prompt_tokens', label:'Prompt T' }, { key:'completion_tokens', label:'Comp T' },
    { key:'cost_usd', label:'Cost', render:v => v != null ? '<span class="cost">$' + Number(v).toFixed(5) + '</span>' : '' },
    { key:'trace_id', label:'Trace ID', render:v => '<span class="trace-id">' + esc((v||'').substring(0,12)) + '</span>' },
  ];
  document.getElementById('traces-content').innerHTML = renderTable(rows, cols);
}
async function loadEvals() {
  const rows = await apiFetch('/api/eval_runs?limit=50');
  const cols = [
    { key:'created_at', label:'Time', render:v => esc((v||'').replace('T',' ').substring(0,19)) },
    { key:'suite_name', label:'Suite' }, { key:'profile', label:'Profile' },
    { key:'status', label:'Status', render:v => badge(v) },
    { key:'pass_count', label:'Pass' }, { key:'fail_count', label:'Fail' }, { key:'total_count', label:'Total' },
  ];
  document.getElementById('evals-content').innerHTML = renderTable(rows, cols);
  const jrows = await apiFetch('/api/judge_runs?limit=50');
  const jcols = [
    { key:'created_at', label:'Time', render:v => esc((v||'').replace('T',' ').substring(0,19)) },
    { key:'judge_model', label:'Judge Model' }, { key:'status', label:'Status', render:v => badge(v) },
    { key:'pass_count', label:'Pass' }, { key:'fail_count', label:'Fail' }, { key:'total_count', label:'Total' },
  ];
  document.getElementById('judge-content').innerHTML = (!jrows || jrows.length === 0)
    ? '<div class="empty">No judge runs yet.</div>' : renderTable(jrows, jcols);
}
async function loadMemories() {
  const rows = await apiFetch('/api/memories?limit=100');
  const cols = [
    { key:'created_at', label:'Time', render:v => esc((v||'').replace('T',' ').substring(0,19)) },
    { key:'profile', label:'Profile' }, { key:'memory_type', label:'Type' },
    { key:'content', label:'Content' },
    { key:'confidence', label:'Confidence', render:v => v != null ? Number(v).toFixed(2) : '' },
  ];
  document.getElementById('memories-content').innerHTML = renderTable(rows, cols);
}
async function loadAlerts() {
  const rows = await apiFetch('/api/alerts?limit=50');
  const cols = [
    { key:'fired_at', label:'Time', render:v => esc((v||'').replace('T',' ').substring(0,19)) },
    { key:'rule_name', label:'Alert' }, { key:'severity', label:'Severity', render:v => badge(v) },
    { key:'message', label:'Message' }, { key:'resolved_at', label:'Resolved' },
  ];
  document.getElementById('alerts-content').innerHTML = (!rows || rows.length === 0)
    ? '<div class="empty">No alert firings yet.</div>' : renderTable(rows, cols);
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
refresh();
setInterval(refresh, 10000);
</script>
</body>
</html>`
