package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tag-agent/tag/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.OpenPath(t.TempDir() + "/s.sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestHealthEndpoint(t *testing.T) {
	db := testDB(t)
	srv := httptest.NewServer(Handler(db, "orchestrator"))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("/health status %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("/health body = %v, want status=ok", body)
	}
}

func TestSnapshotAPI(t *testing.T) {
	db := testDB(t)
	db.Exec(`INSERT INTO runs(id,created_at,kind,task_type,execution,master_profile,board,prompt,route_json,status) VALUES('r1','2026-07-01T00:00:00Z','agent','chat','native','orchestrator','default','hi','{}','completed')`)
	db.Exec(`INSERT INTO queue_jobs(id,profile,task,created_at) VALUES('q1','coder','build it','2026-07-01T00:00:00Z')`)
	db.Exec(`INSERT INTO memory_journal(id,profile,key,value,created_at) VALUES('j1','coder','k','note','2026-07-01T00:00:00Z')`)

	srv := httptest.NewServer(Handler(db, "orchestrator"))
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
	if len(snap.Queue) != 1 || snap.Queue[0]["task"] != "build it" {
		t.Errorf("queue wrong: %+v", snap.Queue)
	}
	if snap.JournalCount != 1 {
		t.Errorf("journal_count = %d", snap.JournalCount)
	}
}

func TestDashboardHTML(t *testing.T) {
	db := testDB(t)
	srv := httptest.NewServer(Handler(db, "orchestrator"))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
}

func TestDashboardHTMLEscapesProfile(t *testing.T) {
	db := testDB(t)
	srv := httptest.NewServer(Handler(db, `<script>alert(1)</script>`))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 8192)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("profile must be HTML-escaped in the dashboard")
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("escaped profile missing from body: %q", body)
	}
}

func TestSSEOneShot(t *testing.T) {
	db := testDB(t)
	db.Exec(`INSERT INTO runs(id,created_at,kind,task_type,execution,master_profile,board,prompt,route_json,status) VALUES('r1','2026-07-01T00:00:00Z','agent','chat','native','p','default','hi','{}','completed')`)
	srv := httptest.NewServer(Handler(db, "p"))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/events", nil)
	req.Header.Set("X-TAG-Once", "1") // return after the first frame instead of streaming forever
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("SSE content-type = %q", ct)
	}
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if !strings.HasPrefix(string(buf[:n]), "data: ") || !strings.Contains(string(buf[:n]), "r1") {
		t.Errorf("SSE frame wrong: %q", string(buf[:n]))
	}
}

func TestNotFound(t *testing.T) {
	db := testDB(t)
	srv := httptest.NewServer(Handler(db, "p"))
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

// TestSnapshotAPIMissingTablesReturns500 covers finding #11: ReadSnapshot used
// to swallow every query/scan error, so a DB with dropped tables was reported
// as a healthy-but-empty one with HTTP 200.
func TestSnapshotAPIMissingTablesReturns500(t *testing.T) {
	db := testDB(t)
	for _, tbl := range []string{"runs", "queue_jobs", "memory_journal"} {
		if _, err := db.Exec("DROP TABLE " + tbl); err != nil {
			t.Fatalf("drop %s: %v", tbl, err)
		}
	}
	if _, err := ReadSnapshot(db); err == nil {
		t.Error("ReadSnapshot returned nil error for a DB with missing tables")
	}

	srv := httptest.NewServer(Handler(db, "orchestrator"))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500 for a broken DB", resp.StatusCode)
	}
}

// TestSnapshotAPIHealthyEmptyDBReturns200 pins the other side of finding #11:
// an empty-but-valid DB must still be 200 with empty arrays.
func TestSnapshotAPIHealthyEmptyDBReturns200(t *testing.T) {
	db := testDB(t)
	srv := httptest.NewServer(Handler(db, "orchestrator"))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 for a healthy empty DB", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"runs":[]`) || !strings.Contains(string(body), `"queue":[]`) {
		t.Errorf("healthy empty snapshot body = %s", body)
	}
}
