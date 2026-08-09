# TAG Application Inventory — 2026-08-03

Generated from the code, not from the docs. **The HTTP table was corrected after audit** — the first version guessed the route-to-command mapping from a grep instead of running the servers, and got it wrong. Regenerate with `docs/qa/inventory.sh`.

## Surface summary

| surface | count |
|---|---|
| Go top-level commands | 91 |
| Go invocable command paths | 232 |
| Python commands | 104 |
| HTTP endpoints | 18 |
| Go packages | 39 |
| Python modules | 45 |

## HTTP surfaces

| endpoint | server | command |
|---|---|---|
| \`/\` | web dashboard | \`tag serve\` |
| \`/\`, \`/api/snapshot\`, \`/events\` (SSE) | dashboard | \`tag serve\` (:7880) |
| \`/\`, \`/health\`, \`/api/snapshot\`, \`/api/runs\`, \`/api/queue\`, \`/api/costs\`, \`/api/spans/<run_id>\`, \`/api/stream\` (SSE) | dashboard | \`tag web\` (:8787) |
| \`/\`, \`/health\`, \`/api/snapshot\`, \`/api/stats\`, \`/api/spans\`, \`/api/eval_runs\`, \`/api/judge_runs\`, \`/api/memories\`, \`/api/alerts\` (no SSE) | dashboard | \`tag devui\` (:7777) |
| \`/v1/chat/completions\`, \`/v1/models\` | OpenAI-compatible gateway | \`tag gateway\` |
| \`/webhook/<platform>\`, \`/webhooks/rules\`, \`/health\` | webhook receiver | \`tag webhook listen\` |

## Go command paths

```
agentic-ci ci-diagnose
fix-vuln
flaky-fix
review
test-gen
agentops
alert check
create
delete
firings
list
annotate add
export
label
next
skip
stats
assignments
benchmark list
run
show
bootstrap
budget check
get
list
remove
set
cache stats
tips
trend
ci diagnose
compare list
show
completion bash
fish
powershell
zsh
context compress
show
trim
costs
cron add
daemon
disable
enable
list
next
remove
run
dag list
run
save
show
state
devui
diff-context
doc check
read
doctor
env
eval list
run
show
eval-ci run
scaffold
eval-dataset add-case
create
delete
export
list
eval-judge list
run
show
gateway
graph build
query
show
help
hooks list
log
test
import-aider
import-aws
import-claude
import-codex
import-continue
import-copilot
import-cursor
import-daytona
import-docker
import-gemini
import-honcho
import-mistral
import-modal
import-opencode
import-ssh
import-zed
issue-solve
logs
loop abort
approve
deny
list
start
status
lsp start
status
marketplace list
pull
push
mcp-connect
mcp-registry add-curated
disable
enable
install
list
mcp-serve
mem add
forget
list
search
stats
mem2 config
episode
extract
fact
gc
store
tier
memory-journal clear
forget
list
save
models
notify add
disable
enable
list
remove
test
otel-export
permissions approve
audit
deny
log
pending
show
persona apply
delete
install
list
preview
remove
show
stack
plugin disable
enable
install
list
pricing get
list
show
prompt diff
get
list
save
versions
prompt-size
queue add
cancel
clear
list
result
worker
review-pr
route
route-fallback add
list
remove
resolve
run
runs list
show
sandbox firewall
run
security list
scan
serve
set-model
setup
shell
split list
plan
show
stats
swarm abort
list
results
run
status
swe-solve
template export
fetch
import
tool-index index
search
status
trace checkpoint
diff
export
list
replay
show
snapshot
tripwire check
history
list
test
tui
version
web
webhook events
listen
rule-add
rule-list
workflow interrupt
list
resume
workspace clear
index
map
status
```

## Python commands

```
agentic-ci agentops alert annotate assignments benchmark bootstrap budget cache chat ci compare 
completion config context costs cron dag dashboard desktop devui diff-context doc doctor env eval 
eval-ci eval-dataset eval-judge gateway graph hooks import-aider import-aws import-claude 
import-codex import-continue import-copilot import-cursor import-daytona import-docker 
import-gemini import-honcho import-mistral import-modal import-nous-portal import-opencode 
import-ssh import-supermemory import-zed issue-solve kanban logs loop lsp marketplace mcp 
mcp-registry mem mem2 memory memory-journal model models notify openrouter-models otel-export 
persona plugin plugins pricing profile prompt prompt-size queue queue-dep render review-pr route 
route-fallback runs runtime sandbox security serve sessions set-model setup shell skills split 
status submit swarm swe-solve template tool-index tools trace tui update web webhook workspace 
```
