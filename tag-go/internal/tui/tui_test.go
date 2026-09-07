package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/tag-agent/tag/internal/store"
)

func TestCompleteProfileNavigationAndResize(t *testing.T) {
	db := seededDB(t)
	for i := 1; i <= 60; i++ {
		_, err := db.Exec(`INSERT INTO queue_jobs(id,profile,task,created_at) VALUES(?,?,?,?)`, fmt.Sprintf("job-%03d", i), "coder", fmt.Sprintf("task-%03d 界 long content", i), fmt.Sprintf("2026-09-08T00:00:%02dZ", i))
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(`INSERT INTO runs(id,created_at,kind,task_type,execution,master_profile,board,prompt,route_json,status) VALUES(?,?,'agent','chat','native','coder','default','hi','{}','completed')`, fmt.Sprintf("run-%03d", i), fmt.Sprintf("2026-09-08T00:00:%02dZ", i))
		if err != nil {
			t.Fatal(err)
		}
	}
	m := New(db, "coder")
	if len(m.snap.Runs) != 60 || len(m.snap.Queue) != 61 || m.snap.JournalCount != 1 {
		t.Fatalf("incomplete snapshot: %+v", m.snap)
	}
	for _, size := range [][2]int{{80, 24}, {30, 12}, {20, 6}, {100, 40}, {15, 3}} {
		u, _ := m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m = u.(Model)
		view := m.View()
		if len(strings.Split(view, "\n")) > size[1] {
			t.Fatalf("too tall %v: %q", size, view)
		}
		for _, line := range strings.Split(view, "\n") {
			if ansi.StringWidth(line) > size[0] {
				t.Fatalf("too wide %v: %q", size, line)
			}
		}
	}
	u, _ := m.Update(tea.WindowSizeMsg{Width: 30, Height: 12})
	m = u.(Model)
	seen := ""
	for i := 0; i < len(m.lines()); i++ {
		seen += strings.ReplaceAll(m.View(), "\n", "")
		u, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = u.(Model)
	}
	for i := 1; i <= 60; i++ {
		for _, prefix := range []string{"run-", "job-", "task-"} {
			if !strings.Contains(seen, fmt.Sprintf("%s%03d", prefix, i)) {
				t.Fatalf("unreachable %s%d", prefix, i)
			}
		}
	}
	u, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome})
	m = u.(Model)
	if m.offset != 0 {
		t.Fatal("home failed")
	}
	u, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = u.(Model)
	if !strings.Contains(m.View(), "Queue (61)") {
		t.Fatal("tab did not reach queue")
	}
	u, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	m = u.(Model)
	if !strings.Contains(m.View(), "Journal entries: 1") {
		t.Fatal("end failed")
	}
	if _, err := db.Exec(`DELETE FROM queue_jobs WHERE profile='coder'`); err != nil {
		t.Fatal(err)
	}
	u, _ = m.Update(refreshMsg{})
	m = u.(Model)
	if !strings.Contains(m.View(), "Queue (0)") {
		t.Fatal("refresh did not clamp offset")
	}
}

func TestStoredControlCharactersAreNotTerminalInstructions(t *testing.T) {
	got := display("abc\x1b[2J\r\n\tdef\x07")
	if strings.ContainsAny(got, "\x1b\r\n\t\x07") {
		t.Fatalf("unsafe terminal text: %q", got)
	}
}

func seededDB(t *testing.T) *store.DB {
	db, err := store.OpenPath(t.TempDir() + "/t.sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.Exec(`INSERT INTO runs(id,created_at,kind,task_type,execution,master_profile,board,prompt,route_json,status) VALUES('r1','2026-07-01T00:00:00Z','agent','chat','native','orchestrator','default','hi','{}','completed')`)
	db.Exec(`INSERT INTO queue_jobs(id,profile,task,created_at) VALUES('q1','coder','build it','2026-07-01T00:00:00Z')`)
	db.Exec(`INSERT INTO memory_journal(id,profile,key,value,created_at) VALUES('j1','coder','k','v','2026-07-01T00:00:00Z')`)
	return db
}

func TestViewRendersSnapshot(t *testing.T) {
	m := New(seededDB(t), "orchestrator")
	view := m.View()
	if !strings.Contains(view, "TAG") || !strings.Contains(view, "Runs (1)") || !strings.Contains(view, "r1") {
		t.Errorf("view missing runs: %q", view)
	}
	if !strings.Contains(view, "Queue (0)") || strings.Contains(view, "build it") {
		t.Errorf("view missing queue: %q", view)
	}
	if !strings.Contains(view, "Journal entries: 0") {
		t.Errorf("view missing journal count: %q", view)
	}
}

func TestQuitKey(t *testing.T) {
	m := New(seededDB(t), "p")
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Error("q should return a quit command")
	}
	if !strings.Contains(updated.View(), "Goodbye") {
		t.Errorf("after quit the view should say Goodbye: %q", updated.View())
	}
}

func TestRefreshMsgReloads(t *testing.T) {
	db := seededDB(t)
	m := New(db, "coder")
	// add another run, then send a refresh tick
	db.Exec(`INSERT INTO runs(id,created_at,kind,task_type,execution,master_profile,board,prompt,route_json,status) VALUES('r2','2026-07-02T00:00:00Z','agent','chat','native','coder','default','x','{}','running')`)
	updated, cmd := m.Update(refreshMsg{})
	if cmd == nil {
		t.Error("refresh should re-arm the ticker")
	}
	if !strings.Contains(updated.View(), "Runs (1)") {
		t.Errorf("refresh should pick up the new run: %q", updated.View())
	}
}
