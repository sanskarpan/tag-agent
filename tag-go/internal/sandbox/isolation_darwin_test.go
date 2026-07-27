package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestSbplQuote pins the escaping of the two characters that are meaningful
// inside an SBPL string literal, and the outright rejection of control chars.
func TestSbplQuote(t *testing.T) {
	cases := map[string]string{
		`/tmp/plain`:      `/tmp/plain`,
		`/tmp/a"b`:        `/tmp/a\"b`,
		`/tmp/a\b`:        `/tmp/a\\b`,
		`/tmp/a"b\c`:      `/tmp/a\"b\\c`,
		`/tmp/") (allow`:  `/tmp/\") (allow`,
		`/tmp/unicode-é/`: `/tmp/unicode-é/`,
	}
	for in, want := range cases {
		got, err := sbplQuote(in)
		if err != nil {
			t.Fatalf("sbplQuote(%q): unexpected error %v", in, err)
		}
		if got != want {
			t.Fatalf("sbplQuote(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"/tmp/a\nb", "/tmp/a\tb", "/tmp/a\x00b", "/tmp/a\x7fb"} {
		if _, err := sbplQuote(bad); err == nil {
			t.Fatalf("sbplQuote(%q) accepted a control character; it must be rejected", bad)
		}
	}
}

// TestSbplProfileInjection: a run dir crafted to close the literal and append
// its own rules must not produce a profile with a real `(allow network*)` rule.
func TestSbplProfileInjection(t *testing.T) {
	evil := `/tmp/pwn") (allow network*) (allow file-read* (subpath "/`
	prof, err := sbplProfile(evil, "/Users/nobody")
	if err != nil {
		return // rejecting the path is also acceptable
	}
	// The escaped form must appear, and the injected rule must not exist as a
	// standalone rule (it may only appear inside the quoted literal).
	if !strings.Contains(prof, `\") (allow network*)`) {
		t.Fatalf("injection payload was not escaped in profile:\n%s", prof)
	}
	for _, line := range strings.Split(prof, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "(allow network") {
			t.Fatalf("injected rule became a top-level profile rule:\n%s", prof)
		}
	}
	if !strings.Contains(prof, "(deny network*)") {
		t.Fatalf("profile lost its network denial:\n%s", prof)
	}
}

// TestSbplProfileRuleOrder: the run-dir re-allow must come after the $HOME
// read denial, otherwise a scratch dir under $HOME is unreadable (SBPL is
// last-match-wins).
func TestSbplProfileRuleOrder(t *testing.T) {
	prof, err := sbplProfile("/Users/nobody/scratch", "/Users/nobody")
	if err != nil {
		t.Fatalf("sbplProfile: %v", err)
	}
	denyHome := strings.Index(prof, `(deny file-read* (subpath "/Users/nobody"))`)
	allowRun := strings.Index(prof, `(allow file-read* file-write* (subpath "/Users/nobody/scratch"))`)
	if denyHome < 0 || allowRun < 0 {
		t.Fatalf("expected rules missing from profile:\n%s", prof)
	}
	if allowRun < denyHome {
		t.Fatalf("run-dir re-allow precedes the $HOME denial; a scratch dir under $HOME would be unreadable:\n%s", prof)
	}
	for _, want := range []string{
		"(deny network*)",
		`(subpath "/private/etc")`,
		`(literal "/private/etc/master.passwd")`,
		`(subpath "/Users/nobody/.ssh")`,
		`(subpath "/Users/nobody/Library/Keychains")`,
		`(deny file-write* (subpath "/usr")`,
	} {
		if !strings.Contains(prof, want) {
			t.Fatalf("profile missing %q:\n%s", want, prof)
		}
	}
}

// TestExecFailsClosedWithoutSandboxExec: with sandbox-exec unreachable the run
// must NOT fall back to an unconfined host shell. It must return exit 127 with
// Python's message naming --backend docker.
func TestExecFailsClosedWithoutSandboxExec(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir: sandbox-exec is not findable
	res, err := Exec(context.Background(), Options{Command: "echo leaked", Dir: t.TempDir(), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("expected a fail-closed Result, got error: %v", err)
	}
	if res.Exit != 127 {
		t.Fatalf("expected exit 127 when sandbox-exec is missing, got %+v", res)
	}
	if strings.Contains(res.Stdout, "leaked") {
		t.Fatalf("the command RAN without sandbox-exec: stdout=%q", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "--backend docker") {
		t.Fatalf("fail-closed error should point at --backend docker, got %q", res.Stderr)
	}
	if !strings.Contains(res.Isolation, "none") {
		t.Fatalf("fail-closed Result should report no isolation, got %q", res.Isolation)
	}
}

// TestExecDeniesPrivateEtcPasswd covers the macOS-real path behind /etc.
func TestExecDeniesPrivateEtcPasswd(t *testing.T) {
	res, err := Exec(context.Background(), Options{Command: "cat /private/etc/passwd", Dir: t.TempDir(), Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Exit == 0 {
		t.Fatalf("/private/etc/passwd was readable inside the sandbox: %q", res.Stdout)
	}
}

// TestExecDeniesHomeSecretRead: a file under $HOME outside the run dir must be
// unreadable even though the process runs as the same user.
func TestExecDeniesHomeSecretRead(t *testing.T) {
	res, err := Exec(context.Background(), Options{
		Command: `ls "$(dirname "$PWD")" >/dev/null 2>&1; cat /Users/*/.ssh/* 2>/dev/null | head -c 32`,
		Dir:     t.TempDir(),
		Timeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "" {
		t.Fatalf("~/.ssh material leaked into the sandbox: %q", res.Stdout)
	}
}
