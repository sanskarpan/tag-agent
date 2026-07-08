// Web dashboard (PRD-036) — ports api.py's DashboardServer: an HTML dashboard
// plus JSON endpoints for runs, per-run span waterfalls, queue jobs, and cost
// summaries, with an SSE live feed. Bound to loopback only; no wildcard CORS on
// the live data stream (it carries run/queue/cost data). Handlers are pure funcs
// of *store.DB so they are testable with net/http/httptest.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tag-agent/tag/internal/store"
)

// webFetchRuns returns recent runs for the dashboard/stream.
func webFetchRuns(db *store.DB) []map[string]any {
	out := []map[string]any{}
	rows, err := db.Query(`SELECT id, master_profile, status, created_at, estimated_cost_usd,
		prompt_tokens, completion_tokens, model_id FROM runs ORDER BY created_at DESC LIMIT 50`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, profile, status, created string
		var model *string
		var cost float64
		var pt, ct int
		if rows.Scan(&id, &profile, &status, &created, &cost, &pt, &ct, &model) != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "profile": profile, "status": status, "created_at": created,
			"cost_usd": cost, "prompt_tokens": pt, "completion_tokens": ct,
			"model": derefStr(model),
		})
	}
	return out
}

// webFetchSpans returns the span waterfall for a single run (trace).
func webFetchSpans(db *store.DB, runID string) []map[string]any {
	out := []map[string]any{}
	rows, err := db.Query(`SELECT id, name, started_at, finished_at, duration_ms, status,
		prompt_tokens, completion_tokens, model_id FROM spans WHERE trace_id=? ORDER BY started_at`, runID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, name, started, status string
		var finished, model *string
		var dur *int
		var pt, ct int
		if rows.Scan(&id, &name, &started, &finished, &dur, &status, &pt, &ct, &model) != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "name": name, "started_at": started, "finished_at": derefStr(finished),
			"duration_ms": derefInt(dur), "status": status,
			"prompt_tokens": pt, "completion_tokens": ct, "model": derefStr(model),
		})
	}
	return out
}

// webFetchQueue returns recent queue jobs.
func webFetchQueue(db *store.DB) []map[string]any {
	out := []map[string]any{}
	rows, err := db.Query(`SELECT id, profile, task, status, created_at, started_at, finished_at, exit_code
		FROM queue_jobs ORDER BY created_at DESC LIMIT 50`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, profile, task, status, created string
		var started, finished *string
		var exit *int
		if rows.Scan(&id, &profile, &task, &status, &created, &started, &finished, &exit) != nil {
			continue
		}
		if len(task) > 80 {
			task = task[:80]
		}
		out = append(out, map[string]any{
			"id": id, "profile": profile, "task": task, "status": status,
			"created_at": created, "started_at": derefStr(started),
			"finished_at": derefStr(finished), "exit_code": derefInt(exit),
		})
	}
	return out
}

// webFetchCosts returns a per-profile/model cost summary.
func webFetchCosts(db *store.DB) []map[string]any {
	out := []map[string]any{}
	rows, err := db.Query(`SELECT master_profile, COUNT(*), SUM(estimated_cost_usd),
		SUM(prompt_tokens + completion_tokens), model_id
		FROM runs GROUP BY master_profile, model_id ORDER BY SUM(estimated_cost_usd) DESC`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var profile string
		var model *string
		var runs, tokens int
		var cost float64
		if rows.Scan(&profile, &runs, &cost, &tokens, &model) != nil {
			continue
		}
		out = append(out, map[string]any{
			"profile": profile, "runs": runs, "total_cost_usd": roundTo(cost, 6),
			"total_tokens": tokens, "model": derefStr(model),
		})
	}
	return out
}

func webSendJSON(w http.ResponseWriter, data any, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// WebHandler builds the HTTP mux for the web dashboard + JSON API + SSE stream.
func WebHandler(db *store.DB) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, webDashboardHTML)
			return
		}
		// Per-run span waterfall: /api/spans/<run_id>
		if strings.HasPrefix(r.URL.Path, "/api/spans/") {
			runID := strings.TrimPrefix(r.URL.Path, "/api/spans/")
			webSendJSON(w, webFetchSpans(db, runID), http.StatusOK)
			return
		}
		http.NotFound(w, r)
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		webSendJSON(w, map[string]string{"status": "ok"}, http.StatusOK)
	})

	// /api/snapshot — shared control-plane snapshot (task requirement).
	mux.HandleFunc("/api/snapshot", func(w http.ResponseWriter, r *http.Request) {
		snap, err := ReadSnapshot(db)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		webSendJSON(w, snap, http.StatusOK)
	})

	mux.HandleFunc("/api/runs", func(w http.ResponseWriter, r *http.Request) {
		webSendJSON(w, webFetchRuns(db), http.StatusOK)
	})
	mux.HandleFunc("/api/queue", func(w http.ResponseWriter, r *http.Request) {
		webSendJSON(w, webFetchQueue(db), http.StatusOK)
	})
	mux.HandleFunc("/api/costs", func(w http.ResponseWriter, r *http.Request) {
		webSendJSON(w, webFetchCosts(db), http.StatusOK)
	})

	mux.HandleFunc("/api/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		writeFrame := func() bool {
			payload := map[string]any{
				"runs":  webFetchRuns(db),
				"queue": webFetchQueue(db),
				"costs": webFetchCosts(db),
			}
			b, _ := json.Marshal(payload)
			if _, err := fmt.Fprintf(w, "event: update\ndata: %s\n\n", b); err != nil {
				return false
			}
			if ok {
				flusher.Flush()
			}
			return true
		}
		writeFrame()
		// one-shot in tests; the CLI streams on a ticker otherwise.
		if r.Header.Get("X-TAG-Once") == "1" {
			return
		}
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				if !writeFrame() {
					return
				}
			}
		}
	})

	return mux
}

// ServeWeb starts the web dashboard on 127.0.0.1:port (blocking).
func ServeWeb(db *store.DB, port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	srv := &http.Server{Addr: addr, Handler: WebHandler(db)}
	fmt.Printf("TAG web dashboard: http://%s  (Ctrl+C to stop)\n", addr)
	return srv.ListenAndServe()
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}

const webDashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>TAG Web Dashboard</title>
<style>
body{font-family:monospace;background:#111;color:#eee;margin:0;padding:16px}
h1{color:#7ec8e3;margin-bottom:4px}
nav{margin:8px 0;display:flex;gap:12px}
nav a{color:#7ec8e3;text-decoration:none;cursor:pointer}
nav a:hover{text-decoration:underline}
.panel{display:none} .panel.active{display:block}
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
<div id=runs class="panel active"><h2>Recent Runs</h2>
  <table id=runs-table><tr><th>ID</th><th>Profile</th><th>Model</th><th>Status</th><th>Tokens</th><th>Cost</th><th>When</th></tr></table></div>
<div id=queue class="panel"><h2>Queue Jobs</h2>
  <table id=queue-table><tr><th>ID</th><th>Profile</th><th>Task</th><th>Status</th><th>When</th></tr></table></div>
<div id=costs class="panel"><h2>Cost Summary</h2>
  <table id=costs-table><tr><th>Profile</th><th>Model</th><th>Runs</th><th>Tokens</th><th>Total Cost</th></tr></table></div>
<script>
const es=new EventSource('/api/stream');
es.addEventListener('update',e=>{
  const d=JSON.parse(e.data);
  document.getElementById('status').textContent='Updated '+new Date().toLocaleTimeString();
  renderRuns(d.runs||[]); renderQueue(d.queue||[]); renderCosts(d.costs||[]);
});
function cls(s){return s==='completed'?'ok':s==='failed'?'fail':s==='running'?'run':'pend';}
function esc(s){if(s===null||s===undefined)return '';return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');}
function renderRuns(rs){
  const t=document.getElementById('runs-table');
  t.innerHTML='<tr><th>ID</th><th>Profile</th><th>Model</th><th>Status</th><th>Tokens</th><th>Cost</th><th>When</th></tr>';
  rs.slice(0,20).forEach(r=>{
    const when=(r.created_at||'').substring(11,16);
    const tok=((r.prompt_tokens||0)+(r.completion_tokens||0)).toLocaleString();
    const cost=r.cost_usd?'$'+r.cost_usd.toFixed(4):'—';
    t.innerHTML+='<tr><td>'+esc((r.id||'').substring(0,12))+'</td><td>'+esc(r.profile||'')+'</td><td>'+esc(r.model||'')+'</td><td class="'+cls(r.status)+'">'+esc(r.status)+'</td><td>'+esc(tok)+'</td><td>'+esc(cost)+'</td><td>'+esc(when)+'</td></tr>';
  });
}
function renderQueue(qs){
  const t=document.getElementById('queue-table');
  t.innerHTML='<tr><th>ID</th><th>Profile</th><th>Task</th><th>Status</th><th>When</th></tr>';
  qs.slice(0,15).forEach(q=>{
    const when=(q.created_at||'').substring(11,16);
    t.innerHTML+='<tr><td>'+esc((q.id||'').substring(0,12))+'</td><td>'+esc(q.profile||'')+'</td><td>'+esc((q.task||'').substring(0,50))+'</td><td class="'+cls(q.status)+'">'+esc(q.status)+'</td><td>'+esc(when)+'</td></tr>';
  });
}
function renderCosts(cs){
  const t=document.getElementById('costs-table');
  t.innerHTML='<tr><th>Profile</th><th>Model</th><th>Runs</th><th>Tokens</th><th>Total Cost</th></tr>';
  cs.forEach(c=>{
    const tok=(c.total_tokens||0).toLocaleString();
    const cost='$'+(c.total_cost_usd||0).toFixed(4);
    t.innerHTML+='<tr><td>'+esc(c.profile||'')+'</td><td>'+esc(c.model||'')+'</td><td>'+esc(c.runs)+'</td><td>'+esc(tok)+'</td><td>'+esc(cost)+'</td></tr>';
  });
}
function show(panel){
  document.querySelectorAll('.panel').forEach(p=>p.classList.remove('active'));
  document.getElementById(panel).classList.add('active');
}
</script></body></html>`
