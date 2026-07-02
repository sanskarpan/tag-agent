# PRD-116: MemEx Persistent Scratchpad (`tag scratchpad`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** S (3-5 days)
**Category:** Computer Use
**Affects:** `internal/memory (Scratchpad) + internal/agent (auto-injection) + internal/cli (scratchpad command group)`
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
| FR-04 | Auto-injection: the agent loop in `internal/agent` queries the last 10 scratchpad entries for the current session at session start and prepends a "Working memory:" block to the system prompt. |
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

DB-neutral DDL; targets `modernc.org/sqlite` (pure-Go, CGO_ENABLED=0) in the single `tag.sqlite3` store owned by `internal/store`. Writes go through the single-writer + `gofrs/flock` atomic read-modify-write path.

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

### 9.2 Go core (`internal/memory`)

The Python `Scratchpad` class maps 1:1 to a Go struct holding the shared `*sql.DB` store handle (the same `modernc.org/sqlite` connection used by the rest of `internal/memory`). This is a clean mechanical port with no capability loss. Entry types are a small validated set of string constants; JSON is not involved in the storage path (rows map directly to a struct).

```go
package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// NoteType is a validated scratchpad entry type.
type NoteType string

const (
	Hypothesis NoteType = "hypothesis"
	Finding    NoteType = "finding"
	Todo       NoteType = "todo"
	Note       NoteType = "note"
)

func (t NoteType) valid() bool {
	switch t {
	case Hypothesis, Finding, Todo, Note:
		return true
	default:
		return false
	}
}

// Entry is a single scratchpad row.
type Entry struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Type      NoteType  `json:"type"`
	Text      string    `json:"text"`
	Tags      string    `json:"tags,omitempty"` // comma-separated
	CreatedAt time.Time `json:"created_at"`
}

// Scratchpad is the per-session working-memory buffer. It holds the shared
// store handle; all access goes through the single-writer store contract.
type Scratchpad struct {
	db *sql.DB
}

func NewScratchpad(db *sql.DB) *Scratchpad { return &Scratchpad{db: db} }

// Write inserts a note; returns the short entry id.
func (s *Scratchpad) Write(ctx context.Context, sessionID, text string, t NoteType, tags string) (string, error) {
	if !t.valid() {
		return "", fmt.Errorf("invalid type %q: must be one of hypothesis|finding|todo|note", t)
	}
	id := uuid.NewString()[:8]
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO scratchpad_entries(id,session_id,type,text,tags,created_at) VALUES(?,?,?,?,?,?)`,
		id, sessionID, string(t), text, nullIfEmpty(tags), now)
	if err != nil {
		return "", err
	}
	return id, nil
}

// ReadOpts filters a Read query.
type ReadOpts struct {
	Type  NoteType // "" = any
	Tags  []string // OR-match; nil = any
	Limit int
}

// Read returns entries ordered by created_at DESC.
func (s *Scratchpad) Read(ctx context.Context, sessionID string, opts ReadOpts) ([]Entry, error) {
	where := []string{"session_id = ?", "deleted_at IS NULL"}
	args := []any{sessionID}
	if opts.Type != "" {
		where = append(where, "type = ?")
		args = append(args, string(opts.Type))
	}
	limit := opts.Limit
	if limit == 0 {
		limit = 50
	}
	q := fmt.Sprintf(
		`SELECT id, session_id, type, text, COALESCE(tags,''), created_at
		   FROM scratchpad_entries WHERE %s ORDER BY created_at DESC LIMIT ?`,
		strings.Join(where, " AND "))
	rows, err := s.db.QueryContext(ctx, q, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		var created string
		if err := rows.Scan(&e.ID, &e.SessionID, &e.Type, &e.Text, &e.Tags, &created); err != nil {
			return nil, err
		}
		e.CreatedAt, _ = time.Parse(time.RFC3339, created)
		if len(opts.Tags) > 0 && !tagsIntersect(e.Tags, opts.Tags) {
			continue // FR-08: OR-match tag filter, done in Go
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// BuildContextSummary renders the auto-injection block (FR-04, G4).
func (s *Scratchpad) BuildContextSummary(ctx context.Context, sessionID string, maxEntries int) (string, error) {
	if maxEntries == 0 {
		maxEntries = 10
	}
	entries, err := s.Read(ctx, sessionID, ReadOpts{Limit: maxEntries})
	if err != nil || len(entries) == 0 {
		return "", err
	}
	var b strings.Builder
	b.WriteString("Working memory (scratchpad):\n")
	for _, e := range entries {
		tagStr := ""
		if e.Tags != "" {
			tagStr = fmt.Sprintf(" [%s]", e.Tags)
		}
		fmt.Fprintf(&b, "  [%s]%s %s\n", e.Type, tagStr, truncate(e.Text, 100))
	}
	return b.String(), nil
}
```

Helpers `nullIfEmpty`, `tagsIntersect`, and `truncate` are small unexported utilities; `truncate` is rune-aware to avoid splitting a multi-byte character at the 100-char cap.

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Sensitive data in scratchpad auto-injected into prompts | Auto-injection truncates entries to 100 chars; flag `--no-auto-inject` available |
| Session ID spoofing | Session ID validated against active sessions table before write |

---

## 11. Testing Strategy

Go `testing`, table-driven, against an in-memory `modernc.org/sqlite` store; §4 latency targets measured with `testing.B`.

| Layer | Tests |
|-------|-------|
| Unit | `Write`/`Read` round-trip; `NoteType.valid()` type validation; OR-match tag filtering; `BuildContextSummary` token/length bounds |
| Integration | Full session: write 10 entries, read back, export, promote to memory |
| Benchmark | `testing.B` for the < 10ms write / < 20ms read targets in §4 |

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

