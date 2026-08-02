package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tag-agent/tag/internal/agent"
	"github.com/tag-agent/tag/internal/permission"
)

// The regression suite for the in-root symlink permission bypass (CWE-59/200/863).
//
// The gate adjudicates a file tool's PATH. Before the fix that subject was
// computed lexically (filepath.Clean) while the tool then opened the path with
// symlinks resolved, so the two disagreed: a symlink INSIDE the tool root with
// an innocuous name could be adjudicated as the innocuous name and then read or
// written through to a credential-shaped file. The builtin `*.env` / `*.pem` /
// `~/.ssh/**` denies were bypassable by anything that could create a symlink in
// the workspace (including the model's own bash tool).

// TestSymlinkedReadCannotBypassCredentialDeny: `notes.txt -> secrets.env`, both
// inside the root. read_file("notes.txt") must be DENIED, because what it would
// actually read is a `.env`.
func TestSymlinkedReadCannotBypassCredentialDeny(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(root, "secrets.env")
	if err := os.WriteFile(secret, []byte("API_KEY=super-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "notes.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	reg := agent.NewRegistry()
	// DefaultPolicy: read_file is `allow`, but the credential denies sit above it.
	Register(reg, Options{Root: root, Guard: permission.NewGuard(permission.DefaultPolicy(), nil, nil)})

	out, err := runNamed(t, reg, "read_file", map[string]any{"path": "notes.txt"})
	if err == nil {
		t.Fatalf("BYPASS: read_file through an in-root symlink returned the secret: %q", out)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("expected a permission denial, got: %v", err)
	}
	if strings.Contains(out, "super-secret") {
		t.Fatalf("BYPASS: the secret leaked in the output: %q", out)
	}
}

// TestSymlinkedReadIsDeniedForRelativeLinkTargets covers the same bypass built
// from a RELATIVE symlink (`notes.txt -> ./secrets.env`), which is what a
// `ln -s` from inside the workspace actually produces.
func TestSymlinkedReadIsDeniedForRelativeLinkTargets(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "secrets.env"), []byte("API_KEY=super-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("secrets.env", filepath.Join(root, "notes.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	reg := agent.NewRegistry()
	Register(reg, Options{Root: root, Guard: permission.NewGuard(permission.DefaultPolicy(), nil, nil)})

	out, err := runNamed(t, reg, "read_file", map[string]any{"path": "notes.txt"})
	if err == nil || strings.Contains(out, "super-secret") {
		t.Fatalf("BYPASS: relative in-root symlink leaked the secret: %q %v", out, err)
	}
}

// TestSymlinkedDirectoryComponentCannotBypassCredentialDeny: the link is an
// intermediate DIRECTORY (`pub/ -> private/`), not the leaf.
func TestSymlinkedDirectoryComponentCannotBypassCredentialDeny(t *testing.T) {
	root := t.TempDir()
	priv := filepath.Join(root, "private")
	if err := os.MkdirAll(priv, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(priv, "deploy.pem"), []byte("-----BEGIN PRIVATE KEY-----"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(priv, filepath.Join(root, "pub")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	reg := agent.NewRegistry()
	Register(reg, Options{Root: root, Guard: permission.NewGuard(permission.DefaultPolicy(), nil, nil)})

	// The leaf name is still credential-shaped, so this one is caught either way;
	// what this asserts is that the resolved subject keeps naming the real file.
	if _, err := runNamed(t, reg, "read_file", map[string]any{"path": "pub/deploy.pem"}); err == nil {
		t.Fatal("read of a .pem through a symlinked directory must be denied")
	}
}

// TestSymlinkedWriteCannotRedirectOntoADeniedFile: write_file is granted for
// `notes.txt`, which is a symlink to `.env`. The write must NOT land on `.env`.
func TestSymlinkedWriteCannotRedirectOntoADeniedFile(t *testing.T) {
	root := t.TempDir()
	env := filepath.Join(root, ".env")
	if err := os.WriteFile(env, []byte("API_KEY=original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(env, filepath.Join(root, "notes.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// A blanket `--allow-tool write_file`. Blanket allows are already skipped for
	// credential paths — but only if the subject is recognisable as one.
	allow, err := permission.ParseSpec("write_file", permission.Allow)
	if err != nil {
		t.Fatal(err)
	}
	pol := permission.Policy{Rules: permission.Resolve(permission.Sources{Flags: []permission.Rule{allow}})}
	reg := agent.NewRegistry()
	Register(reg, Options{Root: root, Guard: permission.NewGuard(pol, nil, nil)})

	if _, err := runNamed(t, reg, "write_file", map[string]any{"path": "notes.txt", "content": "API_KEY=pwned"}); err == nil {
		t.Fatal("write_file through an in-root symlink onto .env must be denied")
	}
	if b, _ := os.ReadFile(env); string(b) != "API_KEY=original" {
		t.Fatalf("SIDE EFFECT RAN: .env was overwritten through the symlink: %q", string(b))
	}
}

// TestSymlinkedListDirCannotBypassAUserDeny: the builtin credential globs are
// all file-shaped, so the directory case is exercised with an operator rule.
func TestSymlinkedListDirCannotBypassAUserDeny(t *testing.T) {
	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	if err := os.MkdirAll(vault, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vault, "prod-token"), []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(vault, filepath.Join(root, "docs")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	deny, err := permission.ParseSpec("list_dir:**/vault", permission.Deny)
	if err != nil {
		t.Fatal(err)
	}
	pol := permission.Policy{Rules: permission.Resolve(permission.Sources{Flags: []permission.Rule{deny}})}
	reg := agent.NewRegistry()
	Register(reg, Options{Root: root, Guard: permission.NewGuard(pol, nil, nil)})

	if _, err := runNamed(t, reg, "list_dir", map[string]any{"path": "vault"}); err == nil {
		t.Fatal("precondition: the direct list_dir of vault should be denied")
	}
	out, err := runNamed(t, reg, "list_dir", map[string]any{"path": "docs"})
	if err == nil {
		t.Fatalf("BYPASS: list_dir through an in-root symlink listed the denied dir: %q", out)
	}
}

// TestPostResolutionRecheckClosesTheSwapRace simulates the TOCTOU window: the
// gate adjudicates `notes.txt` while it still points at a harmless file, and the
// link is repointed at `.env` before the tool opens it. The second, ruleset-only
// look inside resolvePath must catch the swapped-in target.
//
// It drives resolvePath directly because the race cannot be scheduled reliably
// through the public tool; the point is that the re-check exists and fires.
func TestPostResolutionRecheckClosesTheSwapRace(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "harmless.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("API_KEY=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "notes.txt")
	if err := os.Symlink("harmless.txt", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	opts := Options{Root: root, Guard: permission.NewGuard(permission.DefaultPolicy(), nil, nil)}

	// t0: what the gate would see, and what it decides.
	_, subj := pathSubject(opts, "path")(map[string]any{"path": "notes.txt"})
	if filepath.Base(subj) != "harmless.txt" {
		t.Fatalf("precondition: subject should resolve to harmless.txt, got %q", subj)
	}
	if _, denied := opts.guard().StaticDeny(permission.Request{
		Tool: "read_file", Kind: permission.KindPath, Subject: subj}); denied {
		t.Fatal("precondition: the adjudicated path should be allowed")
	}

	// t1: the attacker repoints the link.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".env", link); err != nil {
		t.Fatal(err)
	}

	// t2: the tool resolves again, immediately before the open.
	got, err := resolvePath(opts, "read_file", "notes.txt")
	if err == nil {
		t.Fatalf("TOCTOU: resolvePath handed back %q after the swap instead of refusing", got)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("expected a permission denial from the re-check, got: %v", err)
	}
}

// TestPostResolutionRecheckDoesNotReprompt: the re-check consults rules only. An
// `ask` that a human already answered must not produce a second prompt or a
// spurious denial.
func TestPostResolutionRecheckDoesNotReprompt(t *testing.T) {
	root := t.TempDir()
	prompts := 0
	g := permission.NewGuard(permission.DefaultPolicy(),
		permission.FuncPrompter(func(ctx context.Context, r permission.Request) (permission.Response, error) {
			prompts++
			return permission.ResponseAllowOnce, nil
		}), nil)
	reg := agent.NewRegistry()
	Register(reg, Options{Root: root, Guard: g})

	if _, err := runNamed(t, reg, "write_file", map[string]any{"path": "notes.md", "content": "hi"}); err != nil {
		t.Fatalf("an approved write must succeed: %v", err)
	}
	if prompts != 1 {
		t.Fatalf("the post-resolution re-check must not prompt again: %d prompts", prompts)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "notes.md")); string(b) != "hi" {
		t.Fatalf("approved write did not happen: %q", string(b))
	}
}

// TestPostResolutionRecheckHonoursDangerouslyAllowAll keeps the documented
// escape hatch working end to end.
func TestPostResolutionRecheckHonoursDangerouslyAllowAll(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "secrets.env"), []byte("K=v"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("secrets.env", filepath.Join(root, "notes.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	reg := agent.NewRegistry()
	Register(reg, Options{Root: root, Guard: permission.NewGuard(
		permission.Policy{DangerouslyAllowAll: true}, nil, nil)})

	if out, err := runNamed(t, reg, "read_file", map[string]any{"path": "notes.txt"}); err != nil || out != "K=v" {
		t.Errorf("--dangerously-allow-all must still bypass the gate: %q %v", out, err)
	}
}

// --- invariants the fix must NOT regress --------------------------------------

// TestSymlinkFixPreservesWritesToNewPaths: a path that does not exist yet (and
// whose parents do not exist yet) must still be writable.
func TestSymlinkFixPreservesWritesToNewPaths(t *testing.T) {
	root := t.TempDir()
	reg := agent.NewRegistry()
	Register(reg, Options{Root: root, Guard: permission.UnsafeAllowAllGuard()})

	if _, err := runNamed(t, reg, "write_file", map[string]any{"path": "a/b/c/new.txt", "content": "hi"}); err != nil {
		t.Fatalf("write to a not-yet-existing nested path must still work: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a/b/c/new.txt")); string(b) != "hi" {
		t.Fatalf("content not written: %q", string(b))
	}
}

// TestSymlinkFixPreservesRootEscapeGuard: a symlink pointing OUT of the root is
// still refused by the confinement check.
func TestSymlinkFixPreservesRootEscapeGuard(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "inside.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	reg := agent.NewRegistry()
	Register(reg, Options{Root: root, Guard: permission.UnsafeAllowAllGuard()})

	out, err := runNamed(t, reg, "read_file", map[string]any{"path": "inside.txt"})
	if err == nil {
		t.Fatalf("a symlink out of the root must be refused, got %q", out)
	}
	if !strings.Contains(err.Error(), "tool root") {
		t.Fatalf("expected the root-confinement error, got: %v", err)
	}
}

// TestSymlinkFixPreservesEnvExampleCarveOut: the `.env.example` family is still
// readable, including through a symlink.
func TestSymlinkFixPreservesEnvExampleCarveOut(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{".env.example", ".env.sample", ".env.template"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("KEY=changeme"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, ".env.example"), filepath.Join(root, "tpl")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	reg := agent.NewRegistry()
	Register(reg, Options{Root: root, Guard: permission.NewGuard(permission.DefaultPolicy(), nil, nil)})

	for _, name := range []string{".env.example", ".env.sample", ".env.template", "tpl"} {
		out, err := runNamed(t, reg, "read_file", map[string]any{"path": name})
		if err != nil || out != "KEY=changeme" {
			t.Errorf("%s must stay readable: %q %v", name, out, err)
		}
	}
}

// TestOrdinaryInRootSymlinkStillWorks: symlinks are not banned — one pointing at
// an ordinary in-root file is still followed.
func TestOrdinaryInRootSymlinkStillWorks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real.txt"), filepath.Join(root, "alias.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	reg := agent.NewRegistry()
	Register(reg, Options{Root: root, Guard: permission.NewGuard(permission.DefaultPolicy(), nil, nil)})

	if out, err := runNamed(t, reg, "read_file", map[string]any{"path": "alias.txt"}); err != nil || out != "payload" {
		t.Errorf("an ordinary in-root symlink must still resolve: %q %v", out, err)
	}
}
