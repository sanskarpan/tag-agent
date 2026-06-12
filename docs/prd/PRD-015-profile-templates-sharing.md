# PRD-015: Profile Templates & Sharing

**Status:** Proposed  
**Priority:** P2  
**Estimated Effort:** M (2 weeks)  
**Affects:** `controller.py` (new `cmd_template`), new `tag/templates.py`

---

## 1. Overview

TAG profiles encode significant expertise: model selection, memory configuration, tool permissions, routing rules, budget limits. Today there is no way to share this configuration between projects or team members — each installation starts from the same blank `default.yaml`. This PRD defines profile templates: shareable, versionable YAML files that can be exported from a running TAG instance, imported into another, and published to GitHub. Think Docker images for agent configurations.

---

## 2. Problem Statement

- Every developer or team starts from scratch with the same generic profiles.
- There is no community registry of proven agent configurations.
- Onboarding a new team member requires manually copying credentials and profile configs.
- Organizations running agents across projects need a way to standardize configurations.
- Expert users who have tuned profiles for specific use cases (legal research, code review, data analysis) cannot share their work.

---

## 3. Goals

1. `tag template export --profile researcher` exports a portable template YAML with credentials redacted.
2. `tag template import <file>` imports a template and creates/updates the named profile.
3. `tag template pull <owner/repo>` fetches a template from GitHub and imports it.
4. `tag template list` shows available templates (local + cached remote).
5. Templates support placeholder substitution: `${OPENROUTER_API_KEY}` is filled from env at import time.
6. Template format is human-readable YAML, version-controlled friendly.

---

## 4. Non-Goals

- Running a template registry server (use GitHub for now).
- Template versioning beyond what git provides.
- Encrypted credential storage in templates.

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | export my tuned researcher profile | my teammate can import it |
| U2 | New user | run `tag template pull nous-research/tag-templates/researcher-pro` | I start with a production-grade researcher |
| U3 | Team lead | commit a `tag-templates/` folder to the project repo | all team members use the same profiles |
| U4 | Power user | share my `tag-template.yaml` on Twitter | community can benefit from my work |
| U5 | Developer | import a template and have it ask for missing env vars | I fill in credentials interactively |

---

## 6. Technical Design

### 6.1 Template format

```yaml
# tag-template.yaml — exported by: tag template export --profile researcher
version: "1"
name: researcher-pro
description: "Research-optimized profile with web search and Supermemory"
tags: [research, web, memory]
exported_at: "2026-06-11T10:00:00Z"
exported_by: "tag/0.3.0"

profile:
  description: "Research worker for docs, web evidence, source extraction"
  tags: [research, web, extraction]
  config:
    display:
      skin: tag-control
      tui_statusbar: top
    model:
      provider: openrouter
      default: deepseek/deepseek-v4-flash
    memory:
      provider: supermemory
      supermemory:
        session_ingest: true
    gateway:
      enabled: true
    budget_limit_usd: 5.00

credentials:
  # Fill these in before importing, or set as env vars
  OPENROUTER_API_KEY: "${OPENROUTER_API_KEY}"
  SUPERMEMORY_API_KEY: "${SUPERMEMORY_API_KEY}"
  NOUS_PORTAL_API_KEY: "${NOUS_PORTAL_API_KEY}"

mcp_servers:
  - name: mcp-brave-search
    requires_env: [BRAVE_API_KEY]
```

### 6.2 Core functions

```python
def export_profile_template(
    cfg: dict[str, Any],
    profile_name: str,
    output_path: Path | None = None,
) -> dict[str, Any]:
    """Export profile as a portable template (credentials redacted)."""
    from tag import __version__
    
    # Get profile config from default.yaml
    profile_data = cfg.get("profiles", {}).get(profile_name, {})
    
    # Get installed MCP servers from profile config
    profile_config_file = profile_home(cfg, profile_name) / "config.yaml"
    mcp_servers = []
    if profile_config_file.exists():
        profile_cfg = yaml.safe_load(profile_config_file.read_text()) or {}
        for server_name in profile_cfg.get("mcp", {}).get("servers", {}).keys():
            registry = load_mcp_registry()
            server_info = registry.get("servers", {}).get(server_name, {})
            mcp_servers.append({
                "name": server_name,
                "requires_env": server_info.get("requires_env", []),
            })
    
    # Collect env keys from profile .env (redacted)
    env_file = profile_home(cfg, profile_name) / ".env"
    env_data = read_dotenv(env_file) if env_file.exists() else {}
    credentials_redacted = {k: f"${{{k}}}" for k in env_data if k.endswith("_KEY") or k.endswith("_TOKEN")}
    
    template = {
        "version": "1",
        "name": profile_name,
        "description": profile_data.get("description", ""),
        "tags": profile_data.get("tags", []),
        "exported_at": utc_now(),
        "exported_by": f"tag/{__version__}",
        "profile": profile_data,
        "credentials": credentials_redacted,
        "mcp_servers": mcp_servers,
    }
    
    if output_path:
        output_path.write_text(yaml.dump(template, default_flow_style=False, sort_keys=False))
    
    return template


def import_profile_template(
    cfg: dict[str, Any],
    template_path: Path,
    *,
    profile_name: str | None = None,
    force: bool = False,
) -> dict[str, Any]:
    """Import a template, substituting env var placeholders."""
    template = yaml.safe_load(template_path.read_text())
    
    target_name = profile_name or template.get("name", "imported")
    
    # Substitute credentials from env
    credentials = template.get("credentials", {})
    missing_creds = []
    for key, value in credentials.items():
        if value.startswith("${") and value.endswith("}"):
            env_key = value[2:-1]
            actual = os.environ.get(env_key, "")
            if actual:
                credentials[key] = actual
            else:
                missing_creds.append(env_key)
    
    if missing_creds:
        print(f"Missing credentials: {', '.join(missing_creds)}")
        print("Set these env vars or pass them manually after import.")
    
    # Write profile to default.yaml
    cfg.setdefault("profiles", {})[target_name] = template.get("profile", {})
    cfg["profiles"][target_name]["description"] = template.get("description", "")
    save_config(config_path(None), cfg)
    
    # Bootstrap profile
    bootstrap_profiles(cfg)
    render_profiles(cfg, force=False)
    
    # Write credentials to profile .env
    env_file = profile_home(cfg, target_name) / ".env"
    for key, value in credentials.items():
        if value and not value.startswith("${"):
            _upsert_env_line(env_file, key, value)
    
    # Install MCP servers
    mcp_installs = []
    for server in template.get("mcp_servers", []):
        result = mcp_install_server(cfg, server["name"], target_name)
        mcp_installs.append(result)
    
    return {
        "status": "imported",
        "profile": target_name,
        "missing_creds": missing_creds,
        "mcp_installs": mcp_installs,
    }


def fetch_github_template(owner_repo_path: str, output_dir: Path) -> Path:
    """Fetch a template from GitHub raw URL."""
    parts = owner_repo_path.split("/", 2)
    owner, repo = parts[0], parts[1]
    path = parts[2] if len(parts) > 2 else "tag-template.yaml"
    
    url = f"https://raw.githubusercontent.com/{owner}/{repo}/main/{path}"
    output_path = output_dir / f"{repo}-{path.replace('/', '-')}"
    
    req = urllib.request.Request(url, headers={"User-Agent": "tag-agent/0.3.0"})
    with urllib.request.urlopen(req, timeout=10) as resp:
        output_path.write_bytes(resp.read())
    
    return output_path
```

### 6.3 `cmd_template` with subcommands

```python
def cmd_template(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    sub = args.template_subcommand
    
    if sub == "export":
        profile = getattr(args, "profile", cfg["defaults"]["master_profile"])
        output = Path(args.output) if getattr(args, "output", None) else Path(f"{profile}-template.yaml")
        result = export_profile_template(cfg, profile, output_path=output)
        print(f"Exported: {output}")
        if creds := result.get("credentials"):
            print(f"Credentials redacted: {', '.join(creds.keys())}")
    
    elif sub == "import":
        template_path = Path(args.template_file)
        result = import_profile_template(cfg, template_path, force=args.force)
        print(f"Imported: {result['profile']}")
        if result["missing_creds"]:
            print(f"Missing: {', '.join(result['missing_creds'])}")
    
    elif sub == "pull":
        cache_dir = tag_home() / "template-cache"
        cache_dir.mkdir(parents=True, exist_ok=True)
        template_path = fetch_github_template(args.github_ref, cache_dir)
        print(f"Fetched: {template_path}")
        result = import_profile_template(cfg, template_path, force=args.force)
        print(f"Imported: {result['profile']}")
    
    elif sub == "list":
        cache_dir = tag_home() / "template-cache"
        templates = list(cache_dir.glob("*.yaml")) if cache_dir.exists() else []
        for t in templates:
            data = yaml.safe_load(t.read_text()) or {}
            print(f"  {data.get('name', t.stem)}: {data.get('description', '')}")
    
    return 0
```

---

## 7. Implementation Plan

| Step | Task |
|------|------|
| 1 | Define template YAML schema |
| 2 | Implement `export_profile_template` |
| 3 | Implement `import_profile_template` with env substitution |
| 4 | Implement `fetch_github_template` |
| 5 | Implement `cmd_template` with export/import/pull/list subcommands |
| 6 | Register `template` parser |
| 7 | Tests: `test_export_redacts_credentials`, `test_import_substitutes_env_vars`, `test_import_bootstrap_profile` |
| 8 | Create example templates in `src/tag/config/templates/` directory |

---

## 8. Success Metrics

- Exported template YAML contains no raw credential values.
- `tag template import researcher-pro-template.yaml` creates a working researcher profile.
- `tag template pull nous-research/tag-templates/researcher` fetches and imports successfully.
- Import with missing credentials warns but does not fail.

---

## 9. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Malicious template from GitHub executes arbitrary code | Templates are pure YAML data; no code execution in import |
| Template references profile/model that doesn't exist in target install | Warn on unknown models; use best-match from local model inventory |
| Credential placeholder substitution exposes secrets in template files | Document: never commit template files with real credential values |
