# TASK: In-Depth End-to-End Testing & Audit of the TAG CLI Project

You are an expert QA engineer + SRE. Perform a **full in-depth E2E test pass**
on the TAG project in this repo. Do not stop at unit tests. **Actually run
commands**, **mutate the filesystem**, **kill processes**, **break things**,
**restore them**, and **verify recovery**. Your job is to discover every bug,
gap, race condition, edge case, and TUI regression in the project. Produce a
concrete, reproducible test report with file/line citations.

## 0. Ground Rules

- **Read everything first** before running anything. Skim in this order and
  take notes:
  - `README.md`, `TODO.md`, `MANIFEST.in`, `pyproject.toml`, `package.json`
  - `src/tag/__init__.py`, `src/tag/__main__.py`, `src/tag/cli.py`
  - `src/tag/controller.py` (the entire 2249-line file — every command,
    every helper, every argparse branch)
  - `src/tag/config/default.yaml`, `src/tag/config/benchmark-suite.yaml`
  - `src/tag/assets/skins/tag-control.yaml`
  - `src/tag/patches/hermes-ui.patch` (every hunk — what files in
    Hermes TUI it touches and how)
  - `src/tag/docs/gap-analysis.md`, `src/tag/docs/hermes-capability-audit.md`
  - `bin/tag.js` (the entire npm launcher)
  - `tests/test_controller.py` (the existing 16 unit tests — understand
    what is NOT covered)
  - `.github/workflows/ci.yml`, `.github/workflows/release.yml`
  - `.gitignore`, `.npmignore`
- **Always work against an isolated `TAG_HOME`** via
  `export TAG_HOME="$(mktemp -d)/tag-home"` unless a section explicitly
  requires the real home. Never let tests pollute `~/.tag`.
- **Capture every command's stdout, stderr, and exit code.** Save them to
  `e2e-report/<test-id>/{stdout,stderr,exitcode}.txt`.
- **Run on the host platform first** (darwin). Re-run the critical paths
  on Linux via Docker (`python:3.11-slim` + `node:20`) before publishing
  the final report.
- **Cite file:line for every finding** using the format
  `src/tag/controller.py:1234`. Never hand-wave.

## 1. Static / Source-Level Audit (read-only)

For every finding, cite line numbers and quote the offending code.

### 1.1 controller.py hygiene
- [x] **Argparse surface**: list every subparser and its flags. Confirm
      every flag has a sensible default and is documented. Check
      `build_parser()` lines ~1992–2237.
- [x] **Help text quality**: run `tag --help`, `tag setup --help`,
      `tag submit --help`, `tag benchmark --help`, `tag openrouter-models
      --help`, `tag set-model --help`, `tag import-codex --help`, every
      Hermes wrapper (`tag chat --help` … `tag tui --help`). Flag any
      that are missing, inconsistent, or misleading.
- [x] **Error messages**: every `raise SystemExit(...)` should have a
      user-actionable message. Audit them all.
- [x] **Path safety**: review `safe_extract_tar_gz` (controller.py:~1037).
      Verify it actually blocks: absolute paths, `..` traversal, symlink
      escapes, and `safe_extract_tar_gz` is the ONLY call to `tarfile`.
      Look for any other `extractall`, `unpack`, `zipfile.extract*`,
      `shutil.unpack_archive` in the codebase.
- [x] **Subprocess hygiene**: review every `subprocess.run` /
      `subprocess.Popen` / `os.system` / `os.popen` call. Confirm:
      `shell=False`, `check=` is intentional, `env=` is built via
      `hermes_env`/`profile_exec_env` and never leaks the parent env's
      secrets accidentally.
- [x] **SQL injection**: review every `conn.execute` with f-strings or
      `%`-formatting. The two `INSERT`s use parameterized queries
      (good) — confirm no new ones slipped in.
- [x] **JSON parsing**: every `json.loads(...)` should be wrapped or
      tested for malformed input. Find the bare ones (e.g. in
      `cmd_submit` kanban branch, `show_kanban_task`).
- [x] **YAML loading**: `load_config` uses `safe_load` (good). Find any
      `yaml.load(` (unsafe) or `yaml.unsafe_load` — must be zero.
- [x] **Type coercion bugs**: review `parse_model_ref`, `format_model_ref`,
      `slugify`, `nonnegative_int`, `positive_int`. Edge cases:
      - provider="" / model="" (caught?)
      - "openrouter/" (caught?)
      - "/model" (caught?)
      - model with `/` in its id (split bug?)
- [x] **TUI guard** `can_launch_interactive_tui` and `cmd_tui` /
      `cmd_default`: confirm the `TAG_FORCE_TUI` bypass works, confirm
      the error message tells users what to do, confirm piping stdin
      (e.g. `echo q | tag`) exits 2 with a useful message.
- [x] **Update lifecycle** `cmd_update` (~1351): verify the
      bundled/git/missing branches are mutually exclusive and don't
      double-apply the patch.
- [x] **Codex passthrough** `profile_exec_env` (~230): verify
      `TAG_PASSTHROUGH_HOME_PROFILES` and `TAG_REAL_HOME` defaults
      match the documented behavior in
      `src/tag/docs/gap-analysis.md` lines 187–196.

### 1.2 bin/tag.js hygiene
- [x] **Python version probe order** is correct on Unix AND Windows
      (lines 40–78). Test by shadowing `PATH` so that a fake
      `python3.13` returns version `3.9` and a real `python3.12` exists.
- [x] **Reinstall stamp logic** (lines 94–130): break the stamp, set
      `TAG_NPM_FORCE_REINSTALL=1`, set `--reinstall-runtime`, edit the
      venv to remove `tag`. Each must trigger a rebuild.
- [x] **Windows path handling** for `Scripts\tag.exe` and
      `Scripts\python.exe` (lines 80–92). Cannot run on darwin, but
      at minimum manually trace the branch.
- [x] **stdio inheritance** (`stdio: "inherit"`): confirm child failures
      surface correct exit codes.

### 1.3 Hermes TUI patch (`src/tag/patches/hermes-ui.patch`)
- [x] **Apply cleanly** against the bundled tarball:
      `tar -xzf src/tag/vendor/hermes-agent-upstream.tar.gz -C /tmp/e2e`,
      then `git init && git add -A && git commit` inside it, then
      `git apply --check` the patch. Then `git apply` it. Confirm
      zero rejects. Note Hermes version/commit it was generated
      against (the tarball contains it).
- [x] **Reverse cleanly** (the `patch_status` function relies on
      `git apply --reverse --check`).
- [x] **Idempotency** (apply twice → second attempt must say
      "already-applied" without corrupting files).
- [x] **No-context application**: try applying to a non-Hermes tree
      (e.g. the TAG repo itself) and confirm graceful failure.
- [x] **Hunk review**: walk each hunk and verify the modified code
      still compiles in TypeScript:
      - `ui-tui/src/__tests__/theme.test.ts` — does the new test
        reference functions/colors that actually exist in the
        surrounding code?
      - `ui-tui/src/components/appChrome.tsx` — does `StatusRule`
        accept and render the new `profileName` prop?
      - `ui-tui/src/components/appLayout.tsx` — does `ui.info` have
        a `profile_name` field upstream?
      - `ui-tui/src/components/branding.tsx` — does `SessionInfo`
        have `profile_name`, `fast`, `service_tier`,
        `reasoning_effort`? Does `visibleProfileName` actually
        return what the renderer expects?
      - `ui-tui/src/theme.ts` — do the new `status_bar_*` keys
        exist in the skin schema (`tag-control.yaml`)? Yes they
        do, but verify no other Hermes skin in the bundled tree
        breaks.
- [x] **Build the patched TUI**: `cd ui-tui && npm install && npm
      run build`. Capture all warnings/errors. If React or Vitest
      are missing, the test won't run.

### 1.4 Configuration schema
- [x] `default.yaml` keys vs `load_config` consumers: enumerate every
      key the code reads (`lab_name`, `upstream`, `runtime`, `skins`,
      `defaults`, `env_examples`, `profiles`, `routing`) and confirm
      no key in the YAML is silently ignored.
- [x] `tag-control.yaml` keys vs `fromSkin` in Hermes theme.ts (the
      patch adds support for `status_bar_*`, `ui_muted`, `agent_icon`,
      `selection_bg`, `completion_menu_*`, `shell_dollar`,
      `banner_logo`, `banner_hero`, `tool_prefix`). Flag any key in
      the skin that the theme ignores or that crashes the loader.
- [x] Verify the skin does not contain invalid hex colors (must match
      `#[0-9A-Fa-f]{6}` or `#[0-9A-Fa-f]{3}`).

## 2. Build & Install E2E

Run on a clean `TAG_HOME` and on a fresh venv.

- [x] `pip install -e .[dev]` from source — must succeed on Python 3.11,
      3.12, 3.13.
- [x] `pip install -e .[dev]` MUST FAIL on Python 3.10 and 3.14 with
      a clear `Requires-Python` error.
- [x] `python -m build` — produces both `sdist` and `wheel` under
      `dist/`. Inspect `dist/*.whl` with `unzip -l` and confirm it
      contains:
      - `tag/__init__.py`, `tag/__main__.py`, `tag/cli.py`,
        `tag/controller.py`
      - `tag/config/default.yaml`, `tag/config/benchmark-suite.yaml`
      - `tag/assets/skins/tag-control.yaml`
      - `tag/patches/hermes-ui.patch`
      - `tag/vendor/hermes-agent-upstream.tar.gz` (the 28MB file)
      - `tag-0.1.0.dist-info/METADATA`, `LICENSE`, `README.md`
- [x] `pip install dist/tag_agent-0.1.0-py3-none-any.whl` into a
      throwaway venv. Run `which tag`, `tag --version`, `tag --help`.
- [x] `npm pack` — produces `tag-agent-0.1.0.tgz`. Inspect with
      `tar -tzf` and confirm:
      - `bin/tag.js`
      - `package/...` mirror of the Python source layout
      - NO `tests/`, NO `*.egg-info/`, NO `__pycache__/`
- [x] `npm install -g ./tag-agent-0.1.0.tgz` in a sandboxed
      `npm config set prefix` directory. Run `tag --version`.
      Uninstall, confirm cleanup.
- [x] `python -m tag --version` and `node bin/tag.js --version`
      must both return `0.1.0` (matches `ci.yml` step "Smoke-test").
- [x] **Reproducible build**: rebuild twice and `diff` the wheel
      contents. Must be byte-identical (modulo timestamps).

## 3. Setup / Doctor E2E Paths

For every variant, set `TAG_HOME` to a fresh temp dir and capture
`tag doctor --json` before and after.

- [x] **Bare `tag` invocation in a TTY** (use `script` to fake a
      TTY): should auto-launch `tag setup` if `~/.tag` is missing
      AND `hermes_bin` doesn't exist.
- [x] **Bare `tag` invocation non-TTY** (pipe stdin): must print
      the "non-interactive shell" message to stderr and exit 2.
      Confirm exit code, confirm NO attempt to launch the TUI.
- [x] **Bare `tag` with `TAG_FORCE_TUI=1`** non-TTY: must still
      respect the bypass and try to launch (it'll fail, but the
      guard must be skipped).
- [x] **`tag setup` from clean**: must:
      1. create `~/.tag/config/tag.yaml` and `benchmark-suite.yaml`
      2. extract the bundled tarball into `managed/hermes-agent-upstream`
      3. create `.venv`
      4. install Hermes with `[cli,web,mcp]` extras
      5. apply `hermes-ui.patch`
      6. `npm install` and `npm run build` in `ui-tui`
      7. `hermes profile create` for each profile
      8. render profile configs and `.env.example` files
      9. import Codex if `~/.codex/auth.json` exists
  Each step must be reported in `--json` mode. Verify all 9
  observable side effects on disk.
- [x] **`tag setup --skip-tui-build`**: must NOT run npm; verify
      `ui-tui/node_modules` is absent but `ui-tui` exists.
- [x] **`tag setup --skip-python-install`**: must NOT pip-install
      Hermes; venv may be empty.
- [x] **`tag setup --refresh`** twice: second run must report
      `clone.status == "updated"` (or `existing`), not re-extract
      the tarball over a git checkout.
- [x] **`tag setup` with `TAG_HERMES_ROOT=/tmp/foo`**:
      must use `/tmp/foo` as the checkout dir.
- [x] **`tag setup` with `TAG_HERMES_REPO=https://...`** and
      bundled tarball deleted: must `git clone` from the override.
- [x] **`tag setup` with `TAG_HERMES_REF=some-branch`**: must
      check out that branch.
- [x] **`tag setup` with no git on PATH and no bundled tarball**:
      must fail with a clear "git not found" or "snapshot not
      available" error, not a traceback.
- [x] **`tag setup` with no npm on PATH and no `--skip-tui-build`**:
      must fail with a clear "npm not found" error.
- [x] **`tag setup` with Python 3.10**: must fail with the
      "TAG currently requires Python >=3.11 and <3.14" message
      BEFORE touching the filesystem.
- [x] **`tag setup` with read-only `TAG_HOME`**: must fail with a
      `PermissionError`-derived message, not a stack trace.
- [x] **`tag setup` with corrupt tarball** (truncate the file
      by 1 byte): must fail at `safe_extract_tar_gz`, not silently
      produce a broken checkout.
- [x] **`tag setup` with a tampered tarball** containing `../../../etc/passwd`:
      `safe_extract_tar_gz` MUST reject. Test by injecting such an
      entry into a copy of the tarball with Python.
- [x] **`tag setup` with `TAG_IMPORT_CODEX_HOME` pointing at a
      real `auth.json`**: must import into both `orchestrator` and
      `codex-runtime-master` profiles.
- [x] **`tag setup` with `TAG_IMPORT_CODEX_HOME` pointing at a
      dir without `auth.json`**: must report `skipped-no-auth` for
      both profiles (NOT crash).
- [x] **`tag setup` run twice idempotently**: the second run must
      not re-render profiles unless `--force`-equivalent is used
      via `tag bootstrap --force`.
- [x] **`tag doctor`** before setup: must show
      `hermes_checkout_exists=false`, `patch_status=checkout-missing`,
      `tui_dist_exists=false`, `tui_react_installed=false`,
      `tui_vitest_installed=false`, `python_runtime_supported=true`.
- [x] **`tag doctor`** after setup: all of the above flip to `true`,
      `hermes_version` is populated, no `hermes_version_error`.
- [x] **`tag doctor --json`** output must be valid JSON and
      contain every documented key.

## 4. Native TAG Command E2E

### 4.1 `tag bootstrap`
- [x] Cold: creates all 5 profile homes.
- [x] Warm: marks them `existing`, does NOT recreate.
- [x] `--force`: re-renders config.yaml and skins.
- [x] `--json`: emits valid JSON.

### 4.2 `tag render`
- [x] Produces `config.yaml`, `.env.example`, and `skins/tag-control.yaml`
      under each profile's home.
- [x] The rendered `config.yaml` has `display.skin: tag-control`.
- [x] `--force` overwrites; without it, leaves existing files alone.

### 4.3 `tag route`
- [x] `tag route --task-type research` → master=orchestrator,
      workers=[researcher], verifier=reviewer, execution=kanban.
- [x] `tag route --task-type implementation` → workers=[coder].
- [x] `tag route --task-type review` → workers=[reviewer],
      execution=direct.
- [x] `tag route --task-type mixed` → workers=[researcher, coder].
- [x] `tag route --task-type bogus` → exits non-zero with
      "Unknown task type 'bogus'. Available: implementation, mixed,
      research, review".
- [x] `tag route --task-type research --master-profile coder`
      → uses coder as master (override honored).
- [x] `tag route --task-type research --master-profile nonesuch`
      → "Master profile 'nonesuch' is not defined in config."
- [x] `tag route --task-type research --worker-profile researcher
      --worker-profile bogus` → "Worker profile 'bogus' is not
      defined in config."
- [x] `tag route --task-type research --master-model openai-codex/gpt-5.4`
      → `route.master.model.provider == "openai-codex"`,
      `.default == "gpt-5.4"`.
- [x] `tag route --task-type research --master-model openai-codex`
      (missing model) → clear error.
- [x] `tag route --task-type research --verifier-model
      openrouter/foo/bar` → verifier model updated.
- [x] `tag route --task-type mixed --worker-model-override
      researcher=openrouter/x/y --worker-model-override
      coder=openrouter/z/w` → both workers updated.
- [x] `tag route --task-type mixed --worker-model-override
      bogus=openrouter/x/y` → no change to workers (only matching
      names updated) — confirm and document.
- [x] `tag route --task-type research --worker-model-override
      noseprovider` → "Invalid worker override 'noseprovider'.
      Use profile=provider/model-id."
- [x] `tag route --json` → valid JSON with all keys.

### 4.4 `tag env`
- [x] Prints `HOME`, `HERMES_HOME`, `CODEX_HOME`, `PATH`.
- [x] `PATH` is prefixed with `<hermes_root>/.venv/bin`.
- [x] Override with `TAG_HERMES_HOME`, `TAG_CODEX_HOME`.

### 4.5 `tag assignments`
- [x] Lists all 5 profiles with their primary model and delegation
      model (where present).
- [x] `orchestrator` shows `delegation: openrouter/...`.
- [x] `codex-runtime-master` shows `[codex_app_server]` in the
      runtime column.
- [x] `--json` → valid JSON.

### 4.6 `tag models`
- [x] `tag models --profile orchestrator` lists providers with
      `openai-codex` and possibly `openrouter`.
- [x] `--provider openrouter` filters to just that provider.
- [x] `--provider bogus` produces an empty list (not an error).
- [x] `--limit 0` produces no models.
- [x] `--limit -1` must be rejected by `nonnegative_int` argparse
      type with a clear error.
- [x] `--json` → valid JSON with `providers` list.

### 4.7 `tag set-model`
- [x] `tag set-model --profile researcher --ref openrouter/foo/bar`
      mutates `~/.tag/config/tag.yaml`, sets
      `profiles.researcher.config.model.provider` and `.default`.
- [x] Re-running `tag models --profile researcher` reflects the
      new primary.
- [x] `tag set-model --profile orchestrator --target delegation
      --ref openrouter/x/y` mutates the delegation block, not the
      primary.
- [x] `tag set-model --profile orchestrator --target primary
      --ref openai-codex/gpt-5.4 --openai-runtime codex_app_server`
      sets the runtime.
- [x] `tag set-model --ref foo` (no slash) → error.
- [x] `tag set-model --profile nonesuch --ref x/y` → "Unknown
      profile 'nonesuch'".
- [x] `tag set-model` on a profile missing `config.model` should
      not crash — verify the `setdefault` chain.
- [x] After mutation, `tag render --force` should pick up the new
      model in the rendered profile config.

### 4.8 `tag submit`
- [x] `tag submit --task-type research --execution direct
      --prompt "Reply with exactly: smoke-ok" --json` → returns
      `run_id`, `status`, and a step per worker.
- [x] `tag submit --task-type research --execution kanban
      --prompt "..." --wait-seconds 5 --json` → returns with
      task IDs and `status="ok"` if the worker finishes within
      the deadline.
- [x] `tag submit --task-type research --execution kanban
      --prompt "..." --wait-seconds 0` → returns immediately
      with `status="queued"`.
- [x] `tag submit --task-type research --execution auto` →
      picks the route's `execution` default.
- [x] `tag submit --task-type research --execution bogus` →
      argparse rejects.
- [x] `tag submit --task-type research --prompt ""` → "Prompt
      cannot be empty."
- [x] `tag submit --task-type research --prompt "   "` (whitespace
      only) → "Prompt cannot be empty." (after `strip()`).
- [x] `tag submit --verify --execution direct` → runs verifier
      after workers and includes `result["verifier"]`.
- [x] `tag submit --task-type mixed --execution direct` → two
      worker steps in parallel; verify both rows in the DB.
- [x] **Concurrent submits**: launch 3 `tag submit` calls in
      parallel, confirm the SQLite DB doesn't corrupt (WAL mode
      must hold), and `tag runs` shows all 3.
- [x] **Long prompt** (10KB): slugify must truncate at 48 chars
      and the run_id must be unique.
- [x] **Unicode prompt** ("Reply with exactly: ✓"): must round-
      trip through SQLite and JSON cleanly.
- [x] **Newline in prompt**: the prompt is passed as a single CLI
      arg; verify with quotes.
- [x] **Kanban `--wait-seconds` timeout**: tasks that don't
      complete within the deadline must remain `status="queued"`,
      not crash.
- [x] **Kanban task `blocked` / `archived`** status must be
      treated as terminal (not pending).
- [x] **Hermes failure** (hermes binary not executable): step
      `status="error"`, run `status="error"`.
- [x] **No Hermes binary at all** + `hermes_bin` patched out:
      must trigger the auto-bootstrap path in `ensure_hermes_ready`
      (verified in the existing unit test, but re-test the real
      path).
- [x] DB persistence: `tag runs` must show the new run with
      `kind="submit"`, `status` matching the result.
- [x] `--source manual` (default) appears in metadata.
- [x] `--source codex-cli` (custom value) appears in metadata.
- [x] `--title "My Run"` appears in metadata.
- [x] `tag submit --json` output is valid JSON and parseable.

### 4.9 `tag benchmark`
- [x] `tag benchmark --profile researcher` with no `--model-ref`
      uses the profile's primary model.
- [x] `tag benchmark --profile researcher --model-ref
      openrouter/foo/bar --model-ref openrouter/baz/qux` runs
      against both models and produces two `models` entries.
- [x] `tag benchmark --case exact-echo` runs only the exact-echo
      case.
- [x] `tag benchmark --case bogus` produces 0 cases → "No
      benchmark cases selected."
- [x] `tag benchmark --case exact-echo --case math-json` runs
      both.
- [x] `tag benchmark --suite /nonexistent/path.yaml` → clear
      error from `benchmark_suite_path`.
- [x] `tag benchmark --suite /tmp/bad.yaml` (non-dict root) →
      "Config at … must be a YAML object."
- [x] `tag benchmark --suite /tmp/no-cases.yaml` (no `cases`
      key) → empty list runs → "No benchmark cases selected."
- [x] **Temp profile collision**: run twice with the same
      `--model-ref`; second run should NOT recreate the
      temp profile (existing path is reused).
- [x] **Temp profile gets benchmark-isolated config**: confirm
      the temp `bench-...` profile is created, runs, and
      doesn't pollute the base profile.
- [x] **Output normalization**: an output containing
      "session_id: 123" and a "tirith security scanner
      enabled but not available" line must still pass the
      `expected_exact: bench-ok` case after `normalize_chat_output`.
- [x] **Fenced JSON**: an output of
      ```` ```json\n{"status":"ok","sum":42}\n``` ````
      must pass the `expected_json` case.
- [x] **JSON with extra fields**: output
      `{"status":"ok","sum":42,"extra":"x"}` must still pass
      (only the documented keys are checked).
- [x] **Regex case**: output `- alpha\n- beta` must pass.
- [x] **Regex case with leading whitespace**: confirm whether
      `re.MULTILINE` + `^` requires the line to start at column 0.
- [x] DB persistence: each case becomes a `step` row with
      `role="benchmark"`, `extra.case_id` and `extra.reason`.
- [x] Overall `status="ok"` only if every model and every case
      passed.

### 4.10 `tag runs`
- [x] Default `--limit 20` shows the most recent 20.
- [x] `--limit 0` is rejected by `positive_int`.
- [x] `--limit 1` shows exactly one row.
- [x] `--json` → valid JSON list of dicts.
- [x] Empty DB → empty list, exit 0.

### 4.11 `tag openrouter-models`
- [x] Requires `OPENROUTER_API_KEY` in the profile's `.env`.
      Without it: "OPENROUTER_API_KEY is not set for profile
      'researcher'."
- [x] With a valid key: lists models from
      `https://openrouter.ai/api/v1/models`.
- [x] `--search gemini` filters to gemini-related models.
- [x] `--search ""` (empty) matches all (substring "" is in
      everything) — confirm and document.
- [x] `--sort id` (default): alphabetical by `id`.
- [x] `--sort prompt`: ascending by `pricing.prompt`.
- [x] `--sort completion`: ascending by `pricing.completion`.
- [x] `--sort context`: descending by `context_length`.
- [x] `--sort bogus` → argparse rejects.
- [x] `--limit 0` → no rows printed.
- [x] `--limit 5` → at most 5 rows.
- [x] `--ids-only` → prints `openrouter/<id>` lines, no pricing.
- [x] `--json` → valid JSON.
- [x] **HTTP 401**: stub the URL with a local server returning
      401; verify a clear error, not a stack trace.
- [x] **HTTP 500**: same.
- [x] **Network timeout** (>30s): error.
- [x] **Malformed JSON response**: error, not crash.
- [x] **Pricing field missing or non-numeric**: must not crash;
      `prompt_cost` returns 0.0.

### 4.12 `tag import-codex`
- [x] `--profile orchestrator --codex-home ~/.codex` imports
      tokens.
- [x] `--profile nonesuch` → "Unknown profile 'nonesuch'".
- [x] `--profile orchestrator` when profile home is missing →
      "Profile home does not exist for 'orchestrator'. Run
      bootstrap first."
- [x] `--codex-home /nonexistent` → "No importable Codex CLI
      tokens found." (or similar).
- [x] `--json` → valid JSON with `status: imported`.
- [x] Override `TAG_IMPORT_CODEX_HOME` is respected by
      `cmd_import_codex` when `--codex-home` is omitted.

## 5. Hermes Wrapper E2E (all 19 wrappers)

For every one of these:
`chat`, `gateway`, `kanban`, `model`, `profile`, `status`,
`config`, `sessions`, `skills`, `plugins`, `tools`, `mcp`,
`logs`, `dashboard`, `memory`, `completion`, `prompt-size`,
`update`, `tui`:

- [x] `tag <name> --help` returns 0 and shows Hermes help.
- [x] `tag <name> --profile orchestrator -- --help` returns 0.
- [x] `tag <name> --profile nonesuch -- --help` → Hermes runs
      with an empty profile home (verify behavior; should at
      least not crash TAG).
- [x] Missing `hermes_bin` triggers `ensure_hermes_ready` and
      auto-bootstraps (skip TUI for non-TUI wrappers).
- [x] `tag <name> --profile orchestrator` (no positional args)
      does not hang; verify it terminates within 5s for
      non-interactive wrappers.

### 5.1 TUI wrapper deep dive (`tag tui`, `tag`)
- [x] In a TTY (`script -q /dev/null tag tui`): launches Ink UI.
      Press `q` / `Ctrl-C` and verify clean exit (exit code 0
      or 130, not a traceback).
- [x] Status bar shows the active profile name in `[profile]`
      chrome (per `appChrome.tsx` patch).
- [x] Status bar palette uses the `tag-control` skin colors
      (`status_bar_bg`, `status_bar_text`, etc.).
- [x] Profile names like `default` / `custom` are hidden in the
      status bar (per `visibleProfileName` filter).
- [x] Profile names > 16 chars are truncated to 15 + `…`.
- [x] `banner_hero` art renders.
- [x] `prompt_symbol` from the skin (`›`) is used instead of the
      Hermes default.
- [x] **Non-TTY guard**: `tag tui` without a TTY exits 2 with
      the documented message.
- [x] **`TAG_FORCE_TUI=1`**: bypasses the guard.
- [x] **Bare `tag` in TTY**: launches the TUI in `orchestrator`
      profile.
- [x] **Bare `tag` non-TTY**: prints the "non-interactive shell"
      message, exits 2.
- [x] **TUI crash recovery**: send a malformed stdin to the TUI
      (e.g. `tag tui < /dev/urandom`) — must exit, not OOM.
- [x] **Ink + status bar perf**: rapid resize (resize the
      terminal 100x in 1s) — must not leak memory or freeze.
- [x] **TUI build with missing `node_modules`**: must report a
      clear error from `install_tui_dependencies`.

## 6. `tag hermes -- ...` passthrough
- [x] `tag hermes -- --version` → Hermes version.
- [x] `tag hermes --version` (no `--`) → Hermes version
      (the `REMAINDER` branch strips `--` only if present).
- [x] `tag hermes --` (empty) → Hermes prints its own help
      and exits 0.
- [x] `tag hermes -- bogus-subcommand` → Hermes error,
      propagated exit code.
- [x] `tag hermes -- auth list`, `-- fallback list`,
      `-- security`, `-- backup create`, `-- webhook list`,
      `-- portal info` — at minimum, each should not crash
      TAG. Confirm exit codes.
- [x] `--profile orchestrator` injects the orchestrator env.
- [x] `--tui` arg triggers `ensure_hermes_ready(need_tui=True)`.
- [x] `tag hermes -- --tui` (with explicit `--`) — verify the
      arg-stripping branch in `cmd_hermes_passthrough`.

## 7. SQLite Runtime
- [x] Schema is created on first `open_db` call.
- [x] `runs` columns: `id, created_at, kind, task_type,
      execution, master_profile, board, prompt, route_json,
      status, metadata_json`.
- [x] `steps` columns: `id, run_id, role, profile, model_ref,
      prompt, output, status, started_at, finished_at,
      duration_ms, extra_json`.
- [x] Foreign key: deleting a `run` cascades to `steps` (or
      prevents deletion — verify the `FOREIGN KEY` is
      actually enforced because `PRAGMA foreign_keys = ON`).
- [x] WAL files (`-wal`, `-shm`) appear after writes.
- [x] **Concurrent writers**: open two `tag submit` in
      parallel; both succeed; no "database is locked" errors.
- [x] **Huge `output` field** (e.g. 1MB chat output): must
      not break SQLite.
- [x] **Special characters in prompt**: quotes, nulls (use
      `\\0` escaped), newlines, tabs, emoji.
- [x] **`tag runs` after `tag submit`**: row present with
      correct `status` and `master_profile`.
- [x] **`tag runs` filter by `kind`**: not supported in CLI
      — document this gap.
- [x] **DB file path override**: `cfg.runtime.db_path` in
      config — verify it actually changes the path used.

## 8. Environment & Path Resolution
- [x] `TAG_HOME=/custom/path` → all state goes there.
- [x] `TAG_HOME=~/with-tilde` → expands.
- [x] `TAG_HOME=relative/path` → resolved against CWD
      (because of `.resolve()`).
- [x] `TAG_HERMES_ROOT=/elsewhere` → overrides config and
      discovery.
- [x] `TAG_HERMES_HOME`, `TAG_CODEX_HOME`, `TAG_REAL_HOME`,
      `TAG_PASSTHROUGH_HOME_PROFILES`, `TAG_HERMES_REPO`,
      `TAG_HERMES_REF`, `TAG_FORCE_TUI`, `TAG_NPM_RUNTIME_HOME`,
      `TAG_NPM_FORCE_REINSTALL` — each documented and tested.
- [x] `HOME` is rewritten by `hermes_env` to `runtime_home`
      (unless profile is in passthrough list).
- [x] `PATH` is prefixed with `<hermes_root>/.venv/bin`.

## 9. Cross-platform Matrix
- [x] Re-run the critical paths (sections 3, 4.8, 4.9, 5.1)
      on `python:3.11-slim` Docker.
- [x] Re-run on `python:3.12-slim` and `python:3.13-slim`.
- [x] Trace the Windows branch of `bin/tag.js` (read-only).
- [x] Test with a `TAG_HOME` path > 200 chars.
- [x] Test with a `TAG_HOME` path that contains a space.

## 10. Negative / Fuzz Tests
- [x] `tag submit --prompt` containing only `\\n\\n\\n` →
      "Prompt cannot be empty."
- [x] `tag set-model --ref` containing control chars →
      argparse or `parse_model_ref` must reject.
- [x] `tag.yaml` with circular references or `&alias`
      must not cause infinite recursion in `yaml.safe_load`.
- [x] Profile name with `/` (which would break
      `parse_model_ref` if used as a worker override).
- [x] Profile name with `=` (same reason).
- [x] `benchmark-suite.yaml` with 1000 cases — confirm
      performance is acceptable and DB inserts don't fail.
- [x] Submit 50 runs in a tight loop — verify no resource
      leak (file descriptors, threads).

## 11. TUI Patch Behavior (Hermes side)
After applying the patch and building:
- [x] `npm test` (Vitest) passes — including the new
      `maps status bar palette from skins` test added by
      the patch.
- [x] The new test in `theme.test.ts` (lines added by
      patch hunk 1) compiles and runs.
- [x] The skin YAML loads with `agent_icon: "◈"` and the
      UI displays the icon in the status bar.
- [x] `prompt_symbol: "›"` is trimmed to a single line and
      rendered (the patch leaves the
      `cleanPromptSymbol` invocation in place).
- [x] A non-default profile (`researcher`) shows
      `│ researcher` in the status bar when active.
- [x] The `banner_hero` ASCII art renders within the banner
      width (test with `cols=40` and `cols=120`).
- [x] No regressions in the existing Hermes TUI tests
      (the patch only adds, never modifies, lines outside
      the `+` hunks).

## 12. Reporting Format

Produce a single `e2e-report.md` with these sections:

TAG E2E Test Report
Date:
Host: (uname -a)
Python: (python --version)
Node: (node --version)
TAG_HOME used:
Hermes commit (from bundled tarball): (git rev-parse HEAD
inside the extracted tree)
Summary
Total tests executed: N
Passed: N
Failed: N
Skipped: N
Bugs filed: N (with severity: blocker/major/minor)
Findings
For each finding:
F-001: <title>
Severity: blocker | major | minor
Component: e.g. src/tag/controller.py:cmd_submit
Repro: <exact commands to reproduce>
Expected: <what should happen>
Actual: <what happens>
Suggested fix: <patch or pointer to doc>
Citations: file:line for every claim
Coverage Matrix
Command
tag setup
tag doctor
...
TUI Coverage
Patch hunk
theme.test.ts
appChrome.tsx
...
Open Questions / Risks
...

## 13. Definition of Done

The pass is complete only when:

1. Every command in `tag --help` has been invoked at least
   once with `--json` (where supported) and the JSON is
   valid.
2. Every `--flag` of every command has been tested with
   at least one valid and one invalid value.
3. Every env var documented in `README.md` and the source
   has been tested.
4. The Hermes TUI has been patched, built, and visually
   inspected.
5. The `bin/tag.js` launcher has been exercised end-to-end
   (install via npm pack → `node bin/tag.js --version` →
   `node bin/tag.js setup` → `node bin/tag.js submit ...`).
6. `e2e-report.md` exists with the format above and every
   finding has a `file:line` citation.
7. All blocker and major findings have a recommended fix
   (a diff or a TODO) — do not silently leave them open.
8. The existing 16 unit tests still pass.
9. The new test artifacts under `e2e-report/` are committed
   (or referenced) for reproducibility.

Start by creating `e2e-report/` and a `e2e-report/00-inventory.md`
that lists every command, every flag, every env var, and
every config key you plan to exercise. Then execute
section by section.

