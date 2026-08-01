package hitl

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrInterrupt is the sentinel Interrupt returns when it has just recorded a NEW
// pause. The caller unwinds via ordinary Go error propagation (errors.Is) and
// the engine suspends the session — PRD-109 FR-01/FR-02.
//
// It is deliberately a plain sentinel, not a cancellation: `context` is already
// how this tree signals "stop now" (SIGTERM, abort), and overloading it would
// make an interrupt indistinguishable from a kill.
var ErrInterrupt = errors.New("workflow interrupted: awaiting human input")

// Gate carries per-session interrupt configuration. It is threaded explicitly
// (or via context with WithGate) — never a package-level global — so it is
// goroutine-safe and testable.
type Gate struct {
	// DB is the store the pause rows live in. Nil is fail-closed: Interrupt
	// errors rather than pretending the pause was recorded.
	DB *sql.DB
	// SessionID scopes interrupts to one run/workflow.
	SessionID string
	// AutoApprove short-circuits every interrupt with AutoInput, recording
	// nothing and prompting nobody (PRD-109 FR-06). Opt-in only; never a default.
	AutoApprove bool
	// AutoInput is the value returned under AutoApprove ("approved" when empty).
	AutoInput string
}

type gateKey struct{}

// WithGate attaches a Gate to ctx.
func WithGate(ctx context.Context, g *Gate) context.Context {
	return context.WithValue(ctx, gateKey{}, g)
}

// GateFrom retrieves the Gate attached to ctx.
func GateFrom(ctx context.Context) (*Gate, bool) {
	g, ok := ctx.Value(gateKey{}).(*Gate)
	return g, ok && g != nil
}

// InterruptRequest is what the operator is asked.
type InterruptRequest struct {
	// StepID identifies the decision point. It MUST be stable across a
	// re-execution of the same node, because that stability is what lets a
	// resumed run find its answer instead of parking again. Empty is derived
	// from the question text via StepIDFor.
	StepID   string         `json:"step_id"`
	Question string         `json:"question"`
	Context  map[string]any `json:"context,omitempty"`
	// TimeoutS is advisory metadata stored on the row for `workflow interrupt
	// show`; it does not itself bound anything. Bounding is Wait's job.
	TimeoutS int `json:"timeout_s,omitempty"`
}

// StepIDFor derives a deterministic step id from a node label and call ordinal,
// so the same interrupt in a re-executed node maps to the same row (FR-05).
func StepIDFor(node string, ordinal int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s#%d", node, ordinal)))
	return "step_" + hex.EncodeToString(sum[:])[:16]
}

// Interrupt is the PRD-109 primitive.
//
// Semantics, in order:
//
//	AutoApprove          -> return AutoInput, nil        (no row, no prompt)
//	already responded    -> return stored input, nil     (the resume path)
//	already denied/timed -> return "", an explaining error (never a silent pass)
//	still pending        -> return "", ErrInterrupt      (caller suspends)
//	new                  -> record pending, return ErrInterrupt
//
// It NEVER blocks. Blocking is a separate, explicitly bounded step (Wait), so
// that a caller which merely wants to checkpoint-and-exit — the mode PRD-109
// actually specifies, NFR-02 "no background goroutine/daemon" — cannot
// accidentally acquire a hang.
func Interrupt(ctx context.Context, g *Gate, req InterruptRequest) (string, error) {
	if g == nil {
		return "", errors.New("hitl: Interrupt called outside a workflow session (no gate)")
	}
	if g.AutoApprove {
		in := g.AutoInput
		if in == "" {
			in = "approved"
		}
		return in, nil
	}
	if g.DB == nil {
		// Fail closed. Returning "approved" here would be a fabricated human
		// decision, which is strictly worse than refusing to run.
		return "", errors.New("hitl: Interrupt has no store to record the pause in; refusing to continue unreviewed")
	}
	if strings.TrimSpace(g.SessionID) == "" {
		return "", errors.New("hitl: Interrupt requires a session id")
	}
	step := req.StepID
	if step == "" {
		step = StepIDFor(req.Question, 0)
	}

	existing, err := FindStep(g.DB, g.SessionID, step)
	switch {
	case err == nil:
		switch existing.Status {
		case StatusApproved:
			return existing.Response, nil
		case StatusPending:
			return "", ErrInterrupt
		default:
			return "", fmt.Errorf("workflow interrupt %s was %s (%s); not resuming",
				existing.ID, existing.Status, strOr(existing.Rationale, "no rationale"))
		}
	case errors.Is(err, ErrNotFound):
		// fall through and open one
	default:
		return "", err
	}

	ctxJSON := "{}"
	if len(req.Context) > 0 {
		if b, merr := json.Marshal(req.Context); merr == nil {
			ctxJSON = string(b)
		} else {
			return "", fmt.Errorf("hitl: interrupt context is not JSON-serialisable: %w", merr)
		}
	}
	if _, err := Open(g.DB, Pause{
		Kind:        KindWorkflowInterrupt,
		SessionID:   g.SessionID,
		StepID:      step,
		Question:    req.Question,
		ContextJSON: ctxJSON,
		TimeoutS:    req.TimeoutS,
	}); err != nil {
		return "", err
	}
	return "", ErrInterrupt
}

// Respond is `tag workflow resume`: it stores the operator's input on a pending
// interrupt so the next re-execution of that node short-circuits.
func Respond(db *sql.DB, id, input, rationale string) (Pause, error) {
	return Resolve(db, id, Decision{
		Status: StatusApproved, Response: input, Source: "cli", Rationale: rationale,
	})
}

// Sessions summarises interrupt state per session, newest activity first. Used
// by `tag workflow list`.
type Session struct {
	SessionID string `json:"session_id"`
	Pending   int    `json:"pending"`
	Total     int    `json:"total"`
	Status    string `json:"status"`
	Question  string `json:"question,omitempty"`
	LastAt    string `json:"last_at"`
}

// Sessions returns per-session interrupt rollups. `interrupted` means at least
// one pending interrupt. Never nil.
func Sessions(db *sql.DB, pendingOnly bool, limit int) ([]Session, error) {
	if err := EnsureSchema(db); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Query(`
		SELECT session_id,
		       SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END) AS pending,
		       COUNT(*) AS total,
		       MAX(created_at) AS last_at
		FROM hitl_pauses WHERE kind=?
		GROUP BY session_id ORDER BY last_at DESC LIMIT ?`, KindWorkflowInterrupt, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.SessionID, &s.Pending, &s.Total, &s.LastAt); err != nil {
			return nil, err
		}
		s.Status = "resolved"
		if s.Pending > 0 {
			s.Status = "interrupted"
		}
		if pendingOnly && s.Pending == 0 {
			continue
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Attach the oldest open question per interrupted session so `workflow list`
	// shows WHAT is being waited on rather than just a count.
	for i := range out {
		if out[i].Pending == 0 {
			continue
		}
		var q string
		if err := db.QueryRow(`SELECT question FROM hitl_pauses
			WHERE kind=? AND session_id=? AND status='pending'
			ORDER BY created_at LIMIT 1`, KindWorkflowInterrupt, out[i].SessionID).Scan(&q); err == nil {
			out[i].Question = q
		}
	}
	return out, nil
}

func strOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
