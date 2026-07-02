# PRD-103: Dynamic Task-Type Classifier via Embeddings (vs Static YAML) (`tag route classify`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (1-2 weeks)
**Category:** Advanced Reasoning & Planning
**Affects:** `internal/routing` (new classifier package), `internal/memory/embed` (Embedder interface), `internal/cli` (`route.go` subcommand group), `internal/store` (schema/DDL)
**Depends on:** PRD-043 (vector-based tool retrieval — SentenceTransformer infrastructure), PRD-027 (eval framework — classifier quality scoring), PRD-028 (sandbox code execution), PRD-013 (agent tracing/observability), PRD-034 (secret scanning — prompt content before embedding), PRD-012 (cost tracking/budget), PRD-041 (OTel span cost attribution)
**Inspired by:** DSPy task classification, LangGraph routing, Semantic Kernel planners

---

## 1. Overview

TAG's routing system today maps user-supplied `--task-type` strings (e.g., `"coding"`, `"research"`, `"security"`) to profile configurations declared in a static YAML file. `ResolveRoute()` in the `internal/cli` route group performs a direct map lookup: if the exact string is found under `routing.task_types`, the associated master profile, workers, and verifier are returned; if not, execution halts with a fatal error. This design is explicit and predictable, but it places an unreasonable maintenance burden on operators. Adding support for a new task type — `"data-viz"`, `"ml-training"`, `"devops-pipeline"` — requires a manual YAML edit, a config reload, and operator knowledge of every string alias a user might type. Misspellings, synonyms, and domain-specific terminology all produce hard failures rather than graceful nearest-neighbor resolution.

The field has moved past static string dispatch. RouteLLM (arXiv:2406.18665) demonstrates that a lightweight BERT-based binary router can decide model allocation in under 10 ms, outperforming keyword matching at every operating point on the MT-Bench routing curve. DSPy's `Predict` module allows declarative task-type classification from few examples without prompt engineering. LangGraph's router nodes use embedding similarity to dispatch graph edges. Semantic Kernel's planner selects skills via cosine similarity over skill descriptions. The common thread across all of these is: embed the task description, find the nearest known category, and dispatch — no static string required.

PRD-103 replaces TAG's static YAML lookup with an embedding classifier stored entirely in the local SQLite database. At training time (`tag route train`), the classifier embeds all task examples from the existing YAML routing table (one or more natural-language examples per task type) and stores the resulting vectors in a `route_examples` table. At classify time (`tag route classify`), the user's prompt is embedded with the same embedder, cosine similarity is computed in-process against all stored examples, and the task type with the highest aggregate similarity score is returned — along with a confidence value and the runner-up. When confidence falls below a configurable threshold, the system emits a warning and falls back to the static YAML path or prompts the user to add examples.

Embeddings are produced through the `Embedder` interface in `internal/memory/embed`. The **default** embedder is a provider embedding API (OpenAI embeddings by default; Voyage/Cohere/Gemini pluggable) — a single static Go binary has no numpy/torch/sentence-transformers peer for in-process neural encoding. This is the headline change from the Python framing: the default classify/train path now performs a network round-trip and incurs a per-embed cost. The **offline** story is twofold: (a) when no embedder is reachable, `tag route classify` degrades to the existing exact-match YAML resolution (the primary offline path), and (b) air-gapped operators may compile a build-tagged pure-Go MiniLM embedder (`nlpodyssey/cybertron`, `all-MiniLM-L6-v2`, 384-dim) that runs in-process with no network at the cost of slower encode and a larger binary. Cold-start latency for the local build-tag path is under ~600 ms; the default provider path is dominated by the network round-trip (see NFRs). No existing exact-match workflows break.

> **Offline/no-network tradeoff (re-framed by the Go move).** The Python premise — a fully-local, no-network embedding model — is fundamentally reconsidered under Go (see docs/GO_MIGRATION_RESEARCH.md risk #2, docs/GO_MIGRATION_PLAN.md decision #3). The default is now a provider embedding API (network + cost + an offline failure mode). Fully-offline classification is available only via FTS5/exact-match degradation or the opt-in build-tagged local model. Goal G5 and NFR-11 are revised accordingly below.

The new subcommand surface is `tag route classify`, `tag route train`, `tag route add-example`, and an augmented `tag route list`. These integrate with TAG's existing tracing infrastructure (PRD-013, `go.opentelemetry.io/otel`) so each classify call emits a span with `route.classifier=embedding`, `route.task_type`, `route.confidence`, and `route.method` attributes for observability. Classify decisions are persisted to a `route_decisions` SQLite table for audit, cost attribution, and future fine-tuning of the classifier.

---

## 2. Problem Statement

### 2.1 Static YAML Dispatch Is Brittle and Unmaintainable at Scale

The current `ResolveRoute()` function performs an exact-string map lookup against `routing.task_types`. This means that `--task-type coding` succeeds but `--task-type code`, `--task-type "write code"`, and `--task-type python` all fail with a fatal non-zero exit. Operators must maintain a complete enumeration of every string alias users might supply, which is impossible in practice. As TAG deployments grow — enterprise teams with 20+ task types, shared configs with dozens of profiles — the YAML routing table becomes a maintenance bottleneck. Adding a single new capability (e.g., `data-visualization`) requires: (a) deciding on the canonical string, (b) updating the YAML, (c) communicating the exact string to all users, and (d) handling the inevitable misspellings manually. Every YAML edit also risks breaking existing routes through YAML formatting errors or key name collisions.

### 2.2 Natural-Language Task Descriptions Cannot Be Dispatched

Agentic workflows increasingly receive tasks as natural-language strings from orchestrators, webhooks, or CI triggers — not as pre-enumerated task-type codes. A GitHub Actions webhook might send `"Fix the failing authentication test in auth_service.py"`. A Slack slash command might send `"Summarize the Q3 security audit findings"`. Neither matches any YAML key. TAG today requires a pre-processing step to convert natural-language inputs to known task-type codes before routing. This step is manual, fragile, and absent from TAG's own tooling — meaning operators write ad-hoc scripts or use LLM API calls (adding latency and cost) to perform a classification step that belongs inside TAG's routing layer.

### 2.3 Zero Feedback on Routing Quality or Classifier Drift

The static YAML router produces no observable signal about routing quality. There is no mechanism to detect: that a task type has too few examples and classifies poorly; that a new cluster of similar tasks is appearing in production that belongs in a new category; or that confidence is systematically low for a particular profile, indicating that its YAML examples are stale or insufficiently diverse. Without these signals, routing quality silently degrades as usage patterns evolve, and operators have no data to drive example curation or taxonomy updates.

---

## 3. Goals and Non-Goals

### 3.1 Goals

| ID | Goal |
|----|------|
| G1 | `tag route classify --prompt "<text>"` classifies a natural-language prompt to a task type using embedding cosine similarity, returning task_type, confidence, runner_up, and method in JSON or human-readable form. |
| G2 | `tag route train --from-yaml <path>` ingests all examples from the existing YAML routing table and stores their embeddings in SQLite, making the classifier immediately usable without any additional configuration. |
| G3 | `tag route add-example --task-type <type> --example "<text>"` adds a single training example to the live classifier without retraining all embeddings — the new vector is inserted and immediately active for classify calls. |
| G4 | `tag route list --json` outputs all known task types, their example counts, and per-type confidence statistics from the `route_decisions` table. |
| G5 | **(Revised for Go — see Overview tradeoff note.)** Embedding is produced via the `Embedder` interface (`internal/memory/embed`). The default embedder is a provider API (network + per-embed cost); a build-tagged pure-Go MiniLM embedder (`all-MiniLM-L6-v2`, 384-dim) provides a genuine no-network option for air-gapped builds. The fully-local, zero-network guarantee is no longer the default — it is available only via the offline build tag or the exact-match degradation path (G6). |
| G6 | When no embedder is reachable (no provider credentials/network, and the offline build tag was not compiled in), all four subcommands degrade gracefully: `classify` falls back to exact-match YAML lookup, and the other three print an actionable hint (configure a provider or build with the `offline_embed` tag) with exit code 0. |
| G7 | Confidence threshold is configurable (`routing.classifier.confidence_threshold`, default 0.60). When confidence falls below threshold, a warning is emitted and `ResolveRoute()` is used as fallback. |
| G8 | Every classify call persists a row to the `route_decisions` SQLite table with prompt hash, predicted type, confidence, runner-up, and latency — enabling audit and drift detection. |
| G9 | `tag route classify` emits an OTel-compatible span (PRD-013) with `route.classifier`, `route.task_type`, `route.confidence`, `route.method`, and `route.latency_ms` attributes. |
| G10 | `tag run` and `tag queue add` accept `--auto-classify` flag that routes to task_type via the embedding classifier when `--task-type` is omitted, enabling fully natural-language task dispatch. |
| G11 | `tag route calibrate` evaluates classifier accuracy against a labelled YAML test set and prints per-type precision, recall, and F1 — integrating with PRD-027 eval framework for regression gating. |

### 3.2 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Fine-tuning or retraining the base embedding model. The classifier uses the embedder (provider model, or the frozen `all-MiniLM-L6-v2` weights under the offline build tag) as-is; the only "training" is embedding the few-shot examples. |
| NG2 | Multi-label classification (one prompt assigned to multiple task types simultaneously). PRD-103 produces exactly one task-type prediction per classify call. |
| NG3 | Online learning or gradient-based updates. The classifier state is a set of example vectors; adding examples is the only update mechanism. |
| NG4 | Replacing the YAML routing table as the source of profile configuration. The YAML table continues to define master/worker/verifier assignments for each task type; the classifier only predicts which key to look up. |
| NG5 | LLM-based task classification (sending the prompt to Claude to classify it). PRD-103 is specifically the local, no-API-call path. A future PRD may add an LLM classification mode. |
| NG6 | Semantic clustering or automatic taxonomy discovery. PRD-103 classifies into a fixed, operator-defined taxonomy. Automatic discovery of new task types is out of scope. |
| NG7 | Distributed or multi-process embedding index. The classifier index lives in the single-node SQLite database and is accessed by one process at a time. |

---

## 4. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Classify latency (warm, local build tag) | p50 < 15 ms, p99 < 50 ms (in-Go cosine over ≤ 500 BLOB vectors dominates) | `testing.B` benchmark: 100 classify calls after embedder warmup |
| Classify latency (warm, default provider) | p50 < 250 ms, p99 < 800 ms (dominated by the embedding-API network round-trip) | `testing.B` benchmark against a stubbed/live provider |
| Classify latency (cold, local build tag) | p50 < 600 ms | `testing.B`: first call in a fresh process (embedder init via `sync.Once`) |
| Accuracy on YAML examples (leave-one-out) | ≥ 90% on configs with ≥ 3 examples per type | `tag route calibrate --loo` |
| False-fallback rate | < 5% of classify calls fall back to exact-match when an embedder is reachable | `route_decisions` table: `method='fallback'` / total |
| Graceful degradation | `tag route classify` exits 0 with a human-readable warning when no embedder is reachable | Table-driven test with a stub Embedder returning an error |
| Package isolation | Loading `internal/routing` does not construct the embedder; the embedder is built lazily via `sync.Once` on first classify/train | Unit test asserting no provider client / model init at package init |
| Backward compatibility | All existing `tag route --task-type <exact>` calls continue to work identically | Integration test suite against existing YAML configs |
| Example persistence | `tag route add-example` row immediately visible in `tag route list` output | Integration test |
| Span emission | Each classify call produces one span in the `traces` table with a `route.classifier` attribute | Integration test |
| Train throughput | `tag route train` on 50-type / 10-example-per-type YAML completes in < 5 s (local build tag) / bounded by provider batch-embed latency + rate limits (default) | Benchmark with synthetic YAML |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Platform engineer | run `tag route train --from-yaml routing.yaml` once after writing my YAML | The embedding classifier is immediately active and I never need to update it again as long as my task descriptions are stable |
| U2 | Developer | run `tag run --auto-classify "Fix the failing OAuth test in auth_service"` without knowing the canonical task-type string | TAG routes my natural-language task to the right profile automatically, without a pre-processing step |
| U3 | Operator | run `tag route classify --prompt "Create a bar chart of monthly revenue" --json` | I can verify at the command line that a given prompt classifies to the `data-viz` task type before wiring it into an automation |
| U4 | Operator | run `tag route add-example --task-type data-viz --example "Plot a heatmap of user engagement"` | I can incrementally improve classifier accuracy for new task types without retraining all embeddings |
| U5 | Team lead | run `tag route calibrate` before merging a routing.yaml change | I get a per-type precision/recall table that reveals which task types are confusable, enabling targeted example improvement |
| U6 | Developer | receive a clear warning with confidence score when `tag route classify` is uncertain (confidence < 0.60) | I can catch misrouting before it dispatches an expensive agent run to the wrong profile |
| U7 | DevOps engineer | inspect the `route_decisions` table in the TAG SQLite database | I can audit which prompts were classified to which task types over the past 30 days, identifying routing drift |
| U8 | Operator | run `tag route classify` when no embedder is reachable (no provider configured, offline build tag absent) | I get clear guidance and the system falls back to exact-match routing rather than crashing |
| U9 | Developer | run `tag route list --json` | I see every task type, how many examples each has, and the average confidence from recent decisions, so I know which types need more examples |
| U10 | Platform engineer | set `routing.classifier.confidence_threshold: 0.75` in my config | High-confidence classification is required before embedding routing is used, ensuring that uncertain prompts always fall through to explicit `--task-type` flags |

---

## 6. Proposed CLI Surface

All new subcommands are rooted under `tag route` alongside the existing `tag route` (which becomes `tag route resolve` for clarity, with backward-compatible alias).

### 6.1 `tag route classify`

Embed a prompt and return the predicted task type.

```
tag route classify \
  --prompt "Write a data visualization of monthly sales trends" \
  [--config PATH] \
  [--threshold 0.60] \
  [--top-k 3] \
  [--json] \
  [--no-persist]
```

**Flags:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--prompt TEXT` | str | required | Natural-language task description to classify |
| `--config PATH` | str | `~/.tag/config.yaml` | Config file path |
| `--threshold FLOAT` | float | from config (0.60) | Confidence threshold below which fallback warning is emitted |
| `--top-k INT` | int | 3 | Number of candidate task types to show in output |
| `--json` | flag | false | Emit machine-readable JSON to stdout |
| `--no-persist` | flag | false | Do not write a row to `route_decisions` (useful for scripted dry-run probing) |

**Human-readable output:**
```
task_type:  data-viz
confidence: 0.847
runner_up:  reporting (0.612)
method:     embedding
latency_ms: 12

Top-3 candidates:
  1. data-viz     0.847  ████████████████████░
  2. reporting    0.612  █████████████░░░░░░░░
  3. analytics    0.598  ████████████░░░░░░░░░
```

**JSON output (`--json`):**
```json
{
  "task_type": "data-viz",
  "confidence": 0.847,
  "runner_up": "reporting",
  "runner_up_confidence": 0.612,
  "method": "embedding",
  "latency_ms": 12,
  "threshold": 0.60,
  "above_threshold": true,
  "candidates": [
    {"task_type": "data-viz",  "score": 0.847},
    {"task_type": "reporting", "score": 0.612},
    {"task_type": "analytics", "score": 0.598}
  ]
}
```

**Below-threshold warning (stderr):**
```
warning: classifier confidence 0.43 is below threshold 0.60 for task type 'data-viz'
warning: falling back to exact-match routing (--task-type required)
hint: add more examples with: tag route add-example --task-type data-viz --example "..."
```

---

### 6.2 `tag route train`

Embed all examples from a YAML routing config and upsert into SQLite.

```
tag route train \
  [--from-yaml PATH] \
  [--config PATH] \
  [--force] \
  [--dry-run] \
  [--json]
```

**Flags:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--from-yaml PATH` | str | resolved from `--config` | YAML file containing routing table with examples |
| `--config PATH` | str | `~/.tag/config.yaml` | TAG config (used if `--from-yaml` is omitted) |
| `--force` | flag | false | Re-embed examples that already have stored vectors |
| `--dry-run` | flag | false | Parse YAML and print what would be embedded, no DB writes |
| `--json` | flag | false | Emit training summary as JSON |

**Human-readable output:**
```
Loading routing.yaml...
Found 8 task types with 47 total examples.
Embedding with all-MiniLM-L6-v2 (22 MB, cached)...

  coding        8 examples  [████████████████████] done  1.2s
  security      6 examples  [████████████████████] done  0.9s
  research      7 examples  [████████████████████] done  1.0s
  data-viz      4 examples  [████████████████████] done  0.6s
  devops        5 examples  [████████████████████] done  0.7s
  writing       6 examples  [████████████████████] done  0.9s
  analytics     5 examples  [████████████████████] done  0.7s
  testing       6 examples  [████████████████████] done  0.9s

Upserted 47 examples into route_examples table.
Classifier ready. Run: tag route classify --prompt "..."
```

**JSON output:**
```json
{
  "task_types": 8,
  "examples_total": 47,
  "examples_upserted": 47,
  "examples_skipped": 0,
  "model": "all-MiniLM-L6-v2",
  "latency_ms": 7100,
  "source": "routing.yaml"
}
```

---

### 6.3 `tag route add-example`

Add a single training example for an existing or new task type.

```
tag route add-example \
  --task-type "data-viz" \
  --example "Create a bar chart of monthly sales figures" \
  [--config PATH] \
  [--json]
```

**Flags:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--task-type TEXT` | str | required | Task type key (must exist in YAML routing table) |
| `--example TEXT` | str | required | Natural-language example to embed and store |
| `--config PATH` | str | `~/.tag/config.yaml` | Config file path |
| `--json` | flag | false | Emit result as JSON |

**Human-readable output:**
```
Embedding example for task type 'data-viz'...
Inserted example (id: ex-a3f2c9d1).
task type 'data-viz' now has 5 examples.
```

---

### 6.4 `tag route list`

List all task types, example counts, and recent decision statistics.

```
tag route list \
  [--config PATH] \
  [--json] \
  [--stats]
```

**Flags:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--config PATH` | str | `~/.tag/config.yaml` | Config file path |
| `--json` | flag | false | Machine-readable JSON |
| `--stats` | flag | false | Include per-type confidence statistics from `route_decisions` |

**Human-readable output (`--stats`):**
```
TASK TYPE     EXAMPLES  AVG CONF  P50 CONF  DECISIONS (7d)
coding        8         0.891     0.903     142
security      6         0.844     0.861     38
research      7         0.812     0.828     71
data-viz      5         0.763     0.789     14
devops        5         0.731     0.748     9
writing       6         0.802     0.819     55
analytics     5         0.688     0.701     7   ← below threshold (hint: add examples)
testing       6         0.851     0.867     22
```

---

### 6.5 `tag route calibrate`

Evaluate classifier accuracy against labelled examples.

```
tag route calibrate \
  [--from-yaml PATH] \
  [--loo] \
  [--config PATH] \
  [--threshold 0.60] \
  [--json]
```

**Flags:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--from-yaml PATH` | str | from config | YAML with labelled examples |
| `--loo` | flag | false | Leave-one-out cross-validation instead of full-set |
| `--threshold FLOAT` | float | from config | Confidence threshold for "classified" vs "fallback" |
| `--json` | flag | false | Machine-readable JSON |

**Human-readable output:**
```
Leave-one-out calibration on 47 examples across 8 task types.

TASK TYPE   PRECISION  RECALL  F1     SUPPORT
coding      1.000      0.875   0.933  8
security    1.000      0.833   0.909  6
research    0.875      1.000   0.933  7
data-viz    1.000      0.750   0.857  4
...

Overall accuracy: 0.894
Macro F1:         0.901
Above-threshold:  89.4%

hint: 'data-viz' has low recall — add more examples with:
  tag route add-example --task-type data-viz --example "..."
```

---

## 7. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | `tag route train --from-yaml <path>` parses a YAML routing config (`gopkg.in/yaml.v3`), extracts per-task-type `examples` lists, embeds each via the configured `Embedder`, and upserts results into the `route_examples` table with `ON CONFLICT(task_type, example_hash) DO UPDATE`. | Must |
| FR-02 | `tag route classify --prompt <text>` embeds the prompt, computes cosine similarity against all rows in `route_examples`, groups scores by task_type (mean aggregation over all examples for that type), returns the highest-scoring type. | Must |
| FR-03 | When the top-1 confidence is below `routing.classifier.confidence_threshold` (default 0.60), `classify` emits a warning to stderr and sets `"above_threshold": false` in JSON output. | Must |
| FR-04 | Every call to `tag route classify` (unless `--no-persist`) inserts a row into `route_decisions` with prompt_hash (SHA-256 of prompt), predicted_type, confidence, runner_up, runner_up_confidence, method, latency_ms, and created_at. | Must |
| FR-05 | `tag route add-example --task-type <type> --example <text>` embeds the example, inserts into `route_examples`, and returns the new example's assigned ID. The task type does not need to already have examples in the table. | Must |
| FR-06 | `tag route list` reads task types from both the YAML config (for source-of-truth type names) and the `route_examples` table (for example counts), producing a merged view. Types in YAML but not yet in the table show 0 examples. | Must |
| FR-07 | When no `Embedder` is reachable (provider error/no network and the offline build tag absent), `tag route classify` falls back to exact-match resolution via `ResolveRoute()` and sets `method="exact_match"` in the decision row. It does not crash. | Must |
| FR-08 | `tag route train --dry-run` parses the YAML, counts examples per type, prints the plan, and exits 0 without writing to the database or loading the embedding model. | Must |
| FR-09 | `tag route calibrate --loo` performs leave-one-out evaluation: for each example `e` of type `T`, removes `e` from the index, classifies `e`, checks if prediction equals `T`, then re-inserts `e`. Reports per-type precision, recall, F1, and overall accuracy. | Should |
| FR-10 | `tag run --auto-classify` and `tag queue add --auto-classify` call `classify_task_type(cfg, prompt)` when `--task-type` is omitted. If confidence is above threshold, the predicted type is used; otherwise, execution fails with an actionable error message. | Should |
| FR-11 | The YAML `examples` field under each task type is a list of strings. If a task type has no `examples` field, `tag route train` skips it with a warning but does not fail. | Must |
| FR-12 | `tag route train --force` re-embeds all examples, replacing existing vectors. Without `--force`, examples whose `example_hash` already exists in `route_examples` are skipped. | Should |
| FR-13 | All four subcommands support `--json` output and print structured JSON to stdout with a consistent schema. Human-readable output goes to stdout; warnings go to stderr. | Must |
| FR-14 | The embedder is constructed lazily on first use via `sync.Once` (provider client or, under the offline build tag, the local model). Importing `internal/routing` does not construct the embedder or open a network connection. | Must |
| FR-15 | `tag route classify` emits an OpenTelemetry span via `internal/obs` / `go.opentelemetry.io/otel` (PRD-013) with attributes: `route.classifier`, `route.task_type`, `route.confidence`, `route.runner_up`, `route.method`, `route.latency_ms`. | Should |
| FR-16 | The `route_examples` table stores the embedding vector as a BLOB of little-endian float32 (serialised via `encoding/binary`, or an `unsafe` `[]float32`↔`[]byte` reinterpret guarded by a length check). The model name and vector dimension are stored in a `route_classifier_meta` table to validate compatibility on load. | Must |
| FR-17 | If the stored embedding dimension or model name does not match the current embedder's output (e.g., after switching from the 384-dim local MiniLM to a 1536/3072-dim provider), `tag route classify` prints an error and instructs the user to run `tag route train --force` (rebuild embeddings). | Must |
| FR-18 | Per-type score aggregation in `classify` uses the mean of the top-3 example cosine similarities for each type (not mean of all examples), reducing the influence of outlier examples. The aggregation strategy is configurable (`routing.classifier.aggregation`: `mean`, `max`, `top3_mean`; default `top3_mean`). | Should |
| FR-19 | `tag route list --stats` queries `route_decisions` for decisions in the past 7 days, computes per-type average confidence and p50 confidence, and flags types whose average is below threshold. | Should |
| FR-20 | Secret scanning (PRD-034) is applied to the `--prompt` value before it is embedded or persisted. If a secret pattern is detected, classification is aborted with a clear error. | Must |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Classify latency — local build tag (warm) | p50 < 15 ms, p99 < 50 ms on CPU (Mac M-series or Linux x86-64) with ≤ 500 stored examples. The in-Go cosine loop over BLOB vectors, not the encode, dominates once the local model is warm. |
| NFR-02 | Classify latency — default provider path | p50 < 250 ms, p99 < 800 ms, dominated by the embedding-API network round-trip. Cold call adds provider-client + TLS setup; local build-tag cold call (`sync.Once` model init) is p50 < 600 ms. |
| NFR-03 | Train throughput | Local build tag: ≤ 5 s wall time for 50 task types × 10 examples on CPU. Default provider: bounded by batched embed calls (`Embedder.EmbedBatch`) plus provider rate limits; train batches examples to minimise round-trips. |
| NFR-04 | Memory footprint | Default provider embedder: negligible resident (an HTTP client). Offline build tag: the pure-Go MiniLM model (`nlpodyssey/cybertron`) holds its weights resident (~90–150 MB); acceptable for CLI invocation and gated behind the build tag so the default binary stays small and CGO-free. |
| NFR-05 | SQLite storage | 384-dim float32 vector = 1,536 bytes/example (local); a 1536-dim provider vector = 6,144 bytes/example. 500 examples ≈ 0.75–3 MB — negligible. Vectors stored as a float32 BLOB column (matches the `internal/memory` VectorIndex convention). |
| NFR-06 | Backward compatibility | All existing `tag route --task-type <exact>` calls continue to work without change |
| NFR-07 | Package isolation | Importing `internal/routing` must not construct the embedder, load a model, or open a network connection; the embedder is built lazily via `sync.Once`. |
| NFR-08 | Concurrency safety | The classifier is used from concurrent goroutines (DAG parallelism, PRD-033). The lazy embedder init is guarded by `sync.Once`; any embedder impl that is not goroutine-safe serialises encode calls behind a `sync.Mutex`. All DB access goes through the single-writer `internal/store` handle. |
| NFR-09 | Determinism | Cosine over L2-normalised float32 vectors is deterministic for a given stored vector set. The local MiniLM embedder is deterministic per input+model version; provider embeddings are deterministic per model version but the model is a remote, versioned dependency (pin the model id in `route_classifier_meta`). Stored vectors are stable across process restarts. |
| NFR-10 | Disk persistence | `route_examples` and `route_decisions` survive process restarts; they live in the standard TAG SQLite store (`modernc.org/sqlite`, CGO_ENABLED=0, WAL) at `~/.tag/runtime/tag.sqlite3`, accessed via the shared `internal/store` handle. |
| NFR-11 | Network posture **(revised for Go)** | The default embedder makes an outbound HTTPS call per embed (provider API) — this replaces the Python "no network" guarantee. Fully-offline operation requires either (a) the exact-match degradation path, or (b) a binary compiled with the `offline_embed` build tag (pure-Go MiniLM, no network). Which mode is active is recorded on the decision row / span. |
| NFR-12 | Graceful degradation | If no embedder is reachable, affected subcommands exit 0 with a human-readable warning and actionable guidance (configure a provider or build with `offline_embed`); they do not panic or return unhandled errors. |

---

## 9. Technical Design

### 9.1 New Package: `internal/routing`

This package owns all embedding-classifier logic. It depends on `internal/memory/embed` (the `Embedder` interface + adapters), `internal/store` (the single-writer SQLite handle), and `internal/obs` (OTel). The `internal/cli` `route` command group calls into it; construction of the embedder is deferred (`sync.Once`) so importing the package does no network / model work.

```go
// internal/routing/routing.go
//
// PRD-103: Dynamic Task-Type Classifier via Embeddings.
//
// Replaces static YAML task_type lookup with an embedding nearest-neighbour
// classifier stored in SQLite (float32 BLOB vectors + in-Go cosine).
//
// Embeddings come from internal/memory/embed.Embedder:
//   - default: a provider embedding API (OpenAI/Voyage/Cohere/Gemini)
//   - offline: pure-Go MiniLM, compiled in with `-tags offline_embed`
package routing

import (
	"context"
	"sync"

	"github.com/sanskarpan/tag/internal/memory/embed"
	"github.com/sanskarpan/tag/internal/store"
)

const (
	DefaultThreshold   = 0.60
	DefaultAggregation = "top3_mean" // "mean" | "max" | "top3_mean"
	MetaSchemaVersion  = 1
)

// Classifier bundles the store handle and the lazily-initialised embedder.
type Classifier struct {
	db       *store.DB
	embedder embed.Embedder
	once     sync.Once // guards embedder construction (NFR-07)
	mu       sync.Mutex // serialises encode for non-goroutine-safe embedders (NFR-08)
}

// ClassifyResult is the result of a single classify call.
// Struct tags drive JSON output (replacing Python to_dict()); omitempty on the
// nullable runner-up fields mirrors the Python `| None`.
type ClassifyResult struct {
	TaskType           string      `json:"task_type"`
	Confidence         float64     `json:"confidence"`
	RunnerUp           string      `json:"runner_up,omitempty"`
	RunnerUpConfidence *float64    `json:"runner_up_confidence"`
	Method             string      `json:"method"` // "embedding" | "exact_match" | "fallback"
	LatencyMS          float64     `json:"latency_ms"`
	AboveThreshold     bool        `json:"above_threshold"`
	Candidates         []Candidate `json:"candidates"`
	DecisionID         string      `json:"decision_id"`
}

// Candidate is one ranked task-type score.
type Candidate struct {
	TaskType string  `json:"task_type"`
	Score    float64 `json:"score"`
}

// TrainResult is the result of a train call.
type TrainResult struct {
	TaskTypes        int     `json:"task_types"`
	ExamplesTotal    int     `json:"examples_total"`
	ExamplesUpserted int     `json:"examples_upserted"`
	ExamplesSkipped  int     `json:"examples_skipped"`
	Model            string  `json:"model"`
	LatencyMS        float64 `json:"latency_ms"`
	Source           string  `json:"source"`
}

// CalibrationResult holds per-type and aggregate calibration metrics.
type CalibrationResult struct {
	PerType           []TypeMetrics `json:"per_type"`
	OverallAccuracy   float64       `json:"overall_accuracy"`
	MacroF1           float64       `json:"macro_f1"`
	AboveThresholdPct float64       `json:"above_threshold_pct"`
	NExamples         int           `json:"n_examples"`
	Mode              string        `json:"mode"` // "loo" | "full"
}

type TypeMetrics struct {
	TaskType  string  `json:"task_type"`
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	F1        float64 `json:"f1"`
	Support   int     `json:"support"`
}
```

> **JSON rounding parity.** The Python `to_dict()` rounds floats with `round(x, 4)` (banker's / round-half-to-even). `encoding/json` emits full float64 precision and `math.Round` rounds half-away-from-zero to integers only. Emit these fields through a shared round-half-even-to-N-decimals helper (in `internal/obs` or a local `roundHalfEven`) — used by the whole memory family — so JSON output is stable and matches fixtures.

### 9.2 SQLite DDL

New tables are registered as a migration under `internal/store/migrate` and applied by the shared `store.Open()` path (idempotent `CREATE TABLE IF NOT EXISTS`), consistent with how all other feature tables are initialised. All access goes through the single `*store.DB` handle (`modernc.org/sqlite`, WAL, CGO_ENABLED=0) — no raw per-command connections (honours the single-writer contract).

```sql
-- Stores embedded examples for the task-type classifier.
CREATE TABLE IF NOT EXISTS route_examples (
    id           TEXT PRIMARY KEY,          -- "ex-{uuid8}"
    task_type    TEXT NOT NULL,             -- e.g. "coding", "data-viz"
    example_text TEXT NOT NULL,
    example_hash TEXT NOT NULL,             -- crypto/sha256(example_text), hex
    vector_blob  BLOB NOT NULL,             -- little-endian float32 vector (dim per meta)
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    source       TEXT NOT NULL DEFAULT 'manual',  -- "yaml" | "manual" | "api"
    UNIQUE(task_type, example_hash)
);

CREATE INDEX IF NOT EXISTS idx_route_examples_task_type
    ON route_examples(task_type);

-- Metadata for model compatibility validation.
CREATE TABLE IF NOT EXISTS route_classifier_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
-- Populated on first train (values reflect the active embedder):
-- ('model_name', 'text-embedding-3-small')   -- or 'all-MiniLM-L6-v2' under offline_embed
-- ('embed_dim',  '1536')                      -- or '384' for local MiniLM
-- ('provider',   'openai')                    -- or 'local-minilm'
-- ('schema_version', '1')
-- ('trained_at', '<ISO8601>')
-- The {model_name, embed_dim} pair is the read-time compatibility guard (FR-17).

-- Audit log of every classify decision.
CREATE TABLE IF NOT EXISTS route_decisions (
    id                    TEXT PRIMARY KEY,   -- "rd-{uuid8}"
    prompt_hash           TEXT NOT NULL,      -- SHA-256(prompt)
    predicted_type        TEXT NOT NULL,
    confidence            REAL NOT NULL,
    runner_up             TEXT,
    runner_up_confidence  REAL,
    method                TEXT NOT NULL,      -- "embedding" | "exact_match" | "fallback"
    above_threshold       INTEGER NOT NULL,   -- 0 or 1
    latency_ms            REAL NOT NULL,
    threshold_used        REAL NOT NULL,
    created_at            TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_route_decisions_created_at
    ON route_decisions(created_at);

CREATE INDEX IF NOT EXISTS idx_route_decisions_predicted_type
    ON route_decisions(predicted_type, created_at);
```

### 9.3 Core Algorithms

#### 9.3.1 The `Embedder` interface + lazy init + BLOB (de)serialisation

The `Embedder` interface lives in `internal/memory/embed` (shared with the memory subsystem). The default impl is a provider adapter; the offline MiniLM impl is selected at build time.

```go
// internal/memory/embed/embed.go
package embed

import "context"

// Embedder returns L2-normalised float32 vectors. Implementations:
//   - openaiEmbedder  (default; network + per-embed cost)
//   - voyage/cohere/gemini adapters (pluggable)
//   - miniLMEmbedder  (build tag `offline_embed`; pure-Go nlpodyssey/cybertron)
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) // batches train calls
	Model() string // e.g. "text-embedding-3-small" or "all-MiniLM-L6-v2"
	Dim() int       // 1536 / 3072 / 384
}
```

```go
// internal/routing/embed.go — lazy construction + BLOB (de)serialisation

// getEmbedder builds the configured embedder exactly once (NFR-07, NFR-08).
// Selection is driven by config (koanf); the offline build tag registers the
// local MiniLM factory. Returns an error (never panics) so callers can degrade
// to exact-match (FR-07).
func (c *Classifier) getEmbedder(cfg *Config) error {
	c.once.Do(func() {
		c.embedder = embed.New(cfg.Embed) // provider adapter, or offline MiniLM
	})
	if c.embedder == nil {
		return ErrNoEmbedder // -> exact-match fallback, method="exact_match"
	}
	return nil
}

// embedText returns an L2-normalised float32 vector for text.
func (c *Classifier) embedText(ctx context.Context, text string) ([]float32, error) {
	c.mu.Lock() // serialise for non-goroutine-safe embedders (local MiniLM)
	defer c.mu.Unlock()
	return c.embedder.Embed(ctx, text)
}

// vecToBlob serialises a float32 slice to little-endian bytes (numpy.tobytes peer).
func vecToBlob(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// blobToVec deserialises bytes -> float32 slice with a length guard
// (prevents buffer overread — replaces numpy.frombuffer + fixed-dtype).
func blobToVec(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("route: vector blob length %d not a multiple of 4", len(b))
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v, nil
}
```

> On hot paths the `blobToVec` copy loop can be replaced with an `unsafe` reinterpret of the backing array to `[]float32` — but only after asserting `len(b) == dim*4`. Keep the safe copy as the default; the length guard is mandatory either way.

#### 9.3.2 `Classify()` — Core Classifier

`numpy.dot` becomes a ~5-line Go cosine over the L2-normalised BLOB vectors (matches `internal/memory` `_cosine_sim` / VectorIndex). Aggregation groups by task type in a `map[string][]float64`.

```go
// cosine over already-L2-normalised vectors == dot product.
func dot(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

// Classify embeds a prompt and returns the highest-scoring task type.
func (c *Classifier) Classify(ctx context.Context, prompt string, threshold float64, topK int, agg string) (ClassifyResult, error) {
	t0 := time.Now()

	if err := c.validateModelCompat(ctx); err != nil { // FR-17 dim/model guard
		return ClassifyResult{}, err
	}
	queryVec, err := c.embedText(ctx, prompt)
	if err != nil {
		return ClassifyResult{}, err // caller degrades to exact-match (FR-07)
	}

	rows, err := c.db.QueryContext(ctx,
		`SELECT task_type, vector_blob FROM route_examples ORDER BY task_type`)
	if err != nil {
		return ClassifyResult{}, err
	}
	defer rows.Close()

	typeVecs := map[string][][]float32{}
	for rows.Next() {
		var tt string
		var blob []byte
		if err := rows.Scan(&tt, &blob); err != nil {
			return ClassifyResult{}, err
		}
		v, err := blobToVec(blob)
		if err != nil {
			return ClassifyResult{}, err
		}
		if len(v) != c.embedder.Dim() { // second-line dim guard per row
			return ClassifyResult{}, ErrDimMismatch
		}
		typeVecs[tt] = append(typeVecs[tt], v)
	}
	if err := rows.Err(); err != nil {
		return ClassifyResult{}, err
	}
	if len(typeVecs) == 0 {
		return ClassifyResult{}, ErrNoExamples // "run: tag route train --from-yaml ..."
	}

	// Per-type aggregate score.
	type ts struct {
		t string
		s float64
	}
	ranked := make([]ts, 0, len(typeVecs))
	for tt, vecs := range typeVecs {
		sims := make([]float64, len(vecs))
		for i, v := range vecs {
			sims[i] = dot(queryVec, v)
		}
		sort.Sort(sort.Reverse(sort.Float64Slice(sims)))
		ranked = append(ranked, ts{tt, aggregate(sims, agg)})
	}
	// Stable sort by score desc, then task_type asc for deterministic ties.
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].s != ranked[j].s {
			return ranked[i].s > ranked[j].s
		}
		return ranked[i].t < ranked[j].t
	})

	res := ClassifyResult{
		TaskType:       ranked[0].t,
		Confidence:     ranked[0].s,
		Method:         "embedding",
		LatencyMS:      float64(time.Since(t0).Microseconds()) / 1000.0,
		AboveThreshold: ranked[0].s >= threshold,
		DecisionID:     "rd-" + uuid.NewString()[:10],
	}
	if len(ranked) > 1 {
		res.RunnerUp = ranked[1].t
		ru := ranked[1].s
		res.RunnerUpConfidence = &ru
	}
	for i := 0; i < topK && i < len(ranked); i++ {
		res.Candidates = append(res.Candidates, Candidate{ranked[i].t, ranked[i].s})
	}
	return res, nil
}

// aggregate implements "max" | "top3_mean" | "mean".
func aggregate(sortedDesc []float64, agg string) float64 {
	switch agg {
	case "max":
		return sortedDesc[0]
	case "top3_mean":
		n := min(3, len(sortedDesc))
		return mean(sortedDesc[:n])
	default: // "mean"
		return mean(sortedDesc)
	}
}
```

#### 9.3.3 `TrainFromYAML()` — Bulk Training

YAML is parsed with `gopkg.in/yaml.v3` into a typed struct. `--dry-run` never constructs the embedder or writes to the DB (FR-08). Under the default provider embedder, examples are embedded via `EmbedBatch` to minimise round-trips; upserts run in a single transaction.

```go
// routingYAML mirrors the extended routing schema (yaml.v3 struct tags
// replace safe_load dict access; yaml.v3 has no arbitrary-object exec path,
// so this is the Go peer of the "safe_load only" security note).
type routingYAML struct {
	Routing struct {
		TaskTypes map[string]struct {
			Examples []string `yaml:"examples"`
		} `yaml:"task_types"`
	} `yaml:"routing"`
}

func (c *Classifier) TrainFromYAML(ctx context.Context, path string, force, dryRun bool) (TrainResult, error) {
	t0 := time.Now()

	raw, err := os.ReadFile(path)
	if err != nil {
		return TrainResult{}, err
	}
	var doc routingYAML
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return TrainResult{}, err
	}
	if len(doc.Routing.TaskTypes) == 0 {
		return TrainResult{}, fmt.Errorf("route: no task_types found in %s", path)
	}

	if !dryRun { // dry-run must NOT construct the embedder
		if err := c.getEmbedder(c.cfg); err != nil {
			return TrainResult{}, err
		}
	}

	var total, upserted, skipped, nTypes int
	// Deterministic iteration: sort task-type keys.
	types := slices.Sorted(maps.Keys(doc.Routing.TaskTypes))

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return TrainResult{}, err
	}
	defer tx.Rollback() // no-op after Commit

	for _, tt := range types {
		examples := doc.Routing.TaskTypes[tt].Examples
		if len(examples) == 0 {
			continue // FR-11: skip types without examples, no error
		}
		nTypes++
		for _, ex := range examples {
			total++
			sum := sha256.Sum256([]byte(ex))
			exHash := hex.EncodeToString(sum[:])
			if !force {
				var one int
				err := tx.QueryRowContext(ctx,
					`SELECT 1 FROM route_examples WHERE task_type=? AND example_hash=?`,
					tt, exHash).Scan(&one)
				if err == nil {
					skipped++
					continue
				} else if err != sql.ErrNoRows {
					return TrainResult{}, err
				}
			}
			if dryRun {
				upserted++
				continue
			}
			vec, err := c.embedText(ctx, ex)
			if err != nil {
				return TrainResult{}, err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO route_examples(id, task_type, example_text, example_hash, vector_blob, source)
				 VALUES(?,?,?,?,?,?)
				 ON CONFLICT(task_type, example_hash) DO UPDATE SET
				   vector_blob=excluded.vector_blob, source=excluded.source`,
				"ex-"+uuid.NewString()[:10], tt, ex, exHash, vecToBlob(vec), "yaml"); err != nil {
				return TrainResult{}, err
			}
			upserted++
		}
	}

	model := "(dry-run)"
	if !dryRun {
		if err := c.writeMeta(ctx, tx); err != nil { // {model, dim, provider, ...}
			return TrainResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return TrainResult{}, err
		}
		model = c.embedder.Model()
	}

	return TrainResult{
		TaskTypes:        nTypes,
		ExamplesTotal:    total,
		ExamplesUpserted: upserted,
		ExamplesSkipped:  skipped,
		Model:            model,
		LatencyMS:        float64(time.Since(t0).Microseconds()) / 1000.0,
		Source:           path,
	}, nil
}
```

#### 9.3.4 `Calibrate()` — Leave-One-Out Evaluation

All vectors are loaded once (pre-decoded) and evaluated in memory — no re-embedding, so calibrate makes zero network calls even on the default provider path. LOO excludes the current example by `id`. The per-type maps become plain Go `map[string]int` counters.

```go
func (c *Classifier) Calibrate(ctx context.Context, threshold float64, loo bool, agg string) (CalibrationResult, error) {
	type exRow struct {
		id  string
		tt  string
		vec []float32
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT id, task_type, vector_blob FROM route_examples`)
	if err != nil {
		return CalibrationResult{}, err
	}
	defer rows.Close()

	var all []exRow
	for rows.Next() {
		var r exRow
		var blob []byte
		if err := rows.Scan(&r.id, &r.tt, &blob); err != nil {
			return CalibrationResult{}, err
		}
		if r.vec, err = blobToVec(blob); err != nil {
			return CalibrationResult{}, err
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return CalibrationResult{}, err
	}

	tp := map[string]int{}
	fp := map[string]int{}
	fn := map[string]int{}
	support := map[string]int{}
	typeSet := map[string]struct{}{}
	for _, r := range all {
		typeSet[r.tt] = struct{}{}
	}

	correct, aboveThresh := 0, 0
	for _, q := range all {
		support[q.tt]++
		typeVecs := map[string][]float64{}
		for _, cand := range all {
			if loo && cand.id == q.id {
				continue // leave-one-out
			}
			typeVecs[cand.tt] = append(typeVecs[cand.tt], dot(q.vec, cand.vec))
		}
		if len(typeVecs) == 0 {
			continue
		}
		predType, predScore := "", math.Inf(-1)
		for tt, sims := range typeVecs {
			sort.Sort(sort.Reverse(sort.Float64Slice(sims)))
			if s := aggregate(sims, agg); s > predScore || (s == predScore && tt < predType) {
				predType, predScore = tt, s
			}
		}
		if predScore >= threshold {
			aboveThresh++
		}
		if predType == q.tt {
			correct++
			tp[q.tt]++
		} else {
			fp[predType]++
			fn[q.tt]++
		}
	}

	perType := make([]TypeMetrics, 0, len(typeSet))
	var f1Sum float64
	for tt := range typeSet {
		p := ratio(tp[tt], tp[tt]+fp[tt])
		r := ratio(tp[tt], tp[tt]+fn[tt])
		f1 := 0.0
		if p+r > 0 {
			f1 = 2 * p * r / (p + r)
		}
		f1Sum += f1
		perType = append(perType, TypeMetrics{tt, p, r, f1, support[tt]})
	}
	sort.Slice(perType, func(i, j int) bool { return perType[i].TaskType < perType[j].TaskType })

	n := len(all)
	mode := "full"
	if loo {
		mode = "loo"
	}
	return CalibrationResult{
		PerType:           perType,
		OverallAccuracy:   ratio(correct, n),
		MacroF1:           safeDiv(f1Sum, len(typeSet)),
		AboveThresholdPct: ratio(aboveThresh, n),
		NExamples:         n,
		Mode:              mode,
	}, nil
}
```

> **O(N²) note (OQ-04).** LOO is O(N²) over stored vectors but never re-embeds (vectors are pre-loaded), so it stays fully local and cheap at TAG's scale; a `--fast` centroid mode remains the documented escape hatch past a few thousand examples.

### 9.4 YAML Schema Extension

The existing routing YAML is extended with an optional `examples` list per task type. Existing YAML files without `examples` continue to work for exact-match routing; they simply produce no classifier training data.

```yaml
# routing.yaml (extended schema)
routing:
  task_types:
    coding:
      master: coder
      workers: [coder-worker]
      execution: kanban
      examples:
        - "Write a Python function that parses JSON"
        - "Implement a binary search tree in TypeScript"
        - "Fix the failing unit tests in auth_service.py"
        - "Refactor the database connection pool"

    data-viz:
      master: analyst
      workers: [viz-worker]
      execution: loop
      examples:
        - "Create a bar chart of monthly sales figures"
        - "Plot a time series of daily active users"
        - "Generate a heatmap of user engagement by hour and day"
        - "Visualize the distribution of response times"

    security:
      master: security-reviewer
      workers: []
      execution: loop
      examples:
        - "Review this authentication code for vulnerabilities"
        - "Check for SQL injection in the user input handling"
        - "Audit the API endpoints for authorization issues"
```

### 9.5 Integration Points

#### 9.5.1 `internal/cli/route.go` — New Command Wiring

The `route` command group is a `spf13/cobra` command tree (mirroring `src/tag/cmd/*.py`). Existing `tag route` becomes `tag route resolve` with a backward-compatible alias.

```go
// internal/cli/route.go
func newRouteCmd(app *App) *cobra.Command {
	route := &cobra.Command{Use: "route", Short: "Task-type routing and classification"}

	classify := &cobra.Command{
		Use:   "classify",
		Short: "Classify a prompt to a task type",
		RunE:  app.runRouteClassify,
	}
	classify.Flags().String("prompt", "", "natural-language task description (required)")
	classify.Flags().Float64("threshold", 0, "confidence threshold (default: config)")
	classify.Flags().Int("top-k", 3, "candidate task types to show")
	classify.Flags().Bool("json", false, "machine-readable JSON output")
	classify.Flags().Bool("no-persist", false, "do not write to route_decisions")
	_ = classify.MarkFlagRequired("prompt")

	train := &cobra.Command{Use: "train", Short: "Embed YAML examples into the classifier", RunE: app.runRouteTrain}
	train.Flags().String("from-yaml", "", "routing YAML (default: from --config)")
	train.Flags().Bool("force", false, "re-embed examples that already have vectors")
	train.Flags().Bool("dry-run", false, "parse+plan only; no embedder, no DB writes")
	train.Flags().Bool("json", false, "machine-readable JSON output")

	addEx := &cobra.Command{Use: "add-example", Short: "Add a classifier training example", RunE: app.runRouteAddExample}
	addEx.Flags().String("task-type", "", "task-type key (required)")
	addEx.Flags().String("example", "", "example text to embed and store (required)")
	addEx.Flags().Bool("json", false, "machine-readable JSON output")
	_ = addEx.MarkFlagRequired("task-type")
	_ = addEx.MarkFlagRequired("example")

	route.AddCommand(classify, train, addEx, newRouteListCmd(app), newRouteCalibrateCmd(app))
	return route
}
```

> **Sentinel-flag parity.** The Python argparse→cobra translation must preserve zero-valued sentinels the way the memory family does: `--threshold 0` (or unset) means "use config default", `--top-k` rejects negatives. Human-readable output goes to stdout; warnings go to stderr; `--json` emits the struct via `encoding/json` (FR-13).

#### 9.5.2 `internal/obs` OTel Integration (PRD-013)

Spans use `go.opentelemetry.io/otel` directly. When no tracer provider is configured, the OTel API returns a no-op tracer, so there is no error branch to guard (unlike the Python `try/except ImportError`).

```go
// In Classify(), wrap the core after computing result:
ctx, span := otel.Tracer("tag/routing").Start(ctx, "route.classify")
defer span.End()
span.SetAttributes(
	attribute.String("route.classifier", "embedding"),
	attribute.String("route.task_type", res.TaskType),
	attribute.Float64("route.confidence", res.Confidence),
	attribute.String("route.runner_up", res.RunnerUp),
	attribute.String("route.method", res.Method),
	attribute.Float64("route.latency_ms", res.LatencyMS),
	attribute.Bool("route.above_threshold", res.AboveThreshold),
)
```

#### 9.5.3 `internal/security` Integration (PRD-034)

```go
// In runRouteClassify(), before calling Classify():
if findings := security.ScanForSecrets(prompt); len(findings) > 0 {
	return fmt.Errorf("secret detected in prompt: %s; aborting", findings[0].RuleID)
}
```

Secret scanning (18 RE2 patterns + entropy check, per the migration plan) runs before the prompt is embedded, sent to a provider, or hashed — critical now that the default path transmits the prompt to a third-party embedding API.

#### 9.5.4 `--auto-classify` in `run` and `queue add`

```go
// In runRun() / runQueueAdd(), before ResolveRoute():
taskType := flags.taskType
if taskType == "" && flags.autoClassify {
	clf := routing.New(app.db, app.cfg)
	res, err := clf.Classify(ctx, prompt, threshold, 3, agg)
	if err != nil {
		// No embedder reachable: actionable guidance (configure provider or offline build).
		return fmt.Errorf("--auto-classify needs an embedder: configure routing.classifier.embed "+
			"or build with -tags offline_embed (%w)", err)
	}
	if !res.AboveThreshold {
		return fmt.Errorf("auto-classify confidence %.2f below threshold %.2f for %q; "+
			"specify --task-type explicitly or add more examples", res.Confidence, threshold, res.TaskType)
	}
	taskType = res.TaskType
	if !flags.json {
		fmt.Fprintf(os.Stderr, "warning: auto-classified as %q (confidence %.3f)\n", taskType, res.Confidence)
	}
}
```

> Budget enforcement (PRD-012/039): on the default provider path a classify call is a billable embed, so `--auto-classify` runs the pre-run budget gate before issuing the embed request; on the offline build-tag path the classify is free and the gate is a no-op.

### 9.6 Config Schema Extension

Config is loaded via `knadh/koanf/v2` (YAML file provider, `gopkg.in/yaml.v3` parser) and decoded into a typed `Config` struct. The classifier gains an `embed` block selecting the `Embedder` implementation.

```yaml
# ~/.tag/config.yaml — new optional section
routing:
  classifier:
    enabled: true                     # bool, default true
    confidence_threshold: 0.60        # float, default 0.60
    aggregation: top3_mean            # "mean" | "max" | "top3_mean"
    fallback_to_exact_match: true     # bool: fallback on low confidence / no embedder
    embed:
      provider: openai                # "openai" | "voyage" | "cohere" | "gemini" | "local"
      model: text-embedding-3-small   # provider model id (dim recorded in route_classifier_meta)
      # "local" (dim 384, all-MiniLM-L6-v2) is only available in a binary built
      # with `-tags offline_embed`; otherwise selecting it is a config error.
  task_types:
    coding:
      master: coder
      examples:
        - "..."
```

```go
type Config struct {
	Enabled            bool        `koanf:"enabled"`
	ConfidenceThreshold float64    `koanf:"confidence_threshold"`
	Aggregation        string      `koanf:"aggregation"`
	FallbackToExact    bool        `koanf:"fallback_to_exact_match"`
	Embed              EmbedConfig `koanf:"embed"`
}

type EmbedConfig struct {
	Provider string `koanf:"provider"` // default "openai"
	Model    string `koanf:"model"`
}
```

---

## 10. Security Considerations

1. **Prompt content in `route_decisions`:** Only the `crypto/sha256` hash of the prompt (hex) is stored in `route_decisions`, not the raw prompt text. This prevents sensitive task descriptions (which might contain business logic, PII, or credentials) from accumulating in the audit log. The prompt hash is sufficient for deduplication and drift analysis.

2. **Prompt transmission on the default provider path (new under Go):** The default embedder sends the raw prompt text over HTTPS to a third-party embedding API. This is a material change from the Python fully-local premise: task descriptions now leave the host. Secret scanning (below) runs *before* transmission, but operators handling regulated or confidential prompts should either configure a self-hosted/OpenAI-compatible embedding endpoint (`option.WithBaseURL`) or build with `-tags offline_embed` for a no-egress local embedder. The active mode is recorded on each decision row / span.

3. **Secret scanning before embedding:** PRD-034 secret patterns (RE2, no catastrophic backtracking) plus an entropy check are applied to the `--prompt` value before the text is embedded, transmitted to a provider, or hashed. If a secret or known credential pattern is detected, classification is aborted with a non-zero exit. This prevents secret exfiltration via embedding vectors, provider logs, or the audit log.

4. **No unsafe deserialisation:** Vector storage/retrieval uses fixed-width little-endian float32 via `encoding/binary` (no `encoding/gob`, no reflection-based decoding of untrusted bytes). `blobToVec()` validates `len(blob) % 4 == 0` and (per-row) that the decoded length equals the embedder's `Dim()` before use, preventing buffer overread. The optional `unsafe` fast path is gated behind the same length assertion.

5. **Model compatibility validation:** `validateModelCompat()` checks that the stored `model_name`/`embed_dim` in `route_classifier_meta` match the active embedder. A mismatch — e.g. switching from 384-dim local MiniLM to a 1536/3072-dim provider without a rebuild — returns a clear error and instructs `tag route train --force`, rather than silently comparing incompatible vectors and returning garbage similarity scores.

6. **YAML parsing safety:** `gopkg.in/yaml.v3` `Unmarshal` into a typed struct has no arbitrary-code / arbitrary-object construction path (the Go peer of "`yaml.safe_load` only"). Untrusted routing YAML cannot execute code.

7. **SQL injection:** All SQL uses parameterised queries via `?` placeholders through `database/sql`; no query string is assembled with `fmt.Sprintf`. Identifiers are never interpolated.

8. **Confidence as a gate, not a guarantee:** The confidence score is a cosine similarity ratio, not a calibrated probability. Operators should not use the classifier as the sole control for high-security routing. For security-critical task types, explicit `--task-type security` is recommended over `--auto-classify`.

9. **Embedding-model integrity:** For the provider path, trust is delegated to the provider over TLS (pin the model id in `route_classifier_meta`). For the offline build-tag path, the `all-MiniLM-L6-v2` weights are fetched/bundled at build time; operators in security-sensitive environments should vendor the weights and verify their `crypto/sha256` out-of-band before building the `offline_embed` binary.

---

## 11. Testing Strategy

Tests use Go's `testing` package with table-driven cases and `testing.B` benchmarks, against an in-memory `modernc.org/sqlite` store (`file::memory:?cache=shared`). Embedders are injected via the `Embedder` interface — a deterministic `stubEmbedder` (fixed vectors per keyword) makes classifier logic testable without a network call or the local model; a separate build-tagged suite (`//go:build offline_embed`) exercises the real MiniLM path.

### 11.1 Unit Tests

Location: `internal/routing/routing_test.go`

| Test | Description |
|------|-------------|
| `TestBlobRoundTrip` | Table-driven: `blobToVec(vecToBlob(v))` equals `v` for random float32 slices; length-guard rejects non-multiple-of-4 blobs |
| `TestStubEmbedderNormalised` | Assert stub vectors have `Dim()` length and L2 norm ≈ 1.0 (`math.Abs(norm-1) < 1e-6`) |
| `TestClassifyResultJSON` | `json.Marshal(ClassifyResult{})` contains all required keys; `runner_up` omitted when empty; floats round-half-even to 4 dp |
| `TestClassifySelectsCorrectType` | In-memory store, 3 task types × 3 stub examples; sub-tests assert `Classify` returns the correct type per example |
| `TestClassifyThreshold` | Table-driven over confidences; assert `AboveThreshold` boundary at exactly 0.60 |
| `TestClassifyEmptyTableError` | Assert `errors.Is(err, ErrNoExamples)` when `route_examples` is empty |
| `TestTrainUpsertSkipsExistingHash` | `force=false` skips rows whose `example_hash` exists; `force=true` re-embeds |
| `TestTrainDryRunNoWrites` | `dryRun=true`: row count unchanged and embedder never constructed (spy Embedder records zero calls) |
| `TestTrainNoExamplesSkipped` | Types without an `examples` field are skipped, no error (FR-11) |
| `TestCalibrateLOO` | 3 well-separated stub types; assert LOO `OverallAccuracy ≥ 0.90` |
| `TestModelCompatMismatch` | Seed meta `embed_dim=512`; assert `validateModelCompat` returns `ErrDimMismatch` |
| `TestPackageInitNoEmbedder` | Construct `Classifier`; assert embedder is nil until first `Classify`/`Train` (NFR-07 lazy `sync.Once`) |
| `TestNoEmbedderGraceful` | Stub `embed.New` returns nil; assert `getEmbedder` returns `ErrNoEmbedder` (no panic) with actionable message |
| `TestSQLParameterised` | Inject SQL metacharacters into task_type/example; assert no error and no injection |

### 11.2 Integration Tests

Location: `internal/routing/integration_test.go` (real store; embedder via stub by default, real MiniLM under `-tags offline_embed`)

| Test | Description |
|------|-------------|
| `TestTrainThenClassifyRoundtrip` | Full cycle: train from fixture YAML, classify known prompts, assert ≥ 85% accuracy |
| `TestAddExampleImprovesConfidence` | Classify an OOD prompt before/after `AddExample`; assert confidence increases |
| `TestRouteDecisionsPersisted` | After classify, assert one row in `route_decisions` with the correct `prompt_hash` |
| `TestNoPersistSkipsDB` | `--no-persist`; assert `route_decisions` count unchanged |
| `TestRouteClassifyJSON` | Drive the cobra command via `cmd.Execute()` with captured stdout; assert valid JSON with required keys |
| `TestRouteTrainIdempotent` | Run train twice; assert second run reports `ExamplesSkipped == ExamplesTotal` |
| `TestRouteCalibrateLOO` | Calibrate on fixture YAML; assert `OverallAccuracy ≥ 0.85` |
| `TestFallbackWhenNoEmbedder` | Inject an error-returning Embedder; assert classify returns `Method=="exact_match"` for a valid task type |
| `TestAutoClassifyInRun` | Fake classifier returning a known type; assert `run` consumes it |
| `TestSecretInPromptAborts` | Prompt containing a mock API key; assert non-zero exit and no decision row (before any embed) |

### 11.3 Benchmarks

Location: `internal/routing/bench_test.go` (`go test -bench=. -benchmem`)

| Benchmark | Target |
|------|--------|
| `BenchmarkClassifyWarm` (stub / local build tag) | Warm classify over ≤ 500 vectors; p50 < 15 ms, p99 < 50 ms (in-Go cosine dominates) |
| `BenchmarkClassifyProvider` | Classify against a stubbed provider round-trip; document p50 < 250 ms, p99 < 800 ms |
| `BenchmarkClassifyCold` (offline build tag) | First classify in a fresh process (`sync.Once` model init); wall time < 600 ms |
| `BenchmarkTrainThroughput` | Train on synthetic 50-type × 10-example YAML (stub); < 5 s |
| `BenchmarkClassify500` | 500 stored vectors; assert classify p99 < 100 ms (local path) |
| `BenchmarkBlobRoundTrip` | `-benchmem` on `vecToBlob`/`blobToVec` to confirm the copy path allocates once per call |

---

## 12. Acceptance Criteria

| ID | Criterion | Test Method |
|----|-----------|-------------|
| AC-01 | `tag route train --from-yaml routing.yaml` on a YAML with 8 task types and 47 examples completes in < 10 seconds on CPU and reports `examples_upserted: 47`. | Integration test |
| AC-02 | `tag route classify --prompt "Write a Python function" --json` returns `task_type: "coding"` with `confidence > 0.70` after training on a fixture YAML. | Integration test |
| AC-03 | `tag route classify --prompt "..."` with no reachable embedder exits 0 and prints a human-readable warning telling the operator to configure `routing.classifier.embed` or rebuild with `-tags offline_embed`. | Table-driven test with an error-returning stub Embedder |
| AC-04 | After `tag route add-example --task-type data-viz --example "Plot a scatter chart"`, the example appears in `route_examples` and `tag route list` shows `data-viz: N+1 examples`. | Integration test |
| AC-05 | `tag route classify --prompt "..."` with confidence < threshold exits 0, sets `above_threshold: false` in JSON, and prints a warning to stderr. | Unit test |
| AC-06 | Every `classify` call (without `--no-persist`) inserts exactly one row into `route_decisions` with the correct `prompt_hash` (SHA-256 of prompt), `predicted_type`, and `method`. | Integration test |
| AC-07 | `tag route train --dry-run` produces no rows in `route_examples` and does not load the embedding model. | Integration test |
| AC-08 | `tag route calibrate --loo` on the fixture YAML reports `overall_accuracy ≥ 0.85` and prints per-type F1 scores. | Integration test |
| AC-09 | Constructing a `routing.Classifier` does not build the embedder or open a network connection; the embedder is created lazily via `sync.Once` on first classify/train. | Unit test |
| AC-10 | `tag route classify` emits a span with `route.classifier = "embedding"` in the `traces` table when tracing is active. | Integration test |
| AC-11 | Running `tag route train` twice on the same YAML (without `--force`) reports `examples_skipped = examples_total` on the second run. | Integration test |
| AC-12 | A mismatch between stored `embed_dim` and current model dimension causes `classify` to exit with a non-zero code and an actionable error message. | Unit test |
| AC-13 | `tag run --auto-classify "Fix the failing OAuth test"` (with a reachable embedder and a trained index) resolves to the correct task type and proceeds to agent dispatch. | Integration test |
| AC-14 | A prompt containing a mock API key (`sk-test-...`) passed to `tag route classify` is rejected with exit code 1 before any embedding is computed (secret scanning guard). | Integration test |
| AC-15 | `tag route list --json --stats` output includes `avg_confidence` and `decisions_7d` fields for each task type with at least one entry in `route_decisions`. | Integration test |

---

## 13. Dependencies

| Dependency | Type | Version | Notes |
|------------|------|---------|-------|
| `modernc.org/sqlite` | Go module | GA | Pure-Go SQLite (FTS5/JSON1 compiled in, CGO_ENABLED=0); the single `internal/store` driver; holds `route_examples`/`route_decisions`/`route_classifier_meta` |
| `github.com/openai/openai-go/v3` | Go module | v3.41.x | Default `Embedder` impl (embeddings endpoint); `option.WithBaseURL` supports OpenAI-compatible / self-hosted endpoints |
| `github.com/nlpodyssey/cybertron` | Go module (build tag) | latest | Pure-Go MiniLM (`all-MiniLM-L6-v2`, 384-dim) for the `offline_embed` binary; slower, no network |
| `gopkg.in/yaml.v3` | Go module | v3 | Routing-YAML parsing (typed `Unmarshal`, no arbitrary-object exec) |
| `github.com/knadh/koanf/v2` | Go module | v2 | Config loading/decoding (`routing.classifier.*` + `embed`) |
| `go.opentelemetry.io/otel` | Go module | GA | Span emission in the classify path (PRD-013) |
| `github.com/spf13/cobra` | Go module | latest | `tag route` command tree |
| `github.com/google/uuid` | Go module | latest | `ex-`/`rd-` id generation |
| stdlib `crypto/sha256`, `encoding/binary`, `encoding/json`, `database/sql`, `sort`, `sync` | Go stdlib | — | Hashing, float32 BLOB (de)serialisation, JSON output, queries, ranking, lazy init/locking |
| Voyage / Cohere / Gemini HTTP | Go module/HTTP | — | Optional pluggable `Embedder` adapters (config-selected) |
| PRD-043 (vector tool retrieval) | Internal | — | Shares the `internal/memory/embed` `Embedder` iface + in-Go BLOB-cosine convention |
| PRD-013 (agent tracing) | Internal | — | Span emission in classify path (`go.opentelemetry.io/otel`) |
| PRD-034 (secret scanning) | Internal | — | Prompt scanning before embedding/transmission |
| PRD-027 (eval framework) | Internal | — | `calibrate` output can feed eval regression gating |
| PRD-012 (budget enforcement) | Internal | — | Provider-path classify is a billable embed; pre-run budget gate before `--auto-classify` |
| `internal/store` (WAL) | Runtime | — | Existing TAG database; the three route tables land as an `internal/store/migrate` migration |

---

## 14. Open Questions

| ID | Question | Owner | Resolution Target |
|----|----------|-------|-------------------|
| OQ-01 | Should `aggregation: top3_mean` be the permanent default, or should we collect production data from `route_decisions` before freezing it? The `max` aggregation may perform better for task types with only 1-2 examples. | Routing maintainer | After 2 weeks of production data |
| OQ-02 | Should `tag route train` be run automatically on first `tag run --auto-classify` if the table is empty, or should it always require explicit invocation? Automatic training is more ergonomic but hides the training step from operators. | CLI UX lead | Before AC-13 is written |
| OQ-03 | Should `route_decisions.prompt_hash` be salted with a user-specific secret to prevent rainbow-table attacks on task descriptions? The SHA-256 of a short, predictable prompt is easily reversed. | Security reviewer | Sprint 1 |
| OQ-04 | The `calibrate --loo` run embeds each example once per LOO iteration, which is O(N²) in the number of examples. For > 1,000 examples, this becomes slow. Should we add a `--fast` mode that uses pre-computed vectors? | Performance lead | After 500-example benchmark |
| OQ-05 | Should `tag route add-example` validate that the new example's nearest neighbour is the specified `task_type` (i.e., it is actually representative), or should it accept any text unconditionally? Validation adds latency; no validation risks degrading accuracy. | Product | Sprint 2 |
| OQ-06 | `all-MiniLM-L6-v2` was chosen for consistency with `tool_retrieval.py`. For task classification specifically, `all-mpnet-base-v2` (768-dim, 420 MB) achieves higher accuracy. Should PRD-103 support model selection, or lock to MiniLM for now? | ML lead | Before GA |
| OQ-07 | The current design stores one embedding per example. A future approach (mean pooling of type centroid) would reduce storage and lookup time but would require retraining after every `add-example`. Is the current per-example design the right long-term architecture? | Architecture | Q3 planning |
| OQ-08 | Should `--auto-classify` be opt-in (requires flag) or opt-out (enabled by default when an embedder is available)? Opt-out is more ergonomic but could silently change routing behaviour for existing users. | Product | Before Sprint 2 |

---

## 15. Complexity and Timeline

**Overall estimate:** M (8-10 working days)

### Phase 1 — Schema, Embedder, and Core Classifier (Days 1-3)

- Add the `route_examples`/`route_decisions`/`route_classifier_meta` migration under `internal/store/migrate`
- Define the `Embedder` interface in `internal/memory/embed` (if not already landed for PRD-043) + the default `openai-go` adapter; wire the `offline_embed` build-tag MiniLM factory
- Implement `internal/routing`: lazy `getEmbedder` (`sync.Once`), `embedText`, `vecToBlob`/`blobToVec` (length guard), `validateModelCompat`, `dot`/`aggregate`
- Implement `Classify()` with `top3_mean` aggregation and the round-half-even JSON helper
- Write core unit tests + `stubEmbedder` (`routing_test.go` items 1-10)
- Implement `TrainFromYAML()` (batched embeds, single tx) and the `route train` cobra command

**Deliverable:** `tag route train` and `tag route classify` functional end-to-end (default provider + stub)

### Phase 2 — Full CLI Surface and Persistence (Days 4-6)

- Implement `AddExample()` and the `route add-example` command
- Implement `route list` with the `--stats` query over `route_decisions`
- Add the `--no-persist` path; implement decision-row insertion
- Wire `go.opentelemetry.io/otel` span emission in `Classify()`
- Wire `internal/security` prompt scanning before embed/transmit
- Write integration tests (`integration_test.go` items 1-8)

**Deliverable:** All four subcommands functional; audit trail active; tracing active

### Phase 3 — Calibration and `--auto-classify` (Days 7-9)

- Implement `Calibrate()` with LOO mode (pre-loaded vectors, no re-embed) + `route calibrate`
- Add the `--auto-classify` flag to `run` and `queue add` (with the PRD-012 budget gate on the provider path)
- Add `routing.classifier.*` + `embed` koanf config decoding + docs
- Write `testing.B` benchmarks (`bench_test.go`); validate the local vs provider latency split
- Fix any regressions found by benchmarks

**Deliverable:** Feature-complete; all AC-01 through AC-15 passing

### Phase 4 — Hardening and Documentation (Day 10)

- Code review against security considerations (prompt transmission on the provider path; OQ-03 salt; OQ-02 auto-train)
- Edge-case tests: empty YAML, single-example types, model/dim mismatch, offline build-tag suite (`-tags offline_embed`)
- Update `tag route --help` strings and `tag doctor` to report the active embedder + offline availability
- Verify the default binary is CGO-free (`CGO_ENABLED=0` cross-compile) and the `offline_embed` binary builds
- Final pass on `--json` schema consistency across all four subcommands

**Deliverable:** PR ready for merge; all tests green; all AC passing

---

*GitHub Issue: #349*
*Cluster: G — Advanced Reasoning & Planning*
*Related PRDs: PRD-043 (tool retrieval), PRD-101 (self-consistency), PRD-102 (multi-agent debate)*

