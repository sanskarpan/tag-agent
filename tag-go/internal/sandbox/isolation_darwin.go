package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// sandboxExecMissingMsg matches src/tag/sandbox.py's wording so the Python and
// Go CLIs report the same fail-closed error (Python returns exit 127 too).
const sandboxExecMissingMsg = "sandbox-exec not available: cannot isolate on this platform. " +
	"Use --backend docker for isolated execution."

// darwinIsolationBase is the guarantee string for a run where every rule below
// was emitted unweakened. It is never reported verbatim unless that is true:
// see darwinIsolationString.
const darwinIsolationBase = "sandbox-exec (SBPL): network denied; /etc, /private/etc, /var/db, " +
	"/private/var/db and master.passwd unreadable; $HOME reads denied (~/.ssh, ~/.aws, " +
	"~/.gnupg, ~/.config, ~/.gcloud, ~/.kube, ~/.docker, ~/Library/Keychains denied for " +
	"read+write); run dir read/write allowed; /usr, /bin, /sbin, /System, /Library read-only"

// darwinIsolationString renders the honest guarantee for one run. Result.Isolation
// is documented as never being an aspirational claim, so any rule that had to be
// weakened (or could not be emitted at all) is appended verbatim rather than
// silently dropped from a compile-time constant.
func darwinIsolationString(weakened []string) string {
	if len(weakened) == 0 {
		return darwinIsolationBase
	}
	return darwinIsolationBase + "; WEAKENED: " + strings.Join(weakened, "; ")
}

// systemWriteRoots are made read-only by the profile.
var systemWriteRoots = []string{"/usr", "/bin", "/sbin", "/System", "/Library"}

// The run-dir boundary check (validateRunDir, normalizeRunDir, the darwin
// boundary tables and sensitiveHomeDirs) lives in rundir.go: Linux has the same
// class of flaw -- Landlock makes the run dir the only writable tree -- so the
// logic is shared and only the tables vary per platform.

// sbplQuote renders p as an SBPL string literal body, escaping the two
// characters that are meaningful inside one (`\` and `"`).
//
// This is a security boundary: the run dir is interpolated into a policy that
// is handed to sandbox-exec, so an unescaped quote would let a crafted
// directory name terminate the literal and inject arbitrary SBPL (e.g.
// re-allowing network). Control characters (including newline) are rejected
// outright rather than escaped, because SBPL's escape handling for them is not
// something we want to depend on. Invalid UTF-8 is rejected for a related
// reason: ranging over a string yields utf8.RuneError for a bad byte, so
// escaping it would silently write U+FFFD and describe a DIFFERENT path than
// the one the process actually runs in.
func sbplQuote(p string) (string, error) {
	if !utf8.ValidString(p) {
		return "", fmt.Errorf("sandbox: refusing to build a policy for a path that is not valid UTF-8: %q", p)
	}
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

// sbplProfile builds the seatbelt profile for a run confined to runDir, and
// reports any rule it had to weaken so Result.Isolation can say so.
//
// Rule order is the security-critical part. SBPL is last-match-wins, so the
// run-dir re-allow used to cancel every denial that came before it whenever the
// run dir sat at or above one of them. The denials therefore come LAST and can
// no longer be cancelled:
//
//	(allow default) -> (deny network*) -> (allow rw runDir) -> all denials
//
// A scratch dir under $HOME still works because the $HOME read denial carves
// the run dir back out with (require-not (subpath runDir)) instead of relying
// on rule order. That carve-out is safe only because validateRunDir has already
// guaranteed the run dir is strictly below $HOME, never $HOME itself.
func sbplProfile(runDir, home string) (string, []string, error) {
	dir, err := sbplQuote(runDir)
	if err != nil {
		return "", nil, err
	}
	if err := validateRunDir(runDir, home); err != nil {
		return "", nil, err
	}
	var weakened []string
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n(deny network*)\n")
	// The run dir is allowed FIRST; every denial below outranks it.
	fmt.Fprintf(&b, "(allow file-read* file-write* (subpath \"%s\"))\n", dir)
	b.WriteString(`(deny file-read* file-write*` +
		` (subpath "/etc") (subpath "/private/etc")` +
		` (subpath "/var/db") (subpath "/private/var/db")` +
		` (literal "/etc/master.passwd") (literal "/private/etc/master.passwd"))` + "\n")
	if home != "" {
		h, err := sbplQuote(home)
		if err != nil {
			return "", nil, err
		}
		// Deny reading the user's home tree by default (protects secrets). When
		// the run dir lives under $HOME, carve exactly that subtree back out --
		// a scoped exception, not a re-ordering that cancels the whole rule.
		if isAtOrUnder(normalizeRunDir(runDir), normalizeRunDir(home)) {
			fmt.Fprintf(&b, "(deny file-read* (require-all (subpath \"%s\") (require-not (subpath \"%s\"))))\n", h, dir)
		} else {
			fmt.Fprintf(&b, "(deny file-read* (subpath \"%s\"))\n", h)
		}
		// Explicitly deny sensitive credential dirs for reads AND writes. These
		// are unconditional: validateRunDir guarantees the run dir is neither
		// inside one nor an ancestor of one.
		var parts []string
		for _, d := range sensitiveHomeDirs {
			q, err := sbplQuote(home + "/" + d)
			if err != nil {
				return "", nil, err
			}
			parts = append(parts, fmt.Sprintf("(subpath \"%s\")", q))
		}
		fmt.Fprintf(&b, "(deny file-read* file-write* %s)\n", strings.Join(parts, " "))
	} else {
		weakened = append(weakened, "$HOME read denial NOT applied (no home directory could be resolved)")
	}
	// System trees are read-only. If the run dir is inside one (e.g.
	// /usr/local/build) it is carved back out, since the run dir must stay
	// writable -- and that exception is reported rather than hidden.
	var sysParts []string
	inSystem := ""
	for _, root := range systemWriteRoots {
		sysParts = append(sysParts, fmt.Sprintf("(subpath \"%s\")", root))
		if isAtOrUnder(normalizeRunDir(runDir), root) {
			inSystem = root
		}
	}
	if inSystem != "" {
		fmt.Fprintf(&b, "(deny file-write* (require-all (require-any %s) (require-not (subpath \"%s\"))))\n",
			strings.Join(sysParts, " "), dir)
		weakened = append(weakened, fmt.Sprintf("%s is read-only EXCEPT the run dir %s, which stays writable", inSystem, runDir))
	} else {
		fmt.Fprintf(&b, "(deny file-write* %s)\n", strings.Join(sysParts, " "))
	}
	return b.String(), weakened, nil
}

// buildIsolation wraps the command in sandbox-exec. If sandbox-exec is not on
// PATH, or the run dir is too broad to confine, the run fails closed with exit
// 127 rather than executing unconfined.
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
	if home != "" {
		// Match confineDir's resolution so a symlinked home compares equal.
		if real, err := filepath.EvalSymlinks(home); err == nil {
			home = real
		}
	}
	if err := validateRunDir(runDir, home); err != nil {
		return nil, &Result{
			Exit:      127,
			Stderr:    err.Error(),
			Isolation: runDirTooBroadIsolation,
		}, nil
	}
	profile, weakened, err := sbplProfile(runDir, home)
	if err != nil {
		return nil, nil, err
	}
	return &isolationPlan{
		Prefix:    []string{se, "-p", profile},
		Isolation: darwinIsolationString(weakened),
	}, nil, nil
}
