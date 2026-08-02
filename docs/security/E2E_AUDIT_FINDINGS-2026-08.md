# Adversarial E2E audit — 13 newly-completed features (2026-08-01)

Read-only audit. Binary built and every scenario RUN in an isolated TAG_HOME with API
keys unset, local mock SSE/embeddings servers, and Docker for real egress tests.
Repo unmodified during the audit.

## HIGH

**H1 — `--allow-tool 'read_file:*'` defeats the built-in credential deny.**
`internal/permission/permission.go:312` skips an allow rule only when `r.Pattern == ""`.
A *wildcard* pattern is a blanket allow in every meaningful sense but has a non-empty
Pattern, so it is honoured — and ranked ABOVE all 28 credential denies.
Verified leaking `.env` and `*.pem` with `*`, `**`, `*.*`, `?*`, `.*`
(a genuinely restrictive glob `[a-z.]*` is correctly refused). Same hole via the
config-file form. `permissions show` displays the bypass with no warning.
Contradicts the shipped tag.yaml header: "catch-all; cannot unprotect credential paths".

**H2 — Symlink bypass leaks credentials with NO FLAGS AT ALL.**
`ln -sf .env link.txt` then a default `tag run --tools` (read_file is allow-by-default)
returns `.env` contents to the model. `resolvePath` does EvalSymlinks but only to enforce
the tool-root boundary; `pathSubject` (`internal/tool/tools.go:116-132`) builds the
permission subject lexically with no resolution, so credential globs match `link.txt`,
never `.env`. Independently found by the Codex Security scan (CWE-59).

## MEDIUM

**M1 — SIGTERM strands a `pending` approval forever; approving it silently does nothing.**
Process dies -> row stays `pending` past its own timeout -> `permissions approve` returns
"APPROVED" exit 0 with nothing listening. Fake success. No reaper; `permissions pending`
shows no deadline, so a stale row is indistinguishable from a live one.

**M2 — `bash "cat .env"` bypasses every credential rule AND is offered to the reviewer.**
Credential rules are KindPath; bash is KindCommand, so its argument is never screened —
including by the `standard` tripwire preset. Violates "must never be offered to a reviewer".

**M3 — Dual-stack hostname allow rules fail at exit 127; `--egress pypi` is unusable.**
Docker's default bridge is IPv4-only and every real host is dual-stack, so the whole
hostname-allow feature and the flagship named policy cannot run on stock Docker Desktop.
Fail-closed (not a hole) but functionally dead, and the error blames the destination.

**M4 — `trace snapshot`/`replay`/`checkpoint` report success on things that did not happen.**
`snapshot <nonexistent-trace>` -> exit 0, nothing written. `replay` of a never-snapshotted
trace silently rebuilds from live spans and prints "Captured: <now>". Snapshot PK is
sha256(traceID)[:16] with step_index hard-coded 0 + INSERT OR REPLACE, so a trace can hold
only ONE checkpoint and each call destroys the previous.

**M5 — `trace show --min-cost-usd 0` is not a no-op**: flattens the tree, drops every
unpriced span, and zeroes `unpriced_spans` so the omission warning disappears exactly when
the filter causes the omission. Same class in `stats` (compares raw `g.Cost` not `g.cost()`).

**M6 — $0.00 laundering on the TOTAL row and in --json.** Per-group is correct (`—`/null);
the total emits `$0.000000` / `total_cost_usd: 0` for a window where nothing could be priced.
The mitigating `note:` exists only in text output.

## LOW
- `mem2 config set` accepts junk, reports success, value inert (`auto_extract maybe`).
- Structurally-wrong extractor response reported identically to "nothing worth remembering".
- `tool-index search` on a never-built index reports a clean empty result (status knows better).
- `--egress open` silently downgraded to a total network block on both backends.
- Wrong policy name ("open") in the deny-all/network contradiction message.
- Exit-code drift: `--provider nope`, `--steps notjson`, `--limit -1`, `--timeout 0` return 1, not 2.
- `tripwire check` per-stage fake pass; cannot exercise tool-scoped rules (no --tool flag).
- Duplicate findings from overlapping detectors; openai-api-key regex too loose.
- `mcp-registry add-curated --dry-run` errors instead of printing the plan on missing env.
- `trace snapshot --json` emits plain text (only --json gap across 30+ surfaces).
- `mem search` LIKE supplement only fires when len(res) < limit, so a full BM25 page drops
  substring-only matches (base #574 CJK cases unaffected).

## STRUCTURAL NOTE (not a bug today)
The no-hang guarantee is **a convention, not a type**. `noPrompt` is a plain bool stomped
onto a copied permFlags by each background surface (queue.go:351, loop.go:398, swarm.go:198,
evalrun.go:250, agenticci.go:124/:986), and `hitl.ToolPauser` is exported and constructible
anywhere. It holds on every surface today, but nothing structurally prevents a new
background command from omitting the line.
