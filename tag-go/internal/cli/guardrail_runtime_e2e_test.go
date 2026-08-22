package cli_test

import (
	"strings"
	"testing"
)

// TestGuardrailRuntimeAliasParity: `tag guardrail runtime <sub>` is a thin alias
// over `tag tripwire <sub>` and must produce identical output and exit codes.
// RED against pre-alias code, where `tag guardrail` is an unknown command
// (exit 2, cobra usage error).
func TestGuardrailRuntimeAliasParity(t *testing.T) {
	h := newHome(t)

	cases := [][]string{
		{"list"},
		{"test", "--tool", "bash", "--args", `{"command":"rm -rf /"}`},
		{"check", "--stage", "model_output", "--text", "hello"},
		{"history"},
	}
	for _, sub := range cases {
		twOut, twCode := run(t, h, append([]string{"tripwire"}, sub...)...)
		grOut, grCode := run(t, h, append([]string{"guardrail", "runtime"}, sub...)...)
		if grCode != twCode {
			t.Errorf("%v: exit code differs — tripwire=%d guardrail runtime=%d", sub, twCode, grCode)
		}
		if grOut != twOut {
			t.Errorf("%v: output differs\n tripwire:\n%s\n guardrail runtime:\n%s", sub, twOut, grOut)
		}
	}
}

// TestGuardrailRuntimeResolves guards the specific RED symptom: the alias path
// must resolve rather than fall through to cobra's unknown-command error.
func TestGuardrailRuntimeResolves(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "guardrail", "runtime", "list"); code != 0 {
		t.Fatalf("guardrail runtime list should resolve and exit 0, got %d: %s", code, out)
	}
}

// TestGuardrailRuntimeAddRemove exercises the config-editing verbs (#720):
// a valid rule persists and is visible next run; an invalid rule is refused at
// the CLI boundary and never written; removal works and reports honestly.
// RED against pre-#720 code, where `add`/`remove` are unknown subcommands.
func TestGuardrailRuntimeAddRemove(t *testing.T) {
	h := newHome(t)

	out, code := run(t, h, "guardrail", "runtime", "add",
		"--name", "approve-http", "--tool", "http_*", "--type", "require-approval")
	if code != 0 {
		t.Fatalf("add valid rule: exit %d: %s", code, out)
	}
	// NFR-03 honesty: the command must say the change is effective next run, not
	// that it altered a live ruleset.
	if !strings.Contains(out, "NEXT run") {
		t.Errorf("add must state the change is effective on the NEXT run, got: %q", out)
	}
	if lst, _ := run(t, h, "guardrail", "runtime", "list"); !strings.Contains(lst, "approve-http") {
		t.Errorf("added rule should be visible on the next invocation, got: %q", lst)
	}

	// Invalid regex must be refused AND not written.
	if _, code := run(t, h, "guardrail", "runtime", "add", "--name", "bad", "--pattern", "("); code != 2 {
		t.Errorf("bad regex should exit 2, got %d", code)
	}
	if lst, _ := run(t, h, "guardrail", "runtime", "list"); strings.Contains(lst, "bad") {
		t.Error("a rejected rule must not be written to the config")
	}

	// Duplicate name refused.
	if _, code := run(t, h, "guardrail", "runtime", "add", "--name", "approve-http", "--tool", "x", "--type", "require-approval"); code != 2 {
		t.Errorf("duplicate name should exit 2, got %d", code)
	}

	// Engine-level validation is enforced at the CLI boundary.
	if _, code := run(t, h, "guardrail", "runtime", "add", "--name", "ra", "--type", "require-approval", "--pattern", "foo"); code != 2 {
		t.Errorf("require-approval + pattern should exit 2, got %d", code)
	}

	// Remove works; removing a missing rule is a usage error.
	if _, code := run(t, h, "guardrail", "runtime", "remove", "--name", "approve-http"); code != 0 {
		t.Errorf("remove existing rule should exit 0, got %d", code)
	}
	if lst, _ := run(t, h, "guardrail", "runtime", "list"); strings.Contains(lst, "approve-http") {
		t.Error("removed rule should be gone next invocation")
	}
	if _, code := run(t, h, "guardrail", "runtime", "remove", "--name", "ghost"); code != 2 {
		t.Errorf("removing a missing rule should exit 2, got %d", code)
	}
}

// TestGuardrailRuntimeInterruptShowsApprovalRequired: an interrupt rule that
// fires must read "APPROVAL REQUIRED" and exit 3 — never "clean" (a fabricated
// pass while the exit code says a rule fired).
func TestGuardrailRuntimeInterruptShowsApprovalRequired(t *testing.T) {
	h := newHome(t)
	if _, code := run(t, h, "guardrail", "runtime", "add",
		"--name", "approve-http", "--tool", "http_*", "--type", "require-approval",
		"--message", "approve?"); code != 0 {
		t.Fatalf("add interrupt rule failed: %d", code)
	}
	out, code := run(t, h, "tripwire", "test", "--tool", "http_post", "--args", `{"url":"https://x"}`)
	if code != 3 {
		t.Errorf("a fired interrupt rule must exit 3, got %d: %s", code, out)
	}
	if strings.Contains(out, "clean") {
		t.Errorf("a fired interrupt rule must not read as clean: %q", out)
	}
	if !strings.Contains(out, "APPROVAL REQUIRED") {
		t.Errorf("expected APPROVAL REQUIRED header, got: %q", out)
	}
}

// TestGuardrailRuntimeRejectsPhantomProfile: `guardrail runtime add/remove
// --profile <typo>` must refuse an unknown profile, not silently write into a
// phantom profiles.<typo> block no run reads (#747). RED against pre-fix code,
// which created the phantom profile and exited 0.
func TestGuardrailRuntimeRejectsPhantomProfile(t *testing.T) {
	h := newHome(t)
	if _, code := run(t, h, "guardrail", "runtime", "add",
		"--name", "r1", "--tool", "http_*", "--type", "require-approval",
		"--profile", "no-such-profile"); code != 2 {
		t.Errorf("add with a typo'd profile should exit 2, got %d", code)
	}
	if _, code := run(t, h, "guardrail", "runtime", "remove",
		"--name", "r1", "--profile", "no-such-profile"); code != 2 {
		t.Errorf("remove with a typo'd profile should exit 2, got %d", code)
	}
	// A real profile still works.
	if _, code := run(t, h, "guardrail", "runtime", "add",
		"--name", "r1", "--tool", "http_*", "--type", "require-approval",
		"--profile", "coder"); code != 0 {
		t.Errorf("add to a real profile should exit 0, got %d", code)
	}
}
