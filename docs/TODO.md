# TAG Todo

This file tracks the remaining work needed before TAG can reasonably be
described as a polished standalone product instead of a strong prototype.

## In Progress

- [ ] None

## Pending

- [ ] None

## Completed

- [x] Harden packaging and distribution
  - add package metadata suitable for publication
  - include shipped resources and license in sdist/wheel builds
  - verify non-editable wheel install behavior
  - clean generated artifacts from the source tree

- [x] Re-run regression testing across all maintained surfaces
  - TAG unit tests
  - hermes-lab unit tests
  - Hermes patched TUI tests/build
  - installed `tag` smoke tests

- [x] Harden bare `tag` startup behavior
  - add TTY detection
  - add a non-interactive fallback mode
  - avoid hanging when launched from non-interactive contexts

- [x] Improve `doctor` and `setup` prerequisite validation
  - verify `git`, `npm`, and usable Python tooling up front
  - surface missing dependencies before setup begins
  - report whether Hermes checkout, patch state, and TUI build state are healthy

- [x] Clarify and strengthen TAG command-surface coverage
  - decide which Hermes capabilities should remain passthrough-only
  - document the intended contract of `tag hermes`
  - add any missing high-value first-class TAG commands

- [x] Standalone `tag` Python package skeleton
- [x] Managed Hermes bootstrap flow
- [x] TAG config and benchmark suite persistence
- [x] Shipped `tag-control` skin
- [x] Hermes TUI patch integration
- [x] Profile bootstrap and rendered config installation

