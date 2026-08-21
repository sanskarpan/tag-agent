package cli_test

import "testing"

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
