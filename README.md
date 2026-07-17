<p align="center">
  <img src="https://raw.githubusercontent.com/sanskarpan/tag-agent/main/docs/logo.svg" alt="TAG" width="120" />
</p>

<h1 align="center">TAG — Terminal Agent Gateway</h1>

<p align="center">
  <strong>Orchestrate AI agents from your terminal.<br/>Any model. Any provider. One CLI.</strong>
</p>

<p align="center">
  <a href="https://pypi.org/project/tag-agent/"><img src="https://img.shields.io/pypi/v/tag-agent?style=flat-square&label=PyPI&color=3776AB" alt="PyPI" /></a>
  <a href="https://www.npmjs.com/package/tag-agent"><img src="https://img.shields.io/npm/v/tag-agent?style=flat-square&label=npm&color=CB3837" alt="npm" /></a>
  <a href="https://pypi.org/project/tag-agent/"><img src="https://img.shields.io/pypi/dm/tag-agent?style=flat-square&label=installs&color=brightgreen" alt="Downloads" /></a>
  <a href="https://pypi.org/project/tag-agent/"><img src="https://img.shields.io/pypi/pyversions/tag-agent?style=flat-square" alt="Python 3.11+" /></a>
  <a href="https://github.com/sanskarpan/tag-agent/actions"><img src="https://img.shields.io/github/actions/workflow/status/sanskarpan/tag-agent/ci.yml?branch=main&label=CI&style=flat-square" alt="CI" /></a>
  <a href="https://github.com/sanskarpan/tag-agent/blob/main/LICENSE"><img src="https://img.shields.io/github/license/sanskarpan/tag-agent?style=flat-square&color=blue" alt="MIT" /></a>
</p>

<p align="center">
  <a href="#install">Install</a> •
  <a href="#quick-start">Quick Start</a> •
  <a href="#features">Features</a> •
  <a href="#command-reference">Commands</a> •
  <a href="#profiles">Profiles</a> •
  <a href="#architecture">Architecture</a> •
  <a href="#native-go-harness-tag-go">Go Harness</a> •
  <a href="#how-tag-compares">Comparison</a> •
  <a href="https://github.com/sanskarpan/tag-agent/tree/main/docs/prd">PRDs</a>
</p>

---

TAG is a production-grade AI agent orchestration CLI. It routes tasks across 10+ AI providers, manages autonomous loops and cron schedules, tracks spend with hard budget limits, scans for credential leaks, and ships with 44 built-in capabilities — from background job queues to architect/editor agent splits — all backed by a crash-safe WAL-mode SQLite store.

---

## Install

**Python (recommended):**

```bash
pip install tag-agent
```

**pipx (isolated environment):**

```bash
pipx install tag-agent
```

**npm / pnpm (global):**

```bash
npm install -g tag-agent
# pnpm add -g tag-agent
```

> The npm package is a thin Node launcher. On first run it provisions an isolated Python runtime under `~/.tag/npm-runtime/<version>`. Python **3.11+** must be on your `PATH`.

**From source:**

```bash
git clone https://github.com/sanskarpan/tag-agent
cd tag-agent
pip install -e .
```

Requires **Python 3.11 – 3.14**.

---

## Quick Start

```bash
# 1. Provision runtime and import credentials
tag setup

# 2. Import keys from tools you already use
tag import-claude      # Anthropic
tag import-gemini      # Google
tag import-opencode    # OpenCode / OpenRouter

# 3. Submit your first task
tag submit --prompt "Summarise this repository in 3 bullet points"

# 4. Watch the live dashboard
tag dashboard

# 5. Check health
tag doctor --json
```

---

## Features

### Multi-Provider Routing

Route each profile to a different provider. Switch models per-task without touching config files.

| Provider | Import command | Notes |
|---|---|---|
| **Anthropic (Claude)** | `tag import-claude` | Claude 4 Opus/Sonnet/Haiku |
| **OpenAI / Codex** | `tag import-codex` | GPT-5, o3, Codex |
| **Google Gemini** | `tag import-gemini` | Gemini 2.0 Flash / Pro |
| **OpenRouter** | `tag import-opencode` | 300+ models via one key |
| **Mistral** | `tag import-mistral` | Mistral Large / Codestral |
| **AWS Bedrock** | `tag import-aws` | Claude via Bedrock |
| **Cursor** | `tag import-cursor` | BYOK keys from local SQLite |
| **GitHub Copilot** | `tag import-copilot` | `gh` CLI token |
| **Aider** | `tag import-aider` | `.aider.conf.yml` keys |
| **Continue.dev** | `tag import-continue` | All configured providers |
| **Zed** | `tag import-zed` | Language model settings |
| **SSH remote** | `tag import-ssh` | Run agents on remote hosts |
| **Docker** | `tag import-docker` | Containerised agent execution |
| **Modal** | `tag import-modal` | Serverless agent runs |
| **Daytona** | `tag import-daytona` | Workspace-based execution |

Every import writes only to the target profile's local `.env` — no keys leave the machine.

---

### Autonomous Agent Modes

#### Loop — continuous autonomous runs

```bash
# Start a self-iterating agent loop
tag loop start --goal "Monitor the test suite and fix every failing test" --max-iters 20

# Watch progress
tag loop list --json

# Stop when done
tag loop stop <loop-id>
```

The loop worker runs detached from your terminal, commits fixes, re-runs tests, and iterates until the goal is reached or the iteration cap is hit. Approval modes: `auto` (no intervention) or `human` (pause before each iteration).

#### Cron — scheduled agents

```bash
# Run a daily security scan
tag cron add --name nightly-scan --schedule "0 2 * * *" "tag security scan src/"

# Vixie aliases work too
tag cron add --name hourly-health --schedule "@hourly" "tag doctor"

# List, enable, disable
tag cron list
tag cron disable nightly-scan
```

Cron expressions are validated for both format and field ranges (minute 0–59, hour 0–23, etc.). Vixie aliases (`@reboot`, `@daily`, `@weekly`, `@monthly`, `@hourly`) are accepted.

#### Swarm — fan-out to parallel workers

```bash
# Spread one goal across 4 parallel agents
tag swarm --goal "Review all open PRs and leave a comment on each" --workers 4

# Target a named kanban board
tag swarm --board code-review --goal "Analyse security posture" --workers 6
```

Workers use SHA-256 idempotency keys — safe to retry without creating duplicates.

---

### Background Queue

```bash
# Enqueue jobs with priority (1 = lowest, 10 = highest)
tag queue add --prompt "Generate API docs" --priority 8
tag queue add --prompt "Run regression tests" --priority 5

# With dependency ordering
tag queue-dep add "Generate changelog" --depends-on <upstream-job-id>
tag queue-dep promote   # unlock jobs whose deps are done

# Monitor
tag queue list
tag queue status --job-id <id>
tag queue cancel --job-id <id>
```

The queue worker runs detached, survives terminal close, and respects priority ordering across restarts.

---

### Budget Enforcement

```bash
# Set a hard token cap per profile
tag budget set --profile orchestrator --limit 500000 --period daily

# Check current usage
tag budget check --profile orchestrator --json

# List all budgets
tag budget list --json
```

Agents that exceed their budget receive a `BudgetExceeded` error and halt cleanly. The `--json` flag returns `{"profile": "...", "used": 12345, "limit": 500000, "pct": 2.5, "period": "daily"}`.

---

### Architect / Editor Agent Split

Delegate planning to a high-capability model and execution to a faster, cheaper one:

```bash
# Architect designs the changes; editor implements file by file
tag split plan "Refactor the authentication module to use JWT" \
  --architect claude-opus-4 \
  --editor claude-haiku-4-5

# Monitor progress
tag split list
tag split show <run-id> --json

# Supply a pre-built spec (skip the architect call)
tag split plan "Apply these changes" --spec-json '{"items": [...]}'
```

Each accepted change item is committed separately. Rejected items are logged with the editor's reasoning.

---

### Model Fallback Chains

```bash
# If claude-opus-4 fails (context overflow, error, timeout), fall back to sonnet
tag route-fallback add \
  --primary claude-opus-4 \
  --fallback claude-sonnet-4-6 \
  --condition context_overflow \
  --priority 1

# Show chains for the current profile
tag route-fallback list --json

# Test resolution for a given primary + condition
tag route-fallback resolve --primary claude-opus-4 --condition error

# Walk the chain live during inference (native Go harness): on a retryable
# provider error before any content streams, fail over to the next step.
tag run "Summarise this repo" --provider openai --fallback
```

Supported conditions: `context_overflow`, `error`, `timeout`, `cost_limit`, `any`.

`tag run --fallback` (native Go harness) executes the stored chain at runtime: it wraps the primary provider so a retryable error (429 / 401 / 400 / timeout / overload) that occurs *before* any content streams advances to the next declared step, honoring each edge's condition (a `rate_limit`-gated hop won't rescue an auth error) and streaming the winning provider live. It only fails over on transient/auth/model errors — deterministic malformed-request errors are not retried — and the chain is walked transitively (primary → A → B). The flag is opt-in; without it a single-provider run is unchanged.

---

### Secret Scanning

```bash
# Scan the whole repo
tag security scan

# Scan a single file
tag security scan src/tag/controller.py

# Machine-readable output
tag security scan --json

# View past scan history
tag security list --json
```

Detects API keys, tokens, passwords, and high-entropy strings across 40+ patterns. Findings include file path, line number, and pattern name — matched values are never displayed.

---

### Diff-Aware Context Injection

```bash
# Inject current branch diff into next agent run
tag diff-context

# Only staged changes
tag diff-context --staged

# From a GitHub PR
tag diff-context --pr 1234 --repo owner/repo

# Output only (for piping)
tag diff-context --output-only --json
```

The diff context is written to `~/.tag/runtime/context/diff_context.md` and picked up automatically by `tag submit`.

---

### OTel / Observability

```bash
# Export spans to a local collector
tag otel-export enable --endpoint http://localhost:4317

# Export to Honeycomb, Datadog, etc.
tag otel-export configure --endpoint https://api.honeycomb.io \
  --header "x-honeycomb-team=<key>"

# Check status
tag otel-export status --json
```

Every agent run emits GenAI semantic conventions spans: model name, token counts, cost, latency per node, tool calls, and error events.

---

### AgentOps Session Observability

```bash
# Connect to AgentOps
tag agentops configure --api-key <key>

# View recent sessions
tag agentops sessions --json

# Deep-dive a session
tag agentops show <session-id> --json
```

---

### Cache Analytics

```bash
# Show prompt cache hit/miss rates per run
tag cache stats --json

# Clear stale cache entries
tag cache clear --older-than 7d
```

---

### Eval Framework

```bash
# Create an eval suite
tag eval create --name "qa-regression" --profile researcher

# Run a suite
tag eval run <eval-id>

# List results
tag eval list --json
```

Use evals to regression-test prompts and catch model degradation in CI.

---

### Notification Hooks

```bash
# Notify on Slack when any run completes
tag notify add --channel slack \
  --event run.completed \
  --config-json '{"webhook_url": "https://hooks.slack.com/..."}'

# Desktop notification
tag notify add --channel desktop --event run.failed

# Test a hook
tag notify test <hook-id>

# List hooks
tag notify list --json
```

Supported channels: `slack`, `email`, `desktop`, `webhook`. Events: `run.completed`, `run.failed`, `budget.exceeded`.

---

### Persona Management

```bash
# Create a named persona with a system prompt
tag persona add --name "strict-reviewer" \
  --system "You are a senior security engineer. Be concise and critical."

# Activate for a submit
tag submit --persona strict-reviewer --prompt "Review this PR"

# List personas
tag persona list --json
```

---

### DAG / Dependency-Aware Queue

```bash
# Add jobs with explicit dependencies
A=$(tag queue-dep add "Fetch upstream data" --json | jq -r .job_id)
B=$(tag queue-dep add "Process data" --depends-on $A --json | jq -r .job_id)
tag queue-dep add "Generate report" --depends-on $B

# Promote jobs whose dependencies are complete
tag queue-dep promote --json

# Inspect the DAG
tag queue-dep list --json
tag dag show --json
```

---

### Vector Tool Retrieval

```bash
# Index all tools in the current profile
tag tool-index index

# Semantic search for the right tool
tag tool-index search "send a Slack message"

# Status
tag tool-index status --json
```

High-cardinality MCP server catalogs are indexed and searched semantically so only the relevant subset enters the context window.

---

### Workspace Context

```bash
# Index the repo for fast context injection
tag workspace index --max-files 5000

# Show what's indexed
tag workspace status --json

# Clear stale entries
tag workspace clear
```

---

### Profile Marketplace

```bash
# Browse published profiles
tag marketplace list
tag marketplace search "security"

# Download and install
tag marketplace install <profile-name>

# Publish your own
tag marketplace publish --profile coder
```

---

### Sandbox / Code Execution

```bash
# Run agent-generated code in an isolated sandbox
tag sandbox run --code "print('hello')" --language python

# List past sandbox runs
tag sandbox list --json
```

---

## Command Reference

### Orchestration & Setup

| Command | Description |
|---|---|
| `tag setup` | Full first-run: runtime, profiles, credentials |
| `tag doctor` | Health check; `--json` for machine-readable |
| `tag bootstrap` | Re-provision profiles without full setup |
| `tag update` | Update the managed Hermes runtime |
| `tag status` | Current profile and model status |
| `tag dashboard` | Rich live view — active runs, queue depth, health |
| `tag tui` | Full orchestrator TUI |
| `tag serve` | Start the TAG HTTP API server |
| `tag web` | Launch web dashboard |

### Task Submission

| Command | Description |
|---|---|
| `tag submit` | Submit a task (direct or kanban) |
| `tag benchmark` | Run benchmark suite; `--model-ref` to compare |
| `tag runs` | Show benchmark history |
| `tag prompt-size` | Measure prompt token count |
| `tag diff-context` | Inject git diff into next run |

### Autonomous Agents

| Command | Description |
|---|---|
| `tag loop start` | Start an autonomous iteration loop |
| `tag loop list` | Show running loops |
| `tag loop stop` | Stop a loop |
| `tag cron add` | Add a scheduled agent job |
| `tag cron list` | List cron jobs |
| `tag cron enable/disable` | Toggle a cron job |
| `tag cron run` | Trigger a cron job immediately |
| `tag cron daemon` | Start the cron scheduler daemon |
| `tag swarm` | Fan-out to N parallel workers |

### Queue & Scheduling

| Command | Description |
|---|---|
| `tag queue add` | Enqueue a job (priority 1–10) |
| `tag queue list` | Show pending/active jobs |
| `tag queue cancel` | Cancel a job |
| `tag queue status` | Job status and result |
| `tag queue-dep add` | Add job with `--depends-on` |
| `tag queue-dep promote` | Unlock dependency-satisfied jobs |
| `tag queue-dep list` | Inspect DAG job graph |
| `tag dag show` | Visualise the dependency graph |

### Models & Profiles

| Command | Description |
|---|---|
| `tag models` | List available models |
| `tag set-model` | Set active model for a profile |
| `tag assignments` | Show all profile→model mappings |
| `tag openrouter-models` | Search OpenRouter catalog |
| `tag route-fallback add` | Add a model fallback rule |
| `tag route-fallback list` | List fallback chains |
| `tag route-fallback resolve` | Test fallback resolution |

### Credential Import

| Command | Description |
|---|---|
| `tag import-claude` | Anthropic API key |
| `tag import-gemini` | Google Gemini key |
| `tag import-codex` | OpenAI Codex CLI |
| `tag import-opencode` | OpenCode / OpenRouter |
| `tag import-continue` | Continue.dev all providers |
| `tag import-mistral` | Mistral / Vibe CLI |
| `tag import-zed` | Zed editor models |
| `tag import-copilot` | GitHub Copilot token |
| `tag import-aider` | Aider config keys |
| `tag import-aws` | AWS Bedrock credentials |
| `tag import-cursor` | Cursor BYOK keys |
| `tag import-ssh` | Remote execution via SSH |
| `tag import-docker` | Containerised execution |
| `tag import-modal` | Modal serverless |
| `tag import-daytona` | Daytona workspaces |
| `tag import-nous-portal` | Nous Portal API |
| `tag import-supermemory` | Supermemory integration |
| `tag import-honcho` | Honcho sessions |

### Observability

| Command | Description |
|---|---|
| `tag otel-export` | Configure OTel span export |
| `tag agentops` | AgentOps session management |
| `tag trace` | View run traces |
| `tag costs` | Spend analytics |
| `tag cache stats` | Prompt cache hit rates |
| `tag logs` | Stream agent logs |

### Budget & Safety

| Command | Description |
|---|---|
| `tag budget set` | Set token/cost limit |
| `tag budget check` | Check usage vs limit |
| `tag budget list` | All configured budgets |
| `tag security scan` | Detect secrets in files |
| `tag security list` | Past scan results |

### Context & Memory

| Command | Description |
|---|---|
| `tag workspace index` | Index repo for context |
| `tag workspace status` | Indexing status |
| `tag mem list` | List memory entries |
| `tag memory-journal add` | Append to journal |
| `tag memory-journal search` | Full-text search |
| `tag memory-journal list` | Recent entries |

### Integrations & Extensions

| Command | Description |
|---|---|
| `tag mcp` | Pass-through to MCP server |
| `tag mcp-registry` | Browse/install MCP servers |
| `tag plugin install` | Install a TAG plugin |
| `tag plugin list` | List installed plugins |
| `tag tools` | List available tools |
| `tag tool-index` | Semantic tool search |
| `tag hooks` | Manage lifecycle hooks |
| `tag template` | Profile templates |

### Agent Split & Eval

| Command | Description |
|---|---|
| `tag split plan` | Architect→Editor agent split |
| `tag split list` | Ongoing split runs |
| `tag split show` | Inspect a split run |
| `tag eval create` | Create eval suite |
| `tag eval run` | Run evaluations |
| `tag eval list` | Eval history |

### Notifications & Personas

| Command | Description |
|---|---|
| `tag notify add` | Add notification hook |
| `tag notify list` | List hooks |
| `tag notify test` | Send test notification |
| `tag persona add` | Create a named persona |
| `tag persona list` | List personas |

### Pass-through (run inside a profile's managed environment)

```bash
tag chat --profile orchestrator -- --help
tag gateway --profile orchestrator -- start
tag kanban --profile orchestrator -- list
tag sessions --profile orchestrator -- list
tag skills --profile orchestrator -- list
tag memory --profile orchestrator -- status
tag model --profile orchestrator -- list
tag profile -- list

# Full escape hatch
tag hermes --profile orchestrator -- gateway start
```

---

## Profiles

TAG ships five built-in profiles, each with independent model, credential, and routing configuration:

| Profile | Role | Default model |
|---|---|---|
| `orchestrator` | Master — delegates tasks, routes results | `openai-codex/gpt-5.4` |
| `researcher` | Worker — web research and summarisation | `openrouter/deepseek/deepseek-v4-flash` |
| `coder` | Worker — implementation and refactoring | `openrouter/qwen/qwen3-coder` |
| `reviewer` | Worker + verifier — code review and QA | `openrouter/deepseek/deepseek-v4-pro` |
| `codex-runtime-master` | Alternate master for Codex app-server flows | Codex runtime |

Override the model for any profile:

```bash
tag set-model --profile coder --ref openrouter/anthropic/claude-sonnet-4-6
tag set-model --profile orchestrator --ref claude-opus-4
```

---

## Task Routing

| Task type | Workers | Verifier | Execution mode |
|---|---|---|---|
| `research` | researcher | reviewer | Kanban |
| `implementation` | coder | reviewer | Kanban |
| `review` | reviewer | reviewer | Direct |
| `mixed` | researcher + coder | reviewer | Kanban |

---

## Architecture

```
tag CLI
├── Management plane  (SQLite WAL — no API key needed)
│   ├── loop.py        autonomous iteration loop
│   ├── cron.py        scheduled cron agent jobs
│   ├── kanban.py      native task management
│   ├── queue_worker   priority background jobs
│   ├── dag.py         dependency-aware DAG queue
│   ├── budget.py      token/cost enforcement
│   ├── security.py    secret scanning
│   ├── diff_context   git diff injection
│   ├── split_agent    architect/editor split
│   ├── otel_semconv   OpenTelemetry spans
│   ├── agentops       session observability
│   ├── notifications  webhook/Slack/email hooks
│   └── dashboard      Rich live TUI
└── Execution plane   (Hermes gateway — API key required)
    ├── swarm          fan-out topology
    ├── submit         direct / kanban dispatch
    └── tui            full terminal UI
```

State lives entirely in `~/.tag/runtime/tag.sqlite3` (WAL mode, 40+ tables). The split means queue, loop, cron, dashboard, doctor, split, eval, budget, security, and all import commands work **offline and without an active API key**.

```bash
export TAG_HOME=/custom/path   # override root
```

```
~/.tag/
  config/tag.yaml
  config/benchmark-suite.yaml
  managed/hermes-agent-upstream/
  runtime/
    tag.sqlite3          # WAL-mode state store
    context/
      diff_context.md    # last injected diff
    home/                # Hermes runtime home
```

---

## Native Go Harness (`tag-go/`)

A from-scratch **native Go port** of TAG lives in [`tag-go/`](tag-go/): a single static binary (`CGO_ENABLED=0`, ~18 MB) that owns its own runtime — no Python, no managed Hermes checkout, no interpreter startup.

```bash
cd tag-go
CGO_ENABLED=0 go build -o tag ./cmd/tag   # Go 1.25+
./tag --help
go test ./...                             # fully offline; no API keys needed
```

- **88 top-level commands across 29 packages** — the full control plane ported, plus a native runtime: a provider-neutral LLM interface with raw-HTTP SSE streaming for **Anthropic and OpenAI**, a tool-calling agent loop, sandboxed built-in tools, an MCP client + server, HTTP `serve`/`devui`/`web` dashboards, an OpenAI-compatible chat gateway (`tag gateway`), an LSP server, and a terminal TUI.
- **Offline by default:** execution paths run against a built-in `echo` provider so everything is testable without keys; pass `--provider anthropic|openai` to go live.
- **Executes its own queue:** a native execution worker drives queued jobs and DAG dependency chains through the agent loop (`tag queue worker`, `tag dag run --execute`, `tag cron run --execute`).
- **Serves an OpenAI-compatible API:** `tag gateway` fronts the agent loop with `POST /v1/chat/completions` (streaming SSE + non-stream), `GET /v1/models`, and `GET /health` behind optional bearer-token auth — point any OpenAI client at it. A request model may carry a `provider/` prefix (else `--provider` picks the default), and `--fallback` walks the profile's `route_fallbacks` chain at inference time. Loopback-only by default; a public bind requires `--key`/`TAG_GATEWAY_KEY` (or an explicit `--allow-unauthenticated`).
- **~10× faster startup, ~30× faster cold bootstrap, 18 MB binary vs 170 MB venv** — see [`COMPARISON_REPORT.md`](COMPARISON_REPORT.md) for the full benchmark and behavioral comparison against the Python edition.
- The managed-Hermes passthrough commands (`chat`, `kanban`, `sessions`, …) and desktop packaging are deliberately not ported; `serve`/`devui`/`web` replace the dashboard. (The native `tag gateway` above is a from-scratch OpenAI-compatible server, not the Hermes-passthrough `gateway`.)

Per-subsystem status and audit history: [`tag-go/MIGRATION_STATUS.md`](tag-go/MIGRATION_STATUS.md).

---

## How TAG Compares

| Feature | TAG | Claude Code | Aider | AutoGen | CrewAI |
|---|---|---|---|---|---|
| **CLI-first** | ✓ | ✓ | ✓ | — | — |
| **Any provider** | ✓ (15+ imports) | Anthropic only | ✓ | ✓ | ✓ |
| **Background queue** | ✓ priority + DAG | — | — | — | — |
| **Cron scheduling** | ✓ | — | — | — | — |
| **Autonomous loop** | ✓ | via subagents | — | ✓ | ✓ |
| **Budget enforcement** | ✓ hard limit | spend tracking | — | — | — |
| **Secret scanning** | ✓ built-in | — | — | — | — |
| **OTel tracing** | ✓ | — | — | — | — |
| **Architect/editor split** | ✓ | — | ✓ | — | — |
| **Model fallback chains** | ✓ | — | — | — | — |
| **Diff-aware context** | ✓ | — | ✓ (repo-map) | — | — |
| **Notification hooks** | ✓ Slack/email/desktop | — | — | — | — |
| **Eval framework** | ✓ | — | — | — | — |
| **Persona management** | ✓ | CLAUDE.md | — | — | ✓ roles |
| **Profile marketplace** | ✓ | — | — | — | — |
| **WAL-mode persistence** | ✓ | — | — | — | — |
| **MCP registry** | ✓ | ✓ | — | — | — |
| **AgentOps integration** | ✓ | — | — | — | — |
| **Vector tool retrieval** | ✓ | deferred loading | — | — | — |
| **No API key for mgmt** | ✓ | — | — | — | — |

---

## Configuration

All configuration lives in `~/.tag/config/tag.yaml`. The most common overrides:

```yaml
defaults:
  master_profile: orchestrator

profiles:
  coder:
    model:
      provider: openrouter
      ref: qwen/qwen3-coder

security:
  secret_scan_on_submit: true

queue:
  max_workers: 4
  default_priority: 5
```

---

## Requirements

- Python **3.11 – 3.14**
- `npm` — required for the full TUI build (not needed for submit / queue / loop / dashboard)
- `git` — recommended; required for `tag diff-context` and `tag update`

---

## Contributing

```bash
git clone https://github.com/sanskarpan/tag-agent
cd tag-agent
pip install -e ".[dev]"
pytest tests/ -x -q --ignore=tests/hermes_cli
```

For the native Go harness:

```bash
cd tag-go
gofmt -l . && go vet ./... && go test ./... -race
```

Design decisions and feature specs live in [`docs/prd/`](docs/prd/) — 44 PRDs covering every subsystem. Open an issue before implementing a significant feature.

---

## Changelog

See [GitHub Releases](https://github.com/sanskarpan/tag-agent/releases) for the full history.

**v0.6.4** — Adversarial QA complete: 13 bugs fixed across DB resilience, cron validation, `--json` coverage, budget check, queue-dep list, and more.

**v0.3.0** — Added imports for opencode, Zed, Copilot, Aider, AWS, and Cursor.

---

## License

MIT — see [LICENSE](https://github.com/sanskarpan/tag-agent/blob/main/LICENSE).

---

<p align="center">
  Made with care. Built for the terminal.
</p>

