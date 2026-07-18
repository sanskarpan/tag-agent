# TAG-Go — stub / incomplete-path completion tracker

Every honest stub in the Go harness, being finished **end-to-end** with in-depth
E2E testing, one gated PR per cluster. Checked when implemented **and** E2E-tested
**and** merged to `main`.

Legend: `[ ]` todo · `[~]` in progress · `[x]` done (impl + E2E + merged) · `[!]` needs a design decision (flagged to user)

## Cluster 1 — Agentic solvers (real git + gh integration)  · branch `feat/stub-solvers`
- [x] **swe-solve** — `--tools` (requires `--repo`) registers the root-confined built-in file tools (`read_file`/`write_file`/`list_dir`) so a real `--provider` agent reads+edits files under `--repo`; `--allow-bash` adds the bash tool (unrestricted host exec, cwd = `--repo`); `--run-tests <cmd>` runs a command after the loop (cwd = `--repo`) and reports pass/fail in `Result.TestResult`. Echo default preserved (tools register but echo emits no tool calls, so it stays inert offline). (`internal/solver/solver.go`, `cli/swesolve.go`)
- [x] **issue-solve** — a GitHub reference (`#123`, `owner/repo#N`, or a GitHub URL; `--repo` supplies owner/repo for a bare `#123`) is fetched via `gh issue view --json title,body`; if `gh` is missing/unauthenticated the raw reference is passed through and the solver records an honest "could not fetch" note rather than faking it. (`solver.go`, `cli/issuesolve.go`)
- [x] **review-pr** — `--pr <n>` fetches the diff via `gh pr diff`; `--post` posts the review as a PR comment via `gh pr comment` (guarded: `--post` requires `--pr` and refuses `provider=echo`), otherwise a dry-run note is emitted. (`solver.go`, `cli/reviewpr.go`)
- [x] **agentic-ci** — `--check <cmd>` (+ optional `--repo` working dir) runs a real check→fix→re-check loop: run the check, on failure feed its output to the agent loop for a fix suggestion, re-check, up to `--max-iters`, reporting `Result.Iterations` / `Converged`; without `--check` it drives the loop over the task text. (`cli/agenticci.go`, `solver.go`)
- E2E: 12 hermetic tests — a fake `gh` on PATH, temp git repos, a stub SSE provider emitting `write_file` — plus solver unit tests; live smoke with `--provider`. (`cli/solvers_e2e_test.go`, `internal/solver/solver_test.go`, `internal/solver/scripted_test.go`) — impl + E2E done on branch, merge pending.

## Cluster 2 — Native runtime stubs (no Hermes dependency)  · branch `feat/stub-runtime`
- [x] **context compress / trim** — native context assembly (`assembleSession`: run prompt + step turns + profile `memory_journal`) + summarize/trim pass via the agent loop; persists to a self-ensured `context_compressions` table. Echo default, `--provider` for real. (`cli/context.go`, `internal/contextwin/compress.go`)
- [x] **split plan** — drives the architect agent loop to decompose a task into a `{task,rationale,items[]}` spec (tolerates prose around the JSON, deterministic single-item fallback, or strict `--spec-json`); persists to `split_runs`(status=planned) + `split_items`. Echo default, `--provider` for real. (`cli/split.go`)
- [x] **shell** — real REPL: read a line → run through the agent loop → print; echo default, `--provider` for real. (`cli/shell.go`)
- E2E: subprocess drives compress/trim/plan/shell with echo; asserts persisted rows + output. (`cli/runtime_stub_e2e_test.go`, `cli/split_plan_test.go`, `internal/contextwin/compress_test.go`) — impl + E2E done on branch, merge pending.

## Cluster 3 — Sandbox docker backend  · branch `feat/stub-sandbox-docker`
- [ ] **sandbox run --backend docker** — `docker run --rm` with `--memory/--cpus/--network none` limits; capture stdout/stderr/exit; keep `restricted` default. (`internal/sandbox/sandbox.go`, `cli/sandbox.go`)
- E2E: skip if docker absent, else real `docker run alpine echo`; resource-limit + network-deny assertions.

## Cluster 4 — mem2 embeddings + vector search  · branch `feat/stub-mem2-embed`
- [x] **mem2 store / store search (vector) / rebuild** — real embeddings provider (`internal/memory/embed.go`: `Embedder` interface + `OpenAIEmbedder` POSTing to `{base}/embeddings`, default model `text-embedding-3-small`), resolved from `TAG_EMBED_BASE_URL`/`TAG_EMBED_API_KEY`/`TAG_EMBED_MODEL` then `OPENAI_API_KEY`. Vectors persist as little-endian float32 BLOBs on `semantic_memories.embedding` (+`embed_model`), columns self-ensured via `pragma_table_info`-guarded `ALTER`. `store store --id` embeds one memory; `store rebuild` batch-embeds all missing (`--force` re-embeds all); `store search --query` embeds the query and cosine-ranks stored vectors top-`--limit`, with transparent FTS fallback when no key / no vectors (`--json` reports `mode` = `vector|fts`). No backend → store/rebuild error clearly, search degrades to FTS. (`internal/memory/embed.go`, `cli/mem2.go`)
- [ ] **mem2 extract** — still an honest stub: needs the in-process LLM runtime (Phase-2 cutover), out of scope for this cluster. (`cli/mem2.go`)
- E2E: mock embeddings server (deterministic orthogonal vectors) asserts rebuild embeds all + search ranks the semantically-closest memory first (`mode=vector`), keyless FTS fallback (`mode=fts`), and clear store/rebuild errors with no key; a live smoke against real OpenAI `text-embedding-3-small` persisted a 1536-dim vector. Unit tests cover cosine, float32 BLOB round-trip, env precedence, ranking, both FTS-fallback paths, and schema-ensure idempotency. — impl + E2E done on branch, merge pending.
- Limitation: linear cosine scan (no ANN index) — fine at TAG scale, matches the Python port.

## Cluster 5 — plugin install + marketplace push  · branch `feat/stub-plugin-marketplace`
- [ ] **marketplace push** — POST the profile config to a configurable marketplace URL (SSRF-guarded like `pull`); real round-trip. (`cli/marketplace.go`)
- [!] **plugin install** — Go has no Python venv; implement the most sensible native mechanism (install an MCP-server plugin / record+enable) OR keep honest and document the design decision. (`cli/plugin.go:93`) — **flag to user**
- E2E: mock marketplace server round-trip; plugin install against a curated entry.

## Notes
- Every command keeps its **honest** offline behavior; new depth is opt-in via `--provider` / a key / a backend flag, so nothing that works today regresses.
- Each cluster: implement → unit + in-depth E2E (mock + live/docker where possible) → granular commits → no-mistakes gate → merge → check the boxes here.
