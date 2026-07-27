package permission

import (
	"fmt"
	"sort"
	"strings"
)

// Layer is one configuration source's contribution: an ordered list of
// pattern rules, a per-tool map, and an optional catch-all default.
type Layer struct {
	// Rules are pattern rules in author order (first match wins within the layer).
	Rules []Rule
	// Tools are per-tool verdicts with no pattern.
	Tools []Rule
	// Default, when non-empty, is the layer's catch-all.
	Default Action
}

// Empty reports whether the layer contributes nothing.
func (l Layer) Empty() bool { return len(l.Rules) == 0 && len(l.Tools) == 0 && l.Default == "" }

// Sources are the permission inputs for one process, most specific first.
type Sources struct {
	// Flags are per-invocation overrides (--allow-tool/--deny-tool/--ask-tool).
	Flags []Rule
	// Profile is profiles.<name>.config.permissions.
	Profile Layer
	// Root is the top-level `permissions:` block.
	Root Layer
}

// Resolve flattens the sources into a single ordered ruleset, most specific
// first. The layering is:
//
//  1. flag rules, sorted most-specific-first
//  2. profile config `rules:` (author order), then profile `tools:`
//  3. root config `rules:` (author order), then root `tools:`
//  4. built-in credential-path denies
//  5. profile `default:`, then root `default:` catch-alls
//  6. built-in per-tool defaults, then the built-in `ask` catch-all
//
// Step 4 sits above step 5 on purpose: a broad `default: allow` in config must
// not silently strip the protection on `.env` / `~/.ssh` / `*.pem`. Turning
// those off requires writing an explicit rule that names the tool or the path,
// which is visible in `tag permissions show`.
func Resolve(s Sources) []Rule {
	flags := append([]Rule{}, s.Flags...)
	SortBySpecificity(flags)

	out := make([]Rule, 0, len(flags)+32)
	out = append(out, flags...)
	out = append(out, s.Profile.Rules...)
	out = append(out, sortedTools(s.Profile.Tools)...)
	out = append(out, s.Root.Rules...)
	out = append(out, sortedTools(s.Root.Tools)...)
	out = append(out, CredentialRules()...)
	if s.Profile.Default != "" {
		out = append(out, Rule{Tool: "*", Action: s.Profile.Default, Source: "config:profile:default"})
	}
	if s.Root.Default != "" {
		out = append(out, Rule{Tool: "*", Action: s.Root.Default, Source: "config:default"})
	}
	out = append(out, BuiltinDefaults()...)
	return out
}

// sortedTools puts concrete tool names ahead of a "*" entry and is otherwise
// deterministic (a YAML map has no order, so we impose one).
func sortedTools(in []Rule) []Rule {
	out := append([]Rule{}, in...)
	sort.SliceStable(out, func(i, j int) bool {
		wi, wj := out[i].Tool == "*", out[j].Tool == "*"
		if wi != wj {
			return wj
		}
		return out[i].Tool < out[j].Tool
	})
	return out
}

// ParseSpec parses a CLI rule spec into a Rule for the given action.
// Accepted forms:
//
//	bash                 -> tool bash, any argument
//	write_file:*.md      -> tool write_file, path glob *.md
//	bash:git *           -> tool bash, command prefix "git "
//	*:*.pem              -> any path tool, glob *.pem
//
// The Kind is inferred from the tool: `bash` takes command patterns, the file
// tools take path globs. For "*" the Kind is left open, which means a pattern
// is tested with the dialect of whichever tool is being checked.
func ParseSpec(spec string, action Action) (Rule, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Rule{}, fmt.Errorf("empty permission spec (want TOOL or TOOL:PATTERN)")
	}
	tool, pattern := spec, ""
	if i := strings.IndexByte(spec, ':'); i >= 0 {
		tool = strings.TrimSpace(spec[:i])
		pattern = strings.TrimSpace(spec[i+1:])
	}
	if tool == "" {
		return Rule{}, fmt.Errorf("permission spec %q has an empty tool name", spec)
	}
	return Rule{Tool: tool, Kind: KindFor(tool), Pattern: pattern, Action: action, Source: "flag"}, nil
}

// KindFor returns the subject kind of a known built-in tool, or KindNone for
// anything else (which leaves a rule's dialect open).
func KindFor(tool string) Kind {
	switch tool {
	case ToolBash:
		return KindCommand
	case ToolReadFile, ToolWriteFile, ToolListDir:
		return KindPath
	default:
		return KindNone
	}
}
