// Package tui is the interactive terminal dashboard (Track B) built on Charm
// bubbletea + lipgloss. It renders the same control-plane snapshot the HTTP
// `serve` dashboard shows (runs/queue/journal), refreshable live. The Model's
// Update/View are pure and unit-tested offline; only Run() needs a real TTY.
package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/tag-agent/tag/internal/server"
	"github.com/tag-agent/tag/internal/store"
)

var titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))

// refreshMsg triggers a snapshot reload.
type refreshMsg struct{}

// Model is the dashboard bubbletea model.
type Model struct {
	db                    *store.DB
	profile               string
	snap                  *server.Snapshot
	err                   error
	lastLoad              time.Time
	quitting              bool
	width, height, offset int
}

// New builds a dashboard model for a profile.
func New(db *store.DB, profile string) Model {
	m := Model{db: db, profile: profile, width: 80, height: 24}
	m.reload()
	return m
}

func (m *Model) reload() {
	snap, err := server.ReadProfileSnapshot(m.db, m.profile)
	m.snap, m.err, m.lastLoad = snap, err, time.Now()
	m.clamp()
}

// Init loads the first snapshot and starts the refresh ticker.
func (m Model) Init() tea.Cmd { return tick() }

func tick() tea.Cmd {
	return tea.Tick(3*time.Second, func(time.Time) tea.Msg { return refreshMsg{} })
}

// Update handles key presses and refresh ticks.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.clamp()
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			m.quitting = true
			return m, tea.Quit
		case "r":
			m.reload()
			return m, nil
		case "down", "j":
			m.offset++
		case "up", "k":
			m.offset--
		case "pgdown", "ctrl+f", " ":
			m.offset += m.pageSize()
		case "pgup", "ctrl+b":
			m.offset -= m.pageSize()
		case "home", "g":
			m.offset = 0
		case "end", "G":
			m.offset = len(m.lines())
		case "tab":
			lines := m.lines()
			for i := m.offset + 1; i < len(lines); i++ {
				if strings.HasPrefix(lines[i], "Queue (") || strings.HasPrefix(lines[i], "Journal entries:") {
					m.offset = i
					break
				}
			}
		}
	case refreshMsg:
		m.reload()
		return m, tick()
	}
	m.clamp()
	return m, nil
}

func (m Model) pageSize() int { return max(1, m.height-4) }
func (m *Model) clamp()       { m.offset = max(0, min(m.offset, len(m.lines())-m.pageSize())) }

// Sanitize untrusted stored text before rendering it in a terminal. Escape/control
// bytes must never become terminal instructions or extra physical screen lines.
func display(v any) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, fmt.Sprint(v))
}

func (m Model) lines() []string {
	var b strings.Builder
	if m.err != nil {
		b.WriteString("error: " + display(m.err))
	} else {
		snap := m.snap
		if snap == nil {
			snap = &server.Snapshot{}
		}
		fmt.Fprintf(&b, "Runs (%d)\n", len(snap.Runs))
		for _, r := range snap.Runs {
			fmt.Fprintf(&b, "  %s  %s  %s\n", display(r["run_id"]), display(r["master_profile"]), display(r["status"]))
		}
		fmt.Fprintf(&b, "\nQueue (%d)\n", len(snap.Queue))
		for _, q := range snap.Queue {
			fmt.Fprintf(&b, "  %s  %s  %s  %s\n", display(q["id"]), display(q["status"]), display(q["profile"]), display(q["task"]))
		}
		fmt.Fprintf(&b, "\nJournal entries: %d", snap.JournalCount)
	}
	return strings.Split(ansi.Hardwrap(b.String(), max(1, m.width), true), "\n")
}

// View renders the dashboard.
func (m Model) View() string {
	if m.quitting {
		return "Goodbye.\n"
	}
	if m.width < 20 || m.height < 6 {
		return ansi.Truncate("Resize to 20x6; q quits", max(0, m.width), "…")
	}
	lines := m.lines()
	end := min(len(lines), m.offset+m.pageSize())
	body := append([]string{}, lines[m.offset:end]...)
	for len(body) < m.pageSize() {
		body = append(body, "")
	}
	clip := func(s string) string { return ansi.Truncate(s, m.width, "…") }
	return strings.Join([]string{
		clip(titleStyle.Render("TAG — native control plane")),
		clip("profile: " + display(m.profile)),
		strings.Join(body, "\n"),
		clip(fmt.Sprintf("q quit · r refresh · %d-%d/%d", m.offset+1, end, len(lines))),
		clip("↑↓/jk · Tab · PgUp/Dn · g/G Home/End"),
	}, "\n")
}

// Run launches the interactive TUI (needs a TTY).
func Run(db *store.DB, profile string) error {
	_, err := tea.NewProgram(New(db, profile), tea.WithAltScreen()).Run()
	return err
}
