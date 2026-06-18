# PRD-103: Dynamic Task-Type Classifier via Embeddings (vs Static YAML) (`tag route classify`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (1-2 weeks)
**Category:** Advanced Reasoning & Planning
**Affects:** `routing.py` (new), `src/tag/controller.py` (subcommand wiring)
**Depends on:** PRD-043 (vector-based tool retrieval — SentenceTransformer infrastructure), PRD-027 (eval framework — classifier quality scoring), PRD-028 (sandbox code execution), PRD-013 (agent tracing/observability), PRD-034 (secret scanning — prompt content before embedding), PRD-012 (cost tracking/budget), PRD-041 (OTel span cost attribution)
**Inspired by:** DSPy task classification, LangGraph routing, Semantic Kernel planners

---

## 1. Overview

TAG's routing system today maps user-supplied `--task-type` strings (e.g., `"coding"`, `"research"`, `"security"`) to profile configurations declared in a static YAML file. `resolve_route()` in `controller.py` performs a direct dictionary lookup: if the exact string is found in `cfg["routing"]["task_types"]`, the associated master profile, workers, and verifier are returned; if not, execution halts with a fatal error. This design is explicit and predictable, but it places an unreasonable maintenance burden on operators. Adding support for a new task type — `"data-viz"`, `"ml-training"`, `"devops-pipeline"` — requires a manual YAML edit, a config reload, and operator knowledge of every string alias a user might type. Misspellings, synonyms, and domain-specific terminology all produce hard failures rather than graceful nearest-neighbor resolution.

The field has moved past static string dispatch. RouteLLM (arXiv:2406.18665) demonstrates that a lightweight BERT-based binary router can decide model allocation in under 10 ms, outperforming keyword matching at every operating point on the MT-Bench routing curve. DSPy's `Predict` module allows declarative task-type classification from few examples without prompt engineering. LangGraph's router nodes use embedding similarity to dispatch graph edges. Semantic Kernel's planner selects skills via cosine similarity over skill descriptions. The common thread across all of these is: embed the task description, find the nearest known category, and dispatch — no static string required.

PRD-103 replaces TAG's static YAML lookup with a `SentenceTransformer` embedding classifier stored entirely in the local SQLite database. At training time (`tag route train`), the classifier embeds all task examples from the existing YAML routing table (one or more natural-language examples per task type) and stores the resulting vectors in a `route_examples` table. At classify time (`tag route classify`), the user's prompt is embedded with the same model, cosine similarity is computed against all stored examples, and the task type with the highest aggregate similarity score is returned — along with a confidence value and the runner-up. When confidence falls below a configurable threshold, the system emits a warning and falls back to the static YAML path or prompts the user to add examples.

The classifier is fully local: no API call, no network request, no LLM inference. The embedding model (`all-MiniLM-L6-v2`, 22 MB, already used by `tool_retrieval.py`) runs in-process via `sentence-transformers`. Cold-start latency (first encode after process start) is under 400 ms on CPU; warm latency is under 15 ms. The feature degrades gracefully when `sentence-transformers` is not installed: `tag route classify` prints an install hint and falls back to the existing exact-match behaviour. No existing workflows break.

The new subcommand surface is `tag route classify`, `tag route train`, `tag route add-example`, and an augmented `tag route list`. These integrate with TAG's existing tracing infrastructure (PRD-013) so each classify call emits a span with `route.classifier=embedding`, `route.task_type`, `route.confidence`, and `route.method` attributes for observability. Classify decisions are persisted to a `route_decisions` SQLite table for audit, cost attribution, and future fine-tuning of the classifier.

---

## 2. Problem Statement

### 2.1 Static YAML Dispatch Is Brittle and Unmaintainable at Scale

The current `resolve_route()` function performs an exact-string dictionary lookup against `cfg["routing"]["task_types"]`. This means that `--task-type coding` succeeds but `--task-type code`, `--task-type "write code"`, and `--task-type python` all fail with a fatal `SystemExit`. Operators must maintain a complete enumeration of every string alias users might supply, which is impossible in practice. As TAG deployments grow — enterprise teams with 20+ task types, shared configs with dozens of profiles — the YAML routing table becomes a maintenance bottleneck. Adding a single new capability (e.g., `data-visualization`) requires: (a) deciding on the canonical string, (b) updating the YAML, (c) communicating the exact string to all users, and (d) handling the inevitable misspellings manually. Every YAML edit also risks breaking existing routes through YAML formatting errors or key name collisions.

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
| G5 | The classifier is fully local: no LLM API call, no network request. Embedding uses `all-MiniLM-L6-v2` (same model as `tool_retrieval.py`, no additional download if already cached). |
| G6 | When `sentence-transformers` is not installed, all four subcommands degrade gracefully: `classify` falls back to exact-match YAML lookup, and the other three print an install hint with exit code 0. |
| G7 | Confidence threshold is configurable (`routing.classifier.confidence_threshold`, default 0.60). When confidence falls below threshold, a warning is emitted and `resolve_route()` is used as fallback. |
| G8 | Every classify call persists a row to the `route_decisions` SQLite table with prompt hash, predicted type, confidence, runner-up, and latency — enabling audit and drift detection. |
| G9 | `tag route classify` emits an OTel-compatible span (PRD-013) with `route.classifier`, `route.task_type`, `route.confidence`, `route.method`, and `route.latency_ms` attributes. |
| G10 | `tag run` and `tag queue add` accept `--auto-classify` flag that routes to task_type via the embedding classifier when `--task-type` is omitted, enabling fully natural-language task dispatch. |
| G11 | `tag route calibrate` evaluates classifier accuracy against a labelled YAML test set and prints per-type precision, recall, and F1 — integrating with PRD-027 eval framework for regression gating. |

### 3.2 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Fine-tuning or retraining the `sentence-transformers` base model. The classifier uses the pre-trained `all-MiniLM-L6-v2` weights as a frozen encoder; the only "training" is embedding the few-shot examples. |
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
| Classify latency (warm) | p50 < 15 ms, p99 < 50 ms | Benchmark: 100 classify calls after model warmup, `time.perf_counter()` |
| Classify latency (cold) | p50 < 400 ms | Benchmark: first call in fresh process |
| Accuracy on YAML examples (leave-one-out) | ≥ 90% on configs with ≥ 3 examples per type | `tag route calibrate --loo` |
| False-fallback rate | < 5% of classify calls fall back to exact-match when embedding available | `route_decisions` table: `method='fallback'` / total |
| Graceful degradation | `tag route classify` exits 0 with human-readable warning when `sentence-transformers` absent | Unit test with mocked ImportError |
| Import isolation | `import tag.routing` does not import `sentence_transformers` at module level | `sys.modules` assertion in unit test |
| Backward compatibility | All existing `tag route --task-type <exact>` calls continue to work identically | Integration test suite against existing YAML configs |
| Example persistence | `tag route add-example` row immediately visible in `tag route list` output | Integration test |
| Span emission | Each classify call produces one span in `traces` table with `route.classifier` attribute | Integration test |
| Train throughput | `tag route train` on 50-type / 10-example-per-type YAML completes in < 5 seconds on CPU | Benchmark with synthetic YAML |

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
| U8 | Operator | run `tag route classify` when `sentence-transformers` is not installed | I get a clear install hint and the system falls back to exact-match routing rather than crashing |
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
| FR-01 | `tag route train --from-yaml <path>` parses a YAML routing config, extracts per-task-type `examples` lists, embeds each with `all-MiniLM-L6-v2`, and upserts results into `route_examples` table with `ON CONFLICT(task_type, example_hash) DO UPDATE`. | Must |
| FR-02 | `tag route classify --prompt <text>` embeds the prompt, computes cosine similarity against all rows in `route_examples`, groups scores by task_type (mean aggregation over all examples for that type), returns the highest-scoring type. | Must |
| FR-03 | When the top-1 confidence is below `routing.classifier.confidence_threshold` (default 0.60), `classify` emits a warning to stderr and sets `"above_threshold": false` in JSON output. | Must |
| FR-04 | Every call to `tag route classify` (unless `--no-persist`) inserts a row into `route_decisions` with prompt_hash (SHA-256 of prompt), predicted_type, confidence, runner_up, runner_up_confidence, method, latency_ms, and created_at. | Must |
| FR-05 | `tag route add-example --task-type <type> --example <text>` embeds the example, inserts into `route_examples`, and returns the new example's assigned ID. The task type does not need to already have examples in the table. | Must |
| FR-06 | `tag route list` reads task types from both the YAML config (for source-of-truth type names) and the `route_examples` table (for example counts), producing a merged view. Types in YAML but not yet in the table show 0 examples. | Must |
| FR-07 | When `sentence-transformers` is not importable, `tag route classify` falls back to exact-match resolution via `resolve_route()` and sets `method="exact_match"` in the decision row. It does not crash. | Must |
| FR-08 | `tag route train --dry-run` parses the YAML, counts examples per type, prints the plan, and exits 0 without writing to the database or loading the embedding model. | Must |
| FR-09 | `tag route calibrate --loo` performs leave-one-out evaluation: for each example `e` of type `T`, removes `e` from the index, classifies `e`, checks if prediction equals `T`, then re-inserts `e`. Reports per-type precision, recall, F1, and overall accuracy. | Should |
| FR-10 | `tag run --auto-classify` and `tag queue add --auto-classify` call `classify_task_type(cfg, prompt)` when `--task-type` is omitted. If confidence is above threshold, the predicted type is used; otherwise, execution fails with an actionable error message. | Should |
| FR-11 | The YAML `examples` field under each task type is a list of strings. If a task type has no `examples` field, `tag route train` skips it with a warning but does not fail. | Must |
| FR-12 | `tag route train --force` re-embeds all examples, replacing existing vectors. Without `--force`, examples whose `example_hash` already exists in `route_examples` are skipped. | Should |
| FR-13 | All four subcommands support `--json` output and print structured JSON to stdout with a consistent schema. Human-readable output goes to stdout; warnings go to stderr. | Must |
| FR-14 | The embedding model is loaded lazily on first use. `import tag.routing` at module level does not import `sentence_transformers`. | Must |
| FR-15 | `tag route classify` emits an OpenTelemetry-compatible span via `tracing.py` (PRD-013) with attributes: `route.classifier`, `route.task_type`, `route.confidence`, `route.runner_up`, `route.method`, `route.latency_ms`. | Should |
| FR-16 | The `route_examples` table stores the embedding vector as a BLOB (serialised with `numpy.tobytes()` in float32 dtype). The vector dimension is stored in a `route_classifier_meta` table to validate model compatibility on load. | Must |
| FR-17 | If the stored embedding dimension does not match the current model's output dimension (e.g., after a model change), `tag route classify` prints an error and instructs the user to run `tag route train --force`. | Must |
| FR-18 | Per-type score aggregation in `classify` uses the mean of the top-3 example cosine similarities for each type (not mean of all examples), reducing the influence of outlier examples. The aggregation strategy is configurable (`routing.classifier.aggregation`: `mean`, `max`, `top3_mean`; default `top3_mean`). | Should |
| FR-19 | `tag route list --stats` queries `route_decisions` for decisions in the past 7 days, computes per-type average confidence and p50 confidence, and flags types whose average is below threshold. | Should |
| FR-20 | Secret scanning (PRD-034) is applied to the `--prompt` value before it is embedded or persisted. If a secret pattern is detected, classification is aborted with a clear error. | Must |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Classify warm latency | p50 < 15 ms, p99 < 50 ms on CPU (Mac M-series or Linux x86-64) with ≤ 500 stored examples |
| NFR-02 | Classify cold latency | p50 < 400 ms for first call in fresh process (model load from cache) |
| NFR-03 | Train throughput | ≤ 5 seconds wall time for 50 task types × 10 examples on CPU |
| NFR-04 | Memory footprint | `all-MiniLM-L6-v2` uses ~90 MB RAM resident after load; acceptable for CLI invocation |
| NFR-05 | SQLite storage | 384-dim float32 vector = 1,536 bytes per example; 500 examples ≈ 750 KB — negligible |
| NFR-06 | Backward compatibility | All existing `tag route --task-type <exact>` calls continue to work without change |
| NFR-07 | Import isolation | Module-level import of `tag.routing` must not import `sentence_transformers` |
| NFR-08 | Thread safety | The embedding model is not thread-safe; all encode calls are serialised via a module-level `threading.Lock()`. DAG parallelism (PRD-033) must not issue concurrent classify calls without acquiring the lock. |
| NFR-09 | Model determinism | `SentenceTransformer.encode()` with `normalize_embeddings=True` is deterministic for a given input string and model version. Stored vectors are stable across process restarts. |
| NFR-10 | Disk persistence | `route_examples` and `route_decisions` survive process restarts; they live in the standard TAG SQLite database at `~/.tag/runtime/tag.sqlite3` opened via `open_db()`. |
| NFR-11 | No network calls | Neither classify nor train makes any network request. Model weights are loaded from the local HuggingFace cache. If the model is not cached, `tag route train` emits a one-time download prompt. |
| NFR-12 | Graceful degradation | If `sentence-transformers` is absent, affected subcommands exit 0 with a human-readable warning and install instructions; they do not raise unhandled exceptions. |

---

## 9. Technical Design

### 9.1 New File: `src/tag/routing.py`

This module owns all embedding classifier logic. `controller.py` imports from it lazily (inside command functions, not at module level).

```python
# src/tag/routing.py
"""PRD-103: Dynamic Task-Type Classifier via Embeddings.

Replaces static YAML task_type lookup with a SentenceTransformer
embedding nearest-neighbour classifier stored in SQLite.

Optional dep: sentence-transformers
  pip install sentence-transformers
"""
from __future__ import annotations

import hashlib
import sqlite3
import threading
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np

_ST_AVAILABLE = False
_model_lock = threading.Lock()
_model_cache: Any = None  # SentenceTransformer instance, loaded lazily

try:
    from sentence_transformers import SentenceTransformer as _ST
    _ST_AVAILABLE = True
except ImportError:
    pass

EMBED_MODEL_NAME = "all-MiniLM-L6-v2"
EMBED_DIM = 384
DEFAULT_THRESHOLD = 0.60
DEFAULT_AGGREGATION = "top3_mean"
META_TABLE_VERSION = 1


# ---------------------------------------------------------------------------
# Dataclasses
# ---------------------------------------------------------------------------

@dataclass
class ClassifyResult:
    """Result of a single classify call."""
    task_type: str
    confidence: float
    runner_up: str | None
    runner_up_confidence: float | None
    method: str              # "embedding" | "exact_match" | "fallback"
    latency_ms: float
    above_threshold: bool
    candidates: list[dict]   # [{"task_type": str, "score": float}]
    decision_id: str = field(default_factory=lambda: f"rd-{uuid.uuid4().hex[:10]}")

    def to_dict(self) -> dict:
        return {
            "task_type": self.task_type,
            "confidence": round(self.confidence, 4),
            "runner_up": self.runner_up,
            "runner_up_confidence": (
                round(self.runner_up_confidence, 4)
                if self.runner_up_confidence is not None else None
            ),
            "method": self.method,
            "latency_ms": round(self.latency_ms, 1),
            "above_threshold": self.above_threshold,
            "candidates": self.candidates,
            "decision_id": self.decision_id,
        }


@dataclass
class TrainResult:
    """Result of a train call."""
    task_types: int
    examples_total: int
    examples_upserted: int
    examples_skipped: int
    model: str
    latency_ms: float
    source: str

    def to_dict(self) -> dict:
        return {
            "task_types": self.task_types,
            "examples_total": self.examples_total,
            "examples_upserted": self.examples_upserted,
            "examples_skipped": self.examples_skipped,
            "model": self.model,
            "latency_ms": round(self.latency_ms, 1),
            "source": self.source,
        }


@dataclass
class CalibrationResult:
    """Per-type and aggregate calibration metrics."""
    per_type: list[dict]     # [{task_type, precision, recall, f1, support}]
    overall_accuracy: float
    macro_f1: float
    above_threshold_pct: float
    n_examples: int
    mode: str                # "loo" | "full"

    def to_dict(self) -> dict:
        return {
            "per_type": self.per_type,
            "overall_accuracy": round(self.overall_accuracy, 4),
            "macro_f1": round(self.macro_f1, 4),
            "above_threshold_pct": round(self.above_threshold_pct, 4),
            "n_examples": self.n_examples,
            "mode": self.mode,
        }
```

### 9.2 SQLite DDL

New tables are created via `ensure_classifier_tables(conn)` called from the `open_db()` post-connection hook in `controller.py`, consistent with how all other feature tables are initialised.

```sql
-- Stores embedded examples for the task-type classifier.
CREATE TABLE IF NOT EXISTS route_examples (
    id           TEXT PRIMARY KEY,          -- "ex-{uuid8}"
    task_type    TEXT NOT NULL,             -- e.g. "coding", "data-viz"
    example_text TEXT NOT NULL,
    example_hash TEXT NOT NULL,             -- SHA-256(example_text)
    vector_blob  BLOB NOT NULL,             -- float32 numpy array, 384 dims
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
-- Populated on first train:
-- ('model_name', 'all-MiniLM-L6-v2')
-- ('embed_dim',  '384')
-- ('schema_version', '1')
-- ('trained_at', '<ISO8601>')

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

#### 9.3.1 `get_embed_model()` — Lazy Model Loading

```python
def get_embed_model():
    """Load the SentenceTransformer model, caching it in-process."""
    global _model_cache
    if not _ST_AVAILABLE:
        raise ImportError(
            "sentence-transformers is required for the embedding classifier.\n"
            "Install with: pip install sentence-transformers"
        )
    if _model_cache is None:
        with _model_lock:
            if _model_cache is None:  # double-checked locking
                _model_cache = _ST(EMBED_MODEL_NAME)
    return _model_cache


def embed_text(text: str) -> np.ndarray:
    """Return normalised float32 embedding vector for text."""
    model = get_embed_model()
    with _model_lock:
        vec = model.encode(
            [text],
            normalize_embeddings=True,
            show_progress_bar=False,
            convert_to_numpy=True,
        )[0]
    return vec.astype(np.float32)


def vec_to_blob(v: np.ndarray) -> bytes:
    return v.astype(np.float32).tobytes()


def blob_to_vec(b: bytes) -> np.ndarray:
    return np.frombuffer(b, dtype=np.float32)
```

#### 9.3.2 `classify_task_type()` — Core Classifier

```python
def classify_task_type(
    conn: sqlite3.Connection,
    prompt: str,
    threshold: float = DEFAULT_THRESHOLD,
    top_k: int = 3,
    aggregation: str = DEFAULT_AGGREGATION,  # "mean" | "max" | "top3_mean"
) -> ClassifyResult:
    """Classify a prompt to a task type using embedding cosine similarity."""
    t0 = time.perf_counter()

    # Validate model compatibility
    _validate_model_compat(conn)

    # Embed query
    query_vec = embed_text(prompt)

    # Load all example vectors from SQLite
    rows = conn.execute(
        "SELECT task_type, vector_blob FROM route_examples ORDER BY task_type"
    ).fetchall()

    if not rows:
        raise RuntimeError(
            "No examples in route_examples table. "
            "Run: tag route train --from-yaml routing.yaml"
        )

    # Group by task_type, compute per-type aggregate score
    from collections import defaultdict
    type_vecs: dict[str, list[np.ndarray]] = defaultdict(list)
    for task_type, blob in rows:
        type_vecs[task_type].append(blob_to_vec(blob))

    type_scores: dict[str, float] = {}
    for task_type, vecs in type_vecs.items():
        # Cosine similarity (vectors are already normalised)
        sims = [float(np.dot(query_vec, v)) for v in vecs]
        sims.sort(reverse=True)
        if aggregation == "max":
            type_scores[task_type] = sims[0]
        elif aggregation == "top3_mean":
            type_scores[task_type] = float(np.mean(sims[:3]))
        else:  # mean
            type_scores[task_type] = float(np.mean(sims))

    # Sort by score descending
    ranked = sorted(type_scores.items(), key=lambda x: x[1], reverse=True)
    top_type, top_score = ranked[0]
    runner_up = ranked[1][0] if len(ranked) > 1 else None
    runner_up_score = ranked[1][1] if len(ranked) > 1 else None

    latency_ms = (time.perf_counter() - t0) * 1000
    candidates = [
        {"task_type": t, "score": round(s, 4)}
        for t, s in ranked[:top_k]
    ]

    return ClassifyResult(
        task_type=top_type,
        confidence=top_score,
        runner_up=runner_up,
        runner_up_confidence=runner_up_score,
        method="embedding",
        latency_ms=latency_ms,
        above_threshold=top_score >= threshold,
        candidates=candidates,
    )
```

#### 9.3.3 `train_from_yaml()` — Bulk Training

```python
def train_from_yaml(
    conn: sqlite3.Connection,
    yaml_path: Path,
    force: bool = False,
    dry_run: bool = False,
) -> TrainResult:
    """Embed all YAML routing examples and upsert into route_examples."""
    import yaml as _yaml
    t0 = time.perf_counter()

    with open(yaml_path) as f:
        data = _yaml.safe_load(f)

    routing = data.get("routing", data).get("task_types", data)
    if not routing:
        raise ValueError(f"No task_types found in {yaml_path}")

    upserted = 0
    skipped = 0
    total = 0

    for task_type, type_cfg in routing.items():
        examples = type_cfg.get("examples", []) if isinstance(type_cfg, dict) else []
        if not examples:
            continue
        for ex_text in examples:
            total += 1
            ex_hash = hashlib.sha256(ex_text.encode()).hexdigest()
            if not force:
                exists = conn.execute(
                    "SELECT 1 FROM route_examples WHERE task_type=? AND example_hash=?",
                    (task_type, ex_hash),
                ).fetchone()
                if exists:
                    skipped += 1
                    continue
            if dry_run:
                upserted += 1
                continue
            vec = embed_text(ex_text)
            ex_id = f"ex-{uuid.uuid4().hex[:10]}"
            conn.execute(
                """INSERT INTO route_examples(id, task_type, example_text, example_hash, vector_blob, source)
                   VALUES(?,?,?,?,?,?)
                   ON CONFLICT(task_type, example_hash) DO UPDATE SET
                     vector_blob=excluded.vector_blob,
                     source=excluded.source""",
                (ex_id, task_type, ex_text, ex_hash, vec_to_blob(vec), "yaml"),
            )
            upserted += 1

    if not dry_run:
        conn.commit()
        _write_meta(conn)

    n_types = len([t for t, c in routing.items()
                   if isinstance(c, dict) and c.get("examples")])
    return TrainResult(
        task_types=n_types,
        examples_total=total,
        examples_upserted=upserted,
        examples_skipped=skipped,
        model=EMBED_MODEL_NAME,
        latency_ms=(time.perf_counter() - t0) * 1000,
        source=str(yaml_path),
    )
```

#### 9.3.4 `calibrate()` — Leave-One-Out Evaluation

```python
def calibrate(
    conn: sqlite3.Connection,
    threshold: float = DEFAULT_THRESHOLD,
    loo: bool = True,
    aggregation: str = DEFAULT_AGGREGATION,
) -> CalibrationResult:
    """Evaluate classifier accuracy via leave-one-out or full-set evaluation."""
    rows = conn.execute(
        "SELECT id, task_type, example_text, vector_blob FROM route_examples"
    ).fetchall()

    from collections import defaultdict
    correct = 0
    above_thresh = 0
    per_type_tp: dict[str, int] = defaultdict(int)
    per_type_fp: dict[str, int] = defaultdict(int)
    per_type_fn: dict[str, int] = defaultdict(int)
    per_type_support: dict[str, int] = defaultdict(int)

    all_types = list({r[1] for r in rows})

    for row in rows:
        ex_id, true_type, ex_text, ex_blob = row
        per_type_support[true_type] += 1
        query_vec = blob_to_vec(ex_blob)

        # LOO: exclude current example from index
        if loo:
            candidate_rows = [(r[1], blob_to_vec(r[3])) for r in rows if r[0] != ex_id]
        else:
            candidate_rows = [(r[1], blob_to_vec(r[3])) for r in rows]

        type_vecs: dict[str, list] = defaultdict(list)
        for t, v in candidate_rows:
            type_vecs[t].append(v)

        if not type_vecs:
            continue

        type_scores = {}
        for t, vecs in type_vecs.items():
            sims = sorted([float(np.dot(query_vec, v)) for v in vecs], reverse=True)
            if aggregation == "top3_mean":
                type_scores[t] = float(np.mean(sims[:3]))
            elif aggregation == "max":
                type_scores[t] = sims[0]
            else:
                type_scores[t] = float(np.mean(sims))

        ranked = sorted(type_scores.items(), key=lambda x: x[1], reverse=True)
        pred_type, pred_score = ranked[0]

        if pred_score >= threshold:
            above_thresh += 1
        if pred_type == true_type:
            correct += 1
            per_type_tp[true_type] += 1
        else:
            per_type_fp[pred_type] += 1
            per_type_fn[true_type] += 1

    per_type_results = []
    f1_sum = 0.0
    for t in sorted(all_types):
        tp = per_type_tp[t]
        fp = per_type_fp[t]
        fn = per_type_fn[t]
        precision = tp / (tp + fp) if (tp + fp) > 0 else 0.0
        recall = tp / (tp + fn) if (tp + fn) > 0 else 0.0
        f1 = (2 * precision * recall / (precision + recall)
               if (precision + recall) > 0 else 0.0)
        f1_sum += f1
        per_type_results.append({
            "task_type": t,
            "precision": round(precision, 4),
            "recall": round(recall, 4),
            "f1": round(f1, 4),
            "support": per_type_support[t],
        })

    n = len(rows)
    return CalibrationResult(
        per_type=per_type_results,
        overall_accuracy=correct / n if n > 0 else 0.0,
        macro_f1=f1_sum / len(all_types) if all_types else 0.0,
        above_threshold_pct=above_thresh / n if n > 0 else 0.0,
        n_examples=n,
        mode="loo" if loo else "full",
    )
```

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

#### 9.5.1 `controller.py` — New Command Wiring

```python
# In controller.py build_parser():

# Subcommand: tag route classify
route_classify = route_sub.add_parser("classify", help="Classify a prompt to a task type")
route_classify.add_argument("--prompt", required=True)
route_classify.add_argument("--threshold", type=float, default=None)
route_classify.add_argument("--top-k", type=int, default=3)
route_classify.add_argument("--json", action="store_true")
route_classify.add_argument("--no-persist", action="store_true")
route_classify.set_defaults(func=cmd_route_classify)

# Subcommand: tag route train
route_train = route_sub.add_parser("train", help="Embed YAML examples into classifier")
route_train.add_argument("--from-yaml", dest="from_yaml", default=None)
route_train.add_argument("--force", action="store_true")
route_train.add_argument("--dry-run", action="store_true")
route_train.add_argument("--json", action="store_true")
route_train.set_defaults(func=cmd_route_train)

# Subcommand: tag route add-example
route_add_ex = route_sub.add_parser("add-example", help="Add a classifier training example")
route_add_ex.add_argument("--task-type", required=True)
route_add_ex.add_argument("--example", required=True)
route_add_ex.add_argument("--json", action="store_true")
route_add_ex.set_defaults(func=cmd_route_add_example)
```

#### 9.5.2 `tracing.py` Integration (PRD-013)

```python
# In routing.py classify_task_type(), after computing result:
try:
    from tag.tracing import get_tracer
    tracer = get_tracer("tag.routing")
    with tracer.start_as_current_span("route.classify") as span:
        span.set_attribute("route.classifier", "embedding")
        span.set_attribute("route.task_type", result.task_type)
        span.set_attribute("route.confidence", result.confidence)
        span.set_attribute("route.runner_up", result.runner_up or "")
        span.set_attribute("route.method", result.method)
        span.set_attribute("route.latency_ms", result.latency_ms)
        span.set_attribute("route.above_threshold", result.above_threshold)
except ImportError:
    pass  # tracing optional
```

#### 9.5.3 `security.py` Integration (PRD-034)

```python
# In cmd_route_classify(), before calling classify_task_type():
try:
    from tag.security import scan_for_secrets
    findings = scan_for_secrets(args.prompt)
    if findings:
        print_error(f"Secret detected in prompt: {findings[0].rule_id}. Aborting.")
        return 1
except ImportError:
    pass
```

#### 9.5.4 `--auto-classify` in `cmd_run()` and `cmd_queue_add()`

```python
# In cmd_run(), before resolve_route():
task_type = args.task_type
if not task_type and getattr(args, "auto_classify", False):
    if not _ST_AVAILABLE:
        print_error("--auto-classify requires sentence-transformers: pip install sentence-transformers")
        return 1
    from tag import routing as _routing
    db = open_db(cfg)
    result = _routing.classify_task_type(db, prompt, threshold=threshold)
    if not result.above_threshold:
        print_error(
            f"Auto-classify confidence {result.confidence:.2f} below threshold "
            f"{threshold:.2f} for '{result.task_type}'. "
            f"Specify --task-type explicitly or add more examples."
        )
        return 1
    task_type = result.task_type
    if not args.json:
        print_warning(f"Auto-classified as '{task_type}' (confidence {result.confidence:.3f})")
```

### 9.6 Config Schema Extension

```yaml
# ~/.tag/config.yaml — new optional section
routing:
  classifier:
    enabled: true                     # bool, default true
    confidence_threshold: 0.60        # float, default 0.60
    aggregation: top3_mean            # "mean" | "max" | "top3_mean"
    model: all-MiniLM-L6-v2           # str, future: allow overriding
    fallback_to_exact_match: true     # bool: fallback on low confidence
  task_types:
    coding:
      master: coder
      examples:
        - "..."
```

---

## 10. Security Considerations

1. **Prompt content in `route_decisions`:** Only the SHA-256 hash of the prompt is stored in `route_decisions`, not the raw prompt text. This prevents sensitive task descriptions (which might contain business logic, PII, or credentials) from accumulating in the audit log. The prompt hash is sufficient for deduplication and drift analysis.

2. **Secret scanning before embedding:** PRD-034 secret patterns are applied to the `--prompt` value before the text is passed to the embedding model or persisted in any form. If a high-entropy secret or known credential pattern is detected, classification is aborted with exit code 1. This prevents secret exfiltration via embedding vectors or log entries.

3. **No pickle deserialization:** Unlike LangGraph's `_freeze()` which uses pickle (GHSA-mhr3-j7m5-c7c9), all vector storage and retrieval in PRD-103 uses `numpy.tobytes()` / `numpy.frombuffer()` with a known fixed dtype (`float32`). There is no pickle deserialization path. The `blob_to_vec()` function validates that the blob length equals `EMBED_DIM * 4` bytes before constructing the array, preventing buffer overread.

4. **Model compatibility validation:** `_validate_model_compat(conn)` checks that the stored `embed_dim` and `model_name` in `route_classifier_meta` match the current runtime values. A mismatch — which could occur if the model is changed without retraining — raises a clear error rather than silently returning garbage similarity scores. This prevents misrouting due to stale vectors.

5. **YAML parsing safety:** `yaml.safe_load()` is used exclusively; `yaml.load()` is never used. This prevents arbitrary Python object deserialization from untrusted routing YAML files.

6. **SQLite injection:** All SQL operations use parameterised queries via the `?` placeholder. No string formatting of SQL is performed anywhere in `routing.py`.

7. **Confidence as a gate, not a guarantee:** The confidence score is a cosine similarity ratio, not a calibrated probability. Operators should not use the classifier as the sole control for high-security routing decisions. For security-critical task types, explicit `--task-type security` is recommended over `--auto-classify`.

8. **Model weight integrity:** The `all-MiniLM-L6-v2` model is downloaded from HuggingFace Hub on first use. TAG does not verify a cryptographic hash of the downloaded weights. Operators in security-sensitive environments should pre-download and pin the model weights to a local path via the HuggingFace `TRANSFORMERS_CACHE` environment variable and verify the SHA-256 of the model file out-of-band.

---

## 11. Testing Strategy

### 11.1 Unit Tests

Location: `tests/test_routing.py`

| Test | Description |
|------|-------------|
| `test_vec_to_blob_roundtrip` | Assert `blob_to_vec(vec_to_blob(v))` equals `v` for a random float32 array |
| `test_embed_text_shape` | Assert `embed_text("hello").shape == (384,)` and L2 norm ≈ 1.0 |
| `test_embed_text_deterministic` | Assert `embed_text(s) == embed_text(s)` for two calls |
| `test_classify_result_to_dict` | Assert all required keys present in `ClassifyResult.to_dict()` |
| `test_classify_selects_correct_type` | Create in-memory SQLite with 3 task types, 3 examples each; assert `classify_task_type` returns correct type for each example |
| `test_classify_above_threshold` | Assert `above_threshold=True` when confidence ≥ 0.60 |
| `test_classify_below_threshold` | Assert `above_threshold=False` when confidence < 0.60 with sparse examples |
| `test_classify_empty_table_raises` | Assert `RuntimeError` raised when `route_examples` is empty |
| `test_train_from_yaml_upsert` | Assert `train_from_yaml` with `force=False` skips existing hashes |
| `test_train_from_yaml_dry_run` | Assert no DB writes occur with `dry_run=True` |
| `test_train_from_yaml_no_examples` | Assert types without `examples` field are skipped, no exception |
| `test_calibrate_loo_perfect` | With 3 well-separated types, assert LOO accuracy ≥ 0.90 |
| `test_model_compat_mismatch` | Insert stale `embed_dim=512` in meta table; assert `_validate_model_compat` raises |
| `test_import_isolation` | `import tag.routing; assert 'sentence_transformers' not in sys.modules` |
| `test_st_unavailable_graceful` | Mock `_ST_AVAILABLE = False`; assert `get_embed_model()` raises `ImportError` with install hint |
| `test_sql_parameterised` | Inject SQL metacharacters in task_type/example; assert no SQL error, no injection |

### 11.2 Integration Tests

Location: `tests/test_routing_integration.py`

| Test | Description |
|------|-------------|
| `test_train_then_classify_roundtrip` | Full cycle: load real model, train from fixture YAML, classify known prompts, assert ≥ 85% accuracy |
| `test_add_example_improves_accuracy` | Classify an out-of-distribution prompt pre/post `add_example`; assert confidence increases |
| `test_route_decisions_persisted` | After classify, assert row in `route_decisions` with correct prompt_hash |
| `test_no_persist_skips_db` | `--no-persist` flag; assert `route_decisions` count unchanged |
| `test_cmd_route_classify_json` | Invoke via `subprocess`; assert valid JSON output with required keys |
| `test_cmd_route_train_idempotent` | Run train twice; assert second run reports `examples_skipped = examples_total` |
| `test_cmd_route_calibrate_loo` | Run calibrate on fixture YAML; assert `overall_accuracy ≥ 0.85` |
| `test_fallback_when_st_absent` | Patch `_ST_AVAILABLE = False` in routing module; assert classify returns `method="exact_match"` for valid task-type |
| `test_auto_classify_in_cmd_run` | Mock `classify_task_type` returning known type; assert `cmd_run` uses it |
| `test_secret_in_prompt_aborts` | Pass prompt containing mock API key; assert exit code 1, no decision row |

### 11.3 Performance Tests

Location: `tests/test_routing_perf.py`

| Test | Target |
|------|--------|
| `bench_classify_warm_latency` | Warm-model classify over 100 calls; assert p50 < 15 ms, p99 < 50 ms |
| `bench_classify_cold_latency` | First classify in fresh subprocess; assert wall time < 400 ms |
| `bench_train_throughput` | Train on synthetic 50-type × 10-example YAML; assert < 5 seconds |
| `bench_memory_footprint` | Assert RSS delta after model load < 150 MB |
| `bench_500_examples_classify` | Populate 500 examples; assert classify latency p99 < 100 ms |

---

## 12. Acceptance Criteria

| ID | Criterion | Test Method |
|----|-----------|-------------|
| AC-01 | `tag route train --from-yaml routing.yaml` on a YAML with 8 task types and 47 examples completes in < 10 seconds on CPU and reports `examples_upserted: 47`. | Integration test |
| AC-02 | `tag route classify --prompt "Write a Python function" --json` returns `task_type: "coding"` with `confidence > 0.70` after training on a fixture YAML. | Integration test |
| AC-03 | `tag route classify --prompt "..."` with `sentence-transformers` absent exits 0 and prints a human-readable warning including "pip install sentence-transformers". | Unit test with mocked ImportError |
| AC-04 | After `tag route add-example --task-type data-viz --example "Plot a scatter chart"`, the example appears in `route_examples` and `tag route list` shows `data-viz: N+1 examples`. | Integration test |
| AC-05 | `tag route classify --prompt "..."` with confidence < threshold exits 0, sets `above_threshold: false` in JSON, and prints a warning to stderr. | Unit test |
| AC-06 | Every `classify` call (without `--no-persist`) inserts exactly one row into `route_decisions` with the correct `prompt_hash` (SHA-256 of prompt), `predicted_type`, and `method`. | Integration test |
| AC-07 | `tag route train --dry-run` produces no rows in `route_examples` and does not load the embedding model. | Integration test |
| AC-08 | `tag route calibrate --loo` on the fixture YAML reports `overall_accuracy ≥ 0.85` and prints per-type F1 scores. | Integration test |
| AC-09 | `import tag.routing` does not import `sentence_transformers` at module level (`sys.modules` assertion). | Unit test |
| AC-10 | `tag route classify` emits a span with `route.classifier = "embedding"` in the `traces` table when tracing is active. | Integration test |
| AC-11 | Running `tag route train` twice on the same YAML (without `--force`) reports `examples_skipped = examples_total` on the second run. | Integration test |
| AC-12 | A mismatch between stored `embed_dim` and current model dimension causes `classify` to exit with a non-zero code and an actionable error message. | Unit test |
| AC-13 | `tag run --auto-classify "Fix the failing OAuth test"` (with sentence-transformers installed and trained index) resolves to the correct task type and proceeds to agent dispatch. | Integration test |
| AC-14 | A prompt containing a mock API key (`sk-test-...`) passed to `tag route classify` is rejected with exit code 1 before any embedding is computed (secret scanning guard). | Integration test |
| AC-15 | `tag route list --json --stats` output includes `avg_confidence` and `decisions_7d` fields for each task type with at least one entry in `route_decisions`. | Integration test |

---

## 13. Dependencies

| Dependency | Type | Version | Notes |
|------------|------|---------|-------|
| `sentence-transformers` | Python package (optional) | ≥ 2.2.0 | Already used by `tool_retrieval.py`; no new download if cached |
| `numpy` | Python package | ≥ 1.24.0 | Already a TAG dependency; used for vector arithmetic |
| `PyYAML` | Python package | ≥ 6.0 | Already a TAG dependency; used for YAML parsing |
| `all-MiniLM-L6-v2` model weights | HuggingFace model | — | 22 MB; already downloaded if `tool_retrieval.py` has been used |
| PRD-043 (vector tool retrieval) | Internal | — | Establishes `SentenceTransformer` usage pattern; `routing.py` follows same conventions |
| PRD-013 (agent tracing) | Internal | — | Span emission in classify path |
| PRD-034 (secret scanning) | Internal | — | Prompt scanning before embedding |
| PRD-027 (eval framework) | Internal | — | `calibrate` output can be fed into eval regression gating |
| PRD-012 (budget enforcement) | Internal | — | `--auto-classify` path counts as a pre-run step; budget checked before classify |
| SQLite WAL mode | Runtime | — | Existing TAG database; `route_examples` and `route_decisions` tables are added to the existing schema |

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
| OQ-08 | Should `--auto-classify` be opt-in (requires flag) or opt-out (enabled by default when `sentence-transformers` is installed)? Opt-out is more ergonomic but could silently change routing behaviour for existing users. | Product | Before Sprint 2 |

---

## 15. Complexity and Timeline

**Overall estimate:** M (8-10 working days)

### Phase 1 — Schema and Core Classifier (Days 1-3)

- Write SQL DDL for `route_examples`, `route_decisions`, `route_classifier_meta`
- Add `ensure_classifier_tables()` call in `controller.py` `open_db()` post-init
- Implement `routing.py`: `get_embed_model()`, `embed_text()`, `vec_to_blob()`, `blob_to_vec()`, `_validate_model_compat()`
- Implement `classify_task_type()` with `top3_mean` aggregation
- Write unit tests for core classifier: `test_routing.py` (items 1-10)
- Implement `train_from_yaml()` and add `cmd_route_train()` in `controller.py`

**Deliverable:** `tag route train` and `tag route classify` functional end-to-end

### Phase 2 — Full CLI Surface and Persistence (Days 4-6)

- Implement `add_example()` and `cmd_route_add_example()`
- Implement `cmd_route_list()` with `--stats` query on `route_decisions`
- Add `--no-persist` path; implement decision row insertion
- Wire `tracing.py` span emission in classify path
- Wire `security.py` prompt scanning in classify path
- Write integration tests: `test_routing_integration.py` (items 1-8)

**Deliverable:** All four subcommands functional; audit trail active; tracing active

### Phase 3 — Calibration and `--auto-classify` (Days 7-9)

- Implement `calibrate()` with LOO mode
- Implement `cmd_route_calibrate()`
- Add `--auto-classify` flag to `cmd_run()` and `cmd_queue_add()`
- Add `routing.classifier.*` config schema documentation
- Write performance benchmarks: `test_routing_perf.py`
- Fix any regressions found by perf benchmarks

**Deliverable:** Full feature-complete; all AC-01 through AC-15 passing

### Phase 4 — Hardening and Documentation (Day 10)

- Code review against security considerations (OQ-03 salt, OQ-02 auto-train)
- Edge case testing: empty YAML, single-example types, model compat mismatch
- Update `tag route --help` strings and `tag doctor` to check for `sentence-transformers`
- Resolve OQ-03 (prompt hash salting) before merge
- Final pass on `--json` schema consistency across all four subcommands

**Deliverable:** PR ready for merge; all tests green; all AC passing

---

*GitHub Issue: #349*
*Cluster: G — Advanced Reasoning & Planning*
*Related PRDs: PRD-043 (tool retrieval), PRD-101 (self-consistency), PRD-102 (multi-agent debate)*
