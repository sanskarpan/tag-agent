# PRD-018: Context Window & Long-Context Management

**Status:** Proposed  
**Priority:** P1  
**Estimated Effort:** M (2–3 weeks)  
**Affects:** `controller.py` (`cmd_chat`, `run_chat_step`, `hermes_env`), new `tag/context.py`

---

## 1. Overview

As context windows grow to 128k–1M tokens, agents running long tasks can fill them with irrelevant history, causing degraded reasoning, increased cost, and eventually hard errors when the window is exceeded. TAG currently has no context window management — it passes all history to Hermes as-is. This PRD adds per-profile context budgeting, automatic summarization when near the limit, and explicit context controls that users can invoke mid-session.

---

## 2. Problem Statement

- Agents running overnight or multi-hour tasks accumulate massive context histories.
- There is no way to see how much context a profile has used.
- When context fills up, the task fails with a cryptic error.
- Users cannot reset or trim context without losing all session history.
- Cost scales with context size — bloated context burns tokens even for simple follow-up questions.

---

## 3. Goals

1. `tag context status --profile researcher` shows current context usage (tokens used / window size / cost estimate).
2. Per-profile `context.max_tokens` config triggers auto-summarization when approaching the limit.
3. Auto-summarization: compress oldest messages into a summary block, preserving recent context.
4. `tag context trim --profile researcher --keep-last N` manually trims to the N most recent messages.
5. `tag context reset --profile researcher` clears all session context (with confirmation).
6. `tag context export --profile researcher` exports full context as a markdown file.

---

## 4. Non-Goals

- Custom embedding-based semantic retrieval (that's memory, PRD-001/002).
- Multi-modal context (images) compression.
- Cross-profile context sharing.

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | see "researcher: 45,234 / 128,000 tokens (35%)" | I know how close I am to the limit |
| U2 | Developer | have auto-summarization trigger at 80% | I never hit the hard limit mid-task |
| U3 | Developer | run `tag context trim --keep-last 10` | I reduce context cost for a simple follow-up |
| U4 | Developer | run `tag context reset` | I start a fresh session without reinstalling |
| U5 | Developer | run `tag context export > session.md` | I archive the full conversation |

---

## 6. Technical Design

### 6.1 Context size measurement

Hermes exposes `hermes prompt-size` (already a TAG pass-through via `cmd_prompt_size`). Wrap this to get structured output:

```python
def get_context_size(cfg: dict[str, Any], profile_name: str) -> dict[str, Any]:
    """Get current context token count for a profile."""
    try:
        result = run_profile_hermes(cfg, profile_name, "prompt-size", "--json", check=False)
        if result.returncode == 0:
            data = json.loads(result.stdout)
            return {
                "used_tokens": data.get("tokens", 0),
                "max_tokens": data.get("max_tokens", 128000),
                "pct": data.get("tokens", 0) / max(data.get("max_tokens", 128000), 1) * 100,
            }
    except (json.JSONDecodeError, subprocess.CalledProcessError):
        pass
    return {"used_tokens": 0, "max_tokens": 128000, "pct": 0.0}
```

### 6.2 Auto-summarization

```python
CONTEXT_SUMMARY_PROMPT = """You are a context summarizer for an AI agent session.
Below is the conversation history that needs to be summarized.
Create a concise summary that preserves:
- Key decisions made
- Important findings
- File paths and code references mentioned
- Current task state and what remains to do

Keep the summary under 500 tokens.

Conversation to summarize:
{history}
"""

def summarize_context(
    cfg: dict[str, Any],
    profile_name: str,
    *,
    summarizer_profile: str | None = None,
) -> dict[str, Any]:
    """Summarize and compress the context for a profile."""
    summarizer = summarizer_profile or cfg["defaults"]["master_profile"]
    
    # Export current context
    export_result = run_profile_hermes(cfg, profile_name, "sessions", "export", "--format", "json", check=False)
    if export_result.returncode != 0:
        return {"status": "export_failed"}
    
    try:
        session_data = json.loads(export_result.stdout)
        messages = session_data.get("messages", [])
    except json.JSONDecodeError:
        return {"status": "parse_failed"}
    
    if len(messages) < 4:
        return {"status": "too_short", "messages": len(messages)}
    
    # Keep last N messages uncompressed
    keep_last = 6
    to_compress = messages[:-keep_last]
    to_keep = messages[-keep_last:]
    
    # Format history for summarizer
    history_text = "\n\n".join(
        f"[{m.get('role', 'unknown')}]: {m.get('content', '')[:500]}"
        for m in to_compress
    )
    
    # Run summarizer
    summary_result = run_chat_step(
        cfg, summarizer,
        CONTEXT_SUMMARY_PROMPT.format(history=history_text),
    )
    summary = normalize_chat_output(summary_result.get("output", ""))
    
    # Inject summary as a system message + keep_last messages
    summary_message = {
        "role": "system",
        "content": f"[Context Summary — {len(to_compress)} messages compressed]\n\n{summary}",
    }
    new_messages = [summary_message] + to_keep
    
    # Write compressed session back
    # (This requires Hermes session import API — verify availability)
    compressed_data = {**session_data, "messages": new_messages}
    temp_path = tag_home() / "tmp" / f"session-{profile_name}.json"
    temp_path.parent.mkdir(exist_ok=True)
    temp_path.write_text(json.dumps(compressed_data))
    run_profile_hermes(cfg, profile_name, "sessions", "import", str(temp_path), check=False)
    temp_path.unlink(missing_ok=True)
    
    return {
        "status": "summarized",
        "compressed_messages": len(to_compress),
        "kept_messages": len(to_keep),
        "summary_tokens": len(summary.split()),
    }
```

### 6.3 default.yaml schema extension

```yaml
profiles:
  researcher:
    config:
      context:
        max_tokens: 100000          # trigger auto-summarization at this point
        warn_pct: 70                # warn at 70% used
        auto_summarize: true        # auto-compress on warn_pct
        keep_last_messages: 6       # always keep N most recent messages uncompressed
        summarizer_profile: orchestrator  # which profile runs the summarizer LLM
```

### 6.4 Auto-summarization trigger

In `profile_exec_env()` (or at the start of `cmd_chat`):

```python
def maybe_auto_summarize(cfg: dict, profile_name: str, db: sqlite3.Connection) -> None:
    """Check context size and summarize if needed."""
    profile_context_cfg = cfg.get("profiles", {}).get(profile_name, {}).get("config", {}).get("context", {})
    if not profile_context_cfg.get("auto_summarize"):
        return
    
    warn_pct = profile_context_cfg.get("warn_pct", 80)
    size = get_context_size(cfg, profile_name)
    
    if size["pct"] >= warn_pct:
        print(f"[context] {profile_name}: {size['pct']:.0f}% used — auto-summarizing…")
        result = summarize_context(cfg, profile_name)
        if result["status"] == "summarized":
            print(f"[context] Compressed {result['compressed_messages']} → summary block")
```

### 6.5 `cmd_context` command

```
tag context status [--profile PROFILE]    — show token usage per profile
tag context trim   --profile PROFILE [--keep-last N]  — trim to N most recent messages
tag context reset  --profile PROFILE [--confirm]      — clear all session context  
tag context export --profile PROFILE [--output FILE]  — export context as markdown
tag context summarize --profile PROFILE               — manual summarization trigger
```

---

## 7. Implementation Plan

| Step | Task |
|------|------|
| 1 | Implement `get_context_size` using `hermes prompt-size --json` |
| 2 | Add `context` schema to `default.yaml` |
| 3 | Implement `summarize_context` |
| 4 | Add `maybe_auto_summarize` call in `cmd_chat` |
| 5 | Implement `cmd_context` with all subcommands |
| 6 | Register `context` parser |
| 7 | Tests: `test_get_context_size_parses_json`, `test_auto_summarize_threshold` |
| 8 | Update README |

---

## 8. Success Metrics

- `tag context status` shows token usage per profile.
- Auto-summarization fires when configured threshold is crossed.
- `tag context reset --profile researcher --confirm` successfully clears session.
- No context overflow errors in tests running 20+ consecutive `tag chat` turns.

---

## 9. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| `hermes prompt-size --json` output format unknown | Parse defensively; fall back to 0 if format changes |
| `hermes sessions export/import` API not available | Gate auto-summarize on Hermes version check; log warning if unavailable |
| Summarization itself consumes significant tokens | Cap summarizer input at 10,000 tokens; always keep last 6 messages |
| Loss of important context in summary | Summarization is opt-in; add `tag context export` for backup before summarizing |
