package diffcontext

import (
	"strings"
	"testing"
)

func TestIsBlocked(t *testing.T) {
	pats := DefaultBlockedPatterns
	for _, f := range []string{".env", "config/.env", "prod.env", "secrets/db_password.txt", "api.token", "tls.pem", "my_secret_config.yaml"} {
		if !isBlocked(f, pats) {
			t.Errorf("%q should be blocked", f)
		}
	}
	for _, f := range []string{"app.py", "README.md", "src/main.go"} {
		if isBlocked(f, pats) {
			t.Errorf("%q should NOT be blocked", f)
		}
	}
}

func TestIsBinary(t *testing.T) {
	for _, f := range []string{"logo.png", "a.PDF", "lib.so", "archive.tar.gz"} {
		if !isBinary(f) {
			t.Errorf("%q should be binary", f)
		}
	}
	if isBinary("app.py") {
		t.Error("app.py should not be binary")
	}
}

func TestBuildRejectsOptionLikeRef(t *testing.T) {
	for _, ref := range []string{"--output=/tmp/pwn", "-x", "--ext-diff"} {
		if _, err := Build(ref, false, 3, 20, nil, t.TempDir()); err == nil || !strings.Contains(err.Error(), "invalid git ref") {
			t.Errorf("ref %q: expected invalid ref error, got %v", ref, err)
		}
	}
}

func TestFileDiffRejectsOptionLikeRef(t *testing.T) {
	if out := fileDiff("a.txt", "--output=/tmp/pwn", 3, false, t.TempDir()); out != "" {
		t.Errorf("expected empty diff for option-like ref, got %q", out)
	}
}

func TestEstimateTokens(t *testing.T) {
	if estimateTokens("") != 1 {
		t.Error("empty should estimate 1")
	}
	if estimateTokens("12345678") != 2 {
		t.Errorf("8 chars ~ 2 tokens, got %d", estimateTokens("12345678"))
	}
}
