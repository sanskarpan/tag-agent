# TAG Feature Backlog — Master PRD Index

This directory contains Product Requirements Documents for the TAG (tag-agent) platform's feature backlog. Each PRD defines a discrete capability: problem statement, goals, success criteria, technical design, implementation plan, and risk assessment. PRDs are numbered sequentially and may carry hard or soft dependencies on other PRDs.

---

## Summary Table

| PRD# | Title | Category | Priority | Complexity | Status | Dependencies |
|------|-------|----------|----------|------------|--------|--------------|
| [021](PRD-021-agent-loop-autonomous-mode.md) | Agent Loop / Autonomous Mode | Core | P0 Critical | M | Proposed | — |
| [022](PRD-022-cron-scheduled-agents.md) | Cron / Scheduled Agents | Core | P1 High | M | Proposed | PRD-021 (optional) |
| [023](PRD-023-multi-agent-swarm-context-routing.md) | Multi-Agent Swarm (Context-Centric) | Core | P1 High | L | Proposed | PRD-021 (required), PRD-024 (optional) |
| [024](PRD-024-repo-map-workspace-context.md) | Repo-Map / Workspace Context | AI-Native | P1 High | L | Proposed | — |
| [025](PRD-025-semantic-memory-confidence-decay.md) | Semantic Memory with Confidence Decay | AI-Native | P2 Medium | L | Proposed | — |
| [026](PRD-026-profile-marketplace.md) | Profile Marketplace | DX | P2 Medium | M | Proposed — BLOCKED | PRD-034 (required) |
| [027](PRD-027-eval-framework.md) | Eval Framework (DeepEval) | Observability | P1 High | M | Proposed | — |
| [028](PRD-028-sandbox-code-execution.md) | Sandbox Code Execution | Security | P0 Critical | L | Proposed | — |
| [029](PRD-029-streaming-tui-dashboard.md) | Streaming TUI Dashboard | DX | P2 Medium | L | Proposed | — |
| [030](PRD-030-prompt-cache-analytics.md) | Prompt Cache Analytics | Observability | P1 High | S | Proposed | — |
| [031](PRD-031-model-fallback-chains.md) | Model Fallback Chains | Integrations | P2 Medium | M | Proposed | — |
| [032](PRD-032-agent-replay-time-travel-debugging.md) | Agent Replay / Time-Travel Debugging | Observability | P2 Medium | L | Proposed | PRD-013 |
| [033](PRD-033-dependency-aware-task-queue.md) | Dependency-Aware Task Queue | Core | P1 High | M | Proposed | PRD-008 |
| [034](PRD-034-secret-scanning.md) | Secret Scanning | Security | P0 Critical | S | Proposed | — |
| [035](PRD-035-ide-bridge-lsp.md) | IDE Bridge (LSP + VS Code) | Integrations | P2 Medium | XL | Proposed | — |
| [036](PRD-036-web-dashboard.md) | Web Dashboard (`tag serve`) | DX | P2 Medium | L | Proposed | — |
| [037](PRD-037-agent-personas.md) | Agent Personas | AI-Native | P2 Medium | M | Proposed | PRD-026 (optional) |
| [038](PRD-038-diff-aware-context-injection.md) | Diff-Aware Context Injection | DX | P1 High | S | Proposed | — |
| [039](PRD-039-token-budget-enforcement.md) | Token Budget Enforcement | Core | P1 High | S | Proposed | PRD-012 |
| [040](PRD-040-notification-hooks.md) | Notification Hooks | Integrations | P2 Medium | M | Proposed | — |
| [041](PRD-041-otel-genai-span-cost-attribution.md) | OTel GenAI Span Cost Attribution | Observability | P2 Medium | S | Proposed | — |
| [042](PRD-042-architect-editor-agent-split.md) | Architect/Editor Agent Split | AI-Native | P2 Medium | S–M | Proposed | PRD-021 (required) |
| [043](PRD-043-vector-based-tool-retrieval.md) | Vector-Based Tool Retrieval | AI-Native | P3 | M | Proposed | PRD-025 (shared infra) |
| [044](PRD-044-agentops-session-observability.md) | AgentOps Session Observability | Observability | P3 | S | Proposed | — |

---

## Roadmap

### Ship Next Sprint

P0–P1 features with Small or Medium complexity. These can be executed immediately and unblock later work.

| PRD# | Title | Priority | Complexity | Blocking? |
|------|-------|----------|------------|-----------|
| 021 | Agent Loop / Autonomous Mode | P0 Critical | M | Unblocks PRD-022, PRD-023, PRD-042 |
| 034 | Secret Scanning | P0 Critical | S | Unblocks PRD-026 |
| 022 | Cron / Scheduled Agents | P1 High | M | — |
| 027 | Eval Framework (DeepEval) | P1 High | M | — |
| 030 | Prompt Cache Analytics | P1 High | S | — |
| 033 | Dependency-Aware Task Queue | P1 High | M | — |
| 038 | Diff-Aware Context Injection | P1 High | S | — |
| 039 | Token Budget Enforcement | P1 High | S | — |

### Ship Next Quarter

P1–P2 features with Medium or Large complexity, or P0 features whose dependencies land in the sprint above.

| PRD# | Title | Priority | Complexity | Notes |
|------|-------|----------|------------|-------|
| 028 | Sandbox Code Execution | P0 Critical | L | Security-critical; high effort |
| 023 | Multi-Agent Swarm (Context-Centric) | P1 High | L | Requires PRD-021 |
| 024 | Repo-Map / Workspace Context | P1 High | L | — |
| 026 | Profile Marketplace | P2 Medium | M | Requires PRD-034 to ship first |
| 029 | Streaming TUI Dashboard | P2 Medium | L | — |
| 031 | Model Fallback Chains | P2 Medium | M | — |
| 032 | Agent Replay / Time-Travel Debugging | P2 Medium | L | — |
| 036 | Web Dashboard (`tag serve`) | P2 Medium | L | — |
| 037 | Agent Personas | P2 Medium | M | Optional dep on PRD-026 |
| 040 | Notification Hooks | P2 Medium | M | — |
| 041 | OTel GenAI Span Cost Attribution | P2 Medium | S | — |
| 042 | Architect/Editor Agent Split | P2 Medium | S | Requires PRD-021 |

### Future Vision

Large/XL efforts, P3 priority, or features requiring significant ecosystem maturity.

| PRD# | Title | Priority | Complexity | Notes |
|------|-------|----------|------------|-------|
| 025 | Semantic Memory with Confidence Decay | P2 Medium | L | Foundational for PRD-043 |
| 035 | IDE Bridge (LSP + VS Code) | P2 Medium | XL | Major scope; editor-ecosystem bet |
| 043 | Vector-Based Tool Retrieval | P3 | M | Depends on PRD-025 infra |
| 044 | AgentOps Session Observability | P3 | S | Third-party platform dependency |

---

## Security Risk Flags

The following PRDs carry elevated security risk and require dedicated threat-model review before implementation begins.

| PRD# | Title | Risk | Mitigation Required |
|------|-------|------|---------------------|
| **028** | Sandbox Code Execution | **Critical** — arbitrary code execution; escape risk in Docker/E2B/Modal; privilege escalation if sandbox misconfigured | Defense-in-depth isolation (seccomp, namespaces, network egress block); independent security review before any production deployment |
| **034** | Secret Scanning | **Critical** — scanning logic itself may buffer secrets in memory or logs; false-negative misses could provide false confidence | No plaintext logging of matched secrets; fuzzing of regex patterns; integration test with canonical secret fixtures |
| **026** | Profile Marketplace | **High** — community-sourced profiles could inject malicious system prompts or tool configs | Mandatory PRD-034 secret scan gate; SHA-256 pinning on every pulled profile; signature verification roadmap |
| **023** | Multi-Agent Swarm | **High** — agent-to-agent message passing without integrity checks enables prompt injection across agents | Signed inter-agent envelopes; agent identity scoping; output sanitization at swarm coordinator |
| **042** | Architect/Editor Agent Split | **Medium** — architect agent may pass unvalidated instructions to editor agents | Instruction schema validation; scope-limited editor tool grants |
| **035** | IDE Bridge (LSP + VS Code) | **Medium** — LSP server process has filesystem and shell access from within the editor | Restricted tool allowlist per workspace; explicit user consent for each tool category |

---

## Dependencies Graph

Arrows indicate "depends on" (A -> B means A depends on B).

```
PRD-042 (Architect/Editor Split)  ──requires──>  PRD-021 (Agent Loop)
PRD-023 (Multi-Agent Swarm)       ──requires──>  PRD-021 (Agent Loop)
PRD-026 (Profile Marketplace)     ──requires──>  PRD-034 (Secret Scanning)

PRD-022 (Cron/Scheduled Agents)   ──optional──>  PRD-021 (Agent Loop)
PRD-023 (Multi-Agent Swarm)       ──optional──>  PRD-024 (Repo-Map)
PRD-037 (Agent Personas)          ──optional──>  PRD-026 (Profile Marketplace)
PRD-043 (Vector Tool Retrieval)   ──shared infra─>  PRD-025 (Semantic Memory)
```

### Expanded dependency chains

```
PRD-043
  └─shared infra──> PRD-025 (Semantic Memory)

PRD-026 (Profile Marketplace)
  └─requires──> PRD-034 (Secret Scanning)
       └─ no deps

PRD-037 (Agent Personas)
  └─optional──> PRD-026 (Profile Marketplace)
       └─requires──> PRD-034

PRD-042 (Architect/Editor Split)
  └─requires──> PRD-021 (Agent Loop)
       └─ no deps

PRD-023 (Multi-Agent Swarm)
  ├─requires──> PRD-021 (Agent Loop)
  └─optional──> PRD-024 (Repo-Map)

PRD-022 (Cron/Scheduled Agents)
  └─optional──> PRD-021 (Agent Loop)
```

### PRDs with no dependencies (can start immediately)

PRD-021, PRD-024, PRD-025, PRD-027, PRD-028, PRD-029, PRD-030, PRD-031, PRD-032, PRD-033, PRD-034, PRD-035, PRD-036, PRD-038, PRD-039, PRD-040, PRD-041, PRD-044

---

## Related

- [docs/prd/INDEX.md](INDEX.md) — PRD-001 through PRD-020 (prior backlog cohort)
- [docs/](../) — architecture diagrams, logo assets

