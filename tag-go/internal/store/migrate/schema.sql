-- TAG state schema (Go port of core/db.py). Single SQLite store, WAL, single-writer.
CREATE TABLE IF NOT EXISTS runs (
  id TEXT PRIMARY KEY, created_at TEXT NOT NULL, kind TEXT NOT NULL, task_type TEXT NOT NULL,
  execution TEXT NOT NULL, master_profile TEXT NOT NULL, board TEXT NOT NULL, prompt TEXT NOT NULL,
  route_json TEXT NOT NULL, status TEXT NOT NULL, metadata_json TEXT NOT NULL DEFAULT '{}',
  model_id TEXT, prompt_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens INTEGER NOT NULL DEFAULT 0, cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
  estimated_cost_usd REAL NOT NULL DEFAULT 0.0, duration_ms INTEGER, completed_at TEXT
);
CREATE TABLE IF NOT EXISTS steps (
  id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL, role TEXT NOT NULL, profile TEXT NOT NULL,
  model_ref TEXT NOT NULL, prompt TEXT NOT NULL, output TEXT NOT NULL, status TEXT NOT NULL,
  started_at TEXT NOT NULL, finished_at TEXT NOT NULL, duration_ms INTEGER NOT NULL, extra_json TEXT NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS spans (
  id TEXT PRIMARY KEY, trace_id TEXT NOT NULL, parent_id TEXT, name TEXT NOT NULL, profile TEXT, model_id TEXT,
  started_at TEXT NOT NULL, finished_at TEXT, duration_ms INTEGER, status TEXT NOT NULL DEFAULT 'ok',
  prompt_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0,
  attributes TEXT NOT NULL DEFAULT '{}', error_msg TEXT, kind TEXT, cost_usd REAL
);
CREATE TABLE IF NOT EXISTS trace_snapshots (
  id TEXT PRIMARY KEY, trace_id TEXT NOT NULL, step_index INTEGER NOT NULL DEFAULT 0,
  snapshot_json TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_spans_trace ON spans(trace_id);
CREATE TABLE IF NOT EXISTS memory_journal (
  id TEXT PRIMARY KEY, profile TEXT NOT NULL, key TEXT NOT NULL, value TEXT NOT NULL,
  scope TEXT NOT NULL DEFAULT 'profile', created_at TEXT NOT NULL, expires_at TEXT, UNIQUE(profile, key)
);
CREATE INDEX IF NOT EXISTS idx_mj_profile ON memory_journal(profile);
CREATE TABLE IF NOT EXISTS semantic_memories (
  id TEXT PRIMARY KEY, profile TEXT NOT NULL, content TEXT NOT NULL, memory_type TEXT NOT NULL DEFAULT 'fact',
  confidence REAL NOT NULL DEFAULT 1.0, created_at TEXT NOT NULL, accessed_at TEXT NOT NULL,
  access_count INTEGER NOT NULL DEFAULT 0, source TEXT NOT NULL DEFAULT 'manual',
  tier TEXT NOT NULL DEFAULT 'archival', embedding BLOB, embed_model TEXT,
  valid_at TEXT, invalid_at TEXT
);
CREATE TABLE IF NOT EXISTS memory_fact_history (
  history_id TEXT PRIMARY KEY, original_id TEXT NOT NULL, successor_id TEXT, profile TEXT NOT NULL,
  content TEXT NOT NULL, memory_type TEXT NOT NULL, confidence REAL NOT NULL, source TEXT NOT NULL,
  valid_at TEXT NOT NULL, invalid_at TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '', archived_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mfh_original ON memory_fact_history(original_id);
CREATE INDEX IF NOT EXISTS idx_mfh_profile ON memory_fact_history(profile);
CREATE INDEX IF NOT EXISTS idx_sm_profile ON semantic_memories(profile, memory_type);
CREATE VIRTUAL TABLE IF NOT EXISTS semantic_memories_fts USING fts5(id, profile, content, memory_type, tokenize='porter unicode61');
CREATE TABLE IF NOT EXISTS queue_jobs (
  id TEXT PRIMARY KEY, profile TEXT NOT NULL, task TEXT NOT NULL, task_type TEXT NOT NULL DEFAULT 'mixed',
  status TEXT NOT NULL DEFAULT 'queued', priority INTEGER NOT NULL DEFAULT 5, created_at TEXT NOT NULL,
  started_at TEXT, finished_at TEXT, pid INTEGER, result_path TEXT, exit_code INTEGER, error TEXT,
  notify INTEGER NOT NULL DEFAULT 1, deps_json TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX IF NOT EXISTS idx_qj_status ON queue_jobs(status, created_at);
CREATE TABLE IF NOT EXISTS queue_dags (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, spec_json TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS token_budgets (
  id TEXT PRIMARY KEY, profile TEXT NOT NULL UNIQUE, period TEXT NOT NULL DEFAULT 'daily', max_tokens INTEGER NOT NULL,
  warn_pct REAL NOT NULL DEFAULT 0.8, enabled INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS personas (
  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, description TEXT NOT NULL DEFAULT '', style_prompt TEXT NOT NULL,
  inject TEXT NOT NULL DEFAULT 'prepend', tags_json TEXT NOT NULL DEFAULT '[]', source TEXT NOT NULL DEFAULT 'builtin', created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS active_personas (profile TEXT NOT NULL, persona_name TEXT NOT NULL, position INTEGER NOT NULL DEFAULT 0, session_id TEXT, created_at TEXT, PRIMARY KEY(profile, persona_name));
CREATE TABLE IF NOT EXISTS cron_jobs (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, schedule TEXT NOT NULL, task TEXT NOT NULL, profile TEXT NOT NULL DEFAULT 'orchestrator',
  enabled INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL, last_run TEXT, run_count INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS route_fallbacks (
  id TEXT PRIMARY KEY, profile TEXT NOT NULL, primary_model TEXT NOT NULL, fallback_model TEXT NOT NULL,
  condition TEXT NOT NULL DEFAULT 'context_overflow', priority INTEGER NOT NULL DEFAULT 1, enabled INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS security_scans (id TEXT PRIMARY KEY, scanned_path TEXT NOT NULL, finding_count INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'ok', created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS notification_hooks (
  id TEXT PRIMARY KEY, profile TEXT, event TEXT NOT NULL, channel TEXT NOT NULL,
  config_json TEXT NOT NULL DEFAULT '{}', template TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_nh_event ON notification_hooks(event, enabled);
CREATE TABLE IF NOT EXISTS notification_log (
  id TEXT PRIMARY KEY, hook_id TEXT NOT NULL, event TEXT NOT NULL, channel TEXT NOT NULL,
  outcome TEXT NOT NULL, http_status INTEGER, attempt INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL,
  FOREIGN KEY(hook_id) REFERENCES notification_hooks(id)
);
CREATE TABLE IF NOT EXISTS workspace_files (
  path TEXT PRIMARY KEY, content_hash TEXT NOT NULL, byte_size INTEGER NOT NULL, token_count INTEGER NOT NULL,
  rank REAL NOT NULL DEFAULT 0, indexed_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS maintenance_state (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS entities (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, entity_type TEXT NOT NULL DEFAULT 'other',
  description TEXT NOT NULL DEFAULT '', confidence REAL NOT NULL DEFAULT 1.0, profile TEXT NOT NULL,
  created_at TEXT NOT NULL, mention_count INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_ent_profile ON entities(profile, entity_type);
CREATE INDEX IF NOT EXISTS idx_ent_name ON entities(profile, name COLLATE NOCASE);
CREATE TABLE IF NOT EXISTS relations (
  id TEXT PRIMARY KEY, source_entity_id TEXT NOT NULL, target_entity_id TEXT NOT NULL,
  relation_type TEXT NOT NULL DEFAULT 'related_to', confidence REAL NOT NULL DEFAULT 1.0,
  source_memory_id TEXT, created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_rel_source ON relations(source_entity_id);
CREATE INDEX IF NOT EXISTS idx_rel_target ON relations(target_entity_id);
CREATE TABLE IF NOT EXISTS entity_communities (
  id TEXT PRIMARY KEY, member_ids_json TEXT NOT NULL, label TEXT NOT NULL,
  cohesion_score REAL NOT NULL DEFAULT 0.5, profile TEXT NOT NULL, computed_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS prompt_versions (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, version INTEGER NOT NULL, content TEXT NOT NULL,
  variables_json TEXT NOT NULL DEFAULT '[]', tags_json TEXT NOT NULL DEFAULT '[]',
  parent_version_id TEXT, author TEXT, message TEXT, sha256 TEXT NOT NULL,
  created_at TEXT NOT NULL, is_active INTEGER NOT NULL DEFAULT 1, UNIQUE(name, version)
);
CREATE INDEX IF NOT EXISTS idx_pv_name_version ON prompt_versions(name, version);
CREATE TABLE IF NOT EXISTS alert_rules (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, metric TEXT NOT NULL, condition TEXT NOT NULL,
  threshold REAL NOT NULL, severity TEXT NOT NULL, profile TEXT, suite TEXT,
  enabled INTEGER NOT NULL DEFAULT 1, notify_channels TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL, last_triggered_at TEXT
);
CREATE TABLE IF NOT EXISTS alert_firings (
  id TEXT PRIMARY KEY, rule_id TEXT NOT NULL, rule_name TEXT NOT NULL, metric TEXT NOT NULL,
  actual_value REAL NOT NULL, threshold REAL NOT NULL, severity TEXT NOT NULL,
  fired_at TEXT NOT NULL, resolved_at TEXT, message TEXT NOT NULL,
  FOREIGN KEY(rule_id) REFERENCES alert_rules(id)
);
CREATE INDEX IF NOT EXISTS idx_alert_firings_rule_fired ON alert_firings(rule_id, fired_at);
CREATE TABLE IF NOT EXISTS annotation_tasks (
  id TEXT PRIMARY KEY, source_type TEXT NOT NULL, source_id TEXT NOT NULL, content TEXT NOT NULL,
  question TEXT NOT NULL, label_schema TEXT NOT NULL DEFAULT '{}', status TEXT NOT NULL DEFAULT 'pending',
  assigned_to TEXT, label TEXT, notes TEXT, created_at TEXT NOT NULL, completed_at TEXT,
  priority INTEGER NOT NULL DEFAULT 0, tags TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX IF NOT EXISTS idx_at_status_priority ON annotation_tasks(status, priority DESC, created_at);
CREATE INDEX IF NOT EXISTS idx_at_assigned ON annotation_tasks(assigned_to, status);
CREATE TABLE IF NOT EXISTS eval_datasets (
  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, description TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL, version INTEGER NOT NULL DEFAULT 1, source_type TEXT NOT NULL DEFAULT 'manual',
  case_count INTEGER NOT NULL DEFAULT 0, tags_json TEXT NOT NULL DEFAULT '[]'
);
CREATE TABLE IF NOT EXISTS eval_dataset_cases (
  id TEXT PRIMARY KEY, dataset_id TEXT NOT NULL REFERENCES eval_datasets(id), case_id TEXT NOT NULL,
  input TEXT NOT NULL, expected_output TEXT, reference_context TEXT,
  metadata_json TEXT NOT NULL DEFAULT '{}', created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_edc_dataset ON eval_dataset_cases(dataset_id);
CREATE TABLE IF NOT EXISTS eval_runs (
  id TEXT PRIMARY KEY, suite_path TEXT NOT NULL, profile TEXT NOT NULL, suite_name TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'running', pass_count INTEGER NOT NULL DEFAULT 0,
  fail_count INTEGER NOT NULL DEFAULT 0, total_count INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, completed_at TEXT
);
CREATE TABLE IF NOT EXISTS eval_cases (
  id TEXT PRIMARY KEY, eval_run_id TEXT NOT NULL, case_id TEXT NOT NULL, input TEXT NOT NULL,
  output TEXT NOT NULL DEFAULT '', passed INTEGER NOT NULL DEFAULT 0, score REAL NOT NULL DEFAULT 0.0,
  failure_reason TEXT, created_at TEXT NOT NULL, FOREIGN KEY(eval_run_id) REFERENCES eval_runs(id)
);
CREATE TABLE IF NOT EXISTS swarm_runs (
  swarm_id TEXT PRIMARY KEY, goal TEXT NOT NULL, coordinator_profile TEXT NOT NULL,
  failure_policy TEXT NOT NULL DEFAULT 'best_effort', status TEXT NOT NULL DEFAULT 'pending',
  max_agents INTEGER NOT NULL DEFAULT 4, started_at TEXT, completed_at TEXT,
  total_tokens_prompt INTEGER DEFAULT 0, total_tokens_completion INTEGER DEFAULT 0,
  total_cost_usd REAL DEFAULT 0.0, task_count INTEGER DEFAULT 0, final_output TEXT,
  manifest_json TEXT, created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE TABLE IF NOT EXISTS swarm_tasks (
  id INTEGER PRIMARY KEY AUTOINCREMENT, swarm_id TEXT NOT NULL REFERENCES swarm_runs(swarm_id),
  task_id TEXT NOT NULL, profile TEXT NOT NULL, description TEXT, context_slice_json TEXT,
  status TEXT NOT NULL DEFAULT 'pending', pid INTEGER, started_at TEXT, completed_at TEXT,
  tokens_prompt INTEGER DEFAULT 0, tokens_completion INTEGER DEFAULT 0, cost_usd REAL DEFAULT 0.0,
  model TEXT, output TEXT, error_message TEXT, artifacts_json TEXT, UNIQUE(swarm_id, task_id)
);
CREATE TABLE IF NOT EXISTS memory_gc_runs (
  id TEXT PRIMARY KEY, profile TEXT NOT NULL, evicted INTEGER NOT NULL DEFAULT 0,
  merged INTEGER NOT NULL DEFAULT 0, promoted INTEGER NOT NULL DEFAULT 0,
  duration_s REAL NOT NULL DEFAULT 0.0, run_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS memory_episodes (
  episode_id TEXT PRIMARY KEY, profile TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
  session_id TEXT, started_at TEXT NOT NULL, ended_at TEXT, summary TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'open'
);
CREATE INDEX IF NOT EXISTS idx_ep_profile ON memory_episodes(profile, started_at DESC);
CREATE TABLE IF NOT EXISTS memory_episode_links (
  memory_id TEXT NOT NULL, episode_id TEXT NOT NULL, linked_at TEXT NOT NULL,
  PRIMARY KEY (memory_id, episode_id)
);
CREATE INDEX IF NOT EXISTS idx_el_episode ON memory_episode_links(episode_id);
CREATE INDEX IF NOT EXISTS idx_el_memory ON memory_episode_links(memory_id);
CREATE TABLE IF NOT EXISTS hook_log (
  id TEXT PRIMARY KEY, hook_name TEXT NOT NULL, event_id TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'ok', response TEXT, fired_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_hook_log_name ON hook_log(hook_name, fired_at);
CREATE TABLE IF NOT EXISTS tool_index_meta (
  id TEXT PRIMARY KEY DEFAULT 'singleton', tool_count INTEGER NOT NULL DEFAULT 0, built_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tool_index (
  name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', server TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(server, name)
);
CREATE TABLE IF NOT EXISTS trigger_rules (
  id TEXT PRIMARY KEY, platform TEXT NOT NULL, event TEXT NOT NULL, profile TEXT NOT NULL,
  action TEXT NOT NULL, filter_labels TEXT NOT NULL DEFAULT '[]', created_at TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_tr_platform ON trigger_rules(platform, event);
CREATE TABLE IF NOT EXISTS webhook_events (
  id TEXT PRIMARY KEY, platform TEXT NOT NULL, event_type TEXT NOT NULL, payload_json TEXT NOT NULL DEFAULT '{}',
  received_at TEXT NOT NULL, signature_valid INTEGER NOT NULL DEFAULT 0, matched_rules TEXT NOT NULL DEFAULT '[]',
  status TEXT NOT NULL DEFAULT 'pending'
);
CREATE INDEX IF NOT EXISTS idx_we_platform ON webhook_events(platform, received_at);
CREATE TABLE IF NOT EXISTS benchmark_comparisons (
  id TEXT PRIMARY KEY, suite_path TEXT NOT NULL, models TEXT NOT NULL DEFAULT '[]',
  judge_model TEXT, created_at TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'running'
);
CREATE TABLE IF NOT EXISTS benchmark_results (
  id TEXT PRIMARY KEY, comparison_id TEXT NOT NULL, model_id TEXT NOT NULL, case_id TEXT NOT NULL,
  output TEXT, passed INTEGER, quality_score REAL, latency_ms INTEGER, prompt_tokens INTEGER,
  completion_tokens INTEGER, cost_usd REAL, error TEXT, created_at TEXT NOT NULL
);
