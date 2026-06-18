# PRD-083: Agent-as-Tool Pattern: Invoke Specialist Agents as Function Tools (`tag agent tool`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (1-2 weeks)
**Category:** Multi-Agent Interoperability Protocols
**Affects:** `controller.py`
**Depends on:** PRD-013 (Agent Tracing & Observability), PRD-027 (Eval Framework), PRD-028 (Sandbox Code Execution), PRD-034 (Security / Secret Scanning), PRD-001 (Structured Memory Configuration), PRD-014 (MCP Server Registry), PRD-077 (Scope-Based Tool Filtering)
**Inspired by:** OpenAI Agents SDK agent-as-tool, LangGraph subgraph-as-node, AutoGen nested agents
**GitHub Issue:** #347

---

## 1. Overview

Modern multi-agent architectures increasingly rely on the ability to decompose complex tasks across specialized agents that each excel in a narrow domain — one agent focused on code generation, another on knowledge retrieval, another on data analysis. TAG already supports profile-based specialization and multi-agent orchestration via swarm and DAG constructs. What is missing is the ability for one running agent (the orchestrator) to invoke another TAG profile as a discrete, synchronous function call, receiving structured output back as a tool response within the same conversation turn. This pattern — agent-as-tool — closes that gap.

The agent-as-tool pattern is conceptually distinct from both handoff and spawning. A handoff transfers execution control to another agent; the original agent's conversation ends. A spawn creates a background task with no return value flowing back into the orchestrator's reasoning. Agent-as-tool is a synchronous invocation: the orchestrator calls `write_code(task="implement a binary search")`, the `coder` profile executes the task in a sandboxed subprocess, and the result is returned to the orchestrator as a tool response string — exactly as if any other MCP tool had been called. The orchestrator can then use that result as input to the next reasoning step, chain multiple specialist calls, or compare outputs from different specialist agents on the same sub-task.

This design is directly inspired by the OpenAI Agents SDK `agent.as_tool()` method, which wraps an agent as a `FunctionTool` accepting a string input and returning a string output. LangGraph's subgraph-as-node pattern achieves the same semantics in a graph-execution model by treating a compiled `StateGraph` as a node callable. AutoGen's nested agent pattern is similar, though AutoGen Swarm selects the next agent by scanning for `HandoffMessage.target` rather than explicit tool-call dispatch. TAG's approach follows the OpenAI Agents SDK model most closely: registered tool names are explicitly declared, schema is generated from a `description` and optional typed input schema, and execution is fully synchronous from the orchestrator's perspective.

The implementation is anchored in `controller.py` with a new `cmd_agent_tool` family of subcommands. Tool registrations are persisted to a new `agent_tools` table in `~/.tag/runtime/tag.sqlite3`. At submit time, when `--enable-agent-tools` is passed, `cmd_submit` queries the `agent_tools` table and injects synthetic tool definitions into the tool list before the first orchestrator LLM call. When the orchestrator emits a tool call matching a registered agent tool name, the TAG runtime spawns a child agent run using the referenced profile, captures its output, and returns it as a tool response. The orchestrator's context window sees only the tool name, input, and response — all the complexity of the child agent's execution is hidden behind the tool call boundary.

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

The TAG runtime intercepts the tool call before dispatching to MCP. It:
1. Looks up the `agent_tools` record by `tool_name`.
2. Checks current invocation depth against `max_agent_tool_depth`; if at limit, returns tool error string.
3. Checks remaining parent budget; if insufficient, returns tool error string.
4. Extracts the `prompt` field (or full JSON input if custom schema) from the tool call arguments.
5. Spawns a child run: `cmd_submit`-equivalent with `--profile <registered-profile>`, `--prompt <extracted-prompt>`, `parent_run_id=<current-run-id>`, `agent_tool_depth=<current-depth+1>`.
6. Awaits child run completion (synchronous, blocking).
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
| FR-05 | The `agent_tools` table MUST be created by `open_db()` within the existing `executescript` block, following the established schema migration pattern. |
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
| FR-19 | A child agent run MUST NOT have access to the parent's conversation history. The child receives only the `prompt` extracted from the tool call arguments. |
| FR-20 | `tag agent tool list --json` MUST emit a valid JSON array to stdout. Each element MUST include: `id`, `tool_name`, `profile`, `description`, `input_schema`, `timeout_seconds`, `max_output_tokens`, `sandbox`, `created_at`, `updated_at`, `invocation_count`, `last_used_at`. |

---

## 9. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **Synchronous blocking latency:** The orchestrator's LLM streaming output MUST be paused (no new tokens requested) while awaiting the child run. The wait MUST be implemented using `threading.Event` or a `Future`, not a polling sleep loop, to avoid wasted CPU. |
| NFR-02 | **Child run isolation:** A child agent run failure (exception, OOM, crash) MUST NOT crash the orchestrator run. All child run errors are caught, converted to a tool response error string, and the orchestrator continues. |
| NFR-03 | **SQLite WAL concurrency:** The parent run and child run may concurrently write to SQLite (parent writes steps; child writes its own steps). Both use the existing `open_db()` connection with WAL mode and `PRAGMA busy_timeout = 5000`. No additional locking primitives are required beyond WAL mode. |
| NFR-04 | **Tracing span completeness:** Every agent-tool invocation MUST produce: (a) a `tool_call` span on the parent trace with `tag.tool_name`, `tag.child_profile`, `tag.child_run_id`; (b) the child run's own root span with `tag.parent_run_id` set. These spans MUST be linked via OpenTelemetry span links (PRD-013). |
| NFR-05 | **Tool list injection size:** Injecting N agent tools adds N synthetic tool definitions to the orchestrator's system prompt. Each tool definition MUST be compact: `name` + `description` (<=200 chars) + `input_schema` should total <= 300 tokens per tool. This limits token overhead to < 3,000 tokens for 10 registered tools. |
| NFR-06 | **Backward compatibility:** Adding `--enable-agent-tools` to `tag submit` is purely additive. Existing `tag submit` invocations without this flag are entirely unaffected. The `agent_tools` table creation (in `open_db()`) is idempotent via `CREATE TABLE IF NOT EXISTS`. |
| NFR-07 | **No new required dependencies:** The agent-as-tool implementation uses only stdlib (`threading`, `subprocess`, `json`, `re`) and packages already present in TAG's dependency set. No new third-party packages are required. |
| NFR-08 | **`max_agent_tool_depth` is enforced at dispatch time, not at registration time.** A tool that would create a cycle (profile A calls profile B which calls profile A) is only detected and blocked at the point the depth limit is reached, not at registration. The error message MUST indicate which profile caused the depth limit breach. |

---

## 10. Technical Design

### 10.1 New Files

- **`src/tag/agent_tool.py`** — Core module: `AgentTool` dataclass, `AgentToolRegistry` (CRUD over SQLite), `AgentToolDispatcher` (intercept + child run execution), `build_tool_schema()`, `truncate_output()`. Fully independently testable without running a live agent.
- No new tables need a separate migration file — DDL is added to the existing `open_db()` `executescript` in `controller.py`.

### 10.2 SQLite DDL

The following DDL is added inside the `conn.executescript(...)` call in `open_db()`:

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
-- Added inside open_db() executescript, after the existing runs table CREATE:
-- These are ALTER TABLE statements guarded by a try/except in Python
-- (SQLite raises OperationalError if column already exists)
ALTER TABLE runs ADD COLUMN parent_run_id TEXT REFERENCES runs(id);
ALTER TABLE runs ADD COLUMN agent_tool_depth INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_runs_parent
  ON runs(parent_run_id)
  WHERE parent_run_id IS NOT NULL;
```

The `ALTER TABLE` statements are wrapped in Python try/except blocks to handle idempotency:

```python
for stmt in [
    "ALTER TABLE runs ADD COLUMN parent_run_id TEXT REFERENCES runs(id)",
    "ALTER TABLE runs ADD COLUMN agent_tool_depth INTEGER NOT NULL DEFAULT 0",
]:
    try:
        conn.execute(stmt)
    except sqlite3.OperationalError as e:
        if "duplicate column" not in str(e).lower():
            raise
```

### 10.3 Core Dataclasses (`src/tag/agent_tool.py`)

```python
from __future__ import annotations

import json
import re
import threading
import time
import uuid
from dataclasses import dataclass, field
from typing import Any

TOOL_NAME_RE = re.compile(r"^[a-z][a-z0-9_]{0,63}$")
MAX_TOOL_NAME_LEN = 64


@dataclass
class AgentTool:
    """Persisted registration of a TAG profile as a callable function tool."""

    id: str                      # "at-" + 12 hex chars
    tool_name: str               # snake_case, matches TOOL_NAME_RE
    profile: str                 # TAG profile directory name
    description: str             # LLM-facing description
    input_schema: dict[str, Any] # JSON Schema object (type: object)
    timeout_seconds: int = 120
    max_output_tokens: int = 4096
    sandbox_override: str | None = None   # None | "force_on" | "force_off"
    invocation_count: int = 0
    last_used_at: str | None = None
    created_at: str = field(default_factory=lambda: _utcnow())
    updated_at: str = field(default_factory=lambda: _utcnow())

    @classmethod
    def make_id(cls) -> str:
        return "at-" + uuid.uuid4().hex[:12]

    def to_llm_tool_definition(self) -> dict[str, Any]:
        """Return the tool definition dict injected into the orchestrator's tool list."""
        return {
            "name": self.tool_name,
            "description": self.description[:200],  # hard cap for token efficiency
            "input_schema": self.input_schema,
        }


@dataclass
class AgentToolInvocation:
    """Single audit record for one agent-tool dispatch."""

    id: str
    tool_name: str
    parent_run_id: str
    child_run_id: str | None
    invoked_at: str
    duration_ms: int | None
    input_tokens: int | None
    output_tokens: int | None
    status: str   # "success" | "error" | "timeout" | "budget_exceeded" | "depth_limit"
    error_message: str | None


@dataclass
class ToolDispatchResult:
    """Result returned from AgentToolDispatcher.dispatch()."""

    tool_response: str        # content to return as tool response to orchestrator
    status: str               # matches AgentToolInvocation.status
    child_run_id: str | None
    duration_ms: int
    output_tokens: int | None
    error_message: str | None = None


def _utcnow() -> str:
    import datetime
    return datetime.datetime.utcnow().strftime("%Y-%m-%dT%H:%M:%SZ")
```

### 10.4 Registry (`AgentToolRegistry`)

```python
class AgentToolRegistry:
    """CRUD operations for agent_tools table."""

    def __init__(self, conn: sqlite3.Connection) -> None:
        self._conn = conn

    def register(self, tool: AgentTool) -> None:
        self._conn.execute(
            """
            INSERT INTO agent_tools
              (id, tool_name, profile, description, input_schema_json,
               timeout_seconds, max_output_tokens, sandbox_override,
               invocation_count, last_used_at, created_at, updated_at)
            VALUES (?,?,?,?,?,?,?,?,0,NULL,?,?)
            """,
            (
                tool.id, tool.tool_name, tool.profile, tool.description,
                json.dumps(tool.input_schema), tool.timeout_seconds,
                tool.max_output_tokens, tool.sandbox_override,
                tool.created_at, tool.updated_at,
            ),
        )
        self._conn.commit()

    def get(self, tool_name: str) -> AgentTool | None:
        row = self._conn.execute(
            "SELECT * FROM agent_tools WHERE tool_name = ?", (tool_name,)
        ).fetchone()
        return _row_to_agent_tool(row) if row else None

    def list_all(self, profile: str | None = None) -> list[AgentTool]:
        if profile:
            rows = self._conn.execute(
                "SELECT * FROM agent_tools WHERE profile = ? ORDER BY created_at",
                (profile,),
            ).fetchall()
        else:
            rows = self._conn.execute(
                "SELECT * FROM agent_tools ORDER BY created_at"
            ).fetchall()
        return [_row_to_agent_tool(r) for r in rows]

    def delete(self, tool_name: str) -> bool:
        """Returns True if a row was deleted."""
        cur = self._conn.execute(
            "DELETE FROM agent_tools WHERE tool_name = ?", (tool_name,)
        )
        self._conn.execute(
            "DELETE FROM agent_tool_invocations WHERE tool_name = ?", (tool_name,)
        )
        self._conn.commit()
        return cur.rowcount > 0

    def record_invocation(self, inv: AgentToolInvocation) -> None:
        self._conn.execute(
            """
            INSERT INTO agent_tool_invocations
              (id, tool_name, parent_run_id, child_run_id, invoked_at,
               duration_ms, input_tokens, output_tokens, status, error_message)
            VALUES (?,?,?,?,?,?,?,?,?,?)
            """,
            (
                inv.id, inv.tool_name, inv.parent_run_id, inv.child_run_id,
                inv.invoked_at, inv.duration_ms, inv.input_tokens,
                inv.output_tokens, inv.status, inv.error_message,
            ),
        )
        self._conn.execute(
            """
            UPDATE agent_tools
            SET invocation_count = invocation_count + 1,
                last_used_at = ?,
                updated_at = ?
            WHERE tool_name = ?
            """,
            (_utcnow(), _utcnow(), inv.tool_name),
        )
        self._conn.commit()


def _row_to_agent_tool(row: sqlite3.Row) -> AgentTool:
    return AgentTool(
        id=row["id"],
        tool_name=row["tool_name"],
        profile=row["profile"],
        description=row["description"],
        input_schema=json.loads(row["input_schema_json"]),
        timeout_seconds=row["timeout_seconds"],
        max_output_tokens=row["max_output_tokens"],
        sandbox_override=row["sandbox_override"],
        invocation_count=row["invocation_count"],
        last_used_at=row["last_used_at"],
        created_at=row["created_at"],
        updated_at=row["updated_at"],
    )
```

### 10.5 Dispatcher (`AgentToolDispatcher`)

```python
class AgentToolDispatcher:
    """
    Intercepts LLM tool calls matching registered agent tools and dispatches
    them as synchronous child agent runs.
    """

    def __init__(
        self,
        registry: AgentToolRegistry,
        cfg: dict[str, Any],
        parent_run_id: str,
        current_depth: int,
        max_depth: int,
        parent_remaining_budget_usd: float | None,
    ) -> None:
        self._registry = registry
        self._cfg = cfg
        self._parent_run_id = parent_run_id
        self._current_depth = current_depth
        self._max_depth = max_depth
        self._parent_remaining_budget_usd = parent_remaining_budget_usd

    def is_agent_tool(self, tool_name: str) -> bool:
        return self._registry.get(tool_name) is not None

    def dispatch(self, tool_name: str, tool_input: dict[str, Any]) -> ToolDispatchResult:
        start_ms = int(time.monotonic() * 1000)
        inv_id = str(uuid.uuid4())
        invoked_at = _utcnow()

        # Depth check
        if self._current_depth >= self._max_depth:
            msg = (
                f"Error: maximum agent tool depth ({self._max_depth}) reached. "
                f"Cannot invoke '{tool_name}'."
            )
            self._registry.record_invocation(AgentToolInvocation(
                id=inv_id, tool_name=tool_name, parent_run_id=self._parent_run_id,
                child_run_id=None, invoked_at=invoked_at,
                duration_ms=0, input_tokens=None, output_tokens=None,
                status="depth_limit", error_message=msg,
            ))
            return ToolDispatchResult(
                tool_response=msg, status="depth_limit",
                child_run_id=None, duration_ms=0, output_tokens=None,
                error_message=msg,
            )

        agent_tool = self._registry.get(tool_name)
        assert agent_tool is not None  # is_agent_tool() was checked by caller

        # Extract prompt from tool input
        prompt = _extract_prompt(tool_input)
        child_run_id = "run-" + uuid.uuid4().hex[:12]

        try:
            result = _run_child_agent(
                cfg=self._cfg,
                profile=agent_tool.profile,
                prompt=prompt,
                child_run_id=child_run_id,
                parent_run_id=self._parent_run_id,
                agent_tool_depth=self._current_depth + 1,
                timeout_seconds=agent_tool.timeout_seconds,
                max_output_tokens=agent_tool.max_output_tokens,
                sandbox_override=agent_tool.sandbox_override,
                parent_remaining_budget_usd=self._parent_remaining_budget_usd,
            )
        except _TimeoutError as e:
            duration_ms = int(time.monotonic() * 1000) - start_ms
            msg = f"Error: agent tool '{tool_name}' timed out after {agent_tool.timeout_seconds} seconds."
            self._registry.record_invocation(AgentToolInvocation(
                id=inv_id, tool_name=tool_name, parent_run_id=self._parent_run_id,
                child_run_id=child_run_id, invoked_at=invoked_at,
                duration_ms=duration_ms, input_tokens=None, output_tokens=None,
                status="timeout", error_message=str(e),
            ))
            return ToolDispatchResult(
                tool_response=msg, status="timeout",
                child_run_id=child_run_id, duration_ms=duration_ms,
                output_tokens=None, error_message=str(e),
            )
        except _BudgetExceededError as e:
            duration_ms = int(time.monotonic() * 1000) - start_ms
            msg = f"Error: agent tool '{tool_name}' cannot run: budget exceeded ({e})."
            self._registry.record_invocation(AgentToolInvocation(
                id=inv_id, tool_name=tool_name, parent_run_id=self._parent_run_id,
                child_run_id=None, invoked_at=invoked_at,
                duration_ms=duration_ms, input_tokens=None, output_tokens=None,
                status="budget_exceeded", error_message=str(e),
            ))
            return ToolDispatchResult(
                tool_response=msg, status="budget_exceeded",
                child_run_id=None, duration_ms=duration_ms,
                output_tokens=None, error_message=str(e),
            )

        duration_ms = int(time.monotonic() * 1000) - start_ms
        self._registry.record_invocation(AgentToolInvocation(
            id=inv_id, tool_name=tool_name, parent_run_id=self._parent_run_id,
            child_run_id=child_run_id, invoked_at=invoked_at,
            duration_ms=duration_ms, input_tokens=result.input_tokens,
            output_tokens=result.output_tokens, status="success",
            error_message=None,
        ))
        return ToolDispatchResult(
            tool_response=result.output, status="success",
            child_run_id=child_run_id, duration_ms=duration_ms,
            output_tokens=result.output_tokens,
        )


def _extract_prompt(tool_input: dict[str, Any]) -> str:
    """Extract a prompt string from tool call arguments.
    
    Looks for 'prompt' key first (default schema), then 'task', then 'query',
    then falls back to JSON-serializing the entire input dict.
    """
    for key in ("prompt", "task", "query", "input"):
        if key in tool_input and isinstance(tool_input[key], str):
            return tool_input[key]
    return json.dumps(tool_input, ensure_ascii=False)
```

### 10.6 Integration Point in `controller.py`

The integration in `cmd_submit` follows this pattern:

```python
# Inside cmd_submit(), after argument parsing and before the main agent loop:

dispatcher: AgentToolDispatcher | None = None
if getattr(args, "enable_agent_tools", False):
    from tag.agent_tool import AgentToolDispatcher, AgentToolRegistry
    registry = AgentToolRegistry(conn)
    all_tools = registry.list_all()

    # Apply --agent-tools allowlist filter if specified
    if getattr(args, "agent_tools", None):
        allowed = set(args.agent_tools.split(","))
        all_tools = [t for t in all_tools if t.tool_name in allowed]

    # Inject tool definitions into the orchestrator's tool list
    for at in all_tools:
        extra_tool_definitions.append(at.to_llm_tool_definition())

    max_depth = getattr(args, "max_agent_tool_depth", 3)
    current_depth = getattr(args, "_agent_tool_depth", 0)  # set by child run invocations

    dispatcher = AgentToolDispatcher(
        registry=registry,
        cfg=cfg,
        parent_run_id=run_id,
        current_depth=current_depth,
        max_depth=max_depth,
        parent_remaining_budget_usd=_get_remaining_budget(conn, run_id),
    )

# Inside the tool-call dispatch loop:
if dispatcher is not None and dispatcher.is_agent_tool(tool_call.name):
    result = dispatcher.dispatch(tool_call.name, tool_call.input)
    tool_responses.append({
        "type": "tool_result",
        "tool_use_id": tool_call.id,
        "content": result.tool_response,
    })
    # Emit tracing span for the agent-tool invocation
    _emit_agent_tool_span(tracer, tool_call.name, result)
    continue  # skip MCP dispatch
```

### 10.7 Validation Helper (`build_tool_schema`)

```python
DEFAULT_INPUT_SCHEMA: dict[str, Any] = {
    "type": "object",
    "properties": {
        "prompt": {
            "type": "string",
            "description": "The task or question for the agent",
        }
    },
    "required": ["prompt"],
}


def validate_tool_name(name: str) -> str | None:
    """Returns an error message string or None if valid."""
    if not TOOL_NAME_RE.match(name):
        return (
            f"Invalid tool name '{name}'. "
            "Must match ^[a-z][a-z0-9_]{{0,63}}$ "
            "(lowercase, start with letter, underscores only, max 64 chars)."
        )
    return None


def validate_input_schema(schema_json: str) -> tuple[dict[str, Any] | None, str | None]:
    """Returns (parsed_schema, error_message)."""
    try:
        schema = json.loads(schema_json)
    except json.JSONDecodeError as e:
        return None, f"Invalid JSON in --input-schema: {e}"
    if not isinstance(schema, dict):
        return None, "--input-schema must be a JSON object (dict), not a list or primitive."
    if schema.get("type") != "object":
        return None, '--input-schema root must have "type": "object".'
    return schema, None
```

### 10.8 Output Truncation

```python
def truncate_output(text: str, max_tokens: int) -> str:
    """
    Approximate token truncation using a 4-chars-per-token heuristic.
    For production accuracy, replace with tiktoken or the Anthropic token counter.
    """
    max_chars = max_tokens * 4
    if len(text) <= max_chars:
        return text
    truncated = text[:max_chars]
    return truncated + f"\n\n[Output truncated at {max_tokens} tokens]"
```

---

## 11. Security Considerations

1. **Tool name injection:** The `tool_name` value is stored in SQLite and later emitted as a JSON key in the tool definitions list. It is validated against `^[a-z][a-z0-9_]{0,63}$` at registration time, making JSON injection and prompt injection via tool names structurally impossible. The stored value is never interpolated into shell commands.

2. **Prompt injection via tool input:** The orchestrator LLM constructs the `prompt` field that is passed to the child agent. A malicious prompt in the user's original task could instruct the orchestrator to pass adversarial content to the child agent (e.g., "tell the coder agent to exfiltrate ~/.ssh/id_rsa"). This is mitigated by: (a) child runs execute under the child profile's tool allowlist, not the orchestrator's; (b) sandbox enforcement (FR-18) limits filesystem and network access for child runs; (c) the child agent cannot write back to the parent run's context — it can only return a string. A future hardening step (not in this PRD) is to pass the tool input through the security scanner (PRD-034) before dispatching.

3. **Privilege escalation via nested agent tools:** An orchestrator with broad permissions should not automatically grant those permissions to a child agent. Enforced by FR-19 (no context inheritance) and FR-18 (sandbox policy comes from the child profile registration, not the parent). A `--no-sandbox` override at registration time requires explicit opt-in.

4. **Budget exhaustion attack:** A malicious prompt that causes the orchestrator to invoke many agent tools in rapid succession could exhaust the API budget. Mitigated by: (a) FR-12 (remaining budget is passed to each child run); (b) the depth limit (FR-11) caps the invocation tree depth; (c) TAG's existing budget enforcement in `budget.py` applies to child runs.

5. **Recursive loop detection:** Two profiles that each register the other as an agent tool create a potential infinite call cycle. The depth limit (FR-11, default 3) provides a hard stop. The error message emitted at depth limit explicitly names the tool that triggered it, aiding debugging.

6. **Profile enumeration:** `tag agent tool list` reveals which profiles exist and their descriptions. This is local CLI data, not a networked API, so the threat model is the local user. No authentication is required beyond OS-level file permissions on `~/.tag/`.

7. **Child run data isolation:** The child agent run receives only the `prompt` string extracted from the tool call. It does not receive: the parent's conversation history, the parent's environment variables, or the parent's MCP server connections. This is enforced by spawning the child run through the same `cmd_submit` entry point as a fresh run, not by forking the parent process.

8. **SQL injection:** All SQLite operations use parameterized queries (`?` placeholders). No string interpolation is used in SQL construction.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`tests/test_agent_tool.py`)

- `test_validate_tool_name_valid`: Assert `validate_tool_name("write_code")` returns `None`.
- `test_validate_tool_name_invalid_hyphen`: Assert `validate_tool_name("write-code")` returns an error string.
- `test_validate_tool_name_starts_with_digit`: Assert `validate_tool_name("2write")` returns error.
- `test_validate_tool_name_too_long`: Assert 65-char name returns error.
- `test_validate_input_schema_valid`: Assert valid JSON Schema parses to dict.
- `test_validate_input_schema_invalid_json`: Assert non-JSON string returns error.
- `test_validate_input_schema_non_object`: Assert `{"type": "array"}` returns error.
- `test_agent_tool_to_llm_definition`: Assert `AgentTool.to_llm_tool_definition()` returns expected dict structure with `name`, `description`, `input_schema`.
- `test_agent_tool_description_capped`: Assert description > 200 chars is capped at 200 in LLM definition.
- `test_truncate_output_under_limit`: Assert short text passes through unchanged.
- `test_truncate_output_over_limit`: Assert long text is truncated and suffix appended.
- `test_extract_prompt_prompt_key`: Assert `_extract_prompt({"prompt": "hello"})` returns `"hello"`.
- `test_extract_prompt_task_key_fallback`: Assert `_extract_prompt({"task": "do X"})` returns `"do X"`.
- `test_extract_prompt_json_fallback`: Assert `_extract_prompt({"foo": "bar"})` returns JSON string.

### 12.2 Registry Unit Tests (with in-memory SQLite)

```python
import sqlite3
from tag.agent_tool import AgentToolRegistry, AgentTool

def make_test_conn():
    conn = sqlite3.connect(":memory:")
    conn.row_factory = sqlite3.Row
    # Apply the DDL
    conn.executescript(AGENT_TOOLS_DDL)
    return conn

def test_registry_register_and_get():
    conn = make_test_conn()
    reg = AgentToolRegistry(conn)
    tool = AgentTool(
        id=AgentTool.make_id(), tool_name="write_code", profile="coder",
        description="Write code", input_schema=DEFAULT_INPUT_SCHEMA,
    )
    reg.register(tool)
    fetched = reg.get("write_code")
    assert fetched is not None
    assert fetched.tool_name == "write_code"
    assert fetched.profile == "coder"

def test_registry_duplicate_name_raises():
    conn = make_test_conn()
    reg = AgentToolRegistry(conn)
    tool = AgentTool(id=AgentTool.make_id(), tool_name="t1", ...)
    reg.register(tool)
    with pytest.raises(sqlite3.IntegrityError):
        reg.register(AgentTool(id=AgentTool.make_id(), tool_name="t1", ...))

def test_registry_delete_returns_true():
    ...

def test_registry_delete_nonexistent_returns_false():
    ...

def test_registry_list_filter_by_profile():
    ...
```

### 12.3 Dispatcher Unit Tests (mocked child run)

The `_run_child_agent` function is extracted as a dependency that can be mocked:

```python
def test_dispatcher_depth_limit():
    reg = AgentToolRegistry(make_test_conn())
    reg.register(AgentTool(tool_name="write_code", profile="coder", ...))
    dispatcher = AgentToolDispatcher(
        registry=reg, cfg={}, parent_run_id="run-abc",
        current_depth=3, max_depth=3, parent_remaining_budget_usd=1.0,
    )
    result = dispatcher.dispatch("write_code", {"prompt": "test"})
    assert result.status == "depth_limit"
    assert "maximum agent tool depth" in result.tool_response

def test_dispatcher_success_records_invocation(mock_run_child_agent):
    mock_run_child_agent.return_value = ChildRunResult(
        output="def fib(n): ...", input_tokens=50, output_tokens=80
    )
    ...
    result = dispatcher.dispatch("write_code", {"prompt": "write fibonacci"})
    assert result.status == "success"
    assert "def fib" in result.tool_response
    inv = reg._conn.execute("SELECT * FROM agent_tool_invocations").fetchone()
    assert inv["status"] == "success"
    assert inv["tool_name"] == "write_code"
```

### 12.4 Integration Tests

- `test_cli_register_then_list`: Run `tag agent tool register --profile coder --as-tool write_code` on a test DB, then `tag agent tool list --json`, assert JSON contains the registered tool.
- `test_cli_register_dry_run_no_write`: Run with `--dry-run`, assert SQLite row count is 0 after.
- `test_cli_register_invalid_profile`: Register with `--profile nonexistent`, assert exit code 1.
- `test_cli_register_duplicate_without_force`: Register same name twice, assert exit code 1 on second.
- `test_cli_unregister_existing`: Register then unregister, assert list is empty.
- `test_cli_unregister_nonexistent`: Unregister a name that was never registered, assert exit code 1.
- `test_submit_enable_agent_tools_injects_definitions`: Mock the LLM call capture, assert injected tool definitions include registered tool schemas.
- `test_submit_child_run_has_parent_run_id`: Run a submit that triggers an agent-tool call (using a mock LLM that returns a tool call), assert child run's `parent_run_id` is set in SQLite.

### 12.5 Performance Tests

- **Dispatch latency overhead:** Time 100 `dispatcher.is_agent_tool()` calls with 50 registered tools. Assert average < 1ms (single SQLite index lookup).
- **Tool definition injection size:** With 20 registered tools, measure added token count in the orchestrator's system prompt. Assert total tool injection is < 6,000 tokens (300 tokens × 20 tools).
- **Concurrent child runs:** Dispatch 5 child runs simultaneously (using `ThreadPoolExecutor`), assert all complete without SQLite locking errors, assert WAL mode absorbs the concurrency.

---

## 13. Acceptance Criteria

| ID | Criterion | How to Test |
|----|-----------|-------------|
| AC-01 | `tag agent tool register --profile coder --as-tool write_code` exits 0 and writes a row to `agent_tools` with `tool_name='write_code'` and `profile='coder'`. | `sqlite3 ~/.tag/runtime/tag.sqlite3 "SELECT tool_name, profile FROM agent_tools WHERE tool_name='write_code'"` returns one row. |
| AC-02 | `tag agent tool register --profile nonexistent --as-tool foo` exits 1 with a message containing "profile" and "nonexistent". | Subprocess return code check + stderr pattern match. |
| AC-03 | `tag agent tool register --as-tool "bad-name"` exits 1 with a message mentioning the invalid name and expected pattern. | Subprocess return code check + stderr pattern match. |
| AC-04 | `tag agent tool list --json` emits valid JSON array; each element has `id`, `tool_name`, `profile`, `description`, `input_schema`, `timeout_seconds`, `max_output_tokens`, `created_at` fields. | `tag agent tool list --json | python3 -m json.tool` exits 0. |
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
| PRD-028 Sandbox Code Execution | Internal PRD | Required for `sandbox_override` enforcement in child runs; the `--sandbox` flag on child `cmd_submit` invocations must be wired |
| PRD-034 Security / Secret Scanning | Internal PRD | Recommended as a hardening dependency — future work to scan tool inputs before child dispatch |
| PRD-027 Eval Framework | Internal PRD | Integration: `tag eval run` can use `--enable-agent-tools` to test orchestrator profiles against multi-agent eval suites |
| PRD-014 MCP Server Registry | Internal PRD | The tool interception logic runs before MCP dispatch; must hook into the same dispatch point where MCP tools are resolved |
| PRD-077 Scope-Based Tool Filtering | Internal PRD | Child runs inherit the scope filters of their registered profile; filter application in child `cmd_submit` is via existing `tool_retrieval.py` path |
| `budget.py` | Internal module | `_get_remaining_budget()` is called before each child dispatch; budget decrement after child run must propagate to parent's budget tracking |
| `tracing.py` | Internal module | `_emit_agent_tool_span()` uses the existing OpenTelemetry tracer from `tracing.py` |
| `sqlite3` (stdlib) | Runtime | WAL mode, parameterized queries, `open_db()` pattern |
| `threading` (stdlib) | Runtime | `threading.Event` for synchronous wait on child run completion |
| `json` (stdlib) | Runtime | Input schema serialization, tool definition emission |
| `re` (stdlib) | Runtime | Tool name validation regex |
| `uuid` (stdlib) | Runtime | `AgentTool.make_id()`, invocation IDs |

---

## 15. Open Questions

| ID | Question | Owner | Target Resolution |
|----|----------|-------|-------------------|
| OQ-01 | Should agent tools be composable with MCP tool calls in the same orchestrator turn? (i.e., can the orchestrator call `write_code` and `github:create_pr` in the same multi-tool call batch?) Current design says yes — agent tools are injected into the same tool list. Risk: the LLM may try to call them in parallel. In that case, do we dispatch child runs in parallel or sequentially? | Engineering | Before implementation start — decide whether to add `parallel_dispatch: bool` to `AgentTool` |
| OQ-02 | The `_run_child_agent` internal function shares significant logic with `cmd_submit`. Should it call `cmd_submit` as a function, or duplicate the minimal subset of submit logic needed for a headless child run? Calling `cmd_submit` risks pulling in TTY/TUI logic into a non-interactive context. | Engineering | Architecture review before implementation |
| OQ-03 | Should the orchestrator receive streaming output from the child run as it is generated (enabling progress tokens or intermediate reasoning), or is the current "return final output as a string" sufficient for v1? Streaming would require significant protocol changes (SSE passthrough or WebSocket bridge). | Product | Deferred to v2 unless user research shows blocking is a significant pain point |
| OQ-04 | How should agent tool descriptions handle multi-language prompts? The `--description` is authored in the registration language (presumably English). If the orchestrator operates in another language, the tool selection may degrade. Should descriptions support locale variants? | Product | Deferred — internationalization is a separate workstream |
| OQ-05 | Is `max_output_tokens: 4096` the right default? Researcher profiles often produce long-form output (references, multi-paragraph summaries). At 4096 tokens (~16KB), a detailed research summary fits. But a coder profile that generates a large file may truncate mid-function. Should `max_output_tokens` default differ per profile type? | Engineering | Gather feedback from early adopters in first sprint; adjust default before GA |
| OQ-06 | Should `tag agent tool register --force` overwrite-in-place (UPDATE) or delete-and-insert? UPDATE preserves `invocation_count` and `created_at`; delete-and-insert resets them. The UX implication is whether updating a tool's profile or description is treated as a configuration edit (preserve history) or a fresh registration (reset). | Engineering | Decide before implementing `--force`; current lean is UPDATE to preserve history |
| OQ-07 | Is 3 the right default for `max_agent_tool_depth`? A depth of 3 supports: orchestrator (depth 0) → specialist A (depth 1) → sub-specialist B (depth 2) → leaf tool call (depth 3, blocked). In practice, are there legitimate use cases for depth 4+? | Engineering/Product | Gather data from multi-agent pilot users during beta |
| OQ-08 | Should `tag agent tool list` show the full `input_schema` JSON inline, or should it be summarized? For wide TTYs it fits; for narrow terminals it will wrap badly. Rich's `Table` supports truncated columns — should `input_schema` be hidden by default with a `--verbose` flag to show it? | Engineering | Low priority — resolve during implementation based on terminal width heuristics |

---

## 16. Complexity and Timeline

**Overall Effort:** M (1-2 weeks for a single engineer)

### Phase 1 — SQLite DDL and Core Dataclasses (Day 1-2)

- Add `agent_tools` and `agent_tool_invocations` DDL to `open_db()` in `controller.py`.
- Add `ALTER TABLE runs ADD COLUMN parent_run_id` and `agent_tool_depth` with idempotent guards.
- Implement `AgentTool`, `AgentToolInvocation`, `ToolDispatchResult` dataclasses in new file `src/tag/agent_tool.py`.
- Implement `AgentToolRegistry` (register, get, list_all, delete, record_invocation).
- Implement `validate_tool_name()`, `validate_input_schema()`, `truncate_output()`, `_extract_prompt()`.
- Write unit tests for all pure functions and registry CRUD against in-memory SQLite.

**Deliverable:** `agent_tool.py` with full registry and validation, tested in isolation. `open_db()` creates new tables without breaking existing tests.

### Phase 2 — CLI Commands (Day 3-4)

- Add `cmd_agent_tool_register()`, `cmd_agent_tool_list()`, `cmd_agent_tool_unregister()`, `cmd_agent_tool_show()` to `controller.py`.
- Wire `tag agent tool` subcommand group in the argparse router.
- Implement `--dry-run`, `--json`, `--force` flags on register.
- Write CLI integration tests using subprocess + in-memory (temp-dir) SQLite.

**Deliverable:** All `tag agent tool` subcommands functional end-to-end. `tag agent tool register --dry-run` and `tag agent tool list --json` produce correct output.

### Phase 3 — Dispatcher and `cmd_submit` Integration (Day 5-8)

- Implement `AgentToolDispatcher` with depth checking, budget checking, and `_run_child_agent()` stub.
- Implement `_run_child_agent()` as a function that internally invokes the submit flow without TTY dependencies.
- Add `--enable-agent-tools`, `--agent-tools`, and `--max-agent-tool-depth` flags to `tag submit` argparse.
- Wire dispatcher into `cmd_submit` tool-dispatch loop.
- Implement `_emit_agent_tool_span()` for tracing integration (PRD-013).
- Write integration tests: mock LLM that emits agent-tool calls, assert child runs created with correct `parent_run_id`, assert depth limit error, assert invocation audit record.

**Deliverable:** End-to-end multi-agent run works: orchestrator → agent-tool-call → child agent run → tool response returned → orchestrator continues.

### Phase 4 — Hardening and Eval Integration (Day 9-10)

- Add budget propagation: `_get_remaining_budget()` hook into `budget.py`, pass to dispatcher.
- Add sandbox policy enforcement for child runs via `sandbox_override`.
- Add `--enable-agent-tools` support to `tag eval run` (pass-through to the spawned submit).
- Write performance tests: dispatch latency, tool definition injection token count.
- Update `docs/prd/INDEX.md` to include PRD-083.
- Final acceptance criteria verification run.

**Deliverable:** All 15 acceptance criteria pass. Performance targets met. Eval integration functional.

---

*End of PRD-083*

