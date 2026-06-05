# TAG

TAG is a standalone orchestration layer on top of Hermes. It packages the work
done in this workspace into one installable CLI with:

- a bundled Hermes source snapshot for first-run provisioning
- managed Hermes bootstrap
- shipped Hermes TUI patching
- a custom `tag-control` skin
- profile-based master/worker orchestration
- OpenRouter worker routing
- Codex import and runtime support
- direct execution, Kanban execution, and benchmark history

## Install

```bash
pip install tag-agent
```

This installs the `tag` command.

For npm:

```bash
npm install -g tag-agent
```

For pnpm:

```bash
pnpm add -g tag-agent
```

The npm package installs a thin Node launcher for `tag`. On first run it
creates an isolated Python runtime under `~/.tag/npm-runtime/<version>`,
installs the bundled TAG Python package there, and then executes the same TAG
CLI. That means the npm path still requires Python `>=3.11` and `<3.14` on
`PATH`. The launcher will probe `python3.13`, `python3.12`, `python3.11`,
`python3`, and `python` in that order on Unix-like systems.

For a build artifact install:

```bash
python -m build
pip install dist/tag_agent-0.1.0-py3-none-any.whl
```

For an npm artifact check:

```bash
npm pack
```

## First run

```bash
tag
```

Default behavior:

1. create `~/.tag/config/tag.yaml` and the benchmark suite if missing
2. extract the bundled Hermes snapshot into `~/.tag/managed/hermes-agent-upstream` if needed
3. create a virtualenv for Hermes
4. install Hermes with the required extras
5. apply the TAG TUI patch
6. install/build the Hermes TUI workspace
7. bootstrap the default TAG profiles
8. launch the orchestrator TUI

The same managed bootstrap now happens automatically for non-TUI commands that
need Hermes, for example:

```bash
tag submit --task-type mixed --execution direct --prompt "Reply with exactly: smoke-ok"
tag benchmark --profile researcher --model-ref openrouter/deepseek/deepseek-v4-flash
tag model --profile orchestrator -- list
```

For those commands, TAG provisions Hermes on demand and skips the TUI build if
it is not needed, so `npm` is not required just to use submit/benchmark/model
flows.

If `tag` is started from a non-interactive context, it does not try to open the
TUI blindly. It exits with a clear message and tells you to use `tag doctor`,
`tag setup`, or `tag tui` from a real terminal.

If you want the setup step explicitly:

```bash
tag setup
tag tui
```

## Main commands

```bash
tag
tag setup
tag doctor
tag bootstrap
tag status
tag assignments
tag models --profile researcher
tag openrouter-models --profile researcher --search gemini
tag set-model --profile reviewer --ref openrouter/deepseek/deepseek-v4-pro
tag submit --task-type mixed --execution direct --prompt "Reply with exactly: smoke-ok"
tag benchmark --profile researcher --model-ref openrouter/deepseek/deepseek-v4-flash
tag runs
tag import-codex --profile orchestrator --codex-home ~/.codex
tag chat --profile orchestrator -- --help
tag config --profile orchestrator -- edit
tag gateway --profile orchestrator -- start
tag kanban --profile orchestrator -- list
tag sessions --profile orchestrator -- list
tag skills --profile orchestrator -- list
tag plugins --profile orchestrator -- list
tag tools --profile orchestrator -- list
tag mcp --profile orchestrator -- list
tag logs --profile orchestrator -- --since 1h
tag dashboard --profile orchestrator -- --status
tag memory --profile orchestrator -- status
tag model --profile orchestrator -- list
tag profile -- list
tag completion --profile orchestrator -- zsh
tag prompt-size --profile orchestrator
tag update
tag hermes --profile orchestrator -- gateway start
tag tui --profile orchestrator
```

## Persistence

TAG stores its managed state under `~/.tag` by default:

- `config/tag.yaml`
- `config/benchmark-suite.yaml`
- `managed/hermes-agent-upstream`
- `runtime/home`
- `runtime/tag.sqlite3`

Override the root with:

```bash
export TAG_HOME=/some/other/location
```

## Notes

- The managed Hermes checkout remains Hermes internally; TAG is the packaged
  experience and command surface around it.
- TAG does not require a preinstalled Hermes checkout. By default it provisions
  Hermes from a bundled source snapshot and only falls back to `git clone` if
  that snapshot is unavailable.
- If TAG can already discover a valid local Hermes source checkout, it will
  reuse it automatically instead of forcing a second separate install.
- If a real Codex CLI home already exists with `auth.json`, TAG imports that
  into the managed orchestrator profiles automatically during setup.
- The shipped patch only touches Hermes TUI skin handling and profile-aware
  chrome.
- OpenRouter keys and Codex auth remain per-profile concerns.
- TAG currently targets Python `>=3.11` and `<3.14` and expects `npm` for full first-run
  bootstrap because Hermes' TUI workspace is built locally. `git` is still
  recommended for Hermes features like worktrees and for refresh/update flows,
  but it is no longer required for the default bundled install path.
- `tag update` is lifecycle-aware:
  - on a bundled Hermes checkout, it refreshes the managed runtime from the
    packaged snapshot and rebuilds it
  - on a git-backed checkout, it delegates to Hermes' own update flow
- The npm distribution is a launcher wrapper around the Python package, not a
  separate Node reimplementation.

## Command strategy

TAG currently has three layers of command surface:

1. Native TAG orchestration commands
   - `setup`, `doctor`, `bootstrap`, `route`, `submit`, `benchmark`, `runs`
2. High-value managed Hermes wrappers
   - `chat`, `gateway`, `kanban`, `model`, `profile`, `status`, `config`, `sessions`, `skills`, `plugins`, `tools`, `mcp`, `logs`, `dashboard`, `memory`, `completion`, `prompt-size`, `update`, `tui`
3. Full escape hatch
   - `tag hermes -- ...`

This means TAG does not reimplement all of Hermes. Instead, it owns the
installation, profile layout, patching, orchestration, and default UX, while
still letting you reach the underlying Hermes runtime when needed.

## Release Checks

Before publishing, run:

```bash
pytest -q tests/test_controller.py
python -m build
npm pack
tag doctor
tag setup --json
```
