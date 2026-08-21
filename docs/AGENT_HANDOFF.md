# TAG — Agent Handoff

**Audience: an AI agent picking up work on this repository.** Read this before touching code. It is written to save you from the specific mistakes that have already been made here, several of them more than once.

Current: **`tag-agent` 0.13.0** (PyPI + npm) · **`0.14.0-go`** (Go harness) · `main` green · 0 open issues.

---

## 1. What this project is

Two distributions of one product, sharing one on-disk state directory (`$TAG_HOME`).

| | path | shape |
|---|---|---|
| **Python** | `src/tag/` | 45 modules, ~274 invocable command paths. Wraps the Hermes runtime. The original. |
| **Go harness** | `tag-go/` | 40 packages, ~232 invocable command paths. Native rewrite. CGO-free static binary. |

Both write the same SQLite store. Both ship `tag doc`, `tag queue`, `tag loop`, `tag swarm`, `tag agentic-ci`, and four HTTP servers (`serve`, `web`, `devui`, `gateway`, `webhook listen`).

**The Go harness is a port, not a clone.** It deliberately diverges where Python was wrong, and those divergences are documented in the code. Do not "restore parity" without reading the comment first — you will usually be reintroducing a bug.

Key directories:

```
docs/prd/            137 PRDs. PRD-133 (document ingestion) is the most recent worked example.
docs/qa/             INVENTORY (every command + endpoint) and CHECKLIST (the QA method).
docs/security/       Codex Security threat models + the E2E port-audit findings.
issues.md            The 2026-08-03 production-readiness audit: 56 entries + negative results.
.claude/skills/harness-security/   READ THIS. The failure taxonomy, grounded in real bugs here.
```

---

## 2. The one cultural invariant: no fabricated success

Everything else follows from this. **A command must never report success for work that failed or did not happen.**

This is not aspirational — it is the single most violated rule in the repo's history, and nearly every P0 found here is an instance of it:

- `swarm` returned the *un-executed synthesis prompt* as `final_output`, with `status: completed`.
- `eval-ci` scored the submission **receipt** (`status: queued`) and printed a green CI gate at 100% pass, with no model ever invoked.
- `security scan --max-files 0` walked zero files and printed `✓ No secrets found`, exit 0, persisting a `status='ok'` row.
- `read_file` handed the model ~100k tokens of deflate output for a PDF and exited 0.
- `queue worker` exited 0 with `1 claimed, 0 done, 1 failed`.
- `template export` said "secrets redacted" and exported `PRIVATE_KEY` in the clear.
- `plugin enable` wrote to a directory the runtime never reads and printed "Enabled".

Practical rules that fall out of it:

1. **Report the achieved guarantee, not the attempted one.** If you truncated, say so. If a page produced no text, say so. If the engine is absent, say absent — not "failed".
2. **Fail closed on a boundary value.** `0`, `-1`, empty, missing. When in doubt, refuse with exit 2.
3. **A name is a promise.** A flag called `--sandbox` that does not sandbox is worse than no flag.
4. **Refusing beats guessing.** A gate that cannot obtain the answer must fail, not pass.

---

## 3. Exit-code contract

| code | meaning |
|---|---|
| 0 | success |
| 1 | the run itself failed |
| 2 | usage error (bad flag value, missing arg, unparseable input) |
| 3 | ran fine, **found problems** — the gating outcome. Usually paired with `--exit-zero` |
| 4 | aborted (SIGTERM, deny, approval timeout) |
| 5 | partial (swarm only) |
| 130 | interrupted |

`--json` contract: parseable on **both** success and failure, `{"error": ...}` on stdout for errors, `[]` never `null` for empty lists, warnings on stderr so stdout stays a parseable document.

---

## 4. Method traps — these have all burned someone here

**Capture exit codes with a redirect, never through a pipe.**
```bash
cmd >/tmp/o 2>/tmp/e; echo $?     # correct
cmd 2>&1 | head -3; echo $?       # WRONG — this is head's status
```
This produced two wrong conclusions in one session, including a reported "exit 0" that was actually 1.

**zsh does not word-split unquoted `$var`.** `for c in $list` iterates once over the whole string. Use `${=var}` or run under bash. This has caused false "all commands MISSING" sweeps twice.

**`perl -e 'alarm N'` does not bound a Go binary.** Go ignores unhandled SIGALRM. Background it and kill, or use a real timeout.

**A local build proves nothing about a branch when the working tree is dirty.** A commit was pushed that deleted a function while its caller's fix sat uncommitted; `main` broke because the local tree hid it. Verify against a clean checkout.

**Waiting for CI: check for `pass`, not for the absence of `pending`.** A fast failure looks like completion. This merged a red PR once.

**GitGuardian scans every commit in a PR, not just the tip.** Amending will not clear it. A test that exercises redaction necessarily contains credential shapes — compose fixtures at runtime (`fake("gh"+"p_", 24, "a")`), because the scanner matches the assignment *shape*, so splitting the value alone does not help.

**Editing YAML by hand creates duplicate keys.** Three reproduction attempts were invalidated this way. Use a real parser.

---

## 5. Recurring bug shapes — hunt for these

The same handful of shapes recur across unrelated packages. When you find one instance, **grep for the others**; there usually are some.

| shape | example found here |
|---|---|
| **Unbounded peer-fed accumulator** | bash output, sandbox capture, SSE tool-call args, audit summary — all CWE-400, all separate packages |
| **The gate and the syscall disagree** | permission checked the lexical path while the tool opened the symlink-resolved one |
| **Permissive zero value** | empty `Pattern` matches everything; absent `tool` becomes `*`; `omitempty` on a bool deletes `false` |
| **Attacker-controlled namespace key** | replay table keyed on the caller-supplied URL path segment |
| **The guard that exists elsewhere** | `workspace index` validated `--max-files`; `security scan` did not. `gateway` had a bind guard; `webhook listen` did not — and the gateway's comment *claimed* to mirror it |
| **Producer with no consumer** | a helper written, tested, and never called, so it changed nothing |
| **Two implementations of one contract** | two eval scorers; only one got fixed, and the other's comment claimed parity |
| **Name-only matching** | secret redaction by variable name, so `PRIVATE_KEY` sailed through |

---

## 6. Tests here have repeatedly asserted the bug

**A passing test is not evidence.** Verified instances in this repo:

- Four plugin tests read a phantom directory, so they passed while the command had **no effect at all**.
- A DAG test asserted a job *failed* **and** required exit 0 — the contradiction that let `dag run` report success with every node failed.
- A webhook test asserted "fingerprints must be namespaced per platform" — that property **was** the vulnerability.
- Two bootstrap tests patched `TAG.cmd_setup`, which *created* the module global the production code was missing. The mock supplied the missing name; the fresh-install path was broken for every real user.
- A `read_file` test pinned the literal string `"Convert it"`, which stopped being the right advice once `read_document` existed.

**So: every regression test must be proven to fail against the pre-fix code before you accept it.** Neuter the fix, run the test, watch it fail, restore. This is not optional here — it has caught at least three tests that would have passed against broken code.

Also verify by **running**, in an isolated `TAG_HOME`, checking SQLite — not by reading the code and not from stdout alone.

---

## 7. The user's working process

Follow this unless told otherwise. It is consistent across sessions.

1. **GitHub issues first** — one per defect, detailed: summary, reproduction, expected vs actual, root cause with `file:line`, proposed fix, acceptance checklist.
2. **One file per commit.** Many small commits. The message explains *why*, and references the issue.
3. **Themed PRs**, each referencing its issues.
4. **Stack the PRs into an integration branch**, merge them in dependency order, then one final PR from integration → `main`.
5. Closing keywords go in the **final PR body** (and in commit messages) — a keyword in a PR merged into a non-default branch does not close anything.
6. **Every PR must be merged, never closed unmerged.** Issues close automatically.
7. **`gh` CLI only.** No self-attribution anywhere — no `Co-Authored-By`, no "generated with", not in commits, issues, or PR bodies.
8. Ship a release when behaviour changes: bump all five version sites, tag `v*` (Python/npm) and `go-v*` (Go binaries), then **verify from a clean install**, not from the build tree.

Version sites (all five, or the pin-drift test fails): `pyproject.toml`, `package.json`, `src/tag/__init__.py`, `tag-go/internal/version/version.go`, `ScaffoldPinnedVersion` in `tag-go/internal/ciauto/scaffold.go`.

---

## 8. Hard constraints

- **`.env` at the repo root holds real credentials.** Never commit, never echo, never `git add -A`.
- **The Go binary is CGO-free and static.** Do not add a cgo dependency. This is why pdf-inspector is a subprocess, not a link.
- Python deps are **exact-pinned**, deliberately — a PyPI supply-chain worm is the stated reason. Optional deps go in an extra and are lazy-installed via `tools/lazy_deps.py` — which is **not in this repo's tree**. It ships inside `src/tag/vendor/hermes-agent-upstream.tar.gz` and only exists on disk after setup extracts the runtime into `runtime/home/.hermes/`. `pyproject.toml` and several PRDs reference the path as if it were local; it is not. (Grepping for it and finding nothing is the expected result, not a broken checkout.)
- CI now runs **both full suites** (`pytest` and `go test ./... -race`). Before 2026-08-03 it ran one Python file and one Go package — so any "this was always tested" assumption about older code is false.

---

## 9. Where things stand

**Done and shipped.** A security + port audit (19 PRs), document ingestion (PRD-133 Tier 1: `read_document`, `tag doc read/check`), and a six-domain production-readiness QA audit (10 PRs, 4 P0s, 9 P1s).

**Known open, with reasons:**

- **Codex Security scan of `src/tag`** — blocked on OpenAI credits. Two keys authenticated; the account has none. Billing, not engineering.
- **Second-precision timestamps** are load-bearing in three places: the queue is not FIFO within a one-second window, `dag state` resolves the wrong run, and `tag logs` needed a normalisation shim. The real fix changes stored precision — a schema migration nobody has scoped.
- **No graceful shutdown** on any HTTP server (no `srv.Shutdown` in the repo). This is what makes the webhook partial-failure window reachable without fault injection.
- **`tag run`/`tag shell` are the least-finished commands**: `steps` is never written, so `runs show` can never display a result; `shell` is stateless and untraced.
- **Cross-distribution schema drift** — both sides use `CREATE TABLE IF NOT EXISTS`, so whichever creates the DB first wins and three Python commands break on a Go-created home. Nobody owns the schema.
- `issues.md` P2/P3 sections list ~45 further items, each with a reproduction.

---

## 10. Read these first

1. `.claude/skills/harness-security/SKILL.md` — the failure taxonomy. Fifteen sections, every one grounded in a real bug from this repo.
2. `issues.md` — the audit, **including the negative results**: 16 security properties confirmed *holding*. Recorded deliberately so the next audit does not repeat the work.
3. `docs/qa/CHECKLIST.md` — how to test this thing, and the web-to-CLI translation table (the QA brief was written for a web app; TAG is a CLI harness with four HTTP servers).
4. `docs/qa/INVENTORY-2026-08-03.md` — every command and endpoint. Note the header: the HTTP table was **corrected after the audit**, because the first version guessed the route-to-command mapping from a grep instead of running the servers, and got it wrong.

That last point is the note to end on. The inventory was wrong because it was inferred rather than observed. Most of what was found in this repo was found by running things, and most of what was missed was missed by reading them.
