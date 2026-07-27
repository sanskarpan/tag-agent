package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// sandboxExecMissingMsg matches src/tag/sandbox.py's wording so the Python and
// Go CLIs report the same fail-closed error (Python returns exit 127 too).
const sandboxExecMissingMsg = "sandbox-exec not available: cannot isolate on this platform. " +
	"Use --backend docker for isolated execution."

// darwinIsolation is the guarantee string reported in Result.Isolation.
const darwinIsolation = "sandbox-exec (SBPL): network denied; /etc, /private/etc, /var/db, " +
	"/private/var/db and master.passwd unreadable; $HOME reads denied (~/.ssh, ~/.aws, " +
	"~/.gnupg, ~/.config, ~/.gcloud, ~/.kube, ~/.docker, ~/Library/Keychains denied for " +
	"read+write); run dir read/write allowed; /usr, /bin, /sbin, /System, /Library read-only"

// sensitiveHomeDirs are credential locations denied for BOTH read and write.
var sensitiveHomeDirs = []string{
	".ssh", ".aws", ".gnupg", ".config", ".gcloud", ".kube", ".docker",
	"Library/Keychains",
}

// sbplQuote renders p as an SBPL string literal body, escaping the two
// characters that are meaningful inside one (`\` and `"`).
//
// This is a security boundary: the run dir is interpolated into a policy that
// is handed to sandbox-exec, so an unescaped quote would let a crafted
// directory name terminate the literal and inject arbitrary SBPL (e.g.
// re-allowing network). Control characters (including newline) are rejected
// outright rather than escaped, because SBPL's escape handling for them is not
// something we want to depend on.
func sbplQuote(p string) (string, error) {
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("sandbox: refusing to build a policy for a path containing a control character: %q", p)
		}
	}
	var b strings.Builder
	b.Grow(len(p) + 8)
	for _, r := range p {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String(), nil
}

// sbplProfile builds the seatbelt profile for a run confined to runDir.
//
// Rule order matters: SBPL is last-match-wins, so the re-allow of runDir must
// come AFTER the blanket home denial for a scratch dir under $HOME to work.
func sbplProfile(runDir, home string) (string, error) {
	dir, err := sbplQuote(runDir)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n(deny network*)\n")
	b.WriteString(`(deny file-read* file-write*` +
		` (subpath "/etc") (subpath "/private/etc")` +
		` (subpath "/var/db") (subpath "/private/var/db")` +
		` (literal "/etc/master.passwd") (literal "/private/etc/master.passwd"))` + "\n")
	if home != "" {
		h, err := sbplQuote(home)
		if err != nil {
			return "", err
		}
		// Deny reading the user's home tree by default (protects secrets).
		fmt.Fprintf(&b, "(deny file-read* (subpath \"%s\"))\n", h)
		// Explicitly deny sensitive credential dirs for reads AND writes.
		var parts []string
		for _, d := range sensitiveHomeDirs {
			q, err := sbplQuote(home + "/" + d)
			if err != nil {
				return "", err
			}
			parts = append(parts, fmt.Sprintf("(subpath \"%s\")", q))
		}
		fmt.Fprintf(&b, "(deny file-read* file-write* %s)\n", strings.Join(parts, " "))
	}
	// Re-allow the run directory (must follow the home denial above).
	fmt.Fprintf(&b, "(allow file-read* file-write* (subpath \"%s\"))\n", dir)
	b.WriteString(`(deny file-write*` +
		` (subpath "/usr") (subpath "/bin") (subpath "/sbin")` +
		` (subpath "/System") (subpath "/Library"))` + "\n")
	return b.String(), nil
}

// buildIsolation wraps the command in sandbox-exec. If sandbox-exec is not on
// PATH the run fails closed with exit 127 rather than executing unconfined.
func buildIsolation(runDir string, _ time.Duration) (*isolationPlan, *Result, error) {
	se, err := exec.LookPath("sandbox-exec")
	if err != nil {
		return nil, &Result{
			Exit:      127,
			Stderr:    sandboxExecMissingMsg,
			Isolation: "none (failed closed: sandbox-exec missing)",
		}, nil
	}
	home, _ := os.UserHomeDir()
	profile, err := sbplProfile(runDir, home)
	if err != nil {
		return nil, nil, err
	}
	return &isolationPlan{
		Prefix:    []string{se, "-p", profile},
		Isolation: darwinIsolation,
	}, nil, nil
}
