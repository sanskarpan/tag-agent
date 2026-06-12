# PRD-010: Dashboard & Admin Panel Integration

**Status:** Proposed  
**Priority:** P2  
**Estimated Effort:** XS (2 days)  
**Affects:** `controller.py` (`cmd_dashboard`, `cmd_setup`)

---

## 1. Overview

Hermes v0.16.0 ships a full browser-based admin panel that replaces hand-editing `config.yaml`. It covers MCP catalog enable/disable, credential management, webhook creation, memory configuration, gateway controls, and Channels (Telegram, Discord, Slack, Matrix). TAG's `tag dashboard` is currently a bare pass-through — it doesn't configure port, profile, or any of the panel's settings during setup. This PRD upgrades `tag dashboard` to be profile-aware, port-configurable, and auto-launched with a browser after `tag setup`.

---

## 2. Problem Statement

- `tag dashboard` just runs `hermes dashboard` with no profile context — the panel shows Hermes defaults, not TAG's profile setup.
- After `tag setup`, users have no idea the admin panel exists or how to reach it.
- The panel URL and port are not shown anywhere in TAG output.
- Users who configure memory, MCP servers, or channels through the panel have those settings overwritten when `tag render` is run (because `render_profiles()` doesn't read the panel's output).

---

## 3. Goals

1. `tag dashboard --profile researcher` launches the admin panel configured for that profile.
2. `tag dashboard --port N` sets a custom port (default: 3333, configurable in `default.yaml`).
3. After `tag setup --include-dashboard`, a brief message tells users the panel URL.
4. `tag status` includes the dashboard URL if Hermes gateway is running.
5. Panel changes to `config.yaml` are not overwritten by `tag render` (read-merge instead of overwrite).

---

## 4. Non-Goals

- Embedding the panel in a TAG-controlled server — we launch Hermes' panel as-is.
- Building a separate TAG admin panel.
- Syncing panel changes back to `default.yaml`.

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | New user | see "Dashboard at http://localhost:3333" after `tag setup` | I can configure MCP servers without editing YAML |
| U2 | Developer | run `tag dashboard --profile coder` | the panel shows the coder profile's config |
| U3 | Developer | run `tag dashboard --port 8888` | I avoid port conflicts |
| U4 | Developer | run `tag status` | I see the dashboard URL if it's running |

---

## 6. Technical Design

### 6.1 `cmd_dashboard` upgrade

```python
def cmd_dashboard(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    
    profile = getattr(args, "profile", cfg["defaults"]["master_profile"])
    port = getattr(args, "port", cfg.get("dashboard", {}).get("port", 3333))
    
    env = profile_exec_env(cfg, profile)
    
    hermes_args = ["dashboard"]
    if port:
        hermes_args += ["--port", str(port)]
    if getattr(args, "open_browser", True):
        hermes_args.append("--open")
    
    hermes_args += normalize_hermes_passthrough_args(list(getattr(args, "hermes_args", [])))
    
    proc = subprocess.Popen(
        [str(hermes_bin(cfg))] + hermes_args,
        env={**os.environ, **env},
    )
    
    print(f"Dashboard running at http://localhost:{port} (profile: {profile})")
    print("Press Ctrl+C to stop.")
    
    try:
        proc.wait()
    except KeyboardInterrupt:
        proc.terminate()
    return 0
```

### 6.2 `default.yaml` schema extension

```yaml
dashboard:
  port: 3333
  auto_open_browser: true
  default_profile: orchestrator
```

### 6.3 `tag setup` dashboard hint

After setup completes, print:
```
Setup complete! Next steps:
  tag doctor              — verify your installation
  tag dashboard           — open the admin panel (http://localhost:3333)
  tag chat --profile orchestrator "hello"  — start chatting
```

### 6.4 `render_profiles()` read-merge fix

Current `render_profiles()` overwrites `config.yaml`. Change to merge instead:

```python
def render_profiles(cfg: dict[str, Any], force: bool) -> list[dict[str, str]]:
    results = []
    for profile_name, profile_data in cfg.get("profiles", {}).items():
        config_file = profile_home(cfg, profile_name) / "config.yaml"
        
        # Read existing config if present (preserve panel edits)
        existing = {}
        if config_file.exists() and not force:
            try:
                existing = yaml.safe_load(config_file.read_text()) or {}
            except yaml.YAMLError:
                existing = {}
        
        # Deep-merge: TAG's config wins for keys it explicitly sets
        # Panel edits (e.g. MCP servers, channels) are preserved in existing
        merged = _deep_merge(existing, _build_profile_config(cfg, profile_name))
        
        write_yaml(config_file, merged, force=True)
        results.append({"profile": profile_name, "config": str(config_file), "status": "rendered"})
    return results


def _deep_merge(base: dict, override: dict) -> dict:
    """Merge override into base, recursively for nested dicts."""
    result = dict(base)
    for k, v in override.items():
        if k in result and isinstance(result[k], dict) and isinstance(v, dict):
            result[k] = _deep_merge(result[k], v)
        else:
            result[k] = v
    return result
```

### 6.5 Parser update

```python
p_dashboard = subparsers.add_parser("dashboard", help="Open admin panel")
p_dashboard.add_argument("--profile", metavar="NAME")
p_dashboard.add_argument("--port", type=int, metavar="N")
p_dashboard.add_argument("--no-browser", dest="open_browser", action="store_false", default=True)
p_dashboard.add_argument("hermes_args", nargs=argparse.REMAINDER)
p_dashboard.set_defaults(func=cmd_dashboard)
```

---

## 7. Implementation Plan

| Step | Task |
|------|------|
| 1 | Add `dashboard` section to `default.yaml` |
| 2 | Upgrade `cmd_dashboard` with profile/port args |
| 3 | Implement `_deep_merge` utility function |
| 4 | Update `render_profiles()` to use read-merge pattern |
| 5 | Add post-setup hint message to `cmd_setup` |
| 6 | Update `cmd_status` to include dashboard URL if gateway running |
| 7 | Add tests: `test_render_profiles_preserves_mcp_config`, `test_deep_merge_nested` |

---

## 8. Success Metrics

- `tag dashboard --profile researcher` launches panel with researcher's HERMES_HOME.
- `render_profiles()` does not overwrite MCP server config added via the panel.
- `tag setup` output includes dashboard URL hint.
- `tag doctor` shows dashboard port and running status.

---

## 9. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Hermes dashboard port flag name may differ | Verify against `hermes dashboard --help` in vendor tarball |
| Deep-merge may silently clobber panel config | Add `--no-merge` flag to `render_profiles` for full overwrite when needed |
| Dashboard not available in all Hermes versions | Check for `dashboard` in `hermes --help` output; graceful fallback message |
