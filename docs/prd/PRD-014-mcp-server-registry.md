# PRD-014: MCP Server Registry & Discovery

**Status:** Proposed  
**Priority:** P1  
**Estimated Effort:** M (2 weeks)  
**Affects:** `controller.py` (new `cmd_mcp_registry`), `default.yaml`, profile configs

---

## 1. Overview

MCP (Model Context Protocol) is the emerging standard for agent tool integrations. Hermes supports MCP servers but users must know the server name and configure it manually in Hermes' admin panel or `config.yaml`. This PRD adds a TAG-curated MCP server registry with search, one-command install, per-profile enable/disable, and health checks. It makes TAG the easiest way to add tools to any Hermes-based agent.

---

## 2. Problem Statement

- Hermes supports MCP but TAG's `tag mcp` is a bare pass-through.
- There is no discoverable list of available MCP servers in TAG.
- Installing an MCP server requires editing `config.yaml` inside the Hermes profile directory.
- Per-profile MCP enable/disable is not exposed by TAG.
- The MCP ecosystem is growing rapidly (filesystem, databases, GitHub, web search, email, calendar) and users have no managed path to adopt new servers.

---

## 3. Goals

1. `tag mcp registry search [query]` lists available MCP servers from a curated list embedded in TAG.
2. `tag mcp registry install <server>` installs and enables a server for a profile.
3. `tag mcp list --profile researcher` shows which servers are enabled for that profile.
4. `tag mcp enable/disable <server> --profile <name>` toggles per-profile.
5. `tag mcp check` verifies all configured servers are reachable (TCP or subprocess ping).
6. Registry is bundled (no network required for list) but can be updated with `tag mcp registry update`.

---

## 4. Non-Goals

- Building or hosting MCP servers.
- Payment-gated server installation.
- Custom MCP server development scaffolding.

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag mcp registry search git` | I find all git-related MCP servers |
| U2 | Developer | run `tag mcp registry install mcp-filesystem --profile coder` | coder can read/write files via MCP |
| U3 | Researcher | run `tag mcp registry install mcp-brave-search --profile researcher` | researcher has web search without Nous Portal |
| U4 | DevOps | run `tag mcp check` | I verify all MCP servers respond before running a task |
| U5 | Developer | run `tag mcp list --profile coder` | I see exactly which servers coder has access to |

---

## 6. Technical Design

### 6.1 Registry data structure

Bundled in `src/tag/config/mcp-registry.yaml`:

```yaml
servers:
  mcp-filesystem:
    description: "Read, write, and search files on the local filesystem"
    category: filesystem
    install:
      type: npm
      package: "@modelcontextprotocol/server-filesystem"
    config:
      command: "npx"
      args: ["-y", "@modelcontextprotocol/server-filesystem", "${WORKSPACE_DIR}"]
    requires_env: []
    profiles:
      recommended: [coder, reviewer]
  
  mcp-brave-search:
    description: "Web search via Brave Search API"
    category: web
    install:
      type: npm
      package: "@modelcontextprotocol/server-brave-search"
    config:
      command: "npx"
      args: ["-y", "@modelcontextprotocol/server-brave-search"]
    requires_env: ["BRAVE_API_KEY"]
    profiles:
      recommended: [researcher]
  
  mcp-github:
    description: "GitHub repository operations: clone, PR, issues, commits"
    category: vcs
    install:
      type: npm
      package: "@modelcontextprotocol/server-github"
    config:
      command: "npx"
      args: ["-y", "@modelcontextprotocol/server-github"]
    requires_env: ["GITHUB_TOKEN"]
    profiles:
      recommended: [coder, reviewer]
  
  mcp-postgres:
    description: "PostgreSQL read/write via MCP"
    category: database
    install:
      type: npm
      package: "@modelcontextprotocol/server-postgres"
    config:
      command: "npx"
      args: ["-y", "@modelcontextprotocol/server-postgres", "${POSTGRES_URL}"]
    requires_env: ["POSTGRES_URL"]
    profiles:
      recommended: [coder]
  
  mcp-slack:
    description: "Send and read Slack messages"
    category: messaging
    install:
      type: npm
      package: "@modelcontextprotocol/server-slack"
    config:
      command: "npx"
      args: ["-y", "@modelcontextprotocol/server-slack"]
    requires_env: ["SLACK_BOT_TOKEN"]
    profiles:
      recommended: [orchestrator]
```

### 6.2 Core functions

```python
def load_mcp_registry() -> dict[str, Any]:
    """Load bundled MCP server registry."""
    registry_path = resource_path("config", "mcp-registry.yaml")
    return yaml.safe_load(registry_path.read_text())


def mcp_install_server(cfg: dict, server_name: str, profile_name: str) -> dict[str, Any]:
    """Install MCP server and enable for profile."""
    registry = load_mcp_registry()
    server = registry.get("servers", {}).get(server_name)
    if not server:
        return {"status": "not_found", "server": server_name}
    
    install = server.get("install", {})
    if install.get("type") == "npm":
        # Install globally via npm
        result = subprocess.run(
            ["npm", "install", "-g", install["package"]],
            capture_output=True, text=True,
        )
        if result.returncode != 0:
            return {"status": "install_failed", "stderr": result.stderr[-300:]}
    
    # Write MCP config to profile's config.yaml
    _mcp_enable_for_profile(cfg, profile_name, server_name, server["config"])
    
    # Write required env vars as blank lines to profile .env
    for env_key in server.get("requires_env", []):
        env_file = profile_home(cfg, profile_name) / ".env"
        if not read_dotenv(env_file).get(env_key):
            # Don't overwrite existing values
            with open(env_file, "a") as f:
                f.write(f"\n# Required by {server_name}\n{env_key}=\n")
    
    return {"status": "installed", "server": server_name, "profile": profile_name}


def _mcp_enable_for_profile(cfg: dict, profile_name: str, server_name: str, server_config: dict) -> None:
    """Add MCP server to profile's Hermes config."""
    config_file = profile_home(cfg, profile_name) / "config.yaml"
    profile_cfg = {}
    if config_file.exists():
        profile_cfg = yaml.safe_load(config_file.read_text()) or {}
    
    mcp_servers = profile_cfg.setdefault("mcp", {}).setdefault("servers", {})
    mcp_servers[server_name] = server_config
    write_yaml(config_file, profile_cfg, force=True)


def mcp_list_for_profile(cfg: dict, profile_name: str) -> list[dict[str, Any]]:
    """List MCP servers enabled for a profile."""
    config_file = profile_home(cfg, profile_name) / "config.yaml"
    if not config_file.exists():
        return []
    profile_cfg = yaml.safe_load(config_file.read_text()) or {}
    servers = profile_cfg.get("mcp", {}).get("servers", {})
    registry = load_mcp_registry()
    return [
        {
            "name": name,
            "description": registry.get("servers", {}).get(name, {}).get("description", ""),
            "config": config,
        }
        for name, config in servers.items()
    ]
```

### 6.3 `cmd_mcp_registry` and upgraded `cmd_mcp`

```python
def cmd_mcp(args: argparse.Namespace) -> int:
    sub = getattr(args, "mcp_subcommand", None)
    
    if sub == "registry":
        return _cmd_mcp_registry(args)
    elif sub == "list":
        cfg = load_config(config_path(args.config))
        profile = getattr(args, "profile", cfg["defaults"]["master_profile"])
        servers = mcp_list_for_profile(cfg, profile)
        if args.json:
            print(json.dumps(servers, indent=2))
        else:
            for s in servers:
                print(f"  {s['name']}: {s['description']}")
        return 0
    elif sub == "enable":
        cfg = load_config(config_path(args.config))
        profile = getattr(args, "profile", cfg["defaults"]["master_profile"])
        registry = load_mcp_registry()
        server = registry.get("servers", {}).get(args.server_name)
        if server:
            _mcp_enable_for_profile(cfg, profile, args.server_name, server["config"])
            print(f"enabled: {args.server_name} for {profile}")
        return 0
    elif sub == "disable":
        # Remove from profile config
        ...
    elif sub == "check":
        # Run health checks for all configured servers
        ...
    else:
        # Fall through to Hermes pass-through
        return cmd_hermes_command(args, "mcp")
```

---

## 7. Implementation Plan

| Step | Task |
|------|------|
| 1 | Create `src/tag/config/mcp-registry.yaml` with 10+ curated servers |
| 2 | Implement `load_mcp_registry`, `mcp_install_server`, `_mcp_enable_for_profile` |
| 3 | Implement `mcp_list_for_profile` |
| 4 | Upgrade `cmd_mcp` with `registry`, `list`, `enable`, `disable`, `check` subcommands |
| 5 | Update parser to handle mcp subcommands before falling through to Hermes |
| 6 | Add `tag doctor` mcp server checks |
| 7 | Tests: `test_mcp_install_writes_profile_config`, `test_mcp_list_reads_profile_config` |
| 8 | Update README with MCP registry section |

---

## 8. Success Metrics

- `tag mcp registry search` lists 10+ servers.
- `tag mcp registry install mcp-filesystem --profile coder` updates coder's config.yaml.
- `tag mcp list --profile coder` shows installed servers.
- Hermes picks up the MCP server on next `tag chat`.

---

## 9. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| MCP server config format changes between Hermes versions | Write config generically; test against vendor tarball |
| npm install fails in restricted environments | Support `--local-only` to skip install, configure existing server |
| Registry becomes outdated | Embed version in registry YAML; `tag mcp registry update` fetches latest from GitHub |
