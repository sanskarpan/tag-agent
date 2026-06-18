# PRD-116: MemEx Persistent Scratchpad (`tag scratchpad`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** S (3-5 days)
**Category:** Computer Use
**Affects:** `scratchpad.py + controller.py`
**Depends on:** PRD-065 (automatic post-run memory extraction), PRD-067 (hierarchical memory tiers)
**Inspired by:** Vannevar Bush's Memex, MemGPT external scratchpad, LangMem working memory, AgentOps session scratchpad

---

## 1. Overview

During complex multi-step agent tasks — writing a long document, debugging a codebase, conducting research — an agent accumulates working notes, hypotheses, partial results, and open questions that don't belong in long-term memory but are essential context for the current task. Today, this working context either bloats the context window (reducing effective capacity for new information) or is lost between tool calls.

MemEx Persistent Scratchpad (`tag scratchpad`) introduces a lightweight, first-class working memory buffer modeled after Vannevar Bush's Memex concept and MemGPT's external scratchpad design. Agents can write structured notes to a per-session scratchpad (SQLite-backed), read them back selectively, and accumulate task-specific context that persists between tool calls and LLM calls within a session. The scratchpad is distinct from long-term memory (PRD-065/067): it is ephemeral by session but persists across the tool calls within that session.

The scratchpad supports four note types: **hypothesis** (working theories), **finding** (facts discovered), **todo** (pending work), and **note** (freeform). Notes can be tagged, searched, and exported. The agent's system prompt automatically includes a summary of current scratchpad entries to keep context fresh without consuming the full context window.

---

## 2. Problem Statement

### 2.1 Context window consumed by working notes

When an agent writes working notes in the conversation history (as tool outputs or assistant turns), they consume context window capacity. A 60-step research task may accumulate 50k tokens of working notes, leaving only 10k tokens for new information.

### 2.2 No structured working memory between tool calls

Tool calls within a session share no persistent state. If tool call 1 discovers "the database uses PostgreSQL 14," tool call 3 cannot access that fact without it being re-stated in the context.

### 2.3 Working notes lost on task completion

When a session ends, valuable working notes (open questions, hypotheses, partial findings) are lost. They are not structured enough for long-term memory (PRD-065) but useful for resuming the task later.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | `tag scratchpad write --type hypothesis|finding|todo|note --text TEXT` adds a note to the current session's scratchpad. |
| G2 | `tag scratchpad read [--type TYPE] [--tags TAG,...]` retrieves scratchpad entries filtered by type and tags. |
| G3 | Scratchpad entries persisted in SQLite `scratchpad_entries` table; scoped to session ID. |
| G4 | Auto-injection: scratchpad summary (last 10 entries, 50 tokens each) injected into agent system prompt at session start. |
| G5 | `tag scratchpad export --session ID` exports session scratchpad as Markdown. |
| G6 | `tag scratchpad promote --to-memory --entry-id ID` promotes a scratchpad entry to long-term memory (PRD-065). |
| G7 | `tag scratchpad clear [--session ID]` removes all entries for a session. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Persistent cross-session scratchpad. Use PRD-065 memory extraction for long-term persistence. |
| NG2 | Collaborative scratchpad shared across multiple agents. Per-session only. |
| NG3 | Semantic search over scratchpad. Use PRD-066 hybrid memory search for semantic queries. |
| NG4 | Real-time sync to external note-taking apps. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Write latency | `tag scratchpad write` completes in < 10ms | Benchmark test |
| Read latency | `tag scratchpad read --type finding` returns in < 20ms for 100 entries | Benchmark test |
| Context injection overhead | Scratchpad summary adds < 200 tokens to system prompt | Token count assertion |
| Export quality | Exported Markdown is well-structured and readable | Manual review |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Agent | Write a hypothesis note after discovering a pattern | I track my working theory without consuming context |
| US2 | Agent | Read all todo notes at the start of each tool call | I don't forget pending work |
| US3 | Developer | Export the session scratchpad after a research session | I have a structured record of discoveries |
| US4 | Agent | Promote a key finding to long-term memory | I preserve it beyond the current session |

---

## 6. CLI Surface

```
tag scratchpad write \
  --text "The authentication service uses JWT with 1-hour expiry" \
  --type finding \
  [--tags "auth,security"] \
  [--session SESSION_ID]

tag scratchpad read \
  [--type hypothesis|finding|todo|note] \
  [--tags TAG,...] \
  [--session SESSION_ID] \
  [--limit N]

tag scratchpad list [--session SESSION_ID]
tag scratchpad show <entry-id>
tag scratchpad delete <entry-id>
tag scratchpad export [--session SESSION_ID] [--format markdown|json]
tag scratchpad promote <entry-id> [--tier recall|core]
tag scratchpad clear [--session SESSION_ID]

Note types:
  hypothesis   Working theory or assumption to be validated
  finding      Fact or discovery (confirmed)
  todo         Pending action item
  note         Freeform note
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `tag scratchpad write` inserts a row into `scratchpad_entries` with type, text, tags (comma-separated), and current session_id. |
| FR-02 | `tag scratchpad read` queries entries filtered by type and tags; returns ordered by created_at DESC. |
| FR-03 | Session ID defaults to the current active session (read from `~/.tag/state/current_session`); `--session` overrides. |
| FR-04 | Auto-injection: `cmd_run` in `controller.py` queries the last 10 scratchpad entries for the current session and prepends a "Working memory:" block to the system prompt. |
| FR-05 | `tag scratchpad export --format markdown` renders entries as `## Type\n- text (tags)` grouped by type. |
| FR-06 | `tag scratchpad promote <id>` calls PRD-065 memory extraction with the entry text as the source fact; removes entry from scratchpad. |
| FR-07 | `tag scratchpad clear` soft-deletes all entries for the session (sets `deleted_at`). |
| FR-08 | Tag filtering: `--tags auth,security` returns entries where any of the specified tags match. |
| FR-09 | `tag scratchpad list` renders a table: entry_id (short), type, text (truncated 80 chars), tags, created_at. |
| FR-10 | Scratchpad entries created/read via the agent's system context (tool calls) as well as via CLI. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | All scratchpad reads/writes complete in < 20ms (direct SQLite, no joins needed). |
| NFR-02 | Auto-injection adds < 500 tokens to system prompt for up to 20 entries. |
| NFR-03 | Scratchpad entries older than 90 days are automatically pruned on startup. |
| NFR-04 | `scratchpad_entries` indexed on `(session_id, type, created_at)`. |

---

## 9. Technical Design

### 9.1 SQLite DDL

```sql
CREATE TABLE IF NOT EXISTS scratchpad_entries (
  id          TEXT PRIMARY KEY,
  session_id  TEXT NOT NULL,
  type        TEXT NOT NULL DEFAULT 'note',  -- 'hypothesis'|'finding'|'todo'|'note'
  text        TEXT NOT NULL,
  tags        TEXT,  -- comma-separated
  promoted    INTEGER NOT NULL DEFAULT 0,
  created_at  TEXT NOT NULL,
  deleted_at  TEXT
);
CREATE INDEX IF NOT EXISTS idx_scratchpad_session_type
  ON scratchpad_entries(session_id, type, created_at DESC);
```

### 9.2 Python core

```python
from __future__ import annotations
import sqlite3
import uuid
from datetime import datetime, timezone
from typing import List, Optional

VALID_TYPES = frozenset({"hypothesis", "finding", "todo", "note"})

class Scratchpad:
    def __init__(self, conn: sqlite3.Connection) -> None:
        self.conn = conn

    def write(self, session_id: str, text: str, note_type: str = "note",
              tags: Optional[str] = None) -> str:
        if note_type not in VALID_TYPES:
            raise ValueError(f"Invalid type: {note_type}. Must be one of {VALID_TYPES}")
        entry_id = uuid.uuid4().hex[:8]
        now = datetime.now(timezone.utc).isoformat()
        self.conn.execute(
            "INSERT INTO scratchpad_entries(id,session_id,type,text,tags,created_at) VALUES(?,?,?,?,?,?)",
            (entry_id, session_id, note_type, text, tags, now)
        )
        self.conn.commit()
        return entry_id

    def read(self, session_id: str, note_type: Optional[str] = None,
             tags: Optional[str] = None, limit: int = 50) -> List[dict]:
        where = ["session_id=?", "deleted_at IS NULL"]
        params: list = [session_id]
        if note_type:
            where.append("type=?"); params.append(note_type)
        rows = self.conn.execute(
            f"SELECT * FROM scratchpad_entries WHERE {' AND '.join(where)} ORDER BY created_at DESC LIMIT ?",
            params + [limit]
        ).fetchall()
        if tags:
            tag_set = set(tags.split(","))
            rows = [r for r in rows if r["tags"] and tag_set & set(r["tags"].split(","))]
        return [dict(r) for r in rows]

    def build_context_summary(self, session_id: str, max_entries: int = 10) -> str:
        entries = self.read(session_id, limit=max_entries)
        if not entries:
            return ""
        lines = ["Working memory (scratchpad):"]
        for e in entries:
            tag_str = f" [{e['tags']}]" if e.get("tags") else ""
            lines.append(f"  [{e['type']}]{tag_str} {e['text'][:100]}")
        return "\n".join(lines)
```

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Sensitive data in scratchpad auto-injected into prompts | Auto-injection truncates entries to 100 chars; flag `--no-auto-inject` available |
| Session ID spoofing | Session ID validated against active sessions table before write |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | `write/read` round-trip; type validation; tag filtering; `build_context_summary` token count |
| Integration | Full session: write 10 entries, read back, export, promote to memory |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | `tag scratchpad write --type finding --text "JWT uses 1h expiry"` creates entry |
| AC-02 | `tag scratchpad read --type finding` returns the written entry |
| AC-03 | Auto-injection adds scratchpad summary to system prompt |
| AC-04 | `tag scratchpad export --format markdown` produces valid Markdown |
| AC-05 | `tag scratchpad promote <id>` creates a memory entry and removes from scratchpad |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| PRD-065 memory extraction | `promote` target |
| PRD-013 agent tracing | Session ID infrastructure |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should scratchpad entries have a priority/importance score for injection ordering? |
| OQ-02 | Should agents be able to delete specific entries (not just clear all)? |

---

## 15. Complexity & Timeline

**Complexity:** Small (S)
**Estimated effort:** 3–5 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | SQLite DDL, `Scratchpad` class, unit tests | 1 |
| 2 | CLI commands, export/promote | 1 |
| 3 | Auto-injection integration, integration tests | 1 |

