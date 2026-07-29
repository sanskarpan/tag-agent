package cli

import (
	"math"
	"testing"
)

// Issue #578 finding 5 — the embedded pricing table carried only 7
// provider-prefixed entries and missed every model TAG ships in its own default
// config, so `tag pricing get` reported `model not found` for the bare aliases
// users actually type and for the deepseek/qwen defaults.

func costOf(t *testing.T, model string, in, out int64) float64 {
	t.Helper()
	p, ok := lookupPrice(model)
	if !ok {
		t.Fatalf("model not found: %q", model)
	}
	return float64(in)/1e6*p.In + float64(out)/1e6*p.Out
}

func TestLookupPriceResolvesBareAliases(t *testing.T) {
	// Pre-fix these all failed with `model not found` because only the
	// provider-prefixed key existed in the table.
	cases := []struct {
		model    string
		wantCost float64 // for 1M input + 1M output
	}{
		{"claude-opus-4-8", 30.0},
		{"claude-sonnet-4-6", 18.0},
		{"claude-haiku-4-5", 6.0},
		{"gpt-4o", 12.5},
	}
	for _, tc := range cases {
		got := costOf(t, tc.model, 1_000_000, 1_000_000)
		if math.Abs(got-tc.wantCost) > 1e-9 {
			t.Errorf("%s: cost = %v, want %v", tc.model, got, tc.wantCost)
		}
	}
}

func TestLookupPricePrefixedAndBareAgree(t *testing.T) {
	for _, pair := range [][2]string{
		{"claude-opus-4-8", "anthropic/claude-opus-4-8"},
		{"claude-sonnet-4-6", "anthropic/claude-sonnet-4-6"},
		{"claude-haiku-4-5", "anthropic/claude-haiku-4-5"},
	} {
		bare, ok := lookupPrice(pair[0])
		if !ok {
			t.Fatalf("bare alias not found: %q", pair[0])
		}
		prefixed, ok := lookupPrice(pair[1])
		if !ok {
			t.Fatalf("prefixed id not found: %q", pair[1])
		}
		if bare != prefixed {
			t.Errorf("%s=%v but %s=%v", pair[0], bare, pair[1], prefixed)
		}
	}
}

func TestPricingCoversShippedDefaultModels(t *testing.T) {
	// These are defaults in src/tag/config/default.yaml; pre-fix every one of
	// them reported `model not found`.
	for _, m := range []string{
		"deepseek/deepseek-v4-flash",
		"deepseek/deepseek-v4-pro",
		"qwen/qwen3-coder",
	} {
		if _, ok := lookupPrice(m); !ok {
			t.Errorf("shipped default model %q missing from pricing table", m)
		}
	}
}

func TestAnthropicPricesMatchPublishedRates(t *testing.T) {
	// Guards against the Python-side error where Opus 4.8 was listed at 15/75
	// (a 3x overcharge) and Haiku 4.5 at 0.80/4.00.
	want := map[string][2]float64{
		"claude-opus-4-8":   {5.0, 25.0},
		"claude-sonnet-4-6": {3.0, 15.0},
		"claude-haiku-4-5":  {1.0, 5.0},
	}
	for model, exp := range want {
		got, ok := lookupPrice(model)
		if !ok {
			t.Fatalf("model not found: %q", model)
		}
		if got.In != exp[0] || got.Out != exp[1] {
			t.Errorf("%s: got %v/%v, want %v", model, got.In, got.Out, exp)
		}
		if got.Estimated {
			t.Errorf("%s: published Anthropic rate must not be flagged estimated", model)
		}
	}
}

// TestPricingCoversGPT54 pins the one shipped default the earlier "cover shipped
// models" pass missed: `gpt-5.4` is the master/orchestrator default in
// src/tag/config/default.yaml, yet `tag pricing get --model gpt-5.4` returned
// `model not found: "gpt-5.4"` and every cost rollup for the default profile
// silently priced it at $0.
// The rate is 2.50/15.00 (models.dev, corroborated 2026-07); the earlier 11.25
// expectation encoded an unsourced 1.25/10.00 copied from GPT-5.
func TestPricingCoversGPT54(t *testing.T) {
	got := costOf(t, "gpt-5.4", 1_000_000, 1_000_000)
	if math.Abs(got-17.50) > 1e-9 {
		t.Errorf("gpt-5.4: cost = %v, want 17.50", got)
	}
}

// TestLookupPriceResolvesUnknownVendorPrefix covers the runtime-flavoured ids
// TAG actually writes (README/config use "openai-codex/gpt-5.4"). "openai-codex/"
// is not a key prefix in the table, so the id fell straight through to
// `model not found` and the span contributed $0 to `tag costs`.
func TestLookupPriceResolvesUnknownVendorPrefix(t *testing.T) {
	for _, m := range []string{"openai-codex/gpt-5.4", "openai/gpt-5.4", "gpt-5.4"} {
		p, ok := lookupPrice(m)
		if !ok {
			t.Fatalf("model not found: %q", m)
		}
		if p.In != 2.50 || p.Out != 15.00 {
			t.Errorf("%s: got %v/%v, want 2.50/15.00", m, p.In, p.Out)
		}
	}
	// The fallback must not turn genuinely unknown models into a price.
	if _, ok := lookupPrice("somevendor/not-a-real-model-xyz"); ok {
		t.Error("unknown prefixed model unexpectedly resolved")
	}
}

func TestLookupPriceUnknownModelStillFails(t *testing.T) {
	// A model with no authoritative published price must NOT be silently
	// invented; it stays a lookup failure.
	if _, ok := lookupPrice("definitely-not-a-real-model-xyz"); ok {
		t.Error("unknown model unexpectedly resolved")
	}
}
