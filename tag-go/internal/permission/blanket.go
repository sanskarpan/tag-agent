package permission

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// IsBlanketPattern reports whether a rule pattern grants BLANKET coverage —
// i.e. it names nothing in particular and is a "match everything" in every
// meaningful sense.
//
// This exists because of a real bypass. resolve() skips a blanket ALLOW for a
// credential-shaped path, so that typing `--allow-tool read_file` cannot
// silently hand the model your .env. That skip used to test how the rule was
// SPELLED — `r.Pattern == ""` — which is not the same question. Every universal
// wildcard (`*`, `**`, `*.*`, `?*`, `.*`, `**/*`) carries a NON-empty pattern,
// so it was honoured, and because SortBySpecificity ranks a patterned flag rule
// above an unpatterned one it landed at position 1, outranking all 28 built-in
// credential denies. `--allow-tool 'read_file:*'` leaked .env and *.pem.
//
// The classification is therefore by WHAT A PATTERN MATCHES, not how it is
// written: a pattern is blanket unless it contains at least one NAMING
// character — a letter or a digit. Wildcards (`*`, `?`), separators (`/`), dots
// and `~` constrain nothing an operator could point at, so a pattern built only
// from them has not named the credential it is unprotecting.
//
// The consequence is the documented doctrine, enforced rather than assumed: to
// cover a credential path you must NAME it (`--allow-tool 'read_file:*.env'`,
// `read_file:/srv/app/.env`) — or use --dangerously-allow-all, which announces
// itself. `[a-z.]*` and every other genuinely restrictive glob are unaffected.
func IsBlanketPattern(pattern string) bool {
	p := strings.TrimSpace(pattern)
	if p == "" {
		return true
	}
	for _, r := range p {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// credentialProbePaths are concrete paths that the built-in credential denies
// protect. They exist so a rule can be tested for "could this match a
// credential?" by MATCHING rather than by parsing its glob.
func credentialProbePaths() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "/home/user"
	}
	return []string{
		"/workspace/.env",
		"/workspace/prod.env",
		"/workspace/.env.production",
		"/workspace/deploy.pem",
		"/workspace/server.key",
		"/workspace/bundle.p12",
		"/workspace/bundle.pfx",
		"/workspace/id_rsa",
		"/workspace/id_ed25519",
		"/workspace/.netrc",
		"/workspace/.npmrc",
		"/workspace/.pypirc",
		"/workspace/credentials.json",
		"/workspace/service-account-prod.json",
		filepath.Join(home, ".ssh", "id_ed25519"),
		filepath.Join(home, ".aws", "credentials"),
		filepath.Join(home, ".gnupg", "secring.gpg"),
		filepath.Join(home, ".config", "gcloud", "creds.db"),
	}
}

// AllowsOutrankingCredentialGuards returns the indices of ALLOW rules that are
// ordered above the built-in credential denies AND can match at least one
// credential-shaped path.
//
// It is what makes the ordering auditable instead of implicit: `tag permissions
// show` prints a warning next to any such rule, so an operator can see that the
// rule they wrote sits in front of the protections rather than behind them.
// A rule that NAMES a credential is reported too — that is a deliberate choice
// the operator made, and the display should still say so.
func AllowsOutrankingCredentialGuards(rules []Rule) []int {
	guards := -1
	for i, r := range rules {
		if r.Source == SourceBuiltinCredential {
			guards = i
			break
		}
	}
	if guards <= 0 {
		return nil
	}
	probes := credentialProbePaths()
	var out []int
	for i := 0; i < guards; i++ {
		r := rules[i]
		if r.Action != Allow {
			continue
		}
		if r.Kind != KindNone && r.Kind != KindPath {
			continue
		}
		tool := r.Tool
		if tool == "" || tool == "*" {
			tool = ToolReadFile
		}
		for _, p := range probes {
			if r.matches(Request{Tool: tool, Kind: KindPath, Subject: p}) {
				out = append(out, i)
				break
			}
		}
	}
	return out
}
