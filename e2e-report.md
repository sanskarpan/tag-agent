# TAG E2E Test Report

Date: 2026-06-07
Host: `Darwin Sanskars-MacBook-Pro.local 25.0.0 Darwin Kernel Version 25.0.0: Wed Sep 17 21:41:50 PDT 2025; root:xnu-12377.1.9~141/RELEASE_ARM64_T6030 arm64`
Python: `Python 3.12.12`
Node: `v25.6.1`
TAG version: `tag 0.1.0`
TAG_HOME used:
- primary: `/var/folders/fn/s__2ftd56_gc3z3wm7klqlbc0000gn/T/tmp.NLCAndll4O/tag-home`
- standalone bundled path: `/var/folders/fn/s__2ftd56_gc3z3wm7klqlbc0000gn/T/tmp.NLCAndll4O/standalone-home`
Hermes commit from bundled tarball: unavailable as shipped; the vendored snapshot extracts without `.git` metadata. Runtime version reported by `tag doctor` was `Hermes Agent v0.15.1 (2026.5.29)`.

## Summary

Initial classified pass: `61` command captures, `57` passed, `4` failed.

Additional coverage added afterward: `46` more command captures, for `107`
total capture directories under [e2e-report](/Users/sanskar/dev/test/tag/e2e-report).

Confirmed findings after the extended pass: `5`.

## Remediation Verification On 2026-06-08

Follow-up verification against the live codebase changed the status of several
items from this original report:

- `F-001` is **fixed in code**. `safe_extract_tar_gz()` now blocks absolute,
  `..`, and symlink entries. The original `safe-extract-*` capture directories
  were stale because their probe scripts had syntax errors. Fresh direct
  repros now yield:
  - `abs ALLOWED` for `/abs.txt` because the tarfile module normalizes that
    member name to `abs.txt` before inspection
  - `dotdot BLOCKED Bundled Hermes archive contains an unsafe entry: ../../../etc/passwd`
  - `symlink BLOCKED Bundled Hermes archive contains an unsupported link entry: link`
- `F-002` is **fixed in code**. Fresh isolated repro now exits cleanly with:
  `Hermes Python is not installed; cannot bootstrap profiles. Re-run tag setup without --skip-python-install.`
- `F-003` is **fixed in code**. `tag tui` in a non-TTY now exits `2`, matching
  bare `tag`.
- `F-005` is **fixed in code** for wrapper help passthrough. Fresh repros such
  as `tag config --profile nonesuch -- --help` and
  `tag completion --profile nonesuch -- --help` now return `0` and show Hermes
  help instead of invalid-choice errors.
- `tag hermes --version` is now supported directly and returns Hermes version
  without requiring the explicit `--` separator.
- `tag hermes --` now falls through to Hermes help and returns `0`.

Still open after remediation verification:

- the vendored Hermes patch artifact is still logically inconsistent with the
  pre-patched vendored tree; forward apply fails while reverse-check succeeds
- `docker-py311-smoke` still records exit `2`, but that is a harness issue
  caused by ending the script with bare `tag` in a non-TTY shell

Additional remediation verification for submit semantics:

- submit-path semantic success is now stricter. A known infrastructure/auth
  failure embedded in a zero-exit Hermes response is classified as
  `status="error"` with a `failure_reason`.
- Fresh live repro on `2026-06-08`:
  - `tag submit --task-type review --execution direct --source auth-check ...`
  - returned worker `status: "error"`
  - returned run `status: "error"`
  - populated `failure_reason: "error: codex authentication failed"`

Artifacts:
- inventory: [00-inventory.md](/Users/sanskar/dev/test/tag/e2e-report/00-inventory.md)
- command captures: [e2e-report](/Users/sanskar/dev/test/tag/e2e-report)

## Findings

### F-001: `safe_extract_tar_gz` allows symlink-based escape outside the extraction root
Severity: `blocker`
Component: `src/tag/controller.py:1037`

Repro:
```bash
/var/folders/fn/s__2ftd56_gc3z3wm7klqlbc0000gn/T/tmp.NLCAndll4O/venv/bin/python /var/folders/fn/s__2ftd56_gc3z3wm7klqlbc0000gn/T/tmp.NLCAndll4O/tar-probes/symlink.py
```

Expected:
- extraction rejects archives that can write outside the target directory, including symlink pivots

Actual:
- the extractor accepted a tar containing `link -> /tmp/.../outside` and then wrote `payload.txt` through that symlink
- captured result in [safe-extract-symlink-escape](/Users/sanskar/dev/test/tag/e2e-report/safe-extract-symlink-escape/stdout.txt) shows:
  - `extract-ok`
  - `True`
  - `pwned`

Suggested fix:
- reject symlink and hardlink members in `safe_extract_tar_gz`
- if symlinks must be allowed, resolve and validate `member.linkname` before extraction and replace `extractall` with a member-by-member safe writer

Citations:
- [controller.py](/Users/sanskar/dev/test/tag/src/tag/controller.py:1037)
- [controller.py](/Users/sanskar/dev/test/tag/src/tag/controller.py:1048)

Quoted code:
```python
def safe_extract_tar_gz(archive: Path, target: Path) -> None:
    target_real = target.resolve()
    with tarfile.open(archive, "r:gz") as tf:
        members = tf.getmembers()
        for member in members:
            member_name = member.name
            if member_name.startswith("/") or member_name.startswith(".."):
                raise SystemExit(...)
            dest = (target / member_name).resolve()
            if target_real != dest and target_real not in dest.parents:
                raise SystemExit(...)
        tf.extractall(target)
```

### F-002: `tag setup --skip-python-install` crashes with a traceback instead of failing cleanly
Severity: `major`
Component: `src/tag/controller.py:1214`

Repro:
```bash
TAG_HOME=/var/folders/fn/s__2ftd56_gc3z3wm7klqlbc0000gn/T/tmp.NLCAndll4O/standalone-home \
TAG_HERMES_ROOT=/var/folders/fn/s__2ftd56_gc3z3wm7klqlbc0000gn/T/tmp.NLCAndll4O/standalone-home/managed/hermes-agent-upstream \
/var/folders/fn/s__2ftd56_gc3z3wm7klqlbc0000gn/T/tmp.NLCAndll4O/venv/bin/tag setup --skip-python-install --skip-tui-build --json
```

Expected:
- either `--skip-python-install` should be rejected when later steps require the Hermes CLI, or setup should skip bootstrap phases that require `hermes`
- no raw traceback

Actual:
- setup continues into `bootstrap_profiles()`
- `bootstrap_profiles()` calls `run_hermes()`
- `run_hermes()` tries to exec a Hermes binary that does not exist yet
- result: raw `FileNotFoundError` traceback in [standalone-setup-skip-python](/Users/sanskar/dev/test/tag/e2e-report/standalone-setup-skip-python/stderr.txt)

Suggested fix:
- gate bootstrap on `args.skip_python_install`
- or raise an explicit `SystemExit` before bootstrap when `.venv/bin/hermes` is absent

Citations:
- [controller.py](/Users/sanskar/dev/test/tag/src/tag/controller.py:1214)
- [controller.py](/Users/sanskar/dev/test/tag/src/tag/controller.py:1226)
- [controller.py](/Users/sanskar/dev/test/tag/src/tag/controller.py:1232)
- [controller.py](/Users/sanskar/dev/test/tag/src/tag/controller.py:516)
- [controller.py](/Users/sanskar/dev/test/tag/src/tag/controller.py:528)
- [controller.py](/Users/sanskar/dev/test/tag/src/tag/controller.py:305)

Quoted code:
```python
if not args.skip_python_install:
    steps["python_install"] = install_hermes_python(cfg)
steps["patch"] = apply_hermes_patch(cfg)
if not args.skip_tui_build:
    steps["tui"] = install_tui_dependencies(cfg)
steps["bootstrap"] = {
    "profiles": bootstrap_profiles(cfg),
    "rendered": render_profiles(cfg, force=True),
}
```

### F-003: `tag tui` non-TTY path exits with code `1`, unlike bare `tag` which exits `2`
Severity: `major`
Component: `src/tag/controller.py:1266`

Repro:
```bash
TAG_HOME=/var/folders/fn/s__2ftd56_gc3z3wm7klqlbc0000gn/T/tmp.NLCAndll4O/tag-home \
/var/folders/fn/s__2ftd56_gc3z3wm7klqlbc0000gn/T/tmp.NLCAndll4O/venv/bin/tag tui
```

Expected:
- explicit non-interactive guard should return the same documented status as bare `tag`
- `cmd_default()` already returns `2` for the same class of error

Actual:
- `tag tui` exits `1` because `cmd_tui()` raises `SystemExit(<string>)`
- bare `tag` exits `2` because `cmd_default()` returns `2`
- artifacts:
  - [tui-non-tty](/Users/sanskar/dev/test/tag/e2e-report/tui-non-tty/exitcode.txt)
  - [root-non-tty](/Users/sanskar/dev/test/tag/e2e-report/root-non-tty/exitcode.txt)

Suggested fix:
- make `cmd_tui()` return `2` after printing the message, or raise `SystemExit(2)` after writing to `stderr`

Citations:
- [controller.py](/Users/sanskar/dev/test/tag/src/tag/controller.py:1266)
- [controller.py](/Users/sanskar/dev/test/tag/src/tag/controller.py:1274)
- [controller.py](/Users/sanskar/dev/test/tag/src/tag/controller.py:1366)
- [controller.py](/Users/sanskar/dev/test/tag/src/tag/controller.py:1374)

Quoted code:
```python
if not can_launch_interactive_tui() and ...:
    raise SystemExit(
        "TAG TUI requires an interactive terminal. ..."
    )
```

### F-004: the shipped patch no longer applies forward to the vendored Hermes snapshot
Severity: `minor`
Component: `src/tag/controller.py:1106`

Repro:
```bash
git -C /var/folders/fn/s__2ftd56_gc3z3wm7klqlbc0000gn/T/tmp.NLCAndll4O/patch-check-2 apply --check /Users/sanskar/dev/test/tag/src/tag/patches/hermes-ui.patch
git -C /var/folders/fn/s__2ftd56_gc3z3wm7klqlbc0000gn/T/tmp.NLCAndll4O/patch-check-2 apply --reverse --check /Users/sanskar/dev/test/tag/src/tag/patches/hermes-ui.patch
```

Expected:
- either the patch should apply cleanly to the vendored Hermes source, or the package should document that the vendored snapshot already includes the patch and the patch file is retained only for provenance

Actual:
- forward apply fails on every hunk in [patch-git-apply-check](/Users/sanskar/dev/test/tag/e2e-report/patch-git-apply-check/stderr.txt)
- reverse-check succeeds in [patch-git-reverse-check](/Users/sanskar/dev/test/tag/e2e-report/patch-git-reverse-check/exitcode.txt)
- runtime still works because `apply_hermes_patch()` treats reverse-check success as `already-applied`

Suggested fix:
- either refresh the patch so it applies to the vendored snapshot, or explicitly treat the vendored Hermes tree as pre-patched and stop shipping a forward-apply patch contract

Citations:
- [controller.py](/Users/sanskar/dev/test/tag/src/tag/controller.py:1106)
- [controller.py](/Users/sanskar/dev/test/tag/src/tag/controller.py:1115)
- [controller.py](/Users/sanskar/dev/test/tag/src/tag/controller.py:1116)
- [controller.py](/Users/sanskar/dev/test/tag/src/tag/controller.py:1124)

Quoted code:
```python
reverse = run_external(["git", "apply", "--reverse", "--check", str(patch)], ...)
if reverse.returncode == 0:
    return {"patch": str(patch), "status": "already-applied"}
forward = run_external(["git", "apply", "--check", str(patch)], ...)
```

### F-005: wrapper passthrough leaks the `--` separator into Hermes and breaks documented `-- --help` flows
Severity: `major`
Component: `src/tag/controller.py:1246`

Repro:
```bash
tag status --profile orchestrator -- --help
tag config --profile orchestrator -- --help
tag completion --profile orchestrator -- --help
tag mcp --profile orchestrator -- --help
```

Expected:
- TAG should consume the `--` separator before invoking Hermes, so these wrappers
  become `hermes status --help`, `hermes config --help`, etc.
- The documented wrapper form in the README should work consistently.

Actual:
- wrapper commands inject the Hermes subcommand first and only strip a leading
  `--` when it is the first forwarded token
- for wrapper subcommands, the forwarded argv becomes `status -- --help`,
  `config -- --help`, and similar
- Hermes then treats `--help` as an unexpected positional or invalid choice
- captured examples:
  - [wrapper-status-help](/Users/sanskar/dev/test/tag/e2e-report/wrapper-status-help/stderr.txt)
  - [wrapper-config-help](/Users/sanskar/dev/test/tag/e2e-report/wrapper-config-help/stderr.txt)
  - [wrapper-completion-help](/Users/sanskar/dev/test/tag/e2e-report/wrapper-completion-help/stderr.txt)
  - [wrapper-mcp-help](/Users/sanskar/dev/test/tag/e2e-report/wrapper-mcp-help/stderr.txt)
  - [wrapper-logs-help](/Users/sanskar/dev/test/tag/e2e-report/wrapper-logs-help/stdout.txt)

Suggested fix:
- normalize `args.hermes_args` before prepending the wrapper command, or strip
  a sentinel `--` after the wrapper command is injected
- update the passthrough examples only after the forwarded argv contract is
  fixed and verified

Citations:
- [controller.py](/Users/sanskar/dev/test/tag/src/tag/controller.py:1246)
- [controller.py](/Users/sanskar/dev/test/tag/src/tag/controller.py:1253)
- [controller.py](/Users/sanskar/dev/test/tag/src/tag/controller.py:1278)
- [README.md](/Users/sanskar/dev/test/tag/README.md:112)

Quoted code:
```python
def cmd_hermes_passthrough(args: argparse.Namespace) -> int:
    ...
    hermes_args = list(args.hermes_args)
    if hermes_args[:1] == ["--"]:
        hermes_args = hermes_args[1:]
    proc = subprocess.run([str(hermes_bin(cfg)), *hermes_args], ...)

def cmd_hermes_command(args: argparse.Namespace, command_name: str) -> int:
    forwarded = [command_name, *args.hermes_args]
```

## Coverage Matrix

| Surface | Evidence |
| --- | --- |
| Install / packaging | `build-host-python39-fail`, `python-build`, `wheel-list`, `npm-pack`, `npm-pack-list`, `node-launcher-version` |
| Root CLI / help | `build-version`, `help-root`, `help-setup`, `help-submit`, `help-benchmark`, `help-openrouter-models`, `help-set-model`, `help-import-codex`, `help-hermes`, `wrapper-chat-help`, `wrapper-gateway-help`, `wrapper-tui-help` |
| Doctor / setup lifecycle | `doctor-before`, `setup-skip-tui`, `doctor-after-skip-tui`, `standalone-doctor-before`, `standalone-setup-skip-python`, `standalone-setup-skip-tui`, `standalone-doctor-after`, `standalone-update-json` |
| Render / routing / env | `bootstrap-json`, `render-json`, `route-research-json`, `route-implementation-json`, `route-review-json`, `route-mixed-json`, `route-master-coder`, `route-master-model-missing`, `route-bogus`, `env-print`, `assignments-json`, `models-orchestrator-json`, `models-provider-bogus`, `models-limit-zero`, `set-model-researcher`, `assignments-after-set-model` |
| Negative validation | `openrouter-missing-key`, `import-codex-missing-profile`, `import-codex-missing-home`, `runs-limit-zero`, `models-limit-negative`, `submit-empty`, `benchmark-no-cases`, `root-non-tty`, `tui-non-tty` |
| Stateful runtime | `import-codex-reviewer`, `set-model-reviewer-codex`, `submit-direct-orchestrator`, `submit-direct-verify`, `benchmark-orchestrator-exact`, `runs-after-runtime` |
| Tar / patch / TUI | `safe-extract-abs-path`, `safe-extract-dotdot`, `safe-extract-symlink-escape`, `patch-git-apply-check`, `patch-git-apply`, `patch-git-reverse-check`, `tui-build-standalone`, `tui-test-standalone` |
| Wrapper passthrough | `hermes-version`, `hermes-version-no-separator`, `hermes-empty-separator`, `wrapper-status-help`, `wrapper-status-nonesuch-help`, `wrapper-config-help`, `wrapper-config-nonesuch-help`, `wrapper-completion-help`, `wrapper-completion-nonesuch-help`, `wrapper-dashboard-help`, `wrapper-dashboard-nonesuch-help`, `wrapper-logs-help`, `wrapper-logs-nonesuch-help`, `wrapper-mcp-help`, `wrapper-mcp-nonesuch-help`, `wrapper-memory-help`, `wrapper-memory-nonesuch-help`, `wrapper-plugins-help`, `wrapper-plugins-nonesuch-help`, `wrapper-prompt-size-help`, `wrapper-prompt-size-nonesuch-help`, `wrapper-sessions-help`, `wrapper-sessions-nonesuch-help`, `wrapper-skills-help`, `wrapper-skills-nonesuch-help`, `wrapper-tools-help`, `wrapper-tools-nonesuch-help` |
| npm launcher reinstall | `npm-launcher-version-isolated`, `npm-launcher-version-second`, `npm-launcher-version-bad-stamp`, `npm-launcher-version-force`, `npm-launcher-version-missing-tag` |
| Linux smoke | `docker-py311-smoke`, `docker-py312-smoke`, `docker-py313-install` |
| Existing unit suite | `pytest-controller` |

## TUI Coverage

- Non-TTY guard exercised through `tui-non-tty`
- Bundled standalone TUI build exercised through `tui-build-standalone`
- Patched Vitest surface exercised through `tui-test-standalone`
- Patch applicability and reverse-check audited through `patch-git-apply-check` and `patch-git-reverse-check`
- Added upstream Vitest coverage for banner width and rapid resize behavior in
  [brandingBanner.test.tsx](/Users/sanskar/dev/test/hermes-agent-upstream/ui-tui/src/__tests__/brandingBanner.test.tsx)

## Additional Coverage Added On 2026-06-07

- The npm launcher reinstall paths are now covered and behaved correctly:
  - broken stamp rebuilds
  - `TAG_NPM_FORCE_REINSTALL=1` rebuilds
  - missing `venv/bin/tag` rebuilds
- Linux/Docker smoke was completed for Python `3.11`, `3.12`, and `3.13`.
- `TAG_FORCE_TUI=1` was verified on a non-TTY path; the TAG-side guard is
  bypassed and Hermes itself exits cleanly with `hermes-tui: no TTY` in
  [force-tui-non-tty-timeout](/Users/sanskar/dev/test/tag/e2e-report/force-tui-non-tty-timeout/stdout.txt).
- Additional route coverage is present for `research`, `implementation`,
  `review`, and master-profile/model override cases.

## Additional Coverage Added On 2026-06-08

- Clean standalone setup on Python `3.11` was re-run after fixing managed-root
  provisioning so TAG no longer accidentally binds to a sibling
  `hermes-agent-upstream` checkout during setup.
- Docker critical-path reruns were completed on `python:3.11-slim`,
  `python:3.12-slim`, and `python:3.13-slim` with:
  - `tag doctor --json`
  - `tag setup --json`
  - `tag submit --task-type research --execution direct`
  - `tag benchmark --profile researcher --case exact-echo`
  - `tag tui --profile researcher` under `script` with `HERMES_TUI_DIR`
- `banner_hero` width behavior is now covered deterministically at `cols=40`
  and `cols=120` in the Hermes TUI test suite.
- Rapid resize stress is now covered deterministically by 100 alternating
  narrow/wide rerenders in the Hermes TUI test suite.

## Open Questions / Risks

- The Windows-specific branch in `bin/tag.js` was still not executed.
- `tag hermes -- --version` works, but `tag hermes --version` still fails at
  TAG argparse in [hermes-version-no-separator](/Users/sanskar/dev/test/tag/e2e-report/hermes-version-no-separator/stderr.txt).
- A second `tag setup --skip-tui-build --refresh --json` run did not complete
  within the forced `>70s` window in
  [setup-refresh-second](/Users/sanskar/dev/test/tag/e2e-report/setup-refresh-second/stderr.txt),
  so the refresh-idempotence path remains incomplete in this audit.
- The full upstream Hermes TUI suite is not fully green in this checkout:
  `virtualHeights.test.ts`, `cursorDriftRegression.test.ts`, and
  `packages/hermes-ink/src/utils/execFileNoThrow.test.ts` still fail
  independently of the TAG-specific additions.
