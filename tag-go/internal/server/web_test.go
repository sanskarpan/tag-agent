package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tag-agent/tag/internal/store"
)

func seedWeb(t *testing.T, db *store.DB) {
	t.Helper()
	db.Exec(`INSERT INTO runs(id,created_at,kind,task_type,execution,master_profile,board,prompt,route_json,status,model_id,prompt_tokens,completion_tokens,estimated_cost_usd) VALUES('r1','2026-07-01T00:00:00Z','agent','chat','native','coder','default','hi','{}','completed','m1',100,50,0.01)`)
	db.Exec(`INSERT INTO spans(id,trace_id,name,started_at,finished_at,duration_ms,status,prompt_tokens,completion_tokens,model_id) VALUES('sp1','r1','plan','2026-07-01T00:00:00Z','2026-07-01T00:00:01Z',1000,'ok',10,5,'m1')`)
	db.Exec(`INSERT INTO queue_jobs(id,profile,task,created_at) VALUES('q1','coder','build it','2026-07-01T00:00:00Z')`)
}

func TestWebDashboardHTML(t *testing.T) {
	db := testDB(t)
	srv := httptest.NewServer(WebHandler(db))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
}

func TestWebRuns(t *testing.T) {
	db := testDB(t)
	seedWeb(t, db)
	srv := httptest.NewServer(WebHandler(db))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rows []map[string]any
	json.NewDecoder(resp.Body).Decode(&rows)
	if len(rows) != 1 || rows[0]["id"] != "r1" || rows[0]["model"] != "m1" {
		t.Errorf("runs wrong: %+v", rows)
	}
}

func TestWebSpansWaterfall(t *testing.T) {
	db := testDB(t)
	seedWeb(t, db)
	srv := httptest.NewServer(WebHandler(db))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/spans/r1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rows []map[string]any
	json.NewDecoder(resp.Body).Decode(&rows)
	if len(rows) != 1 || rows[0]["name"] != "plan" {
		t.Errorf("spans wrong: %+v", rows)
	}
}

func TestWebQueueAndCosts(t *testing.T) {
	db := testDB(t)
	seedWeb(t, db)
	srv := httptest.NewServer(WebHandler(db))
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/api/queue")
	var q []map[string]any
	json.NewDecoder(resp.Body).Decode(&q)
	resp.Body.Close()
	if len(q) != 1 || q[0]["task"] != "build it" {
		t.Errorf("queue wrong: %+v", q)
	}

	resp2, _ := http.Get(srv.URL + "/api/costs")
	var c []map[string]any
	json.NewDecoder(resp2.Body).Decode(&c)
	resp2.Body.Close()
	if len(c) != 1 || c[0]["profile"] != "coder" {
		t.Errorf("costs wrong: %+v", c)
	}
}

func TestWebSnapshotAndHealth(t *testing.T) {
	db := testDB(t)
	seedWeb(t, db)
	srv := httptest.NewServer(WebHandler(db))
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/health")
	var h map[string]string
	json.NewDecoder(resp.Body).Decode(&h)
	resp.Body.Close()
	if h["status"] != "ok" {
		t.Errorf("health = %+v", h)
	}

	resp2, _ := http.Get(srv.URL + "/api/snapshot")
	var snap Snapshot
	json.NewDecoder(resp2.Body).Decode(&snap)
	resp2.Body.Close()
	if len(snap.Runs) != 1 {
		t.Errorf("snapshot runs = %+v", snap.Runs)
	}
}

func TestWebStreamOneShot(t *testing.T) {
	db := testDB(t)
	seedWeb(t, db)
	srv := httptest.NewServer(WebHandler(db))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/stream", nil)
	req.Header.Set("X-TAG-Once", "1") // return after the first frame instead of streaming forever
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("SSE content-type = %q", ct)
	}
	buf := make([]byte, 8192)
	n, _ := resp.Body.Read(buf)
	frame := string(buf[:n])
	if !strings.Contains(frame, "event: update") || !strings.Contains(frame, "r1") {
		t.Errorf("SSE frame wrong: %q", frame)
	}
}

func TestWebNotFound(t *testing.T) {
	db := testDB(t)
	srv := httptest.NewServer(WebHandler(db))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}
