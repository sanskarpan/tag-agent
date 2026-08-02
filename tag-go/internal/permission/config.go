package permission

import (
	"fmt"
	"sort"
	"strings"
)

// ParseLayer reads a `permissions:` block out of a decoded YAML map.
//
//	permissions:
//	  default: ask            # optional catch-all
//	  auto_approve: false     # optional; same effect as --auto-approve
//	  tools:
//	    read_file: allow
//	    bash: deny
//	  rules:                  # ordered; first match wins
//	    - tool: bash
//	      pattern: "git *"
//	      action: allow
//	    - tool: write_file
//	      pattern: "*.env.example"
//	      action: allow
//
// A malformed entry is a hard error: a permission block that silently
// half-loads is worse than one that refuses to start. That applies to a field of
// the wrong TYPE as much as to an unknown action word — the zero value of a
// dropped `tool` or `pattern` is the permissive one, so tolerating it would turn
// a typo into a broader grant than the operator wrote.
func ParseLayer(block map[string]any, source string) (Layer, bool, error) {
	var l Layer
	autoApprove := false
	if block == nil {
		return l, false, nil
	}
	if v, ok := block["default"]; ok {
		s, ok := v.(string)
		if !ok {
			return l, false, fmt.Errorf("%s: permissions.default must be a string", source)
		}
		a, err := ParseAction(s)
		if err != nil {
			return l, false, fmt.Errorf("%s: permissions.default: %w", source, err)
		}
		l.Default = a
	}
	if v, ok := block["auto_approve"]; ok {
		b, ok := v.(bool)
		if !ok {
			return l, false, fmt.Errorf("%s: permissions.auto_approve must be true or false", source)
		}
		autoApprove = b
	}
	if v, ok := block["tools"]; ok {
		m, ok := v.(map[string]any)
		if !ok {
			return l, false, fmt.Errorf("%s: permissions.tools must be a mapping of tool -> allow|ask|deny", source)
		}
		names := make([]string, 0, len(m))
		for k := range m {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, name := range names {
			s, ok := m[name].(string)
			if !ok {
				return l, false, fmt.Errorf("%s: permissions.tools.%s must be a string", source, name)
			}
			a, err := ParseAction(s)
			if err != nil {
				return l, false, fmt.Errorf("%s: permissions.tools.%s: %w", source, name, err)
			}
			l.Tools = append(l.Tools, Rule{Tool: name, Kind: KindFor(name), Action: a, Source: source + ":tools"})
		}
	}
	if v, ok := block["rules"]; ok {
		list, ok := v.([]any)
		if !ok {
			return l, false, fmt.Errorf("%s: permissions.rules must be a list", source)
		}
		for i, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				return l, false, fmt.Errorf("%s: permissions.rules[%d] must be a mapping", source, i)
			}
			// Every field is TYPE-CHECKED before it is used. A comma-ok assertion
			// whose failure is discarded would hand back the zero value, and the
			// zero values here are the PERMISSIVE ones: an empty Tool becomes "*"
			// and an empty Pattern matches ANY subject (see Rule.matches). So
			// `pattern: 42` — an unquoted scalar, the easiest typo in the file —
			// would quietly turn `write_file: "*.md" = allow` into a blanket
			// `write_file: * = allow`. Widening a grant on a typo is exactly the
			// silent half-load this function refuses to do everywhere else.
			tool := "*"
			if v, ok := m["tool"]; ok && v != nil {
				s, ok := v.(string)
				if !ok {
					return l, false, fmt.Errorf(
						"%s: permissions.rules[%d].tool must be a string (a tool name or \"*\"), got %T; "+
							"refusing to load rather than widen the rule to every tool", source, i, v)
				}
				if strings.TrimSpace(s) != "" {
					tool = s
				}
			}
			pattern := ""
			if v, ok := m["pattern"]; ok && v != nil {
				s, ok := v.(string)
				if !ok {
					return l, false, fmt.Errorf(
						"%s: permissions.rules[%d].pattern must be a string, got %T; refusing to load rather "+
							"than widen the rule to every subject (quote it, e.g. pattern: \"*.md\")", source, i, v)
				}
				pattern = s
			}
			actStr := ""
			if v, ok := m["action"]; ok && v != nil {
				s, ok := v.(string)
				if !ok {
					return l, false, fmt.Errorf(
						"%s: permissions.rules[%d].action must be a string (allow, ask or deny), got %T",
						source, i, v)
				}
				actStr = s
			}
			a, err := ParseAction(actStr)
			if err != nil {
				return l, false, fmt.Errorf("%s: permissions.rules[%d].action: %w", source, i, err)
			}
			kind := KindFor(tool)
			if v, ok := m["kind"]; ok && v != nil {
				k, ok := v.(string)
				if !ok {
					return l, false, fmt.Errorf(
						"%s: permissions.rules[%d].kind must be a string (path or command), got %T",
						source, i, v)
				}
				if strings.TrimSpace(k) != "" {
					switch Kind(strings.ToLower(strings.TrimSpace(k))) {
					case KindPath:
						kind = KindPath
					case KindCommand:
						kind = KindCommand
					default:
						return l, false, fmt.Errorf("%s: permissions.rules[%d].kind must be path or command", source, i)
					}
				}
			}
			l.Rules = append(l.Rules, Rule{
				Tool: tool, Kind: kind, Pattern: strings.TrimSpace(pattern),
				Action: a, Source: source + ":rules",
			})
		}
	}
	return l, autoApprove, nil
}
