# PRD-049: Versioned Eval Dataset Management (`tag eval dataset`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** S (3-5 days)
**Category:** Evaluation & Observability
**Affects:** `eval_datasets SQLite table + controller.py`
**Depends on:** PRD-027 (eval framework — `eval_runs`/`eval_cases` tables, `cmd_eval` entrypoint), PRD-013 (agent tracing — `runs`/`steps` tables, span infrastructure), PRD-028 (sandbox code execution — safe execution context for import validation), PRD-034 (secret scanning — dataset content scanning before export/import), PRD-012 (cost tracking — token counts on captured runs)
**Inspired by:** LangSmith datasets, Braintrust datasets, W&B Weave

---

## 1. Overview

TAG's eval framework (PRD-027) enables behavioral regression testing against TAG profiles, but currently requires engineers to hand-author every test case in YAML. Real agent runs — the richest source of ground truth about system behavior — live in the `runs` and `steps` SQLite tables and are never systematically harvested for reuse in evals. The result is that eval suites grow slowly, stay small, and fail to represent the distribution of real production tasks.

Versioned Eval Dataset Management introduces `tag eval dataset`: a first-class system for creating, versioning, and reusing collections of (input, expected output) pairs captured from production runs, hand-authored, or imported from external sources. Datasets are stored persistently in SQLite under an `eval_datasets` table family, versioned with immutable snapshots, and exportable/importable as JSONL files. Any dataset can be attached to a PRD-027 eval run as its source of test cases, replacing the YAML `cases` block with a pointer to a named, versioned dataset.

The design is directly inspired by LangSmith's dataset management (capture from traces, version history, golden sets), Braintrust's dataset versioning (immutable snapshots, experiment linkage), and W&B Weave's artifact approach (named datasets with version lineage). Unlike those cloud products, TAG's implementation is entirely local-first: all data lives in `~/.tag/runtime/tag.sqlite3` using the existing WAL-mode SQLite infrastructure, with no external service dependency. Teams that want to share datasets can use the JSONL export/import surface to exchange files via git or object storage.

Datasets serve as the persistent "memory" of the eval system. A `golden` dataset for a profile is a curated collection of the best-known (input, expected output) pairs that define what "correct" behavior looks like. As the profile evolves, the golden dataset stays stable — enabling reproducible evals across profile versions, model swaps, and system prompt changes. Snapshot versioning means that a run against `my-golden@v3` is always reproducible even after the dataset is later updated to `v4`.

This feature is a force multiplier for the eval system introduced in PRD-027. It addresses the cold-start problem (no test cases), the maintenance problem (test cases drift from production), and the reproducibility problem (dataset changes invalidate historical comparisons). The `--from-runs` capture path specifically bridges the gap between production traffic and eval coverage by letting engineers promote real runs into a dataset with a single command.

---

## 2. Problem Statement

### 2.1 Eval suites are expensive to author and disconnected from production

PRD-027 requires engineers to write YAML test cases by hand: input prompt, expected output description, tool lists, retrieval context. For a non-trivial eval suite of 50 cases, this is multiple engineer-days of work. Meanwhile, TAG already executes hundreds of real tasks per day, storing full prompt + output in the `runs` and `steps` tables. The inputs and outputs from those real runs are the best possible source of eval cases — yet no tooling exists to harvest them. Engineers who want realistic eval coverage must manually copy-paste from `tag runs show` output into YAML files.

### 2.2 No reproducibility across eval runs over time

Even when engineers build eval suites, the YAML files evolve as they add, remove, and edit cases. There is no version history for the case set itself. This means that when `tag eval history` shows a score drop from run N to run N+1, it is impossible to determine whether the drop reflects a profile regression or a change in the test cases. Without immutable dataset snapshots, longitudinal eval comparisons are unreliable.

### 2.3 No interoperability with external tools or teams

TAG eval results are useful for internal quality tracking, but teams increasingly want to share golden test sets across repositories, integrate with external eval tooling (LangSmith, Braintrust, custom scripts), or import datasets from annotation workflows. The current YAML format is TAG-specific, and there is no import path for external data. This creates friction when onboarding teams that already have eval datasets in JSONL or CSV format from other tools.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Capture eval dataset rows from recent production `runs`/`steps` records with a single command, filtered by time window, profile, or tag. |
| G2 | Store datasets and their row contents persistently in SQLite under the existing `open_db()` infrastructure, with full CRUD operations accessible via `tag eval dataset` subcommands. |
| G3 | Support immutable version snapshots: every `tag eval dataset snapshot` call creates a numbered, timestamped version that is permanently readable even after the live dataset is modified. |
| G4 | Export any dataset or snapshot to JSONL (`{"input": ..., "expected_output": ..., "metadata": {...}}` per line) for use with external tools or sharing via git. |
| G5 | Import JSONL files as new datasets or append rows to existing datasets, with schema validation and secret scanning before writing. |
| G6 | Allow a PRD-027 eval run (`tag eval run`) to reference a named dataset (and optionally a specific version) as its source of test cases instead of a YAML `cases` block. |
| G7 | Tag datasets with arbitrary key-value metadata (owner, task-type, capture-date, profile) to enable filtering in `tag eval dataset list`. |
| G8 | `tag eval dataset show` displays full dataset contents, row counts, version history, and attached metadata without requiring external tools. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Cloud synchronization or multi-user dataset sharing. Dataset state lives in local SQLite only; sharing is via JSONL file export/import. A future PRD may add remote backends. |
| NG2 | Automatic labeling or LLM-judge scoring of dataset rows at capture time. Datasets store raw (input, expected_output) pairs; scoring happens at eval run time via PRD-027's judge. |
| NG3 | Replacing the YAML `cases` block in PRD-027 eval suites. The dataset system is additive — YAML cases continue to work; datasets are an alternative source. |
| NG4 | Dataset deduplication or semantic similarity checks at import/capture time. Duplicate rows are allowed; deduplication is a future enhancement. |
| NG5 | GUI or web dashboard for dataset browsing. The `tag eval dataset show` CLI surface and JSONL export are the only UI surfaces in this PRD. |
| NG6 | Streaming capture of live runs in real time. Capture is always retrospective, querying completed runs from the `runs`/`steps` tables. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Time to first golden dataset | Engineer captures a 50-case golden dataset from production runs in < 2 minutes | Manual timing benchmark |
| Snapshot reproducibility | `tag eval run --dataset my-golden@v1` produces identical case set across 2 invocations on the same DB | Deterministic row-count assertion in integration test |
| JSONL round-trip fidelity | 100% of rows survive export → import → re-export with byte-identical JSON per line | Automated round-trip test |
| Secret scan coverage | Zero datasets containing `sk-*` or `Bearer *` patterns can be exported without an explicit `--allow-secrets` flag | Security test |
| SQLite write latency | `tag eval dataset create --from-runs --since 7d --limit 50` completes in < 5s on a 100k-row `runs` table | Benchmark test with seeded DB |
| Eval integration | `tag eval run --dataset my-golden` resolves dataset rows and runs PRD-027 scorer without YAML modification | Integration test |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|------------|----------|
| U1 | Profile author | run `tag eval dataset create my-golden --from-runs --since 7d --limit 50` | I get a curated golden set from last week's real production runs without hand-authoring YAML cases |
| U2 | Team lead | run `tag eval dataset snapshot my-golden` before making a major profile change | I have a frozen, numbered version of the dataset that any future eval run can reference for apples-to-apples comparison |
| U3 | DevOps engineer | run `tag eval run --suite evals/coding.yaml --dataset my-golden@v3` in CI | The CI eval always runs against the same v3 snapshot regardless of how the live dataset evolves, making score history trustworthy |
| U4 | Developer | run `tag eval dataset export my-golden --format jsonl --output golden.jsonl` | I can share the golden set with a colleague or check it into the repo for external eval tooling |
| U5 | Developer | run `tag eval dataset import --file golden.jsonl --name imported-golden` | I can ingest a dataset built by a teammate or from an annotation workflow without touching SQLite directly |
| U6 | Platform engineer | run `tag eval dataset list --json` and filter by `task_type=coding` | I get a machine-readable inventory of all datasets for dashboard display or CI gating decisions |
| U7 | Developer | run `tag eval dataset show my-golden --json` | I can inspect all rows, metadata, and version history in a structured format for scripting |
| U8 | Developer | run `tag eval dataset add my-golden --run-id abc123` | I can manually promote a single notable run into the golden set without re-running the full `--from-runs` capture |
| U9 | Security engineer | rely on secret scanning at export time | Datasets containing API keys or tokens are blocked from export unless explicitly overridden, preventing accidental credential leakage |
| U10 | Developer | run `tag eval dataset delete my-golden --version 2` | I can drop a specific stale snapshot version without losing the live dataset or other versions |

---

## 6. Proposed CLI Surface

All dataset subcommands live under `tag eval dataset`. They extend the existing `tag eval` namespace established in PRD-027.

### 6.1 `tag eval dataset create`

Create a new dataset, optionally pre-populated from recent production runs.

```
tag eval dataset create <name> \
  [--from-runs] \
  [--since <duration>] \
  [--until <duration>] \
  [--profile <profile>] \
  [--limit <n>] \
  [--status <completed|failed|all>] \
  [--tag <key=value>...] \
  [--description <text>] \
  [--json]
```

**Arguments:**
- `<name>`: Dataset name. Must be unique across all datasets. Alphanumeric, hyphens, underscores only. 1-64 characters.
- `--from-runs`: Populate dataset from existing `runs`/`steps` records. If omitted, creates an empty dataset.
- `--since <duration>`: Time window lower bound for run capture. Duration strings: `7d`, `24h`, `30m`, `2026-01-01`. Defaults to `7d` when `--from-runs` is set.
- `--until <duration>`: Time window upper bound. Defaults to `now`.
- `--profile <profile>`: Filter captured runs to a specific TAG profile.
- `--limit <n>`: Maximum number of rows to capture. Applies after all other filters, ordered by `created_at DESC`. Default: 100. Maximum: 10000.
- `--status <completed|failed|all>`: Filter by run status. Default: `completed`.
- `--tag <key=value>`: Attach metadata tag to the dataset. Repeatable. Example: `--tag task_type=coding --tag owner=alice`.
- `--description <text>`: Human-readable dataset description stored in metadata.
- `--json`: Output the created dataset record as JSON.

**Example:**

```
$ tag eval dataset create my-golden --from-runs --since 7d --limit 50 \
    --profile coder --tag task_type=coding --description "Golden set from coding profile"

Created dataset: my-golden
  id:          ds_a1b2c3d4
  rows:        50
  version:     none (use 'tag eval dataset snapshot my-golden' to pin a version)
  description: Golden set from coding profile
  tags:        task_type=coding
  created_at:  2026-06-17T10:00:00Z
```

**Error cases:**
- Name already exists: `Error: dataset 'my-golden' already exists. Use 'tag eval dataset add' to append rows.`
- `--from-runs` with no matching runs: `Warning: no completed runs found in last 7d for profile 'coder'. Dataset created empty.`
- Invalid `--since` format: `Error: cannot parse duration '7days'. Use formats like '7d', '24h', '2026-01-01'.`

### 6.2 `tag eval dataset list`

List all datasets with summary metadata.

```
tag eval dataset list \
  [--tag <key=value>...] \
  [--profile <profile>] \
  [--json]
```

**Flags:**
- `--tag <key=value>`: Filter to datasets that have all specified metadata tags.
- `--profile <profile>`: Filter to datasets captured from a specific profile (stored in metadata).
- `--json`: Machine-readable JSON array output.

**Example (TTY):**

```
$ tag eval dataset list

  NAME                 ROWS   VERSIONS   LAST SNAPSHOT         TAGS
  ─────────────────────────────────────────────────────────────────────────────
  my-golden              50          2   2026-06-10T09:12:00Z  task_type=coding
  imported-golden        23          0   —                     source=external
  research-baseline      15          1   2026-06-01T14:00:00Z  task_type=research

3 datasets.
```

**Example (--json):**

```json
[
  {
    "id": "ds_a1b2c3d4",
    "name": "my-golden",
    "row_count": 50,
    "version_count": 2,
    "latest_version": 2,
    "latest_snapshot_at": "2026-06-10T09:12:00Z",
    "description": "Golden set from coding profile",
    "tags": {"task_type": "coding"},
    "created_at": "2026-06-17T10:00:00Z"
  }
]
```

### 6.3 `tag eval dataset show`

Inspect a dataset's rows, metadata, and version history.

```
tag eval dataset show <name>[@<version>] \
  [--limit <n>] \
  [--offset <n>] \
  [--json]
```

**Arguments:**
- `<name>[@<version>]`: Dataset name, optionally pinned to a snapshot version (e.g., `my-golden@v2`). Without version, shows the live dataset.
- `--limit <n>`: Max rows to display. Default: 20 for TTY, unlimited for `--json`.
- `--offset <n>`: Row offset for pagination. Default: 0.
- `--json`: Full row-level JSON output including all fields.

**Example (TTY):**

```
$ tag eval dataset show my-golden

Dataset: my-golden  (ds_a1b2c3d4)
Description: Golden set from coding profile
Tags: task_type=coding
Rows: 50  |  Versions: v1 (2026-06-05), v2 (2026-06-10)
Created: 2026-06-17T10:00:00Z

  ROW  SOURCE_RUN_ID   INPUT (truncated 80 chars)                              ADDED_AT
  ─────────────────────────────────────────────────────────────────────────────────────
    1  run_abc12345    Write a Python function that returns the nth Fibonacci…  2026-06-17
    2  run_def67890    Fix the off-by-one error in this C++ loop...             2026-06-17
   ...
   50  run_xyz99999    Refactor this class to use dataclasses...                2026-06-17

Showing rows 1-20 of 50. Use --offset 20 to see more, --json for full output.
```

### 6.4 `tag eval dataset snapshot`

Create an immutable, numbered version snapshot of the current live dataset.

```
tag eval dataset snapshot <name> \
  [--note <text>] \
  [--json]
```

**Arguments:**
- `<name>`: Dataset to snapshot.
- `--note <text>`: Optional human-readable note attached to this snapshot (e.g., "before model upgrade to claude-opus-5").
- `--json`: Output the created snapshot record as JSON.

**Example:**

```
$ tag eval dataset snapshot my-golden --note "before model upgrade to claude-opus-5"

Snapshot created: my-golden@v3
  dataset:    my-golden  (ds_a1b2c3d4)
  version:    3
  rows:       50
  note:       before model upgrade to claude-opus-5
  created_at: 2026-06-17T11:00:00Z
  row_hash:   sha256:a3f1c8b2...

Tip: reference this snapshot in eval runs with: --dataset my-golden@v3
```

The `row_hash` is a SHA-256 of the sorted, serialized row contents — a content fingerprint that makes it verifiable that the snapshot has not changed.

### 6.5 `tag eval dataset export`

Export a dataset or snapshot to a file.

```
tag eval dataset export <name>[@<version>] \
  --format <jsonl|csv> \
  [--output <path>] \
  [--allow-secrets] \
  [--json]
```

**Arguments:**
- `<name>[@<version>]`: Dataset to export, optionally pinned to a version.
- `--format <jsonl|csv>`: Output format. `jsonl` (default): one JSON object per line. `csv`: header row + comma-delimited rows.
- `--output <path>`: Write to file instead of stdout. If omitted, writes to stdout.
- `--allow-secrets`: Bypass the secret pattern check. Required if the dataset contains credential-like strings.
- `--json`: When exporting to stdout without `--format jsonl`, output a JSON wrapper with dataset metadata + rows array.

**JSONL format per row:**

```jsonl
{"id": "dr_00001", "input": "Write a Python function...", "expected_output": "A Python function using a loop...", "metadata": {"source_run_id": "run_abc12345", "profile": "coder", "captured_at": "2026-06-17T10:00:00Z", "tags": {"task_type": "coding"}}}
{"id": "dr_00002", "input": "Fix the off-by-one error...", "expected_output": "Change lst[len(lst)] to lst[-1]...", "metadata": {"source_run_id": "run_def67890", "profile": "coder", "captured_at": "2026-06-17T10:00:00Z", "tags": {}}}
```

**Example:**

```
$ tag eval dataset export my-golden --format jsonl --output golden.jsonl

Scanning 50 rows for secrets... OK
Exported 50 rows to golden.jsonl (12.4 KB)
```

### 6.6 `tag eval dataset import`

Import a JSONL or CSV file as a new dataset or append to an existing one.

```
tag eval dataset import \
  --file <path> \
  --name <name> \
  [--append] \
  [--allow-secrets] \
  [--tag <key=value>...] \
  [--description <text>] \
  [--json]
```

**Arguments:**
- `--file <path>`: Path to the JSONL or CSV file to import.
- `--name <name>`: Target dataset name. If the dataset does not exist, it is created. If it exists, `--append` is required.
- `--append`: Allow appending rows to an existing dataset. Without this flag, importing to an existing name is an error.
- `--allow-secrets`: Bypass secret scanning on import.
- `--tag <key=value>`: Attach metadata to the created/updated dataset.
- `--description <text>`: Dataset description (ignored if dataset already exists without `--append --overwrite-description`).
- `--json`: Output import summary as JSON.

**JSONL minimal format (import accepts either full export format or minimal format):**

```jsonl
{"input": "Write a Python function...", "expected_output": "A correct iterative implementation..."}
{"input": "Fix this bug...", "expected_output": "The corrected code with the off-by-one fixed..."}
```

**Example:**

```
$ tag eval dataset import --file golden.jsonl --name imported-golden \
    --tag source=external --description "Imported from annotation team"

Parsing golden.jsonl... 23 rows found
Scanning for secrets... OK
Created dataset: imported-golden
  id:    ds_b5e6f7g8
  rows:  23
  tags:  source=external
```

### 6.7 `tag eval dataset add`

Add a single run to a dataset.

```
tag eval dataset add <name> \
  --run-id <run_id> \
  [--expected-output <text>] \
  [--json]
```

**Arguments:**
- `<name>`: Target dataset (must already exist).
- `--run-id <run_id>`: The `runs.id` value to promote to a dataset row. The `prompt` field becomes `input`; the final assistant `steps.output` becomes the draft `expected_output`.
- `--expected-output <text>`: Override the draft `expected_output` extracted from the run. If omitted, the run's actual output is used as-is.
- `--json`: Output the added row as JSON.

**Example:**

```
$ tag eval dataset add my-golden --run-id run_abc12345 \
    --expected-output "A correct iterative Fibonacci function with docstring and edge case handling."

Added 1 row to my-golden (now 51 rows).
  row_id:          dr_00051
  source_run_id:   run_abc12345
  input:           "Write a Python function that returns the nth Fibonacci..."
  expected_output: "A correct iterative Fibonacci function with docstring and edge case handling."
```

### 6.8 `tag eval dataset delete`

Delete a dataset or a specific version snapshot.

```
tag eval dataset delete <name>[@<version>] \
  [--yes]
```

**Arguments:**
- `<name>[@<version>]`: If no version specified, deletes the entire dataset and all its snapshots. If a version is specified (e.g., `my-golden@v2`), deletes only that snapshot; the live dataset and other snapshots are unaffected.
- `--yes`: Skip confirmation prompt.

**Safety:** Deletion of a dataset version that is referenced by an existing `eval_runs` row is blocked with an error unless `--force` is also passed.

### 6.9 Integration with `tag eval run`

The existing `tag eval run` command (PRD-027) gains a `--dataset` flag:

```
tag eval run \
  --suite evals/coding.yaml \
  --dataset my-golden[@<version>] \
  [--profile <profile>] \
  [--threshold 0.7] \
  [--json]
```

When `--dataset` is specified, dataset rows are used as test cases in place of (or in addition to, if `--suite` is also given) the YAML `cases` block. Each dataset row's `input` maps to `case.input`; `expected_output` maps to `case.expected_output`. The `eval_results` table stores `dataset_id` and `dataset_version` on each result row for traceability.

---

## 7. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | **Dataset name uniqueness:** `tag eval dataset create <name>` must fail with exit code 1 and a descriptive message if a dataset with that name already exists in the `eval_datasets` table. Name validation: `[a-zA-Z0-9_-]{1,64}` regex; error on violation. |
| FR-02 | **Run capture query:** When `--from-runs` is set, the system queries `SELECT r.id, r.prompt, r.master_profile, r.created_at FROM runs r JOIN steps s ON s.run_id = r.id WHERE r.status = ? AND r.created_at >= ? AND r.created_at <= ? AND r.master_profile = ? ORDER BY r.created_at DESC LIMIT ?`. Profile and status filters are omitted when not specified. The final assistant step (`s.role = 'assistant'` and `s.id = MAX(s.id) WHERE s.run_id = r.id`) is used as the draft `expected_output`. |
| FR-03 | **Row content schema:** Every `eval_dataset_rows` row must have non-null, non-empty `input` and `expected_output` fields. Import/capture that would produce an empty field for either must log a warning and skip that row, not fail the entire operation. |
| FR-04 | **Snapshot immutability:** Once a snapshot version is created via `tag eval dataset snapshot`, the set of rows and their content referenced by that version must never change. The snapshot stores a copy of row IDs (via `eval_dataset_snapshot_rows` join table or inline serialized JSON) at snapshot time. Adding or removing rows from the live dataset after snapshotting must not affect any prior snapshot. |
| FR-05 | **Snapshot row hash:** Every snapshot record stores a `row_hash TEXT NOT NULL` computed as `sha256(sorted JSON serialization of all row {input, expected_output} pairs)`. This hash is displayed in `tag eval dataset snapshot` output and verified on read via `tag eval dataset show my-golden@v1`. |
| FR-06 | **Export secret scan:** Before writing any row to an output file, `tag eval dataset export` must scan `input` and `expected_output` for the secret patterns defined in PRD-034's `SENSITIVE_PATTERNS` (or an equivalent local regex set). If any match is found, export is blocked with a descriptive error naming the row ID and pattern matched. Override requires `--allow-secrets`. |
| FR-07 | **Import secret scan:** Same as FR-06 but applied at import time before any rows are written to SQLite. |
| FR-08 | **Import minimal format:** The importer must accept JSONL rows that have only `input` and `expected_output` fields. Additional recognized fields: `id` (used as a hint, regenerated if collision), `metadata` (dict, merged with dataset-level metadata). Unknown fields are stored in `metadata_json` without error. |
| FR-09 | **JSONL format compliance:** Each exported JSONL line must be valid JSON (parseable by `json.loads`), encode unicode characters correctly (not escaped as `\uXXXX` unless required), and end with a newline. The file must not have a trailing blank line. |
| FR-10 | **CSV export format:** When `--format csv` is used, the header row is `id,input,expected_output,metadata_json` (quoted). `input` and `expected_output` values are double-quote escaped. `metadata_json` is a JSON object serialized as a single quoted string. |
| FR-11 | **Version reference syntax:** `<name>@<version>` syntax must be supported in `show`, `export`, and the `tag eval run --dataset` flag. `<version>` is an integer (e.g., `my-golden@3`). Alias `v<N>` is also accepted (`my-golden@v3`). Requesting a non-existent version exits 1 with `Error: no snapshot version 3 for dataset 'my-golden'.` |
| FR-12 | **Dataset metadata tags:** Tags are stored as a JSON object (`{"key": "value", ...}`) in the `tags_json` column. `--tag key=value` on create/import sets tag pairs. Tags do not have a schema; any string key and value are accepted. `tag eval dataset list --tag key=value` filters using SQL `json_extract(tags_json, '$.key') = ?`. |
| FR-13 | **Append safety:** `tag eval dataset import --name existing-dataset` without `--append` must exit 1 with `Error: dataset 'existing-dataset' already exists. Pass --append to add rows to it.` |
| FR-14 | **Delete confirmation:** `tag eval dataset delete <name>` (without `@<version>`) must prompt `Delete dataset 'my-golden' and all 3 snapshots? [y/N]` unless `--yes` is passed. Destructive operation; no undo. |
| FR-15 | **Version deletion blocked by active eval references:** Deleting a snapshot version that has rows in `eval_results.dataset_version` matching the version number must be blocked unless `--force` is also passed. Error: `Error: snapshot my-golden@v2 is referenced by 5 eval_results rows. Pass --force to delete anyway.` |
| FR-16 | **`tag eval run --dataset` integration:** When `--dataset <name>[@<version>]` is passed to `tag eval run`, the system loads dataset rows and converts each to a `case` dict with `id`, `input`, `expected_output` fields, then passes the list to the existing `run_suite` function from `eval_framework.py` as if they were YAML cases. The `eval_results` rows written for this run must include `dataset_id` and `dataset_version` (or `null` for live dataset). |
| FR-17 | **`--limit` enforcement:** `tag eval dataset create --limit N` must capture at most N rows. If fewer than N matching runs exist, the dataset is created with however many are found; no error is raised. The `row_count` in the created dataset record reflects the actual number of rows written. |
| FR-18 | **`tag eval dataset add` run resolution:** The `--run-id` value must be validated against the `runs` table before insertion. If the run ID does not exist, exit 1 with `Error: run 'run_abc12345' not found.` If the run has no assistant step in `steps`, exit 1 with `Error: run 'run_abc12345' has no completed assistant output step.` |
| FR-19 | **Atomic create-and-populate:** `tag eval dataset create --from-runs` must be atomic: either all captured rows are committed together with the dataset record, or none are. A partial capture (e.g., due to process kill after dataset row but before all `eval_dataset_rows` rows are written) must not leave a corrupt dataset. Use a single SQLite transaction for the entire create-and-populate operation. |
| FR-20 | **`--json` output consistency:** All subcommands that accept `--json` must produce output parseable by `json.loads` on stdout with zero prose lines intermixed. Progress and warning messages in `--json` mode go to stderr only. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **SQLite WAL compatibility:** All writes to `eval_datasets`, `eval_dataset_rows`, and `eval_dataset_snapshots` must use WAL-mode journal (inherited from `open_db()`). Concurrent reads during a long `--from-runs` capture must not block `tag runs list` or other read-only commands. |
| NFR-02 | **Large dataset performance:** `tag eval dataset create --from-runs --limit 10000` must complete in under 30 seconds on a `runs` table with 500k rows. The capture query must use the `idx_runs_created_at` index (or equivalent). Row inserts must be batched: `executemany()` in chunks of 500 rows inside a single transaction. |
| NFR-03 | **JSONL streaming export:** `tag eval dataset export` must stream rows from SQLite to the output file/stdout without loading the entire dataset into memory. Use a `SELECT` with `conn.execute()` row-by-row iteration (`cursor.fetchmany(100)` batches) to support datasets of 10k+ rows without OOM risk. |
| NFR-04 | **No deepeval dependency:** Dataset management commands (`create`, `list`, `show`, `snapshot`, `export`, `import`, `add`, `delete`) must not import or require `deepeval`. Only `tag eval run --dataset` requires `deepeval` (inherited from PRD-027 scoring). |
| NFR-05 | **TTY vs. pipe rendering:** When stdout is a TTY, `show` and `list` render Rich tables. When stdout is a pipe or `--json` is set, plain JSON or JSONL is written. This mirrors `cmd_runs` and `cmd_eval list` patterns in `controller.py`. |
| NFR-06 | **Idempotent schema migration:** The `eval_datasets`, `eval_dataset_rows`, and `eval_dataset_snapshots` DDL uses `CREATE TABLE IF NOT EXISTS`, ensuring repeated calls to `open_db()` are safe. No `ALTER TABLE` migrations are required for the initial schema. |
| NFR-07 | **Error message quality:** All user-facing errors must include: (a) what went wrong, (b) the offending value, (c) what the user should do instead. Single-line errors use `Error: <message>`. Multi-line errors use the pattern from `print_error()` in `controller.py`. |

---

## 9. Technical Design

### 9.1 New files

- **`src/tag/eval_datasets.py`** — All dataset logic: schema DDL, capture, CRUD, snapshot, export, import, secret scanning, and the `DatasetRow`/`DatasetRecord`/`SnapshotRecord` dataclasses. Imported lazily by `cmd_eval_dataset` in `controller.py`.
- **No new directories required.** The module sits alongside `eval_framework.py` in `src/tag/`.

### 9.2 SQLite DDL

```sql
-- Primary dataset registry
CREATE TABLE IF NOT EXISTS eval_datasets (
  id           TEXT PRIMARY KEY,           -- uuid4 prefixed 'ds_'
  name         TEXT NOT NULL UNIQUE,       -- user-facing name, alphanumeric/hyphen/underscore
  description  TEXT NOT NULL DEFAULT '',   -- human-readable description
  tags_json    TEXT NOT NULL DEFAULT '{}', -- JSON object {"key": "value", ...}
  row_count    INTEGER NOT NULL DEFAULT 0, -- denormalized count, updated on row insert/delete
  created_at   TEXT NOT NULL,              -- ISO-8601 UTC
  updated_at   TEXT NOT NULL               -- ISO-8601 UTC, updated on any row mutation
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_ed_name ON eval_datasets(name);

-- Individual dataset rows (the actual (input, expected_output) pairs)
CREATE TABLE IF NOT EXISTS eval_dataset_rows (
  id                TEXT PRIMARY KEY,        -- uuid4 prefixed 'dr_'
  dataset_id        TEXT NOT NULL,           -- FK -> eval_datasets.id
  input             TEXT NOT NULL,           -- the prompt / task sent to the agent
  expected_output   TEXT NOT NULL,           -- ideal agent response or behavioral description
  source_run_id     TEXT,                    -- optional FK -> runs.id (if captured from a run)
  metadata_json     TEXT NOT NULL DEFAULT '{}', -- arbitrary metadata dict (profile, captured_at, etc.)
  created_at        TEXT NOT NULL,           -- ISO-8601 UTC
  FOREIGN KEY(dataset_id) REFERENCES eval_datasets(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_edr_dataset ON eval_dataset_rows(dataset_id, created_at);
CREATE INDEX IF NOT EXISTS idx_edr_source_run ON eval_dataset_rows(source_run_id);

-- Immutable version snapshots
CREATE TABLE IF NOT EXISTS eval_dataset_snapshots (
  id           TEXT PRIMARY KEY,          -- uuid4 prefixed 'snap_'
  dataset_id   TEXT NOT NULL,             -- FK -> eval_datasets.id
  version      INTEGER NOT NULL,          -- monotonically increasing per dataset, starting at 1
  note         TEXT NOT NULL DEFAULT '',  -- optional human note
  row_hash     TEXT NOT NULL,             -- SHA-256 of sorted serialized rows at snapshot time
  row_ids_json TEXT NOT NULL,             -- JSON array of eval_dataset_rows.id values at snapshot time
  row_count    INTEGER NOT NULL,          -- count of rows at snapshot time
  created_at   TEXT NOT NULL,             -- ISO-8601 UTC
  FOREIGN KEY(dataset_id) REFERENCES eval_datasets(id) ON DELETE CASCADE,
  UNIQUE(dataset_id, version)
);

CREATE INDEX IF NOT EXISTS idx_eds_dataset ON eval_dataset_snapshots(dataset_id, version);

-- Migration: add dataset reference columns to eval_results (PRD-027)
-- Run only if column does not exist (checked programmatically before ALTER TABLE)
-- ALTER TABLE eval_results ADD COLUMN dataset_id TEXT;
-- ALTER TABLE eval_results ADD COLUMN dataset_version INTEGER;
```

The `ALTER TABLE` migration for `eval_results` is applied programmatically using the existing migration pattern in `controller.py` (`_migrate_add_column` helper or equivalent column-existence check).

### 9.3 Core dataclasses

```python
# src/tag/eval_datasets.py
from __future__ import annotations

import hashlib
import json
import re
import sqlite3
import uuid
from dataclasses import dataclass, field, asdict
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


DATASET_NAME_RE = re.compile(r'^[a-zA-Z0-9_-]{1,64}$')

# Secret patterns mirrored from PRD-034 / security.py
SENSITIVE_PATTERNS: list[re.Pattern[str]] = [
    re.compile(r'sk-[A-Za-z0-9]{32,}'),
    re.compile(r'Bearer [A-Za-z0-9+/=]{20,}'),
    re.compile(r'ghp_[A-Za-z0-9]{36}'),
    re.compile(r'AKIA[0-9A-Z]{16}'),       # AWS access key
    re.compile(r'sk-ant-[A-Za-z0-9\-_]{40,}'),  # Anthropic key
]


@dataclass
class DatasetRow:
    id: str
    dataset_id: str
    input: str
    expected_output: str
    source_run_id: str | None
    metadata: dict[str, Any]
    created_at: str

    @staticmethod
    def new(
        dataset_id: str,
        input: str,
        expected_output: str,
        source_run_id: str | None = None,
        metadata: dict[str, Any] | None = None,
    ) -> "DatasetRow":
        return DatasetRow(
            id=f"dr_{uuid.uuid4().hex[:12]}",
            dataset_id=dataset_id,
            input=input.strip(),
            expected_output=expected_output.strip(),
            source_run_id=source_run_id,
            metadata=metadata or {},
            created_at=utc_now(),
        )

    def content_fingerprint(self) -> str:
        """Stable fingerprint for snapshot hashing."""
        return json.dumps(
            {"input": self.input, "expected_output": self.expected_output},
            sort_keys=True,
            ensure_ascii=False,
        )


@dataclass
class DatasetRecord:
    id: str
    name: str
    description: str
    tags: dict[str, str]
    row_count: int
    created_at: str
    updated_at: str

    @staticmethod
    def new(name: str, description: str = "", tags: dict[str, str] | None = None) -> "DatasetRecord":
        now = utc_now()
        return DatasetRecord(
            id=f"ds_{uuid.uuid4().hex[:8]}",
            name=name,
            description=description,
            tags=tags or {},
            row_count=0,
            created_at=now,
            updated_at=now,
        )


@dataclass
class SnapshotRecord:
    id: str
    dataset_id: str
    version: int
    note: str
    row_hash: str
    row_ids: list[str]
    row_count: int
    created_at: str


@dataclass
class CaptureResult:
    dataset: DatasetRecord
    rows_captured: int
    rows_skipped: int
    skip_reasons: list[str] = field(default_factory=list)
```

### 9.4 Core algorithms

#### 9.4.1 Capture from runs

```python
def capture_from_runs(
    conn: sqlite3.Connection,
    dataset_id: str,
    *,
    since: datetime,
    until: datetime,
    profile: str | None = None,
    status: str = "completed",
    limit: int = 100,
) -> tuple[list[DatasetRow], list[str]]:
    """
    Query runs+steps for candidate rows.
    Returns (rows_to_insert, skip_reasons).
    """
    params: list[Any] = [since.isoformat(), until.isoformat()]
    where_clauses = [
        "r.created_at >= ?",
        "r.created_at <= ?",
    ]
    if status != "all":
        where_clauses.append("r.status = ?")
        params.append(status)
    if profile:
        where_clauses.append("r.master_profile = ?")
        params.append(profile)
    params.append(limit)

    sql = f"""
        SELECT
            r.id        AS run_id,
            r.prompt    AS input,
            r.master_profile AS profile,
            r.created_at    AS captured_at,
            (
                SELECT s.output FROM steps s
                WHERE s.run_id = r.id AND s.role = 'assistant'
                ORDER BY s.id DESC LIMIT 1
            ) AS expected_output
        FROM runs r
        WHERE {' AND '.join(where_clauses)}
        ORDER BY r.created_at DESC
        LIMIT ?
    """

    rows: list[DatasetRow] = []
    skipped: list[str] = []

    for rec in conn.execute(sql, params):
        if not rec["input"] or not rec["input"].strip():
            skipped.append(f"run {rec['run_id']}: empty input prompt")
            continue
        if not rec["expected_output"] or not rec["expected_output"].strip():
            skipped.append(f"run {rec['run_id']}: no assistant output step found")
            continue
        rows.append(DatasetRow.new(
            dataset_id=dataset_id,
            input=rec["input"],
            expected_output=rec["expected_output"],
            source_run_id=rec["run_id"],
            metadata={
                "profile": rec["profile"],
                "captured_at": rec["captured_at"],
            },
        ))

    return rows, skipped
```

#### 9.4.2 Snapshot creation and row hash

```python
def create_snapshot(
    conn: sqlite3.Connection,
    dataset: DatasetRecord,
    note: str = "",
) -> SnapshotRecord:
    """
    Create an immutable snapshot of the current live dataset.
    The snapshot stores the list of row IDs and a content hash.
    """
    rows = fetch_all_rows(conn, dataset.id)
    row_ids = [r.id for r in rows]

    # Deterministic content hash: sort by row id to eliminate insertion-order variance
    sorted_fingerprints = sorted(r.content_fingerprint() for r in rows)
    combined = "\n".join(sorted_fingerprints)
    row_hash = "sha256:" + hashlib.sha256(combined.encode("utf-8")).hexdigest()

    # Next version number
    cur = conn.execute(
        "SELECT COALESCE(MAX(version), 0) + 1 FROM eval_dataset_snapshots WHERE dataset_id = ?",
        (dataset.id,),
    )
    version = cur.fetchone()[0]

    snap = SnapshotRecord(
        id=f"snap_{uuid.uuid4().hex[:10]}",
        dataset_id=dataset.id,
        version=version,
        note=note,
        row_hash=row_hash,
        row_ids=row_ids,
        row_count=len(row_ids),
        created_at=utc_now(),
    )

    conn.execute(
        """
        INSERT INTO eval_dataset_snapshots
          (id, dataset_id, version, note, row_hash, row_ids_json, row_count, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        """,
        (snap.id, snap.dataset_id, snap.version, snap.note,
         snap.row_hash, json.dumps(snap.row_ids), snap.row_count, snap.created_at),
    )
    conn.commit()
    return snap
```

#### 9.4.3 Secret scanning

```python
def scan_for_secrets(rows: list[DatasetRow]) -> list[tuple[str, str, str]]:
    """
    Returns list of (row_id, field_name, matched_pattern) for any row that
    matches a sensitive pattern in input or expected_output.
    """
    findings: list[tuple[str, str, str]] = []
    for row in rows:
        for field_name in ("input", "expected_output"):
            text = getattr(row, field_name)
            for pat in SENSITIVE_PATTERNS:
                if pat.search(text):
                    findings.append((row.id, field_name, pat.pattern))
                    break  # one report per (row, field)
    return findings
```

#### 9.4.4 JSONL streaming export

```python
def export_jsonl(
    conn: sqlite3.Connection,
    dataset_id: str,
    snapshot_row_ids: list[str] | None,
    out,  # file-like object or sys.stdout
    batch_size: int = 100,
) -> int:
    """
    Stream rows from SQLite to `out` as JSONL, one row per line.
    Returns the number of rows written.
    """
    if snapshot_row_ids is not None:
        placeholders = ",".join("?" * len(snapshot_row_ids))
        sql = f"""
            SELECT id, input, expected_output, source_run_id, metadata_json, created_at
            FROM eval_dataset_rows
            WHERE id IN ({placeholders})
            ORDER BY id
        """
        params = snapshot_row_ids
    else:
        sql = """
            SELECT id, input, expected_output, source_run_id, metadata_json, created_at
            FROM eval_dataset_rows
            WHERE dataset_id = ?
            ORDER BY created_at ASC, id ASC
        """
        params = [dataset_id]

    cursor = conn.execute(sql, params)
    written = 0
    while True:
        batch = cursor.fetchmany(batch_size)
        if not batch:
            break
        for rec in batch:
            meta = json.loads(rec["metadata_json"] or "{}")
            if rec["source_run_id"]:
                meta["source_run_id"] = rec["source_run_id"]
            obj = {
                "id": rec["id"],
                "input": rec["input"],
                "expected_output": rec["expected_output"],
                "metadata": meta,
            }
            out.write(json.dumps(obj, ensure_ascii=False) + "\n")
            written += 1
    return written
```

### 9.5 Integration point: `cmd_eval_dataset` in `controller.py`

A new top-level dispatch function `cmd_eval_dataset(args)` is added to `controller.py`, following the existing pattern of `cmd_eval`, `cmd_runs`, `cmd_queue`, etc. The function:

1. Loads config with `load_config()`.
2. Opens the database with `open_db(cfg)` — this also runs the DDL migration for new tables.
3. Dispatches on `args.dataset_subcommand` to one of: `create`, `list`, `show`, `snapshot`, `export`, `import`, `add`, `delete`.
4. Imports from `tag.eval_datasets` lazily inside the dispatch branch.
5. Closes the DB connection on all exit paths.

The `tag eval dataset` subcommand tree is registered in the CLI argument parser under the existing `eval` subparser group.

### 9.6 `tag eval run` integration

The existing `cmd_eval` function in `controller.py` is extended with a `--dataset` argument:

```python
# Pseudocode for dataset resolution in cmd_eval (controller.py)

dataset_ref = getattr(args, "dataset", None)  # e.g. "my-golden@v2" or "my-golden"
if dataset_ref:
    from tag.eval_datasets import resolve_dataset_cases
    # Returns list of dicts compatible with PRD-027 case format
    cases = resolve_dataset_cases(db, dataset_ref)
    dataset_id, dataset_version = parse_dataset_ref(dataset_ref, db)
else:
    cases = suite.get("cases", [])
    dataset_id = dataset_version = None
```

The `resolve_dataset_cases` function loads rows from the live dataset or a specific snapshot and converts them to the dict format expected by `score_case` and `record_case_result` in `eval_framework.py`. Each case dict has: `id` (row ID), `input`, `expected_output`.

The `record_case_result` call is extended to pass `dataset_id` and `dataset_version` when available, which are stored in the `eval_cases` table via an additive column migration.

### 9.7 Duration parsing

```python
import re
from datetime import datetime, timedelta, timezone

_DURATION_RE = re.compile(r'^(\d+)(d|h|m|s)$')

def parse_since(value: str) -> datetime:
    """
    Parse --since / --until values.
    Accepts: '7d', '24h', '30m', '60s', ISO date '2026-01-01', ISO datetime.
    Returns UTC datetime.
    """
    now = datetime.now(timezone.utc)
    m = _DURATION_RE.match(value)
    if m:
        n, unit = int(m.group(1)), m.group(2)
        delta = {"d": timedelta(days=n), "h": timedelta(hours=n),
                 "m": timedelta(minutes=n), "s": timedelta(seconds=n)}[unit]
        return now - delta
    # Try ISO date or datetime
    for fmt in ("%Y-%m-%d", "%Y-%m-%dT%H:%M:%S", "%Y-%m-%dT%H:%M:%SZ"):
        try:
            return datetime.strptime(value, fmt).replace(tzinfo=timezone.utc)
        except ValueError:
            continue
    raise ValueError(
        f"Cannot parse duration '{value}'. Use formats like '7d', '24h', '2026-01-01'."
    )
```

---

## 10. Security Considerations

1. **Secret scanning at export and import:** Both `export` and `import` scan all row `input` and `expected_output` fields against `SENSITIVE_PATTERNS` from PRD-034 before writing to file or SQLite. The same patterns used by the existing secret scanner (`security.py`) are reused by importing from that module. Positive matches block the operation; the error message names the row ID and which field triggered the match so the user can sanitize the dataset. The `--allow-secrets` bypass is logged to stderr even when used.

2. **JSONL file path traversal:** The `--file` and `--output` paths in import/export are resolved to absolute paths using `Path.resolve()`. Resolved paths that point outside the user's home directory trigger a warning. Paths containing null bytes are rejected with exit code 1.

3. **SQL injection via dataset names and tags:** All user-supplied values (dataset name, tag keys/values, run IDs) are passed to SQLite via parameterized queries (`conn.execute(sql, params)`) — never via string interpolation. Tag-based filtering uses `json_extract()` with parameterized values.

4. **Row content from production runs:** Captured runs may contain sensitive file contents, tokens, or PII that were part of the agent's working context. The secret scan at capture time (FR-06 applied at export time) is a last-resort check; operators are encouraged to capture from sandboxed or anonymized profiles. The `--profile` filter helps restrict capture to less-sensitive workloads.

5. **Dataset deletion and referential integrity:** Deleting a dataset does not cascade to `eval_results` rows that reference it (by `dataset_id`). The FK relationship is informational only; `eval_results.dataset_id` may become a dangling reference after dataset deletion. `tag eval history` handles null lookups gracefully and displays `(deleted)` for the dataset name.

6. **Snapshot row hash verification:** `tag eval dataset show my-golden@v1` recomputes the hash of the rows currently referenced by the snapshot and compares it to the stored `row_hash`. A mismatch indicates database corruption or tampering and is reported as `Warning: snapshot hash mismatch. Expected sha256:... got sha256:.... The snapshot may have been modified.`

7. **Import from untrusted JSONL files:** Imported `metadata` JSON is stored verbatim. Malicious metadata could contain very large strings. The importer enforces a maximum per-row size: if `len(json.dumps(row_obj))` exceeds 1 MB, the row is skipped with a warning. This prevents disk exhaustion from adversarially crafted import files.

---

## 11. Testing Strategy

### 11.1 Unit tests (`tests/test_eval_datasets.py`)

- **Name validation:** `DatasetRecord.new("invalid name!")` raises `ValueError` matching `DATASET_NAME_RE`.
- **Capture query correctness:** Seed a `runs`/`steps` table with 20 rows (10 within time window, 5 with wrong profile, 5 with empty output). Assert `capture_from_runs` returns exactly 10 rows and 5 skip reasons.
- **Snapshot hash determinism:** Create a dataset with 3 rows, call `create_snapshot` twice (no mutations between calls). Assert both snapshots have identical `row_hash`.
- **Snapshot immutability:** Snapshot at v1 with 3 rows. Add 2 more rows. Snapshot at v2 with 5 rows. Assert `fetch_rows_for_snapshot(conn, snap_v1)` still returns exactly 3 rows.
- **Secret scan:** Create a row with `input = "my key is sk-abc123def456ghi789jkl012mno345"`. Assert `scan_for_secrets([row])` returns one finding for `("dr_xxx", "input", "sk-[A-Za-z0-9]{32,}")`.
- **Duration parsing:** Parameterized tests for `'7d'` (now - 7 days), `'24h'` (now - 24 hours), `'2026-01-01'` (fixed date), invalid `'7days'` (raises `ValueError`).
- **JSONL round-trip:** Write 10 rows to a `StringIO` via `export_jsonl`, then parse each line with `json.loads`. Assert all fields present and values match original row data.

### 11.2 Integration tests (`tests/test_eval_datasets_integration.py`)

- **Full create-and-list cycle:** `create_dataset("test-ds")` → `add_rows(3)` → `list_datasets()` → assert `test-ds` appears with `row_count = 3`.
- **Export-import round-trip:** Create dataset with 5 rows → export to temp JSONL → import to new dataset `imported` → assert both datasets have identical `input`/`expected_output` values for all rows (order-insensitive).
- **Snapshot version sequence:** Create dataset → snapshot (v1) → add row → snapshot (v2) → assert v2 has `row_count = v1.row_count + 1`, `v2.version = 2`, `v2.row_hash != v1.row_hash`.
- **`--from-runs` end-to-end:** Seed 10 `runs` + `steps` rows in a test DB → call `cmd_eval_dataset_create` with `--from-runs --limit 5` → assert dataset has 5 rows, each referencing a valid `source_run_id`.
- **Delete with referential guard:** Create dataset → snapshot v1 → insert a fake `eval_results` row referencing `dataset_version = 1` → attempt `delete my-ds@v1` without `--force` → assert exit code 1, no rows deleted.
- **Secret scan blocks export:** Create dataset with a secret in row input → call export → assert exit code 1 and error message contains the row ID and pattern.

### 11.3 Performance tests

- **Large capture:** Seed 100k `runs` rows in WAL-mode SQLite. Time `capture_from_runs` with `--limit 10000`. Assert completion in under 30 seconds. Assert the query plan uses an index on `runs.created_at` (`EXPLAIN QUERY PLAN` check).
- **Large export streaming:** Dataset with 10k rows. Time `export_jsonl` to `/dev/null`. Assert completion in under 10 seconds. Assert peak RSS memory increase is under 50 MB (no full load into memory).

### 11.4 CLI smoke tests

```bash
# Create from runs (requires seeded DB)
tag eval dataset create smoke-test --from-runs --since 30d --limit 5
tag eval dataset list
tag eval dataset show smoke-test
tag eval dataset snapshot smoke-test --note "CI smoke snapshot"
tag eval dataset export smoke-test --format jsonl --output /tmp/smoke.jsonl
tag eval dataset import --file /tmp/smoke.jsonl --name smoke-imported
tag eval dataset delete smoke-imported --yes
tag eval dataset delete smoke-test --yes
```

---

## 12. Acceptance Criteria

| ID | Criterion | How to Verify |
|----|-----------|---------------|
| AC-01 | `tag eval dataset create my-golden --from-runs --since 7d --limit 50` completes without error, creates a dataset row in `eval_datasets`, and writes up to 50 rows to `eval_dataset_rows`. | Query `SELECT row_count FROM eval_datasets WHERE name='my-golden'`; assert `row_count <= 50`. |
| AC-02 | `tag eval dataset create my-golden` when `my-golden` already exists exits 1 with `Error: dataset 'my-golden' already exists`. | Run create twice; assert second invocation exits 1 and error message matches. |
| AC-03 | `tag eval dataset list --json` returns a valid JSON array where each element has keys: `id`, `name`, `row_count`, `version_count`, `tags`, `created_at`. | `json.loads` the output; assert schema. |
| AC-04 | `tag eval dataset show my-golden` displays row `input` truncated to 80 characters in TTY mode and full content in `--json` mode. | Compare TTY output column width; compare JSON output field length to source row. |
| AC-05 | `tag eval dataset snapshot my-golden --note "test"` creates a `v1` snapshot (or next version number) with matching `row_count` and a non-null `row_hash` beginning with `sha256:`. | Query `eval_dataset_snapshots WHERE dataset_id = (SELECT id FROM eval_datasets WHERE name='my-golden')`; assert `version = 1`, `row_hash LIKE 'sha256:%'`. |
| AC-06 | Adding a row after snapshot v1 and taking snapshot v2 produces `v2.row_count = v1.row_count + 1` and `v2.row_hash != v1.row_hash`. | Automated integration test assertion. |
| AC-07 | `tag eval dataset export my-golden --format jsonl --output /tmp/out.jsonl` writes exactly `row_count` lines to the file, each parseable by `json.loads` with keys `id`, `input`, `expected_output`, `metadata`. | Count lines in output file; assert equals `row_count`. `json.loads` each line. |
| AC-08 | `tag eval dataset import --file /tmp/out.jsonl --name imported` creates a new dataset with the same number of rows, with `input` and `expected_output` values byte-identical to the exported values. | Export → import → re-export; diff the two JSONL files on `input`+`expected_output` fields. |
| AC-09 | `tag eval dataset export my-secret-ds` where a row contains `sk-abc123...` (32+ chars) exits 1 with an error naming the row ID and the matched pattern. | Inject a secret into a row; run export; assert exit 1 and error message matches. |
| AC-10 | `tag eval dataset export my-secret-ds --allow-secrets` with the same secret row exports successfully. | Run export with `--allow-secrets`; assert exit 0 and output line count matches row count. |
| AC-11 | `tag eval dataset show my-golden@v1` returns only the rows that were present at snapshot v1, even if additional rows have been added to the live dataset since. | Snapshot at v1 (3 rows) → add 2 rows → `show my-golden@v1 --json` → assert 3 rows returned. |
| AC-12 | `tag eval dataset delete my-golden@v1 --yes` removes the snapshot from `eval_dataset_snapshots` but leaves the live dataset and v2 snapshot intact. | After delete, query `eval_dataset_snapshots`; assert v1 row absent, v2 row present, `eval_datasets` row present. |
| AC-13 | `tag eval dataset delete my-golden@v1` (without `--force`) when `eval_results` rows reference `dataset_version = 1` exits 1 with a message citing the number of referencing rows. | Seed `eval_results` with `dataset_version = 1`; run delete without `--force`; assert exit 1 and message contains the count. |
| AC-14 | `tag eval run --suite evals/coding.yaml --dataset my-golden@v1 --dry-run` prints the count of cases from the v1 snapshot, exits 0, and makes no SQLite writes to `eval_results`. | Run with `--dry-run`; assert output mentions v1 row count; query `eval_results`; assert no new rows. |
| AC-15 | `tag eval dataset add my-golden --run-id run_nonexistent` exits 1 with `Error: run 'run_nonexistent' not found.` | Run with a bogus run ID; assert exit 1 and error message. |
| AC-16 | `tag eval dataset create bad name!` exits 1 with an error explaining the valid name format. | Run with invalid name; assert exit 1 and message references `[a-zA-Z0-9_-]`. |
| AC-17 | All `tag eval dataset` subcommands run without importing `deepeval`. | `python -c "import sys; sys.modules['deepeval'] = None; from tag import eval_datasets"` succeeds (or equivalent importguard test). |

---

## 13. Dependencies

| Dependency | Type | Version / Notes |
|------------|------|-----------------|
| PRD-027 (eval framework) | Internal — required | Provides `eval_runs`, `eval_cases`, `eval_framework.py`, and `cmd_eval` entrypoint. Dataset integration extends `cmd_eval` with `--dataset` flag. `eval_results` table gains `dataset_id`/`dataset_version` columns via migration. |
| PRD-013 (agent tracing) | Internal — required | Provides the `runs` and `steps` tables that `--from-runs` queries. Without PRD-013's tables, `--from-runs` returns zero rows; the rest of the dataset system functions independently. |
| PRD-034 (secret scanning) | Internal — soft dependency | `SENSITIVE_PATTERNS` from `security.py` is imported for export/import scanning. If `security.py` patterns are not available, a local fallback pattern set is used. |
| PRD-028 (sandbox) | Internal — informational | Operators should use sandboxed profiles for eval data capture to limit tool surface. No code dependency. |
| PRD-012 (cost tracking) | Internal — informational | `metadata_json` on captured rows may include `cost_usd` from the source run if PRD-012 cost columns are populated in the `steps` table. |
| `hashlib` | Python stdlib | SHA-256 for snapshot row hash. No new install required. |
| `json` | Python stdlib | JSONL serialization/deserialization. |
| `re` | Python stdlib | Duration parsing and secret scanning patterns. |
| `uuid` | Python stdlib | ID generation for dataset, row, and snapshot records. |
| SQLite WAL mode | Runtime — existing | `open_db()` already enables WAL; no additional configuration required. |

---

## 14. Open Questions

| ID | Question | Impact | Owner |
|----|----------|--------|-------|
| OQ-01 | **Should `--from-runs` include failed runs?** Currently the default is `--status completed`. Failed runs may contain interesting edge cases useful for regression testing, but their `expected_output` (the failed output) may not be suitable as ground truth. Recommendation: default `completed`; add `--status failed` for explicit failure-case capture, with a CLI warning that `expected_output` is from the failed run. | Dataset quality | Product |
| OQ-02 | **Snapshot storage: row ID list vs. full row copy?** The current design stores `row_ids_json` in the snapshot and re-fetches row content from `eval_dataset_rows` at read time. This means if a row is deleted from the live dataset, the snapshot's rows will be missing. Alternative: deep-copy row content into a `eval_dataset_snapshot_rows` table at snapshot time. Deep copy is safer for immutability but doubles storage. Recommendation: use deep copy if the `eval_dataset_rows` table supports `ON DELETE CASCADE` (which it currently does), since live row deletion would break snapshot reads. | Immutability guarantee | Engineering |
| OQ-03 | **Row deduplication on capture?** If `--from-runs` is run twice in the same time window, the same run could be captured twice into the dataset. Should the system check `source_run_id` for duplicates and skip already-captured runs? Recommendation: yes, add `UNIQUE(dataset_id, source_run_id)` constraint on `eval_dataset_rows` and skip on conflict with a warning. | Data quality | Engineering |
| OQ-04 | **JSONL import: should `id` field in source JSONL be preserved or regenerated?** Preserving the source `id` enables re-import idempotency (re-importing the same file does not create duplicates if `UNIQUE(dataset_id, id)` is enforced). Regenerating ensures no collisions. Recommendation: preserve `id` from source JSONL if it matches the `dr_` prefix format; regenerate otherwise. | Import idempotency | Engineering |
| OQ-05 | **`tag eval run` case ordering with `--dataset`?** When loading rows from a dataset, should they be presented to the eval runner in insertion order (chronological) or randomized? Randomization reduces order-dependent bias in LLM-judge scoring but makes results less reproducible. Recommendation: default insertion order; add `--shuffle-cases` flag to `tag eval run` for randomization. | Eval reproducibility | Product |
| OQ-06 | **Should `--from-runs` support a `--filter-prompt <regex>` flag?** Engineers may want to capture only runs whose prompts match a specific pattern (e.g., only coding-related prompts). A regex filter on `r.prompt` would add this capability without requiring manual curation. Impact: minor implementation effort, significant usability improvement for targeted dataset building. | Capture precision | Engineering |
| OQ-07 | **Maximum dataset size enforcement?** Should there be a hard limit on the total number of rows per dataset (e.g., 50,000) to prevent unbounded disk usage? Or should we rely on the `--limit` flag per-capture? Recommendation: no hard cap at the dataset level; add a `warn_at_row_count` config key (`eval.dataset_warn_rows`, default 10000) that prints a warning when exceeded. | Disk usage | Engineering |

---

## 15. Complexity and Timeline

**Estimated Effort:** S (3-5 days)
**Complexity:** 2/5
**Risk Level:** Low — purely additive; no modifications to existing tables (only new tables + additive column migrations)

| Phase | Tasks | Days |
|-------|-------|------|
| **Phase 1: Schema + core CRUD** (Days 1–2) | DDL for `eval_datasets`, `eval_dataset_rows`, `eval_dataset_snapshots`; `DatasetRow`, `DatasetRecord`, `SnapshotRecord` dataclasses; `create_dataset`, `add_rows`, `fetch_rows`, `delete_dataset`; migration for `eval_results.dataset_id` column; `cmd_eval_dataset` dispatch stub in `controller.py`; unit tests for name validation, duration parsing, row insertion | 2 |
| **Phase 2: Capture + snapshot + CLI** (Days 2–3) | `capture_from_runs` query with full filter support; `create_snapshot` with row hash; `tag eval dataset create`, `list`, `show`, `snapshot`, `add`, `delete` CLI commands; TTY vs. JSON rendering; integration test for create-list-show cycle; snapshot immutability integration test | 1.5 |
| **Phase 3: Export + import + secret scan** (Days 3–4) | `export_jsonl`, `export_csv`; streaming read with `fetchmany`; `import_jsonl`; `scan_for_secrets` with PRD-034 pattern import; `--allow-secrets` flag; export-import round-trip integration test; secret-blocked export test | 1 |
| **Phase 4: `tag eval run` integration + polish** (Days 4–5) | `resolve_dataset_cases` function; `--dataset` flag on `tag eval run`; version reference parsing (`my-golden@v2`); `dataset_id`/`dataset_version` on eval result rows; `--dry-run` dataset case count display; performance test for 10k-row export; CLI smoke test suite; documentation | 0.5 |

**Total: 5 days**

**Risks:**

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| PRD-027 `eval_framework.py` case format changes break `resolve_dataset_cases` | Low | Medium | Pin to the `cases` dict format defined in PRD-027 §8.3; write an integration test that runs the full pipeline |
| `runs` table has no index on `created_at`, making `--from-runs` slow on large DBs | Low | Medium | Add `CREATE INDEX IF NOT EXISTS idx_runs_created_at ON runs(created_at)` in the dataset schema migration |
| Snapshot deep-copy vs. row-ID reference decision (OQ-02) delays Phase 1 | Medium | Low | Default to row-ID reference for Phase 1; track deep-copy as a follow-up if snapshot reads break on row deletion |
| Secret scanning regex false positives block legitimate datasets | Low | Low | Provide `--allow-secrets` bypass; document which patterns are checked so engineers can anticipate false positives |
