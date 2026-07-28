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
// half-loads is worse than one that refuses to start.
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
			tool, _ := m["tool"].(string)
			if strings.TrimSpace(tool) == "" {
				tool = "*"
			}
			pattern, _ := m["pattern"].(string)
			actStr, _ := m["action"].(string)
			a, err := ParseAction(actStr)
			if err != nil {
				return l, false, fmt.Errorf("%s: permissions.rules[%d].action: %w", source, i, err)
			}
			kind := KindFor(tool)
			if k, ok := m["kind"].(string); ok && strings.TrimSpace(k) != "" {
				switch Kind(strings.ToLower(strings.TrimSpace(k))) {
				case KindPath:
					kind = KindPath
				case KindCommand:
					kind = KindCommand
				default:
					return l, false, fmt.Errorf("%s: permissions.rules[%d].kind must be path or command", source, i)
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
