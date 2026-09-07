# Native dashboard fixes — 2026-09-08

Scope: GitHub #809, #810, #811, #812. Implemented on isolated branch
`qa/native-tui-fixes` from `99c0ff6`, without importing unrelated dirty changes.

The native TUI now validates configured profiles before entering alternate-screen
mode. Every displayed run, queue job and journal count is scoped to that profile.
The dashboard loads complete profile records, displays actual counts, wraps long
rows, and supports arrows/j/k, PageUp/PageDown, Home/End/g/G and Tab navigation.
Title, profile, quit/refresh instructions and navigation remain outside the
scrollable region. Below 20 columns or six rows it displays a resize instruction.
Database text is stripped of control characters before terminal rendering.

This remains a read-only dashboard: it does not acquire streaming-chat capability.
The feature matrix and migration inventory now explicitly distinguish it from the
managed Python streaming chat interface.

## Automated regression checks

`go test -race ./internal/tui ./internal/server` covers selected-profile filtering,
journal counts, complete snapshots exceeding both old caps (60 runs, 61 queue
jobs), traversal to every record and wrapped task, narrow/wide/tiny resize bounds,
Home/End/Tab, refresh after deletion and malicious stored terminal control bytes.
Existing HTTP snapshot behavior remains bounded and backward compatible.

`go test ./internal/cli -run TestE2ETUI -count=1` verifies nonexistent profiles fail
before attempting terminal setup.

## Direct PTY verification

Built `go build -o /tmp/tag-native-fixed-20260908 ./cmd/tag`; bootstrapped an
isolated `TAG_HOME=/tmp/tag-native-verify.F39btp`. Ran with `stty rows 12 cols 30`
and `tui --profile coder`. No model calls or real credentials were used.

Initially title, selected profile, Runs (0), Queue (0), journal count and footer
were all visible. Seeded 25 runs with alternating coder/orchestrator profiles
and 55 coder queue jobs into its SQLite database while the TUI was running.
Pressed `r`: displayed Runs (12), with only even-numbered coder runs. Pressed
`G`: reached qa-q-03, qa-q-02, qa-q-01 and the journal count. Pressed `g`, then
Tab: reached Queue (55), showing distinguishable wrapped task-55, task-54,
task-53 records. The title/profile remained fixed at the top throughout.
Pressed `q`: process exited successfully and restored the normal terminal.

Separately invoking `tui --profile does-not-exist` returned nonzero with
`unknown profile "does-not-exist"`, before entering terminal mode.

The temporary paths are evidence locations, not dependencies. Regression tests
recreate fixtures in fresh temporary directories. Live terminal resize dimensions
are verified by WindowSizeMsg model tests; the direct PTY run used 30×12.
