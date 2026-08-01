package hitl

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/tag-agent/tag/internal/permission"
)

// ToolPauser is the PRD-078 pause/resume adjudicator: it turns a permission
// `ask` into a durable approval request that a human answers from another shell
// with `tag permissions approve|deny <id>`, then resumes the tool call.
//
// It implements permission.Pauser. Everything about it is built so it cannot
// become a hang:
//
//   - the wait is bounded by the timeout the Guard passes; expiry is an
//     AUTO-DENY recorded in the audit trail, never a default-approve;
//   - it announces the parked call on Notify (stderr) with the exact command to
//     unblock it, so a waiting run is visible rather than silent;
//   - if it cannot even publish the request, it denies and says why.
type ToolPauser struct {
	// DB is the store the request is published to. Nil is fail-closed.
	DB *sql.DB
	// SessionID ties requests to a run (`tag permissions pending --session`).
	SessionID string
	// Poll is the store poll cadence (DefaultPoll when zero).
	Poll time.Duration
	// Notify receives the "parked, waiting for approval" banner. Nil is allowed
	// but strongly discouraged: an unannounced wait is the thing we are avoiding.
	Notify io.Writer
}

// Pause implements permission.Pauser.
func (t *ToolPauser) Pause(ctx context.Context, req permission.Request, timeout time.Duration) (permission.Response, error) {
	if t == nil || t.DB == nil {
		return permission.ResponseDeny,
			errors.New("the approval gate has no store to publish the request to")
	}
	if timeout <= 0 {
		// Belt to the Guard's braces. Never wait unbounded.
		return permission.ResponseDeny, ErrUnboundedWait
	}
	argsJSON, sum := CanonicalArgs(req.Args)
	p, err := Open(t.DB, Pause{
		Kind:       KindToolApproval,
		SessionID:  t.SessionID,
		Tool:       req.Tool,
		Subject:    req.Subject,
		Question:   "Approve tool call: " + req.Describe(),
		ArgsJSON:   argsJSON,
		ArgsSHA256: sum,
		TimeoutS:   int(timeout / time.Second),
	})
	if err != nil {
		return permission.ResponseDeny, fmt.Errorf("could not publish the approval request: %w", err)
	}
	if t.Notify != nil {
		fmt.Fprintf(t.Notify,
			"\n  APPROVAL REQUIRED  %s\n    %s\n    args sha256: %s\n"+
				"    approve: tag permissions approve %s\n    deny:    tag permissions deny %s\n"+
				"    waiting up to %s; no decision = DENY\n",
			p.ID, req.Describe(), sum, p.ID, p.ID, timeout)
	}

	res, err := Wait(ctx, t.DB, p.ID, timeout, t.Poll)
	if err != nil {
		return permission.ResponseDeny, err
	}
	switch res.Status {
	case StatusApproved:
		if t.Notify != nil {
			fmt.Fprintf(t.Notify, "  approved by %s after %s (%s)\n",
				strOr(res.Pause.Reviewer, "unknown"), res.Elapsed.Round(time.Millisecond), p.ID)
		}
		return permission.ResponseAllowOnce, nil
	case StatusTimedOut:
		if t.Notify != nil {
			fmt.Fprintf(t.Notify, "  DENIED: no approval within %s (%s)\n", timeout, p.ID)
		}
		return permission.ResponseDeny, nil
	default:
		if t.Notify != nil {
			fmt.Fprintf(t.Notify, "  DENIED (%s) by %s: %s (%s)\n", res.Status,
				strOr(res.Pause.Reviewer, "unknown"), strOr(res.Pause.Rationale, "no rationale"), p.ID)
		}
		return permission.ResponseDeny, nil
	}
}

// compile-time assertion: a drift in permission.Pauser must break the build here
// rather than silently leave the gate uninstallable.
var _ permission.Pauser = (*ToolPauser)(nil)
