# ADR 0001 — Deferring the Postgres/pgvector backend and native Honcho recall

- **Status:** Accepted (deferred, not rejected)
- **Date:** 2026-07-17
- **Context:** the hermes-octo parity review (`COMPARISON_REPORT.md`) surfaced seven
  capability gaps. Five were implemented and merged; this ADR records why the
  remaining two — a Postgres/pgvector state backend (gap #6) and a native
  Honcho live-recall memory backend (gap #3) — are **deliberately deferred**.

## Gaps addressed and shipped

| Gap | What | PR |
|---|---|---|
| #2 | Runtime provider fallback chain (`FallbackProvider`, `run --fallback`) | #548 |
| #1 | OpenAI-compatible gateway (`/v1/chat/completions`, `/v1/models`, bearer auth) | #549 |
| #4 | Local last-resort model provider (`local`, llama.cpp/ollama) | #550 |
| #5 | Exa `web_search` tool + tool-budget discipline | #551 |
| #7 | Turnkey cloud-deploy recipe (Dockerfile, render.yaml, deploy guide) | #552 |

## Decision

Do **not** build gap #6 (Postgres/pgvector) or gap #3 (native Honcho recall) at
this time. Both conflict with load-bearing TAG design tenets and would make the
codebase worse for a use case TAG deliberately does not target. Each is captured
below with the evidence and a concrete future-implementation sketch so the
decision can be revisited if the constraints change.

---

## Gap #6 — Postgres/pgvector state backend

### Why hermes-octo has it
hermes-octo is a *hosted, multi-process* deployment (gateway + Honcho + deriver
+ llama.cpp under supervisord) that needs **shared, durable state across
restarts and instances**, so it uses Neon serverless Postgres + pgvector.

### Why TAG defers it
1. **It contradicts a core tenet.** TAG (Go) is a **single static CGO-free
   binary with embedded SQLite** (`modernc.org/sqlite`, WAL) and *zero external
   services*. That is the whole distribution story — `go build` produces one
   file that runs anywhere, `docker run` needs no sidecar database. A Postgres
   backend reintroduces the operational dependency the rewrite exists to remove.
2. **Coupling is deep.** 49 non-test files execute SQL directly against
   `store.DB`. A Postgres option means abstracting all of them behind a
   `store.Store` interface and maintaining two SQL dialects forever.
3. **FTS5 is a hard blocker.** Memory search (`internal/memory/memory.go`,
   `internal/store/store.go`) uses SQLite **FTS5** full-text search. Postgres has
   no FTS5; the entire search path would have to be reimplemented on `tsvector`/
   `to_tsquery` (or pgvector) — a *reimplementation on a different engine*, not a
   port — with subtly different ranking semantics to reconcile.
4. **~18 SQLite-specific SQL sites** (upserts, pragmas, date functions) would
   need dialect-specific rewrites and dual testing.

### Cost/benefit
Multi-day refactor + permanent dual-dialect maintenance burden, in exchange for
serverless multi-instance shared state — which TAG's single-binary model does
not need. The parity report's own recommendation was **skip**.

### Future-implementation sketch (if revisited)
- Introduce a `store.Store` interface (Exec/Query/QueryRow/Tx + a migration hook)
  and make the current SQLite code the default implementation — a mechanical but
  large seam across 49 files.
- Add a `postgres` implementation using `github.com/jackc/pgx/v5` (CGO-free).
- Replace FTS5 memory search with a Postgres `tsvector` GIN index (and/or
  `pgvector` for embedding search once we ship real embeddings).
- Select the backend via `runtime.store.backend: sqlite|postgres` in config;
  keep SQLite the default so the single-binary story is unaffected.
- Gate behind an integration test suite that runs the full command surface
  against a throwaway Postgres container.

## Gap #3 — Native Honcho live-recall memory backend

### Why hermes-octo has it
hermes-octo has **no memory of its own** — it delegates to Honcho (Plastic Labs):
an async deriver extracts facts from conversations into pgvector and serves them
back as context.

### Why TAG defers it
TAG already ships a **richer, self-contained memory suite** that is a *superset*
of Honcho's recall:
- `internal/memory/memory.go` — semantic memory with FTS/BM25 search and
  type-specific confidence **decay**.
- `internal/memory/fact.go` — temporal facts with validity windows + supersession.
- `internal/memory/episode.go` — episodic memory.
- `internal/memory/gc.go` — GC + **tiering** (core/archival) with access-count
  promotion.
- `internal/graph/graph.go` — an **entity knowledge graph** (extraction,
  co-occurrence relations, union-find communities) — something Honcho does not do.

Adding a native Honcho backend would introduce an **external Honcho server +
Postgres dependency** to duplicate — with *less* capability — what TAG already
does locally in the single binary. Credentials for anyone who wants Honcho are
already importable via `tag import-honcho`.

### Future-implementation sketch (if revisited)
- Only worthwhile if a user specifically wants Honcho's derive-from-conversation
  model as an *alternate* recall path. It would slot in as a memory provider
  behind a `memory.Recaller` interface (assemble-context call), selected per
  profile, driving the existing `import-honcho` credentials — not as a
  replacement for the native suite.

## Consequences

- The five shipped gaps turn TAG from a CLI into a **deployable, provider-failing-
  over, web-searching inference service** — the substantive parity with
  hermes-octo. The two deferred gaps are infrastructure choices, not capability
  gaps a user would feel.
- Revisit this ADR if TAG grows a genuine multi-instance hosted mode (would
  justify #6) or a concrete demand for Honcho-style conversational derivation
  (would justify #3).
