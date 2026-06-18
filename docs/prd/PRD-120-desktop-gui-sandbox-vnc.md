# PRD-120: Desktop GUI Sandbox VNC (`tag sandbox --vnc`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** L (8-13 days)
**Category:** Computer Use
**Affects:** `sandbox_vnc.py + controller.py`
**Depends on:** PRD-028 (sandbox code execution), PRD-092 (desktop GUI sandbox — basic VNC), PRD-118 (computer use CLI), PRD-089 (sandbox streaming)
**Inspired by:** Anthropic computer-use demo (Docker+Xvfb+VNC), E2B desktop sandbox, Daytona desktop VM, Browserbase cloud browser

---

## 1. Overview

Running computer use AI agents (PRD-118, PRD-119) on the engineer's actual desktop is unsafe: the model can accidentally close important windows, modify system files, or access sensitive data outside the task scope. Production computer use requires an isolated virtual desktop that the model can control but that has no access to the host system.

Desktop GUI Sandbox VNC (`tag sandbox --vnc`) extends TAG's sandbox system (PRD-028) with a virtual desktop environment: an Xvfb virtual display + a VNC server + a lightweight window manager (Openbox or XFCE) running in a Docker container or VM. The sandbox exposes a VNC endpoint that TAG's computer-use loop (PRD-119) connects to for screenshot capture and action dispatch. The sandbox is isolated from the host filesystem and network (configurable egress rules via PRD-094).

The design is inspired by Anthropic's computer-use demo repository (Docker + Xvfb + x11vnc + noVNC for browser access), E2B's desktop sandbox (Xvfb + VNC in a VM), and Daytona's workspace VMs. TAG's implementation uses Docker (primary) with a fallback to Xvfb on the host for development environments without Docker.

---

## 2. Problem Statement

### 2.1 Computer use runs on the host desktop

Without a sandbox, computer-use agents control the engineer's actual desktop. A single misclick on the wrong window, or a `Ctrl+A` followed by `Delete` in the wrong application, can cause irreversible damage.

### 2.2 No isolation from host filesystem

Computer-use agents can read and write the host filesystem — including sensitive files like SSH keys, credentials, and source code outside the task scope.

### 2.3 No reproducible desktop environment

Each computer-use session on the host desktop starts with a different desktop state (open windows, running apps). A sandbox provides a clean, reproducible starting state for each session.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | `tag sandbox --vnc start` launches a Docker container with Xvfb virtual display, x11vnc server, and Openbox WM. |
| G2 | The VNC sandbox exposes a local VNC endpoint (127.0.0.1:5900) for TAG's computer-use loop connection. |
| G3 | PRD-119 `ComputerUseLoop` uses the VNC backend for screenshot capture and action dispatch when running in sandbox mode. |
| G4 | Filesystem isolation: sandbox container has no host filesystem mounts (except configurable read-only data volumes). |
| G5 | Network isolation: sandbox container egress filtered by PRD-094 egress firewall rules. |
| G6 | `tag sandbox --vnc stop [--session-id ID]` cleanly terminates the sandbox container. |
| G7 | Snapshot/restore: `tag sandbox --vnc snapshot` saves the current sandbox state as a Docker image layer for rapid restore. |
| G8 | noVNC web viewer: optional `--web-viewer` flag starts a noVNC HTTP server for browser-based observation. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | GPU-accelerated rendering in the sandbox. |
| NG2 | macOS or Windows sandbox (Linux/Docker only). |
| NG3 | Multi-user shared sandbox. Each session gets its own isolated container. |
| NG4 | Full VM isolation (KVM/QEMU). Docker container isolation only in this PRD. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Container startup time | VNC sandbox ready in < 15s from `sandbox --vnc start` | Benchmark test |
| Screenshot capture latency via VNC | < 300ms per screenshot via RFB protocol | Benchmark test |
| Filesystem isolation | Cannot read `/etc/passwd` from within sandbox | Security test |
| Container cleanup | `sandbox --vnc stop` terminates container in < 5s | Integration test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Developer | Run computer use in a sandboxed desktop | I protect my host system from accidental actions |
| US2 | Developer | Connect my computer-use loop to the VNC sandbox automatically | I don't need to configure VNC connections manually |
| US3 | Developer | Start the sandbox with `--web-viewer` and watch the session in a browser | I observe what the agent is doing |
| US4 | Security engineer | Know the sandbox can't access my host files or network | I trust computer use for production tasks |

---

## 6. CLI Surface

```
tag sandbox --vnc <subcommand> [options]

Subcommands:
  start       Launch a VNC sandbox container
  stop        Stop a VNC sandbox container
  list        List running VNC sandbox sessions
  screenshot  Capture a screenshot from the sandbox
  snapshot    Save sandbox state as a Docker image layer
  restore     Restore from a saved snapshot
  logs        Show sandbox container logs

tag sandbox --vnc start \
  [--image tag-desktop:latest] \
  [--display-width 1280] \
  [--display-height 800] \
  [--vnc-port 5900] \
  [--web-viewer / --no-web-viewer] \
  [--web-port 6080] \
  [--session-id ID] \
  [--memory-limit 2g] \
  [--cpu-limit 2.0] \
  [--egress-rules deny-all|allow-http-only|custom]

tag sandbox --vnc stop [--session-id ID | --all]
tag sandbox --vnc screenshot [--session-id ID] [--output PATH]
tag sandbox --vnc snapshot [--session-id ID] [--tag NAME]
tag sandbox --vnc restore --snapshot NAME [--session-id ID]
tag sandbox --vnc logs [--session-id ID] [--tail N]

Options:
  --image         Docker image for the desktop sandbox
  --display-width Virtual display width (default: 1280)
  --display-height Virtual display height (default: 800)
  --vnc-port      Host VNC port (default: 5900)
  --web-viewer    Start noVNC HTTP server for browser access
  --web-port      noVNC HTTP port (default: 6080)
  --memory-limit  Container memory limit (default: 2g)
  --cpu-limit     Container CPU quota (default: 2.0)
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `tag sandbox --vnc start` pulls the `tag-desktop` Docker image (or builds it from the embedded Dockerfile), runs it with configured display/VNC settings, and records the container ID and VNC endpoint in `vnc_sandbox_sessions` SQLite table. |
| FR-02 | The Docker container runs: Xvfb (virtual display), x11vnc (VNC server), Openbox (WM), and optionally novnc (web viewer). |
| FR-03 | VNC endpoint (127.0.0.1:5900) is forwarded from container port 5900; the endpoint is stored in SQLite for PRD-119 to connect. |
| FR-04 | PRD-119 `ComputerUseLoop` with `--sandbox` flag queries `vnc_sandbox_sessions` to get the VNC endpoint for the current session_id. |
| FR-05 | Screenshot capture via VNC backend uses the RFB protocol (`vncdotool` library or `python-xlib`) to request a framebuffer update. |
| FR-06 | Action dispatch via VNC backend: translate cursor coordinates, mouse events, and key events to RFB protocol messages. |
| FR-07 | `tag sandbox --vnc snapshot` calls `docker commit <container_id> <tag>` and stores the image tag in SQLite. |
| FR-08 | `tag sandbox --vnc restore` stops the current container (if running), runs a new container from the snapshot image, and updates the SQLite session record. |
| FR-09 | Container runs with `--no-new-privileges --security-opt=no-new-privileges --cap-drop=ALL` Docker security flags. |
| FR-10 | `tag sandbox --vnc stop` calls `docker stop <container_id>` and updates session status in SQLite. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Docker image size < 1GB (use Alpine or Ubuntu Minimal base). |
| NFR-02 | VNC screenshot via RFB protocol < 300ms P95 for 1280×800 display. |
| NFR-03 | Container resource limits enforced: `--memory 2g`, `--cpus 2.0` by default. |
| NFR-04 | Host port binding on 127.0.0.1 only; never 0.0.0.0. |
| NFR-05 | No host filesystem mounts unless explicitly configured via `--data-volume HOST:CONTAINER:ro`. |

---

## 9. Technical Design

### 9.1 Dockerfile (embedded)

```dockerfile
FROM ubuntu:22.04
RUN apt-get update && apt-get install -y \
    xvfb x11vnc openbox novnc websockify \
    python3 python3-pip xterm \
    && rm -rf /var/lib/apt/lists/*
EXPOSE 5900 6080
ENV DISPLAY=:1 SCREEN_WIDTH=1280 SCREEN_HEIGHT=800
CMD ["/start.sh"]
```

### 9.2 SQLite DDL

```sql
CREATE TABLE IF NOT EXISTS vnc_sandbox_sessions (
  id              TEXT PRIMARY KEY,
  container_id    TEXT,
  vnc_endpoint    TEXT NOT NULL DEFAULT '127.0.0.1:5900',
  web_endpoint    TEXT,
  display_width   INTEGER NOT NULL DEFAULT 1280,
  display_height  INTEGER NOT NULL DEFAULT 800,
  status          TEXT NOT NULL DEFAULT 'running',
  snapshot_image  TEXT,
  created_at      TEXT NOT NULL,
  stopped_at      TEXT
);
```

### 9.3 Python VNC client (RFB screenshot)

```python
from __future__ import annotations
import socket
import struct

def capture_vnc_screenshot(host: str, port: int, width: int, height: int) -> bytes:
    """Minimal RFB framebuffer capture using raw socket."""
    try:
        import vncdotool.api as vnc
        client = vnc.connect(host, password="", port=port)
        return client.captureScreen(None)
    except ImportError:
        # Fallback: raw socket RFB handshake + FramebufferUpdateRequest
        raise NotImplementedError("vncdotool required for VNC screenshot capture")
```

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Container escape | Docker `--no-new-privileges`, `--cap-drop=ALL`, no host filesystem mounts |
| VNC accessible from external hosts | Bind only to `127.0.0.1`; no 0.0.0.0 binding |
| Container persisting after session end | Auto-stop on session completion; `--rm` Docker flag for auto-cleanup |
| noVNC browser access from other users | Web viewer binds to `127.0.0.1` only; authentication optional |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | VNC session SQLite CRUD; endpoint parsing; container ID storage |
| Integration | Start container → VNC connect → screenshot → stop container |
| Security | Filesystem isolation: attempt host file access from within container |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | `tag sandbox --vnc start` produces a running container with VNC accessible on 127.0.0.1:5900 |
| AC-02 | PRD-119 loop with `--sandbox` connects to VNC and captures a screenshot |
| AC-03 | Container cannot read `/etc/passwd` from host |
| AC-04 | `tag sandbox --vnc stop` terminates container within 5 seconds |
| AC-05 | `tag sandbox --vnc snapshot` creates a Docker image tag stored in SQLite |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| Docker | Container runtime |
| PRD-028 sandbox execution | Base sandbox infrastructure |
| PRD-094 egress firewall | Network isolation for sandbox container |
| `vncdotool` (optional) | VNC screenshot capture |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should the desktop image include a specific browser by default (Chromium/Firefox)? |
| OQ-02 | Should sandbox containers share a base image layer cache for faster startup? |

---

## 15. Complexity & Timeline

**Complexity:** Large (L)
**Estimated effort:** 8–13 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | Dockerfile, base image build, container start/stop | 2 |
| 2 | SQLite session management, VNC endpoint tracking | 1 |
| 3 | VNC screenshot capture via RFB / vncdotool | 2 |
| 4 | Action dispatch via VNC, PRD-119 integration | 2 |
| 5 | Snapshot/restore, noVNC web viewer, security hardening | 2 |
| 6 | Integration tests, documentation | 2 |
