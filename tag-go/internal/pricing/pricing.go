// Package pricing owns the embedded per-1M-token USD price table and the model
// -id resolution rules used for cost attribution.
//
// It used to live inside internal/cli, which made it unreachable from the agent
// loop: internal/agent and internal/worker cannot import internal/cli (cli
// imports them). That is why Go spans were written without a cost — see #590.
// The table is unchanged; only its home moved, so `tag pricing`/`tag costs`
// report exactly the same numbers.
package pricing

import "strings"

// Table is the embedded per-1M-token USD cost (gen_ai cost attribution).
// Kept in sync with src/tag/assets/pricing.yaml — in particular it must cover the
// models TAG ships in its own default config (src/tag/config/default.yaml), which
// previously all reported "model not found".
// Price is one row of the embedded pricing table. Source/Estimated carry
// provenance so a rate that TAG cannot corroborate against a published price is
// never presented as authoritative.
type Price struct {
	In, Out   float64 // $/1M tokens
	Source    string  // provenance, e.g. "models.dev"; "" if unknown
	Estimated bool    // true = NOT an authoritative published rate
}

const srcModelsDev = "models.dev"

// srcPricingYAML marks rates that match src/tag/assets/pricing.yaml exactly and
// are corroborated by models.dev.
const srcPricingYAML = "models.dev (matches src/tag/assets/pricing.yaml)"

// Table is the embedded per-1M-token USD price table, keyed by model id.
var Table = map[string]Price{
	"openai/gpt-4o":      {In: 2.5, Out: 10.0, Source: srcPricingYAML},
	"openai/gpt-4o-mini": {In: 0.15, Out: 0.6, Source: srcPricingYAML},
	"gpt-4o":             {In: 2.5, Out: 10.0, Source: srcPricingYAML},
	"gpt-4o-mini":        {In: 0.15, Out: 0.6, Source: srcPricingYAML},
	// TAG's own default master/orchestrator model. It was missing here and in
	// src/tag/assets/pricing.yaml, so the shipped default profile priced at $0.
	// The value first added here — 1.25/10.00 — was UNSOURCED: it was GPT-5's
	// rate copied onto GPT-5.4. The corroborated GPT-5.4 rate (models.dev plus
	// several independent public pricing aggregators, 2026-07) is 2.50/15.00.
	"openai/gpt-5.4": {In: 2.50, Out: 15.00,
		Source: "models.dev (corroborated by multiple public pricing aggregators, 2026-07)"},
	"anthropic/claude-opus-4-8":   {In: 5.0, Out: 25.0, Source: srcPricingYAML},
	"anthropic/claude-sonnet-4-6": {In: 3.0, Out: 15.0, Source: srcPricingYAML},
	"anthropic/claude-haiku-4-5":  {In: 1.0, Out: 5.0, Source: srcPricingYAML},
	// The two engines previously disagreed on Gemini 2.5 Pro output: this table
	// said 5.00 while src/tag/assets/pricing.yaml said 10.00. models.dev confirms
	// 10.00, so the Go side was the wrong one.
	"google/gemini-2.5-pro": {In: 1.25, Out: 10.0, Source: srcModelsDev},
	// No authoritative resolution: this table previously said 0.075/0.30,
	// pricing.yaml says 0.15/0.60 and models.dev says 0.30/2.50. The rate stays
	// unverified, but the two engines must not ship *different* numbers for the
	// same model, so this mirrors the canonical pricing.yaml value and is
	// flagged estimated rather than silently trusted.
	"google/gemini-2.5-flash": {In: 0.15, Out: 0.6, Estimated: true,
		Source: "unverified — pricing.yaml value; conflicts with models.dev (0.30/2.50)"},
	// Models TAG ships as profile defaults.
	"deepseek/deepseek-v4-flash": {In: 0.14, Out: 0.28, Source: srcPricingYAML},
	// models.dev lists 1.74/3.48 for this model; the repo says 0.27/1.10. The
	// repo value is kept (changing it would silently restate every past cost)
	// but marked estimated.
	"deepseek/deepseek-v4-pro": {In: 0.27, Out: 1.10, Estimated: true,
		Source: "unverified — conflicts with models.dev"},
	"deepseek/deepseek-r1": {In: 0.55, Out: 2.19, Source: srcPricingYAML},
	// Not present in models.dev at all; no public rate could be found.
	"qwen/qwen3-coder": {In: 0.50, Out: 2.00, Estimated: true,
		Source: "unverified — no public rate found"},
	"qwen/qwen-plus": {In: 0.40, Out: 1.20, Estimated: true,
		Source: "unverified — no public rate found"},
}

// knownProviderPrefixes are the vendor namespaces used in Table keys. A
// bare model alias (e.g. "claude-sonnet-4-6") is the form users type and the form
// run.go treats as distinct from the prefixed id, so resolve it by trying each
// prefix rather than reporting "model not found".
var knownProviderPrefixes = []string{"anthropic/", "openai/", "google/", "deepseek/", "qwen/"}

// Lookup resolves a model id to its price row (including provenance),
// accepting either the fully prefixed id or the bare alias.
func Lookup(model string) (Price, bool) {
	if p, ok := Table[model]; ok {
		return p, true
	}
	if !strings.Contains(model, "/") {
		for _, prefix := range knownProviderPrefixes {
			if p, ok := Table[prefix+model]; ok {
				return p, true
			}
		}
		return Price{}, false
	}
	// A vendor namespace we do not carry in the table (TAG ships runtime-flavoured
	// ids such as "openai-codex/gpt-5.4"): retry on the bare alias, then on that
	// alias under each known prefix, so the model is priced rather than skipped.
	bare := model[strings.LastIndex(model, "/")+1:]
	if p, ok := Table[bare]; ok {
		return p, true
	}
	for _, prefix := range knownProviderPrefixes {
		if p, ok := Table[prefix+bare]; ok {
			return p, true
		}
	}
	return Price{}, false
}

// CostUSD prices a prompt/completion token pair for a model id. ok is false when
// the model is not in Table, in which case the caller must record "no cost known"
// (a NULL cost_usd) rather than a misleading $0.
func CostUSD(model string, promptTokens, completionTokens int) (cost float64, ok bool) {
	p, found := Lookup(model)
	if !found {
		return 0, false
	}
	return float64(promptTokens)/1e6*p.In + float64(completionTokens)/1e6*p.Out, true
}
