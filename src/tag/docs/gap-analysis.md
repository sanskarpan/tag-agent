# Hermes Gap Analysis

This file records what Hermes already covers and what still needs to be built
to match the target architecture.

## Verified Hermes capabilities

Validated against upstream `0.15.1` docs and source:

- Profiles:
  - separate `config.yaml`, `.env`, `SOUL.md`, memory, sessions, cron, skills
  - command aliases and profile descriptions
- Provider runtime:
  - provider plugin registry
  - OpenRouter, OpenAI Codex, DeepSeek, Xiaomi, MiniMax, custom endpoints
  - separate API modes: `chat_completions`, `codex_responses`,
    `anthropic_messages`
- Delegation:
  - isolated child agents
  - configurable concurrency
  - optional nested orchestration with spawn depth limit
  - provider/model override for delegated children
- Kanban:
  - durable SQLite-backed board
  - cross-profile task routing
  - dispatcher hosted in gateway
  - worker and orchestrator skills
  - resumable task lifecycle and comments
- API server:
  - OpenAI-compatible `/v1/chat/completions`
  - OpenAI-compatible `/v1/responses`
  - runs API and health endpoints
- Codex:
  - `openai-codex` provider
  - optional Codex app-server runtime
  - Codex-aware plugin and MCP integration

## What Hermes does not fully solve by itself

Hermes is an agent runtime. The target system also needs a control plane.

Missing or incomplete pieces:

- Dynamic master selection per run
  - Hermes is profile-centric
  - profile config is strong, but not a full run scheduler
- Automatic worker model selection by task type
  - Hermes can override delegation model/provider
  - it does not ship a full policy engine for cost/latency/risk-based routing
- Central benchmark and eval loop
  - no built-in per-task scoring layer that learns "research should use model X"
- Cross-run cost policy
  - no native budget engine for "escalate only if confidence drops"
- Unified run planner across:
  - direct chat
  - delegation
  - Kanban
  - API server runs
- Stable manual override surface
  - e.g. "master = codex, research worker = OpenRouter model A, review worker = model B"
- Fleet observability for external orchestration
  - traces, route decisions, policy outcomes, benchmark history

Recently closed in this lab:

- role-level model switching via control-plane commands
- per-run master/worker/verifier model overrides
- persistent run history for submits and benchmarks
- a Codex bridge entrypoint so external Codex sessions can offload to Hermes
- full OpenRouter catalog querying in addition to Hermes' curated picker view
- benchmark normalization for runtime noise like session IDs, optional warning
  lines, and fenced JSON
- a clone-safe custom TUI skin plus profile-aware status/banner chrome
- a TUI skin bridge fix so Hermes' status-bar palette and branding icon apply
  correctly in the Ink frontend, not just the classic CLI

## Comparison audit

### Hermes

What Hermes already does well for this project:

- live session model switching through `/model`
- profile-based separation for orchestrator and worker roles
- curated provider/model inventory, including OpenRouter and Codex-aware paths
- synchronous fan-out with `delegate_task`
- durable multi-profile workflows with Kanban

Where Hermes still needs a control plane on top:

- switching several role defaults from one surface
- per-run overrides for master and worker lanes
- benchmark-driven automatic model promotion/demotion
- route policy that chooses "which worker model for which task"

### OpenCode

Relevant patterns worth copying:

- model selection is a first-class UX surface, not a hidden config edit
- primary agents and subagents can each override their own model
- if a subagent has no model override, it inherits the invoking primary agent's
  model
- providers and models are discovered from a live catalog instead of a hardcoded
  local list

Implication for this lab:

- role-specific default models should be easy to inspect and mutate
- worker inheritance should be explicit when no override is set
- live provider catalogs should back the switch surface

### OpenHands

Relevant patterns worth copying:

- LLM profiles are surfaced as a user-facing abstraction
- active profile switching is a direct UI action
- routing exists, but the current open-source routing surface is still narrow
  and mostly configuration-led

Limits relative to the target architecture:

- OpenHands does not natively give us the same orchestrator/worker role graph
  we want here
- the experimental routing config is far less expressive than the desired
  master/worker/verifier policy engine

Implication for this lab:

- profile switching UX is a good pattern to copy
- Hermes remains the stronger runtime substrate for multi-profile local
  orchestration

## Important Hermes tradeoffs

### `delegate_task`

Strengths:

- low-latency parallel fan-out
- clean isolated child context
- provider/model override already exists

Limits:

- synchronous only
- canceled when parent turn is interrupted
- not durable

Conclusion:

- good for short tactical subtasks
- not enough for long-lived orchestration

### Kanban

Strengths:

- durable
- cross-profile
- resumable
- auditable
- good fit for orchestrator -> specialists -> verifier pipelines

Limits:

- task routing still depends on profile design and orchestrator behavior
- no built-in automatic model benchmark/policy layer

Conclusion:

- best native Hermes foundation for your desired system

### Codex app-server runtime

Strengths:

- native Codex shell/file runtime
- can reuse Codex plugin ecosystem
- useful when you want Codex to be the execution engine

Limits documented by Hermes:

- `delegate_task` unavailable
- `memory` unavailable
- `session_search` unavailable
- `todo` unavailable

Local integration quirk validated in this lab:

- `codex app-server` worked only when the profile preserved the user's real
  `HOME`, even with `CODEX_HOME` set explicitly.
- Keeping `HERMES_HOME` isolated but preserving `HOME` for Codex-runtime
  profiles fixed the issue locally.

Conclusion:

- useful for specific profiles
- not a universal runtime for the whole architecture

## Recommended architecture

Base:

- Hermes profiles
- Hermes Kanban
- Hermes API server

Added control-plane layer:

- isolated local launcher
- profile bootstrapper
- routing policy file
- route selection CLI/API
- benchmark storage
- later: scoring and auto-promotion/demotion of models

## Proposed upgrade phases

### Phase 1

- local Hermes lab
- isolated profiles
- orchestrator + worker profile config
- deterministic route policy

### Phase 2

- benchmark harness for task classes
- route history and simple scoring
- manual override flags per run
- role-level model switch commands backed by live provider inventory
- Codex bridge command for external sessions

### Phase 3

- policy-aware submission API
- Kanban card creation from route engine
- verifier escalation rules
- cost ceilings and fallback chains

### Phase 4

- adaptive routing from benchmark history
- optional Hermes API-server based external UI
- richer observability and traces
