# PRD-083: Agent-as-Tool Pattern: Invoke Specialist Agents as Function Tools (`tag agent tool`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (1-2 weeks)
**Category:** Multi-Agent Interoperability Protocols
**Affects:** `internal/cli`, `internal/agent`, `internal/tool`, `internal/runtime`
**Depends on:** PRD-013 (Agent Tracing & Observability), PRD-027 (Eval Framework), PRD-028 (Sandbox Code Execution), PRD-034 (Security / Secret Scanning), PRD-001 (Structured Memory Configuration), PRD-014 (MCP Server Registry), PRD-077 (Scope-Based Tool Filtering)
**Inspired by:** OpenAI Agents SDK agent-as-tool, LangGraph subgraph-as-node, AutoGen nested agents
**GitHub Issue:** #347

---

## 1. Overview

Modern multi-agent architectures increasingly rely on the ability to decompose complex tasks across specialized agents that each excel in a narrow domain — one agent focused on code generation, another on knowledge retrieval, another on data analysis. TAG already supports profile-based specialization and multi-agent orchestration via swarm and DAG constructs. What is missing is the ability for one running agent (the orchestrator) to invoke another TAG profile as a discrete, synchronous function call, receiving structured output back as a tool response within the same conversation turn. This pattern — agent-as-tool — closes that gap.

The agent-as-tool pattern is conceptually distinct from both handoff and spawning. A handoff transfers execution control to another agent; the original agent's conversation ends. A spawn creates a background task with no return value flowing back into the orchestrator's reasoning. Agent-as-tool is a synchronous invocation: the orchestrator calls `write_code(task="implement a binary search")`, the `coder` profile executes the task in a sandboxed subprocess, and the result is returned to the orchestrator as a tool response string — exactly as if any other MCP tool had been called. The orchestrator can then use that result as input to the next reasoning step, chain multiple specialist calls, or compare outputs from different specialist agents on the same sub-task.

This design is directly inspired by the OpenAI Agents SDK `agent.as_tool()` method, which wraps an agent as a `FunctionTool` accepting a string input and returning a string output. LangGraph's subgraph-as-node pattern achieves the same semantics in a graph-execution model by treating a compiled `StateGraph` as a node callable. AutoGen's nested agent pattern is similar, though AutoGen Swarm selects the next agent by scanning for `HandoffMessage.target` rather than explicit tool-call dispatch. TAG's approach follows the OpenAI Agents SDK model most closely: registered tool names are explicitly declared, schema is generated from a `description` and optional typed input schema, and execution is fully synchronous from the orchestrator's perspective.

The implementation is anchored in the `internal/tool` package (a new `AgentTool` type implementing the unified Go tool interface) and the `internal/cli` package with a new `tag agent tool` command family. Tool registrations are persisted to a new `agent_tools` table in `~/.tag/runtime/tag.sqlite3` via `modernc.org/sqlite` (pure-Go, `CGO_ENABLED=0`). At submit time, when `--enable-agent-tools` is passed, the submit handler queries the `agent_tools` table and registers synthetic tools with the orchestrator's tool set before the first orchestrator LLM call. Because agent tools implement the same `Info()`/`Run(ctx, ToolCall)`/`ProviderOptions()` interface as builtin and MCP tools, they are indistinguishable to the `internal/agent` loop. When the orchestrator emits a tool call matching a registered agent tool name, the tool's `Run` method spins up a child agent loop via a goroutine (managed by `golang.org/x/sync/errgroup`), captures its output, and returns it as a tool response. The orchestrator's context window sees only the tool name, input, and response — all the complexity of the child agent's execution is hidden behind the tool interface boundary.

Security and isolation are first-class concerns. Each agent-as-tool invocation runs in the same sandboxing infrastructure as `tag submit` with `--sandbox` (PRD-028), inheriting the profile's allowed-tool list and budget constraints. The child run is linked to the parent run in the `runs` table via a `parent_run_id` column, making the full invocation tree visible to `tag trace` (PRD-013). Budgets propagate from parent to child, and a configurable `max_agent_tool_depth` prevents runaway recursive invocations.

---

## 2. Problem Statement

### 2.1 Orchestrator Agents Cannot Delegate to Specialists as Native Tool Calls

TAG's current multi-agent constructs — swarm and DAG — operate at the run level. A `tag swarm` job fans out work to multiple profiles and aggregates results after all profiles complete. A DAG job sequences profile invocations in a declared dependency order. Neither construct allows an orchestrator agent to make an inline decision, mid-reasoning, to delegate a sub-task to a specialist and immediately use the result in its next reasoning step. The orchestrator must have all necessary capabilities baked into its own tool list, which forces either bloated profiles (violating single-responsibility) or capability gaps that the model must reason around. The consequence is that complex tasks requiring sequenced research → implementation → validation workflows are handled by a single monolithic profile, leading to worse specialization, higher token costs (from larger tool lists), and harder-to-debug agent behavior.

### 2.2 Profile Specialization Has No Programmatic Invocation Primitive

TAG profiles encode deep, carefully curated agent specializations: a `coder` profile with the right editor tools, the right system prompt, and the right context window management strategy; a `researcher` profile with web search, memory retrieval, and citation tools; a `tester` profile that knows how to run test suites and interpret failures. These specializations have significant engineering investment behind them. Currently, the only way to leverage them programmatically from within another agent run is to use `tag queue` to enqueue a background task and poll for its result — which is entirely asynchronous, breaks the orchestrator's reasoning continuity, and requires the user to wire the result back in manually. There is no single-command primitive that says "call the researcher profile with this question and return me the answer as a string."

### 2.3 Multi-Agent Workflows Lack a Structured Delegation Audit Trail

When a complex task is manually broken across multiple `tag submit` invocations by the user, there is no relationship between those runs in SQLite. The `runs` table has no parent/child linkage. Consequently, `tag trace` cannot reconstruct the full causal chain from orchestrator prompt to specialist sub-tasks. Costs are fragmented across disconnected run records. This makes it impossible to reason about the total cost of a multi-agent workflow, to detect which specialist invocation caused a regression, or to replay a workflow with a different specialist profile. The agent-as-tool pattern, combined with `parent_run_id` linkage, provides this structured audit trail as a natural side effect.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | CLI command `tag agent tool register` persists a named function-tool binding from a tool name to a TAG profile, with description and optional input schema. |
| G2 | CLI command `tag agent tool list` displays all registered agent tools with profile mapping, creation timestamp, and usage count. |
| G3 | CLI command `tag agent tool unregister` removes a named agent tool registration. |
| G4 | `tag submit --enable-agent-tools` injects registered agent tools into the orchestrator's tool list and intercepts tool calls to dispatch child agent runs. |
| G5 | Child agent runs are synchronous from the orchestrator's perspective: the orchestrator LLM blocks on the tool response. |
| G6 | All child runs are linked to the parent run via `parent_run_id` in the `runs` table, making the full invocation tree queryable. |
| G7 | Budget propagation: the parent run's remaining token/cost budget is shared with child runs; a child that would exceed the remaining budget fails with a budget error returned as the tool response. |
| G8 | Depth limiting: `max_agent_tool_depth` (configurable, default 3) prevents recursive or circular agent-as-tool invocations. |
| G9 | `tag agent tool list --json` emits machine-readable JSON for scripting and CI. |
| G10 | Sandboxing: child agent runs inherit the sandbox policy of their registered profile (PRD-028). |
| G11 | Tracing: each child run creates a child span under the parent run's trace (PRD-013), with `tool_name` and `child_profile` span attributes. |
| G12 | `tag agent tool register --dry-run` validates the profile exists and the tool name is valid without writing to SQLite. |

---

## 4. Non-Goals

| ID | Non-Goal |
|----|-----------| 
| NG1 | Agent handoff semantics (transfer of full conversation history to a child agent). This PRD covers synchronous tool invocation only; handoff is a separate pattern. |
| NG2 | Remote agent invocation over A2A, ACP, or ANP protocols. This PRD covers local TAG profile invocation. Cross-protocol invocation is covered by companion Cluster E PRDs. |
| NG3 | Streaming child agent output back to the orchestrator in real-time. The child run must complete before its output is returned as a tool response. Streaming orchestration is a future enhancement. |
| NG4 | Dynamic tool registration at orchestrator runtime (the orchestrator LLM requesting that a new agent tool be registered). Registrations are static, declared before the run starts. |
| NG5 | Tool output schema validation. The child agent returns a string; structured JSON schema enforcement on the output is a future feature. |
| NG6 | Cross-machine invocation. Agent tools always invoke profiles available on the local TAG installation. |
| NG7 | Automatic agent tool recommendation (suggesting which profiles to register as tools based on task analysis). |
| NG8 | Modifying the registered profile's system prompt at invocation time. Tool input maps to the `--prompt` of the child run; system prompt comes from the profile as-is. |

---

## 5. Success Metrics

| Metric | Baseline | Target | Measurement Method |
|--------|----------|--------|--------------------|
| Orchestrator task completion rate on multi-specialist tasks (eval suite) | Not measured (single-profile baseline) | >= 15% improvement over single monolithic profile | `tag eval run --suite evals/multi-agent.yaml --profile orchestrator --enable-agent-tools` vs. single-profile baseline |
| Time to register a tool and run first multi-agent task | No primitive exists | <= 3 CLI commands, <= 60 seconds | Manual timing of `register` + `submit` happy path |
| Child run `parent_run_id` linkage completeness | 0% (no linkage today) | 100% of agent-tool-spawned runs have `parent_run_id` set | `SELECT COUNT(*) FROM runs WHERE parent_run_id IS NOT NULL` during integration test |
| Budget overrun in child runs | Untracked | 0 child runs exceed parent's remaining budget | Assert in integration test: child run cost <= parent remaining budget |
| Depth-limit enforcement | No enforcement | Invocation at depth > `max_agent_tool_depth` returns tool error, not a crash | Integration test: profile A calls profile B calls profile A, assert error at depth 4 |
| `tag agent tool list --json` schema stability | N/A | Stable across patch versions; no breaking field renames | JSON schema snapshot test in CI |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|------------|----------|
| U1 | Platform engineer | Run `tag agent tool register --profile coder --as-tool write_code --description "Write or modify code given a task description"` | My orchestrator agent can call `write_code()` as a native tool and receive implemented code as the response, without manually managing a second agent session |
| U2 | Researcher | Run `tag submit --enable-agent-tools --profile orchestrator --prompt "Research the performance of SQLite WAL mode, then implement a benchmark script"` | The orchestrator automatically delegates research to the `researcher` profile and implementation to the `coder` profile, chaining their outputs without my intervention |
| U3 | DevOps engineer | Run `tag agent tool list --json` | I can programmatically enumerate available agent tools in a CI pipeline to validate that required specialists are registered before kicking off an automated multi-agent workflow |
| U4 | Developer | Inspect `tag trace --run-id <orchestrator-run-id>` after a multi-agent run | I can see the full span tree: orchestrator span -> `write_code` tool call span -> child coder run spans, enabling me to diagnose which specialist caused a failure |
| U5 | Cost-conscious user | See a pre-run cost estimate when `--enable-agent-tools` is active | I understand that each agent tool invocation incurs additional token cost before I commit to a potentially expensive orchestration run |
| U6 | Security engineer | Know that child agent runs are sandboxed to the permissions declared in the specialist profile | I can audit that the `researcher` agent-as-tool cannot write files even when called from a trusted orchestrator that has write permissions |
| U7 | Developer | Run `tag agent tool register --dry-run --profile nonexistent --as-tool foo` | I get an immediate error message saying the profile does not exist, rather than silently registering a broken tool that fails only at runtime |
| U8 | Team lead | Run `tag agent tool list` and see last-used timestamp and invocation count for each tool | I can identify stale or unused agent tool registrations and clean them up with `tag agent tool unregister` |

---

## 7. Proposed CLI Surface

All subcommands live under `tag agent tool`. The `tag agent` namespace already exists conceptually; `tool` is a new second-level subcommand group within it.

### 7.1 `tag agent tool register`

Register a TAG profile as a named callable function tool.

```
tag agent tool register \
  --profile <profile-name> \
  --as-tool <tool-name> \
  [--description "Human-readable description for the LLM"] \
  [--input-schema '{"type":"object","properties":{"task":{"type":"string"}},"required":["task"]}'] \
  [--timeout-seconds 120] \
  [--max-output-tokens 4096] \
  [--sandbox] \
  [--no-sandbox] \
  [--dry-run] \
  [--json]
```

**Arguments:**
- `--profile` (required): Name of an existing TAG profile in `~/.tag/profiles/<name>/`. Validation: profile directory must exist.
- `--as-tool` (required): The function tool name the orchestrator LLM will use to invoke this agent. Must match `^[a-z][a-z0-9_]{0,63}$` (snake_case, max 64 chars). Must be unique across all registered agent tools; attempting to register a duplicate name is an error unless `--force` is passed.
- `--description`: Human-readable description injected into the tool schema as the `description` field. The LLM uses this to decide when to call the tool. Defaults to the profile's `description` field in its config YAML, or `"Run the <profile> agent with the given prompt"` if none is set.
- `--input-schema`: JSON string of a JSON Schema object describing the tool's input. Defaults to `{"type": "object", "properties": {"prompt": {"type": "string", "description": "The task or question for the agent"}}, "required": ["prompt"]}`. When provided, the specified schema is stored and served to the orchestrator LLM verbatim.
- `--timeout-seconds`: Maximum wall-clock time in seconds the child run may take before it is forcibly terminated and an error is returned as the tool response. Default: 120. Max: 600.
- `--max-output-tokens`: Hard cap on the child run's output length (in tokens). Output is truncated to this limit before being returned as the tool response string. Default: 4096.
- `--sandbox` / `--no-sandbox`: Override the profile's default sandbox policy for this tool registration. `--sandbox` forces sandboxed execution regardless of the profile setting. `--no-sandbox` disables sandboxing (requires explicit flag to prevent accidental use).
- `--dry-run`: Validate all arguments, check the profile exists, validate `--input-schema` as valid JSON Schema, print what would be registered. No write to SQLite.
- `--json`: Output the registration record as JSON on success.

**Example success output (TTY):**
```
Agent tool registered:
  Tool name : write_code
  Profile   : coder
  Description: Write or modify code given a task description
  Input schema: {"type":"object","properties":{"prompt":{"type":"string"}},"required":["prompt"]}
  Timeout   : 120s
  Max output: 4096 tokens
  Sandbox   : yes (profile default)
```

**Example success output (--json):**
```json
{
  "id": "at-a1b2c3d4",
  "tool_name": "write_code",
  "profile": "coder",
  "description": "Write or modify code given a task description",
  "input_schema": {"type":"object","properties":{"prompt":{"type":"string"}},"required":["prompt"]},
  "timeout_seconds": 120,
  "max_output_tokens": 4096,
  "sandbox": true,
  "created_at": "2026-06-17T09:00:00Z",
  "updated_at": "2026-06-17T09:00:00Z"
}
```

### 7.2 `tag agent tool list`

List all registered agent tools.

```
tag agent tool list [--json] [--profile <filter>]
```

**Arguments:**
- `--profile`: Filter to only show tools backed by the specified profile.
- `--json`: Emit a JSON array of tool registration objects (same schema as `register --json` output, plus `invocation_count` and `last_used_at`).

**Example TTY output:**
```
AGENT TOOLS (2 registered)
─────────────────────────────────────────────────────────────────────────────
 Tool Name        Profile      Timeout  Calls  Last Used            Created
 write_code       coder        120s     47     2026-06-16 14:22:01  2026-06-01
 research_topic   researcher   180s     12     2026-06-15 09:11:44  2026-06-03
─────────────────────────────────────────────────────────────────────────────
Use 'tag submit --enable-agent-tools' to make these tools available to orchestrators.
```

### 7.3 `tag agent tool unregister`

Remove a registered agent tool.

```
tag agent tool unregister <tool-name> [--yes] [--json]
```

**Arguments:**
- `<tool-name>` (positional, required): The `--as-tool` name to remove.
- `--yes`: Skip confirmation prompt.
- `--json`: Emit confirmation as JSON.

If the tool name does not exist in the `agent_tools` table, exit 1 with error: `"No agent tool named '<tool-name>' is registered. Run 'tag agent tool list' to see available tools."`.

### 7.4 `tag agent tool show`

Show full detail for a single registered agent tool.

```
tag agent tool show <tool-name> [--json]
```

Displays: all fields from `agent_tools` row, full `input_schema` JSON pretty-printed, invocation count and last-used timestamp from `agent_tool_invocations` aggregate, and last 5 invocation records (run ID, parent run ID, duration, status).

### 7.5 `tag submit` with `--enable-agent-tools`

Extend the existing `tag submit` command with agent-tool support:

```
tag submit \
  --enable-agent-tools \
  [--agent-tools write_code,research_topic] \
  [--max-agent-tool-depth 3] \
  --profile orchestrator \
  --prompt "Research the top 3 Python async web frameworks, then implement a hello-world server in the fastest one"
```

**New flags on `tag submit`:**
- `--enable-agent-tools`: Activates agent-as-tool mode. All tools in the `agent_tools` table are injected into the orchestrator's tool list. Without this flag, agent tools are never visible to the LLM (zero-friction for existing workflows).
- `--agent-tools <name>[,<name>...]`: When specified alongside `--enable-agent-tools`, only the listed tool names are injected (allowlist). Useful for scoping orchestrator access.
- `--max-agent-tool-depth <n>`: Maximum recursion depth for agent-tool invocations. Default: 3. Value of 1 means the orchestrator can call agent tools but those child runs cannot themselves call agent tools.

**Runtime behavior when orchestrator emits a tool call matching a registered agent tool:**

The matched tool's `Run(ctx, ToolCall)` method drives the dispatch (agent tools and MCP tools share the same interface, so the agent loop needs no special branch). It:
1. Looks up the `agent_tools` record by `tool_name`.
2. Checks current invocation depth against `max_agent_tool_depth`; if at limit, returns tool error string.
3. Checks remaining parent budget; if insufficient, returns tool error string.
4. Extracts the `prompt` field (or full JSON input if custom schema) from the tool call arguments.
5. Spawns a child run via the `internal/runtime` submit core (goroutine under `errgroup`): `Profile=<registered-profile>`, `Prompt=<extracted-prompt>`, `ParentRunID=<current-run-id>`, `AgentToolDepth=<current-depth+1>`.
6. Awaits child run completion via `errgroup.Wait()` (synchronous, blocking), bounded by `context.WithTimeout`.
7. Returns child run's final output (truncated to `max_output_tokens`) as the tool response string.
8. Records the invocation in `agent_tool_invocations`.

**Example orchestrator session (annotated):**

```
User prompt: "Research the performance characteristics of SQLite WAL mode, then implement a Python benchmark script."

[Orchestrator LLM, turn 1]
<tool_call name="research_topic">
  {"prompt": "SQLite WAL mode performance characteristics: throughput, concurrency, crash recovery, filesystem requirements"}
</tool_call>

[TAG runtime intercepts — spawns child run with profile=researcher]
[Child researcher run completes in 34s, 1,847 tokens]

<tool_response name="research_topic">
  WAL mode enables concurrent reads with a single writer. Write throughput increases
  3-5x over DELETE journal mode under concurrent read workloads. WAL files persist
  until a full checkpoint occurs... [truncated at 4096 tokens]
</tool_response>

[Orchestrator LLM, turn 2]
<tool_call name="write_code">
  {"prompt": "Implement a Python benchmark script that compares SQLite WAL mode vs DELETE journal mode. Measure throughput (inserts/sec) and read latency under 4 concurrent readers. Use pytest-benchmark."}
</tool_call>

[TAG runtime intercepts — spawns child run with profile=coder]
[Child coder run completes in 52s, 3,201 tokens]

<tool_response name="write_code">
  import sqlite3
  import threading
  import time
  import pytest
  ...
</tool_response>

[Orchestrator LLM, turn 3]
Task complete. The research identified WAL mode's key advantages and I've implemented
a benchmark script that validates those claims with pytest-benchmark...
```

---

## 8. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | `tag agent tool register` MUST validate that `--profile` refers to an existing directory under `~/.tag/profiles/`. If the profile does not exist, exit 1 with a clear error message. |
| FR-02 | `tag agent tool register` MUST validate `--as-tool` matches `^[a-z][a-z0-9_]{0,63}$`. Reject names starting with digits, containing hyphens, or exceeding 64 characters. |
| FR-03 | Tool names MUST be unique. Registering an existing name without `--force` exits 1 with: `"Tool name '<name>' is already registered (profile: <profile>). Use --force to overwrite."` |
| FR-04 | When `--input-schema` is provided, it MUST be valid JSON and MUST be a valid JSON Schema object (type: object). Invalid JSON exits 1. Non-object schemas exit 1 with a descriptive error. |
| FR-05 | The `agent_tools` table MUST be created by the `internal/memory` store's `Migrate(ctx)` routine within the existing embedded-migration set (`//go:embed` SQL files applied under a `flock` guard), following the established schema migration pattern. |
| FR-06 | `tag agent tool list` with no filters MUST return all rows from `agent_tools` joined with aggregated invocation counts from `agent_tool_invocations`. An empty table MUST print a helpful zero-state message rather than an empty table. |
| FR-07 | `tag agent tool unregister` MUST delete the row from `agent_tools` and all associated rows from `agent_tool_invocations`. It MUST NOT fail silently if the tool name does not exist. |
| FR-08 | When `--enable-agent-tools` is passed to `tag submit`, all tools from `agent_tools` (or the subset specified by `--agent-tools`) MUST be appended to the tool definitions list before the first LLM call. Each tool definition MUST include `name`, `description`, and `input_schema`. |
| FR-09 | The tool interception logic MUST distinguish agent-tool calls from MCP tool calls. Agent tools are identified by matching the `tool_call.name` against the `agent_tools` table at dispatch time, before MCP lookup. |
| FR-10 | Each intercepted agent-tool invocation MUST spawn a child run with `parent_run_id` set to the current run's ID. The child `runs` row MUST be inserted before the child agent is invoked. |
| FR-11 | Depth enforcement: at the start of each agent-tool dispatch, the system MUST query the depth of the current run in the parent chain (via recursive `parent_run_id` traversal). If depth >= `max_agent_tool_depth`, the tool response MUST be: `"Error: maximum agent tool depth (<n>) reached. Cannot invoke <tool_name>."` No child run is spawned. |
| FR-12 | Budget propagation: before spawning a child run, the system MUST check `remaining_budget = parent_budget - parent_spend_so_far`. If the child profile's estimated minimum cost exceeds `remaining_budget`, the tool response MUST be a budget error. The child run MUST be allocated at most `remaining_budget` tokens/USD. |
| FR-13 | The child run's output (final agent message, not including intermediate steps) MUST be truncated to `max_output_tokens` tokens using the same tokenizer used for cost estimation, before being returned as the tool response string. |
| FR-14 | Each agent-tool invocation MUST be recorded in `agent_tool_invocations` with: `tool_name`, `parent_run_id`, `child_run_id`, `duration_ms`, `input_tokens`, `output_tokens`, `status` (`success` | `error` | `timeout` | `budget_exceeded`). |
| FR-15 | If a child run exceeds `timeout_seconds`, the child run MUST be forcibly terminated, its `runs.status` set to `timeout`, and the tool response MUST be: `"Error: agent tool '<name>' timed out after <n> seconds."` |
| FR-16 | `tag agent tool register --dry-run` MUST NOT write to SQLite. It MUST print the registration record that would be created and exit 0. If validation fails, it MUST print the validation error and exit 1. |
| FR-17 | When `--agent-tools` is specified without `--enable-agent-tools`, the CLI MUST warn: `"--agent-tools has no effect without --enable-agent-tools"` and proceed with a normal single-agent run. |
| FR-18 | Sandbox policy: if `--sandbox` was set at registration, the child run MUST be invoked with sandbox enforcement active (equivalent to `tag submit --sandbox`). If `--no-sandbox` was set, sandbox is disabled for the child run. If neither was set, the profile's own default sandbox setting applies. |
| FR-19 | A child agent run MUST NOT have access to the parent's conversation history. The child receives only the `prompt` extracted from the tool call arguments, and is constructed via the shared `internal/runtime` submit path (not by copying the parent's in-memory agent state). |
| FR-20 | `tag agent tool list --json` MUST emit a valid JSON array to stdout. Each element MUST include: `id`, `tool_name`, `profile`, `description`, `input_schema`, `timeout_seconds`, `max_output_tokens`, `sandbox`, `created_at`, `updated_at`, `invocation_count`, `last_used_at`. |

---

## 9. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **Synchronous blocking latency:** The orchestrator's LLM streaming output MUST be paused (no new tokens requested) while awaiting the child run. The wait MUST be implemented by blocking on the child goroutine via `errgroup.Wait()` (or a result channel receive), not a polling sleep loop, to avoid wasted CPU. Cancellation MUST propagate through `context.Context`. |
| NFR-02 | **Child run isolation:** A child agent run failure (exception, OOM, crash) MUST NOT crash the orchestrator run. All child run errors are caught, converted to a tool response error string, and the orchestrator continues. |
| NFR-03 | **SQLite WAL concurrency:** The parent run and child run may concurrently write to SQLite (parent writes steps; child writes its own steps). Both share the process-wide `*sql.DB` handle (backed by `modernc.org/sqlite`) opened with `_journal_mode=WAL` and `_busy_timeout=5000`. Writes are serialized through a single-connection writer pool (`db.SetMaxOpenConns(1)` for the writer handle); no additional Go-level mutexes are required beyond WAL mode plus the standard `database/sql` connection pooling. |
| NFR-04 | **Tracing span completeness:** Every agent-tool invocation MUST produce: (a) a `tool_call` span on the parent trace with `tag.tool_name`, `tag.child_profile`, `tag.child_run_id`; (b) the child run's own root span with `tag.parent_run_id` set. These spans MUST be linked via OpenTelemetry span links (PRD-013). |
| NFR-05 | **Tool list injection size:** Injecting N agent tools adds N synthetic tool definitions to the orchestrator's system prompt. Each tool definition MUST be compact: `name` + `description` (<=200 chars) + `input_schema` should total <= 300 tokens per tool. This limits token overhead to < 3,000 tokens for 10 registered tools. |
| NFR-06 | **Backward compatibility:** Adding `--enable-agent-tools` to `tag submit` is purely additive. Existing `tag submit` invocations without this flag are entirely unaffected. The `agent_tools` table creation (in the embedded migration applied by `Migrate(ctx)`) is idempotent via `CREATE TABLE IF NOT EXISTS`. |
| NFR-07 | **No new required dependencies:** The agent-as-tool implementation uses only the Go stdlib (`context`, `database/sql`, `encoding/json`, `regexp`, `time`) plus modules already present in TAG's `go.mod` (`golang.org/x/sync/errgroup`, `modernc.org/sqlite`, `invopop/jsonschema`). No new third-party modules are added to `go.mod`. |
| NFR-08 | **`max_agent_tool_depth` is enforced at dispatch time, not at registration time.** A tool that would create a cycle (profile A calls profile B which calls profile A) is only detected and blocked at the point the depth limit is reached, not at registration. The error message MUST indicate which profile caused the depth limit breach. |

---

## 10. Technical Design

### 10.1 New Files / Packages

- **`internal/tool/agent_tool.go`** — Core types: the `AgentTool` struct (which implements the unified tool interface `Info() ToolInfo` / `Run(ctx, ToolCall) (ToolResult, error)` / `ProviderOptions() ProviderOptions`), plus `BuildToolSchema()` and `TruncateOutput()`. An `AgentTool` value is registered into the agent's tool set exactly like a builtin or MCP tool.
- **`internal/tool/agent_registry.go`** — `Registry` type: CRUD over the `agent_tools` / `agent_tool_invocations` tables, backed by an injected `*sql.DB`. Fully independently testable without running a live agent.
- **`internal/tool/agent_dispatch.go`** — the child-agent spawn logic invoked from `AgentTool.Run`: depth check, budget check, and `runChildAgent()` which starts a child agent loop via `errgroup`.
- **`internal/tool/schema.go`** — validation helpers (`ValidateToolName`, `ValidateInputSchema`) built on `invopop/jsonschema`.
- No standalone migration binary is required — the DDL below is added to the embedded migration set under `internal/memory/migrations/` and applied by the store's `Migrate(ctx)` method.

### 10.2 SQLite DDL

The following DDL is added as a new embedded migration file (`internal/memory/migrations/00NN_agent_tools.sql`), applied by the store's `Migrate(ctx)` method against the `modernc.org/sqlite` connection:

```sql
-- Agent tool registrations: maps a function-tool name to a TAG profile
CREATE TABLE IF NOT EXISTS agent_tools (
  id                TEXT PRIMARY KEY,          -- "at-" + uuid4 hex (12 chars)
  tool_name         TEXT NOT NULL UNIQUE,      -- snake_case name, e.g. "write_code"
  profile           TEXT NOT NULL,             -- TAG profile name, e.g. "coder"
  description       TEXT NOT NULL,             -- LLM-facing tool description
  input_schema_json TEXT NOT NULL,             -- JSON Schema object as text
  timeout_seconds   INTEGER NOT NULL DEFAULT 120,
  max_output_tokens INTEGER NOT NULL DEFAULT 4096,
  sandbox_override  TEXT,                      -- NULL | "force_on" | "force_off"
  invocation_count  INTEGER NOT NULL DEFAULT 0,
  last_used_at      TEXT,                      -- ISO-8601 UTC, updated on each use
  created_at        TEXT NOT NULL,             -- ISO-8601 UTC
  updated_at        TEXT NOT NULL              -- ISO-8601 UTC
);

CREATE INDEX IF NOT EXISTS idx_agent_tools_profile
  ON agent_tools(profile);

-- Per-invocation audit log for agent-tool calls
CREATE TABLE IF NOT EXISTS agent_tool_invocations (
  id             TEXT PRIMARY KEY,             -- uuid4
  tool_name      TEXT NOT NULL,                -- FK to agent_tools.tool_name (not enforced for speed)
  parent_run_id  TEXT NOT NULL,                -- runs.id of the orchestrator run
  child_run_id   TEXT,                         -- runs.id of the spawned child run (NULL if depth error)
  invoked_at     TEXT NOT NULL,                -- ISO-8601 UTC start of invocation
  duration_ms    INTEGER,                      -- wall clock ms, NULL if not yet complete
  input_tokens   INTEGER,                      -- tokens in the tool call arguments
  output_tokens  INTEGER,                      -- tokens in the tool response (before truncation)
  status         TEXT NOT NULL,                -- "success" | "error" | "timeout" | "budget_exceeded" | "depth_limit"
  error_message  TEXT,                         -- populated on non-success status
  FOREIGN KEY(parent_run_id) REFERENCES runs(id)
);

CREATE INDEX IF NOT EXISTS idx_ati_parent_run
  ON agent_tool_invocations(parent_run_id);

CREATE INDEX IF NOT EXISTS idx_ati_tool_name
  ON agent_tool_invocations(tool_name);
```

Additionally, the existing `runs` table must gain a `parent_run_id` column and `agent_tool_depth` column. Because SQLite does not support `ADD COLUMN` with non-NULL defaults on existing rows, these are added with nullable semantics:

```sql
-- Added in the migration file, after the existing runs table CREATE:
-- These ALTER TABLE statements are guarded in Go (SQLite returns a
-- "duplicate column name" error if the column already exists).
ALTER TABLE runs ADD COLUMN parent_run_id TEXT REFERENCES runs(id);
ALTER TABLE runs ADD COLUMN agent_tool_depth INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_runs_parent
  ON runs(parent_run_id)
  WHERE parent_run_id IS NOT NULL;
```

Migrations are applied under a `gofrs/flock` advisory lock (atomic RMW across processes). The `ALTER TABLE` statements are made idempotent in Go by tolerating the duplicate-column error:

```go
func addRunColumns(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		"ALTER TABLE runs ADD COLUMN parent_run_id TEXT REFERENCES runs(id)",
		"ALTER TABLE runs ADD COLUMN agent_tool_depth INTEGER NOT NULL DEFAULT 0",
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
				continue // already migrated; idempotent
			}
			return fmt.Errorf("agent_tools migration: %w", err)
		}
	}
	return nil
}
```

### 10.3 Core Types (`internal/tool/agent_tool.go`)

```go
package tool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"time"
)

var toolNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

const maxToolNameLen = 64

// InvocationStatus enumerates the terminal states of a dispatch.
type InvocationStatus string

const (
	StatusSuccess        InvocationStatus = "success"
	StatusError          InvocationStatus = "error"
	StatusTimeout        InvocationStatus = "timeout"
	StatusBudgetExceeded InvocationStatus = "budget_exceeded"
	StatusDepthLimit     InvocationStatus = "depth_limit"
)

// AgentTool is a persisted registration of a TAG profile as a callable
// function tool. It implements the unified tool interface
// (Info / Run / ProviderOptions) so it is indistinguishable to the agent
// loop from a builtin or MCP tool.
type AgentTool struct {
	ID              string          `json:"id"`                // "at-" + 12 hex chars
	ToolName        string          `json:"tool_name"`         // snake_case, matches toolNameRE
	Profile         string          `json:"profile"`           // TAG profile directory name
	Description     string          `json:"description"`       // LLM-facing description
	InputSchema     json.RawMessage `json:"input_schema"`      // JSON Schema object (type: object)
	TimeoutSeconds  int             `json:"timeout_seconds"`   // default 120
	MaxOutputTokens int             `json:"max_output_tokens"` // default 4096
	SandboxOverride *string         `json:"sandbox_override"`  // nil | "force_on" | "force_off"
	InvocationCount int             `json:"invocation_count"`
	LastUsedAt      *string         `json:"last_used_at"`
	CreatedAt       string          `json:"created_at"`
	UpdatedAt       string          `json:"updated_at"`

	// dispatcher is injected at registration time so Run can spawn child runs.
	dispatcher *Dispatcher
}

// NewID returns a fresh agent-tool id: "at-" + 12 hex chars.
func NewID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "at-" + hex.EncodeToString(b)
}

// Info returns the tool definition surfaced to the orchestrator LLM. It is the
// Go analogue of the former to_llm_tool_definition(); description is hard-capped
// for token efficiency.
func (t *AgentTool) Info() ToolInfo {
	desc := t.Description
	if len(desc) > 200 {
		desc = desc[:200]
	}
	return ToolInfo{
		Name:        t.ToolName,
		Description: desc,
		InputSchema: t.InputSchema,
	}
}

// ProviderOptions lets provider-specific hints (e.g. cache control) attach to
// the tool; agent tools use defaults.
func (t *AgentTool) ProviderOptions() ProviderOptions { return ProviderOptions{} }

// Run spins up a child agent loop and returns its output as the tool result.
// See §10.5.
func (t *AgentTool) Run(ctx context.Context, call ToolCall) (ToolResult, error) {
	return t.dispatcher.Dispatch(ctx, t, call)
}

// AgentToolInvocation is a single audit record for one agent-tool dispatch.
type AgentToolInvocation struct {
	ID           string
	ToolName     string
	ParentRunID  string
	ChildRunID   *string
	InvokedAt    string
	DurationMS   *int64
	InputTokens  *int
	OutputTokens *int
	Status       InvocationStatus
	ErrorMessage *string
}

// DispatchResult is returned from Dispatcher.Dispatch.
type DispatchResult struct {
	ToolResponse string
	Status       InvocationStatus
	ChildRunID   *string
	DurationMS   int64
	OutputTokens *int
	ErrorMessage *string
}

func utcNow() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }
```

### 10.4 Registry (`internal/tool/agent_registry.go`)

CRUD is performed through the standard `database/sql` API against the `modernc.org/sqlite` driver, using parameterized queries and `context.Context` on every call.

```go
package tool

import (
	"context"
	"database/sql"
)

// Registry provides CRUD over the agent_tools / agent_tool_invocations tables.
type Registry struct {
	db *sql.DB
}

func NewRegistry(db *sql.DB) *Registry { return &Registry{db: db} }

func (r *Registry) Register(ctx context.Context, t *AgentTool) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO agent_tools
		  (id, tool_name, profile, description, input_schema_json,
		   timeout_seconds, max_output_tokens, sandbox_override,
		   invocation_count, last_used_at, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,0,NULL,?,?)`,
		t.ID, t.ToolName, t.Profile, t.Description, string(t.InputSchema),
		t.TimeoutSeconds, t.MaxOutputTokens, t.SandboxOverride,
		t.CreatedAt, t.UpdatedAt,
	)
	return err // duplicate tool_name surfaces as a UNIQUE constraint error
}

func (r *Registry) Get(ctx context.Context, toolName string) (*AgentTool, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT id, tool_name, profile, description, input_schema_json,
		        timeout_seconds, max_output_tokens, sandbox_override,
		        invocation_count, last_used_at, created_at, updated_at
		 FROM agent_tools WHERE tool_name = ?`, toolName)
	t, err := scanAgentTool(row)
	if err == sql.ErrNoRows {
		return nil, nil // not found is not an error for the caller
	}
	return t, err
}

func (r *Registry) ListAll(ctx context.Context, profile string) ([]*AgentTool, error) {
	q := `SELECT id, tool_name, profile, description, input_schema_json,
	             timeout_seconds, max_output_tokens, sandbox_override,
	             invocation_count, last_used_at, created_at, updated_at
	      FROM agent_tools`
	var (
		rows *sql.Rows
		err  error
	)
	if profile != "" {
		rows, err = r.db.QueryContext(ctx, q+" WHERE profile = ? ORDER BY created_at", profile)
	} else {
		rows, err = r.db.QueryContext(ctx, q+" ORDER BY created_at")
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AgentTool
	for rows.Next() {
		t, err := scanAgentTool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Delete removes the tool and its invocation history in a single transaction.
// Reports whether a row was deleted.
func (r *Registry) Delete(ctx context.Context, toolName string) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "DELETE FROM agent_tools WHERE tool_name = ?", toolName)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM agent_tool_invocations WHERE tool_name = ?", toolName); err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, tx.Commit()
}

func (r *Registry) RecordInvocation(ctx context.Context, inv AgentToolInvocation) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agent_tool_invocations
		  (id, tool_name, parent_run_id, child_run_id, invoked_at,
		   duration_ms, input_tokens, output_tokens, status, error_message)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		inv.ID, inv.ToolName, inv.ParentRunID, inv.ChildRunID, inv.InvokedAt,
		inv.DurationMS, inv.InputTokens, inv.OutputTokens, string(inv.Status), inv.ErrorMessage,
	); err != nil {
		return err
	}
	now := utcNow()
	if _, err := tx.ExecContext(ctx, `
		UPDATE agent_tools
		SET invocation_count = invocation_count + 1, last_used_at = ?, updated_at = ?
		WHERE tool_name = ?`, now, now, inv.ToolName); err != nil {
		return err
	}
	return tx.Commit()
}

// scanAgentTool accepts anything with a Scan method (*sql.Row or *sql.Rows).
func scanAgentTool(s interface{ Scan(...any) error }) (*AgentTool, error) {
	var (
		t         AgentTool
		schemaTxt string
	)
	if err := s.Scan(
		&t.ID, &t.ToolName, &t.Profile, &t.Description, &schemaTxt,
		&t.TimeoutSeconds, &t.MaxOutputTokens, &t.SandboxOverride,
		&t.InvocationCount, &t.LastUsedAt, &t.CreatedAt, &t.UpdatedAt,
	); err != nil {
		return nil, err
	}
	t.InputSchema = json.RawMessage(schemaTxt)
	return &t, nil
}
```

### 10.5 Dispatcher (`internal/tool/agent_dispatch.go`)

The `Dispatcher` holds the per-run invocation context and spawns child agent loops. Because `AgentTool.Run(ctx, call)` is the entry point, there is no separate "interception" step in the agent loop: the loop simply calls `Run` on whichever tool matched, and the dispatcher does depth/budget checks before spinning a child. Timeouts and budget failures are modelled as sentinel errors returned by `runChildAgent` and mapped to a tool response — the child goroutine is bounded by a `context.WithTimeout`.

```go
package tool

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"
)

var (
	errTimeout = errors.New("timeout")
	errBudget  = errors.New("budget exceeded")
)

// ChildRunner spawns a headless child agent loop. It is an interface so it can
// be faked in tests without a live LLM. The concrete implementation lives in
// internal/runtime and starts a child agent via errgroup (see §10.6).
type ChildRunner interface {
	Run(ctx context.Context, spec ChildRunSpec) (ChildRunResult, error)
}

type ChildRunSpec struct {
	Profile                  string
	Prompt                   string
	ChildRunID               string
	ParentRunID              string
	AgentToolDepth           int
	MaxOutputTokens          int
	SandboxOverride          *string
	ParentRemainingBudgetUSD *float64
}

type ChildRunResult struct {
	Output       string
	InputTokens  int
	OutputTokens int
}

// Dispatcher carries the per-run context needed to spawn children.
type Dispatcher struct {
	registry              *Registry
	runner                ChildRunner
	parentRunID           string
	currentDepth          int
	maxDepth              int
	parentRemainingBudget *float64
}

func NewDispatcher(reg *Registry, runner ChildRunner, parentRunID string, depth, maxDepth int, budget *float64) *Dispatcher {
	return &Dispatcher{
		registry: reg, runner: runner, parentRunID: parentRunID,
		currentDepth: depth, maxDepth: maxDepth, parentRemainingBudget: budget,
	}
}

// Dispatch runs the child agent synchronously (from the orchestrator's
// perspective) and returns a ToolResult. It never returns a non-nil error for
// child-run failures: those are converted to a tool-response string so the
// orchestrator loop keeps running (NFR-02).
func (d *Dispatcher) Dispatch(ctx context.Context, at *AgentTool, call ToolCall) (ToolResult, error) {
	start := time.Now()
	invID := NewUUID()
	invokedAt := utcNow()

	// Depth check.
	if d.currentDepth >= d.maxDepth {
		msg := fmt.Sprintf("Error: maximum agent tool depth (%d) reached. Cannot invoke '%s'.", d.maxDepth, at.ToolName)
		_ = d.registry.RecordInvocation(ctx, AgentToolInvocation{
			ID: invID, ToolName: at.ToolName, ParentRunID: d.parentRunID,
			InvokedAt: invokedAt, Status: StatusDepthLimit, ErrorMessage: &msg,
		})
		return ToolResult{Content: msg, IsError: true}, nil
	}

	prompt := extractPrompt(call.Input)
	childRunID := "run-" + NewShortID()

	spec := ChildRunSpec{
		Profile: at.Profile, Prompt: prompt, ChildRunID: childRunID,
		ParentRunID: d.parentRunID, AgentToolDepth: d.currentDepth + 1,
		MaxOutputTokens: at.MaxOutputTokens, SandboxOverride: at.SandboxOverride,
		ParentRemainingBudgetUSD: d.parentRemainingBudget,
	}

	// Bound the child run by the registered timeout via context.
	cctx, cancel := context.WithTimeout(ctx, time.Duration(at.TimeoutSeconds)*time.Second)
	defer cancel()

	var res ChildRunResult
	g, gctx := errgroup.WithContext(cctx)
	g.Go(func() error {
		r, err := d.runner.Run(gctx, spec)
		res = r
		return err
	})
	err := g.Wait()
	durMS := time.Since(start).Milliseconds()

	switch {
	case errors.Is(err, errTimeout) || errors.Is(err, context.DeadlineExceeded):
		msg := fmt.Sprintf("Error: agent tool '%s' timed out after %d seconds.", at.ToolName, at.TimeoutSeconds)
		_ = d.registry.RecordInvocation(ctx, AgentToolInvocation{
			ID: invID, ToolName: at.ToolName, ParentRunID: d.parentRunID,
			ChildRunID: &childRunID, InvokedAt: invokedAt, DurationMS: &durMS,
			Status: StatusTimeout, ErrorMessage: &msg,
		})
		return ToolResult{Content: msg, IsError: true}, nil

	case errors.Is(err, errBudget):
		msg := fmt.Sprintf("Error: agent tool '%s' cannot run: budget exceeded (%v).", at.ToolName, err)
		_ = d.registry.RecordInvocation(ctx, AgentToolInvocation{
			ID: invID, ToolName: at.ToolName, ParentRunID: d.parentRunID,
			InvokedAt: invokedAt, DurationMS: &durMS,
			Status: StatusBudgetExceeded, ErrorMessage: &msg,
		})
		return ToolResult{Content: msg, IsError: true}, nil

	case err != nil:
		msg := fmt.Sprintf("Error: agent tool '%s' failed: %v.", at.ToolName, err)
		_ = d.registry.RecordInvocation(ctx, AgentToolInvocation{
			ID: invID, ToolName: at.ToolName, ParentRunID: d.parentRunID,
			ChildRunID: &childRunID, InvokedAt: invokedAt, DurationMS: &durMS,
			Status: StatusError, ErrorMessage: &msg,
		})
		return ToolResult{Content: msg, IsError: true}, nil
	}

	_ = d.registry.RecordInvocation(ctx, AgentToolInvocation{
		ID: invID, ToolName: at.ToolName, ParentRunID: d.parentRunID,
		ChildRunID: &childRunID, InvokedAt: invokedAt, DurationMS: &durMS,
		InputTokens: &res.InputTokens, OutputTokens: &res.OutputTokens,
		Status: StatusSuccess,
	})
	return ToolResult{Content: res.Output}, nil
}

// extractPrompt pulls a prompt string from tool call arguments: it prefers the
// "prompt" key (default schema), then "task", "query", "input", and finally
// falls back to the raw JSON of the whole input object.
func extractPrompt(input json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		return string(input)
	}
	for _, key := range []string{"prompt", "task", "query", "input"} {
		if v, ok := m[key].(string); ok {
			return v
		}
	}
	return string(input)
}
```

### 10.6 Integration Point in `internal/runtime`

Because agent tools implement the same `tool.Tool` interface as builtin and MCP tools, integration is a matter of *registering* them into the agent's `tool.Set` before the loop starts — there is no bespoke interception branch inside the agent loop. The loop resolves a tool call by name against the set and calls `Run`; for an `*AgentTool`, `Run` spins the child. Tracing is handled uniformly for every tool via an OTel-instrumented wrapper (PRD-013), so no agent-tool-specific span emission is needed in the loop.

```go
// In internal/runtime, inside Submit(ctx, opts) before constructing the agent loop:

if opts.EnableAgentTools {
	reg := tool.NewRegistry(db)
	agentTools, err := reg.ListAll(ctx, "") // "" = no profile filter
	if err != nil {
		return err
	}

	// Apply --agent-tools allowlist filter if specified.
	if len(opts.AgentToolAllowlist) > 0 {
		allowed := make(map[string]bool, len(opts.AgentToolAllowlist))
		for _, n := range opts.AgentToolAllowlist {
			allowed[n] = true
		}
		agentTools = slices.DeleteFunc(agentTools, func(t *tool.AgentTool) bool {
			return !allowed[t.ToolName]
		})
	}

	// runner is the internal/runtime implementation of tool.ChildRunner; it
	// starts a headless child agent loop via errgroup (see OQ-02).
	dispatcher := tool.NewDispatcher(
		reg, runner,
		runID,               // parent_run_id
		opts.AgentToolDepth, // current depth (0 for top-level, set by child specs)
		opts.MaxAgentToolDepth,
		budget.Remaining(ctx, db, runID),
	)

	// Register each agent tool into the same tool.Set as builtin + MCP tools.
	for _, at := range agentTools {
		at.BindDispatcher(dispatcher) // wires *Dispatcher into AgentTool.Run
		toolSet.Register(at)          // identical call used for builtin/MCP tools
	}
}

// The agent loop dispatches uniformly — no special-casing:
//
//   t, ok := toolSet.Lookup(call.Name)
//   if ok {
//       res, _ := t.Run(ctx, call) // *AgentTool.Run spins the child agent loop
//       msgs = append(msgs, tool.ResultMessage(call.ID, res))
//   }
```

The child agent loop is started by the `ChildRunner` implementation. It calls back into the same `runtime.Submit` code path in headless mode (`opts.TTY = false`, structured output captured to a buffer), avoiding the TUI/streaming-to-terminal concerns raised in OQ-02 by sharing the non-interactive submit core rather than the CLI entry point.

### 10.7 Validation Helpers (`internal/tool/schema.go`)

The default input schema is generated from a Go struct via `invopop/jsonschema`, and custom schemas are validated as JSON-Schema objects.

```go
package tool

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/invopop/jsonschema"
)

// defaultInput is the Go struct from which the default tool input schema is
// reflected by invopop/jsonschema.
type defaultInput struct {
	Prompt string `json:"prompt" jsonschema:"description=The task or question for the agent,required"`
}

// DefaultInputSchema returns the reflected JSON Schema for the default tool input.
func DefaultInputSchema() json.RawMessage {
	r := &jsonschema.Reflector{DoNotReference: true}
	b, _ := json.Marshal(r.Reflect(&defaultInput{}))
	return b
}

// ValidateToolName returns an error if the name is not a valid snake_case tool name.
func ValidateToolName(name string) error {
	if !toolNameRE.MatchString(name) {
		return fmt.Errorf(
			"invalid tool name %q: must match ^[a-z][a-z0-9_]{0,63}$ "+
				"(lowercase, start with letter, underscores only, max 64 chars)", name)
	}
	return nil
}

// ValidateInputSchema parses schemaJSON and confirms it is a JSON object with
// "type": "object". It returns the canonicalized schema bytes.
func ValidateInputSchema(schemaJSON string) (json.RawMessage, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(schemaJSON), &m); err != nil {
		// json.Unmarshal into a map fails for arrays/primitives and bad JSON alike.
		var syn *json.SyntaxError
		if errors.As(err, &syn) {
			return nil, fmt.Errorf("invalid JSON in --input-schema: %w", err)
		}
		return nil, errors.New("--input-schema must be a JSON object, not a list or primitive")
	}
	if t, _ := m["type"].(string); t != "object" {
		return nil, errors.New(`--input-schema root must have "type": "object"`)
	}
	return json.RawMessage(schemaJSON), nil
}
```

### 10.8 Output Truncation

Truncation uses `tiktoken-go` for accurate token boundaries (the same tokenizer used for cost estimation, per FR-13), decoding back to a valid string prefix.

```go
package tool

import (
	"fmt"

	"github.com/pkoukk/tiktoken-go"
)

// TruncateOutput trims text to at most maxTokens tokens using the tiktoken
// encoding, appending a truncation marker when it cuts.
func TruncateOutput(text string, maxTokens int) string {
	enc, err := tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		// Fallback: 4-chars-per-token heuristic if the encoder is unavailable.
		if maxChars := maxTokens * 4; len(text) > maxChars {
			return text[:maxChars] + fmt.Sprintf("\n\n[Output truncated at %d tokens]", maxTokens)
		}
		return text
	}
	toks := enc.Encode(text, nil, nil)
	if len(toks) <= maxTokens {
		return text
	}
	return enc.Decode(toks[:maxTokens]) + fmt.Sprintf("\n\n[Output truncated at %d tokens]", maxTokens)
}
```

---

## 11. Security Considerations

1. **Tool name injection:** The `tool_name` value is stored in SQLite and later emitted as a JSON key in the tool definitions list. It is validated against `^[a-z][a-z0-9_]{0,63}$` at registration time, making JSON injection and prompt injection via tool names structurally impossible. The stored value is never interpolated into shell commands.

2. **Prompt injection via tool input:** The orchestrator LLM constructs the `prompt` field that is passed to the child agent. A malicious prompt in the user's original task could instruct the orchestrator to pass adversarial content to the child agent (e.g., "tell the coder agent to exfiltrate ~/.ssh/id_rsa"). This is mitigated by: (a) child runs execute under the child profile's tool allowlist, not the orchestrator's; (b) sandbox enforcement (FR-18) limits filesystem and network access for child runs; (c) the child agent cannot write back to the parent run's context — it can only return a string. A future hardening step (not in this PRD) is to pass the tool input through the security scanner (PRD-034) before dispatching.

3. **Privilege escalation via nested agent tools:** An orchestrator with broad permissions should not automatically grant those permissions to a child agent. Enforced by FR-19 (no context inheritance) and FR-18 (sandbox policy comes from the child profile registration, not the parent). A `--no-sandbox` override at registration time requires explicit opt-in.

4. **Budget exhaustion attack:** A malicious prompt that causes the orchestrator to invoke many agent tools in rapid succession could exhaust the API budget. Mitigated by: (a) FR-12 (remaining budget is passed to each child run); (b) the depth limit (FR-11) caps the invocation tree depth; (c) TAG's existing budget enforcement in the `internal/runtime` budget package applies to child runs.

5. **Recursive loop detection:** Two profiles that each register the other as an agent tool create a potential infinite call cycle. The depth limit (FR-11, default 3) provides a hard stop. The error message emitted at depth limit explicitly names the tool that triggered it, aiding debugging.

6. **Profile enumeration:** `tag agent tool list` reveals which profiles exist and their descriptions. This is local CLI data, not a networked API, so the threat model is the local user. No authentication is required beyond OS-level file permissions on `~/.tag/`.

7. **Child run data isolation:** The child agent run receives only the `prompt` string extracted from the tool call. It does not receive: the parent's conversation history, the parent's environment variables, or the parent's MCP server connections. This is enforced by constructing a fresh `ChildRunSpec` and starting the child through the shared non-interactive `runtime.Submit` core in a new goroutine — the parent's in-memory agent state and `context.Context` values are not shared beyond cancellation.

8. **SQL injection:** All SQLite operations go through `database/sql` with parameterized queries (`?` placeholders) and never string-interpolate values into SQL. Transactions (`BeginTx`) are used for multi-statement mutations (delete, invocation recording).

---

## 12. Testing Strategy

### 12.1 Unit Tests (`internal/tool/agent_tool_test.go`)

Standard-library `testing` with `testify/assert` for concise assertions. Table-driven tests where the cases share shape.

- `TestValidateToolName_Valid`: Assert `ValidateToolName("write_code")` returns `nil`.
- `TestValidateToolName_InvalidHyphen`: Assert `ValidateToolName("write-code")` returns a non-nil error.
- `TestValidateToolName_StartsWithDigit`: Assert `ValidateToolName("2write")` returns error.
- `TestValidateToolName_TooLong`: Assert 65-char name returns error.
- `TestValidateInputSchema_Valid`: Assert a valid JSON-Schema object parses without error.
- `TestValidateInputSchema_InvalidJSON`: Assert non-JSON string returns error.
- `TestValidateInputSchema_NonObject`: Assert `{"type": "array"}` returns error.
- `TestAgentTool_Info`: Assert `(*AgentTool).Info()` returns a `ToolInfo` with `Name`, `Description`, `InputSchema`.
- `TestAgentTool_DescriptionCapped`: Assert description > 200 chars is capped at 200 in `Info()`.
- `TestTruncateOutput_UnderLimit`: Assert short text passes through unchanged.
- `TestTruncateOutput_OverLimit`: Assert long text is truncated and the marker suffix appended.
- `TestExtractPrompt_PromptKey`: Assert `extractPrompt([]byte(\`{"prompt":"hello"}\`))` returns `"hello"`.
- `TestExtractPrompt_TaskKeyFallback`: Assert `extractPrompt([]byte(\`{"task":"do X"}\`))` returns `"do X"`.
- `TestExtractPrompt_JSONFallback`: Assert `extractPrompt([]byte(\`{"foo":"bar"}\`))` returns the raw JSON string.

### 12.2 Registry Unit Tests (with in-memory SQLite)

Tests open a `modernc.org/sqlite` in-memory database (`file::memory:?cache=shared`) and apply the embedded DDL. No CGO required.

```go
package tool

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), agentToolsDDL) // embedded migration SQL
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry(makeTestDB(t))
	tool := &AgentTool{
		ID: NewID(), ToolName: "write_code", Profile: "coder",
		Description: "Write code", InputSchema: DefaultInputSchema(),
		CreatedAt: utcNow(), UpdatedAt: utcNow(),
	}
	require.NoError(t, reg.Register(ctx, tool))

	fetched, err := reg.Get(ctx, "write_code")
	require.NoError(t, err)
	require.NotNil(t, fetched)
	assert.Equal(t, "write_code", fetched.ToolName)
	assert.Equal(t, "coder", fetched.Profile)
}

func TestRegistry_DuplicateNameErrors(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry(makeTestDB(t))
	mk := func() *AgentTool {
		return &AgentTool{ID: NewID(), ToolName: "t1", Profile: "p",
			Description: "d", InputSchema: DefaultInputSchema(),
			CreatedAt: utcNow(), UpdatedAt: utcNow()}
	}
	require.NoError(t, reg.Register(ctx, mk()))
	// UNIQUE(tool_name) constraint violation surfaces as an error.
	assert.Error(t, reg.Register(ctx, mk()))
}

func TestRegistry_DeleteReturnsTrue(t *testing.T)          { /* ... */ }
func TestRegistry_DeleteNonexistentReturnsFalse(t *testing.T) { /* ... */ }
func TestRegistry_ListFilterByProfile(t *testing.T)        { /* ... */ }
```

### 12.3 Dispatcher Unit Tests (faked child run)

`ChildRunner` is an interface, so tests inject a fake implementation — no live LLM needed.

```go
type fakeRunner struct {
	res ChildRunResult
	err error
}

func (f fakeRunner) Run(ctx context.Context, spec ChildRunSpec) (ChildRunResult, error) {
	return f.res, f.err
}

func TestDispatcher_DepthLimit(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry(makeTestDB(t))
	at := &AgentTool{ID: NewID(), ToolName: "write_code", Profile: "coder",
		Description: "d", InputSchema: DefaultInputSchema(),
		CreatedAt: utcNow(), UpdatedAt: utcNow()}
	require.NoError(t, reg.Register(ctx, at))

	d := NewDispatcher(reg, fakeRunner{}, "run-abc", 3 /*depth*/, 3 /*max*/, ptr(1.0))
	res, err := d.Dispatch(ctx, at, ToolCall{Input: []byte(`{"prompt":"test"}`)})
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, "maximum agent tool depth")
}

func TestDispatcher_SuccessRecordsInvocation(t *testing.T) {
	ctx := context.Background()
	db := makeTestDB(t)
	reg := NewRegistry(db)
	at := &AgentTool{ID: NewID(), ToolName: "write_code", Profile: "coder",
		Description: "d", InputSchema: DefaultInputSchema(), MaxOutputTokens: 4096,
		TimeoutSeconds: 120, CreatedAt: utcNow(), UpdatedAt: utcNow()}
	require.NoError(t, reg.Register(ctx, at))

	runner := fakeRunner{res: ChildRunResult{Output: "func fib(n int) int {...}", InputTokens: 50, OutputTokens: 80}}
	d := NewDispatcher(reg, runner, "run-abc", 0, 3, ptr(1.0))

	res, err := d.Dispatch(ctx, at, ToolCall{Input: []byte(`{"prompt":"write fibonacci"}`)})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content, "func fib")

	var status, name string
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT status, tool_name FROM agent_tool_invocations").Scan(&status, &name))
	assert.Equal(t, "success", status)
	assert.Equal(t, "write_code", name)
}
```

### 12.4 Integration Tests

Integration tests build the binary once (`go test` with a `TestMain` that invokes `go build`, or `os/exec` against a compiled test binary) and run against a temp-dir `TAG_HOME` with its own SQLite file. A fake `internal/llm` provider (scripted `Stream` returning canned tool-call events) drives the orchestrator without a network call.

- `TestCLI_RegisterThenList`: Run `tag agent tool register --profile coder --as-tool write_code` on a test DB, then `tag agent tool list --json`, assert JSON contains the registered tool.
- `TestCLI_RegisterDryRunNoWrite`: Run with `--dry-run`, assert SQLite row count is 0 after.
- `TestCLI_RegisterInvalidProfile`: Register with `--profile nonexistent`, assert exit code 1.
- `TestCLI_RegisterDuplicateWithoutForce`: Register same name twice, assert exit code 1 on second.
- `TestCLI_UnregisterExisting`: Register then unregister, assert list is empty.
- `TestCLI_UnregisterNonexistent`: Unregister a name that was never registered, assert exit code 1.
- `TestSubmit_EnableAgentToolsRegistersTools`: With a recording fake provider, assert the tool set passed to the first `Stream` call includes the registered tool definitions.
- `TestSubmit_ChildRunHasParentRunID`: Run a submit that triggers an agent-tool call (fake provider returns a tool call), assert the child run's `parent_run_id` is set in SQLite.

### 12.5 Performance Tests / Benchmarks

Written as Go benchmarks (`func BenchmarkXxx(b *testing.B)`).

- **Dispatch lookup overhead:** `BenchmarkToolSetLookup` — resolve a tool by name against a set of 50 registered tools. Assert allocation-light, sub-microsecond map lookup (the tool set is an in-memory map, not a per-call SQLite query).
- **Tool definition injection size:** With 20 registered tools, measure the added token count in the orchestrator's system prompt (via `tiktoken-go`). Assert total tool injection is < 6,000 tokens (300 tokens × 20 tools).
- **Concurrent child runs:** Dispatch 5 child runs simultaneously via `errgroup` goroutines; assert all complete without SQLite `SQLITE_BUSY` errors, confirming WAL mode plus the single-writer pool absorbs the concurrency. Run under `go test -race` to catch data races.

---

## 13. Acceptance Criteria

| ID | Criterion | How to Test |
|----|-----------|-------------|
| AC-01 | `tag agent tool register --profile coder --as-tool write_code` exits 0 and writes a row to `agent_tools` with `tool_name='write_code'` and `profile='coder'`. | `sqlite3 ~/.tag/runtime/tag.sqlite3 "SELECT tool_name, profile FROM agent_tools WHERE tool_name='write_code'"` returns one row. |
| AC-02 | `tag agent tool register --profile nonexistent --as-tool foo` exits 1 with a message containing "profile" and "nonexistent". | Subprocess return code check + stderr pattern match. |
| AC-03 | `tag agent tool register --as-tool "bad-name"` exits 1 with a message mentioning the invalid name and expected pattern. | Subprocess return code check + stderr pattern match. |
| AC-04 | `tag agent tool list --json` emits valid JSON array; each element has `id`, `tool_name`, `profile`, `description`, `input_schema`, `timeout_seconds`, `max_output_tokens`, `created_at` fields. | `tag agent tool list --json | jq -e '.'` exits 0 (or a Go test that `json.Unmarshal`s stdout into `[]AgentTool`). |
| AC-05 | `tag agent tool unregister write_code --yes` after registration exits 0 and removes the row from both `agent_tools` and `agent_tool_invocations`. | `sqlite3 ... "SELECT COUNT(*) FROM agent_tools WHERE tool_name='write_code'"` returns 0. |
| AC-06 | `tag agent tool unregister ghost_tool --yes` exits 1 with an error message that mentions `ghost_tool`. | Subprocess return code + stderr check. |
| AC-07 | After `tag submit --enable-agent-tools --profile orchestrator --prompt "..."` where the orchestrator LLM emits a tool call for a registered agent tool, a child run row exists in `runs` with `parent_run_id` equal to the orchestrator run's ID. | SQLite assertion in integration test. |
| AC-08 | Child run's `agent_tool_depth` column equals `parent.agent_tool_depth + 1`. | SQLite assertion in integration test. |
| AC-09 | When orchestrator triggers an agent tool call at depth == `max_agent_tool_depth`, the tool response string begins with "Error: maximum agent tool depth". No child run is spawned. | Integration test: verify `agent_tool_invocations.status == 'depth_limit'` and `child_run_id IS NULL`. |
| AC-10 | Agent tool invocation is recorded in `agent_tool_invocations` with correct `tool_name`, `parent_run_id`, `child_run_id`, non-null `duration_ms`, and `status='success'` on a successful dispatch. | SQLite assertion after integration test run. |
| AC-11 | `agent_tools.invocation_count` increments by 1 after each successful dispatch. `last_used_at` is updated to within 5 seconds of the invocation time. | SQLite assertion after integration test. |
| AC-12 | `tag submit --enable-agent-tools` without any registered tools emits a warning on stderr: "No agent tools registered. Run 'tag agent tool register' first." and proceeds with a normal agent run. | Subprocess stderr capture check. |
| AC-13 | `tag agent tool register --dry-run --profile coder --as-tool foo` exits 0, prints a registration preview, and writes zero rows to `agent_tools`. | SQLite row count assertion + subprocess exit code check. |
| AC-14 | Child run output exceeding `max_output_tokens` is truncated. The tool response string ends with `[Output truncated at N tokens]`. | Integration test: register tool with `--max-output-tokens 10`; trigger dispatch with a long-output mock agent. |
| AC-15 | `tag submit --agent-tools write_code --enable-agent-tools` with `research_topic` also registered injects only `write_code` into the tool definitions; `research_topic` is not present. | Mock LLM capture of tool definition list in integration test. |

---

## 14. Dependencies

| Dependency | Type | Version / Notes |
|------------|------|-----------------|
| PRD-013 Agent Tracing & Observability | Internal PRD | Required for `tag.child_run_id` and `tag.parent_run_id` OTEL span attributes; span linking between parent and child traces |
| PRD-028 Sandbox Code Execution | Internal PRD | Required for `sandbox_override` enforcement in child runs; the sandbox policy on child `runtime.Submit` invocations must be wired (via `internal/sandbox`) |
| PRD-034 Security / Secret Scanning | Internal PRD | Recommended as a hardening dependency — future work to scan tool inputs before child dispatch |
| PRD-027 Eval Framework | Internal PRD | Integration: `tag eval run` can use `--enable-agent-tools` to test orchestrator profiles against multi-agent eval suites |
| PRD-014 MCP Server Registry | Internal PRD | The tool interception logic runs before MCP dispatch; must hook into the same dispatch point where MCP tools are resolved |
| PRD-077 Scope-Based Tool Filtering | Internal PRD | Child runs inherit the scope filters of their registered profile; filter application in the child submit path reuses the existing `internal/tool` retrieval logic |
| `internal/runtime` (budget) | Internal package | `budget.Remaining(ctx, db, runID)` is called before each child dispatch; budget decrement after child run must propagate to parent's budget tracking |
| `internal/obs` (tracing) | Internal package | The OTel-instrumented tool wrapper emits the agent-tool span; uses the existing `go.opentelemetry.io/otel` tracer provider |
| `modernc.org/sqlite` | Module (existing) | Pure-Go driver via `database/sql`; WAL mode, parameterized queries, `Migrate(ctx)` pattern; `CGO_ENABLED=0` |
| `golang.org/x/sync/errgroup` | Module (existing) | Bounded goroutine + `Wait()` for synchronous blocking on child run completion |
| `encoding/json` (stdlib) | Runtime | Input schema serialization, tool definition emission |
| `regexp` (stdlib) | Runtime | Tool name validation regex |
| `crypto/rand` + `encoding/hex` (stdlib) | Runtime | `NewID()`, invocation IDs |
| `github.com/invopop/jsonschema` | Module (existing) | Default input schema reflection from Go structs |
| `github.com/pkoukk/tiktoken-go` | Module (existing) | Token-accurate output truncation (FR-13) |
| `github.com/stretchr/testify` | Module (existing, test) | Assertions in `_test.go` files |

---

## 15. Open Questions

| ID | Question | Owner | Target Resolution |
|----|----------|-------|-------------------|
| OQ-01 | Should agent tools be composable with MCP tool calls in the same orchestrator turn? (i.e., can the orchestrator call `write_code` and `github:create_pr` in the same multi-tool call batch?) Current design says yes — agent tools are injected into the same tool list. Risk: the LLM may try to call them in parallel. In that case, do we dispatch child runs in parallel or sequentially? | Engineering | Before implementation start — decide whether to add `parallel_dispatch: bool` to `AgentTool` |
| OQ-02 | The `ChildRunner` implementation shares significant logic with the top-level submit flow. Should it call `runtime.Submit` directly (with `opts.TTY=false`), or should the interactive CLI/TUI concerns be factored out into a headless `runtime.RunAgent(ctx, spec)` core that both the CLI and the child runner call? The latter avoids pulling `internal/tui` and terminal streaming into a non-interactive goroutine. | Engineering | Architecture review before implementation |
| OQ-03 | Should the orchestrator receive streaming output from the child run as it is generated (enabling progress tokens or intermediate reasoning), or is the current "return final output as a string" sufficient for v1? Streaming would require significant protocol changes (SSE passthrough or WebSocket bridge). | Product | Deferred to v2 unless user research shows blocking is a significant pain point |
| OQ-04 | How should agent tool descriptions handle multi-language prompts? The `--description` is authored in the registration language (presumably English). If the orchestrator operates in another language, the tool selection may degrade. Should descriptions support locale variants? | Product | Deferred — internationalization is a separate workstream |
| OQ-05 | Is `max_output_tokens: 4096` the right default? Researcher profiles often produce long-form output (references, multi-paragraph summaries). At 4096 tokens (~16KB), a detailed research summary fits. But a coder profile that generates a large file may truncate mid-function. Should `max_output_tokens` default differ per profile type? | Engineering | Gather feedback from early adopters in first sprint; adjust default before GA |
| OQ-06 | Should `tag agent tool register --force` overwrite-in-place (UPDATE) or delete-and-insert? UPDATE preserves `invocation_count` and `created_at`; delete-and-insert resets them. The UX implication is whether updating a tool's profile or description is treated as a configuration edit (preserve history) or a fresh registration (reset). | Engineering | Decide before implementing `--force`; current lean is UPDATE to preserve history |
| OQ-07 | Is 3 the right default for `max_agent_tool_depth`? A depth of 3 supports: orchestrator (depth 0) → specialist A (depth 1) → sub-specialist B (depth 2) → leaf tool call (depth 3, blocked). In practice, are there legitimate use cases for depth 4+? | Engineering/Product | Gather data from multi-agent pilot users during beta |
| OQ-08 | Should `tag agent tool list` show the full `input_schema` JSON inline, or should it be summarized? For wide TTYs it fits; for narrow terminals it will wrap badly. Rich's `Table` supports truncated columns — should `input_schema` be hidden by default with a `--verbose` flag to show it? | Engineering | Low priority — resolve during implementation based on terminal width heuristics |

---

## 16. Complexity and Timeline

**Overall Effort:** M (1-2 weeks for a single engineer)

### Phase 1 — SQLite DDL and Core Types (Day 1-2)

- Add `agent_tools` and `agent_tool_invocations` DDL as an embedded migration under `internal/memory/migrations/`, applied by `Migrate(ctx)`.
- Add `ALTER TABLE runs ADD COLUMN parent_run_id` and `agent_tool_depth` with idempotent (duplicate-column-tolerant) guards.
- Implement `AgentTool` (with `Info`/`Run`/`ProviderOptions`), `AgentToolInvocation`, `DispatchResult` types in `internal/tool/agent_tool.go`.
- Implement `Registry` (Register, Get, ListAll, Delete, RecordInvocation) over `database/sql`.
- Implement `ValidateToolName()`, `ValidateInputSchema()`, `TruncateOutput()`, `extractPrompt()`.
- Write unit tests for all pure functions and registry CRUD against in-memory `modernc.org/sqlite`.

**Deliverable:** `internal/tool` agent-tool types with full registry and validation, tested in isolation. `Migrate(ctx)` creates new tables without breaking existing tests.

### Phase 2 — CLI Commands (Day 3-4)

- Add `tag agent tool register/list/unregister/show` handlers in `internal/cli` (chi/cobra-style command tree consistent with the rest of the CLI).
- Wire the `tag agent tool` subcommand group into the CLI router.
- Implement `--dry-run`, `--json`, `--force` flags on register.
- Write CLI integration tests via `os/exec` against the compiled binary + a temp-dir `TAG_HOME` SQLite file.

**Deliverable:** All `tag agent tool` subcommands functional end-to-end. `tag agent tool register --dry-run` and `tag agent tool list --json` produce correct output.

### Phase 3 — Dispatcher and `internal/runtime` Integration (Day 5-8)

- Implement `Dispatcher` with depth checking, budget checking, and the `ChildRunner` interface.
- Implement the `internal/runtime` `ChildRunner` that starts a headless child agent loop via `errgroup` (no TTY dependencies).
- Add `--enable-agent-tools`, `--agent-tools`, and `--max-agent-tool-depth` flags to `tag submit`.
- Register agent tools into the shared `tool.Set` in `runtime.Submit`; dispatch is uniform (no special-casing in the agent loop).
- Ensure the OTel tool wrapper emits the agent-tool span (PRD-013).
- Write integration tests: fake `internal/llm` provider that emits agent-tool calls, assert child runs created with correct `parent_run_id`, assert depth-limit error, assert invocation audit record. Run with `-race`.

**Deliverable:** End-to-end multi-agent run works: orchestrator → agent-tool-call → child agent run → tool response returned → orchestrator continues.

### Phase 4 — Hardening and Eval Integration (Day 9-10)

- Add budget propagation: `budget.Remaining(ctx, db, runID)` hook, passed to the dispatcher.
- Add sandbox policy enforcement for child runs via `SandboxOverride` (PRD-028 / `internal/sandbox`).
- Add `--enable-agent-tools` support to `tag eval run` (pass-through to the spawned submit).
- Write benchmarks: tool-set lookup, tool definition injection token count.
- Update `docs/prd/INDEX.md` to include PRD-083.
- Final acceptance criteria verification run.

**Deliverable:** All 15 acceptance criteria pass. Performance targets met. Eval integration functional.

---

*End of PRD-083*

