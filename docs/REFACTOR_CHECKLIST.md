# TAG CLI — Complete Feature & Refactor Checklist

## How to use this checklist
After refactor, run each item. ✅ = verified working | ❌ = broken (needs fix)

---

## 1. Core Modules (Python APIs — verified by unit tests)

### 1.1 Cost Table (`tag.cost_table`)
- [ ] `compute_cost(model, input_tokens, output_tokens)` returns float USD
- [ ] `reload_pricing_table()` loads from assets/pricing.yaml (35+ models)
- [ ] `list_all_models()` returns all ModelPrice entries
- [ ] Fallback to built-in defaults when YAML missing
- [ ] YAML list format parsed correctly (not just dict format)

### 1.2 Tracing (`tag.tracing`)
- [ ] `Span` dataclass with all fields
- [ ] `open_span()` / `close_span()` / `open_tool_span()`
- [ ] `TraceProcessor` Protocol implemented
- [ ] `ProcessorChain` fan-out to multiple processors
- [ ] `save_spans_to_db()` persists to SQLite
- [ ] `render_trace_terminal()` renders ASCII tree
- [ ] `migrate_spans_table()` idempotent schema migration

### 1.3 Eval Judge (`tag.eval_judge`)
- [ ] `ensure_schema(conn)` creates tables
- [ ] `run_judge_on_eval()` calls LLM judge
- [ ] `list_judge_runs()` queries DB
- [ ] `get_judge_results()` returns results
- [ ] `_parse_judge_response()` parses score from text
- [ ] `JudgeScore` dataclass

### 1.4 Eval Datasets (`tag.eval_datasets`)
- [ ] `create_dataset(conn, name, description)` → Dataset
- [ ] `add_case(conn, dataset_id, ...)` → DatasetCase
- [ ] `list_datasets(conn)` → list
- [ ] `get_dataset(conn, name)` → Dataset | None
- [ ] `export_to_yaml(conn, dataset_id)` → YAML string
- [ ] `delete_dataset(conn, id)`

### 1.5 Alerts (`tag.alerts`)
- [ ] `create_rule(conn, name, metric, op, threshold, severity)` → AlertRule
- [ ] `list_rules(conn)` → list[AlertRule]
- [ ] `evaluate_rule(conn, rule)` → AlertFiring | None
- [ ] `AlertSeverity.WARNING / INFO / CRITICAL`
- [ ] `list_firings(conn)` → list[AlertFiring]
- [ ] `delete_rule(conn, id)`

### 1.6 Annotation Queue (`tag.annotation_queue`)
- [ ] `enqueue_task(conn, data, ...)` → AnnotationTask
- [ ] `next_task(conn, annotator)` → AnnotationTask | None
- [ ] `submit_label(conn, task_id, label, annotator)`
- [ ] `get_stats(conn)` → dict
- [ ] `export_labeled(conn)` → list

### 1.7 Prompt Hub (`tag.prompt_hub`)
- [ ] `save_prompt(conn, name, content)` → PromptVersion
- [ ] `get_prompt(conn, name)` → PromptVersion | None
- [ ] `list_prompts(conn)` → list
- [ ] `diff_versions(conn, name, v1, v2)` → str

### 1.8 DevUI (`tag.devui`)
- [ ] `DevUIServer` class starts SSE server
- [ ] `/events` SSE endpoint streams spans
- [ ] `push_event()` method
- [ ] Background thread mode

### 1.9 Issue Solver (`tag.issue_solver`)
- [ ] `IssueSolver` class
- [ ] `solve(issue_url, profile, cfg)` → SolveResult
- [ ] GitHub issue parsing

### 1.10 Webhook Server (`tag.webhook_server`)
- [ ] `WebhookServer` class with start/stop
- [ ] `verify_signature(platform, payload, sig, secret)` HMAC check
- [ ] `create_rule(conn, platform, event, profile, action)` → TriggerRule
- [ ] `list_rules(conn)` → list
- [ ] `match_rules(conn, platform, event_type, payload)` → list
- [ ] `parse_event(platform, payload)` → dict
- [ ] `list_events(conn)` → list
- [ ] GitHub, Linear, Slack, Generic platform support

### 1.11 CI Extensions (`tag.ci`)
- [ ] `build_diagnose_prompt(log)` → str
- [ ] `build_review_prompt(diff)` → str
- [ ] `read_ci_log(path)` → str
- [ ] `detect_git_host()` → str
- [ ] PRD-057 to PRD-063 CI extensions

### 1.12 SWE Harness (`tag.swe_harness`)
- [ ] `SWEHarness` class
- [ ] XML action tag parsing (bash, python, read_file, write_file)
- [ ] `run_task(task, profile, cfg)` → SolveResult

### 1.13 Memory Extractor (`tag.memory_extractor`)
- [ ] `extract_memories(text, profile, conn)` → list[Memory]
- [ ] Pattern-based extraction

### 1.14 Memory GC (`tag.memory_gc`)
- [ ] `run_gc(conn, max_age_days)` → int (items removed)
- [ ] Tiered memory support

### 1.15 Semantic Memory (`tag.semantic_memory`)
- [ ] BM25 + FTS5 hybrid search
- [ ] RRF fusion
- [ ] `add_memory(conn, content, ...)` → Memory
- [ ] `search(conn, query, mode)` → list[Memory]
- [ ] Temporal memory support
- [ ] Vector embedding support

### 1.16 Entity Graph (`tag.entity_graph`)
- [ ] `EntityGraph` class with union-find
- [ ] `add_entity(name, type)` / `add_relation(e1, rel, e2)`
- [ ] `get_community(entity)` → list
- [ ] `show_summary()` → str
- [ ] `build_from_memories(conn)` → EntityGraph

---

## 2. CLI Commands (smoke tests)

### 2.1 System Commands
- [ ] `tag setup --help`
- [ ] `tag doctor --help`
- [ ] `tag bootstrap --help`
- [ ] `tag render --help`
- [ ] `tag env --help`
- [ ] `tag update --help`
- [ ] `tag tui --help`
- [ ] `tag runtime --help`
- [ ] `tag completion --help`

### 2.2 Session Commands
- [ ] `tag chat --help`
- [ ] `tag gateway --help`
- [ ] `tag kanban --help`
- [ ] `tag model --help`
- [ ] `tag profile --help`
- [ ] `tag status --help`
- [ ] `tag config --help`
- [ ] `tag sessions --help`
- [ ] `tag skills --help`
- [ ] `tag plugins --help`
- [ ] `tag tools --help`
- [ ] `tag mcp --help`
- [ ] `tag logs --help`
- [ ] `tag dashboard --help`
- [ ] `tag desktop --help`

### 2.3 Routing & Submission
- [ ] `tag route --help`
- [ ] `tag assignments --help`
- [ ] `tag models --help`
- [ ] `tag set-model --help`
- [ ] `tag submit --help`
- [ ] `tag benchmark --help`
- [ ] `tag runs --help`
- [ ] `tag openrouter-models --help`

### 2.4 Import Commands
- [ ] `tag import-codex --help`
- [ ] `tag import-claude --help`
- [ ] `tag import-gemini --help`
- [ ] `tag import-continue --help`
- [ ] `tag import-mistral --help`
- [ ] `tag import-opencode --help`
- [ ] `tag import-zed --help`
- [ ] `tag import-copilot --help`
- [ ] `tag import-aider --help`
- [ ] `tag import-aws --help`
- [ ] `tag import-cursor --help`
- [ ] `tag import-supermemory --help`
- [ ] `tag import-honcho --help`
- [ ] `tag import-nous-portal --help`
- [ ] `tag import-docker --help`
- [ ] `tag import-ssh --help`
- [ ] `tag import-modal --help`
- [ ] `tag import-daytona --help`

### 2.5 Memory Commands
- [ ] `tag memory --help`
- [ ] `tag memory-journal --help`
- [ ] `tag mem --help`
- [ ] `tag mem list --help`
- [ ] `tag mem add --help`
- [ ] `tag mem search --help`
- [ ] `tag mem forget --help`
- [ ] `tag mem stats --help`
- [ ] `tag mem2 --help`
- [ ] `tag mem2 gc --help`
- [ ] `tag mem2 extract --help`
- [ ] `tag mem2 tier --help`
- [ ] `tag mem2 fact --help`
- [ ] `tag mem2 episode --help`
- [ ] `tag mem2 store --help`

### 2.6 Queue & DAG
- [ ] `tag queue --help`
- [ ] `tag queue-dep --help`
- [ ] `tag dag --help`

### 2.7 Swarm
- [ ] `tag swarm --help`

### 2.8 Observability
- [ ] `tag costs --help`
- [ ] `tag trace --help`
- [ ] `tag trace list --help`
- [ ] `tag trace show --help`
- [ ] `tag trace export --help`
- [ ] `tag cache --help`
- [ ] `tag cache stats --help`
- [ ] `tag cache trend --help`
- [ ] `tag cache tips --help`
- [ ] `tag otel-export --help`
- [ ] `tag agentops --help`

### 2.9 Workflow Management
- [ ] `tag hooks --help`
- [ ] `tag compare --help`
- [ ] `tag context --help`
- [ ] `tag shell --help`
- [ ] `tag route-fallback --help`
- [ ] `tag mcp-registry --help`
- [ ] `tag template --help`

### 2.10 CI/CD
- [ ] `tag ci --help`
- [ ] `tag review-pr --help`
- [ ] `tag loop --help`
- [ ] `tag cron --help`
- [ ] `tag workspace --help`
- [ ] `tag agentic-ci --help`

### 2.11 Marketplace & Eval Framework
- [ ] `tag marketplace --help`
- [ ] `tag eval --help`
- [ ] `tag sandbox --help`
- [ ] `tag serve --help`
- [ ] `tag web --help`
- [ ] `tag lsp --help`

### 2.12 Agent Tools
- [ ] `tag security --help`
- [ ] `tag persona --help`
- [ ] `tag diff-context --help`
- [ ] `tag budget --help`
- [ ] `tag notify --help`
- [ ] `tag split --help`
- [ ] `tag tool-index --help`

### 2.13 PRD Cluster A — Evaluation & Observability
- [ ] `tag pricing list` → lists 35+ models with costs
- [ ] `tag pricing get --model <model> --input-tokens 1000 --output-tokens 500`
- [ ] `tag eval-judge list --help`
- [ ] `tag eval-judge run --help`
- [ ] `tag eval-dataset list --help`
- [ ] `tag eval-dataset create --help`
- [ ] `tag eval-dataset export --help`
- [ ] `tag eval-ci run --help`
- [ ] `tag eval-ci scaffold --help`
- [ ] `tag alert list --help`
- [ ] `tag alert create --help`
- [ ] `tag alert check --help`
- [ ] `tag alert firings --help`
- [ ] `tag annotate next --help`
- [ ] `tag annotate label --help`
- [ ] `tag annotate stats --help`
- [ ] `tag prompt list --help`
- [ ] `tag prompt save --help`
- [ ] `tag prompt get --help`
- [ ] `tag devui --help`

### 2.14 PRD Cluster B — CI/CD & Agentic Dev Workflows
- [ ] `tag issue-solve --help`
- [ ] `tag webhook listen --help`
- [ ] `tag webhook rule-add --help`
- [ ] `tag webhook rule-list --help`
- [ ] `tag webhook events --help`
- [ ] `tag agentic-ci test-gen --help`
- [ ] `tag agentic-ci install-action --help`
- [ ] `tag agentic-ci fix-vuln --help`
- [ ] `tag agentic-ci ci-diagnose --help`
- [ ] `tag agentic-ci review --help`
- [ ] `tag agentic-ci gen-pipeline --help`
- [ ] `tag agentic-ci flaky-fix --help`
- [ ] `tag swe-solve --help`

### 2.15 PRD Cluster C — Memory & Knowledge
- [ ] `tag mem2 gc --help`
- [ ] `tag mem2 extract --help`
- [ ] `tag mem2 tier --help`
- [ ] `tag mem2 fact --help`
- [ ] `tag mem2 episode --help`
- [ ] `tag mem2 store --help`
- [ ] `tag graph show --help`
- [ ] `tag graph query --help`
- [ ] `tag graph build --help`

---

## 3. Architecture Verification

### 3.1 New Package Structure
- [ ] `src/tag/core/__init__.py` exists and re-exports all utilities
- [ ] `src/tag/core/config.py` has load_config, save_config, config_path
- [ ] `src/tag/core/paths.py` has all path utilities
- [ ] `src/tag/core/db.py` has open_db, schema migrations, journal/queue functions
- [ ] `src/tag/core/utils.py` has misc utilities
- [ ] `src/tag/core/profile.py` has render_profiles, bootstrap_profiles, resolve_route
- [ ] `src/tag/core/run.py` has run_hermes, run_profile_hermes, run_profile_python
- [ ] `src/tag/cmd/__init__.py` lists all command modules
- [ ] `src/tag/cmd/system.py` exists
- [ ] `src/tag/cmd/session.py` exists
- [ ] `src/tag/cmd/import_.py` exists
- [ ] `src/tag/cmd/routing.py` exists
- [ ] `src/tag/cmd/memory.py` exists
- [ ] `src/tag/cmd/queue_dag.py` exists
- [ ] `src/tag/cmd/swarm.py` exists
- [ ] `src/tag/cmd/observability.py` exists
- [ ] `src/tag/cmd/workflow_mgmt.py` exists
- [ ] `src/tag/cmd/ci_loop.py` exists
- [ ] `src/tag/cmd/marketplace.py` exists
- [ ] `src/tag/cmd/agent_tools.py` exists
- [ ] `src/tag/cmd/prd_clusters.py` exists
- [ ] `controller.py` reduced to ≤500 lines (thin dispatcher + re-exports)

### 3.2 Test Suite
- [ ] `python3 -m pytest tests/ -q --tb=short` → all 674+ tests pass
- [ ] `python3 -m pytest tests/test_controller.py -q` → all controller tests pass
- [ ] `python3 -m pytest tests/test_clusters_abc.py -q` → all 104 cluster tests pass
- [ ] No new test failures introduced

### 3.3 Import Integrity
- [ ] `python3 -c "import tag.controller; print('OK')"` works
- [ ] `python3 -c "from tag.core import load_config; print('OK')"` works
- [ ] `python3 -c "from tag.cmd import COMMAND_MODULES; print(len(COMMAND_MODULES))"` shows 13
- [ ] No circular import errors

### 3.4 CLI Entrypoint
- [ ] `python3 -m tag --help` shows all commands
- [ ] `python3 -m tag pricing list` works (calls PRD handler through new dispatcher)
- [ ] `python3 -m tag mem list` works
- [ ] `python3 -m tag eval-judge list` works (with tmp DB)
- [ ] `python3 -m tag alert list` works (with tmp DB)
- [ ] `python3 -m tag graph show` works (with tmp DB)
