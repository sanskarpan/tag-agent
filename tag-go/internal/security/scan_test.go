package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanDetectsPlantedSecret(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "config.env")
	os.WriteFile(f, []byte("HOST=x\nAWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE\n"), 0o644)
	found := ScanFile(f)
	if len(found) == 0 {
		t.Fatal("expected to detect the AWS key")
	}
}

func TestScanSkipsBinary(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "data.bin")
	os.WriteFile(f, append([]byte("AKIAIOSFODNN7EXAMPLE"), 0x00, 0x01, 0x02), 0o644)
	if got := ScanFile(f); len(got) != 0 {
		t.Errorf("binary file should be skipped, got %d findings", len(got))
	}
}

func TestScanBigSecretAtEndOfLargeFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "big.txt")
	os.WriteFile(f, []byte(strings.Repeat("x", 2_000_000)+"\nAKIAIOSFODNN7EXAMPLE\n"), 0o644)
	if len(ScanFile(f)) == 0 {
		t.Error("secret in a 2MB text file should still be detected")
	}
}

// hasPattern reports whether any finding used the given pattern name.
func hasPattern(fs []Finding, name string) bool {
	for _, f := range fs {
		if f.Pattern == name {
			return true
		}
	}
	return false
}

func TestScanDetectsNewPatterns(t *testing.T) {
	cases := []struct {
		name    string
		content string
		pattern string
	}{
		// Bare values (no secret/token/api_key keyword) so the specific named
		// pattern fires rather than the generic_secret fallback.
		{"stripe", "config sk_" + "live_4eC39HqLyjWDarjtT1zdp7dcABCDEFGHIJ", "stripe_secret"},
		{"slack", "config xoxb" + "-1234567890-abcdefghijklmnop", "slack_token"},
		{"jwt", "config eyJ" + "hbGciOiJIUzI1NiIs.eyJzdWIiOiIxMjM0NTY3.SflKxwRJSMeKKF2QT4", "jwt_token"},
		{"google", "config AIza" + "SyD-abc123DEF456ghi789JKL012mno345PQR", "google_api_key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			f := filepath.Join(dir, "secrets.env")
			os.WriteFile(f, []byte(c.content+"\n"), 0o644)
			got := ScanFile(f)
			if !hasPattern(got, c.pattern) {
				t.Errorf("expected pattern %s to be detected, got %+v", c.pattern, got)
			}
		})
	}
}

func TestScanSkipsOutOfTreeSymlink(t *testing.T) {
	// Secret file OUTSIDE the scanned dir.
	outside := t.TempDir()
	secret := filepath.Join(outside, "passwd.txt")
	os.WriteFile(secret, []byte("AWS=AKIAIOSFODNN7EXAMPLE\n"), 0o644)

	// Scanned dir contains only a symlink pointing out of the tree.
	scanned := t.TempDir()
	link := filepath.Join(scanned, "link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	found := ScanDir(scanned, 100)
	if len(found) != 0 {
		t.Errorf("out-of-tree symlink target must not be reported, got %+v", found)
	}
}

func TestScanFollowsExplicitSymlink(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "passwd.txt")
	os.WriteFile(secret, []byte("AWS=AKIAIOSFODNN7EXAMPLE\n"), 0o644)

	link := filepath.Join(t.TempDir(), "link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	if found := ScanFile(link); len(found) == 0 {
		t.Error("an explicitly named symlink must be followed and its target scanned")
	}
}

func TestScanDirSurfacesWalkErrors(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root; permissions are not enforced")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(locked, "x.txt"), []byte("hi\n"), 0o644)
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	if !hasPattern(ScanDir(root, 100), "walk_error") {
		t.Error("permission-denied subtree must surface a walk_error finding")
	}
}
