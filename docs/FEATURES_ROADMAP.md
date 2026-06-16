# TAG Feature Roadmap

Features identified through competitive analysis against Aider, CrewAI, AutoGen, Claude Code, Composio, ShellGPT, Open Interpreter, and related projects.

Each item includes a proposed CLI surface so design and implementation can be scoped independently.

---

## Tier 1 — High-impact, low-complexity

### 1. Lifecycle Hooks System

**Gap**: Claude Code ships 30+ hooks (`PreToolUse`, `PostToolUse`, `SessionStart`, `Stop`, `SubagentStop`). TAG has no equivalent event system.

**Proposed CLI**:
```bash
tag hooks add --event PreSubmit  --command "tag security scan {file}"
tag hooks add --event PostRun    --command "slack-notify finished"
tag hooks add --event BudgetHit  --command "pagerduty alert budget"
tag hooks list
tag hooks remove <id>
```

Events: `PreSubmit`, `PostRun`, `RunFailed`, `BudgetHit`, `CronFired`, `LoopIteration`.

---

### 2. Stream-JSON Output Mode

**Gap**: `--json` returns a single object after completion. No streaming format for long-running runs.

**Proposed CLI**:
```bash
tag submit --stream-json --prompt "Generate 500 unit tests"
# Emits newline-delimited JSON events:
# {"event":"start","run_id":"..."}
# {"event":"token","text":"..."}
# {"event":"tool_use","tool":"bash","input":"pytest"}
# {"event":"cost","tokens":1234,"usd":0.012}
# {"event":"done","exit_code":0}
```

Compatible with `jq -r` pipelines and log aggregators.

---

### 3. Process Reward Model (PRM) Evals

**Gap**: TAG's eval framework scores outputs but has no step-level reward signal.

**Proposed CLI**:
```bash
tag eval create --name "code-quality" --prm                # enable PRM
tag eval run <id> --score-steps                            # score each reasoning step
tag eval report <id> --json                                # step-level scores
```

PRM assigns a correctness probability to each chain-of-thought step, surfacing which reasoning node went wrong.

---

### 4. A/B Model Testing and Shadow Mode

**Gap**: No way to run two models on the same task and compare outputs or cost.

**Proposed CLI**:
```bash
# A/B: run both, pick winner by evaluator
tag ab-test --model-a claude-opus-4 --model-b gpt-5 \
  --prompt "Refactor this auth module" \
  --judge claude-sonnet-4-6

# Shadow: primary serves the user; shadow runs silently for comparison
tag shadow add --primary claude-opus-4 --shadow gpt-5 --sample-rate 0.1
tag shadow report --json
```

---

### 5. Headless JSON API Server

**Gap**: `tag serve` exists but is not documented or tested for machine-to-machine use.

**Proposed surface**:
```
POST /v1/submit   {"prompt":"...","profile":"coder"}
GET  /v1/runs/{id}
GET  /v1/runs/{id}/stream   (SSE)
GET  /v1/health
```

---

## Tier 2 — Medium complexity

### 6. Agent-to-Agent (A2A) Protocol

**Gap**: A2A is now Linux Foundation-governed; CrewAI and LangGraph both support it. TAG has no Agent Card publication or A2A client.

**Proposed CLI**:
```bash
# Publish an Agent Card at /.well-known/agent.json
tag agent publish \
  --name "tag-coder" \
  --description "Autonomous coding agent" \
  --url https://myhost.example.com \
  --capabilities code,review

# Discover and call remote agents
tag agent discover --url https://remote.example.com/.well-known/agent.json
tag agent call <remote-agent-id> --task "Generate changelog" --json
```

Agent Card schema: `name`, `description`, `url`, `capabilities[]`, `auth{}`, `defaultInputModes[]`.

---

### 7. Constitutional AI Guardrails

**Gap**: No built-in content policy layer. TAG submits prompts without a filter stage.

**Proposed 7-layer stack** (inspired by Anthropic's Constitutional AI research):
1. Immutability check — rejects requests to alter core system prompts
2. Temporal guard — rejects requests with impossible timestamps
3. Referential integrity — rejects hallucinated tool/file references
4. Authority escalation guard — rejects requests that claim elevated permissions
5. Deduplication — surfaces if the same request was already answered
6. Provenance tracking — attaches source chain to every response
7. Constitutional policy — scores output against a configurable policy document

**Proposed CLI**:
```bash
tag guard enable --layers 1,2,4,7
tag guard policy set --file my-policy.yaml
tag guard check --prompt "..." --json    # dry-run without submit
```

---

### 8. Reasoning Mode Selection

**Gap**: No way to ask a model to use structured reasoning (ReAct, Tree-of-Thought, etc.) without manually crafting the system prompt.

**Proposed CLI**:
```bash
tag submit --reason react   \
  --prompt "Debug this failing test"
# Injects: Thought/Action/Observation loop scaffolding

tag submit --reason tot --branches 3 \
  --prompt "Design the database schema for a social app"
# Generates 3 candidate plans, evaluates each, picks best

tag submit --reason reflexion \
  --prompt "Write a quicksort implementation" --max-iters 5
# Iterates with self-critique until tests pass or cap hit
```

Modes: `react`, `reflact`, `tot` (Tree-of-Thought), `lats` (Language Agent Tree Search), `reflexion`, `mcts`.

---

### 9. Vector-Backed Memory with Semantic Search

**Gap**: `memory-journal` is full-text keyword search only. Semantic retrieval is missing.

**Proposed CLI**:
```bash
tag mem add "The payments module uses idempotency keys for all Stripe calls"
tag mem search "idempotency" --top-k 5 --json
tag mem forget <id>
tag mem status --json    # index size, embedding model, store path
```

Local embeddings via `fastembed` (no external API). Stored in `~/.tag/runtime/mem.lance` (LanceDB).

---

### 10. Multi-Agent Teams

**Gap**: `tag swarm` fans out identical tasks. No concept of role-differentiated teams with a shared task list.

**Proposed CLI**:
```bash
# Define a team
tag team create "fullstack-crew" \
  --agent orchestrator:orchestrator \
  --agent coder:coder \
  --agent reviewer:reviewer

# Run a team on a goal
tag team run "fullstack-crew" \
  --goal "Build a REST API for user authentication"

# Inspect
tag team list
tag team show "fullstack-crew" --json
```

The orchestrator agent breaks the goal into tasks; coder implements; reviewer verifies. Results fed back through a shared kanban board.

---

### 11. Per-User Entity-Scoped Authentication (Composio pattern)

**Gap**: TAG credential store is global per profile. No per-entity/user scoping.

**Proposed CLI**:
```bash
# Create an entity (e.g. one per end-user of your product)
tag entity create --id user-42

# Attach credentials to the entity
tag entity auth --id user-42 --provider github --token <tok>
tag entity auth --id user-42 --provider slack --token <tok>

# Run a task scoped to an entity
tag submit --entity user-42 --prompt "Open a PR on their repo"
```

Useful when building multi-tenant products on top of TAG.

---

### 12. Webhook Trigger Subscriptions

**Gap**: Cron fires on time. No way to trigger agents on external events (GitHub webhook, Slack message, Stripe event).

**Proposed CLI**:
```bash
# Register a webhook receiver
tag triggers add \
  --name on-pr-opened \
  --event github.pull_request.opened \
  --task "Review this PR: {event.pull_request.html_url}"

# TAG starts an HTTP receiver at :7832 by default
tag triggers serve --port 7832

# List triggers
tag triggers list --json
```

Payload templating: `{event.field}` interpolation from the incoming JSON body.

---

### 13. Prompt Optimization (DSPy pattern)

**Gap**: Prompts are static strings. No automatic optimization loop.

**Proposed CLI**:
```bash
# Optimize a prompt against a training set
tag prompt-opt run \
  --prompt-file prompts/code-review.txt \
  --training-set evals/code-review-gold.jsonl \
  --metric pass-at-1 \
  --iterations 10

# Apply optimized prompt to a profile
tag prompt-opt apply <opt-id> --profile reviewer
```

Gradient-free optimizer (e.g. MIPROv2-style): generates candidate variations, scores on training set, keeps best.

---

## Tier 3 — Research / Long-horizon

### 14. MCTS-Based Planning

Tree search over possible action sequences before committing. Each leaf is evaluated by a value model (LLM-as-judge or PRM). Higher accuracy on complex, multi-step tasks at the cost of more tokens.

```bash
tag submit --planner mcts --budget-tokens 50000 \
  --prompt "Migrate this Rails app from ActiveRecord to Sequel"
```

---

### 15. MemEx Persistent Scratchpad

An agent-maintained working memory that persists across sessions. The agent reads its scratchpad at the start of each run and writes conclusions at the end.

```bash
tag scratchpad show --profile coder
tag scratchpad clear --profile coder
```

---

### 16. Automated Incident Response

Integrate with PagerDuty / OpsGenie. When an alert fires, TAG auto-triggers a debugging loop, attaches logs, and drafts a post-mortem.

```bash
tag incident configure --provider pagerduty --key <key>
tag incident rules add \
  --alert "High error rate" \
  --task "Investigate and draft incident report"
```

---

### 17. Multi-modal Input (images + files)

```bash
tag submit --image screenshot.png --prompt "What's wrong with this UI?"
tag submit --file design.pdf --prompt "Summarise the architecture decisions"
```

---

### 18. Automated Changelog Generation

On release tag, diff changes since last tag, group by type (feat/fix/perf), draft release notes, open a PR.

```bash
tag changelog generate --since v0.6.4 --format keepachangelog
tag changelog pr --base main --head release/v0.7.0
```

---

### 19. Agent Skill Marketplace

Publish and install reusable agent skill packages:

```bash
tag marketplace skill install @community/data-science
tag marketplace skill list --search security
```

---

### 20. Latency-aware Model Selection

Automatically pick the fastest model that meets a quality threshold for interactive tasks.

```bash
tag submit --latency-budget 2s --prompt "Fix this typo"
# Picks a fast model; degrades to larger model only if needed
```

---

### 21. Hierarchical Memory (short/long-term split)

Separate in-session scratchpad (cleared per run) from a long-term knowledge base (persists forever, requires explicit commits).

---

### 22. Fine-grained Permission System

Per-profile, per-tool permission grants. Requires human approval for dangerous tools (bash exec, file delete).

```bash
tag permissions set --profile coder --tool bash --require-approval true
tag permissions set --profile researcher --tool web_search --auto true
```

---

### 23. Headless Eval CI Integration

```bash
tag eval run --suite qa-regression --fail-below 95 --exit-code
# Exits non-zero if pass rate < 95% — suitable for CI gate
```

---

### 24. Automated Dependency Update PRs

Periodically checks for outdated packages, creates a branch, updates lockfiles, runs tests, opens a PR.

```bash
tag dep-update schedule --cron "0 8 * * 1"   # every Monday 8am
```

---

### 25. Cost Forecasting

Before running an expensive task, estimate cost based on prompt size, model pricing, and expected output length.

```bash
tag estimate --profile orchestrator \
  --prompt-file my-task.txt \
  --json
# {"estimated_input_tokens": 12500, "estimated_output_tokens": 3000,
#   "estimated_usd": 0.045, "model": "claude-opus-4"}
```

---

## Implementation Notes

- Features 1-5 can be implemented without changes to the management plane schema.
- Features 6, 10, 12 require new SQLite tables.
- Features 8, 9, 13 benefit from optional Python extras (`tag[reasoning]`, `tag[memory]`).
- Feature 7 (Constitutional AI) should be implemented as a middleware wrapper around `cmd_submit`, not woven into individual commands.
- All new subcommands must ship with a `--json` flag and a regression test.
