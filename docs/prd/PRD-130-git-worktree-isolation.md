# PRD-130: Git Worktree Isolation for Parallel Agents (`tag worktree`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD is Go-native from the start — there is no Python precursor to re-frame.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (1-2 weeks)
**Category:** Execution Isolation & Parallel Orchestration
**Affects:** `internal/worktree` (new package), `internal/worker` (per-job working directory — `runJob` currently has none), `internal/tool` (`Options.Root` becomes per-job rather than process-wide), `internal/cli` (new `worktree` command group; `--worktree` flag on `run`, `queue worker`, `dag run --execute`, `cron run --execute`, `issue-solve`, `swe-solve`, `agentic-ci`), `internal/store` (`worktrees` table; `queue_jobs.worktree_path` column), `internal/permission` (path rules resolved against the job's worktree root), `internal/paths` (`~/.tag/worktrees/` layout)
**Depends on:** PRD-008 (background task queue — `internal/worker` is the primary consumer), PRD-033 (dependency-aware task queue — DAG jobs are the case where collision is guaranteed rather than possible), PRD-021 (agent loop / autonomous mode — the loop that runs inside a worktree), PRD-022 (cron / scheduled agents — note the file `PRD-022-cron-scheduled-agents.md` carries a stale `# PRD-021:` heading), PRD-013 (distributed agent tracing & observability — worktree identity as a span attribute), PRD-024 (repo-map / workspace context — the repo map must be built per worktree, not once per process; note `PRD-024-repo-map-workspace-context.md` also carries a stale `# PRD-021:` heading), PRD-038 (diff-aware context injection — diffs are computed against the worktree's own HEAD)
**Supersedes in scope:** the `--worktree` flag described in PRD-055 §FR-18 (`tag issue-solve --worktree`), which is a single-command, single-consumer implementation of the same idea. This PRD generalizes it into shared infrastructure; PRD-055's flag becomes a consumer of `internal/worktree` rather than its own implementation. **PRD-055 is not otherwise changed or blocked by this PRD.**
**Composes with:** the shipped tool-permission model in `internal/permission` (no PRD)
**Inspired by:** Claude Code `isolation: 'worktree'`, Kilo Code Agent Manager (worktree-per-agent), Cline, Emdash, Hermes

---

## 1. Overview

TAG-Go ships a genuinely rare execution stack. The July 2026 competitive audit is emphatic about it: `queue worker` + `dag run --execute` + `cron daemon` + HMAC-verified `webhook` form a closed local automation loop, **no surveyed peer ships a declarative task DAG at all**, and the scheduled/event-driven agent capability that competitors sell as a hosted product (Devin Automations, Claude Code Routines, Amp Orbs, Warp Oz, KiloClaw, Cursor Automations) runs locally in TAG's 19 MB binary for free.

That stack has one structural flaw, and it is only visible when you look at what parallel jobs actually share. `internal/worker`'s `runJob` builds an agent loop and calls `tool.Register(reg, topts)` with `tool.DefaultOptions()` — whose `Root` field is empty, which `resolvePath` resolves to `os.Getwd()`. Every job in a drain therefore operates on **the same working directory**, which is the directory the worker process happened to be started in. `bash` runs there. `write_file` writes there. Two DAG nodes that both edit `src/auth/session.go` are not two isolated agents; they are two writers racing on one file. The DAG's dependency edges order *job dispatch*, not filesystem access, so any two nodes the DAG declares independent — exactly the ones the scheduler will run concurrently — are precisely the ones with no protection.

Git worktrees are the field's answer. `git worktree add` gives each agent its own checkout of the same repository — its own working tree, its own index, its own `HEAD`, its own branch — backed by one shared object database. Creation is fast (no re-clone, no object copy), disk cost is roughly one working copy, and cleanup is a single `git worktree remove`. Claude Code exposes it as `isolation: 'worktree'` on its Agent tool; Kilo Code's Agent Manager gives every agent a tab and a worktree; Cline, Emdash and Hermes all ship variants. The audit rates it "🟡 Medium, consolidating fast."

This PRD introduces `internal/worktree` — creation, leasing, garbage collection, safety checks — plus a `tag worktree` command group for direct management, plus a `--worktree` flag threaded through every execution path. The design constraint that shapes everything is that **the worktree must become the tool root, not merely the shell's `cd` target.** `tool.Options.Root` is what `resolvePath` confines file tools to, with a lexical `..` guard followed by a symlink guard that walks up to the deepest existing ancestor and rejects any escape. Setting `Root` to the worktree path means isolation and path confinement become the same mechanism, and it means a job cannot reach out of its worktree into a sibling's — which is the property that makes parallelism actually safe rather than merely tidy. A worktree that only changed `bash`'s `cwd` would leave `write_file` pointed at the shared tree and would be worse than useless, because it would look isolated.

The second design constraint is that this must degrade honestly. Not every repository is a git repository. Not every job is a code task. `--worktree` in a non-git directory must fail with a clear message, not silently fall back to the shared tree — silent fallback would mean an operator believes their parallel jobs are isolated when they are not, which is the failure mode the audit praises TAG for otherwise avoiding.

---

## 2. Problem Statement

### 2.1 Parallel jobs share one working directory — verified in the code, not assumed

`internal/worker/runJob` constructs its tool options as `topts := tool.DefaultOptions()`, sets `topts.Guard`, and calls `tool.Register`. `DefaultOptions` returns `Options{BashTimeout: 30 * time.Second, MaxReadBytes: 256 * 1024}` — `Root` is the zero value. `resolvePath` then does `root, _ = os.Getwd()`. There is no per-job working directory anywhere in the worker, and no field on `jobRow` that could carry one. Every job in a drain, and every job across concurrent drains, is confined to the *same* root: the worker process's cwd.

The consequence is that TAG's most differentiated capability has an undefended interior. `tag dag run --execute` walks a real dependency chain and dispatches independent nodes — and independent nodes are, by construction, the ones with no ordering between their writes. A DAG whose whole value proposition is "these three refactors don't depend on each other, run them together" produces three agents editing one checkout.

### 2.2 Git state is process-global, so even correct file isolation would be insufficient

Even if each job had its own directory, agents doing repository work run `git checkout -b`, `git commit`, `git stash`, `git rebase`. Branch and index state belong to the repository, not to a directory. Two agents on one checkout will trample each other's branch even with perfect file separation. A worktree is the only mechanism that gives each agent its own `HEAD` and index while sharing the object database — which is why the entire field converged on it rather than on "just use different directories".

### 2.3 The scheduled/triggered paths are where this hurts most

The audit's second-strongest finding is that TAG's local cron + webhook + queue stack is rarer than it looks. But an unattended overnight cron job that mutates a developer's working tree — the tree they left dirty when they went home — is not a feature anyone wants twice. Without isolation, the honest operating advice for `cron run --execute` with `--tools` is "point it at a scratch clone you maintain by hand", which is exactly the manual toil the automation was supposed to remove. Worktree isolation is what makes the automation stack usable on a repository someone is actively working in.

### 2.4 The capability exists in the repository, scoped to one command, and is about to be duplicated

PRD-055 (`tag issue-solve`) specifies `--worktree` at `~/.tag/worktrees/<run-id>/` in FR-18, with tests in its §11 and AC-12. That is the right idea in the wrong scope: as written it is an implementation detail of one command. Every other execution path needs the same thing, and if each grows its own version there will be several path layouts, several cleanup policies, and several sets of safety checks — some of them wrong. Building it once, as `internal/worktree`, and having `issue-solve` consume it, is strictly cheaper than building it twice.

### 2.5 There is no lifecycle owner, so isolation without GC is a disk leak

A worktree per job on a busy queue is hundreds of working copies. Without a lease model, a registry, an idle policy and a `gc` command, "isolation" becomes "the disk fills up on Thursday". Crashed workers leave orphaned worktrees that `git worktree list` still reports and that hold branch references. The lifecycle is not a nice-to-have around the edge of this feature; it is half the feature.

---

## 3. Goals and Non-Goals

### Goals

| # | Goal |
|---|------|
| G1 | `internal/worktree` provides `Create`, `Lease`, `Release`, `Remove`, `List`, `GC` over `git worktree`, with a SQLite-backed registry that survives worker crashes. |
| G2 | `--worktree` on `tag run`, `queue worker`, `dag run --execute`, `cron run --execute`, `issue-solve`, `swe-solve` and `agentic-ci` runs each job in its own worktree. |
| G3 | The worktree path becomes `tool.Options.Root`, so file-tool confinement and job isolation are the same mechanism — a job structurally cannot reach a sibling's tree. |
| G4 | `bash` executes with `Dir` set to the worktree, so shell commands and file tools agree on where "here" is. |
| G5 | `internal/worker` gains a per-job working directory: `Options.WorkdirFor(job) string` plus a `queue_jobs.worktree_path` column, closing the shared-cwd hole in `runJob`. |
| G6 | Concurrency is bounded and enforced: `--max-worktrees N` caps live worktrees; a drain that would exceed it waits rather than creating an N+1th. |
| G7 | `tag worktree list/create/remove/gc/prune` gives direct management, each with `--json`. |
| G8 | Lifecycle: leases with TTL, idle reaping, an explicit `gc`, and startup reconciliation between the registry, `git worktree list`, and the filesystem — the three can disagree after a crash and reconciliation must be deterministic. |
| G9 | Honest degradation: `--worktree` outside a git repository, in a bare repo, or with a `git` older than 2.5 fails with a specific actionable error and a non-zero exit. It never silently falls back to the shared tree. |
| G10 | Results are recoverable: `tag worktree diff <id>` and `--keep-on-failure` mean a failed job's work is inspectable rather than deleted. |
| G11 | Permission path rules resolve against the job's worktree root, so `--allow-tool write_file:src/**` means the same thing inside a worktree as outside it. |
| G12 | Fully exercisable offline: worktree operations are local `git`; no network, no API key, `echo` provider throughout. |

### Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Container, VM or OS-level isolation. A worktree isolates the *working tree*; `bash` still runs as the user with full host access. That is `internal/sandbox`'s job (PRD-028), and the audit's finding that `tag sandbox` does not currently sandbox is a separate, higher-severity problem this PRD does not address or compensate for. |
| NG2 | Automatic merging of parallel results. Worktrees produce branches; merging them is `git`'s job and the operator's decision. |
| NG3 | Conflict detection or resolution between concurrent worktrees. Two agents editing the same file in two worktrees produce two branches that conflict on merge, exactly as two humans would. |
| NG4 | Non-git VCS. `jj`, Mercurial and Perforce are out of scope. The abstraction is deliberately `git worktree`, not a generic VCS interface — a fake generality here would cost more than it buys. |
| NG5 | Remote or distributed worktrees. Local filesystem only; cross-machine is PRD-088's territory. |
| NG6 | A worktree-per-*session* model for `tag shell`. v1 is per-job/per-run. Interactive sessions are single-agent and the shared tree is what the user wants. |
| NG7 | Copying untracked files or `.env` into worktrees by default. A fresh worktree has only tracked content. `--copy-untracked` exists as an explicit opt-in with a warning, because copying untracked files into N worktrees is a credential-fan-out risk. |
| NG8 | Porting to the Python edition. Go-native. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Isolation correctness | 100 concurrent jobs each writing the same relative path produce 100 distinct file contents, zero interference | Stress integration test |
| Escape resistance | 0 of N generated traversal attempts (`../`, absolute paths, symlinks to a sibling worktree) succeed | Fuzz test against `resolvePath` with a worktree root |
| Creation latency | `worktree.Create` on a 100 MB repository completes in < 500 ms (p95) | Benchmark against a generated fixture repo |
| Disk bound | Live worktrees never exceed `--max-worktrees`; GC reclaims 100% of released worktrees within one interval | Integration test with a low cap |
| Crash recovery | After `SIGKILL` of a worker mid-job, `tag worktree gc` reconciles registry/git/filesystem with zero orphans and zero false removals | Integration test with a real kill |
| Honest failure | 100% of `--worktree` invocations outside a git repo exit non-zero with an actionable message and create nothing | Integration test |
| No regression | Without `--worktree`, `runJob` behaviour and the resolved tool root are byte-identical to today | Regression test |
| Offline honesty | Every `tag worktree` subcommand runs with egress blackholed and no keys | Offline CI job |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag dag run refactor --execute --worktree` | three independent refactors run in parallel without racing on my checkout |
| U2 | Developer | keep working in my editor while `tag queue worker --worktree` drains jobs | background agents never touch the tree I am editing |
| U3 | Platform engineer | schedule `tag cron` with `--worktree` overnight | a scheduled agent cannot dirty a developer's working tree |
| U4 | Developer | run `tag worktree list` | I can see what agents are holding what, and on which branch |
| U5 | Developer | run `tag worktree diff job-8f21` after a job finishes | I can review the change before merging the branch |
| U6 | Developer | keep a failed job's worktree via `--keep-on-failure` | I can debug the failure in situ instead of re-running it |
| U7 | Platform engineer | cap live worktrees with `--max-worktrees 8` | a runaway queue cannot fill the disk |
| U8 | Platform engineer | run `tag worktree gc` from cron | orphans from crashed workers are reclaimed automatically |
| U9 | Security-conscious operator | confirm a job cannot write outside its worktree | parallel isolation is a real boundary, not a convention |
| U10 | Operator | get a clear error when I pass `--worktree` in a non-git directory | I never believe I have isolation I do not have |
| U11 | CI engineer | run `tag agentic-ci --worktree` on a shared runner | concurrent CI agents on one checkout do not corrupt each other |
| U12 | Developer | have `tag issue-solve --worktree` use the same machinery as everything else | there is one worktree layout and one cleanup policy, not several |

---

## 6. Proposed CLI Surface

**Collision check.** `tag --help` on the built binary lists no `worktree` command; the `w` neighbourhood is `web`, `webhook`, `workspace`. No existing PRD claims `tag worktree` — the only prior mention of worktrees anywhere in `docs/prd/` is PRD-055's `--worktree` *flag* on `issue-solve` (§FR-18, §11, AC-12), which this PRD generalizes rather than collides with. The `--worktree` flag name is unused on all target commands (verified against `tag run --help`, `tag queue worker --help`, `tag dag run --help`, `tag cron run --help`).

### 6.1 `tag worktree list`

```bash
tag worktree list [--all] [--stale] [--json]
```

```
ID            Branch                   Owner              Age     Status   Path
────────────────────────────────────────────────────────────────────────────────────────
wt-8f21c0d4   tag/job-8f21c0d4         queue:job-8f21     4m12s   leased   ~/.tag/worktrees/wt-8f21c0d4
wt-3ac91b02   tag/dag-refactor-n2      dag:refactor#n2    4m10s   leased   ~/.tag/worktrees/wt-3ac91b02
wt-77e0aa15   tag/cron-nightly-docs    cron:nightly-docs  2h03m   stale    ~/.tag/worktrees/wt-77e0aa15
wt-11bb9930   tag/issue-412            —                  6d      orphan   ~/.tag/worktrees/wt-11bb9930

4 worktrees · 2 leased · 1 stale · 1 orphan · 412 MB
```

`orphan` means present in git and/or on disk but with no live lease — the crashed-worker case.

### 6.2 `tag worktree create` / `remove`

```bash
tag worktree create [--branch NAME] [--from REF] [--repo PATH] [--json]
tag worktree remove ID [--force] [--keep-branch] [--json]
```

`create` is mostly a debugging and scripting affordance; the normal path is `--worktree` on an execution command. `remove` refuses a leased worktree without `--force`, and refuses one with uncommitted changes without `--force` — losing an agent's work to an over-eager cleanup is the failure mode to design against.

### 6.3 `tag worktree gc` / `prune`

```bash
tag worktree gc [--max-age 24h] [--max-count N] [--dry-run] [--json]
tag worktree prune [--json]
```

`gc` removes released worktrees older than `--max-age`, and if over `--max-count` removes the oldest released ones until under the cap. It **never** removes a leased worktree, and never one with uncommitted changes unless `--force`. `--dry-run` prints what would go.

`prune` is the reconciliation pass: it compares the `worktrees` registry, `git worktree list --porcelain`, and the filesystem, and repairs disagreements — registry rows with no directory are deleted, directories with no registry row are registered as orphans (never silently deleted), and `git worktree prune` is run for git's own stale administrative entries. Reconciliation is deliberately *conservative in the delete direction*: unknown state becomes an orphan for a human to look at, not a deletion.

### 6.4 `tag worktree diff`

```bash
tag worktree diff ID [--stat] [--name-only] [--json]
```

Runs `git diff` inside the worktree against its base ref. This is what makes `--keep-on-failure` useful and what lets `dag run --execute --worktree` be reviewed node by node.

### 6.5 Execution-path flags

Added to `tag run`, `tag queue worker`, `tag dag run`, `tag cron run`, `tag issue-solve`, `tag swe-solve`, `tag agentic-ci`:

| Flag | Default | Description |
|------|---------|-------------|
| `--worktree` | false | Run each job in its own git worktree |
| `--worktree-base REF` | current `HEAD` | Base ref for created worktrees |
| `--worktree-branch TPL` | `tag/{kind}-{id}` | Branch name template (`{kind}`, `{id}`, `{profile}`) |
| `--max-worktrees N` | `4` | Cap on simultaneously live worktrees |
| `--keep-worktree` | false | Do not remove on success |
| `--keep-on-failure` | true | Do not remove on failure (so the work is inspectable) |
| `--copy-untracked` | false | Copy untracked, non-ignored files into the worktree (warns; see NG7) |

### 6.6 Configuration

```yaml
worktree:
  enabled: false
  root: ~/.tag/worktrees
  base: HEAD
  branch_template: "tag/{kind}-{id}"
  max_count: 4
  max_age: 24h
  keep_on_failure: true
  gc_on_worker_start: true
```

---

## 7. Functional Requirements

| ID | Requirement | Acceptance Test |
|----|------------|-----------------|
| FR-01 | `worktree.Create(repo, opts)` shells out to `git worktree add -b <branch> <path> <base>` via `exec.CommandContext` with an explicit argv (never a shell string), and returns a `Worktree` with id, path, branch, base ref and repo root. | Integration test against a temp git repo. |
| FR-02 | Repository detection uses `git rev-parse --show-toplevel`. A non-git directory, a bare repository, or `git` < 2.5 produces a distinct, actionable error and creates nothing. | Table-driven integration test covering all three cases. |
| FR-03 | `--worktree` **never** silently falls back to the shared working tree. Any creation failure fails the job. | Integration test asserting a non-zero exit and an unchanged parent tree. |
| FR-04 | Worktree paths are `<worktree.root>/<id>` with `id` matching `^wt-[0-9a-f]{8}$`, derived from the job/run id. The path is validated to be inside `worktree.root` before any filesystem call. | Unit test with a hostile id. |
| FR-05 | Branch names are rendered from the template and validated with `git check-ref-format --branch`; an invalid name fails before creation. | Unit test with `../evil` as a profile name. |
| FR-06 | A created worktree is registered in `worktrees` with status `leased`, owner, lease expiry, and `created_at`, in the same transaction that records the job association. | Integration test. |
| FR-07 | `tool.Options.Root` is set to the worktree path for every tool registered for that job, so `resolvePath` confines file tools to it. | Unit test: `write_file ../escape.txt` from inside a worktree is refused with "escapes the tool root". |
| FR-08 | The `bash` tool's `Dir` is the worktree path, so shell and file tools agree on the working directory. | Integration test asserting `pwd` output. |
| FR-09 | A symlink inside a worktree pointing at a sibling worktree, or at the parent repository, is rejected by the existing symlink guard in `resolvePath`. | Fuzz test (Success-Metrics escape row). |
| FR-10 | `worker.Options` gains `WorkdirFor func(jobID, profile string) (string, func(), error)` returning the directory and a release closure; `runJob` uses it to set `topts.Root`. A nil `WorkdirFor` reproduces today's behaviour exactly. | Regression test asserting byte-identical behaviour when nil. |
| FR-11 | `queue_jobs` gains a `worktree_path` column via the same self-healing `PRAGMA table_info` pattern `ensureResultColumn` already uses, so an existing database upgrades without migration ceremony. | Integration test against a pre-existing DB file. |
| FR-12 | A drain that would exceed `--max-worktrees` blocks on a semaphore until one is released, rather than creating an N+1th or failing the job. | Concurrency test with cap 2 and 10 jobs. |
| FR-13 | On success, the worktree is removed unless `--keep-worktree`; the branch is retained by default so committed work survives removal. | Integration test asserting the branch exists after removal. |
| FR-14 | On failure, the worktree is retained by default (`--keep-on-failure`, default true) and its id is printed in the error output. | Integration test. |
| FR-15 | Removal refuses a worktree with uncommitted changes unless `--force`, and the refusal names the dirty files. | Integration test. |
| FR-16 | `tag worktree gc` never removes a `leased` worktree whose lease has not expired. | Unit test with a live lease. |
| FR-17 | Leases carry a TTL (default 2× the job timeout, min 1h). An expired lease makes a worktree `stale` and GC-eligible; it does not by itself delete anything. | Unit test with a clock injection. |
| FR-18 | `tag worktree prune` reconciles registry ↔ `git worktree list --porcelain` ↔ filesystem. Unknown directories become registered orphans; they are never silently deleted. | Integration test with hand-corrupted state in all three directions. |
| FR-19 | Worker startup runs `prune` when `worktree.gc_on_worker_start` is true, so a crashed predecessor's orphans are surfaced immediately. | Integration test with a simulated crash. |
| FR-20 | Permission path rules (`--allow-tool write_file:src/**`) are matched against paths already resolved relative to the worktree root, so a rule means the same thing inside and outside a worktree. | Integration test comparing decisions in both contexts. |
| FR-21 | Worktree id and branch are emitted as span attributes (`tag.worktree.id`, `tag.worktree.branch`) on the job span. | Integration test querying `spans`. |
| FR-22 | `--copy-untracked` copies untracked, non-`.gitignore`d files only, prints the count and a warning, and never copies a path matched by `permission.CredentialRules()`. | Unit test with a `.env` and an untracked source file present. |
| FR-23 | The repo map (PRD-024) and diff context (PRD-038) are computed against the worktree root when one is active, not against the process cwd. | Integration test asserting the repo map contains a worktree-only file. |
| FR-24 | Without `--worktree`, no worktree is created, no registry row is written, and the resolved tool root is `os.Getwd()` exactly as today. | Regression test. |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|------------|--------|
| NFR-01 | `Create` on a 100 MB repo completes in < 500 ms p95. | Benchmark against a generated fixture |
| NFR-02 | Disk overhead per worktree is one working copy; the object database is shared (no `--no-hardlinks` clone path is ever used). | Disk-usage assertion in an integration test |
| NFR-03 | No new direct Go modules. `git` is invoked via `os/exec` with explicit argv and `Setpgid` so a cancelled job's process group dies. `CGO_ENABLED=0` unchanged. | `go mod graph` diff empty |
| NFR-04 | Every `tag worktree` subcommand works offline with no keys. | Offline CI job |
| NFR-05 | All subcommands support `--json` with a stable schema. | `jsonparity` test |
| NFR-06 | Registry writes honour the single-writer + WAL contract; `Lease` is atomic (`BEGIN IMMEDIATE` + conditional update) so two concurrent workers cannot lease the same worktree. | Concurrency integration test |
| NFR-07 | `git` invocations are bounded by `context.Context` with a default 60 s timeout; a hung `git` fails the job with a timeout error rather than blocking a drain forever. | Test with a stubbed slow `git` on `PATH` |
| NFR-08 | `internal/worktree` does not import `internal/cli` or `internal/agent`; it is a leaf consumed by both. | Import-graph architecture test |
| NFR-09 | `go vet`, `golangci-lint`, `staticcheck` clean; `gofmt` clean. | CI gate |
| NFR-10 | Worktree creation and removal are idempotent: creating an existing id returns the existing worktree; removing a missing one succeeds with a note. | Unit test |

---

## 9. Technical Design

### 9.1 New and Modified Files

| File | Change | Description |
|---|---|---|
| `internal/worktree/worktree.go` | **New** | `Worktree`, `Manager`; `Create`, `Lease`, `Release`, `Remove`, `List`, `Diff`. |
| `internal/worktree/git.go` | **New** | Thin `os/exec` wrappers: `RepoRoot`, `WorktreeAdd`, `WorktreeRemove`, `WorktreeListPorcelain`, `CheckRefFormat`, `Version`. Explicit argv, `CommandContext`, `Setpgid`. |
| `internal/worktree/gc.go` | **New** | `GC`, `Prune`, three-way reconciliation, lease expiry. |
| `internal/worktree/store.go` | **New** | Registry CRUD against the shared `internal/store` DB. |
| `internal/worker/worker.go` | **Extend** | `Options.WorkdirFor`; `runJob` sets `topts.Root` and the bash `Dir`; `queue_jobs.worktree_path` via the self-healing column pattern. |
| `internal/tool/tools.go` | **Extend** | `Options.Dir` for the bash working directory (today `Root` only affects file tools; bash inherits the process cwd — this closes that inconsistency). |
| `internal/cli/worktree.go` | **New** | `tag worktree` group. |
| `internal/cli/helpers.go` | **Extend** | `worktreeFlags` binder, mirroring `permFlags`/`modeFlags`. |
| `internal/store/schema` | **Extend** | `worktrees` table. |

### 9.2 SQLite DDL

```sql
CREATE TABLE IF NOT EXISTS worktrees (
  id            TEXT PRIMARY KEY,          -- "wt-8f21c0d4"
  repo_root     TEXT NOT NULL,             -- git rev-parse --show-toplevel
  path          TEXT NOT NULL UNIQUE,      -- ~/.tag/worktrees/wt-8f21c0d4
  branch        TEXT NOT NULL,
  base_ref      TEXT NOT NULL,
  status        TEXT NOT NULL,             -- leased|released|stale|orphan
  owner_kind    TEXT,                      -- run|queue|dag|cron|issue-solve|manual
  owner_id      TEXT,
  lease_expires TEXT,                      -- RFC3339; NULL when released
  keep          INTEGER NOT NULL DEFAULT 0,
  last_error    TEXT,
  created_at    TEXT NOT NULL,
  released_at   TEXT
);

CREATE INDEX IF NOT EXISTS idx_wt_status ON worktrees(status, created_at);
CREATE INDEX IF NOT EXISTS idx_wt_owner  ON worktrees(owner_kind, owner_id);

-- Additive, applied by the same PRAGMA table_info self-healing pattern the
-- worker already uses for queue_jobs.result:
--   ALTER TABLE queue_jobs ADD COLUMN worktree_path TEXT;
```

### 9.3 Core Types

```go
// internal/worktree/worktree.go
package worktree

type Status string

const (
	StatusLeased   Status = "leased"
	StatusReleased Status = "released"
	StatusStale    Status = "stale"  // lease expired, not yet reclaimed
	StatusOrphan   Status = "orphan" // on disk / in git, no registry lease
)

type Worktree struct {
	ID           string
	RepoRoot     string
	Path         string
	Branch       string
	BaseRef      string
	Status       Status
	OwnerKind    string
	OwnerID      string
	LeaseExpires time.Time
	Keep         bool
	CreatedAt    time.Time
}

// Manager owns the worktree lifecycle for one repository root.
type Manager struct {
	DB       *sql.DB
	Root     string        // ~/.tag/worktrees
	MaxCount int           // hard cap on live worktrees
	GitTimeout time.Duration
	sem      chan struct{} // bounded concurrency; len == MaxCount
}

type CreateOpts struct {
	BaseRef        string
	BranchTemplate string
	OwnerKind      string
	OwnerID        string
	LeaseTTL       time.Duration
	CopyUntracked  bool
}
```

### 9.4 Acquire / Release

```go
// Acquire blocks until a worktree slot is free, creates the worktree, registers
// the lease, and returns a release closure. The closure is idempotent and safe
// to defer — a job that panics still releases its slot.
func (m *Manager) Acquire(ctx context.Context, id string, o CreateOpts) (*Worktree, func(error), error) {
	select {
	case m.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	wt, err := m.create(ctx, id, o) // git worktree add + registry insert
	if err != nil {
		<-m.sem
		return nil, nil, err
	}
	var once sync.Once
	release := func(jobErr error) {
		once.Do(func() {
			defer func() { <-m.sem }()
			// Retain on failure so the work is inspectable (FR-14).
			if jobErr != nil && m.KeepOnFailure {
				_ = m.markReleased(wt.ID, jobErr)
				return
			}
			if wt.Keep {
				_ = m.markReleased(wt.ID, nil)
				return
			}
			_ = m.Remove(context.WithoutCancel(ctx), wt.ID, false)
		})
	}
	return wt, release, nil
}
```

`context.WithoutCancel` on the removal path matters: a job cancelled by `Ctrl-C` or a timeout must still get its worktree cleaned up, and using the already-cancelled context would leak one on every interrupt.

### 9.5 Worker Integration

```go
// internal/worker/worker.go — Options gains:
//
//   // WorkdirFor supplies a per-job working directory. Nil = today's behaviour
//   // (every job shares the worker process cwd), preserved exactly so this
//   // change is opt-in and the no-flag path is provably unchanged.
//   WorkdirFor func(ctx context.Context, jobID, profile string) (dir string, release func(error), err error)

func runJob(ctx context.Context, opts Options, j jobRow) (string, error) {
	dir := ""
	if opts.WorkdirFor != nil {
		d, release, err := opts.WorkdirFor(ctx, j.id, j.profile)
		if err != nil {
			return "", fmt.Errorf("worktree for job %s: %w", j.id, err)
		}
		dir = d
		var jobErr error
		defer func() { release(jobErr) }()
		// … jobErr assigned from the loop result below
	}
	loop := &agent.Loop{Provider: opts.Provider}
	if opts.WithTools {
		reg := agent.NewRegistry()
		topts := tool.DefaultOptions()
		topts.Guard = opts.Guard
		topts.Root = dir // "" preserves the existing os.Getwd() behaviour
		topts.Dir = dir
		tool.Register(reg, topts)
		loop.Tools = reg
	}
	// …
}
```

`topts.Root = dir` is the whole isolation mechanism, and it is one line because `resolvePath` was already written correctly — the lexical `..` guard and the walk-up symlink guard were built for path confinement and turn out to be exactly what per-job isolation needs. The design is cheap precisely because that groundwork exists.

### 9.6 Three-Way Reconciliation

There are three sources of truth and after a crash they disagree:

| Registry | `git worktree list` | Filesystem | Action |
|---|---|---|---|
| leased, unexpired | present | present | leave alone |
| leased, expired | present | present | mark `stale`; GC-eligible |
| released | present | present | remove if past `max_age` or over `max_count` |
| present | absent | absent | delete the registry row (already gone) |
| present | present | **absent** | `git worktree prune`, then delete the row |
| **absent** | present | present | register as `orphan` — surface it, never auto-delete |
| absent | absent | present | register as `orphan` — a stray directory under the worktree root |

The asymmetry is deliberate: unknown state is always resolved *toward keeping data*. An over-eager reconciler that deletes an unrecognized directory under `~/.tag/worktrees/` will eventually delete an agent's unmerged work, and that is a far worse outcome than a disk-usage warning.

### 9.7 Integration Points

| Package | Integration |
|---|---|
| `internal/worker` | `WorkdirFor` hook; per-job `Root`/`Dir`; `queue_jobs.worktree_path`. |
| `internal/tool` | `Options.Root` (existing) becomes per-job; new `Options.Dir` for bash cwd. |
| `internal/permission` | Unchanged. Path rules match against subjects already resolved relative to the worktree root, so `KindPath` semantics are preserved (FR-20). |
| `internal/store` | `worktrees` table; additive `queue_jobs` column. |
| `internal/cli` | `tag worktree` group; `worktreeFlags` on execution commands. |
| PRD-024 repo map | Built per worktree (FR-23). |
| PRD-038 diff context | Diffs against the worktree's own base ref. |
| PRD-013 tracing | `tag.worktree.id` / `tag.worktree.branch` span attributes. |
| PRD-055 `issue-solve` | Its `--worktree` flag is reimplemented on top of `internal/worktree`; behaviour and tests as specified in PRD-055 are preserved. |

---

## 10. Security Considerations

1. **A worktree is not a sandbox.** It isolates the working tree and git state. `bash` still runs as the invoking user with full host access, and the audit's finding that `tag sandbox`'s `restricted` backend reached the network and read `/etc/passwd` is a separate, higher-severity problem. This PRD must not be cited as mitigating it. Isolation of *state* and isolation of *execution* are different guarantees and the docs say which one this is.

2. **Path confinement is the isolation boundary and it is a tested one.** Setting `tool.Options.Root` to the worktree means the existing `resolvePath` guards apply: lexical `..` rejection before any filesystem access, then a symlink guard that walks up to the deepest existing ancestor and rejects anything resolving outside the root, failing closed on `EvalSymlinks` errors. FR-09 and the escape-resistance fuzz test target exactly the cross-worktree case.

3. **Branch and path injection.** Ids match `^wt-[0-9a-f]{8}$` and paths are validated to be under `worktree.root`. Branch names go through `git check-ref-format --branch`. All `git` invocation uses `exec.CommandContext` with explicit argv — never a shell string — so metacharacters in a profile name are inert. `Setpgid` ensures a cancelled job's `git` process group dies.

4. **Credential fan-out via `--copy-untracked`.** Copying untracked files into N worktrees multiplies any `.env` sitting in the tree by N. It is off by default, warns loudly, and refuses to copy any path matching `permission.CredentialRules()` — reusing the exact same pattern set that guards `read_file`, so there is one definition of "credential-shaped path" in the codebase rather than two that can drift.

5. **Destructive cleanup is the realistic data-loss vector.** Mitigations: removal refuses a dirty worktree without `--force`; `--keep-on-failure` defaults true; branches survive worktree removal; reconciliation resolves ambiguity toward retention (§9.6); `--dry-run` on `gc`.

6. **Disk exhaustion.** `--max-worktrees` is a blocking semaphore, not an error, so a large queue degrades to serialized execution rather than either failing or filling the disk. `gc --max-age`/`--max-count` bound the long tail.

7. **Lease races.** `Lease` uses `BEGIN IMMEDIATE` plus a conditional update, so two concurrent workers cannot both claim one worktree — the same single-writer discipline `internal/store` already enforces and `internal/worker`'s `claim` already relies on.

8. **Symlinked repository roots.** `RepoRoot` resolves symlinks before comparison (mirroring `resolvePath`'s `EvalSymlinks` handling of macOS `/tmp` → `/private/tmp`), so a symlinked repo path cannot produce a root mismatch that appears to be an escape or, worse, appears to be safe.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`internal/worktree/*_test.go`)

- `TestIDValidation` / `TestPathContainment` — hostile ids and paths rejected before any filesystem call.
- `TestBranchNameValidation` — `../evil`, spaces, empty.
- `TestLeaseExpiry` — injected clock; expired lease becomes `stale`.
- `TestAcquireSemaphoreBlocks` — cap 2, 5 goroutines; assert never more than 2 live.
- `TestReleaseIdempotent` — double release does not double-free the semaphore.
- `TestReleaseOnCancelledContext` — worktree still removed (the `WithoutCancel` path).
- `TestReconciliationMatrix` — table-driven over all seven rows of §9.6.
- `TestGCNeverRemovesLeased` / `TestRemoveRefusesDirty`.
- `TestCopyUntrackedSkipsCredentials` — `.env` present, not copied.

### 11.2 Integration Tests (`internal/cli/worktree_e2e_test.go`)

Each creates a real temp git repository (`git init`, one commit) plus a temp `TAG_HOME` and store.

- `TestCreateRemoveRoundTrip` — worktree exists, branch exists after removal.
- `TestNonGitDirFailsClearly` / `TestBareRepoFailsClearly` — non-zero exit, nothing created.
- `TestToolRootIsWorktree` — `write_file ../escape.txt` refused with "escapes the tool root".
- `TestBashCwdIsWorktree` — `pwd` output matches.
- `TestSiblingWorktreeSymlinkDenied` — symlink to a sibling rejected.
- `TestParallelJobsIsolated` — 100 concurrent jobs write the same relative path; assert 100 distinct contents.
- `TestDagExecuteWorktree` — independent DAG nodes get distinct worktrees.
- `TestCronExecuteWorktreeLeavesParentClean` — parent `git status` clean after a cron run.
- `TestWorkerCrashRecovery` — `SIGKILL` mid-job, then `prune`: orphan surfaced, nothing falsely deleted.
- `TestKeepOnFailureRetainsWorktree` — failed job's worktree survives and `tag worktree diff` shows its changes.
- `TestPermissionPathRuleInsideWorktree` — `--allow-tool write_file:src/**` behaves identically inside and outside.
- `TestNoWorktreeFlagUnchanged` — regression against today's resolved root.

### 11.3 Fuzz (`internal/worktree/fuzz_test.go`)

`go test -fuzz` over relative paths, `..` sequences, absolute paths, and symlink shapes against a worktree root; assert zero escapes (Success-Metrics escape row).

### 11.4 Benchmarks

- `BenchmarkCreate100MB` — < 500 ms p95 against a generated fixture repo.
- `BenchmarkPruneReconcile1000` — reconciliation over 1,000 registry rows.

---

## 12. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-01 | `tag dag run X --execute --worktree` runs independent nodes in distinct worktrees; the parent working tree is unchanged. | Integration test |
| AC-02 | 100 concurrent jobs writing the same relative path produce 100 distinct files. | Stress test |
| AC-03 | `write_file ../escape.txt` from inside a worktree is refused with "escapes the tool root". | Integration test |
| AC-04 | A symlink to a sibling worktree is rejected by the symlink guard. | Fuzz + integration test |
| AC-05 | `--worktree` in a non-git directory exits non-zero, prints an actionable message, and creates nothing. | Integration test |
| AC-06 | `--max-worktrees 2` with 10 queued jobs never exceeds 2 live worktrees and completes all 10. | Concurrency test |
| AC-07 | After `SIGKILL` of a worker, `tag worktree prune` reports the orphan and deletes nothing unexpected. | Integration test |
| AC-08 | `tag worktree remove` on a dirty worktree refuses without `--force` and names the dirty files. | Integration test |
| AC-09 | A failed job's worktree is retained and `tag worktree diff` shows its changes. | Integration test |
| AC-10 | `tag cron run X --execute --worktree` leaves the parent `git status` clean. | Integration test |
| AC-11 | Without `--worktree`, the resolved tool root and worker behaviour are byte-identical to the pre-feature build. | Regression test |
| AC-12 | Every `tag worktree` subcommand completes with egress blackholed and no API keys. | Offline CI job |
| AC-13 | `--copy-untracked` copies an untracked source file and does not copy `.env`. | Integration test |
| AC-14 | `tag worktree list --json` parses under `jq` with the documented schema. | CI `jq` test |

---

## 13. Dependencies

| Dependency | Type | Justification |
|---|---|---|
| `git` ≥ 2.5 | External runtime | `git worktree` was introduced in 2.5; version is checked and a clear error emitted below it |
| `os/exec` | Stdlib | `git` invocation with explicit argv + `Setpgid` |
| `modernc.org/sqlite` | Core (project driver) | `worktrees`; additive `queue_jobs` column |
| `github.com/spf13/cobra` | Core | `tag worktree` group |
| PRD-008 (background task queue) | Internal | Primary consumer; `internal/worker` is where the shared-cwd hole is |
| PRD-033 (dependency-aware task queue) | Internal | DAG parallelism is the case where collision is guaranteed |
| PRD-021 (agent loop) | Internal | The loop that runs inside a worktree |
| PRD-022 (cron / scheduled agents) | Internal | Unattended runs are the highest-risk consumer |
| PRD-013 (tracing) | Internal | Worktree span attributes |
| PRD-024 (repo map / workspace context) | Internal | Must be built per worktree |
| PRD-038 (diff-aware context injection) | Internal | Diffs against the worktree's base ref |
| PRD-055 (issue-to-PR loop) | Internal | Its `--worktree` flag becomes a consumer of this package |
| PRD-028 (sandbox) | **Not a dependency** | Different guarantee (execution isolation vs state isolation). Explicitly not compensated for here — see §10.1. |
| `internal/permission` (shipped, no PRD) | Internal | Path rules resolve against the worktree root |

---

## 14. Open Questions

| # | Question | Owner | Resolution Target |
|---|----------|-------|-------------------|
| OQ-1 | Should `--worktree` become the default for `queue worker`/`dag run --execute`/`cron run --execute` when the cwd is a git repo? It is the safe posture and matches Kilo's worktree-per-agent default, but it silently changes where existing automation writes. Proposal: opt-in in v1, warn when running with `--tools` in a git repo without it, revisit for v2. | Product | Before implementation |
| OQ-2 | Should branches be deleted with the worktree when the job produced no commits? Leaving empty `tag/*` branches clutters `git branch`. Proposal: delete only if the branch is unchanged from its base. | Engineering | During implementation |
| OQ-3 | What is the right `--max-worktrees` default? 4 matches the queue's practical parallelism, but a 2 GB monorepo × 4 is 8 GB. Should it be disk-aware rather than a fixed count? | Engineering | Empirical, during alpha |
| OQ-4 | Should a worktree be reusable across jobs (a pool) rather than created per job? A pool amortizes creation cost but leaks state between jobs, which defeats the point. Proposal: no pooling; revisit only if benchmarks show creation dominating. | Arch | Before implementation |
| OQ-5 | How should this interact with PRD-129 plan mode? A plan-mode run mutates nothing, so a worktree is arguably wasted — but `--mode auto` transitions to act mid-run. Proposal: create the worktree lazily at the first mutating call. | Engineering | After PRD-129 |
| OQ-6 | Should `tag worktree` expose `merge`/`land` helpers, or stay strictly out of the merge business (NG2)? Operators will ask. | Product | Defer to v2 |
| OQ-7 | Should the worktree root be repo-scoped (`~/.tag/worktrees/<repo-hash>/`) rather than flat? Flat is simpler; repo-scoped makes `gc` per-repo and avoids id collisions across repositories. | Engineering | Before implementation |

---

## 15. Complexity and Timeline

**Total Estimated Effort:** M (1-2 weeks, 1 engineer)

### Phase 1 — Git wrappers and safety (Days 1-3)
- `internal/worktree/git.go`: `RepoRoot`, `WorktreeAdd/Remove/ListPorcelain`, `CheckRefFormat`, `Version`, all with explicit argv, `CommandContext`, `Setpgid`
- Id/path/branch validation; non-git, bare-repo and old-git error paths
- Deliverable: create/remove round-trips against a temp repo; all three failure modes produce distinct errors

### Phase 2 — Registry and lifecycle (Days 4-6)
- `worktrees` DDL; `Acquire`/`Release` with the bounded semaphore and idempotent release
- Lease TTL, `stale` transition, `GC`, three-way `Prune`
- Deliverable: reconciliation matrix tests pass; crash-recovery test passes

### Phase 3 — Execution wiring (Days 7-9)
- `worker.Options.WorkdirFor`; `runJob` sets `topts.Root` and `topts.Dir`; `queue_jobs.worktree_path`
- `tool.Options.Dir` for bash cwd
- `--worktree` on `run`, `queue worker`, `dag run`, `cron run`
- Deliverable: AC-01 through AC-04 and AC-06 pass

### Phase 4 — CLI surface and consumers (Days 10-12)
- `tag worktree list/create/remove/gc/prune/diff` with `--json`
- Repo map and diff context per worktree; span attributes
- `issue-solve`/`swe-solve`/`agentic-ci` migrated onto `internal/worktree`
- Deliverable: AC-07 through AC-10, AC-14 pass

### Phase 5 — Hardening (Days 13-14)
- Escape fuzzing, `--copy-untracked` credential filtering, benchmarks, offline CI job
- Regression test proving the no-flag path is unchanged
- Deliverable: all 14 AC items pass; NFR targets met

---

*PRD-130 authored for TAG. Status: Proposed — not built. Generalizes the `--worktree` flag scoped to `tag issue-solve` in PRD-055 §FR-18 into shared infrastructure.*
