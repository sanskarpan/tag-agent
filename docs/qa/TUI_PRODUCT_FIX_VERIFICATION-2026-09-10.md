# TUI/product fix verification — 2026-09-10

This is the post-remediation verification for the 13 findings in
`TUI_PRODUCT_QA-2026-09-08.md`. The fixes were reviewed and merged through
PRs #821–#825. GitHub issues #808–#820 are all closed.

## Merged remediation

| Area | PR | Result |
| --- | --- | --- |
| Native Go TUI profile scope, overflow, viewport behavior, and parity docs | #822 | Merged |
| Python dashboard profile isolation and honest terminal options | #821 | Merged |
| Pinned `ty`/Ruff gates and runtime contracts | #823 | Merged |
| Bundled patch verification, managed TUI slash/queue/personality fixes, self-contained tests | #824 | Merged |
| Composer-level raw Ctrl+D/Ctrl+L pass-through | #825 | Merged |

## Evidence

- `go test ./...` in `tag-go`: all packages passed, including the native TUI,
  server, sandbox, and CLI integration packages.
- Project-pinned checks in a clean PR worktree: `ty check src/tag` and
  `ruff check src/tag` both report no diagnostics.
- Python regression contracts: 21 tests passed across patch verification,
  dashboard contracts, and pinned runtime contracts.
- Managed TUI: TypeScript type-check passed; focused slash, branding,
  gateway, input-pass-through, cancellation, and fixture tests passed (137
  tests in the final focused run).
- Packaged runtime patch verification: all 7 tests passed. The shipped bytes
  pass forced reverse verification, while partial, missing, and unapplied
  hunk fixtures are rejected without file mutation.
- Fresh disposable PTY replay: the TAG banner renders, raw Ctrl+L leaves the
  composer unchanged, and raw Ctrl+D exits with code 0.

The broad managed Vitest run reached 997 passing tests and 4 skipped tests;
one unrelated daemon-stdio timing assertion occasionally exceeded its fixed
500 ms threshold in this host (506–513 ms). It passes in isolation and was not
changed or hidden by the remediation.

## Scope note

This closes the 13 findings from the September TUI/product audit. It is not a
claim that every historical item in the older `issues.md` inventory, every
provider integration, or every terminal emulator combination is defect-free.
Those remain separate QA scope and should not be silently marked resolved.
