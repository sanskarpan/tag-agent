package permission

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tag-agent/tag/internal/redact"
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
	// Redact before the row is written, not when it is displayed.
	//
	// Both subject and args_summary were stored raw, for ALLOWED and denied
	// calls alike, so a `curl -H "Authorization: Bearer sk-..."` landed in the
	// database twice and `tag permissions log` printed it. This table was the
	// only one in the store that leaked. The DB is also world-readable, which
	// made a display-time fix insufficient.
	_, _ = r.DB.Exec(`INSERT INTO permission_decisions
		(created_at, tool, subject, args_summary, verdict, via, rule, reason, run_id)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		time.Now().UTC().Format(time.RFC3339), req.Tool, redact.Secrets(req.Subject),
		redact.Secrets(SummarizeArgs(req.Args)),
		string(d.Action), d.Via, d.Rule.String(), redact.Secrets(d.Reason), r.RunID)
}

// maxSummaryBytes caps a rendered audit summary. Tool arguments are
// model-controlled, so the row size is chosen by the peer unless we choose it.
const maxSummaryBytes = 4096

// SummarizeArgs renders tool input compactly for the audit row: long string
// values are truncated so a 1 MB write_file body does not land in the log.
//
// The truncation walks NESTED values. Only top-level strings used to be
// truncated, and everything else was passed through to json.Marshal verbatim —
// so `{"data": {"blob": "<1 MB>"}}` or `{"items": ["<1 MB>"]}` landed in the
// audit table in full, from an argument map the model controls. The doc comment
// above described the guarantee the function was reaching for rather than the
// one it achieved.
//
// A whole-summary ceiling backstops the per-value one: enough small values still
// add up, and the row has to be bounded, not merely mostly-bounded.
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
		out[k] = truncateValue(args[k], 0)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "{}"
	}
	if len(b) > maxSummaryBytes {
		// Truncating the JSON text would produce a string that is no longer JSON,
		// and an audit row that cannot be parsed is worse than a short one.
		return fmt.Sprintf(`{"_truncated":"audit summary exceeded %d bytes (%d keys)"}`,
			maxSummaryBytes, len(args))
	}
	return string(b)
}

// maxSummaryDepth bounds recursion. Deeply nested input is a peer-supplied
// shape too, and an unbounded walk is its own denial of service.
const maxSummaryDepth = 6

// maxSummaryElems bounds how many entries of a nested collection are rendered.
const maxSummaryElems = 32

// truncateValue applies the per-value truncation at any depth.
func truncateValue(v any, depth int) any {
	if depth >= maxSummaryDepth {
		// Brackets rather than angle brackets: json.Marshal HTML-escapes < and >,
		// and "\u003cnested\u003e" in an audit row a human reads is noise.
		return "[nested]"
	}
	switch t := v.(type) {
	case string:
		return truncate(t, 200)
	case []any:
		n := len(t)
		if n > maxSummaryElems {
			n = maxSummaryElems
		}
		out := make([]any, 0, n)
		for _, e := range t[:n] {
			out = append(out, truncateValue(e, depth+1))
		}
		if len(t) > n {
			out = append(out, fmt.Sprintf("[%d more]", len(t)-n))
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > maxSummaryElems {
			keys = keys[:maxSummaryElems]
		}
		out := make(map[string]any, len(keys))
		for _, k := range keys {
			out[k] = truncateValue(t[k], depth+1)
		}
		if len(t) > len(keys) {
			out["_more"] = len(t) - len(keys)
		}
		return out
	default:
		return v
	}
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
