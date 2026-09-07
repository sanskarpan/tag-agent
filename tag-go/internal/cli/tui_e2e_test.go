package cli_test

import (
	"strings"
	"testing"
)

func TestE2ETUIRejectsUnknownProfileBeforeTerminalEntry(t *testing.T) {
	out, code := run(t, newHome(t), "tui", "--profile", "does-not-exist")
	if code == 0 || !strings.Contains(out, `unknown profile "does-not-exist"`) || strings.Contains(out, "open /dev/tty") {
		t.Fatalf("expected profile validation before TTY setup: code=%d output=%q", code, out)
	}
}
