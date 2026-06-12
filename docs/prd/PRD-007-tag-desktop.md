# PRD-007: `tag desktop` Subcommand

**Status:** Proposed  
**Priority:** P2  
**Estimated Effort:** M (2 weeks)  
**Affects:** `controller.py` (new `cmd_desktop`), setup/install flow

---

## 1. Overview

Hermes v0.16.0 ("The Surface Release") shipped a native Electron desktop application under `apps/desktop/` — 488 files, cross-platform (macOS/Linux/Windows), with streaming chat, session list, drag-and-drop files, clipboard image paste, a Cmd+K command palette, and an in-status-bar model picker. The Electron app is present in TAG's vendor tarball but completely unexposed. This PRD defines `tag desktop` — a subcommand that builds and launches the Hermes Electron app configured against TAG's profile setup.

---

## 2. Problem Statement

- Non-terminal users (designers, PMs, non-technical stakeholders) have no GUI entry point to TAG agents.
- The Electron app is in the vendor tarball but requires a build step; TAG never runs it.
- `tag tui` requires a terminal; `tag chat` is also terminal-only. There is no windowed interface.
- Cursor, Claude Desktop, and other tools ship GUI apps — TAG appears terminal-only by comparison.

---

## 3. Goals

1. `tag desktop build` builds the Electron app from the vendor tarball on first use (one-time, ~2–3 min).
2. `tag desktop` (or `tag desktop open`) launches the app configured against the current TAG profile.
3. `tag desktop` respects `--profile` to launch with a specific profile pre-selected.
4. Build artifacts are cached in `~/.tag/runtime/desktop/` and not rebuilt unless `--refresh` is passed.
5. `tag doctor` checks if desktop has been built and reports build status.

---

## 4. Non-Goals

- Publishing TAG's own Electron app to app stores — we're launching Hermes' Electron app.
- Custom branding of the Electron window (future consideration).
- Mobile app.

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Non-technical user | run `tag desktop` and get a chat window | I don't need to learn terminal commands |
| U2 | Developer | run `tag desktop --profile coder` | the coder profile opens ready to use |
| U3 | Developer | have `tag setup` optionally build the desktop app | I can skip it if I don't need GUI |
| U4 | Developer | run `tag doctor` and see "desktop: not built (run 'tag desktop build')" | I can discover the feature |

---

## 6. Technical Design

### 6.1 Desktop build directory

```
~/.tag/runtime/desktop/
  build/           — Electron build artifacts
  .build_hash      — hash of source used for build, for change detection
```

### 6.2 New functions

```python
def desktop_build_root(cfg: dict[str, Any]) -> Path:
    return runtime_home(cfg) / "desktop"


def desktop_app_path(cfg: dict[str, Any]) -> Path | None:
    """Return path to built desktop app binary, or None if not built."""
    build_root = desktop_build_root(cfg)
    import platform
    system = platform.system()
    if system == "Darwin":
        candidates = list((build_root / "build").glob("*.app/Contents/MacOS/*"))
    elif system == "Linux":
        candidates = list((build_root / "build").glob("*.AppImage")) + \
                     list((build_root / "build").glob("linux-unpacked/tag-desktop"))
    elif system == "Windows":
        candidates = list((build_root / "build").glob("win-unpacked/*.exe"))
    else:
        return None
    return candidates[0] if candidates else None


def build_desktop_app(cfg: dict[str, Any], *, force: bool = False) -> dict[str, Any]:
    """Build Electron desktop app from vendor tarball."""
    hermes_checkout = hermes_root(cfg)
    desktop_src = hermes_checkout / "apps" / "desktop"
    
    if not desktop_src.exists():
        return {"status": "no_source", "message": "apps/desktop not found in hermes checkout"}
    
    build_root = desktop_build_root(cfg)
    build_root.mkdir(parents=True, exist_ok=True)
    
    # Copy desktop source
    if force or not (build_root / "package.json").exists():
        shutil.copytree(desktop_src, build_root, dirs_exist_ok=True)
    
    # npm install
    result = subprocess.run(
        ["npm", "install"],
        cwd=build_root,
        check=True,
        capture_output=True,
        text=True,
    )
    
    # npm run build (Electron Builder)
    result = subprocess.run(
        ["npm", "run", "build"],
        cwd=build_root,
        env={**os.environ, "ELECTRON_BUILDER_COMPRESSION_LEVEL": "1"},
        check=True,
        capture_output=True,
        text=True,
    )
    
    app_path = desktop_app_path(cfg)
    return {
        "status": "built" if app_path else "build_failed",
        "app_path": str(app_path) if app_path else None,
    }
```

### 6.3 `cmd_desktop`

```python
def cmd_desktop(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    
    subcommand = getattr(args, "desktop_subcommand", "open")
    
    if subcommand == "build":
        print("Building Electron desktop app (this may take 2–3 minutes)…")
        result = build_desktop_app(cfg, force=args.force)
        if args.json:
            print(json.dumps(result, indent=2))
            return 0
        if result["status"] == "built":
            print(f"Built: {result['app_path']}")
        else:
            print(f"Build failed: {result.get('message', 'unknown error')}", file=sys.stderr)
            return 1
        return 0
    
    # subcommand == "open"
    app_path = desktop_app_path(cfg)
    if not app_path:
        print("Desktop app not built. Run: tag desktop build", file=sys.stderr)
        return 1
    
    profile = getattr(args, "profile", cfg["defaults"]["master_profile"])
    env = {
        **os.environ,
        **profile_exec_env(cfg, profile),
        "TAG_DESKTOP_PROFILE": profile,
    }
    subprocess.Popen([str(app_path)], env=env)
    print(f"Launched desktop ({profile} profile)")
    return 0
```

### 6.4 Parser registration

```python
p_desktop = subparsers.add_parser("desktop", help="Build and launch Electron desktop app")
p_desktop.add_argument("desktop_subcommand", nargs="?", choices=["open", "build"], default="open")
p_desktop.add_argument("--profile", metavar="NAME")
p_desktop.add_argument("--force", action="store_true", help="Rebuild even if already built")
p_desktop.add_argument("--json", action="store_true")
p_desktop.set_defaults(func=cmd_desktop)
```

### 6.5 `tag setup` integration

Add `--include-desktop` flag to `tag setup`. When passed:
```python
if args.include_desktop:
    steps["desktop"] = build_desktop_app(cfg)
```

Default: off (to keep setup fast). Document as opt-in.

---

## 7. Implementation Plan

| Step | Task |
|------|------|
| 1 | Verify `apps/desktop/` exists in vendor tarball and has valid `package.json` |
| 2 | Implement `desktop_build_root`, `desktop_app_path`, `build_desktop_app` |
| 3 | Implement `cmd_desktop` |
| 4 | Register `desktop` parser |
| 5 | Add `--include-desktop` to `tag setup` parser |
| 6 | Update `cmd_doctor` with desktop build status |
| 7 | Test build on macOS (primary platform) |
| 8 | Update README with "Desktop App" section |

---

## 8. Success Metrics

- `tag desktop build` produces a launchable app on macOS.
- `tag desktop` launches the Electron window without error.
- `tag desktop --profile researcher` sets the correct HERMES_HOME for that profile.
- `tag doctor` shows "desktop: built at /path" or "desktop: not built".

---

## 9. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| apps/desktop not in vendor tarball for all versions | Check existence before attempting build; graceful error message |
| Electron build requires specific Node.js version | Check `node --version` in `build_desktop_app`; document requirements |
| Build takes too long and blocks user | Only build on explicit `tag desktop build`; never auto-build |
| Profile env vars not picked up by Electron app | Verify Hermes Electron app reads `HERMES_HOME` from env at startup |
| macOS Gatekeeper quarantine | Document: first launch may need `xattr -d com.apple.quarantine` |
