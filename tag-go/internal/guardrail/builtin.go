package guardrail

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// detector is one shipped content check.
type detector struct {
	name string
	re   *regexp.Regexp
	// verify optionally rejects a regex hit (cheap false-positive suppression).
	verify func(string) bool
}

// secretDetectors recognise credential-shaped strings. They are deliberately
// high-precision rather than exhaustive: a guardrail that cries wolf gets turned
// off, and internal/security already owns broad repository secret scanning. This
// set exists to catch a credential moving through a TOOL boundary at runtime.
var secretDetectors = []detector{
	{name: "aws-access-key-id", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{name: "github-token", re: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,255}\b`)},
	{name: "slack-token", re: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)},
	{name: "google-api-key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`)},
	{name: "anthropic-api-key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{20,}\b`)},
	{name: "openai-api-key", re: regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_\-]{32,}\b`)},
	{name: "private-key-block", re: regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |PGP |DSA )?PRIVATE KEY-----`)},
	{name: "jwt", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\b`)},
	{
		name: "generic-api-key-assignment",
		re: regexp.MustCompile(`(?i)\b(?:api[_\-]?key|secret[_\-]?key|access[_\-]?token|auth[_\-]?token|password)\b` +
			`\s*[:=]\s*["']?([A-Za-z0-9/+_\-]{16,})["']?`),
		// Placeholder values in examples and templates are not leaks.
		verify: func(s string) bool { return !placeholderRe.MatchString(s) },
	},
}

var placeholderRe = regexp.MustCompile(`(?i)(x{8,}|\.{3,}|<[a-z_ -]+>|your[_\- ]?(api[_\- ]?)?key|` +
	`example|placeholder|redacted|changeme|dummy|s+a+m+p+l+e|\$\{[^}]+\}|\bnull\b|\bnone\b)`)

// destructiveDetectors recognise commands that destroy data or hand the host to
// a remote party. They target the ARGUMENT text of a tool call.
var destructiveDetectors = []detector{
	// A recursive-force rm whose TARGET starts at / or ~. Long options are
	// allowed on either side of the -rf so `rm -rf --no-preserve-root /` — the
	// canonical way to actually destroy a box — cannot slip between the flag and
	// the path. Order-insensitive: -rf and -fr both count.
	{name: "rm-rf-root", re: regexp.MustCompile(
		`(?i)\brm\s+(?:-{1,2}[a-z][a-z-]*\s+)*-[a-z]*[rR][a-z]*[fF][a-z]*\s+(?:-{1,2}[a-z][a-z-]*\s+|--\s+)*[/~]\S*`)},
	{name: "rm-rf-root-alt", re: regexp.MustCompile(
		`(?i)\brm\s+(?:-{1,2}[a-z][a-z-]*\s+)*-[a-z]*[fF][a-z]*[rR][a-z]*\s+(?:-{1,2}[a-z][a-z-]*\s+|--\s+)*[/~]\S*`)},
	{name: "mkfs", re: regexp.MustCompile(`(?i)\bmkfs(?:\.[a-z0-9]+)?\s`)},
	{name: "dd-to-device", re: regexp.MustCompile(`(?i)\bdd\b[^\n]*\bof=/dev/`)},
	{name: "fork-bomb", re: regexp.MustCompile(`:\(\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`)},
	{name: "chmod-777-root", re: regexp.MustCompile(`(?i)\bchmod\s+(?:-[a-zA-Z]+\s+)*777\s+/(?:\s|$)`)},
	{name: "curl-pipe-shell", re: regexp.MustCompile(`(?i)\b(?:curl|wget)\b[^\n|]*\|\s*(?:sudo\s+)?(?:ba|z|k)?sh\b`)},
	{name: "history-wipe", re: regexp.MustCompile(`(?i)\b(?:history\s+-c|shred\s+|>\s*~?/?\.bash_history)`)},
	{name: "drop-database", re: regexp.MustCompile(`(?i)\bdrop\s+(?:database|schema|table)\b`)},
	{name: "git-force-push-main", re: regexp.MustCompile(`(?i)\bgit\s+push\b[^\n]*(?:--force|-f)\b[^\n]*\b(?:main|master)\b`)},
}

// builtinNames lists the shipped detector families.
func builtinNames() []string { return []string{"destructive", "secrets"} }

// ValidateBuiltin rejects an unknown detector family at CONFIG LOAD time, so a
// typo (`builtin: secret`) is a startup error instead of a rule that silently
// never fires.
func ValidateBuiltin(name string) error {
	for _, n := range builtinNames() {
		if n == name {
			return nil
		}
	}
	return fmt.Errorf("unknown builtin guardrail detector %q (want one of %v)", name, builtinNames())
}

func detectorsFor(name string) []detector {
	switch name {
	case "secrets":
		return secretDetectors
	case "destructive":
		return destructiveDetectors
	}
	return nil
}

// builtinScan runs a shipped detector family over content and returns redacted
// hits, deduplicated by (detector, offset) and ordered by offset so output is
// deterministic.
func builtinScan(family, content string) []hit {
	var out []hit
	for _, d := range detectorsFor(family) {
		locs := d.re.FindAllStringIndex(content, 8)
		for _, l := range locs {
			raw := content[l[0]:l[1]]
			if d.verify != nil && !d.verify(raw) {
				continue
			}
			out = append(out, hit{
				detector: family + ":" + d.name,
				excerpt:  Redact(strings.TrimSpace(raw)),
				offset:   l[0],
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].offset != out[j].offset {
			return out[i].offset < out[j].offset
		}
		return out[i].detector < out[j].detector
	})
	return out
}

// BuiltinPresets are ready-made rulesets an operator can switch on by name
// instead of writing regexes. `tripwire: {preset: strict}` is the shortest path
// to a working guardrail, which matters because an unused guardrail protects
// nobody.
func BuiltinPresets() map[string][]Rule {
	return map[string][]Rule{
		// standard: stop credentials leaving through a tool boundary, and stop the
		// handful of shell commands that are never a legitimate agent action.
		"standard": {
			{Name: "builtin-destructive-command", Type: TypePattern, Stage: StageToolInput,
				Tool: "bash", Builtin: "destructive", Action: ActionBlock,
				Message: "destructive shell command", Source: "preset:standard"},
			{Name: "builtin-secret-in-tool-input", Type: TypePattern, Stage: StageToolInput,
				Tool: "*", Builtin: "secrets", Action: ActionBlock,
				Message: "credential-shaped value in tool arguments", Source: "preset:standard"},
			{Name: "builtin-secret-in-tool-output", Type: TypePattern, Stage: StageToolOutput,
				Tool: "*", Builtin: "secrets", Action: ActionBlock,
				Message: "credential-shaped value in tool result", Source: "preset:standard"},
		},
		// warn-only: identical coverage, nothing halts. For rolling a policy out
		// on a live workload before turning it into a gate.
		"observe": {
			{Name: "builtin-destructive-command", Type: TypePattern, Stage: StageToolInput,
				Tool: "bash", Builtin: "destructive", Action: ActionWarn,
				Message: "destructive shell command", Source: "preset:observe"},
			{Name: "builtin-secret-in-tool-input", Type: TypePattern, Stage: StageToolInput,
				Tool: "*", Builtin: "secrets", Action: ActionWarn,
				Message: "credential-shaped value in tool arguments", Source: "preset:observe"},
			{Name: "builtin-secret-in-tool-output", Type: TypePattern, Stage: StageToolOutput,
				Tool: "*", Builtin: "secrets", Action: ActionWarn,
				Message: "credential-shaped value in tool result", Source: "preset:observe"},
		},
	}
}

// PresetNames lists the presets in a stable order.
func PresetNames() []string {
	names := make([]string, 0, len(BuiltinPresets()))
	for k := range BuiltinPresets() {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
