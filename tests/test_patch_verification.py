"""Real patch-tool regression coverage for partial bundled-runtime patches."""
import shutil
import subprocess
import tarfile
from pathlib import Path

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


def test_shipped_runtime_satisfies_current_patch(tmp_path, monkeypatch):
    """Check packaged bytes, not just a developer's patched checkout."""
    if not shutil.which("patch"):
        pytest.skip("patch executable required")
    patch = controller.hermes_patch_path()
    paths = {
        line[6:]
        for line in patch.read_text(encoding="utf-8").splitlines()
        if line.startswith("+++ b/")
    }
    with tarfile.open(controller.bundled_hermes_archive(), "r:gz") as archive:
        for member in archive:
            relative = member.name.removeprefix("./")
            if relative not in paths:
                continue
            assert member.isfile(), relative
            path = Path(relative)
            assert not path.is_absolute() and ".." not in path.parts
            destination = tmp_path / path
            destination.parent.mkdir(parents=True, exist_ok=True)
            data = archive.extractfile(member)
            assert data is not None
            destination.write_bytes(data.read())
    monkeypatch.setattr(controller, "hermes_root", lambda _cfg=None: tmp_path)
    assert controller.patch_status({}) == "prepatched"
    assert controller.apply_hermes_patch({})["status"] == "prepatched"
