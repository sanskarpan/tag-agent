# PRD-052: Prompt Versioning Hub with Terminal Playground (`tag prompt`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (1-2 weeks)
**Category:** Evaluation & Observability
**Affects:** `prompts SQLite table + controller.py`
**Depends on:** PRD-027 (Eval Framework — LLM judge infrastructure), PRD-028 (Sandbox — safe execution context for playground runs), PRD-013 (Agent Tracing/Observability — run IDs for playground sessions), PRD-034 (Secret Scanning — prompt content scanning before storage), PRD-037 (Agent Personas — promotion path overlaps), PRD-030 (Prompt Cache Analytics — cache-control breakpoints in stored prompts)
**Inspired by:** LangSmith prompt hub, Braintrust prompt playground, PromptLayer

---

## 1. Overview

TAG profiles today embed their system prompts directly inside YAML files on disk. When a developer iterates on a system prompt — tightening the persona, adding constraints, restructuring tool-use instructions — there is no structured history, no diff view, no way to test a candidate version against live model output before committing it to a profile, and no mechanism to roll back to a known-good version. The system prompt is simultaneously the most important parameter of an agent and the least observable one.

The Prompt Versioning Hub introduces `tag prompt` as a first-class subcommand family that turns system prompts into versioned, queryable, testable artifacts stored in a dedicated `prompts` SQLite table. Every `tag prompt save` creates an immutable version row with a content hash, author, timestamp, and optional tags. The version history for any named prompt is retrievable at any time, diffs between versions are rendered inline in the terminal, and named versions can be A/B tested deterministically.

The Terminal Playground (`tag prompt play`) is the core value proposition: given a named prompt version and a user message, it invokes the configured model, streams the response to the terminal with Rich formatting, records latency and token usage, and stores the session in a `prompt_runs` table for later analysis. This gives developers a tight feedback loop — edit a prompt, play it, compare outputs, iterate — all inside the terminal without switching to a web UI.

The promotion workflow (`tag prompt promote`) closes the loop between experimentation and production: once a prompt version is proven through playground testing or eval runs (PRD-027), it can be promoted to become the active system prompt of any named profile. Promotion is recorded as a versioned event, so it is always possible to see which prompt version a profile was running at any point in time. This enables confident iteration: try in the playground, validate with evals, promote to production, roll back if needed.

The feature is designed to integrate with TAG's existing infrastructure at every layer: the SQLite WAL-mode database at `~/.tag/runtime/tag.sqlite3` for storage, the Hermes bridge for model invocation in playground mode, the tracing subsystem (PRD-013) for recording playground runs as spans, the eval framework (PRD-027) for structured quality scoring of prompt versions, and secret scanning (PRD-034) for detecting credentials accidentally included in prompt text before they are persisted.

---

## 2. Problem Statement

### 2.1 System Prompts are Unversioned Configuration with Outsized Impact

A system prompt is not source code in the traditional sense — it is a natural language specification that is also executable configuration. Small changes — rephrasing a constraint, reordering bullet points, switching from first-person to second-person instructions — can produce dramatically different model behaviors. Yet TAG currently stores system prompts as a plain string inside a YAML file with no version history, no audit trail, and no regression harness. When a prompt change causes a quality regression, the only recovery path is `git blame` on the profile YAML, which assumes the user is tracking profiles in git and committed with meaningful messages. Most users do not.

### 2.2 The Iteration Cycle Between Editing and Testing is Too Slow

To test a system prompt change today, a developer must: (1) edit the profile YAML, (2) run `tag submit` or `tag run` with a test task, (3) wait for the full agent loop to complete, (4) read the output. There is no way to quickly send a single test message against a draft prompt without committing it to a profile and triggering a full agent run. LangSmith's prompt playground, Braintrust's playground, and PromptLayer all solve this by decoupling prompt testing from agent orchestration. TAG needs the same capability in the terminal, where its users already live.

### 2.3 A/B Testing Prompt Variants is Manual and Non-Deterministic

When a developer wants to compare two prompt variants — e.g., `code-reviewer-v1` (concise feedback) vs `code-reviewer-v2` (structured critique format) — they must manually run both variants, mentally compare outputs, and make a subjective judgment. There is no framework for deterministic A/B assignment, no structured recording of comparison results, and no integration with the eval framework (PRD-027) for objective scoring. The decision to promote a variant to production is therefore intuition-driven rather than data-driven.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | Store system prompts as immutable, versioned rows in a dedicated `prompts` SQLite table, with content hashing to detect duplicate saves. |
| G2 | Provide a terminal playground (`tag prompt play`) that streams model responses to a named prompt version inline, records latency and token usage, and persists the session. |
| G3 | Render unified diffs between any two versions of a named prompt inline in the terminal with syntax highlighting via Rich. |
| G4 | Support deterministic A/B variant routing by hashing an arbitrary key (e.g., user ID or task ID) modulo 100 for consistent assignment. |
| G5 | Promote a validated prompt version to become the active system prompt of any named profile, recording the promotion event. |
| G6 | Integrate with the eval framework (PRD-027) to run a full eval suite against a specific prompt version. |
| G7 | Integrate with secret scanning (PRD-034) to block persistence of prompts containing detected credentials. |
| G8 | Support `--json` on all read-only subcommands for machine-readable output and scripting/CI integration. |
| G9 | Provide rollback: given a profile name, restore the system prompt to any previously promoted version in one command. |
| G10 | Tag and filter prompt versions by arbitrary labels (e.g., `stable`, `experiment`, `v3-candidate`). |

## 3.1 Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Replacing profile YAML as the primary configuration format. Prompts table is a versioning overlay; profiles on disk remain the source of truth until a `promote` is executed. |
| NG2 | Multi-turn conversation management in the playground. The playground runs single-turn (system prompt + one user message); multi-turn is out of scope. |
| NG3 | Cloud sync or team sharing of prompt versions. All storage is local SQLite; a future PRD may introduce a remote prompt registry. |
| NG4 | Fine-tuning or distillation workflows triggered from prompt versions. |
| NG5 | Automatic prompt optimization (e.g., DSPy-style). The hub stores and versions prompts written by humans; it does not modify them. |
| NG6 | Integrating with external prompt hubs (LangSmith Hub, PromptLayer). Import/export via plaintext is sufficient; no API integration with third-party services. |
| NG7 | Prompt templating with variable substitution. Prompts are stored as static strings; Jinja2 rendering is out of scope for this PRD. |

---

## 4. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Save-to-play cycle time | User can save a prompt and receive streamed playground output in under 5 s (excluding model latency) | Automated timing test excluding API call duration |
| Diff render correctness | `tag prompt diff v1 v2` produces output identical to `diff -u` on the two version contents | Property-based test: generate random prompt pairs, compare outputs |
| Version integrity | Content hash matches SHA-256 of stored content for 100% of rows | DB-level assertion in migration test |
| Promotion atomicity | Concurrent `promote` calls for the same profile never produce a split-brain state | Concurrent integration test with 10 parallel promotions |
| Secret scan coverage | 0 prompts containing AWS key patterns, GitHub PATs, or Anthropic API key patterns are persisted to DB | Integration test injecting each pattern type |
| Playground span capture | 100% of playground runs appear as spans in `traces` table with correct `prompt_name` and `prompt_version` attributes | Integration test |
| A/B determinism | Hash-based assignment is identical across 1 000 runs for the same key | Determinism unit test |
| JSON output validity | `tag prompt list --json` output passes `json.loads()` and matches schema for all rows | Schema validation unit test |
| Promotion rollback | `tag prompt rollback --profile P` restores previous system prompt and profile YAML in under 500 ms | Timing + content assertion test |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|--------|-----------|----------|
| U1 | Prompt engineer | run `tag prompt save --name code-reviewer --content "..."` | I can start versioning a prompt that previously only lived in a profile YAML |
| U2 | Prompt engineer | run `tag prompt play --name code-reviewer --input "Review this function"` | I can interactively test the latest version of a prompt without committing it to a profile or running a full agent task |
| U3 | Developer | run `tag prompt diff v1 v3 --name code-reviewer` | I can see exactly what changed between two versions in a unified diff before deciding which to promote |
| U4 | Team lead | run `tag prompt list --name code-reviewer --json` | I can pipe version metadata into a CI script that gates merges on prompt quality scores |
| U5 | Developer | run `tag prompt promote --name code-reviewer --version 3 --profile reviewer` | I can replace the system prompt in the `reviewer` profile with a version I have already validated in the playground |
| U6 | Platform engineer | run `tag prompt rollback --profile reviewer` | I can instantly revert to the previous prompt version when a promotion causes a quality regression |
| U7 | Researcher | run `tag prompt play --name researcher-v2 --input "Summarize this paper" --model anthropic/claude-opus-4` | I can test a prompt against a specific model without changing the profile's default model |
| U8 | Developer | run `tag prompt tag-version --name code-reviewer --version 3 --tag stable` | I can mark a version as stable for teammates (or scripts) to reference by label rather than integer |
| U9 | Evaluator | run `tag prompt eval --name code-reviewer --version 3 --suite evals/review.yaml` | I can score a specific prompt version against a structured eval suite before promoting |
| U10 | Developer | run `tag prompt history --name code-reviewer --last 10` | I can see a time-ordered table of versions with authors, timestamps, and tags |
| U11 | Security engineer | have `tag prompt save` refuse to persist a prompt containing an API key | I can be confident that accidentally copy-pasted credentials do not end up in the local database |
| U12 | Developer | run `tag prompt ab-test --name code-reviewer --variants v2,v3 --key task-id-abc` | I can get a deterministic variant assignment for a given task ID to enable consistent A/B comparison |

---

## 6. Proposed CLI Surface

All prompt subcommands live under the `tag prompt` namespace.

### 6.1 `tag prompt save`

Save a new version of a named prompt. Content can be provided via `--content`, a file path with `--file`, or piped via stdin.

```
tag prompt save \
  --name <name> \
  [--content "You are a senior engineer..."] \
  [--file path/to/prompt.txt] \
  [--description "Added structured output constraint"] \
  [--tag stable,v3-candidate] \
  [--force-duplicate]        # suppress duplicate-hash warning
```

**Output (default):**
```
Saved prompt 'code-reviewer' version 4
  SHA-256: a3f7e2c1...
  Size: 312 tokens (estimated)
  Tags: v3-candidate
  Use 'tag prompt play --name code-reviewer' to test it.
```

**Output (`--json`):**
```json
{
  "name": "code-reviewer",
  "version": 4,
  "content_hash": "a3f7e2c1d9b4e8f0a2c6d1e3f5a7b9c0",
  "description": "Added structured output constraint",
  "tags": ["v3-candidate"],
  "created_at": "2026-06-17T11:42:00Z",
  "size_chars": 892
}
```

**Exit codes:** 0 = saved; 1 = validation error (empty content, secret detected); 2 = duplicate hash (unless `--force-duplicate`).

---

### 6.2 `tag prompt list`

List all versions of a named prompt, or list all prompt names.

```
tag prompt list \
  [--name <name>]            # if omitted, list all prompt names
  [--tag <tag>]              # filter by tag label
  [--json]
  [--last N]                 # show N most recent versions (default: all)
```

**Output (table, single name):**
```
code-reviewer  (4 versions)
  v1  2026-05-01T09:00Z  sha:a1b2c3d4  Initial draft
  v2  2026-05-10T14:23Z  sha:e5f6a7b8  Added tone constraint
  v3  2026-06-01T10:11Z  sha:c9d0e1f2  Structured output section  [stable]
  v4  2026-06-17T11:42Z  sha:a3f7e2c1  Added output constraint     [v3-candidate]
```

**Output (`--json`):**
```json
[
  {"version": 1, "content_hash": "a1b2c3d4", "created_at": "2026-05-01T09:00:00Z",
   "description": "Initial draft", "tags": []},
  {"version": 4, "content_hash": "a3f7e2c1", "created_at": "2026-06-17T11:42:00Z",
   "description": "Added output constraint", "tags": ["v3-candidate"]}
]
```

---

### 6.3 `tag prompt diff`

Render a unified diff between two versions of a named prompt.

```
tag prompt diff <v_from> <v_to> \
  --name <name> \
  [--context N]              # lines of context (default: 3)
  [--json]                   # emit hunks as JSON instead of ANSI diff
  [--no-color]
```

**Output:**
```diff
--- code-reviewer v2 (2026-05-10T14:23Z)
+++ code-reviewer v3 (2026-06-01T10:11Z)
@@ -1,4 +1,7 @@
 You are a senior software engineer reviewing a pull request.
-Be concise. Focus on correctness.
+Be thorough. Focus on correctness first, then style.
+
+Format your review as:
+1. Summary
+2. Critical issues
+3. Suggestions
```

**Exit codes:** 0 = diff produced; 1 = name not found; 2 = version not found; 3 = identical versions.

---

### 6.4 `tag prompt play`

Run a prompt version against a model with a single user input, stream the response inline, and record the session.

```
tag prompt play \
  --name <name> \
  --input "Review this function" \
  [--version N]              # default: latest
  [--model anthropic/claude-sonnet-4-6]  # override profile model
  [--max-tokens 2048]
  [--temperature 0.0]
  [--profile <profile>]      # inherit model + settings from profile
  [--no-stream]              # collect full response before printing
  [--record]                 # save session to prompt_runs table (default: true)
  [--no-record]
  [--json]                   # emit structured response JSON instead of streaming
  [--timeout 60]             # seconds before abandoning model call
```

**Output (streaming, TTY):**
```
Playing prompt 'code-reviewer' v4 against claude-sonnet-4-6
Input: "Review this function..."
────────────────────────────────────────────────────────────
The function has a potential off-by-one error on line 12...
[streams token by token]
────────────────────────────────────────────────────────────
Latency: 1.24 s  |  Input: 312 tok  |  Output: 187 tok  |  Cost: $0.0009
Session recorded: play-run-a1b2c3d4
```

**Output (`--json`):**
```json
{
  "prompt_name": "code-reviewer",
  "prompt_version": 4,
  "model": "anthropic/claude-sonnet-4-6",
  "input": "Review this function",
  "output": "The function has a potential off-by-one error...",
  "latency_ms": 1240,
  "input_tokens": 312,
  "output_tokens": 187,
  "cost_usd": 0.000936,
  "run_id": "play-run-a1b2c3d4",
  "created_at": "2026-06-17T11:50:00Z"
}
```

---

### 6.5 `tag prompt promote`

Promote a specific version of a named prompt to be the active system prompt of a profile.

```
tag prompt promote \
  --name <name> \
  --version N \
  --profile <profile-name> \
  [--dry-run]                # show what would change, do not write
  [--yes]                    # skip confirmation prompt
  [--backup]                 # (default: true) save current profile before overwriting
  [--no-backup]
```

**Output:**
```
Promoting 'code-reviewer' v3 → profile 'reviewer'

Current system prompt (reviewer):
  "You are a code reviewer. Be brief..."  (first 80 chars)

New system prompt (code-reviewer v3):
  "You are a senior software engineer reviewing..."  (first 80 chars)

Profile backup saved: ~/.tag/config/profiles/reviewer.yaml.bak.20260617T115300Z
Profile 'reviewer' updated.
Promotion recorded: promotion-id-9f3a1c2d

Roll back with: tag prompt rollback --profile reviewer
```

**Exit codes:** 0 = promoted; 1 = profile not found; 2 = prompt version not found; 3 = dry-run (always 0 for dry-run).

---

### 6.6 `tag prompt rollback`

Restore a profile's system prompt to the version active before the most recent promotion.

```
tag prompt rollback \
  --profile <profile-name> \
  [--to-promotion <promotion-id>]  # roll back to specific promotion event
  [--dry-run]
  [--yes]
```

**Output:**
```
Rolling back 'reviewer' to system prompt before promotion-id-9f3a1c2d
  Restoring: 'code-reviewer' v2 (promoted 2026-06-01T10:00Z)
Profile 'reviewer' restored.
```

---

### 6.7 `tag prompt history`

Show the version history for a named prompt with metadata.

```
tag prompt history \
  --name <name> \
  [--last N]                 # default: 20
  [--json]
```

---

### 6.8 `tag prompt tag-version`

Apply or remove a label tag on a specific version.

```
tag prompt tag-version \
  --name <name> \
  --version N \
  --tag <label>[,<label2>...] \
  [--remove]                 # remove the tag instead of adding it
```

---

### 6.9 `tag prompt eval`

Run a PRD-027 eval suite against a specific prompt version (temporarily substituting it as the profile's system prompt during the eval run).

```
tag prompt eval \
  --name <name> \
  --version N \
  --suite evals/review.yaml \
  --profile <profile-name> \
  [--judge-model anthropic/claude-sonnet-4-6] \
  [--yes]                    # skip cost confirmation
  [--json]
```

---

### 6.10 `tag prompt ab-test`

Deterministically route to one of two (or more) variant versions based on a hash key.

```
tag prompt ab-test \
  --name <name> \
  --variants v2,v3 \
  --key "task-id-abc" \
  [--json]
```

**Output:**
```
A/B assignment for key 'task-id-abc': code-reviewer v3 (bucket 71/100, variant B)
```

---

### 6.11 `tag prompt show`

Print the full content of a specific version.

```
tag prompt show \
  --name <name> \
  [--version N]              # default: latest
  [--json]
```

---

### 6.12 `tag prompt delete`

Delete a named prompt and all its versions, or a single version.

```
tag prompt delete \
  --name <name> \
  [--version N]              # if omitted, delete all versions
  [--yes]
```

**Constraint:** A version that is currently promoted to a profile cannot be deleted unless `--force` is passed.

---

## 7. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | `tag prompt save` stores prompt content as a UTF-8 string in the `prompts` table with an auto-incrementing per-name version integer, a SHA-256 content hash, a timestamp, and optional description and tags fields. | Must |
| FR-02 | `tag prompt save` computes the SHA-256 hash of the raw content bytes and warns (but does not block by default) if a prompt with the same hash already exists for that name. `--force-duplicate` suppresses the warning and saves a new version anyway. | Must |
| FR-03 | `tag prompt save` invokes the secret scanning subsystem (PRD-034) on the content string before any DB write. If a secret pattern is detected, the save is aborted with exit code 1 and an actionable error message identifying the matched pattern type (not the matched value). | Must |
| FR-04 | `tag prompt list` without `--name` lists all distinct prompt names with their version count and latest version timestamp. With `--name`, lists all versions for that name in ascending version order. | Must |
| FR-05 | `tag prompt list --tag <label>` filters versions to those carrying the specified tag label. | Should |
| FR-06 | `tag prompt diff <v_from> <v_to> --name <name>` fetches the content of both versions and renders a unified diff using Python's `difflib.unified_diff` with the specified `--context` lines (default 3). When stdout is a TTY, green/red ANSI colouring is applied to added/removed lines via Rich. | Must |
| FR-07 | `tag prompt play` fetches the specified (or latest) version content from the `prompts` table, constructs a single-turn Hermes inference request with the content as the system prompt and `--input` as the user message, and streams or collects the response. | Must |
| FR-08 | `tag prompt play` records the session (prompt name, version, model, input, output, latency_ms, input_tokens, output_tokens, cost_usd) in the `prompt_runs` table whether or not `--no-record` is passed, unless `--no-record` is explicitly set. | Must |
| FR-09 | `tag prompt play` emits a tracing span (PRD-013) for the playground run with attributes: `prompt.name`, `prompt.version`, `prompt.run_id`, `llm.model`, `llm.input_tokens`, `llm.output_tokens`. | Should |
| FR-10 | `tag prompt promote` writes the selected version's content string into the profile YAML's `system_prompt` field (or equivalent key), creates a timestamped backup of the profile file at `<profile>.yaml.bak.<ISO8601>`, and inserts a row into the `prompt_promotions` table. | Must |
| FR-11 | `tag prompt promote` is atomic at the SQLite level: the promotion row and the profile file write succeed or both are rolled back. If the file write fails after the DB insert, the DB insert is rolled back. | Must |
| FR-12 | `tag prompt rollback` reads the most recent `prompt_promotions` row for the specified profile (or a specific `--to-promotion` row), retrieves the content of the prior prompt version, and applies it to the profile YAML. If no prior promotion exists, it restores from the `.bak` file and reports this. | Must |
| FR-13 | `tag prompt tag-version` inserts or removes a row in the `prompt_version_tags` table. Adding a tag that already exists on that version is idempotent. | Must |
| FR-14 | `tag prompt eval` constructs a temporary profile override with the specified prompt version as the system prompt, delegates to the PRD-027 eval framework's `cmd_eval_run` internal function, and returns the same exit codes (0 = pass, 2 = threshold failure, 3 = regression). | Should |
| FR-15 | `tag prompt ab-test` computes `bucket = int(hashlib.sha256(key.encode()).hexdigest(), 16) % 100` and selects the variant at index `floor(bucket / (100 / len(variants)))`. The same key always produces the same variant. | Must |
| FR-16 | All read-only subcommands (`list`, `diff`, `history`, `show`, `ab-test`) support `--json` and emit valid JSON to stdout. Error messages go to stderr. `--json` output never mixes prose into stdout. | Must |
| FR-17 | `tag prompt delete --version N` is blocked (exit code 1 with an error) if the version is currently the active system prompt of any profile (as recorded in `prompt_promotions`). `--force` overrides this check and records the deletion. | Must |
| FR-18 | `tag prompt save --file <path>` reads content from the specified file path and behaves identically to `--content` thereafter. Stdin piping is auto-detected when neither `--content` nor `--file` is provided and stdin is not a TTY. | Must |
| FR-19 | `tag prompt history` returns versions in descending order (newest first) with columns: version, created_at (ISO8601), content_hash (first 8 chars), description (truncated to 60 chars), tags. | Should |
| FR-20 | The `prompts` table enforces a UNIQUE constraint on `(name, version)`. The `version` integer is computed as `MAX(version) + 1` for the given `name` inside a transaction with `BEGIN IMMEDIATE` to prevent race conditions. | Must |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | `tag prompt save` latency (excluding secret scan) is under 50 ms on a cold DB connection for content up to 32 KB. | < 50 ms |
| NFR-02 | `tag prompt list` for a name with 1 000 versions returns and renders within 200 ms. | < 200 ms |
| NFR-03 | `tag prompt diff` for two versions with content up to 32 KB each renders within 100 ms (exclusive of TTY render time). | < 100 ms |
| NFR-04 | `tag prompt play` time-to-first-token (after model API call begins) is not measurably increased by prompt retrieval overhead (target: < 20 ms overhead vs. direct model call). | < 20 ms overhead |
| NFR-05 | The SQLite `prompts` table uses WAL mode (inherited from existing DB), and concurrent readers do not block `save` writes. | WAL-mode compliant |
| NFR-06 | Prompt content is stored as plain UTF-8 text. Binary content is rejected at `save` time with an actionable error. Maximum content size is 512 KB (approximately 128k tokens) — larger content is rejected with a clear error. | Max 512 KB |
| NFR-07 | `tag prompt play` respects `--timeout` and cancels the model request cleanly without leaving dangling connections or partial `prompt_runs` rows. Partial rows use a `status` field: `running`, `completed`, `timeout`, `error`. | Timeout-safe |
| NFR-08 | No prompt content is logged to stdout or disk in non-`show` commands — only metadata (hash, version, description). This prevents accidental leakage of prompt IP in CI logs. | Content-safe logging |
| NFR-09 | The `prompt_promotions` table is append-only; no rows are ever UPDATE-d or DELETE-d. This provides an immutable audit trail. | Append-only |
| NFR-10 | `tag prompt delete` is the only mechanism to remove content from the `prompts` table, and it requires explicit `--yes` confirmation. There is no `--all` flag that silently deletes everything. | Explicit deletion |
| NFR-11 | `--json` output on all subcommands uses `json.dumps(ensure_ascii=False, indent=2)` and is deterministically ordered (no dict ordering surprises). | Stable JSON |
| NFR-12 | The module adds no new required third-party dependencies. `difflib` is stdlib; Rich and SQLite are already in TAG's dependency set. The eval integration is conditional on PRD-027 being present. | No new deps |

---

## 9. Technical Design

### 9.1 New Files

| File | Purpose |
|------|---------|
| `src/tag/prompt_hub.py` | Core logic: `PromptVersion`, `PromptRunSession`, `PromptPromotion` dataclasses; DB helpers; diff, play, promote algorithms |
| `src/tag/controller.py` | New `cmd_prompt_*` handler functions and argparse subparsers under `tag prompt` |

No new Python packages are required.

### 9.2 SQLite DDL

All tables are created in the existing database at `~/.tag/runtime/tag.sqlite3` via `open_db()`.

```sql
-- Immutable version rows. Content stored as TEXT (UTF-8).
CREATE TABLE IF NOT EXISTS prompts (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT    NOT NULL,
    version         INTEGER NOT NULL,
    content         TEXT    NOT NULL,
    content_hash    TEXT    NOT NULL,        -- SHA-256 hex digest of content UTF-8 bytes
    size_bytes      INTEGER NOT NULL,
    description     TEXT,
    created_at      TEXT    NOT NULL,        -- ISO8601 UTC e.g. 2026-06-17T11:42:00Z
    created_by      TEXT    DEFAULT NULL,    -- OS username (os.getenv('USER'))
    UNIQUE (name, version)
);

CREATE INDEX IF NOT EXISTS idx_prompts_name ON prompts (name, version DESC);
CREATE INDEX IF NOT EXISTS idx_prompts_hash ON prompts (content_hash);

-- Many-to-many tags on versions.
CREATE TABLE IF NOT EXISTS prompt_version_tags (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    prompt_name     TEXT    NOT NULL,
    prompt_version  INTEGER NOT NULL,
    tag             TEXT    NOT NULL,
    created_at      TEXT    NOT NULL,
    UNIQUE (prompt_name, prompt_version, tag),
    FOREIGN KEY (prompt_name, prompt_version) REFERENCES prompts (name, version)
);

CREATE INDEX IF NOT EXISTS idx_pvtags_name ON prompt_version_tags (prompt_name, tag);

-- Playground run sessions.
CREATE TABLE IF NOT EXISTS prompt_runs (
    id              TEXT    PRIMARY KEY,    -- UUID4, e.g. "play-run-a1b2c3d4"
    prompt_name     TEXT    NOT NULL,
    prompt_version  INTEGER NOT NULL,
    model           TEXT    NOT NULL,
    input           TEXT    NOT NULL,
    output          TEXT,                  -- NULL while status='running'
    status          TEXT    NOT NULL DEFAULT 'running',  -- running|completed|timeout|error
    latency_ms      INTEGER,
    input_tokens    INTEGER,
    output_tokens   INTEGER,
    cost_usd        REAL,
    error_message   TEXT,
    span_id         TEXT,                  -- PRD-013 trace span ID
    created_at      TEXT    NOT NULL,
    completed_at    TEXT,
    FOREIGN KEY (prompt_name, prompt_version) REFERENCES prompts (name, version)
);

CREATE INDEX IF NOT EXISTS idx_pruns_name ON prompt_runs (prompt_name, prompt_version, created_at DESC);

-- Promotion audit trail. Append-only, never updated.
CREATE TABLE IF NOT EXISTS prompt_promotions (
    id              TEXT    PRIMARY KEY,    -- UUID4
    prompt_name     TEXT    NOT NULL,
    prompt_version  INTEGER NOT NULL,
    profile_name    TEXT    NOT NULL,
    promoted_at     TEXT    NOT NULL,
    promoted_by     TEXT,
    prior_prompt_name    TEXT,             -- name of prompt previously in profile (NULL if first promotion)
    prior_prompt_version INTEGER,
    prior_content_hash   TEXT,            -- hash of raw content before promotion (for rollback validation)
    backup_path     TEXT,                  -- absolute path to .bak file
    FOREIGN KEY (prompt_name, prompt_version) REFERENCES prompts (name, version)
);

CREATE INDEX IF NOT EXISTS idx_promotions_profile ON prompt_promotions (profile_name, promoted_at DESC);
```

**Schema migration:** Applied at startup via `open_db()` → `_apply_prompt_hub_migrations(conn)`. Uses `PRAGMA user_version` to gate migration idempotently (increment version from current to N).

---

### 9.3 Core Python Dataclasses

```python
# src/tag/prompt_hub.py
from __future__ import annotations

import difflib
import hashlib
import json
import os
import sqlite3
import time
import uuid
from dataclasses import dataclass, asdict, field
from pathlib import Path
from typing import Optional


@dataclass
class PromptVersion:
    id: int
    name: str
    version: int
    content: str
    content_hash: str
    size_bytes: int
    description: Optional[str]
    created_at: str
    created_by: Optional[str]
    tags: list[str] = field(default_factory=list)

    def to_dict(self, include_content: bool = False) -> dict:
        d = {
            "name": self.name,
            "version": self.version,
            "content_hash": self.content_hash,
            "size_bytes": self.size_bytes,
            "description": self.description,
            "created_at": self.created_at,
            "created_by": self.created_by,
            "tags": self.tags,
        }
        if include_content:
            d["content"] = self.content
        return d


@dataclass
class PromptRunSession:
    id: str                        # UUID4 play-run-<hex>
    prompt_name: str
    prompt_version: int
    model: str
    input: str
    output: Optional[str]
    status: str                    # running | completed | timeout | error
    latency_ms: Optional[int]
    input_tokens: Optional[int]
    output_tokens: Optional[int]
    cost_usd: Optional[float]
    error_message: Optional[str]
    span_id: Optional[str]
    created_at: str
    completed_at: Optional[str]

    def to_dict(self) -> dict:
        return {k: v for k, v in asdict(self).items()}


@dataclass
class PromptPromotion:
    id: str
    prompt_name: str
    prompt_version: int
    profile_name: str
    promoted_at: str
    promoted_by: Optional[str]
    prior_prompt_name: Optional[str]
    prior_prompt_version: Optional[int]
    prior_content_hash: Optional[str]
    backup_path: Optional[str]
```

---

### 9.4 Core Algorithms

#### 9.4.1 Version Assignment (race-safe)

```python
def _next_version(conn: sqlite3.Connection, name: str) -> int:
    """Compute next version inside a BEGIN IMMEDIATE transaction."""
    row = conn.execute(
        "SELECT COALESCE(MAX(version), 0) FROM prompts WHERE name = ?", (name,)
    ).fetchone()
    return (row[0] if row else 0) + 1
```

Caller wraps in `with conn:` (which issues `BEGIN IMMEDIATE` on WAL-mode SQLite), guaranteeing no two concurrent saves for the same name pick the same version integer.

#### 9.4.2 Content Hash

```python
def _sha256_hex(content: str) -> str:
    return hashlib.sha256(content.encode("utf-8")).hexdigest()
```

#### 9.4.3 Diff Rendering

```python
def render_diff(
    old_content: str,
    new_content: str,
    old_label: str,
    new_label: str,
    context: int = 3,
    color: bool = True,
) -> str:
    lines = list(
        difflib.unified_diff(
            old_content.splitlines(keepends=True),
            new_content.splitlines(keepends=True),
            fromfile=old_label,
            tofile=new_label,
            n=context,
        )
    )
    if not color:
        return "".join(lines)
    # Apply ANSI colour: green for additions, red for deletions
    coloured: list[str] = []
    for line in lines:
        if line.startswith("+") and not line.startswith("+++"):
            coloured.append(f"\033[32m{line}\033[0m")
        elif line.startswith("-") and not line.startswith("---"):
            coloured.append(f"\033[31m{line}\033[0m")
        else:
            coloured.append(line)
    return "".join(coloured)
```

#### 9.4.4 A/B Variant Assignment

```python
def ab_assign(key: str, variants: list[str]) -> tuple[str, int]:
    """
    Deterministically assign a key to one of the variants.
    Returns (variant_label, bucket_0_to_99).
    Uses SHA-256 for uniformity; avoids Python hash() which is salted.
    """
    bucket = int(hashlib.sha256(key.encode("utf-8")).hexdigest(), 16) % 100
    n = len(variants)
    idx = min(bucket * n // 100, n - 1)
    return variants[idx], bucket
```

This matches the cluster research pattern: `hash(user_id) % 100` for consistent per-user (or per-task) assignment. Buckets are split evenly; for 2 variants, buckets 0–49 → variant A, 50–99 → variant B.

#### 9.4.5 Promotion Atomicity

The promotion flow must handle the case where the DB insert succeeds but the YAML file write fails. The implementation uses a two-phase approach:

1. Compute the new profile YAML content in memory.
2. Write the `.bak` file to disk (a rename-safe operation).
3. Open a DB transaction with `BEGIN IMMEDIATE`.
4. Insert the `prompt_promotions` row.
5. Write the new YAML file using `Path.write_text()` (atomic on POSIX via `os.replace` after writing to a temp file).
6. Commit the transaction.

If step 5 fails, step 6 is never reached and the DB transaction is rolled back. If the process dies after step 5 but before step 6, the `.bak` file enables recovery via `tag prompt rollback`.

```python
def promote_version(
    conn: sqlite3.Connection,
    pv: PromptVersion,
    profile_name: str,
    profile_path: Path,
    *,
    dry_run: bool = False,
) -> PromptPromotion:
    import tempfile, yaml as _yaml

    # Load existing profile
    raw = _yaml.safe_load(profile_path.read_text(encoding="utf-8")) or {}
    prior_content = raw.get("system_prompt") or raw.get("prompt", {}).get("system", "")
    prior_hash = _sha256_hex(prior_content) if prior_content else None

    # Compute new YAML
    raw["system_prompt"] = pv.content
    new_yaml = _yaml.safe_dump(raw, sort_keys=False, allow_unicode=True)

    if dry_run:
        return _build_dry_run_promotion(pv, profile_name, prior_content, prior_hash)

    # Write backup
    ts = _utc_now().replace(":", "").replace("-", "").replace("T", "T")[:15]
    backup_path = profile_path.with_suffix(f".yaml.bak.{ts}Z")
    backup_path.write_text(profile_path.read_text(encoding="utf-8"), encoding="utf-8")

    promotion_id = f"promo-{uuid.uuid4().hex[:12]}"
    promotion = PromptPromotion(
        id=promotion_id,
        prompt_name=pv.name,
        prompt_version=pv.version,
        profile_name=profile_name,
        promoted_at=_utc_now(),
        promoted_by=os.environ.get("USER"),
        prior_prompt_name=None,  # resolved from promotions table at call site
        prior_prompt_version=None,
        prior_content_hash=prior_hash,
        backup_path=str(backup_path),
    )

    # Atomic: DB insert + file write
    with conn:  # BEGIN IMMEDIATE on WAL-mode
        conn.execute(
            """INSERT INTO prompt_promotions
               (id, prompt_name, prompt_version, profile_name, promoted_at,
                promoted_by, prior_content_hash, backup_path)
               VALUES (?,?,?,?,?,?,?,?)""",
            (promotion.id, promotion.prompt_name, promotion.prompt_version,
             promotion.profile_name, promotion.promoted_at, promotion.promoted_by,
             promotion.prior_content_hash, promotion.backup_path),
        )
        # Write profile atomically via temp file + rename
        tmp = profile_path.with_suffix(".yaml.tmp")
        tmp.write_text(new_yaml, encoding="utf-8")
        os.replace(tmp, profile_path)
        # If os.replace raises, the transaction rolls back

    return promotion
```

---

### 9.5 Playground Model Invocation

`tag prompt play` does not fork a full Hermes agent process. Instead it uses the same Anthropic SDK client already instantiated in `controller.py` (the `hermes_bridge.py` pattern) to make a single `messages.create` call with `stream=True`:

```python
def run_playground(
    pv: PromptVersion,
    user_input: str,
    model: str,
    max_tokens: int = 2048,
    temperature: float = 0.0,
    timeout: int = 60,
    stream: bool = True,
) -> PromptRunSession:
    import anthropic
    client = anthropic.Anthropic()
    run_id = f"play-run-{uuid.uuid4().hex[:8]}"
    started = time.monotonic()
    session = PromptRunSession(
        id=run_id,
        prompt_name=pv.name,
        prompt_version=pv.version,
        model=model,
        input=user_input,
        output=None,
        status="running",
        latency_ms=None,
        input_tokens=None,
        output_tokens=None,
        cost_usd=None,
        error_message=None,
        span_id=None,
        created_at=_utc_now(),
        completed_at=None,
    )
    try:
        with client.messages.stream(
            model=model,
            max_tokens=max_tokens,
            temperature=temperature,
            system=pv.content,
            messages=[{"role": "user", "content": user_input}],
        ) as stream_ctx:
            chunks: list[str] = []
            for text in stream_ctx.text_stream:
                print(text, end="", flush=True)
                chunks.append(text)
            final = stream_ctx.get_final_message()
        elapsed_ms = int((time.monotonic() - started) * 1000)
        session.output = "".join(chunks)
        session.status = "completed"
        session.latency_ms = elapsed_ms
        session.input_tokens = final.usage.input_tokens
        session.output_tokens = final.usage.output_tokens
        session.cost_usd = _estimate_cost(model, final.usage.input_tokens, final.usage.output_tokens)
        session.completed_at = _utc_now()
    except anthropic.APITimeoutError:
        session.status = "timeout"
        session.error_message = f"Model call timed out after {timeout}s"
    except Exception as exc:
        session.status = "error"
        session.error_message = str(exc)
    return session
```

The `_estimate_cost` function uses the `llm_pricing` table (PRD-012/PRD-041) if available, otherwise falls back to hardcoded defaults for known models.

---

### 9.6 Secret Scanning Integration

```python
def _check_for_secrets(content: str) -> list[str]:
    """
    Delegate to PRD-034 security.py scanner.
    Returns list of matched pattern type names (NOT matched strings).
    """
    try:
        from tag.security import scan_text_for_secrets
        return scan_text_for_secrets(content)  # returns ["AWS_ACCESS_KEY", ...]
    except ImportError:
        # PRD-034 not yet available; skip scan with a warning
        return []
```

If `scan_text_for_secrets` returns any hits, `cmd_prompt_save` prints:

```
error: Potential secrets detected in prompt content:
  - AWS_ACCESS_KEY (pattern match at approximate char 145)
  - ANTHROPIC_API_KEY (pattern match at approximate char 312)
Refusing to save. Remove the secrets and retry.
Use --skip-secret-scan to bypass (not recommended).
```

---

### 9.7 Integration with PRD-027 Eval Framework

`tag prompt eval` calls the eval framework's internal API rather than shelling out to `tag eval run`. The integration point:

```python
def run_prompt_eval(
    pv: PromptVersion,
    profile_name: str,
    suite_path: Path,
    judge_model: str,
    yes: bool,
) -> int:  # exit code
    """Temporarily override profile system_prompt and delegate to eval framework."""
    try:
        from tag.eval_framework import run_eval_suite, EvalOverrides
    except ImportError:
        print_error("Eval framework (PRD-027) is not available.")
        return 1
    overrides = EvalOverrides(system_prompt=pv.content)
    return run_eval_suite(
        suite_path=suite_path,
        profile_name=profile_name,
        judge_model=judge_model,
        overrides=overrides,
        yes=yes,
    )
```

This requires PRD-027's `eval_framework.py` to expose `EvalOverrides` — a lightweight dataclass holding optional per-run config overrides. If PRD-027 is not available, `tag prompt eval` fails gracefully with an actionable error.

---

### 9.8 Controller Integration Points

New argparse subparsers added to `controller.py` under `subparsers.add_parser("prompt")`:

```
tag prompt save          → cmd_prompt_save(args, cfg, conn)
tag prompt list          → cmd_prompt_list(args, cfg, conn)
tag prompt show          → cmd_prompt_show(args, cfg, conn)
tag prompt diff          → cmd_prompt_diff(args, cfg, conn)
tag prompt play          → cmd_prompt_play(args, cfg, conn)
tag prompt promote       → cmd_prompt_promote(args, cfg, conn)
tag prompt rollback      → cmd_prompt_rollback(args, cfg, conn)
tag prompt history       → cmd_prompt_history(args, cfg, conn)
tag prompt tag-version   → cmd_prompt_tag_version(args, cfg, conn)
tag prompt eval          → cmd_prompt_eval(args, cfg, conn)
tag prompt ab-test       → cmd_prompt_ab_test(args, cfg, conn)
tag prompt delete        → cmd_prompt_delete(args, cfg, conn)
```

Each `cmd_*` function signature matches the existing pattern in `controller.py`:
- `args`: `argparse.Namespace`
- `cfg`: `dict[str, Any]` loaded from `tag.yaml`
- `conn`: `sqlite3.Connection` from `open_db(runtime_db_path(cfg))`

The main `cmd_prompt(args, cfg)` dispatcher opens the DB and routes to the appropriate sub-handler.

---

### 9.9 Estimated Token Count

The `save` output shows "estimated tokens". Estimation uses:

```python
def _estimate_tokens(content: str) -> int:
    """Conservative estimate: 1 token ≈ 4 characters for English prose."""
    return max(1, len(content) // 4)
```

This is intentionally approximate. For precise counts, the playground run records actual `input_tokens` from the model response.

---

## 10. Security Considerations

1. **Secret scanning before persistence:** All `tag prompt save` calls scan content for credential patterns (PRD-034) before any DB write. The scanned content never appears in logs — only the pattern type name is surfaced in error messages.

2. **Content-safe logging:** Prompt content is never emitted to stdout in subcommands other than `tag prompt show`. All other commands show only metadata (hash, version, description). This prevents prompt IP leakage in CI log outputs.

3. **File path validation for `--file`:** The `--file` argument is resolved to an absolute path and validated with `path.resolve().is_relative_to(Path.cwd())` by default. A `--allow-absolute-path` flag is required to read from outside the working directory, preventing TOCTOU path traversal.

4. **Profile backup before promotion:** Every promotion creates a timestamped `.bak` file before overwriting the profile YAML. The backup path is recorded in `prompt_promotions` for auditability. Backup files are world-readable by default (inherited from profile YAML permissions, typically `0o600`).

5. **Append-only promotions table:** The `prompt_promotions` table is never modified after insert. This provides an immutable audit log for compliance purposes. A future PRD may add an export of this log to JSONL.

6. **SQL injection prevention:** All DB operations use parameterized queries (`?` placeholders). Prompt content, description, tags, and name fields are never interpolated into SQL strings.

7. **Maximum content size enforcement:** Content larger than 512 KB is rejected at the application layer before reaching the DB, preventing accidental storage of binary file contents or extremely large documents.

8. **YAML injection in promotion:** When writing promoted content into a profile YAML, the content is inserted as a Python string value passed to `yaml.safe_dump`. It is never interpolated into a raw YAML string, preventing YAML injection.

9. **Model API key exposure in playground:** `tag prompt play` never logs the `ANTHROPIC_API_KEY` (or equivalent) to stdout, stderr, or the `prompt_runs` table. The Anthropic SDK handles key injection from the environment.

10. **`--skip-secret-scan` audit logging:** If a user bypasses secret scanning with `--skip-secret-scan`, this fact is recorded in the `prompts` row (a `secret_scan_skipped BOOLEAN` column) so the bypass is auditable.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`tests/test_prompt_hub.py`)

| Test | Coverage |
|------|----------|
| `test_sha256_hex_deterministic` | Same content always produces same hash; different content never collides in test set |
| `test_next_version_sequential` | Three sequential saves for same name produce versions 1, 2, 3 |
| `test_next_version_concurrent` | 10 concurrent saves for same name produce distinct versions (uses `threading.Thread`) |
| `test_render_diff_adds_color` | Additions are wrapped in `\033[32m...\033[0m`; removals in `\033[31m...\033[0m` |
| `test_render_diff_no_color` | `color=False` produces plain unified diff matching `difflib.unified_diff` directly |
| `test_ab_assign_deterministic` | 1 000 calls with same key return same variant |
| `test_ab_assign_distribution` | 10 000 random keys produce variants within 5% of equal distribution |
| `test_estimate_tokens` | Content of 400 chars → 100 tokens; edge cases (0 chars, 1 char) |
| `test_secret_scan_blocks_save` | A content string with `AKIA...` pattern causes `cmd_prompt_save` to exit 1 without writing to DB |
| `test_promote_version_dry_run` | `dry_run=True` returns `PromptPromotion` but writes no files and no DB rows |
| `test_build_run_session_fields` | `PromptRunSession.to_dict()` contains all required keys |

### 11.2 Integration Tests (`tests/test_prompt_hub_integration.py`)

These use a temporary SQLite DB created by `open_db(tmp_path / "tag.sqlite3")`.

| Test | Coverage |
|------|----------|
| `test_save_list_show_round_trip` | Save two versions, list returns both in correct order, show returns correct content |
| `test_diff_output_matches_stdlib` | `render_diff` output for two DB-stored versions matches `difflib.unified_diff` on fetched content |
| `test_promote_then_rollback` | Promote v2, verify YAML written; rollback, verify YAML restored to pre-promotion state |
| `test_duplicate_hash_warning` | Saving identical content twice emits warning but creates second row when `--force-duplicate` set |
| `test_delete_blocks_promoted_version` | Attempting to delete a version currently promoted to a profile raises an error with exit code 1 |
| `test_tag_version_idempotent` | Applying same tag twice does not error and does not duplicate the row |
| `test_play_records_to_db` | A mocked Anthropic client call via `unittest.mock` produces a completed `prompt_runs` row with all fields |
| `test_play_timeout_records_status` | A mock that raises `APITimeoutError` produces a `prompt_runs` row with `status='timeout'` |
| `test_promotion_atomicity_file_write_fail` | Mocking `os.replace` to raise `OSError` leaves the DB `prompt_promotions` table unchanged |
| `test_json_output_all_commands` | Each `--json` subcommand produces valid `json.loads`-parseable output |

### 11.3 Performance Tests

| Test | Threshold |
|------|-----------|
| `test_save_latency_under_50ms` | 100 sequential saves each complete in < 50 ms (P99) on CI hardware |
| `test_list_1000_versions_under_200ms` | Insert 1 000 rows for one prompt name; `cmd_prompt_list` completes in < 200 ms |
| `test_diff_32kb_under_100ms` | Two 32 KB content strings; `render_diff` completes in < 100 ms |

### 11.4 CLI Surface Tests (`tests/test_prompt_cli.py`)

Uses `subprocess.run(["tag", "prompt", ...])` against a `TAG_HOME` pointed at a temp directory. Verifies:
- All subcommands produce correct exit codes
- `--json` output parses and contains expected keys
- `--dry-run` on `promote` produces no file writes
- Error messages go to stderr, not stdout

---

## 12. Acceptance Criteria

| ID | Criterion | Verifiable By |
|----|-----------|---------------|
| AC-01 | `tag prompt save --name foo --content "bar"` exits 0 and inserts a row in `prompts` with `name='foo'`, `version=1`, `content='bar'`, and a valid SHA-256 hash. | Integration test + `sqlite3` row assertion |
| AC-02 | A second `tag prompt save --name foo --content "baz"` exits 0 and inserts `version=2` for `name='foo'`. | Integration test |
| AC-03 | `tag prompt save --name foo --content "$(cat file_with_aws_key.txt)"` exits 1, prints error to stderr mentioning `AWS_ACCESS_KEY`, and inserts no row into `prompts`. | Integration test with injected AWS key pattern |
| AC-04 | `tag prompt list --name foo --json` exits 0, stdout is valid JSON, array contains both saved versions in ascending version order. | CLI test + `json.loads` assertion |
| AC-05 | `tag prompt diff 1 2 --name foo` exits 0 and stdout contains a unified diff header with `--- foo v1` and `+++ foo v2`. | CLI test + regex assertion on stdout |
| AC-06 | `tag prompt diff 1 1 --name foo` exits 3 (identical versions). | CLI test + exit code assertion |
| AC-07 | `tag prompt play --name foo --input "hello" --no-record` exits 0 (with mocked model call) and inserts no row into `prompt_runs`. | Integration test with mocked Anthropic client |
| AC-08 | `tag prompt play --name foo --input "hello"` (default `--record`) exits 0 and inserts a `prompt_runs` row with `status='completed'`. | Integration test |
| AC-09 | `tag prompt promote --name foo --version 2 --profile test-profile --yes` exits 0, updates `test-profile.yaml` `system_prompt` field to `v2` content, creates a `.bak` file, and inserts a row into `prompt_promotions`. | Integration test with temp profile YAML |
| AC-10 | `tag prompt rollback --profile test-profile --yes` after AC-09 exits 0, restores `test-profile.yaml` `system_prompt` to the pre-promotion value. | Integration test continuing from AC-09 |
| AC-11 | `tag prompt promote --name foo --version 2 --profile test-profile --dry-run` exits 0, writes no YAML file, inserts no DB row, and prints a diff of the change to stdout. | CLI test |
| AC-12 | `tag prompt delete --name foo --version 2 --yes` after AC-09 exits 1 with an error stating the version is currently promoted. | CLI test |
| AC-13 | `tag prompt ab-test --name foo --variants v1,v2 --key "user-abc"` produces the same output on 100 consecutive invocations. | CLI test |
| AC-14 | `tag prompt tag-version --name foo --version 1 --tag stable` exits 0 and subsequent `tag prompt list --name foo --tag stable --json` returns only `v1`. | CLI test |
| AC-15 | `tag prompt save --name foo --file /etc/passwd` (symlink to sensitive path) is rejected if the resolved path is outside CWD without `--allow-absolute-path`. | Security CLI test |
| AC-16 | `tag prompt list --json` without `--name` returns a JSON array of all prompt names with `version_count` and `latest_version_at` fields. | CLI test + schema assertion |
| AC-17 | Inserting 1 000 versions for one name followed by `tag prompt list --name foo` completes in under 200 ms. | Performance test |
| AC-18 | `tag prompt play --name foo --input "x" --timeout 1` with a mocked model call that blocks for 5 s exits within 2 s with a `status='timeout'` record. | Integration test |

---

## 13. Dependencies

| Dependency | Type | Version Constraint | Notes |
|------------|------|--------------------|-------|
| `sqlite3` (stdlib) | Runtime | Python 3.11+ | WAL mode, `BEGIN IMMEDIATE` |
| `difflib` (stdlib) | Runtime | Python 3.11+ | `unified_diff` for `tag prompt diff` |
| `hashlib` (stdlib) | Runtime | Python 3.11+ | SHA-256 content hashing |
| `anthropic` | Runtime | `>=0.25.0` | Streaming messages API for playground |
| `rich` | Runtime | Already in TAG deps | Diff colouring, table rendering |
| `yaml` (PyYAML) | Runtime | Already in TAG deps | Profile YAML read/write in `promote` |
| `tag.security` (PRD-034) | Runtime optional | Any | Secret scanning; degrades gracefully if absent |
| `tag.eval_framework` (PRD-027) | Runtime optional | Any | `tag prompt eval` delegate; degrades gracefully if absent |
| `tag.tracing` (PRD-013) | Runtime optional | Any | Span emission from playground runs |
| GitHub Issue #343 | Tracking | N/A | Feature request tracking |

---

## 14. Open Questions

| # | Question | Owner | Resolution Target |
|---|----------|-------|------------------|
| OQ-1 | Should `prompts.content` be stored as compressed BLOB (zlib) to reduce DB size for large prompts (>16KB), or plain TEXT? Plain TEXT is simpler to query but may bloat the DB for teams storing hundreds of versions of long prompts. | Engineering | Before implementation start |
| OQ-2 | Should `tag prompt play` support multi-turn conversation (passing a prior `play-run-id` to continue a thread)? This would significantly increase implementation complexity but addresses a real use case. Current scope: single-turn only. | Product | Before M1 |
| OQ-3 | Should the `prompt_promotions` table also record which eval suite run (if any) validated the promotion? This would provide a "validated-before-promote" audit trail but requires tighter coupling to PRD-027. | Architecture | Before M1 |
| OQ-4 | What is the retention policy for `prompt_runs` records? Playground sessions accumulate indefinitely in the current design. Should `tag prompt play` accept a `--no-record` default after N days, or should `tag db vacuum` prune old play sessions? | Engineering | M2 |
| OQ-5 | Should `tag prompt promote` support promoting to multiple profiles in a single invocation (e.g., `--profile reviewer,editor`)? The current design is one-profile-per-command for atomicity simplicity. | Product | Before M1 |
| OQ-6 | Should version labels (e.g., `--tag stable`) be usable as version specifiers in other commands (e.g., `tag prompt play --name foo --version stable`)? This would improve ergonomics but requires tag-to-version resolution in all version-accepting args. | Engineering | M2 |
| OQ-7 | How should `tag prompt ab-test` integrate with `tag run` to enable live A/B routing in production task submissions? A `--ab-name` flag on `tag run` is the most natural surface but is a cross-PRD concern. | Architecture | Future PRD |
| OQ-8 | Should `tag prompt diff` support three-way diffs (common ancestor + two variants) for A/B comparison scenarios? Python stdlib `difflib` does not support three-way diff natively; this would require a third-party library. | Engineering | M2 |

---

## 15. Complexity and Timeline

### Phase Overview

| Phase | Name | Duration | Deliverables |
|-------|------|----------|-------------|
| M1 | Core Storage and CLI | Days 1–4 | SQLite schema migration, `prompt_hub.py` dataclasses and DB helpers, `save`, `list`, `show`, `history`, `diff`, `delete`, `tag-version` subcommands, unit + integration tests for all M1 commands |
| M2 | Playground | Days 5–7 | `cmd_prompt_play` with streaming, `prompt_runs` table, timeout handling, tracing span emission (PRD-013), `--json` output, performance tests |
| M3 | Promotion and Rollback | Days 8–10 | `cmd_prompt_promote` with atomic file write, `.bak` creation, `prompt_promotions` table, `cmd_prompt_rollback`, promotion atomicity test, security path validation |
| M4 | A/B Testing and Eval Integration | Days 11–12 | `cmd_prompt_ab_test`, `cmd_prompt_eval` delegate to PRD-027, secret scanning integration (PRD-034), `--skip-secret-scan` audit logging, full CLI surface tests |
| M5 | Polish and Acceptance | Days 13–14 | All AC assertions pass, performance regression tests, documentation in `docs/prompt-hub.md`, `tag prompt --help` copy review, edge case hardening (Unicode content, very large content rejection) |

### Complexity Notes

- **M1** is straightforward SQLite CRUD with well-understood patterns from `controller.py`. The main complexity is the race-safe version assignment, which is solved by `BEGIN IMMEDIATE`.
- **M2** (playground) has the highest interaction surface: streaming, timeout, token counting, cost estimation, and span emission. Mocking the Anthropic client in tests requires careful `unittest.mock.patch` setup.
- **M3** (promotion) carries the most correctness risk due to the atomicity requirement. The `os.replace` + transaction pattern is well-established on POSIX but requires an explicit test for the failure-mid-write case.
- **M4** integration dependencies (PRD-027, PRD-034) are soft — both degrade gracefully — so M4 can be developed in parallel with M3 if needed.
- **Total estimated effort:** 14 engineering days (within the M estimate of 1–2 weeks for a 2-person team or ~3 weeks for a solo engineer).

---

*GitHub Issue: #343*

