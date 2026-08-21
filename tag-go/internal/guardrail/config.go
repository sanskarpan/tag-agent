package guardrail

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// ParseLayer reads a `tripwire:` block out of a decoded YAML map.
//
//	tripwire:
//	  preset: standard          # optional: standard | observe
//	  rules:
//	    - name: no-prod-writes
//	      stage: tool_input     # tool_input | tool_output | model_output | user_input
//	      tool: write_file      # glob over tool names; default "*"
//	      pattern: "/etc/|/var/lib/"
//	      action: block         # block | warn
//	      message: "writes outside the workspace are policy-blocked"
//	    - name: http-flood
//	      type: tripwire
//	      tool: "http_*"
//	      threshold: 10
//	      window: 1h
//	      action: block
//
// A malformed entry is a HARD ERROR. This mirrors permission.ParseLayer for the
// same reason: a guardrail block that silently half-loads leaves the operator
// believing they are protected when they are not, which is worse than refusing
// to start.
func ParseLayer(block map[string]any, source string) ([]Rule, error) {
	if block == nil {
		return nil, nil
	}
	var out []Rule

	if v, ok := block["preset"]; ok {
		name, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s: tripwire.preset must be a string", source)
		}
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" && name != "none" {
			preset, found := BuiltinPresets()[name]
			if !found {
				return nil, fmt.Errorf("%s: unknown tripwire.preset %q (want one of %v)",
					source, name, PresetNames())
			}
			for _, r := range preset {
				r.Source = source + ":preset:" + name
				out = append(out, r)
			}
		}
	}

	if v, ok := block["rules"]; ok {
		list, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("%s: tripwire.rules must be a list", source)
		}
		for i, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s: tripwire.rules[%d] must be a mapping", source, i)
			}
			r, err := parseRule(m, fmt.Sprintf("%s: tripwire.rules[%d]", source, i))
			if err != nil {
				return nil, err
			}
			r.Source = source + ":rules"
			out = append(out, r)
		}
	}
	return Compile(out)
}

func parseRule(m map[string]any, where string) (Rule, error) {
	var r Rule
	r.Name, _ = m["name"].(string)
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		return r, fmt.Errorf("%s: name is required (it identifies the rule in findings and counters)", where)
	}

	r.Type = TypePattern
	if v, ok := m["type"]; ok {
		s, _ := v.(string)
		switch Type(strings.ToLower(strings.TrimSpace(s))) {
		case TypePattern, "":
			r.Type = TypePattern
		case TypeTripwire:
			r.Type = TypeTripwire
		case TypeRequireApproval:
			r.Type = TypeRequireApproval
		default:
			return r, fmt.Errorf("%s: type must be 'pattern', 'tripwire', or 'require-approval', got %q", where, s)
		}
	}

	if v, ok := m["stage"]; ok {
		s, _ := v.(string)
		if strings.TrimSpace(s) != "" {
			st, err := ParseStage(s)
			if err != nil {
				return r, fmt.Errorf("%s: %w", where, err)
			}
			r.Stage = st
		}
	}

	r.Tool, _ = m["tool"].(string)
	r.Tool = strings.TrimSpace(r.Tool)
	if r.Tool == "" {
		r.Tool = "*"
	}

	r.Builtin, _ = m["builtin"].(string)
	r.Builtin = strings.TrimSpace(r.Builtin)
	if r.Builtin != "" {
		if err := ValidateBuiltin(r.Builtin); err != nil {
			return r, fmt.Errorf("%s: %w", where, err)
		}
	}
	r.Pattern, _ = m["pattern"].(string)
	if r.Builtin != "" && strings.TrimSpace(r.Pattern) != "" {
		return r, fmt.Errorf("%s: set either 'builtin' or 'pattern', not both", where)
	}

	if v, ok := m["threshold"]; ok {
		n, err := asInt(v)
		if err != nil {
			return r, fmt.Errorf("%s: threshold must be an integer: %w", where, err)
		}
		r.Threshold = n
	}
	if v, ok := m["window"]; ok {
		s, _ := v.(string)
		d, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil {
			return r, fmt.Errorf("%s: window must be a duration like 30m or 1h: %w", where, err)
		}
		r.Window = d
	}

	act, _ := m["action"].(string)
	a, err := ParseAction(act)
	if err != nil {
		return r, fmt.Errorf("%s: %w", where, err)
	}
	r.Action = a
	// A require-approval rule exists to pause for a human; interrupt is its
	// natural default when the operator did not spell an action out.
	if r.Type == TypeRequireApproval && strings.TrimSpace(act) == "" {
		r.Action = ActionInterrupt
	}
	r.Message, _ = m["message"].(string)
	return r, nil
}

func asInt(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		return int(n), nil
	}
	return 0, fmt.Errorf("got %T", v)
}

// Compile validates and pre-compiles a ruleset. Every failure mode here is a
// startup error, never a rule that quietly matches nothing.
func Compile(rules []Rule) ([]Rule, error) {
	out := make([]Rule, 0, len(rules))
	seen := map[string]bool{}
	for _, r := range rules {
		if r.Name == "" {
			return nil, fmt.Errorf("guardrail rule with no name")
		}
		if seen[r.Name] {
			// Names key the tripwire counters, so duplicates would share a counter
			// and quietly change each other's thresholds.
			return nil, fmt.Errorf("duplicate guardrail rule name %q", r.Name)
		}
		seen[r.Name] = true

		if r.Tool == "" {
			r.Tool = "*"
		}
		if _, err := path.Match(r.Tool, "probe"); err != nil {
			return nil, fmt.Errorf("guardrail rule %q: invalid tool glob %q: %w", r.Name, r.Tool, err)
		}
		r.toolPat = r.Tool

		if r.Type == "" {
			r.Type = TypePattern
		}
		if r.Action == "" {
			// require-approval defaults to interrupt (its whole point is a human
			// gate); every other rule defaults to the safe, halting block.
			if r.Type == TypeRequireApproval {
				r.Action = ActionInterrupt
			} else {
				r.Action = ActionBlock
			}
		}
		if r.Builtin != "" {
			if err := ValidateBuiltin(r.Builtin); err != nil {
				return nil, fmt.Errorf("guardrail rule %q: %w", r.Name, err)
			}
		}
		if p := strings.TrimSpace(r.Pattern); p != "" {
			// Case-insensitive by default: a policy that misses "RM -RF /" because
			// of case is not a policy.
			expr := p
			if !strings.HasPrefix(expr, "(?") {
				expr = "(?i)" + expr
			}
			re, err := regexp.Compile(expr)
			if err != nil {
				return nil, fmt.Errorf("guardrail rule %q: invalid pattern %q: %w", r.Name, r.Pattern, err)
			}
			r.re = re
		}
		switch r.Type {
		case TypePattern:
			if r.re == nil && r.Builtin == "" {
				return nil, fmt.Errorf("guardrail rule %q: a pattern rule needs 'pattern' or 'builtin'", r.Name)
			}
		case TypeTripwire:
			if r.Threshold <= 0 {
				return nil, fmt.Errorf("guardrail rule %q: a tripwire needs a positive 'threshold'", r.Name)
			}
		case TypeRequireApproval:
			// It matches every invocation of the tool, so a content matcher or a
			// threshold would be silently ignored — refuse rather than mislead.
			if r.re != nil || r.Builtin != "" {
				return nil, fmt.Errorf("guardrail rule %q: a require-approval rule matches every invocation and takes no 'pattern'/'builtin'", r.Name)
			}
			if r.Threshold != 0 {
				return nil, fmt.Errorf("guardrail rule %q: a require-approval rule takes no 'threshold'", r.Name)
			}
		}
		out = append(out, r)
	}
	return out, nil
}
