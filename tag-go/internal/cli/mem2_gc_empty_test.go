package cli_test

import (
	"strings"
	"testing"
)

// TestE2EMem2GCAllProfilesEmptyEmitsArray guards the empty-result JSON contract
// (#559-568): `mem2 gc --all-profiles --json` on a DB with no memories printed
// the bare literal `null`, which a --json consumer cannot iterate.
func TestE2EMem2GCAllProfilesEmptyEmitsArray(t *testing.T) {
	h := newHome(t)
	out, code := run(t, h, "--json", "mem2", "gc", "--all-profiles")
	if code != 0 {
		t.Fatalf("mem2 gc --all-profiles: exit %d: %s", code, out)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("empty --all-profiles GC = %q, want %q", strings.TrimSpace(out), "[]")
	}
}
