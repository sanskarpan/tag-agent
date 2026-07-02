# PRD-092: Desktop/GUI Sandbox for Computer-Use (Ubuntu + Xfce + VNC Stream) (`tag sandbox run --gui`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** XL (4–8 weeks)
**Category:** Sandbox & Execution Environment
**Affects:** `internal/sandbox`, `internal/server`
**Depends on:** PRD-028 (Sandbox Code Execution), PRD-013 (Agent Tracing/Observability), PRD-034 (Secret Scanning), PRD-027 (Eval Framework), PRD-005 (Execution Backend Selection), PRD-040 (Notification Hooks)
**Inspired by:** E2B Desktop, Anthropic computer-use, Browserbase
**GitHub Issue:** #348

---

## 1. Overview

Modern AI agent workflows increasingly require more than a terminal. Web automation, form filling, GUI testing, native desktop app interaction, and visual verification tasks all demand a full graphical environment — a real browser with a real display, a desktop file manager, or an IDE running in a visible window. The existing TAG sandbox subsystem (PRD-028) provides excellent process-level and container-level isolation for headless code execution but has no concept of a display server, virtual framebuffer, VNC stream, or screenshot loop. This gap forces users to maintain separate tooling (E2B Desktop, Browserbase, or custom Docker Compose stacks with noVNC) alongside TAG, fragmenting workflow orchestration and making computer-use agents first-class citizens nowhere.

PRD-092 closes this gap by introducing a **Desktop GUI Sandbox** backend to the `internal/sandbox` Go package (with proxy/streaming endpoints in `internal/server`). The design provisions an Ubuntu 22.04 container — launched via the Docker moby client (`github.com/docker/docker/client`) and hardened through the sandbox isolation ladder — running Xfce4 as a lightweight window manager, backed by an Xvfb virtual framebuffer on display `:0`. A VNC server (`x11vnc`) inside the container exposes the display on port 5900; rather than shipping a separate `websockify` process, the Go server reverse-proxies the VNC/WebSocket stream to the browser noVNC client using `github.com/coder/websocket`, so the user can observe the session in any browser without extra tooling. Optionally, `--screenshot-interval` enables a frame-capture goroutine that writes JPEG images to a local output directory — the primary data feed for computer-use agent perception loops.

From the agent perspective, the desktop sandbox is just another execution target. The `tag sandbox run --gui` command starts the environment, prints the noVNC URL, and hands control back to the agent loop (or the orchestrator profile) which can then drive the GUI through a combination of screenshot observation, `xdotool` keyboard/mouse injection, and direct application invocation. The `--profile orchestrator --goal` flag wires the entire stack together in one command: start sandbox, attach an agent loop that receives screenshots and issues mouse/keyboard actions, and shut down cleanly when the goal is achieved or the timeout expires.

The feature is deliberately scoped to the Docker backend for v1 — no new cloud dependencies are added. The Docker image `ghcr.io/tag-agent/desktop-sandbox:22.04` is built from a versioned Dockerfile bundled in the repository, giving operators full auditability and the ability to extend the base image with custom applications. A follow-on integration path with E2B Desktop (which provides Firecracker-isolated GUI sandboxes) is documented in the Open Questions section and planned for v2.

The primary audience is computer-use agent authors and platform engineers who need a reproducible, auditable, isolated GUI environment that integrates seamlessly with TAG's existing profile system, tracing infrastructure, and budget controls. The secondary audience is QA automation engineers who want to run visual regression tests in a headless browser inside a sandbox and capture screenshot diffs.

---

## 2. Problem Statement

### 2.1 Computer-Use Agents Have No Integrated Execution Environment in TAG

Anthropic's computer-use capability (claude-3-5-sonnet and later models) produces `computer` tool calls that expect to drive a real display: move mouse, click, type text, take a screenshot. Without an integrated GUI sandbox, TAG users who want to build computer-use agents must manually provision a Docker container with Xfce+VNC, expose ports, run websockify, and wire the screenshot feed back into the agent loop. This is 200–400 lines of infrastructure code per project, not reusable across profiles, not recorded in TAG's audit trail, and not subject to TAG's budget controls. Teams end up with as many bespoke implementations as they have projects.

### 2.2 No Frame Capture or Screenshot-Loop Primitive

Even users who successfully provision a desktop sandbox have no standard way to consume it from an agent loop. The typical computer-use pattern requires: take screenshot → encode as base64 → pass to model → interpret tool calls → apply mouse/keyboard → repeat. TAG has no built-in primitive for the capture half of this loop. The `--screenshot-interval` flag and `--output-dir` option in this PRD provide that primitive, writing timestamped JPEG frames that the agent loop (or an offline analysis pass using `tag eval`) can consume directly.

### 2.3 VNC Sessions Are Invisible in TAG's Audit Trail

When users run GUI sessions outside TAG, those sessions produce no rows in `sandbox_runs`, no spans in the traces table, no cost attribution, and no audit log entries. A GUI session that accessed corporate systems or executed sensitive automation is completely invisible to platform engineers reviewing TAG's audit trail. PRD-092 makes every GUI sandbox session a first-class `sandbox_runs` row with VNC port, noVNC URL, container ID, screenshot count, and frame output directory recorded from creation to teardown.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Provision an Ubuntu 22.04 + Xfce4 + VNC desktop environment as a Docker container with a single `tag sandbox run --gui` command, with container ready in under 45 seconds on a machine with the image pre-pulled. |
| G2 | Expose the desktop via a browser-accessible noVNC WebSocket URL (`http://localhost:6080/vnc.html`) so the user can observe and interact with the session in any browser without installing a VNC client. |
| G3 | Support a screenshot capture loop (`--screenshot-interval`) that writes timestamped JPEG frames to a local output directory, providing the perception feed for computer-use agent loops. |
| G4 | Wire an orchestrator agent loop to the GUI sandbox via `--profile <name> --goal "<task>"` so a computer-use agent can receive screenshots, issue tool calls, and drive the GUI without additional plumbing. |
| G5 | Record every GUI sandbox session in `sandbox_runs` (extended schema) and emit OTel spans compatible with PRD-013, including session ID, VNC port, noVNC URL, frame count, and teardown reason. |
| G6 | Enforce the same resource controls (CPU, memory, timeout) and credential mount protections as the existing sandbox backends, with no privileged container capabilities beyond what X11/VNC strictly require. |
| G7 | Provide a versioned, auditable Docker image (`ghcr.io/tag-agent/desktop-sandbox:22.04`) built from a Dockerfile checked into the repository, with a `tag sandbox build-gui-image` command for local builds. |
| G8 | Integrate with TAG's budget system (PRD-012) so GUI sandbox sessions accrue wall-clock cost at a configurable per-second rate and respect `budget.max_usd` limits. |
| G9 | Support clean shutdown on `Ctrl+C`, timeout expiry, or agent goal completion — stopping the container, closing the VNC server, and writing a final summary row to `sandbox_runs`. |

---

## 4. Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | **GPU-accelerated graphics rendering.** The desktop sandbox uses software rendering (llvmpipe via Xvfb). GPU passthrough is not supported in v1. |
| NG2 | **Audio support.** PulseAudio or ALSA in the container is not configured. Applications requiring audio will run silently. |
| NG3 | **Multi-display / multi-monitor.** A single virtual display at 1280×800 is provided. Xrandr extension support is available but not exposed via CLI flags in v1. |
| NG4 | **E2B Desktop or Browserbase cloud backends.** The v1 implementation is Docker-only. Cloud GUI sandbox providers are documented as a v2 integration path. |
| NG5 | **Persistent desktop sessions.** Containers are ephemeral. When the session ends (timeout, goal completion, or manual stop) the container is removed. Persistence via Docker volumes or named containers is deferred. |
| NG6 | **Full RFB protocol implementation.** TAG does not implement the RFB (VNC) protocol natively. It relies on `x11vnc` and `websockify` inside the container, and the noVNC HTML client in the browser. |
| NG7 | **Windows host support.** The Docker backend requires a Linux kernel for X11 namespace isolation. macOS (via Docker Desktop) is supported. Windows via Docker Desktop is untested in v1. |
| NG8 | **Input device passthrough from host.** The user drives the session through the noVNC browser UI. Direct USB HID device passthrough is not supported. |

---

## 5. Success Metrics

| Metric | Baseline (v0) | Target (v1) |
|--------|---------------|-------------|
| Time to desktop ready (image pre-pulled) | N/A (no feature) | ≤ 45 seconds p95 |
| noVNC frame latency (local Docker) | N/A | ≤ 150 ms p50 |
| Screenshot capture throughput at 1 s interval | N/A | ≥ 59/60 frames captured per minute (< 2% drop) |
| Computer-use agent task success rate (fill web form goal) | N/A | ≥ 70% on standard benchmark suite |
| Sandbox teardown time after `Ctrl+C` | N/A | ≤ 5 seconds p95 |
| `sandbox_runs` row completeness for GUI sessions | 0% (no feature) | 100% of sessions have start + end row with VNC port and frame count |
| Credential mount rejection rate | 100% (existing) | 100% (unchanged) |
| Budget accrual accuracy | N/A | Within 5% of actual wall-clock duration × per-second rate |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Computer-use agent author | run `tag sandbox run --gui` and get a browser URL | I can watch my agent navigate a GUI in real time without setting up any infrastructure |
| U2 | Platform engineer | see every GUI sandbox session in `tag sandbox list` with container ID, VNC port, frame count, and status | I have a complete audit trail of all GUI automation that ran under TAG |
| U3 | QA automation engineer | run `tag sandbox run --gui --screenshot-interval 1s --output-dir ./frames` | I can collect a frame-by-frame record of a GUI test run for visual regression analysis |
| U4 | Agent developer | run `tag sandbox run --gui --profile orchestrator --goal "Fill out the contact form at http://localhost:3000"` | The agent drives the GUI end-to-end without me writing any plumbing code |
| U5 | Developer | run `tag sandbox run --gui --url http://localhost:8080` | The noVNC viewer opens automatically in my browser so I can observe the session immediately |
| U6 | Security engineer | confirm that `tag sandbox run --gui` mounts no host credential paths | GUI sessions have the same credential isolation guarantees as headless sandbox sessions |
| U7 | DevOps engineer | run `tag sandbox build-gui-image --tag my-registry/desktop:v1` | I can build and push a custom desktop image with additional applications pre-installed |
| U8 | Agent developer | set `--resolution 1920x1080` to match the target deployment environment | Screenshots taken in the sandbox reflect real-world viewport dimensions |
| U9 | Platform engineer | receive a Slack notification (via PRD-040) when a GUI sandbox session exceeds its budget cap | I know immediately when an agent session is running long and costing more than expected |
| U10 | Computer-use agent author | pass `--env DISPLAY_WIDTH=1280 --env DISPLAY_HEIGHT=800` | The sandbox display geometry matches the training distribution of my computer-use model |

---

## 7. Proposed CLI Surface

### 7.1 `tag sandbox run --gui`

Start a GUI sandbox session and print the noVNC URL.

```
tag sandbox run --gui \
  [--image ghcr.io/tag-agent/desktop-sandbox:22.04] \
  [--url http://localhost:6080]           # open noVNC URL in browser after start
  [--vnc-port 5900]                       # VNC port on host (default: 5900)
  [--novnc-port 6080]                     # noVNC WebSocket port (default: 6080)
  [--resolution 1280x800]                 # virtual display resolution (default: 1280x800)
  [--screenshot-interval 1s]             # capture interval: e.g. 500ms, 1s, 5s
  [--output-dir ./frames]                # directory for JPEG frame output
  [--profile orchestrator]               # TAG profile to attach as the driving agent
  [--goal "Fill out this web form: ..."] # goal string passed to the agent loop
  [--timeout 600]                        # session wall-clock timeout in seconds (default: 300)
  [--cpu 2]                              # container CPU limit (default: 2)
  [--memory 2g]                          # container memory limit (default: 2g)
  [--env KEY=VALUE]                      # pass additional env vars into the container
  [--mount /host/path:/container/path]   # bind mount (subject to credential rejection)
  [--no-browser]                         # do not open the noVNC URL in the system browser
  [--json]                               # output machine-readable JSON
```

**Example: basic GUI session**

```
$ tag sandbox run --gui
[sandbox] Pulling ghcr.io/tag-agent/desktop-sandbox:22.04 ... done (12.3 s)
[sandbox] Starting container tag-desktop-a3f9b2 ...
[sandbox] Xvfb :0 ready
[sandbox] Xfce4 session started
[sandbox] x11vnc listening on :5900
[sandbox] noVNC WebSocket: http://localhost:6080/vnc.html
[sandbox] Session ID: gui-a3f9b2c1
[sandbox] Timeout: 300 s | CPU: 2 | Memory: 2g
[sandbox] Press Ctrl+C to stop.
```

**Example: with screenshot capture**

```
$ tag sandbox run --gui \
    --screenshot-interval 1s \
    --output-dir ./frames \
    --timeout 60

[sandbox] Session ID: gui-8d2e1f44
[sandbox] noVNC: http://localhost:6080/vnc.html
[sandbox] Frame capture: ./frames/ @ 1s interval
[sandbox] [00:00:01] frame-000001.jpg (48 KB)
[sandbox] [00:00:02] frame-000002.jpg (51 KB)
...
[sandbox] [00:01:00] frame-000060.jpg (49 KB)
[sandbox] Timeout reached. Stopping container.
[sandbox] Captured 60 frames in ./frames/
[sandbox] Session summary written to sandbox_runs (id=gui-8d2e1f44)
```

**Example: computer-use agent loop**

```
$ tag sandbox run --gui \
    --profile orchestrator \
    --goal "Navigate to http://example.com and fill in Name='Alice', Email='alice@example.com', then submit." \
    --screenshot-interval 500ms \
    --timeout 120

[sandbox] Session ID: gui-f7a09d33
[sandbox] noVNC: http://localhost:6080/vnc.html
[sandbox] Agent: orchestrator | Goal: Navigate to http://example.com and fill in ...
[sandbox] [step 1] screenshot → model → tool: computer(action=screenshot)
[sandbox] [step 2] tool: computer(action=left_click, coordinate=[640, 400])
[sandbox] [step 3] tool: computer(action=type, text="Alice")
...
[sandbox] [step 14] Goal achieved. Exit code: 0
[sandbox] Steps: 14 | Duration: 42.1 s | Cost: $0.0234
```

**Example: open noVNC URL automatically**

```
$ tag sandbox run --gui --url http://localhost:6080
[sandbox] Session ID: gui-c1d2e3f4
[sandbox] Opening http://localhost:6080/vnc.html in browser ...
```

**Exit codes:**

| Code | Meaning |
|------|---------|
| 0 | Session completed successfully (goal achieved or timeout with `--timeout` as expected end) |
| 1 | Internal error (Docker not found, image pull failed, port conflict) |
| 2 | Session timed out before goal achieved (when `--goal` is provided) |
| 3 | Agent loop error (model API failure, profile not found) |
| 4 | Budget cap exceeded |
| 130 | Interrupted by Ctrl+C |

### 7.2 `tag sandbox build-gui-image`

Build the desktop Docker image locally.

```
tag sandbox build-gui-image \
  [--tag ghcr.io/tag-agent/desktop-sandbox:22.04] \
  [--dockerfile path/to/Dockerfile.gui] \
  [--push]                # push to registry after build
  [--no-cache]
```

### 7.3 `tag sandbox screenshot`

Take a single screenshot from a running GUI sandbox session.

```
tag sandbox screenshot \
  --session gui-a3f9b2c1 \
  [--output ./screenshot.jpg] \
  [--quality 85]           # JPEG quality 1–100, default 85
  [--json]                 # output base64-encoded JPEG in JSON
```

### 7.4 `tag sandbox inject`

Inject keyboard or mouse input into a running GUI sandbox session.

```
tag sandbox inject \
  --session gui-a3f9b2c1 \
  --action left_click \
  --coordinate 640,400

tag sandbox inject \
  --session gui-a3f9b2c1 \
  --action type \
  --text "Hello, world!"

tag sandbox inject \
  --session gui-a3f9b2c1 \
  --action key \
  --key Return
```

Actions: `left_click`, `right_click`, `double_click`, `middle_click`, `scroll`, `type`, `key`, `move_mouse`, `screenshot`.

### 7.5 `tag sandbox stop`

Stop a running GUI sandbox session.

```
tag sandbox stop <session-id> [--timeout 10]
```

### 7.6 `tag sandbox list` (extended output for GUI sessions)

```
$ tag sandbox list --type gui

ID              TYPE  STATUS   VNC    NOVNC  FRAMES  UPTIME   PROFILE
gui-a3f9b2c1    gui   running  5900   6080   142     00:02:21  orchestrator
gui-8d2e1f44    gui   stopped  —      —      60      00:01:00  —
```

---

## 8. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | `tag sandbox run --gui` must start a Docker container (via the Docker moby client, `github.com/docker/docker/client`) running Ubuntu 22.04 with Xfce4, Xvfb on display `:0`, and x11vnc on port 5900. The container must be ready (display + VNC reachable) within 45 seconds p95 when the image is pre-pulled. The Go server (`net/http` + `go-chi/chi`) reverse-proxies the VNC stream to the browser noVNC client over WebSocket. |
| FR-02 | The VNC port (default 5900) and noVNC proxy port (default 6080) must be configurable via `--vnc-port` and `--novnc-port`. If the default port is in use, the `internal/sandbox` package must detect the conflict, increment the port by 1, and retry up to 10 times before failing with exit code 1. |
| FR-03 | `--screenshot-interval` accepts durations parsed by `time.ParseDuration` (e.g., `500ms`, `1s`, `5m`). The frame capture goroutine must use `x11vnc`'s RFB client or a direct `DISPLAY=:0 scrot` call inside the container (via the Docker exec API) to produce JPEG files at the specified interval. Files are named `frame-NNNNNN.jpg` (zero-padded 6 digits) and written to `--output-dir`. |
| FR-04 | `--output-dir` must be created if it does not exist. If it does exist and already contains frames from a prior run, new frames must not overwrite old frames — either by using a timestamped subdirectory or by continuing the frame counter from the highest existing number. |
| FR-05 | When `--profile` and `--goal` are both provided, the `internal/sandbox` package must spawn a TAG agent loop using the specified profile, passing the goal as the initial prompt. The agent loop must have access to a `computer` tool (defined in the `internal/sandbox` package) that maps Anthropic computer-use tool call schemas to `xdotool` and `scrot` commands executed inside the container. |
| FR-06 | The `computer` tool must implement the following actions matching the Anthropic computer-use API schema: `screenshot`, `left_click`, `right_click`, `double_click`, `middle_click`, `scroll`, `type`, `key`, `move_mouse`. All actions except `screenshot` are implemented via a Docker exec of `xdotool ...` inside the container. `screenshot` execs `scrot -q 85 /tmp/screenshot.jpg` and streams the file out through the Docker `CopyFromContainer` API, returning it as base64. |
| FR-07 | `--url` opens `http://localhost:<novnc-port>/vnc.html` in the system browser (via `github.com/pkg/browser` / `exec.Command` on the platform opener) after the sandbox is confirmed ready. `--no-browser` suppresses this behavior. When `--url` is provided with a custom URL value, that URL is opened instead of the auto-derived localhost URL. |
| FR-08 | Every GUI sandbox session must create a row in `sandbox_runs` at session start with `type='gui'`, `status='running'`, `container_id`, `vnc_port`, `novnc_port`, `resolution`, and `session_id`. At session end, the row must be updated with `status` (completed/failed/timeout/interrupted), `completed_at`, `frame_count`, `output_dir`, `exit_code`, and `error` (if any). |
| FR-09 | Resource limits `--cpu` and `--memory` must be set on the moby `container.HostConfig.Resources` (`NanoCPUs`, `Memory`). The container must also have `ShmSize=512m` set by default (required for Chromium and other browsers that use `/dev/shm`). |
| FR-10 | The `--env KEY=VALUE` flag must be passed to the container's `Env` slice in the moby `container.Config`. Environment variable names matching patterns in `internal/security`'s `SensitiveEnvPatterns` list must be rejected with a descriptive error before the container starts. |
| FR-11 | The `--mount /host/path:/container/path` flag must validate the host path against `internal/security`'s `CredentialPathPatterns`. Paths matching `*.env`, `*.pem`, `*.key`, `*secret*`, `~/.ssh/*`, `~/.aws/*`, `~/.config/gcloud/*` must be rejected with exit code 1 and a descriptive error. Valid mounts are passed as bind mounts in the moby `container.HostConfig`. |
| FR-12 | `tag sandbox screenshot --session <id>` must exec `scrot` inside the running container and write the resulting JPEG to `--output` (or stdout if `--output` is `-`). When `--json` is set, the output must be `{"session_id": "...", "timestamp": "...", "image_base64": "..."}`. |
| FR-13 | `tag sandbox inject` must translate the `--action` and associated flags into `xdotool <...>` calls run via the Docker exec API. The `type` action must use `xdotool type --clearmodifiers --delay 50 "<text>"`. The `key` action must use `xdotool key <key>`. Coordinate-based actions must use `xdotool mousemove <x> <y>; xdotool click <button>`. |
| FR-14 | `tag sandbox stop <session-id>` must call the moby client `ContainerStop` with a `<timeout>` grace period, update `sandbox_runs.status` to `stopped` and `sandbox_runs.completed_at` to the current UTC timestamp, and exit 0. |
| FR-15 | On `Ctrl+C` (SIGINT), the process must catch the signal via `signal.NotifyContext`, call `ContainerStop` with a 5 s grace period, update `sandbox_runs` with `status='interrupted'`, and exit 130. |
| FR-16 | When `--profile` and `--goal` are provided and the agent loop exits with tool result `{"goal_achieved": true}`, the session must stop the container, update `sandbox_runs.status` to `completed`, and exit 0. |
| FR-17 | Budget integration: if `budget.max_usd` is set in the active profile and the GUI session's accrued cost (wall-clock seconds × `sandbox.gui_cost_per_second_usd`, default `0.001`) exceeds the budget cap, the session must stop gracefully and emit a budget-exceeded notification via PRD-040 hooks. |
| FR-18 | `tag sandbox build-gui-image` must invoke the moby client `ImageBuild` with the tarred build context from the repository root, streaming build output to stdout. The bundled `Dockerfile.gui` must be located at `internal/sandbox/docker/Dockerfile.gui` and embedded into the single static binary via Go `embed.FS`. |
| FR-19 | `--resolution WxH` must be passed into the container as env vars `DISPLAY_WIDTH=W` and `DISPLAY_HEIGHT=H`. The container entrypoint script must use these variables to set the Xvfb geometry: `Xvfb :0 -screen 0 ${DISPLAY_WIDTH}x${DISPLAY_HEIGHT}x24`. |
| FR-20 | When `--json` is passed to `tag sandbox run --gui`, the command must print a JSON object `{"session_id": "...", "container_id": "...", "vnc_port": N, "novnc_url": "...", "status": "running"}` (marshaled via `encoding/json`) immediately after the container is ready, then stream frame metadata to stderr (not stdout) so the JSON output stream remains machine-parseable. |

---

## 9. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **Startup latency:** With the image pre-pulled, the time from `tag sandbox run --gui` invocation to "VNC ready" (port 5900 accepting connections) must be ≤ 45 seconds p95 on a MacBook Pro M3 with Docker Desktop 4.x. |
| NFR-02 | **Frame capture accuracy:** At `--screenshot-interval 1s`, at least 58 of 60 frames per minute must be captured successfully (≤ 3% frame drop) under normal system load. |
| NFR-03 | **noVNC frame latency:** The visual latency between a container-side display change and the noVNC browser rendering the update must be ≤ 150 ms p50 on localhost. |
| NFR-04 | **Teardown time:** From `ContainerStop` invocation to container removal confirmed, teardown must complete in ≤ 10 seconds including final `sandbox_runs` row update. |
| NFR-05 | **Memory footprint:** The idle desktop container (Xvfb + Xfce4 + x11vnc + websockify, no browser) must consume ≤ 400 MB RSS. The default `--memory 2g` cap leaves 1.6 GB headroom for user applications. |
| NFR-06 | **No new mandatory runtime:** the moby client is compiled into the single static binary (`CGO_ENABLED=0`), so no separate SDK install is required. The GUI path is guarded by a runtime feature-detect that connects to the Docker daemon socket lazily (only when `--gui` is passed); if no reachable Docker daemon is found, the error message must instruct the user to start Docker / point `DOCKER_HOST` at a daemon. The existing headless sandbox backends must remain functional without a Docker daemon. |
| NFR-07 | **Idempotent cleanup:** If TAG is killed (SIGKILL) while a GUI session is running, the orphaned container must be detectable via `sandbox_runs.status = 'running'` with a `created_at` timestamp older than `--timeout`. `tag sandbox list` must surface these sessions with status `orphaned`. `tag sandbox stop <id>` must handle orphaned containers. |
| NFR-08 | **Port isolation:** Two concurrent GUI sessions must not conflict. Each session's VNC and noVNC ports must be distinct. Port allocation must scan for availability before assigning. |
| NFR-09 | **OTel tracing compatibility:** GUI sandbox sessions must emit spans compatible with PRD-013. Minimum span attributes: `sandbox.session_id`, `sandbox.type=gui`, `sandbox.container_id`, `sandbox.vnc_port`, `sandbox.novnc_port`, `sandbox.resolution`, `sandbox.frame_count`, `sandbox.duration_seconds`. |
| NFR-10 | **Image reproducibility:** The `Dockerfile.gui` must pin all package versions. The image build must be reproducible given the same base digest. A `sha256` digest pin for the Ubuntu 22.04 base image must be included in the Dockerfile. |

---

## 10. Technical Design

### 10.1 New Files

| File | Purpose |
|------|---------|
| `internal/sandbox/gui.go` | `GuiSandboxConfig`, `GuiSandboxSession`, `RunGuiSandbox()`, `guiScreenshot()`, `guiInject()`, `guiStop()` |
| `internal/sandbox/computer_tool.go` | Anthropic computer-use tool schema adapter; maps tool calls to `xdotool`/`scrot` executed via the Docker exec API |
| `internal/server/vnc_proxy.go` | `net/http` + `go-chi/chi` handler that reverse-proxies the VNC/WebSocket stream (`github.com/coder/websocket`) to the in-container RFB endpoint; huma v2 control routes for session lifecycle |
| `internal/sandbox/docker/Dockerfile.gui` | Ubuntu 22.04 base image definition for the desktop sandbox (embedded via `embed.FS`) |
| `internal/sandbox/docker/entrypoint.sh` | Container entrypoint: starts Xvfb, Xfce4, x11vnc (websockify optional) |
| `internal/sandbox/gui_test.go` | Unit and integration tests (`go test`) |

### 10.2 SQLite DDL — Extended `sandbox_runs` Table

The existing `sandbox_runs` table (from PRD-028) lives in `modernc.org/sqlite` (pure-Go, CGO-free) and is extended with GUI-specific columns via `ALTER TABLE ... ADD COLUMN` migration applied in `EnsureSchema()`:

```sql
-- Migration: PRD-092 GUI sandbox columns
-- Applied by EnsureSchema() in internal/sandbox via the shared *sql.DB (modernc.org/sqlite)

ALTER TABLE sandbox_runs ADD COLUMN type TEXT NOT NULL DEFAULT 'headless';
-- values: 'headless' (existing) | 'gui'

ALTER TABLE sandbox_runs ADD COLUMN container_id TEXT;
-- Docker container short ID (12 hex chars)

ALTER TABLE sandbox_runs ADD COLUMN vnc_port INTEGER;
-- Host VNC port (e.g. 5900)

ALTER TABLE sandbox_runs ADD COLUMN novnc_port INTEGER;
-- Host noVNC WebSocket port (e.g. 6080)

ALTER TABLE sandbox_runs ADD COLUMN resolution TEXT;
-- e.g. '1280x800'

ALTER TABLE sandbox_runs ADD COLUMN output_dir TEXT;
-- Absolute path to frame output directory (NULL if no capture)

ALTER TABLE sandbox_runs ADD COLUMN frame_count INTEGER NOT NULL DEFAULT 0;
-- Number of JPEG frames written

ALTER TABLE sandbox_runs ADD COLUMN goal TEXT;
-- User-provided goal string (NULL for non-agentic sessions)

ALTER TABLE sandbox_runs ADD COLUMN profile TEXT;
-- TAG profile name driving the session (NULL for non-agentic)

ALTER TABLE sandbox_runs ADD COLUMN cost_usd REAL;
-- Accrued wall-clock cost at shutdown

-- New index for GUI session queries
CREATE INDEX IF NOT EXISTS idx_sr_gui
  ON sandbox_runs(type, status, created_at);
```

**Full `sandbox_runs` schema post-migration:**

```sql
CREATE TABLE IF NOT EXISTS sandbox_runs (
  id            TEXT PRIMARY KEY,         -- 'gui-<12hex>' for GUI, '<12hex>' for headless
  type          TEXT NOT NULL DEFAULT 'headless',
  command       TEXT,                     -- NULL for pure GUI sessions
  backend       TEXT NOT NULL DEFAULT 'docker',
  image         TEXT,
  status        TEXT NOT NULL DEFAULT 'running',
  -- status values: running | completed | failed | timeout | interrupted | orphaned | stopped
  exit_code     INTEGER,
  output        TEXT NOT NULL DEFAULT '',
  error         TEXT,
  container_id  TEXT,
  vnc_port      INTEGER,
  novnc_port    INTEGER,
  resolution    TEXT,
  output_dir    TEXT,
  frame_count   INTEGER NOT NULL DEFAULT 0,
  goal          TEXT,
  profile       TEXT,
  cost_usd      REAL,
  created_at    TEXT NOT NULL,
  completed_at  TEXT
);
```

### 10.3 Core Structs

```go
// internal/sandbox/gui.go (additions for PRD-092)
package sandbox

import (
	"sync/atomic"
	"time"
)

// GuiSandboxConfig configures a GUI sandbox session.
// JSON schema for the huma control API is derived via invopop/jsonschema.
type GuiSandboxConfig struct {
	Image              string            `json:"image" default:"ghcr.io/tag-agent/desktop-sandbox:22.04"`
	VNCPort            int               `json:"vnc_port" default:"5900"`
	NoVNCPort          int               `json:"novnc_port" default:"6080"`
	Resolution         string            `json:"resolution" default:"1280x800"`
	ScreenshotInterval time.Duration     `json:"screenshot_interval"` // zero = no capture
	OutputDir          string            `json:"output_dir,omitempty"`
	Profile            string            `json:"profile,omitempty"`
	Goal               string            `json:"goal,omitempty"`
	Timeout            time.Duration     `json:"timeout" default:"300s"` // wall-clock
	CPU                float64           `json:"cpu" default:"2"`
	Memory             string            `json:"memory" default:"2g"`
	ShmSize            string            `json:"shm_size" default:"512m"`
	Env                map[string]string `json:"env,omitempty"`
	Mounts             []string          `json:"mounts,omitempty"` // "host:container" strings
	OpenBrowser        bool              `json:"open_browser" default:"true"`
	CostPerSecondUSD   float64           `json:"cost_per_second_usd" default:"0.001"`
}

// GuiSandboxSession holds the runtime state of a running GUI sandbox.
// status: starting | running | stopping | completed | failed | timeout | interrupted
type GuiSandboxSession struct {
	SessionID   string
	ContainerID string
	Config      GuiSandboxConfig
	Status      string
	StartTime   time.Time
	EndTime     time.Time
	FrameCount  atomic.Int64 // incremented from the capture goroutine
	NoVNCURL    string
	Err         error
}

// DurationSeconds returns the elapsed wall-clock time in seconds.
func (s *GuiSandboxSession) DurationSeconds() float64 {
	if s.StartTime.IsZero() {
		return 0
	}
	end := s.EndTime
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(s.StartTime).Seconds()
}

// CostUSD returns the accrued wall-clock cost.
func (s *GuiSandboxSession) CostUSD() float64 {
	return s.DurationSeconds() * s.Config.CostPerSecondUSD
}

// ComputerToolAction is a single computer-use tool action dispatched to xdotool/scrot.
// Action: screenshot | left_click | right_click | double_click | middle_click | scroll | type | key | move_mouse
type ComputerToolAction struct {
	Action          string `json:"action"`
	Coordinate      *[2]int `json:"coordinate,omitempty"`
	Text            string `json:"text,omitempty"`
	Key             string `json:"key,omitempty"`
	ScrollDirection string `json:"scroll_direction,omitempty"` // up | down | left | right
	ScrollClicks    int    `json:"scroll_clicks,omitempty"`
}
```

### 10.4 Core Algorithms

#### 10.4.1 Port Availability Scan

```go
// findFreePort returns the first available TCP port >= start.
func findFreePort(start, maxAttempts int) (int, error) {
	for port := start; port < start+maxAttempts; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return port, nil
	}
	return 0, fmt.Errorf("no free port found in range %d-%d", start, start+maxAttempts-1)
}
```

#### 10.4.2 VNC Readiness Poll

```go
// waitForVNC polls until the VNC port accepts a TCP connection.
// Returns nil when ready, or ctx.Err()/deadline error on timeout.
func waitForVNC(ctx context.Context, host string, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("%s:%d", host, port)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("vnc %s not ready within %s", addr, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
```

#### 10.4.3 Frame Capture Loop

```go
// frameCaptureLoop runs as a goroutine (started via errgroup): it captures a
// JPEG screenshot from the container every cfg.ScreenshotInterval and writes it
// to outputDir. It exits cleanly when ctx is cancelled. session.FrameCount is an
// atomic counter, so the increment is data-race free.
func (m *Manager) frameCaptureLoop(
	ctx context.Context,
	containerID, outputDir string,
	interval time.Duration,
	session *GuiSandboxSession,
) error {
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return err
	}

	// Detect existing frames to avoid overwriting.
	frameNum := 1
	if existing, _ := filepath.Glob(filepath.Join(outputDir, "frame-*.jpg")); len(existing) > 0 {
		sort.Strings(existing)
		last := strings.TrimSuffix(filepath.Base(existing[len(existing)-1]), ".jpg") // frame-000042
		if n, err := strconv.Atoi(strings.TrimPrefix(last, "frame-")); err == nil {
			frameNum = n + 1
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		// Take screenshot inside the container and stream it out via the exec API.
		jpeg, err := m.execScreenshot(ctx, containerID)
		if err == nil && len(jpeg) > 0 {
			framePath := filepath.Join(outputDir, fmt.Sprintf("frame-%06d.jpg", frameNum))
			if werr := os.WriteFile(framePath, jpeg, 0o600); werr == nil {
				session.FrameCount.Add(1)
				frameNum++
			}
		}
		// err is logged, never fatal: the loop must survive transient exec failures.

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
```

#### 10.4.4 Computer Tool Dispatch

```go
// ActionResult is returned by the computer-use dispatcher.
type ActionResult struct {
	Output      string `json:"output"`
	Err         string `json:"error,omitempty"`
	ImageBase64 string `json:"image_base64,omitempty"`
}

// dispatchComputerAction translates a ComputerToolAction into Docker exec calls.
// All argv is passed as a []string to the exec API (never a shell string), so
// user-controlled text cannot inject shell metacharacters (see Security §11.9).
func (m *Manager) dispatchComputerAction(ctx context.Context, containerID string, a ComputerToolAction) ActionResult {
	coord := func(dx, dy int) (int, int) {
		if a.Coordinate != nil {
			return a.Coordinate[0], a.Coordinate[1]
		}
		return dx, dy
	}

	switch a.Action {
	case "screenshot":
		jpeg, err := m.execScreenshot(ctx, containerID)
		if err != nil {
			return ActionResult{Err: err.Error()}
		}
		return ActionResult{ImageBase64: base64.StdEncoding.EncodeToString(jpeg)}

	case "left_click", "right_click", "double_click", "middle_click":
		button := map[string]string{"left_click": "1", "right_click": "3", "double_click": "1", "middle_click": "2"}[a.Action]
		x, y := coord(0, 0)
		argv := []string{"xdotool", "mousemove", strconv.Itoa(x), strconv.Itoa(y), "click"}
		if a.Action == "double_click" {
			argv = append(argv, "--repeat", "2", "--delay", "100")
		}
		argv = append(argv, button)
		return m.exec(ctx, containerID, argv)

	case "type":
		// Text is a discrete argv element -> no shell interpolation.
		argv := []string{"xdotool", "type", "--clearmodifiers", "--delay", "50", "--", a.Text}
		return m.exec(ctx, containerID, argv)

	case "key":
		if !isAllowedKeyName(a.Key) { // whitelist, see Security §11.9
			return ActionResult{Err: fmt.Sprintf("rejected key name: %q", a.Key)}
		}
		return m.exec(ctx, containerID, []string{"xdotool", "key", a.Key})

	case "move_mouse":
		x, y := coord(0, 0)
		return m.exec(ctx, containerID, []string{"xdotool", "mousemove", strconv.Itoa(x), strconv.Itoa(y)})

	case "scroll":
		x, y := coord(640, 400)
		button := "4" // up
		if a.ScrollDirection != "" && a.ScrollDirection != "up" {
			button = "5"
		}
		clicks := a.ScrollClicks
		if clicks == 0 {
			clicks = 3
		}
		res := m.exec(ctx, containerID, []string{"xdotool", "mousemove", strconv.Itoa(x), strconv.Itoa(y)})
		if res.Err != "" {
			return res
		}
		for i := 0; i < clicks; i++ {
			if r := m.exec(ctx, containerID, []string{"xdotool", "click", button}); r.Err != "" {
				return r
			}
		}
		return ActionResult{}

	default:
		return ActionResult{Err: fmt.Sprintf("unknown action: %s", a.Action)}
	}
}
```

### 10.5 Docker Image Architecture (`Dockerfile.gui`)

**Isolation ladder.** The desktop stack runs inside a sandbox selected by feature-detection at runtime. The GUI/VNC-in-sandbox path is **Linux-only** (it requires a Linux kernel for X11 namespace isolation, cf. NG7); the harness feature-detects the kernel and available runtimes and degrades to the strongest tier present, disabling the `--gui` path where no supported container/VM runtime exists. From least to most isolated: a *restricted tier* using `landlock-lsm/go-landlock` (filesystem), `elastic/go-seccomp-bpf` (syscall filtering) and `google/nftables` (egress control) applied to the container process; the default **Docker/moby** container (`github.com/docker/docker/client`) launching the GUI/VNC server; **gVisor** (`runsc` runtime) for stronger syscall interposition; and **Firecracker** (`firecracker-microvm/firecracker-go-sdk`) microVMs for hardware-level isolation. TAG orchestrates the chosen tier and launches the desktop image into it; it does not embed any GUI toolkit in-process.

```dockerfile
# internal/sandbox/docker/Dockerfile.gui
# Embedded into the TAG static binary via Go embed.FS; built through the moby
# client ImageBuild API. Ubuntu 22.04 LTS desktop sandbox for TAG computer-use
# agents. websockify is optional: the Go server proxies the RFB stream directly.
# Pinned base digest for reproducibility.
FROM ubuntu:22.04@sha256:77906da86b60585ce12215807090eb327e7386c8fafb5402369e421f44eff17e

ARG DISPLAY_WIDTH=1280
ARG DISPLAY_HEIGHT=800
ENV DEBIAN_FRONTEND=noninteractive \
    DISPLAY=:0 \
    DISPLAY_WIDTH=${DISPLAY_WIDTH} \
    DISPLAY_HEIGHT=${DISPLAY_HEIGHT} \
    VNC_PORT=5900 \
    NOVNC_PORT=6080

# Install desktop stack (pinned minor versions for reproducibility)
RUN apt-get update && apt-get install -y --no-install-recommends \
    xfce4=4.16.0-1 \
    xfce4-terminal=1.0.0-1 \
    xvfb=2:21.1.3-2ubuntu2 \
    x11vnc=0.9.16-8 \
    websockify=0.10.0+repack-3 \
    scrot=1.8.1-1 \
    xdotool=3.20160805.1-4 \
    novnc=1.3.0-2 \
    curl \
    wget \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 5900 6080

ENTRYPOINT ["/entrypoint.sh"]
```

### 10.6 Container Entrypoint (`entrypoint.sh`)

```bash
#!/usr/bin/env bash
# internal/sandbox/docker/entrypoint.sh
# Starts Xvfb -> Xfce4 -> x11vnc (-> optional websockify).
# The Go server (internal/server/vnc_proxy.go) normally reverse-proxies the RFB
# port over WebSocket, so websockify is only started when NOVNC_INPROC=1.

set -e

W=${DISPLAY_WIDTH:-1280}
H=${DISPLAY_HEIGHT:-800}
VNC=${VNC_PORT:-5900}
NOVNC=${NOVNC_PORT:-6080}

# 1. Start virtual framebuffer
Xvfb :0 -screen 0 "${W}x${H}x24" -ac &
XVFB_PID=$!

# Wait for display
sleep 1

# 2. Start Xfce4 (background, no wait)
DISPLAY=:0 startxfce4 &
XFCE_PID=$!

# Wait for WM to settle
sleep 3

# 3. Start VNC server (no password, localhost only)
x11vnc -display :0 -forever -nopw -listen localhost -rfbport "${VNC}" -bg -quiet

# 4. Optional in-container noVNC bridge (default: Go server proxies instead)
WS_PID=""
if [ "${NOVNC_INPROC:-0}" = "1" ]; then
  websockify --web /usr/share/novnc/ "${NOVNC}" "localhost:${VNC}" &
  WS_PID=$!
fi

echo "TAG-SANDBOX-READY vnc=localhost:${VNC} rfbport=${VNC}"

# 5. Keep container alive; forward signals for clean shutdown
trap 'kill $XVFB_PID $XFCE_PID $WS_PID 2>/dev/null; exit 0' SIGTERM SIGINT
wait $XVFB_PID
```

### 10.7 Integration Points

| System | Integration |
|--------|-------------|
| `internal/cli` (chi/huma command layer) | A `cmdSandboxRunGui()` handler is added alongside the existing `cmdSandboxRun()`. It binds `GuiSandboxConfig` from CLI flags (koanf/v2) and calls `sandbox.RunGuiSandbox(ctx, db, config)`. |
| `OpenDB()` | All SQLite writes use the shared `*sql.DB` (`modernc.org/sqlite`, WAL journal mode) from `internal/store.OpenDB()`. `EnsureSchema()` is extended to run the PRD-092 `ALTER TABLE` migrations idempotently. |
| `internal/security` | `ValidateMount()` is called for each `--mount` argument. `CredentialPathPatterns` and `SensitiveEnvPatterns` are reused without modification. |
| `internal/budget` | A `BudgetTracker` is instantiated in `RunGuiSandbox()`. The tracking loop runs as a goroutine (managed by `errgroup`) checking `session.CostUSD()` against `budget.max_usd` every 10 seconds via a `time.Ticker`. |
| `internal/tracing` (OTel semconv) | A root span `sandbox.gui.session` is opened at session start and closed at teardown. Child spans are emitted for each computer-use tool action: `sandbox.gui.action`. |
| `internal/notifications` | Budget-exceeded and session-completed events are dispatched via `notifications.Dispatch()` if hooks are configured (PRD-040), optionally streamed with `tmaxmax/go-sse`. |
| `internal/agent` (Hermes bridge) | When `--profile` is provided, `RunGuiSandbox()` starts the agent loop via `agent.RunWithTools()`, injecting `computer_tool.go`'s tool definition (schema generated via invopop/jsonschema) into the tool set. |

---

## 11. Security Considerations

1. **No privileged containers.** The desktop sandbox container runs without `--privileged`. The only elevated capabilities needed are `SYS_PTRACE` (for `xdotool` on some kernels). All other capabilities are dropped via `--cap-drop=ALL --cap-add=SYS_PTRACE`.

2. **VNC bound to localhost only.** `x11vnc` is started with `-listen localhost`, binding to `127.0.0.1:5900` inside the container. The Docker port mapping is `127.0.0.1:<host_port>:5900` (not `0.0.0.0`). External network access to the VNC port is blocked by default.

3. **noVNC on localhost only.** websockify binds to `127.0.0.1:<novnc_port>`. The Docker port mapping uses `127.0.0.1` as the host bind address. This prevents exposure on external interfaces.

4. **No VNC password by default (localhost-only mitigation).** Since both VNC and noVNC bind to localhost only, the risk of unauthenticated access is limited to the local machine. A `--vnc-password` flag is documented as a future enhancement for multi-user environments.

5. **Credential mount rejection.** `internal/security.ValidateMount()` rejects host paths matching credential patterns before the container is started. This is enforced in the Go control plane, not the Docker layer, so it cannot be bypassed by the container.

6. **Environment variable filtering.** Environment variables with sensitive names (e.g., `AWS_SECRET_ACCESS_KEY`, `ANTHROPIC_API_KEY`, `DATABASE_URL`) matching `SENSITIVE_ENV_PATTERNS` are rejected before being passed via `--env`.

7. **Docker socket not mounted.** The GUI sandbox container does not receive the Docker socket (`/var/run/docker.sock`). Container-in-container execution is not supported and not exposed.

8. **Network isolation.** By default, the container is attached to a custom Docker bridge network with outbound internet access disabled (`--network tag-sandbox-net` where the network is created with `--internal`). Users who need internet access (e.g., browser automation against live sites) must explicitly pass `--allow-network`.

9. **xdotool injection sanitization.** The `computer_tool.go` dispatcher must not pass user-controlled text through a shell. All commands are sent to the Docker exec API as an explicit argv `[]string` (no `sh -c`), so text arguments cannot inject shell metacharacters. The `key` action validates the key name against a whitelist of xdotool key names (`isAllowedKeyName`) before execution.

10. **Audit log completeness.** Even if TAG crashes (SIGKILL), the `sandbox_runs` row written at session start persists in WAL-mode SQLite. The `status='running'` row with an old `created_at` enables post-hoc orphan detection. Platform engineers can query `SELECT * FROM sandbox_runs WHERE type='gui' AND status='running' AND created_at < datetime('now', '-1 hour')` to find crashed sessions.

11. **Frame output directory permissions.** `--output-dir` is created with mode `0o700` (owner-only read/write/execute). Frame files are written with mode `0o600`. This prevents other users on a shared machine from reading captured screenshots.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`internal/sandbox/gui_test.go`, run via `go test ./...`)

Docker interactions are abstracted behind a `dockerClient` interface and faked with a test double; table-driven tests (`t.Run`) cover the action matrix.

| Test | Description |
|------|-------------|
| `TestFindFreePort_Basic` | `findFreePort(5900, 10)` returns a port that is subsequently bindable. |
| `TestFindFreePort_Occupied` | When port 5900 is pre-bound, `findFreePort(5900, 10)` returns 5901. |
| `TestFindFreePort_Exhausted` | When 10 consecutive ports are all pre-bound, `findFreePort` returns a non-nil error. |
| `TestGuiConfig_Defaults` | A zero-value `GuiSandboxConfig` decoded through huma/koanf has correct default values for all fields. |
| `TestSession_CostAccrual` | `GuiSandboxSession.CostUSD()` returns `duration * costPerSecond` correctly. |
| `TestCredentialMount_Rejected` | `ValidateMount("~/.aws/credentials:/creds")` returns an error with a descriptive message. |
| `TestEnvSensitive_Rejected` | `ValidateEnvVars(map[string]string{"AWS_SECRET_ACCESS_KEY": "x"})` returns an error. |
| `TestFrameFilename_Sequence` | Frame capture goroutine produces `frame-000001.jpg`, `frame-000002.jpg`, etc. in sequence. |
| `TestFrame_NoOverwrite` | When `outputDir` already contains `frame-000042.jpg`, next frame is `frame-000043.jpg`. |
| `TestComputerAction_Screenshot` | `dispatchComputerAction` with `Action:"screenshot"` invokes the exec+CopyFromContainer path (faked client). |
| `TestComputerAction_LeftClick` | `dispatchComputerAction` with `Action:"left_click", Coordinate:{100,200}` builds argv `xdotool mousemove 100 200 click 1` (faked client). |
| `TestComputerAction_Type` | `type` action passes text as a discrete `[]string` argv element, never a shell string. |
| `TestComputerAction_KeyWhitelist` | `Key:"Return"` passes; `Key:"$(rm -rf /)"` is rejected by `isAllowedKeyName`. |
| `TestSandboxRun_DBRowCreated` | `RunGuiSandbox()` (faked Docker) creates a `sandbox_runs` row with `type='gui'` and `status='running'`. |
| `TestSandboxRun_DBRowCompleted` | After the faked session completes, the row has `status='completed'` and non-null `completed_at`. |
| `TestResolution_Parsing` | `--resolution 1920x1080` sets `DISPLAY_WIDTH=1920, DISPLAY_HEIGHT=1080` in the container env. |
| `TestDurationInterval_Parsing` | `time.ParseDuration` maps `500ms`→500ms, `2s`→2s, `1m`→60s. |

### 12.2 Integration Tests (Docker required, CI-gated)

These tests require a reachable Docker daemon and are guarded with a build tag (`//go:build docker`) plus a runtime skip (`if _, err := client.Ping(ctx); err != nil { t.Skip(...) }`):

| Test | Description |
|------|-------------|
| `TestContainerStartsAndVNCReady` | Pull a minimal test image (Alpine + socat emulating VNC), verify `waitForVNC` returns nil within 30 seconds. |
| `TestContainerTeardownOnCtrlC` | Start a real desktop container, cancel the signal context (SIGINT), verify container is removed within 10 seconds. |
| `TestNoVNCURLAccessible` | After startup, the Go proxy endpoint `http://localhost:<novnc_port>/vnc.html` returns HTTP 200. |
| `TestScreenshotReturnsJPEG` | `dispatchComputerAction(ctx, id, ComputerToolAction{Action:"screenshot"})` returns a non-empty base64 JPEG decodable as a valid JPEG (header `FFD8FF`). |
| `TestTypeActionInsertsText` | Type "hello" into a running xterm window, take screenshot, verify the text appears via basic pixel-change detection. |
| `TestFrameCapture60Frames` | Run capture goroutine at 1 s interval for 62 s, verify ≥ 59 frames written to output dir. |
| `TestBudgetCapStopsSession` | Set `CostPerSecondUSD=1.0, max_usd=2.0`. Verify session stops within 5 seconds of 2-second budget exhaustion. |
| `TestSandboxListShowsGuiType` | After starting a GUI session, `SandboxList()` output includes the session with `type=gui`. |

### 12.3 Performance Tests / Benchmarks (`go test -bench`)

| Test | Target |
|------|--------|
| `BenchmarkStartupLatency` | Measure time from `ContainerStart` to VNC port accepting connections across 10 runs; assert p95 ≤ 45 s. |
| `BenchmarkFrameDropRate` | Capture at 1 s interval for 5 minutes; assert ≤ 3% frames dropped. |
| `BenchmarkTeardownTime` | From `ContainerStop` to confirmed container removal; assert p95 ≤ 10 s. |
| `BenchmarkScreenshotRoundtrip` | Time from `dispatchComputerAction(screenshot)` call to base64 JPEG returned; assert p50 ≤ 500 ms. |

---

## 13. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-01 | `tag sandbox run --gui` starts a container with VNC on port 5900 and noVNC on port 6080 and exits only when the session ends. | Manual test + `sandbox_runs` row query |
| AC-02 | The noVNC URL (`http://localhost:6080/vnc.html`) returns HTTP 200 and renders the Xfce4 desktop in a browser within 45 seconds of command invocation. | Integration test `test_novnc_url_accessible` |
| AC-03 | `--screenshot-interval 1s --output-dir ./frames` produces at least 59 JPEG frames per 60-second window. | Integration test `test_frame_capture_60_frames` |
| AC-04 | `--profile orchestrator --goal "..."` drives a computer-use agent loop that completes the form-filling task in the standard benchmark with ≥ 70% success rate. | Computer-use eval suite |
| AC-05 | `tag sandbox screenshot --session <id>` returns a valid JPEG (FFD8FF header). | Integration test `test_screenshot_returns_jpeg` |
| AC-06 | `tag sandbox inject --session <id> --action type --text "hello"` causes "hello" to appear in the active window. | Integration test `test_type_action_inserts_text` |
| AC-07 | `Ctrl+C` stops the container within 10 seconds and sets `sandbox_runs.status = 'interrupted'`. | Integration test `test_container_teardown_on_ctrl_c` |
| AC-08 | `--mount ~/.aws/credentials:/creds` is rejected with exit code 1 and a message containing `credential pattern`. | Unit test `test_credential_mount_rejected` |
| AC-09 | `--env AWS_SECRET_ACCESS_KEY=xxx` is rejected with exit code 1 and a message containing `sensitive environment variable`. | Unit test `test_env_sensitive_rejected` |
| AC-10 | Every GUI session appears in `tag sandbox list` with `type=gui`, `session_id`, `container_id`, `vnc_port`, `novnc_port`, `frame_count`, and `status`. | Integration test `test_sandbox_list_shows_gui_type` |
| AC-11 | Two concurrent `tag sandbox run --gui` invocations use distinct VNC and noVNC ports without conflicts. | Integration test: spawn two sessions, check distinct ports |
| AC-12 | VNC port is bound to `127.0.0.1` only (not `0.0.0.0`); `ss -tlnp | grep 5900` must not show `0.0.0.0:5900`. | Security test: parse `docker inspect` network bindings |
| AC-13 | `tag sandbox build-gui-image` completes without error and the resulting image is locally available as `ghcr.io/tag-agent/desktop-sandbox:22.04`. | Build smoke test |
| AC-14 | When `budget.max_usd` is set and the session cost exceeds it, the session stops within 15 seconds of budget exhaustion. | Integration test `test_budget_cap_stops_session` |
| AC-15 | OTel spans for the session appear in the local OTLP trace store with `sandbox.type=gui` attribute. | Unit test: mock tracer, verify span attributes |

---

## 14. Dependencies

| Dependency | Type | Version | Justification |
|------------|------|---------|---------------|
| `github.com/docker/docker/client` (moby) | Go module | `>=v27` | Docker container lifecycle: `client.NewClientWithOpts`, `ContainerCreate/Start/Stop`, `ContainerExecCreate`, `CopyFromContainer`, `ImageBuild` |
| `github.com/coder/websocket` | Go module | `>=v1.8` | WebSocket reverse-proxy of the RFB/VNC stream to the browser noVNC client |
| `github.com/go-chi/chi/v5` | Go module | `>=v5.1` | HTTP router for the VNC proxy + control endpoints (`net/http`) |
| `github.com/danielgtaylor/huma/v2` | Go module | `>=v2.20` | Typed control API for GUI session lifecycle (OpenAPI generated) |
| `github.com/invopop/jsonschema` | Go module | `>=v0.12` | JSON Schema generation for the computer-use tool definition and config structs |
| `modernc.org/sqlite` | Go module | `>=v1.29` | Pure-Go (CGO-free) SQLite driver for `sandbox_runs` session state |
| `golang.org/x/sync/errgroup` | Go module | latest | Goroutine lifecycle for capture / budget / proxy loops |
| `github.com/knadh/koanf/v2` + `gopkg.in/yaml.v3` | Go module | `>=v2` / `>=v3` | Profile/config loading (`--profile`, budget, resolution defaults) |
| `landlock-lsm/go-landlock`, `elastic/go-seccomp-bpf`, `google/nftables` | Go module | latest | Restricted-tier isolation ladder (Linux-only; feature-detected) |
| `firecracker-microvm/firecracker-go-sdk` | Go module | latest | Optional Firecracker microVM isolation tier |
| `xdotool` | Container package | `3.20160805.1-4` | X11 keyboard/mouse injection inside the container |
| `scrot` | Container package | `1.8.1-1` | JPEG screenshot capture from Xvfb display inside the container |
| `x11vnc` | Container package | `0.9.16-8` | VNC server exposing Xvfb display over RFB protocol |
| `websockify` | Container package | `0.10.0+repack-3` | WebSocket-to-TCP bridge enabling noVNC browser client |
| `novnc` | Container package | `1.3.0-2` | Static HTML/JS noVNC client served by websockify |
| `xfce4` | Container package | `4.16.0-1` | Lightweight desktop environment on Xvfb |
| `Xvfb` | Container package | `2:21.1.3-2ubuntu2` | Virtual framebuffer X server |
| PRD-028 | Internal PRD | ≥ v1 | Base `sandbox_runs` schema, `EnsureSchema()`, backend registry, and moby-client `runDocker()` foundation |
| PRD-013 | Internal PRD | ≥ v1 | `internal/tracing` OTel span API |
| PRD-034 | Internal PRD | ≥ v1 | `internal/security` `CredentialPathPatterns`, `SensitiveEnvPatterns` |
| PRD-012 | Internal PRD | ≥ v1 | `internal/budget` `BudgetTracker` |
| PRD-040 | Internal PRD | ≥ v1 | `internal/notifications` `Dispatch()` for budget-exceeded hooks |

---

## 15. Open Questions

| ID | Question | Options | Owner | Target |
|----|----------|---------|-------|--------|
| OQ-01 | Should VNC authentication be added (password or token-based)? Current design relies on localhost-only binding. Multi-user servers need additional auth. | (a) Add optional `--vnc-password` flag; (b) Add VNC TLS tunnel via `stunnel`; (c) Defer to v2 | Security team | v1.5 |
| OQ-02 | E2B Desktop integration for v2: E2B provides Firecracker-isolated GUI sandboxes with `e2b.Desktop.create()`. Should the `GuiSandboxConfig` backend field be extended to `'docker' | 'e2b'` in v2? | (a) Yes, follow provider abstraction from cluster research context; (b) Separate PRD | Platform team | v2.0 |
| OQ-03 | How should multi-turn computer-use conversations handle screenshot history? Anthropic recommends sending only the last N screenshots to avoid token overflow. What is the default N, and should it be configurable? | (a) Default N=3, configurable via `--history-screenshots N`; (b) Let the profile YAML set `computer_use.screenshot_history_size` | Agent team | v1 |
| OQ-04 | Should `tag sandbox run --gui` support attaching to an already-running container (re-attach semantics)? Useful for long-running sessions that outlive the CLI process. | (a) `tag sandbox attach <session-id>` subcommand; (b) `--attach` flag to `run --gui` | Platform team | v1.5 |
| OQ-05 | Frame storage: should frames be stored in SQLite as BLOBs (for portability) or always as files (for performance)? At 1 s interval and ~50 KB/frame, 1 hour = ~180 MB — too large for SQLite BLOBs. | Files only. SQLite stores path + metadata only. | Arch team | v1 |
| OQ-06 | Browserbase integration: should a `--browserbase` flag launch a Browserbase session instead of a full desktop, for browser-only computer-use tasks? | (a) New `tag sandbox run --browser` subcommand powered by Browserbase SDK; (b) Out of scope — defer to a Browserbase MCP server | Agent team | v2.0 |
| OQ-07 | What is the right default `--screenshot-interval` when `--profile` is set (computer-use mode)? Too fast burns API tokens; too slow misses UI transitions. | (a) 500 ms default in computer-use mode; (b) Agent-driven (model requests screenshot when needed); (c) Configurable per profile YAML | Agent team | v1 |
| OQ-08 | Should the frame capture loop use `scrot` (one Docker exec per frame) or maintain a persistent RFB client connection (more efficient but complex)? At 1 fps, per-frame exec overhead is acceptable; at > 4 fps, a persistent client is recommended. | scrot for ≤ 4 fps; persistent RFB client for > 4 fps | Platform team | v1 |

---

## 16. Complexity and Timeline

### Phase 1 — Core Infrastructure (Days 1–10)

**Goal:** Docker image builds, container starts, VNC is reachable, `sandbox_runs` records the session.

- Day 1–2: Write `Dockerfile.gui` and `entrypoint.sh` (embedded via `embed.FS`). Build via the moby `ImageBuild` API and smoke-test locally. Verify `Xvfb + Xfce4 + x11vnc` stack starts within 45 seconds.
- Day 3–4: Add `internal/sandbox/gui.go` with `GuiSandboxConfig`, `GuiSandboxSession`, the `EnsureSchema()` migration (modernc.org/sqlite), and `waitForVNC()`.
- Day 5–6: Implement `RunGuiSandbox()`: port allocation, `ContainerCreate/Start` via moby client, VNC readiness poll, `sandbox_runs` row creation, `signal.NotifyContext` SIGINT handler, teardown.
- Day 7–8: Implement the `tag sandbox run --gui` CLI surface (chi/huma command layer, koanf flag binding). Wire `--vnc-port`, `--novnc-port`, `--resolution`, `--timeout`, `--cpu`, `--memory`, `--env`, `--mount`, `--no-browser` flags.
- Day 9–10: Unit tests (`go test`) for `findFreePort`, `GuiSandboxConfig`, `GuiSandboxSession`, `ValidateMount`, `ValidateEnvVars`. Integration test: VNC ready within 45 s.

**Milestone:** `tag sandbox run --gui` starts a desktop, prints the noVNC URL, and records the session in SQLite. `Ctrl+C` stops cleanly.

---

### Phase 2 — Screenshot Capture and Injection (Days 11–18)

**Goal:** Frame capture loop works. `tag sandbox screenshot` and `tag sandbox inject` work against a running session.

- Day 11–13: Implement the `frameCaptureLoop()` goroutine (errgroup-managed). Wire `--screenshot-interval` and `--output-dir`. Test frame sequence numbering and no-overwrite behavior.
- Day 14–15: Implement `computer_tool.go` with `ComputerToolAction` and `dispatchComputerAction()`. Cover all 9 action types.
- Day 16: Implement `tag sandbox screenshot` and `tag sandbox inject` subcommands in the CLI layer.
- Day 17–18: Integration tests for screenshot JPEG validity, type action text insertion, frame drop rate benchmark.

**Milestone:** Screenshot capture loop runs at configured interval. Injection commands move the mouse and type text verifiably.

---

### Phase 3 — Computer-Use Agent Loop (Days 19–28)

**Goal:** `--profile --goal` wires an orchestrator agent to the GUI sandbox via the computer tool.

- Day 19–21: Integrate `computer_tool.go` with the `internal/agent` Hermes bridge. Define the Anthropic computer-use tool schema (invopop/jsonschema). Map tool call results back to the agent loop. Handle multi-turn screenshot history (OQ-03 default: N=3).
- Day 22–24: Implement the goal-completion detection logic. When the model returns `{"goal_achieved": true}` or a configured stop signal, stop the container and exit 0.
- Day 25–26: Budget integration via `internal/budget.BudgetTracker`. OTel span emission via `internal/tracing`. Notification dispatch via `internal/notifications`.
- Day 27–28: End-to-end test: `--profile orchestrator --goal "Fill out contact form"` against a local test web server. Verify ≥ 1 successful completion. Fix any agent loop issues.

**Milestone:** `tag sandbox run --gui --profile orchestrator --goal "..."` runs an end-to-end computer-use task successfully.

---

### Phase 4 — Polish, Security, and Release (Days 29–40)

**Goal:** All ACs pass, security review complete, documentation written, image published.

- Day 29–30: `tag sandbox build-gui-image` command. `tag sandbox list` extended output for GUI type. `tag sandbox stop` command.
- Day 31–32: Security hardening: localhost-only VNC binding, `--cap-drop=ALL --cap-add=SYS_PTRACE`, `--network tag-sandbox-net` internal network, xdotool key name whitelist.
- Day 33–34: Full unit test suite to 90% coverage on the `internal/sandbox` GUI path (`go test -cover`). Benchmarks: startup latency p95, frame drop rate, teardown time.
- Day 35–36: `--json` output mode. `--url` browser auto-open. Orphaned session detection and `tag sandbox list` `orphaned` status.
- Day 37–38: Security review of Dockerfile (no secrets, pinned digests, no `--privileged`). Pen-test VNC localhost binding.
- Day 39: Publish `ghcr.io/tag-agent/desktop-sandbox:22.04` via GitHub Actions; release the TAG static binary via GoReleaser (cosign-signed, SLSA provenance). Pin moby/websockify module versions in `go.mod`.
- Day 40: Final AC verification run. Update `docs/prd/INDEX.md`. Cut release notes.

**Milestone:** All 15 ACs pass. Image published. Feature shipped in TAG v0.4.0.

---

**Total estimated duration:** 40 working days (8 weeks) for a single engineer. Can be compressed to 4–5 weeks with a second engineer parallelizing Phase 3 (agent loop) with Phase 2 (capture/injection).

