# PRD-011: Plugin Management System (`tag plugins`)

**Status:** Proposed  
**Priority:** P1  
**Estimated Effort:** M (2 weeks)  
**Affects:** `controller.py` (new `cmd_plugin_manage`), `cmd_setup`, `tag doctor`

---

## 1. Overview

Hermes ships a plugin discovery system with three discovery sources: user-level, project-level, and pip entry points. Plugins can be memory providers, context engines, toolsets, or arbitrary extensions. TAG's `tag plugins` command is a bare pass-through to `hermes plugins`. This PRD builds a TAG-native plugin management layer that lets users install, list, enable/disable, and verify plugins through a curated registry, with automatic installation into the correct Hermes venv.

---

## 2. Problem Statement

- `tag plugins` shows Hermes' raw plugin list with no TAG context.
- Installing `hermes-local-memory` (the SQLite memory plugin) requires users to manually activate the Hermes venv and run `pip install` — entirely undiscoverable via TAG.
- There is no curated list of TAG-compatible plugins.
- No mechanism to persist which plugins should be installed after `tag setup --refresh`.
- Plugins that require credentials (like Supermemory) have no TAG import command integration.

---

## 3. Goals

1. `tag plugin install <name>` installs a plugin into the Hermes venv.
2. `tag plugin list` shows installed plugins with TAG-friendly descriptions.
3. `tag plugin enable/disable <name>` toggles a plugin in the active profile's config.
4. A curated registry in `default.yaml` (`plugins.registry`) lists known good plugins with install paths.
5. `tag setup` installs all plugins listed in `plugins.auto_install`.
6. `tag doctor` checks that configured plugins are installed and enabled.

---

## 4. Non-Goals

- Building a plugin repository server — we point at PyPI and GitHub.
- Plugin sandboxing or security scanning.
- Custom plugin development scaffolding (separate PRD).

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag plugin install hermes-local-memory` | I get persistent memory without reading Hermes docs |
| U2 | Developer | run `tag plugin list` | I see which plugins are installed and active |
| U3 | Developer | add `hermes-local-memory` to `plugins.auto_install` in config | every new `tag setup` installs it automatically |
| U4 | Developer | run `tag doctor` and see "hermes-local-memory: installed" | I can verify my memory plugin is active |
| U5 | Developer | run `tag plugin enable hermes-web-search --profile researcher` | only researcher gets web search, not coder |

---

## 6. Technical Design

### 6.1 default.yaml schema extension

```yaml
plugins:
  auto_install:              # installed during tag setup
    - hermes-local-memory
  registry:                  # curated known plugins
    hermes-local-memory:
      description: "Persistent SQLite-backed memory for Hermes agents"
      pypi: "hermes-local-memory"
      github: "smarzola/hermes-local-memory"
      memory_provider: true
    hermes-web-search:
      description: "Web search toolset via SerpAPI"
      pypi: "hermes-web-search"
      requires_env: ["SERP_API_KEY"]
```

### 6.2 Core functions

```python
def hermes_venv_pip(cfg: dict[str, Any]) -> Path:
    """Return path to pip inside Hermes venv."""
    return hermes_root(cfg) / ".venv" / "bin" / "pip"


def plugin_install(cfg: dict[str, Any], package: str) -> dict[str, Any]:
    """Install plugin into Hermes venv via pip."""
    pip = hermes_venv_pip(cfg)
    if not pip.exists():
        return {"status": "error", "message": "Hermes venv not found; run tag setup first"}
    result = subprocess.run(
        [str(pip), "install", package],
        capture_output=True, text=True,
    )
    return {
        "status": "installed" if result.returncode == 0 else "failed",
        "package": package,
        "stdout": result.stdout[-500:],  # trim long output
        "stderr": result.stderr[-500:] if result.returncode != 0 else "",
    }


def plugin_list_installed(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    """List installed plugins via hermes plugins list --json."""
    try:
        result = run_hermes(cfg, "plugins", "list", "--json")
        return json.loads(result.stdout)
    except (subprocess.CalledProcessError, json.JSONDecodeError):
        return []


def plugin_enable(cfg: dict[str, Any], profile_name: str, plugin_name: str) -> dict[str, Any]:
    """Enable plugin in profile config.yaml."""
    config_file = profile_home(cfg, profile_name) / "config.yaml"
    profile_cfg = yaml.safe_load(config_file.read_text()) if config_file.exists() else {}
    plugins = profile_cfg.setdefault("plugins", {}).setdefault("enabled", [])
    if plugin_name not in plugins:
        plugins.append(plugin_name)
    write_yaml(config_file, profile_cfg, force=True)
    return {"status": "enabled", "plugin": plugin_name, "profile": profile_name}


def plugin_disable(cfg: dict[str, Any], profile_name: str, plugin_name: str) -> dict[str, Any]:
    """Disable plugin in profile config.yaml."""
    config_file = profile_home(cfg, profile_name) / "config.yaml"
    profile_cfg = yaml.safe_load(config_file.read_text()) if config_file.exists() else {}
    plugins_enabled = profile_cfg.get("plugins", {}).get("enabled", [])
    if plugin_name in plugins_enabled:
        plugins_enabled.remove(plugin_name)
    write_yaml(config_file, profile_cfg, force=True)
    return {"status": "disabled", "plugin": plugin_name, "profile": profile_name}
```

### 6.3 `cmd_plugin_manage` with subcommands

```python
def cmd_plugin_manage(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    sub = args.plugin_subcommand
    
    if sub == "install":
        registry = cfg.get("plugins", {}).get("registry", {})
        package = registry.get(args.name, {}).get("pypi", args.name)
        result = plugin_install(cfg, package)
        print(f"{'✓' if result['status'] == 'installed' else '✗'} {args.name}: {result['status']}")
        return 0 if result["status"] == "installed" else 1
    
    elif sub == "list":
        installed = plugin_list_installed(cfg)
        registry = cfg.get("plugins", {}).get("registry", {})
        if args.json:
            print(json.dumps(installed, indent=2))
            return 0
        print(f"{'Name':<30} {'Status':<12} {'Description'}")
        print("─" * 70)
        for p in installed:
            name = p.get("name", "?")
            status = p.get("status", "unknown")
            desc = registry.get(name, {}).get("description", "")
            print(f"{name:<30} {status:<12} {desc}")
    
    elif sub in ("enable", "disable"):
        profile = getattr(args, "profile", cfg["defaults"]["master_profile"])
        fn = plugin_enable if sub == "enable" else plugin_disable
        result = fn(cfg, profile, args.name)
        print(f"{result['status']}: {args.name} for profile {profile}")
    
    elif sub == "registry":
        registry = cfg.get("plugins", {}).get("registry", {})
        for name, info in registry.items():
            print(f"  {name}: {info.get('description', '')}")
    
    return 0
```

### 6.4 `tag setup` auto-install integration

```python
if auto_install := cfg.get("plugins", {}).get("auto_install", []):
    results = []
    for pkg_name in auto_install:
        results.append(plugin_install(cfg, pkg_name))
    steps["plugin_install"] = results
```

---

## 7. Implementation Plan

| Step | Task |
|------|------|
| 1 | Add `plugins` schema to `default.yaml` |
| 2 | Implement `hermes_venv_pip`, `plugin_install`, `plugin_list_installed` |
| 3 | Implement `plugin_enable`, `plugin_disable` |
| 4 | Implement `cmd_plugin_manage` with subcommands |
| 5 | Register `plugin` parser |
| 6 | Add auto-install to `cmd_setup` |
| 7 | Add `tag doctor` plugin health checks |
| 8 | Tests: `test_plugin_install_uses_hermes_venv_pip`, `test_plugin_enable_writes_config` |

---

## 8. Success Metrics

- `tag plugin install hermes-local-memory` installs into Hermes venv.
- `tag plugin list` shows installed plugins.
- `tag plugin enable hermes-local-memory --profile researcher` updates profile config.
- Auto-install during `tag setup` works for listed plugins.

---

## 9. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| `hermes-local-memory` alpha API breaks after install | Pin specific version in registry |
| User installs malicious package | Show package source (pypi/github) before installing; require `--confirm` for non-registry packages |
| pip install modifies Hermes venv and breaks it | Always use `pip install --no-deps` for tested plugins; document risk |
