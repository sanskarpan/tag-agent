// Package tui is the interactive terminal dashboard (Track B) built on Charm
// bubbletea + lipgloss. It renders the same control-plane snapshot the HTTP
// `serve` dashboard shows (runs/queue/journal), refreshable live. The Model's
// Update/View are pure and unit-tested offline; only Run() needs a real TTY.
package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/tag-agent/tag/internal/server"
	"github.com/tag-agent/tag/internal/store"
)

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
)

// refreshMsg triggers a snapshot reload.
type refreshMsg struct{}

// Model is the dashboard bubbletea model.
type Model struct {
	db       *store.DB
	profile  string
	snap     *server.Snapshot
	err      error
	lastLoad time.Time
	quitting bool
}

// New builds a dashboard model for a profile.
func New(db *store.DB, profile string) Model {
	m := Model{db: db, profile: profile}
	m.reload()
	return m
}

func (m *Model) reload() {
	snap, err := server.ReadSnapshot(m.db)
	m.snap, m.err, m.lastLoad = snap, err, time.Now()
}

// Init loads the first snapshot and starts the refresh ticker.
func (m Model) Init() tea.Cmd { return tick() }

func tick() tea.Cmd {
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg { return refreshMsg{} })
}

// Update handles key presses and refresh ticks.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			m.quitting = true
			return m, tea.Quit
		case "r":
			m.reload()
			return m, nil
		}
	case refreshMsg:
		m.reload()
		return m, tick()
	}
	return m, nil
}

// View renders the dashboard.
func (m Model) View() string {
	if m.quitting {
		return "Goodbye.\n"
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render("TAG — native control plane") + "\n")
	b.WriteString(dimStyle.Render(fmt.Sprintf("profile: %s   updated: %s", m.profile, m.lastLoad.Format("15:04:05"))) + "\n\n")
	if m.err != nil {
		b.WriteString("error: " + m.err.Error() + "\n")
		return b.String()
	}
	snap := m.snap
	if snap == nil {
		snap = &server.Snapshot{}
	}
	b.WriteString(headerStyle.Render(fmt.Sprintf("Runs (%d)", len(snap.Runs))) + "\n")
	for i, r := range snap.Runs {
		if i >= 8 {
			b.WriteString(dimStyle.Render(fmt.Sprintf("  … %d more", len(snap.Runs)-8)) + "\n")
			break
		}
		status := fmt.Sprint(r["status"])
		line := fmt.Sprintf("  %-12v %-8v %v", r["run_id"], r["master_profile"], status)
		if status == "completed" {
			line = okStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n" + headerStyle.Render(fmt.Sprintf("Queue (%d)", len(snap.Queue))) + "\n")
	for i, q := range snap.Queue {
		if i >= 5 {
			break
		}
		b.WriteString(fmt.Sprintf("  %-8v %-8v %v\n", q["status"], q["profile"], q["task"]))
	}
	b.WriteString("\n" + headerStyle.Render(fmt.Sprintf("Journal entries: %d", snap.JournalCount)) + "\n")
	b.WriteString("\n" + dimStyle.Render("[r] refresh   [q] quit") + "\n")
	return b.String()
}

// Run launches the interactive TUI (needs a TTY).
func Run(db *store.DB, profile string) error {
	_, err := tea.NewProgram(New(db, profile), tea.WithAltScreen()).Run()
	return err
}
