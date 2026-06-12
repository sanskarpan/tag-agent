# PRD-001: Structured Memory Configuration Per Profile

**Status:** Proposed  
**Priority:** P0 (Highest Impact)  
**Estimated Effort:** M (2–3 weeks)  
**Affects:** `controller.py`, `default.yaml`, `cmd_setup`, `render_profiles`

---

## 1. Overview

TAG currently has zero memory configuration — `tag memory` is a bare pass-through to `hermes memory` and profile configs never mention a memory backend. Hermes v0.16.0 ships three supported memory backends (Supermemory cloud, Honcho self-hosted, and the `hermes-local-memory` SQLite plugin) plus a session-level Supermemory ingest flag. None of these are surfaced by TAG today. This PRD defines how TAG adds first-class memory backend selection, per-profile configuration, and credential import for all three backends.

---

## 2. Problem Statement

- Users who want persistent memory across agent sessions have no documented path in TAG.
- Hermes requires hand-editing `config.yaml` deep inside `~/.tag/runtime/home/.hermes/profiles/<name>/` to enable any memory backend — TAG users cannot discover this.
- The three available backends (Supermemory, Honcho, local SQLite) have completely different credential and configuration requirements; a unified abstraction is needed.
- `tag setup` runs four auto-import functions but never configures memory, leaving every profile with the default (no persistent memory).

---

## 3. Goals

1. Users can select a memory backend (`local`, `supermemory`, `honcho`) during or after `tag setup` without touching raw YAML.
2. Each profile can have a **different** memory backend (e.g., orchestrator uses Supermemory, coder uses local SQLite).
3. New `tag import-supermemory` and `tag import-honcho` commands write credentials to profile `.env` files with the same pattern as existing import commands.
4. `render_profiles()` writes the correct Hermes config keys for whichever backend is configured.
5. `tag doctor` reports memory backend status per profile.

---

## 4. Non-Goals

- Building a custom memory backend — TAG configures Hermes' backends, it does not replace them.
- Vector embedding or semantic search — that is Hermes' concern.
- Cross-profile memory sharing in this release (deferred to PRD-006).

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag setup` and have it ask "which memory backend?" | I don't have to research Hermes config keys |
| U2 | Researcher | give the researcher profile Supermemory so it recalls past research | sessions build on each other |
| U3 | Developer | run `tag import-supermemory --profile researcher` | my API key lands in the right `.env` without manual editing |
| U4 | Ops engineer | run `tag doctor` and see memory backend health per profile | I can debug memory issues quickly |
| U5 | Solo developer | use `local` memory without signing up for any cloud service | I keep data local and free |

---

## 6. Technical Design

### 6.1 default.yaml schema extension

Add `memory` block to each profile config section:

```yaml
profiles:
  researcher:
    config:
      memory:
        provider: supermemory   # local | supermemory | honcho | none
        supermemory:
          session_ingest: true  # set SUPERMEMORY_SESSION_INGEST=1
        honcho:
          base_url: http://localhost:8001
          app_name: tag-researcher
```

Default for all existing profiles: `provider: local` (uses `hermes-local-memory` SQLite plugin if installed, else falls back to Hermes' built-in session memory).

### 6.2 `render_profiles()` changes

In `render_profiles()`, after writing the standard config keys, read the `memory` block and inject:

**For `supermemory`:**
```python
profile_config["memory"] = {
    "provider": "supermemory",
    "session_ingest": mem_cfg.get("supermemory", {}).get("session_ingest", False),
}
```
Also call `_upsert_env_line(env_file, "SUPERMEMORY_SESSION_INGEST", "1")` if session_ingest is true.

**For `honcho`:**
```python
profile_config["memory"] = {
    "provider": "honcho",
    "base_url": mem_cfg.get("honcho", {}).get("base_url", "http://localhost:8001"),
    "app_name": mem_cfg.get("honcho", {}).get("app_name", f"tag-{profile_name}"),
}
```

**For `local`:**
```python
profile_config["memory"] = {"provider": "local"}
```
This matches the `hermes-local-memory` plugin's expected config key.

### 6.3 New credential import functions

#### `_detect_supermemory_credentials(source_config_dir: Path | None) -> dict[str, str]`
Read `~/.config/supermemory/config.json` (or `~/.supermemory/config.json`) for `api_key`. Return `{"SUPERMEMORY_API_KEY": "<key>"}`.

#### `import_supermemory_into_profile(cfg, profile_name, *, source_config_dir=None, force=False) -> dict`
Write `SUPERMEMORY_API_KEY` to profile `.env` using `_upsert_env_line`. Also write `SUPERMEMORY_SESSION_INGEST=1`.

#### `_detect_honcho_credentials(source_config: Path | None) -> dict[str, str]`
Read `~/.honcho/.env` or `~/.config/honcho/config.yaml` for `HONCHO_API_KEY` / `HONCHO_BASE_URL`. Return both keys.

#### `import_honcho_into_profile(cfg, profile_name, *, source_config=None, force=False) -> dict`
Write both keys to profile `.env`.

### 6.4 New CLI commands

```
tag import-supermemory [--profile PROFILE] [--source PATH] [--force]
tag import-honcho [--profile PROFILE] [--source PATH] [--force] [--base-url URL]
tag memory-config [--profile PROFILE] [--provider local|supermemory|honcho]
```

`tag memory-config` writes the `memory.provider` key into the profile's config YAML and triggers `render_profiles()` for that profile only.

### 6.5 `cmd_setup` changes

After existing auto-import calls, add:
```python
if args.memory_provider and args.memory_provider != "none":
    steps["memory_config"] = configure_memory_backends(cfg, args.memory_provider)
```

Add `--memory-provider {none,local,supermemory,honcho}` argument to `tag setup`, defaulting to `none` for backwards-compatibility.

### 6.6 `tag doctor` changes

Per profile, check:
- Is a memory provider configured in profile config?
- If supermemory: is `SUPERMEMORY_API_KEY` in `.env`?
- If honcho: is `HONCHO_BASE_URL` reachable (HTTP GET ping)?
- If local: is `hermes-local-memory` pip package installed in the Hermes venv?

---

## 7. Implementation Plan

| Step | Task | Owner |
|------|------|-------|
| 1 | Add `memory` schema to `default.yaml` with `provider: none` default | eng |
| 2 | Add `_detect_supermemory_credentials()` + `import_supermemory_into_profile()` | eng |
| 3 | Add `_detect_honcho_credentials()` + `import_honcho_into_profile()` | eng |
| 4 | Update `render_profiles()` to write memory config keys | eng |
| 5 | Add `cmd_import_supermemory`, `cmd_import_honcho`, `cmd_memory_config` + parser registrations | eng |
| 6 | Update `cmd_setup` with `--memory-provider` arg | eng |
| 7 | Update `cmd_doctor` with per-profile memory health checks | eng |
| 8 | Add tests: `test_detect_supermemory_reads_config`, `test_import_honcho_writes_env`, `test_render_profiles_writes_memory_keys` | eng |
| 9 | Update README memory section | eng |

---

## 8. Success Metrics

- `tag setup --memory-provider supermemory` produces profile `.env` files with valid `SUPERMEMORY_API_KEY` (measured by test).
- `tag doctor` reports memory backend for each profile without error.
- Zero regression in existing import command tests.
- User can run `tag chat --profile researcher` and Hermes picks up the configured memory backend (verified by checking Hermes startup logs).

---

## 9. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| `hermes-local-memory` is alpha; API could break | Pin to a specific version, add a compatibility check in `tag doctor` |
| Honcho self-hosted setup is complex | Provide clear `tag import-honcho --help` text + README section; don't make it default |
| Supermemory cloud stores conversation data | Add explicit warning in import command: "Your conversations will be stored in Supermemory cloud" |
| Hermes config key names for memory may differ from what we expect | Verify against vendor tarball `config.yaml` schema before implementing step 4 |
