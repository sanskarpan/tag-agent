package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// usageErr marks an error as a usage/validation failure so that isUsageError
// classifies it as exit code 2 (parity with Python argparse), regardless of its
// message. Commands that reject a bad flag VALUE (e.g. `budget set
// --max-tokens -5`, which parses as a valid int but is semantically invalid)
// can wrap the failure with usageErrorf so it is treated as a usage error (#537a)
// rather than a generic runtime failure (exit 1). Adoption is per-command; this
// is the shared mechanism.
type usageErr struct{ err error }

func (u usageErr) Error() string { return u.err.Error() }
func (u usageErr) Unwrap() error { return u.err }

// usageErrorf builds a usage-classified error (exit 2).
func usageErrorf(format string, a ...any) error {
	return usageErr{fmt.Errorf(format, a...)}
}

// friendlyDBError rewrites a raw SQLite "unable to open database file" failure
// (SQLITE_CANTOPEN, "(14)"), typically caused by a read-only TAG_HOME, into an
// actionable message. Other errors pass through unchanged (#537d).
func friendlyDBError(err error) error {
	if err == nil {
		return nil
	}
	m := err.Error()
	if strings.Contains(m, "unable to open database file") || strings.Contains(m, "(14)") {
		return fmt.Errorf("cannot open TAG database — check that TAG_HOME is writable (%v)", err)
	}
	return err
}

// asMap coerces v to map[string]any (nil-safe, returns empty map on mismatch).
func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// asSlice coerces v to []any (nil-safe).
func asSlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

// str coerces v to string ("" on mismatch/nil).
func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// strOr returns s if non-empty, otherwise def.
func strOr(s, def string) string {
	if s != "" {
		return s
	}
	return def
}

// sortedKeys returns the map keys in sorted order.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// childMap returns m[key] as a map, creating and storing an empty map if absent
// (mirrors Python's dict.setdefault chain used in set-model).
func childMap(m map[string]any, key string) map[string]any {
	if existing, ok := m[key].(map[string]any); ok {
		return existing
	}
	child := map[string]any{}
	m[key] = child
	return child
}

// emitJSON prints obj as a JSON line (used by the flagJSON branches).
func emitJSON(obj any) error {
	b, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// validProfileName rejects profile names that could escape the profiles dir
// (path traversal / absolute paths / separators). Reuses profileNameRe from
// template.go. Applied wherever a profile name becomes a filesystem path.
func validProfileName(name string) error {
	if !profileNameRe.MatchString(name) {
		return fmt.Errorf("invalid profile name %q (use letters, digits, dot, dash, underscore; no path separators)", name)
	}
	return nil
}
