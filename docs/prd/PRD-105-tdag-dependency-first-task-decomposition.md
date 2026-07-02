# PRD-105: Dependency-First Hierarchical Task Decomposition (TDAG) (`tag plan decompose`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** L (2-4 weeks)
**Category:** Advanced Reasoning & Planning
**Affects:** `internal/queue + internal/agent`
**Depends on:** PRD-027 (eval framework), PRD-028 (sandbox), PRD-013 (agent tracing/observability), PRD-034 (security), PRD-033 (dependency-aware task queue), PRD-043 (vector-based tool retrieval), PRD-025 (semantic memory), PRD-021 (agent loop/autonomous mode), PRD-012 (cost tracking/budget)
**Inspired by:** TDAG paper (2024), HuggingGPT, LLM Compiler, Plan-and-Execute
**GitHub issue:** #349

---

## 1. Overview

Complex agentic goals — "Build and deploy a REST API", "Refactor the authentication module and update all tests", "Research competitors and produce a slide deck" — cannot be reliably achieved in a single LLM call or even in a flat sequence of tool calls. They require decomposition into subtasks with explicit dependency relationships, parallel execution where the dependency graph permits, and dynamic adaptation when individual subtasks fail. The TAG agent loop (`internal/agent`) currently operates as a flat iteration: a single goal is pursued iteration-by-iteration with no structural awareness of sub-goals, their ordering constraints, or opportunities for concurrency. When a subtask fails, the loop retries the full goal with no surgical intervention.

TDAG (Task DAG) introduces `tag plan decompose`, a planning command that takes a high-level goal and emits a directed acyclic graph (DAG) of typed subtasks with explicit dependency edges. The decomposition is performed by an LLM acting as a planner, guided by a structured output schema (Go structs + `invopop/jsonschema`) that enforces node typing (compute, research, review, write, deploy), skill-to-tool assignment via embedding cosine similarity (threshold θ=0.7), and a topological sort validation pass before the plan is persisted. The resulting plan is a JSON artifact — a `plan.json` — that can be inspected, edited, re-validated, and then executed via `tag plan run`. Execution honors the partial order defined by the DAG: nodes with no unresolved dependencies enter a ready queue immediately, while nodes with unsatisfied dependencies remain pending. Ready nodes can be dispatched concurrently by a bounded goroutine worker pool, with a configurable parallelism cap.

Execution is driven by the bespoke SQLite-backed goroutine DAG scheduler in `internal/queue` (see GO_MIGRATION_PLAN.md decision (5)) — a worker pool over `golang.org/x/sync/errgroup` and channels, not an external queue such as River (Postgres) or asynq (Redis), both of which would break the single-binary mandate. There is no drop-in pure-Go embedded durable DAG queue, so this scheduler is genuine engineering rather than a library drop-in. The engine builds on the MagenticOne Dual-Ledger pattern: a Task Ledger (the DAG itself, with per-node status and outputs) is the outer strategic record, while a Progress Ledger (a structured JSON self-reflection updated after every node completion) tracks tactical state. A stall counter monitors for repeated identical states; when the stall counter exceeds a configurable threshold (default 2), the engine triggers a re-plan call that splices replacement nodes into the live DAG without restarting already-completed work. This dynamic replanning differentiates TDAG from simpler Plan-and-Execute architectures that must restart from scratch on failure.

The feature integrates with TAG's existing infrastructure at every layer: the DAG spec and all execution state live in the single `modernc.org/sqlite` store (pure-Go, WAL mode, `CGO_ENABLED=0`, accessed through `internal/store`) under the `plan_graphs` and `plan_nodes` tables. State transitions are event-sourced to SQLite for durability, crash recovery, and time-travel replay. Per-node span traces are emitted through `internal/obs` (`go.opentelemetry.io/otel`) to the `traces` table. Tool retrieval for subtask-to-skill assignment reuses `internal/toolindex` (an `Embedder` interface + in-Go cosine over a float32 BLOB column, replacing the Python SentenceTransformer + ChromaDB path). Budget enforcement per-plan uses `internal/obs` cost tracking. Sandbox isolation for code-execution subtasks uses `internal/sandbox`. The `--json` flag on every subcommand makes all outputs machine-readable for CI pipelines and the upcoming web dashboard.

The inspiration sources each contribute a specific mechanism: TDAG (2024) contributes the skill-retrieval loop and ordered dependency list with dynamic updates; HuggingGPT contributes the typed node taxonomy and model-to-task assignment pattern; LLM Compiler contributes the parallel dispatch pattern and the `$node_id` variable substitution syntax for passing outputs between nodes; Plan-and-Execute contributes the two-phase planner/executor separation and the re-plan trigger on executor failure.

---

## 2. Problem Statement

### 2.1 The agent loop has no structural understanding of subtask dependencies

The `internal/agent` loop runs a single goal through repeated agent invocations. There is no mechanism to express that "writing unit tests" must follow "implementing the function", or that "deploying to staging" must follow both "building the Docker image" and "passing the test suite". When the LLM produces a plan implicitly in its chain-of-thought, that plan is ephemeral — it is not persisted, not validated for cycles or missing dependencies, not tracked per-subtask, and not available for inspection or reuse. If the agent fails mid-way, the operator has no visibility into which subtasks completed and which did not. The only recovery path is a full restart.

### 2.2 Parallelism opportunities are left on the table

Many real-world goals decompose into subtasks that are structurally independent and could execute concurrently. Research subtasks, documentation generation, and test writing for different modules are often fully parallel. The current agent loop serializes all work into a single sequential goroutine. On goals with natural parallelism, this means the wall-clock time scales linearly with subtask count rather than with the critical path length. For a 10-subtask goal where 5 subtasks are independent, this is a 2-5x wall-clock penalty.

### 2.3 Failure recovery is coarse-grained and stateless

When the current loop agent encounters a subtask failure (e.g., a build fails, a test suite errors out), it has two options: continue to the next iteration (potentially building on a broken state) or abort. There is no mechanism to detect which subtask failed, diagnose the failure, replace only that subtask with a repaired version, and resume from the checkpoint. The absence of a persisted, per-node execution state means that recovery always starts from scratch, wasting all completed work and incurring full re-execution cost. For goals with expensive subtasks (LLM calls, web searches, long builds), this is both slow and costly.

---

## 3. Goals and Non-Goals

### Goals

| # | Goal |
|---|------|
| G1 | `tag plan decompose --goal "<text>" --profile <p>` produces a validated DAG plan as a JSON artifact, persisted to SQLite and optionally written to `plan.json`. |
| G2 | `tag plan run --plan plan.json --profile <p>` executes the DAG in dependency order, dispatching ready nodes concurrently up to `--parallel N` (default 4). |
| G3 | `tag plan show --plan plan.json` renders the DAG as a rich ASCII graph or JSON, showing node status, type, assigned tools, and dependency edges. |
| G4 | Per-node execution state is persisted to SQLite after every node transition, enabling crash recovery by re-running `tag plan run` on a previously started plan. |
| G5 | When a node fails, the engine triggers structured re-plan: the planner LLM receives the failed node's output and error, the completed-nodes context, and the remaining DAG, and returns replacement node(s) to splice in. |
| G6 | Tool-to-subtask assignment uses embedding cosine similarity (threshold θ=0.7) against the indexed tool registry, reusing `internal/toolindex` (Embedder interface + in-Go cosine) — so subtasks automatically get the right tools without manual specification. |
| G7 | All plan operations (`decompose`, `run`, `show`, `list`, `export`) support `--json` output for CI/pipeline integration. |
| G8 | Per-plan budget caps (token and USD) are enforced via `internal/obs` cost tracking; the plan engine checks remaining budget before dispatching each node. |
| G9 | Stall detection: a stall counter per plan increments when no node changes status between dispatch cycles; at threshold 2 (configurable), re-plan is triggered even without an explicit failure. |
| G10 | A `$node_id.output` variable substitution syntax allows downstream node prompts to reference the literal output of upstream nodes, enabling data flow across the DAG. |
| G11 | Plan files are signed (HMAC-SHA256) on write and verified on load, so tampered plan files are rejected before execution. See Security Considerations. |
| G12 | `tag plan list` lists all plans stored in SQLite with status summaries (total nodes, completed, failed, pending). |

### Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | General workflow orchestration (Airflow, Temporal, Prefect). TDAG is scoped to LLM-agentic subtasks within a single TAG session, not long-running distributed workflows. |
| NG2 | Cross-machine distributed execution. All nodes execute on the local machine via the existing `tag submit` / queue infrastructure. PRD-088 (distributed agent runtime gRPC) is the path for multi-machine. |
| NG3 | Visual GUI plan editor. `tag plan show` renders to terminal. A web UI is out of scope for this PRD; the JSON plan file is the editing surface. |
| NG4 | Automatic cost optimization of the DAG (e.g., choosing cheaper models per node based on complexity). Model assignment per node is supported but not auto-optimized. PRD-017 (multi-model benchmarking) is the right vehicle. |
| NG5 | Cycle detection is a validation step at plan creation time; execution does not attempt to handle cyclic graphs. Plans with cycles are rejected at `decompose` and `plan run` load time. |
| NG6 | Self-consistency or multi-agent debate during decomposition (PRD-101). The planner uses a single LLM call with structured output; ensemble planning is a future extension. |
| NG7 | Automatic reversion of completed node side-effects on re-plan. Re-plan splices new nodes for the failed subtask; it does not undo filesystem or API changes made by completed nodes. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Decomposition latency | `tag plan decompose` returns a validated plan within 15 seconds for goals up to 500 characters | Timed in integration test suite (p95) |
| Parallelism speedup | Wall-clock time for a 6-node plan with 3 independent root nodes is ≤ 55% of sequential wall-clock time | Benchmark test with mock agent that sleeps 1s per node |
| Re-plan success rate | Re-plan splices correct replacement nodes (no cycle introduction, valid topological order) in ≥ 95% of triggered cases | Property test with random DAG + random failure injection |
| Plan persistence | After `SIGKILL` of `tag plan run`, re-running the same command resumes from the last completed node with 0 re-executed completed nodes | Integration test |
| Tool assignment accuracy | Embedding skill retrieval (`internal/toolindex`) assigns ≥ 1 relevant tool for ≥ 90% of non-trivial subtask descriptions | Eval against 50 manually labeled subtask→tool pairs |
| Budget enforcement | Plan engine never dispatches a node when accumulated cost exceeds plan budget cap | Unit test with mock cost accumulator |
| Cycle rejection | 100% of plans containing cycles are rejected at validation time with a clear error identifying the cycle | Fuzz test with randomly generated graphs |
| Variable substitution | `$node_id.output` resolves correctly in 100% of tested downstream prompts | Unit test with synthetic plan fixtures |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag plan decompose --goal "Implement OAuth2 login and add integration tests" --profile coder` | I get a structured plan I can review before committing to a long execution |
| U2 | Developer | run `tag plan show --plan plan.json` and see the DAG with node types, dependencies, and assigned tools | I can verify the plan makes sense before spending tokens on execution |
| U3 | Developer | run `tag plan run --plan plan.json --profile coder --parallel 3` | Execution proceeds at maximum safe concurrency, finishing faster than sequential |
| U4 | Platform engineer | observe that `tag plan run` resumes from the correct checkpoint after a crash | I do not need to restart expensive completed work |
| U5 | Developer | receive a clear error and a re-plan proposal when a subtask fails | I understand what went wrong and can approve or reject the re-plan |
| U6 | DevOps engineer | pipe `tag plan decompose --json` into a CI job that validates and then runs the plan | I can automate the entire plan-and-execute workflow in a pipeline |
| U7 | Team lead | review `tag plan list --json` to see all plans run this week with their status and cost | I can audit what complex goals were attempted and their outcomes |
| U8 | Developer | set `--budget-usd 2.00` on `tag plan run` | The engine stops dispatching new nodes if the plan would exceed my cost cap |
| U9 | Developer | use `$decompose_api.output` in a downstream node's prompt to reference the API specification produced by an upstream node | I can wire data flow between subtasks without manual copy-paste |
| U10 | Security-conscious operator | see that plan files are HMAC-signed and rejected if tampered | I can trust that a plan.json from a trusted source has not been modified in transit |

---

## 6. Proposed CLI Surface

All plan subcommands live under the `tag plan` namespace.

### 6.1 `tag plan decompose`

Decompose a high-level goal into a validated DAG plan.

```bash
tag plan decompose \
  --goal "Build and deploy a REST API" \
  --profile orchestrator \
  [--output plan.json] \
  [--max-nodes 20] \
  [--budget-usd 5.00] \
  [--budget-tokens 50000] \
  [--model anthropic/claude-sonnet-4-6] \
  [--skill-threshold 0.7] \
  [--dry-run] \
  [--json]
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--goal TEXT` | required | High-level goal string to decompose |
| `--profile NAME` | required | TAG profile used as the planner |
| `--output PATH` | `plan.json` in cwd | Write the plan JSON to this file (also stored in SQLite) |
| `--max-nodes N` | `20` | Maximum number of subtask nodes the planner may emit |
| `--budget-usd FLOAT` | none | Maximum USD budget for the entire plan execution |
| `--budget-tokens INT` | none | Maximum token budget for the entire plan execution |
| `--model MODEL_ID` | profile default | Override the planner model for the decomposition call |
| `--skill-threshold FLOAT` | `0.7` | Cosine similarity threshold for tool-to-subtask assignment |
| `--dry-run` | false | Validate that the planner prompt can be constructed; do not call the LLM |
| `--json` | false | Output machine-readable JSON instead of formatted table |

**Human-readable output example:**

```
Planning goal: "Build and deploy a REST API"
Profile: orchestrator
Model: claude-sonnet-4-6

Decomposing... done (3.2s, 847 tokens, $0.003)

Plan ID: plan-a3f7c2
Nodes: 7  |  Critical path: 4 hops  |  Max parallelism: 3

ID               Type       Tools Assigned          Depends On
──────────────────────────────────────────────────────────────────
design_api       research   web_search, read_file   —
scaffold_server  compute    bash, write_file        design_api
write_tests      compute    bash, write_file        scaffold_server
write_docs       write      write_file              design_api
lint_and_type    compute    bash                    scaffold_server
build_docker     compute    bash                    scaffold_server, lint_and_type
deploy_staging   deploy     bash                    build_docker, write_tests

Saved to: plan.json
Stored in SQLite: plan-a3f7c2
```

**JSON output (`--json`) example:**

```json
{
  "plan_id": "plan-a3f7c2",
  "goal": "Build and deploy a REST API",
  "profile": "orchestrator",
  "created_at": "2026-06-17T10:22:14Z",
  "status": "ready",
  "budget_usd": null,
  "budget_tokens": null,
  "nodes": [
    {
      "id": "design_api",
      "label": "Design REST API specification",
      "type": "research",
      "prompt": "Research best practices for REST API design and produce an OpenAPI 3.0 spec for a task management API.",
      "tools": ["web_search", "read_file"],
      "depends_on": [],
      "status": "pending",
      "output": null,
      "error": null
    },
    {
      "id": "scaffold_server",
      "label": "Scaffold FastAPI server from spec",
      "type": "compute",
      "prompt": "Using the API spec from $design_api.output, scaffold a FastAPI server with all endpoints, Pydantic models, and SQLite persistence.",
      "tools": ["bash", "write_file", "read_file"],
      "depends_on": ["design_api"],
      "status": "pending",
      "output": null,
      "error": null
    }
  ],
  "metadata": {
    "planner_model": "anthropic/claude-sonnet-4-6",
    "decompose_tokens": 847,
    "decompose_cost_usd": 0.003,
    "skill_threshold": 0.7,
    "hmac_sha256": "e3b0c44298fc1c149afb..."
  }
}
```

### 6.2 `tag plan run`

Execute a plan in dependency order with optional parallelism.

```bash
tag plan run \
  --plan plan.json \
  --profile coder \
  [--parallel N] \
  [--budget-usd 10.00] \
  [--budget-tokens 200000] \
  [--stall-threshold N] \
  [--replan-model MODEL_ID] \
  [--no-replan] \
  [--approval auto|human] \
  [--timeout-per-node SECONDS] \
  [--json] \
  [--resume]
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--plan PATH` | required | Path to plan.json (or plan ID from `tag plan list`) |
| `--profile NAME` | required | TAG profile used for node execution |
| `--parallel N` | `4` | Maximum concurrent node executions |
| `--budget-usd FLOAT` | plan default | Override per-execution USD budget cap |
| `--budget-tokens INT` | plan default | Override per-execution token budget cap |
| `--stall-threshold N` | `2` | Consecutive stall cycles before triggering re-plan |
| `--replan-model MODEL_ID` | profile default | Model used for re-plan calls (can be cheaper than executor) |
| `--no-replan` | false | Abort on first node failure instead of attempting re-plan |
| `--approval auto\|human` | `auto` | `human` requires terminal confirmation before each node dispatch |
| `--timeout-per-node SECONDS` | `600` | Wall-clock timeout for a single node execution |
| `--json` | false | Emit structured JSON events to stdout (one JSON object per line) |
| `--resume` | false | Skip nodes already marked `done` in SQLite; resume from last checkpoint |

**Human-readable streaming output:**

```
Running plan: plan-a3f7c2
Profile: coder  |  Parallel: 4  |  Budget: $10.00

[10:22:18] DISPATCH  design_api       (research)
[10:22:31] DONE      design_api       (13.1s, 1203 tokens, $0.005)
[10:22:31] DISPATCH  scaffold_server  (compute)
[10:22:31] DISPATCH  write_docs       (write)     ← parallel with scaffold_server
[10:22:58] DONE      write_docs       (27s, 890 tokens, $0.003)
[10:23:04] DONE      scaffold_server  (33.1s, 2100 tokens, $0.008)
[10:23:04] DISPATCH  write_tests      (compute)
[10:23:04] DISPATCH  lint_and_type    (compute)   ← parallel with write_tests
[10:23:19] FAILED    lint_and_type    (mypy: 3 type errors)
[10:23:19] RE-PLAN   Requesting replacement for lint_and_type...
[10:23:22] SPLICE    fix_type_errors  → lint_and_type_retry (2 replacement nodes)
[10:23:22] DISPATCH  fix_type_errors  (compute)
...
[10:24:45] DONE      deploy_staging   (compute)
────────────────────────────────────────────────
Plan complete: 8/8 nodes done (1 re-plan)
Total: 2m 27s  |  Tokens: 9,847  |  Cost: $0.038
```

**JSON streaming events (`--json`):**

```json
{"event": "dispatch", "node_id": "design_api", "ts": "2026-06-17T10:22:18Z"}
{"event": "done", "node_id": "design_api", "elapsed_s": 13.1, "tokens": 1203, "cost_usd": 0.005, "ts": "2026-06-17T10:22:31Z"}
{"event": "failed", "node_id": "lint_and_type", "error": "mypy: 3 type errors", "ts": "2026-06-17T10:23:19Z"}
{"event": "replan", "failed_node": "lint_and_type", "replacement_nodes": ["fix_type_errors", "lint_and_type_retry"], "ts": "2026-06-17T10:23:22Z"}
{"event": "plan_complete", "total_nodes": 8, "elapsed_s": 147, "tokens": 9847, "cost_usd": 0.038}
```

### 6.3 `tag plan show`

Inspect a plan's structure and current execution state.

```bash
tag plan show \
  --plan plan.json \
  [--json] \
  [--dot]     # Emit Graphviz DOT format for rendering
```

**Human-readable output:**

```
Plan: plan-a3f7c2  |  Status: running  |  Nodes: 7

  design_api [DONE]
  └─► scaffold_server [DONE]
  │   ├─► write_tests [DONE]
  │   ├─► lint_and_type [FAILED → replaced by fix_type_errors]
  │   └─► build_docker [pending]
  └─► write_docs [DONE]

  fix_type_errors [running]
  └─► lint_and_type_retry [pending]
      └─► build_docker [pending]
          └─► deploy_staging [pending]
```

### 6.4 `tag plan list`

List all plans stored in SQLite.

```bash
tag plan list [--status pending|running|done|failed] [--last N] [--json]
```

**Output example:**

```
ID            Goal (truncated)                   Status   Nodes  Done  Failed  Created
─────────────────────────────────────────────────────────────────────────────────────────
plan-a3f7c2   Build and deploy a REST API        running  7      5     1       2026-06-17
plan-b9d12f   Refactor auth module + add tests   done     5      5     0       2026-06-16
plan-c1e44a   Research competitors + slide deck  failed   4      2     1       2026-06-15
```

### 6.5 `tag plan export`

Export a plan and its execution results to a portable JSON archive.

```bash
tag plan export --plan plan-a3f7c2 --output plan-a3f7c2-export.json [--include-outputs]
```

Writes a self-contained JSON file with the plan graph, per-node outputs (if `--include-outputs`), and execution metadata. Suitable for sharing, auditing, or importing into another TAG instance.

### 6.6 `tag plan validate`

Validate a plan file without executing it.

```bash
tag plan validate --plan plan.json
```

Checks: JSON schema conformance, no cycles (topological sort), all `depends_on` IDs exist in the node list, HMAC signature valid, node count within configured `max-nodes`, and all node types are recognized. Exits 0 on valid, 1 on any violation with a detailed error message.

---

## 7. Functional Requirements

| ID | Requirement | Acceptance Test |
|----|------------|-----------------|
| FR-01 | `tag plan decompose` calls the planner LLM with a structured output schema enforcing `PlanNode` fields: `id`, `label`, `type`, `prompt`, `depends_on`. | Unit test: mock LLM returns malformed JSON; command exits 1 with schema error. |
| FR-02 | Decomposition validates the graph for cycles using Kahn's algorithm before persisting. Any cycle causes an immediate error with the cycle path. | Unit test: inject a plan with A→B→A; expect `CycleError` with path `['A','B','A']`. |
| FR-03 | Tool-to-subtask assignment is performed by computing cosine similarity between each node's `prompt` embedding and indexed tool descriptions via `internal/toolindex` (Embedder interface + in-Go cosine over the float32 BLOB index). Only tools with similarity ≥ `--skill-threshold` are assigned. | Integration test: subtask "write Python code" assigns `bash`/`write_file`; subtask "search the web" assigns `web_search`. |
| FR-04 | All plans are persisted to `plan_graphs` and `plan_nodes` tables via `open_db()` before `decompose` returns. | Integration test: `tag plan list` shows the plan after `decompose`; kill process before output; `tag plan list` still shows it. |
| FR-05 | `tag plan run` reads the plan from SQLite (using `plan_id` from the JSON file) and dispatches nodes whose `depends_on` set is fully in `{done}` state. | Unit test: plan with nodes A (no deps), B (depends A), C (depends A). At t=0, only A dispatched. |
| FR-06 | `tag plan run` maintains a `ready_queue` and an `in_flight` set; at each dispatch cycle it promotes all nodes with all deps `done` into `ready_queue` and dispatches up to `--parallel` from `ready_queue`. | Integration test: mock executor sleeps 0.1s; verify 3 concurrent dispatches when `--parallel 3` and 3 independent root nodes. |
| FR-07 | A failed node triggers a re-plan call to the planner LLM with: the failed node spec, its error output, the list of completed node IDs and their outputs, and the remaining (not-yet-started) portion of the DAG. The planner returns 1-N replacement nodes to splice in. | Integration test with mock failure; verify replacement nodes have correct `depends_on` edges. |
| FR-08 | Replacement nodes from re-plan are validated (no cycles, valid IDs) before being spliced into the live DAG. If validation fails, the engine falls back to `--no-replan` behavior (abort). | Unit test: re-plan response introduces cycle; engine aborts with clear error. |
| FR-09 | The stall counter increments when a dispatch cycle completes without any node state transition. At `stall_threshold` increments, re-plan is triggered with a stall reason (no failed node, but no progress). | Unit test: mock executor returns `stuck` status N times; verify re-plan triggered after N=stall_threshold. |
| FR-10 | `$node_id.output` variable references in node prompts are resolved at dispatch time by substituting the `output` field of the referenced node. Missing references (node not done) block dispatch. | Unit test: node B prompt contains `$node_a.output`; verify substitution at dispatch; verify dispatch blocked if node_a not done. |
| FR-11 | Per-node execution state (status, output, error, start_time, end_time, tokens, cost_usd) is written to `plan_nodes` after every transition. | Integration test: `SIGKILL` mid-run; verify `plan_nodes` shows correct `done`/`running` state for completed/active nodes. |
| FR-12 | `tag plan run --resume` skips nodes with `status='done'` in `plan_nodes` and re-queues nodes with `status='running'` (crashed in-flight) as `pending`. | Integration test: pre-seed `plan_nodes` with 3 done + 1 running; verify only non-done nodes are executed. |
| FR-13 | Budget caps (USD and tokens) are checked before dispatching each node. If `accumulated_cost_usd + estimated_node_cost > budget_usd`, dispatch is blocked and the engine emits a `budget_exceeded` event and aborts. | Unit test: set budget=$0.01; mock executor accumulates cost; verify abort before third node. |
| FR-14 | Per-node spans are emitted to the `traces` table via `internal/obs` (`otel` custom SpanProcessor), tagged with `plan_id`, `node_id`, `node_type`, and `depends_on` attributes. | Integration test: verify `SELECT * FROM traces WHERE attributes LIKE '%plan_id%'` returns one span per executed node. |
| FR-15 | Plan files are HMAC-SHA256 signed using the TAG instance key on write. `tag plan validate` and `tag plan run` verify the signature before trusting the file. | Unit test: modify one byte of plan.json; verify rejection with `SignatureError`. |
| FR-16 | `tag plan show --dot` emits a valid Graphviz DOT file representing the DAG, with node shapes colored by type and edge labels showing dependency direction. | Unit test: parse DOT output with `gonum.org/v1/gonum/graph/encoding/dot`; verify node count and edge count match plan spec. |
| FR-17 | `tag plan export --include-outputs` produces a JSON archive containing the full plan spec plus the `output` field of every completed node. | Integration test: run a plan to completion; export; verify all node outputs present. |
| FR-18 | `tag plan list` reads from `plan_graphs` and emits one row per plan with `id`, `goal` (truncated to 60 chars), `status`, total node count, done count, failed count, and `created_at`. | Unit test with seeded SQLite; verify output row count and field values. |
| FR-19 | Node types are constrained to the enum `{research, compute, write, review, deploy}`. The planner prompt includes the enum definition and the schema validator rejects unknown types. | Unit test: inject node with `type: "unknown"`; verify `SchemaValidationError`. |
| FR-20 | `tag plan decompose --dry-run` constructs the planner prompt, validates profile existence, and prints the estimated prompt token count and cost without making any LLM API call. | Unit test: verify no HTTP calls made (`net/http/httptest` round-tripper asserting zero requests); verify cost estimate printed. |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|------------|--------|
| NFR-01 | `tag plan show` renders for a 20-node plan within 200ms (SQLite read + ASCII render). | Benchmark test |
| NFR-02 | `tag plan decompose` holds no LLM connection open beyond the single planning call; does not keep a server socket open between runs. | Code review + network mock test |
| NFR-03 | SQLite writes to `plan_nodes` use WAL mode and `PRAGMA busy_timeout=5000` to prevent blocking when `tag plan show` reads concurrently with `tag plan run` writing. | Concurrent read/write integration test |
| NFR-04 | The `tag plan run` dispatch loop polls SQLite at most once per second when waiting for in-flight nodes to complete (to avoid busy-wait CPU burn). | CPU profiling test: verify <5% CPU during wait phase |
| NFR-05 | Plan files (plan.json) are human-readable, pretty-printed JSON with no binary content. | Schema test |
| NFR-06 | `tag plan run` with `--json` emits newline-delimited JSON (NDJSON), one JSON object per event, so pipelines can process events incrementally with `while read line`. | Shell integration test |
| NFR-07 | The re-plan LLM call must complete within 30 seconds; if it times out, the engine falls back to abort behavior. | Unit test with mock timeout |
| NFR-08 | Memory usage of the dispatch engine must not grow with plan size beyond O(N) where N is node count (no full output materialization in memory for non-referenced outputs). | Memory profiling test with 50-node plan |
| NFR-09 | Skill retrieval via `internal/toolindex` (Embedder call + in-Go cosine over the BLOB index) must complete within 2 seconds per node at decomposition time. | Benchmark test (`testing.B`) |
| NFR-10 | All new packages must pass `go vet`, `golangci-lint`, and `staticcheck` with zero findings, and be `gofmt`-clean. | CI gate |
| NFR-11 | The feature must not introduce new modules beyond what is already required by `internal/toolindex` (the `Embedder` interface + provider embedding client, `gonum`) and the existing TAG Go stack. The default build stays `CGO_ENABLED=0`. | `go build ./...` must not pull new direct modules outside the pinned `go.mod` set. |
| NFR-12 | `tag plan decompose` and `tag plan run` must be idempotent with respect to SQLite state: running `decompose` twice for the same goal and plan ID is a no-op (returns existing plan). Idempotency is enforced by the single-writer + `flock`/`os.Rename` atomic RMW contract in `internal/store`. | Integration test: run decompose twice; verify single row in `plan_graphs`. |

---

## 9. Technical Design

### 9.1 New and Modified Packages

| Package / file | Change Type | Description |
|------|-------------|-------------|
| `internal/queue/plan.go` | **New** | `PlanGraph`, `PlanNode` structs; `Decompose()`, `RunPlan()`, `Replan()`, `ValidateDAG()`; Kahn topological sort + cycle detection. Reuses the existing `internal/queue` errgroup worker pool + scheduler goroutine (the bespoke SQLite-backed DAG engine). |
| `internal/queue/scheduler.go` | **Extend** | The pending→ready promotion loop and bounded goroutine dispatch already used for `queue_jobs`/`queue_dags` is generalized to schedule `plan_nodes`. |
| `internal/store/plan_schema.go` | **New** | `EnsurePlanSchema(ctx, db)` migration creating `plan_graphs`, `plan_nodes`, `plan_progress_ledger` on the single `modernc.org/sqlite` store. |
| `internal/agent/ledger.go` | **New** | `TaskLedger` + `ProgressLedger` + `ProgressReflection` structs; `RunWithLedger()`; stall detection. Wraps the hand-rolled inner agent loop. |
| `internal/cli/plan.go` | **New** | Cobra command group `tag plan` with `decompose`, `run`, `show`, `list`, `export`, `validate` subcommands. |
| `internal/queue/plan_test.go` | **New** | Table-driven unit + integration tests for all FR items. |
| `internal/queue/plan_prop_test.go` | **New** | Property-based tests (`testing/quick`, optionally `pgregory.net/rapid`) for DAG validation, cycle detection, re-plan splice correctness. |

### 9.2 SQLite DDL

These tables are created by `EnsurePlanSchema(ctx, db)` in `internal/store`, invoked once at migration time against the single pure-Go `modernc.org/sqlite` connection (WAL, `CGO_ENABLED=0`, FTS5 compiled in). All writes go through the single-writer + `flock` atomic RMW contract.

```sql
-- Plan graph: one row per decompose call
CREATE TABLE IF NOT EXISTS plan_graphs (
  id              TEXT PRIMARY KEY,          -- e.g. "plan-a3f7c2"
  goal            TEXT NOT NULL,
  profile         TEXT NOT NULL,
  status          TEXT NOT NULL DEFAULT 'ready',  -- ready|running|done|failed|aborted
  planner_model   TEXT NOT NULL,
  decompose_tokens INTEGER NOT NULL DEFAULT 0,
  decompose_cost_usd REAL NOT NULL DEFAULT 0.0,
  skill_threshold REAL NOT NULL DEFAULT 0.7,
  budget_usd      REAL,                      -- NULL means no cap
  budget_tokens   INTEGER,                   -- NULL means no cap
  accumulated_cost_usd REAL NOT NULL DEFAULT 0.0,
  accumulated_tokens   INTEGER NOT NULL DEFAULT 0,
  stall_count     INTEGER NOT NULL DEFAULT 0,
  replan_count    INTEGER NOT NULL DEFAULT 0,
  file_path       TEXT,                      -- canonical path of plan.json if written
  hmac_sha256     TEXT NOT NULL,             -- signature of serialized plan
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL,
  completed_at    TEXT
);

CREATE INDEX IF NOT EXISTS idx_pg_status ON plan_graphs(status, created_at);

-- Plan nodes: one row per node per plan
CREATE TABLE IF NOT EXISTS plan_nodes (
  id              TEXT NOT NULL,             -- node ID, unique within plan
  plan_id         TEXT NOT NULL,
  label           TEXT NOT NULL,
  node_type       TEXT NOT NULL,             -- research|compute|write|review|deploy
  prompt_template TEXT NOT NULL,             -- may contain $node_id.output refs
  tools_json      TEXT NOT NULL DEFAULT '[]', -- JSON array of assigned tool names
  depends_on_json TEXT NOT NULL DEFAULT '[]', -- JSON array of node IDs
  status          TEXT NOT NULL DEFAULT 'pending', -- pending|ready|running|done|failed|skipped
  output          TEXT,                      -- raw output text from agent execution
  error           TEXT,                      -- error message if status=failed
  replaced_by     TEXT,                      -- node ID that replaced this node after re-plan
  tokens_used     INTEGER NOT NULL DEFAULT 0,
  cost_usd        REAL NOT NULL DEFAULT 0.0,
  started_at      TEXT,
  completed_at    TEXT,
  created_at      TEXT NOT NULL,
  PRIMARY KEY (plan_id, id),
  FOREIGN KEY (plan_id) REFERENCES plan_graphs(id)
);

CREATE INDEX IF NOT EXISTS idx_pn_status ON plan_nodes(plan_id, status);
CREATE INDEX IF NOT EXISTS idx_pn_plan   ON plan_nodes(plan_id, id);

-- Progress ledger: dual-ledger pattern (MagenticOne)
CREATE TABLE IF NOT EXISTS plan_progress_ledger (
  id              TEXT PRIMARY KEY,
  plan_id         TEXT NOT NULL,
  cycle           INTEGER NOT NULL,
  nodes_done_json TEXT NOT NULL DEFAULT '[]',
  nodes_failed_json TEXT NOT NULL DEFAULT '[]',
  nodes_in_flight_json TEXT NOT NULL DEFAULT '[]',
  stall_count     INTEGER NOT NULL DEFAULT 0,
  reflection_json TEXT,                      -- 5-question structured self-reflection JSON
  created_at      TEXT NOT NULL,
  FOREIGN KEY (plan_id) REFERENCES plan_graphs(id)
);

CREATE INDEX IF NOT EXISTS idx_ppl_plan ON plan_progress_ledger(plan_id, cycle);
```

### 9.3 Core Structs

```go
// internal/queue/plan.go
package queue

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
)

type NodeType string

const (
	NodeResearch NodeType = "research"
	NodeCompute  NodeType = "compute"
	NodeWrite    NodeType = "write"
	NodeReview   NodeType = "review"
	NodeDeploy   NodeType = "deploy"
)

type NodeStatus string

const (
	StatusPending NodeStatus = "pending"
	StatusReady   NodeStatus = "ready"
	StatusRunning NodeStatus = "running"
	StatusDone    NodeStatus = "done"
	StatusFailed  NodeStatus = "failed"
	StatusSkipped NodeStatus = "skipped"
)

// ErrCycle is returned by TopologicalOrder when the graph is not acyclic.
var ErrCycle = errors.New("cycle detected in plan DAG")

type PlanNode struct {
	ID             string     `json:"id"`
	Label          string     `json:"label"`
	NodeType       NodeType   `json:"type"`
	PromptTemplate string     `json:"prompt"` // may contain $node_id.output refs
	DependsOn      []string   `json:"depends_on"`
	Tools          []string   `json:"tools"`
	Status         NodeStatus `json:"status"`
	Output         string     `json:"output,omitempty"`
	Error          string     `json:"error,omitempty"`
	ReplacedBy     string     `json:"replaced_by,omitempty"`
	TokensUsed     int        `json:"tokens_used"`
	CostUSD        float64    `json:"cost_usd"`
	StartedAt      string     `json:"started_at,omitempty"`
	CompletedAt    string     `json:"completed_at,omitempty"`
}

var outputRef = regexp.MustCompile(`\$([a-zA-Z_][a-zA-Z0-9_]*)\.output`)

// ResolvePrompt substitutes $node_id.output references with actual outputs.
func (n *PlanNode) ResolvePrompt(outputs map[string]string) (string, error) {
	var missing string
	res := outputRef.ReplaceAllStringFunc(n.PromptTemplate, func(m string) string {
		ref := outputRef.FindStringSubmatch(m)[1]
		v, ok := outputs[ref]
		if !ok {
			missing = ref
			return m
		}
		return v
	})
	if missing != "" {
		return "", fmt.Errorf("output reference $%s.output not yet available", missing)
	}
	return res, nil
}

type PlanGraph struct {
	ID                 string               `json:"plan_id"`
	Goal               string               `json:"goal"`
	Profile            string               `json:"profile"`
	Nodes              map[string]*PlanNode `json:"-"` // keyed by node ID; serialized as an ordered slice
	PlannerModel       string               `json:"planner_model"`
	DecomposeTokens    int                  `json:"decompose_tokens"`
	DecomposeCostUSD   float64              `json:"decompose_cost_usd"`
	SkillThreshold     float64              `json:"skill_threshold"`
	BudgetUSD          *float64             `json:"budget_usd"`     // nil means no cap
	BudgetTokens       *int                 `json:"budget_tokens"`  // nil means no cap
	AccumulatedCostUSD float64              `json:"accumulated_cost_usd"`
	AccumulatedTokens  int                  `json:"accumulated_tokens"`
	StallCount         int                  `json:"stall_count"`
	ReplanCount        int                  `json:"replan_count"`
	Status             string               `json:"status"`
	HMACSHA256         string               `json:"hmac_sha256"`
}

// TopologicalOrder implements Kahn's algorithm; wraps ErrCycle if a cycle exists.
func (g *PlanGraph) TopologicalOrder() ([]string, error) {
	inDeg := make(map[string]int, len(g.Nodes))
	adj := make(map[string][]string, len(g.Nodes))
	for id := range g.Nodes {
		inDeg[id] = 0
	}
	for id, node := range g.Nodes {
		for _, dep := range node.DependsOn {
			if _, ok := g.Nodes[dep]; !ok {
				return nil, fmt.Errorf("node %q depends on unknown node %q", id, dep)
			}
			adj[dep] = append(adj[dep], id)
			inDeg[id]++
		}
	}
	queue := make([]string, 0)
	for id, d := range inDeg {
		if d == 0 {
			queue = append(queue, id)
		}
	}
	sort.Strings(queue) // deterministic ordering
	order := make([]string, 0, len(g.Nodes))
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		order = append(order, id)
		for _, succ := range adj[id] {
			inDeg[succ]--
			if inDeg[succ] == 0 {
				queue = append(queue, succ)
			}
		}
	}
	if len(order) != len(g.Nodes) {
		var members []string
		seen := make(map[string]bool, len(order))
		for _, id := range order {
			seen[id] = true
		}
		for id := range g.Nodes {
			if !seen[id] {
				members = append(members, id)
			}
		}
		sort.Strings(members)
		return nil, fmt.Errorf("%w involving nodes: %v", ErrCycle, members)
	}
	return order, nil
}

// ReadyNodes returns node IDs whose deps are all done and whose status is pending.
func (g *PlanGraph) ReadyNodes() []string {
	done := make(map[string]bool)
	for id, n := range g.Nodes {
		if n.Status == StatusDone {
			done[id] = true
		}
	}
	var ready []string
	for id, node := range g.Nodes {
		if node.Status != StatusPending {
			continue
		}
		ok := true
		for _, dep := range node.DependsOn {
			if !done[dep] {
				ok = false
				break
			}
		}
		if ok {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	return ready
}

// Sign computes HMAC-SHA256 over a canonical JSON of the nodes (prompt + deps only).
func (g *PlanGraph) Sign(key []byte) string {
	type canon struct {
		Prompt    string   `json:"prompt_template"`
		DependsOn []string `json:"depends_on"`
	}
	ids := make([]string, 0, len(g.Nodes))
	for id := range g.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	m := make(map[string]canon, len(ids))
	for _, id := range ids {
		n := g.Nodes[id]
		deps := append([]string(nil), n.DependsOn...)
		sort.Strings(deps)
		m[id] = canon{Prompt: n.PromptTemplate, DependsOn: deps}
	}
	buf, _ := json.Marshal(m) // map keys are marshaled in sorted order by encoding/json
	mac := hmac.New(sha256.New, key)
	mac.Write(buf)
	return hex.EncodeToString(mac.Sum(nil))
}
```

`Nodes` is held as a map for O(1) lookup during scheduling but is (de)serialized to/from an ordered `[]PlanNode` slice via custom `MarshalJSON`/`UnmarshalJSON` so `plan.json` stays stable and diff-friendly.

### 9.4 Dual-Ledger Structs (`internal/agent/ledger.go`)

```go
// internal/agent/ledger.go
package agent

// TaskLedger is the outer strategic record: the plan DAG reference + high-level goal.
type TaskLedger struct {
	PlanID           string   `json:"plan_id"`
	Goal             string   `json:"goal"`
	Profile          string   `json:"profile"`
	TotalNodes       int      `json:"total_nodes"`
	CompletedNodeIDs []string `json:"completed_node_ids"`
	FailedNodeIDs    []string `json:"failed_node_ids"`
}

// ProgressLedger is the inner tactical record: a per-cycle state snapshot plus a
// 5-question self-reflection.
type ProgressLedger struct {
	PlanID        string              `json:"plan_id"`
	Cycle         int                 `json:"cycle"`
	NodesDone     []string            `json:"nodes_done"`
	NodesFailed   []string            `json:"nodes_failed"`
	NodesInFlight []string            `json:"nodes_in_flight"`
	StallCount    int                 `json:"stall_count"`
	Reflection    *ProgressReflection `json:"reflection,omitempty"` // populated by the LLM self-reflection call
}

// ProgressReflection is the 5-question MagenticOne-style self-reflection, decoded
// from structured LLM output (schema generated via invopop/jsonschema).
type ProgressReflection struct {
	IsGoalAchieved     bool   `json:"is_goal_achieved"`
	WhatHasBeenDone    string `json:"what_has_been_done"`  // 1-2 sentences
	WhatRemains        string `json:"what_remains"`        // 1-2 sentences
	AreThereBlockers   bool   `json:"are_there_blockers"`
	BlockerDescription string `json:"blocker_description"` // empty if no blockers
	RecommendedAction  string `json:"recommended_action"`  // "continue" | "replan" | "abort"
}
```

### 9.5 Core Algorithms

#### 9.5.1 Decomposition Algorithm

```go
// Decompose builds a validated, signed PlanGraph and persists it.
func Decompose(ctx context.Context, opts DecomposeOpts) (*PlanGraph, error)

type DecomposeOpts struct {
	Goal           string
	Profile        string
	PlannerModel   string
	SkillThreshold float64 // default 0.7
	MaxNodes       int     // default 20
	BudgetUSD      *float64
	BudgetTokens   *int
	Store          *store.DB
	HMACKey        []byte
}

// Steps:
//  1. Build the planner prompt with goal, node schema, tool list, and constraints.
//  2. Call the planner model through the internal/llm provider interface with a
//     JSON-schema-constrained response (schema from invopop/jsonschema over PlanNode).
//  3. Decode the []PlanNode from the response (encoding/json).
//  4. For each node, call internal/toolindex to embed the prompt and assign tools
//     whose cosine similarity >= SkillThreshold.
//  5. Validate: TopologicalOrder (ErrCycle), all DependsOn IDs exist, node types
//     valid, len(nodes) <= MaxNodes.
//  6. Assign plan ID, Sign with HMAC, persist plan_graphs + plan_nodes inside a
//     single write transaction (single-writer contract).
//  7. Return the PlanGraph.
```

The planner call uses the provider-neutral `Stream(ctx, Request) -> <-chan Event` interface from `internal/llm`; the decomposition consumes the accumulated `Finish` event and its `Usage` for token/cost accounting.

The planner prompt instructs the LLM to return a JSON object with a `nodes` array. Each element must match the `PlanNode` schema. The prompt includes:
- The full node type enum with descriptions
- A `$node_id.output` variable substitution explanation
- The instruction to keep prompts self-contained (assume no context beyond provided variables)
- A node count constraint (`max_nodes`)
- A prohibition on cycles ("do not create circular dependencies")

#### 9.5.2 Dispatch Loop (the bespoke `internal/queue` scheduler)

Execution runs on the SQLite-backed goroutine DAG scheduler (GO_MIGRATION_PLAN.md decision (5)) — **not** an external durable queue. A single scheduler goroutine owns the promotion loop (`pending → ready` when all `deps_json` predecessors are `done`); a bounded worker pool driven by `golang.org/x/sync/errgroup` executes ready nodes over channels, capped at `parallel`. `context.Context` propagates cancellation (interrupt, budget abort, `fail_fast`). Every state transition is written to SQLite before the next transition, giving durability + replay.

```go
type RunOpts struct {
	Profile        string
	Parallel       int  // default 4
	StallThreshold int  // default 2
	NoReplan       bool
	Store          *store.DB
	Events         chan<- Event // nil-safe; buffered NDJSON/stream sink
}

// RunPlan schedules the DAG to completion and returns the final PlanGraph.
func RunPlan(ctx context.Context, plan *PlanGraph, opts RunOpts) (*PlanGraph, error) {
	sem := make(chan struct{}, opts.Parallel) // bounded concurrency
	g, ctx := errgroup.WithContext(ctx)
	results := make(chan nodeResult)          // workers -> scheduler
	inFlight := 0
	stall := 0
	doneOutputs := map[string]string{}

	for !plan.allTerminal() {
		ready := plan.ReadyNodes()
		if len(ready) == 0 && inFlight == 0 {
			stall++
			if stall >= opts.StallThreshold {
				if err := Replan(ctx, plan, ReplanReq{Reason: "stall", Store: opts.Store}); err != nil {
					return plan, err
				}
			}
			continue
		}
		stall = 0

		// Dispatch up to (parallel - inFlight) ready nodes.
		for _, id := range ready[:min(len(ready), opts.Parallel-inFlight)] {
			node := plan.Nodes[id]
			prompt, err := node.ResolvePrompt(doneOutputs) // blocks dispatch on missing refs
			if err != nil {
				continue
			}
			plan.mark(ctx, opts.Store, id, StatusRunning)
			opts.emit(Event{Kind: "dispatch", NodeID: id})
			inFlight++
			g.Go(func() error {
				sem <- struct{}{}
				defer func() { <-sem }()
				res := executeNode(ctx, id, prompt, opts.Profile) // runs the agent inner loop
				select {
				case results <- res:
				case <-ctx.Done():
				}
				return nil
			})
		}

		// Await one completion (channel receive; no busy-wait poll).
		res := <-results
		inFlight--
		if res.Success {
			plan.markDone(ctx, opts.Store, res)
			doneOutputs[res.NodeID] = res.Output
			opts.emit(Event{Kind: "done", NodeID: res.NodeID})
		} else {
			plan.markFailed(ctx, opts.Store, res)
			if opts.NoReplan {
				plan.abort(ctx, opts.Store)
				return plan, nil
			}
			if err := Replan(ctx, plan, ReplanReq{FailedNode: res.NodeID, Reason: "failure", Store: opts.Store}); err != nil {
				return plan, err
			}
		}
	}
	if err := g.Wait(); err != nil {
		return plan, err
	}
	plan.finalize(ctx, opts.Store)
	return plan, nil
}
```

`executeNode` invokes the agent inner loop in `internal/agent` for that node (mirroring the single-iteration path), for `compute`/`deploy` types wrapped by `internal/sandbox`. Because completions are delivered over a channel rather than a polled future set, the scheduler is event-driven and consumes no CPU while waiting (satisfies NFR-04). All node executions share the same `errgroup`, so a `fail_fast` cancellation propagates through `ctx` to every in-flight worker.

#### 9.5.3 Re-plan Splice Algorithm

```go
type ReplanReq struct {
	FailedNode  string // "" when Reason == "stall"
	Reason      string // "failure" | "stall"
	ReplanModel string
	Store       *store.DB
	HMACKey     []byte
}

// Replan splices LLM-proposed replacement nodes into the live DAG.
func Replan(ctx context.Context, plan *PlanGraph, req ReplanReq) error

// Steps:
//  1. Build the re-plan prompt:
//       - original goal
//       - completed nodes + outputs (truncated to 500 chars each)
//       - failed node spec + error (Reason == "failure")
//       - remaining (not-yet-started) node IDs and their prompts
//       - stall description (Reason == "stall")
//       - instruction: return replacement_nodes[] with updated depends_on
//  2. Call the re-plan model via internal/llm with a JSON-schema-constrained response.
//  3. Decode the []PlanNode replacement list.
//  4. Validate replacements against a trial-spliced copy of the graph:
//       a. TopologicalOrder returns no ErrCycle over (done + replacement + remaining)
//       b. every DependsOn references a done node or another replacement node
//       c. IDs do not collide with existing non-failed node IDs
//     On any validation failure, fall back to NoReplan (abort) — never splice.
//  5. Mark the failed node (if any) Status="skipped", ReplacedBy=first replacement ID.
//  6. Insert replacement nodes into plan.Nodes.
//  7. Drop the failed node from ready/pending consideration.
//  8. Re-sign the plan HMAC.
//  9. Persist updated rows to plan_nodes in one write txn.
// 10. plan_graphs.replan_count++.
```

The whole splice runs inside a single SQLite write transaction so a crash mid-re-plan leaves the DAG in its pre-splice state (replayable from the event log).

#### 9.5.4 Embedding-Based Skill Assignment

This reuses `internal/toolindex` — an `Embedder` interface plus brute-force in-Go cosine over a float32 BLOB index (replacing the Python SentenceTransformer + ChromaDB path; the same substitution the memory subsystem makes per GO_MIGRATION_RESEARCH.md):

```go
// internal/toolindex/assign.go

type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// AssignTools returns tool names whose cosine similarity to the node prompt
// meets threshold, degrading to nil (manual assignment) if no index is available.
func AssignTools(ctx context.Context, idx *Index, emb Embedder, node *queue.PlanNode, threshold float64, topK int) ([]string, error) {
	if idx == nil || !idx.Available() {
		return nil, nil // degrade gracefully; operator can specify tools manually
	}
	vecs, err := emb.Embed(ctx, []string{node.PromptTemplate})
	if err != nil {
		return nil, err
	}
	q := vecs[0]
	// idx.TopK does a brute-force cosine scan over the BLOB-stored tool vectors
	// (≤500 tools => no ANN engine needed).
	hits := idx.TopK(q, topK)
	var assigned []string
	for _, h := range hits {
		if h.Similarity >= threshold { // cosine similarity, not distance
			assigned = append(assigned, h.ToolName)
		}
	}
	return assigned, nil
}
```

### 9.6 Progress Self-Reflection Prompt

The 5-question self-reflection (MagenticOne Dual-Ledger pattern) is a structured LLM call issued at each stall detection check and after re-plan. The prompt is:

```
You are monitoring the execution of a multi-step plan.
Goal: {goal}
Completed nodes: {done_list}
Failed nodes: {failed_list}
In-flight nodes: {inflight_list}

Answer the following 5 questions as a JSON object:
{
  "is_goal_achieved": <true|false>,
  "what_has_been_done": "<1-2 sentence summary>",
  "what_remains": "<1-2 sentence summary>",
  "are_there_blockers": <true|false>,
  "blocker_description": "<description or null>",
  "recommended_action": "<continue|replan|abort>"
}
```

The response is parsed as `ProgressReflection` and stored in `plan_progress_ledger.reflection_json`.

### 9.7 Plan File Format and HMAC Signing

The plan file written by `tag plan decompose` is a JSON object with a top-level `hmac_sha256` field. The HMAC is computed over the canonical serialization of the node map (alphabetically sorted node IDs, prompt and depends_on fields only — see `PlanGraph.Sign`). The signing key is derived from the TAG instance key stored in `~/.tag/instance.key` (32 random bytes from `crypto/rand`, created on first run, written with mode 0600).

```go
// internal/credentials/instance_key.go
func InstanceKey(configDir string) ([]byte, error) {
	path := filepath.Join(configDir, "instance.key")
	if b, err := os.ReadFile(path); err == nil {
		return b, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil { // crypto/rand
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}
```

Verification uses `hmac.Equal` (constant-time) against the recomputed signature. This prevents untrusted plan files from being executed without explicit operator opt-in (via `tag plan validate --allow-unsigned`).

### 9.8 Integration Points

| Package | Integration |
|----------------|-------------|
| `internal/queue` | The bespoke SQLite-backed DAG scheduler. Gains `PlanGraph`, `PlanNode`, `Decompose()`, `RunPlan()`, `Replan()`. The existing `queue_jobs`/`queue_dags` promotion loop (pending→ready on `deps_json`) and errgroup worker pool are reused to schedule `plan_nodes`. |
| `internal/agent` | Gains `TaskLedger`, `ProgressLedger`, `ProgressReflection`, `RunWithLedger()`. The ledger pattern wraps the hand-rolled inner agent loop invoked per node. |
| `internal/toolindex` | `AssignTools()` calls the `Embedder` and runs in-Go cosine over the float32 BLOB tool index (replaces SentenceTransformer + ChromaDB). No schema change to the memory store. |
| `internal/obs` | Cost/budget tracking: `CheckBudget(planID, estimated)` before each dispatch; accumulated cost updated after each node. Also hosts token/cost accounting off `Usage` events. |
| `internal/obs` (tracing) | `otel` spans emitted at node start/end via the custom `SpanProcessor`, tagged `plan_id`/`node_id`/`node_type`/`depends_on`, persisted to the `traces` table. |
| `internal/credentials` | Instance-key derivation (`crypto/rand`); plan file HMAC signature verification (`hmac.Equal`). |
| `internal/sandbox` | Nodes of type `compute` and `deploy` run inside the isolation ladder (landlock+seccomp → docker → gVisor) when `sandbox.enabled=true` in config. |
| `internal/cli` | New `tag plan` cobra command group registered on the root command. |
| `internal/store` | `EnsurePlanSchema` migration; single-writer + `flock` atomic RMW for all plan writes. |

---

## 10. Security Considerations

1. **Plan file tampering (no unsafe deserialization).** Plan files are plain JSON decoded with `encoding/json` into typed structs. There is no `encoding/gob`, reflection-based code loading, or `eval`-equivalent in the plan serialization path. This avoids the RCE vector present in LangGraph's `_freeze()` pickle-based cache keys (GHSA-mhr3-j7m5-c7c9). The HMAC-SHA256 signature (verified with constant-time `hmac.Equal`) provides integrity without deserialization risk.

2. **Prompt injection via goal input.** The `--goal` argument is included verbatim in the planner prompt. A malicious goal string could attempt to override the system prompt or inject additional instructions. Mitigation: the planner prompt wraps the goal in a clearly delimited XML-style tag (`<user_goal>...</user_goal>`) and the system prompt explicitly instructs the model to treat content inside this tag as untrusted user input, not instructions.

3. **Variable substitution injection.** The `$node_id.output` substitution in node prompts could inject malicious content from one node's output into a downstream node's execution context. Mitigation: substituted output is wrapped in a delimiter block (`<upstream_output>...</upstream_output>`) and the downstream execution system prompt treats it as data, not instructions. Additionally, output length is capped at 4096 characters for substitution (full output still persisted in SQLite).

4. **HMAC key exposure.** The instance key is stored at `~/.tag/instance.key` with mode `0600`. It must not be logged, included in plan exports, or transmitted over the network. The `tag plan export` command explicitly excludes the HMAC key from the export. The `--allow-unsigned` flag logs a warning that the plan's integrity cannot be verified.

5. **Command injection via node prompts.** When `executeNode` shells out to tools it uses `os/exec.CommandContext` with an explicit argument slice (`exec.CommandContext(ctx, "tag", "submit", "--prompt", resolvedPrompt)`) — never a shell string. Go's `os/exec` does not invoke a shell, so metacharacters in a resolved prompt are passed as inert argv and cannot be interpreted. Child processes are started with `Setpgid` so a cancelled/timed-out node's whole process group is killed.

6. **Budget bypass via re-plan node insertion.** Re-plan inserts new nodes, which increases the total planned spend. The budget check must run against the accumulated total (already-spent + in-flight estimated cost + new node estimated cost) not just the next single node. The `_check_budget_for_replan` function sums all outstanding node estimated costs before approving the splice.

7. **Cycle injection via re-plan response.** A compromised planner LLM (or a prompt-injected goal) could attempt to introduce cycles in re-plan responses. Mitigation: `validate_dag()` is always run on the post-splice graph before nodes are promoted to `ready`. Any cycle causes the re-plan to be rejected and the engine to abort safely.

8. **SQLite concurrent write safety.** Multiple `tag plan run` invocations against the same plan ID would corrupt state. The engine takes the process-level `gofrs/flock` file lock plus a SQLite `BEGIN EXCLUSIVE` transaction before marking a plan `running` (the single-writer contract from `internal/store`). A second invocation detecting `status='running'` emits an error and exits 1. The `--resume` flag bypasses this check (operator intent). `flock` closes the Windows `fcntl` no-op gap present in the Python implementation.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`internal/queue/plan_test.go`, table-driven)

- `TestTopologicalOrderSimple`: 3-node linear chain, verify order.
- `TestTopologicalOrderParallel`: diamond graph (A→B, A→C, B→D, C→D), verify A first, D last, B/C in between.
- `TestCycleDetection`: A depends on B, B depends on A; verify `errors.Is(err, ErrCycle)` with both IDs in the message.
- `TestReadyNodes`: diamond graph with A done; verify B and C are ready, D is not.
- `TestPromptVariableSubstitution`: node with `$upstream.output`; verify correct substitution.
- `TestPromptVariableMissing`: reference to not-yet-done node; verify a non-nil error.
- `TestHMACSignVerify`: sign a plan, verify with `hmac.Equal`; flip one byte, verify rejection.
- `TestSchemaValidationUnknownType`: decode a node with `type="hack"`; verify a validation error.
- `TestToolAssignmentThreshold`: fake `Embedder` yielding cosine similarities [0.8, 0.65, 0.6]; with threshold 0.7, only the first tool is assigned.
- `TestBudgetCheckBlocksDispatch`: accumulate cost to 99% of budget; verify next dispatch blocked.
- `TestReplanSpliceNoCycle`: re-plan injects 2 replacement nodes; verify final graph has valid topo order.
- `TestReplanSpliceCycleRejected`: re-plan injects nodes that create a cycle; verify rejection + abort.
- `TestStallCounterTriggersReplan`: fake worker returns no transition 3 times; verify re-plan called at `stallThreshold=2`.
- `TestProgressReflectionParsing`: fake LLM returns 5-question JSON; verify `ProgressReflection` fields decode.

Table-driven cases share a `t.Run(tc.name, ...)` harness; the LLM provider and `Embedder` are stubbed via interfaces (no network).

### 11.2 Integration Tests (`internal/queue/plan_integration_test.go`)

Each test opens a temp `modernc.org/sqlite` DB (`t.TempDir()`), so the real store path is exercised with `CGO_ENABLED=0`.

- `TestDecomposePersistsToSQLite`: run `Decompose()` with a stub provider; verify rows in `plan_graphs` and `plan_nodes`.
- `TestRunResumeAfterCrash`: pre-seed a plan with 2 done nodes; verify `RunPlan` with `--resume` only executes remaining nodes.
- `TestParallelDispatch`: 3 independent root nodes with `parallel=3`; a fake node fn sleeps 0.1s; assert overlapping `started_at` timestamps.
- `TestFailedNodeTriggersReplan`: fake worker fails node 3; verify re-plan call, splice, and continuation.
- `TestFullPlanLifecycle`: end-to-end with stub provider + fake worker; verify all events emitted in order over the channel.
- `TestPlanShowOutput`: verify the ASCII DAG render contains all node IDs and correct dependency arrows.
- `TestBudgetAbort`: set budget=$0.005; fake nodes cost $0.003 each; verify abort after node 2 and a cancelled `errgroup`.
- `TestNodeTraceEmission`: verify `SELECT * FROM traces WHERE attributes LIKE '%plan_id%'` returns one span per node.

### 11.3 Property-Based Tests (`internal/queue/plan_prop_test.go`)

Using `pgregory.net/rapid` (or stdlib `testing/quick`):

- Generate random DAGs (up to 15 nodes, random edges); assert `TopologicalOrder()` either returns a valid order or `ErrCycle` — never a wrong order.
- Generate a valid DAG + a random replacement node list; assert the post-splice graph either has a valid topo order or is rejected (`Replan` returns an error and does not mutate the graph).
- Fuzz goal strings through the HMAC sign/verify cycle; assert round-trip integrity via `go test -fuzz`.

### 11.4 Performance Benchmarks (`internal/queue/plan_bench_test.go`, `testing.B`)

- `BenchmarkTopologicalSort100Nodes`: 100-node linear chain, verify <10ms/op via `b.ReportMetric`.
- `BenchmarkParallelSpeedup`: 6-node plan with 3 independent roots, fake worker sleeps 0.5s; verify wall time < 2.0s with `parallel=3` vs 3.5s with `parallel=1`.
- `BenchmarkSkillRetrieval10Nodes`: 10-node plan, real provider `Embedder` + in-Go cosine; verify total `AssignTools` time < 5s.

---

## 12. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-01 | `tag plan decompose --goal "Build a REST API" --profile orchestrator` completes within 15s and prints a plan with ≥ 3 nodes to stdout. | Manual test + CI timing assertion |
| AC-02 | `tag plan validate --plan plan.json` exits 0 for a valid plan and exits 1 for a plan with a cycle, printing the cycle path. | Automated test |
| AC-03 | `tag plan run --plan plan.json --profile coder --parallel 3` dispatches 3 independent root nodes concurrently (verified by overlapping timestamps in `plan_nodes.started_at`). | Integration test |
| AC-04 | After `kill -9` of a running `tag plan run`, re-running with `--resume` skips all nodes with `status='done'` and resumes from pending/failed nodes. | Integration test |
| AC-05 | `tag plan run` triggers re-plan when a node fails, splices replacement nodes, and continues execution to completion without operator intervention. | Integration test with mock failure |
| AC-06 | `tag plan show --plan plan.json` renders an ASCII DAG where all edges correctly reflect the `depends_on` relationships defined in the plan. | Visual + automated string-match test |
| AC-07 | `tag plan run --budget-usd 0.01` halts execution and prints a `budget_exceeded` message when accumulated cost exceeds the cap. | Unit test with mock cost tracking |
| AC-08 | `$node_id.output` in a node prompt is correctly substituted with the actual output of the referenced node before that node's execution. | Unit test |
| AC-09 | A plan.json with a modified byte (simulated tampering) is rejected by `tag plan validate` and `tag plan run` with a `SignatureError`. | Unit test |
| AC-10 | `tag plan decompose --json` produces valid JSON with all required fields (`plan_id`, `goal`, `nodes`, `metadata.hmac_sha256`). | `jq` parse test in CI |
| AC-11 | `tag plan run --json` produces NDJSON with one JSON object per event; events include `dispatch`, `done`, `failed`, `replan`, `plan_complete`. | `python -c "import json; [json.loads(l) for l in open('out.ndjson')]"` |
| AC-12 | `tag plan list` shows the new plan immediately after `decompose` completes, with `status=ready` and correct node count. | Integration test |
| AC-13 | Nodes of type `compute` run inside the sandbox when `sandbox.enabled=true`; verify via sandbox audit log. | Integration test with sandbox enabled |
| AC-14 | Per-node spans appear in `SELECT * FROM traces WHERE attributes LIKE '%plan_id%'` after plan execution. | Integration test |
| AC-15 | `tag plan decompose --dry-run` prints a token count estimate and exits 0 without making any HTTP requests. | Unit test with mocked HTTP |

---

## 13. Dependencies

| Dependency | Version | Type | Justification |
|-----------|---------|------|---------------|
| `modernc.org/sqlite` | pinned GA | Core (project-wide driver) | Pure-Go store for `plan_graphs`/`plan_nodes`/`plan_progress_ledger`; WAL, FTS5, `CGO_ENABLED=0` |
| `golang.org/x/sync/errgroup` | latest | Core | Bounded goroutine worker pool for the DAG scheduler |
| `github.com/invopop/jsonschema` | latest | Core | JSON-schema generation for the structured planner/re-plan/reflection responses |
| Embedding provider client (behind `internal/toolindex` `Embedder`) | — | Core (already required by PRD-043) | Skill-to-tool embedding for node tool assignment; in-Go cosine over BLOB index |
| `gonum.org/v1/gonum/graph/encoding/dot` | latest | Optional | Emitting/parsing Graphviz DOT for `tag plan show --dot` and its tests |
| `pgregory.net/rapid` | latest | Test-only | Property-based testing for DAG validation and re-plan splice correctness (stdlib `testing/quick` fallback) |
| `github.com/spf13/cobra` | latest | Core | `tag plan` command group |
| PRD-013 (tracing) | — | Internal | Per-node `otel` span emission to `traces` table via `internal/obs` |
| PRD-028 (sandbox) | — | Internal | Sandboxed execution for `compute` and `deploy` node types (`internal/sandbox`) |
| PRD-034 (security) | — | Internal | HMAC key management, `internal/credentials` instance-key derivation |
| PRD-033 (queue base) | — | Internal | Existing `internal/queue` schema, `AddJob`, `PromoteReady` scheduler reused/extended |
| PRD-043 (toolindex) | — | Internal | `Embedder` + in-Go cosine index for skill assignment |
| PRD-012 (budget) | — | Internal | `CheckBudget()`, cost accumulation per plan via `internal/obs` |
| PRD-021 (agent loop) | — | Internal | Inner agent loop reused for per-node execution; ledger pattern extends `internal/agent` |

---

## 14. Open Questions

| # | Question | Owner | Resolution Target |
|---|----------|-------|-------------------|
| OQ-1 | Should re-plan calls use a cheaper/faster model (e.g., `haiku`) than the node executor by default? The re-plan prompt is structured and bounded; a smaller model may suffice and reduce cost. | Arch team | Before implementation sprint |
| OQ-2 | Should `tag plan run` support a `--human-approval` mode where each node dispatch is confirmed interactively in the terminal? This is listed as a flag but the UX for async/parallel approval is unclear. | Product | Before implementation sprint |
| OQ-3 | Should completed node outputs be stored in `plan_nodes.output` verbatim (potentially large) or only a content-addressed hash with a separate blob store? Large outputs (e.g., generated code files) could bloat SQLite. | Engineering | During implementation |
| OQ-4 | The `$node_id.output` substitution is text-only. Should structured outputs (JSON objects) be supported via `$node_id.output.field_name` dot-path access? This would require output schema declaration per node. | Product | Defer to v2 |
| OQ-5 | Should `tag plan decompose` optionally run multiple decomposition calls and pick the best plan via self-consistency (PRD-101)? The tradeoff is 3-10x decomposition cost for potentially higher-quality plans. | Arch team | Defer to PRD-101 integration |
| OQ-6 | What is the right stall threshold default? MagenticOne uses 2. A threshold too low triggers unnecessary re-plans; too high wastes time on stuck loops. Should this be auto-calibrated per goal complexity? | Engineering | Empirical testing during alpha |
| OQ-7 | Should `tag plan export` include per-node outputs by default (for auditability) or exclude them by default (for privacy/size)? The `--include-outputs` flag makes it opt-in currently. | Product | Before v1 release |
| OQ-8 | The default embedding model (`all-MiniLM-L6-v2`) is used by `internal/toolindex` via the `Embedder` interface. For skill assignment in decomposition, is a domain-specific model (e.g., `paraphrase-mpnet-base-v2`) worth the size/latency tradeoff? | ML team | During implementation |

---

## 15. Complexity and Timeline

**Total Estimated Effort:** L (2-4 weeks, 1 engineer)

### Phase 1 — Schema and Data Model (Days 1-3)

- Add `EnsurePlanSchema()` to `internal/store` with DDL for `plan_graphs`, `plan_nodes`, `plan_progress_ledger`
- Implement `PlanGraph`, `PlanNode` structs and the `ErrCycle` sentinel in `internal/queue`
- Implement `TopologicalOrder()`, `ReadyNodes()`, `Sign()` methods + custom map↔slice JSON
- Implement `TaskLedger`, `ProgressLedger`, `ProgressReflection` in `internal/agent`
- Write table-driven unit tests for all struct methods (FR-02, FR-10, FR-15 coverage)
- Deliverable: passing unit tests for schema and data model; `tag plan validate` exits 0 for valid fixture

### Phase 2 — Decomposition Pipeline (Days 4-8)

- Implement `Decompose()`: planner prompt construction, JSON-schema-constrained LLM call, PlanNode decode, validation
- Integrate `internal/toolindex` `AssignTools()` for skill-to-tool mapping
- Implement `tag plan decompose` cobra handler in `internal/cli`
- Implement `tag plan validate` cobra handler
- Write integration tests for decompose + SQLite persistence (AC-01, AC-10, AC-15)
- Deliverable: `tag plan decompose --goal "..." --profile orchestrator` produces a valid signed plan.json

### Phase 3 — Execution Engine (Days 9-16)

- Implement `RunPlan()` dispatch loop in `internal/queue`: scheduler goroutine + errgroup worker pool + channel completions, bounded by `--parallel`
- Implement `executeNode()` wrapper over the `internal/agent` inner loop (`os/exec.CommandContext` with `Setpgid` where it shells out)
- Implement `$node_id.output` variable substitution (FR-10)
- Implement resume logic (FR-12)
- Implement budget enforcement hooks (FR-13, `internal/obs` integration)
- Implement per-node tracing (FR-14, `otel` via `internal/obs`)
- Implement stall detection (FR-09, G9)
- Write integration tests for parallel dispatch, resume, budget abort (AC-03, AC-04, AC-07, AC-08)
- Deliverable: `tag plan run --plan plan.json --profile coder --parallel 3` runs to completion on a test plan

### Phase 4 — Re-plan and Dynamic Adaptation (Days 17-21)

- Implement `Replan()`: failure context prompt, JSON-schema-constrained LLM call, splice validation, DAG update
- Implement `ProgressReflection` 5-question self-reflection call
- Integrate re-plan trigger into the scheduler (failure path + stall path)
- Write integration tests for re-plan trigger, splice validation, cycle-in-replan rejection (AC-05, FR-07, FR-08)
- Write property-based tests (`pgregory.net/rapid`) for DAG and re-plan splice correctness
- Deliverable: `tag plan run` survives a node failure, re-plans, and completes

### Phase 5 — Display, Export, and Polish (Days 22-26)

- Implement `tag plan show` ASCII DAG renderer and `--dot` Graphviz output
- Implement `tag plan list` SQLite query handler
- Implement `tag plan export` with optional `--include-outputs`
- Implement `--json` / NDJSON event streaming for `tag plan run`
- Sandbox integration for `compute`/`deploy` node types
- Run full acceptance criteria suite; fix failures
- Deliverable: all 15 AC items pass; `tag plan show --dot` produces valid Graphviz

### Phase 6 — Documentation and Eval (Days 27-28)

- Add eval suite `evals/tdag.yaml` covering 5 representative decompose+run scenarios
- Run `tag eval run --suite evals/tdag.yaml --profile orchestrator` as CI gate
- Update `docs/prd/INDEX.md` with PRD-105 entry
- Deliverable: CI green; eval suite baseline score ≥ 0.75 task-completion

---

*PRD-105 authored for TAG CLI. GitHub issue: #349.*

