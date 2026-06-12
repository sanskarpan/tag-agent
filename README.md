<p align="center">
  <img src="https://raw.githubusercontent.com/sanskarpan/tag-agent/main/docs/logo.svg" alt="TAG" width="140" />
</p>

<h1 align="center">TAG</h1>

<p align="center">
  <strong>Orchestrate AI agents from your terminal.</strong>
</p>

<p align="center">
  Multi-provider routing &bull; Native kanban layer &bull; Background job queue &bull; Live dashboard &bull; Swarm topology
</p>

<p align="center">
  <a href="https://github.com/sanskarpan/tag-agent/actions">
    <img src="https://img.shields.io/github/actions/workflow/status/sanskarpan/tag-agent/ci.yml?branch=main&label=CI&style=flat-square" alt="CI" />
  </a>
  <a href="https://pypi.org/project/tag-agent/">
    <img src="https://img.shields.io/pypi/v/tag-agent?style=flat-square&label=PyPI&color=3776AB" alt="PyPI version" />
  </a>
  <a href="https://www.npmjs.com/package/tag-agent">
    <img src="https://img.shields.io/npm/v/tag-agent?style=flat-square&label=npm&color=CB3837" alt="npm version" />
  </a>
  <a href="https://pypi.org/project/tag-agent/">
    <img src="https://img.shields.io/pypi/pyversions/tag-agent?style=flat-square" alt="Python 3.11+" />
  </a>
  <a href="https://github.com/sanskarpan/tag-agent/blob/main/LICENSE">
    <img src="https://img.shields.io/github/license/sanskarpan/tag-agent?style=flat-square&label=license&color=blue" alt="MIT License" />
  </a>
</p>

---

## What's new in v0.4.0

- **Native kanban layer** — pure SQLite management plane; create and monitor tasks without an API key
- **Swarm topology** — fan out one goal to multiple workers with idempotent deduplication (`tag swarm`)
- **Background job queue** — detached queue with priority scheduling; survives terminal close (`tag queue`)
- **Memory journal** — persistent agent reflection log with search and expiry (`tag memory-journal`)
- **Enhanced `tag doctor`** — JSON output mode, accurate patch-state detection, non-zero exit on failures
- **Enhanced `tag dashboard`** — native live view powered by Rich; no external dependencies

## Features

- **Multi-provider routing** — workers run on OpenRouter, Codex, Claude, Gemini, Mistral, Groq, DeepSeek, or any OpenAI-compatible endpoint; model and provider switch per profile
- **Profile-based orchestration** — four built-in roles (orchestrator, researcher, coder, reviewer) each with independent model, credential, and routing config
- **Zero-dependency bootstrap** — bundles Hermes v0.16.0; provisions a managed runtime on first run, no manual steps required
- **Broad credential import** — one command to pull keys from 10+ local AI tools: Claude Code, Gemini CLI, Codex, Continue.dev, Mistral Vibe, opencode, Zed, Cursor, GitHub Copilot, Aider, AWS Bedrock
- **Native kanban layer** — SQLite-backed task management plane; no hermes binary required to create or monitor tasks
- **Swarm topology** — fan out a single goal to a configurable worker pool; SHA-256 idempotency key prevents duplicate task creation on retries
- **Background queue** — priority-ordered job queue (1–10) with status tracking, graceful cancellation, and truncation warnings
- **Memory journal** — append, search, and expire reflection entries without touching the hermes runtime
- **Live dashboard** — Rich-powered terminal dashboard showing active runs, queue depth, and profile health
- **Full TUI** — patched Hermes terminal UI with TAG skin; also works fully headless for CI and scripting
- **Benchmark suite** — built-in task runner with persistent history via `tag benchmark` / `tag runs`
- **Escape hatch** — `tag hermes -- ...` passes any command through to the underlying runtime

## Install

**Python (recommended):**

```bash
pip install tag-agent
```

**pipx (isolated, no venv management):**

```bash
pipx install tag-agent
```

**npm / pnpm:**

```bash
npm install -g tag-agent
# or
pnpm add -g tag-agent
```

> The npm package is a thin Node launcher. On first run it creates an isolated Python runtime
> under `~/.tag/npm-runtime/<version>`. Python **3.11–3.13** must be on your `PATH`.

Requires Python **3.11 – 3.13**.

## Quick start

```bash
tag setup       # provision runtime, create profiles, import credentials
tag tui         # launch the full orchestrator TUI
```

Without the TUI:

```bash
tag submit --task-type mixed --execution direct --prompt "Summarise this repo"
tag benchmark --profile researcher --model-ref openrouter/deepseek/deepseek-v4-flash
```

## Credential import

TAG detects and imports API keys from local AI tool configs with a single command.
No keys are sent anywhere — they are written to the target profile's `.env` file only.

| Command | Source |
|---|---|
| `tag import-claude` | `ANTHROPIC_API_KEY` env, `~/.claude/.credentials.json`, `~/.claude.json` |
| `tag import-gemini` | `GEMINI_API_KEY` env, `~/.gemini/.env`, `~/.gemini/oauth_creds.json` |
| `tag import-codex` | `~/.codex/auth.json` (OpenAI Codex CLI) |
| `tag import-continue` | `~/.continue/config.yaml` or `config.json` (all configured providers) |
| `tag import-mistral` | `MISTRAL_API_KEY` env, `~/.vibe/.env` (Mistral Vibe CLI) |
| `tag import-opencode` | `~/.local/share/opencode/auth.json` (all configured providers) |
| `tag import-zed` | `~/.config/zed/settings.json` `language_models.<provider>.api_key` |
| `tag import-copilot` | `GITHUB_TOKEN` env, `~/.config/gh/hosts.yml` (`gh` CLI) |
| `tag import-aider` | `~/.aider.conf.yml`, `~/.env`, `~/.aider.env` |
| `tag import-aws` | `~/.aws/credentials` (Amazon Bedrock / Q Developer) |
| `tag import-cursor` | Cursor's local SQLite store (BYOK API keys) |
| `tag import-ssh` | SSH key + host → profile env (remote agent execution) |
| `tag import-docker` | Docker image name → profile env (containerised agents) |
| `tag import-modal` | Modal token-id + token-secret → profile env |
| `tag import-daytona` | Daytona workspace-id → profile env |
| `tag import-nous-portal` | Nous Portal API key → profile env |

Each command accepts `--profile <name>` and `--json` for machine-readable output.

## Command reference

**Orchestration:**

| Command | Description |
|---|---|
| `tag setup` | Full first-run bootstrap — runtime, profiles, credentials |
| `tag doctor` | Check runtime health and configuration; `--json` for machine-readable output |
| `tag dashboard` | Live Rich dashboard — active runs, queue depth, profile health |
| `tag tui` | Launch the orchestrator TUI |
| `tag tui --profile coder` | Launch TUI inside a specific profile |
| `tag submit` | Submit a task for direct or kanban execution |
| `tag benchmark` | Run the benchmark suite against a profile/model |
| `tag runs` | Show benchmark run history |
| `tag bootstrap` | Re-bootstrap profiles without full setup |
| `tag update` | Update the managed Hermes runtime |
| `tag status` | Show current profile and model status |

**Swarm & queue (v0.4.0):**

| Command | Description |
|---|---|
| `tag swarm --goal "..." --workers 4` | Fan out a goal to N parallel workers |
| `tag swarm --board research --goal "..."` | Target a named kanban board |
| `tag queue add --prompt "..." --priority 8` | Add a job to the background queue (priority 1–10) |
| `tag queue list` | Show pending and active jobs; `--limit N` to page |
| `tag queue cancel --job-id <id>` | Cancel a pending job |
| `tag queue status --job-id <id>` | Show job status and result |

**Memory journal (v0.4.0):**

| Command | Description |
|---|---|
| `tag memory-journal add "..."` | Append an entry to the journal |
| `tag memory-journal list` | List recent entries |
| `tag memory-journal search "..."` | Full-text search across entries |
| `tag memory-journal forget <id>` | Remove a specific entry |

**Model management:**

| Command | Description |
|---|---|
| `tag models --profile researcher` | List available models for a profile |
| `tag openrouter-models --profile researcher --search gemini` | Search OpenRouter catalog |
| `tag set-model --profile reviewer --ref openrouter/deepseek/deepseek-v4-pro` | Set active model |
| `tag assignments` | Show all profile → model assignments |

**Pass-through commands** (run inside a profile's managed environment):

```bash
tag chat --profile orchestrator -- --help
tag gateway --profile orchestrator -- start
tag kanban --profile orchestrator -- list
tag sessions --profile orchestrator -- list
tag skills --profile orchestrator -- list
tag plugins --profile orchestrator -- list
tag tools --profile orchestrator -- list
tag mcp --profile orchestrator -- list
tag logs --profile orchestrator -- --since 1h
tag memory --profile orchestrator -- status
tag model --profile orchestrator -- list
tag profile -- list
tag completion --profile orchestrator -- zsh
tag prompt-size --profile orchestrator
```

**Full escape hatch:**

```bash
tag hermes --profile orchestrator -- gateway start
```

## Profiles

TAG ships five built-in profiles:

| Profile | Role | Default model |
|---|---|---|
| `orchestrator` | Master — delegates tasks, routes results | `openai-codex/gpt-5.4` |
| `researcher` | Worker — web research and summarisation | `openrouter/deepseek/deepseek-v4-flash` |
| `coder` | Worker — implementation and refactoring | `openrouter/qwen/qwen3-coder` |
| `reviewer` | Worker + verifier — code review | `openrouter/deepseek/deepseek-v4-pro` |
| `codex-runtime-master` | Alternate master for Codex app-server flows | (Codex runtime) |

Override the model for any profile:

```bash
tag set-model --profile coder --ref openrouter/anthropic/claude-sonnet-4-5
```

## Task routing

| Task type | Workers | Verifier | Execution |
|---|---|---|---|
| `research` | researcher | reviewer | Kanban |
| `implementation` | coder | reviewer | Kanban |
| `review` | reviewer | reviewer | Direct |
| `mixed` | researcher + coder | reviewer | Kanban |

## Configuration

State lives under `~/.tag/` by default:

```
~/.tag/
  config/tag.yaml
  config/benchmark-suite.yaml
  managed/hermes-agent-upstream/
  runtime/home/
  runtime/tag.sqlite3
```

```bash
export TAG_HOME=/custom/path   # override root
```

## Requirements

- Python **3.11 – 3.13**
- `npm` — required for the full TUI build on first run; not needed for `submit` / `benchmark` / `queue` / `swarm` / `doctor` / `dashboard`
- `git` — recommended for `tag update` on git-backed checkouts

## Architecture

```
tag CLI
├── Management plane  (SQLite — no API key needed)
│   ├── kanban.py     native task create / monitor
│   ├── queue_worker  priority background jobs
│   └── dashboard     Rich live view
└── Execution plane   (Hermes gateway — API key required)
    ├── swarm         fan-out topology
    ├── submit        direct / kanban dispatch
    └── tui           full terminal UI
```

The split means `tag queue`, `tag swarm`, `tag dashboard`, `tag doctor`, and credential-import commands work offline and without an active API key.

## Notes

- TAG does not require a pre-installed Hermes checkout. It provisions one from the bundled source snapshot on first run, and falls back to `git clone` only if the snapshot is unavailable.
- If a valid Hermes checkout is already present on the machine, TAG reuses it automatically.
- `tag update` is lifecycle-aware: on a bundled checkout it refreshes from the packaged snapshot; on a git-backed checkout it delegates to Hermes' own update flow.
- The npm distribution is a launcher wrapper around the Python package, not a Node reimplementation.
- Credential import commands only write to the target profile's local `.env` — no keys leave the machine.

## License

MIT — see [LICENSE](https://github.com/sanskarpan/tag-agent/blob/main/LICENSE).
