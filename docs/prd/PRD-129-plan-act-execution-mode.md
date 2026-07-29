# PRD-129: Plan/Act Execution Mode — a Read-Only Planning Phase Before Mutation (`tag run --mode plan`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD is Go-native from the start — there is no Python precursor to re-frame.

> ⚠️ **This is not PRD-105.** PRD-105 (`tag plan decompose`) is a **task-graph** feature: an LLM planner emits a DAG of typed subtasks with dependency edges, persisted as `plan.json` and executed by the `internal/queue` scheduler. PRD-129 is an **execution-mode** feature: a single agent loop runs with all mutating tools denied, produces a human-readable plan, and stops. They are orthogonal and must not be merged. See §2.4 for the full distinction, and note the deliberately non-overlapping command surfaces: PRD-105 owns the `tag plan` command *group*; PRD-129 owns the `--mode` *flag* on the existing run commands and adds no new top-level command.

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** S-M (1-2 weeks)
**Category:** Agent Loop & Execution Safety
**Affects:** `internal/agent` (loop `Options`, mode-aware system prompt, `Result.Plan`), `internal/permission` (a mode-derived rule layer; no changes to the package's own types), `internal/tool` (mode-aware registration), `internal/cli` (`--mode`/`--plan-out`/`--from-plan` on `run`, `shell`, `ci`, `loop`, `issue-solve`, `swe-solve`, `agentic-ci`), `internal/store` (`plans` table, `runs.mode` column), `internal/contextwin` (plan carried into the act phase as a budgeted context item)
**Depends on:** PRD-021 (agent loop / autonomous mode — the loop this mode modifies), PRD-013 (distributed agent tracing & observability — plan and act phases are distinct spans), PRD-018 (context window & long-context management — the plan is the compacted handoff between phases), PRD-032 (agent replay / time-travel debugging — a saved plan is a replayable artifact), PRD-039 (token budget enforcement — plan and act phases are budgeted separately), PRD-055 (issue-to-PR autonomous loop — the highest-value consumer of a reviewable pre-mutation plan)
**Related, explicitly NOT a dependency:** PRD-105 (TDAG task decomposition) — see §2.4. PRD-078 (HITL tool approval) — adjacent but per-call rather than per-phase; §2.5 explains why per-call approval does not solve this.
**Composes with:** the shipped tool-permission model in `internal/permission` (no PRD; landed with `tag permissions show|log`)
**Inspired by:** Cline Plan/Act, Kilo Code, OpenHands Planning Mode + `PLAN.md`, Kiro spec-driven Requirements→Design→Tasks, Aider `/ask`, Claude Code plan mode, Goose

---

## 1. Overview

Plan/Act is the pattern where an agent first runs in a **read-only** phase — it may read files, list directories, search, and reason, but it may not write, execute, or otherwise mutate anything — and produces a structured plan of what it intends to do. The human reads the plan. If it is right, they transition to the **act** phase, in which the plan is carried forward as context and the normal tool set is restored. If it is wrong, they have spent a few cents and thirty seconds rather than discovering the misunderstanding after fourteen files were rewritten.

The competitive audit (`docs/COMPETITIVE_PARITY_2026_07.md` §5.1) lists plan/act separation as shipped by Cline, Kilo, OpenHands, Kiro, Aider, Jules, Devin, Claude Code and Goose — nine peers — and rates it "🟠 High. Cheap to add, high perceived value." TAG-Go has nothing equivalent: `tag run --tools` either has the mutating tools or it does not, and the decision is made before the model has seen the repository.

The insight that makes this cheap for TAG specifically is that **plan mode is a permission policy, not a new execution engine**. TAG shipped a first-match-wins permission gate in `internal/permission` where every tool is routed through `permission.Wrap`, rules carry a `Source` for audit, and a nil guard fails closed. Plan mode is, precisely: prepend `{Tool: <every mutating tool>, Action: Deny, Source: "mode:plan"}` to the resolved ruleset, adjust the system prompt to explain the constraint and request a plan, and stop after the model produces one. There is no second loop, no new scheduler, no new provider path. The existing `Guard`, `Rule`, `Source` and audit machinery carry the entire feature — which is a strong signal that the permission model was designed at the right altitude.

The second design decision is the **shape of the artifact**. A plan is not free text. It is a `plan.md` with YAML frontmatter (goal, profile, model, token/cost accounting, a SHA-256 of the plan body, and the set of files the model says it will touch) plus a Markdown body of ordered steps. It is written to disk, recorded in SQLite, and — critically — the act phase can be re-entered from it later, on another machine, or by a different operator, via `--from-plan`. This turns "plan mode" from an interactive convenience into an automation primitive: a scheduled job can produce a plan for human review overnight and a human can execute it in the morning with `tag run --mode act --from-plan`, and the audit trail links the two runs.

The third decision is honesty about what plan mode is **not**. It is not a sandbox. A read-only phase constrains *TAG's tools*; it does not constrain the model's ability to be wrong, and it does not stop a plan from describing a destructive action that the human then approves. The audit is blunt that `tag sandbox` currently does not sandbox; this PRD must not be read as compensating for that. Plan mode is a review checkpoint, and the PRD says so in §10 rather than implying isolation it does not provide.

---

## 2. Problem Statement

### 2.1 The decision to mutate is made before the agent has any information

`tag run --tools "refactor the auth module"` registers `bash`, `read_file`, `write_file` and `list_dir` up front. From the operator's side, the moment of consent is *before* the model has read a single file — before anyone, human or model, knows what "refactor the auth module" is going to mean in this repository. The operator is consenting to an unbounded set of future actions on the basis of their own prompt. This is the wrong ordering: the information needed to consent well only exists after the read-only investigation, and there is currently no way to pause there.

### 2.2 Per-call approval solves a different problem and solves it badly for this one

TAG's `ask` verdict does prompt per call. But approving fourteen sequential `write_file` prompts is not review — it is a clickthrough. The human sees each edit in isolation, without the shape of the whole change, and has no way to say "the third step is wrong, start over" other than denying and watching the model improvise around the denial. Worse, the audit's own framing applies: `ask` with no TTY is a deny, so per-call approval is unavailable in exactly the headless contexts (`queue worker`, `dag run --execute`, `cron run --execute`, CI) where an unreviewed mutation is most dangerous. A plan is reviewable *asynchronously* and *as a whole*; a prompt is neither.

### 2.3 There is no artifact between "prompt" and "diff"

Today the only durable outputs of a run are the transcript and whatever the tools did to the filesystem. There is nothing in between — no statement of intent that can be diffed against the outcome, attached to a PR, reviewed by someone who was not at the terminal, or replayed. For the autonomous workflows TAG already ships (`issue-solve`, `swe-solve`, `agentic-ci`, `review-pr`), this is the missing artifact: the thing a reviewer would actually want to see first.

### 2.4 The distinction from PRD-105, stated so it cannot be collapsed later

This repository has a documented history of cross-references drifting after renumbering, and "plan" is an overloaded word. To make the boundary unmergeable:

| | **PRD-105** — `tag plan decompose` | **PRD-129** — `tag run --mode plan` |
|---|---|---|
| Kind of feature | Task-graph construction | Execution-mode constraint |
| What it produces | `plan.json` — a DAG of typed nodes with `depends_on` edges, HMAC-signed | `plan.md` — ordered prose steps for a human to read |
| Who consumes it | The `internal/queue` scheduler, which dispatches nodes concurrently | A human reviewer, then the same single agent loop |
| Number of agent loops | Many — one per DAG node, up to `--parallel N` | Exactly one, in each phase |
| Purpose | Parallelism, dependency ordering, per-node failure recovery and re-plan | Safety — separating "decide what to do" from "do it" |
| Permission relationship | Orthogonal; nodes run under the ordinary policy | *Is* a permission policy |
| Command surface | The `tag plan` command group | The `--mode` flag on existing run commands |
| Effort class | L (2-4 weeks) | S-M (1-2 weeks) |

They compose rather than compete: a future `tag plan decompose --mode plan` could produce a DAG whose nodes are themselves plan-mode runs. Nothing in this PRD forecloses that, and nothing in it duplicates PRD-105's scheduler, DAG validation, `$node.output` substitution, re-plan, or HMAC plan signing.

### 2.5 Headless automation has no review checkpoint at all

`tag cron run --execute` and `tag queue worker` deliberately run with a nil `Prompter` (the `permFlags.guard` comment is explicit that this is the safety hinge: `ask` becomes an immediate deny rather than a hang). The consequence is that a scheduled job either runs with pre-granted permissions or does nothing. There is no middle setting — "investigate and tell me what you would do, then wait for me" — which is precisely the setting most operators want for an overnight automation that touches a repository.

---

## 3. Goals and Non-Goals

### Goals

| # | Goal |
|---|------|
| G1 | `tag run --mode plan "<task>"` runs the agent loop with every mutating tool denied and emits a structured plan; the process exits without having changed anything outside the plan file. |
| G2 | The read-only constraint is implemented as a `Source: "mode:plan"` rule layer prepended to the resolved policy — reusing `internal/permission` wholesale, adding no new enforcement path. |
| G3 | Denials in plan mode are visible: `tag permissions log` shows them with source `mode:plan`, so "the agent tried to write during planning" is an observable event rather than an invisible one. |
| G4 | The plan is a durable artifact: `plan.md` (YAML frontmatter + Markdown body) written to `--plan-out`, recorded in the `plans` table, content-addressed by SHA-256. |
| G5 | `tag run --mode act --from-plan PATH` resumes from a saved plan on any machine, injecting it as leading context and linking the two runs in `runs`. |
| G6 | `tag run --mode auto` runs both phases in one invocation with an interactive confirmation between them; on a non-TTY it degrades to plan-only and says so, rather than silently proceeding to act. |
| G7 | Plan and act are separate spans (PRD-013) and separately budgeted (PRD-039), so the cost of planning is attributable and cappable independently. |
| G8 | Headless execution paths (`queue worker`, `dag run --execute`, `cron run --execute`) accept `--mode plan`, producing a reviewable plan instead of either mutating or refusing. |
| G9 | Plan mode is fully exercisable offline against the `echo` provider. |
| G10 | Which tools count as mutating is derived from a single declared classification in `internal/tool`, so a newly added tool must be classified — it cannot default to "safe in plan mode" by omission. |
| G11 | `tag run --mode act --from-plan` verifies the plan's SHA-256 and refuses a plan whose body was edited after signing unless `--allow-modified-plan` is passed. |
| G12 | Existing behaviour is unchanged: with no `--mode` flag, `tag run` is byte-for-byte what it is today. |

### Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Task-graph decomposition, DAG execution, parallel node dispatch, re-plan, stall detection. That is PRD-105 in its entirety. |
| NG2 | Structured/machine-executable plan steps. The v1 plan body is prose for a human. A machine-executable plan is PRD-105's `plan.json`. |
| NG3 | An interactive plan *editor*. The plan file is the editing surface; `$EDITOR` is the editor. |
| NG4 | OS-level isolation. Plan mode denies TAG's mutating tools; it is not a sandbox and this PRD explicitly does not claim to compensate for the sandbox regression noted in the July 2026 audit. |
| NG5 | Automatic plan-vs-outcome verification ("did the act phase do what the plan said?"). Valuable, needs `internal/diffcontext` + an LLM judge, and belongs in a follow-up that can build on PRD-045. |
| NG6 | Replacing per-call approval. `ask` remains and composes: plan-mode review is coarse-grained and asynchronous; `ask` is fine-grained and synchronous. Both, not either. |
| NG7 | A separate "architect" model for the plan phase. `tag split` (PRD-042) already owns architect/editor separation; `--mode plan` accepts whatever model the profile resolves. |
| NG8 | Porting to the Python edition. Go-native. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Mutation containment | Zero filesystem changes outside `--plan-out` across 200 randomized plan-mode runs | Filesystem-hash before/after in a fuzz harness |
| Enforcement reuse | Zero new enforcement code paths: every plan-mode denial flows through `Guard.Check` | Code review + a test asserting every denial has a non-empty `Rule.Source` |
| Plan phase cost | Plan phase for a mid-size repo task completes in ≤ 40% of the tokens of a full act run | Benchmark over 10 fixture tasks |
| Round-trip fidelity | `--from-plan` reproduces the plan text in the act phase's first user-visible context with 100% fidelity | Integration test |
| Headless degradation | `--mode auto` on a non-TTY produces a plan and exits 0 with an explicit "not proceeding to act" message in 100% of cases | Integration test with no TTY |
| Tamper detection | 100% of byte-modified plan files are refused without `--allow-modified-plan` | Unit test |
| Classification completeness | 100% of registered tools have an explicit mutation classification; adding an unclassified tool fails a test | Exhaustiveness test over the registry |
| Offline honesty | Plan and act phases both run to completion against `echo` with no keys | Offline CI job |
| No regression | `tag run` without `--mode` produces a byte-identical system prompt and tool registry to the pre-feature build | Regression test |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag run --mode plan --tools "migrate the auth module to the new session API"` | I see the intended change before any file is touched |
| U2 | Developer | read `plan.md`, fix one wrong assumption in my prompt, and re-plan | I iterate on intent for cents instead of on diffs for minutes |
| U3 | Developer | run `tag run --mode act --from-plan plan.md` after approving | execution follows the plan I actually read |
| U4 | Reviewer | receive `plan.md` attached to a PR | I can review intent separately from implementation |
| U5 | Team lead | schedule `tag cron` with `--mode plan` overnight | I arrive to a reviewable plan rather than an unreviewed commit |
| U6 | Security-conscious operator | confirm via `tag permissions log` that no write was permitted during planning | the read-only claim is auditable, not asserted |
| U7 | Security-conscious operator | see that the agent *attempted* to write during planning | I learn the model misunderstood the mode, which is a quality signal |
| U8 | CI engineer | run `tag agentic-ci --mode plan` on a PR and post the plan as a comment | reviewers see what the bot would do before it does it |
| U9 | Cost-conscious operator | cap the plan phase at `--plan-budget-usd 0.10` independently of the act phase | investigation cannot silently become the expensive part |
| U10 | Operator | have `--mode auto` refuse to proceed on a non-TTY | automation never silently self-approves |
| U11 | Auditor | trace an act run back to the plan that authorized it | the approval chain is reconstructible after the fact |

---

## 6. Proposed CLI Surface

**Collision check.** This PRD adds **no new top-level command**. It adds flags to commands that already exist (`tag run`, `tag shell`, `tag ci`, `tag loop`, `tag issue-solve`, `tag swe-solve`, `tag agentic-ci`, `tag queue worker`, `tag dag run`, `tag cron run`). The names `--mode`, `--plan-out`, `--from-plan`, `--plan-budget-usd` and `--allow-modified-plan` are unused on those commands in the built binary (`tag run --help` verified: `--allow-tool`, `--ask-tool`, `--auto-approve`, `--dangerously-allow-all`, `--deny-tool`, `--disable-tools`, `--fallback`, `--max-steps`, `--no-prompt`, `--profile`, `--provider`, `--system`, `--timeout`, `--tools`, `--web`). The `tag plan` command group is left entirely to PRD-105 — this PRD does not register it, reference it as a surface, or claim any subcommand under it.

### 6.1 Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--mode plan\|act\|auto` | `act` | `plan` = read-only, produce a plan, stop. `act` = today's behaviour. `auto` = plan, confirm, then act. |
| `--plan-out PATH` | `plan.md` (plan/auto only) | Where the plan is written. `-` writes to stdout. |
| `--from-plan PATH` | none | Act on a previously saved plan; implies `--mode act`. |
| `--plan-budget-usd F` | none | Cap for the plan phase only. |
| `--allow-modified-plan` | false | Accept a plan whose body SHA-256 does not match its frontmatter. |

`--mode` is deliberately a tri-state on one flag rather than three booleans, so `--mode plan --mode act` is impossible to express.

### 6.2 Plan phase

```bash
tag run --mode plan --tools --provider anthropic \
  "Migrate the auth module from the legacy session store to the new API"
```

```
mode: plan (read-only — write_file, bash and all mutating tools are DENIED for this phase)
profile: coder   model: claude-sonnet-4-6

  [step 1] list_dir  src/auth
  [step 2] read_file src/auth/session.go
  [step 3] read_file src/auth/middleware.go
  [step 4] read_file internal/store/session_test.go

Plan written to plan.md  (sha256 a3f7c2…, 1,204 tokens, $0.006)

  4 files to modify · 1 file to add · 0 to delete
  next: review plan.md, then `tag run --mode act --from-plan plan.md --tools`
```

### 6.3 The plan artifact

```markdown
---
plan_version: 1
plan_id: plan-a3f7c2e1
goal: Migrate the auth module from the legacy session store to the new API
profile: coder
model: anthropic/claude-sonnet-4-6
created_at: 2026-07-29T11:04:18Z
run_id: 8f21c0d4ae61b3
body_sha256: a3f7c2e1b904...
tokens: {prompt: 8140, completion: 1204}
cost_usd: 0.006
files_touched:
  - {path: src/auth/session.go, op: modify}
  - {path: src/auth/middleware.go, op: modify}
  - {path: src/auth/newapi.go, op: add}
denied_attempts: 0
---

## 1. Replace the session store interface in `src/auth/session.go`
`SessionStore` currently exposes `Get(id) (*Session, error)`. The new API is
context-first… (etc.)

## 2. Update middleware call sites
…

## Risks
- `session_test.go` asserts the legacy error string; step 4 updates it.
- No migration is needed for stored data: the wire format is unchanged.
```

`files_touched` is the model's *declaration of intent*, extracted from its structured output. It is used for review and for the (NG5, future) plan-vs-outcome comparison. **It is not an enforcement mechanism** — the act phase does not restrict writes to that list, because doing so would be security theatre: the list comes from the same model whose behaviour is in question. The PRD says this plainly rather than letting a reader assume otherwise. An operator who wants enforcement has the real mechanism already: `--allow-tool write_file:src/auth/**`.

`denied_attempts` records how many times the model tried a mutating tool during planning — a useful quality signal about whether the model understood the mode.

### 6.4 Act phase from a plan

```bash
tag run --mode act --from-plan plan.md --tools --provider anthropic
```

Verifies `body_sha256`, injects the plan body as leading context in a delimited block, restores the ordinary policy, records `runs.plan_id` linking this run to the plan, and proceeds. A mismatched hash is refused with the computed and expected digests printed.

### 6.5 `--mode auto`

Plan, render the plan, prompt `Proceed? [y/N]` on stderr, then act in the same process (no re-read from disk; the in-memory plan is used, and still written to `--plan-out` for the record). On a non-TTY — the queue worker, cron, CI — it does **not** prompt and does **not** proceed:

```
mode: auto requested, but no interactive terminal is available.
Plan written to plan.md. Not proceeding to act.
Re-run with `--mode act --from-plan plan.md` once reviewed.
```

Exit code 0. This mirrors the existing `ask`-becomes-deny discipline exactly: the non-interactive path is the *conservative* one, never the permissive one.

### 6.6 Configuration

```yaml
plan:
  default_mode: act          # set to `plan` to make read-only the default posture
  out: plan.md
  budget_usd: 0.25
  extra_readonly_tools: []   # tools from plugins/MCP to treat as safe in plan mode
  extra_mutating_tools: []   # tools to treat as mutating (default for unknown tools)

profiles:
  ci-bot:
    config:
      plan:
        default_mode: plan   # this profile can never mutate without an explicit --mode act
```

---

## 7. Functional Requirements

| ID | Requirement | Acceptance Test |
|----|------------|-----------------|
| FR-01 | `tool.Mutating(name) bool` classifies every built-in: `read_file`, `list_dir`, `web_search` (and `load_skill`, if PRD-128 lands) are read-only; `bash` and `write_file` are mutating. | Exhaustiveness unit test over `Registry.Defs()`; a tool with no classification fails the test. |
| FR-02 | Unknown tools (MCP bridge, plugins) default to **mutating**. A tool must be explicitly declared read-only to be usable in plan mode. | Unit test with a synthetic unknown tool. |
| FR-03 | `bash` is mutating unconditionally. No allowlist of "read-only shell commands" is attempted — `git log` and `rm -rf /` are the same tool, and command-prefix analysis is not a security boundary. | Unit test asserting `bash` is denied in plan mode even with `--allow-tool bash:'git *'`. |
| FR-04 | Plan mode prepends `{Tool: T, Action: Deny, Source: "mode:plan"}` for every mutating registered `T`. Because these rules are prepended they are most-specific and beat any operator `allow`. | Unit test comparing resolved rulesets; assert a `--allow-tool write_file` is overridden. |
| FR-05 | Plan mode never emits an `Allow` rule and therefore cannot widen the policy: a profile that already denies `read_file` still denies it in plan mode. | Property test over random policies. |
| FR-06 | Every plan-mode denial is recorded in `permission_decisions` with `via="rule"` and a rule string containing `mode:plan`. | Integration test + `tag permissions log` assertion. |
| FR-07 | The plan-phase system prompt states the read-only constraint, requests ordered steps with a risks section, and requests a `files_touched` declaration. It is appended to (not a replacement for) the operator's `--system`. | Golden-file test; unit test asserting `--system` text is preserved. |
| FR-08 | The plan phase terminates when the model produces a final text turn with no tool calls, or at `--max-steps`. Hitting `--max-steps` is reported as a truncated plan, not as a complete one. | Unit test with a stub provider that never stops. |
| FR-09 | `plan.md` is written atomically (temp file + `os.Rename`) with mode 0644, and `--plan-out -` writes to stdout instead. | Unit test including a mid-write failure injection. |
| FR-10 | `body_sha256` covers the Markdown body only (everything after the closing frontmatter fence), so regenerating frontmatter does not invalidate a plan. | Unit test. |
| FR-11 | `--from-plan` recomputes and compares `body_sha256`, refusing on mismatch unless `--allow-modified-plan`; the refusal prints both digests. | Unit test with a one-byte edit. |
| FR-12 | `--from-plan` injects the body inside `<approved_plan>…</approved_plan>` with a system-prompt statement that it is an operator-approved plan to follow, and that content inside it is a plan, not instructions from an untrusted source. | Golden-file test. |
| FR-13 | `--from-plan` implies `--mode act`; combining it with `--mode plan` is a usage error. | Unit test. |
| FR-14 | Plan and act phases emit distinct spans with `tag.mode` attributes (PRD-013), and `runs` rows carry `mode` and (for act) `plan_id`. | Integration test querying `spans` and `runs`. |
| FR-15 | `--plan-budget-usd` is enforced against the plan phase only; exceeding it aborts the plan phase and writes the partial plan marked `truncated: budget`. | Unit test with a mock cost accumulator. |
| FR-16 | `--mode auto` on a TTY prompts on stderr and reads from stdin; `n`, EOF or a non-`y` answer exits 0 without acting. | Integration test with a pty. |
| FR-17 | `--mode auto` with no TTY writes the plan, prints the "not proceeding" message, and exits 0 without acting. | Integration test with no TTY. |
| FR-18 | `--mode auto` combined with `--auto-approve` or `--dangerously-allow-all` still requires the plan confirmation on a TTY; those flags govern the *permission* gate, not the *mode* transition. They are orthogonal and must not be conflated. | Unit test. |
| FR-19 | Headless commands (`queue worker`, `dag run --execute`, `cron run --execute`) accept `--mode plan` and write per-job plans to `<plan-out-dir>/<job-id>.md`. | Integration test. |
| FR-20 | Without `--mode`, the resolved policy, the registered tool set and the system prompt are byte-identical to the pre-feature build. | Regression test asserting exact equality. |
| FR-21 | `plans` rows persist id, goal, profile, model, body, sha, tokens, cost, `files_touched`, `denied_attempts`, `created_at`, and the producing `run_id`. | Integration test. |
| FR-22 | `tag runs show <id>` displays the mode and, for an act run, the linked plan id. | Integration test. |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|------------|--------|
| NFR-01 | Plan-mode rule construction is O(registered tools) and adds < 1 ms to startup. | Benchmark |
| NFR-02 | No new direct Go modules. Frontmatter uses the vendored `gopkg.in/yaml.v3`; hashing is `crypto/sha256`. `CGO_ENABLED=0` unchanged. | `go mod graph` diff empty |
| NFR-03 | Both phases run against `echo` with no network and no keys. | Offline CI job |
| NFR-04 | `--json` emits the plan as a structured object on all supporting commands, with a stable schema. | `jsonparity` test |
| NFR-05 | `internal/agent` gains no import edge to `internal/permission`. Mode → rules translation lives in `internal/tool` (which already imports both), keeping the loop provider- and policy-agnostic. | Import-graph architecture test |
| NFR-06 | Plan files are human-readable UTF-8 Markdown with no binary content and no embedded secrets (the plan body passes `internal/security` scanning before being written). | Schema + scan test |
| NFR-07 | `go vet`, `golangci-lint`, `staticcheck` clean; `gofmt` clean. | CI gate |
| NFR-08 | Plan writes to SQLite honour the single-writer + WAL contract. | Concurrent integration test |

---

## 9. Technical Design

### 9.1 New and Modified Files

| File | Change | Description |
|---|---|---|
| `internal/tool/mutation.go` | **New** | `Mutating(name) bool`, the read-only/mutating classification, and `PlanModeRules(registered []string) []permission.Rule`. |
| `internal/tool/tools.go` | **Extend** | `Options.Mode`; when `ModePlan`, `Register` composes plan rules into the guard's policy before wrapping. |
| `internal/agent/loop.go` | **Extend** | `Options.Mode`, `Options.ApprovedPlan`; `Result.DeniedAttempts`. |
| `internal/agent/plan.go` | **New** | `Plan` struct, `Marshal`/`Unmarshal` for the frontmatter+body format, `BodySHA256`, `ExtractFilesTouched`. |
| `internal/cli/mode.go` | **New** | `modeFlags` binder (mirroring the existing `permFlags` pattern), `--mode/--plan-out/--from-plan/--plan-budget-usd/--allow-modified-plan`, and the `auto` confirmation. |
| `internal/cli/run.go` and siblings | **Extend** | Bind `modeFlags`; two-phase orchestration for `auto`. |
| `internal/store/schema` | **Extend** | `plans` table; `runs.mode`, `runs.plan_id` columns (additive, self-healing like `ensureResultColumn`). |

### 9.2 SQLite DDL

```sql
CREATE TABLE IF NOT EXISTS plans (
  id              TEXT PRIMARY KEY,          -- "plan-a3f7c2e1"
  run_id          TEXT NOT NULL,             -- the plan-phase run that produced it
  goal            TEXT NOT NULL,
  profile         TEXT NOT NULL,
  model           TEXT NOT NULL,
  body            TEXT NOT NULL,
  body_sha256     TEXT NOT NULL,
  files_json      TEXT NOT NULL DEFAULT '[]',
  denied_attempts INTEGER NOT NULL DEFAULT 0,
  truncated       TEXT,                      -- NULL | "max_steps" | "budget"
  prompt_tokens   INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cost_usd        REAL NOT NULL DEFAULT 0.0,
  file_path       TEXT,
  created_at      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_plans_created ON plans(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_plans_run     ON plans(run_id);

-- Additive columns on runs, applied by the same self-healing PRAGMA table_info
-- pattern the worker already uses for queue_jobs.result:
--   ALTER TABLE runs ADD COLUMN mode    TEXT NOT NULL DEFAULT 'act';
--   ALTER TABLE runs ADD COLUMN plan_id TEXT;
```

### 9.3 Mode and Classification

```go
// internal/tool/mutation.go
package tool

// Mode is the execution posture for one loop.
type Mode string

const (
	ModeAct  Mode = "act"  // today's behaviour
	ModePlan Mode = "plan" // read-only: mutating tools denied
)

// readOnlyTools is the CLOSED set of tools safe in plan mode. Everything absent
// is mutating — including every MCP-bridged and plugin tool — so a tool added
// without thought is conservative by default rather than permissive by default.
var readOnlyTools = map[string]bool{
	ToolReadFile:  true,
	ToolListDir:   true,
	ToolWebSearch: true,
	// "load_skill" is added here if PRD-128 lands: it only reads local files
	// already discovered from the operator's own configured directories.
}

// Mutating reports whether a tool may change state outside the process.
// Unknown => true. This is the fail-closed default and the reason FR-02 exists.
func Mutating(name string) bool { return !readOnlyTools[name] }

// PlanModeRules denies every mutating tool. It emits Deny exclusively, so plan
// mode is strictly a narrowing of the operator's policy — it can never grant
// anything, and a profile that already denies read_file keeps denying it.
func PlanModeRules(registered []string) []permission.Rule {
	var out []permission.Rule
	for _, name := range registered {
		if Mutating(name) {
			out = append(out, permission.Rule{
				Tool:   name,
				Action: permission.Deny,
				Source: "mode:plan",
			})
		}
	}
	return out
}
```

`bash` is deliberately absent from `readOnlyTools` with no per-command carve-out. A "read-only bash" allowlist is the kind of feature that looks reasonable and is not: `git log` is read-only, `git log --ext-diff` with a hostile `.gitattributes` is not, and command-prefix matching cannot tell them apart. FR-03 makes the refusal to attempt this a tested property. An operator who genuinely wants shell access during planning has a supported route — run in `act` mode with `--deny-tool write_file` — which is honest about being a different, weaker guarantee.

### 9.4 Wiring

```go
// internal/tool/tools.go — inside Register, before the add() closure is used.
if opts.Mode == ModePlan {
	g = g.WithRules(PlanModeRules(builtinNames(opts)))  // prepends; most-specific wins
}
```

`Guard.WithRules` is the one small addition to `internal/permission`: it returns a shallow copy of the guard whose `Policy.Rules` are the supplied rules followed by the original ones. It is a copy rather than a mutation so a guard shared across a `queue worker` drain cannot be permanently narrowed by one job — this is the concurrency-safety point that makes the copy semantics non-negotiable.

### 9.5 Two-Phase Orchestration for `--mode auto`

```go
// 1. plan phase — its own guard, its own budget, its own span
planRes, plan, err := runPhase(ctx, ModePlan, planBudget)
if err != nil { return err }
writePlan(plan, planOut)

// 2. gate. The ONLY affirmative path is an explicit "y" from a real terminal.
if !permission.StdioInteractive() {
	fmt.Fprintln(os.Stderr, "mode: auto requested, but no interactive terminal is available.")
	fmt.Fprintf(os.Stderr, "Plan written to %s. Not proceeding to act.\n", planOut)
	return nil
}
if !confirm(os.Stdin, os.Stderr, "Proceed with this plan?") {
	return nil
}

// 3. act phase — ordinary policy restored, plan injected as leading context
_, err = runPhaseWithPlan(ctx, ModeAct, plan, actBudget)
```

Reusing `permission.StdioInteractive()` — the same predicate that decides whether an `ask` can reach a human — is deliberate: there is exactly one definition of "a human is present" in the codebase, and the mode transition uses it rather than inventing a second one that could drift.

### 9.6 Integration Points

| Package | Integration |
|---|---|
| `internal/permission` | Consumes `PlanModeRules` via the new `Guard.WithRules` copy-constructor. No changes to `Rule`, `Policy`, `Resolve` or `Check`. |
| `internal/tool` | Owns the mutation classification and mode→rules translation. |
| `internal/agent` | Mode-aware system prompt; approved-plan injection; `DeniedAttempts` counting. |
| `internal/contextwin` | The plan body is a budgeted context item in the act phase; `tag context status` accounts for it. |
| `internal/store` | `plans`; additive `runs.mode`/`runs.plan_id`. |
| `internal/security` | Plan body scanned before write (a plan quoting a config file could otherwise persist a secret to disk). |
| `internal/cli` | `modeFlags` bound on every loop-running command; `auto` orchestration. |
| PRD-013 spans | `tag.mode` span attribute; plan and act are sibling spans under one trace. |
| PRD-039 budgets | Separate plan/act budget envelopes. |

---

## 10. Security Considerations

1. **Plan mode is not a sandbox, and this PRD does not pretend otherwise.** It denies TAG's mutating tools. It does not constrain a process TAG did not start, does not provide OS-level isolation, and does not make a wrong plan safe. The July 2026 audit's finding that `tag sandbox` does not actually sandbox is a separate, open, higher-severity problem; a reader must not come away thinking `--mode plan` addresses it. Naming this explicitly is the point.

2. **Narrowing-only, structurally.** `PlanModeRules` emits `Deny` exclusively. There is no input — flag, config or model output — that causes plan mode to produce an `Allow`. Asserted by property test (FR-05), not by convention.

3. **`--dangerously-allow-all` still wins, and that is correct.** `Guard.decide` bypasses the ruleset entirely under that flag, so `--mode plan --dangerously-allow-all` is not read-only. Rather than special-casing it — which would create a second, subtler bypass path — plan mode prints a loud warning when both are set, and the flag retains its single documented meaning: it disables the gate. One escape hatch, greppable, unchanged.

4. **Prompt injection via the approved plan.** In the act phase the plan body is model-generated text re-entering context with elevated trust ("the operator approved this"). If the plan phase was itself injected via a hostile file read, the injection is laundered through human approval. Mitigations: the plan is delimited and labelled as a plan rather than as instructions; the human reads it before approving (that is the entire feature); `body_sha256` proves what was approved is what runs; and every act-phase tool call is still gated. The residual risk — a human approving a plan they did not read carefully — is real and is stated rather than hidden.

5. **Plan file tampering.** Body-scoped SHA-256 with `--allow-modified-plan` as the explicit opt-out. Deliberately *not* HMAC-signed: a plan is meant to be reviewed and shared, and an unforgeable signature would imply an authenticity guarantee the workflow does not have. PRD-105's `plan.json` is HMAC-signed because it is machine-executed without a human in the loop; PRD-129's `plan.md` is human-reviewed, so integrity detection is the right strength. The asymmetry is intentional.

6. **Secrets in plans.** A plan quoting a config file could persist a credential to a file that then gets attached to a PR. `internal/security` scans the body before write; a finding refuses the write and reports it.

7. **`files_touched` is not enforcement.** Stated in §6.3 and repeated here because it is the most likely misreading. It is a model-authored declaration used for review, never a write allowlist. The real mechanism is `--allow-tool write_file:<glob>`.

8. **Denial-of-service via denial loops.** A model that repeatedly retries a denied tool burns steps. `--max-steps` already bounds this; additionally, `denied_attempts` exceeding a threshold (default 5) ends the plan phase early with a truncation reason, so a confused model fails fast and visibly.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`internal/tool/mutation_test.go`, `internal/agent/plan_test.go`)

- `TestMutatingClassificationExhaustive` — every name in a freshly built registry has an explicit classification; a synthetic unclassified tool is `Mutating`.
- `TestBashAlwaysMutating` — including with `--allow-tool bash:'git *'`.
- `TestPlanModeRulesOnlyDeny` — over 1,000 random registries, no `Allow` with source `mode:plan`.
- `TestPlanModeCannotWiden` — property test: a policy denying `read_file` still denies it in plan mode.
- `TestPlanRulesPrependedWinOverAllow` — `--allow-tool write_file` is overridden.
- `TestGuardWithRulesIsCopy` — narrowing one derived guard does not affect the parent (the queue-worker concurrency case).
- `TestPlanMarshalRoundTrip`, `TestBodySHA256ExcludesFrontmatter`, `TestPlanTamperDetected`.
- `TestFromPlanImpliesActMode`, `TestFromPlanWithModePlanIsUsageError`.

### 11.2 Integration Tests (`internal/cli/mode_e2e_test.go`)

Temp `TAG_HOME` + temp store, following `internal/cli/e2e_test.go`.

- `TestPlanModeNoMutation` — hash the tree before/after; assert only `plan.md` changed.
- `TestPlanModeDenialLogged` — stub provider requests `write_file`; assert denial with `mode:plan` in `tag permissions log`.
- `TestPlanModeDeniedAttemptsRecorded`.
- `TestAutoModeNoTTYDoesNotAct` — assert exit 0, plan written, no mutation, explicit message.
- `TestAutoModeTTYConfirmThenAct` — pty harness, answer `y`.
- `TestAutoModeTTYDeclineDoesNotAct` — answer `n`.
- `TestFromPlanInjectsBody` / `TestFromPlanRefusesModified`.
- `TestPlanActSpansLinked` — assert two spans with distinct `tag.mode` under one trace, and `runs.plan_id` set.
- `TestQueueWorkerPlanMode` — per-job plans written, nothing mutated.
- `TestNoModeFlagIsByteIdentical` — regression against a golden system prompt and registry.
- `TestDangerouslyAllowAllWarnsInPlanMode`.

### 11.3 Fuzz / Property (`internal/tool/mutation_prop_test.go`)

200 randomized plan-mode runs against a stub provider that emits random tool calls, with a filesystem hash assertion before and after (the Success-Metrics containment row).

### 11.4 Benchmarks

- `BenchmarkPlanModeRules` — 50 tools, < 1 ms.
- `BenchmarkPlanPhaseTokens` — plan vs full act over 10 fixture tasks.

---

## 12. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-01 | `tag run --mode plan --tools` leaves the working tree unchanged except for `plan.md`, over 200 randomized runs. | Fuzz harness |
| AC-02 | A `write_file` attempt during planning is denied and appears in `tag permissions log` with source `mode:plan`. | Integration test |
| AC-03 | `tag run --mode plan --tools --allow-tool write_file` still denies `write_file`. | Unit test |
| AC-04 | `plan.md` round-trips: parse → re-serialize → identical bytes. | Unit test |
| AC-05 | A one-byte edit to the plan body is refused by `--from-plan` with both digests printed. | Unit test |
| AC-06 | `--mode auto` with no TTY writes a plan, exits 0, and mutates nothing. | Integration test |
| AC-07 | `--mode auto` on a TTY answered `n` mutates nothing. | pty integration test |
| AC-08 | An act run started with `--from-plan` records `runs.plan_id` and is reachable from `tag runs show`. | Integration test |
| AC-09 | Plan and act appear as separate spans with distinct `tag.mode`. | Integration test |
| AC-10 | `tag run` with no `--mode` is byte-identical to the pre-feature build (system prompt + registry + resolved policy). | Regression test |
| AC-11 | Both phases complete against `--provider echo` with egress blackholed. | Offline CI job |
| AC-12 | A plan whose body contains a fake AWS key is refused before write, with a security finding. | Integration test |
| AC-13 | Adding a new tool without classifying it fails `TestMutatingClassificationExhaustive`. | CI gate |

---

## 13. Dependencies

| Dependency | Type | Justification |
|---|---|---|
| `gopkg.in/yaml.v3` | Core (already vendored) | Plan frontmatter |
| `crypto/sha256` | Stdlib | Body integrity |
| `modernc.org/sqlite` | Core (project driver) | `plans`; additive `runs` columns |
| `github.com/spf13/cobra` | Core | Flag binding |
| `pgregory.net/rapid` | Test-only | Narrowing property tests |
| `internal/permission` (shipped, no PRD) | Internal | **Hard prerequisite** — plan mode *is* a permission layer; without the gate there is nothing to enforce it |
| PRD-021 (agent loop) | Internal | The loop being modified |
| PRD-013 (tracing) | Internal | Phase spans |
| PRD-018 (context management) | Internal | Plan is the compacted phase handoff |
| PRD-032 (agent replay) | Internal | Plans are replayable artifacts |
| PRD-039 (token budget) | Internal | Per-phase budgets |
| PRD-055 (issue-to-PR loop) | Internal | Highest-value consumer; gains a reviewable pre-mutation artifact |
| PRD-105 (TDAG) | **Not a dependency** | Orthogonal. See §2.4. Neither blocks the other; they may compose later. |
| PRD-078 (HITL tool approval) | **Not a dependency** | Adjacent. Per-call, synchronous, TTY-bound; §2.2 explains why it does not solve this. |

---

## 14. Open Questions

| # | Question | Owner | Resolution Target |
|---|----------|-------|-------------------|
| OQ-1 | Should `plan.default_mode: plan` be the shipped default for a fresh install? It is the safer posture and matches Cline's most-praised behaviour, but it changes what `tag run --tools` does for existing users. Proposal: keep `act` as the global default, ship a `planner` example profile with `plan`, and document the one-line config change. | Product | Before implementation |
| OQ-2 | Should `--mode auto` re-plan on rejection ("here's why it's wrong") rather than exiting? Attractive, but it turns a two-phase feature into an interactive session and starts overlapping `tag shell`. | Product | Defer to v2 |
| OQ-3 | Is `denied_attempts > 0` a warning or a failure? It reliably means the model misunderstood the mode. Proposal: warn at ≥1, truncate the phase at ≥5. | Engineering | During implementation |
| OQ-4 | Should the plan phase force a cheaper model by default? PRD-042 (`tag split`) already owns architect/editor separation; duplicating it here would be a second routing mechanism. Proposal: no — document `--profile planner`. | Arch | Before implementation |
| OQ-5 | Should MCP-bridged tools be classifiable as read-only via config (`plan.extra_readonly_tools`)? The config key is in §6.6, but it is operator-asserted safety with no verification. Ship it with a loud doc warning, or omit it in v1? | Security | Before implementation |
| OQ-6 | Should `files_touched` extraction use provider structured output (`invopop/jsonschema`) or a Markdown convention? Structured output is more reliable but not uniformly supported across the four adapters. | Engineering | During implementation |
| OQ-7 | Should `tag runs` gain a `--mode plan` filter and a `tag runs plans` listing, or is that better as a `tag plan list` — which would collide with PRD-105's namespace? Current answer: extend `tag runs`, avoid the collision entirely. | Product | Before v1 |

---

## 15. Complexity and Timeline

**Total Estimated Effort:** S-M (1-2 weeks, 1 engineer)

### Phase 1 — Classification and rules (Days 1-3)
- `internal/tool/mutation.go`: `Mode`, `readOnlyTools`, `Mutating`, `PlanModeRules`
- `permission.Guard.WithRules` copy-constructor
- Exhaustiveness, only-deny and non-widening property tests
- Deliverable: plan-mode rules provably narrowing-only

### Phase 2 — Plan artifact (Days 4-6)
- `internal/agent/plan.go`: struct, marshal/unmarshal, body-scoped SHA-256, `files_touched` extraction
- `plans` table; additive `runs.mode`/`runs.plan_id` via the self-healing PRAGMA pattern
- Atomic write; security scan before write
- Deliverable: round-trip and tamper-detection tests pass

### Phase 3 — Mode wiring and single-phase modes (Days 7-9)
- `modeFlags`; `--mode plan` on `run`/`shell`/`ci`/`loop`
- Mode-aware system prompt; `denied_attempts`; phase spans
- Deliverable: AC-01, AC-02, AC-03 pass

### Phase 4 — `--from-plan` and `--mode auto` (Days 10-12)
- Plan verification and injection; act-run linkage
- `auto` orchestration with `StdioInteractive` gating and pty tests
- Deliverable: AC-05 through AC-09 pass

### Phase 5 — Headless surfaces and polish (Days 13-14)
- `--mode plan` on `queue worker`, `dag run --execute`, `cron run --execute`, `issue-solve`, `agentic-ci`
- `--json` parity, offline CI job, regression test against the pre-feature build
- Deliverable: all 13 AC items pass

---

*PRD-129 authored for TAG. Status: Proposed — not built. Distinct from PRD-105 (`tag plan decompose`); see §2.4.*
