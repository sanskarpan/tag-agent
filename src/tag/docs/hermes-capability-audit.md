# Hermes Capability Audit

Audit date: 2026-06-04

This document compares the managed TAG command surface with the upstream Hermes
CLI that TAG wraps.

At audit time:

- Hermes exposed 53 top-level CLI commands
- TAG exposed 19 first-class Hermes wrappers
- all remaining Hermes commands were still reachable through `tag hermes -- ...`

## Summary

TAG does not reimplement Hermes. Instead, it provides:

1. TAG-native orchestration and packaging commands
2. first-class wrappers for the most important Hermes operator workflows
3. a raw passthrough escape hatch via `tag hermes -- ...`

That means Hermes feature coverage should be evaluated in three buckets:

- directly implemented in TAG
- directly wrapped by TAG
- available through managed passthrough

## TAG-native commands

These commands are owned by TAG itself:

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

These are the differentiators over Hermes:

- managed bootstrap and patching
- multi-profile routing policy
- OpenRouter catalog querying
- benchmark persistence
- direct and Kanban submit execution

## First-class Hermes wrappers

These Hermes capabilities are surfaced directly in TAG:

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

These cover the most common operator paths for a published CLI.

## Managed passthrough

Everything else remains reachable with:

```bash
tag hermes -- <command> ...
```

Examples:

- `tag hermes -- auth list`
- `tag hermes -- fallback list`
- `tag hermes -- security`
- `tag hermes -- backup create`
- `tag hermes -- webhook list`
- `tag hermes -- portal info`

The currently passthrough-only Hermes top-level commands are:

- `acp`
- `auth`
- `backup`
- `bundles`
- `checkpoints`
- `claw`
- `computer-use`
- `cron`
- `curator`
- `debug`
- `desktop`
- `doctor`
- `dump`
- `fallback`
- `gui`
- `hooks`
- `import`
- `insights`
- `login`
- `logout`
- `lsp`
- `migrate`
- `pairing`
- `portal`
- `postinstall`
- `proxy`
- `secrets`
- `security`
- `send`
- `setup`
- `slack`
- `uninstall`
- `version`
- `webhook`
- `whatsapp`

## Remaining intentional gaps

TAG intentionally does not rename or re-specify every Hermes subcommand.
Doing so would create unnecessary maintenance drag and slower Hermes parity.

The contract is:

- high-value workflows get first-class TAG wrappers
- TAG-specific orchestration is owned natively
- long-tail Hermes features stay available through passthrough

Lifecycle note:

- `tag update` is TAG-managed on bundled installs rather than blindly invoking
  `hermes update` against a non-git checkout
- on git-backed Hermes checkouts, `tag update` delegates to Hermes upstream

## Release readiness conclusion

For public release, the current model is acceptable because:

- critical Hermes operations are directly exposed
- the full Hermes surface is still reachable
- TAG-specific value is additive rather than replacing Hermes internals

The main publication risks are now branding, release automation, and install UX,
not missing access to Hermes runtime capabilities.
