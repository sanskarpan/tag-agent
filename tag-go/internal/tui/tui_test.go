package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tag-agent/tag/internal/store"
)

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
	if !strings.Contains(view, "Queue (1)") || !strings.Contains(view, "build it") {
		t.Errorf("view missing queue: %q", view)
	}
	if !strings.Contains(view, "Journal entries: 1") {
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
	m := New(db, "p")
	// add another run, then send a refresh tick
	db.Exec(`INSERT INTO runs(id,created_at,kind,task_type,execution,master_profile,board,prompt,route_json,status) VALUES('r2','2026-07-02T00:00:00Z','agent','chat','native','coder','default','x','{}','running')`)
	updated, cmd := m.Update(refreshMsg{})
	if cmd == nil {
		t.Error("refresh should re-arm the ticker")
	}
	if !strings.Contains(updated.View(), "Runs (2)") {
		t.Errorf("refresh should pick up the new run: %q", updated.View())
	}
}
