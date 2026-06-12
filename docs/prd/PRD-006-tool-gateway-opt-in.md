# PRD-006: Tool Gateway Opt-in Wiring

**Status:** Proposed  
**Priority:** P1  
**Estimated Effort:** XS (2–3 days)  
**Affects:** `controller.py` (`render_profiles`, new `cmd_import_nous_portal`), `default.yaml`

---

## 1. Overview

Hermes v0.10.0 introduced the Nous Tool Gateway — a managed cloud service accessible with a Nous Portal subscription. When `use_gateway: true` is set in a profile's config, Hermes automatically routes tool calls for web search (Firecrawl), image generation (FAL/FLUX 2 Pro), TTS (OpenAI TTS), and browser automation to the gateway instead of requiring the user to have individual API keys. Currently users must hand-edit Hermes config YAML to activate this. This PRD adds `tag import-nous-portal` to make the gateway accessible in one command.

---

## 2. Problem Statement

- Nous Portal subscribers who want web search or image gen via Hermes have to know to set `use_gateway: true` in `~/.tag/runtime/home/.hermes/profiles/<name>/config.yaml` — completely undiscoverable via TAG.
- There is no TAG command that accepts a Nous Portal token and wires it up.
- `tag import-*` covers 11 providers but misses the Nous Portal.
- Users get the impression TAG has no web search capability when it actually does (via gateway).

---

## 3. Goals

1. `tag import-nous-portal` writes `NOUS_PORTAL_API_KEY` to profile `.env` and sets `use_gateway: true` in profile config.
2. Supports `--all-profiles` to enable gateway for every profile at once.
3. `render_profiles()` respects a `gateway.enabled: true` key in `default.yaml` profile config.
4. `tag doctor` checks if `NOUS_PORTAL_API_KEY` is set for profiles that have `use_gateway: true`.
5. README documents gateway capabilities clearly.

---

## 4. Non-Goals

- Building or hosting a gateway — this is Nous' infrastructure.
- Free tier / self-hosted gateway — Nous Portal subscription is required.
- Supporting non-Nous gateways (future consideration).

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Nous Portal subscriber | run `tag import-nous-portal` | web search and image gen work in all my profiles |
| U2 | Researcher | have gateway enabled only on the researcher profile | I pay only for what researcher uses |
| U3 | Developer | run `tag doctor` | I can confirm gateway is active for the profiles I expect |
| U4 | New user | read the README and understand what gateway adds | I know whether it's worth subscribing |

---

## 6. Technical Design

### 6.1 `_detect_nous_portal_credentials() -> dict[str, str]`

```python
def _detect_nous_portal_credentials(source_config: Path | None = None) -> dict[str, str]:
    """Read Nous Portal API key from known config locations."""
    candidates = [
        source_config or Path.home() / ".config" / "nousresearch" / "portal.json",
        Path.home() / ".nousresearch" / "config.json",
    ]
    for path in candidates:
        if path.exists():
            try:
                data = json.loads(path.read_text())
                if key := data.get("api_key") or data.get("token"):
                    return {"NOUS_PORTAL_API_KEY": key}
            except (json.JSONDecodeError, OSError):
                pass
    # Check environment
    if key := os.environ.get("NOUS_PORTAL_API_KEY"):
        return {"NOUS_PORTAL_API_KEY": key}
    return {}
```

### 6.2 `import_nous_portal_into_profile()`

```python
def import_nous_portal_into_profile(
    cfg: dict[str, Any],
    profile_name: str,
    *,
    api_key: str | None = None,
    source_config: Path | None = None,
    force: bool = False,
) -> dict[str, Any]:
    creds = {"NOUS_PORTAL_API_KEY": api_key} if api_key else _detect_nous_portal_credentials(source_config)
    if not creds:
        return {"status": "no_credentials"}
    
    env_file = profile_home(cfg, profile_name) / ".env"
    _upsert_env_line(env_file, "NOUS_PORTAL_API_KEY", creds["NOUS_PORTAL_API_KEY"])
    
    # Set use_gateway: true in profile's Hermes config
    profile_config_path = profile_home(cfg, profile_name) / "config.yaml"
    if profile_config_path.exists():
        profile_cfg = yaml.safe_load(profile_config_path.read_text()) or {}
        profile_cfg.setdefault("gateway", {})["use_gateway"] = True
        profile_config_path.write_text(yaml.dump(profile_cfg, default_flow_style=False))
    
    return {"status": "ok", "profile": profile_name, "env_file": str(env_file)}
```

### 6.3 `default.yaml` schema extension

```yaml
profiles:
  researcher:
    config:
      gateway:
        enabled: true             # writes use_gateway: true to Hermes config
        tools:                    # optional allowlist; empty = all gateway tools
          - web_search
          - browser
```

### 6.4 `render_profiles()` changes

```python
gateway_cfg = profile_data.get("config", {}).get("gateway", {})
if gateway_cfg.get("enabled"):
    profile_config["gateway"] = {"use_gateway": True}
    if tools := gateway_cfg.get("tools"):
        profile_config["gateway"]["allowed_tools"] = tools
```

### 6.5 `cmd_import_nous_portal`

```python
def cmd_import_nous_portal(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    profiles_to_update = (
        list(cfg.get("profiles", {}).keys())
        if getattr(args, "all_profiles", False)
        else [getattr(args, "profile", cfg["defaults"]["master_profile"])]
    )
    results = []
    for p in profiles_to_update:
        ensure_profile_exists(cfg, p)
        result = import_nous_portal_into_profile(
            cfg, p,
            api_key=getattr(args, "api_key", None),
            force=args.force,
        )
        results.append(result)
    
    if args.json:
        print(json.dumps(results, indent=2))
        return 0
    for r in results:
        status = "✓" if r["status"] == "ok" else "–"
        print(f"  {status} {r.get('profile', '?')}: {r['status']}")
    if any(r["status"] == "no_credentials" for r in results):
        print("Hint: pass --api-key YOUR_KEY or set NOUS_PORTAL_API_KEY env var.")
    return 0
```

### 6.6 Parser registration

```python
p_nous = import_subparsers.add_parser("nous-portal", help="Import Nous Portal API key")
p_nous.add_argument("--api-key", metavar="KEY", help="Nous Portal API key (or set NOUS_PORTAL_API_KEY)")
p_nous.add_argument("--profile", metavar="NAME")
p_nous.add_argument("--all-profiles", action="store_true")
p_nous.add_argument("--force", action="store_true")
p_nous.add_argument("--json", action="store_true")
p_nous.set_defaults(func=cmd_import_nous_portal)
```

---

## 7. Implementation Plan

| Step | Task |
|------|------|
| 1 | Add `gateway` schema to `default.yaml` |
| 2 | Implement `_detect_nous_portal_credentials` |
| 3 | Implement `import_nous_portal_into_profile` |
| 4 | Update `render_profiles()` for gateway config |
| 5 | Implement `cmd_import_nous_portal` |
| 6 | Register parser |
| 7 | Update `cmd_doctor` to check gateway key when `use_gateway: true` |
| 8 | Add tests: `test_detect_nous_portal_reads_config_json`, `test_import_nous_portal_sets_use_gateway` |
| 9 | Update README with "Premium Tools" section |

---

## 8. Success Metrics

- `tag import-nous-portal --api-key sk-...` writes key to `.env` and sets `use_gateway: true` in profile config.
- `tag doctor` flags profiles with `use_gateway: true` but missing `NOUS_PORTAL_API_KEY`.
- `tag benchmark` passes all 12 existing tests.

---

## 9. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Nous Portal API key format unknown | Accept any non-empty string; let Hermes validate at runtime |
| `use_gateway: true` config key name may differ | Verify against vendor tarball config schema before implementation |
| Gateway subscription requirement not clear to users | Add prominent "Requires Nous Portal subscription" note in help text |
