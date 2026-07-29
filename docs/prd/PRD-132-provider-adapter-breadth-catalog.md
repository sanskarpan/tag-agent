# PRD-132: Provider Adapter Breadth via One Multi-Provider Adapter + a Model Catalog (`tag providers`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD is Go-native from the start — there is no Python precursor to re-frame.

> **Scope boundary.** This PRD is about **which providers TAG can call at all**. It is not about **choosing between** them. PRD-031 (model fallback chains) walks a declared chain on retryable errors; PRD-107 (confidence-aware routing) picks a model on a cost/accuracy Pareto frontier. Both *route between* providers that already exist. This one *adds the providers*. All three compose: a wider catalog makes fallback chains and Pareto routing better, and neither is modified here.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (1-2 weeks)
**Category:** Provider & Model Infrastructure
**Affects:** `internal/llm` (new `openaicompat.go` parameterized adapter, `catalog.go` embedded catalog; `Registry` population becomes catalog-driven), `internal/cli` (new `providers` command group; `models`, `pricing`, `route-fallback`, `set-model` become catalog-aware), `internal/cli/observability.go` (`pricingTable` is absorbed into the catalog, preserving its `Source`/`Estimated` provenance fields), `internal/importer` (the 18 credential importers become the credential on-ramp for catalog entries), `internal/store` (`provider_probes` table for `tag providers test` results), `internal/config` (per-provider `base_url`/`api_key_env` overrides)
**Depends on:** PRD-012 (cost tracking & budget management — every catalog entry needs a price, with provenance), PRD-017 (multi-model benchmarking & comparison — a wider catalog is what `tag compare` compares), PRD-031 (model fallback chains — chains become expressible across many providers; note the file `PRD-031-model-fallback-chains.md` carries a stale `# PRD-036:` heading), PRD-041 (OTel GenAI span cost attribution — `gen_ai.system` must name the real provider; note `PRD-041-otel-genai-span-cost-attribution.md` carries a stale `# PRD-037:` heading), PRD-046 (per-span USD cost attribution — resolves prices through the catalog), PRD-107 (confidence-aware model routing — routes over the catalog's cost/latency metadata)
**Explicitly NOT duplicating:** PRD-031 and PRD-107 — see the scope box above.
**Inspired by:** Pi's `pi-ai` unified provider layer (30+), Goose (50+), Hermes `plugins/providers/` (32), Crush + Catwalk community registry (25), Factory (39), models.dev

---

## 1. Overview

TAG-Go has **four** provider adapters: `echo`, `anthropic`, `openai`, `local`. The July 2026 competitive audit is direct about the consequence — §5.1 rates provider breadth a genuine gap against Pi's 30+, Goose's 50+, Hermes's 32 and Crush's 25 — and equally direct that the framing usually applied to it is wrong. Two findings from that audit set the shape of this PRD:

**First, the claim is a worse problem than the capability.** The README says TAG "routes tasks across 10+ AI providers". The audit calls this "TAG's most inflated claim" and files correcting it as **Tier 0 work — do it regardless of strategy**. The 18 `import-*` commands are *credential importers*, not adapters. This PRD does not wait for an implementation to fix that sentence; correcting it is Phase 0 and lands first.

**Second, this is a catalog and credential problem, not a protocol problem.** `internal/llm/openai.go` already factors its implementation into `streamOpenAICompatible(ctx, req, baseURL, apiKey, errLabel, client)`. `local.go` is 51 lines and does nothing but supply a different `baseURL`, a different key env var, and a different error label to that same function — its doc comment says so explicitly: *"It reuses the OpenAI body-builder + SSE parser via streamOpenAICompatible, so tool-calling and usage accounting work too."* Groq, Cerebras, OpenRouter, DeepSeek, Together, Fireworks, xAI, Mistral, DeepInfra, Nebius, Hyperbolic, SambaNova, Perplexity and most others are all OpenAI-shaped. **The generalization is already written; what is missing is the table that parameterizes it.**

So the design is: promote `streamOpenAICompatible` into a `Provider` implementation parameterized by a catalog entry, and ship an embedded catalog. Each entry supplies a slug, a base URL, a key environment variable, a display name, quirk flags, and a list of models with prices. Registration becomes a loop over the catalog. Adding a provider becomes adding a table row plus a test, not writing an adapter — which is the difference between a number that grows once and a number that keeps growing.

The catalog is also where an existing honesty problem gets fixed. `internal/cli/observability.go` carries a `pricingTable` of 15 entries whose comments are a model of the project's engineering culture: each rate records a `Source`, rates that could not be corroborated are flagged `Estimated: true` with the conflict spelled out (*"unverified — conflicts with models.dev"*, *"unverified — no public rate found"*). But that table prices `deepseek/deepseek-v4-pro`, `deepseek/deepseek-r1`, `qwen/qwen3-coder`, `qwen/qwen-plus` and `google/gemini-2.5-pro` — **models TAG has no adapter to call.** TAG can tell you what DeepSeek R1 costs and cannot invoke it. Merging the pricing table into the catalog resolves that: an entry either has a reachable endpoint or is marked unreachable, and the provenance fields carry over unchanged rather than being reinvented.

The final asset is the one the audit calls "genuinely unique": **18 credential importers** (aider, aws, claude, codex, continue, copilot, cursor, daytona, docker, gemini, honcho, mistral, modal, nous-portal, opencode, ssh, supermemory, zed), verified working. Today they import keys for providers TAG largely cannot call. Once the catalog exists, they become the on-ramp: a developer who already uses opencode or Cursor runs one `import-*` command and their existing keys light up catalog entries. No peer ships this, and its value is roughly zero until there is something for the credentials to unlock.

---

## 2. Problem Statement

### 2.1 Four adapters is genuinely thin, and the gap is concentrated in one place

`llm.Registry` is populated by four `init()` calls. `echo` is offline-only. That leaves three real adapters against a field norm of 25-50. The practical effect is not that TAG cannot reach these models — `local` can be pointed at any OpenAI-compatible endpoint via `TAG_LOCAL_BASE_URL` — but that doing so is undiscoverable, undocumented, unnamed and un-priced. A user wanting Groq must know that `local` is secretly a general-purpose OpenAI-compatible client, set an environment variable whose name says "local", and accept that traces will report the provider as `local` and costs as unknown. The capability is half-present and entirely unusable as a product.

### 2.2 The documented claim overstates the code, and that is the urgent part

"Routes tasks across 10+ AI providers" is not true of the Go harness. In a repository whose strongest cultural asset — per the audit's own §5.2 — is that "every degraded path *says* it is degraded", an overstated capability claim in the README is a self-inflicted wound. The audit files the correction as Tier 0.2, size XS, *"Currently the most inflated claim in the repo. Contradicts 'no fake success.'"* This PRD treats the doc fix as Phase 0, shipping before any adapter work, because it is correct independent of whether the rest of the PRD is ever built.

### 2.3 The pricing table prices models TAG cannot call

`pricingTable` covers `deepseek/*`, `qwen/*` and `google/*` families. `llm.Registry` has no adapter for any of them. Two subsystems disagree about what TAG can do, and the more optimistic one is the user-facing one. Worse, it is not a bug in either — the pricing table is correct about prices and the registry is correct about adapters; the missing thing is a single source of truth that reconciles them.

### 2.4 The 18 credential importers are an asset that currently unlocks almost nothing

`tag import-opencode` reads `auth.json` and imports API keys. `tag import-cursor` imports BYOK keys. `tag import-zed` reads editor settings. These are a genuinely unique on-ramp — the audit says no peer does this — and they currently import credentials for providers with no adapter behind them. The catalog is what converts a clever import mechanism into a first-run experience.

### 2.5 Adding a provider today means writing an adapter, so the number will not grow

Even though `streamOpenAICompatible` exists, there is no declarative path to a new provider: you add a file, a struct, three methods, an `init()`, a pricing entry in a different package, and documentation. Nobody does that for the eleventh provider. Peers that ship 30-50 do so because they have a table (Crush has Catwalk; Pi has `pi-ai`; models.dev is a shared registry). Without one, TAG's provider count is structurally capped at whatever someone hand-writes.

---

## 3. Goals and Non-Goals

### Goals

| # | Goal |
|---|------|
| G0 | **Correct the claim first.** README and `docs/FEATURES.md` state the accurate number and shape before any adapter work lands. This ships in Phase 0 and is independently valuable. |
| G1 | Promote `streamOpenAICompatible` into `OpenAICompatProvider`, a `Provider` parameterized by a catalog entry — reusing the existing body builder, SSE parser, tool-call linkage, usage accounting and `checkStreamResponse` guard verbatim. |
| G2 | Ship an embedded, versioned catalog of OpenAI-compatible providers (target ≥ 15 at v1: Groq, Cerebras, OpenRouter, DeepSeek, Together, Fireworks, xAI, Mistral, DeepInfra, Nebius, Hyperbolic, SambaNova, Perplexity, Moonshot, Z.ai, plus the existing openai/local). |
| G3 | Absorb `pricingTable` into the catalog **preserving its `Source`/`Estimated` provenance semantics exactly**, so no rate loses its provenance and no unverified rate is silently promoted to authoritative. |
| G4 | Every catalog model is reachable: an entry with a price but no callable endpoint is either given one or explicitly marked unreachable and shown as such. |
| G5 | `tag providers list/show/test/models/catalog` makes providers discoverable, with `--json` throughout. |
| G6 | `tag providers test <slug>` performs a minimal live probe and reports reachability, auth, tool-calling and streaming honestly — including that it costs a few tokens. |
| G7 | Credentials resolve through a documented precedence: config → provider-specific env → `import-*`-populated store, so the 18 importers become the on-ramp. |
| G8 | Per-provider quirks (no `tools`, no streaming, no `usage` in stream, required headers) are declared as catalog flags and handled by one adapter — never by a bespoke code path. |
| G9 | Traces and costs name the real provider: `gen_ai.system` is the catalog slug, not `local` (PRD-041), and price lookup resolves through the catalog (PRD-046). |
| G10 | `tag route-fallback` chains and PRD-107 routing become expressible across all catalog providers with no changes to either feature. |
| G11 | Adding a provider is a catalog row plus a table-test case — documented in `CONTRIBUTING`-level terms so it is a community-sized contribution. |
| G12 | Fully offline-honest: the catalog, `list`, `show`, `models` and `catalog` all work with no network and no keys; only `test` requires either, and it says so. |

### Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Routing, selection or fallback policy. PRD-031 and PRD-107 own those and are unmodified here. See the scope box. |
| NG2 | Non-OpenAI-shaped wire protocols beyond the existing Anthropic adapter. Native Bedrock SigV4, Vertex AI OAuth and Azure's deployment-scoped URLs are §14 open questions, not v1 scope — each is a real protocol, not a base-URL change, and pretending otherwise is how a clean design rots. |
| NG3 | A live remote registry. The catalog is embedded in the binary; a `--refresh` from models.dev is deferred (it would add a network dependency to a startup path and a supply-chain surface). |
| NG4 | OAuth / subscription login flows (Pi's 7 subscription providers, Claude Pro, ChatGPT Plus). API keys only in v1; OAuth is per-provider bespoke work. |
| NG5 | Embeddings, image, audio or rerank endpoints. Chat completions only, matching the current `Provider` interface. |
| NG6 | Guaranteeing every catalog model works. Providers change models weekly. The catalog records what is *declared*; `tag providers test` reports what is *observed*; the two are presented separately and never conflated. |
| NG7 | Local model management (downloading weights, launching llama.cpp). `local` stays a pointer at a running server. |
| NG8 | Porting to the Python edition. Go-native. The Python edition reaches providers through Hermes's own 32 plugins. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Provider count | ≥ 15 OpenAI-compatible providers registered at v1 | `len(llm.Registry)` assertion |
| Code per provider | Median ≤ 12 lines of catalog data and 0 lines of adapter code | Diff review; a test fails if any provider ships a bespoke `Stream` |
| Claim accuracy | README and FEATURES state a number equal to `len(llm.Registry)-1` (excluding `echo`) | Doc test parsing the README against the built binary |
| Price coverage | 100% of catalog models have a price with explicit provenance; unverified rates are flagged | Catalog validation test |
| Reachability integrity | 0 catalog models that are priced but have no callable endpoint and no `unreachable` marker | Catalog validation test |
| Importer coverage | ≥ 12 of the 18 importers populate credentials for at least one catalog provider | Mapping test |
| Regression safety | `openai`, `anthropic`, `local`, `echo` behave byte-identically after the refactor | Golden-transcript replay tests |
| Offline honesty | `list`, `show`, `models`, `catalog` complete with egress blackholed and no keys | Offline CI job |
| Trace fidelity | `gen_ai.system` equals the catalog slug for 100% of catalog-provider runs | Integration test |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag providers list` and see every provider with credential status | I know what I can actually use right now |
| U2 | Developer | run `tag run --provider groq` with `GROQ_API_KEY` set | fast inference works without learning that `local` is secretly general-purpose |
| U3 | Developer | run `tag import-opencode` and have my existing keys light up catalog providers | switching to TAG costs one command |
| U4 | Cost-conscious operator | run `tag providers models --sort price` | I can pick a cheap model with a sourced rate |
| U5 | Skeptical operator | see `estimated` next to an uncorroborated price | I know which numbers to trust |
| U6 | Operator | run `tag providers test deepseek` | I learn whether my key works before a real job depends on it |
| U7 | Operator | run `tag route-fallback` across anthropic → groq → deepseek → local | my fallback chain spans vendors, and `local` is a real last resort |
| U8 | Platform engineer | override a provider's `base_url` in config for a corporate gateway | I route through my proxy without a code change |
| U9 | Contributor | add a provider by appending a catalog row | contributing breadth does not require understanding the SSE parser |
| U10 | Auditor | see the real provider in `tag trace` and `tag costs` | cost attribution is per-vendor, not lumped under `local` |
| U11 | Honest reader | find a README claim that matches the binary | the project's credibility survives a `--help` |
| U12 | Offline user | browse the full catalog with no network | discovery does not require connectivity |

---

## 6. Proposed CLI Surface

**Collision check.** `tag --help` on the built binary lists no `providers` command. The closest existing names are `pricing`, `prompt`, `prompt-size`, `persona`, `plugin` (p-neighbourhood) and, under Routing, `models`, `assignments`, `route`, `route-fallback`, `set-model` — none of which is `providers`. No existing PRD claims `tag providers`. The plural is deliberate: `tag provider` singular reads like a setter and would invite confusion with `--provider`.

### 6.1 `tag providers list`

```bash
tag providers list [--available] [--configured] [--json]
```

```
Slug        Provider          Models  Credential          Status
──────────────────────────────────────────────────────────────────────────
anthropic   Anthropic             3   ANTHROPIC_API_KEY   configured
openai      OpenAI                4   OPENAI_API_KEY      configured
groq        Groq                  5   GROQ_API_KEY        configured (import-opencode)
deepseek    DeepSeek              3   DEEPSEEK_API_KEY    missing
openrouter  OpenRouter          200+  OPENROUTER_API_KEY  configured (import-cursor)
cerebras    Cerebras              3   CEREBRAS_API_KEY    missing
together    Together AI          12   TOGETHER_API_KEY    missing
fireworks   Fireworks AI          8   FIREWORKS_API_KEY   missing
local       Local (OpenAI-compat) —   (none required)     endpoint unreachable
echo        Echo (offline)        1   (none required)     always available

16 providers · 4 configured · 1 offline
credentials resolve: config → env → import-* store   (`tag providers show <slug>` for detail)
```

### 6.2 `tag providers show`

```bash
tag providers show SLUG [--json]
```

```
provider: groq  (Groq)
  base_url        https://api.groq.com/openai/v1        [config override: none]
  wire            openai-compatible
  credential      GROQ_API_KEY — set (from `tag import-opencode`, 2026-07-14)
  quirks          usage_in_stream=false (usage is fetched from the final chunk)
  models
    slug                          ctx     in $/1M  out $/1M  tools  source
    llama-3.3-70b-versatile     128000       0.59      0.79    yes  models.dev
    llama-3.1-8b-instant        128000       0.05      0.08    yes  models.dev
    mixtral-8x7b-32768           32768       0.24      0.24    yes  estimated — no public rate found
  last probe      2026-07-28 14:02  ok (312 ms, tool-calling: yes, streaming: yes)
```

### 6.3 `tag providers test`

```bash
tag providers test [SLUG ...] [--all] [--model ID] [--timeout 20] [--json]
```

Sends a minimal completion (`"say ok"`, `max_tokens=5`) and, separately, a one-tool tool-calling probe. Reports reachability, auth, streaming, tool-calling, latency and observed usage accounting. It **states up front that it makes a real, billable API call** and requires `--yes` in non-interactive contexts. Results land in `provider_probes` so `show` can display the last known state offline.

```
groq        ok    312 ms   auth ok   stream ok   tools ok   usage ok
deepseek    fail  —        auth: HTTP 401 invalid_api_key
cerebras    skip  —        no credential (set CEREBRAS_API_KEY or run tag import-*)
local       fail  —        dial tcp 127.0.0.1:8080: connection refused
```

The `skip` row matters: "no credential" is not "broken", and conflating them is the kind of dishonest diagnostic the audit praises TAG for avoiding elsewhere.

### 6.4 `tag providers models` / `tag providers catalog`

```bash
tag providers models [--provider SLUG] [--sort price|context|name] [--tools] [--json]
tag providers catalog [--version] [--validate] [--json]
```

`catalog --validate` runs the same invariants the CI test runs (unique slugs, every model priced with provenance, no priced-but-unreachable model, valid URLs), so an operator can verify a build's catalog without reading Go.

### 6.5 Configuration

```yaml
providers:
  groq:
    api_key_env: GROQ_API_KEY        # override the catalog default
  openrouter:
    base_url: https://proxy.internal/openrouter/v1   # corporate gateway
    headers:
      HTTP-Referer: https://example.com
  deepseek:
    enabled: false                   # hide from list and refuse selection
```

---

## 7. Functional Requirements

| ID | Requirement | Acceptance Test |
|----|------------|-----------------|
| FR-00 | README and `docs/FEATURES.md` state the provider count and shape accurately, matching `len(llm.Registry)` minus `echo`. | Doc test parsing the README against the built binary. |
| FR-01 | `OpenAICompatProvider` implements `llm.Provider` parameterized by a catalog entry and delegates to the existing `streamOpenAICompatible`. No new SSE parsing or body-building code is written. | Code review + a test asserting the openai/local transcripts are unchanged. |
| FR-02 | The existing `openai` and `local` providers are re-expressed as catalog entries. Their observable behaviour — base URL defaults, env var precedence, error labels, `TAG_LOCAL_BASE_URL` fallback to `http://localhost:8080/v1` — is preserved exactly. | Golden-transcript replay against recorded fixtures. |
| FR-03 | `anthropic` remains a bespoke adapter (different wire format) and is unaffected. `echo` is unaffected. | Regression test. |
| FR-04 | The catalog is embedded via `go:embed` of a JSON file plus a typed decode, so it is reviewable as data and diffable in PRs. | Unit test decoding the embedded file. |
| FR-05 | Catalog validation enforces: unique slugs; valid absolute `https` base URLs (except `local`); non-empty `api_key_env` unless `no_auth`; every model priced; every price carrying `Source` or `Estimated: true`; no model priced without a reachable provider unless explicitly `unreachable: true`. | Table-driven catalog test, run in CI. |
| FR-06 | `pricingTable`'s 15 entries are migrated into catalog entries with `Source` and `Estimated` carried over **verbatim**, including the three explanatory unverified-rate comments. No rate changes value or provenance during migration. | Migration test comparing old table to new catalog entry-by-entry. |
| FR-07 | `lookupPrice` resolves through the catalog, preserving its existing bare-alias-plus-prefix resolution behaviour so no previously-priced model becomes "not found". | Regression test over every key in the old table, both prefixed and bare. |
| FR-08 | Credential resolution order is: `providers.<slug>.api_key` in config → `providers.<slug>.api_key_env` → the catalog's default env var → the `import-*`-populated store. The winning source is reported by `tag providers show`. | Table-driven unit test over all four sources. |
| FR-09 | A missing credential yields "not configured", distinct from "configured but rejected". `tag run --provider X` without a credential fails before any HTTP request with an actionable message naming the env var and the relevant `import-*` command. | Unit test asserting zero HTTP requests. |
| FR-10 | Quirk flags (`no_tools`, `no_streaming`, `usage_in_stream=false`, `extra_headers`, `max_tokens_field`) are handled inside the one adapter. No provider gets a bespoke `Stream`. | Architecture test asserting exactly two `Stream` implementations beyond `echo`/`fallback`: `AnthropicProvider` and `OpenAICompatProvider`. |
| FR-11 | A provider declaring `no_tools` and receiving a request with tools returns a clear error rather than silently dropping them — silent capability loss is exactly the "fake success" the project forbids. | Unit test. |
| FR-12 | `tag providers test` performs completion and tool-calling probes, reports each dimension independently, and distinguishes `skip` (no credential) from `fail` (credential rejected or endpoint unreachable). | Integration test with a stub HTTP server. |
| FR-13 | Probe results persist to `provider_probes`; `tag providers show` displays the last probe offline with its timestamp. | Integration test. |
| FR-14 | `tag providers test` refuses to run in a non-interactive context without `--yes`, and always states that it makes a billable call. | Unit test. |
| FR-15 | `gen_ai.system` span attribute is the catalog slug (`groq`), not the wire family (`openai`) and not `local`. | Integration test querying `spans`. |
| FR-16 | Cost attribution (PRD-046) resolves prices through the catalog and marks estimated rates in output, preserving the existing provenance display. | Integration test. |
| FR-17 | `providerForModel` continues to resolve `slug/model` refs and now resolves every catalog slug. `stripProviderPrefix` behaviour is unchanged. | Unit test over catalog slugs. |
| FR-18 | `tag route-fallback` chains accept catalog slugs with no changes to `internal/llm/fallback.go`. | Integration test with a multi-vendor chain. |
| FR-19 | Config `base_url` and `headers` overrides apply per provider and are shown by `tag providers show` as overrides. | Unit test. |
| FR-20 | `providers.<slug>.enabled: false` removes the provider from `list` and makes `--provider <slug>` a clear error. | Unit test. |
| FR-21 | All `tag providers` subcommands except `test` work with no network and no credentials. | Offline CI job. |
| FR-22 | `tag providers catalog --validate` runs the FR-05 invariants and exits non-zero on violation. | CI test. |
| FR-23 | Adding a provider requires no changes outside the catalog JSON and its test-case list. | Enforced by the FR-10 architecture test plus a documented contributor checklist. |
| FR-24 | `checkStreamResponse`'s existing non-SSE-error guard applies to every catalog provider, so an HTML error page or a JSON error body surfaces as an error rather than an empty stream. | Table-driven test with hostile responses per quirk profile. |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|------------|--------|
| NFR-01 | Catalog decode at init adds < 2 ms to startup (the 56-68 ms baseline must not regress meaningfully). | Timed test, 20 warm runs |
| NFR-02 | No new direct Go modules. `go:embed` + `encoding/json` + the existing `net/http` client. `CGO_ENABLED=0` unchanged. | `go mod graph` diff empty |
| NFR-03 | Binary size growth ≤ 100 KB (the catalog is text). | Size assertion in CI against the 19 MB baseline |
| NFR-04 | `internal/llm` gains no import edge to `internal/cli` or `internal/config`; config overrides are passed in as data. | Import-graph architecture test |
| NFR-05 | All `tag providers` subcommands support `--json` with a stable schema. | `jsonparity` test |
| NFR-06 | Probe requests are bounded by `context.Context` (default 20 s) and use the shared `llm.DefaultHTTPClient()` timeouts. | Test with a stubbed slow server |
| NFR-07 | API keys never appear in `list`, `show`, `test`, logs, spans, `--json` output or error messages — only "set"/"not set" and the source. | Grep-based test over all command output with a sentinel key value |
| NFR-08 | The catalog is human-reviewable: one JSON object per provider, models sorted, stable key order, so a PR adding a provider is readable. | Format test |
| NFR-09 | `go vet`, `golangci-lint`, `staticcheck` clean; `gofmt` clean. | CI gate |
| NFR-10 | Every catalog price carries provenance; no rate is ever displayed as authoritative without a `Source`. | Catalog validation test |

---

## 9. Technical Design

### 9.1 New and Modified Files

| File | Change | Description |
|---|---|---|
| `internal/llm/catalog.go` | **New** | `Catalog`, `ProviderEntry`, `ModelEntry`, `Quirks`; `go:embed catalog.json`; validation; lookup helpers. |
| `internal/llm/catalog.json` | **New** | The data. Reviewable, diffable, the only file a new provider touches. |
| `internal/llm/openaicompat.go` | **New** | `OpenAICompatProvider` — a `Provider` over a `ProviderEntry`, delegating to `streamOpenAICompatible`. |
| `internal/llm/openai.go` | **Extend** | `streamOpenAICompatible` gains an optional header map and quirk struct; the existing signature's behaviour is preserved for current callers. |
| `internal/llm/local.go` | **Replace** | Becomes a catalog entry. The file is deleted; its `TAG_LOCAL_BASE_URL`/`TAG_LOCAL_API_KEY` handling and localhost default move into the entry, with a regression test proving equivalence. |
| `internal/llm/register.go` | **New** | Catalog-driven `init()` registration, replacing per-file `init()` calls for compat providers. |
| `internal/cli/providers.go` | **New** | `tag providers` group. |
| `internal/cli/observability.go` | **Modify** | `pricingTable` and `knownProviderPrefixes` are derived from the catalog; `lookupPrice` keeps its resolution semantics and its `modelPrice{Source, Estimated}` shape. |
| `internal/store/schema` | **Extend** | `provider_probes`. |

### 9.2 Catalog Types

```go
// internal/llm/catalog.go

// Quirks are the per-provider deviations from the OpenAI shape. Every deviation
// must be expressible here: the moment a provider needs a bespoke Stream, the
// design has failed and FR-10's architecture test says so.
type Quirks struct {
	NoTools        bool              `json:"no_tools,omitempty"`
	NoStreaming    bool              `json:"no_streaming,omitempty"`
	UsageInStream  *bool             `json:"usage_in_stream,omitempty"`  // nil = yes
	MaxTokensField string            `json:"max_tokens_field,omitempty"` // e.g. "max_completion_tokens"
	ExtraHeaders   map[string]string `json:"extra_headers,omitempty"`
}

// ModelEntry carries price WITH provenance. The Source/Estimated pair is lifted
// verbatim from internal/cli/observability.go's modelPrice, deliberately: that
// table's discipline — never present an uncorroborated rate as authoritative —
// is the behaviour being preserved, not reinvented.
type ModelEntry struct {
	Slug        string  `json:"slug"`
	Context     int     `json:"context"`
	InPer1M     float64 `json:"in_per_1m"`
	OutPer1M    float64 `json:"out_per_1m"`
	Source      string  `json:"source"`
	Estimated   bool    `json:"estimated,omitempty"`
	Tools       bool    `json:"tools"`
	Unreachable bool    `json:"unreachable,omitempty"` // priced but no callable endpoint — must be explicit
}

type ProviderEntry struct {
	Slug        string       `json:"slug"`
	Name        string       `json:"name"`
	BaseURL     string       `json:"base_url"`
	APIKeyEnv   string       `json:"api_key_env"`
	NoAuth      bool         `json:"no_auth,omitempty"`
	Wire        string       `json:"wire"` // "openai" | "anthropic"
	Quirks      Quirks       `json:"quirks,omitempty"`
	Models      []ModelEntry `json:"models"`
	Importers   []string     `json:"importers,omitempty"` // which tag import-* can supply this key
	Docs        string       `json:"docs,omitempty"`
}
```

`Importers` is the field that operationalizes the credential asset: `tag providers list` can say "configured (import-opencode)" and, when a key is missing, name the importer that could supply it.

### 9.3 The Adapter

```go
// internal/llm/openaicompat.go

// OpenAICompatProvider is the ONE adapter behind every OpenAI-shaped vendor.
// It exists because local.go already proved the shape: that file is 51 lines of
// base-URL and key selection wrapped around streamOpenAICompatible. This
// generalizes those 51 lines into a table lookup.
type OpenAICompatProvider struct {
	Entry      ProviderEntry
	Override   *ProviderOverride // config: base_url, api_key, headers
	HTTPClient *http.Client
}

func (p OpenAICompatProvider) Name() string { return p.Entry.Slug }

func (p OpenAICompatProvider) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	if p.Entry.Quirks.NoTools && len(req.Tools) > 0 {
		// Silent capability loss is the failure mode this project explicitly
		// forbids. Say so instead of dropping the tools and returning prose.
		return nil, fmt.Errorf("provider %q does not support tool calling; "+
			"remove --tools or choose another provider", p.Entry.Slug)
	}
	key := p.key()
	if key == "" && !p.Entry.NoAuth {
		return nil, fmt.Errorf("provider %q has no credential: set %s, or run `tag %s`",
			p.Entry.Slug, p.Entry.APIKeyEnv, importerHint(p.Entry))
	}
	return streamOpenAICompatible(ctx, req, p.base(), key, p.Entry.Slug, p.client(),
		withQuirks(p.Entry.Quirks))
}
```

The error label passed to `streamOpenAICompatible` is the catalog slug, so `checkStreamResponse`'s messages — and therefore `tag trace` and every user-facing failure — name the actual vendor rather than `local` or `openai`.

### 9.4 Registration

```go
// internal/llm/register.go
func init() {
	for _, e := range EmbeddedCatalog().Providers {
		if e.Wire != "openai" {
			continue // anthropic keeps its bespoke adapter
		}
		Register(OpenAICompatProvider{Entry: e})
	}
}
```

`Registry` is a plain `map[string]Provider` and `Register` is a one-line setter, so this needs no changes to the registry itself. Config overrides are applied at selection time in `internal/cli` (where config already lives), preserving NFR-04's import-direction rule.

### 9.5 Pricing Migration

```go
// internal/cli/observability.go — pricingTable becomes derived, not authoritative.
//
// The migration is strictly value-preserving: every key in the old table maps to
// exactly one catalog model with the SAME In/Out/Source/Estimated values, asserted
// entry-by-entry by TestPricingMigrationPreservesValues. Three rates in that table
// carry carefully written "unverified" provenance strings (deepseek-v4-pro,
// gemini-2.5-flash, qwen/*). Those strings move across verbatim. Losing them
// would silently promote unverified numbers to authoritative — the exact error
// the original comments were written to prevent.
func lookupPrice(model string) (modelPrice, bool) {
	// same bare-alias + knownProviderPrefixes resolution as today, over the catalog
}
```

The models the old table priced without an adapter (`deepseek/*`, `qwen/*`, `google/gemini-*`) resolve one of two ways, each explicit: DeepSeek and Qwen gain real catalog providers (both ship OpenAI-compatible endpoints), so they become callable; Google's Gemini API is not OpenAI-shaped, so `google/gemini-*` models are marked `unreachable: true` and `tag providers models` shows them as priced-but-not-callable rather than implying they work. Making the second case *visible* is the point — it is currently invisible.

### 9.6 SQLite DDL

```sql
CREATE TABLE IF NOT EXISTS provider_probes (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  slug        TEXT NOT NULL,
  model       TEXT NOT NULL,
  outcome     TEXT NOT NULL,          -- ok|fail|skip
  latency_ms  INTEGER,
  auth_ok     INTEGER,
  stream_ok   INTEGER,
  tools_ok    INTEGER,
  usage_ok    INTEGER,
  error       TEXT,
  created_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_probe_slug ON provider_probes(slug, created_at DESC);
```

### 9.7 Integration Points

| Package | Integration |
|---|---|
| `internal/llm` | Catalog, one compat adapter, catalog-driven registration. `Provider`, `Request`, `Event`, `Registry`, `Register` unchanged. |
| `internal/llm/fallback.go` | **Unchanged.** Chains gain reach because `Registry` has more entries — the mechanism needs nothing. |
| `internal/cli/observability.go` | `pricingTable` derived from the catalog; `lookupPrice` semantics and `modelPrice` shape preserved. |
| `internal/cli/run.go` | `providerForModel` / `stripProviderPrefix` unchanged; they now resolve more slugs. |
| `internal/importer` | Catalog `Importers` field maps importers to providers for `list`/`show` hints. |
| `internal/store` | `provider_probes`. |
| `internal/config` | `providers.<slug>` overrides, read in `internal/cli` and passed in as data. |
| PRD-031 fallback chains | More reachable steps; no code change. |
| PRD-107 confidence routing | Routes over catalog cost/context metadata; no code change here. |
| PRD-041 / PRD-046 | `gen_ai.system` = catalog slug; prices resolved through the catalog with provenance. |

---

## 10. Security Considerations

1. **Credential handling.** Keys are read from config/env/store and passed to the HTTP client. They never appear in `list`, `show`, `test`, error messages, logs, spans or `--json`. NFR-07 tests this with a sentinel value grepped across all output.

2. **Base-URL overrides are a credential-exfiltration vector.** A config `base_url` pointing at an attacker's host would send API keys there. Mitigations: overrides are config-only (no environment variable and no flag, so a poisoned env cannot redirect traffic); `tag providers show` displays an override prominently; non-`https` overrides require an explicit `allow_insecure: true`; and `tag doctor` surfaces any provider with a non-catalog base URL.

3. **Catalog integrity is a supply-chain property.** The catalog is embedded at build time and is not fetched at runtime (NG3), so there is no remote fetch to poison. A malicious catalog entry would have to arrive through a reviewed PR to a JSON file whose diff shows exactly which host is being added — which is precisely why the catalog is data rather than code.

4. **SSRF via a provider base URL.** Catalog URLs are fixed `https` endpoints validated at build time. User overrides are the operator's own decision about their own network; `internal/marketplace`'s SSRF guard is deliberately *not* applied here, because a legitimate corporate gateway is frequently a private-range host and blocking it would break the feature that override exists for. The trade-off is stated rather than silently made.

5. **Error responses leaking secrets.** `checkStreamResponse` snippets error bodies into messages. Provider error bodies occasionally echo request headers. The snippet path applies redaction for anything matching a key-shaped pattern before display (FR-24's test set includes an echoing error body).

6. **Probe cost.** `tag providers test` makes billable calls. It says so, caps `max_tokens` at 5, and requires `--yes` when non-interactive so a CI job cannot silently spend money across 16 providers.

7. **Silent capability degradation.** A provider without tool support that quietly drops tools would produce prose where a tool call was expected — the "fake success" pattern the project forbids. FR-11 makes it a hard error.

8. **Price provenance is a security-adjacent honesty property.** Budget enforcement (PRD-039) and cost attribution (PRD-046) act on these numbers. An unverified rate presented as authoritative causes wrong budget decisions. The `Estimated` flag is carried through migration verbatim (FR-06) and surfaced everywhere a price is shown.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`internal/llm/catalog_test.go`, `openaicompat_test.go`)

- `TestCatalogValidates` — all FR-05 invariants over the shipped catalog.
- `TestCatalogSlugsUnique` / `TestCatalogURLsHTTPS` / `TestEveryModelPriced`.
- `TestNoPricedUnreachableModelWithoutFlag`.
- `TestPricingMigrationPreservesValues` — every old `pricingTable` key maps to a catalog model with identical `In`/`Out`/`Source`/`Estimated`.
- `TestLookupPriceBareAndPrefixed` — every old key resolves in both forms.
- `TestCredentialPrecedence` — config → provider env → catalog env → import store.
- `TestMissingCredentialNoHTTP` — assert zero requests via a counting `RoundTripper`.
- `TestNoToolsProviderErrors` — not a silent drop.
- `TestQuirkHeadersApplied` / `TestUsageInStreamFalse`.
- `TestOnlyTwoStreamImplementations` — the FR-10 architecture test.
- `TestKeysNeverInOutput` — sentinel key grepped across every command's output.

### 11.2 Regression Tests (`internal/llm/regression_test.go`)

Golden-transcript replay proving the refactor is behaviour-preserving: recorded SSE fixtures for `openai` and `local` are replayed through the new adapter and asserted byte-identical in emitted `Event` sequences, including tool-call linkage and usage accumulation. This is the test that makes deleting `local.go` safe.

### 11.3 Integration Tests (`internal/cli/providers_e2e_test.go`)

- `TestProvidersListOffline` — full list with egress blackholed.
- `TestProvidersShowRedactsKey`.
- `TestProvidersTestSkipVsFail` — no credential vs rejected credential vs unreachable.
- `TestProbePersistedAndShownOffline`.
- `TestRunWithCatalogProvider` — stub HTTP server; `tag run --provider groq`.
- `TestSpanSystemIsSlug` — `gen_ai.system == "groq"`.
- `TestCostUsesCatalogPriceWithProvenance`.
- `TestFallbackChainAcrossVendors` — anthropic → groq → local with injected 429s.
- `TestConfigBaseURLOverrideShown` / `TestInsecureOverrideRequiresFlag`.
- `TestDisabledProviderHidden`.
- `TestImporterHintShown` — missing key names the right `import-*` command.

### 11.4 Doc Test

`TestReadmeProviderClaimMatchesBinary` parses the provider count out of the README and asserts it equals `len(llm.Registry)-1`. This is the test that keeps FR-00 true after the PRD is closed, and it is the reason the doc fix is a *test*, not a one-time edit.

---

## 12. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-00 | README and FEATURES state an accurate provider count, enforced by a test. | Doc test |
| AC-01 | `tag providers list` shows ≥ 15 providers with credential status, offline. | Integration test |
| AC-02 | `tag run --provider groq` works with `GROQ_API_KEY` set (stub server in CI). | Integration test |
| AC-03 | `openai`, `anthropic`, `local` and `echo` behave byte-identically post-refactor. | Golden-transcript replay |
| AC-04 | Every catalog model has a price with provenance; unverified rates display `estimated`. | Catalog validation test |
| AC-05 | No model is priced without either a callable endpoint or an explicit `unreachable` marker. | Catalog validation test |
| AC-06 | `tag providers test` distinguishes `ok` / `fail` / `skip` correctly. | Integration test |
| AC-07 | API keys never appear in any output. | Sentinel grep test |
| AC-08 | `tag route-fallback` works across vendors with no `fallback.go` change. | Integration test |
| AC-09 | `gen_ai.system` is the catalog slug in emitted spans. | Integration test |
| AC-10 | Adding a provider requires only a catalog row and a test case. | Demonstrated by a PR-shaped diff in the repo history |
| AC-11 | All subcommands except `test` complete with egress blackholed and no keys. | Offline CI job |
| AC-12 | A missing credential names the env var and the relevant `import-*` command. | Integration test |
| AC-13 | Binary growth ≤ 100 KB; startup regression < 2 ms. | CI size + timing assertions |
| AC-14 | `tag providers catalog --validate` exits non-zero on a deliberately corrupted catalog. | CI test |

---

## 13. Dependencies

| Dependency | Type | Justification |
|---|---|---|
| `embed` | Stdlib | Catalog embedding |
| `encoding/json` | Stdlib | Catalog decode |
| `net/http` | Stdlib | Existing `llm.DefaultHTTPClient()` |
| `modernc.org/sqlite` | Core (project driver) | `provider_probes` |
| `github.com/spf13/cobra` | Core | `tag providers` group |
| PRD-012 (cost tracking & budget) | Internal | Catalog prices feed budgets |
| PRD-017 (multi-model benchmarking) | Internal | A wider catalog is what `tag compare` compares |
| PRD-031 (model fallback chains) | Internal | **Consumer, not duplicate** — chains gain reach; `fallback.go` is unmodified |
| PRD-041 (OTel GenAI span cost attribution) | Internal | `gen_ai.system` = catalog slug |
| PRD-046 (per-span USD cost attribution) | Internal | Prices resolved via the catalog with provenance |
| PRD-107 (confidence-aware routing) | Internal | **Consumer, not duplicate** — routes over catalog metadata |
| `internal/importer` (shipped, 18 importers) | Internal | Credential on-ramp; the asset this PRD activates |

---

## 14. Open Questions

| # | Question | Owner | Resolution Target |
|---|----------|-------|-------------------|
| OQ-1 | Should the catalog be refreshable from models.dev (`tag providers catalog --refresh`)? It solves staleness — providers change models weekly — but adds a network dependency and a supply-chain surface to a startup path. Proposal: embedded in v1; refresh only if staleness becomes a real complaint, and then with signature verification. | Arch | After v1 soak |
| OQ-2 | How is catalog staleness handled between releases? A model removed by a vendor produces a 404 with a confusing message. Proposal: catalog carries a `generated_at`; `tag doctor` warns when it is > 90 days old. | Product | Before implementation |
| OQ-3 | Should Bedrock (SigV4), Vertex (OAuth) and Azure (deployment-scoped URLs) be in scope? Each is a real protocol, not a base-URL change. `tag import-aws` already exists, which makes Bedrock tempting. Proposal: out of v1 (NG2); one follow-up PRD per wire format. | Arch | Defer |
| OQ-4 | OpenRouter exposes 200+ models. Enumerating them bloats the catalog and goes stale immediately. Proposal: catalog a curated ~10 plus a note that arbitrary `openrouter/<model>` refs pass through unvalidated and unpriced. | Engineering | Before implementation |
| OQ-5 | Should `google/gemini-*` be dropped from pricing entirely rather than marked `unreachable`? Marking is more honest (the numbers were researched and are correct); dropping is simpler. Proposal: mark. | Product | Before implementation |
| OQ-6 | Should `tag providers test --all` be a `tag doctor` check? It costs money, so probably not by default — but a `doctor --probe-providers` opt-in may be right. | Product | After v1 |
| OQ-7 | Should quirks be auto-detected by probing rather than declared? Auto-detection drifts and is unreviewable; declaration is greppable. Proposal: declare, and have `test` *report* mismatches against the declaration rather than silently adapting. | Engineering | Before implementation |
| OQ-8 | Does `local` remain a distinct slug once the catalog exists, or become `custom` with a required `base_url`? `local` is documented and used as the last step in fallback chains; renaming breaks configs. Proposal: keep `local`, add `custom` as an alias for arbitrary endpoints. | Product | Before implementation |

---

## 15. Complexity and Timeline

**Total Estimated Effort:** M (1-2 weeks, 1 engineer)

### Phase 0 — Correct the claim (Day 1)
- README + `docs/FEATURES.md`: accurate provider count and shape
- `TestReadmeProviderClaimMatchesBinary` so it stays accurate
- **Ships independently and is valuable even if nothing else in this PRD is built.**

### Phase 1 — Catalog and validation (Days 2-4)
- `catalog.go`, `catalog.json`, `go:embed`, typed decode
- All FR-05 invariants as CI tests; `tag providers catalog --validate`
- Migrate `pricingTable` with the entry-by-entry provenance-preservation test
- Deliverable: catalog validates; no price changes value or provenance

### Phase 2 — The one adapter (Days 5-7)
- `OpenAICompatProvider`; quirk plumbing through `streamOpenAICompatible`
- Re-express `openai` and `local` as catalog entries; delete `local.go`
- Golden-transcript replay proving byte-identical behaviour
- Deliverable: AC-03 passes; only two `Stream` implementations remain

### Phase 3 — Breadth and credentials (Days 8-10)
- ≥ 15 provider entries with models, prices and provenance
- Credential precedence; importer mapping; missing-credential hints
- Deliverable: AC-01, AC-12 pass

### Phase 4 — CLI and observability (Days 11-13)
- `tag providers list/show/test/models/catalog`; `provider_probes`
- `gen_ai.system` slug; catalog-resolved cost attribution
- Deliverable: AC-06, AC-07, AC-09 pass

### Phase 5 — Integration and hardening (Days 14)
- Cross-vendor fallback chain test; config overrides; offline CI job
- Binary-size and startup assertions; sentinel-key grep test
- Deliverable: all 15 AC items pass

---

*PRD-132 authored for TAG. Status: Proposed — not built.*
*Scope note: this PRD **adds** providers. PRD-031 (fallback chains) and PRD-107 (confidence-aware routing) **choose between** them and are unmodified here.*
