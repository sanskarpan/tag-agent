# PRD-009: Enhanced `tag doctor` Diagnostics

**Status:** Proposed  
**Priority:** P1  
**Estimated Effort:** S (3–5 days)  
**Affects:** `controller.py` (`cmd_doctor`, `doctor_prerequisites`)

---

## 1. Overview

The current `tag doctor` command is a minimal dump of paths and binary versions. It does not check per-profile health, credential validity, memory backend reachability, execution backend availability, or queue worker status. This PRD upgrades `tag doctor` into a comprehensive health-check command that gives users a clear pass/warn/fail status for every component of their TAG installation, with actionable fix suggestions.

---

## 2. Problem Statement

- `tag doctor` today prints a flat key→value dump with no color, no pass/fail, and no suggestions.
- Users have no way to know if their credential import worked without actually running a task.
- When `tag setup` completes, users can't verify that everything is correctly wired.
- Memory backend configuration, execution backends, and gateway settings have no health checks.
- There is no way to diagnose why a profile is failing without manual log inspection.

---

## 3. Goals

1. `tag doctor` produces a Rich-formatted table (or plain-text equivalent) with three-level status: ✓ pass / ⚠ warn / ✗ fail.
2. Every check includes a suggested fix command when it fails.
3. Checks are organized into groups: System, Hermes Runtime, Profiles (per-profile), Memory, Execution Backends, Gateway, Queue.
4. `tag doctor --profile researcher` runs checks only for that profile.
5. `tag doctor --json` outputs machine-readable JSON for CI/scripting.
6. `tag doctor --fix` auto-runs suggested fix commands for safe, non-destructive fixes.

---

## 4. Non-Goals

- Automated fixes for destructive operations (re-running setup, deleting profiles).
- Network connectivity checks beyond the configured backends.

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | New user | run `tag doctor` after setup | I immediately see if anything is wrong |
| U2 | Developer | see "researcher: OPENROUTER_API_KEY missing" with fix hint | I know exactly what to run |
| U3 | DevOps | pipe `tag doctor --json` into a health check script | CI fails early if TAG environment is broken |
| U4 | Developer | run `tag doctor --fix` | safe fixes are applied automatically |
| U5 | Developer | run `tag doctor --profile coder` | I focus on one profile |

---

## 6. Technical Design

### 6.1 Check result structure

```python
from dataclasses import dataclass
from typing import Literal

@dataclass
class CheckResult:
    group: str
    name: str
    status: Literal["pass", "warn", "fail"]
    message: str
    fix_cmd: str | None = None
    fix_fn: callable | None = None  # auto-fixable if not None
```

### 6.2 Check groups

#### System checks
```python
def check_python_version() -> CheckResult:
    ok = python_runtime_supported(sys.version_info[:2])
    return CheckResult("system", "python_version",
        "pass" if ok else "fail",
        f"Python {sys.version_info[:3]} {'supported' if ok else 'not supported'}",
        fix_cmd="Install Python 3.11–3.13" if not ok else None)

def check_node_version() -> CheckResult:
    node = tool_version(["node", "--version"])
    major = int(node.lstrip("v").split(".")[0]) if node else 0
    ok = major >= 18
    return CheckResult("system", "node_version",
        "pass" if ok else "warn",
        f"Node.js {node or 'not found'} (need ≥18)",
        fix_cmd="brew install node" if not ok else None)

def check_npm_available() -> CheckResult: ...
def check_git_available() -> CheckResult: ...
def check_disk_space(cfg) -> CheckResult:
    # warn if < 1GB free in tag_home
    ...
```

#### Hermes runtime checks
```python
def check_hermes_binary(cfg) -> CheckResult:
    exists = hermes_bin(cfg).exists()
    return CheckResult("hermes", "binary",
        "pass" if exists else "fail",
        str(hermes_bin(cfg)),
        fix_cmd="tag setup" if not exists else None)

def check_hermes_version(cfg) -> CheckResult:
    try:
        v = run_hermes(cfg, "--version").stdout.strip()
        return CheckResult("hermes", "version", "pass", v)
    except subprocess.CalledProcessError as e:
        return CheckResult("hermes", "version", "fail", str(e), fix_cmd="tag setup --refresh")

def check_hermes_venv(cfg) -> CheckResult: ...
def check_hermes_patch(cfg) -> CheckResult: ...
def check_tui_built(cfg) -> CheckResult: ...
```

#### Per-profile checks
```python
def check_profile_home(cfg, profile_name) -> CheckResult:
    ph = profile_home(cfg, profile_name)
    return CheckResult(f"profile:{profile_name}", "home",
        "pass" if ph.exists() else "fail",
        str(ph),
        fix_cmd=f"tag bootstrap --profile {profile_name}" if not ph.exists() else None)

def check_profile_env_keys(cfg, profile_name) -> list[CheckResult]:
    """Check that required API keys are present for configured providers."""
    env_file = profile_home(cfg, profile_name) / ".env"
    if not env_file.exists():
        return [CheckResult(f"profile:{profile_name}", "env_file", "warn",
                           "No .env file", fix_cmd=f"tag import-claude --profile {profile_name}")]
    
    env = read_dotenv(env_file)
    results = []
    
    # Check model provider key
    profile_cfg = load_config(config_path(None))["profiles"].get(profile_name, {})
    provider = profile_cfg.get("config", {}).get("model", {}).get("provider", "openrouter")
    
    required_keys = {
        "openrouter": "OPENROUTER_API_KEY",
        "anthropic": "ANTHROPIC_API_KEY",
        "openai": "OPENAI_API_KEY",
        "google": "GEMINI_API_KEY",
    }
    if key := required_keys.get(provider):
        has_key = bool(env.get(key))
        results.append(CheckResult(
            f"profile:{profile_name}", key,
            "pass" if has_key else "warn",
            "present" if has_key else "missing",
            fix_cmd=f"tag import-{provider.replace('anthropic','claude').replace('google','gemini')} --profile {profile_name}" if not has_key else None
        ))
    return results

def check_profile_config_yaml(cfg, profile_name) -> CheckResult:
    config_file = profile_home(cfg, profile_name) / "config.yaml"
    return CheckResult(f"profile:{profile_name}", "config.yaml",
        "pass" if config_file.exists() else "fail",
        str(config_file),
        fix_cmd=f"tag render --profile {profile_name}" if not config_file.exists() else None)
```

#### Memory checks (added when PRD-001 implemented)
```python
def check_memory_backend(cfg, profile_name) -> CheckResult: ...
```

#### Execution backend checks (added when PRD-005 implemented)
```python
def check_execution_backend(cfg, profile_name) -> CheckResult: ...
```

### 6.3 Rich output format

```
TAG Doctor Report ─────────────────────────────────────────────────────────────

SYSTEM
  ✓ python_version    Python 3.12.3 (supported)
  ✓ node_version      Node.js v22.4.0
  ✓ npm               10.8.1
  ⚠ disk_space        1.2 GB free in ~/.tag (warn: <2GB)

HERMES RUNTIME
  ✓ binary            ~/.tag/managed/hermes-agent-upstream/.venv/bin/hermes
  ✓ version           hermes 0.16.0
  ✓ venv              ~/.tag/managed/hermes-agent-upstream/.venv
  ✓ patch             applied
  ✓ tui               built

PROFILE: orchestrator
  ✓ home              ~/.tag/runtime/home/.hermes/profiles/orchestrator
  ✓ config.yaml       present
  ✗ OPENROUTER_API_KEY missing → run: tag import-openrouter --profile orchestrator

PROFILE: researcher
  ✓ home              present
  ✓ config.yaml       present
  ✓ OPENROUTER_API_KEY present

──────────────────────────────────────────────────────────────────────────────
Summary: 12 pass, 1 warn, 1 fail
Run with --fix to auto-apply safe fixes.
```

### 6.4 `--fix` mode

```python
if args.fix:
    failed = [c for c in all_checks if c.status == "fail" and c.fix_fn is not None]
    for check in failed:
        print(f"Fixing: {check.name}…")
        check.fix_fn()
```

Only auto-fix checks that have a `fix_fn` (i.e., safe, non-destructive). Checks with only a `fix_cmd` string show the command but don't run it.

---

## 7. Implementation Plan

| Step | Task |
|------|------|
| 1 | Define `CheckResult` dataclass |
| 2 | Implement system checks (python, node, npm, git, disk) |
| 3 | Implement Hermes runtime checks (binary, version, venv, patch, tui) |
| 4 | Implement per-profile checks (home, config, env keys) |
| 5 | Implement Rich output formatter for pass/warn/fail table |
| 6 | Add `--profile`, `--fix`, `--json`, `--group` args to `tag doctor` parser |
| 7 | Add tests: `test_doctor_reports_missing_binary`, `test_doctor_json_output_structure` |
| 8 | Update README with `tag doctor` section |

---

## 8. Success Metrics

- `tag doctor` after fresh `tag setup` shows all green (no fail).
- Deleting a profile home causes `tag doctor` to report `fail` for that profile.
- `tag doctor --json` produces valid JSON parseable by `jq`.
- `tag doctor --fix` runs `tag render` for profiles with missing `config.yaml`.

---

## 9. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Check too many things and become slow | Each check must complete in < 500ms; network checks have 3s timeout |
| False positives on CI where Hermes isn't installed | `--level core` flag for minimal checks only |
| Credential check reveals key existence to shell history | Never print key values; only check presence |
