---
name: harness-security
description: Use when writing, reviewing, or auditing any code in an AI agent harness that touches tool execution, sandboxing, permissions, credentials, provider I/O, or externally-triggered automation — and before claiming any safety control works. Encodes failure modes observed in this codebase, not generic secure-coding advice.
---

# Agent-harness security

An agent harness is not an ordinary application. It combines **valuable credentials**,
**a private source tree**, **network access**, and **a model that chooses which tools to
run**. The security question is therefore narrower and sharper than "is this code safe":

> **A defect that crosses the model-to-tool boundary becomes host compromise, even when
> the model itself is not hostile.**

Everything below is a failure mode that actually occurred in this repository. Each is
cheap to check and was expensive to find.

---

## 1. The name of a control is a promise. Keep it or rename it.

`sandbox run --backend restricted` ran `sh -c` on the host with a fixed `PATH`. It
reached the network and read `/etc/passwd`. It was called a sandbox for months.

A control whose name overclaims is **worse than no control**, because users delegate
trust to it and stop thinking. The same applies to `--dry-run` that writes rows, an
`isolation` string that describes a policy that failed to install, and a `--timeout` that
does not bound anything.

**Rule.** Either the control delivers what its name says, or the name changes. There is no
third option where the docs quietly explain the gap.

## 2. Report the guarantee you achieved, never the one you attempted

Every isolation/enforcement result must carry a machine-readable statement of what was
*actually* applied, including partial failure:

- `Landlock unavailable (ENOSYS): filesystem NOT confined` — good.
- `Landlock unavailable: kernel < 5.13` on a 6.10 kernel — **bad**: sends the operator to
  the wrong remedy. Report the errno, not a guess.
- `rlimits via ulimit (CPU 35s, AS 512MB)` when the shell silently ignored `ulimit -v` —
  **bad**: that is a fabricated guarantee.

**Rule.** If a layer cannot be applied, the run either **fails closed** or **says so in the
result**. Never both silently proceed and claim the guarantee.

## 3. Fail-closed vs fail-open is a per-check decision that must be written down

Not a global default. State the reasoning at each site.

| Condition | Decision | Reasoning |
|---|---|---|
| Policy config malformed | **Fail closed** — refuse to start | A half-loaded policy leaves the operator believing they are protected |
| Content unreadable / unparseable | **Fail closed** | Content you cannot inspect is indistinguishable from content that violates |
| Counter/store unreachable | **Fail closed** | "We could not count, so we allowed" is silent fail-open |
| Telemetry/audit write fails | **Fail open, but LOUD** | Telemetry must never make the security decision |
| Approval cannot be published | **Fail closed** | Never approve without a recorded human decision |
| Approval wait expires | **Deny + audit** | Expiry is refusal, not consent |
| Guard is nil (wiring bug) | **Deny** | A wiring mistake must not equal an ungated tool |

## 4. Put the gate inside the registration path, not at call sites

If each caller must remember to attach the guard, one eventually will not.

```go
// GOOD: no caller can register an ungated tool
func Register(reg *Registry, opts Options) {
    add := func(t Tool) { reg.Add(permission.Wrap(opts.Guard, t, subj)) }
}
```

A nil guard must resolve to the **secure default**, not to "unguarded". Then a missed
wiring site is merely safe rather than catastrophic.

**Corollary — conventions are not types.** In this codebase the no-hang guarantee depends
on every background surface remembering to set `noPrompt = true` on a copied struct.
It holds today across five call sites and nothing structurally prevents the sixth from
omitting it. If a safety property is enforced by "remember to", it is not enforced.

## 5. Authorize the *resolved* subject, never the string the model typed

This is the single highest-yield check in this document, and it produced two independent
credential-exfiltration paths here.

- **Symlink indirection.** The permission subject was computed with `filepath.Clean()` —
  purely lexical. `EvalSymlinks` ran later, and only to enforce the tool-root boundary.
  So `ln -s .env notes.txt` was authorized as `notes.txt`, then resolved and read.
  Credential denies bypassed, **with no flags at all**, because `read_file` is allow-by-default.
- **Different kind, same file.** Credential rules matched `KindPath`. `bash` is
  `KindCommand`, so `bash "cat .env"` was never screened — and was cheerfully offered to a
  human reviewer at the approval gate for one-keystroke approval.

**Rule.** Resolve the path (symlinks, `..`, aliases) **before** the authorization decision,
and re-check after resolution if a TOCTOU window remains. Then ask: *what other route
reaches the same bytes?* Shell commands, archive extraction, and editor plugins are all
paths to a file that a path-shaped rule will not see.

## 6. "Blanket" must be defined semantically, not syntactically

A carve-out that prevented a catch-all allow from unprotecting credential paths tested
`pattern == ""`. The pattern `*` is not the empty string — so `--allow-tool 'read_file:*'`
was honoured and ranked **above** all 28 credential denies. `.env` and `*.pem` went
straight to the model.

**Rule.** Classify a rule by **what it matches**, not by how it is spelled. If a pattern
matches everything, it is a blanket rule. Additionally: surface any allow rule that
outranks a built-in protection in whatever `show`-style command exists — a bypass the
operator can see is a bypass they can fix.

## 7. Never block a background path on a human

Queue workers, cron daemons, DAG executors and CI runs must be **structurally incapable**
of parking on an approval prompt. Two layers:

1. **Structural** — the background surface refuses to install an interactive gate at all,
   and says so loudly rather than ignoring the flag.
2. **Temporal** — where a gate *is* installed, the wait is always bounded. Reject a
   non-positive timeout: there is no wait-forever mode.

Then close the loop: **a pending approval whose waiter died must be reaped.** Here, SIGTERM
left a row `pending` past its own timeout, and `permissions approve` returned
`APPROVED` exit 0 with nothing listening. A recorded human approval that had no effect is
worse than no approval surface.

## 8. Bound every accumulator fed by a peer you do not control

Model output, SSE frames, MCP frames, tool stdout, docker capture, audit arguments. An
unbounded `ReadBytes` on an MCP transport consumed **36 GB** in a test harness before
anyone noticed; a newline-less peer drove RSS to 1.75 GB in seconds.

**Rule.** Every read from a provider, MCP peer, or subprocess gets an explicit cap, and
exceeding it is an error — not a truncation that silently changes meaning.

## 9. Deny rules must cover the local link, not just the default route

A `blackhole default` route did not cover the container's own connected subnet
(`172.17.0.0/16 dev eth0` is more specific), so "deny everything except X" left **every
sibling container reachable**.

**Rule.** When implementing a deny-by-default network policy, enumerate what remains
reachable *after* the policy is installed — connected subnets, link-local, loopback,
the gateway — and verify from inside the sandbox against a live sibling, not by reading
the ruleset.

## 10. Replay protection needs durable identity and a freshness bound

Observed gaps: delivery-ID replay protection lost on cache eviction and restart; signed
payloads with no freshness identifier replayable forever; a captured payload replayable
freely inside the timestamp tolerance.

**Rule.** Signature verification alone is not replay protection. Require a durable
delivery id **and** a timestamp tolerance, and treat unsigned acceptance as an explicit,
loudly-named opt-in — never the default. (This receiver accepted unsigned webhooks by
default and enqueued agent work from anonymous callers.)

## 11. A producer with no consumer looks exactly like a working feature

Recurring here, three times:

- The `spans` table, the whole `trace` command tree and an OTLP exporter all shipped —
  and **nothing ever wrote a span**. Four PRDs looked complete and were functionally empty.
- `swarm list/status/results` read tables **no Go code path ever wrote**.
- `mem2 extract` read the transcript, **discarded it** (`_ = parts`), and printed
  "Extracted 0 memories".

Each degraded *politely*, which reads as an empty database rather than a missing feature.

**Rule.** For any read surface, verify a producer exists and run the round trip. When
adding a feature, ask "who writes this?" before "who reads this?".

## 12. Verify by running. Green tests are not evidence.

A green suite here masked ~30 dispatch-layer bugs. Since then, running the binary found
what `go build` and `go test` could not:

- `panic: flag redefined` on **every** invocation (two flags collided)
- `stats --cost` returning zero spans for a run made seconds earlier (a second-precision
  bound string-compared against fractional RFC3339)
- an egress policy leaving siblings reachable
- a worker sharing one working directory across concurrent jobs, silently losing files

**Checklist before claiming any control works:**

- [ ] Build the binary and run the actual command — not just the unit test
- [ ] Use an isolated `HOME`/state dir, with credentials unset
- [ ] Capture exit codes **without piping** (a pipe masks the status)
- [ ] Attempt the thing the control forbids and confirm it fails **and did not run**
- [ ] Confirm the honest path too: does the allowed case still work?
- [ ] Test a *stalled* peer, not only an unreachable one — they fail differently
- [ ] Bound every hang-prone test so a regression **fails** instead of wedging
- [ ] Check a skip is not standing in for a pass

## 13. Test fixtures that look like secrets will break your secret scanning

Deliberately-fake credentials in scanner tests tripped both GitHub push protection and
GitGuardian. Splitting the *value* was not enough — detectors match the
`KEY = "value"` **assignment shape**. Compose them at runtime instead.

This matters beyond tidiness: **a permanently-red security check trains people to merge
past it**, which is exactly what happened here before it was fixed.

## 14. Vendored runtimes are executable supply-chain input

A vendored upstream tarball is not inert sample data. This repo pins a snapshot ~5,400
commits behind upstream, whose maintainers have since deprecated the install method the
wrapper depends on. Pin to a **tag** with a scheduled bump; neither a frozen tarball nor
an unpinned `main` is maintainable.

---

## Using the Codex Security scanner

`@openai/codex-security` found the symlink bypass in minutes. Operating notes learned the
expensive way:

- **Scope by path.** `--path <dir>` is the reliable way to keep credentials out of an
  upload; do not rely on ignore semantics you have not verified. Confirm afterwards:
  the `02_discovery/in_scope_files.txt` artifact lists exactly what was sent.
- **Do not run `xhigh` across a whole repo.** Cost was near-flat then went vertical
  ($21 → $27 inside one minute) and the run capped out **before producing any findings** —
  phases 3-5 empty. Scope to the security-critical packages and use `medium` first.
- **`--max-cost` overshoots** (it checks after a unit of work). Budget ~10% over.
- **Partial output is still valuable.** A capped run leaves
  `02_discovery/candidate_ledger.jsonl` — CWE-tagged candidates with locations.
- **Candidates are not findings.** If the validation phase did not run, reproduce each
  one before acting. Several here were real; the `/proc` exposure it flagged was already
  a disclosed, accepted limitation.
- The generated `01_context/threat_model.md` is worth keeping on its own.
