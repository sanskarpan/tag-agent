package sandbox

import (
	"context"
	"os"
	"path/filepath"
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
	// Invalid UTF-8 must be rejected, not silently transliterated: ranging over
	// a string yields utf8.RuneError per bad byte, so escaping would write
	// U+FFFD and the profile would describe a DIFFERENT path than the process
	// actually runs in.
	for _, bad := range []string{"/tmp/a\xffb", "/tmp/\x80\x81", "/tmp/caf\xe9"} {
		got, err := sbplQuote(bad)
		if err == nil {
			t.Fatalf("sbplQuote(%q) accepted invalid UTF-8 and produced %q; it must be rejected", bad, got)
		}
	}
}

// TestSbplProfileInjection: a run dir crafted to close the literal and append
// its own rules must not produce a profile with a real `(allow network*)` rule.
func TestSbplProfileInjection(t *testing.T) {
	evil := `/tmp/pwn") (allow network*) (allow file-read* (subpath "/`
	prof, _, err := sbplProfile(evil, "/Users/nobody")
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

// TestSbplProfileRuleOrder pins the CORRECTED invariant. SBPL is
// last-match-wins, so the run-dir re-allow must come BEFORE every denial: when
// it came last, a run dir at or above a protected path silently cancelled that
// path's denial (`--dir "$HOME"` re-opened ~/.ssh; `--dir /` re-opened /etc).
//
// The earlier version of this test asserted the opposite (allowRun > denyHome)
// -- i.e. it asserted the vulnerability -- because the $HOME carve-out for a
// scratch dir was done by ordering. It is now done by (require-not (subpath
// rundir)) inside the denial itself, which is scoped instead of cancelling.
func TestSbplProfileRuleOrder(t *testing.T) {
	prof, weakened, err := sbplProfile("/Users/nobody/scratch", "/Users/nobody")
	if err != nil {
		t.Fatalf("sbplProfile: %v", err)
	}
	if len(weakened) != 0 {
		t.Fatalf("unexpected weakened rules for an ordinary run dir: %v", weakened)
	}
	allowRun := strings.Index(prof, `(allow file-read* file-write* (subpath "/Users/nobody/scratch"))`)
	if allowRun < 0 {
		t.Fatalf("run-dir allowance missing from profile:\n%s", prof)
	}
	// Every denial must outrank the run-dir allowance.
	for _, deny := range []string{
		"(deny network*)",
		`(deny file-read* file-write* (subpath "/etc")`,
		`(deny file-read* (require-all (subpath "/Users/nobody")`,
		`(deny file-read* file-write* (subpath "/Users/nobody/.ssh")`,
		`(deny file-write* (subpath "/usr")`,
	} {
		i := strings.Index(prof, deny)
		if i < 0 {
			t.Fatalf("profile missing denial %q:\n%s", deny, prof)
		}
		if deny == "(deny network*)" {
			continue // network denial legitimately precedes everything
		}
		if i < allowRun {
			t.Fatalf("denial %q precedes the run-dir allowance; a broad run dir could cancel it:\n%s", deny, prof)
		}
	}
	// The scratch dir under $HOME is carved out of the home denial by scope,
	// not by ordering.
	if !strings.Contains(prof, `(require-not (subpath "/Users/nobody/scratch"))`) {
		t.Fatalf("home denial does not carve out the run dir; a scratch dir under $HOME would be unreadable:\n%s", prof)
	}
	for _, want := range []string{
		`(subpath "/private/etc")`,
		`(literal "/private/etc/master.passwd")`,
		`(subpath "/Users/nobody/.ssh")`,
		`(subpath "/Users/nobody/Library/Keychains")`,
	} {
		if !strings.Contains(prof, want) {
			t.Fatalf("profile missing %q:\n%s", want, prof)
		}
	}
}

// TestValidateRunDirRejectsBroadDirs is the unit half of the "broad run dir
// re-opens every denial" regression: a run dir at or above a protected tree is
// refused outright, while an ordinary scratch dir under $HOME is accepted.
//
// The table now lives in rundir_test.go (darwinRunDirCases) because the check
// itself is shared with Linux; this test pins that the RUNTIME entry point --
// validateRunDir, i.e. what buildIsolation calls -- still selects the darwin
// tables on a darwin host.
func TestValidateRunDirRejectsBroadDirs(t *testing.T) {
	c := darwinRunDirCases()
	for _, d := range c.reject {
		if err := validateRunDir(d, c.home); err == nil {
			t.Fatalf("validateRunDir(%q) accepted a run dir at/above a protected boundary", d)
		} else if !strings.Contains(err.Error(), "--backend docker") {
			t.Fatalf("validateRunDir(%q) error should name the escape hatch, got %q", d, err)
		}
	}
	for _, d := range c.accept {
		if err := validateRunDir(d, c.home); err != nil {
			t.Fatalf("validateRunDir(%q) rejected an ordinary run dir: %v", d, err)
		}
	}
}

// TestSbplProfileRejectsBroadRunDirs: the profile builder itself refuses a
// boundary run dir, so no caller can construct a self-cancelling policy.
func TestSbplProfileRejectsBroadRunDirs(t *testing.T) {
	for _, d := range []string{"/", "/Users", "/Users/nobody", "/System/Volumes/Data", "/private"} {
		if prof, _, err := sbplProfile(d, "/Users/nobody"); err == nil {
			t.Fatalf("sbplProfile(%q) built a policy that re-opens its own denials:\n%s", d, prof)
		}
	}
}

// TestExecRejectsBroadRunDirs is the end-to-end half: `--dir /`, `--dir /Users`,
// `--dir $HOME` (and the default `--dir` == cwd when cwd is $HOME) must fail
// closed with exit 127 -- and must NOT be able to read /etc/passwd or ~/.ssh.
//
// Before the fix each of these ran with exit 0 and printed the secret while
// Result.Isolation still claimed those exact paths were denied.
func TestExecRejectsBroadRunDirs(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory available")
	}
	probes := map[string]string{
		"etc_passwd": "cat /etc/passwd",
		"home_ssh":   `cat ` + home + `/.ssh/* 2>/dev/null | head -c 64`,
		"rel_ssh":    `head -c 64 .ssh/id_* 2>/dev/null`,
	}
	for _, dir := range []string{"/", "/Users", home, "/System/Volumes/Data", "/private"} {
		for name, probe := range probes {
			t.Run(filepath.Base(dir)+"_"+name, func(t *testing.T) {
				res, err := Exec(context.Background(), Options{Command: probe, Dir: dir, Timeout: 20 * time.Second})
				if err != nil {
					return // rejected outright: also fail-closed, acceptable
				}
				if res.Exit == 127 && strings.Contains(res.Stderr, "--backend docker") {
					return // the expected fail-closed answer
				}
				if res.Exit == 0 || strings.TrimSpace(res.Stdout) != "" {
					t.Fatalf("run dir %q leaked with probe %q: exit=%d stdout=%q isolation=%q",
						dir, probe, res.Exit, res.Stdout, res.Isolation)
				}
			})
		}
	}
}

// TestIsolationStringIsConditional: Result.Isolation is documented as never
// being an aspirational claim, but it used to be a compile-time constant that
// listed guarantees regardless of what the profile actually contained. Any rule
// the builder had to weaken must now show up in the reported string.
func TestIsolationStringIsConditional(t *testing.T) {
	// Fully-enforced profile: no weakening, the plain guarantee.
	if _, weakened, err := sbplProfile("/Users/nobody/scratch", "/Users/nobody"); err != nil || len(weakened) != 0 {
		t.Fatalf("ordinary run dir should be fully enforced: weakened=%v err=%v", weakened, err)
	}
	if got := darwinIsolationString(nil); strings.Contains(got, "WEAKENED") {
		t.Fatalf("unweakened isolation string should not mention weakening: %q", got)
	}

	// No home resolvable: the $HOME read denial cannot be emitted at all, and
	// the string must admit it rather than keep claiming it.
	_, weakened, err := sbplProfile("/private/tmp/x", "")
	if err != nil {
		t.Fatalf("sbplProfile: %v", err)
	}
	if len(weakened) == 0 || !strings.Contains(strings.Join(weakened, " "), "$HOME read denial NOT applied") {
		t.Fatalf("a profile with no $HOME denial still claims one: weakened=%v", weakened)
	}
	if !strings.Contains(darwinIsolationString(weakened), "WEAKENED") {
		t.Fatal("weakened rules are not surfaced in the isolation string")
	}

	// Run dir inside a read-only system root: it must stay writable, and that
	// exception must be reported instead of hidden behind "/usr ... read-only".
	prof, weakened, err := sbplProfile("/usr/local/build", "/Users/nobody")
	if err != nil {
		t.Fatalf("sbplProfile: %v", err)
	}
	if !strings.Contains(prof, `(require-not (subpath "/usr/local/build"))`) {
		t.Fatalf("run dir inside /usr is not carved out of the read-only denial:\n%s", prof)
	}
	joined := strings.Join(weakened, " ")
	if !strings.Contains(joined, "/usr is read-only EXCEPT the run dir /usr/local/build") {
		t.Fatalf("the /usr write exception is not reported: weakened=%v", weakened)
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
//
// The previous version globbed /Users/*/.ssh/* and only asserted stdout == "".
// On a host with no ~/.ssh that passes with ZERO isolation (verified: with
// buildIsolation neutered to a plain `sh -c` it still went green), so the test
// now plants its own canary and asserts the canary's contents are absent -- and
// proves the canary is genuinely readable outside the sandbox first.
func TestExecDeniesHomeSecretRead(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory available")
	}
	// A credential-shaped directory under $HOME, created and removed by the test
	// so the assertion never depends on the host having real secrets.
	credDir, err := os.MkdirTemp(home, "tag-sbx-cred-")
	if err != nil {
		t.Skipf("cannot create a canary dir under %s: %v", home, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(credDir) })
	const canary = "CANARY-8f3a1c2e-home-secret-must-not-leak"
	secret := filepath.Join(credDir, "id_canary")
	if err := os.WriteFile(secret, []byte(canary+"\n"), 0o600); err != nil {
		t.Fatalf("writing canary: %v", err)
	}
	// Precondition: the canary IS readable unsandboxed, so a clean stdout below
	// can only mean the sandbox denied it.
	if b, err := os.ReadFile(secret); err != nil || !strings.Contains(string(b), canary) {
		t.Fatalf("canary is not readable on the host, the test would prove nothing: %v", err)
	}

	res, err := Exec(context.Background(), Options{
		Command: "cat " + secret + " 2>&1",
		Dir:     t.TempDir(),
		Timeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Contains(res.Stdout, canary) {
		t.Fatalf("a secret under $HOME leaked into the sandbox: %q (isolation %q)", res.Stdout, res.Isolation)
	}
	if res.Exit == 0 {
		t.Fatalf("reading a $HOME secret SUCCEEDED inside the sandbox: stdout=%q", res.Stdout)
	}
}

// TestExecDeniesCredentialDirWrite covers the read+write half of the credential
// denial: the sandbox must not be able to append to ~/.ssh (which would mean
// persistent host compromise via authorized_keys).
func TestExecDeniesCredentialDirWrite(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory available")
	}
	sshDir := filepath.Join(home, ".ssh")
	if st, err := os.Stat(sshDir); err != nil || !st.IsDir() {
		t.Skipf("no %s on this host; cannot prove the write denial", sshDir)
	}
	target := filepath.Join(sshDir, "tag-sbx-write-canary")
	t.Cleanup(func() { _ = os.Remove(target) })
	res, err := Exec(context.Background(), Options{
		Command: "echo PWNED > " + target,
		Dir:     t.TempDir(),
		Timeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatalf("the sandbox WROTE into %s (exit=%d, isolation=%q): persistent host compromise",
			sshDir, res.Exit, res.Isolation)
	}
	if res.Exit == 0 {
		t.Fatalf("writing into %s reported success inside the sandbox", sshDir)
	}
}
