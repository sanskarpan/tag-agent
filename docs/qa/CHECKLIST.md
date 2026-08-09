# TAG QA Checklist

The brief this came from was written for a web application — routes, modals, viewports, Playwright. TAG is a CLI harness that also runs four HTTP servers. This checklist translates the intent rather than pretending the shapes match, and says which is which.

| web concept | TAG equivalent |
|---|---|
| route | command path (`tag swarm run`) |
| page render | the command starts, prints, and exits |
| button / form control | flag, subcommand, positional argument |
| navigation | command → command handoff via the store (`run` → `runs show`) |
| deep link / direct URL | invoking a subcommand cold, with no prior state |
| refresh | re-running a command against existing state |
| protected route | a command gated by the permission model |
| loading state | progress output while work is in flight |
| empty state | the command with no data (`tag runs` on a fresh home) |
| toast / notification | stderr notes and the exit code |
| API endpoint | genuinely an API endpoint — these apply literally |

The literal web parts apply to `tag serve`, `tag devui`, `tag gateway` and `tag webhook listen`. Everything else is translated.

---

## A. Per-command checks

For every command path in `INVENTORY-2026-08-03.md`:

- [ ] `--help` exits 0 and prints usage (catches `flag redefined`, nil-map panics at init, broken registration)
- [ ] Runs against a **fresh** `TAG_HOME` — the first-run path, which developer machines never exercise
- [ ] Runs against an **existing** `TAG_HOME` — including one created by the *other* distribution
- [ ] Empty state: no data, no config, no prior runs
- [ ] Invalid input: bad flag values, out-of-range numbers, nonexistent files, malformed JSON/YAML
- [ ] Missing required arguments
- [ ] Exit code matches the documented contract (0 ok / 1 failed / 2 usage / 3 findings / 4 aborted / 130 interrupted)
- [ ] `--json` where supported: parseable on **both** success and failure, `[]` not `null`, stdout uncontaminated by warnings
- [ ] Interruption (SIGTERM/SIGINT) leaves no row stranded in a non-terminal state
- [ ] Offline: works, or says clearly that it cannot and why
- [ ] Nothing is reported as succeeded that did not succeed

## B. HTTP surfaces (the literal web checks)

For `serve`, `devui`, `gateway`, `webhook listen`:

- [ ] Server starts, binds, and `/health` responds
- [ ] Every endpoint returns valid JSON with the right content type
- [ ] Correct status codes: 200 / 400 / 401 / 404 / 405 / 409 / 500
- [ ] Unauthenticated request to a protected endpoint is rejected
- [ ] Malformed body, wrong method, oversized body, missing headers
- [ ] SSE streams open, deliver, and terminate cleanly; client disconnect does not leak a goroutine
- [ ] Empty state: every `/api/*` on a fresh home
- [ ] The dashboard HTML renders, assets load, no console errors
- [ ] Concurrent requests do not corrupt state
- [ ] Server shuts down cleanly on signal

## C. End-to-end journeys

- [ ] **First run**: install → `bootstrap`/`setup` → `doctor` → first `run` → inspect via `runs`/`trace`
- [ ] **Queue lifecycle**: `queue add` → `queue list` → `worker` → completion → `runs show` → persistence across restart
- [ ] **Loop**: `loop start` → `loop list`/`status` → approve/deny from another process → terminal state → exit code
- [ ] **Swarm**: `swarm run` → `swarm list`/`status`/`results` → `swarm abort` on a live run
- [ ] **Agentic CI**: SARIF in → `fix-vuln` → exit code → `review` → `flaky-fix` → generated workflow is valid and invocable
- [ ] **Documents**: `doc check` → `doc read` → `read_document` through the agent loop
- [ ] **Cross-distribution**: Python writes state, Go reads it, and the reverse
- [ ] **Recovery**: a failed API call → error state → retry → success

## D. Cross-cutting

- [ ] Permission model: deny rules hold for path *and* command subjects; `ask` in a non-TTY denies rather than hanging
- [ ] Sandbox: the isolation claimed is the isolation achieved, and a policy that cannot be enforced fails closed
- [ ] Secrets never reach logs, audit rows, traces, or `--json`
- [ ] Config: missing, malformed, and partially-valid all fail loudly rather than half-loading
- [ ] Concurrency: two processes against one `TAG_HOME` do not corrupt or starve each other
- [ ] Schema drift: an old `TAG_HOME` is migrated, not crashed on

## E. Standard of proof

- A command is **not** "working" because it renders help or exits 0 once.
- Verify by **running**, in an isolated `TAG_HOME`, and check the resulting state — not by reading the code.
- Capture exit codes with a redirect, never through a pipe (`cmd >out 2>err; echo $?` — a pipe returns the *last* command's status and has produced wrong conclusions in this repo before).
- Anything asynchronous gets more than one run.
