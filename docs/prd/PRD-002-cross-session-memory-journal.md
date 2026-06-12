# PRD-002: TAG-Native Cross-Session Memory Journal

**Status:** Proposed  
**Priority:** P0 (Highest Impact)  
**Estimated Effort:** S (1 week)  
**Affects:** `controller.py` (`open_db`, `hermes_env`), `tag.sqlite3` schema

---

## 1. Overview

TAG's SQLite database currently stores only `runs` and `steps` (benchmark/execution history). There is no mechanism for storing persistent facts, user preferences, or agent knowledge that should survive across sessions. This PRD defines a lightweight `memory` table and a `tag memory-journal` command set that lets users (and agents themselves) store and retrieve key→value facts per profile, injected into Hermes prompts at run time.

This is distinct from PRD-001 (which configures Hermes' own memory backends). This is TAG's own knowledge layer that can supplement whatever Hermes does internally — useful for facts that should always be present regardless of which memory backend Hermes uses.

---

## 2. Problem Statement

- An agent working on a project across multiple days has no way to know what it concluded last time unless the user re-explains it.
- Hermes' own memory is internal, opaque, and not manageable through TAG.
- There is no place to store structured facts like "the API base URL is ...", "user prefers TypeScript", "don't touch file X" that should be injected every session.
- Competing tools (Claude Code's CLAUDE.md, Windsurf Cascade Memories, Cursor's Rules) all offer this and users expect it.

---

## 3. Goals

1. A `memory` table in `tag.sqlite3` stores per-profile key→value facts with optional TTL.
2. `tag memory-journal save/list/forget/clear` commands manage the journal.
3. At run time, non-expired entries are formatted and injected into the agent's system prompt via an env var that Hermes can pick up.
4. Works entirely offline — no cloud dependency.
5. Facts can be tagged with a `scope`: `global` (all profiles), `profile` (specific profile), or `session` (current run only, not persisted).

---

## 4. Non-Goals

- Vector semantic search over memory — plain key→value lookup only in this release.
- Automatic memory extraction from agent output (the agent or user must call `tag memory-journal save` explicitly).
- Replacing Hermes' internal memory system.

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag memory-journal save --profile coder "prefer TypeScript over JavaScript"` | every coder session respects this preference |
| U2 | Researcher | save `tag memory-journal save --profile researcher "Project context: building a REST API for X"` | research agents know what they're researching |
| U3 | Developer | run `tag memory-journal list` | see all current facts at a glance |
| U4 | Developer | run `tag memory-journal forget <id>` | remove stale or wrong facts |
| U5 | Agent | have facts injected automatically at startup | I don't need to be told the same things every session |

---

## 6. Technical Design

### 6.1 Schema

```sql
CREATE TABLE IF NOT EXISTS memory_journal (
    id          TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(8)))),
    profile     TEXT NOT NULL,          -- profile name, or '*' for global
    key         TEXT NOT NULL,
    value       TEXT NOT NULL,
    scope       TEXT NOT NULL DEFAULT 'profile',  -- 'global' | 'profile' | 'session'
    created_at  TEXT NOT NULL,
    expires_at  TEXT,                   -- NULL = never expires
    UNIQUE(profile, key)                -- upsert on conflict
);
CREATE INDEX IF NOT EXISTS idx_memory_journal_profile ON memory_journal(profile);
```

Add this table creation to `open_db()` alongside the existing `runs`/`steps` tables.

### 6.2 Core functions

```python
def journal_save(
    db: sqlite3.Connection,
    profile: str,
    key: str,
    value: str,
    *,
    ttl_days: int | None = None,
) -> str:
    """Upsert a key→value fact. Returns the row id."""

def journal_list(
    db: sqlite3.Connection,
    profile: str,
    *,
    include_global: bool = True,
) -> list[dict[str, Any]]:
    """Return all non-expired entries for profile (+ global if include_global)."""

def journal_forget(db: sqlite3.Connection, entry_id: str) -> bool:
    """Delete entry by id. Returns True if deleted."""

def journal_clear(db: sqlite3.Connection, profile: str) -> int:
    """Delete all non-global entries for profile. Returns count deleted."""

def journal_to_prompt_prefix(
    db: sqlite3.Connection,
    profile: str,
) -> str | None:
    """Format non-expired entries as a system prompt injection block.
    
    Returns a markdown-formatted string like:
    
    ## Persistent Context (TAG Memory Journal)
    - prefer TypeScript over JavaScript
    - Project context: building a REST API for X
    
    Returns None if no entries exist.
    """
```

### 6.3 Prompt injection mechanism

Hermes reads `HERMES_SYSTEM_INJECT` environment variable as a system prompt prefix (confirmed in Hermes v0.16.0 architecture docs). In `profile_exec_env()`, after setting the existing env vars:

```python
db = open_db(cfg)
prefix = journal_to_prompt_prefix(db, profile_name)
if prefix:
    env["HERMES_SYSTEM_INJECT"] = prefix
db.close()
```

If `HERMES_SYSTEM_INJECT` is not supported by the installed Hermes version, fall back to a warning in `tag doctor` but don't fail.

### 6.4 CLI commands

```
tag memory-journal save   KEY VALUE [--profile PROFILE] [--ttl-days N] [--global]
tag memory-journal list   [--profile PROFILE] [--json]
tag memory-journal forget ID
tag memory-journal clear  [--profile PROFILE] [--confirm]
tag memory-journal export [--profile PROFILE] [--output FILE]
tag memory-journal import [--profile PROFILE] FILE
```

The existing `tag memory` command remains unchanged (it's the Hermes pass-through).

**Note:** `tag memory-journal` is distinct from `tag memory` (Hermes pass-through). This naming prevents confusion.

### 6.5 Command implementations

```python
def cmd_memory_journal(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    db = open_db(cfg)
    profile = getattr(args, "profile", cfg["defaults"]["master_profile"])
    
    if args.subcommand == "save":
        entry_id = journal_save(db, profile, args.key, args.value, ttl_days=args.ttl_days)
        print(f"saved: {entry_id}")
    elif args.subcommand == "list":
        entries = journal_list(db, profile)
        if args.json:
            print(json.dumps(entries, indent=2))
        else:
            for e in entries:
                exp = f" (expires {e['expires_at'][:10]})" if e['expires_at'] else ""
                print(f"  [{e['id']}] {e['key']}: {e['value']}{exp}")
    elif args.subcommand == "forget":
        deleted = journal_forget(db, args.id)
        print("deleted" if deleted else "not found")
    elif args.subcommand == "clear":
        if not args.confirm:
            print("Pass --confirm to clear all entries for this profile.")
            return 1
        count = journal_clear(db, profile)
        print(f"cleared {count} entries")
    db.close()
    return 0
```

---

## 7. Implementation Plan

| Step | Task |
|------|------|
| 1 | Add `memory_journal` table to `open_db()` |
| 2 | Implement `journal_save`, `journal_list`, `journal_forget`, `journal_clear`, `journal_to_prompt_prefix` |
| 3 | Inject prompt prefix in `profile_exec_env()` |
| 4 | Add `cmd_memory_journal` with subcommand routing |
| 5 | Register `memory-journal` subparser with all sub-subcommands |
| 6 | Add tests: `test_journal_save_upserts`, `test_journal_list_excludes_expired`, `test_journal_prompt_prefix_format`, `test_journal_global_entries_included` |
| 7 | Update README with memory journal section |

---

## 8. Success Metrics

- `tag memory-journal save researcher "context: X"` followed by `tag memory-journal list --profile researcher` shows the entry.
- `profile_exec_env()` includes `HERMES_SYSTEM_INJECT` when journal has entries.
- Expired entries are excluded from `journal_list` and `journal_to_prompt_prefix`.
- TTL expiry works: entry with `--ttl-days 0` is excluded the next day.

---

## 9. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| `HERMES_SYSTEM_INJECT` env var not supported in Hermes | Detect at startup: check Hermes version, log warning in `tag doctor`, don't fail |
| Large journals inflate system prompts and blow context window | Cap at 50 entries per profile per inject; add `tag memory-journal trim --max N` command |
| Key collisions between profiles | The UNIQUE constraint is `(profile, key)` — different profiles can have same key, global entries use `profile='*'` |
| SQLite concurrent write contention | Already solved: `open_db()` uses WAL mode with busy_timeout |
