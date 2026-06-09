from __future__ import annotations

import importlib.util
from copy import deepcopy
import io
import json
import os
from pathlib import Path
import threading
import subprocess
import tarfile
import urllib.error

import pytest


ROOT = Path(__file__).resolve().parents[1]
MODULE_PATH = ROOT / "src" / "tag" / "controller.py"
SPEC = importlib.util.spec_from_file_location("tag_controller", MODULE_PATH)
assert SPEC and SPEC.loader
TAG = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(TAG)


def load_cfg():
    return TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")


def test_parse_model_ref():
    provider, model = TAG.parse_model_ref("openrouter/deepseek/deepseek-v4-flash")
    assert provider == "openrouter"
    assert model == "deepseek/deepseek-v4-flash"


def test_resolve_route_mixed():
    route = TAG.resolve_route(load_cfg(), "mixed", None, [])
    assert route["master"]["name"] == "orchestrator"
    assert [worker["name"] for worker in route["workers"]] == ["researcher", "coder"]
    assert route["verifier"]["name"] == "reviewer"


def test_apply_route_model_overrides():
    route = TAG.resolve_route(load_cfg(), "mixed", None, [])
    updated = TAG.apply_route_model_overrides(
        route,
        master_model="openai-codex/gpt-5.4",
        verifier_model="openrouter/deepseek/deepseek-v4-pro",
        worker_models=[
            "researcher=openrouter/deepseek/deepseek-v4-flash",
            "coder=openrouter/qwen/qwen3-coder",
        ],
    )
    assert updated["master"]["model"]["default"] == "gpt-5.4"
    assert updated["verifier"]["model"]["default"] == "deepseek/deepseek-v4-pro"
    assert updated["workers"][0]["model"]["default"] == "deepseek/deepseek-v4-flash"
    assert updated["workers"][1]["model"]["default"] == "qwen/qwen3-coder"


def test_case_passed_exact():
    ok, reason = TAG.case_passed({"expected_exact": "bench-ok"}, "bench-ok")
    assert ok is True
    assert "bench-ok" in reason


def test_case_passed_json():
    ok, _ = TAG.case_passed(
        {"expected_json": {"status": "ok", "sum": 42}},
        '{"status":"ok","sum":42}',
    )
    assert ok is True


def test_normalize_chat_output_strips_runtime_noise():
    output = (
        "⚠ tirith security scanner enabled but not available — command scanning will use pattern matching only\n"
        "bench-ok\n"
        "session_id: 123"
    )
    assert TAG.normalize_chat_output(output) == "bench-ok"


def test_infrastructure_failure_reason_detects_codex_auth_failure():
    output = (
        "⚠ tirith security scanner enabled but not available — command scanning will use pattern matching only\n"
        "Error: Codex authentication failed — your ChatGPT/Codex login looks expired or invalid.\n"
        "session_id: 123\n"
    )
    assert TAG.infrastructure_failure_reason(output) == "error: codex authentication failed"


def test_case_passed_json_fenced():
    ok, _ = TAG.case_passed(
        {"expected_json": {"status": "ok", "sum": 42}},
        '```json\n{"status":"ok","sum":42}\n```',
    )
    assert ok is True


def test_render_profiles_installs_skin_assets(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    cfg = deepcopy(load_cfg())
    rendered = TAG.render_profiles(cfg, force=True)
    assert rendered

    researcher_home = (
        Path(TAG.runtime_home(cfg)).resolve() / ".hermes" / "profiles" / "researcher"
    )
    skin_file = researcher_home / "skins" / "tag-control.yaml"
    config_file = researcher_home / "config.yaml"

    assert skin_file.exists()
    assert config_file.exists()
    rendered_cfg = TAG.load_config(config_file)
    assert rendered_cfg["display"]["skin"] == "tag-control"


class FakeTty(io.StringIO):
    def __init__(self, is_tty: bool):
        super().__init__()
        self._is_tty = is_tty

    def isatty(self):
        return self._is_tty


def test_can_launch_interactive_tui_requires_tty():
    assert TAG.can_launch_interactive_tui(FakeTty(True), FakeTty(True), FakeTty(True)) is True
    assert TAG.can_launch_interactive_tui(FakeTty(False), FakeTty(True), FakeTty(True)) is False
    assert TAG.can_launch_interactive_tui(FakeTty(True), FakeTty(False), FakeTty(False)) is False


def test_doctor_prerequisites_reports_missing_checkout(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    monkeypatch.setenv("TAG_HERMES_ROOT", str(tmp_path / "missing-hermes"))
    report = TAG.doctor_prerequisites(load_cfg())
    assert report["hermes_checkout_exists"] is False
    assert report["bundled_hermes_available"] is True
    assert report["patch_status"] == "checkout-missing"
    assert report["tui_react_installed"] is False
    assert report["tui_vitest_installed"] is False


def test_patch_status_marks_bundled_prepatched(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    bundled_root = tmp_path / "bundled-hermes"
    bundled_root.mkdir()
    monkeypatch.setenv("TAG_HERMES_ROOT", str(bundled_root))

    class Result:
        def __init__(self, returncode):
            self.returncode = returncode
            self.stdout = ""
            self.stderr = ""

    calls = []

    def fake_run_external(cmd, **kwargs):
        calls.append(cmd)
        if "--reverse" in cmd:
            return Result(0)
        return Result(1)

    monkeypatch.setattr(TAG, "run_external", fake_run_external)
    assert TAG.patch_status(load_cfg()) == "prepatched"


def test_bundled_hermes_archive_exists():
    assert TAG.bundled_hermes_archive().exists() is True


def test_python_runtime_supported_range():
    assert TAG.python_runtime_supported((3, 11)) is True
    assert TAG.python_runtime_supported((3, 13)) is True
    assert TAG.python_runtime_supported((3, 10)) is False
    assert TAG.python_runtime_supported((3, 14)) is False


def test_hermes_checkout_kind(tmp_path):
    missing = tmp_path / "missing"
    assert TAG.hermes_checkout_kind(missing) == "missing"

    bundled = tmp_path / "bundled"
    bundled.mkdir()
    assert TAG.hermes_checkout_kind(bundled) == "bundled"

    git_root = tmp_path / "git-root"
    (git_root / ".git").mkdir(parents=True)
    assert TAG.hermes_checkout_kind(git_root) == "git"


def test_build_parser_exposes_extended_hermes_surface():
    parser = TAG.build_parser()
    help_text = parser.format_help()
    for command in (
        "status",
        "config",
        "sessions",
        "skills",
        "plugins",
        "tools",
        "mcp",
        "logs",
        "dashboard",
        "memory",
        "completion",
        "prompt-size",
        "update",
    ):
        assert command in help_text


def test_hermes_root_falls_back_to_discovered_checkout(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    checkout = tmp_path / "cwd" / "hermes-agent-upstream"
    (checkout / "ui-tui").mkdir(parents=True)
    (checkout / "ui-tui" / "package.json").write_text("{}", encoding="utf-8")
    (checkout / "pyproject.toml").write_text("[build-system]\nrequires=[]\n", encoding="utf-8")
    monkeypatch.chdir(tmp_path / "cwd")
    assert TAG.hermes_root(load_cfg()) == checkout.resolve()


def test_clone_or_update_hermes_prefers_managed_checkout_over_discovered_sibling(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    cfg = load_cfg()
    discovered = tmp_path / "cwd" / "hermes-agent-upstream"
    (discovered / "ui-tui").mkdir(parents=True)
    (discovered / "ui-tui" / "package.json").write_text("{}", encoding="utf-8")
    (discovered / "pyproject.toml").write_text("[build-system]\nrequires=[]\n", encoding="utf-8")
    monkeypatch.chdir(tmp_path / "cwd")

    extracted_root = TAG.resolve_home_relative(str(cfg["upstream"]["checkout_dir"]))
    calls: list[tuple[Path, Path]] = []

    def fake_extract(root: Path) -> dict[str, str]:
        calls.append((root, discovered.resolve()))
        return {"checkout": str(root), "status": "bundled"}

    monkeypatch.setattr(TAG, "extract_bundled_hermes", fake_extract)
    result = TAG.clone_or_update_hermes(cfg, refresh=False)

    assert result["status"] == "bundled"
    assert calls == [(extracted_root, discovered.resolve())]


def test_cmd_submit_auto_bootstraps_when_hermes_missing(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    cfg_path = TAG.config_path(None)
    cfg = TAG.load_config(cfg_path)
    calls = []
    state = {"ready": False}

    monkeypatch.setattr(TAG, "discover_local_hermes_checkout", lambda: None)
    monkeypatch.setattr(
        TAG,
        "hermes_bin",
        lambda cfg=None: (TAG.hermes_root(cfg) / ".venv" / "bin" / "hermes") if state["ready"] else (tmp_path / "missing-hermes"),
    )

    def fake_setup(args):
        calls.append(("setup", args.skip_tui_build))
        state["ready"] = True
        return 0

    monkeypatch.setattr(TAG, "cmd_setup", fake_setup)
    monkeypatch.setattr(
        TAG,
        "run_chat_step",
        lambda *_a, **_k: {
            "profile": "researcher",
            "status": "ok",
            "prompt": "x",
            "output": "ok",
            "started_at": "a",
            "finished_at": "b",
            "duration_ms": 1,
            "returncode": 0,
            "model_ref": "openrouter/model",
        },
    )

    args = TAG.argparse.Namespace(
        config=str(cfg_path),
        task_type="research",
        prompt="Reply with exactly: smoke-ok",
        title=None,
        source="manual",
        execution="direct",
        master_profile=None,
        worker_profile=[],
        master_model=None,
        verifier_model=None,
        worker_model_override=[],
        verify=False,
        wait_seconds=0,
        json=True,
    )
    assert TAG.cmd_submit(args) == 0
    assert calls == [("setup", True)]


def test_run_chat_step_marks_infrastructure_failure_as_error(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    cfg = load_cfg()

    class Proc:
        returncode = 0
        stdout = (
            "⚠ tirith security scanner enabled but not available — command scanning will use pattern matching only\n"
            "Error: Codex authentication failed — your ChatGPT/Codex login looks expired or invalid.\n"
            "session_id: abc\n"
        )
        stderr = ""

    monkeypatch.setattr(TAG, "run_profile_hermes", lambda *a, **k: Proc())
    step = TAG.run_chat_step(cfg, profile_name="reviewer", prompt="x")
    assert step["status"] == "error"
    assert step["failure_reason"] == "error: codex authentication failed"


def test_cmd_hermes_passthrough_auto_bootstraps_for_tui(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    cfg_path = TAG.config_path(None)
    calls = []
    state = {"ready": False}
    monkeypatch.setattr(TAG, "discover_local_hermes_checkout", lambda: None)
    monkeypatch.setattr(
        TAG,
        "cmd_setup",
        lambda args: calls.append(("setup", args.skip_tui_build)) or state.__setitem__("ready", True) or 0,
    )
    monkeypatch.setattr(
        TAG,
        "hermes_bin",
        lambda cfg=None: Path("/bin/echo") if state["ready"] else (tmp_path / "missing-hermes"),
    )
    monkeypatch.setattr(
        TAG.subprocess,
        "run",
        lambda *a, **k: TAG.argparse.Namespace(returncode=0),
    )
    args = TAG.argparse.Namespace(config=str(cfg_path), profile="orchestrator", hermes_args=["--tui"])
    assert TAG.cmd_hermes_passthrough(args) == 0
    assert calls == [("setup", False)]


def test_safe_extract_rejects_traversal_and_links(tmp_path):
    payload = tmp_path / "payload.txt"
    payload.write_text("x", encoding="utf-8")

    traversal = tmp_path / "traversal.tar.gz"
    with tarfile.open(traversal, "w:gz") as tf:
        tf.add(payload, arcname="../../../etc/passwd")
    with pytest.raises(SystemExit):
        TAG.safe_extract_tar_gz(traversal, tmp_path / "out1")

    symlink = tmp_path / "symlink.tar.gz"
    with tarfile.open(symlink, "w:gz") as tf:
        info = tarfile.TarInfo("link")
        info.type = tarfile.SYMTYPE
        info.linkname = "/tmp/outside"
        tf.addfile(info)
    with pytest.raises(SystemExit):
        TAG.safe_extract_tar_gz(symlink, tmp_path / "out2")


def test_safe_extract_rejects_corrupt_archive(tmp_path):
    archive = tmp_path / "corrupt.tar.gz"
    archive.write_bytes(b"not-a-real-tarball")
    with pytest.raises(SystemExit, match="Bundled Hermes archive could not be read:"):
        TAG.safe_extract_tar_gz(archive, tmp_path / "out")


def test_cmd_setup_skip_python_install_fails_cleanly(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    cfg_path = TAG.config_path(None)
    cfg = TAG.load_config(cfg_path)

    monkeypatch.setattr(TAG, "ensure_setup_prereqs", lambda *a, **k: None)
    monkeypatch.setattr(TAG, "ensure_runtime_dirs", lambda *_a, **_k: None)
    monkeypatch.setattr(TAG, "doctor_prerequisites", lambda *_a, **_k: {})
    monkeypatch.setattr(TAG, "clone_or_update_hermes", lambda *_a, **_k: {"status": "existing"})
    monkeypatch.setattr(TAG, "ensure_venv", lambda *_a, **_k: {"status": "existing"})
    monkeypatch.setattr(TAG, "apply_hermes_patch", lambda *_a, **_k: {"status": "already-applied"})
    monkeypatch.setattr(TAG, "hermes_bin", lambda *_a, **_k: tmp_path / "missing-hermes")

    args = TAG.argparse.Namespace(
        config=str(cfg_path),
        refresh=False,
        skip_python_install=True,
        skip_tui_build=True,
        json=True,
    )
    with pytest.raises(SystemExit, match="Hermes Python is not installed; cannot bootstrap profiles"):
        TAG.cmd_setup(args)


def test_normalize_hermes_passthrough_args_strips_separator_and_defaults_help():
    assert TAG.normalize_hermes_passthrough_args(["--"]) == ["--help"]
    assert TAG.normalize_hermes_passthrough_args(["status", "--", "--help"]) == ["status", "--help"]
    assert TAG.normalize_hermes_passthrough_args(["--", "--version"]) == ["--version"]


def test_cmd_tui_non_tty_returns_2(tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    monkeypatch.setattr(TAG, "can_launch_interactive_tui", lambda *a, **k: False)
    monkeypatch.delenv("TAG_FORCE_TUI", raising=False)
    args = TAG.argparse.Namespace(config=None, profile="orchestrator", hermes_args=[])
    rc = TAG.cmd_tui(args)
    captured = capsys.readouterr()
    assert rc == 2
    assert "TAG TUI requires an interactive terminal" in captured.err


def test_cmd_tui_help_bypasses_non_tty_guard(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    seen = {}

    def fake_passthrough(args):
        seen["hermes_args"] = args.hermes_args
        return 0

    monkeypatch.setattr(TAG, "cmd_hermes_passthrough", fake_passthrough)
    monkeypatch.setattr(TAG, "can_launch_interactive_tui", lambda *a, **k: False)
    monkeypatch.delenv("TAG_FORCE_TUI", raising=False)
    args = TAG.argparse.Namespace(config=None, profile="orchestrator", hermes_args=["--", "--help"])
    assert TAG.cmd_tui(args) == 0
    assert seen["hermes_args"] == ["--help"]


def test_cmd_hermes_passthrough_supports_version_flag(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    cfg_path = TAG.config_path(None)
    monkeypatch.setattr(TAG, "ensure_hermes_ready", lambda *a, **k: None)
    monkeypatch.setattr(TAG, "hermes_bin", lambda *_a, **_k: Path("/bin/echo"))
    seen = {}

    def fake_run(cmd, **kwargs):
        seen["cmd"] = cmd
        return TAG.argparse.Namespace(returncode=0)

    monkeypatch.setattr(TAG.subprocess, "run", fake_run)
    args = TAG.argparse.Namespace(
        config=str(cfg_path),
        profile=None,
        hermes_args=[],
        hermes_version=True,
    )
    assert TAG.cmd_hermes_passthrough(args) == 0
    assert seen["cmd"] == ["/bin/echo", "--version"]


def test_bootstrap_profiles_wraps_hermes_errors(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    cfg = deepcopy(load_cfg())
    cfg["profiles"] = {
        "bad/name": {
            "description": "slash profile",
            "config": {"model": {"provider": "openai-codex", "default": "gpt-5.4"}},
        }
    }

    err = subprocess.CalledProcessError(
        1,
        ["hermes", "profile", "create", "bad/name"],
        stderr="invalid profile name",
    )
    monkeypatch.setattr(TAG, "run_hermes", lambda *_a, **_k: (_ for _ in ()).throw(err))
    with pytest.raises(SystemExit, match="Failed to create Hermes profile 'bad/name'"):
        TAG.bootstrap_profiles(cfg)


def test_cmd_setup_auto_imports_existing_codex(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    fake_bin = tmp_path / "tag-home" / "managed" / "hermes-agent-upstream" / ".venv" / "bin" / "hermes"
    fake_bin.parent.mkdir(parents=True, exist_ok=True)
    fake_bin.write_text("", encoding="utf-8")
    codex_home = tmp_path / "real-codex"
    codex_home.mkdir()
    (codex_home / "auth.json").write_text("{}", encoding="utf-8")
    monkeypatch.setenv("TAG_IMPORT_CODEX_HOME", str(codex_home))
    monkeypatch.setattr(TAG, "ensure_setup_prereqs", lambda *a, **k: None)
    monkeypatch.setattr(TAG, "clone_or_update_hermes", lambda *a, **k: {"status": "existing"})
    monkeypatch.setattr(TAG, "ensure_venv", lambda *a, **k: {"status": "existing"})
    monkeypatch.setattr(TAG, "install_hermes_python", lambda *a, **k: {"status": "installed"})
    monkeypatch.setattr(TAG, "apply_hermes_patch", lambda *a, **k: {"status": "already-applied"})
    monkeypatch.setattr(TAG, "install_tui_dependencies", lambda *a, **k: {"status": "built"})
    monkeypatch.setattr(
        TAG,
        "bootstrap_profiles",
        lambda cfg: [
            {"profile": "orchestrator", "status": "created"},
            {"profile": "codex-runtime-master", "status": "created"},
        ],
    )
    monkeypatch.setattr(TAG, "render_profiles", lambda *a, **k: [{"profile": "orchestrator"}])
    monkeypatch.setattr(TAG, "hermes_bin", lambda cfg=None: fake_bin)
    imports = []
    monkeypatch.setattr(
        TAG,
        "import_codex_into_profile",
        lambda cfg, *, profile_name, source_codex_home: imports.append((profile_name, source_codex_home)) or {"profile": profile_name, "status": "imported"},
    )
    args = TAG.argparse.Namespace(config=None, refresh=False, skip_python_install=False, skip_tui_build=False, json=True)
    assert TAG.cmd_setup(args) == 0
    assert imports == [
        ("orchestrator", codex_home.resolve()),
        ("codex-runtime-master", codex_home.resolve()),
    ]


def test_cmd_setup_does_not_force_rerender(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    fake_bin = tmp_path / "tag-home" / "managed" / "hermes-agent-upstream" / ".venv" / "bin" / "hermes"
    fake_bin.parent.mkdir(parents=True, exist_ok=True)
    fake_bin.write_text("", encoding="utf-8")
    monkeypatch.setattr(TAG, "ensure_setup_prereqs", lambda *a, **k: None)
    monkeypatch.setattr(TAG, "clone_or_update_hermes", lambda *a, **k: {"status": "existing"})
    monkeypatch.setattr(TAG, "ensure_venv", lambda *a, **k: {"status": "existing"})
    monkeypatch.setattr(TAG, "install_hermes_python", lambda *a, **k: {"status": "installed"})
    monkeypatch.setattr(TAG, "apply_hermes_patch", lambda *a, **k: {"status": "already-applied"})
    monkeypatch.setattr(TAG, "install_tui_dependencies", lambda *a, **k: {"status": "built"})
    monkeypatch.setattr(
        TAG,
        "bootstrap_profiles",
        lambda cfg: [{"profile": "orchestrator", "status": "existing"}],
    )
    seen = {}
    monkeypatch.setattr(
        TAG,
        "render_profiles",
        lambda cfg, force: seen.setdefault("force", force) or [{"profile": "orchestrator"}],
    )
    monkeypatch.setattr(TAG, "hermes_bin", lambda cfg=None: fake_bin)
    monkeypatch.setattr(TAG, "auto_import_codex_profiles", lambda *_a, **_k: [])
    args = TAG.argparse.Namespace(config=None, refresh=False, skip_python_install=False, skip_tui_build=False, json=True)
    assert TAG.cmd_setup(args) == 0
    assert seen["force"] is False


def test_open_db_tolerates_concurrent_initialization(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    cfg = load_cfg()
    barrier = threading.Barrier(2)
    errors: list[Exception] = []

    def worker():
        try:
            barrier.wait(timeout=5)
            conn = TAG.open_db(cfg)
            conn.close()
        except Exception as exc:  # pragma: no cover - failure path asserted below
            errors.append(exc)

    threads = [threading.Thread(target=worker), threading.Thread(target=worker)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join(timeout=10)
    assert errors == []


def test_cmd_benchmark_missing_suite_fails_cleanly(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    monkeypatch.setattr(TAG, "ensure_hermes_ready", lambda *a, **k: None)
    args = TAG.argparse.Namespace(
        config=None,
        profile="researcher",
        model_ref=[],
        case=[],
        suite=str(tmp_path / "missing-suite.yaml"),
        json=False,
    )
    with pytest.raises(SystemExit, match="Benchmark suite not found:"):
        TAG.cmd_benchmark(args)


def test_cmd_import_codex_respects_tag_import_codex_home(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    import_home = tmp_path / "import-codex-home"
    import_home.mkdir()
    monkeypatch.setenv("TAG_IMPORT_CODEX_HOME", str(import_home))
    cfg_path = TAG.config_path(None)
    monkeypatch.setattr(TAG, "ensure_hermes_ready", lambda *a, **k: None)
    monkeypatch.setattr(TAG, "ensure_runtime_dirs", lambda *a, **k: None)
    monkeypatch.setattr(TAG, "profile_home", lambda *_a, **_k: tmp_path / "profile")
    (tmp_path / "profile").mkdir()
    seen = {}

    def fake_import(cfg, *, profile_name, source_codex_home):
        seen["source"] = source_codex_home
        return {"profile": profile_name, "status": "imported"}

    monkeypatch.setattr(TAG, "import_codex_into_profile", fake_import)
    args = TAG.argparse.Namespace(config=str(cfg_path), profile="orchestrator", codex_home=None, json=False)
    assert TAG.cmd_import_codex(args) == 0
    assert seen["source"] == import_home.resolve()


def test_ensure_default_file_permission_error_is_clean(tmp_path, monkeypatch):
    target = tmp_path / "config" / "tag.yaml"
    source = ROOT / "src" / "tag" / "config" / "default.yaml"

    def deny(*_a, **_k):
        raise PermissionError(13, "Permission denied")

    monkeypatch.setattr(Path, "mkdir", deny)
    with pytest.raises(SystemExit, match="Cannot initialize TAG file"):
        TAG.ensure_default_file(target, source)


def test_load_openrouter_catalog_http_and_json_failures(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    cfg = load_cfg()
    profile_dir = TAG.profile_home(cfg, "researcher")
    profile_dir.mkdir(parents=True, exist_ok=True)
    (profile_dir / ".env").write_text("OPENROUTER_API_KEY=test-key\n", encoding="utf-8")

    req = urllib.error.HTTPError(
        url="https://openrouter.ai/api/v1/models",
        code=401,
        msg="Unauthorized",
        hdrs=None,
        fp=None,
    )
    monkeypatch.setattr(TAG.urllib.request, "urlopen", lambda *a, **k: (_ for _ in ()).throw(req))
    with pytest.raises(SystemExit, match="OpenRouter models request failed with HTTP 401."):
        TAG.load_openrouter_catalog(cfg, "researcher")

    req = urllib.error.HTTPError(
        url="https://openrouter.ai/api/v1/models",
        code=500,
        msg="Server Error",
        hdrs=None,
        fp=None,
    )
    monkeypatch.setattr(TAG.urllib.request, "urlopen", lambda *a, **k: (_ for _ in ()).throw(req))
    with pytest.raises(SystemExit, match="OpenRouter models request failed with HTTP 500."):
        TAG.load_openrouter_catalog(cfg, "researcher")

    monkeypatch.setattr(
        TAG.urllib.request,
        "urlopen",
        lambda *a, **k: (_ for _ in ()).throw(urllib.error.URLError("timed out")),
    )
    with pytest.raises(SystemExit, match="OpenRouter models request failed: timed out"):
        TAG.load_openrouter_catalog(cfg, "researcher")

    class FakeResp:
        def __enter__(self):
            return self
        def __exit__(self, exc_type, exc, tb):
            return False
        def read(self):
            return b"{not-json"

    monkeypatch.setattr(TAG.urllib.request, "urlopen", lambda *a, **k: FakeResp())
    with pytest.raises(SystemExit, match="OpenRouter models response was not valid JSON."):
        TAG.load_openrouter_catalog(cfg, "researcher")
