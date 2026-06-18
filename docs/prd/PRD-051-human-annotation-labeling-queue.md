# PRD-051: Human Annotation and Labeling Queue (`tag annotate`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (5-8 days)
**Category:** Evaluation & Observability
**Affects:** `annotation_queue SQLite table + controller.py`
**Depends on:** PRD-027 (eval framework — eval_runs/eval_cases), PRD-013 (agent tracing — runs/steps), PRD-049 (versioned eval datasets — eval_datasets table), PRD-044 (AgentOps session observability)
**Inspired by:** Argilla, Scale AI labeling, LangSmith human annotation, Snorkel labeling functions

---

## 1. Overview

LLM-as-judge scoring (PRD-045) automates run quality assessment, but certain failure modes require human judgment: subtle factual errors, domain-specific correctness, safety-adjacent outputs, and edge cases where the judge model itself is unreliable. TAG currently has no first-class mechanism to route runs to human reviewers, collect their labels, or incorporate those labels into eval datasets (PRD-049) or RLHF feedback pipelines.

Human Annotation and Labeling Queue (`tag annotate`) introduces a structured workflow for routing agent runs to human reviewers, collecting binary/categorical/freeform labels, and exporting labeled data as JSONL for downstream eval or fine-tuning use. Reviewers access their queue via `tag annotate review` — a terminal-friendly TUI that presents run outputs one at a time with context — and submit labels via keyboard shortcuts. Queue items are assigned, tracked for completion, and aggregated into consensus labels when multiple reviewers annotate the same item.

The design is inspired by Argilla's dataset annotation workflow (queue assignment, annotation tasks, label schemas), LangSmith's human annotation (thumbs up/down, freeform notes, dataset attachment), and Scale AI's labeling pipeline concepts (assignment, inter-annotator agreement, review). TAG's implementation is entirely local-first — annotation tasks live in SQLite, the review TUI runs in the terminal, and export produces standard JSONL compatible with PRD-049.

---

## 2. Problem Statement

### 2.1 LLM judges are unreliable for safety-critical evaluation

PRD-045 uses LLM-as-judge scoring for eval cases, but judge models have known failure modes: sycophancy, position bias, and poor calibration on domain-specific correctness. For medical, legal, or security-adjacent use cases, human review of at least a sample of runs is required for trustworthy quality metrics.

### 2.2 No feedback loop from production to eval datasets

Engineers observe interesting or problematic runs in `tag runs show` but have no workflow to add them to an eval dataset (PRD-049). The annotation queue bridges this gap: a flagged run goes into the queue, gets human-labeled, and is promoted to a dataset with `tag annotate export --to-dataset`.

### 2.3 Multi-reviewer consensus is manual

When two engineers independently review the same run, reconciling their labels requires manual comparison. There is no support for inter-annotator agreement metrics or label aggregation.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | `tag annotate add` routes one or more run IDs (or all runs matching a filter) to the annotation queue, creating queue items with an optional task description and label schema. |
| G2 | `tag annotate review` presents a TUI queue review interface: run output, task instructions, label choices; accepts keyboard input for label submission. |
| G3 | Persist labels in the `annotation_labels` SQLite table with reviewer identity, timestamp, and optional freeform comment. |
| G4 | `tag annotate export` generates JSONL from labeled items, optionally filtered by label value, consensus level, or task name; compatible with PRD-049 dataset import. |
| G5 | Support binary (correct/incorrect), categorical (multi-class), rating (1-5), and freeform label schemas. |
| G6 | Compute inter-annotator agreement (Cohen's κ for binary; Fleiss' κ for multi-class) when multiple reviewers label the same item. |
| G7 | `tag annotate stats` reports queue depth, completion rate, label distribution, and IAA metrics per task. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Web-based annotation interface. Terminal TUI only. |
| NG2 | Crowd-sourcing or external annotator management. All reviewers are local system users. |
| NG3 | Active learning or uncertainty sampling for queue prioritization. |
| NG4 | RLHF fine-tuning pipeline. Export provides the data; fine-tuning is out of scope. |
| NG5 | Real-time collaborative annotation (multiple reviewers simultaneously on same item). |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Review throughput | Reviewer labels 10 items/minute in TUI mode | Manual timing benchmark |
| Label persistence | Zero data loss on TUI crash mid-session; partial labels persisted per item | Fault injection test |
| Export fidelity | JSONL export passes PRD-049 dataset import validation | Integration test |
| IAA computation | Cohen's κ computed correctly for 3 known-answer test cases | Unit test |
| Queue add latency | `tag annotate add --from-runs --since 1h` populates queue in < 2s for 100 runs | Benchmark test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | ML engineer | Add the last 50 runs from my eval profile to the annotation queue | I can review them for quality and build a golden dataset |
| US2 | Domain expert | Review runs in a TUI and mark them as correct/incorrect with a comment | I provide ground-truth labels for eval |
| US3 | Team lead | Export labeled data as JSONL for import into the eval dataset | I can build a human-curated golden set |
| US4 | QA engineer | See inter-annotator agreement metrics for a labeling task | I can assess label quality before trusting the dataset |
| US5 | ML engineer | Filter the review queue to show only low-confidence LLM-judge items | I focus human review on uncertain cases |

---

## 6. CLI Surface

```
tag annotate <subcommand> [options]

Subcommands:
  add        Add runs to the annotation queue
  review     Open the TUI queue reviewer
  list       List queue items and their status
  show       Show a single queue item and its labels
  label      Submit a label for an item non-interactively
  export     Export labeled items as JSONL
  stats      Show queue statistics and IAA metrics
  clear      Remove completed items from queue

tag annotate add \
  --run-ids <id1,id2,...> | --from-runs [--since DURATION] [--profile PROFILE] [--limit N] \
  --task "Review output quality" \
  --schema binary | categorical:<A,B,C> | rating | freeform \
  [--assignee USERNAME]

tag annotate review \
  [--task TASK_NAME] \
  [--assignee USERNAME] \
  [--filter pending|all]

tag annotate label <item-id> \
  --value <label-value> \
  [--comment TEXT] \
  [--reviewer USERNAME]

tag annotate export \
  [--task TASK_NAME] \
  [--label <value>] \
  [--min-reviews N] \
  [--consensus-only] \
  --output <file.jsonl> | --to-dataset <name>

tag annotate stats [--task TASK_NAME]

Options (review TUI keybindings):
  j/k         Navigate items
  1-5         Rate (rating schema) or select nth categorical option
  y/n         Binary label (correct/incorrect)
  c           Add comment
  s           Skip item
  q           Quit (saves progress)
  ?           Show help
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `tag annotate add` inserts one `annotation_queue_items` row per run, with status `pending`, task name, schema, and optional assignee. |
| FR-02 | `tag annotate review` fetches pending items assigned to the current user (or unassigned), claims each item (status `in_progress`), and presents it in the TUI. |
| FR-03 | On label submission in TUI: insert `annotation_labels` row, update item status to `labeled`, and advance to next item without re-querying. |
| FR-04 | On TUI crash or quit mid-item: revert claimed item status to `pending` so another session can pick it up. |
| FR-05 | Binary schema presents "y=correct / n=incorrect" prompt; categorical presents numbered options; rating presents 1-5 numeric prompt; freeform opens line editor. |
| FR-06 | `tag annotate export --consensus-only` only includes items where ≥ `min-reviews` labels agree on the same value (majority vote). |
| FR-07 | `tag annotate export --to-dataset NAME` calls PRD-049 dataset import internally, creating or appending a dataset. |
| FR-08 | IAA: when ≥ 2 reviewers label the same binary item, compute Cohen's κ; for multi-class, Fleiss' κ. Store in `annotation_tasks` table. |
| FR-09 | `tag annotate list` shows item ID, run ID, task, assignee, status, label count, and consensus label. |
| FR-10 | `tag annotate stats` shows: total items, pending/in_progress/labeled counts, label distribution, IAA score, mean time to label. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | TUI must render in any terminal with at least 80×24 characters; use `rich` or `prompt_toolkit` for rendering. |
| NFR-02 | All label writes use SQLite transactions; partial session loss must not corrupt queue state. |
| NFR-03 | Queue item fetch uses `SELECT ... FOR UPDATE` equivalent (SQLite advisory lock via `BEGIN IMMEDIATE`) to prevent double-assignment. |
| NFR-04 | `tag annotate add --from-runs --limit 1000` must complete in < 10s. |
| NFR-05 | TUI must not block on SQLite writes longer than 100ms; use deferred commit batching. |

---

## 9. Technical Design

### 9.1 Target files

| File | Change |
|------|--------|
| `src/tag/annotation_queue.py` | New module: `AnnotationQueue`, `ReviewTUI`, IAA computations |
| `src/tag/controller.py` | Add `cmd_annotate` entrypoint; register `annotate` subparser |

### 9.2 SQLite DDL

```sql
CREATE TABLE IF NOT EXISTS annotation_tasks (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  description TEXT,
  schema      TEXT NOT NULL DEFAULT 'binary',  -- 'binary'|'categorical'|'rating'|'freeform'
  schema_opts TEXT,  -- JSON: {"options": ["A","B","C"]} for categorical
  iaa_score   REAL,
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS annotation_queue_items (
  id          TEXT PRIMARY KEY,
  task_id     TEXT NOT NULL REFERENCES annotation_tasks(id),
  run_id      TEXT NOT NULL,
  profile     TEXT,
  assignee    TEXT,
  status      TEXT NOT NULL DEFAULT 'pending',  -- 'pending'|'in_progress'|'labeled'|'skipped'
  claimed_at  TEXT,
  labeled_at  TEXT,
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS annotation_labels (
  id          TEXT PRIMARY KEY,
  item_id     TEXT NOT NULL REFERENCES annotation_queue_items(id),
  task_id     TEXT NOT NULL,
  reviewer    TEXT NOT NULL,
  value       TEXT NOT NULL,
  comment     TEXT,
  duration_s  REAL,
  created_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_annotation_items_status_assignee
  ON annotation_queue_items(status, assignee, task_id);
CREATE INDEX IF NOT EXISTS idx_annotation_labels_item
  ON annotation_labels(item_id, task_id);
```

### 9.3 Python core

```python
from __future__ import annotations
import dataclasses
import sqlite3
import uuid
from typing import List, Optional

@dataclasses.dataclass
class QueueItem:
    id: str
    task_id: str
    run_id: str
    profile: Optional[str]
    assignee: Optional[str]
    status: str

@dataclasses.dataclass
class AnnotationLabel:
    item_id: str
    reviewer: str
    value: str
    comment: Optional[str]

class AnnotationQueue:
    def __init__(self, conn: sqlite3.Connection) -> None:
        self.conn = conn

    def add_items(self, run_ids: List[str], task_name: str,
                  schema: str = "binary", assignee: Optional[str] = None) -> int:
        now = _utc_now()
        # Ensure task exists
        task = self.conn.execute(
            "SELECT id FROM annotation_tasks WHERE name=?", (task_name,)
        ).fetchone()
        if not task:
            task_id = uuid.uuid4().hex[:8]
            self.conn.execute(
                "INSERT INTO annotation_tasks(id,name,schema,created_at,updated_at) VALUES(?,?,?,?,?)",
                (task_id, task_name, schema, now, now)
            )
        else:
            task_id = task["id"]
        count = 0
        for run_id in run_ids:
            self.conn.execute(
                "INSERT OR IGNORE INTO annotation_queue_items"
                "(id,task_id,run_id,assignee,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?)",
                (uuid.uuid4().hex[:8], task_id, run_id, assignee, "pending", now, now)
            )
            count += 1
        self.conn.commit()
        return count

    def claim_next(self, task_name: str, reviewer: str) -> Optional[QueueItem]:
        with self.conn:
            row = self.conn.execute(
                "SELECT qi.* FROM annotation_queue_items qi "
                "JOIN annotation_tasks t ON t.id=qi.task_id "
                "WHERE t.name=? AND qi.status='pending' "
                "AND (qi.assignee IS NULL OR qi.assignee=?) "
                "ORDER BY qi.created_at LIMIT 1",
                (task_name, reviewer)
            ).fetchone()
            if not row:
                return None
            now = _utc_now()
            self.conn.execute(
                "UPDATE annotation_queue_items SET status='in_progress',assignee=?,claimed_at=?,updated_at=? WHERE id=?",
                (reviewer, now, now, row["id"])
            )
            return QueueItem(**{k: row[k] for k in ["id","task_id","run_id","profile","assignee","status"]})

    def submit_label(self, item_id: str, reviewer: str, value: str,
                     comment: Optional[str] = None, duration_s: float = 0.0) -> None:
        now = _utc_now()
        item = self.conn.execute("SELECT task_id FROM annotation_queue_items WHERE id=?", (item_id,)).fetchone()
        self.conn.execute(
            "INSERT INTO annotation_labels(id,item_id,task_id,reviewer,value,comment,duration_s,created_at) "
            "VALUES(?,?,?,?,?,?,?,?)",
            (uuid.uuid4().hex[:8], item_id, item["task_id"], reviewer, value, comment, duration_s, now)
        )
        self.conn.execute(
            "UPDATE annotation_queue_items SET status='labeled',labeled_at=?,updated_at=? WHERE id=?",
            (now, now, item_id)
        )
        self.conn.commit()

    def cohen_kappa(self, task_name: str) -> Optional[float]:
        labels = self.conn.execute(
            "SELECT al.item_id, al.reviewer, al.value "
            "FROM annotation_labels al "
            "JOIN annotation_tasks t ON t.id=al.task_id "
            "WHERE t.name=? AND t.schema='binary'",
            (task_name,)
        ).fetchall()
        # Build (item, reviewer) -> value map, compute κ
        from collections import defaultdict
        item_labels: dict = defaultdict(dict)
        for row in labels:
            item_labels[row["item_id"]][row["reviewer"]] = row["value"]
        pairs = [(v for v in d.values()) for d in item_labels.values() if len(d) >= 2]
        if not pairs:
            return None
        # Simplified 2-reviewer κ
        agree = sum(1 for d in item_labels.values()
                    if len(d) >= 2 and len(set(list(d.values())[:2])) == 1)
        total = sum(1 for d in item_labels.values() if len(d) >= 2)
        if total == 0:
            return None
        po = agree / total
        pe = 0.5  # random agreement for balanced binary
        return (po - pe) / (1 - pe) if pe < 1.0 else 1.0

def _utc_now() -> str:
    from datetime import datetime, timezone
    return datetime.now(timezone.utc).isoformat()
```

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Run output containing credentials shown in TUI | Truncate output display to 4000 chars; apply PRD-034 secret scanner before rendering |
| Reviewer impersonation (faking reviewer name) | Reviewer name defaults to `whoami`; warn if explicitly overridden |
| Label data export to unintended destinations | `--to-dataset` validates dataset name against SQLite; `--output` path checked for write permission |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | `add_items` idempotency, `claim_next` race condition (two concurrent claims on same item), `cohen_kappa` correctness against known test cases |
| Integration | Full annotation workflow: add → review → label → export → PRD-049 import |
| TUI | Keyboard navigation unit tests (simulated keypress → expected state transition) |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | `tag annotate add --from-runs --since 1h --task "quality-review" --schema binary` adds N items to the queue |
| AC-02 | `tag annotate review --task quality-review` presents items one by one; pressing `y` submits `correct`, pressing `n` submits `incorrect` |
| AC-03 | After labeling 5 items, `tag annotate stats --task quality-review` shows label distribution |
| AC-04 | `tag annotate export --task quality-review --output labeled.jsonl` produces valid JSONL importable by PRD-049 |
| AC-05 | IAA κ computed correctly when two reviewers label the same 10 items |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| PRD-027 eval framework | `eval_cases` table structure for export compatibility |
| PRD-049 eval datasets | Export target (`--to-dataset` flag) |
| PRD-034 secret scanning | Output redaction in TUI display |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should annotation items persist indefinitely or expire after N days? |
| OQ-02 | Should the TUI support rich markdown rendering of run outputs? |
| OQ-03 | Is consensus-based label aggregation by majority vote sufficient, or do we need a dedicated adjudicator role? |

---

## 15. Complexity & Timeline

**Complexity:** Medium (M)
**Estimated effort:** 5–8 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | SQLite DDL, `AnnotationQueue` core CRUD, unit tests | 2 |
| 2 | Review TUI (render, keyboard handling, progress persistence) | 2 |
| 3 | Export/import pipeline, `stats` command, IAA computation | 2 |
| 4 | CLI wiring, integration tests, TUI polish | 2 |
