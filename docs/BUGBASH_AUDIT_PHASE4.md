# TAG CLI — Second Adversarial Bug-Bash (Phase 4)

Second pass focused on: verifying pass-3 fixes are correct/complete, hunting bugs introduced by the ~45-file change, and stressing under-covered angles (integration, concurrency, security bypasses, packaging). **51 confirmed bugs** (0 critical, 8 high, 16 medium, 27 low).

All fixed except C006 (packaging — a deployment decision, see below). Verified: 678 tests pass, 103-command --help sweep clean (0 tracebacks).

## HIGH (8)

- ☑ **C001** [regression] Global --config silently ignored by memory-journal and swarm subcommands (wrong config/profile used)
  - root_cause: src/tag/cmd/memory.py:532-534 and src/tag/cmd/swarm.py:677-679 — loop `if 'config' not in {...}: p.add_argument('--config', help=SUPPRESS)` adds a subparser-level --config with default None that argparse writes over the 
- ☑ **C002** [exit-code] tag doctor --json returns exit 0 when the managed runtime is missing/broken (text mode returns 1)
  - root_cause: src/tag/cmd/system.py:259-264 — has_fail iterates only profiles_report.values(); the JSON branch (217-264) never calls _doctor_system_checks/_doctor_hermes_checks, so runtime/patch/tui failures are absent from the payloa
- ☑ **C003** [crash] Cron matcher crashes on any comma-list field containing a range or step; validation accepts, daemon silently never fires the job
  - root_cause: src/tag/cron_scheduler.py:21-43 (_field_matches) tests '/' then '-' then ',' on the WHOLE field, so a mixed field like '1,2-4' takes the '-' branch and int()s '1,2'. Contrast _validate_cron_field (src/tag/cron_scheduler.
- ☑ **C004** [wrong-behavior] tag issue-solve invokes the agent with an invalid CLI form — feature never actually runs the model
  - root_cause: src/tag/issue_solver.py:231 — cmd=[tag_bin,'-q',prompt,'-p',profile] uses hermes-style flags the TAG wrapper does not expose; _find_tag_bin (190-196) resolves the tag/tag-agent wrapper. capture_output captures stdout onl
- ☑ **C005** [security] SSRF guards validate only the initial URL — urllib follows redirects to internal/metadata addresses (bypass, incomplete B025)
  - root_cause: src/tag/cmd/marketplace.py:202,209 (mirrored src/tag/cmd/workflow_mgmt.py:344,349 and src/tag/notifications.py:218/293) — urlopen uses the global opener whose HTTPRedirectHandler transparently follows redirects; validati
- ☐ DEFERRED (deployment decision) **C006** [data-integrity] 54MB vendor tarball baked into the wheel despite pyproject comment claiming exclusion
  - root_cause: pyproject.toml:262-273 declares package-data for assets/config/docs/patches but never prunes vendor/; no [tool.setuptools] block and no include_package_data=false, so setuptools' default include_package_data=True pulls t
- ☑ **C007** [security] Restricted sandbox on macOS does not isolate $HOME — reads arbitrary user files and writes outside the run dir
  - root_cause: src/tag/sandbox.py:88-99 — sandbox-exec profile is '(allow default)' with only a tiny system-path denylist (/etc,/var/db,master.passwd; /usr /bin /sbin /System /Library write); it never denies read of $HOME/user tree nor
- ☑ **C008** [wrong-behavior] Webhook matched-rule dispatch calls nonexistent queue_worker.enqueue — every trigger silently enqueues nothing
  - root_cause: src/tag/webhook_server.py:364-372 — the per-rule loop calls queue_worker.enqueue(conn,...), but queue_worker exposes no enqueue (only _utc_now/_open_db/_mark_*/_get_job/_run_job/_send_notification/main). The AttributeErr

## MEDIUM (16)

- ☑ **C009** [wrong-behavior] doctor false-positive: empty / whitespace-only API key value reported as 'found'
  - root_cause: src/tag/controller.py:1546-1550 — api_key_vars filters by key NAME (endswith _API_KEY/_TOKEN) only; never checks env_vals[k].strip() is non-empty, so blank/whitespace values pass as configured.
- ☑ **C010** [data-integrity] Concurrent tag bootstrap / setup crashes with fatal 'Profile already exists' (TOCTOU race)
  - root_cause: src/tag/core/profile.py:334-348 — home.exists() check then run_hermes('profile create'); the loser's CalledProcessError is re-raised as SystemExit at line 347 instead of being absorbed as idempotent.
- ☑ **C011** [incomplete-fix] Box-title re-centering (BUG-011 fix) is defeated for most titles by rule ordering
  - root_cause: src/tag/core/utils.py:246 vs 250 — _fix_box_title_alignment() runs BEFORE the catch-all `re.sub(r'(?<![/.])\bHermes\b(?![-/.])','TAG')` at line 250. Titles not among the three hard-coded pre-realign strings (Hermes Confi
- ☑ **C012** [wrong-behavior] Branding rewrite corrupts email addresses / handles of the form hermes@domain
  - root_cause: src/tag/core/utils.py:250 — the catch-all lookahead exclusion set `(?![-/.])` omits '@', so a brand-as-local-part 'hermes@...' is over-matched and rewritten.
- ☑ **C013** [security] Path traversal in tag loop approve|deny <loop_id> clobbers existing .json files outside loop-approvals
  - root_cause: src/tag/cmd/ci_loop.py:330 — approval_file = runtime_db_path(cfg).parent/'loop-approvals'/f'{loop_id}.json' built from unvalidated loop_id; write at line 342. The .exists() guard limits scope to existing *.json but still
- ☑ **C014** [validation] import-nous-portal accepts a whitespace-only API key of length >= 20 (B120 length-check has no strip)
  - root_cause: src/tag/cmd/import_.py:667-680 — `if effective_key is not None and len(effective_key) < 20: raise` with no .strip()/emptiness check, unlike the supermemory path at import_.py:551-556 which checks `not api_key.strip()`.
- ☑ **C015** [security] marketplace push does not validate profile_name — path traversal reads arbitrary config.yaml (SHA + path disclosure)
  - root_cause: src/tag/cmd/marketplace.py:256-277 — push omits the _validate_profile_name(name) guard that pull applies (:195); the raw name is joined into runtime_home/.hermes/profiles/<name>/config.yaml (:266) and read via _profile_s
- ☑ **C016** [security] tag serve dashboard SSE still sends Access-Control-Allow-Origin: * (incomplete B067 fix)
  - root_cause: src/tag/cmd/marketplace.py:567 — _serve_sse unconditionally sends Access-Control-Allow-Origin:* on the /events stream carrying _dashboard_snapshot (runs, queue jobs incl. task text, journal counts, kanban). B067 removed 
- ☑ **C017** [data-integrity] Racing set-model leaves rendered profile config.yaml stale (runtime reads wrong model) despite correct tag.yaml
  - root_cause: src/tag/cmd/routing.py:187-188 — render_profiles(cfg,force=True) runs OUTSIDE the fcntl lock with the caller's possibly-stale cfg snapshot. src/tag/core/profile.py:260,316-317 — with force=True the existing-file merge br
- ☑ **C018** [incomplete-fix] template import writes profile .env (containing API keys) world-readable 0644 — 0600 fix not applied to this path
  - root_cause: src/tag/cmd/workflow_mgmt.py:332 — env_file.write_text(...) with no os.chmod(0o600); the Phase-3 0600 hardening was added only to core/utils._upsert_env_line, not to the template-import writer.
- ☑ **C019** [wrong-behavior] Cron N/step (numeric base with step, e.g. 5/10) validates but the step is silently ignored at match time
  - root_cause: src/tag/cron_scheduler.py:34 — in the `if '/' in field` branch a numeric base falls through to `return value == int(base)`, ignoring step; _validate_cron_field (133-136) accepts the step, so the expression is accepted bu
- ☑ **C020** [data-integrity] get_entity_neighbors returns duplicate relation entries (once per visited endpoint)
  - root_cause: src/tag/entity_graph.py:418-423 — for each visited node it re-queries WHERE source_entity_id=? OR target_entity_id=? and unconditionally all_relations.append(dict(r)); there is no seen-relation-id set (visited only guard
- ☑ **C021** [data-integrity] graph build is not idempotent — duplicates all relations and inflates entity mention_count on every re-run
  - root_cause: src/tag/entity_graph.py:230-243 — add_relation does unconditional INSERT with a fresh uuid and no UNIQUE on (source_entity_id,target_entity_id,relation_type); src/tag/cmd/prd_clusters.py:~752-758 — graph build loops extr
- ☑ **C022** [data-integrity] eval-dataset YAML export silently drops a case's expected_output when it is an empty string
  - root_cause: src/tag/eval_datasets.py:201 — `if c['expected_output']:` is a truthiness test treating '' as absent; should be `if c['expected_output'] is not None:`.
- ☑ **C023** [incomplete-fix] LSP status reports crashed/hard-killed sessions as 'running' forever (incomplete B068 fix)
  - root_cause: src/tag/lsp_server.py:281-292 — get_lsp_status trusts stored status without verifying the recorded PID is alive (no os.kill/psutil); _mark_stopped (line 193) is the only writer and runs only in the graceful finally block
- ☑ **C024** [security] SWEHarness bash action is unrestricted — reads files outside working dir; network block trivially bypassed
  - root_cause: src/tag/swe_harness.py:129-146 — _exec_bash runs subprocess.run(shell=True) with no _is_safe_path containment (containment at :76 is only applied to view/edit/create) and only a two-token curl|wget regex (_EXTERNAL_CURL,

## LOW (27)

- ☑ **C025** [validation] persona apply/stack accept a nonexistent profile with no validation
  - root_cause: src/tag/persona.py apply_persona validates the persona name but never checks the profile against cfg['profiles']; cmd_persona passes the profile straight through.
- ☑ **C026** [consistency] split plan rejects --json though sibling subcommands accept it
  - root_cause: src/tag/cmd/agent_tools.py — the sp_plan subparser omits add_argument('--json', ...) that sp_list and sp_show register.
- ☑ **C027** [validation] alert create accepts an empty rule name
  - root_cause: src/tag/alerts.py:179-198 (create_rule) — no `if not name.strip()` guard; cmd_alert create in prd_clusters.py also does not validate.
- ☑ **C028** [wrong-behavior] npm launcher silently ignores --reinstall-runtime when combined with --version
  - root_cause: bin/tag.js:203-216 — the forwardedArgs.length===1 && --version fast path returns without checking FORCE_REINSTALL (set true at 19-21); ensureRuntime() (the only FORCE_REINSTALL consumer) is never reached.
- ☑ **C029** [dead-code] save_config() is dead code — imported in 6 modules, never called; its write-only lock is a latent race footgun
  - root_cause: src/tag/core/config.py:69 — save_config retained after update_config replaced its callers; its lock (config.py:78-90) guards only _write_config_atomic, not the read-modify-write, so any future caller reusing load_config-
- ☑ **C030** [crash] cron_matches raises ZeroDivisionError on '*/0' — matcher lacks the guard create-time validation has
  - root_cause: src/tag/cron_scheduler.py:27-33 — _field_matches does int(step_str) and value % step with no zero/format check; validation lives only in the separate _validate_cron_field, so any cron_jobs row that bypassed validation (d
- ☑ **C031** [wrong-behavior] cron daemon 55s same-minute dedup can drop a legitimately-due every-minute (* * * * *) fire under poll drift
  - root_cause: src/tag/cron_scheduler.py:213 — `(now - last_dt).total_seconds() < 55`; should compare last_dt and now truncated to the minute (schedule resolution) rather than a fixed 55s window, which conflates 'twice in the same minu
- ☑ **C032** [validation] DAG step objects silently ignore unrecognized dependency keys (e.g. deps), producing an edge-free DAG with no warning
  - root_cause: src/tag/dag.py:326 — dep_refs = step.get('depends_on', []) with no validation of other keys; src/tag/dag.py:268-282 validate_dag_spec checks only name/per-step task, never depends_on references or unknown keys.
- ☑ **C033** [dead-code] DevUI _qs() helper is dead code that references a nonexistent attribute
  - root_cause: src/tag/devui.py:421-423 — leftover helper referencing parsed.query_params instead of the parse_qs dict; do_GET uses its own local qp closure (430-431), so _qs is unreachable dead code but a latent AttributeError trap.
- ☑ **C034** [consistency] Inconsistent relation counting across entity_graph summary / community / query
  - root_cause: src/tag/entity_graph.py:438-442 (format_graph_summary, source-only JOIN) vs 312-319 (detect_communities, both-endpoint JOIN) vs 380-385 (query_graph, OR match). The relations table has no profile column, so membership is
- ☑ **C035** [wrong-behavior] issue-solve --dry-run still spawns the agent subprocess
  - root_cause: src/tag/issue_solver.py:229-238 — agent invocation sits outside any dry_run guard; only branch/test/commit are gated on dry_run (214, 241). Presently masked only because the invocation form is broken (C004); fixing C004 
- ☑ **C036** [security] SSRF URL validator is DNS-rebinding susceptible (TOCTOU between validation and fetch)
  - root_cause: src/tag/cmd/marketplace.py:53-88 (mirrored in workflow_mgmt) — validation and connection resolve DNS separately; a low-TTL rebinding record can return a public IP during validation and 127.0.0.1/169.254.169.254 at urlope
- ☑ **C037** [incomplete-fix] tag mem2 extract on a fresh TAG_HOME crashes with confusing 'no such table: steps'
  - root_cause: src/tag/cmd/memory.py:288-309 — extract uses `_sq3.connect(db_path)` and SELECTs from steps without any ensure_schema call (unlike tier/fact/episode/store branches); ensure_runtime_dirs makes the dir but not the schema.
- ☑ **C038** [wrong-behavior] kanban.list_tasks treats limit=0 as 'no limit', returning all rows
  - root_cause: src/tag/kanban.py:377 — `if limit:` then `query += f' LIMIT {int(limit)}'`; because 0 is falsy the LIMIT clause is skipped. Should be `if limit is not None:`.
- ☑ **C039** [wrong-behavior] OTLP export maps spans with unset/missing status to ERROR (code 2) instead of UNSET (0)
  - root_cause: src/tag/otel_semconv.py:158 and src/tag/tracing.py:604 — `"status":{"code":1 if mapped.get('status')=='ok' else 2}` with no UNSET(0) branch; mapped.get('status') is None for status-less spans -> else -> code 2. Mainly bi
- ☑ **C040** [wrong-behavior] graph query --depth 0 is silently coerced to depth 2
  - root_cause: src/tag/cmd/prd_clusters.py:737 — `depth = getattr(args,'depth',2) or 2`; 0 is falsy so `or 2` substitutes 2. argparse already sets default=2 (prd_clusters.py:990), so getattr never falls back; the `or 2` only alters the
- ☑ **C041** [consistency] annotation/prompt-hub CLI opens the shared runtime SQLite with no busy_timeout/WAL, unlike every other new consumer
  - root_cause: src/tag/cmd/prd_clusters.py:282,328 — raw _sq3.connect missing the busy_timeout=5000+WAL hardening applied by queue_worker.py:28-31, cron_scheduler run_daemon _open, and core/db.py:47-57. Route through core.db.open_db or
- ☑ **C042** [json-contract] graph subcommands have inconsistent --json contracts: query rejects --json yet always emits JSON, show requires --json, build has none
  - root_cause: src/tag/cmd/prd_clusters.py register() — the graph 'show' parser defines --json but 'query' and 'build' do not, and the query handler always json.dumps its result regardless of any flag.
- ☑ **C043** [consistency] prompt get <name> --version <missing> reports 'Prompt not found: name' though the prompt exists
  - root_cause: src/tag/cmd/prd_clusters.py:~343-345 — get_prompt(conn,name,version=...) returns None both for unknown name and missing version, but the error branch hardcodes print_error(f'Prompt not found: {args.name!r}') without chec
- ☑ **C044** [resource-leak] eval-judge / eval-dataset / webhook-rule handlers leak sqlite connections (no close / try-finally)
  - root_cause: src/tag/cmd/prd_clusters.py:88,131 — raw _sq3.connect with no matching close and no try/finally, so WAL checkpoints/lock release rely on process exit/GC.
- ☑ **C045** [json-contract] tag swarm --json (no task/subcommand) emits plain-text usage instead of JSON
  - root_cause: src/tag/cmd/swarm.py:166-169 — the bare-invocation guard `if getattr(args,'task',None) is None: print('usage: ...'); return 0` does not check getattr(args,'json',False).
- ☑ **C046** [validation] tag doctor crashes with ugly AttributeError on scalar profiles: while render/bootstrap give a clean message
  - root_cause: src/tag/cmd/system.py:243/271 — defined_profiles = cfg.get('profiles') or {} then list(defined_profiles.keys()); a truthy scalar string bypasses the _config_profiles() isinstance/dict validation and .keys() raises Attrib
- ☑ **C047** [validation] tag doctor --profile '' silently checks all profiles instead of reporting an unknown profile
  - root_cause: src/tag/cmd/system.py:244-246/272-275 — profiles_to_check = [target_profile] if target_profile else list(defined_profiles.keys()); target_profile='' is falsy so it falls through to all profiles, conflating an explicit em
- ☑ **C048** [wrong-behavior] Branding rewrite force-capitalizes lowercase prose: 'hermes runtime' -> 'TAG Runtime'
  - root_cause: src/tag/core/utils.py:193 — `re.sub(r'\bHermes Runtime\b','TAG Runtime', flags=IGNORECASE)` case-insensitively matches lowercase input but substitutes a fixed Title-Case literal.
- ☑ **C049** [crash] Webhook server crashes on malformed / negative Content-Length (unhandled ValueError, no response; negative reads until EOF)
  - root_cause: src/tag/webhook_server.py:316-317 — length = int(self.headers.get('Content-Length',0)) with bare int() and no try/except, then self.rfile.read(length) with no validation or max-body cap.
- ☑ **C050** [doc-mismatch] Slack webhook signature verified over body only, not 'v0:timestamp:body' — rejects all genuine Slack signatures
  - root_cause: src/tag/webhook_server.py:108-115 — the Slack branch omits the 'v0:{timestamp}:' prefix and never reads X-Slack-Request-Timestamp, diverging from Slack's spec.
- ☑ **C051** [security] workflow_mgmt _execute_hook: 'webhook' type has no SSRF guard and 'shell' type interpolates payload into shell=True
  - root_cause: src/tag/cmd/workflow_mgmt.py:380-391 (webhook branch lacks _validate_fetch_url) and :374-376 (shell branch passes interpolated payload to subprocess.run(shell=True)). Inconsistent with SSRF/injection hardening applied el

## C006 — vendor tarball packaging (deployment decision)
The 54MB bundled Hermes runtime tarball ships in the pip **wheel** (setuptools include_package_data default) but was excluded from **npm** by a Phase-3 change — the two formats are now inconsistent. `tag setup` works either way (git-clone fallback when the bundle is absent). Decision needed: bundle in both (offline, but 55MB uploads that previously failed on PyPI) vs. exclude from both (lean 320KB, network git-clone at setup).