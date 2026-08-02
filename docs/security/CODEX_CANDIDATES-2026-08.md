# Codex Security — candidate findings (UNVALIDATED)

Produced by `@openai/codex-security` v0.1.5, medium effort, scoped to the 8 security-critical
Go packages. **The scan's validation phase never ran** (OpenAI account credits exhausted), so
every entry below is a *candidate*, not a confirmed vulnerability. Three were confirmed by hand;
see issue #608. Reproduce before acting on any of them.

| # | CWE | Location | Summary |
|---|---|---|---|
| 1 | CWE-20,CWE-693 | `tag-go/internal/permission/config.go:79` | Malformed permission rule fields can silently broaden an allow rule instead of failing configuration loading. |
| 2 | CWE-200 | `tag-go/internal/sandbox/sandbox.go:172` | The Linux restricted sandbox grants untrusted commands read access to the host's entire /proc tree without a PID namespace, exposing other same-UID processes' environment variables and command lines. |
| 3 | CWE-250,CWE-284 | `tag-go/internal/cli/sandbox.go:147` | A granular Docker egress policy accepts the user-selected `host` network and starts its firewall helper in that namespace with CAP_NET_ADMIN, allowing the helper to rewrite host routes and placing the supposedly confined workload on the host network. |
| 4 | CWE-294 | `tag-go/internal/webhook/webhook.go:339` | GitHub and Linear delivery IDs lose replay protection after cache eviction or server restart. |
| 5 | CWE-294 | `tag-go/internal/webhook/webhook.go:62` | Generic signed webhooks have no freshness or replay identifier and can be replayed indefinitely. |
| 6 | CWE-294 | `tag-go/internal/webhook/webhook.go:49` | A captured Slack webhook can be replayed repeatedly within the timestamp tolerance to enqueue duplicate jobs. |
| 7 | CWE-400 | `tag-go/internal/permission/audit.go:53` | Nested tool arguments bypass the audit summary's size bound and can cause oversized SQLite audit writes. |
| 8 | CWE-400 | `tag-go/internal/llm/openai.go:79` | A hostile or malfunctioning OpenAI-compatible SSE peer can grow tool-call accumulators without a total response, argument, or tool-count bound and exhaust process memory. |
| 9 | CWE-400 | `tag-go/internal/sandbox/docker.go:124` | A sandboxed Docker workload can exhaust host memory through unbounded output capture |
| 10 | CWE-400 | `tag-go/internal/tool/tools.go:199` | The bash tool buffers command output without a size limit |
| 11 | CWE-400 | `tag-go/internal/llm/anthropic.go:185` | Anthropic tool-call streaming accumulates unbounded partial JSON in memory |
| 12 | CWE-59,CWE-200 | `tag-go/internal/permission/gate.go:36` | read_file permission checks can be bypassed through an in-root symlink alias |
| 13 | CWE-59,CWE-200 | `tag-go/internal/permission/gate.go:36` | list_dir permission checks can be bypassed through an in-root symlink alias |
| 14 | CWE-59,CWE-863 | `tag-go/internal/permission/gate.go:36` | write_file path grants can be redirected to denied in-root files through symlink aliases |
| 15 | CWE-770 | `tag-go/internal/gateway/gateway.go:135` | The chat-completions gateway accepts an arbitrarily large positive max_tokens value and forwards it to the selected provider instead of enforcing the configured MaxTokens limit. |
