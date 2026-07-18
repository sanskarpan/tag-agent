# TAG-Go — stub / incomplete-path completion tracker

Every honest stub in the Go harness, being finished **end-to-end** with in-depth
E2E testing, one gated PR per cluster. Checked when implemented **and** E2E-tested
**and** merged to `main`.

Legend: `[ ]` todo · `[~]` in progress · `[x]` done (impl + E2E + merged) · `[!]` needs a design decision (flagged to user)

## Cluster 1 — Agentic solvers (real git + gh integration)  · branch `feat/stub-solvers`
- [ ] **swe-solve** — enable built-in tools confined to `--repo` (read/write/bash) so the agent actually reads+edits files; `--run-tests`. (`internal/solver/solver.go:70,110`, `cli/swesolve.go`)
- [ ] **issue-solve** — fetch a live issue via `gh issue view` / `gh api` when the input is a reference (`#123`, `owner/repo#N`, URL). (`solver.go:158`, `cli/issuesolve.go`)
- [ ] **review-pr** — fetch the PR diff via `gh pr diff`; optionally post review via `gh pr comment`/`review` with `--post`. (`solver.go:171`, `cli/reviewpr.go`)
- [ ] **agentic-ci** — run a real check→fix loop (build/test command), not just N echo passes. (`cli/agenticci.go`)
- E2E: temp git repos, a fake `gh` on PATH, echo-provider loops; live smoke with `--provider`.

## Cluster 2 — Native runtime stubs (no Hermes dependency)  · branch `feat/stub-runtime`
- [ ] **context compress / trim** — native context assembly + summarize/trim pass via the agent loop; persist. (`cli/context.go:100-111`)
- [ ] **split plan** — drive an architect/editor agent loop to produce+persist a `split_runs` spec. (`cli/split.go:232-239`)
- [ ] **shell** — real REPL: read a line → run through the agent loop → print; echo default, `--provider` for real. (`cli/shell.go:16,29,41`)
- E2E: subprocess drives compress/trim/plan/shell with echo; asserts persisted rows + output.

## Cluster 3 — Sandbox docker backend  · branch `feat/stub-sandbox-docker`
- [ ] **sandbox run --backend docker** — `docker run --rm` with `--memory/--cpus/--network none` limits; capture stdout/stderr/exit; keep `restricted` default. (`internal/sandbox/sandbox.go`, `cli/sandbox.go`)
- E2E: skip if docker absent, else real `docker run alpine echo`; resource-limit + network-deny assertions.

## Cluster 4 — mem2 embeddings + vector search  · branch `feat/stub-mem2-embed`
- [ ] **mem2 store / store search (vector) / rebuild / extract** — real embeddings provider (OpenAI `/v1/embeddings` + config), vector store (BLOB), cosine similarity; FTS fallback when no key. (`internal/memory/`, `cli/mem2.go:261,287-311`)
- E2E: mock embeddings server (deterministic vectors) + a live smoke; assert vector recall ranks correctly.

## Cluster 5 — plugin install + marketplace push  · branch `feat/stub-plugin-marketplace`
- [ ] **marketplace push** — POST the profile config to a configurable marketplace URL (SSRF-guarded like `pull`); real round-trip. (`cli/marketplace.go`)
- [!] **plugin install** — Go has no Python venv; implement the most sensible native mechanism (install an MCP-server plugin / record+enable) OR keep honest and document the design decision. (`cli/plugin.go:93`) — **flag to user**
- E2E: mock marketplace server round-trip; plugin install against a curated entry.

## Notes
- Every command keeps its **honest** offline behavior; new depth is opt-in via `--provider` / a key / a backend flag, so nothing that works today regresses.
- Each cluster: implement → unit + in-depth E2E (mock + live/docker where possible) → granular commits → no-mistakes gate → merge → check the boxes here.
