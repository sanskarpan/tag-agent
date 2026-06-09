# TAG E2E Inventory

Date: 2026-06-06
Repo root: `/Users/sanskar/dev/test/tag`
Default isolated home pattern: `TAG_HOME="$(mktemp -d)/tag-home"`

## Commands Under Test

Root:
- `tag`
- `tag --help`
- `tag --version`

Native TAG commands:
- `setup`
- `doctor`
- `bootstrap`
- `render`
- `route`
- `env`
- `assignments`
- `models`
- `set-model`
- `submit`
- `benchmark`
- `runs`
- `openrouter-models`
- `import-codex`

Hermes passthrough and wrappers:
- `hermes`
- `chat`
- `gateway`
- `kanban`
- `model`
- `profile`
- `status`
- `config`
- `sessions`
- `skills`
- `plugins`
- `tools`
- `mcp`
- `logs`
- `dashboard`
- `memory`
- `completion`
- `prompt-size`
- `update`
- `tui`

## Flags Under Test

Global:
- `--config`
- `--version`

`setup`:
- `--refresh`
- `--skip-python-install`
- `--skip-tui-build`
- `--json`

`doctor`:
- `--json`

`bootstrap`:
- `--force`
- `--json`

`render`:
- `--force`
- `--json`

`route`:
- `--task-type`
- `--master-profile`
- `--worker-profile`
- `--master-model`
- `--verifier-model`
- `--worker-model-override`
- `--json`

`assignments`:
- `--json`

`models`:
- `--profile`
- `--provider`
- `--limit`
- `--json`

`set-model`:
- `--profile`
- `--ref`
- `--target`
- `--openai-runtime`
- `--json`

`submit`:
- `--task-type`
- `--prompt`
- `--title`
- `--source`
- `--execution`
- `--master-profile`
- `--worker-profile`
- `--master-model`
- `--verifier-model`
- `--worker-model-override`
- `--verify`
- `--wait-seconds`
- `--json`

`benchmark`:
- `--profile`
- `--suite`
- `--model-ref`
- `--case`
- `--json`

`runs`:
- `--limit`
- `--json`

`openrouter-models`:
- `--profile`
- `--search`
- `--sort`
- `--limit`
- `--ids-only`
- `--json`

`import-codex`:
- `--profile`
- `--codex-home`
- `--json`

Hermes wrappers:
- `--profile`
- `hermes_args` passthrough remainder
- `update --json`

## Environment Variables Under Test

- `TAG_HOME`
- `TAG_HERMES_ROOT`
- `TAG_CODEX_HOME`
- `TAG_HERMES_REPO`
- `TAG_HERMES_REF`
- `TAG_HERMES_HOME`
- `TAG_PASSTHROUGH_HOME_PROFILES`
- `TAG_REAL_HOME`
- `TAG_IMPORT_CODEX_HOME`
- `TAG_FORCE_TUI`
- `TAG_NPM_RUNTIME_HOME`
- `TAG_NPM_FORCE_REINSTALL`

Profile env / external keys:
- `OPENROUTER_API_KEY`

## Config Keys Under Test

Top-level:
- `lab_name`
- `upstream`
- `runtime`
- `skins`
- `defaults`
- `env_examples`
- `profiles`
- `routing`

Nested:
- `upstream.repo`
- `upstream.ref`
- `upstream.checkout_dir`
- `runtime.home_dir`
- `runtime.codex_home`
- `runtime.db_path`
- `skins.tag-control.source`
- `defaults.master_profile`
- `defaults.board`
- `env_examples.shared`
- `env_examples.profiles`
- `profiles.<name>.description`
- `profiles.<name>.tags`
- `profiles.<name>.config.display.skin`
- `profiles.<name>.config.display.tui_statusbar`
- `profiles.<name>.config.display.tui_status_indicator`
- `profiles.<name>.config.model.provider`
- `profiles.<name>.config.model.default`
- `profiles.<name>.config.model.openai_runtime`
- `profiles.<name>.config.delegation.provider`
- `profiles.<name>.config.delegation.model`
- `profiles.<name>.config.delegation.max_concurrent_children`
- `profiles.<name>.config.delegation.max_spawn_depth`
- `profiles.<name>.config.delegation.orchestrator_enabled`
- `profiles.<name>.config.kanban.default_assignee`
- `profiles.<name>.config.kanban.dispatch_in_gateway`
- `profiles.<name>.config.kanban.dispatch_interval_seconds`
- `profiles.<name>.config.kanban.max_in_progress_per_profile`
- `routing.task_types.<task>.workers`
- `routing.task_types.<task>.verifier`
- `routing.task_types.<task>.execution`

## Files Under Direct Audit

- `README.md`
- `TODO.md`
- `MANIFEST.in`
- `pyproject.toml`
- `package.json`
- `src/tag/__init__.py`
- `src/tag/__main__.py`
- `src/tag/cli.py`
- `src/tag/controller.py`
- `src/tag/config/default.yaml`
- `src/tag/config/benchmark-suite.yaml`
- `src/tag/assets/skins/tag-control.yaml`
- `src/tag/patches/hermes-ui.patch`
- `src/tag/docs/gap-analysis.md`
- `src/tag/docs/hermes-capability-audit.md`
- `bin/tag.js`
- `tests/test_controller.py`
- `.github/workflows/ci.yml`
- `.github/workflows/release.yml`
- `.gitignore`
- `.npmignore`

## Planned Execution Groups

1. Static audit: parser surface, env/path handling, subprocess sites, JSON/YAML loads, tar extraction, update/TUI guards.
2. Packaging and install: `pip install -e .[dev]`, `python -m build`, wheel inspection, `npm pack`, launcher smoke.
3. Setup/doctor lifecycle: cold setup, skipped setup variants, doctor before/after, non-interactive root command path.
4. Native command E2E: `bootstrap`, `render`, `route`, `env`, `assignments`, `models`, `set-model`, `runs`.
5. Stateful runtime E2E: `submit`, `benchmark`, SQLite persistence, concurrency, Codex import paths.
6. Wrapper and TUI E2E: `hermes`, Hermes wrappers, TTY/non-TTY TUI paths, patched Hermes TUI build and tests.
7. Negative paths: malformed config, missing tools, invalid refs, invalid limits, invalid prompts, missing API keys.
