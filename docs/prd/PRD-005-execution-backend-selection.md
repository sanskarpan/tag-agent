# PRD-005: Execution Backend Selection Per Profile

**Status:** Proposed  
**Priority:** P1  
**Estimated Effort:** S–M (1–2 weeks)  
**Affects:** `controller.py` (`render_profiles`, `hermes_env`, `profile_exec_env`), `default.yaml`

---

## 1. Overview

Hermes supports 6 terminal execution backends: `local`, `docker`, `ssh`, `daytona`, `modal`, and `singularity`. TAG exposes none of these — every profile always uses the `local` backend. This PRD defines how TAG surfaces backend selection as a per-profile config key, adds credential import commands for Docker and SSH backends, and ensures `tag setup` and `tag doctor` are aware of backend requirements.

---

## 2. Problem Statement

- Developers who want isolated execution (Docker containers per task) must hand-edit Hermes config.
- Cloud developers using Modal for GPU workloads cannot configure it via TAG.
- Remote SSH execution (for running agents on a powerful workstation from a laptop) is completely undiscoverable.
- There is no validation that required tools (Docker daemon, SSH key) are available before attempting execution.

---

## 3. Goals

1. `execution_backend` key in `default.yaml` profiles selects the backend.
2. `render_profiles()` writes the backend config into each profile's Hermes `config.yaml`.
3. `tag import-docker` and `tag import-ssh` commands write backend credentials to profile `.env`.
4. `tag doctor` checks that required binaries/credentials are present for the configured backend.
5. All 6 backends have at least stubs in the import system (even if some are trivial).

---

## 4. Non-Goals

- Building custom execution backends — TAG configures Hermes' backends.
- Container image management — that's Docker's concern.
- Cross-profile backend mixing in a single run — each profile has one backend.

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | set `execution_backend: docker` on the coder profile | code execution is isolated from my host OS |
| U2 | DevOps | run `tag import-ssh --profile coder --host myserver.example.com` | the coder profile runs on a remote server |
| U3 | ML researcher | set `execution_backend: modal` on researcher | GPU workloads run in Modal cloud |
| U4 | Developer | run `tag doctor` and see "docker: available / SSH key: missing" | I can fix config issues before running |
| U5 | Developer | not change anything | `local` backend is the default, no change from current behavior |

---

## 6. Technical Design

### 6.1 default.yaml schema extension

```yaml
profiles:
  coder:
    config:
      execution:
        backend: docker           # local | docker | ssh | daytona | modal | singularity
        docker:
          image: "ubuntu:22.04"
          auto_pull: true
          extra_volumes: []       # list of "host:container" strings
        ssh:
          host: ""
          user: ""
          port: 22
          key_file: "~/.ssh/id_rsa"
          remote_work_dir: "/tmp/tag-agent"
        modal:
          app_name: "tag-coder"
          gpu: "T4"
        daytona:
          workspace_id: ""
```

Default for all profiles: `backend: local` (no config change).

### 6.2 `render_profiles()` changes

After writing existing config keys, write execution backend config:

```python
exec_cfg = profile_data.get("config", {}).get("execution", {})
backend = exec_cfg.get("backend", "local")

if backend != "local":
    profile_config["execution"] = {"backend": backend}
    if backend == "docker":
        profile_config["execution"]["docker"] = exec_cfg.get("docker", {})
    elif backend == "ssh":
        profile_config["execution"]["ssh"] = {
            "host": exec_cfg.get("ssh", {}).get("host", ""),
            "user": exec_cfg.get("ssh", {}).get("user", ""),
            "port": exec_cfg.get("ssh", {}).get("port", 22),
            "key_file": str(resolve_home_relative(
                exec_cfg.get("ssh", {}).get("key_file", "~/.ssh/id_rsa")
            )),
            "remote_work_dir": exec_cfg.get("ssh", {}).get("remote_work_dir", "/tmp/tag-agent"),
        }
    # ... modal, daytona, singularity similarly
```

### 6.3 New credential import functions

#### `_detect_docker_credentials() -> dict[str, str]`
Check `docker info` — if it works, Docker is available. Read `~/.docker/config.json` for registry auth token. Return `{"DOCKER_AVAILABLE": "1"}` plus any registry tokens.

#### `import_docker_into_profile(cfg, profile_name, *, image=None, force=False) -> dict`
Write `DOCKER_DEFAULT_IMAGE` to `.env`. Optionally update profile's `default.yaml` `execution.docker.image`.

#### `_detect_ssh_credentials(host: str) -> dict[str, str]`
Check `~/.ssh/config` and `~/.ssh/known_hosts` for host entry. Return `{"SSH_HOST": host}` if found.

#### `import_ssh_into_profile(cfg, profile_name, *, host, user, key_file=None, force=False) -> dict`
Write `SSH_HOST`, `SSH_USER`, `SSH_KEY_FILE` to profile `.env`. Update profile config `execution.ssh.*`.

#### `import_modal_into_profile(cfg, profile_name, *, token_id, token_secret, force=False) -> dict`
Write `MODAL_TOKEN_ID`, `MODAL_TOKEN_SECRET` to profile `.env`.

### 6.4 New CLI commands

```
tag import-docker  [--profile PROFILE] [--image IMAGE] [--force]
tag import-ssh     [--profile PROFILE] --host HOST [--user USER] [--key-file PATH] [--port N] [--force]
tag import-modal   [--profile PROFILE] --token-id ID --token-secret SECRET [--force]
tag import-daytona [--profile PROFILE] --workspace-id ID [--force]
```

### 6.5 `tag doctor` checks

Per profile:
- `backend: docker` → run `docker info >/dev/null 2>&1`; pass/fail.
- `backend: ssh` → run `ssh -o BatchMode=yes -o ConnectTimeout=3 <user>@<host> exit`; pass/fail.
- `backend: modal` → check `MODAL_TOKEN_ID` in env; check `modal --version`; pass/fail.
- `backend: local` → always pass.

---

## 7. Implementation Plan

| Step | Task |
|------|------|
| 1 | Add `execution` schema to `default.yaml` with `backend: local` default |
| 2 | Update `render_profiles()` to write execution backend config |
| 3 | Implement `_detect_docker_credentials`, `import_docker_into_profile` |
| 4 | Implement `_detect_ssh_credentials`, `import_ssh_into_profile` |
| 5 | Implement `import_modal_into_profile`, `import_daytona_into_profile` |
| 6 | Add parser registrations for all 4 new import commands |
| 7 | Update `cmd_doctor` with backend health checks |
| 8 | Add tests: `test_render_profiles_writes_docker_backend`, `test_import_ssh_writes_env` |
| 9 | Update README execution backends section |

---

## 8. Success Metrics

- Profile with `execution_backend: docker` produces `config.yaml` with correct Docker backend keys.
- `tag import-docker --profile coder` writes `DOCKER_DEFAULT_IMAGE` to `.env`.
- `tag doctor` reports backend status without errors.
- `local` backend profiles are unaffected by the change.

---

## 9. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Hermes backend config key names don't match our assumptions | Verify against vendor tarball before implementing step 2 |
| Docker socket permission issues in test environments | Make docker check a warning, not error, in `tag doctor` |
| SSH key passphrase prompts interrupt automation | Document that passwordless SSH keys are required; add to `tag doctor` warning |
| Modal SDK version compatibility | Check `modal --version` in `tag doctor`; document minimum version |
