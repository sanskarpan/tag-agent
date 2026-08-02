package gateway

import (
	"testing"
)

// TestMaxTokensIsClampedNotJustDefaulted pins the CWE-770 fix: opts.MaxTokens is
// a ceiling, not a fallback. Before the fix a client-supplied max_tokens was
// forwarded verbatim however large, so the configured limit bounded only the
// requests that omitted the field — the ones that were not trying to exceed it.
//
// clampMaxTokens mirrors the handler's arithmetic exactly; the handler is
// exercised end-to-end by the HTTP tests, this pins the rule itself.
func TestMaxTokensIsClampedNotJustDefaulted(t *testing.T) {
	cases := []struct {
		name       string
		requested  int
		configured int
		want       int
	}{
		{"omitted falls back to the configured cap", 0, 1024, 1024},
		{"negative falls back to the configured cap", -1, 1024, 1024},
		{"under the cap is honoured", 256, 1024, 256},
		{"exactly the cap is honoured", 1024, 1024, 1024},
		{"over the cap is CLAMPED", 100000, 1024, 1024},
		{"absurd value is CLAMPED", 1 << 30, 1024, 1024},
		{"no cap configured leaves the request alone", 100000, 0, 100000},
		{"no cap and none requested stays zero", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampMaxTokens(tc.requested, tc.configured); got != tc.want {
				t.Fatalf("clampMaxTokens(requested=%d, configured=%d) = %d, want %d",
					tc.requested, tc.configured, got, tc.want)
			}
		})
	}
}
