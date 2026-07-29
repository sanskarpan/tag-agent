// Package trace is the Go span producer for TAG's agent runs (PRD-013, PRD-032,
// PRD-046, PRD-048).
//
// Everything downstream of spans already existed in Go — the `spans` table, the
// whole `trace` command tree, `otel-export`, the per-span cost column — but
// nothing ever wrote a row, so `tag trace list` always said "No spans recorded"
// (#590). This package is that missing producer.
//
// The span shape deliberately mirrors src/tag/tracing.py so the two engines
// produce comparable data and `otel-export` output stays consistent:
//   - ids are the first 12 hex chars of a UUID4 (OTLP-safe: hex, so otelHexID
//     zero-pads rather than SHA-folding them);
//   - timestamps are ISO-8601 UTC strings;
//   - status is "ok" | "error" | "timeout";
//   - kind is one of Kind* below (Python's SPAN_KINDS).
//
// A Recorder is safe for concurrent use: the worker drains jobs that may run in
// parallel, and each job's tool calls close spans from the same loop goroutine.
package trace

import (
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tag-agent/tag/internal/pricing"
)

// Span kinds (mirrors tracing.SPAN_KINDS).
const (
	KindAgent = "agent"
	KindLLM   = "llm"
	KindTool  = "tool"
)

// Span statuses.
const (
	StatusOK    = "ok"
	StatusError = "error"
)

// timeFormat is the ISO-8601 UTC layout used for started_at/finished_at. It is
// RFC3339 with nanoseconds so `otel-export`'s otelISOToNanos parses it exactly,
// and so sub-millisecond spans still order correctly in `trace show`.
const timeFormat = time.RFC3339Nano

// Span is one unit of traced work, mirroring tracing.Span field-for-field.
type Span struct {
	ID               string
	TraceID          string
	ParentID         string
	Name             string
	Profile          string
	ModelID          string
	StartedAt        time.Time
	FinishedAt       time.Time
	Status           string
	PromptTokens     int
	CompletionTokens int
	Attributes       map[string]any
	ErrorMsg         string
	Kind             string
	// CostUSD is nil when no rate is known for ModelID. A missing rate must stay
	// NULL rather than becoming $0, which would understate a real cost.
	CostUSD *float64

	closed bool
}

// DurationMS is the wall-clock duration, or nil while the span is still open.
func (s *Span) DurationMS() *int64 {
	if s.FinishedAt.IsZero() {
		return nil
	}
	ms := s.FinishedAt.Sub(s.StartedAt).Milliseconds()
	if ms < 0 {
		ms = 0
	}
	return &ms
}

// NewID returns a span id in Python's format: uuid4().hex[:12] — the dashes are
// stripped BEFORE truncating, so the id is 12 hex chars like tracing.Span.id
// (uuid.NewString()[:12] would keep a dash and yield only 11 hex digits).
func NewID() string { return strings.ReplaceAll(uuid.NewString(), "-", "")[:12] }

// Recorder collects the spans of a single trace. The caller owns persistence:
// build one per logical run, hand it to the agent loop, then Save it.
type Recorder struct {
	mu      sync.Mutex
	traceID string
	profile string
	spans   []*Span
}

// NewRecorder starts a recorder for a trace. traceID is the run/job id, so
// `tag trace show <run-id>` resolves a run's spans directly.
func NewRecorder(traceID, profile string) *Recorder {
	return &Recorder{traceID: traceID, profile: profile}
}

// TraceID reports the trace this recorder writes under.
func (r *Recorder) TraceID() string {
	if r == nil {
		return ""
	}
	return r.traceID
}

// Start opens a span and registers it with the recorder. A nil Recorder returns
// a nil span, so every call site can stay untouched when tracing is disabled.
func (r *Recorder) Start(name, kind, parentID, modelID string) *Span {
	if r == nil {
		return nil
	}
	s := &Span{
		ID:         NewID(),
		TraceID:    r.traceID,
		ParentID:   parentID,
		Name:       name,
		Profile:    r.profile,
		ModelID:    modelID,
		StartedAt:  time.Now().UTC(),
		Status:     StatusOK,
		Attributes: map[string]any{},
		Kind:       kind,
	}
	r.mu.Lock()
	r.spans = append(r.spans, s)
	r.mu.Unlock()
	return s
}

// Attr sets an attribute on an open span (no-op on a nil span).
func (r *Recorder) Attr(s *Span, key string, val any) {
	if r == nil || s == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s.Attributes[key] = val
}

// End closes a span with its outcome and token usage. It is idempotent (like
// tracing.close_span) so an error path that closes early cannot be overwritten
// by a deferred close. Cost is resolved from the pricing table for token-bearing
// spans; an unknown model leaves CostUSD nil.
func (r *Recorder) End(s *Span, status, errMsg string, promptTokens, completionTokens int) {
	if r == nil || s == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.FinishedAt = time.Now().UTC()
	if status != "" {
		s.Status = status
	}
	s.ErrorMsg = errMsg
	s.PromptTokens = promptTokens
	s.CompletionTokens = completionTokens
	if promptTokens > 0 || completionTokens > 0 {
		if c, ok := pricing.CostUSD(s.ModelID, promptTokens, completionTokens); ok {
			s.CostUSD = &c
		}
	}
}

// Spans returns a copy of the recorded spans in creation order.
func (r *Recorder) Spans() []Span {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Span, 0, len(r.spans))
	for _, s := range r.spans {
		out = append(out, *s)
	}
	return out
}

const insertSpan = `INSERT OR REPLACE INTO spans
  (id, trace_id, parent_id, name, profile, model_id,
   started_at, finished_at, duration_ms, status,
   prompt_tokens, completion_tokens, attributes, error_msg,
   kind, cost_usd)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

// Save persists every recorded span. Any span still open is closed as an error
// first, so a crashed/cancelled run leaves a truthful trace rather than a row
// with a NULL finished_at that `trace show` renders as still-running.
//
// Save is best-effort by contract at the call sites: a run must not fail because
// its telemetry could not be written.
func (r *Recorder) Save(db *sql.DB) error {
	if r == nil || db == nil {
		return nil
	}
	r.mu.Lock()
	pending := make([]*Span, len(r.spans))
	copy(pending, r.spans)
	r.mu.Unlock()
	if len(pending) == 0 {
		return nil
	}
	for _, s := range pending {
		r.End(s, StatusError, "span not closed (run ended early)", s.PromptTokens, s.CompletionTokens)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(insertSpan)
	if err != nil {
		return err
	}
	defer stmt.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range pending {
		attrs, merr := json.Marshal(s.Attributes)
		if merr != nil {
			attrs = []byte("{}")
		}
		var finished any
		if !s.FinishedAt.IsZero() {
			finished = s.FinishedAt.Format(timeFormat)
		}
		var dur any
		if d := s.DurationMS(); d != nil {
			dur = *d
		}
		var errMsg any
		if s.ErrorMsg != "" {
			errMsg = s.ErrorMsg
		}
		var cost any
		if s.CostUSD != nil {
			cost = *s.CostUSD
		}
		var parent any
		if s.ParentID != "" {
			parent = s.ParentID
		}
		if _, err := stmt.Exec(s.ID, s.TraceID, parent, s.Name, nullIfEmpty(s.Profile), nullIfEmpty(s.ModelID),
			s.StartedAt.Format(timeFormat), finished, dur, s.Status,
			s.PromptTokens, s.CompletionTokens, string(attrs), errMsg, s.Kind, cost); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
