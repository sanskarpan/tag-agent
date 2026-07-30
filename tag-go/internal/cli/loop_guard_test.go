package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/tag-agent/tag/internal/config"
	"github.com/tag-agent/tag/internal/permission"
)

// TestLoopGuardIsAlwaysNonInteractive is the "must never block on an
// interactive approval prompt" bar for the TOOL permission gate, mirroring the
// rule buildWorkerOptions applies to background queue drains.
//
// It asserts the property directly rather than trying to detect a hang: the
// guard the loop hands to tool.Register must carry NO prompter, even when the
// operator did not pass --no-prompt and even if this process happened to own a
// terminal. With no prompter, `ask` resolves to an immediate deny.
func TestLoopGuardIsAlwaysNonInteractive(t *testing.T) {
	app := &App{Cfg: &config.Config{Data: map[string]any{}}}
	rf := loopRunFlags{provName: "echo", tools: true}
	rf.perms.ask = []string{"bash"} // an 'ask' rule with prompting NOT disabled by flag

	opts, err := rf.runnerOptions(app, "default", nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Guard == nil {
		t.Fatal("--tools produced a nil Guard")
	}
	if opts.Guard.Interactive() {
		t.Fatal("loop guard carries a TTY prompter: a background loop could block on consent")
	}
	if !strings.Contains(opts.Guard.NonInteractiveHint, "prompting is disabled") {
		t.Errorf("deny reason would be misleading: %q", opts.Guard.NonInteractiveHint)
	}
	d := opts.Guard.Check(context.Background(), permission.Request{
		Tool: "bash", Kind: permission.KindCommand, Subject: "ls"})
	if d.Allowed() {
		t.Fatal("an 'ask' rule resolved to ALLOW with no human present")
	}
}

// TestLoopWithoutToolsHasNoGuard: the gate is only built when tools are on, and
// a nil Guard is fail-closed downstream (tool.Register substitutes the secure
// default policy) rather than ungated.
func TestLoopWithoutToolsHasNoGuard(t *testing.T) {
	app := &App{Cfg: &config.Config{Data: map[string]any{}}}
	rf := loopRunFlags{provName: "echo"}
	opts, err := rf.runnerOptions(app, "default", nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Guard != nil {
		t.Fatal("no --tools should mean no guard is constructed")
	}
	if opts.WithTools {
		t.Fatal("WithTools must be off")
	}
}

// TestLoopRejectsUnknownProvider keeps the offline default honest and makes a
// typo a usage error (exit 2) instead of a silent fallback.
func TestLoopRejectsUnknownProvider(t *testing.T) {
	rf := loopRunFlags{provName: "definitely-not-a-provider"}
	_, err := rf.provider()
	if err == nil {
		t.Fatal("unknown provider accepted")
	}
	if !isUsageError(err) {
		t.Errorf("unknown provider should be a usage error, got %v", err)
	}
}
