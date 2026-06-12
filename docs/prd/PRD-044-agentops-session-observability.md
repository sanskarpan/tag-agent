# PRD-044: AgentOps Session Observability (`tag config set agentops.api_key`)

**Status:** Proposed
**Priority:** P3 — Nice-to-have
**Estimated Effort:** S (2–3 days)
**Category:** Observability
**Affects:** `src/tag/controller.py` (config set/get/unset for `agentops.*`), `src/tag/integrations/agentops_bridge.py` (new), `src/tag/hermes_bridge.py` (callback injection)
**Depends on:** PRD-013 (agent tracing/observability), PRD-012 (cost tracking) — complementary, not blocking
**Inspired by:** AgentOps Python SDK (`agentops.ai`) — session recording, LLM event tracking, tool call capture, cost attribution at the session level

---

## 1. Overview

TAG already captures comprehensive observability data: SQLite-backed span traces (PRD-013), per-run cost attribution (PRD-012), and optional OTLP export (PRD-041). However, all of this data stays inside TAG's own storage layer. Developers who use AgentOps across their agent stack — whether LangChain flows, CrewAI pipelines, or custom agents — have no way to see TAG runs alongside those other agents in the AgentOps dashboard.

This PRD adds an optional, zero-configuration-overhead integration with the AgentOps Python SDK. When a user sets `agentops.api_key` in their TAG config, every subsequent `tag run` automatically starts an AgentOps session, emits LLM call events (model, tokens, latency), tool call events, and error events, and closes the session on completion. When the key is not set, the integration is completely inert — no imports, no overhead, no network calls.

The feature is purely additive and opt-in. It sits alongside, not in place of, TAG's existing tracing infrastructure. AgentOps session IDs are stored in TAG's own `traces` table for cross-referencing, and `tag trace agentops show <session_id>` opens the AgentOps dashboard URL for that session.

---

## 2. Problem Statement

### 2.1 TAG is invisible to third-party observability platforms

TAG's traces live in a SQLite database at `~/.local/share/tag/tag.db`. They are visible via `tag trace list` / `tag trace show` and can be exported to any OTLP-compatible backend. But developers who have invested in AgentOps as their cross-agent observability layer cannot get TAG runs into their AgentOps project without writing custom integration code. The barrier is high enough that most users simply accept the observability gap.

### 2.2 Multi-stack agent monitoring requires manual correlation

A typical enterprise AI workflow might chain: a LangGraph orchestrator → TAG for code tasks → a CrewAI agent for research. AgentOps can automatically instrument LangGraph and CrewAI agents. TAG cannot currently participate in that session graph, so developers must manually correlate TAG trace IDs with AgentOps session logs by timestamp — tedious and error-prone.

### 2.3 Cost attribution across agent types requires a unified view

PRD-012 gives TAG its own cost tracking, but finance teams and platform engineers often need cost attribution across all agent types in a single dashboard. AgentOps provides exactly this dashboard. Without TAG integration, TAG's costs are missing from the AgentOps cost report.

### 2.4 Existing OTel integration (PRD-013 / PRD-041) does not cover AgentOps natively

While AgentOps can consume OTLP traces (PRD-041 makes TAG spans OTel-compatible), AgentOps also has a richer native SDK that captures structured session metadata, tool call semantics, and LLM pricing data in a form that produces better UI in the AgentOps dashboard than raw OTLP spans. This PRD targets the native SDK path, which is complementary to and richer than the OTLP path.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | A single `tag config set agentops.api_key <key>` command enables full AgentOps observability for all subsequent `tag run` invocations. |
| G2 | Every `tag run` when the key is set starts an AgentOps session, emits all LLM call events, tool call events, and error events, and closes the session on completion or failure. |
| G3 | Zero overhead and zero imports when `agentops.api_key` is not configured. No `import agentops` at module load time. |
| G4 | The AgentOps SDK is an optional dependency; TAG continues to work normally if `agentops` is not installed, with a clear install instruction when the key is configured. |
| G5 | Each TAG trace row in SQLite stores the AgentOps session ID for cross-referencing. |
| G6 | `tag trace agentops show <run_id>` opens the AgentOps dashboard URL for that run's session. |
| G7 | API key is stored masked in `tag config get agentops.api_key` output (show only last 4 characters). |
| G8 | `tag config unset agentops.api_key` removes the key and disables the integration. |

## 3.1 Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Replacing TAG's own SQLite tracing with AgentOps. TAG's internal traces remain the primary storage; AgentOps is a secondary, optional export. |
| NG2 | Sending full conversation history or file contents to AgentOps. Only structured event metadata (model, tokens, latency, tool names, error types) is sent. |
| NG3 | AgentOps-specific dashboards or views within TAG's own TUI. The AgentOps web dashboard is the UI for AgentOps data. |
| NG4 | Supporting other third-party observability SDKs (LangSmith, Arize, Braintrust) in this PRD. This PRD is AgentOps-specific. A generic callback hook system is out of scope. |
| NG5 | Automatic AgentOps session linking across a multi-TAG-agent swarm (PRD-023). Session linking is a future extension. |
| NG6 | Modifying AgentOps session data after the run completes. All events are emitted in real time; no retroactive backfill of historical runs. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Setup time | User can see TAG runs in AgentOps dashboard within 2 minutes of running `tag config set agentops.api_key` | Manual timing test |
| Zero-overhead guarantee | `tag run` wall time with key unset is statistically identical to wall time before this feature | Benchmark 20 runs; t-test on wall time |
| Import isolation | `import tag.controller` does not import `agentops` when key is unset | `sys.modules` assertion in unit test |
| Event completeness | Each `tag run` produces ≥ 1 AgentOps LLM event per Hermes inference step | Verified via AgentOps test session API |
| Session ID persistence | `tag trace show <id>` displays `agentops_session_id` field after an instrumented run | Integration test |
| SDK absence handling | `tag run` with key set but `agentops` not installed prints actionable install hint and proceeds without crashing | Unit test with mocked ImportError |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|------------|----------|
| U1 | Developer using AgentOps | run `tag config set agentops.api_key sk-ao-...` once | All my TAG runs appear in the AgentOps dashboard with full LLM event detail, alongside my other agents |
| U2 | Platform engineer | see TAG's token costs in the AgentOps cost attribution dashboard | I have a single source of truth for agent costs across LangChain, CrewAI, and TAG without manually exporting CSVs |
| U3 | Developer | run `tag trace agentops show abc123` | I can jump directly from a TAG run ID to the AgentOps session replay in my browser |
| U4 | Security-conscious developer | run `tag config get agentops.api_key` and see only `sk-ao-...****abcd` | I can confirm the key is set without accidentally exposing it in a screen share or log file |
| U5 | Developer | run `tag config unset agentops.api_key` | I can immediately stop sending data to AgentOps without modifying YAML files manually |
| U6 | Developer without `agentops` installed | set the API key and get a clear error | I know exactly what `pip install` command to run, and my other `tag run` functionality is not broken |
| U7 | Developer debugging a TAG run | see which tool calls TAG made in the AgentOps session replay | I can cross-reference TAG's tool call trace with the LLM events in a rich timeline UI |

---

## 6. Proposed CLI Surface

### 6.1 `tag config` extensions

```sh
# Set the key (stored in TAG config YAML, masked on display)
tag config set agentops.api_key sk-ao-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx

# Get the key (masked: shows only last 4 chars)
tag config get agentops.api_key
# Output: agentops.api_key = sk-ao-****************************abcd

# Remove the key and disable AgentOps integration
tag config unset agentops.api_key

# Additional optional settings
tag config set agentops.endpoint https://api.agentops.ai   # default, can override for self-hosted
tag config set agentops.tags myteam,production             # comma-separated session tags
tag config get agentops.tags
tag config unset agentops.tags
```

### 6.2 `tag trace agentops` subcommand

```sh
# Open AgentOps session URL for a TAG run
tag trace agentops show <run_id>
# Prints: Opening https://app.agentops.ai/sessions/<agentops_session_id>
# Then opens URL in default browser via `python -m webbrowser`

# Show the AgentOps session ID without opening browser
tag trace agentops show <run_id> --no-open
# Output: agentops_session_id = <uuid>
```

If `agentops_session_id` is not set on the run (i.e., the run was executed before AgentOps was configured, or the key was not set at the time), the command prints:

```
No AgentOps session ID found for run <run_id>.
Run 'tag config set agentops.api_key <key>' to enable AgentOps observability.
```

### 6.3 `tag run` output with AgentOps enabled

When AgentOps is active, `tag run` prints one additional line after the run summary:

```
AgentOps session: https://app.agentops.ai/sessions/f47ac10b-58cc-4372-a567-0e02b2c3d479
```

This line is suppressed when `--quiet` is passed.

---

## 7. Technical Design

### 7.1 Conditional import pattern

The `agentops` SDK is never imported at module load time. All imports happen inside the `AgentOpsBridge` class methods, guarded by `importlib.util.find_spec`:

```python
# src/tag/integrations/agentops_bridge.py

import importlib.util
from typing import Any

_AGENTOPS_AVAILABLE: bool | None = None  # None = not yet checked

def _check_agentops_available() -> bool:
    global _AGENTOPS_AVAILABLE
    if _AGENTOPS_AVAILABLE is None:
        _AGENTOPS_AVAILABLE = importlib.util.find_spec("agentops") is not None
    return _AGENTOPS_AVAILABLE

class AgentOpsBridge:
    """
    Optional bridge to the AgentOps Python SDK.
    All methods are no-ops when api_key is None or agentops is not installed.
    """

    def __init__(self, api_key: str | None, endpoint: str | None = None, tags: list[str] | None = None):
        self._api_key = api_key
        self._endpoint = endpoint or "https://api.agentops.ai"
        self._tags = tags or []
        self._session: Any = None
        self._session_id: str | None = None
        self._enabled = bool(api_key) and _check_agentops_available()

    @classmethod
    def from_config(cls, cfg: dict) -> "AgentOpsBridge":
        """Construct from TAG config dict. Returns a no-op bridge if key is absent."""
        agentops_cfg = cfg.get("agentops", {})
        api_key = agentops_cfg.get("api_key")
        endpoint = agentops_cfg.get("endpoint")
        tags_raw = agentops_cfg.get("tags", "")
        tags = [t.strip() for t in tags_raw.split(",") if t.strip()] if tags_raw else []
        return cls(api_key=api_key, endpoint=endpoint, tags=tags)
```

### 7.2 Session lifecycle

```python
class AgentOpsBridge:

    def start_session(self, run_id: str, task: str, profile: str) -> str | None:
        """
        Start an AgentOps session. Returns the AgentOps session ID, or None if disabled.
        """
        if not self._enabled:
            return None
        import agentops
        agentops.init(
            api_key=self._api_key,
            endpoint_url=self._endpoint,
            default_tags=self._tags + [f"tag_profile:{profile}"],
            auto_start_session=False,
        )
        self._session = agentops.start_session(tags=[f"run_id:{run_id}"])
        self._session_id = str(self._session.session_id)
        return self._session_id

    def end_session(self, status: str = "Success") -> None:
        """
        End the current AgentOps session.
        status: "Success" | "Fail" | "Indeterminate"
        """
        if not self._enabled or self._session is None:
            return
        import agentops
        self._session.end_session(end_state=status)
        self._session = None

    @property
    def session_id(self) -> str | None:
        return self._session_id
```

### 7.3 Event mapping: TAG → AgentOps

Every event type that TAG emits internally has a corresponding AgentOps SDK call. The mapping is:

| TAG event | AgentOps SDK call | Key fields sent |
|-----------|-------------------|-----------------|
| Hermes LLM inference step (start) | `agentops.record(LLMEvent(...))` | `model`, `prompt` (first 500 chars), `params` |
| Hermes LLM inference step (end) | Update same `LLMEvent` | `completion` (first 500 chars), `prompt_tokens`, `completion_tokens`, `cost` |
| Hermes tool call (start) | `agentops.record(ActionEvent(...))` | `action_type="tool_call"`, `params={"tool": name, "input": ...}` |
| Hermes tool call (end) | Update same `ActionEvent` | `returns` (truncated), `logs` |
| Hermes agent error | `agentops.record(ErrorEvent(...))` | `exception` or `message` |
| `tag run` completion | `session.end_session(end_state="Success")` | — |
| `tag run` error/abort | `session.end_session(end_state="Fail")` | — |

**LLM event construction:**

```python
def record_llm_call(
    self,
    model: str,
    prompt_text: str,
    completion_text: str,
    prompt_tokens: int,
    completion_tokens: int,
    latency_ms: float,
    cost_usd: float | None = None,
) -> None:
    if not self._enabled:
        return
    from agentops import LLMEvent
    event = LLMEvent(
        model=model,
        prompt=prompt_text[:500],
        completion=completion_text[:500],
        prompt_tokens=prompt_tokens,
        completion_tokens=completion_tokens,
        cost=cost_usd,
    )
    event.latency = latency_ms / 1000.0  # agentops expects seconds
    self._session.record(event)
```

**Tool call event construction:**

```python
def record_tool_call(
    self,
    tool_name: str,
    tool_input: dict,
    tool_output: str | None,
    duration_ms: float,
    error: str | None = None,
) -> None:
    if not self._enabled:
        return
    from agentops import ActionEvent
    event = ActionEvent(
        action_type="tool_call",
        params={"tool": tool_name, "input": str(tool_input)[:300]},
        returns=str(tool_output)[:300] if tool_output else None,
        logs=error,
    )
    self._session.record(event)
```

### 7.4 Hermes bridge callback injection

`src/tag/hermes_bridge.py` is extended to accept an optional `AgentOpsBridge` and register callbacks on the Hermes Agent's event hooks:

```python
# hermes_bridge.py

def attach_agentops(agent: HermesAgent, bridge: "AgentOpsBridge") -> None:
    """
    Register AgentOps event callbacks on an existing HermesAgent instance.
    No-op if bridge is disabled.
    """
    if not bridge._enabled:
        return

    def on_llm_end(event: HermesLLMEvent) -> None:
        bridge.record_llm_call(
            model=event.model,
            prompt_text=event.prompt_text,
            completion_text=event.completion_text,
            prompt_tokens=event.prompt_tokens,
            completion_tokens=event.completion_tokens,
            latency_ms=event.duration_ms,
            cost_usd=event.cost_usd,
        )

    def on_tool_end(event: HermesToolEvent) -> None:
        bridge.record_tool_call(
            tool_name=event.tool_name,
            tool_input=event.input,
            tool_output=event.output,
            duration_ms=event.duration_ms,
            error=event.error,
        )

    def on_error(event: HermesErrorEvent) -> None:
        if bridge._enabled and bridge._session:
            from agentops import ErrorEvent
            bridge._session.record(ErrorEvent(
                trigger_event=event.trigger_event,
                exception=event.exception_repr,
            ))

    agent.on("llm_end", on_llm_end)
    agent.on("tool_end", on_tool_end)
    agent.on("error", on_error)
```

### 7.5 Config schema: `agentops` section

The AgentOps configuration block in the TAG YAML config (`~/.config/tag/default.yaml` or profile YAML):

```yaml
agentops:
  api_key: "sk-ao-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"   # required to enable
  endpoint: "https://api.agentops.ai"                 # optional, default shown
  tags: "myteam,production"                           # optional, comma-separated
```

The `api_key` value is stored verbatim in the YAML file. Display masking is applied only at read time in `tag config get`. The file itself is `chmod 600` (user-read-only) — this is TAG's existing behavior for all config files.

**Config key operations in `controller.py`:**

```python
# Existing pattern extended for agentops.* keys

MASKED_CONFIG_KEYS = {"agentops.api_key", "openrouter.api_key"}

def _display_config_value(key: str, value: str) -> str:
    """Mask sensitive config values for display."""
    if key in MASKED_CONFIG_KEYS and value and len(value) > 4:
        return "*" * (len(value) - 4) + value[-4:]
    return value
```

### 7.6 Session ID storage in traces table

When AgentOps is active and `start_session()` returns a session ID, TAG stores it on the root trace span:

```python
# In controller.py, cmd_run, after bridge.start_session():
if agentops_session_id:
    tracer.set_span_attr(root_span_id, "agentops_session_id", agentops_session_id)
```

The `traces` table already has a `metadata` JSON column (or equivalent) where arbitrary string attributes are stored. No schema migration is required for the initial implementation — the session ID is stored as a span attribute under the key `agentops_session_id`.

If a schema migration is available (PRD-013 defines the traces schema), a dedicated `agentops_session_id TEXT` column may be added in a follow-up to enable indexed lookup.

### 7.7 `tag trace agentops show` implementation

```python
# controller.py, cmd_trace branch

def cmd_trace_agentops_show(args, cfg):
    run_id = args.run_id
    session_id = db.get_span_attr(run_id, "agentops_session_id")
    if not session_id:
        print(f"No AgentOps session ID found for run {run_id}.", file=sys.stderr)
        print("Run 'tag config set agentops.api_key <key>' to enable AgentOps observability.",
              file=sys.stderr)
        sys.exit(1)
    url = f"https://app.agentops.ai/sessions/{session_id}"
    if getattr(args, "no_open", False):
        print(f"agentops_session_id = {session_id}")
    else:
        print(f"Opening {url}")
        import webbrowser
        webbrowser.open(url)
```

### 7.8 SDK not installed: graceful degradation

When `agentops.api_key` is set but the `agentops` package is not installed, `AgentOpsBridge.__init__` sets `self._enabled = False` because `_check_agentops_available()` returns `False`. All bridge methods are no-ops.

Additionally, `controller.py` checks for this condition during `cmd_run` and emits a one-time warning:

```python
bridge = AgentOpsBridge.from_config(cfg)
if cfg.get("agentops", {}).get("api_key") and not bridge._enabled:
    print(
        "Warning: agentops.api_key is set but the 'agentops' package is not installed.\n"
        "Install it with: pip install 'tag-agent[agentops]'\n"
        "Continuing without AgentOps observability.",
        file=sys.stderr,
    )
```

The `pyproject.toml` optional dependency group:

```toml
[project.optional-dependencies]
agentops = ["agentops>=0.3.0"]
```

---

## 8. Security Considerations

### 8.1 API key storage

The AgentOps API key is stored in plaintext in the TAG config YAML file. This is the same approach used for `openrouter.api_key`. The file is created with `chmod 600` (owner read/write only) by TAG's config writer.

Risks:
- A process running as the same user can read the key.
- Backup tools or version control systems may accidentally capture the file.

Mitigations:
- `tag config get agentops.api_key` always masks the key; only the last 4 characters are shown.
- PRD-034 (secret scan) should extend its patterns to detect `agentops.api_key` in committed files.
- Documentation should note that the config file should not be committed to version control.
- A future PRD may add keychain/OS-secret-store integration for API keys (tracked separately).

### 8.2 PII in agent outputs sent to third-party

AgentOps is a third-party cloud service. TAG truncates all prompt and completion text before sending (500 characters for LLM events, 300 characters for tool events — see §7.3). However, even truncated outputs may contain:
- File paths (potentially revealing project structure)
- Variable names, function names (potentially revealing business logic)
- Fragments of code that include secrets (if the agent was operating on a secrets file)

Mitigations:
- Content is always truncated before transmission; full file contents are never sent.
- The feature is opt-in: users who handle sensitive code can simply not set the API key.
- Documentation must clearly state that truncated prompt/completion text is sent to `api.agentops.ai`.
- A future `agentops.send_content: false` config option can suppress all content and send only token counts and latency. This is tracked as OQ-3 below.

### 8.3 Network calls add latency to `tag run`

AgentOps SDK calls are synchronous HTTP requests. Each LLM event record is a POST to `api.agentops.ai`. On a slow network or under AgentOps API degradation, these calls could block `tag run`.

Mitigations:
- Review the AgentOps SDK version chosen (`agentops >= 0.3.0`) for async/batching support. If available, configure the SDK in async mode.
- Wrap all bridge calls in a `try/except Exception` block; any AgentOps failure is logged to stderr as a warning but does not fail the `tag run`.
- Document that enabling AgentOps adds external network dependency to every run.

### 8.4 API key in environment vs. config

Some users may prefer to supply the API key via environment variable rather than config file. Support `AGENTOPS_API_KEY` environment variable as a fallback:

```python
import os
api_key = agentops_cfg.get("api_key") or os.environ.get("AGENTOPS_API_KEY")
```

Environment variable takes the same precedence as the config file key (config key wins if both are set).

### 8.5 AgentOps endpoint override for self-hosted / air-gapped

The `agentops.endpoint` config key (§6.1) allows users to point the SDK at a self-hosted or on-premises AgentOps instance, which keeps all data on-premises. This mitigates the third-party data concern for enterprise users.

---

## 9. Implementation Plan

### Phase 1 — Core integration (day 1)

- [ ] Create `src/tag/integrations/` directory with `__init__.py`
- [ ] Implement `AgentOpsBridge` in `src/tag/integrations/agentops_bridge.py`: `__init__`, `from_config`, `start_session`, `end_session`, `record_llm_call`, `record_tool_call`
- [ ] Add `attach_agentops(agent, bridge)` to `hermes_bridge.py`
- [ ] Add `agentops` optional dependency to `pyproject.toml`
- [ ] Unit tests for `AgentOpsBridge` with mocked `agentops` SDK (no real API calls)

### Phase 2 — Config CLI and trace storage (day 2)

- [ ] Add `agentops.api_key` to `MASKED_CONFIG_KEYS` in `controller.py`
- [ ] Implement `tag config set agentops.api_key` / `tag config get agentops.api_key` / `tag config unset agentops.api_key` (these reuse existing `cmd_config` machinery; only the masking logic is new)
- [ ] Wire `AgentOpsBridge.from_config(cfg)` into `cmd_run` in `controller.py`
- [ ] Store `agentops_session_id` on root span after `start_session()` returns
- [ ] Implement `tag trace agentops show` subcommand in `controller.py`
- [ ] Unit tests for config masking and `tag trace agentops show` path

### Phase 3 — Polish and documentation (day 3)

- [ ] Add "not installed" warning with `pip install 'tag-agent[agentops]'` hint
- [ ] Add `agentops.endpoint` and `agentops.tags` config support
- [ ] Add AgentOps session URL line to `tag run` completion output (suppressed with `--quiet`)
- [ ] `tag config get agentops.endpoint` and `tag config get agentops.tags` (no masking needed)
- [ ] Update `tag --help` / `tag trace --help` to mention AgentOps subcommand
- [ ] Integration test: full `tag run` with `AgentOpsBridge` mock; assert all event types recorded; assert session ID stored in trace

---

## 10. Risks

| ID | Risk | Likelihood | Impact | Mitigation |
|----|------|-----------|--------|------------|
| R1 | AgentOps SDK version compatibility: `agentops >= 0.3.0` API may break between minor releases | Medium | Low — SDK is isolated behind bridge class | Pin to a tested minor version range; add `agentops>=0.3.0,<1.0` in optional deps; update bridge on next breaking change |
| R2 | Synchronous AgentOps HTTP calls add latency on slow networks | Medium | Medium — `tag run` appears slow; user frustration | Investigate async SDK mode; wrap all calls in `try/except`; document the latency trade-off |
| R3 | AgentOps API key stored in plaintext YAML exposes key if file is accidentally committed | Low | High — potential billing fraud if key is public | Add to PRD-034 secret scan patterns; document do-not-commit guidance; support `AGENTOPS_API_KEY` env var as a keychain-safe alternative |
| R4 | PII in truncated content sent to AgentOps violates user's data handling requirements | Low-Medium | High — compliance risk for enterprise | Document clearly; add future `agentops.send_content: false` option; ensure opt-in only |
| R5 | `agentops` SDK not available in certain Python environments (e.g., restricted enterprise PyPI mirrors) | Low | Low | Graceful degradation: bridge is no-op if SDK not installed; one-time warning on run |
| R6 | AgentOps API outage causes `tag run` to hang waiting for HTTP response | Low | High | Set SDK timeout via `agentops.init(max_wait_time=2000)`; catch all exceptions in bridge methods |

---

## 11. Open Questions

| # | Question | Owner | Status |
|---|----------|-------|--------|
| OQ-1 | Should `agentops_session_id` get a dedicated column in the `traces` table or remain a span attribute in the JSON metadata column? A dedicated column enables indexed lookup (`tag trace list --agentops`). | Engineering | Open — recommend span attribute for Phase 1, migrate to dedicated column in a follow-up schema migration |
| OQ-2 | Should `tag run` emit AgentOps events in real time (as each LLM call completes) or batch them at session end? Real-time gives live updates in the AgentOps dashboard during long runs; batching is safer if the SDK has per-event overhead. | Engineering | Open — depends on AgentOps SDK async capabilities; investigate in Phase 1 |
| OQ-3 | Should there be an `agentops.send_content: false` option that suppresses all prompt/completion text from AgentOps events, sending only token counts, latency, and cost? This would address enterprise PII concerns. | Product | Open — recommend implementing in Phase 3 if AgentOps SDK supports content-free LLMEvent |
| OQ-4 | Should `AGENTOPS_API_KEY` environment variable take precedence over the config file key, or vice versa? Standard practice (12-factor) puts env vars above config files. | Engineering | Proposed: env var takes precedence; config file is fallback |
| OQ-5 | Does the AgentOps SDK support custom `session_url` overrides, or is the URL always `https://app.agentops.ai/sessions/<id>`? | Engineering | Needs verification against AgentOps SDK docs; affects `tag trace agentops show` URL construction |
| OQ-6 | Should `tag run --profile X` with AgentOps enabled tag the session with the profile name automatically? | Product | Proposed: yes, always add `tag_profile:<name>` to session tags (implemented in §7.2) |

---

## 12. Acceptance Criteria

| # | Criterion | How to verify |
|---|-----------|---------------|
| AC-1 | `tag config set agentops.api_key sk-ao-test` writes the key to config YAML | Read config YAML directly; assert key is present verbatim |
| AC-2 | `tag config get agentops.api_key` outputs masked key: last 4 chars visible, rest `*` | Run command; assert output matches `agentops.api_key = ****...XXXX` |
| AC-3 | `tag config unset agentops.api_key` removes the key from config YAML | Read config YAML after unset; assert `agentops.api_key` is absent |
| AC-4 | `import tag.controller` does not import `agentops` when key is not set | `python -c "import tag.controller; import sys; assert 'agentops' not in sys.modules"` |
| AC-5 | With a mock AgentOps SDK, `tag run` with key set calls `start_session`, at least one `record(LLMEvent(...))`, at least one `record(ActionEvent(...))`, and `end_session` | Unit test with mock |
| AC-6 | After an instrumented run, `tag trace show <id>` displays `agentops_session_id: <uuid>` | Integration test |
| AC-7 | `tag trace agentops show <run_id>` prints the AgentOps URL and opens the browser (mock `webbrowser.open`) | Unit test |
| AC-8 | `tag trace agentops show <run_id>` with `--no-open` prints `agentops_session_id = <uuid>` without browser call | Unit test |
| AC-9 | When `agentops` is not installed but key is set, `tag run` prints install hint to stderr and completes normally | Unit test with mocked `importlib.util.find_spec` returning `None` |
| AC-10 | An exception raised inside any AgentOps bridge call does not propagate to `tag run`; run completes with exit 0 | Unit test: mock `bridge.record_llm_call` to raise `RuntimeError`; assert `tag run` still exits 0 |
| AC-11 | All existing `tag run` tests pass without modification | CI green |

---

## 13. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| PRD-013 (agent tracing) | Complementary | TAG's existing span infrastructure is used to store `agentops_session_id`. Not a hard blocker — session ID can be stored in a temporary in-memory dict if tracing infra is not available, but full integration requires PRD-013. |
| PRD-012 (cost tracking) | Complementary | `cost_usd` field on LLM events requires PRD-012's per-call cost computation. If PRD-012 is not available, `cost_usd` is omitted (passed as `None`). |
| `agentops>=0.3.0` | Optional Python dep | Installed via `pip install 'tag-agent[agentops]'`; not a required dependency |
| `src/tag/hermes_bridge.py` | Internal | Must expose per-event callbacks (`on("llm_end", ...)`, `on("tool_end", ...)`, `on("error", ...)`). If Hermes Agent does not yet expose these hooks, a thin wrapper polling the Hermes event queue is needed. |
| `src/tag/controller.py` `cmd_config` | Internal | The `set`/`get`/`unset` operations reuse existing config machinery; only masking logic for `agentops.api_key` is new. |

---

## 14. Complexity and Timeline

**Complexity:** S

**Estimated implementation time:** 2–3 days

| Phase | Task | Hours |
|-------|------|-------|
| 1 | `AgentOpsBridge` class, `attach_agentops`, optional dep in `pyproject.toml`, unit tests | 5 |
| 2 | Config masking, `cmd_run` wiring, session ID storage in traces, `tag trace agentops show` | 4 |
| 3 | "Not installed" warning, endpoint/tags config, run completion URL line, integration tests | 3 |
| **Total** | | **12 hours** |

The implementation is well-isolated in `src/tag/integrations/agentops_bridge.py` and the callback hook additions to `hermes_bridge.py`. The largest risk is the Hermes callback hook API — if Hermes does not expose per-event hooks, a polling wrapper over the Hermes event queue is needed, adding ~4 hours. The config masking and CLI subcommand work is straightforward and low-risk.
