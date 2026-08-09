# TAG QA Audit — Issues

Production-readiness audit, 2026-08-03. Inventory: `docs/qa/INVENTORY-2026-08-03.md`. Method: `docs/qa/CHECKLIST.md`.

**Status: audit COMPLETE across six domains; fixing in progress.** Findings are recorded as they are confirmed. Nothing here is fixed until its Verification section says it was re-tested.

Severity: **P0** application unusable / data loss / security · **P1** major functionality broken · **P2** important, workaround exists · **P3** minor.

---

## ISSUE-001 — `doc read --json` emits no JSON on the error path

- **Severity:** P1
- **Priority:** P1
- **Area:** CLI / document ingestion
- **Route:** `tag doc read`
- **Type:** Functional / API contract
- **Status:** Open

### Description

`doc read` declares its own local `--json` flag, which **shadows the root's persistent `--json`**. It is the only command in the CLI that does this:

```
tag-go/internal/cli/doc.go:74   read.Flags().BoolVar(&asJSON, "json", false, ...)
tag-go/internal/cli/root.go:79  root.PersistentFlags().BoolVar(&flagJSON, "json", false, ...)
```

Because the local flag wins, `flagJSON` stays false, so `jsonErrorMaybe` never fires and the error path emits nothing on stdout.

### Steps to Reproduce

```
$ tag --json runs show nonexistent          # every other command
{ "error": "no run matching id prefix \"nonexistent\"" }

$ tag --json doc read /nope.pdf             # this one
(stdout empty; plain text on stderr)        exit=2
```

### Expected Behavior

A `--json` consumer receives a parseable `{"error": ...}` object on stdout, as it does from every sibling command.

### Actual Behavior

Empty stdout. The consumer sees a successful-looking empty read, or a JSON parse failure.

### Root Cause

A redundant local flag duplicating a persistent one. Self-inflicted in the document-ingestion change (#680).

### Dependencies

None.

### Proposed Fix

Delete the local `--json`; use the root's. One way to ask for JSON, one behaviour.

### Verification

`tag --json doc read <missing>` must print a parseable error object and exit non-zero; `tag --json doc read <good.pdf>` must still print the full JSON document.

### Regression Risk

Low. Removing a flag that duplicates an existing one; the success path already consults both.

---

## ISSUE-002 — `runs show <unknown>` reports an error and exits 0

- **Severity:** P2
- **Priority:** P2
- **Area:** CLI / run inspection
- **Route:** `tag runs show`
- **Type:** Functional / exit-code contract
- **Status:** Open

### Description

Every sibling lookup returns non-zero for a missing id. `runs show` returns **0** while printing an error object, so a script cannot distinguish "found" from "does not exist".

### Steps to Reproduce

```
$ tag --json runs show nope   → exit 0   {"error": "no run matching id prefix \"nope\""}
$ tag --json trace show nope  → exit 1
$ tag --json loop status nope → exit 1
$ tag --json swarm status nope→ exit 1
$ tag --json eval show nope   → exit 1
$ tag --json queue show nope  → exit 2
```

### Expected Behavior

Non-zero, consistent with the family.

### Actual Behavior

Exit 0 with an error payload — the shape this project has repeatedly treated as fabricated success.

### Root Cause

To be confirmed in `internal/cli` run-inspection handler; the error is emitted via `jsonErrorMaybe` but the RunE returns nil.

### Dependencies

Related to ISSUE-003 (the same family disagrees about the error *shape* as well as the code).

### Proposed Fix

Return the error rather than nil after emitting it.

### Verification

All six lookups above exit non-zero for a missing id.

### Regression Risk

Low, but it IS a behaviour change for any caller that currently ignores the exit code.

---

## ISSUE-003 — the not-found family disagrees on both exit code and payload shape

- **Severity:** P3
- **Priority:** P3
- **Area:** CLI / consistency
- **Route:** `runs show`, `trace show`, `loop status`, `swarm status`, `queue show`, `eval show`
- **Type:** UX / API contract
- **Status:** Open

### Description

Six equivalent "look this id up" commands produce four different behaviours: exit 0, 1 or 2, and either `{"error": ...}` or a bare `[]` (`trace show`). `queue show` exits 2 with **empty stdout** under `--json` — the same contract gap as ISSUE-001.

### Expected Behavior

One convention across the family: non-zero exit, `{"error": ...}` on stdout under `--json`.

### Root Cause

Independently written handlers with no shared helper for "not found".

### Proposed Fix

A single `notFoundErr` helper used by all six.

### Verification

Table-driven test asserting identical shape and code across the family.

### Regression Risk

Low; behaviour change for callers reading the current codes.

---

## ISSUE-004 — `tripwire` and `review-pr` read stdin unbounded

- **Severity:** P3
- **Priority:** P3
- **Area:** CLI / resource bounds
- **Route:** `tag tripwire`, `tag review-pr`
- **Type:** Resource exhaustion (CWE-400, low exploitability)
- **Status:** Open

### Description

```
tag-go/internal/cli/tripwire.go:255  b, err := io.ReadAll(in)
tag-go/internal/cli/reviewpr.go:63   b, err := io.ReadAll(cmd.InOrStdin())
```

Both are unbounded. Unlike the SSE, sandbox and bash accumulators fixed earlier, the input here is **operator-supplied via a pipe**, not peer-supplied — piping 10 GB into your own CLI is your own doing. It is a bound worth having rather than a live defect, and is recorded so it is not rediscovered as a security finding later.

Mitigating: a `tripwire` wired into automation over untrusted PR content moves this closer to peer-supplied.

### Proposed Fix

`io.LimitReader` with a stated cap, consistent with `MaxReadBytes`.

### Verification

Oversized stdin is truncated with a stated notice, not silently or unboundedly consumed.

---

## ISSUE-005 — `doc read` on a missing file exits 1, contradicting its own help

- **Severity:** P3
- **Priority:** P3
- **Area:** CLI / document ingestion
- **Route:** `tag doc read`
- **Type:** Exit-code contract
- **Status:** Open

### Description

`doc read --help` documents `2 = usage error`, and the sibling `agentic-ci fix-vuln` exits 2 for a missing SARIF file. But naming a file that does not exist exits **1**:

```
$ tag doc read missing.pdf   → exit 1   error: reading missing.pdf: stat missing.pdf: no such file
$ tag doc read adir.pdf      → exit 1   error: adir.pdf is a directory
```

Both are the operator naming the wrong thing — usage errors.

### Expected Behavior

Exit 2, matching the documented contract and `fix-vuln`.

### Actual Behavior

Exit 1, which the same help text reserves for "the run failed".

### Root Cause

`internal/docs.Extract` returns a plain error for both cases and the CLI passes it through `jsonErrorMaybe` without classifying it. Self-inflicted in #680.

### Dependencies

Same command as ISSUE-001; fix together.

### Proposed Fix

Classify missing-file and is-a-directory as usage errors at the CLI boundary.

### Verification

Both cases exit 2; a genuine engine failure still exits 1.

### Regression Risk

Low.

---

## ISSUE-006 — `webhook listen` has no bind guard; `--host 0.0.0.0 --allow-unsigned` exposes unauthenticated job injection

- **Severity:** P0
- **Priority:** P0
- **Area:** Webhook receiver
- **Route:** `tag webhook listen`
- **Type:** Security / Authorization
- **Status:** Open

### Description

`tag gateway` refuses to bind a non-loopback address without auth. `tag webhook listen` — which **enqueues agent tasks whose text becomes the agent's prompt** — has no equivalent check.

The comment on the gateway's own guard claims it "mirrors the webhook hardening". **That hardening was never written.**

### Steps to Reproduce

```
$ tag gateway --host 0.0.0.0 --port 18992
error: refusing to bind 0.0.0.0 without an auth key   exit 1        ← guard present

$ tag webhook listen --host 0.0.0.0 --port 18993 --allow-unsigned
TAG webhook server: http://0.0.0.0:18993                            ← starts anyway
WARNING: running with --allow-unsigned ...                          ← a warning is not a guard
```

From a non-loopback address with no credentials, a POST enqueues a `queue_jobs` row whose attacker-controlled text becomes the prompt when a worker picks it up. Verified end-to-end by the auditing agent.

### Expected Behavior

Refuse, mirroring the gateway: `--allow-unsigned` is loopback-only unless an explicit opt-in flag is passed.

### Actual Behavior

Serves, warns, and accepts remote unauthenticated job injection.

### Root Cause

`internal/cli/webhook.go:33` calls `webhook.Serve(...)` with no host check. The helper already exists: `isLoopbackHost` at `internal/cli/gateway.go:138`.

### Dependencies

Independent, but compounds ISSUE-007 (a remote attacker also gets the replay bypass).

### Proposed Fix

Apply the gateway's guard, with an `--allow-remote`-shaped opt-in.

### Verification

`--host 0.0.0.0 --allow-unsigned` exits non-zero; `--host 0.0.0.0 --secret s` still serves; loopback unaffected.

### Regression Risk

Behaviour change for anyone deliberately running it exposed — which is the point.

---

## ISSUE-007 — cross-platform signature confusion defeats webhook replay protection entirely

- **Severity:** P0
- **Priority:** P0
- **Area:** Webhook receiver
- **Route:** `POST /webhook/<platform>`
- **Type:** Security / Replay
- **Status:** Open

### Description

The platform is an **unvalidated path segment** that is both (a) the branch selector for signature verification and (b) the prefix of the replay-table key. The generic/linear branch strips everything up to the last `=` and validates a bare HMAC of the body — byte-identical to what GitHub computes.

So one captured signed delivery is replayable **without limit** by changing one path segment.

### Steps to Reproduce (independently confirmed)

```
POST /webhook/github    sig, delivery D  -> 200
POST /webhook/github    same sig, same D -> 409 duplicate    ✓ protection works…
POST /webhook/linear    same sig, same D -> 200              ✗ …only per-platform
POST /webhook/attacker  same sig, same D -> 200              ✗ arbitrary string
POST /webhook/zzz       same sig, same D -> 200              ✗

sqlite> select platform,count(*),sum(signature_valid) from webhook_events group by platform;
attacker|1|1   github|1|1   linear|1|1   zzz|1|1
```

Each accepted replay is recorded `signature_valid=1` and stores the full raw payload.

### Expected Behavior

Unknown platform → 404. The HMAC construction bound to the platform. The replay key not derived from attacker-controlled input.

### Actual Behavior

Unbounded replay of a single captured delivery, each recorded as validly signed.

### Root Cause

`internal/webhook/webhook.go:61-67` (last-`=` strip, no platform binding), `webhook.go:287` (platform read from the path with no allowlist), `internal/webhook/replay.go:46-55` (fingerprint prefixed with the attacker-controlled platform).

Aggravating: one `--secret` is shared across all platforms, which is what makes the confusion reachable.

**This is my own regression.** The durable replay protection I added earlier keys on `platform + id`, and I never asked whether `platform` was trustworthy.

### Dependencies

Blocks nothing; ISSUE-006 widens its reach.

### Proposed Fix

Allowlist the platform segment (404 otherwise) before any verification, and bind the fingerprint to the verified platform rather than the requested one.

### Verification

The reproduction table above must read 200, 409, 404, 404, 404.

### Regression Risk

Low. `generic` is currently unreachable from `rule-add` anyway (ISSUE-011).

---

## ISSUE-008 — a partially-failed delivery is recorded `processed`, enqueues nothing, and permanently rejects the retry

- **Severity:** P1
- **Priority:** P1
- **Area:** Webhook receiver
- **Type:** Data integrity / fabricated success
- **Status:** Open

### Description

`markDelivered` commits the replay fingerprint **before** any work, and the `webhook_events` row is written `status='processed'` with the matched rule ids **before** the job inserts. None of it is in a transaction.

With job insertion failing:

```
POST (valid sig, delivery D) -> HTTP 500 {"enqueued":0,"error":"failed to enqueue job..."}
POST (same D — the retry a 500 is supposed to elicit) -> HTTP 409 duplicate delivery

webhook_events:     ... | processed | matched_rules=["80c21bf1-122"]
queue_jobs:         0 rows
tag webhook events: Status "processed"
```

The HTTP 500 is correct. The **state of record lies**, and the retry path is closed forever.

### Root Cause

`internal/webhook/webhook.go:317-323` (fingerprint first), `webhook.go:347-364` (event + job inserts outside any transaction). Also mine.

Reachable without fault injection: no server does a graceful shutdown, so a signal mid-request severs the handler at exactly this point.

### Proposed Fix

One transaction over fingerprint + event + jobs, or record the fingerprint last.

### Verification

Induce an insert failure: the retry must be accepted, and no `processed` row may exist with zero jobs for a matched rule.

---

## ISSUE-009 — permission rule KEYS are unvalidated; a typo silently widens the policy

- **Severity:** P1
- **Priority:** P1
- **Area:** Permission model
- **Type:** Security / Authorization
- **Status:** Open

### Description

Rule *values and types* are strictly validated (fixed earlier this week). **Keys are not checked at all**, and the zero value of both `Pattern` and `Tool` is the permissive one.

```yaml
- tool: write_file
  path: "*.md"        # the field is `pattern:` — this is a typo
  action: allow
```
```
$ tag permissions show
1. write_file path:* = allow [config:rules]      # every subject. exit 0, no warning
```

Enforced, not cosmetic: the auditing agent drove a real `write_file` through the agent loop and a non-`.md` file was overwritten.

Full typo surface, all exit 0: `path:`/`command:`/`subject:`/`glob:` for `pattern:` → every subject; `tol:` for `tool:` → every tool; `rulez:`/`permisions:` → policy silently dropped.

### Root Cause

`internal/permission/config.go:30-156` type-asserts every value but never enumerates the block's keys. Empty `Pattern` matches any subject (`permission.go:117-119`); absent `tool` defaults to `"*"` (`config.go:94`).

### Proposed Fix

Enumerate allowed keys per rule block; an unknown key refuses the load — the same doctrine already applied to values.

### Verification

Each typo above exits 2 naming the unknown key. Correct configs still load.

---

## ISSUE-010 — a typo'd `--profile` silently discards that profile's permission policy

- **Severity:** P1
- **Priority:** P1
- **Area:** Permission model / profiles
- **Type:** Security / Authorization
- **Status:** Open — **CONFIRMED**

### Description

Reported by the audit agent: with a per-profile deny rule, `--profile orchestrator` enforces it, while `--profile orchestratr` (one character) silently loads builtin defaults, and with `--auto-approve` the denied `bash` command executed — destroying a probe directory. The stderr note actively reassures that "deny rules still apply".

### Confirmation

My first two reproduction attempts were invalid — I edited the YAML by hand and created duplicate keys both times, so the run failed on *that* instead. Re-done with a real YAML parser, merging the policy into the existing profile:

```
$ tag permissions show --profile orchestrator      exit 0,  deny rule PRESENT
$ tag permissions show --profile orchestratorXX    exit 0,  deny rule ABSENT
```

A typo'd profile silently resolves to builtin defaults and exits 0. The auditing agent additionally demonstrated the enforcement consequence: with `--auto-approve`, a denied `rm` executed and destroyed a probe directory, while stderr printed "deny rules still apply".

The code path: `internal/cli/permissions.go:118-129` does `profCfg[prof].(map[string]any)` with an `if ok` that falls through silently rather than erroring, and `ensureProfileExists` (`internal/cli/routing.go:375-380`) exists but is not on this path. Same shape at `routing.go:150` and `mem2.go:564`.

### Verification

Both invocations above must agree, or the typo'd one must exit non-zero.

### Proposed Fix

`if !ok { return error }` — an unknown profile is a usage error, not a silent downgrade.

---

## ISSUE-011 — credentials stored verbatim in the permission audit table and printed by `tag permissions log`

- **Severity:** P1
- **Priority:** P1
- **Area:** Permission audit / secret hygiene
- **Type:** Security / Information disclosure
- **Status:** Open

### Description

No redaction exists anywhere in `internal/permission/`. Both `subject` and `args_summary` are stored raw, twice per row, for allowed **and denied** calls:

```
subject      = curl -H "Authorization: Bearer sk-LEAKPROBE-9988" https://…
args_summary = {"command":"curl -H \"Authorization: Bearer sk-LEAKPROBE-9988\" …"}
args_summary = {"content":"API_KEY=sk-LEAKPROBE-7766","path":"out.txt"}
```

`tag permissions log` prints them to stdout. A full-DB dump showed `permission_decisions` is the **only** leaking table — spans and runs were clean.

Compounded by ISSUE-014: that DB is world-readable.

### Root Cause

`internal/permission/audit.go:31-40`. `SummarizeArgs` truncates to 200 chars but never redacts — ample for a key.

### Proposed Fix

Redact credential-shaped substrings before the insert. The detector already exists (`IsCredentialPath`, `CommandTouchesCredentialPath`).

### Verification

Run the leak probe; grep every table; zero hits.

---

## ISSUE-012 — provider error bodies relayed to stderr and persisted to `spans.error_msg`

- **Severity:** P1
- **Priority:** P1
- **Area:** LLM provider / secret hygiene
- **Type:** Security / Information disclosure
- **Status:** Open

### Description

Against an upstream that reflects the `Authorization` header in its error body — LiteLLM-style gateways and misconfigured proxies do — the key reaches stderr and four `spans.error_msg` rows:

```
error: openai API 401: {"error":{"message":"Incorrect API key provided: Bearer sk-FAKEKEY-4455"}}
```

### Root Cause

`internal/llm/openai.go:84-86` interpolates the upstream body into the error with no scrubbing.

### Proposed Fix

Scrub credential-shaped tokens from upstream bodies before they enter an error, a log, or a span.

### Verification

Mock upstream that echoes the header; the key appears nowhere in stderr or any table.

---

---

## P2 / P3 findings

Full templates are reserved for P0/P1. These carry the fields that drive the fix; each was reproduced by an auditing agent with a concrete command.

### P2 — important, workaround exists

| # | Area | Finding | Root cause |
|---|---|---|---|
| 013 | Onboarding | **`doctor` passes on a broken install.** 4 checks, none touch the DB. Exit 0 all-✓ for: corrupt DB, deleted DB, unwritable `runtime/`, empty config, `master_profile` as a list, `master_profile: ghost`. Meanwhile `runs list` on the same corrupt DB exits 1 with a good message — the signal exists, `doctor` never asks | `internal/cli/doctor.go` |
| 014 | Sandbox | **macOS sandbox is a deny-list** — `(allow default)`. Writes outside the run dir succeed while the banner says "run dir read/write allowed". Linux is allow-list; the claim string does not distinguish them. No rlimits on darwin | `isolation_darwin.go:101,166` |
| 015 | Sandbox | Credential guards keyed to `$HOME`; repointing it accepted `--dir /Users/sanskar` and listed the real `~/.ssh` while the banner claimed it was denied. `HOME` unset → runs with "WEAKENED" note (honest, but fail-open) | `internal/sandbox` |
| 016 | Storage | **`~/.tag` and the DB are world-readable** (`0755`/`0644`, hardcoded not umask-derived) — and that DB holds the unredacted credentials of ISSUE-011 | `config.go:70,72`; `store.go:43` |
| 017 | Permission | bash credential guard bypassed by shell indirection: `f=.en; g=v; cat ${f}${g}` reads `.env`. Documented as a token scan, but the deny message reads as a hard control | `credcmd.go:18-25` |
| 018 | Config | `set-model` strips every comment from `tag.yaml`, including the ~25-line commented `permissions:` block that is the only in-product documentation of the consent gate | config writer |
| 019 | Config | Non-strict decoding: unknown top-level keys accepted, wrong types silently defaulted. `model: "a-string"` where a map belongs → whole model block vanishes, `tag models` exits 0 reporting `current: -` | `config.go:92,183-200` |
| 020 | Storage | Losing the DB mid-run reports success — exit 0, "done in 2 step(s)", then `runs list` → "No runs found" | run recorder |
| 021 | Config | Shipped default config points 4 of 5 profiles at `openrouter`, which the binary does not implement. `run --profile coder` silently ran **echo** while recording `model_id=qwen/qwen3-coder` | default config vs `llm.Registry` |
| 022 | Gateway | Every upstream status collapses to **502** — 429, 401 and 500 alike. Client rate-limit backoff never fires; a permanent credential failure is retried as transient | `gateway.go:186,205,233` |
| 023 | Gateway | An **unparseable upstream stream is reported as a successful empty completion** with `finish_reason:"stop"`. The caller cannot tell "the model said nothing" from "the response was garbage" | `gateway.go:191-209` |
| 024 | Webhook | Oversized body is **truncated, then reported as `invalid signature`** — a legitimate large delivery is diagnosed as an auth failure. Should be 413 | `webhook.go:292` |
| 025 | Webhook | `webhook_events` stores full raw payloads with **no retention**; `pruneDeliveries` covers the other table and runs once at construction. One 9 MiB delivery grew the DB to 28 MB | `webhook.go:347`; `replay.go:93-99` |
| 026 | Webhook | `rule-add` accepts a **nonexistent profile** and reports success; the rule then enqueues real jobs bound to it | `cli/webhook.go:44-51` |
| 027 | Webhook | `--platform generic` is rejected by `rule-add` but `/webhook/generic` accepts events — the generic path can never enqueue anything | `cli/webhook.go:48` vs `webhook.go:138` |
| 028 | Gateway | `--profile <nonexistent>` starts silently and serves `"model": ""`, breaking client-side routing. Same silent-profile shape as ISSUE-010 | `cli/gateway.go:57` |

### P3 — minor

| # | Area | Finding |
|---|---|---|
| 029 | Gateway | Oversized body → 400 not 413; `Authorization: bearer` (lowercase) → 401 though RFC 7235 makes the scheme case-insensitive; `GET /v1/unknown` returns `text/plain` |
| 030 | Webhook | `/v1/models` and `/webhooks/rules` accept any method; `/webhooks/rules` returns `null` on a fresh home (the CLI form correctly returns `[]`); the bare secret is accepted without `Bearer ` |
| 031 | Webhook | `rule-add` usage errors exit **1**, contract says 2 — `usageErr{}` is used by 16 other call sites but not in `cli/webhook.go` or `cli/gateway.go` |
| 032 | Webhook | `webhook events --json` uses PascalCase keys, unlike every other `--json` surface; `--limit -1` returns all rows |
| 033 | Webhook | `FilterLabels` is unreachable from the CLI — `CreateRule(..., nil)` hardcoded, no `--label` flag; the matching logic is dead code |
| 034 | Gateway | Ignores `stream_options.include_usage`; unknown bare models served silently (OpenAI returns 404); unknown message roles coerced to `user` |
| 035 | All servers | **No graceful shutdown anywhere** — no `srv.Shutdown` in the repo. A signal severs in-flight requests, which is what makes ISSUE-008's window reachable without fault injection |
| 036 | MCP | `inputSchema` advertised but not enforced — `tools/call echo` with no `text`, or `text: 123`, both succeed |
| 037 | Permission | `permissions show` flags only the credential-outranking case, never general rule shadowing; the `--json` form drops even that annotation |

---

## ISSUE-038 — [P0, FIXED] the eval and CI gates scored the submission receipt

- **Severity:** P0 · **Area:** Python eval · **Status:** **Resolved**

`tag eval-ci run` scored `proc.stdout` — the human-readable receipt (`run_id: … status: queued / researcher: ok`) — so a suite asserting `"status: queued"` reported **100% pass with no model ever invoked**. `tag eval run` scored `steps[].output`, which in the default kanban execution mode is the created *task's* JSON, so any case asserting a word from its own prompt (or `ready`/`researcher`/`coder`/`scratch`) passed unconditionally for every prompt.

**This is partly my own regression.** I fixed `cmd_eval` in #676 and left `eval_ci.py` untouched — including its comment claiming to use "the same path the `tag eval run` command uses", which had silently stopped being true.

- **Fix:** one shared `eval_framework.extract_model_output`, used by both, which additionally rejects a Kanban task record as "not a model answer" and refuses to score rather than scoring the wrong text.
- **Verified:** 3 new tests including one asserting the two scorers share an implementation; confirmed failing against the reverted code. Full suite 873 passed.
- **Regression risk:** a suite that was passing on receipt text will now fail — correctly.

## ISSUE-039 — [P1, FIXED] stored XSS in the DevUI dashboard, both distributions

- **Severity:** P1 · **Area:** Web dashboard · **Status:** **Resolved**

`badge()` interpolated a DB value into an HTML `class` attribute. Go escaped the text node but **not the attribute**; Python escaped **neither**. Verified in real headless Chromium: `window.pwned === 1` (Go) / `2` plus `document.title = "PWND"` (Python), and the injected script read `/api/memories` same-origin. Sinks are `spans.status`, `eval_runs.status`, `alert_firings.severity` — all written from model- and tool-influenced execution outcomes.

- **Fix:** the class is restricted to `[a-z0-9_-]` by construction (stronger than escaping) and the text node is escaped in both distributions.
- **Verified:** the payload now yields `class="badge-imgsrcqonerrorwindowpwned1"` — inert.

## ISSUE-040 — [P0, FIXED] `webhook listen` had no bind guard

- **Severity:** P0 · **Area:** Webhook · **Status:** **Resolved** — see ISSUE-006.
- **Fix:** the gateway's `isLoopbackHost` guard, applied. `--host 0.0.0.0 --allow-unsigned` now exits 1.

## ISSUE-041 — [P0, FIXED] cross-platform signature confusion defeated replay protection

- **Severity:** P0 · **Area:** Webhook · **Status:** **Resolved** — see ISSUE-007.
- **Fix:** the platform segment is allowlisted (404 otherwise), and the replay fingerprint no longer includes the caller-controlled platform. Verified `200, 409, 409, 404, 404`.
- **Note:** my own earlier test asserted "fingerprints must be namespaced per platform" — the very property that *was* the vulnerability. Corrected.

## ISSUE-042 — [P1, FIXED] `security scan --max-files <= 0` failed open

- **Severity:** P1 · **Area:** Security scanning · **Status:** **Resolved**

Walked zero files, printed `✓ No secrets found`, exit 0, and persisted a `status='ok'` scan row — a green CI gate over an unscanned tree. The sibling `workspace index --max-files` already rejected this; the guard was simply missing.

- **Fix + verified:** `--max-files 0/-1` → exit 2; a real scan still finds the secret and exits 1.

## ISSUE-043 — [P1, FIXED] `template export` leaked secrets it claimed to redact (Go)

- **Severity:** P1 · **Area:** Templates · **Status:** **Resolved (Go); Python OUTSTANDING**

Redaction was a name-only allowlist, so `PRIVATE_KEY`, `STRIPE_SK`, `GH_PAT`, `DB_PASS`, `SESSION_COOKIE`, `AWS_ACCESS_KEY_ID` exported verbatim — from a command whose help says "secrets redacted" and whose output feeds `marketplace push`.

- **Fix:** value-shape detection (11 credential patterns) plus widened, word-boundary-anchored name patterns. `BYPASS_CACHE` and `PASSTHROUGH` deliberately still pass.
- **Verified:** 2 regression tests, confirmed failing against the old regex.
- **Still open:** the Python mirror (`src/tag/cmd/workflow_mgmt.py:335-343`) leaks `SLACK_WEBHOOK`.

---

## Outstanding — ship blockers

| # | P | Area | Finding |
|---|---|---|---|
| 044 | P0 | Python sandbox | **`sandbox run` can write to the real `$HOME`.** Reads are denied, writes are not — the profile has no `file-write*` deny for `$HOME`. Overwriting `~/.zshrc` is code execution outside the sandbox. `src/tag/sandbox.py:160-176` |
| 045 | P1 | Go `run` | **`run --tools` orphans its shell children on SIGINT/SIGTERM** — `sleep 300` reparented to init. `run`, `shell` and `benchmark run` lack the `signal.NotifyContext` every other family has. Model-authored commands survive the operator's Ctrl-C |
| 046 | P1 | Go `run` | **`run` on a read-only `TAG_HOME` reports success and persists nothing**, empty stderr. `run.go:147` and `:82` discard the open error. `queue add` gets this right |
| 047 | P1 | Go `logs` | **`tag logs --limit -1` panics** with a Go stack trace (`logs.go:75`) |
| 048 | P1 | Python | **5 commands are 100% non-functional** — wrong call signatures / unresolvable imports: `agentic-ci ci-diagnose`, `agentic-ci review`, `agentic-ci gen-pipeline`, `swe-solve`, `eval-judge run`, `desktop build`. None can ever have been executed |
| 049 | P1 | Python | **`plugin enable/disable` writes to a phantom directory** the runtime never reads — success always fabricated. `routing.py:693,718` use `tag_home()/profiles` instead of `profile_home()`; the same bug was fixed in `workflow_mgmt.py` and missed here |
| 050 | P1 | Both | **Cross-distribution schema drift**: Python crashes on a Go-created home (`cron_jobs.last_run_at`, `tool_index_meta.registry_mtime`, …). Both use `CREATE TABLE IF NOT EXISTS`, so whoever creates the DB first wins. No corruption, but 3 commands die |
| 051 | P1 | Python | **`template export` leaks `SLACK_WEBHOOK`** — the Python half of ISSUE-043 |
| 052 | P1 | Webhook | **A partially-failed delivery is recorded `processed`, enqueues nothing, and permanently rejects the retry** (ISSUE-008). Fingerprint committed before the work, no transaction |
| 053 | P1 | Permission | **Rule KEYS are unvalidated** — `path:` for `pattern:` silently widens to every subject (ISSUE-009) |
| 054 | P1 | Permission | **A typo'd `--profile` silently discards the profile's policy** (ISSUE-010, confirmed) |
| 055 | P1 | Permission | **Credentials stored verbatim in the audit table** and printed by `permissions log` (ISSUE-011) |
| 056 | P1 | LLM | **Provider error bodies relayed to stderr and persisted to spans** (ISSUE-012) |

## Outstanding — P2, grouped by root cause

**Exit codes lie in batch commands** — `queue worker`, `dag run --execute`, `benchmark run` (Go) and `benchmark` (Python) all exit **0** with everything failed. Any CI wired to these is green on total failure.

**Second-precision timestamps are load-bearing** — one root defect produces a non-FIFO queue, a `dag state` that resolves the wrong run, and a `tag logs` that returns the *oldest* rows. `runs.created_at` is second-precision while `spans.started_at` is microsecond, and the lexicographic sort puts every run above every span.

**Unearned success claims** — `queue` reports `done`/`exit_code 0` for work only *queued*; `trace snapshot <unknown>` claims a snapshot with zero rows written; `loop` decides continue/stop from the same receipt; `mcp-registry`/`plugin` enable names that do not exist; `import-*` print "✓ configured" with no reachability check.

**`tag run`/`tag shell` are the least-finished commands** — no result persistence (`steps` is never written, so `runs show` can never show an answer), no run row on failure, and `shell` is stateless, untraced, and exits 0 on a fully broken session.

**No graceful shutdown anywhere** — no `srv.Shutdown` in the repo; this is what makes ISSUE-052's window reachable without fault injection.

**`doctor` passes on a broken install** (both distributions differ: Go's is all-green on a corrupt DB; Python's is accurate in both directions).

**HTTP hygiene** — no `Host` validation (DNS rebinding defeats the loopback bind), wrong methods return 200/501 instead of 405, no server-side timeouts, `/api/memories` emits a mangled embedding BLOB, no security headers.

Plus ~45 further P2/P3 items across `--json` contract inconsistency, exit-code drift (25 of 30 usage failures return 1 instead of 2), silent empty output, unvalidated numeric flags, and stale help text. Full per-agent detail is preserved in the audit transcripts.
---

## Negative results (checked, found clean)

Recorded so the next audit does not repeat them.

- **"Print an error, return 0"** across the whole Python CLI: no `print_error(...)` is followed by `return 0`. Previously a live shape; now clean.
- **`--json` error objects**: every Go command sampled except those in ISSUE-001/003 emits a parseable error object under the global `--json`.
- **Unbounded `io.ReadAll`** in the Go harness: only the two stdin sites above; every network, subprocess and file path is bounded.
- **Malformed document input**: a truncated PDF, 200 KB of random bytes, an empty file, a non-PDF with a `.pdf` name, a directory and a missing path all fail with an explaining message and no crash. Only the exit *code* is wrong (ISSUE-005).
- **`omitempty` on bools**: five remain (`memory/fact.go:82`, `guardrail/guardrail.go:176`, `eval/run.go:91,92,99`). All are *negative-polarity* flags (`unchecked`, `trivial`, `errored`, `undecidable`) where absence correctly reads as false — unlike `converged`, where absence erased the failing case. Left as-is deliberately.

### Confirmed by the auditing agents as HOLDING (negative results, second batch)

Each verified by a live invocation, not by reading code. These are the security properties that survived the audit.

**Permission model** — deny rules hold for command *and* path subjects; `ask` with no TTY denies immediately with actionable text; blanket `allow: "*"` does **not** grant credential paths while a rule naming the path does (the historical bug stays fixed); `--auto-approve` overrides neither deny rules nor credential guards; `--dangerously-allow-all` does what it says, warns on stderr not stdout, and is audited; malformed rule *values and types* refuse to load; the approval gate times out to deny and is answerable cross-process with a sha256 args binding.

**Sandbox** — fails closed on `--dir /`, `$HOME` and `~/.ssh` (exit 127); from a normal run dir `~/.aws/credentials` and `~/.ssh/*` are unreadable, `cp` exfil is blocked, and a symlink pivot from inside the run dir is blocked; network denied by default on darwin; granular egress on the restricted backend is refused rather than silently unenforced; Docker absent → nothing runs.

**Storage** — 24 simultaneous writers all succeeded with `integrity_check ok`; a 10s foreign exclusive lock was waited out with no corruption or lost write; **the old-schema migration shipped this week holds** (a 10-column legacy `runs` table migrated to 19 on open with the legacy row preserved).

**Secret hygiene** — the API key travels only in the `Authorization` header, never the body; trace spans carry no tool arguments; a full-DB sweep found `permission_decisions` to be the *only* leaking table.

**Gateway** — round-trips cleanly against the **real `openai` Python SDK 2.24.0** (models.list, non-stream, streaming accumulation, and error mapping to AuthenticationError/BadRequestError/NotFoundError); SSE wire format is byte-correct; auth is checked *before* the body is read; a mid-stream upstream close produces an error, **never a false `stop`**; 20 concurrent aborted SSE streams left socket count back at baseline — **no goroutine leak**.

**Webhook** — signature verification is correct for github/slack/linear *within* a platform, including Slack timestamp freshness in both directions; replay protection holds under concurrency (20 threads firing the identical delivery → exactly 1×200 and 19×409, one row each in the DB); 60 concurrent distinct deliveries in 0.12 s with zero `SQLITE_BUSY`; rule matching is honest — an unmatched event enqueues nothing, verified in SQLite.

**MCP** — full protocol exercise passes, including `-32700` on a malformed frame with the stream staying synchronised, and empty stderr (required for a stdio transport).
