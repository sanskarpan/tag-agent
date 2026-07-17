// Package contextwin ports the pure, offline pieces of src/tag/context.py
// (PRD-018: Context Window Management). The live session-usage lookups in the
// Python module shell out to the hermes runtime (`hermes prompt-size`,
// `hermes sessions ...`); those are out of scope for the offline Go port. What
// lives here is the runtime-independent math: the default window size and the
// used/max -> percentage + colour-band helpers, so the `context` command can
// report a profile's context budget without any subprocess.
package contextwin

import "math"

// DefaultMaxTokens mirrors context.py:DEFAULT_MAX_TOKENS.
const DefaultMaxTokens = 128_000

// Usage is the read-only shape reported by `context show` (offline).
// It matches context.py:get_context_size's dict keys plus the profile it
// was computed for.
type Usage struct {
	Profile    string  `json:"profile"`
	UsedTokens int     `json:"used_tokens"`
	MaxTokens  int     `json:"max_tokens"`
	Pct        float64 `json:"pct"`
}

// Pct ports context.py's `(used / max * 100)` rounded to two decimals, with a
// zero guard for a non-positive window.
func Pct(used, max int) float64 {
	if max <= 0 {
		return 0.0
	}
	pct := float64(used) / float64(max) * 100.0
	return math.Round(pct*100) / 100
}

// BarColor ports context.py:format_context_bar's threshold->colour rule:
// green < 50%, yellow 50-80%, red > 80%.
func BarColor(used, max int) string {
	pct := 0.0
	if max > 0 {
		pct = float64(used) / float64(max) * 100.0
	}
	switch {
	case pct < 50:
		return "green"
	case pct <= 80:
		return "yellow"
	default:
		return "red"
	}
}
