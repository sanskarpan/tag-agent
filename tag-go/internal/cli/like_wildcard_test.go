package cli_test

import (
	"strings"
	"testing"
)

// TestE2EIDPrefixResolutionEscapesLikeWildcards is the regression guard for
// unescaped LIKE wildcards in id-prefix resolution. `... WHERE id LIKE ?||'%'`
// bound the RAW user string, so a --session-id of "%" or "_" matched an
// arbitrary real record: `context compress --session-id '%'` reported success
// and WROTE a context_compressions row for a session the user never named.
// Python escapes these (semantic_memory.py:150).
func TestE2EIDPrefixResolutionEscapesLikeWildcards(t *testing.T) {
	h := newHome(t)
	if out, code := run(t, h, "run", "hello world"); code != 0 {
		t.Fatalf("seed run: %s", out)
	}
	if out, code := run(t, h, "split", "plan", "task x"); code != 0 {
		t.Fatalf("seed split: %s", out)
	}

	cases := []struct {
		name string
		args []string
	}{
		{"context compress %", []string{"context", "compress", "--session-id", "%"}},
		{"context compress _", []string{"context", "compress", "--session-id", "_"}},
		{"context trim %", []string{"context", "trim", "--session-id", "%"}},
		{"context trim _", []string{"context", "trim", "--session-id", "_"}},
		{"mem2 extract %", []string{"mem2", "extract", "%"}},
		{"mem2 extract _", []string{"mem2", "extract", "_"}},
		{"split show %", []string{"split", "show", "%"}},
		{"runs show %", []string{"runs", "show", "%"}},
	}
	for _, c := range cases {
		out, code := run(t, h, c.args...)
		if code == 0 {
			t.Errorf("%s: wildcard resolved to a real record (exit 0):\n%s", c.name, out)
			continue
		}
		lower := strings.ToLower(out)
		if !strings.Contains(lower, "not found") && !strings.Contains(lower, "no run matching") {
			t.Errorf("%s: expected a not-found error, got: %q", c.name, out)
		}
	}

	// And nothing must have been written on behalf of an unnamed session.
	if out, code := run(t, h, "--json", "context", "history"); code == 0 && strings.Contains(out, "session_id") {
		t.Errorf("wildcard session ids produced context records: %s", out)
	}
}
