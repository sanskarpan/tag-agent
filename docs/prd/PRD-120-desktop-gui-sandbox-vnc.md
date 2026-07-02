# PRD-120: Desktop GUI Sandbox VNC (`tag sandbox --vnc`)
> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** L (8-13 days)
**Category:** Computer Use
**Affects:** `internal/sandbox (vnc) + internal/cli + internal/store`
**Depends on:** PRD-028 (sandbox code execution), PRD-092 (desktop GUI sandbox — basic VNC), PRD-118 (computer use CLI), PRD-089 (sandbox streaming)
**Inspired by:** Anthropic computer-use demo (Docker+Xvfb+VNC), E2B desktop sandbox, Daytona desktop VM, Browserbase cloud browser

---

## 1. Overview

Running computer use AI agents (PRD-118, PRD-119) on the engineer's actual desktop is unsafe: the model can accidentally close important windows, modify system files, or access sensitive data outside the task scope. Production computer use requires an isolated virtual desktop that the model can control but that has no access to the host system.

Desktop GUI Sandbox VNC (`tag sandbox --vnc`) extends TAG's sandbox system (PRD-028) with a virtual desktop environment: an Xvfb virtual display + a VNC server + a lightweight window manager (Openbox or XFCE) running in a container or VM. **The desktop environment, VNC server, and container/VM runtime are HOST/container dependencies — exactly like Docker is in any language. TAG orchestrates them; it does not bundle or embed a desktop.** The sandbox exposes a VNC endpoint that TAG's computer-use loop (PRD-119) connects to for screenshot capture and action dispatch.

Orchestration flows through TAG's `internal/sandbox` isolation ladder (docker/moby client → gVisor `runsc` runtime → Firecracker microVM). These kernel-level tiers are **Linux-centric** (Firecracker requires `/dev/kvm`); off-Linux, TAG degrades to Docker Desktop or plain-subprocess and disables the stronger tiers with a clear diagnostic. The sandbox is isolated from the host filesystem and network (configurable egress rules via PRD-094, enforced with `google/nftables` on Linux).

The design is inspired by Anthropic's computer-use demo repository (Docker + Xvfb + x11vnc + noVNC for browser access), E2B's desktop sandbox (Xvfb + VNC in a VM), and Daytona's workspace VMs. TAG's implementation uses the Docker/moby client (primary) with a documented fallback to Xvfb on the host for development environments without Docker.

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
| G1 | `tag sandbox --vnc start` launches a container (via the moby client) with an Xvfb virtual display, x11vnc server, and Openbox WM. |
| G2 | The VNC sandbox exposes a local VNC endpoint (127.0.0.1:5900) for TAG's computer-use loop connection. |
| G3 | PRD-119 `ComputerUseLoop` uses the VNC RFB driver for screenshot capture and action dispatch when running in sandbox mode. |
| G4 | Filesystem isolation: the sandbox container has no host filesystem mounts (except configurable read-only data volumes). |
| G5 | Network isolation: sandbox container egress filtered by PRD-094 egress firewall rules (`google/nftables` on Linux). |
| G6 | `tag sandbox --vnc stop [--session-id ID]` cleanly terminates the sandbox container. |
| G7 | Snapshot/restore: `tag sandbox --vnc snapshot` commits the current sandbox state as a container image layer for rapid restore. |
| G8 | noVNC web viewer: optional `--web-viewer` flag starts a noVNC HTTP server (in the container) for browser-based observation. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | GPU-accelerated rendering in the sandbox. |
| NG2 | macOS or Windows sandbox (Linux/container only; degrade off-Linux). |
| NG3 | Multi-user shared sandbox. Each session gets its own isolated container. |
| NG4 | Bundling or embedding a desktop environment in the TAG binary. TAG orchestrates a host/container desktop, it does not ship one. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Container startup time | VNC sandbox ready in < 15s from `sandbox --vnc start` | `testing.B` benchmark |
| Screenshot capture latency via VNC | < 300ms per screenshot via RFB protocol | `testing.B` benchmark |
| Filesystem isolation | Cannot read `/etc/passwd` from the host within the sandbox | Security test |
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
  snapshot    Save sandbox state as a container image layer
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
  --image         Container image for the desktop sandbox
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
| FR-01 | `tag sandbox --vnc start` pulls the `tag-desktop` image (or builds it from the embedded Dockerfile via `go:embed`), runs it through the moby client with configured display/VNC settings, and records the container ID and VNC endpoint in the `vnc_sandbox_sessions` table (`modernc.org/sqlite`). |
| FR-02 | The container runs: Xvfb (virtual display), x11vnc (VNC server), Openbox (WM), and optionally noVNC (web viewer). These are container-image components, not TAG binary components. |
| FR-03 | The VNC endpoint (127.0.0.1:5900) is forwarded from container port 5900; the endpoint is stored in SQLite for PRD-119 to connect. |
| FR-04 | PRD-119 `ComputerUseLoop` with the `--sandbox` flag queries `vnc_sandbox_sessions` to get the VNC endpoint for the current session ID. |
| FR-05 | Screenshot capture via the VNC backend uses the RFB protocol (`github.com/amitbet/vnc2video` framebuffer capture, or a `mitchellh/go-vnc`-style client) to request a framebuffer update. |
| FR-06 | Action dispatch via the VNC backend: translate cursor coordinates, mouse events, and key events to RFB `PointerEvent`/`KeyEvent` protocol messages. |
| FR-07 | `tag sandbox --vnc snapshot` calls the moby client's `ContainerCommit` (equivalent to `docker commit`) and stores the resulting image tag in SQLite. |
| FR-08 | `tag sandbox --vnc restore` stops the current container (if running), runs a new container from the snapshot image, and updates the SQLite session record. |
| FR-09 | The container runs with hardened flags via the moby client `HostConfig`: `SecurityOpt: ["no-new-privileges"]`, `CapDrop: ["ALL"]`, and (Linux) the `internal/sandbox` ladder tier (gVisor `runtime: runsc`, or Firecracker) when available. |
| FR-10 | `tag sandbox --vnc stop` calls the moby client's `ContainerStop` and updates the session status in SQLite. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Container image size < 1GB (use Alpine or Ubuntu Minimal base). |
| NFR-02 | VNC screenshot via RFB protocol < 300ms P95 for a 1280×800 display. |
| NFR-03 | Container resource limits enforced via moby `HostConfig`: `Memory: 2GiB`, `NanoCPUs: 2.0` by default. |
| NFR-04 | Host port binding on `127.0.0.1` only; never `0.0.0.0`. |
| NFR-05 | No host filesystem mounts (`Binds`) unless explicitly configured via `--data-volume HOST:CONTAINER:ro`. |

---

## 9. Technical Design

### 9.1 Dockerfile (embedded via `go:embed`)

The desktop image is a container artifact orchestrated by TAG, not embedded in the Go binary. The Dockerfile text is embedded via `go:embed` so `tag sandbox --vnc start` can build it on first use if the image is absent.

```dockerfile
FROM ubuntu:22.04
RUN apt-get update && apt-get install -y \
    xvfb x11vnc openbox novnc websockify \
    xterm \
    && rm -rf /var/lib/apt/lists/*
EXPOSE 5900 6080
ENV DISPLAY=:1 SCREEN_WIDTH=1280 SCREEN_HEIGHT=800
CMD ["/start.sh"]
```

### 9.2 SQLite DDL

Stored in `modernc.org/sqlite` (pure-Go, CGO-free) under the single-writer contract. The schema is language-neutral SQL; Go maps rows to a `VNCSession` struct.

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

```go
package sandbox

type VNCSession struct {
    ID             string    `json:"id"`
    ContainerID    string    `json:"container_id"`
    VNCEndpoint    string    `json:"vnc_endpoint"`
    WebEndpoint    string    `json:"web_endpoint,omitempty"`
    DisplayWidth   int       `json:"display_width"`
    DisplayHeight  int       `json:"display_height"`
    Status         string    `json:"status"`
    SnapshotImage  string    `json:"snapshot_image,omitempty"`
    CreatedAt      time.Time `json:"created_at"`
    StoppedAt      *time.Time `json:"stopped_at,omitempty"`
}
```

Struct-to-JSON-Schema (for any wire/config surface) is derived with `github.com/invopop/jsonschema` rather than hand-written schemas.

### 9.3 Container orchestration (moby client)

TAG drives the container via the official Docker/moby Go client (`github.com/docker/docker/client`). The `internal/sandbox` ladder selects the strongest available runtime on Linux and degrades off-Linux.

```go
package sandbox

import (
    "context"

    "github.com/docker/docker/api/types/container"
    "github.com/docker/go-connections/nat"
    "github.com/docker/docker/client"
)

func (s *VNCSandbox) Start(ctx context.Context, cfg StartConfig) (VNCSession, error) {
    cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
    if err != nil {
        return VNCSession{}, err
    }

    hostCfg := &container.HostConfig{
        SecurityOpt: []string{"no-new-privileges"},
        CapDrop:     []string{"ALL"},
        Runtime:     s.ladder.Runtime(), // "runsc" (gVisor) on Linux when present, else "" (runc)
        Resources: container.Resources{
            Memory:   cfg.MemoryLimitBytes, // default 2 GiB
            NanoCPUs: cfg.NanoCPUs,         // default 2.0
        },
        PortBindings: nat.PortMap{
            "5900/tcp": []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: cfg.VNCPort}},
        },
        // No Binds unless --data-volume was passed (read-only).
    }

    created, err := cli.ContainerCreate(ctx, &container.Config{
        Image: cfg.Image,
        Env:   []string{fmt.Sprintf("SCREEN_WIDTH=%d", cfg.Width), fmt.Sprintf("SCREEN_HEIGHT=%d", cfg.Height)},
        ExposedPorts: nat.PortSet{"5900/tcp": struct{}{}},
    }, hostCfg, nil, nil, cfg.SessionID)
    if err != nil {
        return VNCSession{}, err
    }
    if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
        return VNCSession{}, err
    }

    sess := VNCSession{
        ID:          cfg.SessionID,
        ContainerID: created.ID,
        VNCEndpoint: "127.0.0.1:" + cfg.VNCPort,
        Status:      "running",
        CreatedAt:   time.Now().UTC(),
    }
    return sess, s.store.InsertVNCSession(ctx, sess) // single-writer SQLite
}
```

`stop`, `snapshot`, and `restore` map to `ContainerStop`, `ContainerCommit`, and a stop-then-`ContainerCreate`-from-image sequence respectively. Egress rules (PRD-094) are applied on Linux via `google/nftables` against the container's network namespace.

### 9.4 Go VNC client (RFB screenshot)

The VNC client is pure Go (no host desktop libs), so it works from the CGO-free single binary. It captures the framebuffer over RFB for PRD-119's screenshot backend.

```go
package sandbox

import (
    "context"
    "image"
    "net"
    "time"

    vnc "github.com/amitbet/vnc2video" // framebuffer capture; input via ClientMessage events
)

// CaptureVNCScreenshot connects over RFB and returns one framebuffer as an image.
func CaptureVNCScreenshot(ctx context.Context, endpoint string, width, height int) (image.Image, error) {
    conn, err := net.DialTimeout("tcp", endpoint, 5*time.Second)
    if err != nil {
        return nil, err
    }
    defer conn.Close()

    ccfg := &vnc.ClientConfig{
        PixelFormat:      vnc.PixelFormat32bit,
        ClientMessageCh:  make(chan vnc.ClientMessage),
        ServerMessageCh:  make(chan vnc.ServerMessage),
        Messages:         vnc.DefaultServerMessages,
        Exclusive:        true,
    }
    client, err := vnc.Connect(ctx, conn, ccfg)
    if err != nil {
        return nil, err
    }
    defer client.Close()

    // Request a full framebuffer update, then read the decoded canvas.
    if err := client.Reset(); err != nil {
        return nil, err
    }
    return client.Canvas.Image, nil // *image.RGBA; encode to PNG at the caller
}
```

Input dispatch (clicks, key presses, scroll) is sent as RFB `PointerEvent` / `KeyEvent` client messages; the coordinate math translates Claude tool-call coordinates directly to framebuffer pixels.

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Container escape | moby `HostConfig` `no-new-privileges` + `CapDrop: ALL`, no host filesystem `Binds`; stronger `internal/sandbox` ladder tier (gVisor/Firecracker) on Linux |
| VNC accessible from external hosts | Bind only to `127.0.0.1`; never `0.0.0.0` |
| Container persisting after session end | Auto-stop on session completion; `AutoRemove: true` on the moby `HostConfig` for auto-cleanup |
| noVNC browser access from other users | Web viewer binds to `127.0.0.1` only; authentication optional |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit (table-driven) | VNC session SQLite CRUD; endpoint parsing; container ID storage |
| Integration | Start container → RFB connect → screenshot → stop container (gated on a Docker-available build tag) |
| Security | Filesystem isolation: attempt host file access from within the container |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | `tag sandbox --vnc start` produces a running container with VNC accessible on `127.0.0.1:5900` |
| AC-02 | PRD-119 loop with `--sandbox` connects over RFB and captures a screenshot |
| AC-03 | The container cannot read `/etc/passwd` from the host |
| AC-04 | `tag sandbox --vnc stop` terminates the container within 5 seconds |
| AC-05 | `tag sandbox --vnc snapshot` creates a container image tag stored in SQLite |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| Docker / container runtime | Host dependency — container runtime (same requirement in any language) |
| `github.com/docker/docker/client` (moby) | Programmatic container lifecycle from Go |
| `internal/sandbox` isolation ladder | gVisor `runsc` / Firecracker tiers (Linux); degrade off-Linux |
| `modernc.org/sqlite` | Pure-Go session state store (single-writer) |
| VNC client (`github.com/amitbet/vnc2video` / `mitchellh/go-vnc`) | RFB framebuffer capture + input dispatch |
| `google/nftables` | Egress firewall for the sandbox container (PRD-094, Linux) |
| PRD-028 sandbox execution | Base sandbox infrastructure |
| PRD-094 egress firewall | Network isolation for the sandbox container |

> **Platform note:** The desktop stack (Xvfb/x11vnc/Openbox/noVNC) is a **container image dependency**, and the kernel-level isolation tiers (gVisor, Firecracker) are **Linux-only host dependencies** (Firecracker needs `/dev/kvm`). TAG orchestrates them via the moby client and feature-detects the ladder — off-Linux it falls back to Docker Desktop / plain container isolation and disables the stronger tiers with a diagnostic. TAG never bundles a desktop environment in its binary.

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
| 1 | Embedded Dockerfile, base image build, container start/stop via moby client | 2 |
| 2 | SQLite session management, VNC endpoint tracking | 1 |
| 3 | VNC screenshot capture via RFB (`vnc2video`) | 2 |
| 4 | Action dispatch via RFB, PRD-119 integration | 2 |
| 5 | Snapshot/restore, noVNC web viewer, security hardening + ladder wiring | 2 |
| 6 | Integration tests, benchmarks, documentation | 2 |
