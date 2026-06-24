"""Shared core utilities for TAG CLI."""
from tag.core.config import load_config, save_config, config_path, benchmark_suite_path
from tag.core.paths import (
    package_root, resource_path, bundled_hermes_archive,
    tag_home, managed_root, hermes_root, hermes_bin,
    resolve_home_relative, ensure_default_file,
    is_hermes_checkout, hermes_checkout_kind, discover_local_hermes_checkout,
    python_runtime_supported, config_root,
    runtime_home, runtime_codex_home, runtime_db_path,
    hermes_repo_url, hermes_ref, hermes_env, profile_home, profile_exec_env,
    ensure_runtime_dirs, tag_cli_label, tag_cli_bin,
    is_tty, can_launch_interactive_tui,
    DEFAULT_TAG_HOME, DEFAULT_HERMES_CHECKOUT, MIN_PYTHON, MAX_PYTHON_EXCLUSIVE,
    APP_NAME, CLI_LABEL,
)
from tag.core.db import (
    open_db, journal_save, journal_list, journal_forget, journal_clear,
    journal_to_prompt_prefix, queue_insert_job, queue_update_pid,
    queue_update_status, queue_get_job, queue_list_jobs, queue_clear_completed,
    launch_queue_worker,
)
from tag.core.utils import (
    utc_now, nonnegative_int, positive_int, slugify, normalize_chat_output,
    rewrite_cli_hints, strip_json_fences,
    merged_env_example, configured_skins, install_profile_skins, _deep_merge,
    write_yaml, write_text, read_dotenv, _sanitize_env_value, _upsert_env_line,
    _fix_box_title_alignment, infrastructure_failure_reason,
)
from tag.core.profile import (
    render_profiles, bootstrap_profiles, resolve_route, parse_model_ref,
    format_model_ref, collect_assignments, load_model_inventory,
    load_openrouter_catalog, ensure_profile_exists, apply_route_model_overrides,
    run_chat_step, load_benchmark_suite, case_passed,
)
from tag.core.run import (
    run_hermes, run_profile_hermes, run_profile_python,
)
