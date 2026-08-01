# TAG Feature PRD Index

> Product Requirements Documents for the TAG agent orchestration platform.
> Each PRD covers one feature area: problem statement, goals, technical design, implementation plan, and risks.

---

## Cluster A–J PRD Summary (PRD-045 to PRD-125)

These 81 PRDs were added in the v0.6.x planning cycle after competitive research across 10 domains.
See [FEATURES_ROADMAP.md](../FEATURES_ROADMAP.md) for the full cluster map.

| Cluster | Domain | PRDs | Count |
|---------|--------|------|-------|
| A | Evaluation & Observability | PRD-045 to PRD-054 | 10 |
| B | CI/CD & Agentic Dev Workflows | PRD-055 to PRD-064 | 10 |
| C | Memory & Knowledge | PRD-065 to PRD-072 | 8 |
| D | MCP Ecosystem & Tool Connectivity | PRD-073 to PRD-080 | 8 |
| E | Multi-Agent Interoperability | PRD-081 to PRD-088 | 8 |
| F | Sandbox & Execution Environment | PRD-089 to PRD-100 | 12 |
| G | Advanced Reasoning & Planning | PRD-101 to PRD-108 | 8 |
| H | Agentic Workflow State & Graph | PRD-109 to PRD-116 | 8 |
| I | Computer Use & Browser Automation | PRD-117 to PRD-120 | 4 |
| J | Security & Guardrails | PRD-121 to PRD-125 | 5 |
| **Total** | | | **81** |

---

## Cluster K — Sakana AI Competitive Gap PRDs (PRD-126 to PRD-127)

Added in v0.7.2 planning cycle after competitive research against Sakana AI (Tokyo lab, $2.65B valuation, Jun 2026).
These PRDs address capabilities where Sakana leads and where TAG can close the gap at the software layer without GPU compute.

| PRD | Feature | Gap Addressed | Priority | Effort |
|-----|---------|--------------|----------|--------|
| [126](PRD-126-inference-time-tree-search-solve.md) | `tag solve` — Inference-time multi-model tree search (AB-MCTS inspired) | Sakana AB-MCTS / TreeQuest — multi-model inference-time scaling | P1 | L |
| [127](PRD-127-evolutionary-profile-optimization.md) | `tag evolve` — Evolutionary profile configuration optimization | Sakana Evolutionary Model Merging / CycleQD / ShinkaEvolve | P2 | L |

**Enhancements to existing PRDs (same sprint cycle):**

| PRD | Enhancement | Gap Addressed |
|-----|------------|--------------|
| [PRD-082](PRD-082-multi-agent-team-primitives.md) | Trinity-style dynamic Thinker/Worker/Verifier role assignment per turn | Sakana Trinity (ICLR 2026) — evolved coordinator with per-turn role rotation |
| [PRD-101](PRD-101-self-consistency-ensemble.md) | Diverse-profile ensemble with reviewer-judge + tournament + synthesize modes | Sakana Conductor (ICLR 2026) — multi-model orchestration with specialist instructions |
| [PRD-023](PRD-023-multi-agent-swarm-context-routing.md) | Per-wave self-review (`--self-review`) + self-improvement loop (`--self-improve`) | Sakana DGM — peer-review mechanism that improved SWE-bench 20%→50% |

---

## Status Matrix (PRD-001 to PRD-044)

> **Status accuracy — read this before trusting the Status column.**
>
> **All 127 PRDs were audited on 2026-07-29 and every Status below is the result of
> that pass.** Nothing in this table is left over from the "nobody revisited this row"
> era: the previous note claiming only PRD-001 to PRD-012 had been checked is obsolete.
>
> **How it was verified — by running, not by reading.** Both distributions were built
> from this tree and driven in isolated `TAG_HOME` sandboxes with no API keys set and
> **no live API calls**:
>
> ```
> cd tag-go && go build -o /tmp/tagprd ./cmd/tag
> uv venv --python 3.11 /tmp/pyprd && uv pip install --python /tmp/pyprd/bin/python -e .
> ```
>
> A recursive `--help` sweep enumerated the whole command surface of each binary
> (88 Go top-level commands, 103 Python), followed by targeted subcommand and flag
> probes for every PRD whose scope hinged on a specific verb or flag
> (e.g. `sandbox run --stream`, `mem search --mode hybrid`, `submit --samples`,
> `route classify`, `trace show --kind`). Span emission was checked end to end with an
> offline `echo`-provider run (`tag run … --provider echo` then `tag trace list`).
>
> **What the labels mean:**
>
> - **Shipped (Go + Python)** / **Shipped (Python only)** / **Shipped (Go only)** — the
>   feature exists and works in the named distribution(s). The two surfaces differ
>   substantially, so the distribution is always named.
> - **Partial** — some of the PRD's scope exists; the row says what is missing.
> - **Proposed** — genuinely not built; the row names the absent command or flag.
> - **Proposed (unverified)** — could not be verified cheaply and is **not** being
>   guessed at. Exactly one row (PRD-048) is in this bucket.
>
> **Counts:** 51 Shipped (Go + Python) · 12 Shipped (Python only) · 13 Partial ·
> 50 Proposed · 1 Proposed (unverified).
>
> **Deliberate non-ports are not "unbuilt".** PRD-007 (desktop) and PRD-010 (dashboard)
> ship in Python and are intentionally not ported to Go — OS desktop packaging and the
> managed-runtime passthrough respectively, with `serve`/`web`/`devui` replacing the
> dashboard. See [`../../tag-go/MIGRATION_STATUS.md`](../../tag-go/MIGRATION_STATUS.md).
> Separately, [ADR-0001](../adr/0001-postgres-and-honcho-deferral.md) records the
> Postgres/pgvector backend and native Honcho recall as **deferred by decision**; no PRD
> in this index covers either, so neither appears as a "Proposed" row.
>
> For a measured inventory of what exists right now, use the CLI surface itself
> (`tag --help`), [`../FEATURES.md`](../FEATURES.md), and
> [`../../tag-go/MIGRATION_STATUS.md`](../../tag-go/MIGRATION_STATUS.md).

| PRD | Feature | Priority | Effort | Status |
|-----|---------|----------|--------|--------|
| [001](PRD-001-structured-memory-configuration.md) | Structured Memory Configuration Per Profile | P0 (Highest Impact) | M (2–3 weeks) | Shipped (Go + Python) |
| [002](PRD-002-cross-session-memory-journal.md) | TAG-Native Cross-Session Memory Journal | P0 (Highest Impact) | S (1 week) | Shipped (Go + Python) |
| [003](PRD-003-rich-streaming-tui.md) | Rich Streaming TUI Output | P0 (Highest Visible Impact) | M (2 weeks) | Shipped (Go + Python) |
| [004](PRD-004-kanban-swarm-helpers.md) | Kanban Swarm Topology Helpers | P1 | M (2 weeks) | Deliberate non-port (Go) / Shipped (Python) — wraps `kanban`, itself a non-port |
| [005](PRD-005-execution-backend-selection.md) | Execution Backend Selection Per Profile | P1 | S–M (1–2 weeks) | Shipped (Go + Python) |
| [006](PRD-006-tool-gateway-opt-in.md) | Tool Gateway Opt-in Wiring | P1 | XS (2–3 days) | Shipped (Go + Python) |
| [007](PRD-007-tag-desktop.md) | `tag desktop` Subcommand | P2 | M (2 weeks) | Shipped (Python only) — Deliberate non-port in Go (OS desktop packaging; see `tag-go/MIGRATION_STATUS.md`) |
| [008](PRD-008-background-task-queue.md) | Background Task Queue (`tag queue`) | P1 | M (2 weeks) | Shipped (Go + Python) |
| [009](PRD-009-enhanced-doctor-diagnostics.md) | Enhanced `tag doctor` Diagnostics | P1 | S (3–5 days) | Shipped (Go + Python) |
| [010](PRD-010-dashboard-admin-panel.md) | Dashboard & Admin Panel Integration | P2 | XS (2 days) | Shipped (Python only) — Deliberate non-port in Go (managed-runtime passthrough; `serve`/`web`/`devui` replace it) |
| [011](PRD-011-plugin-management.md) | Plugin Management System (`tag plugins`) | P1 | M (2 weeks) | Shipped (Go + Python) |
| [012](PRD-012-cost-tracking-budget.md) | Cost Tracking & Budget Management | P1 | M (2 weeks) | Shipped (Go + Python) |
| [013](PRD-013-agent-tracing-observability.md) | Distributed Agent Tracing & Observability | P1 | L (3–4 weeks) | Shipped (Go + Python) |
| [014](PRD-014-mcp-server-registry.md) | MCP Server Registry & Discovery | P1 | M (2 weeks) | Shipped (Go + Python) |
| [015](PRD-015-profile-templates-sharing.md) | Profile Templates & Sharing | P2 | M (2 weeks) | Shipped (Go + Python) |
| [016](PRD-016-webhook-event-triggers.md) | Webhook Event Triggers & Automation | P2 | L (3–4 weeks) | Shipped (Go + Python) |
| [017](PRD-017-multi-model-benchmarking.md) | Multi-Model Benchmarking & Comparison | P2 | M (2 weeks) | Shipped (Go + Python) — Go `compare` is `list`/`show`; the run path is `benchmark run` |
| [018](PRD-018-context-window-management.md) | Context Window & Long-Context Management | P1 | M (2–3 weeks) | Shipped (Go + Python) |
| [019](PRD-019-natural-language-shell.md) | Natural Language Shell Mode (`tag shell`) | P2 | M (2 weeks) | Shipped (Go + Python) |
| [020](PRD-020-cicd-integration.md) | CI/CD Integration & Automated Code Review | P2 | L (3–4 weeks) | Shipped (Go + Python) |
| [021](PRD-021-agent-loop-autonomous-mode.md) | Agent Loop / Autonomous Mode | P1 | M (2–3 weeks) | Shipped (Go + Python) |
| [022](PRD-022-cron-scheduled-agents.md) | Cron / Scheduled Agents (`tag cron`) | P1 | M (2 weeks) | Shipped (Go + Python) |
| [023](PRD-023-multi-agent-swarm-context-routing.md) | Multi-Agent Swarm with Context-Centric Routing | P1 | L (2 sprints, ~4 weeks) | Shipped (Go + Python) |
| [024](PRD-024-repo-map-workspace-context.md) | Repo-Map / Workspace Context | P1 | L (2 sprints, ~4 weeks) | Shipped (Go + Python) |
| [025](PRD-025-semantic-memory-confidence-decay.md) | Semantic Memory with Confidence Decay (`tag memory`) | P1 (High Impact) | L (2 sprints, ~4 weeks) | Shipped (Go + Python) |
| [026](PRD-026-profile-marketplace.md) | Profile Marketplace (tag profile pull/push) | P1 | M (1 sprint, ~2 weeks) | Shipped (Go + Python) — ships as `tag marketplace pull/push/list`, not `tag profile pull/push` |
| [027](PRD-027-eval-framework.md) | Eval Framework (tag eval) | P1 | M (1 sprint, ~2 weeks) | Shipped (Go + Python) |
| [028](PRD-028-sandbox-code-execution.md) | Sandbox Code Execution (`tag sandbox`) | P0 Critical | L (2 sprints / ~4 weeks) | Shipped (Go + Python) — Go has `sandbox run` only (no `list`/`result`) |
| [029](PRD-029-streaming-tui-dashboard.md) | Streaming TUI Dashboard (`tag serve` / `tag dashboard`) | P1 (High Impact, Differentiating) | L (TUI sub-feature: M ~1 sprint; Web bridge sub-feature: L ~2 sprints; total: 2–3 sprints) | Shipped (Go + Python) |
| [030](PRD-030-prompt-cache-analytics.md) | Prompt Cache Analytics | P1 | S (2–3 days) | Shipped (Go + Python) |
| [031](PRD-031-model-fallback-chains.md) | Model Fallback Chains | P1 | M (1 sprint, ~2 weeks) | Shipped (Go + Python) |
| [032](PRD-032-agent-replay-time-travel-debugging.md) | Agent Replay / Time-Travel Debugging (`tag trace replay`) | P2 Medium | L (2 sprints, ~4 weeks) | Partial |
| [033](PRD-033-dependency-aware-task-queue.md) | Dependency-Aware Task Queue (`tag queue`) | P1 High | M (1 sprint, ~2 weeks) | Shipped (Go + Python) — Go covers this via `dag` only; Python adds `queue-dep add/promote/list` |
| [034](PRD-034-secret-scanning.md) | Secret Scanning (`tag security scan`) | P0 Critical | S (3–5 days) | Shipped (Go + Python) |
| [035](PRD-035-ide-bridge-lsp.md) | IDE Bridge — LSP Server & VS Code Extension | P2 | XL (3–4 sprints, ~8–10 weeks) | Shipped (Go + Python) — `tag lsp start/status` verified; the VS Code extension package was not verified |
| [036](PRD-036-web-dashboard.md) | Web Dashboard (`tag serve`) | P1 | L (backend M, frontend L — 2–3 sprints) | Shipped (Go + Python) |
| [037](PRD-037-agent-personas.md) | Agent Personas (tag persona) | P1 | M (1 sprint, ~2 weeks) | Shipped (Go + Python) |
| [038](PRD-038-diff-aware-context-injection.md) | Diff-Aware Context Injection | P1 | S (2–3 days) | Shipped (Go + Python) |
| [039](PRD-039-token-budget-enforcement.md) | Token Budget Enforcement (`tag budget`) | P1 High | S (3–5 days) | Shipped (Go + Python) |
| [040](PRD-040-notification-hooks.md) | Notification Hooks (`tag hooks notify`) | P1 | M (1 sprint, ~1 week) | Shipped (Go + Python) — ships as `tag notify`, not `tag hooks notify` |
| [041](PRD-041-otel-genai-span-cost-attribution.md) | OTel GenAI Span Cost Attribution | P1 | S (1–2 days) | Shipped (Go + Python) — `tag otel-export` emits OTLP/JSON with OTel GenAI semconv 1.28.0 |
| [042](PRD-042-architect-editor-agent-split.md) | Architect/Editor Agent Split (`tag run --architect ... --editor ...`) | P2 — Medium | S–M (1 week) | Shipped (Go + Python) |
| [043](PRD-043-vector-based-tool-retrieval.md) | Vector-Based Tool Retrieval (`tag mcp-registry index`) | P1 | M (1 sprint, ~2 weeks) | Shipped (Go) |
| [044](PRD-044-agentops-session-observability.md) | AgentOps Session Observability (`tag config set agentops.api_key`) | P3 — Nice-to-have | S (2–3 days) | Shipped (Go + Python) |

---

## Cluster L — Competitive Parity Gap PRDs (PRD-128 to PRD-132)

Written 2026-07-29 from `docs/COMPETITIVE_PARITY_2026_07.md`, which surveyed ~25 peer
harnesses (Pi, OpenCode, Hermes, Claude Code, Codex CLI, Crush, Goose, Cline, Kilo,
Cursor, Devin and others). Each PRD below closes a capability shipped by multiple
competitors that TAG lacks. **All five were de-duplicated against PRD-001 to PRD-127
before being written** — see the notes column for the near-misses.

| PRD | Feature | Shipped by | De-duplication note | Priority | Effort |
|---|---|---|---|---|---|
| [128](PRD-128-agent-skills-packages.md) | `tag skill` — portable `SKILL.md` capability packages | ~10 peers (Claude Code, Goose, Amp, Crush, Kilo, Cline, Cursor, Qwen, Hermes, Pi) | No existing PRD covers skill packages. Widest single gap. | P1 | M |
| [129](PRD-129-plan-act-execution-mode.md) | `tag run --mode plan` — read-only planning phase before mutation | Cline, Kilo, OpenHands, Kiro, Aider, Jules, Devin, Claude Code, Goose | **Distinct from PRD-105** (`tag plan decompose`, TDAG task-graph decomposition). That is a task-graph feature; this is an execution *mode*. Registers no top-level command, so the `tag plan` namespace stays PRD-105's. | P1 | S–M |
| [130](PRD-130-git-worktree-isolation.md) | `tag worktree` — git worktree isolation for parallel agents | Claude Code, Kilo, Cline, Emdash, Hermes | Only mentioned incidentally in PRD-055 (FR-18); this supersedes that flag in scope and makes `issue-solve` a consumer. | P2 | M |
| [131](PRD-131-zed-agent-client-protocol-editor-bridge.md) | `tag editor serve` — Zed Agent **Client** Protocol editor bridge | Zed registry (~45 agents, 13 client editors), OpenCode, Goose, Claude Code, Codex, Gemini CLI, OpenHands | ⚠️ **Not PRD-087.** That is IBM's Agent *Communication* Protocol (cluster messaging, BeeAI/AGNTCY, owns `tag acp`). This is Zed's Agent *Client* Protocol — the editor↔agent standard. Same acronym, different protocol. | P2 | M |
| [132](PRD-132-provider-adapter-breadth-catalog.md) | `tag providers` — one multi-provider adapter + a model catalog | Pi 30+, Goose 50+, Hermes 32, Crush 25, Factory 39 | **Distinct from PRD-031** (fallback chains) and **PRD-107** (confidence routing) — those *route between* providers; this *adds* them. | P2 | M |

**Deliberately NOT given a PRD number:**

- **Auto context compaction in-loop** — already owned by [PRD-018](PRD-018-context-window-management.md). It is Python-framed and still Proposed; the correct follow-up is a Go re-frame of 018, not a new number.
- **Multi-surface (IDE / desktop / web)** — a documented deliberate non-port (`tag-go/MIGRATION_STATUS.md`). PRD-131 is the pragmatic substitute: editor reach via a protocol endpoint rather than an owned UI.

---

## Status Matrix (PRD-045 to PRD-132)

Cluster A–K. Same audit pass and same label vocabulary as the table above.

| PRD | Feature | Priority | Effort | Status |
|-----|---------|----------|--------|--------|
| [045](PRD-045-llm-as-judge-evaluators.md) | LLM-as-Judge Evaluators (`tag eval run --judge`) | P1 | S (3-5 days) | Shipped (Go + Python) |
| [046](PRD-046-per-span-usd-cost-attribution.md) | Per-Span USD Cost Attribution (`tag trace show --cost / tag stats --cost`) | P1 | XS (1-2 days) | Shipped (Go) |
| [047](PRD-047-eval-ci-gate-pr-comment.md) | Eval CI Gate with PR Comment Integration (`tag eval ci`) | P1 | S (3-5 days) | Shipped (Go + Python) — Go `eval-ci run` is offline/dry-run |
| [048](PRD-048-structured-tool-call-child-spans.md) | Structured Tool-Call Child Spans with TOOL Kind (`tag trace show --kind tool`) | P2 | S (3-5 days) | Proposed (unverified) — no `--kind` filter on `trace show` in either CLI, and no spans are emitted to inspect |
| [049](PRD-049-versioned-eval-dataset-management.md) | Versioned Eval Dataset Management (`tag eval dataset`) | P2 | S (3-5 days) | Shipped (Go + Python) — Python lacks `eval-dataset add-case` |
| [050](PRD-050-alert-rules-metric-thresholds.md) | Alert Rules on Metric Thresholds (`tag alert`) | P2 | M (5-8 days) | Shipped (Go + Python) |
| [051](PRD-051-human-annotation-labeling-queue.md) | Human Annotation and Labeling Queue (`tag annotate`) | P2 | M (5-8 days) | Shipped (Go + Python) — Python lacks `annotate add`/`skip` |
| [052](PRD-052-prompt-versioning-hub.md) | Prompt Versioning Hub with Terminal Playground (`tag prompt`) | P2 | M (1-2 weeks) | Shipped (Go + Python) — Python lacks `prompt versions` |
| [053](PRD-053-traceprocessor-lifecycle-hooks.md) | TraceProcessor Lifecycle Hooks Protocol (`tag hooks trace`) | P3 | S (3-5 days) | Proposed — `hooks` exposes only `list`/`log`/`test`; there is no trace-lifecycle processor registration (`hooks test trace_end` matched no hooks) |
| [054](PRD-054-local-browser-devui.md) | Local Browser-Based Agent Execution Visualizer (`tag devui`) | P3 | L (2-4 weeks) | Shipped (Go + Python) |
| [055](PRD-055-issue-to-pr-autonomous-loop.md) | Issue-to-PR Autonomous Loop (`tag issue-solve`) | P1 | M (1-2 weeks) | Shipped (Go + Python) |
| [056](PRD-056-inbound-webhook-trigger-server.md) | Inbound Webhook Trigger Server with HMAC Verification (`tag hooks listen`) | P1 | M (1-2 weeks) | Shipped (Go + Python) |
| [057](PRD-057-automated-test-generation.md) | Automated Test Generation on PR/Commit (`tag ci test-gen`) | P2 | M (1-2 weeks) | Shipped (Go + Python) |
| [058](PRD-058-github-actions-workflow-scaffold.md) | GitHub Actions Workflow Scaffold (`tag ci install-action`) | P2 | XS (1-2 days) | Shipped (Go + Python) — `agentic-ci install-action` (Python) / `eval-ci scaffold` (Go) |
| [059](PRD-059-sast-vuln-auto-remediation.md) | SAST Vulnerability Auto-Remediation from SARIF (`tag ci fix-vuln`) | P2 | M (1-2 weeks) | Shipped (Go) |
| [060](PRD-060-ci-diagnose-auto-fix.md) | CI Failure Root-Cause Analysis + Auto-Fix PR (`tag ci diagnose --auto-fix`) | P2 | M (1-2 weeks) | Shipped (Go) |
| [061](PRD-061-configurable-pr-review-signal-classes.md) | Configurable PR Review Signal Classes (`tag ci review --signals`) | P3 | S (3-5 days) | Shipped (Go) |
| [062](PRD-062-gitlab-ci-pipeline-autogen.md) | GitLab CI/CD Pipeline Auto-Generation (`tag ci gen-pipeline --platform gitlab`) | P3 | M (1-2 weeks) | Shipped (Go) |
| [063](PRD-063-self-healing-flaky-test-detection.md) | Self-Healing Flaky Test Detection (`tag ci flaky-fix`) | P3 | L (2-4 weeks) | Shipped (Go + Python) |
| [064](PRD-064-swe-agent-bash-editor-harness.md) | SWE-Agent-Style Structured Bash+Editor Harness (`tag solve --harness swe`) | P2 | M (1-2 weeks) | Shipped (Go + Python) — ships as `tag swe-solve`, not `tag solve --harness swe` |
| [065](PRD-065-automatic-post-run-memory-extraction.md) | Automatic Post-Run Memory Extraction (`tag memory config set auto_extract`) | P1 | M (1-2 weeks) | Shipped (Go) |
| [066](PRD-066-hybrid-memory-search.md) | Hybrid Memory Search (`tag mem search --mode hybrid`) | P1 | M (5-8 days) | Shipped (Go) |
| [067](PRD-067-hierarchical-memory-tiers.md) | Hierarchical Memory Tiers: Core / Recall / Archival (`tag mem tier`) | P2 | L (2-4 weeks) | Shipped (Go + Python) — `mem2 tier` |
| [068](PRD-068-background-sleep-time-memory-consolidation.md) | Background Sleep-Time Memory Consolidation Agent (`tag memory gc`) | P3 | M (1-2 weeks) | Shipped (Go) |
| [069](PRD-069-temporal-fact-versioning.md) | Temporal Fact Versioning with valid_at/invalid_at (`tag mem fact`) | P3 | M (1-2 weeks) | Shipped (Go + Python) — `mem2 fact` |
| [070](PRD-070-entity-relationship-graph-community-detection.md) | Entity-Relationship Graph with Community Detection (`tag mem graph`) | P3 | L (2-4 weeks) | Shipped (Go + Python) — ships as `tag graph show/query/build`, not `tag mem graph` |
| [071](PRD-071-episodic-memory-session-episodes.md) | Episodic Memory: Structured Session Episode Storage (`tag mem episode`) | P3 | M (1-2 weeks) | Shipped (Go + Python) — `mem2 episode` |
| [072](PRD-072-cross-session-vector-store.md) | Cross-Session Vector Store (`tag mem store`) | P1 | L (8-13 days) | Shipped (Go + Python) — `mem2 store store\|search\|rebuild` |
| [073](PRD-073-live-mcp-registry-sync.md) | Live MCP Registry Sync from modelcontextprotocol.io (`tag mcp registry update`) | P1 | S (3-5 days) | Proposed — `mcp-registry` serves an embedded catalog; neither CLI has a registry `update`/sync verb |
| [074](PRD-074-mcp-oauth-pkce-device-flow.md) | MCP OAuth 2.1 with PKCE + Device Authorization Flow (`tag mcp auth`) | P2 | M (1-2 weeks) | Proposed |
| [075](PRD-075-per-user-entity-scoped-multi-tenant-tool-auth.md) | Per-User Entity-Scoped Multi-Tenant Tool Auth (`tag entity`) | P2 | L (2-4 weeks) | Proposed — no `tag entity` command in either CLI |
| [076](PRD-076-high-value-mcp-server-bundle.md) | High-Value MCP Server Bundle (`tag mcp registry add-curated`) | P2 | XS (1-2 days) | Partial |
| [077](PRD-077-scope-based-tool-filtering.md) | Scope-Based Tool Filtering + Schema Transformation (`tag mcp filter`) | P3 | M (1-2 weeks) | Proposed — no `mcp filter` command in either CLI |
| [078](PRD-078-hitl-tool-approval-audit-trail.md) | Human-in-the-Loop Tool Approval with Pause/Resume + Audit Trail (`tag mcp approve`) | P3 | L (2-4 weeks) | Partial |
| [079](PRD-079-cloud-hosted-tool-execution.md) | Cloud-Hosted Tool Execution with Version Pinning (`tag mcp host`) | P3 | L (2-4 weeks) | Proposed |
| [080](PRD-080-enterprise-idp-sso-mcp-servers.md) | Enterprise IdP SSO Across MCP Servers (`tag mcp sso`) | P3 | XL (4-8 weeks) | Proposed |
| [081](PRD-081-a2a-agent-card-publication.md) | A2A Agent Card Publication (`tag agent-card`) | P1 | S (3-5 days) | Proposed — no `agent-card` command in either CLI |
| [082](PRD-082-multi-agent-team-primitives.md) | Multi-Agent Team Primitives: RoundRobin, Selector, Swarm Handoff (`tag team`) | P2 | M (1-2 weeks) | Proposed — no `team` command in either CLI |
| [083](PRD-083-agent-as-tool-pattern.md) | Agent-as-Tool Pattern: Invoke Specialist Agents as Function Tools (`tag agent tool`) | P2 | M (1-2 weeks) | Proposed |
| [084](PRD-084-a2a-signed-agent-cards.md) | A2A Signed Agent Cards (`tag agent-card sign`) | P2 | M (5-8 days) | Proposed |
| [085](PRD-085-formal-handoff-message-primitive.md) | Formal HandoffMessage Primitive for Decentralized Agent Routing (`tag handoff`) | P3 | M (1-2 weeks) | Proposed — no `handoff` command in either CLI |
| [086](PRD-086-anp-identity-layer-w3c-did.md) | ANP Identity Layer: W3C DID-Based Decentralized Agent Identity (`tag identity`) | P3 | XL (4-8 weeks) | Proposed — no `identity` command in either CLI |
| [087](PRD-087-acp-lightweight-rest-adapter.md) | ACP (IBM) Lightweight REST Adapter for Intra-Cluster Agent Messaging (`tag acp`) | P3 | M (1-2 weeks) | Proposed — no `acp` command in either CLI |
| [088](PRD-088-distributed-agent-runtime-grpc.md) | Distributed Agent Runtime (gRPC Host/Worker for Cross-Machine Agents) (`tag runtime`) | P3 | XL (4-8 weeks) | Proposed — Python `tag runtime` is the managed-runtime passthrough, not a gRPC host/worker |
| [089](PRD-089-sandbox-streaming-stdout-stderr.md) | Real-Time Streaming stdout/stderr from Sandbox (`tag sandbox run --stream`) | P1 | S (3-5 days) | Proposed — `sandbox run` has no `--stream` flag in either CLI |
| [090](PRD-090-sandbox-template-snapshot-system.md) | Sandbox Template/Snapshot System for <200ms Cold Start (`tag sandbox template`) | P2 | M (1-2 weeks) | Proposed — no `sandbox template` verb in either CLI |
| [091](PRD-091-configurable-sandbox-ttl-session-refresh.md) | Configurable Sandbox TTL + Session Refresh (`tag sandbox set-ttl`) | P2 | S (3-5 days) | Proposed — no `sandbox set-ttl` verb in either CLI |
| [092](PRD-092-desktop-gui-sandbox-vnc.md) | Desktop/GUI Sandbox for Computer-Use (Ubuntu + Xfce + VNC Stream) (`tag sandbox run --gui`) | P2 | XL (4–8 weeks) | Proposed — no `sandbox run --gui` flag in either CLI |
| [093](PRD-093-gpu-sandbox-modal-backend.md) | GPU Sandbox via Modal Backend (Complete the Modal Integration Stub) (`tag sandbox run --backend modal --gpu`) | P3 | M (1-2 weeks) | Proposed — `import-modal` configures the Modal backend, but `sandbox run --backend` only accepts `restricted`/`docker` |
| [094](PRD-094-per-sandbox-egress-firewall.md) | Per-Sandbox Egress Firewall Rules (CIDR/Hostname Allow/Deny Lists) (`tag sandbox firewall`) | P3 | M (1-2 weeks) | Partial |
| [095](PRD-095-sandbox-pause-resume.md) | Sandbox Pause/Resume with Billing Pause (`tag sandbox pause / tag sandbox resume`) | P3 | M (1-2 weeks) | Proposed — no `sandbox pause`/`resume` verbs in either CLI |
| [096](PRD-096-persistent-volume-mounts.md) | Persistent Volume Mounts Across Sandbox Runs (`tag sandbox volume`) | P3 | M (1-2 weeks) | Proposed — no `sandbox volume` verb in either CLI |
| [097](PRD-097-sandbox-secrets-vault.md) | Sandbox-Level Secrets Injection via Encrypted Vault (`tag sandbox secret`) | P3 | M (1-2 weeks) | Proposed — no `sandbox secret` verb in either CLI |
| [098](PRD-098-sandbox-stdin-signal-delivery.md) | Process stdin Streaming and Signal Delivery (SIGTERM/SIGKILL/SIGINT) (`tag sandbox signal / tag sandbox write`) | P3 | S (3-5 days) | Proposed — no `sandbox signal`/`write` verbs in either CLI |
| [099](PRD-099-per-second-cost-attribution-sandbox.md) | Per-Second Cost Attribution per Sandbox Run (`tag sandbox costs`) | P3 | S (3-5 days) | Proposed — no `sandbox costs` verb in either CLI |
| [100](PRD-100-sandbox-lifecycle-policies.md) | Auto-Stop/Auto-Archive Lifecycle Policies for Idle Sandboxes (`tag sandbox policy`) | P3 | S (3-5 days) | Proposed — no `sandbox policy` verb in either CLI |
| [101](PRD-101-self-consistency-ensemble.md) | Self-Consistency Ensemble: Sample N, Majority-Vote (`tag submit --samples N --vote majority`) | P2 | S (3-5 days) | Proposed — `tag submit` has no `--samples`/`--vote` flags |
| [102](PRD-102-multi-agent-debate.md) | Multi-Agent Debate Pattern: Two Agents Argue, Judge Decides (`tag debate`) | P2 | M (1-2 weeks) | Proposed — no `debate` command in either CLI |
| [103](PRD-103-dynamic-task-type-classifier-embeddings.md) | Dynamic Task-Type Classifier via Embeddings (vs Static YAML) (`tag route classify`) | P2 | M (1-2 weeks) | Proposed — `tag route` has no `classify` verb |
| [104](PRD-104-node-level-cache-ttl.md) | Node-Level Caching with TTL for Expensive LLM Calls (`tag cache node`) | P2 | M (1-2 weeks) | Proposed — `tag cache` is analytics only (`stats`/`tips`/`trend`); there is no node-level LLM response cache |
| [105](PRD-105-tdag-dependency-first-task-decomposition.md) | Dependency-First Hierarchical Task Decomposition (TDAG) (`tag plan decompose`) | P2 | L (2-4 weeks) | Proposed — no `plan decompose` command in either CLI |
| [106](PRD-106-speculative-action-execution.md) | Speculative Action Execution for Latency Reduction (SPAgent Pattern) (`tag loop start --speculative`) | P3 | L (2-4 weeks) | Proposed — `loop` has no `--speculative` flag in either CLI |
| [107](PRD-107-confidence-aware-model-routing.md) | Confidence-Aware Model Routing with Cost/Accuracy Pareto Optimization (`tag route optimize`) | P3 | L (2-4 weeks) | Proposed — `tag route` has no `optimize` verb |
| [108](PRD-108-magentic-one-orchestrator.md) | MagenticOne Dual-Ledger Orchestrator (`tag orchestrate --mode magentic-one`) | P1 | L (8-13 days) | Proposed — no `orchestrate` command in either CLI |
| [109](PRD-109-human-in-the-loop-interrupt.md) | HITL interrupt()+Command(resume=) (`tag workflow interrupt`) | P1 | M (5-8 days) | Partial |
| [110](PRD-110-loop-state-serialization.md) | Loop State Serialization (`tag workflow checkpoint`) | P1 | M (5-8 days) | Proposed — no `workflow checkpoint` command (`trace checkpoint` is unrelated) |
| [111](PRD-111-dynamic-fan-out-map-reduce.md) | Dynamic Fan-Out/Map-Reduce (`tag workflow fan-out`) | P1 | M (5-8 days) | Proposed — no `workflow fan-out` command in either CLI |
| [112](PRD-112-graph-based-workflow.md) | Graph-Based Workflow Engine (`tag workflow graph`) | P1 | L (8-13 days) | Partial |
| [113](PRD-113-time-travel-debugging.md) | Time-Travel Debugging (`tag workflow rewind`) | P2 | M (5-8 days) | Proposed — no `workflow rewind` command; see PRD-032 for the `trace replay` surface |
| [114](PRD-114-team-orchestration-primitives.md) | Five Team Orchestration Primitives (`tag team`) | P1 | L (8-13 days) | Proposed — no `team` command in either CLI |
| [115](PRD-115-stateful-process-framework.md) | Stateful Process Framework (`tag process`) | P2 | M (5-8 days) | Proposed — no `process` command in either CLI |
| [116](PRD-116-memex-persistent-scratchpad.md) | MemEx Persistent Scratchpad (`tag scratchpad`) | P2 | S (3-5 days) | Proposed — no `scratchpad` command in either CLI |
| [117](PRD-117-playwright-mcp-integration.md) | Playwright MCP Integration (`tag playwright`) | P1 | M (5-8 days) | Proposed — no `playwright` command in either CLI |
| [118](PRD-118-computer-use-cli.md) | Computer Use CLI (`tag computer-use`) | P1 | M (5-8 days) | Proposed — no `computer-use` command in either CLI |
| [119](PRD-119-claude-computer-use-screenshot-loop.md) | Claude Computer-Use Screenshot Loop (`tag cu-loop`) | P1 | M (5-8 days) | Proposed — no `cu-loop` command in either CLI |
| [120](PRD-120-desktop-gui-sandbox-vnc.md) | Desktop GUI Sandbox VNC (`tag sandbox --vnc`) | P2 | L (8-13 days) | Proposed — duplicate scope of PRD-092; no VNC sandbox in either CLI |
| [121](PRD-121-output-guardrail-processor.md) | Output Guardrail Processor (`tag guardrail output`) | P1 | M (5-8 days) | Proposed — no `guardrail` command in either CLI |
| [122](PRD-122-input-guardrail-validator.md) | Input Guardrail Validator (`tag guardrail input`) | P1 | M (5-8 days) | Proposed — no `guardrail` command in either CLI |
| [123](PRD-123-runtime-guardrail-hooks.md) | Runtime Guardrail Hooks/Tripwire (`tag guardrail runtime`) | P1 | M (5-8 days) | Partial |
| [124](PRD-124-guardrail-result-dataclass.md) | GuardrailResult Type (`tag guardrail result`) | P1 | S (1-2 days) | Proposed — no `guardrail` command in either CLI |
| [125](PRD-125-constitutional-ai-policy.md) | Constitutional AI Policy (`tag constitutional`) | P2 | M (5-8 days) | Proposed — no `constitutional` command in either CLI |
| [126](PRD-126-inference-time-tree-search-solve.md) | Inference-Time Multi-Model Tree Search (`tag solve`) | P1 | L (2–3 sprints, ~5 weeks) | Proposed — no `solve` command in either CLI (`swe-solve`/`issue-solve` are unrelated single-model solvers) |
| [127](PRD-127-evolutionary-profile-optimization.md) | Evolutionary Profile Configuration Optimization (`tag evolve`) | P2 | L (2–3 sprints, ~5 weeks) | Proposed — no `evolve` command in either CLI |
| [128](PRD-128-agent-skills-packages.md) | Agent Skills — Portable `SKILL.md` Capability Packages (`tag skill`) | P1 | M | Proposed |
| [129](PRD-129-plan-act-execution-mode.md) | Plan/Act Execution Mode — read-only planning before mutation (`tag run --mode plan`) | P1 | S–M | Proposed |
| [130](PRD-130-git-worktree-isolation.md) | Git Worktree Isolation for Parallel Agents (`tag worktree`) | P2 | M | Proposed |
| [131](PRD-131-zed-agent-client-protocol-editor-bridge.md) | Zed Agent **Client** Protocol — editor↔agent bridge (`tag editor serve`) | P2 | M | Proposed |
| [132](PRD-132-provider-adapter-breadth-catalog.md) | Provider Adapter Breadth: multi-provider adapter + model catalog (`tag providers`) | P2 | M | Proposed |

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

16. **PRD-029** — Streaming TUI Dashboard / `tag serve` (builds on PRD-003 Rich TUI, PRD-008 queue, PRD-013 tracing)
17. **PRD-017** — Multi-Model Benchmarking (extends existing benchmark system)
18. **PRD-019** — Natural Language Shell (new REPL module)
19. **PRD-020** — CI/CD Integration (gh CLI + GitHub Actions template)
20. **PRD-007** — Desktop App (Electron build from vendor tarball)
21. **PRD-010** — Dashboard Upgrade (minimal changes, big discoverability win)
22. **PRD-035** — IDE Bridge (LSP server + VS Code extension — editor-native TAG code actions)

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
| `src/tag/dashboard.py` | 029 |
| `src/tag/api.py` | 029 |
| `src/tag/lsp_server.py` | 035 |
| `vscode/` (extension package) | 035 |
| `src/tag/tool_retrieval.py` | 043 |
| `src/tag/vector_store.py` | 025 (semantic memory), 043 (shared ChromaDB client) |

---

## Feature Coverage by Domain

### Memory
- PRD-001: Hermes memory backend selection (Supermemory, Honcho, local)
- PRD-002: TAG-native cross-session facts journal
- PRD-025: Semantic memory with confidence decay (ChromaDB + sentence-transformers, local embeddings)
- PRD-018: Context window management and auto-summarization
- PRD-065: Automatic post-run memory extraction
- PRD-066: Hybrid memory search (BM25 + vector RRF fusion)
- PRD-067: Hierarchical memory tiers (core/recall/archival)
- PRD-068: Background sleep-time memory consolidation
- PRD-069: Temporal fact versioning
- PRD-070: Entity-relationship graph and community detection
- PRD-071: Episodic memory session episodes
- PRD-072: Cross-session vector store (LanceDB embedded)
- PRD-116: MemEx persistent scratchpad

### Developer Experience (TUI / UX)
- PRD-003: Rich streaming output, spinners, progress bars
- PRD-007: Electron desktop app
- PRD-009: Enhanced diagnostics
- PRD-019: Natural language shell REPL
- PRD-054: Local browser DevUI

### Evaluation & Observability
- PRD-045: LLM-as-judge evaluators
- PRD-046: Per-span USD cost attribution
- PRD-047: Eval CI gate PR comment
- PRD-048: Structured tool call child spans
- PRD-049: Versioned eval dataset management
- PRD-050: Alert rules on metric thresholds
- PRD-051: Human annotation and labeling queue
- PRD-052: Prompt versioning hub
- PRD-053: TraceProcessor lifecycle hooks
- PRD-041: OTel GenAI span cost attribution
- PRD-044: AgentOps session observability

### CI/CD & Dev Workflows
- PRD-055: Issue-to-PR autonomous loop
- PRD-056: Inbound webhook trigger server
- PRD-057: Automated test generation
- PRD-058: GitHub Actions workflow scaffold
- PRD-059: SAST vulnerability auto-remediation
- PRD-060: CI diagnose auto-fix
- PRD-061: Configurable PR review signal classes
- PRD-062: GitLab CI pipeline autogen
- PRD-063: Self-healing flaky test detection
- PRD-064: SWE-agent bash/editor harness

### Multi-Agent Orchestration
- PRD-004: Kanban swarm helpers
- PRD-008: Background task queue
- PRD-016: Webhook event triggers
- PRD-082: Multi-agent team primitives
- PRD-108: MagenticOne dual-ledger orchestrator
- PRD-114: Five team orchestration primitives (sequential/hierarchical/supervisor/debate/swarm)

### Agentic Workflow State
- PRD-109: HITL interrupt()+Command(resume=)
- PRD-110: Loop state serialization (SqliteCheckpointer)
- PRD-111: Dynamic fan-out/map-reduce (Send API)
- PRD-112: Graph-based workflow engine (WorkflowGraph)
- PRD-113: Time-travel debugging
- PRD-115: Stateful process framework (@process decorator)

### MCP Ecosystem & Tool Connectivity
- PRD-073: Live MCP registry sync
- PRD-074: MCP OAuth PKCE device flow
- PRD-075: Per-user entity-scoped multi-tenant tool auth
- PRD-076: High-value MCP server bundle
- PRD-077: Scope-based tool filtering
- PRD-078: HITL tool approval audit trail
- PRD-079: Cloud-hosted tool execution
- PRD-080: Enterprise IDP SSO MCP servers

### Multi-Agent Interoperability
- PRD-081: A2A agent card publication
- PRD-083: Agent-as-tool pattern
- PRD-084: A2A signed agent cards (Ed25519 JWS)
- PRD-085: Formal handoff message primitive
- PRD-086: ANP identity layer (W3C DID)
- PRD-087: ACP lightweight REST adapter
- PRD-088: Distributed agent runtime (gRPC)

### Sandbox & Execution
- PRD-028: Sandbox code execution
- PRD-089: Sandbox streaming stdout/stderr
- PRD-090: Sandbox template snapshot system
- PRD-091: Configurable sandbox TTL and session refresh
- PRD-092: Desktop GUI sandbox VNC
- PRD-093: GPU sandbox (Modal backend)
- PRD-094: Per-sandbox egress firewall
- PRD-095: Sandbox pause/resume
- PRD-096: Persistent volume mounts
- PRD-097: Sandbox secrets vault
- PRD-098: Sandbox stdin signal delivery
- PRD-099: Per-second cost attribution (sandbox)
- PRD-100: Sandbox lifecycle policies

### Advanced Reasoning
- PRD-101: Self-consistency ensemble
- PRD-102: Multi-agent debate
- PRD-103: Dynamic task type classifier (embeddings)
- PRD-104: Node-level cache TTL
- PRD-105: TDAG dependency-first task decomposition
- PRD-106: Speculative action execution
- PRD-107: Confidence-aware model routing

### Computer Use & Browser Automation
- PRD-117: Playwright MCP integration
- PRD-118: Computer use CLI (`tag computer-use`)
- PRD-119: Claude computer use screenshot loop
- PRD-120: Desktop GUI sandbox VNC

### Security & Guardrails
- PRD-121: Output guardrail processor
- PRD-122: Input guardrail validator
- PRD-123: Runtime guardrail hooks/tripwire
- PRD-124: GuardrailResult dataclass
- PRD-125: Constitutional AI policy (critique-revision loop)

### Provider & Tool Integrations
- PRD-005: Execution backends (Docker, SSH, Modal, Daytona)
- PRD-006: Nous Portal Tool Gateway
- PRD-011: Plugin management
- PRD-014: MCP server registry
- PRD-043: Vector-based tool retrieval (ChromaDB index over MCP tools, top-K selection at query time)

### Observability & Operations
- PRD-012: Cost tracking and budgets
- PRD-013: Distributed tracing
- PRD-009: Doctor diagnostics

### Observability & Live Dashboards
- PRD-029: Streaming TUI Dashboard — live token stream, cost ticker, tool call inspector, queue status, web bridge

### Collaboration & Ecosystem
- PRD-015: Profile templates and sharing
- PRD-017: Multi-model benchmarking
- PRD-020: CI/CD integration and automated code review
- PRD-026: Profile Marketplace — GitHub-based profile distribution with SHA pinning, secret scanning, and Gist push (BLOCKED on PRD-034)

