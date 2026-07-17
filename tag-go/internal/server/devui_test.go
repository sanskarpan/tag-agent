package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tag-agent/tag/internal/store"
)

func seedDevUI(t *testing.T, db *store.DB) {
	t.Helper()
	db.Exec(`INSERT INTO spans(id,trace_id,name,profile,model_id,started_at,status,prompt_tokens,completion_tokens,cost_usd) VALUES('sp1','tr1','plan','orchestrator','m1','2026-07-01T00:00:00Z','ok',10,20,0.0025)`)
	db.Exec(`INSERT INTO eval_runs(id,suite_path,profile,suite_name,status,pass_count,fail_count,total_count,created_at) VALUES('ev1','s.yaml','coder','smoke','completed',3,1,4,'2026-07-01T00:00:00Z')`)
	db.Exec(`INSERT INTO semantic_memories(id,profile,content,memory_type,confidence,created_at,accessed_at) VALUES('mem1','coder','the sky is blue','fact',0.9,'2026-07-01T00:00:00Z','2026-07-01T00:00:00Z')`)
	db.Exec(`INSERT INTO alert_rules(id,name,metric,condition,threshold,severity,created_at) VALUES('ar1','cost','cost_usd','gt',1.0,'warn','2026-07-01T00:00:00Z')`)
	db.Exec(`INSERT INTO alert_firings(id,rule_id,rule_name,metric,actual_value,threshold,severity,fired_at,message) VALUES('af1','ar1','cost','cost_usd',2.0,1.0,'warn','2026-07-01T00:00:00Z','over budget')`)
}

func TestDevUIDashboardHTML(t *testing.T) {
	db := testDB(t)
	srv := httptest.NewServer(DevUIHandler(db, "orchestrator"))
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

func TestDevUISnapshot(t *testing.T) {
	db := testDB(t)
	db.Exec(`INSERT INTO runs(id,created_at,kind,task_type,execution,master_profile,board,prompt,route_json,status) VALUES('r1','2026-07-01T00:00:00Z','agent','chat','native','orchestrator','default','hi','{}','completed')`)
	srv := httptest.NewServer(DevUIHandler(db, "orchestrator"))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var snap Snapshot
	json.NewDecoder(resp.Body).Decode(&snap)
	if len(snap.Runs) != 1 || snap.Runs[0]["run_id"] != "r1" {
		t.Errorf("runs wrong: %+v", snap.Runs)
	}
}

func TestDevUIStats(t *testing.T) {
	db := testDB(t)
	seedDevUI(t, db)
	srv := httptest.NewServer(DevUIHandler(db, "p"))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var s map[string]any
	json.NewDecoder(resp.Body).Decode(&s)
	if s["total_spans"].(float64) != 1 {
		t.Errorf("total_spans = %v", s["total_spans"])
	}
	if s["total_runs"].(float64) != 1 {
		t.Errorf("total_runs = %v", s["total_runs"])
	}
	if s["total_memories"].(float64) != 1 {
		t.Errorf("total_memories = %v", s["total_memories"])
	}
}

func TestDevUISpans(t *testing.T) {
	db := testDB(t)
	seedDevUI(t, db)
	srv := httptest.NewServer(DevUIHandler(db, "p"))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/spans?limit=10")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var rows []map[string]any
	json.NewDecoder(resp.Body).Decode(&rows)
	if len(rows) != 1 || rows[0]["trace_id"] != "tr1" {
		t.Errorf("spans wrong: %+v", rows)
	}
}

func TestDevUIEvalAlertsMemories(t *testing.T) {
	db := testDB(t)
	seedDevUI(t, db)
	srv := httptest.NewServer(DevUIHandler(db, "p"))
	defer srv.Close()
	for _, tc := range []struct {
		path, key, want string
	}{
		{"/api/eval_runs", "suite_name", "smoke"},
		{"/api/memories", "content", "the sky is blue"},
		{"/api/alerts", "message", "over budget"},
	} {
		resp, err := http.Get(srv.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		var rows []map[string]any
		json.NewDecoder(resp.Body).Decode(&rows)
		resp.Body.Close()
		if len(rows) != 1 || rows[0][tc.key] != tc.want {
			t.Errorf("%s: got %+v", tc.path, rows)
		}
	}
}

func TestDevUIJudgeRunsMissingTable(t *testing.T) {
	// judge_runs table does not exist in the schema; endpoint must degrade to [].
	db := testDB(t)
	srv := httptest.NewServer(DevUIHandler(db, "p"))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/judge_runs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var rows []map[string]any
	json.NewDecoder(resp.Body).Decode(&rows)
	if len(rows) != 0 {
		t.Errorf("expected empty judge_runs, got %+v", rows)
	}
}

func TestDevUINotFound(t *testing.T) {
	db := testDB(t)
	srv := httptest.NewServer(DevUIHandler(db, "p"))
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

func TestDevUISafeLimit(t *testing.T) {
	cases := []struct {
		raw string
		def int
		out int
	}{
		{"abc", 50, 50}, {"-1", 20, 20}, {"5", 50, 5}, {"99999", 50, 1000}, {"", 30, 30},
	}
	for _, c := range cases {
		if got := devSafeLimit(c.raw, c.def); got != c.out {
			t.Errorf("devSafeLimit(%q,%d)=%d want %d", c.raw, c.def, got, c.out)
		}
	}
}
