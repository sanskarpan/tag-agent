# PRD-032: Agent Replay / Time-Travel Debugging (`tag trace replay`)

**Status:** Proposed
**Priority:** P2 Medium
**Estimated Effort:** L (2 sprints, ~4 weeks)
**Category:** Observability
**Affects:** `src/tag/tracing.py` (snapshot capture), `src/tag/controller.py` (replay/diff commands), new `src/tag/replay.py`
**Depends on:** PRD-013 (distributed tracing infrastructure — spans table, trace_id propagation)

---

## 1. Overview

When an agent run produces incorrect output or fails midway, reproducing the exact failure requires re-running the entire task from scratch, often against live, expensive, or non-deterministic external APIs. There is no mechanism in TAG today to "step back in time" to inspect what the agent actually saw, what tools it called, what arguments it passed, and what results it received.

Agent Replay / Time-Travel Debugging adds a first-class replay system that captures a complete snapshot of every agent run — inputs, tool call arguments, tool results, model outputs, token counts, and timestamps — and stores it in the existing SQLite database. Any past run can be replayed at full speed or stepped through interactively, with the option to fork the run from any mid-run checkpoint. Two runs can be diffed to understand why the same prompt produced different outputs.

This is the observability primitive that makes TAG's agent runs auditable, debuggable, and reproducible. It fills the gap between "logs tell you what happened" and "you can actually re-examine it."

---

## 2. Problem Statement

### 2.1 The Debugging Gap

TAG runs agents against OpenRouter-backed models via the Hermes runtime. Each run involves:

- A model receiving a system prompt + user task
- The model emitting tool calls
- TAG executing tools (shell commands, file reads, API calls)
- Tool results fed back to the model
- This loop repeating until the task completes or fails

When something goes wrong — bad output, an unexpected tool call, a mid-run crash — the engineer has only:

1. The final output file at `~/.tag/runtime/queue-results/<job_id>.md`
2. Whatever the Rich TUI printed to stdout at runtime
3. The spans table in SQLite (if PRD-013 is implemented), which has timing and metadata but not full input/output payloads

There is no way to:
- Re-examine the exact prompt the model received
- See what arguments the model passed to a tool
- Inspect the raw tool result that was fed back to the model
- Replay the run without hitting the live model again (at cost)
- Start a new run from step 7 instead of step 1

### 2.2 Cost and Non-Reproducibility

Re-running a failed 40-step coding agent to observe step 23 is expensive (tokens) and non-reproducible (model outputs vary, external APIs change). Engineers often resort to adding print statements, which makes the code fragile and the debugging ad-hoc.

### 2.3 No Run Comparison Capability

Two runs of the same task frequently produce different outcomes. Today there is no structured way to ask: "run A produced a correct implementation; run B produced a broken one — what was different?" The diff would need to span tool call arguments, model outputs, and token consumption simultaneously.

### 2.4 No Checkpoint/Resume for Long Runs

A 2-hour coding agent that fails at step 45 out of 50 must be restarted from scratch. There is no way to resume from a known-good intermediate state or fork from a checkpoint to try a different approach for the remaining steps.

---

## 3. Goals

1. **Complete run capture:** Every tool call's input arguments and output results, every model request/response, and every span event are stored to SQLite during the run. Storage is opt-in per run but on-by-default when tracing is enabled.
2. **Full replay:** `tag trace replay <trace_id>` feeds the stored model inputs back through the agent executor without hitting the live model, producing the same sequence of events against recorded tool results.
3. **Interactive step-through:** `--step-mode` pauses replay after each tool call, showing the tool name, arguments, and result, allowing the engineer to inspect state and optionally edit tool results before continuing.
4. **Run diffing:** `tag trace diff <trace_id_a> <trace_id_b>` produces a structured diff of tool calls, model outputs, and final results across two runs of comparable tasks.
5. **Checkpoint fork:** `tag trace checkpoint <trace_id> --step N` creates a new run initialized with the state snapshot from step N of the original run, allowing a fresh model call to continue from that point.
6. **Storage lifecycle management:** Replay snapshots have a configurable TTL (default: 30 days) and are pruned automatically. Large traces are compressed.
7. **Secret redaction:** Tool results are scanned for high-entropy strings, API key patterns, and environment variable names before storage. Redacted fields are marked but not stored.

## 4. Non-Goals

- **Exact model output reproduction:** Replay does not guarantee the model produces the same output as the original run. Model outputs are inherently stochastic. Replay reuses stored model responses as defaults but can optionally call the live model.
- **Distributed replay across multiple machines:** Replay operates on the local SQLite database. Cross-machine trace sharing is out of scope (see PRD-013's future OTLP export work).
- **Visual GUI replay:** Replay output is CLI-first (Rich TUI panels). A browser-based session replay UI (like AgentOps' timeline view) is out of scope for this PRD.
- **Replay of tool side effects:** Replay does not re-execute destructive tool calls (file writes, shell commands, API calls). It feeds stored results back. Replaying actual side effects is intentionally blocked.
- **Cross-version replay:** If the tool schema or prompt template changes between the original run and the replay, divergence warnings are emitted but the replay is not blocked.

---

## 5. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Time to inspect a failed run | < 2 minutes from `tag trace replay` to viewing the failing tool call | Manual timing in user testing |
| Storage overhead per run (median coding task) | < 500 KB per run with compression | SQLite `ANALYZE` + file size checks |
| Replay fidelity (tool calls match original) | 100% match for deterministic tools | Automated test suite comparing replayed event sequence to stored sequence |
| Divergence false-positive rate | < 5% of replays emit spurious divergence warnings | Controlled test with known-deterministic tools |
| P95 replay startup latency | < 500ms from command to first event output | Benchmark against 50-step trace |
| Secret detection coverage | Detects AWS keys, GH tokens, OpenAI keys in tool results | Unit tests with synthetic secrets |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag trace replay <trace_id>` and watch the run replay in my terminal | I can see exactly what happened without re-running the expensive agent |
| U2 | Developer | run `tag trace replay <trace_id> --step-mode` and pause at each tool call | I can inspect the exact arguments passed to `run_shell` at step 7 before it went wrong |
| U3 | Developer | run `tag trace diff abc123 def456` | I can see that run B used a different file path at step 3, which caused the divergence |
| U4 | Operator | run `tag trace checkpoint abc123 --step 12` to fork a new run | I can try a different model for the remaining steps without paying for the first 12 steps again |
| U5 | Developer | run `tag trace list --failed --last 7d` | I get a table of all failed runs in the past week with their trace IDs, ready to replay |
| U6 | Security engineer | know that `tag trace replay` redacts secrets in stored tool results | I can share trace IDs with colleagues without fear of leaking credentials |
| U7 | Developer | run `tag trace show <trace_id>` to see a Rich tree of all tool calls and durations | I get a quick visual overview of the run before deciding to step through it |
| U8 | Operator | configure `replay.snapshot_ttl_days = 7` in `cli-config.yaml` | Old traces don't accumulate and fill my disk |
| U9 | Developer | run `tag trace replay <trace_id> --from-step 15 --live-model` | I can resume a partial run from step 15 using the live model, skipping the already-correct steps 1–14 |

---

## 7. Technical Design

### 7.1 Data Model

#### 7.1.1 New Table: `trace_snapshots`

Added to the existing `~/.tag/tag.sqlite3` database alongside the `spans` table from PRD-013.

```sql
CREATE TABLE IF NOT EXISTS trace_snapshots (
    id            TEXT PRIMARY KEY,          -- UUID, snapshot_id
    trace_id      TEXT NOT NULL,             -- FK → spans.trace_id
    step_index    INTEGER NOT NULL,          -- 0-based position in the run
    event_type    TEXT NOT NULL,             -- 'model_request' | 'model_response' | 'tool_call' | 'tool_result' | 'run_start' | 'run_end'
    tool_name     TEXT,                      -- NULL for model events
    input_payload TEXT,                      -- JSON blob: full input (prompt, tool args, etc.)
    output_payload TEXT,                     -- JSON blob: full output (model response, tool result)
    token_count   INTEGER,                   -- prompt+completion tokens for model events
    duration_ms   INTEGER,                   -- wall-clock time for this step
    has_redaction INTEGER NOT NULL DEFAULT 0, -- 1 if any field was redacted
    redaction_log TEXT,                      -- JSON list of redacted field paths
    created_at    TEXT NOT NULL,             -- ISO-8601 UTC
    FOREIGN KEY (trace_id) REFERENCES spans(trace_id)
);

CREATE INDEX IF NOT EXISTS idx_snapshots_trace_id ON trace_snapshots(trace_id, step_index);
CREATE INDEX IF NOT EXISTS idx_snapshots_event_type ON trace_snapshots(event_type);
```

#### 7.1.2 Schema Migration for `trace_snapshots`

A migration is applied at startup via the existing `_ensure_schema()` pattern in `controller.py`. The migration is versioned and idempotent:

```sql
-- migration 004: add trace_snapshots
CREATE TABLE IF NOT EXISTS trace_snapshots ( ... );
INSERT OR IGNORE INTO schema_migrations(version, applied_at) VALUES (4, datetime('now'));
```

#### 7.1.3 Additions to `queue_jobs` Table

Two new columns are added to the existing `queue_jobs` table:

```sql
ALTER TABLE queue_jobs ADD COLUMN snapshot_enabled INTEGER NOT NULL DEFAULT 1;
ALTER TABLE queue_jobs ADD COLUMN snapshot_size_bytes INTEGER;
```

#### 7.1.4 New Table: `trace_checkpoints`

```sql
CREATE TABLE IF NOT EXISTS trace_checkpoints (
    id             TEXT PRIMARY KEY,           -- UUID
    source_trace_id TEXT NOT NULL,             -- original trace being forked
    fork_step      INTEGER NOT NULL,           -- step N from which this checkpoint forks
    derived_job_id TEXT,                       -- queue_jobs.id of the new run, set after fork
    created_at     TEXT NOT NULL
);
```

### 7.2 Snapshot Capture: `src/tag/tracing.py`

The existing `tracing.py` module (PRD-013) is extended with a `SnapshotRecorder` class:

```python
class SnapshotRecorder:
    """Records trace events as full-payload snapshots for replay."""

    def __init__(self, trace_id: str, db_path: Path, redact: bool = True):
        self.trace_id = trace_id
        self.db_path = db_path
        self.redact = redact
        self._step = 0
        self._redactor = SecretRedactor() if redact else None

    def record(
        self,
        event_type: str,
        *,
        tool_name: str | None = None,
        input_payload: Any,
        output_payload: Any,
        token_count: int | None = None,
        duration_ms: int | None = None,
    ) -> str:
        """Persists one snapshot row. Returns the snapshot_id."""
        ...

    def close(self) -> None:
        """Flush and compute total snapshot_size_bytes on queue_jobs."""
        ...
```

The `SnapshotRecorder` is instantiated once per run in `controller.py`'s dispatch path and passed into the Hermes execution wrapper. Every tool call invocation and every model API call wraps a `record()` call.

### 7.3 Secret Redaction: `SecretRedactor`

`SecretRedactor` is a class in `src/tag/tracing.py` that scans JSON payloads for:

- **High-entropy strings:** Shannon entropy > 4.5 bits/char for strings ≥ 20 characters
- **Pattern matching:** AWS access keys (`AKIA[0-9A-Z]{16}`), GitHub tokens (`ghp_`, `ghs_`, `github_pat_`), OpenAI keys (`sk-`), generic bearer tokens, private key PEM headers
- **Environment variable names:** any key whose name contains `KEY`, `SECRET`, `TOKEN`, `PASSWORD`, `CREDENTIAL`, `AUTH` (case-insensitive)

Matched values are replaced with `"[REDACTED:<type>:<sha256-prefix-8>]"`. The original field path and redaction type are recorded in `redaction_log`. The redacted value itself is never stored.

### 7.4 Replay Engine: `src/tag/replay.py`

New module. Core class:

```python
class TraceReplayer:
    """Feeds stored trace snapshots back through the agent loop."""

    def __init__(
        self,
        trace_id: str,
        db_path: Path,
        *,
        step_mode: bool = False,
        from_step: int = 0,
        live_model: bool = False,
        console: Console | None = None,
    ):
        ...

    def run(self) -> ReplayResult:
        """Execute the replay. Returns a ReplayResult with divergence info."""
        ...

    def _replay_model_request(self, snapshot: dict) -> ModelResponse:
        """Return stored model response or call live model if live_model=True."""
        ...

    def _replay_tool_call(self, snapshot: dict) -> ToolResult:
        """Return stored tool result. Never re-executes destructive tools."""
        ...

    def _check_divergence(
        self, stored: dict, actual: dict, step: int
    ) -> list[DivergenceEvent]:
        """Compare stored vs actual payload. Returns list of divergence events."""
        ...
```

#### 7.4.1 Step Mode Interaction

When `--step-mode` is active, after each tool call snapshot the replayer pauses and presents a Rich panel:

```
┌─ Step 7 / 42 ─────────────────────────────────────────────────────┐
│ Tool:     run_shell                                                 │
│ Input:    {"command": "pytest tests/ -x"}                          │
│ Result:   {"exit_code": 1, "stdout": "FAILED tests/test_api.py"}   │
│ Duration: 4,231 ms                                                  │
└────────────────────────────────────────────────────────────────────┘
[n]ext  [e]dit result  [s]kip  [l]ive  [q]uit
```

The engineer can edit the stored result in `$EDITOR` before continuing, allowing "what if" exploration.

#### 7.4.2 Divergence Detection

When replaying against a live model (`--live-model`), the replayer tracks divergence: any model output that differs from the stored output at the same step. Divergence events are emitted as warnings and accumulated in `ReplayResult.divergences`. At the end, a summary panel shows the divergence rate.

### 7.5 Diff Engine

`tag trace diff <trace_id_a> <trace_id_b>` calls `TraceDiffer` in `replay.py`:

```python
class TraceDiffer:
    def diff(self, trace_a: str, trace_b: str) -> TraceDiff:
        """
        Aligns steps by event_type+tool_name sequence using Myers diff algorithm.
        Returns a TraceDiff with added, removed, changed, and identical steps.
        """
        ...
```

Output is a Rich table:

```
Step  Event           Run A                          Run B
────  ──────────────  ─────────────────────────────  ─────────────────────────────
1     model_request   [identical]                    [identical]
2     tool_call       run_shell: "make build"        run_shell: "make test"  ← DIFF
3     tool_result     exit_code=0                    exit_code=1             ← DIFF
4     model_request   [+32 tokens in B]              [+32 tokens in B]
5     run_end         success                        failed                  ← DIFF
```

### 7.6 Checkpoint Fork

`tag trace checkpoint <trace_id> --step N` creates a new `queue_jobs` row with:

- `task` = same task as the original
- `profile` = same profile as the original
- A special `checkpoint_snapshot_id` field pointing to step N of the source trace
- Status = `pending`

When the new job's `queue_worker.py` starts, it detects `checkpoint_snapshot_id`, loads all snapshots up to step N from the source trace, and feeds them into the executor as if they had just been produced, then hands off to the live model for step N+1 onward.

### 7.7 CLI Surface in `controller.py`

All subcommands hang off the existing `tag trace` subparser:

```
tag trace list [--failed] [--last N] [--last Nd]
tag trace show <trace_id>
tag trace replay <trace_id> [--step-mode] [--from-step N] [--live-model] [--no-redact]
tag trace diff <trace_id_a> <trace_id_b> [--format table|json|unified]
tag trace checkpoint <trace_id> --step N [--profile PROFILE]
tag trace prune [--older-than Nd] [--dry-run]
tag trace export <trace_id> --out FILE [--format json|jsonl]
```

Implementation pattern for `tag trace replay`:

```python
def cmd_trace_replay(args: argparse.Namespace, cfg: dict, db_path: Path) -> int:
    from tag.replay import TraceReplayer
    replayer = TraceReplayer(
        trace_id=args.trace_id,
        db_path=db_path,
        step_mode=args.step_mode,
        from_step=args.from_step or 0,
        live_model=args.live_model,
        console=get_console(),
    )
    result = replayer.run()
    if result.divergences:
        print_warning(f"{len(result.divergences)} divergence(s) detected")
    return 0 if result.success else 1
```

### 7.8 Storage Compression

For traces with `snapshot_size_bytes > 100 KB`, `output_payload` fields for `tool_result` events are stored as zlib-compressed base64 blobs. The `SnapshotRecorder` compresses automatically. The `TraceReplayer` decompresses transparently.

### 7.9 Pruning and Lifecycle

The existing `tag doctor` and a new `tag trace prune` command manage snapshot lifecycle:

- Default TTL: 30 days (configurable via `replay.snapshot_ttl_days` in `cli-config.yaml`)
- `tag trace prune --older-than 30d` deletes `trace_snapshots` rows and updates `queue_jobs.snapshot_size_bytes = NULL`
- `tag doctor` warns if total snapshot storage exceeds 1 GB

### 7.10 Integration with PRD-013 Spans

The `trace_snapshots` table uses the same `trace_id` namespace as the `spans` table. `tag trace show <trace_id>` joins both tables to produce a unified timeline view:

```
trace_id: a1b2c3d4
  span: agent_run          [0ms → 4,312ms]
    snapshot: run_start    [0ms]
    snapshot: model_req    [12ms → 891ms]   1,204 tokens
    snapshot: tool_call    [892ms]          run_shell
    snapshot: tool_result  [5,123ms]        exit_code=0
    snapshot: model_req    [5,124ms → ...]  ...
    span: tool_execution   [892ms → 5,122ms]
```

---

## 8. Implementation Plan

### Phase 1 — Capture Infrastructure (Sprint 1, Week 1–2)

**Goal:** All runs capture snapshots. No replay yet.

| Task | File(s) | Effort |
|------|---------|--------|
| Add `trace_snapshots` table and migration | `controller.py`, `tracing.py` | S |
| Implement `SnapshotRecorder` class | `tracing.py` | M |
| Implement `SecretRedactor` with pattern matching and entropy check | `tracing.py` | M |
| Wire `SnapshotRecorder` into model dispatch path | `controller.py` | M |
| Wire `SnapshotRecorder` into tool execution path | `controller.py` | M |
| Add `snapshot_enabled` and `snapshot_size_bytes` to `queue_jobs` | `controller.py` (migration) | S |
| Add `tag trace list` and `tag trace show` commands | `controller.py` | S |
| Add compression for large payloads | `tracing.py` | S |
| Unit tests: `SecretRedactor` coverage of all patterns | `tests/` | M |
| Unit tests: `SnapshotRecorder` round-trip | `tests/` | S |

### Phase 2 — Replay and Diff (Sprint 1, Week 3–4)

**Goal:** `tag trace replay` and `tag trace diff` are functional.

| Task | File(s) | Effort |
|------|---------|--------|
| Implement `TraceReplayer` with normal replay mode | `replay.py` (new) | L |
| Implement step mode interaction loop (Rich panel + `$EDITOR` integration) | `replay.py` | M |
| Implement `--from-step` fast-forward | `replay.py` | S |
| Implement `--live-model` divergence detection | `replay.py` | M |
| Implement `TraceDiffer` with Myers alignment | `replay.py` | M |
| Add `tag trace replay` and `tag trace diff` CLI commands | `controller.py` | S |
| Integration test: capture then replay a full synthetic run | `tests/` | M |
| Integration test: diff two diverging synthetic runs | `tests/` | S |

### Phase 3 — Checkpoints and Lifecycle (Sprint 2, Week 1–2)

**Goal:** Checkpoint fork, export, and prune are functional. All features stable.

| Task | File(s) | Effort |
|------|---------|--------|
| Add `trace_checkpoints` table and migration | `controller.py` | S |
| Implement checkpoint creation in `cmd_trace_checkpoint` | `controller.py` | S |
| Wire checkpoint loading into `queue_worker.py` | `queue_worker.py` | M |
| Implement `tag trace export` (JSON/JSONL output) | `controller.py` | S |
| Implement `tag trace prune` with TTL enforcement | `controller.py` | S |
| Wire prune warning into `tag doctor` | `controller.py` | S |
| Add `replay.snapshot_ttl_days` to config schema | `controller.py` | XS |
| End-to-end test: checkpoint fork produces valid new run | `tests/` | M |
| Performance test: P95 replay startup < 500ms on 50-step trace | `tests/` | S |
| Documentation: update `tag trace --help` text | `controller.py` | XS |

---

## 9. Security Considerations

### 9.1 Secret Storage Risk

Tool results frequently contain sensitive data: API responses with tokens, file contents with credentials, shell output with environment variables. The `SecretRedactor` mitigates this but cannot catch all secrets (e.g., a database password that doesn't match any known pattern).

**Mitigation:**
- Default `snapshot_enabled = True` but document the security trade-off prominently
- Provide `--no-snapshot` flag on `tag submit` and `tag queue add`
- `tag trace export` requires explicit `--include-tool-results` flag to export `output_payload` for `tool_result` events; by default, export omits tool results
- Snapshots are stored in `~/.tag/tag.sqlite3` with filesystem permissions `600`

### 9.2 Replay Isolation

Replay must not re-execute tool side effects. A replayed `run_shell` call must feed the stored `exit_code` + `stdout` back to the model rather than running the command again.

**Mitigation:**
- `TraceReplayer` maintains a `_BLOCKED_TOOLS` set and never calls the actual tool executor for those tools during replay
- Any attempt to execute a blocked tool during replay raises `ReplayIsolationError` and aborts the replay
- Destructive tool detection is based on tool name (e.g., `run_shell`, `write_file`, `http_request`) plus a `side_effect: true` annotation in the tool schema

### 9.3 Checkpoint Scope Creep

A checkpoint fork executes with the live model for all steps after step N. This means the new run can execute real tool calls. This is by design (the user wants to continue the run) but must be clearly communicated.

**Mitigation:**
- `tag trace checkpoint` prints a prominent warning: "Forking from step N. Steps N+1 onward will execute real tool calls."
- The checkpoint job is added to the queue in `pending` status rather than auto-started; the user must explicitly `tag queue start <job_id>`

### 9.4 Trace ID Enumeration

Trace IDs are UUIDs (v4). They are stored locally and never exposed over a network interface. Enumeration risk is negligible for single-user local deployments.

---

## 10. Risks

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| Replay divergence makes debugging confusing | Medium | Medium | Clear divergence warnings with step-level highlighting; `--live-model` flag makes divergence explicit |
| Storage costs balloon for long-running agents | Medium | Low | Compression for payloads > 100KB; TTL-based pruning; `tag doctor` storage warning |
| `SecretRedactor` misses a secret pattern | Medium | High | Entropy-based catch-all; explicit security warning in docs; `--no-snapshot` escape hatch |
| Replay introduces security risk (stored creds) | Low | High | File permission `600`; redaction; export requires explicit flag |
| Schema migration fails on corrupt DB | Low | Medium | Migration is wrapped in a transaction; failure leaves DB unchanged |
| `queue_worker.py` checkpoint resume breaks mid-DAG (with PRD-033) | Low | Medium | Checkpoint fork creates an independent job; dependency graph of the fork is reset |
| Myers diff alignment is O(N²) for very long traces | Low | Low | Cap trace display at 500 steps in diff view; offer `--format json` for raw diff |

---

## 11. Open Questions

1. **Should replay capture be opt-in or opt-out?** Current design: opt-out (on by default when tracing is enabled). Some users may object to every run being stored. Counter-argument: opt-in means nobody uses it until they wish they had it. **Proposed resolution:** opt-out, with a `replay.enabled = false` config key.

2. **How should we handle tool schema changes between capture and replay?** If a tool's argument schema changes between the original run and the replay, stored arguments may not deserialize correctly. **Proposed resolution:** store the tool schema version hash at capture time; emit a warning (not an error) on schema mismatch during replay.

3. **Should `--live-model` in replay use the same model as the original run?** The original model may be deprecated or cost-prohibitive. **Proposed resolution:** default to the original model; allow `--model MODEL` override.

4. **Should diff support N-way comparison (>2 traces)?** Useful for benchmarking (PRD-017). Out of scope for this PRD but the `TraceDiffer` API should be designed to accept a list of trace IDs to make this extension natural.

5. **Should snapshots be exportable to OpenTelemetry OTLP format?** This would enable viewing in Jaeger or Honeycomb. The `tag trace export` command currently outputs JSON/JSONL. OTLP export is a natural extension once PRD-013's OTLP exporter is implemented.

6. **What is the maximum safe `input_payload` size?** A coding agent with a 128K context window could produce `input_payload` blobs of ~500 KB per model request step. With compression this is manageable, but we should add a `replay.max_payload_bytes` cap (default: 2 MB per step) above which the snapshot is truncated and flagged.

---

## 12. Appendix: Config Schema Additions

New keys in `cli-config.yaml` (all optional, with defaults):

```yaml
replay:
  enabled: true                  # capture snapshots for all runs
  snapshot_ttl_days: 30          # auto-prune after N days
  max_payload_bytes: 2097152     # 2 MB per-step cap; truncate beyond this
  redact_secrets: true           # run SecretRedactor on all payloads
  compress_threshold_bytes: 102400  # compress output_payload above this size
```

## 13. Appendix: Example `tag trace show` Output

```
$ tag trace show a1b2c3d4-e5f6-7890-abcd-ef1234567890

Trace a1b2c3d4  [2026-06-10 14:32:11 UTC]  profile=coder  status=failed
Task: "Implement pagination for the /users API endpoint"
Duration: 4m 12s  |  Steps: 23  |  Tokens: 18,204  |  Snapshot: 142 KB

Step  Type            Tool / Model          Tokens   Duration   Result
────  ──────────────  ────────────────────  ───────  ─────────  ──────────────
   1  model_request   claude-opus-4         1,204    812ms      → tool_call
   2  tool_call       read_file             —        23ms       OK
   3  tool_result     read_file             —        —          142 bytes
   4  model_request   claude-opus-4         1,841    934ms      → tool_call
  ...
  21  tool_call       run_shell             —        4,231ms    exit_code=1
  22  tool_result     run_shell             —        —          [REDACTED:token]
  23  run_end         —                     18,204   252,441ms  FAILED

Failure at step 21: run_shell returned exit_code=1
  Hint: run `tag trace replay a1b2c3d4 --step-mode --from-step 20` to inspect
```
