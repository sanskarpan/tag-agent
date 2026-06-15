# PRD-021: Agent Loop / Autonomous Mode

**Status:** Proposed  
**Priority:** P1  
**Estimated Effort:** M (2–3 weeks)  
**Affects:** `controller.py` (new `cmd_loop`), new `src/tag/loop.py`, `queue_worker.py`, `pyproject.toml`, `tag.sqlite3` schema

---

## 1. Overview

TAG currently executes single-shot agent invocations: one prompt in, one response out, done. There is no primitive for multi-turn autonomous execution where an agent iterates toward a stated goal, reflects on partial results, decides its next action, and converges on completion without human intervention between turns. This PRD defines `tag loop`, an autonomous multi-turn execution mode that wraps the existing Hermes single-turn runtime in a goal-directed iteration loop. Each turn feeds the previous turn's output back as context for the next prompt. The loop persists a full turn-by-turn journal in SQLite, detects goal completion by evaluating a structured stop signal from the agent's own output, enforces configurable safety limits (max turns, tool approval gates, cost ceilings), and supports abort, resume, and dry-run modes. The design draws from `llm-loop`'s `--max-turns 25` default, Claude Code's `ScheduleWakeup` self-pacing pattern, and Aider's architect/editor split to separate goal planning from step-by-step execution.

---

## 2. Goals

1. `tag loop --profile <p> --goal "<g>"` runs an agent autonomously for up to `--max-turns` iterations (default 25) until the agent signals completion or the turn limit is reached, with all output persisted to SQLite.
2. Each turn is individually observable: `tag loop status <loop-id>` shows turn-by-turn progress, token cost, and current working context in real time.
3. Goal completion is detected via a structured JSON stop signal embedded in the agent's final message (`{"tag_loop_done": true, "summary": "..."}`), with a fallback heuristic for models that do not emit the signal.
4. An interactive approval gate (`--approve`) pauses before every tool call, displaying the tool name and arguments and requiring `y/n` input, so users can supervise dangerous side-effects.
5. A tool allowlist gate (`--approve-tools bash,write_file`) restricts which tool names can be called; any tool not on the list is blocked and the agent is re-prompted to use an allowed alternative.
6. The loop journal — full turn history stored in `loop_runs` SQLite table — is summarized using a rolling compression window when the estimated context size crosses 70% of the model's declared context limit, preventing context overflow without losing goal-critical history.
7. `tag loop abort <loop-id>` sends a SIGTERM to the active worker process, sets status to `aborted`, and writes a partial journal; `tag loop resume <loop-id>` reconstructs context from the journal and continues from the last completed turn.
8. `--dry-run` mode renders what each turn's prompt would look like, estimates token cost per turn from a configured tokens-per-turn baseline, and outputs a forecast report — without calling the Hermes runtime.

---

## 3. Non-Goals

1. **Fully autonomous code deployment** — the loop can write files and run shell commands if the agent profile allows those tools, but pushing to production, merging PRs, or deploying infrastructure is outside scope; operators must configure tool allowlists to prevent it.
2. **Multi-agent loops** — this PRD covers a single profile running N turns. Swarm/kanban orchestration of multiple profiles is covered by PRD-004. A loop can _create_ queue jobs (PRD-008) as sub-tasks, but it does not manage their lifecycle.
3. **Streaming TUI output per turn** — the loop runner writes structured JSON to SQLite; a rich streaming TUI panel for live loop monitoring is a follow-on (PRD-003 extension). In v1, `tag loop status` polls SQLite and renders a static table.
4. **Remote/distributed loop workers** — loops run as local detached processes, the same way queue jobs do (PRD-008). Remote execution on Modal or Daytona is a future integration point.
5. **Goal decomposition / planning** — the loop does not automatically decompose a high-level goal into subtasks or build a dependency graph. The agent is expected to do its own planning within its system prompt.
6. **Persistent cross-loop goal tracking** — goals are scoped to a loop run. Cross-loop goal tracking and reporting belongs in PRD-002 (memory journal) via a dedicated key namespace.

---

## 4. User Stories

### US-01: Single-turn developer to autonomous coder
**As a** developer who currently runs `tag submit "implement feature X"` and pastes the output back as a follow-up,  
**I want to** run `tag loop --profile coder --goal "implement the auth module from the spec in AUTH_SPEC.md"`  
**so that** the agent iterates, writes code, reads it back, fixes compilation errors, and signals done — without me supervising each step.

**Acceptance Criteria:**
- Loop starts with the goal as the first-turn system prompt.
- Each turn's output is stored in `loop_runs` with `turn_number`, `input_prompt`, `output_text`, `tool_calls`, `tokens_used`, `cost_usd`, `status`.
- When the agent emits `{"tag_loop_done": true}`, the loop exits with status `completed`.
- `tag loop status <id>` shows each turn's summary and cumulative cost.
- Total cost is within 20% of `--cost-limit` if provided; loop aborts before exceeding it.

### US-02: Security-conscious team lead enabling supervised tool use
**As a** security-conscious team lead using TAG in a CI-adjacent context,  
**I want to** run `tag loop --profile devops --goal "clean up stale branches" --approve-tools gh,git`  
**so that** the agent can only call `gh` and `git` tools, and any other tool call is blocked and the agent is re-prompted.

**Acceptance Criteria:**
- Tool calls with a name not in `--approve-tools` list are intercepted before execution.
- The agent receives a tool refusal message: `"Tool '<name>' is not in the approved list: [gh, git]. Use an approved tool or change approach."`.
- The turn counter increments for the re-prompt; a blocked tool call does not count as an agent failure.
- `tag loop status <id>` shows blocked tool calls in a `blocked_tools` column per turn.

### US-03: Researcher running a long multi-step analysis
**As a** researcher using TAG with a `researcher` profile,  
**I want to** run `tag loop --profile researcher --goal "analyze all papers in ./papers/ and produce a synthesis report" --max-turns 50 --journal`  
**so that** a detailed turn-by-turn journal is written to a markdown file I can review even if I restart my machine.

**Acceptance Criteria:**
- `--journal` writes each completed turn to `~/.tag/runtime/loop-journals/<loop-id>.md` as markdown with timestamps.
- Journal persists if the loop is aborted or the process crashes.
- Journal file is human-readable: turn number, timestamp, truncated input, full output, tool calls listed.
- `tag loop resume <loop-id>` reconstructs context from the journal and picks up at turn N+1.

### US-04: DevOps engineer testing before committing to a long run
**As a** DevOps engineer before running an expensive loop,  
**I want to** run `tag loop --profile coder --goal "refactor the entire payments module" --max-turns 40 --dry-run`  
**so that** I see the projected turn structure, estimated token cost per turn, and total estimated cost before spending any money.

**Acceptance Criteria:**
- `--dry-run` prints a table: `Turn | Est. input tokens | Est. output tokens | Est. cost (USD)`.
- Each turn's estimated prompt size = (system prompt tokens) + (goal tokens) + (avg turn output tokens * turn_number * compression_ratio).
- No Hermes process is launched; no SQLite rows are written for actual turns.
- A `--cost-limit 5.00` flag causes dry-run to annotate which turn would breach the limit with a warning row.

### US-05: On-call engineer aborting a runaway loop
**As an** on-call engineer who notices a loop is taking unexpected actions,  
**I want to** run `tag loop abort <loop-id>`  
**so that** the loop is stopped immediately, the current turn's partial output is saved, and I can inspect the journal to understand what happened.

**Acceptance Criteria:**
- `tag loop abort <loop-id>` sends SIGTERM to the worker PID.
- The worker catches SIGTERM, writes a partial turn row with `status='aborted'`, sets `loop_runs.status='aborted'`, and exits within 5 seconds.
- `tag loop status <loop-id>` shows `status: aborted` with the turn number at which abort occurred.
- No additional Hermes calls are made after abort is issued.

### US-06: Developer resuming after machine sleep
**As a** developer whose laptop went to sleep mid-loop,  
**I want to** run `tag loop resume <loop-id>`  
**so that** the loop picks up where it left off, using the journal to reconstruct the conversation context.

**Acceptance Criteria:**
- `resume` reads all completed turns from `loop_runs` table for the given `loop_id`.
- Context is reconstructed as: `system_prompt + goal + turn_1_output + turn_2_output + ... + turn_N_output`.
- If context reconstruction would exceed 70% of model context limit, the rolling summarization step is applied first.
- A new worker process is launched; the loop_id and turn numbering continue (turn N+1, N+2, ...).
- `tag loop list` shows the loop with status `running` and `resumed_at` timestamp.

### US-07: Operator running a loop without TAG hooks firing
**As an** operator running a loop in a sandboxed environment where hooks might call external endpoints,  
**I want to** run `tag loop --profile coder --goal "..." --no-hooks`  
**so that** none of the TAG webhook/event hooks fire during the loop run.

**Acceptance Criteria:**
- `--no-hooks` sets an env var `TAG_DISABLE_HOOKS=1` for the worker process.
- All `fire_hook` calls in `controller.py` skip execution when this env var is set.
- Hook log entries are not written for this loop run.
- The flag is shown in `tag loop status <id>` as `hooks: disabled`.

---

## 5. Proposed CLI Surface

### 5.1 Primary command: `tag loop`

```
tag loop [OPTIONS]

  Run an agent in autonomous multi-turn mode toward a stated goal.

Options:
  --profile TEXT          Profile name to use (required). Must exist in
                          ~/.tag/config/tag.yaml profiles section.
  --goal TEXT             Goal statement for the agent. Injected as the
                          initial user message and repeated in the system
                          prompt prefix. Required.
  --max-turns INTEGER     Maximum number of agent turns before the loop
                          terminates with status 'turn_limit_reached'.
                          Default: 25. Range: 1–500.
  --approve               Pause before every tool call and ask the user
                          y/n. Requires an interactive TTY; errors if
                          stdin is not a TTY.
  --approve-tools TEXT    Comma-separated list of tool names that are
                          allowed. Any tool call not in this list is
                          blocked and the agent is re-prompted. Example:
                          --approve-tools "bash,read_file,write_file".
  --cost-limit FLOAT      Abort the loop if cumulative cost_usd exceeds
                          this value. Example: --cost-limit 5.00.
  --dry-run               Estimate turn structure and cost without
                          running the agent. Prints a projection table
                          and exits.
  --journal               Write a human-readable markdown journal to
                          ~/.tag/runtime/loop-journals/<loop-id>.md after
                          each turn completes.
  --no-hooks              Disable TAG event hooks for this loop run.
  --config PATH           Path to tag.yaml config (default: ~/.tag/config/tag.yaml).
  --loop-id TEXT          Use a specific loop ID (UUID) instead of
                          auto-generating one. Useful for idempotent
                          re-runs in CI.
  --parent-loop-id TEXT   Mark this loop as a sub-loop of a parent loop.
  --quiet                 Suppress per-turn progress output. Only print
                          final result or error.
  --json                  Output final result as JSON (loop_id, status,
                          turns_completed, total_cost_usd, summary).
  --timeout INTEGER       Per-turn timeout in seconds. If a single Hermes
                          call exceeds this, the turn is marked 'timeout'
                          and the loop aborts. Default: 300.
  --context-compress-at FLOAT
                          Fraction of model context limit at which to
                          trigger rolling summarization. Default: 0.70.
  --system-prefix TEXT    Additional text prepended to the system prompt
                          for all turns.

Examples:
  # Basic autonomous loop
  tag loop --profile coder --goal "implement the auth module from AUTH_SPEC.md"

  # 50 turns, supervised tools, cost cap, write journal
  tag loop --profile researcher \
    --goal "synthesize all papers in ./papers/ into a report" \
    --max-turns 50 \
    --approve-tools "read_file,bash" \
    --cost-limit 10.00 \
    --journal

  # Dry run to preview cost
  tag loop --profile coder --goal "refactor payments module" --max-turns 40 --dry-run

  # With full tool approval gate
  tag loop --profile devops --goal "clean stale branches" --approve

  # Quiet JSON output for CI
  tag loop --profile coder --goal "fix all linting errors" --quiet --json
```

### 5.2 `tag loop list`

```
tag loop list [OPTIONS]

  List all loop runs (active, completed, aborted, failed).

Options:
  --profile TEXT    Filter by profile name.
  --status TEXT     Filter by status: running|completed|aborted|failed|
                    turn_limit_reached|cost_limit_reached. Comma-separated.
  --limit INTEGER   Maximum rows to show. Default: 20.
  --json            Output as JSON array.

Output columns:
  LOOP ID  | PROFILE | STATUS               | TURNS | COST (USD) | GOAL (truncated 60 chars) | CREATED AT
  ---------|---------|----------------------|-------|------------|---------------------------|------------
  abc12345 | coder   | completed            | 12    | $0.87      | implement the auth module | 2026-06-12T10:00Z
  def67890 | coder   | running              | 7/25  | $0.43      | refactor payments module  | 2026-06-12T11:00Z
  ghi11121 | devops  | turn_limit_reached   | 25/25 | $2.10      | clean stale branches      | 2026-06-11T09:00Z

Examples:
  tag loop list
  tag loop list --profile coder --status running,failed --limit 10 --json
```

### 5.3 `tag loop status <loop-id>`

```
tag loop status LOOP_ID [OPTIONS]

  Show detailed turn-by-turn status for a loop run.

Options:
  --json       Output full details as JSON.
  --turns      Show per-turn breakdown (default: true).
  --no-turns   Hide per-turn table, show summary only.

Output:
  Loop ID:       abc12345
  Profile:       coder
  Goal:          implement the auth module from AUTH_SPEC.md
  Status:        completed
  Turns:         12 / 25
  Total cost:    $0.87 (prompt: $0.61  completion: $0.26)
  Total tokens:  18,430 (prompt: 14,200  completion: 4,230)
  Created:       2026-06-12T10:00:00Z
  Completed:     2026-06-12T10:14:32Z
  Duration:      14m 32s
  Hooks:         enabled
  Parent loop:   —

  TURN | STATUS    | TOKENS | COST   | TOOL CALLS                    | SUMMARY (60 chars)
  -----|-----------|--------|--------|-------------------------------|--------------------
   1   | completed | 1,204  | $0.07  | read_file(AUTH_SPEC.md)       | Read spec, planninng...
   2   | completed | 1,589  | $0.09  | write_file(auth/models.py)    | Wrote User model...
  ...
  12   | completed | 1,102  | $0.06  | —                             | DONE: All tests pass...

Examples:
  tag loop status abc12345
  tag loop status abc12345 --json
  tag loop status abc12345 --no-turns
```

### 5.4 `tag loop abort <loop-id>`

```
tag loop abort LOOP_ID [OPTIONS]

  Abort a running loop. Sends SIGTERM to the worker process.
  The worker saves partial turn state before exiting.

Options:
  --force    Send SIGKILL instead of SIGTERM if the process does not exit
             within 5 seconds.

Exit codes:
  0  Loop was running and has been successfully signalled.
  1  Loop ID not found.
  2  Loop is not in running status.

Examples:
  tag loop abort abc12345
  tag loop abort abc12345 --force
```

### 5.5 `tag loop resume <loop-id>`

```
tag loop resume LOOP_ID [OPTIONS]

  Resume a loop that was aborted, failed, or hit turn_limit_reached.
  Reconstructs context from journal and continues from the next turn.

Options:
  --max-turns INTEGER   Override the max_turns for the resumed run.
                        Adds to the existing turn count by default
                        (--max-turns here means N additional turns).
  --reset-max-turns     Resets turn counter to 0 and restarts max-turns
                        from scratch (use for turn_limit_reached loops).
  --approve             Re-enable approval gate for resumed run.
  --no-hooks            Disable hooks for resumed run.

Behaviour:
  - Creates a new loop_id for the resumed run with parent_loop_id pointing
    to the original loop_id.
  - Reconstructs context from all completed turns in the original loop.
  - Applies rolling summarization if context > context-compress-at threshold.
  - Starts at turn_number = (last completed turn + 1).

Examples:
  tag loop resume abc12345
  tag loop resume abc12345 --max-turns 15
  tag loop resume ghi11121 --reset-max-turns --max-turns 25
```

---

## 6. Functional Requirements

**FR-01 — Turn Orchestration:**  
The loop runner executes turns sequentially (never concurrently). Turn N's full output text becomes the `input_prompt` prefix for turn N+1, prepended with a structured context header: `## Previous Turn Output (Turn {N})`. The agent's goal is re-injected at the top of every turn's user message.

**FR-02 — First Turn Prompt Structure:**  
Turn 1's prompt is structured as:
```
[SYSTEM]
You are running in TAG autonomous mode. Your goal: {goal}
...profile system prompt...
[AGENT STOP SIGNAL]
When you have fully completed the goal, include this exact JSON in your response:
{"tag_loop_done": true, "summary": "<one-sentence summary of what was accomplished>"}

[USER]
Goal: {goal}

Please begin working toward this goal.
```

**FR-03 — Goal Completion Detection (Primary):**  
After each turn, the output text is scanned (with `json.loads` on each candidate substring) for a top-level JSON object containing `"tag_loop_done": true`. The scan is done on the last 500 characters of the output text first (fast path), then the entire output if not found. When detected, the loop exits with status `completed` and persists the `summary` field.

**FR-04 — Goal Completion Detection (Heuristic Fallback):**  
If no `tag_loop_done` signal is found, a configurable heuristic is applied. The heuristic checks the last 200 tokens of the output for any of these completion phrases: `"task complete"`, `"goal accomplished"`, `"all done"`, `"finished successfully"`, `"nothing more to do"`. If matched, the loop exits with status `completed_heuristic`. This fallback is logged as a warning in the loop journal. The heuristic is opt-in via `loop.heuristic_completion: true` in `tag.yaml`.

**FR-05 — Maximum Turns Enforcement:**  
If `turn_number` reaches `max_turns` without a completion signal, the loop exits with status `turn_limit_reached`. The final partial state is committed to `loop_runs`. A desktop notification is sent if `notify` is enabled.

**FR-06 — Cost Limit Enforcement:**  
Before each turn, the cumulative `cost_usd` is calculated from all completed turns in `loop_runs`. If `sum(cost_usd) >= cost_limit`, the loop exits with status `cost_limit_reached` before launching the turn. The exit message includes the total cost spent.

**FR-07 — Per-Turn Timeout:**  
The Hermes subprocess for each turn is launched with `subprocess.run(..., timeout=timeout_seconds)`. On `subprocess.TimeoutExpired`, the current turn row is written with `status='timeout'` and the loop exits with status `failed`. Partial stdout is captured and stored in `output_text`.

**FR-08 — Approval Gate (`--approve`):**  
When `--approve` is active, `tag loop` operates in foreground (not detached). Before each Hermes call, the runner prints the pending turn's full prompt and asks: `"Turn N: run agent? [y/N] "`. If the user inputs `n`, the loop exits with status `user_cancelled`. This flag requires `sys.stdin.isatty() == True`; if stdin is not a TTY, `--approve` raises `SystemExit` with a clear error message.

**FR-09 — Tool Allowlist Gate (`--approve-tools`):**  
The tool allowlist is enforced by post-processing the agent's response. If a tool call (identified by the `tool_calls` JSON in the Hermes output) contains a tool name not in the allowlist, the loop runner does not execute it. Instead, it injects a forced user message for the next turn: `"Tool call to '<name>' was blocked (not in approved list: [...])."` This message replaces the previous turn's output in the context for the following turn. The blocked call is recorded in `tool_calls` with `"blocked": true`.

**FR-10 — Journal Storage:**  
When `--journal` is active, after each turn completes, the runner appends to `~/.tag/runtime/loop-journals/<loop-id>.md`:
```markdown
## Turn {N} — {iso_timestamp}
**Status:** {status}
**Tokens:** {tokens_used} | **Cost:** ${cost_usd:.4f}
**Tool calls:** {comma-separated tool names or "none"}

### Input (truncated to 500 chars)
{input_prompt[:500]}...

### Output
{output_text}

---
```
File writes are atomic (write to a `.tmp` file, then `os.replace`).

**FR-11 — Rolling Context Summarization:**  
Before building the input prompt for each turn, the runner estimates the current context size by summing `len(output_text)` of all completed turns (using a characters-to-tokens ratio of 4:1). If estimated tokens / model_context_limit > `context_compress_at` (default 0.70), the oldest 50% of turns' output texts are replaced by a summary. Summarization is performed by calling Hermes with a dedicated summarization prompt: `"Summarize the following conversation history in 300 words, preserving all key decisions, code written, and tool outputs."`. The summary replaces the original turn outputs in context. The original outputs remain unmodified in `loop_runs`.

**FR-12 — Abort Safety:**  
The loop worker registers `signal.signal(signal.SIGTERM, _sigterm_handler)` on startup. The handler sets a global flag `_abort_requested = True`. The main turn loop checks this flag at the start of each turn and after each Hermes subprocess completes. On abort: (a) current turn is written with `status='aborted'`, (b) loop row `status` is set to `aborted`, (c) journal is flushed, (d) process exits cleanly.

**FR-13 — Resume from Journal:**  
`tag loop resume <loop-id>` reads all `loop_runs` rows for the given `loop_id` ordered by `turn_number`. It reconstructs the context string by concatenating all completed turn outputs with structured headers. It then applies rolling summarization if needed. It creates a new `loop_runs` parent row with a new `loop_id` and sets `parent_loop_id` to the original. It launches a new worker starting at `turn_number = max_completed_turn + 1`.

**FR-14 — Dry-Run Mode:**  
`--dry-run` does not call Hermes. Instead it:
1. Loads the profile system prompt and measures its token length.
2. Projects each turn's input token count as: `system_tokens + goal_tokens + (turn_number * avg_output_tokens * compression_factor)`.
3. Estimates cost using the model's input and output token rates from `~/.tag/config/tag.yaml` model pricing table.
4. Prints a table (via `rich.table.Table`) and exits. No SQLite writes occur.

**FR-15 — `--no-hooks` Flag:**  
When `--no-hooks` is set, the loop worker is launched with `TAG_DISABLE_HOOKS=1` in its environment. Controller code that fires hooks checks `os.environ.get("TAG_DISABLE_HOOKS") == "1"` and skips all hook firing.

**FR-16 — Loop ID Generation:**  
If `--loop-id` is not provided, the runner generates a UUID4 hex truncated to 12 characters (e.g., `a3f9c2e1b804`). If `--loop-id` is provided and a loop with that ID already exists with a non-terminal status, the command errors: `"Loop ID already exists and is not in a terminal state"`.

**FR-17 — Notification on Completion:**  
On loop completion (any terminal status), `send_desktop_notification("TAG Loop", f"{profile}: {status} after {N} turns — {summary[:60]}")` is called unless `--quiet` is set or `notify: false` is in the config.

**FR-18 — Structured JSON Output (`--json`):**  
When `--json` is set, the loop outputs a single JSON object on exit:
```json
{
  "loop_id": "a3f9c2e1b804",
  "profile": "coder",
  "status": "completed",
  "turns_completed": 12,
  "total_tokens": 18430,
  "total_cost_usd": 0.87,
  "summary": "Implemented the auth module with User model, JWT tokens, and tests.",
  "created_at": "2026-06-12T10:00:00Z",
  "completed_at": "2026-06-12T10:14:32Z"
}
```

---

## 7. Non-Functional Requirements

### 7.1 Performance
- **Turn overhead:** The loop orchestration overhead (SQLite write, journal append, context build) must add no more than 500ms per turn above the raw Hermes call latency.
- **Context build time:** Reconstructing context from journal for resume must complete in under 2 seconds for up to 500 turns.
- **DB write latency:** Each `loop_runs` row write must complete in under 100ms with WAL mode enabled.
- **Dry-run speed:** `--dry-run` with 100 turns must complete in under 1 second.

### 7.2 Reliability
- **Crash recovery:** If the worker process crashes mid-turn without writing a terminal status, `tag loop resume` detects the last turn with `status='running'` and re-runs it from the beginning of that turn.
- **SQLite WAL:** All loop DB operations use `PRAGMA journal_mode = WAL` and `PRAGMA busy_timeout = 5000` (identical to existing `queue_worker.py` pattern).
- **Atomic journal writes:** Journal markdown files are written to a `.tmp` path and renamed atomically via `os.replace` to prevent partial reads.
- **Turn idempotency:** If the same `loop_id` and `turn_number` pair already exist with `status='completed'`, the runner skips re-execution and logs a warning.

### 7.3 Observability
- **Structured logging:** Every turn start, completion, tool call, abort, and summarization event is written to the `events` table (existing schema in `controller.py`) with `event_type='loop_turn_start'`, `'loop_turn_complete'`, `'loop_abort'`, `'loop_context_compress'`.
- **Span tracing:** Each turn is traced as a child span in the existing `spans` table (PRD-013), with `name='loop_turn'`, `parent_id` pointing to the loop span, `profile`, `prompt_tokens`, `completion_tokens`.
- **Cost tracking:** Per-turn `cost_usd` is populated from the Hermes response metadata. Cumulative cost is available via `SELECT SUM(cost_usd) FROM loop_runs WHERE loop_id=?`.
- **Live status polling:** `tag loop status <id>` queries the DB and renders the current state; it can be run while the loop is active to see progress.

### 7.4 Security
- See Section 9 for detailed security requirements. At the NFR level: the loop must not escalate filesystem permissions, must not persist API keys in the journal or DB, and must honor the tool allowlist without exception.

---

## 8. Technical Design

### 8.1 New file: `src/tag/loop.py`

Full module outline:

```python
"""
TAG Agent Loop — autonomous multi-turn execution engine (PRD-021).

This module implements the core loop orchestration. It is invoked as a
detached subprocess by cmd_loop in controller.py, exactly as queue_worker.py
is invoked by cmd_queue.

Invocation:
    python -m tag.loop_worker --loop-id LOOP_ID --config CONFIG_PATH --db DB_PATH
"""
from __future__ import annotations

import argparse
import json
import os
import re
import signal
import sqlite3
import subprocess
import sys
import time
import uuid
from pathlib import Path
from typing import Any

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
DEFAULT_MAX_TURNS: int = 25
DEFAULT_TIMEOUT_SECONDS: int = 300
DEFAULT_CONTEXT_COMPRESS_AT: float = 0.70
CHARS_PER_TOKEN_RATIO: int = 4  # conservative estimate for context size calc
COMPLETION_SIGNAL_KEY: str = "tag_loop_done"
COMPLETION_HEURISTIC_PHRASES: list[str] = [
    "task complete",
    "goal accomplished",
    "all done",
    "finished successfully",
    "nothing more to do",
]
JOURNAL_DIR_NAME: str = "loop-journals"
STOP_SIGNAL_INJECTION: str = (
    '\n\n[STOP SIGNAL INSTRUCTION]\n'
    'When you have fully completed the goal, include this exact JSON '
    'anywhere in your response:\n'
    '{"tag_loop_done": true, "summary": "<one-sentence summary>"}\n'
    'Do not include this JSON until the goal is fully accomplished.'
)


# ---------------------------------------------------------------------------
# Dataclasses / types
# ---------------------------------------------------------------------------
class LoopConfig:
    """Parsed configuration for a single loop run."""
    loop_id: str
    profile: str
    goal: str
    max_turns: int
    approve: bool
    approve_tools: list[str]  # empty = no restriction
    cost_limit: float | None
    dry_run: bool
    journal: bool
    no_hooks: bool
    timeout: int
    context_compress_at: float
    system_prefix: str
    parent_loop_id: str | None
    quiet: bool
    json_output: bool


class TurnResult:
    """Result of a single agent turn."""
    turn_number: int
    input_prompt: str
    output_text: str
    tool_calls: list[dict]   # parsed from Hermes JSON output
    blocked_tools: list[str] # tools blocked by allowlist
    tokens_used: int
    cost_usd: float
    status: str              # completed | timeout | error | aborted
    error: str | None


# ---------------------------------------------------------------------------
# DB helpers
# ---------------------------------------------------------------------------
def open_loop_db(db_path: Path) -> sqlite3.Connection:
    """Open the TAG SQLite DB and ensure loop_runs table exists."""
    ...

def create_loop_row(conn: sqlite3.Connection, cfg: LoopConfig) -> None:
    """Insert the initial loop_runs parent row."""
    ...

def write_turn_row(conn: sqlite3.Connection, loop_id: str, result: TurnResult) -> None:
    """Insert or update a turn row in loop_runs."""
    ...

def update_loop_status(conn: sqlite3.Connection, loop_id: str, status: str, summary: str | None = None) -> None:
    """Update the parent loop_runs row status."""
    ...

def load_completed_turns(conn: sqlite3.Connection, loop_id: str) -> list[dict]:
    """Return all completed turn rows ordered by turn_number."""
    ...

def emit_loop_event(conn: sqlite3.Connection, event_type: str, loop_id: str, profile: str, payload: dict) -> None:
    """Write an event row to the events table."""
    ...


# ---------------------------------------------------------------------------
# Context management
# ---------------------------------------------------------------------------
def build_turn_prompt(
    goal: str,
    system_prefix: str,
    completed_turns: list[dict],
    turn_number: int,
) -> str:
    """Assemble the full input prompt for turn N from history + goal."""
    ...

def estimate_context_tokens(turns: list[dict]) -> int:
    """Estimate token count of turn history using CHARS_PER_TOKEN_RATIO."""
    ...

def should_compress(tokens: int, model_context_limit: int, threshold: float) -> bool:
    """Return True if context has crossed the compression threshold."""
    ...

def compress_context(
    cfg_path: str,
    profile: str,
    turns: list[dict],
    compress_ratio: float = 0.5,
) -> list[dict]:
    """
    Summarize oldest (compress_ratio * len(turns)) turns using Hermes.
    Returns a modified turns list where oldest entries have output_text
    replaced with a summary block. Original DB rows are NOT modified.
    """
    ...


# ---------------------------------------------------------------------------
# Completion detection
# ---------------------------------------------------------------------------
def detect_completion(output_text: str) -> tuple[bool, str | None]:
    """
    Scan output_text for tag_loop_done JSON signal.
    Returns (is_complete, summary_or_None).
    Checks last 500 chars first, then full text.
    """
    ...

def detect_completion_heuristic(output_text: str) -> bool:
    """Check last 200 tokens for completion phrases."""
    ...


# ---------------------------------------------------------------------------
# Tool call parsing and allowlist enforcement
# ---------------------------------------------------------------------------
def parse_tool_calls(output_text: str) -> list[dict]:
    """
    Extract tool call JSON blocks from Hermes output.
    Hermes emits tool calls as JSON-in-markdown blocks with a
    known schema: {"tool": "name", "arguments": {...}}.
    Returns list of parsed tool call dicts.
    """
    ...

def filter_tool_calls(
    tool_calls: list[dict],
    allowlist: list[str],
) -> tuple[list[dict], list[str]]:
    """
    Split tool calls into (allowed, blocked_names).
    Returns (allowed_calls, blocked_tool_names).
    """
    ...

def build_tool_block_message(blocked_names: list[str], allowlist: list[str]) -> str:
    """Format the re-prompt message for blocked tools."""
    ...


# ---------------------------------------------------------------------------
# Journal
# ---------------------------------------------------------------------------
def journal_path(db_path: Path, loop_id: str) -> Path:
    """Return path to the loop's markdown journal file."""
    ...

def journal_append_turn(journal_file: Path, turn: TurnResult, turn_number: int) -> None:
    """Atomically append a turn block to the journal markdown file."""
    ...


# ---------------------------------------------------------------------------
# Approval gate
# ---------------------------------------------------------------------------
def approval_gate(turn_number: int, prompt_preview: str) -> bool:
    """
    Print turn preview and ask user y/N.
    Returns True if approved, False if denied.
    Reads from /dev/tty directly (not stdin) to work even if stdin is piped.
    """
    ...


# ---------------------------------------------------------------------------
# Dry run
# ---------------------------------------------------------------------------
def dry_run_forecast(cfg: LoopConfig, system_prompt_tokens: int, model_pricing: dict) -> None:
    """
    Print a turn-by-turn cost projection table using rich.table.Table.
    Does not call Hermes or write to SQLite.
    """
    ...


# ---------------------------------------------------------------------------
# Main loop runner
# ---------------------------------------------------------------------------
def run_loop(cfg: LoopConfig, config_path: str, db_path: str) -> int:
    """
    Main loop execution function.
    Returns exit code: 0 = completed, 1 = failed/aborted/limit_reached.
    """
    ...


# ---------------------------------------------------------------------------
# Entry point (launched as subprocess by controller.py)
# ---------------------------------------------------------------------------
def main() -> int:
    parser = argparse.ArgumentParser(description="TAG loop worker")
    parser.add_argument("--loop-id", required=True)
    parser.add_argument("--config", required=True)
    parser.add_argument("--db", required=True)
    parser.add_argument("--loop-config-json", required=True,
                        help="JSON-encoded LoopConfig fields")
    args = parser.parse_args()
    cfg_dict = json.loads(args.loop_config_json)
    cfg = LoopConfig(**cfg_dict)
    return run_loop(cfg, args.config, args.db)


if __name__ == "__main__":
    sys.exit(main())
```

### 8.2 Schema: `loop_runs` SQLite table

This table stores both the parent loop record and individual turn records, distinguished by the `turn_number` column. Parent row has `turn_number = 0`.

```sql
CREATE TABLE IF NOT EXISTS loop_runs (
    -- Primary key
    id            TEXT PRIMARY KEY,   -- "{loop_id}:{turn_number}" e.g. "abc12345:0", "abc12345:1"

    -- Loop identity
    loop_id       TEXT NOT NULL,      -- shared across all turns of one loop run
    parent_loop_id TEXT,              -- set when this loop was created via 'resume'

    -- Turn metadata
    turn_number   INTEGER NOT NULL,   -- 0 = parent/summary row; 1..N = agent turns
    profile       TEXT NOT NULL,
    goal          TEXT NOT NULL,

    -- Turn content
    input_prompt  TEXT NOT NULL DEFAULT '',
    output_text   TEXT NOT NULL DEFAULT '',
    tool_calls    TEXT NOT NULL DEFAULT '[]',   -- JSON array of {tool, arguments, blocked}
    blocked_tools TEXT NOT NULL DEFAULT '[]',   -- JSON array of blocked tool names

    -- Cost and tokens
    tokens_used   INTEGER NOT NULL DEFAULT 0,   -- total tokens for this turn
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd      REAL NOT NULL DEFAULT 0.0,

    -- Loop-level config (on parent row only, turn_number=0)
    max_turns     INTEGER,
    approve_tools TEXT,    -- JSON array of allowed tool names
    cost_limit    REAL,
    journal       INTEGER, -- 0/1
    no_hooks      INTEGER, -- 0/1
    timeout_secs  INTEGER,

    -- Status
    status        TEXT NOT NULL DEFAULT 'running',
    -- Parent row statuses: running | completed | completed_heuristic |
    --                      aborted | failed | turn_limit_reached | cost_limit_reached | user_cancelled | dry_run
    -- Turn row statuses:   running | completed | timeout | error | aborted | blocked

    error_text    TEXT,    -- populated on error/timeout turns
    summary       TEXT,    -- populated from tag_loop_done JSON on parent row at completion

    -- Timestamps
    created_at    TEXT NOT NULL,
    finished_at   TEXT,
    duration_ms   INTEGER,

    -- Constraints
    UNIQUE(loop_id, turn_number)
);

CREATE INDEX IF NOT EXISTS idx_loop_runs_loop_id   ON loop_runs(loop_id, turn_number);
CREATE INDEX IF NOT EXISTS idx_loop_runs_status    ON loop_runs(status, created_at);
CREATE INDEX IF NOT EXISTS idx_loop_runs_profile   ON loop_runs(profile, created_at);
```

### 8.3 Changed files

#### `controller.py` — new `cmd_loop` function

```python
def cmd_loop(args: argparse.Namespace, cfg: dict[str, Any]) -> int:
    """
    Implements: tag loop, tag loop list, tag loop status, tag loop abort, tag loop resume.
    The subcommand is in args.loop_subcmd.
    """
    db = open_db(cfg)
    label = tag_cli_label()

    if args.loop_subcmd == "list":
        return _loop_list(db, args)
    if args.loop_subcmd == "status":
        return _loop_status(db, args)
    if args.loop_subcmd == "abort":
        return _loop_abort(db, args)
    if args.loop_subcmd == "resume":
        return _loop_resume(db, cfg, args)

    # Default: run a new loop
    if not getattr(args, "profile", None):
        print_error("--profile is required for tag loop")
        return 1
    if not getattr(args, "goal", None):
        print_error("--goal is required for tag loop")
        return 1
    if getattr(args, "approve", False) and not sys.stdin.isatty():
        print_error("--approve requires an interactive TTY")
        return 1

    loop_id = getattr(args, "loop_id", None) or uuid.uuid4().hex[:12]
    loop_cfg_dict = {
        "loop_id": loop_id,
        "profile": args.profile,
        "goal": args.goal,
        "max_turns": getattr(args, "max_turns", 25),
        "approve": getattr(args, "approve", False),
        "approve_tools": [t.strip() for t in (getattr(args, "approve_tools", "") or "").split(",") if t.strip()],
        "cost_limit": getattr(args, "cost_limit", None),
        "dry_run": getattr(args, "dry_run", False),
        "journal": getattr(args, "journal", False),
        "no_hooks": getattr(args, "no_hooks", False),
        "timeout": getattr(args, "timeout", 300),
        "context_compress_at": getattr(args, "context_compress_at", 0.70),
        "system_prefix": getattr(args, "system_prefix", ""),
        "parent_loop_id": getattr(args, "parent_loop_id", None),
        "quiet": getattr(args, "quiet", False),
        "json_output": getattr(args, "json", False),
    }

    if args.dry_run:
        # Import here to avoid circular deps during normal runs
        from tag.loop import dry_run_forecast, LoopConfig
        # ... resolve model pricing, system prompt size, call dry_run_forecast
        return 0

    if args.approve:
        # Foreground: run directly in this process
        from tag.loop import run_loop, LoopConfig
        cfg_obj = LoopConfig(**loop_cfg_dict)
        return run_loop(cfg_obj, str(config_path(None)), str(runtime_db_path(cfg)))

    # Background: launch detached worker process
    pid = _launch_loop_worker(cfg, loop_id, loop_cfg_dict)
    print_success(f"Loop started: {loop_id} (pid {pid})")
    print(f"  tag {label} loop status {loop_id}")
    return 0


def _launch_loop_worker(cfg: dict[str, Any], loop_id: str, loop_cfg_dict: dict) -> int:
    """Launch loop worker as a detached subprocess. Returns PID."""
    cmd = [
        sys.executable,
        "-m", "tag.loop_worker",
        "--loop-id", loop_id,
        "--config", str(config_path(None)),
        "--db", str(runtime_db_path(cfg)),
        "--loop-config-json", json.dumps(loop_cfg_dict),
    ]
    extra_env = os.environ.copy()
    if loop_cfg_dict.get("no_hooks"):
        extra_env["TAG_DISABLE_HOOKS"] = "1"
    proc = subprocess.Popen(
        cmd,
        start_new_session=True,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        close_fds=True,
        env=extra_env,
    )
    return proc.pid
```

#### `controller.py` — argument parser additions

In `build_parser()`, add a new `loop` subparser group:

```python
loop_p = sub.add_parser("loop", help="Run an agent in autonomous multi-turn mode")
loop_sub = loop_p.add_subparsers(dest="loop_subcmd")
# 'list', 'status', 'abort', 'resume' subcommands
loop_sub.add_parser("list", ...)
loop_sub.add_parser("status", ...).add_argument("loop_id")
loop_sub.add_parser("abort", ...).add_argument("loop_id")
loop_sub.add_parser("resume", ...).add_argument("loop_id")

# Default (no subcmd = run)
loop_p.add_argument("--profile", ...)
loop_p.add_argument("--goal", ...)
loop_p.add_argument("--max-turns", type=int, default=25)
loop_p.add_argument("--approve", action="store_true")
loop_p.add_argument("--approve-tools", default="")
loop_p.add_argument("--cost-limit", type=float, default=None)
loop_p.add_argument("--dry-run", action="store_true")
loop_p.add_argument("--journal", action="store_true")
loop_p.add_argument("--no-hooks", action="store_true")
loop_p.add_argument("--loop-id", default=None)
loop_p.add_argument("--parent-loop-id", default=None)
loop_p.add_argument("--quiet", action="store_true")
loop_p.add_argument("--json", action="store_true", dest="json_output")
loop_p.add_argument("--timeout", type=int, default=300)
loop_p.add_argument("--context-compress-at", type=float, default=0.70)
loop_p.add_argument("--system-prefix", default="")
```

#### `queue_worker.py` — no changes required

The existing queue worker is not modified. A loop can enqueue sub-tasks via the existing `queue_insert_job` helper if the agent's tools include a `tag_queue_add` tool, but queue_worker.py itself has no loop-specific logic.

#### `pyproject.toml` — no new dependencies

The loop module uses only stdlib, existing `tag` modules, and existing pinned deps (sqlite3, subprocess, json, signal, pathlib). No new packages are required.

The loop module entry point is added to `[tool.setuptools]` py-modules if `loop_worker` is a top-level module, or it is naturally discovered via `[tool.setuptools.packages.find]` since it lives under `src/tag/`.

### 8.4 Turn orchestration flow

```
cmd_loop()
  │
  ├─ (dry_run) → dry_run_forecast() → exit 0
  │
  ├─ (approve) → run_loop() in foreground process
  │
  └─ (default) → _launch_loop_worker() → detached subprocess
                        │
                        └─ loop_worker.main()
                              │
                              └─ run_loop(cfg, config_path, db_path)
                                    │
                                    ├─ open_loop_db()
                                    ├─ create_loop_row()   ← turn_number=0, status='running'
                                    ├─ register SIGTERM handler
                                    │
                                    └─ for turn_number in 1..max_turns:
                                          │
                                          ├─ check _abort_requested flag → exit if set
                                          ├─ check cumulative cost vs cost_limit → exit if exceeded
                                          │
                                          ├─ load_completed_turns()
                                          ├─ estimate_context_tokens()
                                          ├─ if should_compress() → compress_context()
                                          ├─ build_turn_prompt()
                                          │
                                          ├─ if approve → approval_gate() → exit if denied
                                          │
                                          ├─ write turn row: status='running'
                                          ├─ emit_loop_event('loop_turn_start')
                                          │
                                          ├─ run_profile_hermes(... timeout=timeout)
                                          │     ├─ on TimeoutExpired → write status='timeout', exit loop
                                          │     └─ on success → capture output_text, tokens, cost
                                          │
                                          ├─ parse_tool_calls(output_text)
                                          ├─ filter_tool_calls(tool_calls, approve_tools)
                                          │     └─ if blocked → inject block message into next turn prompt
                                          │
                                          ├─ write turn row: status='completed' + all fields
                                          ├─ emit_loop_event('loop_turn_complete')
                                          ├─ if journal → journal_append_turn()
                                          │
                                          ├─ detect_completion(output_text)
                                          │     └─ if done → update_loop_status('completed') → exit 0
                                          │
                                          └─ (loop continues to turn N+1)
                                    │
                                    └─ (max_turns reached)
                                          ├─ update_loop_status('turn_limit_reached')
                                          ├─ send_desktop_notification()
                                          └─ exit 1
```

---

## 9. Security Considerations

**SEC-01 — Tool Allowlist is Enforced by the Runner, Not the Agent:**  
The tool allowlist is enforced by the loop runner's `filter_tool_calls` function, which post-processes the agent's raw output before any tool execution. The agent cannot bypass this by claiming a tool is approved; the runner's allowlist is the sole authority. The allowlist is persisted in the `loop_runs` parent row and re-validated on resume.

**SEC-02 — Filesystem Access Controls:**  
When `--approve-tools` is set and does not include `bash` or `write_file`, the agent cannot execute shell commands or write to the filesystem. For additional sandboxing, if `TAG_LOOP_CHROOT` is set to a directory path, the worker is launched with `cwd` set to that directory and `HOME` overridden to an isolated runtime home (using the same `hermes_env()` pattern). This is not a true chroot; it is defense-in-depth.

**SEC-03 — API Key Exposure Prevention:**  
The `loop_runs.input_prompt` and `loop_runs.output_text` fields are stored as plain text in SQLite. The DB file permissions follow the existing TAG convention (`chmod 600` set on first creation). Additionally, before writing to `input_prompt`, the runner applies `_sanitize_for_storage(text)` which strips known API key patterns (40-char alphanumeric `sk-...` style strings) with a replacement `[REDACTED_KEY]`. The journal file is written with `0o600` permissions.

**SEC-04 — OWASP LLM Agent Security (OWASP LLM-05 / LLM-07):**  
- **Prompt injection via tool output:** Tool call results returned from the environment (e.g., `read_file` output) could contain adversarial instructions. The loop runner wraps tool outputs in a structured fencing block (`[TOOL RESULT START] ... [TOOL RESULT END]`) that is injected into the agent's next prompt, making injection harder to exploit. This is defense-in-depth, not a complete mitigation; users handling untrusted files should use `--approve-tools` without `read_file` or `bash`.
- **Excessive agency (OWASP LLM-08):** The `--approve` and `--approve-tools` flags directly address excessive agency by requiring human oversight or tool restriction. The default run (no flags) runs unsupervised; the documentation prominently warns of this.

**SEC-05 — Abort Safety (No Partial Tool Execution):**  
When SIGTERM is received, the runner waits for any in-progress `subprocess.run()` Hermes call to complete (up to 5 seconds) before marking the turn aborted. This ensures Hermes finishes or is killed before the loop writes a terminal status. `--force` abort sends `os.kill(pid, signal.SIGKILL)` if the worker does not exit within 5 seconds of SIGTERM.

**SEC-06 — Loop Escape Prevention (Infinite Loop Guard):**  
Beyond `--max-turns`, the loop has a secondary hard limit: if `turn_number` exceeds `max_turns * 2` (which should never happen under normal operation but guards against a corrupted turn counter), the worker immediately exits with status `failed` and logs `"Emergency exit: turn counter exceeded 2x max_turns"`.

**SEC-07 — Credential Exposure in `--approve` Interactive Mode:**  
When `--approve` is active and the turn prompt is printed for user review, the runner calls `_sanitize_for_display(text)` to mask API key patterns before printing to the terminal. This prevents accidental key exposure in terminal scrollback or screen recordings.

**SEC-08 — `loop_id` Injection Prevention:**  
The `loop_id` (whether user-supplied via `--loop-id` or auto-generated) is validated against `re.fullmatch(r'[a-zA-Z0-9_\-]{4,64}', loop_id)`. Rejection with a clear error message prevents path traversal in journal file names (`~/.tag/runtime/loop-journals/<loop-id>.md`) and SQL injection via the `loop_id` parameter (though parameterized queries are used regardless).

**SEC-09 — `--no-hooks` Cannot Be Bypassed by the Agent:**  
The `TAG_DISABLE_HOOKS=1` env var is set in the subprocess environment by the launcher before the worker starts. The agent (running inside Hermes) has no mechanism to unset this var in the loop runner process because they are separate processes.

**SEC-10 — Cost Limit as a Safety Circuit Breaker:**  
Without `--cost-limit`, a loop running into unforeseen recursion or repeated tool failures could incur unbounded cost. The documentation makes `--cost-limit` strongly recommended for any unattended run. A warning is printed at loop start if `--cost-limit` is not set and `--approve` is not active.

---

## 10. Testing Strategy

### 10.1 Unit Tests (`tests/test_loop_unit.py`)

| Test | Description |
|------|-------------|
| `test_detect_completion_valid_json` | Output containing `{"tag_loop_done": true, "summary": "done"}` in last 100 chars → `(True, "done")` |
| `test_detect_completion_buried_json` | Signal buried in middle of long output → detected |
| `test_detect_completion_false_positive_partial` | `{"tag_loop_done": false}` → not detected |
| `test_detect_completion_heuristic_match` | Output ending in "task complete" → heuristic returns True |
| `test_detect_completion_heuristic_no_match` | Normal mid-task output → heuristic returns False |
| `test_filter_tool_calls_all_allowed` | allowlist=["bash"], calls=[{tool:"bash"}] → (all_allowed, no_blocked) |
| `test_filter_tool_calls_partial_block` | allowlist=["bash"], calls=[{tool:"bash"},{tool:"delete_file"}] → (1 allowed, 1 blocked) |
| `test_filter_tool_calls_empty_allowlist` | allowlist=[] → all allowed (empty allowlist = no restriction) |
| `test_build_tool_block_message_format` | Verifies message mentions blocked name and allowed list |
| `test_estimate_context_tokens` | 400 chars of output_text → 100 tokens at ratio 4:1 |
| `test_should_compress_below_threshold` | 6000 tokens, 10000 limit, 0.70 threshold → False |
| `test_should_compress_above_threshold` | 7100 tokens, 10000 limit, 0.70 threshold → True |
| `test_loop_id_validation_valid` | UUID hex 12 chars → passes |
| `test_loop_id_validation_path_traversal` | `"../evil"` → rejected |
| `test_sanitize_for_storage_strips_sk_key` | Input with `sk-abc123...40chars` → replaced with `[REDACTED_KEY]` |
| `test_journal_append_atomic` | Simulates crash during write → journal not corrupted |

### 10.2 Integration Tests (`tests/test_loop_integration.py`)

These tests use a mock Hermes binary (`tests/mocks/hermes_mock.sh`) that outputs deterministic responses.

| Test | Description |
|------|-------------|
| `test_loop_completes_on_signal` | Mock outputs `tag_loop_done: true` on turn 3 → loop exits `completed`, 3 turn rows written |
| `test_loop_max_turns_breach` | Mock never signals done → loop exits after `max_turns=5`, status `turn_limit_reached` |
| `test_loop_abort_mid_turn` | SIGTERM sent to worker while mock sleeps 3s → turn written as `aborted`, parent status `aborted` |
| `test_loop_cost_limit_enforced` | Cumulative cost exceeds `--cost-limit 0.01` after turn 2 → exits `cost_limit_reached` |
| `test_loop_resume_after_abort` | Abort at turn 3, resume → new run starts at turn 4, context includes turns 1-3 |
| `test_loop_tool_allowlist_blocks` | Mock outputs tool call to `delete_file`; allowlist=`["bash"]` → block message injected, extra turn |
| `test_loop_journal_written` | `--journal` flag → journal file exists with all turn blocks after completion |
| `test_loop_no_hooks` | `--no-hooks` → events table has no `loop_turn_complete` rows (hooks disabled) |
| `test_loop_dry_run_no_db_writes` | `--dry-run` → no rows in `loop_runs`, forecast table printed |
| `test_loop_context_compress_triggered` | Inject 100 large turns → compression event emitted at >70% context fill |
| `test_loop_list_filters_by_status` | 3 loops in various states → `--status running` returns only running |
| `test_loop_status_json_output` | `--json` → valid JSON with all expected keys |

### 10.3 Property-Based Tests (`tests/test_loop_property.py`)

Using `hypothesis`:

| Test | Description |
|------|-------------|
| `test_turn_prompt_never_empty` | For any goal + N completed turns, `build_turn_prompt` never returns empty string |
| `test_completion_detection_no_false_positive` | For arbitrary text without `tag_loop_done`, `detect_completion` returns `(False, None)` |
| `test_cost_accumulation_monotonic` | Given N turn results with positive costs, cumulative cost is strictly increasing |
| `test_loop_id_roundtrip` | Any loop_id that passes validation can be used as a filename and DB key without transformation |

### 10.4 Edge Case Test Matrix

| Edge Case | Expected Behavior | Test ID |
|-----------|------------------|---------|
| `max_turns=1`, agent does not signal done | Exit `turn_limit_reached` after 1 turn | `test_single_turn_limit` |
| Agent outputs malformed JSON near `tag_loop_done` | `json.loads` exception caught; heuristic fallback checked | `test_malformed_stop_signal` |
| Goal completion detected on turn 1 | Loop exits immediately with `turns_completed=1` | `test_immediate_completion` |
| Context overflow: total turn output > model limit | Compression fires, loop continues | `test_context_overflow_recovery` |
| Hermes exits with non-zero code on turn 4 | Turn marked `error`, loop status `failed`, subsequent turns not run | `test_hermes_nonzero_exit` |
| `--resume` on a `completed` loop | Error: "Cannot resume a completed loop; use --reset-max-turns to extend" | `test_resume_completed_loop_error` |
| `--approve-tools ""` (empty string) | Treated as no restriction (all tools allowed) | `test_empty_allowlist_no_restriction` |
| `--cost-limit 0` | Error at startup: "cost-limit must be positive" | `test_zero_cost_limit_error` |
| Goal containing SQL metacharacters | All DB writes use parameterized queries; no injection possible | `test_goal_sql_injection` |
| Journal file path with symlink to `/etc` | `journal_path()` resolves to real path and validates it is under `~/.tag/runtime/` | `test_journal_symlink_traversal` |

---

## 11. Acceptance Criteria

**AC-01:** `tag loop --profile coder --goal "hello world"` starts a loop, writes a `loop_runs` parent row with `turn_number=0` and `status='running'`, and launches a detached worker process whose PID is stored in (or retrievable from) the loop record.

**AC-02:** When the agent emits `{"tag_loop_done": true, "summary": "done"}` at any point in its output, the loop exits with `status='completed'` and the `summary` field is persisted in the parent `loop_runs` row.

**AC-03:** A loop started with `--max-turns 5` that never receives a completion signal exits with `status='turn_limit_reached'` after exactly 5 turn rows are written, and a desktop notification is sent.

**AC-04:** `tag loop abort <loop-id>` on a running loop causes the worker to exit within 10 seconds, with the parent `loop_runs` row showing `status='aborted'` and the in-progress turn row showing `status='aborted'`.

**AC-05:** `tag loop resume <loop-id>` on an aborted loop creates a new loop run with `parent_loop_id` set to the original `loop_id`, starts turn numbering at (last_completed_turn + 1), and reconstructs context from all completed turns in the original run.

**AC-06:** `tag loop --approve-tools "bash"` with an agent that calls `delete_file` results in: (a) no `delete_file` execution, (b) a re-prompt injected into the next turn containing the block message, (c) the blocked tool recorded in the turn row's `blocked_tools` field.

**AC-07:** `tag loop --dry-run --max-turns 10` prints a 10-row projection table with columns `Turn | Est. Input Tokens | Est. Output Tokens | Est. Cost (USD)` and exits with code 0 without writing any rows to `loop_runs` and without calling the Hermes binary.

**AC-08:** `tag loop --journal` creates a markdown file at `~/.tag/runtime/loop-journals/<loop-id>.md` with permissions `0o600`, and each turn appended after completion in the specified format, including turn number, timestamp, tokens, cost, tool calls, and output text.

**AC-09:** `tag loop --cost-limit 1.00` aborts the loop before starting turn N if the cumulative `cost_usd` from all previous turns equals or exceeds `1.00`, and the loop exits with `status='cost_limit_reached'`.

**AC-10:** `tag loop list` returns a table with columns `LOOP ID | PROFILE | STATUS | TURNS | COST (USD) | GOAL | CREATED AT` sorted by `created_at DESC`, and `tag loop list --json` returns valid JSON with the same fields.

**AC-11:** `tag loop status <loop-id>` on a running loop shows the current turn number, status of each completed turn, cumulative cost, and the status `running` for the parent row. The command can be run from a different terminal while the loop is in progress.

**AC-12:** When context estimation crosses the `--context-compress-at 0.70` threshold, the `events` table gains a row with `event_type='loop_context_compress'` and the subsequent turn's `input_prompt` is shorter than the full uncompressed history would have been.

---

## 12. Dependencies

### 12.1 Internal PRD dependencies
- **PRD-002 (Cross-Session Memory Journal):** The loop runner reads from `memory_journal` via `journal_to_prompt_prefix()` to inject persistent context into the system prompt prefix of every loop turn. No schema changes required.
- **PRD-003 (Rich Streaming TUI):** `tag loop status` uses `tui_output.py` for table rendering. The loop does not add new TUI primitives in v1; a live streaming panel is deferred.
- **PRD-008 (Background Task Queue):** The loop worker is launched using the same `subprocess.Popen(start_new_session=True)` pattern as `launch_queue_worker`. The patterns stay independent; loops do not use the `queue_jobs` table.
- **PRD-013 (Agent Tracing / Observability):** Each turn is written as a child span in the `spans` table. The loop runner calls `emit_span(...)` helpers from `tracing.py` if available.
- **PRD-018 (Context Window Management):** The loop's `estimate_context_tokens` and `compress_context` functions follow the same model-context-limit discovery pattern defined in PRD-018. If PRD-018 is implemented first, the loop reuses its `get_context_size()` helper directly.

### 12.2 External package dependencies
No new packages required. All needed functionality is covered by:
- `sqlite3` (stdlib) — already used throughout
- `subprocess` (stdlib) — already used for Hermes invocation
- `signal` (stdlib) — already used in `queue_worker.py`
- `json` (stdlib)
- `rich` (already pinned at `14.3.3`) — for dry-run table rendering
- `pathlib` (stdlib)

### 12.3 Infrastructure dependencies
- Hermes binary must be installed and functional (same requirement as all existing `tag submit` flows).
- SQLite WAL mode must be supported (requires SQLite 3.7.0+; macOS and Linux ship >= 3.36 as of 2026).
- The loop worker inherits the same `hermes_env()` environment as `run_profile_hermes()`, so all existing profile API key setup applies.

---

## 13. Open Questions

**OQ-01 — Hermes tool call output format:**  
The `parse_tool_calls` function must extract tool calls from Hermes output text. Does the current Hermes runtime emit tool calls in a stable, parseable JSON format in stdout, or are they interleaved with markdown prose? If the format is not stable, `--approve-tools` filtering cannot be implemented reliably at the runner level. A Hermes `--json-output` flag or structured event stream may be required. **Resolution needed before implementation begins.**

**OQ-02 — Model context limit discovery:**  
`should_compress()` requires the model's declared context limit. This can come from: (a) a hardcoded table in `tag.yaml` (e.g., `models.claude-3-7-sonnet.context_limit: 200000`), (b) a Hermes API introspection call, or (c) a conservative default (32k tokens). Which source is authoritative? If PRD-018 is not yet implemented, option (a) with a default of 32000 is the safe fallback.

**OQ-03 — Resume turn numbering semantics:**  
When resuming, should the resumed run's turns be numbered starting from 1 (reset) or from (last_turn + 1) (continuation)? The PRD currently specifies continuation. However, some users may want to analyze "total turns across all resumes" vs "turns in each segment." An alternative: `tag loop status` shows a `--include-resumptions` flag that flattens the chain.

**OQ-04 — Approval gate in background mode:**  
`--approve` forces foreground execution. This means the user cannot use `--approve` with the async queue-like pattern. Should there be a middle ground: `--approve-turns "1,5,10"` that pauses only at specific turn numbers, allowing background execution between those turns? This would require a mechanism for the background worker to signal back to the terminal, which is non-trivial.

**OQ-05 — Goal completion detection reliability:**  
The `tag_loop_done` JSON signal relies on the model faithfully emitting this exact JSON. In practice, models sometimes paraphrase structured instructions. Should we also support a `--done-marker TEXT` flag that specifies a simpler completion phrase, or should we use a secondary Hermes call to evaluate "did the agent complete its goal?" (adding one extra turn cost per loop). A secondary evaluator turn is more reliable but costs more.

**OQ-06 — Concurrent loops for the same profile:**  
PRD-008 queue prevents concurrent jobs per profile. Should loops have the same constraint? Two loops running on the same profile could collide on `HERMES_HOME` state. The safest default is: one active loop per profile at a time, enforced by checking `loop_runs WHERE profile=? AND status='running'` at launch.

**OQ-07 — Loop-spawned sub-loops and depth limiting:**  
An agent could, in theory, emit a shell command that runs `tag loop ...`, creating a recursive loop tree. Without a depth limit, this could exhaust resources. Should `parent_loop_id` chain depth be limited to, e.g., 3 levels? And should `TAG_LOOP_DEPTH` be propagated as an environment variable to prevent runaway nesting?

---

## 14. Complexity and Timeline

**Complexity Rating: M (Medium)**

Rationale: The loop orchestration logic itself is straightforward iteration over existing `run_profile_hermes` calls. The complexity comes from:
- SIGTERM handling and abort safety (medium)
- Context compression (medium — requires Hermes sub-call for summarization)
- Tool call parsing from unstructured Hermes output (medium-high — depends on OQ-01 resolution)
- Resume context reconstruction (medium)
- Reliable completion detection (medium)

**Sprint Estimate: 2–3 weeks for a senior Python engineer**

| Milestone | Tasks | Days |
|-----------|-------|------|
| **M1: Foundation** | `loop_runs` schema DDL, `open_loop_db()`, `create_loop_row()`, `write_turn_row()`, `update_loop_status()`, `load_completed_turns()` | 2 |
| **M2: Core loop** | `build_turn_prompt()`, `run_loop()` main loop, SIGTERM handler, per-turn Hermes call, `detect_completion()` | 3 |
| **M3: Safety features** | `filter_tool_calls()`, `approval_gate()`, cost limit check, turn limit check | 2 |
| **M4: Context management** | `estimate_context_tokens()`, `should_compress()`, `compress_context()` | 2 |
| **M5: Journal + notifications** | `journal_append_turn()`, `journal_path()`, `send_desktop_notification()` call | 1 |
| **M6: CLI surface** | `cmd_loop()` in `controller.py`, all subcommands (list, status, abort, resume), argparse wiring | 2 |
| **M7: Dry run** | `dry_run_forecast()`, token estimation, pricing table | 1 |
| **M8: Tests** | Unit tests (15), integration tests (12), property tests (4), edge case matrix | 3 |
| **M9: Docs + review** | Update INDEX.md, CLI help text, security review | 1 |
| **Total** | | **17 working days (~3.5 weeks with buffer)** |

**Risk factors that could extend timeline:**
- OQ-01 (Hermes output format for tool calls) requires investigation before M3; if structured output is not available, tool call parsing becomes a significant research item (+3 days).
- OQ-02 (context limit discovery) requires coordination with PRD-018 if that is in flight simultaneously.
- The approval gate interactive TTY handling on Windows (no `/dev/tty` equivalent) may require platform-specific code (+1 day).
