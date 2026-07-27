package permission

import (
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// SQLRecorder appends decisions to the permission_decisions table. It reuses
// the existing single-writer store; there is no separate subsystem.
//
// Recording is strictly best-effort: a write failure is swallowed so an audit
// problem can never turn into a security decision (or a crash) at runtime.
type SQLRecorder struct {
	DB    *sql.DB
	RunID string
}

// NewSQLRecorder returns a recorder, or nil when db is nil (no audit).
func NewSQLRecorder(db *sql.DB, runID string) *SQLRecorder {
	if db == nil {
		return nil
	}
	return &SQLRecorder{DB: db, RunID: runID}
}

// Record implements Recorder.
func (r *SQLRecorder) Record(req Request, d Decision) {
	if r == nil || r.DB == nil {
		return
	}
	_, _ = r.DB.Exec(`INSERT INTO permission_decisions
		(created_at, tool, subject, args_summary, verdict, via, rule, reason, run_id)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		time.Now().UTC().Format(time.RFC3339), req.Tool, req.Subject, SummarizeArgs(req.Args),
		string(d.Action), d.Via, d.Rule.String(), d.Reason, r.RunID)
}

// SummarizeArgs renders tool input compactly for the audit row: long string
// values are truncated so a 1 MB write_file body does not land in the log.
func SummarizeArgs(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]any, len(args))
	for _, k := range keys {
		if s, ok := args[k].(string); ok {
			out[k] = truncate(s, 200)
			continue
		}
		out[k] = args[k]
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// MemoryRecorder collects decisions in memory (tests, and the per-run summary
// printed by `tag run`).
type MemoryRecorder struct {
	Entries []Entry
}

// Entry is one recorded decision.
type Entry struct {
	Request  Request
	Decision Decision
}

// Record implements Recorder.
func (m *MemoryRecorder) Record(req Request, d Decision) {
	m.Entries = append(m.Entries, Entry{Request: req, Decision: d})
}

// MultiRecorder fans a decision out to several recorders (nil entries skipped).
type MultiRecorder []Recorder

// Record implements Recorder.
func (m MultiRecorder) Record(req Request, d Decision) {
	for _, r := range m {
		if r == nil {
			continue
		}
		r.Record(req, d)
	}
}

// Summary renders a one-line-per-decision report for CLI output.
func Summary(entries []Entry) string {
	var b strings.Builder
	for _, e := range entries {
		b.WriteString("  permission ")
		b.WriteString(string(e.Decision.Action))
		b.WriteString(": ")
		b.WriteString(e.Request.Describe())
		b.WriteString(" — ")
		b.WriteString(e.Decision.Via)
		b.WriteString("\n")
	}
	return b.String()
}
