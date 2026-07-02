# PRD-119: Claude Computer-Use Screenshot Loop (`tag cu-loop`)
> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (5-8 days)
**Category:** Computer Use
**Affects:** `internal/agent (cu-loop) + internal/tool + internal/llm (vision)`
**Depends on:** PRD-118 (computer use CLI), PRD-028 (sandbox execution), PRD-041 (OTel span cost attribution)
**Inspired by:** Anthropic computer use demo, Claude 3.5 Sonnet computer_use_20241022 tool, CUA (Computer Use Agent), Screenshot-to-action loop

---

## 1. Overview

The Claude computer-use API (available in Claude 3.5 Sonnet and Claude 4 models) exposes a `computer_20241022` beta tool that enables screenshot capture, keyboard input, mouse control, and cursor position queries. The model requests these actions via structured tool calls; the client must implement the execution loop: take screenshot → send to model as a base64 image content block → model requests an action via a `tool_use` block → execute action → return a `tool_result` (with the next screenshot as an image block) → repeat.

This PRD specifies the Claude Computer-Use Screenshot Loop (`tag cu-loop`) — the core execution engine for all computer use sessions in TAG. It lives in `internal/agent` and drives Claude through TAG's `internal/llm` provider vision interface (backed by `github.com/anthropics/anthropic-sdk-go`). It manages the full agentic loop: screenshot capture (via X11/Xvfb host tools, the `go-vgo/robotgo` + `kbinani/screenshot` OS backend, or a VNC RFB client), encoding (PNG → base64 via `encoding/base64`), Claude beta API calls with the `computer_20241022` tool, action dispatch through `internal/tool` (`xdotool` host binary, `robotgo`, or VNC RFB), loop termination detection, and safety constraints via the `internal/sandbox` permission gate.

The implementation is modeled after Anthropic's open-source computer use demo (anthropic-quickstarts/computer-use-demo) and extended with TAG-specific features: OTel span attachment (`go.opentelemetry.io/otel`), cost tracking, action recording to SQLite (`modernc.org/sqlite`), and sandboxed execution.

---

## 2. Problem Statement

### 2.1 Screenshot-action loop requires custom implementation

Each consumer of the Claude computer-use API must implement the same boilerplate: capture screenshot as PNG, encode base64, construct messages with image content blocks, parse `tool_use` responses, dispatch actions, loop. TAG should provide this as a reusable, tested package.

### 2.2 No error recovery in the loop

If a screenshot fails or an action returns an error, the entire session crashes. Production loops need error recovery: retry failed screenshots, return an error `tool_result` to the model on action failure, and enforce max-retry limits.

### 2.3 Loop termination is ambiguous

The model may declare "I'm done" in text, or it may run forever. A well-implemented loop needs: text-only response detection (model finished — `stop_reason: "end_turn"` with no `tool_use`), max action limit, idle detection (model takes no action), and a context deadline.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | `ComputerUseLoop.Run(ctx, goal, opts)` manages the full screenshot-action loop lifecycle. |
| G2 | Screenshot capture: support X11 (`scrot`/`xwd` host tools), the `robotgo`/`kbinani/screenshot` OS backend, and a VNC RFB client backend. |
| G3 | Claude integration via the `internal/llm` vision interface: send screenshot as a base64 image content block + current action history; parse `computer_20241022` `tool_use` blocks. |
| G4 | Action dispatch: map Claude tool call arguments to system actions (`xdotool` host binary, `robotgo`, VNC RFB) through `internal/tool`. |
| G5 | Loop termination: stop on text-only response (no `tool_use`), max actions, context deadline, or explicit stop token. |
| G6 | Per-iteration cost tracking: extract input/output tokens from the API response `Usage`; accumulate in SQLite. |
| G7 | Error recovery: retry failed screenshots up to 3 times; return an `is_error` `tool_result` to the model on action failure. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Non-Claude model support (OpenAI computer use, etc.). Claude API only in this PRD. |
| NG2 | Custom action execution environments beyond X11/robotgo/VNC. |
| NG3 | Streaming screenshots to an external observer. |
| NG4 | Parallel computer-use sessions. Each `ComputerUseLoop` manages one session. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Loop iteration time | < 500ms per iteration (excluding API latency) | `testing.B` benchmark |
| Screenshot encoding time | PNG capture + base64 encode in < 200ms | `testing.B` benchmark |
| Error recovery | Failed screenshot retried 3 times before failing session | Unit test |
| Cost tracking accuracy | Token counts match the API response `Usage` | Integration test |
| Loop termination | Text-only model response terminates loop in 100% of test cases | Unit test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Developer | Have a correct, tested screenshot-action loop | I don't implement computer-use boilerplate myself |
| US2 | Developer | Have automatic cost tracking per computer-use session | I know how much each automation costs |
| US3 | Developer | Have error recovery on failed actions | Sessions survive transient failures |
| US4 | Developer | Use the loop with a VNC backend for sandbox isolation | I run computer use safely |

---

## 6. CLI Surface

```go
// Go API (internal/agent):
package agent

loop := agent.NewComputerUseLoop(agent.LoopConfig{
    Model:            "claude-sonnet-4-6",
    ScreenshotBackend: agent.BackendRobotgo, // BackendRobotgo | BackendVNC | BackendX11
    DisplayWidth:     1280,
    DisplayHeight:    800,
    AllowActions:     map[string]bool{"screenshot": true, "left_click": true, "type": true, "key": true},
    MaxActions:       50,
    Timeout:          5 * time.Minute,
})

ctx, cancel := context.WithTimeout(context.Background(), loop.Timeout)
defer cancel()

result, err := loop.Run(ctx, "Open Firefox and navigate to example.com", "my-session")
// result: agent.LoopResult{
//   Status:         "completed" | "max_actions" | "timeout" | "screenshot_error",
//   ActionCount:    N,
//   FinalScreenshot []byte, // raw PNG bytes; base64-encode at the surface
//   CostUSD:        0.12,
// }
```

```
# CLI (thin wrapper, main interface is PRD-118):
tag cu-loop run --goal "..." --backend robotgo|vnc|x11 [options]
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `NewComputerUseLoop` validates that the configured model supports computer use (checked against known model IDs in the `internal/llm` model catalog). |
| FR-02 | Each iteration: capture screenshot via configured backend → encode PNG as base64 → append image content block → call Claude via the `internal/llm` vision interface. |
| FR-03 | The Claude call uses the beta header `computer-use-2024-10-22` and includes the `computer_20241022` tool in the tools list (via the `anthropic-sdk-go` beta messages surface behind the provider adapter). |
| FR-04 | Parse the response: if `content` contains a `tool_use` block for `computer_20241022`, extract the action; else, on a text-only response (`stop_reason: "end_turn"`) terminate. |
| FR-05 | Action dispatch: map action type → platform action via the `internal/tool` handler registry (see action map below). |
| FR-06 | Allow-list check before dispatch through the `internal/sandbox` permission gate: if the action is not in `AllowActions`, append an `is_error` `tool_result` and continue the loop. |
| FR-07 | Per-iteration cost: read `Usage.InputTokens` and `Usage.OutputTokens` from the response; accumulate total cost from the `go:embed` pricing table. |
| FR-08 | Screenshot retry: if screenshot capture returns an error, retry up to 3 times with a 500ms backoff; on the 3rd failure, return a wrapped `ErrScreenshot`. |
| FR-09 | Max actions: after `MaxActions` iterations, set status `max_actions` and return. |
| FR-10 | Messages are truncated to keep the context window under ~90% capacity: drop the oldest screenshot image blocks first (keep text content). Token pressure is estimated with the `len(bytes)/4` heuristic for Anthropic (no local Claude tokenizer exists). |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Screenshot size: scale down to max 1092×1092 pixels (via `golang.org/x/image/draw`) before base64 encoding (Anthropic recommendation). |
| NFR-02 | Base64-encoded screenshots are held in memory in the messages slice; not written to disk by the loop itself. |
| NFR-03 | The loop is synchronous over a single `context.Context`; all provider calls block on the returned event channel. Cancellation is via `ctx`. |
| NFR-04 | The beta message request sets `MaxTokens` to 4096 per computer-use call. |

---

## 9. Technical Design

### 9.1 Action dispatch map

Actions are dispatched through a handler registry in `internal/tool`. Each handler takes the parsed action input and returns a `tool_result` payload or an error.

```go
package agent

// Action is the decoded input of a computer_20241022 tool_use block.
type Action struct {
    Type       string `json:"action"`
    Coordinate []int  `json:"coordinate,omitempty"`
    Text       string `json:"text,omitempty"`
    Direction  string `json:"direction,omitempty"`
    Amount     int    `json:"amount,omitempty"`
}

type ActionResult struct {
    Content []byte // e.g. a fresh screenshot PNG, or textual result
    IsError bool
}

type ActionHandler func(ctx context.Context, a Action) (ActionResult, error)

// Registry is populated by the platform-specific backend (robotgo/x11/vnc).
func (l *ComputerUseLoop) handlers() map[string]ActionHandler {
    return map[string]ActionHandler{
        "screenshot":      func(ctx context.Context, a Action) (ActionResult, error) { return l.takeScreenshot(ctx) },
        "left_click":      func(ctx context.Context, a Action) (ActionResult, error) { return l.driver.Click(ctx, a.Coordinate, "left", false) },
        "right_click":     func(ctx context.Context, a Action) (ActionResult, error) { return l.driver.Click(ctx, a.Coordinate, "right", false) },
        "double_click":    func(ctx context.Context, a Action) (ActionResult, error) { return l.driver.Click(ctx, a.Coordinate, "left", true) },
        "type":            func(ctx context.Context, a Action) (ActionResult, error) { return l.driver.Type(ctx, a.Text) },
        "key":             func(ctx context.Context, a Action) (ActionResult, error) { return l.driver.Key(ctx, a.Text) },
        "scroll":          func(ctx context.Context, a Action) (ActionResult, error) { return l.driver.Scroll(ctx, a.Coordinate, a.Direction, a.Amount) },
        "mouse_move":      func(ctx context.Context, a Action) (ActionResult, error) { return l.driver.Move(ctx, a.Coordinate) },
        "cursor_position": func(ctx context.Context, a Action) (ActionResult, error) { return l.driver.CursorPosition(ctx) },
    }
}
```

The `driver` field satisfies a `Driver` interface with one implementation per backend: `robotgoDriver` (`go-vgo/robotgo` + `kbinani/screenshot`), `x11Driver` (`scrot`/`xwd`/`xdotool` host binaries via `os/exec`), and `vncDriver` (RFB framebuffer + input events, e.g. `github.com/amitbet/vnc2video` for capture and a `mitchellh/go-vnc`-style client for input). Browser-only automations may substitute a `chromedpDriver` (`github.com/chromedp/chromedp`) whose "screenshot" is a page capture.

### 9.2 Go core (loop skeleton)

The loop drives the `internal/llm` vision interface. The Anthropic adapter wraps `anthropic-sdk-go` beta messages with the `computer_20241022` tool and the `computer-use-2024-10-22` beta header; the loop itself is provider-neutral over the event channel.

```go
package agent

import (
    "context"
    "encoding/base64"
    "errors"
    "time"

    "github.com/tag-agent/tag/internal/llm"
)

const (
    computerUseBeta = "computer-use-2024-10-22"
    computerToolName = "computer_20241022"
)

var ErrScreenshot = errors.New("screenshot capture failed after retries")

type LoopConfig struct {
    Model            string
    ScreenshotBackend Backend
    DisplayWidth     int
    DisplayHeight    int
    AllowActions     map[string]bool
    MaxActions       int
    Timeout          time.Duration
}

type LoopResult struct {
    Status          string
    ActionCount     int
    FinalScreenshot []byte
    CostUSD         float64
}

type ComputerUseLoop struct {
    cfg      LoopConfig
    provider llm.VisionProvider // internal/llm vision interface
    driver   Driver
    gate     sandbox.PermissionGate
    prices   llm.PriceTable
}

// captureScreenshot returns raw PNG bytes, scaled to <= 1092x1092.
func (l *ComputerUseLoop) captureScreenshot(ctx context.Context) ([]byte, error) {
    var lastErr error
    for attempt := 0; attempt < 3; attempt++ {
        png, err := l.driver.Screenshot(ctx, min(l.cfg.DisplayWidth, 1092), min(l.cfg.DisplayHeight, 1092))
        if err == nil {
            return png, nil
        }
        lastErr = err
        select {
        case <-ctx.Done():
            return nil, ctx.Err()
        case <-time.After(500 * time.Millisecond):
        }
    }
    return nil, errors.Join(ErrScreenshot, lastErr)
}

func (l *ComputerUseLoop) Run(ctx context.Context, goal, sessionID string) (LoopResult, error) {
    msgs := []llm.Message{{Role: "user", Content: []llm.Block{{Type: "text", Text: goal}}}}
    var (
        actionCount int
        totalIn     int
        totalOut    int
        lastShot    []byte
    )

    for actionCount < l.cfg.MaxActions {
        if ctx.Err() != nil {
            return LoopResult{Status: "timeout", ActionCount: actionCount,
                CostUSD: l.cost(totalIn, totalOut), FinalScreenshot: lastShot}, nil
        }

        // Inject the latest screenshot as an image content block (skip the first turn).
        if actionCount > 0 {
            shot, err := l.captureScreenshot(ctx)
            if err != nil {
                return LoopResult{Status: "screenshot_error", ActionCount: actionCount,
                    CostUSD: l.cost(totalIn, totalOut)}, err
            }
            lastShot = shot
            b64 := base64.StdEncoding.EncodeToString(shot)
            msgs = append(msgs, llm.Message{Role: "user", Content: []llm.Block{{
                Type: "tool_result", ToolUseID: "screenshot",
                Content: []llm.Block{{Type: "image", MediaType: "image/png", Data: b64}},
            }}})
        }

        // Call Claude over the vision interface (computer_20241022 + beta header set in the adapter).
        resp, err := l.provider.Message(ctx, llm.VisionRequest{
            Model:     l.cfg.Model,
            MaxTokens: 4096,
            Betas:     []string{computerUseBeta},
            Tools: []llm.Tool{{
                Type: computerToolName, Name: computerToolName,
                DisplayWidthPx: l.cfg.DisplayWidth, DisplayHeightPx: l.cfg.DisplayHeight, DisplayNumber: 1,
            }},
            Messages: l.truncate(msgs),
        })
        if err != nil {
            return LoopResult{}, err
        }
        totalIn += resp.Usage.InputTokens
        totalOut += resp.Usage.OutputTokens

        // Terminate on a text-only response (no tool_use).
        toolUses := resp.ToolUses()
        if len(toolUses) == 0 {
            return LoopResult{Status: "completed", ActionCount: actionCount,
                CostUSD: l.cost(totalIn, totalOut), FinalScreenshot: lastShot}, nil
        }

        msgs = append(msgs, llm.Message{Role: "assistant", Content: resp.Content})

        for _, tu := range toolUses {
            var a Action
            _ = tu.DecodeInput(&a)

            // Permission gate + allow-list.
            if !l.cfg.AllowActions[a.Type] || !l.gate.Allow(sessionID, a.Type) {
                msgs = append(msgs, llm.Message{Role: "user", Content: []llm.Block{{
                    Type: "tool_result", ToolUseID: tu.ID, IsError: true,
                    Content: []llm.Block{{Type: "text", Text: "Action '" + a.Type + "' is not allowed."}},
                }}})
                continue
            }

            h, ok := l.handlers()[a.Type]
            if !ok {
                continue
            }
            res, err := h(ctx, a)
            if err != nil {
                msgs = append(msgs, llm.Message{Role: "user", Content: []llm.Block{{
                    Type: "tool_result", ToolUseID: tu.ID, IsError: true,
                    Content: []llm.Block{{Type: "text", Text: err.Error()}},
                }}})
                continue
            }
            _ = res
            actionCount++
        }
    }

    return LoopResult{Status: "max_actions", ActionCount: actionCount,
        CostUSD: l.cost(totalIn, totalOut), FinalScreenshot: lastShot}, nil
}

// cost applies the go:embed pricing table for the configured model.
func (l *ComputerUseLoop) cost(in, out int) float64 {
    p := l.prices.For(l.cfg.Model) // e.g. claude-sonnet-4-6: 3.0 / 15.0 per 1M
    return float64(in)*p.InputPerToken + float64(out)*p.OutputPerToken
}
```

### 9.3 Token pressure heuristic

Anthropic ships no local tokenizer, so `truncate` estimates per-block token weight with the `len(serialized)/4` heuristic (base64 image blocks dominate). When the running estimate exceeds ~90% of the model's context window, the oldest image content blocks are dropped first while their surrounding text is retained.

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Model executing arbitrary system actions | Allow-list enforced at dispatch through the `internal/sandbox` permission gate |
| Screenshot leaking sensitive desktop content | Screenshots held only in memory during the loop; PRD-118 persists selectively |
| Infinite API loop consuming credits | `MaxActions` and the `context.Context` deadline are hard stops |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit (table-driven) | Action allow-list enforcement; loop termination on text-only response; cost calculation |
| Integration | Fake `llm.VisionProvider` → loop iteration → action dispatch → screenshot → next iteration |
| Error handling | Screenshot retry; action failure recovery (`is_error` tool_result) |
| Benchmarks | `testing.B` for iteration time and screenshot encode time |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | Text-only model response terminates the loop with status `completed` |
| AC-02 | After `MaxActions` iterations, the loop returns status `max_actions` |
| AC-03 | An action not in the allow-list returns an `is_error` `tool_result` and the loop continues |
| AC-04 | Cost calculation matches `InputTokens * rate + OutputTokens * rate` from the pricing table |
| AC-05 | Screenshot capture retries 3 times before returning `ErrScreenshot` |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| `github.com/anthropics/anthropic-sdk-go` (v1.55.x) | Computer-use beta tool + beta messages, behind the `internal/llm` adapter |
| `github.com/go-vgo/robotgo` + `github.com/kbinani/screenshot` | OS-level screenshot capture + input (optional backend; host input libs) |
| `golang.org/x/image/draw` | Downscale screenshots to ≤ 1092×1092 before encoding |
| `encoding/base64`, `image/png` (stdlib) | Image encoding to base64 content blocks |
| VNC client (e.g. `github.com/amitbet/vnc2video` / `mitchellh/go-vnc`) | RFB framebuffer capture + input dispatch (VNC backend) |
| PRD-118 computer use CLI | Session management wrapper |

> **Single-binary note:** `go-vgo/robotgo` requires CGO and host GUI libraries (X11/CoreGraphics/User32), which conflicts with TAG's `CGO_ENABLED=0` single-static-binary constraint. The default backend for headless/sandboxed sessions is therefore the VNC RFB client (pure Go, talks to the container's VNC server from PRD-120); `robotgo`/`x11` are host-attended backends built behind a CGO build tag. `chromedp` (browser) is CGO-free but drives a page, not the OS desktop.

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should the loop consume the provider's streaming event channel for lower TTFB, or block on the accumulated message? |
| OQ-02 | Should screenshot resolution adapt based on Claude's recommended scaling? |

---

## 15. Complexity & Timeline

**Complexity:** Medium (M)
**Estimated effort:** 5–8 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | `ComputerUseLoop` core, `Driver` interface, screenshot capture, action dispatch | 2 |
| 2 | Claude integration via `internal/llm` (beta header, tool definition, response parsing) | 2 |
| 3 | Allow-list / permission gate, error recovery, cost tracking | 1 |
| 4 | Integration tests, benchmarks, documentation | 2 |
