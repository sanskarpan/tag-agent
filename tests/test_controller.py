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


def test_rewrite_cli_hints_prefers_tag_commands():
    rewritten = TAG.rewrite_cli_hints(
        "Run `hermes auth add openrouter`, or edit ~/.hermes/.env for this Hermes profile.\n"
        "Use the proper Hermes/tag auth/config flow.\n"
        "Resume this session with:\n  hermes --resume abc123\n"
        "  │ 📚 skill     hermes-agent  0.0s"
    )
    assert "`tag auth add openrouter`" in rewritten
    assert "the active TAG profile env file" in rewritten
    assert "this TAG profile" in rewritten
    assert "proper TAG auth/config flow" in rewritten
    assert "tag --resume abc123" in rewritten
    assert "skill     tag-agent" in rewritten


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


def test_doctor_prerequisites_detects_hoisted_workspace_deps(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    root = tmp_path / "managed-hermes"
    monkeypatch.setenv("TAG_HERMES_ROOT", str(root))
    (root / "node_modules" / "react").mkdir(parents=True)
    (root / "node_modules" / "vitest").mkdir(parents=True)
    (root / "node_modules" / "react" / "package.json").write_text("{}", encoding="utf-8")
    (root / "node_modules" / "vitest" / "package.json").write_text("{}", encoding="utf-8")

    report = TAG.doctor_prerequisites(load_cfg())

    assert report["tui_react_installed"] is True
    assert report["tui_vitest_installed"] is True


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


def test_hermes_env_exposes_tag_cli_identity(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    monkeypatch.setenv("TAG_BIN", "/tmp/fake-tag")
    cfg = load_cfg()
    env = TAG.hermes_env(cfg)
    assert env["HERMES_BIN"] == "/tmp/fake-tag"
    assert env["HERMES_BIN_LABEL"] == "tag"
    assert env["HERMES_ENV_LABEL"] == "the active TAG profile env file"
    assert env["HERMES_TUI_DIR"] == str(TAG.hermes_root(cfg) / "ui-tui")


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
    runtime_help = next(
        choice.format_help()
        for action in parser._actions
        if isinstance(action, TAG.argparse._SubParsersAction)
        for name, choice in action.choices.items()
        if name == "runtime"
    )
    assert "managed runtime" in runtime_help


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
    with pytest.raises(SystemExit, match="TAG runtime archive could not be read:"):
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
    with pytest.raises(SystemExit, match="managed runtime Python is not installed; cannot bootstrap profiles"):
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


def test_cmd_hermes_passthrough_rewrites_short_lived_output(tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    cfg_path = TAG.config_path(None)
    monkeypatch.setattr(TAG, "ensure_hermes_ready", lambda *a, **k: None)
    monkeypatch.setattr(TAG, "hermes_bin", lambda *_a, **_k: Path("/bin/echo"))

    def fake_run(cmd, **kwargs):
        assert kwargs["capture_output"] is True
        return TAG.argparse.Namespace(
            returncode=0,
            stdout="Resume this session with:\n  hermes --resume abc123\nthis Hermes profile\n",
            stderr="Set OPENROUTER_API_KEY in ~/.hermes/.env\n",
        )

    monkeypatch.setattr(TAG.subprocess, "run", fake_run)
    args = TAG.argparse.Namespace(
        config=str(cfg_path),
        profile="orchestrator",
        hermes_args=["chat", "-q", "hello"],
        hermes_version=False,
    )
    assert TAG.cmd_hermes_passthrough(args) == 0
    captured = capsys.readouterr()
    assert "tag --resume abc123" in captured.out
    assert "this TAG profile" in captured.out
    assert "the active TAG profile env file" in captured.err


def test_cmd_hermes_passthrough_captures_chat_help_for_rewrite(tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    cfg_path = TAG.config_path(None)
    monkeypatch.setattr(TAG, "ensure_hermes_ready", lambda *a, **k: None)
    monkeypatch.setattr(TAG, "hermes_bin", lambda *_a, **_k: Path("/bin/echo"))

    def fake_run(cmd, **kwargs):
        assert kwargs["capture_output"] is True
        return TAG.argparse.Namespace(
            returncode=0,
            stdout="Start an interactive chat session with Hermes Agent\nRun `hermes setup`\n",
            stderr="",
        )

    monkeypatch.setattr(TAG.subprocess, "run", fake_run)
    args = TAG.argparse.Namespace(
        config=str(cfg_path),
        profile="orchestrator",
        hermes_args=["chat", "--help"],
        hermes_version=False,
    )
    assert TAG.cmd_hermes_passthrough(args) == 0
    captured = capsys.readouterr()
    assert "Start an interactive chat session with TAG" in captured.out
    assert "Run `tag setup`" in captured.out


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
    with pytest.raises(SystemExit, match="Failed to create TAG-managed profile 'bad/name'"):
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


def test_install_tui_dependencies_uses_workspace_root_commands(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    root = tmp_path / "managed-hermes"
    root.mkdir()
    monkeypatch.setenv("TAG_HERMES_ROOT", str(root))
    calls: list[tuple[list[str], Path | None]] = []

    def fake_run_external(cmd, *, cwd=None, **kwargs):
        calls.append((cmd, cwd))
        return subprocess.CompletedProcess(cmd, 0, "", "")

    monkeypatch.setattr(TAG, "run_external", fake_run_external)

    result = TAG.install_tui_dependencies(load_cfg())

    assert result["status"] == "built"
    assert calls == [
        (
            [
                "npm",
                "install",
                "--workspace",
                "ui-tui",
                "--silent",
                "--no-fund",
                "--no-audit",
                "--progress=false",
            ],
            root,
        ),
        (
            ["npm", "run", "build", "--workspace", "ui-tui"],
            root,
        ),
    ]


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


# ---------- _upsert_env_line ----------


def test_upsert_env_line_creates_file(tmp_path):
    env_file = tmp_path / ".env"
    TAG._upsert_env_line(env_file, "ANTHROPIC_API_KEY", "sk-ant-test")
    assert "ANTHROPIC_API_KEY=sk-ant-test" in env_file.read_text()


def test_upsert_env_line_replaces_existing(tmp_path):
    env_file = tmp_path / ".env"
    env_file.write_text("ANTHROPIC_API_KEY=old-key\nOTHER=val\n")
    TAG._upsert_env_line(env_file, "ANTHROPIC_API_KEY", "new-key")
    lines = env_file.read_text().splitlines()
    assert "ANTHROPIC_API_KEY=new-key" in lines
    assert "ANTHROPIC_API_KEY=old-key" not in lines
    assert "OTHER=val" in lines


def test_upsert_env_line_appends_when_absent(tmp_path):
    env_file = tmp_path / ".env"
    env_file.write_text("EXISTING=yes\n")
    TAG._upsert_env_line(env_file, "NEW_KEY", "value123")
    text = env_file.read_text()
    assert "EXISTING=yes" in text
    assert "NEW_KEY=value123" in text


# ---------- _detect_claude_code_credentials ----------


def test_detect_claude_picks_up_api_key_from_env(tmp_path, monkeypatch):
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-api-key")
    result = TAG._detect_claude_code_credentials(source_home=tmp_path)
    assert result["api_key"] == "sk-ant-api-key"
    assert result["oauth_token"] is None


def test_detect_claude_reads_credentials_json(tmp_path, monkeypatch):
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    creds_file = tmp_path / ".credentials.json"
    creds_file.write_text(
        json.dumps({"claudeAiOauth": {"accessToken": "sk-ant-oat01-abc", "expiresAt": 9999999999000}}),
        encoding="utf-8",
    )
    result = TAG._detect_claude_code_credentials(source_home=tmp_path)
    assert result["oauth_token"] == "sk-ant-oat01-abc"
    assert result["oauth_expires_at"] == 9999999999000
    assert result["source"] == str(creds_file)


def test_detect_claude_returns_empty_when_nothing_found(tmp_path, monkeypatch):
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    result = TAG._detect_claude_code_credentials(source_home=tmp_path)
    assert result["api_key"] is None
    assert result["oauth_token"] is None


# ---------- import_claude_into_profile ----------


def test_import_claude_api_key_writes_env_file(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-real-key")
    cfg = load_cfg()
    profile_dir = TAG.profile_home(cfg, "researcher")
    profile_dir.mkdir(parents=True, exist_ok=True)
    result = TAG.import_claude_into_profile(cfg, profile_name="researcher")
    assert result["status"] == "imported"
    assert result["mode"] == "api_key"
    assert result["provider"] == "anthropic"
    env_vals = TAG.read_dotenv(profile_dir / ".env")
    assert env_vals.get("ANTHROPIC_API_KEY") == "sk-ant-real-key"


def test_import_claude_oauth_requires_flag(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    source = tmp_path / "claude"
    (source / ".credentials.json").parent.mkdir(parents=True, exist_ok=True)
    (source / ".credentials.json").write_text(
        json.dumps({"claudeAiOauth": {"accessToken": "sk-ant-oat01-tok"}}),
        encoding="utf-8",
    )
    cfg = load_cfg()
    # Without use_oauth: should skip
    profile_dir = TAG.profile_home(cfg, "researcher")
    profile_dir.mkdir(parents=True, exist_ok=True)
    result = TAG.import_claude_into_profile(cfg, profile_name="researcher", source_claude_home=source)
    assert result["status"] == "skipped-no-auth"

    # With use_oauth: should import and carry tos_warning
    result2 = TAG.import_claude_into_profile(
        cfg, profile_name="researcher", source_claude_home=source, use_oauth=True
    )
    assert result2["status"] == "imported"
    assert result2["mode"] == "oauth"
    assert "tos_warning" in result2
    env_vals = TAG.read_dotenv(profile_dir / ".env")
    assert env_vals.get("CLAUDE_CODE_OAUTH_TOKEN") == "sk-ant-oat01-tok"


def test_import_claude_skips_missing_profile(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-key")
    cfg = load_cfg()
    # profile dir not created — should return profile-missing, not crash
    result = TAG.import_claude_into_profile(cfg, profile_name="researcher")
    assert result["status"] == "profile-missing"


# ---------- _detect_gemini_credentials ----------


def test_detect_gemini_picks_up_api_key_from_env(tmp_path, monkeypatch):
    monkeypatch.setenv("GEMINI_API_KEY", "AIzaSy-test-key")
    result = TAG._detect_gemini_credentials(source_home=tmp_path)
    assert result["api_key"] == "AIzaSy-test-key"
    assert result["oauth_token"] is None


def test_detect_gemini_reads_dotenv(tmp_path, monkeypatch):
    monkeypatch.delenv("GEMINI_API_KEY", raising=False)
    (tmp_path / ".env").write_text("GEMINI_API_KEY=AIzaSy-from-file\n", encoding="utf-8")
    result = TAG._detect_gemini_credentials(source_home=tmp_path)
    assert result["api_key"] == "AIzaSy-from-file"


def test_detect_gemini_reads_oauth_creds(tmp_path, monkeypatch):
    monkeypatch.delenv("GEMINI_API_KEY", raising=False)
    oauth = {
        "access_token": "ya29.access",
        "refresh_token": "1//refresh",
        "expiry_date": 9999000000000,
    }
    (tmp_path / "oauth_creds.json").write_text(json.dumps(oauth), encoding="utf-8")
    result = TAG._detect_gemini_credentials(source_home=tmp_path)
    assert result["oauth_token"] == "ya29.access"
    assert result["refresh_token"] == "1//refresh"
    assert result["oauth_expiry_ms"] == 9999000000000


# ---------- import_gemini_into_profile ----------


def test_import_gemini_api_key_writes_env_file(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    monkeypatch.setenv("GEMINI_API_KEY", "AIzaSy-real")
    cfg = load_cfg()
    profile_dir = TAG.profile_home(cfg, "researcher")
    profile_dir.mkdir(parents=True, exist_ok=True)
    result = TAG.import_gemini_into_profile(cfg, profile_name="researcher")
    assert result["status"] == "imported"
    assert result["mode"] == "api_key"
    assert result["provider"] == "gemini"
    env_vals = TAG.read_dotenv(profile_dir / ".env")
    assert env_vals.get("GEMINI_API_KEY") == "AIzaSy-real"


def test_import_gemini_oauth_writes_google_oauth_json(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    monkeypatch.delenv("GEMINI_API_KEY", raising=False)
    source = tmp_path / "gemini"
    source.mkdir()
    oauth = {"access_token": "ya29.x", "refresh_token": "1//r", "expiry_date": 9999000000000}
    (source / "oauth_creds.json").write_text(json.dumps(oauth), encoding="utf-8")
    cfg = load_cfg()
    profile_dir = TAG.profile_home(cfg, "coder")
    profile_dir.mkdir(parents=True, exist_ok=True)

    # Without use_oauth: skip
    result = TAG.import_gemini_into_profile(cfg, profile_name="coder", source_gemini_home=source)
    assert result["status"] == "skipped-no-auth"

    # With use_oauth: write google_oauth.json and include tos_warning
    result2 = TAG.import_gemini_into_profile(
        cfg, profile_name="coder", source_gemini_home=source, use_oauth=True
    )
    assert result2["status"] == "imported"
    assert result2["mode"] == "oauth"
    assert result2["provider"] == "google-gemini-cli"
    assert "tos_warning" in result2
    google_oauth_file = profile_dir / "auth" / "google_oauth.json"
    assert google_oauth_file.exists()
    stored = json.loads(google_oauth_file.read_text())
    assert stored["access_token"] == "ya29.x"
    assert stored["refresh_token"] == "1//r"


# ---------- build_parser exposes new import commands ----------


def test_build_parser_exposes_import_claude_and_import_gemini():
    p = TAG.build_parser()
    cmds = {}
    for action in p._actions:
        if hasattr(action, "_name_parser_map"):
            cmds = action._name_parser_map
    assert "import-claude" in cmds, "import-claude subcommand missing from parser"
    assert "import-gemini" in cmds, "import-gemini subcommand missing from parser"
    # Verify --use-oauth flag exists on both
    claude_p = cmds["import-claude"]
    gemini_p = cmds["import-gemini"]
    claude_opts = {a.option_strings[0] for a in claude_p._actions if a.option_strings}
    gemini_opts = {a.option_strings[0] for a in gemini_p._actions if a.option_strings}
    assert "--use-oauth" in claude_opts
    assert "--use-oauth" in gemini_opts


# ---------- _detect_continue_credentials ----------


def test_detect_continue_reads_yaml_config(tmp_path, monkeypatch):
    monkeypatch.delenv("OPENAI_API_KEY", raising=False)
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    config = {
        "models": [
            {"provider": "openai", "model": "gpt-4o", "apiKey": "sk-openai-abc"},
            {"provider": "anthropic", "model": "claude-3-5-sonnet", "apiKey": "sk-ant-abc"},
            {"provider": "deepseek", "model": "deepseek-coder", "apiKey": "sk-ds-abc"},
        ]
    }
    (tmp_path / "config.yaml").write_text(
        "\n".join([
            "models:",
            "  - provider: openai",
            "    model: gpt-4o",
            "    apiKey: sk-openai-abc",
            "  - provider: anthropic",
            "    model: claude-3-5-sonnet",
            "    apiKey: sk-ant-abc",
            "  - provider: deepseek",
            "    model: deepseek-coder",
            "    apiKey: sk-ds-abc",
        ]),
        encoding="utf-8",
    )
    result = TAG._detect_continue_credentials(source_home=tmp_path)
    assert result.get("OPENAI_API_KEY") == "sk-openai-abc"
    assert result.get("ANTHROPIC_API_KEY") == "sk-ant-abc"
    assert result.get("DEEPSEEK_API_KEY") == "sk-ds-abc"


def test_detect_continue_reads_json_config(tmp_path, monkeypatch):
    monkeypatch.delenv("OPENAI_API_KEY", raising=False)
    cfg = {
        "models": [
            {"provider": "mistral", "model": "mistral-large", "apiKey": "sk-ms-abc"},
            {"provider": "xai", "model": "grok-2", "apiKey": "xai-abc"},
        ]
    }
    (tmp_path / "config.json").write_text(json.dumps(cfg), encoding="utf-8")
    result = TAG._detect_continue_credentials(source_home=tmp_path)
    assert result.get("MISTRAL_API_KEY") == "sk-ms-abc"
    assert result.get("XAI_API_KEY") == "xai-abc"


def test_detect_continue_resolves_localenv_references(tmp_path, monkeypatch):
    monkeypatch.setenv("MY_OPENAI_KEY", "sk-from-env")
    (tmp_path / "config.yaml").write_text(
        "models:\n  - provider: openai\n    apiKey: localEnv:MY_OPENAI_KEY\n",
        encoding="utf-8",
    )
    result = TAG._detect_continue_credentials(source_home=tmp_path)
    assert result.get("OPENAI_API_KEY") == "sk-from-env"


def test_detect_continue_empty_when_no_config(tmp_path, monkeypatch):
    result = TAG._detect_continue_credentials(source_home=tmp_path)
    assert result == {}


# ---------- import_continue_into_profile ----------


def test_import_continue_writes_multiple_env_vars(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    source = tmp_path / "continue"
    source.mkdir()
    (source / "config.yaml").write_text(
        "models:\n"
        "  - provider: openai\n    apiKey: sk-openai-test\n"
        "  - provider: mistral\n    apiKey: sk-mistral-test\n",
        encoding="utf-8",
    )
    cfg = load_cfg()
    profile_dir = TAG.profile_home(cfg, "researcher")
    profile_dir.mkdir(parents=True, exist_ok=True)
    result = TAG.import_continue_into_profile(cfg, profile_name="researcher", source_continue_home=source)
    assert result["status"] == "imported"
    assert "OPENAI_API_KEY" in result["providers_imported"]
    assert "MISTRAL_API_KEY" in result["providers_imported"]
    env_vals = TAG.read_dotenv(profile_dir / ".env")
    assert env_vals.get("OPENAI_API_KEY") == "sk-openai-test"
    assert env_vals.get("MISTRAL_API_KEY") == "sk-mistral-test"


# ---------- _detect_mistral_credentials ----------


def test_detect_mistral_picks_up_env_var(tmp_path, monkeypatch):
    monkeypatch.setenv("MISTRAL_API_KEY", "sk-mistral-env")
    result = TAG._detect_mistral_credentials(source_home=tmp_path)
    assert result["api_key"] == "sk-mistral-env"


def test_detect_mistral_reads_vibe_dotenv(tmp_path, monkeypatch):
    monkeypatch.delenv("MISTRAL_API_KEY", raising=False)
    (tmp_path / ".env").write_text("MISTRAL_API_KEY=sk-mistral-vibe\n", encoding="utf-8")
    result = TAG._detect_mistral_credentials(source_home=tmp_path)
    assert result["api_key"] == "sk-mistral-vibe"
    assert result["source"] is not None


# ---------- import_mistral_into_profile ----------


def test_import_mistral_writes_env_file(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    monkeypatch.setenv("MISTRAL_API_KEY", "sk-mistral-real")
    cfg = load_cfg()
    profile_dir = TAG.profile_home(cfg, "coder")
    profile_dir.mkdir(parents=True, exist_ok=True)
    result = TAG.import_mistral_into_profile(cfg, profile_name="coder")
    assert result["status"] == "imported"
    assert result["provider"] == "mistral"
    env_vals = TAG.read_dotenv(profile_dir / ".env")
    assert env_vals.get("MISTRAL_API_KEY") == "sk-mistral-real"


# ---------- build_parser exposes all import commands ----------


def test_build_parser_exposes_all_import_commands():
    p = TAG.build_parser()
    cmds = {}
    for action in p._actions:
        if hasattr(action, "_name_parser_map"):
            cmds = action._name_parser_map
    for cmd in (
        "import-codex", "import-claude", "import-gemini", "import-continue", "import-mistral",
        "import-opencode", "import-zed", "import-copilot", "import-aider", "import-aws", "import-cursor",
    ):
        assert cmd in cmds, f"{cmd} subcommand missing from parser"


# ---------- opencode credential import ----------


def test_detect_opencode_reads_auth_json(tmp_path, monkeypatch):
    monkeypatch.delenv("OPENAI_API_KEY", raising=False)
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    auth = tmp_path / "auth.json"
    auth.write_text(json.dumps({
        "anthropic": {"type": "api", "key": "sk-ant-opencode-test"},
        "openai": {"type": "api", "key": "sk-openai-test"},
        "github": {"type": "oauth", "access": "ghp_abc"},  # should be skipped (not "api")
        "groq": {"type": "api", "key": "gsk_groq_test"},
    }))
    result = TAG._detect_opencode_credentials(source_data_dir=tmp_path)
    assert result["ANTHROPIC_API_KEY"] == "sk-ant-opencode-test"
    assert result["OPENAI_API_KEY"] == "sk-openai-test"
    assert result["GROQ_API_KEY"] == "gsk_groq_test"
    assert "GITHUB_TOKEN" not in result  # oauth type skipped


def test_detect_opencode_empty_when_missing(tmp_path):
    result = TAG._detect_opencode_credentials(source_data_dir=tmp_path)
    assert result == {}


def test_import_opencode_writes_env_file(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
    cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
    TAG.profile_home(cfg, "researcher").mkdir(parents=True)
    auth_dir = tmp_path / "opencode"
    auth_dir.mkdir()
    (auth_dir / "auth.json").write_text(json.dumps({
        "anthropic": {"type": "api", "key": "sk-ant-oc-test"},
        "openai": {"type": "api", "key": "sk-oc-test"},
    }))
    result = TAG.import_opencode_into_profile(cfg, profile_name="researcher", source_data_dir=auth_dir)
    assert result["status"] == "imported"
    assert "ANTHROPIC_API_KEY" in result["providers_imported"]
    env = TAG.read_dotenv(TAG.profile_home(cfg, "researcher") / ".env")
    assert env["ANTHROPIC_API_KEY"] == "sk-ant-oc-test"
    assert env["OPENAI_API_KEY"] == "sk-oc-test"


# ---------- Zed credential import ----------


def test_detect_zed_reads_settings_json(tmp_path):
    settings = tmp_path / "settings.json"
    settings.write_text(json.dumps({
        "language_models": {
            "anthropic": {"api_key": "sk-ant-zed-test"},
            "openai": {"api_key": "sk-zed-openai", "api_url": "https://api.openai.com/v1"},
        }
    }))
    result = TAG._detect_zed_credentials(source_zed_config=settings)
    assert result["ANTHROPIC_API_KEY"] == "sk-ant-zed-test"
    assert result["OPENAI_API_KEY"] == "sk-zed-openai"


def test_detect_zed_empty_when_no_api_keys(tmp_path):
    settings = tmp_path / "settings.json"
    settings.write_text(json.dumps({"language_models": {"anthropic": {}}}))
    result = TAG._detect_zed_credentials(source_zed_config=settings)
    assert result == {}


def test_import_zed_writes_env_file(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
    cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
    TAG.profile_home(cfg, "coder").mkdir(parents=True)
    settings = tmp_path / "zed_settings.json"
    settings.write_text(json.dumps({
        "language_models": {"anthropic": {"api_key": "sk-ant-zed-import"}}
    }))
    result = TAG.import_zed_into_profile(cfg, profile_name="coder", source_zed_config=settings)
    assert result["status"] == "imported"
    env = TAG.read_dotenv(TAG.profile_home(cfg, "coder") / ".env")
    assert env["ANTHROPIC_API_KEY"] == "sk-ant-zed-import"


# ---------- GitHub Copilot credential import ----------


def test_detect_copilot_reads_gh_hosts_yml(tmp_path, monkeypatch):
    monkeypatch.delenv("GITHUB_TOKEN", raising=False)
    monkeypatch.delenv("GH_TOKEN", raising=False)
    hosts = tmp_path / "hosts.yml"
    hosts.write_text("github.com:\n    oauth_token: ghp_testtoken123\n    user: testuser\n")
    result = TAG._detect_copilot_credentials(source_gh_config=hosts)
    assert result["github_token"] == "ghp_testtoken123"
    assert result["source"] == str(hosts)


def test_detect_copilot_env_var_takes_priority(tmp_path, monkeypatch):
    monkeypatch.setenv("GITHUB_TOKEN", "ghp_from_env")
    result = TAG._detect_copilot_credentials(source_gh_config=tmp_path / "nonexistent.yml")
    assert result["github_token"] == "ghp_from_env"


def test_detect_copilot_empty_when_nothing(tmp_path, monkeypatch):
    monkeypatch.delenv("GITHUB_TOKEN", raising=False)
    monkeypatch.delenv("GH_TOKEN", raising=False)
    result = TAG._detect_copilot_credentials(source_gh_config=tmp_path / "nonexistent.yml")
    assert result["github_token"] is None


def test_import_copilot_writes_github_token(tmp_path, monkeypatch):
    monkeypatch.delenv("GITHUB_TOKEN", raising=False)
    monkeypatch.delenv("GH_TOKEN", raising=False)
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
    cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
    TAG.profile_home(cfg, "researcher").mkdir(parents=True)
    hosts = tmp_path / "hosts.yml"
    hosts.write_text("github.com:\n    oauth_token: ghp_copilot_test\n")
    result = TAG.import_copilot_into_profile(cfg, profile_name="researcher", source_gh_config=hosts)
    assert result["status"] == "imported"
    assert result["provider"] == "github-copilot"
    env = TAG.read_dotenv(TAG.profile_home(cfg, "researcher") / ".env")
    assert env["GITHUB_TOKEN"] == "ghp_copilot_test"


# ---------- Aider credential import ----------


def test_detect_aider_reads_yaml_config(tmp_path):
    conf = tmp_path / ".aider.conf.yml"
    conf.write_text("openai-api-key: sk-aider-openai\nanthropic-api-key: sk-ant-aider\n")
    result = TAG._detect_aider_credentials(source_home=tmp_path)
    assert result["OPENAI_API_KEY"] == "sk-aider-openai"
    assert result["ANTHROPIC_API_KEY"] == "sk-ant-aider"


def test_detect_aider_reads_api_key_list(tmp_path):
    conf = tmp_path / ".aider.conf.yml"
    conf.write_text("api-key:\n  - groq=gsk_aider_groq\n  - openrouter=sk-or-aider\n")
    result = TAG._detect_aider_credentials(source_home=tmp_path)
    assert result["GROQ_API_KEY"] == "gsk_aider_groq"
    assert result["OPENROUTER_API_KEY"] == "sk-or-aider"


def test_detect_aider_reads_dotenv(tmp_path):
    (tmp_path / ".env").write_text("OPENAI_API_KEY=sk-aider-dotenv\nDEEPSEEK_API_KEY=ds-aider\n")
    result = TAG._detect_aider_credentials(source_home=tmp_path)
    assert result["OPENAI_API_KEY"] == "sk-aider-dotenv"
    assert result["DEEPSEEK_API_KEY"] == "ds-aider"


def test_import_aider_writes_multiple_keys(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
    cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
    TAG.profile_home(cfg, "researcher").mkdir(parents=True)
    conf = tmp_path / "aider_home"
    conf.mkdir()
    (conf / ".aider.conf.yml").write_text(
        "openai-api-key: sk-aider-test\nanthropic-api-key: sk-ant-aider-test\n"
    )
    result = TAG.import_aider_into_profile(cfg, profile_name="researcher", source_home=conf)
    assert result["status"] == "imported"
    env = TAG.read_dotenv(TAG.profile_home(cfg, "researcher") / ".env")
    assert env["OPENAI_API_KEY"] == "sk-aider-test"
    assert env["ANTHROPIC_API_KEY"] == "sk-ant-aider-test"


# ---------- AWS credential import ----------


def test_detect_aws_reads_credentials_file(tmp_path, monkeypatch):
    monkeypatch.delenv("AWS_ACCESS_KEY_ID", raising=False)
    monkeypatch.delenv("AWS_SECRET_ACCESS_KEY", raising=False)
    monkeypatch.delenv("AWS_SESSION_TOKEN", raising=False)
    monkeypatch.delenv("AWS_DEFAULT_REGION", raising=False)
    creds = tmp_path / "credentials"
    creds.write_text(
        "[default]\naws_access_key_id = AKIATEST\naws_secret_access_key = testsecret\n"
    )
    result = TAG._detect_aws_credentials(source_aws_dir=tmp_path)
    assert result["access_key_id"] == "AKIATEST"
    assert result["secret_access_key"] == "testsecret"
    assert result["source"] == str(creds)


def test_detect_aws_env_var_takes_priority(tmp_path, monkeypatch):
    monkeypatch.setenv("AWS_ACCESS_KEY_ID", "AKIAFROMENV")
    monkeypatch.setenv("AWS_SECRET_ACCESS_KEY", "secretfromenv")
    result = TAG._detect_aws_credentials(source_aws_dir=tmp_path)
    assert result["access_key_id"] == "AKIAFROMENV"
    assert result["secret_access_key"] == "secretfromenv"


def test_detect_aws_empty_when_missing(tmp_path, monkeypatch):
    monkeypatch.delenv("AWS_ACCESS_KEY_ID", raising=False)
    monkeypatch.delenv("AWS_SECRET_ACCESS_KEY", raising=False)
    result = TAG._detect_aws_credentials(source_aws_dir=tmp_path)
    assert result["access_key_id"] is None
    assert result["secret_access_key"] is None


def test_import_aws_writes_env_file(tmp_path, monkeypatch):
    monkeypatch.delenv("AWS_ACCESS_KEY_ID", raising=False)
    monkeypatch.delenv("AWS_SECRET_ACCESS_KEY", raising=False)
    monkeypatch.delenv("AWS_SESSION_TOKEN", raising=False)
    monkeypatch.delenv("AWS_DEFAULT_REGION", raising=False)
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
    cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
    TAG.profile_home(cfg, "researcher").mkdir(parents=True)
    aws_dir = tmp_path / "aws"
    aws_dir.mkdir()
    (aws_dir / "credentials").write_text(
        "[default]\naws_access_key_id = AKIATEST123\naws_secret_access_key = topsecret\n"
    )
    (aws_dir / "config").write_text("[default]\nregion = us-east-1\n")
    result = TAG.import_aws_into_profile(cfg, profile_name="researcher", source_aws_dir=aws_dir)
    assert result["status"] == "imported"
    assert result["provider"] == "aws-bedrock"
    env = TAG.read_dotenv(TAG.profile_home(cfg, "researcher") / ".env")
    assert env["AWS_ACCESS_KEY_ID"] == "AKIATEST123"
    assert env["AWS_SECRET_ACCESS_KEY"] == "topsecret"
    assert env["AWS_DEFAULT_REGION"] == "us-east-1"


def test_import_aws_skipped_when_no_credentials(tmp_path, monkeypatch):
    monkeypatch.delenv("AWS_ACCESS_KEY_ID", raising=False)
    monkeypatch.delenv("AWS_SECRET_ACCESS_KEY", raising=False)
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
    cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
    TAG.profile_home(cfg, "researcher").mkdir(parents=True)
    result = TAG.import_aws_into_profile(cfg, profile_name="researcher", source_aws_dir=tmp_path)
    assert result["status"] == "skipped-no-auth"


# ---------- Cursor IDE credential import ----------


def test_detect_cursor_reads_sqlite(tmp_path):
    import sqlite3
    db_path = tmp_path / "state.vscdb"
    conn = sqlite3.connect(str(db_path))
    conn.execute("CREATE TABLE ItemTable (key TEXT, value TEXT)")
    conn.execute("INSERT INTO ItemTable VALUES (?, ?)", ("openai.apiKey", "sk-cursor-test"))
    conn.execute("INSERT INTO ItemTable VALUES (?, ?)", ("anthropic.apiKey", "sk-ant-cursor"))
    conn.execute("INSERT INTO ItemTable VALUES (?, ?)", ("unrelated.key", "some-value"))
    conn.commit()
    conn.close()
    result = TAG._detect_cursor_credentials(source_cursor_dir=tmp_path)
    assert result["OPENAI_API_KEY"] == "sk-cursor-test"
    assert result["ANTHROPIC_API_KEY"] == "sk-ant-cursor"
    assert "unrelated.key" not in result


def test_detect_cursor_empty_when_no_db(tmp_path):
    result = TAG._detect_cursor_credentials(source_cursor_dir=tmp_path)
    assert result == {}


def test_detect_cursor_fallback_value_pattern(tmp_path):
    import sqlite3
    db_path = tmp_path / "state.vscdb"
    conn = sqlite3.connect(str(db_path))
    conn.execute("CREATE TABLE ItemTable (key TEXT, value TEXT)")
    conn.execute("INSERT INTO ItemTable VALUES (?, ?)", ("some.unknown.key", "sk-ant-fallback-test"))
    conn.commit()
    conn.close()
    result = TAG._detect_cursor_credentials(source_cursor_dir=tmp_path)
    assert result.get("ANTHROPIC_API_KEY") == "sk-ant-fallback-test"


def test_import_cursor_writes_env_file(tmp_path, monkeypatch):
    import sqlite3
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "taghome"))
    cfg = TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")
    TAG.profile_home(cfg, "coder").mkdir(parents=True)
    cursor_dir = tmp_path / "cursor_storage"
    cursor_dir.mkdir()
    db_path = cursor_dir / "state.vscdb"
    conn = sqlite3.connect(str(db_path))
    conn.execute("CREATE TABLE ItemTable (key TEXT, value TEXT)")
    conn.execute("INSERT INTO ItemTable VALUES (?, ?)", ("openai.apiKey", "sk-cursor-import-test"))
    conn.commit()
    conn.close()
    result = TAG.import_cursor_into_profile(cfg, profile_name="coder", source_cursor_dir=cursor_dir)
    assert result["status"] == "imported"
    env = TAG.read_dotenv(TAG.profile_home(cfg, "coder") / ".env")
    assert env["OPENAI_API_KEY"] == "sk-cursor-import-test"

