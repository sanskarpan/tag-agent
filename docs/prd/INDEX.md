# TAG Feature PRD Index

> Product Requirements Documents for the TAG agent orchestration platform.
> Each PRD covers one feature area: problem statement, goals, technical design, implementation plan, and risks.

---

## Priority Matrix

| PRD | Feature | Priority | Effort | Status |
|-----|---------|----------|--------|--------|
| [001](PRD-001-structured-memory-configuration.md) | Structured Memory Configuration Per Profile | P0 | M | Proposed |
| [002](PRD-002-cross-session-memory-journal.md) | Cross-Session Memory Journal (`tag memory-journal`) | P0 | S | Proposed |
| [003](PRD-003-rich-streaming-tui.md) | Rich Streaming TUI Output (spinners, progress, status bar) | P0 | M | Proposed |
| [004](PRD-004-kanban-swarm-helpers.md) | Kanban Swarm Topology Helpers (`tag swarm`) | P1 | M | Proposed |
| [005](PRD-005-execution-backend-selection.md) | Execution Backend Selection Per Profile (Docker, SSH, Modal) | P1 | S–M | Proposed |
| [006](PRD-006-tool-gateway-opt-in.md) | Tool Gateway Opt-in (`tag import-nous-portal`) | P1 | XS | Proposed |
| [007](PRD-007-tag-desktop.md) | Desktop Electron App Launcher (`tag desktop`) | P2 | M | Proposed |
| [008](PRD-008-background-task-queue.md) | Background Task Queue with Notifications (`tag queue`) | P1 | M | Proposed |
| [009](PRD-009-enhanced-doctor-diagnostics.md) | Enhanced `tag doctor` Diagnostics (pass/warn/fail per component) | P1 | S | Proposed |
| [010](PRD-010-dashboard-admin-panel.md) | Dashboard Admin Panel Integration (`tag dashboard` upgrade) | P2 | XS | Proposed |
| [011](PRD-011-plugin-management.md) | Plugin Management System (`tag plugin install/list/enable`) | P1 | M | Proposed |
| [012](PRD-012-cost-tracking-budget.md) | Cost Tracking & Budget Management (`tag costs`) | P1 | M | Proposed |
| [013](PRD-013-agent-tracing-observability.md) | Distributed Agent Tracing & Observability (`tag trace`) | P1 | L | Proposed |
| [014](PRD-014-mcp-server-registry.md) | MCP Server Registry & Discovery (`tag mcp registry`) | P1 | M | Proposed |
| [015](PRD-015-profile-templates-sharing.md) | Profile Templates & Sharing (`tag template export/import/pull`) | P2 | M | Proposed |
| [016](PRD-016-webhook-event-triggers.md) | Webhook Event Triggers & Automation (`tag hooks`) | P2 | L | Proposed |
| [017](PRD-017-multi-model-benchmarking.md) | Multi-Model Benchmarking & Comparison (`tag compare`) | P2 | M | Proposed |
| [018](PRD-018-context-window-management.md) | Context Window & Long-Context Management (`tag context`) | P1 | M | Proposed |
| [019](PRD-019-natural-language-shell.md) | Natural Language Shell Mode (`tag shell`) | P2 | M | Proposed |
| [020](PRD-020-cicd-integration.md) | CI/CD Integration & Automated Code Review (`tag review-pr`) | P2 | L | Proposed |
| [022](PRD-022-ide-bridge-lsp.md) | IDE Bridge — LSP Server & VS Code Extension (`tag lsp`) | P2 | XL | Proposed |
| [021](PRD-021-streaming-tui-dashboard.md) | Streaming TUI Dashboard (`tag serve` / `tag dashboard`) | P1 | L | Proposed |
| [022](PRD-021-semantic-memory-confidence-decay.md) | Semantic Memory with Confidence Decay (`tag memory`) | P1 | L | Proposed |
| [026](PRD-026-vector-based-tool-retrieval.md) | Vector-Based Tool Retrieval (`tag mcp-registry index`) | P1 | M | Proposed |
| [035](PRD-035-profile-marketplace.md) | Profile Marketplace — pull/push with SHA pinning & secret scan (`tag profile pull/push`) | P1 | M | Proposed — BLOCKED on PRD-034 |
| [sandbox](PRD-021-sandbox-code-execution.md) | Sandbox Code Execution (`tag sandbox`) — Docker/E2B/Modal/restricted isolation | P0 Critical | L | Proposed |
| [037](PRD-037-otel-genai-span-cost-attribution.md) | OTel GenAI Span Cost Attribution (semconv attribute alignment + histogram) | P1 | S | Proposed |
| [037-notify](PRD-037-notification-hooks.md) | Notification Hooks — Slack, email, desktop, webhook (`tag hooks notify`) | P1 | M | Proposed |

---

## Recommended Implementation Order

### Wave 1 — Foundation (P0, quick wins)
Start here: these deliver maximum visible impact with minimum architectural risk.

1. **PRD-003** — Rich TUI (zero new deps, `rich` already in Hermes; biggest UX win)
2. **PRD-002** — Memory Journal (new SQLite table + 5 functions; < 1 week)
3. **PRD-009** — Enhanced Doctor (existing function, add Rich formatting + per-profile checks)
4. **PRD-006** — Tool Gateway Opt-in (2–3 days; follows existing `import-*` pattern)

### Wave 2 — Core Features (P1, medium effort)
These build on Wave 1 infrastructure.

5. **PRD-001** — Structured Memory Config (builds on PRD-002's foundation)
6. **PRD-008** — Background Queue (detached processes + SQLite table)
7. **PRD-011** — Plugin Management (pip into Hermes venv + config writes)
8. **PRD-012** — Cost Tracking (SQLite schema extension + token parsing)
9. **PRD-014** — MCP Registry (bundled YAML registry + profile config writes)
10. **PRD-018** — Context Window Management (wraps existing `prompt-size` command)

### Wave 3 — Advanced Features (P1–P2, complex)
Build after Wave 2 is stable.

11. **PRD-004** — Kanban Swarm (depends on gateway management)
12. **PRD-005** — Execution Backends (depends on `render_profiles()` refactor)
13. **PRD-013** — Tracing (new module + SQLite spans table)
14. **PRD-015** — Profile Templates (export/import flow)
15. **PRD-016** — Webhook Triggers (event system + hook executor)

### Wave 4 — Differentiating Features (P2, high impact)

16. **PRD-021** — Streaming TUI Dashboard / `tag serve` (builds on PRD-003 Rich TUI, PRD-008 queue, PRD-013 tracing)
17. **PRD-017** — Multi-Model Benchmarking (extends existing benchmark system)
18. **PRD-019** — Natural Language Shell (new REPL module)
19. **PRD-020** — CI/CD Integration (gh CLI + GitHub Actions template)
20. **PRD-007** — Desktop App (Electron build from vendor tarball)
21. **PRD-010** — Dashboard Upgrade (minimal changes, big discoverability win)
22. **PRD-022** — IDE Bridge (LSP server + VS Code extension — editor-native TAG code actions)

---

## Cross-Cutting Concerns

### Shared infrastructure these PRDs depend on

| Infrastructure | Used by PRDs |
|---------------|-------------|
| `tui_output.py` (Rich) — PRD-003 | 004, 008, 009, 011, 012, 013, 017, 019, 020 |
| `open_db()` schema migrations | 002, 008, 012, 013, 016 |
| `render_profiles()` deep-merge (PRD-010) | 001, 005, 006, 014, 015 |
| `hermes_env()` / `profile_exec_env()` | 001, 002, 018 |
| `_cmd_import_generic()` pattern | 001, 005, 006 |

### New modules required

| Module | PRDs |
|--------|------|
| `src/tag/tui_output.py` | 003 (creates) + all others |
| `src/tag/tracing.py` | 013 |
| `src/tag/events.py` | 016 |
| `src/tag/shell_mode.py` | 019 |
| `src/tag/ci.py` | 020 |
| `src/tag/queue_worker.py` | 008 |
| `src/tag/dashboard.py` | 021 |
| `src/tag/api.py` | 021 |
| `src/tag/lsp_server.py` | 022 |
| `vscode/` (extension package) | 022 |
| `src/tag/tool_retrieval.py` | 026 |
| `src/tag/vector_store.py` | 022 (semantic memory), 026 (shared ChromaDB client) |

---

## Feature Coverage by Domain

### Memory
- PRD-001: Hermes memory backend selection (Supermemory, Honcho, local)
- PRD-002: TAG-native cross-session facts journal
- PRD-022: Semantic memory with confidence decay (ChromaDB + sentence-transformers, local embeddings)
- PRD-018: Context window management and auto-summarization

### Developer Experience (TUI / UX)
- PRD-003: Rich streaming output, spinners, progress bars
- PRD-007: Electron desktop app
- PRD-009: Enhanced diagnostics
- PRD-019: Natural language shell REPL

### Multi-Agent Orchestration
- PRD-004: Kanban swarm helpers
- PRD-008: Background task queue
- PRD-016: Webhook event triggers

### Provider & Tool Integrations
- PRD-005: Execution backends (Docker, SSH, Modal, Daytona)
- PRD-006: Nous Portal Tool Gateway
- PRD-011: Plugin management
- PRD-014: MCP server registry
- PRD-026: Vector-based tool retrieval (ChromaDB index over MCP tools, top-K selection at query time)

### Observability & Operations
- PRD-012: Cost tracking and budgets
- PRD-013: Distributed tracing
- PRD-009: Doctor diagnostics

### Observability & Live Dashboards
- PRD-021: Streaming TUI Dashboard — live token stream, cost ticker, tool call inspector, queue status, web bridge

### Collaboration & Ecosystem
- PRD-015: Profile templates and sharing
- PRD-017: Multi-model benchmarking
- PRD-020: CI/CD integration and automated code review
- PRD-035: Profile Marketplace — GitHub-based profile distribution with SHA pinning, secret scanning, and Gist push (BLOCKED on PRD-034)
