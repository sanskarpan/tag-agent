# TAG — Complete Feature List

> Consolidated, status-verified feature inventory for the TAG agent-orchestration platform.
> Cross-checked against the live CLI surface (**103 commands**) and the PRD catalog (**PRD-001–127**,
> clusters A–K). Sources: `docs/prd/INDEX.md`, `docs/FEATURES_ROADMAP.md`, and `src/tag/`.

**Legend:** ✅ implemented & shipping (working command) · 📋 planned/proposed (PRD written, not yet built)

**At a glance:** ~72 features implemented across 103 commands (PRD-001–072) · ~55 planned (PRD-073–127).

---

## 0. Core platform (foundation)

- ✅ **Control-plane CLI** wrapping the Hermes agent runtime — the `tag` binary, 103 subcommands
- ✅ **Multi-profile orchestration** — 5 built-in profiles (orchestrator, researcher, coder, reviewer, codex-runtime-master)
- ✅ **Task routing engine** — 4 routes (research / implementation / review / mixed); master/worker/verifier roles; Kanban vs direct execution
- ✅ **Managed runtime provisioning** — `setup`, bundled 52 MB Hermes tarball, branding-patch application (pre-patched-bundle aware), TUI build, per-profile isolated HOMEs
- ✅ **Dual distribution** — pip (`tag-agent`) + npm (auto-provisions a Python venv); Python 3.11–3.13
- ✅ **Branding layer** — Hermes→TAG dual-surface text rewrite (mirrored Python + Node)
- ✅ **SQLite state** — runs/steps/spans/memory/queue/etc. (WAL, atomic + lock-serialized config writes)

---

## 1. Setup, diagnostics & config

| Status | Command(s) | Feature | PRD |
|---|---|---|---|
| ✅ | `setup`, `bootstrap`, `render`, `env` | Provision managed runtime, render per-profile config | — |
| ✅ | `doctor` | Comprehensive health check (pass/warn/fail per component) | PRD-009 |
| ✅ | `config`, `status`, `update` | Config passthrough, status, self-update | — |
| ✅ | `runtime`, `tui`, `chat`, `gateway`, `completion`, `prompt-size`, `logs`, `sessions`, `skills`, `plugins`, `tools`, `mcp`, `model`, `dashboard` | Managed-runtime passthrough surface | — |

## 2. Credential import (18 sources)

| Status | Command(s) | Feature | PRD |
|---|---|---|---|
| ✅ | `import-codex/claude/gemini/continue/mistral/opencode/zed/copilot/aider/aws/cursor/supermemory/honcho/nous-portal` | Multi-source credential import | PRD-001, PRD-006 |
| ✅ | `import-docker/ssh/modal/daytona` | Execution-backend selection per profile | PRD-005 |

## 3. Routing & models

| Status | Command(s) | Feature | PRD |
|---|---|---|---|
| ✅ | `route`, `assignments`, `models`, `set-model`, `submit`, `openrouter-models`, `runs` | Task routing, model assignment, submission, run history | — |
| ✅ | `benchmark`, `compare` | Multi-model benchmarking & comparison | PRD-017 |
| ✅ | `route-fallback` | Model fallback chains (with cycle detection); walked at runtime by the Go harness via `run --fallback` | PRD-031 |

## 4. Memory subsystem

| Status | Command(s) | Feature | PRD |
|---|---|---|---|
| ✅ | `memory-journal` | Cross-session memory journal | PRD-002 |
| ✅ | `mem` | Semantic memory with confidence decay + FTS | PRD-025 |
| ✅ | `mem2 gc` | Sleep-time memory consolidation / garbage collection | PRD-068 |
| ✅ | `mem2 extract` | Automatic post-run memory extraction | PRD-065 |
| ✅ | `mem2 tier` | Hierarchical memory tiers (core/recall/archival) | PRD-067 |
| ✅ | `mem2 fact` | Temporal fact versioning | PRD-069 |
| ✅ | `mem2 episode` | Episodic memory (session episodes) | PRD-071 |
| ✅ | `mem2 store` | Cross-session vector store / hybrid search | PRD-066, PRD-072 |
| ✅ | `graph` | Entity-relationship graph + community detection | PRD-070 |
| ✅ | (per-profile config) | Structured memory configuration | PRD-001 |

## 5. Queue, DAG & swarm

| Status | Command(s) | Feature | PRD |
|---|---|---|---|
| ✅ | `queue` | Background task queue + notifications | PRD-008 |
| ✅ | `dag`, `queue-dep` | Dependency-aware task queue / DAG engine (cycle detection) | PRD-033 |
| ✅ | `swarm` | Multi-agent swarm, context routing | PRD-004, PRD-023 |
| ✅ | `kanban` | Kanban topology helpers | PRD-004 |

## 6. Observability & cost

| Status | Command(s) | Feature | PRD |
|---|---|---|---|
| ✅ | `costs`, `pricing` | Cost tracking / per-span USD attribution | PRD-012, PRD-046 |
| ✅ | `trace` (list/show/export/**replay**/diff/checkpoint/snapshot) | Agent tracing + time-travel/replay debugging | PRD-013, PRD-032 |
| ✅ | `cache` | Prompt-cache analytics | PRD-030 |
| ✅ | `otel-export` | OTel GenAI semconv span export | PRD-041, PRD-048 |
| ✅ | `agentops` | AgentOps session observability | PRD-044 |

## 7. Eval & quality

| Status | Command(s) | Feature | PRD |
|---|---|---|---|
| ✅ | `eval` | Eval framework | PRD-027 |
| ✅ | `eval-judge` | LLM-as-judge evaluators | PRD-045 |
| ✅ | `eval-dataset` | Versioned eval dataset management | PRD-049 |
| ✅ | `eval-ci` | Eval CI gate + PR comment + GH Action scaffold | PRD-047 |
| ✅ | `alert` | Alert rules on metric thresholds | PRD-050 |
| ✅ | `annotate` | Human annotation / labeling queue | PRD-051 |
| ✅ | `prompt` | Prompt versioning hub | PRD-052 |

## 8. Agent tools

| Status | Command(s) | Feature | PRD |
|---|---|---|---|
| ✅ | `security` | Secret scanning & security audit | PRD-034 |
| ✅ | `persona` | Agent personas | PRD-037 |
| ✅ | `diff-context` | Diff-aware context injection | PRD-038 |
| ✅ | `budget` | Token budget enforcement | PRD-039 |
| ✅ | `notify` | Notification hooks (Slack/email/desktop) | PRD-040 |
| ✅ | `split` | Architect/editor agent split | PRD-042 |
| ✅ | `tool-index` | Vector-based tool retrieval | PRD-043 |
| ✅ | `sandbox` | Isolated code execution (restricted / Docker) | PRD-028 |
| ✅ | `context` | Context-window management | PRD-018 |

## 9. CI/CD & agentic dev workflows

| Status | Command(s) | Feature | PRD |
|---|---|---|---|
| ✅ | `review-pr`, `ci` | CI/CD integration + configurable PR-review signal classes | PRD-020, PRD-061 |
| ✅ | `loop` | Autonomous agent loop (goal detection, iteration cap, human approve/deny) | PRD-021 |
| ✅ | `cron` | Cron-style scheduled agent runs | PRD-022 |
| ✅ | `workspace` | Repo-map / workspace context indexing | PRD-024 |
| ✅ | `issue-solve` | Issue-to-PR autonomous loop | PRD-055 |
| ✅ | `webhook` | Inbound webhook trigger server (HMAC-verified) | PRD-056 |
| ✅ | `agentic-ci test-gen` | Automated test generation from diffs | PRD-057 |
| ✅ | `agentic-ci gen-pipeline` | GitHub Actions / GitLab CI pipeline autogen | PRD-058, PRD-062 |
| ✅ | `agentic-ci fix-vuln` | SAST/SARIF vulnerability auto-remediation | PRD-059 |
| ✅ | `agentic-ci ci-diagnose` | CI-failure diagnose & auto-fix | PRD-060 |
| ✅ | `agentic-ci flaky-fix` | Self-healing flaky-test detection | PRD-063 |
| ✅ | `swe-solve` | SWE-agent bash/editor harness | PRD-064 |

## 10. Marketplace, plugins, templates & MCP

| Status | Command(s) | Feature | PRD |
|---|---|---|---|
| ✅ | `marketplace` | Profile marketplace (pull/push) | PRD-026 |
| ✅ | `template` | Profile templates & sharing | PRD-015 |
| ✅ | `hooks` | Webhook / lifecycle event hooks | PRD-016 |
| ✅ | `mcp-registry` | Curated MCP server registry | PRD-014 |
| ✅ | `plugin` | Plugin management | PRD-011 |
| ✅ | `shell` | Natural-language TAG shell | PRD-019 |

## 11. Dashboards, UI & IDE

| Status | Command(s) | Feature | PRD |
|---|---|---|---|
| ✅ | `serve`, `web`, `dashboard` | HTTP dashboard + admin panel (SSE streaming) | PRD-010, PRD-036, PRD-029 |
| ✅ | `devui` | Local browser DevUI | PRD-054 |
| ✅ | `lsp` | IDE bridge / LSP server | PRD-035 |
| ✅ | `desktop` | Electron desktop app launcher | PRD-007 |
| ✅ | (runtime) | Rich streaming TUI (spinners/progress/status bar) | PRD-003 |

Library-level features backing the above (no dedicated command): TraceProcessor lifecycle hooks (PRD-053),
structured tool-call child spans (PRD-048).

---

## 12. 📋 Planned / proposed — clusters D–K (PRD-073–127)

> PRDs written during the v0.6.x–v0.7.x planning cycles; not yet implemented. See `docs/prd/` for each spec.

### D · MCP ecosystem & tool connectivity (PRD-073–080)
📋 Live MCP registry sync · MCP OAuth PKCE/device flow · per-user entity-scoped multi-tenant tool auth ·
high-value MCP server bundle · scope-based tool filtering · HITL tool approval + audit trail ·
cloud-hosted tool execution · enterprise IdP/SSO for MCP servers.

### E · Multi-agent interoperability (PRD-081–088)
📋 A2A agent-card publication · multi-agent team primitives · agent-as-tool pattern · A2A signed agent
cards · formal handoff message primitive · ANP identity layer (W3C DID) · ACP lightweight REST adapter ·
distributed agent runtime (gRPC).

### F · Sandbox & execution environment (PRD-089–100)
📋 Sandbox streaming stdout/stderr · template/snapshot system · configurable TTL + session refresh ·
desktop-GUI sandbox (VNC) · GPU sandbox (Modal) · per-sandbox egress firewall · pause/resume ·
persistent volume mounts · sandbox secrets vault · stdin/signal delivery · per-second cost attribution ·
sandbox lifecycle policies.

### G · Advanced reasoning & planning (PRD-101–108)
📋 Self-consistency ensemble · multi-agent debate · dynamic task-type classifier (embeddings) ·
node-level cache TTL · TDAG dependency-first decomposition · speculative action execution ·
confidence-aware model routing · Magentic-One orchestrator.

### H · Agentic workflow state & graph (PRD-109–116)
📋 Human-in-the-loop interrupt · loop-state serialization · dynamic fan-out/map-reduce ·
graph-based workflow · time-travel debugging · team-orchestration primitives · stateful process
framework · memex persistent scratchpad.

### I · Computer use & browser automation (PRD-117–120)
📋 Playwright MCP integration · computer-use CLI · Claude computer-use screenshot loop ·
desktop-GUI sandbox (VNC).

### J · Security & guardrails (PRD-121–125)
📋 Output guardrail processor · input guardrail validator · runtime guardrail hooks ·
guardrail result dataclass · constitutional-AI policy.

### K · Sakana-gap features (PRD-126–127) + enhancements
📋 `tag solve` — inference-time multi-model tree search (AB-MCTS-inspired) ·
📋 `tag evolve` — evolutionary profile-config optimization ·
📋 planned enhancements to existing PRDs: Trinity-style per-turn Thinker/Worker/Verifier roles (PRD-082),
diverse-profile ensemble with reviewer-judge/tournament/synthesize modes (PRD-101), per-wave
self-review/self-improve for swarm (PRD-023).

---

## Summary

| | Count |
|---|---|
| ✅ Implemented features (PRD-001–072) | ~72 |
| ✅ Live CLI commands | 103 |
| 📋 Planned features (PRD-073–127, clusters D–K) | ~55 |
| **Total PRDs cataloged** | **127** |

*Generated by cross-referencing the live `tag --help` surface against `docs/prd/INDEX.md`. Implemented
status = a working command exists for the PRD; planned = PRD spec written, no command yet.*
