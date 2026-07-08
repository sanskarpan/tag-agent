// Package server exposes the TAG control plane over local HTTP (Track B —
// PRD-029 `serve`). It serves a small dashboard, a JSON snapshot API, and an SSE
// event stream of live state. Bound to loopback only; no wildcard CORS (the
// stream carries run/queue/journal data). Handlers are pure funcs of *store.DB
// so they are testable with net/http/httptest — no port binding required.
package server

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"time"

	"github.com/tag-agent/tag/internal/store"
)

// Snapshot is the dashboard state read from the control-plane DB.
type Snapshot struct {
	Runs         []map[string]any `json:"runs"`
	Queue        []map[string]any `json:"queue"`
	JournalCount int              `json:"journal_count"`
}

// ReadSnapshot gathers current TAG state (pure SQLite, no runtime).
func ReadSnapshot(db *store.DB) (*Snapshot, error) {
	snap := &Snapshot{Runs: []map[string]any{}, Queue: []map[string]any{}}
	rows, err := db.Query(`SELECT id, kind, task_type, master_profile, status, created_at FROM runs ORDER BY created_at DESC LIMIT 20`)
	if err == nil {
		for rows.Next() {
			var id, kind, tt, mp, status, created string
			if rows.Scan(&id, &kind, &tt, &mp, &status, &created) == nil {
				snap.Runs = append(snap.Runs, map[string]any{
					"run_id": id, "kind": kind, "task_type": tt,
					"master_profile": mp, "status": status, "created_at": created,
				})
			}
		}
		rows.Close()
	}
	qrows, err := db.Query(`SELECT id, task, status, profile, created_at FROM queue_jobs ORDER BY created_at DESC LIMIT 50`)
	if err == nil {
		for qrows.Next() {
			var id, task, status, profile, created string
			if qrows.Scan(&id, &task, &status, &profile, &created) == nil {
				snap.Queue = append(snap.Queue, map[string]any{
					"id": id, "task": task, "status": status, "profile": profile, "created_at": created,
				})
			}
		}
		qrows.Close()
	}
	db.QueryRow(`SELECT COUNT(*) FROM memory_journal`).Scan(&snap.JournalCount)
	return snap, nil
}

// Handler builds the HTTP mux for the dashboard/API/SSE endpoints.
func Handler(db *store.DB, profile string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, dashboardHTML, html.EscapeString(profile))
	})
	mux.HandleFunc("/api/snapshot", func(w http.ResponseWriter, r *http.Request) {
		snap, err := ReadSnapshot(db)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(snap)
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		snap, _ := ReadSnapshot(db)
		b, _ := json.Marshal(snap)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if ok {
			flusher.Flush()
		}
		// one-shot in tests; the CLI wraps this in a ticker loop (see Serve).
		if r.Header.Get("X-TAG-Once") == "1" {
			return
		}
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				snap, _ := ReadSnapshot(db)
				b, _ := json.Marshal(snap)
				if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
					return
				}
				if ok {
					flusher.Flush()
				}
			}
		}
	})
	return mux
}

// Serve starts the HTTP server on 127.0.0.1:port (blocking).
func Serve(db *store.DB, profile string, port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	srv := &http.Server{Addr: addr, Handler: Handler(db, profile)}
	fmt.Printf("TAG dashboard server: http://%s  (Ctrl+C to stop)\n", addr)
	return srv.ListenAndServe()
}

const dashboardHTML = `<!doctype html><html><head><meta charset="utf-8"><title>TAG</title></head>
<body><h1>TAG dashboard — profile %s</h1>
<pre id="s">loading…</pre>
<script>
const es = new EventSource('/events');
es.onmessage = e => { document.getElementById('s').textContent = JSON.stringify(JSON.parse(e.data), null, 2); };
</script></body></html>`
