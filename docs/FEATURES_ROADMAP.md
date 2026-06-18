# TAG Feature Roadmap

Deep competitive research across LangGraph, AutoGen 0.4 / MagenticOne, OpenAI Agents SDK, Microsoft Semantic Kernel / MAF, E2B / Daytona / Modal, LangSmith / W&B Weave / Braintrust / Arize Phoenix, mem0 / Zep / Letta / GraphRAG, Composio / MCP ecosystem / Arcade AI, Devin / SWE-agent / GitHub Copilot Coding Agent, and A2A / ANP / ACP protocol landscape.

**118 raw findings → 10 clusters → ranked implementation plan.**

---

## Key Industry Trends (2025–2026)

1. **MCP registry explosive growth** — The official registry grew 407% to 9,652 servers by May 2026. TAG's 10-entry static YAML is a critical bottleneck. Live sync is the highest-leverage single addition.
2. **LLM-as-judge is now table stakes** — Every major eval platform ships semantic LLM scoring. TAG's keyword/regex scorer is 2 years behind. The eval table schema exists — the judge module is the only missing piece.
3. **Event-driven agent activation** — The industry has shifted from CLI-only invocation to inbound webhook triggers (GitHub push, Jira transition, Slack @mention → agent task). TAG has outbound notifications but zero inbound trigger surface.
4. **A2A as the interoperability baseline** — A2A v1.0 (Linux Foundation) is now implemented by MAF, LangGraph, CrewAI, and 150+ platforms. TAG is the only major agent CLI missing an agent card.
5. **Memory goes ambient** — Leading memory frameworks (mem0, Letta, Zep) moved from opt-in journaling to automatic extraction from every conversation turn. TAG's manual `tag memory-journal save` is a friction point.
6. **Sandbox as a first-class service** — E2B, Daytona, Modal have shown production sandboxes need: pause/resume, TTL management, streaming stdout, template snapshots, and per-second cost attribution. TAG's `subprocess.run` model is fine for dev, not production loops.
7. **Issue-to-PR as the flagship agentic dev capability** — Devin, Copilot Coding Agent, and Linear Agent defined "autonomous issue resolution" as the primary value prop for developer AI tools in 2025–2026.
8. **Cost observability becoming mandatory** — Per-span USD attribution (not just token counts) is expected in every production AI system. TAG has tokens but no dollars.
9. **Eval CI gate = quality assurance standard** — The 2025–2026 consensus: every AI system needs both LLM-based quality scoring AND a CI gate that blocks merges when eval scores regress.
10. **Self-consistency as the easiest reliability win** — Research consensus: sampling 3 outputs and majority-voting outperforms single-shot at 2–3x cost — acceptable for high-stakes tasks. TAG already has ThreadPoolExecutor and swarm fan-out.

---

## Top 10 — Ranked by Impact × Feasibility

### #1 LLM-as-Judge Eval Evaluators (online + offline)

**Cluster:** Evaluation & Observability  
**Inspired by:** LangSmith, Braintrust, Arize Phoenix, W&B Weave  
**Difficulty:** 2/5 | **Impact:** 5/5

TAG already has the eval table schema, profile system, and controller wiring. Adding an LLM judge call is ~200 lines and immediately elevates eval quality from heuristic to semantic.

```bash
# Offline: score an existing eval suite with a judge model
tag eval run --judge claude-sonnet-4-6 --criteria factuality,relevance,safety --suite my-suite.yaml

# Online: sample production runs and score them asynchronously
tag eval run --online --sample-rate 0.1 --judge claude-haiku-4-5

# Inspect scores
tag eval judge show --run-id <id> --json
tag eval report --suite my-suite --by criterion --json
```

**Implementation target:** `src/tag/eval_judge.py`

---

### #2 Issue-to-PR Autonomous Loop (`tag issue-solve`)

**Cluster:** CI/CD & Agentic Dev Workflows  
**Inspired by:** Devin, GitHub Copilot Coding Agent, Linear Agent  
**Difficulty:** 3/5 | **Impact:** 5/5

TAG already has `loop_agent`, `sandbox`, `ci.py` with `gh` integration, and `diff_context`. The gap is one new `cmd_issue_solve` function that chains these. Copilot Coding Agent and Devin are winning market share specifically on this use case.

```bash
# Resolve a GitHub issue end-to-end: clone → plan → code → test → PR
tag issue-solve --issue https://github.com/owner/repo/issues/42 \
  --profile coder --sandbox docker

# Linear / Jira platforms
tag issue-solve --issue LINEAR-123 --platform linear --profile coder --auto-pr
tag issue-solve --issue JIRA-456 --platform jira --dry-run

# Auto-detect from branch name
tag issue-solve --auto --profile coder
```

**Implementation target:** `cmd_issue_solve()` in `controller.py` + `src/tag/issue_solver.py`

---

### #3 MCP Registry Live Sync from modelcontextprotocol.io

**Cluster:** MCP Ecosystem & Tool Connectivity  
**Inspired by:** MCP Registry (modelcontextprotocol.io), community rankings from Smithery/mcp.so  
**Difficulty:** 2/5 | **Impact:** 5/5

`cmd_mcp_registry` already exists and `_load_mcp_registry()` parses the YAML. The `update` subcommand is mentioned in PRD-014 but not implemented. The registry OpenAPI is stable (frozen Oct 2025). This multiplies TAG's tool surface from 10 to 9,000+ servers.

```bash
# Sync from official registry
tag mcp registry update [--source https://registry.modelcontextprotocol.io]

# Search the full catalog
tag mcp registry search "calendar scheduling"
tag mcp registry search "browser automation" --json

# Install top community servers
tag mcp registry install notion playwright-mcp context7 github
tag mcp registry add-curated  # installs the top-10 most-used servers

# MCP OAuth 2.1 device flow for CLI auth
tag mcp auth notion --scopes read,write
tag mcp auth github --org myorg
```

**Implementation target:** `_cmd_mcp_registry_update()` + MCP OAuth device flow in `controller.py`

---

### #4 Per-Span USD Cost Attribution with Pricing Table

**Cluster:** Evaluation & Observability  
**Inspired by:** LangSmith, W&B Weave, Braintrust, E2B per-second billing  
**Difficulty:** 1/5 | **Impact:** 4/5

Token data already exists in spans table. A pricing table YAML and a 30-line compute function at `close_span()` time is all that's needed. Budget.py already tracks total spend; this adds per-span granularity.

```bash
# View cost breakdown for a run
tag trace show --run-id <id> --cost

# Aggregate costs by model, profile, or time window
tag stats --cost --since 7d
tag stats --cost --by model --json
tag stats --cost --by profile --json

# Enhanced budget enforcement (per-run limit)
tag budget set --profile coder --limit-usd 5.00 --per-run

# The existing tag costs command gains per-span breakdown
tag costs --run-id <id> --json
```

**Implementation target:** `src/tag/cost_table.py` (pricing YAML + compute function), `otel_semconv.py`

---

### #5 Inbound Webhook Trigger Server with HMAC Verification

**Cluster:** CI/CD & Agentic Dev Workflows  
**Inspired by:** Composio Webhook Triggers V2, Linear AI Agent delegation, Devin Slack integration  
**Difficulty:** 3/5 | **Impact:** 4/5

TAG has `notifications.py` (outbound), `queue_worker.py` (job model), and `cron_scheduler.py`. The missing piece is an HTTP server that validates HMAC signatures and enqueues jobs. Without inbound triggers, TAG agents can only be invoked via CLI — they cannot participate in event-driven workflows.

```bash
# Start the webhook listener
tag hooks listen --port 8080 --platform github,linear,jira,slack
tag hooks listen --port 8080 --secret $WEBHOOK_SECRET --platform github

# Register trigger rules
tag hooks register \
  --platform linear \
  --event issue.assigned \
  --profile coder \
  --action issue-solve

tag hooks register \
  --platform github \
  --event pull_request.opened \
  --profile reviewer \
  --action "tag submit --prompt 'Review this PR: {event.pull_request.html_url}'"

# Inspect
tag hooks list --json
tag hooks test --platform github --event pull_request.opened
```

**Implementation target:** `src/tag/webhook_server.py` + HMAC verifier per platform

---

### #6 Eval CI Gate with PR Comment Integration

**Cluster:** CI/CD & Agentic Dev Workflows  
**Inspired by:** Braintrust GitHub Action, LangSmith CI/CD integration  
**Difficulty:** 2/5 | **Impact:** 4/5

`cmd_ci` and `cmd_eval` already exist. `cmd_ci` has `post_pr_review_comments`. Wiring them together turns TAG evals from a dev-time tool into a merge gate.

```bash
# Fail CI if eval pass rate drops below threshold
tag eval ci \
  --suite evals/golden.yaml \
  --fail-below 0.85 \
  --post-comment \
  --repo owner/repo \
  --pr $PR_NUMBER

# Scaffold the GitHub Actions workflow
tag ci install-action --type eval
# Writes: .github/workflows/tag-eval.yml

# Capture production runs as golden eval dataset
tag eval dataset create my-golden \
  --from-runs --since 7d --limit 50

# Versioned eval datasets
tag eval dataset list --json
tag eval dataset show my-golden --json
```

**Implementation target:** `cmd_eval_ci()` in `controller.py`, eval dataset table in SQLite, `tag ci install-action`

---

### #7 Automatic Post-Run Memory Extraction (mem0 pattern)

**Cluster:** Memory & Knowledge  
**Inspired by:** mem0 automatic entity extraction, Letta/MemGPT sleep-time agents  
**Difficulty:** 3/5 | **Impact:** 4/5

The memory infrastructure (FTS5 table, `add_memory()`, inject path in `context.py`) is fully built. Automatic extraction only requires a prompt template, a post-completion LLM call, and a dedup check. This transforms memory from opt-in to ambient — the pattern that makes mem0 feel magical rather than tedious.

```bash
# Enable auto-extraction for all runs on a profile
tag memory config set auto_extract true --profile coder
tag memory config set auto_extract true --profile orchestrator

# Or per-run
tag submit --auto-memorize --prompt "Refactor the auth module"

# Manual trigger on a past run
tag memory extract --run-id <id>
tag memory extract --run-id <id> --dry-run  # preview without writing

# Enhanced search: hybrid vector + BM25
tag mem search "idempotency stripe" --top-k 5 --json
tag mem search "auth module" --mode semantic
```

**Implementation target:** Post-run hook in `controller.py` close_run(), `src/tag/memory_extractor.py`

---

### #8 A2A Agent Card Publication (`/.well-known/agent.json`)

**Cluster:** Multi-Agent Interoperability Protocols  
**Inspired by:** A2A v1.0 (Linux Foundation), MAF 1.0, CrewAI, LangGraph  
**Difficulty:** 2/5 | **Impact:** 4/5

The A2A Agent Card JSON schema is well-specified and static. `api.py` already has an HTTP server. TAG is behind all three major frameworks (LangGraph, CrewAI, MAF) on this standard — 150+ platforms support A2A discovery.

```bash
# Generate the agent card
tag agent-card generate \
  --profile coder \
  --name "tag-coder" \
  --description "Autonomous coding agent" \
  --url https://myhost.example.com \
  --capabilities code,review,test

# Serve the card at /.well-known/agent.json
tag agent-card serve --port 8080

# Enable A2A endpoint alongside existing API
tag serve --a2a

# Sign the card (RFC 8785 JCS + crypto signature)
tag agent-card sign --key ~/.tag/agent.key

# Call a remote A2A agent
tag agent-card discover --url https://remote.example.com/.well-known/agent.json
tag agent call <remote-agent-id> --task "Generate changelog" --json
```

**Implementation target:** `src/tag/a2a_card.py` + route in `api.py`

---

### #9 Structured Tool-Call Child Spans with TOOL Kind

**Cluster:** Evaluation & Observability  
**Inspired by:** Arize Phoenix OpenInference conventions, W&B Weave, Braintrust trace viewer  
**Difficulty:** 2/5 | **Impact:** 3/5

The `Span` dataclass and tracing infrastructure already exist. Adding tool child spans requires ~10 lines per tool dispatch and a `kind` field on `Span`. "Which tool timed out?" is the first debugging question users ask and currently unanswerable from traces.

```bash
# Automatic — no new CLI needed; tool spans appear in existing commands
tag trace show --run-id <id>               # flame chart now includes tool child spans
tag trace show --run-id <id> --kind tool   # filter to tool spans only
tag stats --by tool --since 7d --json      # aggregate latency/cost/error by tool name
tag otel-export status --json              # confirms TOOL span kind in schema
```

**Implementation target:** `Span.kind` field in `src/tag/tracing.py`, tool dispatch instrumentation in `controller.py`

---

### #10 Self-Consistency Ensemble: Sample N, Majority-Vote

**Cluster:** Advanced Reasoning & Planning  
**Inspired by:** Self-consistency prompting, EMS paper (2025), multi-agent debate research  
**Difficulty:** 2/5 | **Impact:** 3/5

`ThreadPoolExecutor` is already imported. `cmd_swarm` already fans out to parallel workers. The majority-vote aggregation is ~30 lines. For high-stakes tasks (security review, architecture decisions), sampling 3 and majority-voting materially improves reliability with no model change.

```bash
# Sample 3 outputs and return the majority answer
tag submit --samples 3 --vote majority --prompt "Review this security issue"

# Sample 5, stop early if consensus reached
tag submit --samples 5 --vote majority --stop-early --profile reviewer \
  --prompt "Does this code have a SQL injection vulnerability?"

# As a standalone run command
tag run --samples 3 --vote majority --profile coder "refactor the auth module"
```

**Implementation target:** `--samples` / `--vote` flags in `cmd_submit`, `src/tag/ensemble.py`

---

## Full Cluster Map

### Cluster A — Evaluation & Observability (10 features)

| Feature | Target file | Difficulty | Impact |
|---|---|---|---|
| LLM-as-judge evaluators (online + offline) | `eval_judge.py` | 2 | 5 |
| Per-span USD cost attribution with pricing table | `cost_table.py` | 1 | 4 |
| Eval CI gate with PR comment integration | `controller.py` | 2 | 4 |
| Structured tool-call child spans (TOOL kind) | `tracing.py` | 2 | 3 |
| Versioned eval dataset management | SQLite `eval_datasets` | 2 | 3 |
| Alert rules on metric thresholds (p95 latency, error rate, eval pass rate) | `alerts.py` | 3 | 3 |
| Human annotation and labeling queue | SQLite `annotation_queue` | 3 | 3 |
| Prompt versioning hub with side-by-side terminal playground | `prompts` table | 3 | 3 |
| TraceProcessor lifecycle hooks protocol (`on_trace_start/end`, `on_span_start/end`) | `tracing.py` | 2 | 2 |
| Local browser-based agent execution visualizer (MAF DevUI pattern) | `tag devui start` | 4 | 2 |

---

### Cluster B — CI/CD & Agentic Dev Workflows (10 features)

| Feature | Target file | Difficulty | Impact |
|---|---|---|---|
| Issue-to-PR autonomous loop | `issue_solver.py` | 3 | 5 |
| Inbound webhook trigger server with HMAC | `webhook_server.py` | 3 | 4 |
| Eval CI gate | `controller.py` | 2 | 4 |
| Automated test generation on PR/commit | `ci.py` | 3 | 3 |
| GitHub Actions workflow scaffold (`tag ci install-action`) | `ci.py` | 1 | 3 |
| SAST vulnerability auto-remediation from SARIF (`tag ci fix-vuln --sarif`) | `ci.py` | 3 | 3 |
| CI failure root-cause + auto-fix PR (`--auto-fix` on `tag ci diagnose`) | `ci.py` | 3 | 3 |
| Configurable PR review signal classes (security/coverage/style/correctness) | `ci.py` | 2 | 2 |
| GitLab CI/CD pipeline auto-generation | `ci.py` | 3 | 2 |
| Self-healing flaky test detection | `ci.py` | 4 | 2 |

---

### Cluster C — Memory & Knowledge (8 features)

| Feature | Target file | Difficulty | Impact |
|---|---|---|---|
| Automatic post-run memory extraction (mem0 pattern) | `memory_extractor.py` | 3 | 4 |
| Hybrid memory search: vector + BM25 + entity-boosting | `semantic_memory.py` | 3 | 3 |
| Hierarchical memory tiers: core / recall / archival with auto-paging | `semantic_memory.py` | 4 | 3 |
| Background sleep-time memory consolidation agent (`tag memory gc`) | cron job | 3 | 2 |
| Temporal fact versioning with `valid_at`/`invalid_at` edges | schema | 3 | 2 |
| Entity-relationship graph with community detection | `entity_graph.py` | 4 | 2 |
| Episodic memory: structured session episode storage | schema | 3 | 2 |
| Cross-session vector store with semantic search (LangGraph Store pattern) | `semantic_memory.py` | 3 | 3 |

---

### Cluster D — MCP Ecosystem & Tool Connectivity (8 features)

| Feature | Target file | Difficulty | Impact |
|---|---|---|---|
| Live MCP registry sync from modelcontextprotocol.io | `controller.py` | 2 | 5 |
| MCP OAuth 2.1 with PKCE + Device Authorization Flow | `mcp_auth.py` | 3 | 3 |
| Per-user/entity-scoped multi-tenant tool auth | schema + `mcp_auth.py` | 4 | 3 |
| High-value MCP server bundle (Notion, Playwright, Stripe, GitHub, Docker, Jira) | `mcp-registry.yaml` | 1 | 3 |
| Scope-based tool filtering + schema transformation (Composio model) | `tool_retrieval.py` | 3 | 2 |
| Human-in-the-loop tool approval with pause/resume + SOC-2 audit trail (Arcade AI) | `controller.py` | 4 | 2 |
| Cloud-hosted tool execution with version pinning (Toolhouse model) | `sandbox.py` | 4 | 2 |
| Enterprise IdP SSO across MCP servers | `mcp_auth.py` | 5 | 2 |

---

### Cluster E — Multi-Agent Interoperability Protocols (8 features)

| Feature | Target file | Difficulty | Impact |
|---|---|---|---|
| A2A Agent Card at `/.well-known/agent.json` | `a2a_card.py` | 2 | 4 |
| Multi-agent team primitives (RoundRobin, Selector, Swarm handoff) | `teams.py` | 3 | 3 |
| Agent-as-tool pattern: invoke specialist agents as composable function tools | `controller.py` | 3 | 3 |
| A2A v1.0 Signed Agent Cards (RFC 8785 JCS + crypto signature) | `a2a_card.py` | 3 | 2 |
| Formal HandoffMessage primitive for decentralized agent-to-agent routing | `controller.py` | 3 | 2 |
| ANP identity layer: W3C DID-based decentralized agent identity | `anp_identity.py` | 5 | 2 |
| ACP (IBM) lightweight REST adapter for intra-cluster agent messaging | `api.py` | 3 | 1 |
| Distributed agent runtime (gRPC host/worker for cross-machine agents) | `runtime.py` | 5 | 2 |

---

### Cluster F — Sandbox & Execution Environment (12 features)

| Feature | Target file | Difficulty | Impact |
|---|---|---|---|
| Real-time streaming stdout/stderr from sandbox (vs blocking) | `sandbox.py` | 2 | 4 |
| Sandbox template/snapshot system for <200ms cold start | `sandbox.py` | 3 | 3 |
| Configurable sandbox TTL + session refresh | `sandbox.py` | 2 | 3 |
| Desktop/GUI sandbox for computer-use (Ubuntu + Xfce + VNC) | `sandbox.py` | 5 | 3 |
| GPU sandbox via Modal backend (complete the modal integration stub) | `sandbox.py` | 3 | 2 |
| Per-sandbox egress firewall rules (CIDR/hostname allow/deny lists) | `sandbox.py` | 3 | 2 |
| Sandbox pause/resume with billing pause | `sandbox.py` | 3 | 2 |
| Persistent volume mounts across sandbox runs | `sandbox.py` | 3 | 2 |
| Sandbox-level secrets injection via encrypted vault | `sandbox.py` | 3 | 2 |
| Process stdin streaming and signal delivery (SIGTERM/SIGKILL/SIGINT) | `sandbox.py` | 2 | 2 |
| Per-second cost attribution per sandbox run | `sandbox.py` + `cost_table.py` | 2 | 2 |
| Auto-stop/auto-archive lifecycle policies for idle sandboxes | `sandbox.py` | 2 | 1 |

---

### Cluster G — Advanced Reasoning & Planning (8 features)

| Feature | Target file | Difficulty | Impact |
|---|---|---|---|
| Self-consistency ensemble: sample N, majority-vote | `ensemble.py` | 2 | 3 |
| Multi-agent debate pattern: two agents argue, judge decides | `debate.py` | 3 | 3 |
| Dynamic task-type classifier via embeddings (vs static YAML) | `routing.py` | 3 | 3 |
| Node-level caching with TTL for expensive LLM calls (LangGraph CachePolicy) | `tracing.py` | 3 | 3 |
| Dependency-first hierarchical task decomposition (TDAG) | `dag.py` | 4 | 3 |
| Speculative action execution for latency reduction (SPAgent pattern) | `loop_agent.py` | 4 | 2 |
| Confidence-aware model routing with cost/accuracy Pareto optimization | `routing.py` | 4 | 2 |
| MagenticOne dual-ledger orchestrator with autonomous replanning + stall detection | `orchestrator.py` | 5 | 3 |

---

### Cluster H — Agentic Workflow State & Graph (8 features)

| Feature | Target file | Difficulty | Impact |
|---|---|---|---|
| Human-in-the-loop interrupt() + Command(resume=) in agent loops | `loop_agent.py` | 3 | 4 |
| State serialization: save_state/load_state across sessions | `loop_agent.py` | 3 | 3 |
| Dynamic fan-out / map-reduce with per-item state (LangGraph Send API) | `dag.py` | 3 | 3 |
| Graph-based workflow with typed edge state and checkpointing | `graph_engine.py` | 5 | 3 |
| Time-travel debugging: roll back to prior checkpoint | `tracing.py` | 4 | 2 |
| Five team orchestration primitives (RoundRobin, Selector, Swarm, MAFOrch, GraphFlow) | `teams.py` | 4 | 3 |
| Stateful process framework with event routing and scatter-gather (SK Process) | `process_engine.py` | 5 | 3 |
| MemEx persistent scratchpad: agent-maintained working memory per session | `scratchpad.py` | 2 | 2 |

---

### Cluster I — Computer Use & Browser Automation (4 features)

| Feature | Target file | Difficulty | Impact |
|---|---|---|---|
| Playwright MCP server integration (accessibility snapshot mode — no vision needed) | `mcp-registry.yaml` | 1 | 3 |
| `tag computer-use` CLI entry point with `--profile`, `--url`, `--goal` | `controller.py` | 3 | 3 |
| Claude computer-use screenshot loop (capture → analyze → click/type → repeat) | `computer_use.py` | 4 | 3 |
| Desktop GUI sandbox with VNC stream (E2B Desktop pattern) | `sandbox.py` | 5 | 2 |

---

### Cluster J — Security & Guardrails (5 features)

| Feature | Target file | Difficulty | Impact |
|---|---|---|---|
| Output guardrail processor: scan every response chunk through `security.scan_text()` | `guardrails.py` | 2 | 3 |
| Input guardrail: validate user message before LLM sees it | `guardrails.py` | 2 | 3 |
| Runtime guardrail hooks intercepting tool outputs mid-run (tripwire pattern) | `guardrails.py` | 3 | 3 |
| `GuardrailResult` dataclass with tripwire flag in agentic loop | `controller.py` | 2 | 3 |
| Constitutional AI-style policy enforcement via configurable policy templates | `guardrails.py` | 3 | 2 |

---

## Implementation Order

Sorted by (impact × feasibility) for execution sequencing:

| Priority | Feature | Cluster | Est. lines | Reuses |
|---|---|---|---|---|
| 1 | LLM-as-judge evals | Eval | ~200 | eval tables, profile system |
| 2 | Issue-to-PR loop | CI/CD | ~400 | loop_agent, sandbox, ci.py, gh |
| 3 | MCP registry live sync | MCP | ~150 | `_load_mcp_registry()`, YAML |
| 4 | Per-span USD cost | Eval | ~100 | spans table, budget.py |
| 5 | Inbound webhook server | CI/CD | ~300 | queue_worker, notifications |
| 6 | Eval CI gate | Eval | ~200 | cmd_ci, cmd_eval, existing eval suite |
| 7 | Auto memory extraction | Memory | ~250 | semantic_memory.py, controller hooks |
| 8 | A2A Agent Card | Protocol | ~150 | api.py HTTP server |
| 9 | Tool-call child spans | Eval | ~100 | Span dataclass, tracing.py |
| 10 | Self-consistency ensemble | Reasoning | ~150 | ThreadPoolExecutor, swarm |
| 11 | Output guardrails | Security | ~200 | security.py |
| 12 | Streaming sandbox stdout | Sandbox | ~150 | sandbox.py |
| 13 | `tag computer-use` | Browser | ~300 | mcp auth, screenshot loop |
| 14 | Playwright MCP bundle | MCP | ~20 | mcp-registry.yaml only |
| 15 | Human-in-the-loop interrupt | Graph | ~300 | loop_agent.py |

---

## Notes on Existing Infrastructure

TAG's codebase has more built than appears from the feature list. Before implementing any item above, check these existing modules:

- **`semantic_memory.py`** — FTS5 Porter-stem search with confidence decay; add vector embed column for hybrid search
- **`tool_retrieval.py`** — SentenceTransformer already imported; reuse for memory embeddings
- **`tracing.py`** + **`otel_semconv.py`** — Span dataclass and OTel export; extend with `kind` field
- **`ci.py`** — `post_pr_review_comments` already wired; eval gate just needs to call it
- **`security.py`** — `scan_text()` ready for guardrail wrapping; no new scanner needed
- **`loop_agent.py`** — autonomous loop runtime; issue-solve just needs a `cmd_issue_solve` wrapper
- **`dag.py`** — `list_jobs_raw()` and DAG scheduler; fan-out Send API pattern extends this
- **`api.py`** — HTTP server base for A2A card endpoint; one new route
- **`sandbox.py`** — BACKENDS set with docker/modal stubs; complete the streaming stdout first
- **`budget.py`** — total spend tracked; per-span USD just needs pricing table + attribution call

