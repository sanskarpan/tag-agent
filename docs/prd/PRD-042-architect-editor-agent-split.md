# PRD-042: Architect/Editor Agent Split (`tag run --architect ... --editor ...`)

**Status:** Proposed
**Priority:** P2 — Medium
**Estimated Effort:** S–M (1 week)
**Category:** AI-Native
**Affects:** `src/tag/controller.py` (flag wiring), `src/tag/split_agent.py` (new), `src/tag/hermes_bridge.py` (dual-profile spawning), profile YAML schema
**Depends on:** PRD-021 (agent loop — required; architect/editor split is a mode of the agent loop)
**Inspired by:** aider `--architect` / `--editor` flags; Claude Code two-step planning for complex edits

---

## 1. Overview

TAG currently runs every agent task through a single model: the same LLM that drafts the high-level plan also writes every line of code, runs searches, and applies diffs. This is convenient but wasteful and fragile. Expensive frontier models are burned on mechanical edit operations; planning mistakes (hallucinated file paths, wrong assumptions about existing APIs) are executed immediately because there is no validation checkpoint between plan and action.

This PRD introduces a two-role execution mode for `tag run`. When `--architect MODEL` and `--editor MODEL` flags are supplied (or their equivalents are set in a profile YAML), TAG spawns two distinct Hermes Agent instances:

1. **Architect agent** — receives the full task context (repo map, memory, conversation history) and produces a structured JSON change specification describing *what* to change and *why*, but does not touch files directly.
2. **Editor agent** — receives only the spec items it needs to execute (one at a time) plus the relevant file contents, and produces actual code edits. It has a deliberately narrow tool grant: file-write and file-read only, no shell execution.

After the editor produces a diff for each spec item, the architect reviews and accepts or rejects it before it is applied to disk. This creates a clean separation of concerns: the powerful model stays at the planning altitude, the fast model does the mechanical work, and a validation gate prevents the editor from drifting away from the plan.

The feature is fully opt-in. All existing `tag run` invocations without `--architect`/`--editor` flags continue to work exactly as before.

---

## 2. Problem Statement

### 2.1 Cost inefficiency of single-model execution

Frontier models (e.g., `claude-opus-4`, `gpt-4o`) cost 5–15× more per token than fast models (e.g., `claude-haiku-4-5`, `gpt-4o-mini`). A typical coding task involves one or two high-level decisions (which files to touch, what refactoring strategy to use) followed by dozens of mechanical edit operations (rename a variable across 20 files, add error handling to each function, reformat docstrings). Using the frontier model for the mechanical work wastes most of the budget.

### 2.2 Plan-then-execute without validation

When a single model both plans and executes, any error in the plan is executed immediately. Common failures include:
- Hallucinated file paths (model mentions `src/auth/utils.py` which does not exist)
- Incorrect API assumptions (model plans to call `session.commit()` but the codebase uses a different ORM pattern)
- Scope creep (model includes speculative changes unrelated to the task prompt)

In single-model mode, these plan errors only surface after the edit is already applied to disk, requiring manual revert.

### 2.3 Mixed-altitude context degrades both roles

When one model context window contains both the high-level architecture discussion and the low-level file-editing operations, each role gets less focused attention. The planning section competes for attention with hundreds of lines of diff output. The editing section is forced to hold the entire repo map in context even for a one-line change.

### 2.4 No isolation between planning and execution tool grants

In single-model mode the agent has shell execution access during both planning and editing. If the model hallucinates a plan that includes a destructive shell command (e.g., `rm -rf build/` as part of a cleanup step), it may execute it directly. Splitting roles allows the editor to have a restricted tool grant (file-write only), limiting the blast radius of any editor-side mistake.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | Add `--architect MODEL` and `--editor MODEL` flags to `tag run`. When both are present, activate split-agent mode. |
| G2 | Architect agent produces a machine-readable JSON change specification before any file is touched. |
| G3 | Editor agent operates on one spec item at a time with only the file content it needs, reducing context size and cost. |
| G4 | Architect reviews each editor diff before it is applied; rejected diffs trigger a retry or escalation. |
| G5 | Architect/editor configuration is expressible per-profile in profile YAML (no flags required for teams with a standard setup). |
| G6 | Split-agent trace entries are compatible with PRD-032 trace format (span hierarchy: `split_task` → `architect_plan` → `editor_edit[]` → `architect_review[]`). |
| G7 | Editor tool grants are scope-limited to file operations; shell execution is not available to the editor model. |
| G8 | All existing `tag run` invocations without split flags are entirely unaffected. |

## 3.1 Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Parallel editor execution (running multiple editor instances concurrently on different spec items). This is a future optimization; the initial implementation is strictly sequential per spec item. |
| NG2 | A dedicated UI for reviewing architect specs before the editor starts. The initial implementation auto-starts the editor; interactive review mode is a future PRD. |
| NG3 | Supporting more than two roles (e.g., researcher + architect + editor). The two-role model is sufficient for the initial use case. |
| NG4 | Automatic model selection based on task complexity. The user or profile must supply both model names explicitly; TAG does not auto-pick models. |
| NG5 | Modifying the hermes Agent runtime internals. TAG wraps Hermes via its existing bridge layer; the split is implemented entirely in TAG's orchestration layer. |
| NG6 | Cost estimation before running. Cost prediction is tracked in PRD-012. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Cost reduction on coding tasks | ≥ 40% lower cost vs. single-model for tasks with ≥ 5 file edits | Compare `prompt_tokens + completion_tokens` cost in TAG traces for matched tasks |
| Plan validation catch rate | Architect reviewer rejects ≥ 70% of editor outputs that deviate from the spec | Evaluated via eval suite (PRD-027) with injected spec deviations |
| Latency overhead | Total wall time ≤ 1.5× single-model wall time on the same task | Benchmark traces |
| User adoption | ≥ 20% of `tag run` invocations in teams using a `coder` profile use split mode within 30 days of release | Telemetry on `--architect` flag presence |
| Zero regressions | All existing `tests/` pass; no new failures in CI on non-split paths | GitHub Actions CI |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|------------|----------|
| U1 | Cost-conscious developer | run `tag run --architect claude-opus-4 --editor claude-haiku-4-5 "refactor auth module"` | I get high-quality planning from the frontier model but pay haiku rates for the mechanical edits |
| U2 | Team lead | set `architect: claude-opus-4` and `editor: claude-haiku-4-5` in my `coder` profile YAML | My whole team uses split mode automatically without memorizing flags |
| U3 | Developer | see the architect's JSON spec before edits are applied | I can catch hallucinated file paths or wrong API assumptions before anything is written to disk |
| U4 | Developer | have the architect validate each editor diff before it lands | Edits that drift from the plan are automatically rejected and retried rather than silently landing wrong |
| U5 | Security-conscious developer | know the editor model cannot execute shell commands | A compromised or confused editor cannot rm files, run curl, or exfiltrate data via subprocesses |
| U6 | Developer debugging a failed run | inspect the full split trace in `tag trace show` | I can see exactly what the architect planned, what the editor produced, and which diffs were accepted or rejected |
| U7 | Developer with large codebase | run split mode on a 50k-line repo | The editor only receives the file snippet it needs per spec item, not the entire repo map |

---

## 6. Proposed CLI Surface

### 6.1 `tag run` new flags

```
tag run [OPTIONS] TASK

Options (new, split-agent mode):
  --architect MODEL    Model ID to use as the architect (planner).
                       When set, --editor must also be set.
  --editor MODEL       Model ID to use as the editor (implementer).
                       When set, --architect must also be set.
  --spec-only          Run the architect and print the JSON spec to stdout,
                       then exit without invoking the editor. Useful for
                       reviewing the plan before committing to edits.
  --editor-retries N   Number of times to retry a failing editor step before
                       escalating to the architect (default: 2).
```

**Examples:**

```sh
# Basic split mode
tag run --profile coder --architect claude-opus-4 --editor claude-haiku-4-5 \
    "refactor auth module to use the new SessionManager API"

# Review the plan only, no edits
tag run --profile coder --architect claude-opus-4 --editor claude-haiku-4-5 \
    --spec-only "add docstrings to all public functions in src/tag/"

# Override retries
tag run --profile coder --architect claude-opus-4 --editor claude-haiku-4-5 \
    --editor-retries 3 "migrate SQLite schema to add indexes"
```

### 6.2 Profile YAML extensions

Profile YAML files (e.g., `~/.config/tag/profiles/coder.yaml`) gain two optional keys:

```yaml
# coder.yaml
model: claude-opus-4          # default single-model (used when split is not active)
architect: claude-opus-4      # split mode: architect model
editor: claude-haiku-4-5      # split mode: editor model
editor_retries: 2             # optional, default 2
```

When `architect` and `editor` are both set in the profile, split mode is activated automatically for all `tag run` invocations using that profile, unless the user explicitly passes `--model` (which disables split mode for that invocation).

**Precedence (highest to lowest):**

1. `--architect` / `--editor` CLI flags
2. `architect` / `editor` keys in the active profile YAML
3. Single-model mode (no split)

### 6.3 `tag run --spec-only` output

When `--spec-only` is used, the architect spec JSON is printed to stdout (not stderr), enabling pipeline use:

```sh
tag run --spec-only --architect claude-opus-4 --editor claude-haiku-4-5 \
    "add logging to all HTTP handlers" | jq '.items[].description'
```

---

## 7. Technical Design

### 7.1 High-level flow

```
tag run --architect A --editor E "TASK"
         │
         ▼
  SplitAgent.run(task)
         │
         ├─► ArchitectAgent.plan(task, context)
         │        └─► produces ChangeSpec (JSON)
         │
         ├─► validate_spec(spec)  [JSON schema check]
         │
         ├─► FOR each spec_item in spec.items:
         │        ├─► EditorAgent.edit(spec_item, file_content)
         │        │        └─► produces FileDiff
         │        │
         │        ├─► ArchitectAgent.review(spec_item, diff)
         │        │        └─► ACCEPT | REJECT(reason)
         │        │
         │        ├─► if REJECT and retries_remaining:
         │        │        └─► EditorAgent.edit(spec_item, file_content, rejection_feedback)
         │        │
         │        ├─► if REJECT and retries_exhausted:
         │        │        └─► ArchitectAgent.escalate(spec_item)  [architect edits directly]
         │        │
         │        └─► apply_diff(diff) to disk
         │
         └─► SplitAgent emit final trace span
```

### 7.2 Architect prompt template

The architect receives a single prompt structured as follows. The system prompt establishes the planning role; the user prompt contains the task, repo context, and schema instructions.

**System prompt:**

```
You are the Architect agent in a two-stage code editing pipeline.

Your role is EXCLUSIVELY to plan. You do NOT write code or edit files directly.

Given a task description and repository context, you will produce a structured
JSON change specification that a separate Editor agent will implement.

Rules:
1. Every file path you mention must exist in the repository context provided.
   Do not invent file paths.
2. Limit the scope to changes that are strictly necessary to complete the task.
   Do not add speculative improvements.
3. Each spec item must describe a single, atomic change to one file.
4. For each spec item, provide a `context_hint` that quotes the specific
   function name, class name, or line range the editor should focus on.
5. Your output must be valid JSON conforming to the ChangeSpec schema.
   Do not include any text outside the JSON object.
```

**User prompt:**

```
## Task
{task}

## Repository context
{repo_map}

## Memory context
{memory_snippets}

## Conversation history (last N turns)
{history}

## Instructions
Produce a ChangeSpec JSON object with the following schema:

{change_spec_schema_json}

Output ONLY the JSON object. No explanation text.
```

### 7.3 ChangeSpec JSON schema

This is the canonical schema against which all architect outputs are validated before the editor receives them. It is defined in `src/tag/split_agent.py` and also stored in `src/tag/schemas/change_spec.json` for reference.

```json
{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "title": "ChangeSpec",
  "type": "object",
  "required": ["task_summary", "rationale", "items"],
  "additionalProperties": false,
  "properties": {
    "task_summary": {
      "type": "string",
      "description": "One-sentence summary of the overall task being performed.",
      "maxLength": 200
    },
    "rationale": {
      "type": "string",
      "description": "2-5 sentence explanation of the approach chosen and why.",
      "maxLength": 1000
    },
    "items": {
      "type": "array",
      "description": "Ordered list of atomic change items. Each item touches exactly one file.",
      "minItems": 1,
      "maxItems": 50,
      "items": {
        "type": "object",
        "required": ["id", "file", "operation", "description", "context_hint"],
        "additionalProperties": false,
        "properties": {
          "id": {
            "type": "string",
            "pattern": "^[a-z0-9_-]{1,32}$",
            "description": "Unique identifier for this spec item within this run."
          },
          "file": {
            "type": "string",
            "description": "Relative path to the file to be changed (must exist in repo).",
            "maxLength": 260
          },
          "operation": {
            "type": "string",
            "enum": ["modify", "create", "delete", "rename"],
            "description": "Type of file operation."
          },
          "description": {
            "type": "string",
            "description": "Plain-English description of the change for this item.",
            "maxLength": 500
          },
          "context_hint": {
            "type": "string",
            "description": "Quote or reference (function name, class, line range) that helps the editor locate the right section of the file.",
            "maxLength": 300
          },
          "rename_to": {
            "type": "string",
            "description": "New path when operation is 'rename'. Required for rename, forbidden otherwise.",
            "maxLength": 260
          },
          "depends_on": {
            "type": "array",
            "description": "List of spec item IDs that must be applied before this one.",
            "items": {"type": "string"},
            "default": []
          }
        }
      }
    }
  }
}
```

**Example architect JSON spec output** (for the task `"refactor auth module to use the new SessionManager API"`):

```json
{
  "task_summary": "Migrate auth module from deprecated Session() calls to SessionManager API.",
  "rationale": "The codebase uses the deprecated direct Session() constructor in three places within src/tag/auth.py. The new SessionManager.get_session() API provides connection pooling and automatic retry. This spec replaces all three call sites, updates the import, and adjusts the corresponding unit test fixture to use a SessionManager mock.",
  "items": [
    {
      "id": "auth_import",
      "file": "src/tag/auth.py",
      "operation": "modify",
      "description": "Replace 'from database import Session' with 'from database import SessionManager' at the top of the file.",
      "context_hint": "Line 4: 'from database import Session'",
      "depends_on": []
    },
    {
      "id": "auth_login",
      "file": "src/tag/auth.py",
      "operation": "modify",
      "description": "In the login() function, replace 'session = Session()' with 'session = SessionManager.get_session()'.",
      "context_hint": "def login(username: str, password: str) -> bool:",
      "depends_on": ["auth_import"]
    },
    {
      "id": "auth_logout",
      "file": "src/tag/auth.py",
      "operation": "modify",
      "description": "In the logout() function, replace 'session = Session()' with 'session = SessionManager.get_session()'.",
      "context_hint": "def logout(token: str) -> None:",
      "depends_on": ["auth_import"]
    },
    {
      "id": "auth_test_fixture",
      "file": "tests/test_auth.py",
      "operation": "modify",
      "description": "Update the @pytest.fixture 'db_session' to patch 'SessionManager.get_session' instead of 'Session'.",
      "context_hint": "@pytest.fixture\ndef db_session():",
      "depends_on": ["auth_login", "auth_logout"]
    }
  ]
}
```

### 7.4 Editor prompt template

For each spec item, the editor receives a focused prompt containing only the spec item and the relevant file content. The editor never sees the full repo map, conversation history, or other spec items.

**System prompt:**

```
You are the Editor agent in a two-stage code editing pipeline.

Your role is EXCLUSIVELY to implement one specific change described in a
ChangeSpec item. You do NOT plan, interpret business requirements, or make
decisions about scope.

Rules:
1. Implement EXACTLY what the spec item describes. Do not add, remove, or
   modify anything beyond what is specified.
2. Your output must be a unified diff (--- a/file +++ b/file format) for the
   file specified in the spec item.
3. Do not produce prose explanations. Output ONLY the diff.
4. If the context_hint does not match the file content provided, output a
   JSON error object: {"error": "context_not_found", "hint": "<context_hint>"}
5. If the file does not need changes to satisfy the spec item, output:
   {"error": "no_change_needed", "reason": "<brief explanation>"}
```

**User prompt:**

```
## Spec item
{spec_item_json}

## File content: {file_path}
```
{file_content}
```

## Instructions
Produce a unified diff that implements the spec item.
Output ONLY the diff. No explanation.
```

### 7.5 Architect review prompt

After the editor produces a diff, the architect reviews it against the original spec item.

**Review prompt (appended to architect context):**

```
## Review request

Spec item:
{spec_item_json}

Editor's diff:
{diff}

Does this diff correctly and completely implement the spec item?

Respond with EXACTLY one of:
  {"verdict": "accept"}
  {"verdict": "reject", "reason": "<one sentence>", "guidance": "<what to do differently>"}

Output ONLY the JSON object.
```

### 7.6 Validation flow

```
architect output
       │
       ▼
  jsonschema.validate(output, CHANGE_SPEC_SCHEMA)
       │
  PASS ─────────────────────────────────────────────►  proceed to editor loop
       │
  FAIL ──► retry architect with schema error message (max 2 retries)
               │
          still FAIL ──► raise ArchitectSpecError, abort run with error message
```

File path validation (after schema validation passes):

```python
for item in spec["items"]:
    if item["operation"] != "create":
        resolved = (repo_root / item["file"]).resolve()
        if not resolved.exists():
            raise ArchitectSpecError(
                f"Spec item '{item['id']}' references non-existent file: {item['file']}"
            )
        if not str(resolved).startswith(str(repo_root)):
            raise ArchitectSpecError(
                f"Spec item '{item['id']}' path escapes repo root: {item['file']}"
            )
```

### 7.7 Error escalation

When the editor fails on a spec item (either produces an error JSON or is rejected by the architect N times), the system escalates to the architect:

```
editor fails spec_item (retries exhausted)
           │
           ▼
  ArchitectAgent.escalate(spec_item, file_content, last_diff, last_rejection)
           │
           ▼
  Architect produces the diff directly (now acting as editor)
           │
           ▼
  Apply diff without further review (architect is self-reviewing)
```

The escalation is logged in the trace with `escalated: true` on the span.

### 7.8 Hermes bridge: spawning two profiles

`src/tag/hermes_bridge.py` gains a `spawn_split_pair` function:

```python
def spawn_split_pair(
    architect_model: str,
    editor_model: str,
    base_profile: dict,
) -> tuple[HermesAgent, HermesAgent]:
    """
    Spawn two Hermes Agent instances from a base profile.

    The architect agent inherits all tools from base_profile.
    The editor agent receives a restricted tool set: read_file, write_file only.
    """
    architect_profile = {**base_profile, "model": architect_model}
    editor_profile = {
        **base_profile,
        "model": editor_model,
        "tools": ["read_file", "write_file"],  # scope-limited
        "max_tokens": base_profile.get("editor_max_tokens", 4096),
    }
    architect = HermesAgent(architect_profile)
    editor = HermesAgent(editor_profile)
    return architect, editor
```

### 7.9 Trace format (PRD-032 compatibility)

Split-agent runs emit a span hierarchy stored in the existing `traces` table:

```
split_task (root span)
├── architect_plan
│   ├── model: claude-opus-4
│   ├── prompt_tokens: N
│   ├── completion_tokens: M
│   └── output: <spec JSON, truncated to 4KB in trace>
├── spec_validation
│   └── status: pass | fail
├── editor_item[auth_import]
│   ├── editor_edit
│   │   ├── model: claude-haiku-4-5
│   │   └── output: <diff>
│   └── architect_review
│       ├── verdict: accept | reject
│       └── reason: <if reject>
├── editor_item[auth_login]
│   └── ...
└── split_summary
    ├── items_total: 4
    ├── items_accepted: 3
    ├── items_escalated: 1
    ├── architect_cost_usd: 0.0042
    └── editor_cost_usd: 0.0008
```

### 7.10 Implementation files

| File | Change |
|------|--------|
| `src/tag/controller.py` | Add `--architect`, `--editor`, `--spec-only`, `--editor-retries` flags to `tag run` argparse; wire to `SplitAgent` when both are set |
| `src/tag/split_agent.py` | New file — `SplitAgent` class, `ChangeSpec` dataclass, `validate_spec()`, `run()`, `_plan()`, `_edit_item()`, `_review()`, `_escalate()` |
| `src/tag/hermes_bridge.py` | Add `spawn_split_pair()` function |
| `src/tag/schemas/change_spec.json` | New file — canonical ChangeSpec JSON schema |
| Profile YAML schema docs | Document `architect`, `editor`, `editor_retries` keys |

---

## 8. Security Considerations

### 8.1 Instruction injection via architect→editor channel

The architect model could, in theory, produce a spec item whose `description` or `context_hint` fields contain injected instructions designed to make the editor perform operations beyond the spec. Mitigations:

1. **JSON schema validation** (`additionalProperties: false`) rejects any architect output that contains unexpected fields. The editor only receives schema-validated spec items.
2. **Field length limits** in the schema (`maxLength: 500` on `description`, `maxLength: 300` on `context_hint`) bound the injection surface.
3. **Editor system prompt** instructs the model to implement exactly the spec item and output only a diff. Any instruction embedded in a spec field that tries to break out of the diff format triggers the error JSON path, which is then handled by retry/escalation logic — not executed.

### 8.2 Path traversal via architect file references

The architect may reference a file path containing `../` components or an absolute path that escapes the repository root. The file path validation in §7.6 resolves each path and asserts it is within `repo_root` before passing it to the editor. An `ArchitectSpecError` is raised and the run aborts if any item fails this check.

### 8.3 Editor tool scope limitation

The editor Hermes agent receives `tools: ["read_file", "write_file"]` only. It cannot execute shell commands, make network requests, or access the system clipboard. This is enforced at the Hermes Agent profile level, not by prompting alone. Even if the editor model is prompted by an injected spec field to run a shell command, the tool is simply not available in its environment.

### 8.4 `rename` and `delete` operations

`rename` and `delete` are included in the spec `operation` enum but carry heightened risk. For the initial release:
- `delete` operations require explicit user confirmation via a `[y/N]` prompt before being applied.
- `rename` operations are implemented as copy-then-delete (with confirmation before the delete step).
- Both are logged with `operation: delete` or `operation: rename` in the trace for auditability.

A future flag `--allow-destructive` can suppress the confirmation prompt for automation contexts.

### 8.5 API key exposure via prompt logging

The architect and editor prompts are stored in the trace table. If the repo context injected into the architect prompt contains API keys or secrets, they will land in the trace. This risk is pre-existing in single-model mode and is tracked in the security notes for PRD-034 (secret scan). No new exposure is introduced by split mode.

---

## 9. Implementation Plan

### Phase 1 — Core split loop (days 1–3)

- [ ] Define `ChangeSpec` dataclass and load JSON schema from `src/tag/schemas/change_spec.json`
- [ ] Implement `validate_spec()` using `jsonschema` (already a dependency via Hermes)
- [ ] Implement `SplitAgent.__init__`, `_plan()`, `_edit_item()`, `_review()` without escalation
- [ ] Add `spawn_split_pair()` to `hermes_bridge.py`
- [ ] Wire `--architect` / `--editor` flags in `controller.py` with basic error if only one is provided
- [ ] Add `--spec-only` flag; print spec JSON and exit
- [ ] Manual integration test: run split mode on a toy task with a local test profile

### Phase 2 — Escalation, retries, tracing (days 4–5)

- [ ] Implement `_escalate()` (architect produces diff directly after editor failure)
- [ ] Add `--editor-retries` flag and retry loop in `_edit_item()`
- [ ] Emit span hierarchy for all phases (plan, edit, review, escalate, summary)
- [ ] Add `split_summary` span with cost attribution
- [ ] Implement file path validation (§7.6) in `validate_spec()`
- [ ] Implement `delete` / `rename` confirmation prompts

### Phase 3 — Profile YAML support and polish (day 6)

- [ ] Add `architect`, `editor`, `editor_retries` to profile YAML schema and loader
- [ ] Implement precedence chain (CLI flags > profile > single-model)
- [ ] Add Rich progress display: show which spec item is being edited and review verdict
- [ ] Add `tag run --spec-only` output formatting (color-coded spec items in terminal)

### Phase 4 — Tests (day 7)

- [ ] `tests/test_split_agent.py`: unit tests for `validate_spec`, path validation, schema rejection
- [ ] `tests/test_split_agent.py`: mock Hermes; test full `SplitAgent.run()` with accept/reject/escalate scenarios
- [ ] `tests/test_hermes_bridge.py`: assert editor profile has restricted tool set
- [ ] `tests/hermes_cli/` integration tests: run split mode against a fixture repo
- [ ] Update CI to run new test modules

---

## 10. Risks

| ID | Risk | Likelihood | Impact | Mitigation |
|----|------|-----------|--------|------------|
| R1 | Architect/editor disagreement loops: architect repeatedly rejects editor output with vague guidance, causing all items to escalate | Medium | High — run cost doubles | Cap total escalations at `max_escalations = len(spec.items)` and abort with a clear error message if exceeded |
| R2 | Architect spec too large for editor context: a spec item's `context_hint` points to a function that requires 10K lines of context to edit | Low | Medium — editor produces wrong diff | File content is truncated to `editor_max_tokens * 0.6` characters when passed to editor; truncation boundary is at a line break; editor spec item gains a `truncated: true` flag when truncation occurs |
| R3 | JSON schema validation false negatives: architect produces a spec that passes schema validation but is semantically wrong (e.g., valid file path that is the wrong file) | Medium | Low — architect review catches most | Architect review step will reject editor diffs that implement changes in the wrong file; file path validation prevents non-existent path references |
| R4 | Performance overhead of two round-trips per spec item: for a 20-item spec, this is 40 LLM calls (20 editor + 20 review) vs. 1 | High — it is by design | Medium | Document the trade-off clearly; provide `--no-review` flag (skip architect review, apply diffs directly) for users who trust the editor; profile YAML can set `review: false` |
| R5 | Editor model too weak for complex items: haiku-class model produces syntactically broken diffs | Medium | Low | Retry logic with rejection feedback catches this; escalation to architect provides fallback |
| R6 | `additionalProperties: false` in JSON schema breaks if architect wraps JSON in markdown code fences | High — common LLM behavior | Medium | Preprocessing step in `validate_spec()`: strip leading/trailing markdown code fences before JSON parsing |

---

## 11. Open Questions

| # | Question | Owner | Status |
|---|----------|-------|--------|
| OQ-1 | Should `--no-review` (skip architect review, trust editor output directly) be included in Phase 1 or deferred? Reduces cost but removes the validation gate. | Product | Open — leaning toward Phase 1 as a power-user escape hatch |
| OQ-2 | How should `depends_on` ordering be enforced? Currently items are applied in array order and `depends_on` is validated for consistency. Should the implementation topologically sort items by `depends_on`? | Engineering | Open — topological sort is a 20-line addition; recommend including in Phase 2 |
| OQ-3 | Should the architect be allowed to amend the spec mid-run (e.g., after seeing an editor failure, decide to change the spec item rather than just provide review feedback)? | Product | Deferred — this is a more complex "plan-act-replan" loop; out of scope for this PRD |
| OQ-4 | What is the correct behavior when `--spec-only` is used in a CI pipeline and the spec contains a `delete` operation? Should it print a warning? | Engineering | Open — recommend printing a `# WARNING: spec contains delete operations` comment before the JSON |
| OQ-5 | Should split-agent mode be usable with `tag loop` (PRD-021 autonomous loop)? The loop would need to feed task results back to the architect. | Product | Open — compatible in principle; needs explicit design for feedback injection |
| OQ-6 | Should the trace store the full architect spec JSON or a truncated version? Full spec could be large (50 items × 500 chars = 25KB). | Engineering | Proposed: store full spec as a separate `spec` table row referenced by trace ID; store only `spec_id` in the span |

---

## 12. Acceptance Criteria

| # | Criterion | How to verify |
|---|-----------|---------------|
| AC-1 | `tag run --architect claude-opus-4 --editor claude-haiku-4-5 "add logging"` produces file edits and exits 0 | Manual test against fixture repo |
| AC-2 | Without `--editor`, `--architect` alone raises a clear error: `"--architect requires --editor to be set"` | `tag run --architect claude-opus-4 "task"` → non-zero exit, error on stderr |
| AC-3 | `--spec-only` prints valid JSON to stdout matching the ChangeSpec schema and makes no file changes | `tag run --spec-only ... | python -m json.tool` succeeds; `git diff` is empty after |
| AC-4 | A spec item referencing a non-existent file raises `ArchitectSpecError` with the file path in the message | Unit test: inject spec with `file: "does/not/exist.py"` |
| AC-5 | A spec item with a path traversal (`../../../etc/passwd`) raises `ArchitectSpecError` | Unit test |
| AC-6 | Editor agent's Hermes profile contains only `read_file` and `write_file` in its tool list | Unit test `test_hermes_bridge.py::test_editor_tool_scope` |
| AC-7 | When editor is rejected twice and retries are exhausted, escalation runs and the spec item is applied via architect diff | Integration test with mocked reject-then-escalate flow |
| AC-8 | `tag trace show <id>` for a split run shows `architect_plan`, `editor_item[]`, and `split_summary` spans | Manual trace inspection |
| AC-9 | Profile YAML with `architect: X` and `editor: Y` activates split mode without CLI flags | Integration test with profile fixture |
| AC-10 | All existing `tag run` tests (non-split) continue to pass | CI green |

---

## 13. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| PRD-021 (agent loop) | Required | Split mode is a mode of the agent loop. `SplitAgent` wraps the same loop infrastructure. Must be merged first. |
| `jsonschema` | Python dep | Already a transitive dependency via Hermes; used for spec validation |
| `src/tag/hermes_bridge.py` | Internal | Must expose `spawn_split_pair()`; requires that the bridge supports per-call tool scope overrides |
| `src/tag/tracing.py` | Internal | `SplitAgent.run()` opens a root span and emits child spans; uses existing span API unchanged |
| PRD-012 (cost tracking) | Complementary | `split_summary` span includes `architect_cost_usd` and `editor_cost_usd` fields; requires that cost fields are already populated on spans by the cost tracking feature |

---

## 14. Complexity and Timeline

**Complexity:** S–M

**Estimated implementation time:** 1 week (5–7 working days)

| Phase | Task | Days |
|-------|------|------|
| 1 | Core split loop (plan, edit, review, spec validation) | 3 |
| 2 | Escalation, retries, trace integration | 2 |
| 3 | Profile YAML support, Rich display polish | 1 |
| 4 | Tests (unit + integration), CI wiring | 1 |
| **Total** | | **7 days** |

The implementation is self-contained in `split_agent.py` and the two new functions in `hermes_bridge.py` and `controller.py`. No schema migrations are required. The primary complexity is in the retry/escalation state machine and ensuring the spec validation is robust against common LLM output formatting quirks (markdown fences, trailing commas).
