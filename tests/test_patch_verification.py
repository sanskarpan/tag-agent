"""Real patch-tool regression coverage for partial bundled-runtime patches."""
import shutil
import subprocess

import pytest

# Reuse the suite's controller instance: test_controller loads it under an
# importlib alias, and importing a second canonical instance at collection
# time breaks the command tests' monkeypatches.
from test_controller import TAG as controller


PATCH = """diff --git a/one.txt b/one.txt
--- a/one.txt
+++ b/one.txt
@@ -1,3 +1,3 @@
 first
-old-one
+new-one
 last
diff --git a/two.txt b/two.txt
--- a/two.txt
+++ b/two.txt
@@ -1,3 +1,3 @@
 first
-old-two
+new-two
 last
"""


@pytest.fixture
def bundled(tmp_path, monkeypatch):
    if not shutil.which("patch"):
        pytest.skip("patch executable required")
    root = tmp_path / "runtime"
    root.mkdir()
    patch = tmp_path / "changes.patch"
    patch.write_text(PATCH, encoding="utf-8")
    monkeypatch.setattr(controller, "hermes_root", lambda _cfg=None: root)
    monkeypatch.setattr(controller, "hermes_patch_path", lambda: patch)
    monkeypatch.setattr(controller, "hermes_checkout_kind", lambda _root: "bundled")
    # Exercise the fuzzy fallback even where git's exact check would succeed.
    real_run = controller.run_external

    def run(cmd, **kwargs):
        if cmd[0] == "git":
            return subprocess.CompletedProcess(cmd, 1, "", "exact check failed")
        return real_run(cmd, **kwargs)

    monkeypatch.setattr(controller, "run_external", run)
    return root


@pytest.mark.parametrize("state", ["applied", "fuzzy", "partial", "missing", "unapplied"])
def test_patch_verifies_every_hunk_without_modifying_files(bundled, state):
    one = "old-one" if state == "unapplied" else "new-one"
    two = "old-two" if state in {"partial", "unapplied"} else "new-two"
    first = "drifted context" if state == "fuzzy" else "first"
    (bundled / "one.txt").write_text(f"{first}\n{one}\nlast\n", encoding="utf-8")
    if state != "missing":
        (bundled / "two.txt").write_text(f"first\n{two}\nlast\n", encoding="utf-8")
    before = {p.name: p.read_bytes() for p in bundled.iterdir()}
    if state in {"applied", "fuzzy"}:
        assert controller.patch_status({}) == "prepatched"
        assert controller.apply_hermes_patch({})["status"] == "prepatched"
    else:
        assert controller.patch_status({}) == "diverged"
        with pytest.raises(SystemExit, match="exact check failed"):
            controller.apply_hermes_patch({})
    assert {p.name: p.read_bytes() for p in bundled.iterdir()} == before


def test_failure_words_cannot_override_failed_exit_status(bundled, monkeypatch):
    def failed(cmd, **_kwargs):
        return subprocess.CompletedProcess(
            cmd, 1, "previously applied\n1 out of 1 hunks failed while patching", ""
        )

    monkeypatch.setattr(controller, "run_external", failed)
    assert controller.patch_status({}) == "diverged"
    with pytest.raises(SystemExit):
        controller.apply_hermes_patch({})
