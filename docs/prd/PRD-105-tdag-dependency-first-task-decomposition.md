# PRD-105: Dependency-First Hierarchical Task Decomposition (TDAG) (`tag plan decompose`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** L (2-4 weeks)
**Category:** Advanced Reasoning & Planning
**Affects:** `dag.py + loop_agent.py`
**Depends on:** PRD-027 (eval framework), PRD-028 (sandbox), PRD-013 (agent tracing/observability), PRD-034 (security), PRD-033 (dependency-aware task queue), PRD-043 (vector-based tool retrieval), PRD-025 (semantic memory), PRD-021 (agent loop/autonomous mode), PRD-012 (cost tracking/budget)
**Inspired by:** TDAG paper (2024), HuggingGPT, LLM Compiler, Plan-and-Execute
**GitHub issue:** #349

---

## 1. Overview

Complex agentic goals — "Build and deploy a REST API", "Refactor the authentication module and update all tests", "Research competitors and produce a slide deck" — cannot be reliably achieved in a single LLM call or even in a flat sequence of tool calls. They require decomposition into subtasks with explicit dependency relationships, parallel execution where the dependency graph permits, and dynamic adaptation when individual subtasks fail. The TAG agent loop (`loop_agent.py`) currently operates as a flat iteration: a single goal is pursued iteration-by-iteration with no structural awareness of sub-goals, their ordering constraints, or opportunities for concurrency. When a subtask fails, the loop retries the full goal with no surgical intervention.

TDAG (Task DAG) introduces `tag plan decompose`, a planning command that takes a high-level goal and emits a directed acyclic graph (DAG) of typed subtasks with explicit dependency edges. The decomposition is performed by an LLM acting as a planner, guided by a structured output schema that enforces node typing (compute, research, review, write, deploy), skill-to-tool assignment via SentenceTransformer similarity matching (threshold θ=0.7), and a topological sort validation pass before the plan is persisted. The resulting plan is a JSON artifact — a `plan.json` — that can be inspected, edited, re-validated, and then executed via `tag plan run`. Execution honors the partial order defined by the DAG: nodes with no unresolved dependencies enter a ready queue immediately, while nodes with unsatisfied dependencies remain pending. Ready nodes can be dispatched concurrently, with a configurable parallelism cap.

The execution engine builds on the MagenticOne Dual-Ledger pattern: a Task Ledger (the DAG itself, with per-node status and outputs) is the outer strategic record, while a Progress Ledger (a structured JSON self-reflection updated after every node completion) tracks tactical state. A stall counter monitors for repeated identical states; when the stall counter exceeds a configurable threshold (default 2), the engine triggers a re-plan call that splices replacement nodes into the live DAG without restarting already-completed work. This dynamic replanning differentiates TDAG from simpler Plan-and-Execute architectures that must restart from scratch on failure.

The feature integrates with TAG's existing infrastructure at every layer: the DAG spec and all execution state live in SQLite (WAL mode, using `open_db()`) under the `plan_graphs` and `plan_nodes` tables. Per-node span traces are emitted to the `traces` table via the existing `tracing.py` module. Tool retrieval for subtask-to-skill assignment reuses `tool_retrieval.py` (SentenceTransformer + ChromaDB). Budget enforcement per-plan uses `budget.py`. Sandbox isolation for code-execution subtasks uses `sandbox.py`. The `--json` flag on every subcommand makes all outputs machine-readable for CI pipelines and the upcoming web dashboard.

The inspiration sources each contribute a specific mechanism: TDAG (2024) contributes the skill-retrieval loop and ordered dependency list with dynamic updates; HuggingGPT contributes the typed node taxonomy and model-to-task assignment pattern; LLM Compiler contributes the parallel dispatch pattern and the `$node_id` variable substitution syntax for passing outputs between nodes; Plan-and-Execute contributes the two-phase planner/executor separation and the re-plan trigger on executor failure.

---

## 2. Problem Statement

### 2.1 The agent loop has no structural understanding of subtask dependencies

`loop_agent.py` runs a single goal through repeated agent invocations. There is no mechanism to express that "writing unit tests" must follow "implementing the function", or that "deploying to staging" must follow both "building the Docker image" and "passing the test suite". When the LLM produces a plan implicitly in its chain-of-thought, that plan is ephemeral — it is not persisted, not validated for cycles or missing dependencies, not tracked per-subtask, and not available for inspection or reuse. If the agent fails mid-way, the operator has no visibility into which subtasks completed and which did not. The only recovery path is a full restart.

### 2.2 Parallelism opportunities are left on the table

Many real-world goals decompose into subtasks that are structurally independent and could execute concurrently. Research subtasks, documentation generation, and test writing for different modules are often fully parallel. The current loop_agent serializes all work into a single sequential thread. On goals with natural parallelism, this means the wall-clock time scales linearly with subtask count rather than with the critical path length. For a 10-subtask goal where 5 subtasks are independent, this is a 2-5x wall-clock penalty.

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
| G6 | Tool-to-subtask assignment uses SentenceTransformer cosine similarity (threshold θ=0.7) against the indexed tool registry, reusing `tool_retrieval.py` — so subtasks automatically get the right tools without manual specification. |
| G7 | All plan operations (`decompose`, `run`, `show`, `list`, `export`) support `--json` output for CI/pipeline integration. |
| G8 | Per-plan budget caps (token and USD) are enforced via `budget.py`; the plan engine checks remaining budget before dispatching each node. |
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
| Tool assignment accuracy | SentenceTransformer skill retrieval assigns ≥ 1 relevant tool for ≥ 90% of non-trivial subtask descriptions | Eval against 50 manually labeled subtask→tool pairs |
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
| FR-03 | Tool-to-subtask assignment is performed by computing cosine similarity between each node's `prompt` embedding and indexed tool descriptions via `tool_retrieval.py`. Only tools with similarity ≥ `--skill-threshold` are assigned. | Integration test: subtask "write Python code" assigns `bash`/`write_file`; subtask "search the web" assigns `web_search`. |
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
| FR-14 | Per-node spans are emitted to `traces` table via `tracing.py`, tagged with `plan_id`, `node_id`, `node_type`, and `depends_on` attributes. | Integration test: verify `SELECT * FROM traces WHERE attributes LIKE '%plan_id%'` returns one span per executed node. |
| FR-15 | Plan files are HMAC-SHA256 signed using the TAG instance key on write. `tag plan validate` and `tag plan run` verify the signature before trusting the file. | Unit test: modify one byte of plan.json; verify rejection with `SignatureError`. |
| FR-16 | `tag plan show --dot` emits a valid Graphviz DOT file representing the DAG, with node shapes colored by type and edge labels showing dependency direction. | Unit test: parse DOT output with `pydot`; verify node count and edge count match plan spec. |
| FR-17 | `tag plan export --include-outputs` produces a JSON archive containing the full plan spec plus the `output` field of every completed node. | Integration test: run a plan to completion; export; verify all node outputs present. |
| FR-18 | `tag plan list` reads from `plan_graphs` and emits one row per plan with `id`, `goal` (truncated to 60 chars), `status`, total node count, done count, failed count, and `created_at`. | Unit test with seeded SQLite; verify output row count and field values. |
| FR-19 | Node types are constrained to the enum `{research, compute, write, review, deploy}`. The planner prompt includes the enum definition and the schema validator rejects unknown types. | Unit test: inject node with `type: "unknown"`; verify `SchemaValidationError`. |
| FR-20 | `tag plan decompose --dry-run` constructs the planner prompt, validates profile existence, and prints the estimated prompt token count and cost without making any LLM API call. | Unit test: verify no HTTP calls made (mock httpx); verify cost estimate printed. |

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
| NFR-09 | Skill retrieval via `tool_retrieval.py` (SentenceTransformer embed + ChromaDB query) must complete within 2 seconds per node at decomposition time. | Benchmark test |
| NFR-10 | All new modules must pass `ruff check` and `mypy --strict` with zero errors. | CI gate |
| NFR-11 | The feature must not introduce new dependencies beyond what is already required by `tool_retrieval.py` (sentence-transformers, chromadb) and the existing TAG stack. | `pip install tag` must not pull new packages not already optional. |
| NFR-12 | `tag plan decompose` and `tag plan run` must be idempotent with respect to SQLite state: running `decompose` twice for the same goal and plan ID is a no-op (returns existing plan). | Integration test: run decompose twice; verify single row in `plan_graphs`. |

---

## 9. Technical Design

### 9.1 New and Modified Files

| File | Change Type | Description |
|------|-------------|-------------|
| `src/tag/dag.py` | **Extend** | Add `PlanGraph`, `PlanNode` dataclasses; `decompose()`, `run_plan()`, `replan()`, `validate_dag()` functions; new SQLite schema (`plan_graphs`, `plan_nodes`). |
| `src/tag/loop_agent.py` | **Extend** | Add `DualLedger` (TaskLedger + ProgressLedger) dataclasses; `run_with_ledger()` function; stall detection logic. |
| `src/tag/controller.py` | **Extend** | Add `cmd_plan_decompose`, `cmd_plan_run`, `cmd_plan_show`, `cmd_plan_list`, `cmd_plan_export`, `cmd_plan_validate` command handlers under `tag plan` subparser. |
| `tests/test_tdag.py` | **New** | Unit and integration tests for all FR items. |
| `tests/test_tdag_properties.py` | **New** | Property-based tests (Hypothesis) for DAG validation, cycle detection, re-plan splice correctness. |

### 9.2 SQLite DDL

These tables are created by `ensure_plan_schema(conn)` in `dag.py`, called on first access via `open_db()`.

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

### 9.3 Core Dataclasses

```python
# src/tag/dag.py  (additions)
from __future__ import annotations

import hashlib
import hmac
import json
import uuid
from dataclasses import dataclass, field
from enum import Enum
from typing import Literal


class NodeType(str, Enum):
    RESEARCH = "research"
    COMPUTE  = "compute"
    WRITE    = "write"
    REVIEW   = "review"
    DEPLOY   = "deploy"


class NodeStatus(str, Enum):
    PENDING  = "pending"
    READY    = "ready"
    RUNNING  = "running"
    DONE     = "done"
    FAILED   = "failed"
    SKIPPED  = "skipped"


@dataclass
class PlanNode:
    id: str
    label: str
    node_type: NodeType
    prompt_template: str          # may contain $node_id.output variable refs
    depends_on: list[str] = field(default_factory=list)
    tools: list[str] = field(default_factory=list)
    status: NodeStatus = NodeStatus.PENDING
    output: str | None = None
    error: str | None = None
    replaced_by: str | None = None
    tokens_used: int = 0
    cost_usd: float = 0.0
    started_at: str | None = None
    completed_at: str | None = None

    def resolve_prompt(self, outputs: dict[str, str]) -> str:
        """Substitute $node_id.output references with actual outputs."""
        import re
        def _sub(m: re.Match) -> str:
            ref_id = m.group(1)
            if ref_id not in outputs:
                raise ValueError(f"Output reference ${ref_id}.output not yet available")
            return outputs[ref_id]
        return re.sub(r'\$([a-zA-Z_][a-zA-Z0-9_]*)\.output', _sub, self.prompt_template)


@dataclass
class PlanGraph:
    id: str
    goal: str
    profile: str
    nodes: dict[str, PlanNode]    # keyed by node ID
    planner_model: str
    decompose_tokens: int = 0
    decompose_cost_usd: float = 0.0
    skill_threshold: float = 0.7
    budget_usd: float | None = None
    budget_tokens: int | None = None
    accumulated_cost_usd: float = 0.0
    accumulated_tokens: int = 0
    stall_count: int = 0
    replan_count: int = 0
    status: str = "ready"
    hmac_sha256: str = ""

    def topological_order(self) -> list[str]:
        """Kahn's algorithm. Raises CycleError if a cycle is detected."""
        in_degree: dict[str, int] = {nid: 0 for nid in self.nodes}
        adj: dict[str, list[str]] = {nid: [] for nid in self.nodes}
        for nid, node in self.nodes.items():
            for dep in node.depends_on:
                if dep not in self.nodes:
                    raise ValueError(f"Node {nid!r} depends on unknown node {dep!r}")
                adj[dep].append(nid)
                in_degree[nid] += 1
        queue = [nid for nid, deg in in_degree.items() if deg == 0]
        order: list[str] = []
        while queue:
            nid = queue.pop(0)
            order.append(nid)
            for succ in adj[nid]:
                in_degree[succ] -= 1
                if in_degree[succ] == 0:
                    queue.append(succ)
        if len(order) != len(self.nodes):
            # Find cycle members for error reporting
            cycle_members = [nid for nid in self.nodes if nid not in order]
            raise CycleError(f"Cycle detected involving nodes: {cycle_members}")
        return order

    def ready_nodes(self) -> list[str]:
        """Return node IDs whose deps are all done and whose status is pending."""
        done_ids = {nid for nid, n in self.nodes.items() if n.status == NodeStatus.DONE}
        return [
            nid for nid, node in self.nodes.items()
            if node.status == NodeStatus.PENDING
            and all(dep in done_ids for dep in node.depends_on)
        ]

    def sign(self, key: bytes) -> str:
        """Compute HMAC-SHA256 over canonical JSON representation (nodes only)."""
        canonical = json.dumps(
            {nid: {"prompt_template": n.prompt_template, "depends_on": sorted(n.depends_on)}
             for nid, n in sorted(self.nodes.items())},
            sort_keys=True,
        ).encode()
        return hmac.new(key, canonical, hashlib.sha256).hexdigest()


class CycleError(ValueError):
    pass
```

### 9.4 Dual-Ledger Dataclasses (loop_agent.py additions)

```python
# src/tag/loop_agent.py  (additions)
from __future__ import annotations

from dataclasses import dataclass, field


@dataclass
class TaskLedger:
    """Outer strategic record: the plan DAG reference + high-level goal."""
    plan_id: str
    goal: str
    profile: str
    total_nodes: int
    completed_node_ids: list[str] = field(default_factory=list)
    failed_node_ids: list[str] = field(default_factory=list)


@dataclass
class ProgressLedger:
    """Inner tactical record: per-cycle state snapshot + 5-question self-reflection."""
    plan_id: str
    cycle: int
    nodes_done: list[str] = field(default_factory=list)
    nodes_failed: list[str] = field(default_factory=list)
    nodes_in_flight: list[str] = field(default_factory=list)
    stall_count: int = 0
    # Structured 5-question reflection (populated by LLM self-reflection call):
    reflection: ProgressReflection | None = None


@dataclass
class ProgressReflection:
    """5-question MagenticOne-style self-reflection, parsed from structured LLM output."""
    is_goal_achieved: bool
    what_has_been_done: str         # 1-2 sentences
    what_remains: str               # 1-2 sentences
    are_there_blockers: bool
    blocker_description: str | None # None if no blockers
    recommended_action: str         # "continue" | "replan" | "abort"
```

### 9.5 Core Algorithms

#### 9.5.1 Decomposition Algorithm

```python
def decompose(
    goal: str,
    profile: str,
    *,
    planner_model: str,
    skill_threshold: float = 0.7,
    max_nodes: int = 20,
    budget_usd: float | None = None,
    budget_tokens: int | None = None,
    db_path: Path,
    hmac_key: bytes,
) -> PlanGraph:
    """
    1. Build planner prompt with goal, node schema, tool list, and constraints.
    2. Call planner_model with structured output (JSON mode).
    3. Parse PlanNode list from response.
    4. For each node, run SentenceTransformer similarity against tool registry
       (tool_retrieval.py) and assign tools with score >= skill_threshold.
    5. Validate: topological sort (CycleError), all depends_on IDs exist,
       node types valid, count <= max_nodes.
    6. Assign plan_id, sign with HMAC, persist to plan_graphs + plan_nodes.
    7. Return PlanGraph.
    """
```

The planner prompt instructs the LLM to return a JSON object with a `nodes` array. Each element must match the `PlanNode` schema. The prompt includes:
- The full node type enum with descriptions
- A `$node_id.output` variable substitution explanation
- The instruction to keep prompts self-contained (assume no context beyond provided variables)
- A node count constraint (`max_nodes`)
- A prohibition on cycles ("do not create circular dependencies")

#### 9.5.2 Dispatch Loop

```python
import asyncio
import concurrent.futures

def run_plan(
    plan: PlanGraph,
    profile: str,
    *,
    parallel: int = 4,
    stall_threshold: int = 2,
    no_replan: bool = False,
    db_path: Path,
    config_path: str,
    event_callback: Callable[[dict], None] | None = None,
) -> PlanGraph:
    """
    Dispatch loop (synchronous wrapper over async inner loop):

    while not all nodes done/failed/skipped:
        ready = plan.ready_nodes()
        if not ready and not in_flight:
            stall_count += 1
            if stall_count >= stall_threshold:
                trigger replan(reason="stall")
            else:
                sleep(1.0)
            continue
        stall_count = 0

        # Dispatch up to (parallel - len(in_flight)) ready nodes
        to_dispatch = ready[:parallel - len(in_flight)]
        for node_id in to_dispatch:
            resolved_prompt = plan.nodes[node_id].resolve_prompt(done_outputs)
            future = executor.submit(_execute_node, node_id, resolved_prompt, ...)
            in_flight[node_id] = future
            mark_node_running(node_id, db)
            emit_event("dispatch", node_id)

        # Poll for completed futures (non-blocking)
        for node_id, fut in list(in_flight.items()):
            if fut.done():
                result = fut.result()
                if result.success:
                    mark_node_done(node_id, result, db)
                    done_outputs[node_id] = result.output
                    emit_event("done", node_id, result)
                else:
                    mark_node_failed(node_id, result.error, db)
                    if no_replan:
                        abort_plan(plan)
                        return plan
                    trigger_replan(plan, failed_node=node_id, db=db, ...)
                in_flight.pop(node_id)

        sleep(1.0)  # poll interval

    finalize_plan(plan, db)
    return plan
```

The `_execute_node` function submits a `tag submit` subprocess call (same pattern as `loop_agent.py`'s `_run_iteration`), capturing stdout/stderr and exit code.

#### 9.5.3 Re-plan Splice Algorithm

```python
def replan(
    plan: PlanGraph,
    *,
    failed_node_id: str | None,
    reason: str,
    replan_model: str,
    db_path: Path,
    hmac_key: bytes,
) -> PlanGraph:
    """
    1. Build re-plan prompt:
       - Original goal
       - Completed nodes + their outputs (truncated to 500 chars each)
       - Failed node spec + error (if reason="failure")
       - Remaining (not-yet-started) node IDs and their prompts
       - Stall description (if reason="stall")
       - Instruction: return replacement_nodes[] and updated depends_on for each

    2. Call replan_model with structured output.
    3. Parse replacement PlanNode list.
    4. Validate replacement nodes:
       a. No new cycles when spliced into existing (done + replacement + remaining) graph
       b. All depends_on IDs reference either done nodes or other replacement nodes
       c. Node IDs do not collide with existing non-failed node IDs
    5. Mark failed_node (if any) as status='replaced', set replaced_by=first replacement ID
    6. Insert replacement nodes into plan.nodes dict
    7. Remove failed_node from ready/pending queue consideration
    8. Re-sign plan HMAC
    9. Persist updated nodes to plan_nodes table
    10. Update plan_graphs.replan_count += 1
    11. Return updated PlanGraph
    """
```

#### 9.5.4 SentenceTransformer Skill Assignment

This reuses `tool_retrieval.py`'s existing `get_embed_model()` and ChromaDB query pattern:

```python
def assign_tools(
    node: PlanNode,
    *,
    embed_model,           # SentenceTransformer instance
    chroma_collection,     # ChromaDB collection of indexed tools
    threshold: float = 0.7,
    top_k: int = 8,
) -> list[str]:
    """
    1. Embed node.prompt_template using embed_model.encode()
    2. Query chroma_collection.query(query_embeddings=[emb], n_results=top_k)
    3. Filter results where distance <= (1 - threshold)  # cosine: distance = 1 - similarity
    4. Return list of tool names from filtered results
    """
    if not tool_retrieval.is_available():
        return []  # Degrade gracefully; human can specify tools manually
    emb = embed_model.encode([node.prompt_template])[0].tolist()
    results = chroma_collection.query(query_embeddings=[emb], n_results=top_k)
    assigned = []
    for tool_name, distance in zip(results["ids"][0], results["distances"][0]):
        if distance <= (1.0 - threshold):
            assigned.append(tool_name)
    return assigned
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

The plan file written by `tag plan decompose` is a JSON object with a top-level `hmac_sha256` field. The HMAC is computed over the canonical serialization of the `nodes` dict (alphabetically sorted node IDs, prompt and depends_on fields only). The signing key is derived from the TAG instance key stored in `~/.tag/instance.key` (32 random bytes, created on first run, stored with mode 0600).

```python
def _instance_key(config_dir: Path) -> bytes:
    key_path = config_dir / "instance.key"
    if key_path.exists():
        return key_path.read_bytes()
    key = os.urandom(32)
    key_path.write_bytes(key)
    key_path.chmod(0o600)
    return key
```

This prevents untrusted plan files from being executed without explicit operator opt-in (via `tag plan validate --allow-unsigned`).

### 9.8 Integration Points

| Existing Module | Integration |
|----------------|-------------|
| `dag.py` | Extended with `PlanGraph`, `PlanNode`, `decompose()`, `run_plan()`, `replan()`, `ensure_plan_schema()`. The existing `queue_dags` / dependency promotion logic is complementary and reused for queue-based dispatch fallback. |
| `loop_agent.py` | Extended with `DualLedger`, `ProgressLedger`, `ProgressReflection`, `run_with_ledger()`. The new ledger pattern wraps the existing `_run_iteration` function. |
| `tool_retrieval.py` | `assign_tools()` calls `get_embed_model()` and queries the existing ChromaDB collection. No modification needed. |
| `budget.py` | `check_budget(plan_id, estimated_cost)` called before each node dispatch. Plan accumulated cost updated after each node. |
| `tracing.py` | `emit_span(plan_id=..., node_id=..., node_type=..., ...)` called at node start and end using existing span helpers. |
| `security.py` | HMAC key derivation, plan file signature verification. |
| `sandbox.py` | Nodes of type `compute` and `deploy` run inside the existing sandbox when `sandbox.enabled=true` in config. |
| `controller.py` | New `cmd_plan_*` handlers registered under `tag plan` subparser using the existing argparse pattern. |

---

## 10. Security Considerations

1. **Plan file tampering (pickle deserialization is not used).** Plan files are plain JSON. There is no `pickle` or `eval` in the plan serialization path. This avoids the RCE vector present in LangGraph's `_freeze()` pickle-based cache keys (GHSA-mhr3-j7m5-c7c9). The HMAC-SHA256 signature provides integrity without deserialization risk.

2. **Prompt injection via goal input.** The `--goal` argument is included verbatim in the planner prompt. A malicious goal string could attempt to override the system prompt or inject additional instructions. Mitigation: the planner prompt wraps the goal in a clearly delimited XML-style tag (`<user_goal>...</user_goal>`) and the system prompt explicitly instructs the model to treat content inside this tag as untrusted user input, not instructions.

3. **Variable substitution injection.** The `$node_id.output` substitution in node prompts could inject malicious content from one node's output into a downstream node's execution context. Mitigation: substituted output is wrapped in a delimiter block (`<upstream_output>...</upstream_output>`) and the downstream execution system prompt treats it as data, not instructions. Additionally, output length is capped at 4096 characters for substitution (full output still persisted in SQLite).

4. **HMAC key exposure.** The instance key is stored at `~/.tag/instance.key` with mode `0600`. It must not be logged, included in plan exports, or transmitted over the network. The `tag plan export` command explicitly excludes the HMAC key from the export. The `--allow-unsigned` flag logs a warning that the plan's integrity cannot be verified.

5. **Subprocess injection via node prompts.** `_execute_node` invokes `tag submit` as a subprocess with `--prompt` passed as a command-line argument. If the resolved prompt contains shell metacharacters, this could be exploited. Mitigation: the prompt is always passed via `subprocess.run(..., args=[..., "--prompt", resolved_prompt])` as a list (not a shell string), so no shell interpretation occurs.

6. **Budget bypass via re-plan node insertion.** Re-plan inserts new nodes, which increases the total planned spend. The budget check must run against the accumulated total (already-spent + in-flight estimated cost + new node estimated cost) not just the next single node. The `_check_budget_for_replan` function sums all outstanding node estimated costs before approving the splice.

7. **Cycle injection via re-plan response.** A compromised planner LLM (or a prompt-injected goal) could attempt to introduce cycles in re-plan responses. Mitigation: `validate_dag()` is always run on the post-splice graph before nodes are promoted to `ready`. Any cycle causes the re-plan to be rejected and the engine to abort safely.

8. **SQLite concurrent write safety.** Multiple `tag plan run` invocations against the same plan ID would corrupt state. The engine acquires an application-level lock (SQLite `BEGIN EXCLUSIVE`) before marking a plan `running`. A second invocation detecting `status='running'` emits an error and exits 1. The `--resume` flag bypasses this check (operator intent).

---

## 11. Testing Strategy

### 11.1 Unit Tests (`tests/test_tdag.py`)

- `test_topological_order_simple`: 3-node linear chain, verify order.
- `test_topological_order_parallel`: diamond graph (A→B, A→C, B→D, C→D), verify A first, D last, B/C in between.
- `test_cycle_detection`: A depends on B, B depends on A; verify `CycleError` with both IDs.
- `test_ready_nodes`: diamond graph with A done; verify B and C are ready, D is not.
- `test_prompt_variable_substitution`: node with `$upstream.output`; verify correct substitution.
- `test_prompt_variable_missing`: reference to not-yet-done node; verify `ValueError`.
- `test_hmac_sign_verify`: sign a plan, verify signature; flip one byte, verify rejection.
- `test_schema_validation_unknown_type`: inject node with `type="hack"`; verify `SchemaValidationError`.
- `test_tool_assignment_threshold`: mock ChromaDB returns distances [0.2, 0.35, 0.4]; with threshold 0.7, only tools with distance ≤ 0.3 assigned.
- `test_budget_check_blocks_dispatch`: accumulate cost to 99% of budget; verify next dispatch blocked.
- `test_replan_splice_no_cycle`: re-plan injects 2 replacement nodes; verify final graph has valid topo order.
- `test_replan_splice_cycle_rejected`: re-plan injects nodes that create cycle; verify rejection.
- `test_stall_counter_triggers_replan`: mock dispatch cycle with no transitions 3 times; verify re-plan called at stall_threshold=2.
- `test_progress_reflection_parsing`: mock LLM returns 5-question JSON; verify `ProgressReflection` fields.

### 11.2 Integration Tests (`tests/test_tdag_integration.py`)

- `test_decompose_persists_to_sqlite`: run `decompose()` with mock LLM; verify rows in `plan_graphs` and `plan_nodes`.
- `test_run_resume_after_crash`: pre-seed plan with 2 done nodes; verify `run_plan(resume=True)` only executes remaining nodes.
- `test_parallel_dispatch`: 3 independent root nodes with `parallel=3`; verify all 3 dispatched within 2 seconds.
- `test_failed_node_triggers_replan`: mock executor fails node 3; verify re-plan call, splice, and continuation.
- `test_full_plan_lifecycle`: end-to-end with mock LLM and mock executor; verify all events emitted in order.
- `test_plan_show_output`: verify ASCII DAG render contains all node IDs and correct dependency arrows.
- `test_budget_abort`: set budget=$0.005; mock nodes cost $0.003 each; verify abort after node 2.
- `test_node_trace_emission`: verify `SELECT * FROM traces WHERE attributes LIKE '%plan_id%'` returns one span per node.

### 11.3 Property-Based Tests (`tests/test_tdag_properties.py`)

Using `hypothesis`:
- `@given(dag_strategy())` — generate random DAGs (up to 15 nodes, random edges); verify `topological_order()` either succeeds with a valid order or raises `CycleError` (never produces wrong order).
- `@given(dag_strategy(), node_strategy())` — generate valid DAG + random replacement node list from re-plan; verify post-splice graph either has valid topo order or is rejected.
- `@given(st.text())` — fuzz goal strings through the HMAC sign/verify cycle; verify round-trip integrity.

### 11.4 Performance Benchmarks (`tests/test_tdag_perf.py`)

- `bench_topological_sort_100_nodes`: 100-node linear chain, verify <10ms.
- `bench_parallel_speedup`: 6-node plan with 3 independent roots, mock executor sleeps 0.5s; verify wall time < 2.0s with `parallel=3` vs 3.5s with `parallel=1`.
- `bench_skill_retrieval_10_nodes`: 10-node plan, real SentenceTransformer embed; verify total assign_tools time < 5s.

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
| `sentence-transformers` | ≥ 2.2.0 | Optional (already required by PRD-043) | Skill-to-tool SentenceTransformer embedding for node tool assignment |
| `chromadb` | ≥ 0.4.0 | Optional (already required by PRD-043) | Vector store for tool registry queries during decomposition |
| `hypothesis` | ≥ 6.0 | Test-only | Property-based testing for DAG validation and re-plan splice correctness |
| `pydot` | ≥ 1.4 | Optional | Parsing DOT output in `--dot` tests; not required for production |
| PRD-013 (tracing) | — | Internal | Per-node span emission to `traces` table |
| PRD-028 (sandbox) | — | Internal | Sandboxed execution for `compute` and `deploy` node types |
| PRD-034 (security) | — | Internal | HMAC key management, `security.py` instance key derivation |
| PRD-033 (dag.py base) | — | Internal | Existing `ensure_schema`, `add_job`, `promote_ready_jobs` functions extended |
| PRD-043 (tool_retrieval) | — | Internal | `get_embed_model()`, ChromaDB collection access for skill assignment |
| PRD-012 (budget) | — | Internal | `check_budget()`, cost accumulation per plan |
| PRD-021 (loop_agent) | — | Internal | `_run_iteration()` reused for per-node execution; ledger pattern extends this module |

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
| OQ-8 | The SentenceTransformer model `all-MiniLM-L6-v2` is used in `tool_retrieval.py`. For skill assignment in decomposition, is a domain-specific model (e.g., `paraphrase-mpnet-base-v2`) worth the size tradeoff? | ML team | During implementation |

---

## 15. Complexity and Timeline

**Total Estimated Effort:** L (2-4 weeks, 1 engineer)

### Phase 1 — Schema and Data Model (Days 1-3)

- Add `ensure_plan_schema()` to `dag.py` with DDL for `plan_graphs`, `plan_nodes`, `plan_progress_ledger`
- Implement `PlanGraph`, `PlanNode`, `CycleError` dataclasses
- Implement `topological_order()`, `ready_nodes()`, `sign()` methods
- Implement `DualLedger`, `ProgressLedger`, `ProgressReflection` in `loop_agent.py`
- Write unit tests for all dataclass methods (FR-02, FR-10, FR-15 coverage)
- Deliverable: passing unit tests for schema and data model; `tag plan validate` exits 0 for valid fixture

### Phase 2 — Decomposition Pipeline (Days 4-8)

- Implement `decompose()` function: planner prompt construction, structured LLM call, PlanNode parsing, schema validation
- Integrate `tool_retrieval.py` `assign_tools()` for skill-to-tool mapping
- Implement `tag plan decompose` CLI handler in `controller.py`
- Implement `tag plan validate` CLI handler
- Write integration tests for decompose + SQLite persistence (AC-01, AC-10, AC-15)
- Deliverable: `tag plan decompose --goal "..." --profile orchestrator` produces a valid signed plan.json

### Phase 3 — Execution Engine (Days 9-16)

- Implement `run_plan()` dispatch loop: ready queue, in-flight tracking, parallel dispatch via `ThreadPoolExecutor`
- Implement `_execute_node()` subprocess wrapper (tag submit pattern from loop_agent)
- Implement `$node_id.output` variable substitution (FR-10)
- Implement resume logic (FR-12)
- Implement budget enforcement hooks (FR-13, budget.py integration)
- Implement per-node tracing (FR-14, tracing.py integration)
- Implement stall detection (FR-09, G9)
- Write integration tests for parallel dispatch, resume, budget abort (AC-03, AC-04, AC-07, AC-08)
- Deliverable: `tag plan run --plan plan.json --profile coder --parallel 3` runs to completion on a test plan

### Phase 4 — Re-plan and Dynamic Adaptation (Days 17-21)

- Implement `replan()` function: failure context prompt, structured LLM call, splice validation, DAG update
- Implement `ProgressReflection` 5-question self-reflection call
- Integrate re-plan trigger into dispatch loop (failure path + stall path)
- Write integration tests for re-plan trigger, splice validation, cycle-in-replan rejection (AC-05, FR-07, FR-08)
- Write property-based tests (hypothesis) for DAG and re-plan splice correctness
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

